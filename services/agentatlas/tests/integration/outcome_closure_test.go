// Integration test for Task 0H: the orchestrator drives a fully-executed
// WorkCase through DETERMINISTIC Outcome evaluation and Outcome-gated closure.
// It proves end-to-end the load-bearing invariant -- a WorkCase reaches
// `completed` ONLY through a `satisfied` terminal Outcome -- and that every
// not-satisfied conclusion (unsatisfied/absent/stale/forged) leaves the case
// un-completed. The Postgres+NATS leg additionally proves restart/replay and
// identical-content-hash replay equality against the authoritative store.
//
// In-process legs run with no DSN. The Postgres/NATS leg is gated on
// ATLAS_TEST_POSTGRES_DSN:
//
//	docker compose -f deploy/compose/compose.yaml up -d postgres
//	ATLAS_TEST_POSTGRES_DSN=postgres://atlas:atlas@localhost:5432/agentatlas?sslmode=disable \
//	  go test ./tests/integration -run TestOutcomeClosure
package integration

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"testing"
	"time"

	nexus "github.com/astraclawteam/agentatlas/sdk/go/nexus"
	sdkoutcome "github.com/astraclawteam/agentatlas/sdk/go/outcome"
	sdkworkcase "github.com/astraclawteam/agentatlas/sdk/go/workcase"
	governance "github.com/astraclawteam/agentatlas/services/agentatlas/internal/governance"
	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/outcome"
	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/storage"
	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/tasks"
	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/workcase"
)

const (
	ocKeyID     = "nexus-signing-key-1"
	ocAuthority = "erp-system-of-record"
	ocEnt       = "ent-0h"
	ocOrg       = "org:team-a"
	ocActor     = "actor-1"
	ocOrgVer    = int64(7)
	ocPolicyRev = "outcome-policy-rev-7"
)

var ocNow = time.Date(2026, 7, 14, 12, 0, 0, 0, time.UTC)

// --- signing helpers --------------------------------------------------------

func ocSignObs(priv ed25519.PrivateKey, keyID string, o nexus.ObservationReceipt) nexus.ObservationReceipt {
	o.Signature = nil
	payload, _ := json.Marshal(o)
	sig := ed25519.Sign(priv, payload)
	o.Signature = &nexus.Signature{Algorithm: nexus.SignatureAlgorithmEd25519, KeyID: keyID, Value: base64.StdEncoding.EncodeToString(sig)}
	return o
}

func ocSignAct(priv ed25519.PrivateKey, keyID string, r nexus.ActionReceipt) nexus.ActionReceipt {
	r.Signature = nil
	payload, _ := json.Marshal(r)
	sig := ed25519.Sign(priv, payload)
	r.Signature = &nexus.Signature{Algorithm: nexus.SignatureAlgorithmEd25519, KeyID: keyID, Value: base64.StdEncoding.EncodeToString(sig)}
	return r
}

func ocSHA(s string) string {
	sum := sha256.Sum256([]byte(s))
	return "sha256:" + hex.EncodeToString(sum[:])
}

// --- fake governor: deterministic, declares a postcondition + need ----------

type ocGovernor struct{}

func (g ocGovernor) request(c sdkworkcase.WorkCase, planRev uint64, step sdkworkcase.Step) nexus.ActionRequest {
	need := nexus.VerificationNeed{NeedID: step.ID + ":need", Source: "erp.system", Authority: ocAuthority, MaxAge: time.Hour}
	return nexus.ActionRequest{
		ActionID:               c.ID + "::" + step.ID,
		WorkCaseID:             c.ID,
		Actor:                  c.ActorRef,
		OrgScope:               c.OrgScope,
		Capability:             step.Action.BusinessCapability,
		ParameterHash:          ocSHA(c.ID + ":" + step.ID + ":" + step.Action.ParametersHash),
		VerificationNeeds:      []nexus.VerificationNeed{need},
		Postconditions:         []nexus.PostconditionSpec{{PostconditionID: step.ID + ":post", Description: "side effect confirmed", VerificationNeedID: need.NeedID}},
		Risk:                   nexus.RiskLow,
		ApprovalRefs:           []string{"apl:" + step.ID},
		IdempotencyKey:         "idem:" + c.ID + ":" + step.ID + "-00000000",
		ExpiresAt:              ocNow.Add(time.Hour),
		ExecutionReceiptSchema: "test.receipt.v1",
	}
}

