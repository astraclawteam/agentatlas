// Integration test for Task 0J: the governed outcome-learning candidate
// lifecycle end to end against the authoritative PostgreSQL Outcome store (0G),
// the real typed 0I Outcome-Graph query surface, the existing governance draft
// path (0C) and the append-only immutable candidate store (migration 000018).
//
// It proves on real infrastructure: an effective-method candidate is distilled
// ONLY through the typed graph surface, replayed deterministically against the
// exact immutable historical Outcome versions, and — when accepted — handed to
// governance as a suggestion-only DRAFT (never published); the candidate row is
// append-only and immutable (a raw UPDATE/DELETE is rejected); re-distilling the
// same evidence is idempotent (identical deterministic id); the graph going
// unavailable PAUSES distillation with no candidate emitted; and a source that
// is revoked in the read model re-evaluates the candidate to a quarantine on
// withdrawn evidence.
//
// Gated on the authoritative Postgres DSN (the graph leg uses the in-memory 0I
// provider, which is the SAME typed QueryService the AGE provider serves):
//
//	ATLAS_TEST_POSTGRES_DSN=postgres://atlas:atlas@localhost:5432/atlas_test_task0j?sslmode=disable \
//	  go test ./tests/integration -run TestOutcomeLearning -v
package integration

import (
	"context"
	"os"
	"testing"
	"time"

	govmodel "github.com/astraclawteam/agentatlas/sdk/go/governance"
	outcomemodel "github.com/astraclawteam/agentatlas/sdk/go/outcome"
	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/governance"
	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/outcome"
	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/outcomegraph"
	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/outcomelearning"
	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/storage"
)

