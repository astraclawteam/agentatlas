package workcase_test

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	nexus "github.com/astraclawteam/agentatlas/sdk/go/nexus"
	sdkworkcase "github.com/astraclawteam/agentatlas/sdk/go/workcase"
	governance "github.com/astraclawteam/agentatlas/services/agentatlas/internal/governance"
	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/tasks"
	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/workcase"
)

const (
	orgVersion      = int64(7)
	trustedKeyID    = "nexus-signing-key-1"
	orchEnterprise  = "ent-1"
	orchOrg         = "org:team-a"
	orchActor       = "actor-1"
	resultSucceeded = "succeeded"
	resultFailed    = "failed"
)

var fixedNow = time.Date(2026, 7, 14, 12, 0, 0, 0, time.UTC)

// --- signing helpers (mirror sdk/go/nexus action_test.go pattern) -----------

func newKey(t *testing.T) (ed25519.PublicKey, ed25519.PrivateKey) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate ed25519 key: %v", err)
	}
	return pub, priv
}

// signReceipt reproduces nexus.ActionReceipt.signingPayload (Signature cleared,
// json.Marshal) and signs it, so nexus.VerifyActionReceipt accepts it.
func signReceipt(priv ed25519.PrivateKey, keyID string, r nexus.ActionReceipt) nexus.ActionReceipt {
	r.Signature = nil
	payload, _ := json.Marshal(r)
	sig := ed25519.Sign(priv, payload)
	r.Signature = &nexus.Signature{Algorithm: nexus.SignatureAlgorithmEd25519, KeyID: keyID, Value: base64.StdEncoding.EncodeToString(sig)}
	return r
}

func receiptFor(req nexus.ActionRequest, resultCode string) nexus.ActionReceipt {
	return nexus.ActionReceipt{
		ReceiptID:        "rcp:" + req.ActionID,
		ActionID:         req.ActionID,
		ParameterHash:    req.ParameterHash,
		ConnectorRef:     "connector-opaque",
		TargetRef:        "target-opaque",
		ExecutorIdentity: "svc-connector-7",
		ResultCode:       resultCode,
		AuditRef:         "audit:" + req.ActionID,
		IssuedAt:         fixedNow,
	}
}

func hashFor(s string) string {
	sum := sha256.Sum256([]byte(s))
	return "sha256:" + hex.EncodeToString(sum[:])
}

// --- fake gateway -----------------------------------------------------------

type fakeGateway struct {
	mu        sync.Mutex
	priv      ed25519.PrivateKey
	keyID     string
	dispatch  map[string]int // idempotency key -> dispatch count
	reconcile map[string]int // idempotency key -> reconcile count

	// onDispatch/onReconcile default to a valid succeeded receipt / not-found.
	onDispatch  func(g *fakeGateway, req nexus.ActionRequest, n int) (nexus.ActionReceipt, error)
	onReconcile func(g *fakeGateway, req nexus.ActionRequest, n int) (nexus.ActionReceipt, bool, error)
}

func newGateway(priv ed25519.PrivateKey, keyID string) *fakeGateway {
	return &fakeGateway{
		priv:      priv,
		keyID:     keyID,
		dispatch:  map[string]int{},
		reconcile: map[string]int{},
		onDispatch: func(g *fakeGateway, req nexus.ActionRequest, _ int) (nexus.ActionReceipt, error) {
			return signReceipt(g.priv, g.keyID, receiptFor(req, resultSucceeded)), nil
		},
		onReconcile: func(_ *fakeGateway, _ nexus.ActionRequest, _ int) (nexus.ActionReceipt, bool, error) {
			return nexus.ActionReceipt{}, false, nil
		},
	}
}

func (g *fakeGateway) Dispatch(_ context.Context, req nexus.ActionRequest) (nexus.ActionReceipt, error) {
	g.mu.Lock()
	g.dispatch[req.IdempotencyKey]++
	n := g.dispatch[req.IdempotencyKey]
	h := g.onDispatch
	g.mu.Unlock()
	return h(g, req, n)
}

func (g *fakeGateway) Reconcile(_ context.Context, req nexus.ActionRequest) (nexus.ActionReceipt, bool, error) {
	g.mu.Lock()
	g.reconcile[req.IdempotencyKey]++
	n := g.reconcile[req.IdempotencyKey]
	h := g.onReconcile
	g.mu.Unlock()
	return h(g, req, n)
}

func (g *fakeGateway) dispatchTotal() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	total := 0
	for _, n := range g.dispatch {
		total += n
	}
	return total
}

func (g *fakeGateway) distinctKeys() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return len(g.dispatch)
}

// compDispatchTotal sums Dispatch calls for compensation actions (idempotency
// keys built by PrepareCompensation carry the "idem-comp" prefix). The count is
// incremented before onDispatch runs, so it is accurate even while a widened
// compensation Dispatch is still blocking.
func (g *fakeGateway) compDispatchTotal() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	total := 0
	for k, n := range g.dispatch {
		if len(k) >= 9 && k[:9] == "idem-comp" {
			total += n
		}
	}
	return total
}

// --- fake governor ----------------------------------------------------------

// fakeGovernor deterministically expands a Step into a governed
// nexus.ActionRequest plus its upward-review parties. No LLM is involved. The
// side-effect determination is a TRUSTED authority signal derived from the
// business capability, NEVER the plan's (LLM-influenceable) Kind label: any
// Kind other than the literal "read" is treated as side-effecting.
type fakeGovernor struct {
	partiesFor    func(step sdkworkcase.Step, risk nexus.RiskLevel) (governance.ApprovalParties, error)
	sideEffecting func(step sdkworkcase.Step) bool
	// prepareHook, if set, runs at the START of PrepareAction (after the caller
	// has observed the run pending, BEFORE the pending->dispatched gate). The
	// concurrency test uses it as a rendezvous to hold both Advances at the
	// pre-gate point so they race the atomic gate simultaneously.
	prepareHook func()
}

func newGovernor() *fakeGovernor {
	return &fakeGovernor{partiesFor: validParties, sideEffecting: defaultSideEffecting}
}

// defaultSideEffecting: fail-closed -- anything not provably a plain "read" is a
// side effect. A step mislabeled with a non-"write" verb (e.g. "update") is
// STILL side-effecting, so the orchestrator cannot be tricked into skipping
// approval or the timeout safety by a Kind label.
func defaultSideEffecting(step sdkworkcase.Step) bool {
	return !(step.Action != nil && step.Action.Kind == "read")
}

func riskOf(step sdkworkcase.Step) nexus.RiskLevel {
	if step.Action != nil && step.Action.Risk == "high" {
		return nexus.RiskHigh
	}
	return nexus.RiskLow
}

func (gv *fakeGovernor) request(c sdkworkcase.WorkCase, planRev uint64, step sdkworkcase.Step, actionID string) nexus.ActionRequest {
	req := nexus.ActionRequest{
		ActionID:               actionID,
		GoalRef:                nexus.GoalRef("goal:" + c.ID),
		OutcomeRef:             nexus.OutcomeRef("outcome:" + c.ID + ":" + step.ID),
		WorkCaseID:             c.ID,
		WorkPlanRevision:       planRev,
		Actor:                  c.ActorRef,
		OrgScope:               c.OrgScope,
		Capability:             step.Action.BusinessCapability,
		ParameterHash:          hashFor(c.ID + ":" + step.ID + ":" + step.Action.ParametersHash),
		Risk:                   riskOf(step),
		IdempotencyKey:         "idem:" + c.ID + ":" + step.ID + ":r" + itoa(planRev),
		ExpiresAt:              fixedNow.Add(time.Hour),
		ExecutionReceiptSchema: "test.receipt.v1",
	}
	if gv.sideEffecting(step) {
		need := nexus.VerificationNeed{NeedID: step.ID + ":need", Source: "erp.system", Authority: "erp-system-of-record"}
		req.VerificationNeeds = []nexus.VerificationNeed{need}
		req.Postconditions = []nexus.PostconditionSpec{{PostconditionID: step.ID + ":post", Description: "side effect confirmed", VerificationNeedID: need.NeedID}}
		req.ApprovalRefs = []string{"apl:" + step.ID}
		if step.Action.CompensationActionID != nil {
			req.CompensationRef = *step.Action.CompensationActionID
		}
	}
	return req
}

func (gv *fakeGovernor) PrepareAction(_ context.Context, c sdkworkcase.WorkCase, planRev uint64, step sdkworkcase.Step) (nexus.ActionRequest, governance.ApprovalParties, bool, error) {
	if gv.prepareHook != nil {
		gv.prepareHook()
	}
	se := gv.sideEffecting(step)
	req := gv.request(c, planRev, step, c.ID+"::"+step.ID)
	if !se {
		return req, governance.ApprovalParties{}, false, nil
	}
	parties, err := gv.partiesFor(step, riskOf(step))
	if err != nil {
		return nexus.ActionRequest{}, governance.ApprovalParties{}, false, err
	}
	return req, parties, true, nil
}