func (g ocGovernor) PrepareAction(_ context.Context, c sdkworkcase.WorkCase, planRev uint64, step sdkworkcase.Step) (nexus.ActionRequest, governance.ApprovalParties, bool, error) {
	req := g.request(c, planRev, step)
	parties := governance.ApprovalParties{
		Submitter: governance.Party{UserID: "u-submitter", OrgUnitID: "org:team-a", OrgPath: []string{"org:root", "org:div"}, OrgVersion: ocOrgVer},
		Reviewers: []governance.Party{{UserID: "u-mgr", OrgUnitID: "org:div", OrgPath: []string{"org:root"}, OrgVersion: ocOrgVer}},
	}
	return req, parties, true, nil
}

func (g ocGovernor) PrepareCompensation(_ context.Context, _ sdkworkcase.WorkCase, _ uint64, _ sdkworkcase.Step) (nexus.ActionRequest, error) {
	return nexus.ActionRequest{}, errors.New("no compensation in this fixture")
}

// --- fake action gateway: signed succeeded receipt --------------------------

type ocGateway struct {
	priv ed25519.PrivateKey
}

func (g ocGateway) Dispatch(_ context.Context, req nexus.ActionRequest) (nexus.ActionReceipt, error) {
	r := nexus.ActionReceipt{
		ReceiptID: "rcp:" + req.ActionID, ActionID: req.ActionID, ParameterHash: req.ParameterHash,
		ConnectorRef: "connector-opaque", TargetRef: "target-opaque", ExecutorIdentity: "svc-connector-7",
		ResultCode: "succeeded", AuditRef: "audit:" + req.ActionID, IssuedAt: ocNow,
	}
	return ocSignAct(g.priv, ocKeyID, r), nil
}

func (g ocGateway) Reconcile(_ context.Context, _ nexus.ActionRequest) (nexus.ActionReceipt, bool, error) {
	return nexus.ActionReceipt{}, false, nil
}

// --- fake observation gateway (configurable per test) -----------------------

type ocObsMode int

const (
	obsSatisfying ocObsMode = iota // fresh, authoritative, matching
	obsAbsent                      // no observation exists yet
	obsStale                       // authoritative but older than MaxAge
	obsForged                      // signed by the wrong key
	obsRefuting                    // fresh, authoritative, but a different (refuting) hash
)

type ocObserver struct {
	priv     ed25519.PrivateKey
	wrong    ed25519.PrivateKey
	mode     ocObsMode
	normHash string
}

func (o ocObserver) FetchObservation(_ context.Context, req nexus.ActionRequest, needID string) (nexus.ObservationReceipt, bool, error) {
	if o.mode == obsAbsent {
		return nexus.ObservationReceipt{}, false, nil
	}
	var postID string
	for _, p := range req.Postconditions {
		if p.VerificationNeedID == needID {
			postID = p.PostconditionID
		}
	}
	observedAt := ocNow.Add(-30 * time.Minute)
	if o.mode == obsStale {
		observedAt = ocNow.Add(-2 * time.Hour)
	}
	hash := o.normHash
	if o.mode == obsRefuting {
		hash = ocSHA("rejected")
	}
	rec := nexus.ObservationReceipt{
		ObservationID: "obs:" + req.ActionID, ActionID: req.ActionID, ParameterHash: req.ParameterHash,
		PostconditionID: postID, VerificationNeedID: needID, Source: "erp.system", Authority: ocAuthority,
		ObservedAt: observedAt, NormalizedObservationHash: hash, EvidenceRef: "evh:" + req.ActionID, AuditRef: "aud:" + req.ActionID,
	}
	priv := o.priv
	if o.mode == obsForged {
		priv = o.wrong
	}
	return ocSignObs(priv, ocKeyID, rec), true, nil
}

// --- harness ----------------------------------------------------------------

type ocHarness struct {
	svc      *workcase.Service
	orch     *workcase.Orchestrator
	outStore outcome.Store
	caseID   string
	pub      ed25519.PublicKey
}

// acceptingS1 is the published accepting definition for the single write step's
// postcondition: an observed "approved" state CONFIRMS it. A happy path must
// declare this (satisfaction is by confirmation, never by mere presence).
func acceptingS1() map[string][]string { return map[string][]string{"s1:need": {ocSHA("approved")}} }

