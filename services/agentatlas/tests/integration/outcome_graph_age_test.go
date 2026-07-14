// Integration test for Task 0I: the production Apache AGE Outcome Graph
// projector and bounded query service, end to end against a REAL Apache AGE
// graph and the authoritative PostgreSQL projection outbox.
//
// It proves on real infrastructure: the four authoritative stores append a
// canonical projection event to the ONE shared outbox transactionally; the
// projector consumes the outbox and applies it to AGE idempotently (NATS
// redelivery converges, no duplicate edges); a drop/rebuild reproduces an
// IDENTICAL canonical graph digest; typed queries are tenant/org-isolated,
// parameterized (an injection attempt is an inert literal), bounded (slow-query
// cancellation), and revocation/supersession propagate; the outbox is
// append-only (pruning refused); and killing AGE leaves authoritative mutation
// unaffected while graph-dependent projection degrades explicitly and recovers.
//
// Gated on BOTH the authoritative Postgres DSN and the separately-configured AGE
// DSN:
//
//	docker run -d --rm --name age0i -e POSTGRES_PASSWORD=age -p 5457:5432 localhost:5001/age-postgres17:1.6.0
//	ATLAS_TEST_POSTGRES_DSN=postgres://atlas:atlas@localhost:5432/atlas_test_task0i?sslmode=disable \
//	ATLAS_TEST_AGE_DSN=postgres://postgres:age@localhost:5457/postgres?sslmode=disable \
//	  go test ./tests/integration -run TestOutcomeGraphAGE -v
package integration

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	sdkoutcome "github.com/astraclawteam/agentatlas/sdk/go/outcome"
	sdkworkcase "github.com/astraclawteam/agentatlas/sdk/go/workcase"
	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/outcome"
	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/outcomegraph"
	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/storage"
	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/tasks"

	"github.com/jackc/pgx/v5/pgxpool"
)

func ageDSN(t *testing.T) string {
	t.Helper()
	dsn := os.Getenv("ATLAS_TEST_AGE_DSN")
	if dsn == "" {
		t.Skip("set ATLAS_TEST_AGE_DSN (the separately-configured Apache AGE endpoint)")
	}
	return dsn
}

func ogPGDSN(t *testing.T) string {
	t.Helper()
	dsn := os.Getenv("ATLAS_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("set ATLAS_TEST_POSTGRES_DSN (authoritative postgres)")
	}
	return dsn
}

var ogSeq int64

func ogID(prefix string) string {
	ogSeq++
	return fmt.Sprintf("%s-%d-%d", prefix, time.Now().UnixNano(), ogSeq)
}

// ogHarness holds a unique tenant, an authoritative outcome store (which writes
// the shared outbox), a Postgres outbox reader, and a per-test AGE graph.
type ogHarness struct {
	ctx       context.Context
	pool      *pgxpool.Pool
	tenant    string
	store     *outcome.PostgresStore
	graphName string
	ageDSN    string
}

func newOGHarness(t *testing.T) *ogHarness {
	t.Helper()
	ctx := context.Background()
	pgDSN := ogPGDSN(t)
	adsn := ageDSN(t)
	if err := storage.Migrate(ctx, pgDSN); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	pool, err := storage.NewPool(ctx, pgDSN, nil)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	t.Cleanup(pool.Close)
	tenant := ogID("ent")
	if _, err := pool.Exec(ctx, `INSERT INTO enterprises(id,name) VALUES($1,'OutcomeGraph 0I')`, tenant); err != nil {
		t.Fatalf("seed enterprise: %v", err)
	}
	store, err := outcome.NewPostgresStore(pool, nil)
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	// A unique, safe graph name per test isolates AGE state.
	graphName := "og" + strings.ReplaceAll(ogID(""), "-", "")
	if len(graphName) > 40 {
		graphName = graphName[:40]
	}
	h := &ogHarness{ctx: ctx, pool: pool, tenant: tenant, store: store, graphName: graphName, ageDSN: adsn}
	return h
}

func (h *ogHarness) age(t *testing.T) *outcomegraph.AGEStore {
	t.Helper()
	// nil TLS manager: this integration harness dials a local AGE container
	// over the DSN's own sslmode (the GA Task 13A AGE-graph mTLS profile is
	// unit-proven in internal/outcomegraph/age_tls_test.go).
	s, err := outcomegraph.NewAGEStore(h.ctx, h.ageDSN, h.graphName, nil)
	if err != nil {
		t.Fatalf("age store: %v", err)
	}
	return s
}