func (gv *fakeGovernor) PrepareCompensation(_ context.Context, c sdkworkcase.WorkCase, planRev uint64, step sdkworkcase.Step) (nexus.ActionRequest, error) {
	if step.Action == nil || step.Action.CompensationActionID == nil {
		return nexus.ActionRequest{}, errors.New("no compensation action declared")
	}
	comp := step
	compID := *step.Action.CompensationActionID
	// Build a distinct compensation request (its own action id / idempotency key).
	req := gv.request(c, planRev, comp, c.ID+"::"+step.ID+"::comp")
	req.IdempotencyKey = "idem-comp:" + c.ID + ":" + step.ID + ":r" + itoa(planRev)
	req.Capability = compID
	req.VerificationNeeds = nil
	req.Postconditions = nil
	req.ApprovalRefs = nil
	req.CompensationRef = ""
	return req, nil
}

func itoa(u uint64) string {
	if u == 0 {
		return "0"
	}
	var b []byte
	for u > 0 {
		b = append([]byte{byte('0' + u%10)}, b...)
		u /= 10
	}
	return string(b)
}

// validParties returns eligible upward reviewers for the given risk: one for
// low, two distinct for high, all deciding against the current org version.
func validParties(_ sdkworkcase.Step, risk nexus.RiskLevel) (governance.ApprovalParties, error) {
	submitter := governance.Party{UserID: "u-submitter", OrgUnitID: "org:team-a", OrgPath: []string{"org:root", "org:div"}, OrgVersion: orgVersion}
	mgr := governance.Party{UserID: "u-mgr", OrgUnitID: "org:div", OrgPath: []string{"org:root"}, OrgVersion: orgVersion}
	reviewers := []governance.Party{mgr}
	if risk == nexus.RiskHigh {
		dir := governance.Party{UserID: "u-dir", OrgUnitID: "org:root", OrgPath: nil, OrgVersion: orgVersion}
		reviewers = append(reviewers, dir)
	}
	return governance.ApprovalParties{Submitter: submitter, Reviewers: reviewers}, nil
}

// --- fake audit -------------------------------------------------------------

type fakeAudit struct {
	mu   sync.Mutex
	refs []string
}

func (a *fakeAudit) RecordDecision(_ context.Context, caseID string, d workcase.DecisionRecord) (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	ref := "extaudit:" + caseID + ":" + itoa(uint64(len(a.refs)+1))
	a.refs = append(a.refs, ref)
	return ref, nil
}

// --- fake planner -----------------------------------------------------------

type fakePlanner struct {
	plan  sdkworkcase.WorkPlan
	err   error
	calls int
	mu    sync.Mutex
}

func (p *fakePlanner) ProposePlan(_ context.Context, _ sdkworkcase.WorkCase) (sdkworkcase.WorkPlan, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls++
	return p.plan, p.err
}

// --- test fixtures ----------------------------------------------------------

func readStep(id, capability string) sdkworkcase.Step {
	return sdkworkcase.Step{ID: id, Action: &sdkworkcase.ActionSpec{
		Kind: "read", BusinessCapability: capability, ParametersHash: "sha256:fixture-" + id, IdempotencyKey: id + ":v1",
	}}
}

func writeStep(id, capability, failurePolicy string, comp *string) sdkworkcase.Step {
	return sdkworkcase.Step{ID: id, Action: &sdkworkcase.ActionSpec{
		Kind: "write", BusinessCapability: capability, ParametersHash: "sha256:fixture-" + id,
		Risk: "low", IdempotencyKey: id + ":v1", FailurePolicy: failurePolicy, CompensationActionID: comp,
	}}
}

func strptr(s string) *string { return &s }

func idem(prefix string) string {
	k := prefix
	for len(k) < 16 {
		k += "-0"
	}
	return k
}

// executingCase drives a fresh case to StatusExecuting carrying plan, and
// returns the service, the case snapshot and the shared stores.
func executingCase(t *testing.T, plan sdkworkcase.WorkPlan) (*workcase.Service, *workcase.MemoryStore, sdkworkcase.WorkCase) {
	t.Helper()
	ctx := context.Background()
	store := workcase.NewMemoryStore(func() time.Time { return fixedNow })
	svc, err := workcase.NewService(store, nil)
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	c, err := svc.Create(ctx, workcase.CreateCommand{Command: workcase.Command{EnterpriseID: orchEnterprise, OrgScope: orchOrg, ActorRef: orchActor, IdempotencyKey: idem("create")}})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	afterPlan, err := svc.ProposePlan(ctx, workcase.ProposePlanCommand{Command: workcase.Command{EnterpriseID: orchEnterprise, OrgScope: orchOrg, ActorRef: orchActor, CaseID: c.ID, ExpectedRevision: c.Revision, IdempotencyKey: idem("plan")}, Plan: plan})
	if err != nil {
		t.Fatalf("ProposePlan: %v", err)
	}
	afterReview, err := svc.StartReview(ctx, workcase.StartReviewCommand{Command: workcase.Command{EnterpriseID: orchEnterprise, OrgScope: orchOrg, ActorRef: orchActor, CaseID: c.ID, ExpectedRevision: afterPlan.Revision, IdempotencyKey: idem("review")}})
	if err != nil {
		t.Fatalf("StartReview: %v", err)
	}
	afterExec, err := svc.StartExecution(ctx, workcase.StartExecutionCommand{Command: workcase.Command{EnterpriseID: orchEnterprise, OrgScope: orchOrg, ActorRef: orchActor, CaseID: c.ID, ExpectedRevision: afterReview.Revision, IdempotencyKey: idem("exec")}})
	if err != nil {
		t.Fatalf("StartExecution: %v", err)
	}
	return svc, store, afterExec
}

type orchDeps struct {
	svc     *workcase.Service
	runs    *workcase.MemoryRunStore
	gateway *fakeGateway
	gov     *fakeGovernor
	audit   *fakeAudit
	planner *fakePlanner
	pub     ed25519.PublicKey
	priv    ed25519.PrivateKey
}

func newDeps(t *testing.T, svc *workcase.Service) *orchDeps {
	t.Helper()
	pub, priv := newKey(t)
	return &orchDeps{
		svc:     svc,
		runs:    workcase.NewMemoryRunStore(),
		gateway: newGateway(priv, trustedKeyID),
		gov:     newGovernor(),
		audit:   &fakeAudit{},
		pub:     pub,
		priv:    priv,
	}
}

func (d *orchDeps) orch(t *testing.T) *workcase.Orchestrator {
	t.Helper()
	cfg := workcase.Config{
		Service:        d.svc,
		Runs:           d.runs,
		Gateway:        d.gateway,
		Governor:       d.gov,
		TrustedKeyID:   trustedKeyID,
		TrustedKey:     d.pub,
		ApprovalPolicy: governance.UpwardReviewPolicy{CurrentOrgVersion: orgVersion},
		Audit:          d.audit,
		Planner:        d.planner,
		Now:            func() time.Time { return fixedNow },
		MaxAttempts:    3,
	}
	o, err := workcase.NewOrchestrator(cfg)
	if err != nil {
		t.Fatalf("NewOrchestrator: %v", err)
	}
	return o
}

func prepareAndAdvance(t *testing.T, o *workcase.Orchestrator, caseID string) workcase.AdvanceResult {
	t.Helper()
	ctx := context.Background()
	if _, err := o.Prepare(ctx, orchEnterprise, orchOrg, caseID); err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	res, err := o.Advance(ctx, caseID)
	if err != nil {
		t.Fatalf("Advance: %v", err)
	}
	return res
}

func runByStep(ledger workcase.RunLedger, stepID string) (workcase.ActionRun, bool) {
	for _, r := range ledger.Runs {
		if r.StepID == stepID {
			return r, true
		}
	}
	return workcase.ActionRun{}, false
}

// --- tests ------------------------------------------------------------------