func TestOutcomeLearning(t *testing.T) {
	dsn := os.Getenv("ATLAS_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("set ATLAS_TEST_POSTGRES_DSN to run the outcome-learning integration test")
	}
	ctx := context.Background()
	olNow := func() time.Time { return time.Date(2026, 7, 14, 9, 0, 0, 0, time.UTC) }

	if err := storage.Migrate(ctx, dsn); err != nil {
		t.Fatalf("migrate (incl 000018): %v", err)
	}
	pool, err := storage.NewPool(ctx, dsn, nil)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	defer pool.Close()

	const (
		tenant   = "ent-ol-0j"
		org      = "org-ol-0j"
		goalKey  = "goal-ol-approve"
		methodID = "method-ol-two-step"
	)
	// Clean slate for a repeatable run (append-only tables forbid TRUNCATE, so
	// delete children first where allowed; the candidate table is immutable, so
	// use a fresh tenant id suffix per run instead).
	if _, err := pool.Exec(ctx, `INSERT INTO enterprises(id,name) VALUES($1,'0J Learning') ON CONFLICT (id) DO NOTHING`, tenant); err != nil {
		t.Fatalf("seed enterprise: %v", err)
	}

	outStore, err := outcome.NewPostgresStore(pool, olNow)
	if err != nil {
		t.Fatalf("outcome store: %v", err)
	}
	// Append three immutable outcomes concerning one goal: two satisfied via the
	// method, one unsatisfied without it. Unique keys per run avoid PK clashes.
	suffix := time.Now().UTC().Format("150405.000")
	o1 := appendOL(t, ctx, outStore, tenant, goalKey, "o1-"+suffix, outcomemodel.OutcomeSatisfied, []string{methodID}, true)
	o2 := appendOL(t, ctx, outStore, tenant, goalKey, "o2-"+suffix, outcomemodel.OutcomeSatisfied, []string{methodID}, true)
	o3 := appendOL(t, ctx, outStore, tenant, goalKey, "o3-"+suffix, outcomemodel.OutcomeUnsatisfied, nil, true)

	// Project the outcomes into the real typed graph surface (in-memory 0I
	// provider) via the SAME canonical mapper the AGE projector uses.
	graph := outcomegraph.NewMemoryGraph()
	var events []outcomegraph.ProjectionEvent
	for i, o := range []outcomemodel.Outcome{o1, o2, o3} {
		ev := outcomegraph.ProjectionEvent{
			Tenant: tenant, Sequence: uint64(i + 1), Source: outcomegraph.SourceOutcome, Kind: outcomegraph.KindUpsert,
			SubjectLabel: outcomegraph.LabelOutcome, SubjectID: o.OutcomeKey, SubjectRevision: o.Revision,
			Delta: outcomegraph.MapOutcome(o), RecordedAt: olNow(),
		}
		events = append(events, ev.WithHash())
	}
	if _, err := graph.ProjectBatch(ctx, tenant, events); err != nil {
		t.Fatalf("project outcomes into graph: %v", err)
	}
	qs := outcomegraph.NewQueryService(graph)

	// Governance (draft path) + append-only candidate store.
	gov := governance.NewService(governance.NewMemoryStore(olNow), nil, &governance.MemoryAuditAppender{}, governance.NewMemoryPublisher(), olNow)
	candStore, err := outcomelearning.NewPostgresCandidateStore(pool, olNow)
	if err != nil {
		t.Fatalf("candidate store: %v", err)
	}
	actor := governance.Actor{EnterpriseID: tenant, UserID: "system:outcome-learning", OrgUnitIDs: []string{org}, Permissions: []string{"suggest"}}
	svc := outcomelearning.NewService(qs, outStore, gov, outcomelearning.StaticDistiller{Summary: "two-step verification method"}, candStore, olNow)

	req := outcomelearning.DistillRequest{
		Tenant: tenant, Org: org, Kind: outcomelearning.KindMethod,
		Anchor:  outcomegraph.NodeRef{Label: outcomegraph.LabelGoal, BusinessID: goalKey, Revision: 1},
		Subject: outcomelearning.SourceRef{Kind: outcomelearning.SourceContributor, BusinessID: methodID, Revision: 1},
		Actor:   actor,
		Policy:  outcomelearning.Policy{GenerationVersion: "distill-policy-v1", ModelVersion: "static-distiller-1", MinCoverage: 0.5, ShadowBudget: 100},
	}

	// 1. Distill -> accepted, persisted, governance DRAFT (never published).
	cand, err := svc.Distill(ctx, req)
	if err != nil {
		t.Fatalf("Distill: %v", err)
	}
	if cand.Disposition != outcomelearning.DispositionAccepted {
		t.Fatalf("disposition = %q (%q), want accepted", cand.Disposition, cand.QuarantineReason)
	}
	if cand.Watermark != 3 {
		t.Fatalf("watermark = %d, want 3 (projected outbox depth)", cand.Watermark)
	}
	if cand.GovernanceChangeID == "" {
		t.Fatalf("accepted candidate must reach governance")
	}
	draft, err := gov.Get(ctx, actor, cand.GovernanceChangeID)
	if err != nil {
		t.Fatalf("governance draft: %v", err)
	}
	if draft.Draft.State != govmodel.ChangeDraftState || draft.Draft.PermissionMode != govmodel.PermissionSuggestionOnly {
		t.Fatalf("handoff must be a suggestion-only draft, got state=%q mode=%q", draft.Draft.State, draft.Draft.PermissionMode)
	}
	got, ok, err := candStore.Get(ctx, tenant, cand.ID)
	if err != nil || !ok {
		t.Fatalf("candidate not persisted: ok=%v err=%v", ok, err)
	}
	if got.ID != cand.ID || got.Disposition != outcomelearning.DispositionAccepted {
		t.Fatalf("persisted candidate mismatch")
	}

	// 2. Append-only immutability: a raw UPDATE/DELETE is rejected by the trigger.
	if _, err := pool.Exec(ctx, `UPDATE outcome_learning_candidates SET disposition='quarantined' WHERE tenant=$1 AND id=$2`, tenant, cand.ID); err == nil {
		t.Fatalf("expected the immutability trigger to reject an in-place UPDATE")
	}
	if _, err := pool.Exec(ctx, `DELETE FROM outcome_learning_candidates WHERE tenant=$1 AND id=$2`, tenant, cand.ID); err == nil {
		t.Fatalf("expected the immutability trigger to reject a DELETE")
	}

	// 3. Deterministic + idempotent: re-distilling identical evidence yields the
	// same deterministic id and the same replay digest (append is a no-op).
	cand2, err := svc.Distill(ctx, req)
	if err != nil {
		t.Fatalf("Distill (2nd): %v", err)
	}
	if cand2.ID != cand.ID || cand2.Replay.Digest != cand.Replay.Digest {
		t.Fatalf("re-distillation not deterministic: id %q/%q digest %q/%q", cand.ID, cand2.ID, cand.Replay.Digest, cand2.Replay.Digest)
	}

	// 4. Graph unavailable -> distillation PAUSES; no candidate emitted.
	graph.SetUnavailable(true)
	if _, err := svc.Distill(ctx, req); err != outcomegraph.ErrGraphUnavailable {
		t.Fatalf("graph-unavailable Distill = %v, want ErrGraphUnavailable (pause)", err)
	}
	graph.SetUnavailable(false)

	// 5. Revocation re-evaluation: tombstone a source in the read model, then
	// Reevaluate -> quarantined on withdrawn evidence, appended as a new row.
	tomb := outcomegraph.ProjectionEvent{
		Tenant: tenant, Sequence: 4, Source: outcomegraph.SourceOutcome, Kind: outcomegraph.KindTombstone,
		SubjectLabel: outcomegraph.LabelOutcome, SubjectID: o1.OutcomeKey, SubjectRevision: o1.Revision,
		Delta:      outcomegraph.GraphDelta{Mark: &outcomegraph.NodeRef{Label: outcomegraph.LabelOutcome, BusinessID: o1.OutcomeKey, Revision: o1.Revision}},
		RecordedAt: olNow(),
	}
	if _, err := graph.ProjectBatch(ctx, tenant, []outcomegraph.ProjectionEvent{tomb.WithHash()}); err != nil {
		t.Fatalf("tombstone projection: %v", err)
	}
	re, err := svc.Reevaluate(ctx, cand)
	if err != nil {
		t.Fatalf("Reevaluate: %v", err)
	}
	if re.Disposition != outcomelearning.DispositionQuarantined || re.QuarantineReason != outcomelearning.ReasonRevokedSource {
		t.Fatalf("re-evaluated candidate = %q/%q, want quarantined/revoked_source", re.Disposition, re.QuarantineReason)
	}
	if re.SupersedesID != cand.ID {
		t.Fatalf("re-evaluation must supersede the prior candidate")
	}
	if reGot, ok, _ := candStore.Get(ctx, tenant, re.ID); !ok || reGot.SupersedesID != cand.ID {
		t.Fatalf("re-evaluated candidate not appended as a new superseding row")
	}
}