// buildHarness wires a service + outcome store + evaluator + closure + orchestrator
// over the supplied stores, drives a fresh one-write-step case to executing, and
// returns the harness. acceptingHashes (published policy) declares, per need, the
// normalized-observation hash(es) that CONFIRM the postcondition.
func buildHarness(t *testing.T, wcStore workcase.Store, outStore outcome.Store, obs ocObserver, acceptingHashes map[string][]string) ocHarness {
	t.Helper()
	ctx := context.Background()
	svc, err := workcase.NewService(wcStore, nil)
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	ev, err := outcome.NewEvaluator(outStore)
	if err != nil {
		t.Fatalf("NewEvaluator: %v", err)
	}
	cs, err := outcome.NewClosureService(outStore, svc)
	if err != nil {
		t.Fatalf("NewClosureService: %v", err)
	}

	caseID := drive0HToExecuting(t, svc)

	o, err := workcase.NewOrchestrator(workcase.Config{
		Service: svc, Runs: workcase.NewMemoryRunStore(), Gateway: ocGateway{priv: obs.priv}, Governor: ocGovernor{},
		TrustedKeyID: ocKeyID, TrustedKey: obs.pub(), ApprovalPolicy: governance.UpwardReviewPolicy{CurrentOrgVersion: ocOrgVer},
		Now: func() time.Time { return ocNow }, MaxAttempts: 3,
		Outcome: &workcase.OutcomeConfig{
			Store: outStore, Evaluator: ev, Closure: cs, Observer: obs,
			Policy:              outcome.Policy{Version: ocPolicyRev},
			Goal:                sdkoutcome.GoalRef{GoalKey: "mes.close_anomaly", GoalVersion: 3},
			OperatingMapVersion: 4, OrgVersion: 1, AcceptingHashes: acceptingHashes,
		},
	})
	if err != nil {
		t.Fatalf("NewOrchestrator: %v", err)
	}
	if _, err := o.Prepare(ctx, ocEnt, ocOrg, caseID); err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	return ocHarness{svc: svc, orch: o, outStore: outStore, caseID: caseID, pub: obs.pub()}
}

func (o ocObserver) pub() ed25519.PublicKey { return o.priv.Public().(ed25519.PublicKey) }

func drive0HToExecuting(t *testing.T, svc *workcase.Service) string {
	t.Helper()
	ctx := context.Background()
	c, err := svc.Create(ctx, workcase.CreateCommand{Command: workcase.Command{EnterpriseID: ocEnt, OrgScope: ocOrg, ActorRef: ocActor, IdempotencyKey: newID("idem-create")}})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	plan := sdkworkcase.WorkPlan{Steps: []sdkworkcase.Step{{ID: "s1", Action: &sdkworkcase.ActionSpec{
		Kind: "write", BusinessCapability: "erp.purchase_order.approve", ParametersHash: "sha256:fixture-s1",
		Risk: "low", IdempotencyKey: "s1:v1", FailurePolicy: "retry_then_human",
	}}}}
	afterPlan, err := svc.ProposePlan(ctx, workcase.ProposePlanCommand{Command: workcase.Command{EnterpriseID: ocEnt, OrgScope: ocOrg, ActorRef: ocActor, CaseID: c.ID, ExpectedRevision: c.Revision, IdempotencyKey: newID("idem-plan")}, Plan: plan})
	if err != nil {
		t.Fatalf("ProposePlan: %v", err)
	}
	afterReview, err := svc.StartReview(ctx, workcase.StartReviewCommand{Command: workcase.Command{EnterpriseID: ocEnt, OrgScope: ocOrg, ActorRef: ocActor, CaseID: c.ID, ExpectedRevision: afterPlan.Revision, IdempotencyKey: newID("idem-review")}})
	if err != nil {
		t.Fatalf("StartReview: %v", err)
	}
	if _, err := svc.StartExecution(ctx, workcase.StartExecutionCommand{Command: workcase.Command{EnterpriseID: ocEnt, OrgScope: ocOrg, ActorRef: ocActor, CaseID: c.ID, ExpectedRevision: afterReview.Revision, IdempotencyKey: newID("idem-exec")}}); err != nil {
		t.Fatalf("StartExecution: %v", err)
	}
	return c.ID
}