// TestOrchestratorSuccessfulMultiSourceWork: a plan reading one source and
// writing another. Every action is completed by a verified signed receipt; all
// declared VerificationNeeds are enqueued; and -- the crux of 0E -- the
// WorkCase does NOT become completed even though every step is done.
func TestOrchestratorSuccessfulMultiSourceWork(t *testing.T) {
	plan := sdkworkcase.WorkPlan{Steps: []sdkworkcase.Step{
		readStep("s-read", "mes.anomaly.read"),
		writeStep("s-write", "erp.purchase_order.approve", "retry_then_human", nil),
	}}
	svc, _, c := executingCase(t, plan)
	d := newDeps(t, svc)
	res := prepareAndAdvance(t, d.orch(t), c.ID)

	for _, id := range []string{"s-read", "s-write"} {
		r, ok := runByStep(res.Ledger, id)
		if !ok || r.State != workcase.ActionCompleted {
			t.Fatalf("step %s not completed: %+v (ok=%v)", id, r, ok)
		}
		if r.Outcome != workcase.OutcomeUnverified {
			t.Fatalf("completed step %s must carry outcome_unverified, got %q", id, r.Outcome)
		}
		if r.ReceiptID == "" {
			t.Fatalf("completed step %s must record its verified receipt id", id)
		}
	}

	// The write step's VerificationNeed must be enqueued (for Task 0G/0H), not
	// resolved here.
	if len(res.Ledger.Verify) != 1 || res.Ledger.Verify[0].StepID != "s-write" {
		t.Fatalf("expected exactly the write step's verification need enqueued, got %+v", res.Ledger.Verify)
	}

	final, err := svc.Get(context.Background(), orchEnterprise, orchOrg, c.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if final.Status != sdkworkcase.StatusExecuting {
		t.Fatalf("WorkCase must remain executing (completion is gated by a satisfied Outcome in Task 0H), got %q", final.Status)
	}
}

// TestOrchestratorSuccessfulReceiptDoesNotCompleteWorkCase is the explicit
// constraint-#1 proof demanded by the plan: a single fully-successful,
// verified receipt completes the Action step and leaves the receipt's business
// outcome unverified, but must NEVER drive the WorkCase to completed.
func TestOrchestratorSuccessfulReceiptDoesNotCompleteWorkCase(t *testing.T) {
	plan := sdkworkcase.WorkPlan{Steps: []sdkworkcase.Step{writeStep("s1", "erp.approve", "retry_then_human", nil)}}
	svc, _, c := executingCase(t, plan)
	d := newDeps(t, svc)
	res := prepareAndAdvance(t, d.orch(t), c.ID)

	r, _ := runByStep(res.Ledger, "s1")
	if r.State != workcase.ActionCompleted || r.Outcome != workcase.OutcomeUnverified {
		t.Fatalf("step must be completed with an unverified outcome, got state=%q outcome=%q", r.State, r.Outcome)
	}

	final, _ := svc.Get(context.Background(), orchEnterprise, orchOrg, c.ID)
	if final.Status == sdkworkcase.StatusCompleted {
		t.Fatal("FORBIDDEN: the orchestrator drove the WorkCase to completed on a technical receipt")
	}
	if final.Status != sdkworkcase.StatusExecuting {
		t.Fatalf("WorkCase must remain executing, got %q", final.Status)
	}
	// Re-advancing a fully-executed plan must still never complete the case.
	res2, err := d.orch(t).Advance(context.Background(), c.ID)
	if err != nil {
		t.Fatalf("second Advance: %v", err)
	}
	if !res2.Quiescent {
		t.Fatal("a fully-executed plan should be quiescent")
	}
	final2, _ := svc.Get(context.Background(), orchEnterprise, orchOrg, c.ID)
	if final2.Status != sdkworkcase.StatusExecuting {
		t.Fatalf("WorkCase status changed on re-advance to %q", final2.Status)
	}
}

// TestOrchestratorForgedReceiptNeverAdvancesStep: an unverifiable receipt
// (signed by the wrong key) must never complete a step. Fail-closed.
func TestOrchestratorForgedReceiptNeverAdvancesStep(t *testing.T) {
	plan := sdkworkcase.WorkPlan{Steps: []sdkworkcase.Step{writeStep("s1", "erp.approve", "retry_then_human", nil)}}
	svc, _, c := executingCase(t, plan)
	d := newDeps(t, svc)
	_, wrongPriv := newKey(t)
	d.gateway.onDispatch = func(g *fakeGateway, req nexus.ActionRequest, _ int) (nexus.ActionReceipt, error) {
		// Signed by an untrusted key: VerifyActionReceipt must reject it.
		return signReceipt(wrongPriv, trustedKeyID, receiptFor(req, resultSucceeded)), nil
	}
	// The forged receipt claims execution we cannot trust; reconciliation finds
	// no authoritative record, so the step stays unresolved rather than done.
	res := prepareAndAdvance(t, d.orch(t), c.ID)

	r, _ := runByStep(res.Ledger, "s1")
	if r.State == workcase.ActionCompleted {
		t.Fatal("a forged/untrusted receipt completed a step -- fail-open bug")
	}
	if r.ReceiptID != "" || r.Outcome == workcase.OutcomeUnverified {
		t.Fatalf("a forged receipt must not record a receipt id or outcome: %+v", r)
	}
	final, _ := svc.Get(context.Background(), orchEnterprise, orchOrg, c.ID)
	if final.Status == sdkworkcase.StatusCompleted {
		t.Fatal("forged receipt drove the case to completed")
	}
}

// TestOrchestratorLowRiskApprovalReference: a low-risk side effect proceeds
// with exactly one eligible upward reviewer, and is held for a human when the
// upward-review policy is not satisfied.
func TestOrchestratorLowRiskApprovalReference(t *testing.T) {
	plan := sdkworkcase.WorkPlan{Steps: []sdkworkcase.Step{writeStep("s1", "erp.approve", "retry_then_human", nil)}}
	svc, _, c := executingCase(t, plan)

	// Satisfied: one valid upward reviewer -> dispatched + completed.
	d := newDeps(t, svc)
	res := prepareAndAdvance(t, d.orch(t), c.ID)
	if r, _ := runByStep(res.Ledger, "s1"); r.State != workcase.ActionCompleted {
		t.Fatalf("low-risk write with a valid upward approval must complete, got %q", r.State)
	}

	// Unsatisfied: reviewer is a peer -> never dispatched, held for a human.
	svc2, _, c2 := executingCase(t, plan)
	d2 := newDeps(t, svc2)
	d2.gov.partiesFor = func(_ sdkworkcase.Step, _ nexus.RiskLevel) (governance.ApprovalParties, error) {
		return governance.ApprovalParties{
			Submitter: governance.Party{UserID: "u-submitter", OrgUnitID: "org:team-a", OrgPath: []string{"org:root", "org:div"}, OrgVersion: orgVersion},
			Reviewers: []governance.Party{{UserID: "u-peer", OrgUnitID: "org:team-a", OrgPath: []string{"org:root", "org:div"}, OrgVersion: orgVersion}},
		}, nil
	}
	res2 := prepareAndAdvance(t, d2.orch(t), c2.ID)
	r2, _ := runByStep(res2.Ledger, "s1")
	if r2.State != workcase.ActionHumanTakeover {
		t.Fatalf("low-risk write without a valid upward approval must go to human_takeover, got %q", r2.State)
	}
	if d2.gateway.dispatchTotal() != 0 {
		t.Fatalf("an unapproved side effect must never be dispatched, dispatches=%d", d2.gateway.dispatchTotal())
	}
}

// TestOrchestratorHighRiskApprovalReference: a high-risk side effect needs two
// distinct eligible upward reviewers.
func TestOrchestratorHighRiskApprovalReference(t *testing.T) {
	step := writeStep("s1", "erp.wire_transfer", "retry_then_human", nil)
	step.Action.Risk = "high"
	plan := sdkworkcase.WorkPlan{Steps: []sdkworkcase.Step{step}}

	// Two distinct upward reviewers -> proceeds.
	svc, _, c := executingCase(t, plan)
	d := newDeps(t, svc)
	res := prepareAndAdvance(t, d.orch(t), c.ID)
	if r, _ := runByStep(res.Ledger, "s1"); r.State != workcase.ActionCompleted {
		t.Fatalf("high-risk write with two upward reviewers must complete, got %q", r.State)
	}

	// Only one reviewer -> insufficient, held for a human, never dispatched.
	svc2, _, c2 := executingCase(t, plan)
	d2 := newDeps(t, svc2)
	d2.gov.partiesFor = func(_ sdkworkcase.Step, _ nexus.RiskLevel) (governance.ApprovalParties, error) {
		return governance.ApprovalParties{
			Submitter: governance.Party{UserID: "u-submitter", OrgUnitID: "org:team-a", OrgPath: []string{"org:root", "org:div"}, OrgVersion: orgVersion},
			Reviewers: []governance.Party{{UserID: "u-mgr", OrgUnitID: "org:div", OrgPath: []string{"org:root"}, OrgVersion: orgVersion}},
		}, nil
	}
	res2 := prepareAndAdvance(t, d2.orch(t), c2.ID)
	if r2, _ := runByStep(res2.Ledger, "s1"); r2.State != workcase.ActionHumanTakeover {
		t.Fatalf("high-risk write with only one reviewer must go to human_takeover, got %q", r2.State)
	}
	if d2.gateway.dispatchTotal() != 0 {
		t.Fatalf("under-approved high-risk side effect must never be dispatched, dispatches=%d", d2.gateway.dispatchTotal())
	}
}

// TestOrchestratorDuplicateReceiptIsIdempotent: advancing twice (a duplicated
// job delivery) dispatches the side effect exactly once.
func TestOrchestratorDuplicateReceiptIsIdempotent(t *testing.T) {
	plan := sdkworkcase.WorkPlan{Steps: []sdkworkcase.Step{writeStep("s1", "erp.approve", "retry_then_human", nil)}}
	svc, _, c := executingCase(t, plan)
	d := newDeps(t, svc)
	o := d.orch(t)
	ctx := context.Background()
	if _, err := o.Prepare(ctx, orchEnterprise, orchOrg, c.ID); err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if _, err := o.Advance(ctx, c.ID); err != nil {
		t.Fatalf("first Advance: %v", err)
	}
	res, err := o.Advance(ctx, c.ID)
	if err != nil {
		t.Fatalf("second Advance: %v", err)
	}
	if r, _ := runByStep(res.Ledger, "s1"); r.State != workcase.ActionCompleted {
		t.Fatalf("step must remain completed, got %q", r.State)
	}
	if got := d.gateway.dispatchTotal(); got != 1 {
		t.Fatalf("duplicate advancement must dispatch the side effect exactly once, got %d", got)
	}
}

// TestOrchestratorUnknownResultReconciliation: a transport timeout on a
// side-effecting action becomes result_unknown and is reconciled (never
// optimistically completed); reconciliation that finds the signed receipt then
// completes the step -- WITHOUT a second dispatch.
func TestOrchestratorUnknownResultReconciliation(t *testing.T) {
	plan := sdkworkcase.WorkPlan{Steps: []sdkworkcase.Step{writeStep("s1", "erp.approve", "retry_then_human", nil)}}
	svc, _, c := executingCase(t, plan)
	d := newDeps(t, svc)
	d.gateway.onDispatch = func(_ *fakeGateway, _ nexus.ActionRequest, _ int) (nexus.ActionReceipt, error) {
		return nexus.ActionReceipt{}, workcase.ErrTransportTimeout
	}
	d.gateway.onReconcile = func(g *fakeGateway, req nexus.ActionRequest, _ int) (nexus.ActionReceipt, bool, error) {
		return signReceipt(g.priv, g.keyID, receiptFor(req, resultSucceeded)), true, nil
	}
	res := prepareAndAdvance(t, d.orch(t), c.ID)

	r, _ := runByStep(res.Ledger, "s1")
	if r.State != workcase.ActionCompleted {
		t.Fatalf("reconciliation that finds the receipt must complete the step, got %q", r.State)
	}
	if d.gateway.dispatchTotal() != 1 {
		t.Fatalf("a timed-out side effect must be reconciled, never blindly re-dispatched: dispatches=%d", d.gateway.dispatchTotal())
	}
	if d.gateway.reconcile[r.IdempotencyKey] < 1 {
		t.Fatal("expected at least one reconciliation")
	}
}

// TestOrchestratorUnknownResultNeverBlindRetries: after a timeout, if the
// authoritative record shows the action did NOT execute, re-dispatch is safe
// and reuses the SAME idempotency key (idempotent at AgentNexus).
func TestOrchestratorUnknownResultReconcilesToSafeRetry(t *testing.T) {
	plan := sdkworkcase.WorkPlan{Steps: []sdkworkcase.Step{writeStep("s1", "erp.approve", "retry_then_human", nil)}}
	svc, _, c := executingCase(t, plan)
	d := newDeps(t, svc)
	timedOut := false
	d.gateway.onDispatch = func(g *fakeGateway, req nexus.ActionRequest, n int) (nexus.ActionReceipt, error) {
		if n == 1 {
			timedOut = true
			return nexus.ActionReceipt{}, workcase.ErrTransportTimeout
		}
		return signReceipt(g.priv, g.keyID, receiptFor(req, resultSucceeded)), nil
	}
	d.gateway.onReconcile = func(_ *fakeGateway, _ nexus.ActionRequest, _ int) (nexus.ActionReceipt, bool, error) {
		return nexus.ActionReceipt{}, false, nil // authoritative: not executed
	}
	res := prepareAndAdvance(t, d.orch(t), c.ID)
	r, _ := runByStep(res.Ledger, "s1")
	if !timedOut {
		t.Fatal("setup: first dispatch should have timed out")
	}
	if r.State != workcase.ActionCompleted {
		t.Fatalf("after a confirmed non-execution, a safe re-dispatch must complete the step, got %q", r.State)
	}
	if d.gateway.distinctKeys() != 1 {
		t.Fatalf("re-dispatch must reuse the same idempotency key, distinct keys=%d", d.gateway.distinctKeys())
	}
	if d.gateway.dispatch[r.IdempotencyKey] != 2 {
		t.Fatalf("expected exactly two dispatches under one idempotency key, got %d", d.gateway.dispatch[r.IdempotencyKey])
	}
}

// TestOrchestratorIdempotentRetry: a rejected (never-executed) dispatch is
// safely retried under the same idempotency key until it succeeds.
func TestOrchestratorIdempotentRetry(t *testing.T) {
	plan := sdkworkcase.WorkPlan{Steps: []sdkworkcase.Step{readStep("s1", "mes.read")}}
	svc, _, c := executingCase(t, plan)
	d := newDeps(t, svc)
	d.gateway.onDispatch = func(g *fakeGateway, req nexus.ActionRequest, n int) (nexus.ActionReceipt, error) {
		if n == 1 {
			return nexus.ActionReceipt{}, workcase.ErrActionRejected
		}
		return signReceipt(g.priv, g.keyID, receiptFor(req, resultSucceeded)), nil
	}
	res := prepareAndAdvance(t, d.orch(t), c.ID)
	r, _ := runByStep(res.Ledger, "s1")
	if r.State != workcase.ActionCompleted {
		t.Fatalf("a rejected read must be retried to completion, got %q", r.State)
	}
	if d.gateway.distinctKeys() != 1 || d.gateway.dispatch[r.IdempotencyKey] != 2 {
		t.Fatalf("idempotent retry must reuse one key across two dispatches, keys=%d count=%d", d.gateway.distinctKeys(), d.gateway.dispatch[r.IdempotencyKey])
	}
}

// TestOrchestratorRetryExhaustionEscalatesToHuman: repeated rejections beyond
// MaxAttempts land in human_takeover, not an infinite loop.
func TestOrchestratorRetryExhaustionEscalatesToHuman(t *testing.T) {
	plan := sdkworkcase.WorkPlan{Steps: []sdkworkcase.Step{readStep("s1", "mes.read")}}
	svc, _, c := executingCase(t, plan)
	d := newDeps(t, svc)
	d.gateway.onDispatch = func(_ *fakeGateway, _ nexus.ActionRequest, _ int) (nexus.ActionReceipt, error) {
		return nexus.ActionReceipt{}, workcase.ErrActionRejected
	}
	res := prepareAndAdvance(t, d.orch(t), c.ID)
	if r, _ := runByStep(res.Ledger, "s1"); r.State != workcase.ActionHumanTakeover {
		t.Fatalf("exhausted retries must escalate to human_takeover, got %q", r.State)
	}
	if d.gateway.dispatchTotal() > 3 {
		t.Fatalf("retries must be bounded by MaxAttempts, dispatches=%d", d.gateway.dispatchTotal())
	}
}

// TestOrchestratorCompensationSuccess: a write whose verified receipt reports a
// failure runs its declared compensation; a verified compensation receipt
// leaves the action compensated (never completed).
func TestOrchestratorCompensationSuccess(t *testing.T) {
	plan := sdkworkcase.WorkPlan{Steps: []sdkworkcase.Step{writeStep("s1", "erp.approve", "compensate_then_human", strptr("erp.revert_approval"))}}
	svc, _, c := executingCase(t, plan)
	d := newDeps(t, svc)
	d.gateway.onDispatch = func(g *fakeGateway, req nexus.ActionRequest, _ int) (nexus.ActionReceipt, error) {
		// The primary action's receipt reports failure; the compensation
		// action's receipt succeeds. They are told apart by idempotency key.
		code := resultSucceeded
		if req.IdempotencyKey[:4] != "idem" || (len(req.IdempotencyKey) >= 9 && req.IdempotencyKey[:9] == "idem-comp") {
			code = resultSucceeded
		}
		if len(req.IdempotencyKey) < 9 || req.IdempotencyKey[:9] != "idem-comp" {
			code = resultFailed
		}
		return signReceipt(g.priv, g.keyID, receiptFor(req, code)), nil
	}
	res := prepareAndAdvance(t, d.orch(t), c.ID)
	r, _ := runByStep(res.Ledger, "s1")
	if r.State != workcase.ActionCompensated {
		t.Fatalf("a failed write with successful compensation must be compensated, got %q", r.State)
	}
	if r.Outcome == workcase.OutcomeUnverified {
		t.Fatal("a compensated action is not a completed action and must not carry an unverified outcome")
	}
	final, _ := svc.Get(context.Background(), orchEnterprise, orchOrg, c.ID)
	if final.Status == sdkworkcase.StatusCompleted {
		t.Fatal("compensation must not complete the case")
	}
}

// TestOrchestratorCompensationFailure: if the compensation itself cannot be
// verified, the action escalates to human_takeover.
func TestOrchestratorCompensationFailure(t *testing.T) {
	plan := sdkworkcase.WorkPlan{Steps: []sdkworkcase.Step{writeStep("s1", "erp.approve", "compensate_then_human", strptr("erp.revert_approval"))}}
	svc, _, c := executingCase(t, plan)
	d := newDeps(t, svc)
	d.gateway.onDispatch = func(g *fakeGateway, req nexus.ActionRequest, _ int) (nexus.ActionReceipt, error) {
		isComp := len(req.IdempotencyKey) >= 9 && req.IdempotencyKey[:9] == "idem-comp"
		if isComp {
			// Compensation transport fails outright.
			return nexus.ActionReceipt{}, workcase.ErrTransportTimeout
		}
		return signReceipt(g.priv, g.keyID, receiptFor(req, resultFailed)), nil
	}
	res := prepareAndAdvance(t, d.orch(t), c.ID)
	if r, _ := runByStep(res.Ledger, "s1"); r.State != workcase.ActionHumanTakeover {
		t.Fatalf("a failed compensation must escalate to human_takeover, got %q", r.State)
	}
}

// TestOrchestratorHumanTakeover: a policy that demands human handling routes an
// action straight to human_takeover, leaving the case executing.
func TestOrchestratorHumanTakeover(t *testing.T) {
	plan := sdkworkcase.WorkPlan{Steps: []sdkworkcase.Step{writeStep("s1", "erp.approve", "human", nil)}}
	svc, _, c := executingCase(t, plan)
	d := newDeps(t, svc)
	// A verified receipt reports failure; the "human" failure policy escalates
	// immediately rather than retrying or compensating.
	d.gateway.onDispatch = func(g *fakeGateway, req nexus.ActionRequest, _ int) (nexus.ActionReceipt, error) {
		return signReceipt(g.priv, g.keyID, receiptFor(req, resultFailed)), nil
	}
	res := prepareAndAdvance(t, d.orch(t), c.ID)
	if r, _ := runByStep(res.Ledger, "s1"); r.State != workcase.ActionHumanTakeover {
		t.Fatalf("human failure policy must reach human_takeover, got %q", r.State)
	}
	final, _ := svc.Get(context.Background(), orchEnterprise, orchOrg, c.ID)
	if final.Status != sdkworkcase.StatusExecuting {
		t.Fatalf("human takeover leaves the case executing (a human resumes it), got %q", final.Status)
	}
}

// TestOrchestratorVerificationNeedsEnqueuedNotResolved proves needs are queued
// for Task 0G/0H and never turned into an Outcome here.
func TestOrchestratorVerificationNeedsEnqueuedNotResolved(t *testing.T) {
	plan := sdkworkcase.WorkPlan{Steps: []sdkworkcase.Step{writeStep("s1", "erp.approve", "retry_then_human", nil)}}
	svc, _, c := executingCase(t, plan)
	d := newDeps(t, svc)
	res := prepareAndAdvance(t, d.orch(t), c.ID)
	if len(res.Ledger.Verify) != 1 {
		t.Fatalf("expected one enqueued verification need, got %d", len(res.Ledger.Verify))
	}
	q := res.Ledger.Verify[0]
	if q.NeedID != "s1:need" || q.StepID != "s1" || q.ParameterHash == "" {
		t.Fatalf("enqueued need is not bound to its action: %+v", q)
	}
	// The completed action carries only an unverified outcome -- no satisfied
	// Outcome is produced in 0E.
	if r, _ := runByStep(res.Ledger, "s1"); r.Outcome != workcase.OutcomeUnverified {
		t.Fatalf("0E must leave the outcome unverified, got %q", r.Outcome)
	}
}

// TestOrchestratorPersistsDecisionsAndAuditRefs proves every state transition
// is recorded with an external audit reference.
func TestOrchestratorPersistsDecisionsAndAuditRefs(t *testing.T) {
	plan := sdkworkcase.WorkPlan{Steps: []sdkworkcase.Step{writeStep("s1", "erp.approve", "retry_then_human", nil)}}
	svc, _, c := executingCase(t, plan)
	d := newDeps(t, svc)
	res := prepareAndAdvance(t, d.orch(t), c.ID)
	if len(res.Ledger.Decisions) == 0 {
		t.Fatal("no decisions were persisted")
	}
	for _, dec := range res.Ledger.Decisions {
		if dec.AuditRef == "" {
			t.Fatalf("decision %+v has no external audit reference", dec)
		}
	}
	if len(d.audit.refs) == 0 {
		t.Fatal("the external audit sink was never called")
	}
}

// TestOrchestratorLLMProposesButCannotDecide: a Planner may propose a plan
// revision on replanning, but its output is validated and persisted only as a
// new proposed revision -- it can never set case status, complete the case, or
// manufacture a receipt.
func TestOrchestratorLLMProposesButCannotDecide(t *testing.T) {
	plan := sdkworkcase.WorkPlan{Steps: []sdkworkcase.Step{writeStep("s1", "erp.approve", "replan_then_human", nil)}}
	svc, _, c := executingCase(t, plan)
	d := newDeps(t, svc)
	// The replacement plan the LLM proposes: a single read step.
	d.planner = &fakePlanner{plan: sdkworkcase.WorkPlan{Steps: []sdkworkcase.Step{readStep("s-replanned", "mes.read")}}}
	// The primary write reports failure, triggering replanning.
	d.gateway.onDispatch = func(g *fakeGateway, req nexus.ActionRequest, _ int) (nexus.ActionReceipt, error) {
		if req.Capability == "erp.approve" {
			return signReceipt(g.priv, g.keyID, receiptFor(req, resultFailed)), nil
		}
		return signReceipt(g.priv, g.keyID, receiptFor(req, resultSucceeded)), nil
	}
	res := prepareAndAdvance(t, d.orch(t), c.ID)

	final, _ := svc.Get(context.Background(), orchEnterprise, orchOrg, c.ID)
	// The LLM's proposal is persisted as a new revision, but it never completes
	// the case.
	if final.Status == sdkworkcase.StatusCompleted {
		t.Fatal("an LLM plan proposal must never complete the WorkCase")
	}
	if len(final.Plans) < 2 {
		t.Fatalf("the proposed replan should appear as a new WorkPlan revision, got %d plan(s)", len(final.Plans))
	}
	if final.Plans[len(final.Plans)-1].Steps[0].ID != "s-replanned" {
		t.Fatalf("the newest plan revision is not the LLM proposal: %+v", final.Plans[len(final.Plans)-1])
	}
	// The replanned read step is then worked deterministically to completion.
	if r, ok := runByStep(res.Ledger, "s-replanned"); !ok || r.State != workcase.ActionCompleted {
		t.Fatalf("replanned step should be advanced deterministically, got %+v (ok=%v)", r, ok)
	}
}

// TestOrchestratorRejectsInvalidLLMPlan: a malformed LLM proposal is rejected
// (never persisted) and the action escalates to human_takeover.
func TestOrchestratorRejectsInvalidLLMPlan(t *testing.T) {
	plan := sdkworkcase.WorkPlan{Steps: []sdkworkcase.Step{writeStep("s1", "erp.approve", "replan_then_human", nil)}}
	svc, _, c := executingCase(t, plan)
	d := newDeps(t, svc)
	// An invalid plan: a write step with no failure policy (ValidatePlan rejects it).
	bad := sdkworkcase.WorkPlan{Steps: []sdkworkcase.Step{{ID: "bad", Action: &sdkworkcase.ActionSpec{Kind: "write", BusinessCapability: "erp.x", ParametersHash: "sha256:x", IdempotencyKey: "bad:v1"}}}}
	d.planner = &fakePlanner{plan: bad}
	d.gateway.onDispatch = func(g *fakeGateway, req nexus.ActionRequest, _ int) (nexus.ActionReceipt, error) {
		return signReceipt(g.priv, g.keyID, receiptFor(req, resultFailed)), nil
	}
	res := prepareAndAdvance(t, d.orch(t), c.ID)
	if r, _ := runByStep(res.Ledger, "s1"); r.State != workcase.ActionHumanTakeover {
		t.Fatalf("an invalid LLM plan must not be adopted; the action should await a human, got %q", r.State)
	}
	final, _ := svc.Get(context.Background(), orchEnterprise, orchOrg, c.ID)
	if len(final.Plans) != 1 {
		t.Fatalf("an invalid proposal must never be persisted as a revision, got %d plans", len(final.Plans))
	}
}

// TestRestartMidAction: an action whose result is unknown (a timed-out side
// effect the authority cannot yet confirm) rests mid-flight. A brand-new
// Orchestrator over the SAME persisted stores resumes and reconciles it to
// completion -- crucially, WITHOUT re-dispatching the side effect.
func TestRestartMidAction(t *testing.T) {
	plan := sdkworkcase.WorkPlan{Steps: []sdkworkcase.Step{writeStep("s1", "erp.approve", "retry_then_human", nil)}}
	svc, _, c := executingCase(t, plan)
	d := newDeps(t, svc)
	ctx := context.Background()

	authorityReady := false
	d.gateway.onDispatch = func(_ *fakeGateway, _ nexus.ActionRequest, _ int) (nexus.ActionReceipt, error) {
		// The dispatch times out: the write MAY have executed -> result unknown.
		return nexus.ActionReceipt{}, workcase.ErrTransportTimeout
	}
	d.gateway.onReconcile = func(g *fakeGateway, req nexus.ActionRequest, _ int) (nexus.ActionReceipt, bool, error) {
		if !authorityReady {
			return nexus.ActionReceipt{}, false, workcase.ErrTransportTimeout // authority not yet reachable
		}
		return signReceipt(g.priv, g.keyID, receiptFor(req, resultSucceeded)), true, nil
	}

	o1 := d.orch(t)
	if _, err := o1.Prepare(ctx, orchEnterprise, orchOrg, c.ID); err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	res1, err := o1.Advance(ctx, c.ID)
	if err != nil {
		t.Fatalf("Advance (before restart): %v", err)
	}
	if r, _ := runByStep(res1.Ledger, "s1"); r.State != workcase.ActionResultUnknown {
		t.Fatalf("orch1 must rest mid-action at result_unknown, got %q", r.State)
	}
	dispatchesBefore := d.gateway.dispatchTotal()
	if dispatchesBefore != 1 {
		t.Fatalf("exactly one dispatch should have occurred before restart, got %d", dispatchesBefore)
	}

	// "Restart": brand-new Orchestrator object over the SAME run store and work
	// store. The authority is now reachable.
	authorityReady = true
	o2 := d.orch(t)
	res, err := o2.Advance(ctx, c.ID)
	if err != nil {
		t.Fatalf("Advance (after restart): %v", err)
	}
	if r, _ := runByStep(res.Ledger, "s1"); r.State != workcase.ActionCompleted {
		t.Fatalf("restarted orchestrator must reconcile the in-flight action to completion, got %q", r.State)
	}
	if got := d.gateway.dispatchTotal(); got != dispatchesBefore {
		t.Fatalf("restart re-dispatched an in-flight side effect: before=%d after=%d", dispatchesBefore, got)
	}
}

// TestOutOfOrderReceipt: given the pull-based receipt model, this exercises
// redelivery/idempotency convergence rather than wire-level reordering. After a
// step completes, a re-Advance (redelivery) does not re-process it -- an
// already-ActionCompleted run is skipped by currentActionableRun (NOT a
// receipt-id-keyed dedup) -- so its recorded receipt id is stable and each step
// dispatches exactly once.
func TestOutOfOrderReceipt(t *testing.T) {
	plan := sdkworkcase.WorkPlan{Steps: []sdkworkcase.Step{
		writeStep("s1", "erp.approve", "retry_then_human", nil),
		writeStep("s2", "erp.post", "retry_then_human", nil),
	}}
	svc, _, c := executingCase(t, plan)
	d := newDeps(t, svc)
	ctx := context.Background()
	o := d.orch(t)
	if _, err := o.Prepare(ctx, orchEnterprise, orchOrg, c.ID); err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	res, err := o.Advance(ctx, c.ID)
	if err != nil {
		t.Fatalf("Advance: %v", err)
	}
	r1, _ := runByStep(res.Ledger, "s1")
	r2, _ := runByStep(res.Ledger, "s2")
	if r1.State != workcase.ActionCompleted || r2.State != workcase.ActionCompleted {
		t.Fatalf("both steps should complete, got s1=%q s2=%q", r1.State, r2.State)
	}
	firstReceiptS1 := r1.ReceiptID

	// Re-advance (a redelivery): the already-recorded receipts must not be
	// re-applied and the step receipt ids must be stable.
	res2, err := o.Advance(ctx, c.ID)
	if err != nil {
		t.Fatalf("re-Advance: %v", err)
	}
	r1b, _ := runByStep(res2.Ledger, "s1")
	if r1b.ReceiptID != firstReceiptS1 {
		t.Fatalf("receipt id changed on redelivery: %q -> %q", firstReceiptS1, r1b.ReceiptID)
	}
	if d.gateway.dispatchTotal() != 2 {
		t.Fatalf("each step must dispatch exactly once regardless of redelivery, got %d", d.gateway.dispatchTotal())
	}
}

// TestDuplicateNATSDelivery exercises idempotent handling over the tasks bus:
// a duplicated JetStream delivery of the advance job runs Advance again, but
// the side effect is dispatched exactly once.
func TestDuplicateNATSDelivery(t *testing.T) {
	plan := sdkworkcase.WorkPlan{Steps: []sdkworkcase.Step{writeStep("s1", "erp.approve", "retry_then_human", nil)}}
	svc, _, c := executingCase(t, plan)
	d := newDeps(t, svc)
	o := d.orch(t)
	ctx := context.Background()
	if _, err := o.Prepare(ctx, orchEnterprise, orchOrg, c.ID); err != nil {
		t.Fatalf("Prepare: %v", err)
	}

	bus := tasks.NewMemBus()
	runner := tasks.NewRunner(bus)
	// A claim that always admits the delivery, so the orchestrator's OWN
	// idempotency (not just the runner's claim) is what prevents a double side
	// effect under duplicate delivery.
	handler := tasks.WorkCaseAdvanceHandler(
		func(context.Context, string) (bool, error) { return true, nil },
		func(ctx context.Context, caseID string) error { _, err := o.Advance(ctx, caseID); return err },
		func(context.Context, string, error) error { return nil },
	)
	if err := runner.Register(tasks.JobWorkCaseAdvance, handler); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if err := runner.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer runner.Stop()

	// Duplicate delivery of the same job id.
	if err := bus.Publish(ctx, tasks.JobWorkCaseAdvance, c.ID); err != nil {
		t.Fatalf("publish 1: %v", err)
	}
	if err := bus.Publish(ctx, tasks.JobWorkCaseAdvance, c.ID); err != nil {
		t.Fatalf("publish 2: %v", err)
	}

	if got := d.gateway.dispatchTotal(); got != 1 {
		t.Fatalf("duplicate NATS delivery must dispatch the side effect exactly once, got %d", got)
	}
	ledger, err := d.runs.Load(ctx, c.ID)
	if err != nil {
		t.Fatalf("Load ledger: %v", err)
	}
	if r, _ := runByStep(ledger, "s1"); r.State != workcase.ActionCompleted {
		t.Fatalf("step must be completed after delivery, got %q", r.State)
	}
}

// --- POST-REVIEW: BLOCKER/MAJOR/MINOR fixes ---------------------------------

// TestConcurrentAdvanceDispatchesOnce (BLOCKER 1): two concurrent Advance calls
// on the same case must dispatch the side effect exactly once. Hardened to
// RELIABLY force the gate race: a rendezvous in the Governor holds BOTH Advances
// at the pre-gate point (after each has observed the run pending, before the
// pending->dispatched claim) and releases them together, so they contend the
// atomic gate simultaneously (the correct synchronization point -- a post-gate
// Dispatch delay would instead route the loser to the separate reconcile-rest
// defense and mask a graph-enforcement regression). The transition graph
// enforced inside the ledger transaction is the gate, NOT the external job
// claim. Must pass under -race with -count.
func TestConcurrentAdvanceDispatchesOnce(t *testing.T) {
	plan := sdkworkcase.WorkPlan{Steps: []sdkworkcase.Step{writeStep("s1", "erp.approve", "retry_then_human", nil)}}
	svc, _, c := executingCase(t, plan)
	d := newDeps(t, svc)
	arrived := make(chan struct{}, 8)
	release := make(chan struct{})
	d.gov.prepareHook = func() {
		arrived <- struct{}{}
		<-release
	}
	o := d.orch(t)
	ctx := context.Background()
	if _, err := o.Prepare(ctx, orchEnterprise, orchOrg, c.ID); err != nil {
		t.Fatalf("Prepare: %v", err)
	}

	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = o.Advance(ctx, c.ID)
		}()
	}
	// Both goroutines observe the run pending and block in the pre-gate
	// rendezvous (the first stays pending until the second arrives), then race
	// the gate together.
	<-arrived
	<-arrived
	close(release)
	wg.Wait()

	if got := d.gateway.dispatchTotal(); got != 1 {
		t.Fatalf("PRIMARY DOUBLE-DISPATCH: side effect dispatched %d times under concurrent Advance, want exactly 1", got)
	}
	ledger, err := d.runs.Load(ctx, c.ID)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	r, _ := runByStep(ledger, "s1")
	if r.Attempts != 1 {
		t.Fatalf("Attempts=%d, want 1 (a lost-update would double it)", r.Attempts)
	}
	if r.State != workcase.ActionCompleted {
		t.Fatalf("step must complete, got %q", r.State)
	}
	dispatchDecisions := 0
	for _, dec := range ledger.Decisions {
		if dec.To == workcase.ActionDispatched {
			dispatchDecisions++
		}
	}
	if dispatchDecisions != 1 {
		t.Fatalf("expected exactly one 'dispatched' decision, got %d", dispatchDecisions)
	}
}