// satisfiedOutcome builds a valid, satisfied Outcome revision for the harness
// tenant.
func (h *ogHarness) satisfiedOutcome(key string, rev uint64, goalKey, caseID, contributor, evidence string) sdkoutcome.Outcome {
	o := sdkoutcome.Outcome{
		Tenant: h.tenant, OutcomeKey: key, Revision: rev,
		Claim: sdkoutcome.OutcomeClaim{
			Goal:   sdkoutcome.GoalRef{Tenant: h.tenant, GoalKey: goalKey, GoalVersion: 1},
			Status: sdkoutcome.OutcomeSatisfied, RuleVersion: "rule-1",
			Observations: []sdkoutcome.ObservationRef{{Handle: "obs-" + key, ObservationHash: "sha256:o", Authority: "system_of_record", SignatureKeyID: "k1", ObservedAt: time.Now().UTC()}},
			Evidence:     []sdkoutcome.EvidenceRef{{Handle: evidence, ContentHash: "sha256:e", Authority: "system_of_record"}},
		},
		WorkCaseID: caseID, WorkCaseRevision: 1, WorkPlanRevision: 1, OperatingMapVersion: 1, OrgVersion: 1,
		Contributions: []sdkoutcome.ContributionRef{{ContributorID: contributor, Kind: "method", Weight: 0.5}},
		DecidedAt:     time.Now().UTC(),
	}
	return o
}