func newObserver(t *testing.T, mode ocObsMode) ocObserver {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	_ = pub
	if err != nil {
		t.Fatalf("key: %v", err)
	}
	_, wrong, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("wrong key: %v", err)
	}
	return ocObserver{priv: priv, wrong: wrong, mode: mode, normHash: ocSHA("approved")}
}

// --- in-process tests -------------------------------------------------------

// TestOutcomeClosureSatisfiedCompletesCase: the happy path. Every step is
// verified, a fresh authoritative observation satisfies the postcondition, and
// the WorkCase is completed THROUGH the satisfied Outcome.
func TestOutcomeClosureSatisfiedCompletesCase(t *testing.T) {
	ctx := context.Background()
	h := buildHarness(t, workcase.NewMemoryStore(func() time.Time { return ocNow }), outcome.NewMemoryStore(func() time.Time { return ocNow }), newObserver(t, obsSatisfying), acceptingS1())

	if _, err := h.orch.Advance(ctx, h.caseID); err != nil {
		t.Fatalf("Advance: %v", err)
	}
	final, _ := h.svc.Get(ctx, ocEnt, ocOrg, h.caseID)
	if final.Status != sdkworkcase.StatusCompleted {
		t.Fatalf("a satisfied Outcome must complete the case, got %q", final.Status)
	}
	// The Outcome that gated completion is satisfied and persisted.
	o, err := h.outStore.LatestOutcome(ctx, ocEnt, h.caseID+":outcome")
	if err != nil || o.Claim.Status != sdkoutcome.OutcomeSatisfied {
		t.Fatalf("the gating Outcome must be satisfied and persisted: %+v err=%v", o, err)
	}
}

// TestOutcomeClosureUnsatisfiedNeverCompletes (mandatory invariant): an action
// technically succeeds but the authoritative observation refutes the
// postcondition -> unsatisfied -> the case is governed-terminated, NEVER
// completed.
func TestOutcomeClosureUnsatisfiedNeverCompletes(t *testing.T) {
	ctx := context.Background()
	h := buildHarness(t, workcase.NewMemoryStore(func() time.Time { return ocNow }), outcome.NewMemoryStore(func() time.Time { return ocNow }),
		newObserver(t, obsRefuting), acceptingS1())

	if _, err := h.orch.Advance(ctx, h.caseID); err != nil {
		t.Fatalf("Advance: %v", err)
	}
	final, _ := h.svc.Get(ctx, ocEnt, ocOrg, h.caseID)
	if final.Status == sdkworkcase.StatusCompleted {
		t.Fatal("FORBIDDEN: an unsatisfied Outcome completed the case")
	}
	if final.Status != sdkworkcase.StatusTerminated {
		t.Fatalf("an unsatisfied Outcome must govern-terminate the case, got %q", final.Status)
	}
}

// TestOutcomeClosureAbsentObservationNeverCompletes (mandatory invariant): with
// no authoritative observation the required postcondition is unknown -> the case
// is NOT completed and rests executing (a human/reconciliation path).
func TestOutcomeClosureAbsentObservationNeverCompletes(t *testing.T) {
	ctx := context.Background()
	h := buildHarness(t, workcase.NewMemoryStore(func() time.Time { return ocNow }), outcome.NewMemoryStore(func() time.Time { return ocNow }), newObserver(t, obsAbsent), acceptingS1())

	res, err := h.orch.Advance(ctx, h.caseID)
	if err != nil {
		t.Fatalf("Advance: %v", err)
	}
	final, _ := h.svc.Get(ctx, ocEnt, ocOrg, h.caseID)
	if final.Status == sdkworkcase.StatusCompleted {
		t.Fatal("FORBIDDEN: a case with no satisfying observation completed")
	}
	if final.Status != sdkworkcase.StatusExecuting {
		t.Fatalf("an unresolved (unknown) Outcome must leave the case executing, got %q", final.Status)
	}
	if !res.Quiescent {
		t.Fatal("the case should be quiescent (resting on a human/reconciliation path)")
	}
	o, _ := h.outStore.LatestOutcome(ctx, ocEnt, h.caseID+":outcome")
	if o.Claim.Status != sdkoutcome.OutcomeUnknown {
		t.Fatalf("the produced Outcome should be unknown, got %q", o.Claim.Status)
	}
}