// TestConcurrentCompensationDispatchesOnce (SURVIVING BLOCKER): the compensation
// REVERSAL side effect must also be dispatched exactly once under concurrent
// Advance -- a double reversal (revert/refund/cancel) is directly harmful. The
// widened window holds the run inside the compensation Dispatch while a second
// concurrent Advance observes it; only one Advance may drive the compensation.
// Fails on code lacking an atomic compensation claim (double dispatch).
func TestConcurrentCompensationDispatchesOnce(t *testing.T) {
	plan := sdkworkcase.WorkPlan{Steps: []sdkworkcase.Step{writeStep("s1", "erp.approve", "compensate_then_human", strptr("erp.revert_approval"))}}
	svc, _, c := executingCase(t, plan)
	d := newDeps(t, svc)
	compEntered := make(chan struct{}, 8)
	d.gateway.onDispatch = func(g *fakeGateway, req nexus.ActionRequest, _ int) (nexus.ActionReceipt, error) {
		if len(req.IdempotencyKey) >= 9 && req.IdempotencyKey[:9] == "idem-comp" {
			compEntered <- struct{}{}
			time.Sleep(60 * time.Millisecond) // widened window: hold at compensation dispatch
			return signReceipt(g.priv, g.keyID, receiptFor(req, resultSucceeded)), nil
		}
		// Primary action reports failure (fast) -> triggers the compensation.
		return signReceipt(g.priv, g.keyID, receiptFor(req, resultFailed)), nil
	}
	o := d.orch(t)
	ctx := context.Background()
	if _, err := o.Prepare(ctx, orchEnterprise, orchOrg, c.ID); err != nil {
		t.Fatalf("Prepare: %v", err)
	}

	var wg sync.WaitGroup
	wg.Add(1)
	go func() { defer wg.Done(); _, _ = o.Advance(ctx, c.ID) }() // G1: reaches compensation dispatch, blocks in the window
	<-compEntered                                                // G1 is now inside the compensation Dispatch
	wg.Add(1)
	go func() { defer wg.Done(); _, _ = o.Advance(ctx, c.ID) }() // G2: observes the in-flight compensation
	wg.Wait()

	if got := d.gateway.compDispatchTotal(); got != 1 {
		t.Fatalf("COMPENSATION DOUBLE-DISPATCH: compensation side effect dispatched %d times under concurrent Advance, want 1", got)
	}
	ledger, err := d.runs.Load(ctx, c.ID)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	r, _ := runByStep(ledger, "s1")
	if r.Attempts != 1 {
		t.Fatalf("primary Attempts=%d, want 1", r.Attempts)
	}
	if r.State != workcase.ActionCompensated {
		t.Fatalf("compensation must resolve to compensated, got %q", r.State)
	}
	compDecisions := 0
	for _, dec := range ledger.Decisions {
		if dec.To == workcase.ActionCompensationDispatched {
			compDecisions++
		}
	}
	if compDecisions != 1 {
		t.Fatalf("expected exactly one compensation-dispatch decision, got %d", compDecisions)
	}
	final, _ := svc.Get(ctx, orchEnterprise, orchOrg, c.ID)
	if final.Status == sdkworkcase.StatusCompleted {
		t.Fatal("compensation must not complete the case")
	}
}