func appendOL(t *testing.T, ctx context.Context, store *outcome.PostgresStore, tenant, goalKey, key string, status outcomemodel.OutcomeStatus, methods []string, withEvidence bool) outcomemodel.Outcome {
	t.Helper()
	claim := outcomemodel.OutcomeClaim{
		Goal:        outcomemodel.GoalRef{Tenant: tenant, GoalKey: goalKey, GoalVersion: 1},
		Status:      status,
		RuleVersion: "outcome-rule-v1",
	}
	if status == outcomemodel.OutcomeSatisfied {
		claim.Observations = []outcomemodel.ObservationRef{{
			Handle: "obs-" + key, ObservationHash: "sha256:" + rep0('b', 64),
			Authority: "erp-system-of-record", SignatureKeyID: "nexus-key-1", ObservedAt: time.Date(2026, 7, 14, 8, 0, 0, 0, time.UTC),
		}}
	}
	if withEvidence {
		claim.Evidence = []outcomemodel.EvidenceRef{{Handle: "ev-" + key, ContentHash: "sha256:" + rep0('c', 64), Authority: "erp"}}
	}
	var contribs []outcomemodel.ContributionRef
	for _, m := range methods {
		contribs = append(contribs, outcomemodel.ContributionRef{ContributorID: m, Kind: "method", Weight: 0.5})
	}
	o := outcomemodel.Outcome{
		Tenant: tenant, OutcomeKey: key, Revision: 1, Claim: claim,
		WorkCaseID: "case-" + key, WorkCaseRevision: 1, WorkPlanRevision: 1, OperatingMapVersion: 1,
		Contributions: contribs, DecidedAt: time.Date(2026, 7, 14, 8, 30, 0, 0, time.UTC),
	}
	stored, err := store.AppendOutcome(ctx, o)
	if err != nil {
		t.Fatalf("append outcome %s: %v", key, err)
	}
	return stored
}

func rep0(b byte, n int) string {
	out := make([]byte, n)
	for i := range out {
		out[i] = b
	}
	return string(out)
}