// TestOutcomeClosureStaleObservationNeverCompletes: a stale (but authentic)
// observation cannot satisfy -> unknown -> not completed.
func TestOutcomeClosureStaleObservationNeverCompletes(t *testing.T) {
	ctx := context.Background()
	h := buildHarness(t, workcase.NewMemoryStore(func() time.Time { return ocNow }), outcome.NewMemoryStore(func() time.Time { return ocNow }), newObserver(t, obsStale), acceptingS1())

	if _, err := h.orch.Advance(ctx, h.caseID); err != nil {
		t.Fatalf("Advance: %v", err)
	}
	final, _ := h.svc.Get(ctx, ocEnt, ocOrg, h.caseID)
	if final.Status == sdkworkcase.StatusCompleted {
		t.Fatal("FORBIDDEN: a stale observation completed the case")
	}
}

// TestOutcomeClosureForgedObservationNeverCompletes: a forged observation is
// dropped by fail-closed verification -> unknown -> not completed.
func TestOutcomeClosureForgedObservationNeverCompletes(t *testing.T) {
	ctx := context.Background()
	h := buildHarness(t, workcase.NewMemoryStore(func() time.Time { return ocNow }), outcome.NewMemoryStore(func() time.Time { return ocNow }), newObserver(t, obsForged), acceptingS1())

	if _, err := h.orch.Advance(ctx, h.caseID); err != nil {
		t.Fatalf("Advance: %v", err)
	}
	final, _ := h.svc.Get(ctx, ocEnt, ocOrg, h.caseID)
	if final.Status == sdkworkcase.StatusCompleted {
		t.Fatal("FORBIDDEN: a forged observation completed the case")
	}
}

// TestOutcomeClosureIdempotentReAdvance: re-advancing a completed case does not
// double-complete or produce a divergent Outcome (append-only + idempotent
// closure). The single satisfied Outcome remains revision 1.
func TestOutcomeClosureIdempotentReAdvance(t *testing.T) {
	ctx := context.Background()
	h := buildHarness(t, workcase.NewMemoryStore(func() time.Time { return ocNow }), outcome.NewMemoryStore(func() time.Time { return ocNow }), newObserver(t, obsSatisfying), acceptingS1())

	for i := 0; i < 3; i++ {
		if _, err := h.orch.Advance(ctx, h.caseID); err != nil {
			t.Fatalf("Advance %d: %v", i, err)
		}
	}
	final, _ := h.svc.Get(ctx, ocEnt, ocOrg, h.caseID)
	if final.Status != sdkworkcase.StatusCompleted {
		t.Fatalf("case must be completed, got %q", final.Status)
	}
	// Exactly one Outcome revision (revision 1); no divergent re-publication.
	if _, err := h.outStore.GetOutcome(ctx, ocEnt, h.caseID+":outcome", 1); err != nil {
		t.Fatalf("revision 1 must exist: %v", err)
	}
	if _, err := h.outStore.GetOutcome(ctx, ocEnt, h.caseID+":outcome", 2); !errors.Is(err, outcome.ErrNotFound) {
		t.Fatalf("no second Outcome revision must be produced on re-advance, got err=%v", err)
	}
}

// --- Postgres + NATS leg (DSN-gated) ---------------------------------------