// nonWriteSideEffectStep builds a side-effecting step whose Kind is a verb OTHER
// than the literal "write" (so the frozen ValidatePlan write-guard does not
// fire and a Kind-keyed gate would wrongly treat it as a safe read).
func nonWriteSideEffectStep(id, capability, kind string) sdkworkcase.Step {
	return sdkworkcase.Step{ID: id, Action: &sdkworkcase.ActionSpec{
		Kind: kind, BusinessCapability: capability, ParametersHash: "sha256:fixture-" + id,
		Risk: "low", IdempotencyKey: id + ":v1", FailurePolicy: "retry_then_human",
	}}
}

// TestNonWriteSideEffectStillRequiresApproval (BLOCKER 2a): a side-effecting
// step whose Kind is not "write" STILL requires upward review; under-approved it
// is held for a human and never dispatched.
func TestNonWriteSideEffectStillRequiresApproval(t *testing.T) {
	plan := sdkworkcase.WorkPlan{Steps: []sdkworkcase.Step{nonWriteSideEffectStep("s1", "erp.record.update", "update")}}
	svc, _, c := executingCase(t, plan)
	d := newDeps(t, svc)
	d.gov.partiesFor = func(_ sdkworkcase.Step, _ nexus.RiskLevel) (governance.ApprovalParties, error) {
		return governance.ApprovalParties{
			Submitter: governance.Party{UserID: "u-submitter", OrgUnitID: "org:team-a", OrgPath: []string{"org:root", "org:div"}, OrgVersion: orgVersion},
			Reviewers: []governance.Party{{UserID: "u-peer", OrgUnitID: "org:team-a", OrgPath: []string{"org:root", "org:div"}, OrgVersion: orgVersion}},
		}, nil
	}
	res := prepareAndAdvance(t, d.orch(t), c.ID)
	if r, _ := runByStep(res.Ledger, "s1"); r.State != workcase.ActionHumanTakeover {
		t.Fatalf("a non-write side effect must STILL require upward approval; got %q", r.State)
	}
	if d.gateway.dispatchTotal() != 0 {
		t.Fatalf("an unapproved non-write side effect must never dispatch, dispatches=%d", d.gateway.dispatchTotal())
	}
}