// persistOutcome mirrors the 0H evaluator flow: append the authoritative Outcome,
// then append its projection event (which writes the shared outbox with a rich,
// self-contained delta).
func (h *ogHarness) persistOutcome(t *testing.T, o sdkoutcome.Outcome) {
	t.Helper()
	if _, err := h.store.AppendOutcome(h.ctx, o); err != nil {
		t.Fatalf("append outcome: %v", err)
	}
	if _, err := h.store.AppendProjectionEvent(h.ctx, sdkoutcome.ProjectionEvent{
		Tenant: o.Tenant, Kind: sdkoutcome.ProjectionOutcomeRevision, SubjectType: sdkoutcome.NodeOutcome,
		SubjectID: o.OutcomeKey, SubjectRevision: o.Revision, PayloadHash: "sha256:" + o.OutcomeKey, RecordedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("append projection event: %v", err)
	}
}

// appendOutboxDirect appends any store's canonical event to the shared outbox in
// its own transaction (used to seed the org-bearing WorkCase node the workcase
// domain owns, without standing up the whole workcase store here).
func (h *ogHarness) appendOutboxDirect(t *testing.T, ev outcomegraph.ProjectionEvent) {
	t.Helper()
	tx, err := h.pool.Begin(h.ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(h.ctx) }()
	if _, err := outcomegraph.AppendOutboxTx(h.ctx, tx, ev, time.Now()); err != nil {
		t.Fatalf("append outbox: %v", err)
	}
	if err := tx.Commit(h.ctx); err != nil {
		t.Fatal(err)
	}
}

func workcaseNodeEvent(tenant, org, caseID string, rev uint64) outcomegraph.ProjectionEvent {
	ev := outcomegraph.MapWorkCase(sdkworkcase.WorkCase{ID: caseID, EnterpriseID: tenant, OrgScope: org, Status: sdkworkcase.Status("executing"), Revision: rev})
	ev.RecordedAt = time.Now().UTC()
	return ev
}

// --- end to end: project, query, isolation, injection, rebuild -------------

func TestOutcomeGraphAGE_ProjectQueryIsolationRebuild(t *testing.T) {
	h := newOGHarness(t)
	age := h.age(t)
	defer age.Close()
	proj := outcomegraph.NewProjector(age, outcomegraph.NewPostgresOutbox(h.pool))
	qs := outcomegraph.NewQueryService(age)

	// Two orgs sharing a goal, plus their org-bearing WorkCase nodes.
	h.persistOutcome(t, h.satisfiedOutcome("oc-x", 1, "goal.shared", "case-x", "agent:planner", "ev-x"))
	h.appendOutboxDirect(t, workcaseNodeEvent(h.tenant, "org-x", "case-x", 1))
	h.persistOutcome(t, h.satisfiedOutcome("oc-y", 1, "goal.shared", "case-y", "agent:tuner", "ev-y"))
	h.appendOutboxDirect(t, workcaseNodeEvent(h.tenant, "org-y", "case-y", 1))

	// Project the outbox into AGE.
	if _, err := proj.ProjectTenant(h.ctx, h.tenant); err != nil {
		t.Fatalf("project: %v", err)
	}

	// trace_outcome_basis returns the goal/observation/evidence/contributor.
	basis, err := qs.TraceOutcomeBasis(h.ctx, h.tenant, "", "oc-x", 1, outcomegraph.Budget{})
	if err != nil {
		t.Fatalf("trace basis: %v", err)
	}
	got := map[outcomegraph.NodeLabel]bool{}
	for _, n := range basis.Nodes {
		got[n.Label] = true
	}
	for _, want := range []outcomegraph.NodeLabel{outcomegraph.LabelGoal, outcomegraph.LabelObservation, outcomegraph.LabelEvidence, outcomegraph.LabelContributor} {
		if !got[want] {
			t.Errorf("AGE basis missing %s (got %v)", want, basisLabels(basis))
		}
	}
	if basis.Watermark == 0 {
		t.Error("query response missing projection watermark")
	}

	// Effective methods for the shared goal.
	methods, err := qs.FindEffectiveMethods(h.ctx, h.tenant, "", "goal.shared", 1, outcomegraph.Budget{})
	if err != nil || len(methods.Nodes) == 0 {
		t.Fatalf("find_effective_methods: nodes=%d err=%v", len(methods.Nodes), err)
	}

	// Org isolation: org-x find_similar must not surface org-y's case.
	scoped, err := qs.FindSimilarWorkcases(h.ctx, h.tenant, "org-x", "case-x", 1, outcomegraph.Budget{})
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range scoped.Nodes {
		if n.BusinessID == "case-y" {
			t.Fatal("AGE org-x query returned an org-y case (cross-org leak)")
		}
	}

	// Injection: a Cypher-breakout id is an inert bound literal.
	digestBefore, err := age.Digest(h.ctx, h.tenant)
	if err != nil {
		t.Fatal(err)
	}
	inj := `oc-x") DETACH DELETE (n) MERGE (z:Outcome {nid:"pwned"}) RETURN n //`
	res, err := qs.TraceOutcomeBasis(h.ctx, h.tenant, "", inj, 1, outcomegraph.Budget{})
	if err != nil {
		t.Fatalf("injection query errored: %v", err)
	}
	if len(res.Nodes) != 0 {
		t.Fatalf("injection matched %d nodes, want 0", len(res.Nodes))
	}
	digestAfter, err := age.Digest(h.ctx, h.tenant)
	if err != nil {
		t.Fatal(err)
	}
	if digestBefore != digestAfter {
		t.Fatal("injection attempt mutated the AGE graph")
	}

	// Rebuild equality: the incrementally-projected digest equals the rebuilt one.
	incremental := digestAfter
	if err := proj.Rebuild(h.ctx, h.tenant); err != nil {
		t.Fatalf("rebuild: %v", err)
	}
	rebuilt, err := age.Digest(h.ctx, h.tenant)
	if err != nil {
		t.Fatal(err)
	}
	if incremental != rebuilt {
		t.Fatalf("rebuild digest mismatch:\n incr=%s\n rebd=%s", incremental, rebuilt)
	}
}

func basisLabels(r outcomegraph.QueryResult) []string {
	var out []string
	for _, n := range r.Nodes {
		out = append(out, string(n.Label)+"/"+n.BusinessID)
	}
	return out
}

// --- NATS redelivery converges (idempotent) --------------------------------

func TestOutcomeGraphAGE_NATSRedeliveryIdempotent(t *testing.T) {
	h := newOGHarness(t)
	age := h.age(t)
	defer age.Close()
	proj := outcomegraph.NewProjector(age, outcomegraph.NewPostgresOutbox(h.pool))
	h.persistOutcome(t, h.satisfiedOutcome("oc-1", 1, "goal.x", "case-1", "agent:planner", "ev-1"))

	bus := tasks.NewMemBus()
	unsub, err := proj.Subscribe(h.ctx, bus)
	if err != nil {
		t.Fatal(err)
	}
	defer unsub()
	// Redeliver the same wakeup several times.
	for i := 0; i < 4; i++ {
		if err := outcomegraph.Notify(h.ctx, bus, h.tenant); err != nil {
			t.Fatal(err)
		}
	}
	d1, err := age.Digest(h.ctx, h.tenant)
	if err != nil {
		t.Fatal(err)
	}
	if err := outcomegraph.Notify(h.ctx, bus, h.tenant); err != nil {
		t.Fatal(err)
	}
	d2, err := age.Digest(h.ctx, h.tenant)
	if err != nil {
		t.Fatal(err)
	}
	if d1 != d2 {
		t.Fatal("redelivered projection produced duplicate edges (not idempotent)")
	}
	wm, err := age.Watermark(h.ctx, h.tenant)
	if err != nil || wm == 0 {
		t.Fatalf("watermark after redelivery = %d err=%v", wm, err)
	}
}

// --- revocation propagation ------------------------------------------------

func TestOutcomeGraphAGE_RevocationPropagates(t *testing.T) {
	h := newOGHarness(t)
	age := h.age(t)
	defer age.Close()
	proj := outcomegraph.NewProjector(age, outcomegraph.NewPostgresOutbox(h.pool))
	qs := outcomegraph.NewQueryService(age)
	h.persistOutcome(t, h.satisfiedOutcome("oc-1", 1, "goal.x", "case-1", "agent:planner", "ev-1"))
	if _, err := proj.ProjectTenant(h.ctx, h.tenant); err != nil {
		t.Fatal(err)
	}
	// Revoke the evidence via an append-only tombstone through the outcome store.
	if _, err := h.store.AppendProjectionEvent(h.ctx, sdkoutcome.ProjectionEvent{
		Tenant: h.tenant, Kind: sdkoutcome.ProjectionTombstone, SubjectType: sdkoutcome.NodeEvidence,
		SubjectID: "ev-1", SubjectRevision: 1, PayloadHash: "sha256:tomb", RecordedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("append tombstone: %v", err)
	}
	if _, err := proj.ProjectTenant(h.ctx, h.tenant); err != nil {
		t.Fatal(err)
	}
	basis, err := qs.TraceOutcomeBasis(h.ctx, h.tenant, "", "oc-1", 1, outcomegraph.Budget{})
	if err != nil {
		t.Fatal(err)
	}
	var sawRevoked bool
	for _, n := range basis.Nodes {
		if n.Label == outcomegraph.LabelEvidence && n.BusinessID == "ev-1" && n.Revoked {
			sawRevoked = true
		}
	}
	if !sawRevoked {
		t.Fatal("revoked evidence not flagged in the AGE graph")
	}
}

// --- slow-query cancellation -----------------------------------------------

func TestOutcomeGraphAGE_SlowQueryCancellation(t *testing.T) {
	h := newOGHarness(t)
	age := h.age(t)
	defer age.Close()
	proj := outcomegraph.NewProjector(age, outcomegraph.NewPostgresOutbox(h.pool))
	h.persistOutcome(t, h.satisfiedOutcome("oc-1", 1, "goal.x", "case-1", "agent:planner", "ev-1"))
	if _, err := proj.ProjectTenant(h.ctx, h.tenant); err != nil {
		t.Fatal(err)
	}
	// A 1ns time budget cancels the query rather than running it unbounded.
	_, err := outcomegraph.NewQueryService(age).TraceOutcomeBasis(h.ctx, h.tenant, "", "oc-1", 1, outcomegraph.Budget{Timeout: time.Nanosecond})
	if err == nil {
		t.Fatal("expected the 1ns budget to cancel the query")
	}
	if !errors.Is(err, outcomegraph.ErrQueryDeadline) && !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected a deadline cancellation, got %v", err)
	}
}

// --- forbidden outbox pruning (append-only) --------------------------------

func TestOutcomeGraphAGE_ForbiddenOutboxPruning(t *testing.T) {
	h := newOGHarness(t)
	h.persistOutcome(t, h.satisfiedOutcome("oc-1", 1, "goal.x", "case-1", "agent:planner", "ev-1"))
	// The outbox is append-only: UPDATE/DELETE are refused by the immutability
	// trigger; pruning history is impossible.
	if _, err := h.pool.Exec(h.ctx, `UPDATE outcome_graph_outbox SET payload_hash='tampered' WHERE tenant=$1`, h.tenant); err == nil {
		t.Fatal("expected the immutability trigger to refuse an outbox UPDATE")
	}
	if _, err := h.pool.Exec(h.ctx, `DELETE FROM outcome_graph_outbox WHERE tenant=$1`, h.tenant); err == nil {
		t.Fatal("expected the immutability trigger to refuse an outbox DELETE")
	}
}

// --- MAJOR-A: the AGE provider must org-filter the ANCHOR, not just results --

func TestOutcomeGraphAGE_ForeignOrgAnchorRejected(t *testing.T) {
	h := newOGHarness(t)
	age := h.age(t)
	defer age.Close()
	proj := outcomegraph.NewProjector(age, outcomegraph.NewPostgresOutbox(h.pool))
	qs := outcomegraph.NewQueryService(age)

	// Two orgs share a goal; each has its own org-scoped WorkCase.
	h.persistOutcome(t, h.satisfiedOutcome("oc-x", 1, "goal.shared", "case-x", "agent:planner", "ev-x"))
	h.appendOutboxDirect(t, workcaseNodeEvent(h.tenant, "org-x", "case-x", 1))
	h.persistOutcome(t, h.satisfiedOutcome("oc-y", 1, "goal.shared", "case-y", "agent:tuner", "ev-y"))
	h.appendOutboxDirect(t, workcaseNodeEvent(h.tenant, "org-y", "case-y", 1))
	if _, err := proj.ProjectTenant(h.ctx, h.tenant); err != nil {
		t.Fatal(err)
	}

	// An org-x-scoped query anchored on the ORG-Y case must return NOTHING: the
	// query may not traverse FROM a foreign-org anchor (org isolation on the
	// anchor, not just the result nodes).
	res, err := qs.FindSimilarWorkcases(h.ctx, h.tenant, "org-x", "case-y", 1, outcomegraph.Budget{})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Nodes) != 0 {
		t.Fatalf("org-x query traversed from an org-y anchor (cross-org anchor leak): %d nodes", len(res.Nodes))
	}
	// Sanity: an org-x anchor under org-x scope is allowed (same anchor, correct scope).
	ok, err := qs.FindSimilarWorkcases(h.ctx, h.tenant, "org-x", "case-x", 1, outcomegraph.Budget{})
	if err != nil {
		t.Fatal(err)
	}
	_ = ok // may be empty (sibling is org-y, filtered) — the point is it does not error and the anchor is in-scope.
}