// TestOutcomeClosurePostgresNATSRestartReplay drives the happy path over the
// authoritative PostgreSQL stores through the NATS-style task runner, proves
// completion survives a simulated restart (fresh stores over the same DB), and
// checks replay-equality: re-Decide-ing the identical versioned input yields an
// identical Outcome content hash.
func TestOutcomeClosurePostgresNATSRestartReplay(t *testing.T) {
	dsn := os.Getenv("ATLAS_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("set ATLAS_TEST_POSTGRES_DSN (production-standard postgres from deploy/compose)")
	}
	ctx := context.Background()
	if err := storage.Migrate(ctx, dsn); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	pool, err := storage.NewPool(ctx, dsn, nil)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	defer pool.Close()
	if _, err := pool.Exec(ctx, `INSERT INTO enterprises(id,name) VALUES($1,'0H Closure') ON CONFLICT (id) DO NOTHING`, ocEnt); err != nil {
		t.Fatalf("seed enterprise: %v", err)
	}

	wcStore, err := workcase.NewPostgresStore(pool, func() time.Time { return ocNow })
	if err != nil {
		t.Fatalf("workcase NewPostgresStore: %v", err)
	}
	outStore, err := outcome.NewPostgresStore(pool, func() time.Time { return ocNow })
	if err != nil {
		t.Fatalf("outcome NewPostgresStore: %v", err)
	}
	obs := newObserver(t, obsSatisfying)
	h := buildHarness(t, wcStore, outStore, obs, acceptingS1())

	// Drive Advance through the task runner over an in-memory bus (the NATS
	// dispatch shape; PG claims keep it idempotent). Duplicate delivery must not
	// double-complete.
	bus := tasks.NewMemBus()
	runner := tasks.NewRunner(bus)
	handler := tasks.WorkCaseAdvanceHandler(
		func(context.Context, string) (bool, error) { return true, nil },
		func(ctx context.Context, caseID string) error { _, err := h.orch.Advance(ctx, caseID); return err },
		func(context.Context, string, error) error { return nil },
	)
	if err := runner.Register(tasks.JobWorkCaseAdvance, handler); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if err := runner.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer runner.Stop()
	if err := bus.Publish(ctx, tasks.JobWorkCaseAdvance, h.caseID); err != nil {
		t.Fatalf("publish: %v", err)
	}
	if err := bus.Publish(ctx, tasks.JobWorkCaseAdvance, h.caseID); err != nil { // duplicate delivery
		t.Fatalf("publish dup: %v", err)
	}

	// Restart: brand-new stores over the SAME database.
	restarted, err := storage.NewPool(ctx, dsn, nil)
	if err != nil {
		t.Fatalf("reopen pool: %v", err)
	}
	defer restarted.Close()
	wcStore2, _ := workcase.NewPostgresStore(restarted, func() time.Time { return ocNow })
	svc2, _ := workcase.NewService(wcStore2, nil)
	final, err := svc2.Get(ctx, ocEnt, ocOrg, h.caseID)
	if err != nil {
		t.Fatalf("Get after restart: %v", err)
	}
	if final.Status != sdkworkcase.StatusCompleted {
		t.Fatalf("completion must be durable across restart, got %q", final.Status)
	}

	outStore2, _ := outcome.NewPostgresStore(restarted, func() time.Time { return ocNow })
	persisted, err := outStore2.LatestOutcome(ctx, ocEnt, h.caseID+":outcome")
	if err != nil || persisted.Claim.Status != sdkoutcome.OutcomeSatisfied {
		t.Fatalf("the satisfied Outcome must be durable: %+v err=%v", persisted, err)
	}

	// Replay equality: re-Decide the identical governed input; the content hash
	// must match the persisted decision.
	replayObserver := obs
	req := ocGovernor{}.request(final, 1, final.Plans[len(final.Plans)-1].Steps[0])
	rec, _, ferr := replayObserver.FetchObservation(ctx, req, "s1:need")
	if ferr != nil {
		t.Fatalf("fetch for replay: %v", ferr)
	}
	cmd := outcome.EvaluateCommand{
		Tenant: ocEnt, OutcomeKey: h.caseID + ":outcome", Revision: 1,
		Goal:                sdkoutcome.GoalRef{Tenant: ocEnt, GoalKey: "mes.close_anomaly", GoalVersion: 3},
		WorkCaseID:          h.caseID,
		WorkCaseRevision:    persisted.WorkCaseRevision,
		WorkPlanRevision:    persisted.WorkPlanRevision,
		OperatingMapVersion: 4, OrgVersion: 1,
		Policy:    outcome.Policy{Version: ocPolicyRev, Required: []outcome.RequiredPostcondition{{NeedID: "s1:need", PostconditionID: "s1:post", Authority: ocAuthority, AcceptingObservationHashes: []string{ocSHA("approved")}}}},
		DecidedAt: ocNow, TrustedKeyID: ocKeyID, TrustedKey: obs.pub(),
		Observations:  []outcome.ObservationInput{{Request: req, Receipt: rec}},
		Contributions: []sdkoutcome.ContributionRef{{ContributorID: ocActor, Kind: "actor", Weight: 1}},
	}
	replay, err := outcome.Decide(cmd)
	if err != nil {
		t.Fatalf("replay Decide: %v", err)
	}
	if outcome.ContentHash(replay) != outcome.ContentHash(persisted) {
		t.Fatalf("replay equality violated: replay=%s persisted=%s", outcome.ContentHash(replay), outcome.ContentHash(persisted))
	}
}