// TestNonWriteSideEffectTimeoutIsResultUnknown (BLOCKER 2b): a non-"write"
// side-effecting step that times out on dispatch maps to result_unknown (no
// blind re-dispatch), exactly like a write.
func TestNonWriteSideEffectTimeoutIsResultUnknown(t *testing.T) {
	plan := sdkworkcase.WorkPlan{Steps: []sdkworkcase.Step{nonWriteSideEffectStep("s1", "erp.record.post", "post")}}
	svc, _, c := executingCase(t, plan)
	d := newDeps(t, svc)
	d.gateway.onDispatch = func(_ *fakeGateway, _ nexus.ActionRequest, _ int) (nexus.ActionReceipt, error) {
		return nexus.ActionReceipt{}, workcase.ErrTransportTimeout
	}
	d.gateway.onReconcile = func(_ *fakeGateway, _ nexus.ActionRequest, _ int) (nexus.ActionReceipt, bool, error) {
		return nexus.ActionReceipt{}, false, workcase.ErrTransportTimeout // authority not yet reachable -> rest
	}
	res := prepareAndAdvance(t, d.orch(t), c.ID)
	if r, _ := runByStep(res.Ledger, "s1"); r.State != workcase.ActionResultUnknown {
		t.Fatalf("a non-write side effect timeout must map to result_unknown, got %q", r.State)
	}
	if d.gateway.dispatchTotal() != 1 {
		t.Fatalf("a timed-out non-write side effect must not be blindly re-dispatched, dispatches=%d", d.gateway.dispatchTotal())
	}
}