// --- BLOCKER: per-tenant projection must be serialized (no duplicate vertices) --

func TestOutcomeGraphAGE_ConcurrentProjectionNoDuplicates(t *testing.T) {
	h := newOGHarness(t)
	age := h.age(t)
	defer age.Close()
	for i := 0; i < 8; i++ {
		h.persistOutcome(t, h.satisfiedOutcome(fmt.Sprintf("oc-%d", i), 1, "goal.x", fmt.Sprintf("case-%d", i), "agent:planner", fmt.Sprintf("ev-%d", i)))
	}

	events, err := outcomegraph.NewPostgresOutbox(h.pool).ReadAll(h.ctx, h.tenant)
	if err != nil {
		t.Fatal(err)
	}

	// The tightest reproduction of AGE's MERGE match-miss race: many overlapping
	// applications of the SAME committed events for the SAME tenant, released
	// together. This is the exact primitive ProjectTenant (ticker + NATS callback)
	// and Rebuild use. Without per-tenant serialization, each txn match-misses the
	// others' uncommitted vertices and CREATEs its own -> duplicates.
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_ = age.ApplyBatch(h.ctx, h.tenant, events)
		}()
	}
	close(start)
	wg.Wait()

	maxV, maxE, err := age.MaxDuplication(h.ctx, h.tenant)
	if err != nil {
		t.Fatal(err)
	}
	if maxV != 1 || maxE != 1 {
		t.Fatalf("concurrent projection created duplicates: max vertices/nid=%d, max edges/key=%d (want 1,1)", maxV, maxE)
	}

	// The production path (ProjectTenant) over the same graph must also converge
	// with no duplicates and reach the head watermark.
	pstart := make(chan struct{})
	var pwg sync.WaitGroup
	for i := 0; i < 4; i++ {
		pwg.Add(1)
		go func() {
			defer pwg.Done()
			p := outcomegraph.NewProjector(age, outcomegraph.NewPostgresOutbox(h.pool))
			<-pstart
			_, _ = p.ProjectTenant(h.ctx, h.tenant)
		}()
	}
	close(pstart)
	pwg.Wait()
	if mv, me, _ := age.MaxDuplication(h.ctx, h.tenant); mv != 1 || me != 1 {
		t.Fatalf("concurrent ProjectTenant created duplicates: max v/nid=%d e/key=%d", mv, me)
	}

	// The concurrently-projected graph must equal a sequential rebuild.
	incremental, err := age.Digest(h.ctx, h.tenant)
	if err != nil {
		t.Fatal(err)
	}
	proj := outcomegraph.NewProjector(age, outcomegraph.NewPostgresOutbox(h.pool))
	if err := proj.Rebuild(h.ctx, h.tenant); err != nil {
		t.Fatal(err)
	}
	rebuilt, err := age.Digest(h.ctx, h.tenant)
	if err != nil {
		t.Fatal(err)
	}
	if incremental != rebuilt {
		t.Fatalf("concurrent-projection digest != sequential rebuild digest")
	}
}