// TestReconcileFailedReceiptReplans (MAJOR 3): a side effect times out, then
// reconciliation finds it executed-and-FAILED; a replan_then_human policy
// legitimately drives result_unknown -> replanning (a now-enforced legal edge).
func TestReconcileFailedReceiptReplans(t *testing.T) {
	plan := sdkworkcase.WorkPlan{Steps: []sdkworkcase.Step{writeStep("s1", "erp.approve", "replan_then_human", nil)}}
	svc, _, c := executingCase(t, plan)
	d := newDeps(t, svc)
	d.planner = &fakePlanner{plan: sdkworkcase.WorkPlan{Steps: []sdkworkcase.Step{readStep("s-replanned", "mes.read")}}}
	d.gateway.onDispatch = func(g *fakeGateway, req nexus.ActionRequest, _ int) (nexus.ActionReceipt, error) {
		if req.Capability == "erp.approve" {
			return nexus.ActionReceipt{}, workcase.ErrTransportTimeout
		}
		return signReceipt(g.priv, g.keyID, receiptFor(req, resultSucceeded)), nil
	}
	d.gateway.onReconcile = func(g *fakeGateway, req nexus.ActionRequest, _ int) (nexus.ActionReceipt, bool, error) {
		// Found, but the connector reported failure -> replan policy from result_unknown.
		return signReceipt(g.priv, g.keyID, receiptFor(req, resultFailed)), true, nil
	}
	res := prepareAndAdvance(t, d.orch(t), c.ID)
	if r, ok := runByStep(res.Ledger, "s-replanned"); !ok || r.State != workcase.ActionCompleted {
		t.Fatalf("reconcile->failed->replan must adopt the new plan and complete it, got %+v (ok=%v)", r, ok)
	}
	final, _ := svc.Get(context.Background(), orchEnterprise, orchOrg, c.ID)
	if len(final.Plans) < 2 {
		t.Fatalf("replan should add a plan revision, got %d", len(final.Plans))
	}
	if final.Status == sdkworkcase.StatusCompleted {
		t.Fatal("replan must not complete the case")
	}
}