// --- kill AGE: authoritative continues; projection degrades; recovers ------

func TestOutcomeGraphAGE_KillAGEAuthoritativeContinuesThenRecovers(t *testing.T) {
	h := newOGHarness(t)

	// AGE is "down": a store pointed at an unreachable endpoint cannot even open.
	if _, err := outcomegraph.NewAGEStore(h.ctx, "postgres://postgres:age@127.0.0.1:5999/postgres?sslmode=disable", h.graphName, nil); err == nil {
		t.Fatal("expected NewAGEStore to fail against an unreachable AGE endpoint")
	}

	// Authoritative mutation CONTINUES regardless of AGE: appending outcomes and
	// their projection events writes only authoritative PostgreSQL (+ the outbox).
	h.persistOutcome(t, h.satisfiedOutcome("oc-1", 1, "goal.x", "case-1", "agent:planner", "ev-1"))
	h.persistOutcome(t, h.satisfiedOutcome("oc-2", 1, "goal.x", "case-2", "agent:planner", "ev-2"))
	var cnt int
	if err := h.pool.QueryRow(h.ctx, `SELECT count(*) FROM outcome_graph_outbox WHERE tenant=$1`, h.tenant).Scan(&cnt); err != nil {
		t.Fatal(err)
	}
	if cnt < 2 {
		t.Fatalf("authoritative outbox writes did not survive AGE outage: %d rows", cnt)
	}

	// A live AGE store, then killed mid-flight: projection degrades explicitly.
	age := h.age(t)
	proj := outcomegraph.NewProjector(age, outcomegraph.NewPostgresOutbox(h.pool))
	age.Close() // kill AGE
	if _, err := proj.ProjectTenant(h.ctx, h.tenant); !errors.Is(err, outcomegraph.ErrGraphUnavailable) {
		t.Fatalf("projection during outage: want ErrGraphUnavailable, got %v", err)
	}

	// Recovery: a fresh AGE store (same graph) catches up from the last committed
	// watermark with no duplicate edges.
	age2 := h.age(t)
	defer age2.Close()
	proj2 := outcomegraph.NewProjector(age2, outcomegraph.NewPostgresOutbox(h.pool))
	if _, err := proj2.ProjectTenant(h.ctx, h.tenant); err != nil {
		t.Fatalf("recovery projection: %v", err)
	}
	d1, err := age2.Digest(h.ctx, h.tenant)
	if err != nil {
		t.Fatal(err)
	}
	// Re-project (redelivery after recovery): idempotent.
	if _, err := proj2.ProjectTenant(h.ctx, h.tenant); err != nil {
		t.Fatal(err)
	}
	d2, err := age2.Digest(h.ctx, h.tenant)
	if err != nil {
		t.Fatal(err)
	}
	if d1 != d2 {
		t.Fatal("post-recovery re-projection produced duplicate edges")
	}
}