// TestReconcileLivenessEscalates (MINOR): a permanently-unreachable reconcile
// authority must not park result_unknown forever; after a bounded number of
// reconcile failures it escalates to human_takeover (never re-dispatching).
func TestReconcileLivenessEscalates(t *testing.T) {
	plan := sdkworkcase.WorkPlan{Steps: []sdkworkcase.Step{writeStep("s1", "erp.approve", "retry_then_human", nil)}}
	svc, _, c := executingCase(t, plan)
	d := newDeps(t, svc)
	d.gateway.onDispatch = func(_ *fakeGateway, _ nexus.ActionRequest, _ int) (nexus.ActionReceipt, error) {
		return nexus.ActionReceipt{}, workcase.ErrTransportTimeout
	}
	d.gateway.onReconcile = func(_ *fakeGateway, _ nexus.ActionRequest, _ int) (nexus.ActionReceipt, bool, error) {
		return nexus.ActionReceipt{}, false, workcase.ErrTransportTimeout // permanently unreachable
	}
	o := d.orch(t)
	ctx := context.Background()
	if _, err := o.Prepare(ctx, orchEnterprise, orchOrg, c.ID); err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	var last workcase.AdvanceResult
	for i := 0; i < 12; i++ {
		var err error
		last, err = o.Advance(ctx, c.ID)
		if err != nil {
			t.Fatalf("Advance %d: %v", i, err)
		}
		if r, _ := runByStep(last.Ledger, "s1"); r.State == workcase.ActionHumanTakeover {
			break
		}
	}
	if r, _ := runByStep(last.Ledger, "s1"); r.State != workcase.ActionHumanTakeover {
		t.Fatalf("a permanently-unreachable reconcile must eventually escalate to human_takeover, got %q", r.State)
	}
	if d.gateway.dispatchTotal() != 1 {
		t.Fatalf("liveness escalation must not re-dispatch the side effect, dispatches=%d", d.gateway.dispatchTotal())
	}
}

// TestHumanTakeoverReflectsStepFailed (MINOR): a dispatched step that escalates
// to human_takeover must reflect StepFailed on the WorkCase (so a consumer does
// not see it perpetually "running") WITHOUT driving the case terminal.
func TestHumanTakeoverReflectsStepFailed(t *testing.T) {
	plan := sdkworkcase.WorkPlan{Steps: []sdkworkcase.Step{writeStep("s1", "erp.approve", "human", nil)}}
	svc, _, c := executingCase(t, plan)
	d := newDeps(t, svc)
	d.gateway.onDispatch = func(g *fakeGateway, req nexus.ActionRequest, _ int) (nexus.ActionReceipt, error) {
		return signReceipt(g.priv, g.keyID, receiptFor(req, resultFailed)), nil
	}
	res := prepareAndAdvance(t, d.orch(t), c.ID)
	if r, _ := runByStep(res.Ledger, "s1"); r.State != workcase.ActionHumanTakeover {
		t.Fatalf("want human_takeover, got %q", r.State)
	}
	final, _ := svc.Get(context.Background(), orchEnterprise, orchOrg, c.ID)
	step := final.Plans[len(final.Plans)-1].Steps[0]
	if step.Status != sdkworkcase.StepFailed {
		t.Fatalf("a dispatched step that escalated must reflect StepFailed, got %q", step.Status)
	}
	if final.Status != sdkworkcase.StatusExecuting {
		t.Fatalf("reflecting StepFailed must NOT drive the case terminal; want executing, got %q", final.Status)
	}
}

// TestNewOrchestratorRejectsMisconfig (MINOR): fail-closed misconfigs (empty
// trust key, zero org version) are rejected at construction.
func TestNewOrchestratorRejectsMisconfig(t *testing.T) {
	svc, _, _ := executingCase(t, sdkworkcase.WorkPlan{Steps: []sdkworkcase.Step{readStep("s1", "mes.read")}})
	base := func() workcase.Config {
		pub, _ := newKey(t)
		return workcase.Config{
			Service: svc, Runs: workcase.NewMemoryRunStore(), Gateway: newGateway(nil, trustedKeyID),
			Governor: newGovernor(), TrustedKeyID: trustedKeyID, TrustedKey: pub,
			ApprovalPolicy: governance.UpwardReviewPolicy{CurrentOrgVersion: orgVersion},
		}
	}
	if _, err := workcase.NewOrchestrator(base()); err != nil {
		t.Fatalf("valid config rejected: %v", err)
	}
	noKey := base()
	noKey.TrustedKey = nil
	if _, err := workcase.NewOrchestrator(noKey); err == nil {
		t.Fatal("empty TrustedKey must be rejected (fail-closed receipt verification)")
	}
	zeroOrg := base()
	zeroOrg.ApprovalPolicy = governance.UpwardReviewPolicy{CurrentOrgVersion: 0}
	if _, err := workcase.NewOrchestrator(zeroOrg); err == nil {
		t.Fatal("zero CurrentOrgVersion must be rejected")
	}
}

// TestDispatchAmbiguousErrorSideEffectResultUnknown (MINOR coverage): a
// non-sentinel dispatch error on a side effect fails closed to result_unknown.
func TestDispatchAmbiguousErrorSideEffectResultUnknown(t *testing.T) {
	plan := sdkworkcase.WorkPlan{Steps: []sdkworkcase.Step{writeStep("s1", "erp.approve", "retry_then_human", nil)}}
	svc, _, c := executingCase(t, plan)
	d := newDeps(t, svc)
	d.gateway.onDispatch = func(_ *fakeGateway, _ nexus.ActionRequest, _ int) (nexus.ActionReceipt, error) {
		return nexus.ActionReceipt{}, errors.New("connection reset by peer")
	}
	d.gateway.onReconcile = func(_ *fakeGateway, _ nexus.ActionRequest, _ int) (nexus.ActionReceipt, bool, error) {
		return nexus.ActionReceipt{}, false, workcase.ErrTransportTimeout // rest at result_unknown
	}
	res := prepareAndAdvance(t, d.orch(t), c.ID)
	if r, _ := runByStep(res.Ledger, "s1"); r.State != workcase.ActionResultUnknown {
		t.Fatalf("an ambiguous dispatch error on a side effect must be result_unknown (fail-closed), got %q", r.State)
	}
}

// TestDispatchAmbiguousErrorSafeReadRetries (MINOR coverage): a non-sentinel
// dispatch error on a safe read is retryable.
func TestDispatchAmbiguousErrorSafeReadRetries(t *testing.T) {
	plan := sdkworkcase.WorkPlan{Steps: []sdkworkcase.Step{readStep("s1", "mes.read")}}
	svc, _, c := executingCase(t, plan)
	d := newDeps(t, svc)
	d.gateway.onDispatch = func(g *fakeGateway, req nexus.ActionRequest, n int) (nexus.ActionReceipt, error) {
		if n == 1 {
			return nexus.ActionReceipt{}, errors.New("connection reset by peer")
		}
		return signReceipt(g.priv, g.keyID, receiptFor(req, resultSucceeded)), nil
	}
	res := prepareAndAdvance(t, d.orch(t), c.ID)
	if r, _ := runByStep(res.Ledger, "s1"); r.State != workcase.ActionCompleted {
		t.Fatalf("an ambiguous dispatch error on a safe read must retry to completion, got %q", r.State)
	}
	if d.gateway.distinctKeys() != 1 || d.gateway.dispatchTotal() != 2 {
		t.Fatalf("safe-read retry must reuse one idempotency key across two dispatches, keys=%d total=%d", d.gateway.distinctKeys(), d.gateway.dispatchTotal())
	}
}
