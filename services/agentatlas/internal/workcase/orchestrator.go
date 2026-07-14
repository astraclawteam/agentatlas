package workcase

// orchestrator.go implements Task 0E: deterministic execution of a WorkCase's
// current WorkPlan with signed receipts, unknown-result reconciliation,
// idempotent retry, compensation and human takeover.
//
// The load-bearing safety properties (all proven by orchestrator_test.go):
//
//  1. WorkCase "completed" is unreachable here. A verified receipt completes an
//     Action STEP (Service.AdvanceStep, which never drives case Status) and
//     records the receipt's business outcome as OutcomeUnverified. Case
//     completion stays gated on a satisfied Outcome that Task 0H publishes.
//  2. Only a signature-verified ActionReceipt (nexus.VerifyActionReceipt,
//     fail-closed ed25519, trust key from Config) can complete a read/write
//     Action. An unsigned/forged/wrong-key receipt never advances a step.
//  3. A transport timeout is ActionResultUnknown, never success and never a
//     blind retry of a side effect; it is reconciled through an authoritative
//     observe/fetch path.
//  4. The LLM may only PROPOSE a plan (Planner). The orchestrator validates and
//     persists the proposal as a new revision; the model never mutates state,
//     sets a status, or manufactures a receipt.
//  5. Advancement is idempotent and convergent under adverse delivery
//     (duplicate delivery, redelivery, concurrent Advance, restart mid-action)
//     WITHOUT a double side effect. The mechanisms: (a) every run-state change
//     goes through the deterministic transition graph (CanTransitionTo)
//     enforced INSIDE the ledger transaction against the freshly-loaded state,
//     so a concurrent/duplicate advance that finds the run already past its
//     source state is a rejected no-op (this, not an external job claim, is the
//     correctness gate); (b) BOTH side-effecting dispatches are performed only
//     by the winner of an atomic claim -- the primary action by the winner of
//     pending/retrying -> dispatched, and the compensation reversal by the
//     winner of compensating -> compensation_dispatched -- with the claim
//     persisted BEFORE the side effect; (c) already-done steps are skipped by
//     currentActionableRun (ActionCompleted) and needs are de-duplicated by
//     verifyQueued; (d) a deterministic per-step idempotency key is reused
//     across retries so AgentNexus de-duplicates a re-dispatch. A run merely
//     OBSERVED mid-flight (a bare `dispatched` or `compensation_dispatched`)
//     is never re-dispatched -- it rests and escalates after bounded probes.
//  6. Declared VerificationNeeds are ENQUEUED for Task 0G/0H, never evaluated.
//  7. A side-effecting Action -- determined by TRUSTED authority (the Governor),
//     never the plan's open-vocabulary Kind label -- is gated by
//     governance.UpwardReviewPolicy before dispatch, and its transport timeout
//     is result_unknown (never a blind retry).

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	model "github.com/astraclawteam/agentatlas/sdk/go/governance"
	nexus "github.com/astraclawteam/agentatlas/sdk/go/nexus"
	sdkworkcase "github.com/astraclawteam/agentatlas/sdk/go/workcase"
	governance "github.com/astraclawteam/agentatlas/services/agentatlas/internal/governance"
)

// Sentinel errors distinguishing the transport outcomes an ActionGateway may
// report. They are the vocabulary the failure state machine keys off.
var (
	// ErrTransportTimeout: the dispatch call did not return a result in time.
	// The side effect MAY have executed -- the orchestrator must move to
	// ActionResultUnknown and reconcile, never assume success and never blindly
	// re-dispatch.
	ErrTransportTimeout = errors.New("workcase: action transport timeout; result unknown")
	// ErrActionRejected: AgentNexus authoritatively refused the action BEFORE
	// any side effect (e.g. a pre-execution guard). It is safe to re-dispatch
	// under the same idempotency key.
	ErrActionRejected = errors.New("workcase: action rejected before execution; safe to retry")
	// ErrRunNotFound: no run ledger exists for the case (Prepare was not
	// called).
	ErrRunNotFound = errors.New("workcase: orchestrator run ledger not found")
)

// ActionGateway is the orchestrator's dispatch/reconcile port toward
// AgentNexus. It transports the governed nexus.ActionRequest and returns the
// RAW signed ActionReceipt (or a transport error). The orchestrator NEVER
// trusts the gateway to have verified anything: every returned receipt is
// re-checked with nexus.VerifyActionReceipt.
type ActionGateway interface {
	// Dispatch requests execution of req. ErrTransportTimeout means the result
	// is unknown; ErrActionRejected means it provably did not execute.
	Dispatch(ctx context.Context, req nexus.ActionRequest) (nexus.ActionReceipt, error)
	// Reconcile answers, for an action whose result is unknown, whether an
	// authoritative signed execution receipt exists. found=false with a nil
	// error means AgentNexus has no record of execution (safe to re-dispatch).
	Reconcile(ctx context.Context, req nexus.ActionRequest) (receipt nexus.ActionReceipt, found bool, err error)
}

// Governor deterministically expands a WorkPlan Step into a fully-bound,
// governed nexus.ActionRequest plus the upward-review parties its approval
// references resolve to. All of this comes from persisted governance facts --
// the LLM never supplies request bindings, verification needs or approvals.
//
// PrepareAction also returns a TRUSTED sideEffecting determination derived from
// the business capability, NOT the plan's open-vocabulary (and LLM-influenceable)
// Kind label. The orchestrator keys both the upward-review approval gate and the
// timeout->result_unknown safety on this trusted flag, so an action mislabeled
// with a non-"write" Kind can never bypass approval or the no-blind-retry rule.
type Governor interface {
	PrepareAction(ctx context.Context, c sdkworkcase.WorkCase, planRevision uint64, step sdkworkcase.Step) (req nexus.ActionRequest, parties governance.ApprovalParties, sideEffecting bool, err error)
	// PrepareCompensation builds the governed request for a step's declared
	// compensation Action (step.Action.CompensationActionID must be set).
	PrepareCompensation(ctx context.Context, c sdkworkcase.WorkCase, planRevision uint64, step sdkworkcase.Step) (nexus.ActionRequest, error)
}

// Planner is the ONLY seam through which an LLM proposal can influence a
// WorkCase. Its output is advisory: the orchestrator validates it and persists
// it as a new proposed WorkPlan revision. It can never set status or produce a
// receipt.
type Planner interface {
	ProposePlan(ctx context.Context, c sdkworkcase.WorkCase) (sdkworkcase.WorkPlan, error)
}

// AuditSink records an external audit reference for a governance decision. It
// is optional; when nil the orchestrator derives a deterministic internal
// reference so every decision still carries one.
type AuditSink interface {
	RecordDecision(ctx context.Context, caseID string, decision DecisionRecord) (auditRef string, err error)
}

// DecisionRecord is one persisted, append-only orchestration decision: the
// per-step state transition, the reason, and the external audit reference that
// justifies it. The ledger's Decisions slice is the orchestrator's audit trail.
type DecisionRecord struct {
	Seq      int         `json:"seq"`
	StepID   string      `json:"step_id"`
	From     ActionState `json:"from"`
	To       ActionState `json:"to"`
	Reason   string      `json:"reason"`
	AuditRef string      `json:"audit_ref"`
	At       time.Time   `json:"at"`
}

// QueuedVerificationNeed is a declared VerificationNeed the orchestrator has
// ENQUEUED (never resolved) when a receipt completed its Action step. Task
// 0G/0H turn these into Outcomes; 0E only records them.
type QueuedVerificationNeed struct {
	StepID        string `json:"step_id"`
	ActionID      string `json:"action_id"`
	NeedID        string `json:"need_id"`
	Source        string `json:"source"`
	Authority     string `json:"authority"`
	ParameterHash string `json:"parameter_hash"`
}

// ActionRun is the persistent execution state of one WorkPlan Step's Action.
type ActionRun struct {
	StepID   string      `json:"step_id"`
	ActionID string      `json:"action_id"`
	Kind     string      `json:"kind"`
	Risk     string      `json:"risk"`
	State    ActionState `json:"state"`
	// Attempts counts dispatches; ReconcileAttempts counts reconcile probes of a
	// result_unknown run. Both are bounded by Config.MaxAttempts so neither a
	// retry loop nor an unreachable reconcile authority parks a run forever.
	Attempts          int           `json:"attempts"`
	ReconcileAttempts int           `json:"reconcile_attempts"`
	IdempotencyKey    string        `json:"idempotency_key"`
	ReceiptID         string        `json:"receipt_id,omitempty"`
	Outcome           ActionOutcome `json:"outcome,omitempty"`
}

// RunLedger is the orchestrator's per-case persistent aggregate: the tracked
// plan revision, per-step ActionRuns, the append-only decision log, the
// enqueued verification needs and a replan counter. It is DISTINCT from the
// WorkCase aggregate (which cannot express these fine-grained facts) and never
// carries connector data.
type RunLedger struct {
	CaseID       string                   `json:"case_id"`
	EnterpriseID string                   `json:"enterprise_id"`
	OrgScope     string                   `json:"org_scope"`
	ActorRef     string                   `json:"actor_ref"`
	Revision     uint64                   `json:"revision"`
	PlanRevision uint64                   `json:"plan_revision"`
	Replans      int                      `json:"replans"`
	Runs         []ActionRun              `json:"runs"`
	Decisions    []DecisionRecord         `json:"decisions"`
	Verify       []QueuedVerificationNeed `json:"verify"`
}

func cloneLedger(l RunLedger) RunLedger {
	out := l
	out.Runs = append([]ActionRun(nil), l.Runs...)
	out.Decisions = append([]DecisionRecord(nil), l.Decisions...)
	out.Verify = append([]QueuedVerificationNeed(nil), l.Verify...)
	return out
}

// RunStore persists RunLedgers. Load returns ErrRunNotFound for an absent case;
// Save upserts; Transform atomically loads, applies fn and persists. MemoryRunStore
// serializes Transform, so fn is invoked exactly once per call.
type RunStore interface {
	Load(ctx context.Context, caseID string) (RunLedger, error)
	Save(ctx context.Context, ledger RunLedger) error
	Transform(ctx context.Context, caseID string, fn func(RunLedger) (RunLedger, error)) (RunLedger, error)
}

// MemoryRunStore is an in-memory RunStore for unit tests and single-process
// runs. It survives a simulated "restart" (a new Orchestrator over the same
// store) because the store, not the Orchestrator object, holds the ledger.
// Durable (PostgreSQL) persistence of the run ledger is deferred to a later
// task; the RunStore port keeps that swap local.
type MemoryRunStore struct {
	mu      sync.Mutex
	ledgers map[string]RunLedger
}

// NewMemoryRunStore returns an empty MemoryRunStore.
func NewMemoryRunStore() *MemoryRunStore {
	return &MemoryRunStore{ledgers: map[string]RunLedger{}}
}

func (s *MemoryRunStore) Load(_ context.Context, caseID string) (RunLedger, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	l, ok := s.ledgers[caseID]
	if !ok {
		return RunLedger{}, ErrRunNotFound
	}
	return cloneLedger(l), nil
}

func (s *MemoryRunStore) Save(_ context.Context, ledger RunLedger) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	stored := cloneLedger(ledger)
	stored.Revision++
	s.ledgers[ledger.CaseID] = stored
	return nil
}

func (s *MemoryRunStore) Transform(_ context.Context, caseID string, fn func(RunLedger) (RunLedger, error)) (RunLedger, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	l, ok := s.ledgers[caseID]
	if !ok {
		return RunLedger{}, ErrRunNotFound
	}
	next, err := fn(cloneLedger(l))
	if err != nil {
		return RunLedger{}, err
	}
	stored := cloneLedger(next)
	stored.Revision = l.Revision + 1
	s.ledgers[caseID] = stored
	return cloneLedger(stored), nil
}

// Config wires an Orchestrator. Service, Runs, Gateway and Governor are
// required; TrustedKey must be set for any receipt to verify (fail-closed).
type Config struct {
	Service        *Service
	Runs           RunStore
	Gateway        ActionGateway
	Governor       Governor
	TrustedKeyID   string
	TrustedKey     ed25519.PublicKey
	ApprovalPolicy governance.UpwardReviewPolicy
	Audit          AuditSink // optional
	Planner        Planner   // optional
	Now            func() time.Time
	MaxAttempts    int // per-step dispatch/retry bound before human takeover; default 3
}

// Orchestrator deterministically advances WorkCases. It holds no per-case state
// of its own: every fact lives in the RunStore (its ledger) or the WorkCase
// Service, so a fresh Orchestrator resumes exactly where a crashed one left off.
type Orchestrator struct {
	svc         *Service
	runs        RunStore
	gateway     ActionGateway
	gov         Governor
	trustKeyID  string
	trustKey    ed25519.PublicKey
	policy      governance.UpwardReviewPolicy
	audit       AuditSink
	planner     Planner
	now         func() time.Time
	maxAttempts int
}

// NewOrchestrator validates cfg and constructs an Orchestrator. It fails closed
// on the two silent misconfigs that would otherwise make every side effect
// park/escalate: an absent/short receipt trust key, and an unset authoritative
// org version for the upward-review policy.
func NewOrchestrator(cfg Config) (*Orchestrator, error) {
	if cfg.Service == nil || cfg.Runs == nil || cfg.Gateway == nil || cfg.Governor == nil {
		return nil, errors.New("workcase: orchestrator requires Service, Runs, Gateway and Governor")
	}
	if len(cfg.TrustedKey) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("workcase: orchestrator requires a %d-byte ed25519 TrustedKey for fail-closed receipt verification, got %d", ed25519.PublicKeySize, len(cfg.TrustedKey))
	}
	if cfg.ApprovalPolicy.CurrentOrgVersion <= 0 {
		return nil, errors.New("workcase: orchestrator requires ApprovalPolicy.CurrentOrgVersion > 0 (authoritative org version for upward review)")
	}
	now := cfg.Now
	if now == nil {
		now = time.Now
	}
	max := cfg.MaxAttempts
	if max <= 0 {
		max = 3
	}
	return &Orchestrator{
		svc:         cfg.Service,
		runs:        cfg.Runs,
		gateway:     cfg.Gateway,
		gov:         cfg.Governor,
		trustKeyID:  cfg.TrustedKeyID,
		trustKey:    cfg.TrustedKey,
		policy:      cfg.ApprovalPolicy,
		audit:       cfg.Audit,
		planner:     cfg.Planner,
		now:         now,
		maxAttempts: max,
	}, nil
}

// AdvanceResult summarizes one Advance call.
type AdvanceResult struct {
	CaseID     string
	Progressed bool
	Quiescent  bool
	Ledger     RunLedger
}

// Prepare seeds (idempotently) the run ledger for caseID from the case's
// current top WorkPlan revision. It records the tenant so subsequent
// Advance(ctx, caseID) calls can resolve the case without re-supplying scope.
// An existing ledger is returned unchanged (progress is never reset here;
// plan-revision adoption happens in Advance).
func (o *Orchestrator) Prepare(ctx context.Context, enterpriseID, orgScope, caseID string) (RunLedger, error) {
	if existing, err := o.runs.Load(ctx, caseID); err == nil {
		return existing, nil
	} else if !errors.Is(err, ErrRunNotFound) {
		return RunLedger{}, err
	}
	wc, err := o.svc.Get(ctx, enterpriseID, orgScope, caseID)
	if err != nil {
		return RunLedger{}, err
	}
	ledger := RunLedger{
		CaseID:       caseID,
		EnterpriseID: enterpriseID,
		OrgScope:     orgScope,
		ActorRef:     wc.ActorRef,
	}
	ledger = seedRuns(ledger, wc)
	if err := o.runs.Save(ctx, ledger); err != nil {
		return RunLedger{}, err
	}
	return o.runs.Load(ctx, caseID)
}

// seedRuns (re)builds the per-step ActionRuns for the case's current top plan
// revision, preserving ledger-level Decisions/Verify. A Step with no Action is
// a human checkpoint and rests at ActionHumanTakeover.
func seedRuns(ledger RunLedger, wc sdkworkcase.WorkCase) RunLedger {
	planRev, steps := topPlan(wc)
	runs := make([]ActionRun, 0, len(steps))
	for _, st := range steps {
		run := ActionRun{StepID: st.ID, ActionID: wc.ID + "::" + st.ID, State: ActionPending}
		if st.Action == nil {
			run.State = ActionHumanTakeover
		} else {
			run.Kind = st.Action.Kind
			run.Risk = st.Action.Risk
		}
		runs = append(runs, run)
	}
	ledger.PlanRevision = planRev
	ledger.Runs = runs
	return ledger
}

func topPlan(wc sdkworkcase.WorkCase) (uint64, []sdkworkcase.Step) {
	if len(wc.Plans) == 0 {
		return 0, nil
	}
	p := wc.Plans[len(wc.Plans)-1]
	return p.Revision, p.Steps
}

// Advance drives the case's current WorkPlan forward as far as it
// deterministically can, persisting every transition. It is safe to call
// repeatedly (idempotent) and from a fresh Orchestrator (restart-safe).
func (o *Orchestrator) Advance(ctx context.Context, caseID string) (AdvanceResult, error) {
	ledger, err := o.runs.Load(ctx, caseID)
	if err != nil {
		return AdvanceResult{}, err
	}

	progressed := false
	// Bound the loop: each step needs at most a few transitions per attempt.
	maxIters := (len(ledger.Runs)+1)*(o.maxAttempts+6) + 16
	for i := 0; i < maxIters; i++ {
		wc, err := o.svc.Get(ctx, ledger.EnterpriseID, ledger.OrgScope, caseID)
		if err != nil {
			return AdvanceResult{}, err
		}
		if wc.Status != sdkworkcase.StatusExecuting {
			break // only executing cases are driven here
		}
		ledger, err = o.syncPlan(ctx, wc)
		if err != nil {
			return AdvanceResult{}, err
		}
		run, ok := currentActionableRun(ledger)
		if !ok {
			break // every step completed -- but the WorkCase stays executing (0E)
		}
		did, err := o.stepOnce(ctx, wc, ledger, run)
		if err != nil {
			return AdvanceResult{}, err
		}
		if !did {
			break // resting: needs external input (human / later task)
		}
		progressed = true
		ledger, err = o.runs.Load(ctx, caseID)
		if err != nil {
			return AdvanceResult{}, err
		}
	}

	final, err := o.runs.Load(ctx, caseID)
	if err != nil {
		return AdvanceResult{}, err
	}
	return AdvanceResult{CaseID: caseID, Progressed: progressed, Quiescent: isQuiescent(final), Ledger: final}, nil
}

// syncPlan adopts a newer top WorkPlan revision (produced by a replan) by
// re-seeding the run set to pending for the new plan, preserving the ledger's
// audit trail. When the ledger already tracks the top revision it returns the
// current ledger WITHOUT a store write, so Revision stays a meaningful change
// counter and Advance does not amplify writes each loop iteration.
func (o *Orchestrator) syncPlan(ctx context.Context, wc sdkworkcase.WorkCase) (RunLedger, error) {
	planRev, _ := topPlan(wc)
	cur, err := o.runs.Load(ctx, wc.ID)
	if err != nil {
		return RunLedger{}, err
	}
	if cur.PlanRevision == planRev && len(cur.Runs) > 0 {
		return cur, nil // already synced: no write
	}
	return o.runs.Transform(ctx, wc.ID, func(l RunLedger) (RunLedger, error) {
		if l.PlanRevision == planRev && len(l.Runs) > 0 {
			return l, nil
		}
		return seedRuns(l, wc), nil
	})
}

// currentActionableRun returns the first run that is not yet completed. The
// caller decides (via stepOnce) whether that run can be driven or is resting.
func currentActionableRun(l RunLedger) (ActionRun, bool) {
	for _, r := range l.Runs {
		if r.State != ActionCompleted {
			return r, true
		}
	}
	return ActionRun{}, false
}

// isQuiescent reports whether no run can be driven further without external
// input: every run is completed, or the first non-completed run is resting.
func isQuiescent(l RunLedger) bool {
	run, ok := currentActionableRun(l)
	if !ok {
		return true
	}
	switch run.State {
	case ActionCompleted, ActionCompensated, ActionHumanTakeover, ActionTerminated:
		return true
	}
	return false
}

// stepOnce advances the given run by one deterministic unit of work. It returns
// did=true if it changed state (the caller re-loops) and did=false if the run
// is resting/terminal.
func (o *Orchestrator) stepOnce(ctx context.Context, wc sdkworkcase.WorkCase, ledger RunLedger, run ActionRun) (bool, error) {
	switch run.State {
	case ActionCompleted, ActionCompensated, ActionHumanTakeover, ActionTerminated:
		return false, nil
	case ActionPending, ActionRetrying:
		// dispatchStep returns did=false when it loses the atomic dispatch gate
		// to a concurrent Advance -- the loser stops here rather than proceeding.
		return o.dispatchStep(ctx, wc, ledger, run)
	case ActionDispatched, ActionResultUnknown:
		// A run persisted at dispatched means a prior crash between recording
		// the dispatch and its outcome: reconcile rather than re-dispatch. An
		// unresolved result_unknown rests here until reconciliation succeeds, so
		// a restarted Orchestrator resumes exactly here.
		return o.reconcileStep(ctx, wc, ledger, run)
	case ActionCompensating:
		return o.compensateStep(ctx, wc, ledger, run)
	case ActionCompensationDispatched:
		// The compensation reversal was atomically claimed and is in flight (or
		// crash-left). A concurrent/fresh Advance rests here -- never a second
		// reversal dispatch.
		return o.compensationDispatchedRest(ctx, ledger, run)
	case ActionReplanning:
		return o.replanStep(ctx, wc, ledger, run)
	default:
		return false, fmt.Errorf("workcase: unhandled action state %q", run.State)
	}
}

func (o *Orchestrator) stepByID(wc sdkworkcase.WorkCase, stepID string) (sdkworkcase.Step, bool) {
	_, steps := topPlan(wc)
	for _, st := range steps {
		if st.ID == stepID {
			return st, true
		}
	}
	return sdkworkcase.Step{}, false
}

// dispatchStep gates a side effect through the upward-review policy, atomically
// claims the dispatch, performs it, and applies the verified outcome. It returns
// did=false when it LOSES the atomic dispatch gate to a concurrent Advance --
// the loser performs no side effect and stops.
func (o *Orchestrator) dispatchStep(ctx context.Context, wc sdkworkcase.WorkCase, ledger RunLedger, run ActionRun) (bool, error) {
	step, ok := o.stepByID(wc, run.StepID)
	if !ok || step.Action == nil {
		_, err := o.transition(ctx, ledger.CaseID, run.StepID, ActionHumanTakeover, "step has no dispatchable action", nil)
		return true, err
	}
	req, parties, sideEffecting, err := o.gov.PrepareAction(ctx, wc, ledger.PlanRevision, step)
	if err != nil {
		_, e := o.transition(ctx, ledger.CaseID, run.StepID, ActionHumanTakeover, "governor could not prepare action", nil)
		return true, e
	}

	// Approval gate keyed on the Governor's TRUSTED side-effect determination,
	// NEVER the plan's open-vocabulary Kind label: a side effect must satisfy the
	// UpwardReviewPolicy before it is ever dispatched.
	if sideEffecting {
		if perr := o.policy.Evaluate(parties, govRisk(req.Risk)); perr != nil {
			_, e := o.transition(ctx, ledger.CaseID, run.StepID, ActionHumanTakeover, "upward review not satisfied: "+perr.Error(), nil)
			return true, e
		}
	}

	// ATOMIC DISPATCH GATE: record the dispatch (attempt + idempotency key)
	// BEFORE the side effect. Because the transition graph is enforced against
	// the freshly-loaded state inside the ledger transaction, only ONE concurrent
	// Advance wins pending/retrying -> dispatched; a loser is rejected here and
	// must not dispatch.
	applied, err := o.transitionWith(ctx, ledger.CaseID, run.StepID, ActionDispatched, "dispatching action", func(l *RunLedger, i int) {
		l.Runs[i].Attempts++
		l.Runs[i].IdempotencyKey = req.IdempotencyKey
	})
	if err != nil {
		return true, err
	}
	if !applied {
		return false, nil // another Advance owns this run; do not dispatch
	}

	if err := o.markStepRunning(ctx, ledger, run.StepID); err != nil {
		return true, err
	}
	receipt, derr := o.gateway.Dispatch(ctx, req)
	if derr != nil {
		return true, o.handleDispatchError(ctx, ledger, run.StepID, sideEffecting, derr)
	}
	return true, o.applyReceipt(ctx, wc, ledger, run.StepID, step, req, receipt)
}

// handleDispatchError maps a transport error to a safe next state, keyed on the
// TRUSTED sideEffecting flag (not the plan's Kind label).
func (o *Orchestrator) handleDispatchError(ctx context.Context, ledger RunLedger, stepID string, sideEffecting bool, derr error) error {
	switch {
	case errors.Is(derr, ErrActionRejected):
		// Provably not executed -> safe to re-dispatch under the same key.
		return o.retryOrEscalate(ctx, ledger.CaseID, stepID, "action rejected before execution")
	case errors.Is(derr, ErrTransportTimeout):
		if sideEffecting {
			// Side effect MAY have run: unknown, reconcile -- never blind retry.
			_, err := o.transition(ctx, ledger.CaseID, stepID, ActionResultUnknown, "transport timeout on side effect", nil)
			return err
		}
		// Reads are side-effect free: a timeout is safely retryable.
		return o.retryOrEscalate(ctx, ledger.CaseID, stepID, "read timeout")
	default:
		if sideEffecting {
			// Fail closed: an ambiguous error on a side effect is result_unknown.
			_, err := o.transition(ctx, ledger.CaseID, stepID, ActionResultUnknown, "ambiguous dispatch error on side effect: "+derr.Error(), nil)
			return err
		}
		return o.retryOrEscalate(ctx, ledger.CaseID, stepID, "dispatch error on read: "+derr.Error())
	}
}

// applyReceipt verifies a receipt and applies its outcome. An unverifiable
// receipt is an untrusted claim -> ActionResultUnknown (reconcile via a trusted
// path), never a completion.
func (o *Orchestrator) applyReceipt(ctx context.Context, wc sdkworkcase.WorkCase, ledger RunLedger, stepID string, step sdkworkcase.Step, req nexus.ActionRequest, receipt nexus.ActionReceipt) error {
	if err := nexus.VerifyActionReceipt(req, receipt, o.trustKeyID, o.trustKey); err != nil {
		_, e := o.transition(ctx, ledger.CaseID, stepID, ActionResultUnknown, "receipt failed verification: "+err.Error(), nil)
		return e
	}
	if receipt.ResultCode == "succeeded" {
		return o.completeStep(ctx, ledger, stepID, req, receipt)
	}
	// A verified receipt reporting a non-success result: the connector ran and
	// reported failure -> apply the declared failure policy.
	return o.applyFailurePolicy(ctx, ledger, stepID, step, "receipt reported result_code="+receipt.ResultCode)
}

// reconcileStep resolves an ActionResultUnknown (or crash-left dispatched) run
// through the authoritative reconcile path. It returns did=false (resting, no
// state change) when the reconcile authority is momentarily unreachable: an
// unknown result is never guessed, so the run simply waits at result_unknown
// for a later Advance (or a restarted Orchestrator) to resolve it.
func (o *Orchestrator) reconcileStep(ctx context.Context, wc sdkworkcase.WorkCase, ledger RunLedger, run ActionRun) (bool, error) {
	step, ok := o.stepByID(wc, run.StepID)
	if !ok || step.Action == nil {
		_, err := o.transition(ctx, ledger.CaseID, run.StepID, ActionHumanTakeover, "step has no action to reconcile", nil)
		return true, err
	}
	req, _, _, err := o.gov.PrepareAction(ctx, wc, ledger.PlanRevision, step)
	if err != nil {
		_, e := o.transition(ctx, ledger.CaseID, run.StepID, ActionHumanTakeover, "governor could not prepare action for reconcile", nil)
		return true, e
	}
	receipt, found, rerr := o.gateway.Reconcile(ctx, req)
	if rerr != nil {
		if errors.Is(rerr, ErrTransportTimeout) {
			// The authority is unreachable: never guess. Bump a DISTINCT
			// reconcile-attempt counter (dispatch attempts do not) and escalate
			// once bounded reconcile probes are exhausted, so an unreachable
			// authority cannot park result_unknown forever.
			exhausted, terr := o.bumpReconcile(ctx, ledger.CaseID, run.StepID)
			if terr != nil {
				return true, terr
			}
			if exhausted {
				_, e := o.transition(ctx, ledger.CaseID, run.StepID, ActionHumanTakeover, "reconcile authority unreachable; reconcile attempts exhausted", nil)
				return true, e
			}
			return false, nil // rest at result_unknown; await a later Advance
		}
		_, e := o.transition(ctx, ledger.CaseID, run.StepID, ActionHumanTakeover, "reconcile failed: "+rerr.Error(), nil)
		return true, e
	}
	if !found {
		if run.State == ActionResultUnknown {
			// MY dispatch timed out and the authority confirms non-execution:
			// safe to re-dispatch under the same idempotency key.
			return true, o.retryOrEscalate(ctx, ledger.CaseID, run.StepID, "reconcile found no execution; safe retry")
		}
		// A run merely found at `dispatched` (a crash-left claim OR a CONCURRENT
		// Advance's in-flight dispatch) whose non-execution we cannot safely
		// attribute must NEVER be blindly re-dispatched -- that is the concurrent
		// double-dispatch hole. Rest and escalate after bounded probes; a human
		// resolves a genuinely stuck dispatch.
		exhausted, terr := o.bumpReconcile(ctx, ledger.CaseID, run.StepID)
		if terr != nil {
			return true, terr
		}
		if exhausted {
			_, e := o.transition(ctx, ledger.CaseID, run.StepID, ActionHumanTakeover, "dispatched run unresolved; not safe to auto-retry", nil)
			return true, e
		}
		return false, nil
	}
	if err := nexus.VerifyActionReceipt(req, receipt, o.trustKeyID, o.trustKey); err != nil {
		_, e := o.transition(ctx, ledger.CaseID, run.StepID, ActionHumanTakeover, "reconciled receipt failed verification: "+err.Error(), nil)
		return true, e
	}
	if receipt.ResultCode == "succeeded" {
		return true, o.completeStep(ctx, ledger, run.StepID, req, receipt)
	}
	return true, o.applyFailurePolicy(ctx, ledger, run.StepID, step, "reconciled receipt reported result_code="+receipt.ResultCode)
}

// bumpReconcile increments the reconcile-attempt counter for a run and reports
// whether the bounded budget is now exhausted. It is a bare ledger write (no
// state transition, no decision): a rest is not a decision.
func (o *Orchestrator) bumpReconcile(ctx context.Context, caseID, stepID string) (bool, error) {
	exhausted := false
	_, err := o.runs.Transform(ctx, caseID, func(l RunLedger) (RunLedger, error) {
		i := indexRun(l, stepID)
		if i < 0 {
			return l, ErrRunNotFound
		}
		l.Runs[i].ReconcileAttempts++
		exhausted = l.Runs[i].ReconcileAttempts >= o.maxAttempts
		return l, nil
	})
	return exhausted, err
}

// applyFailurePolicy routes a technically-failed (but executed) side effect
// according to its declared FailurePolicy token.
func (o *Orchestrator) applyFailurePolicy(ctx context.Context, ledger RunLedger, stepID string, step sdkworkcase.Step, reason string) error {
	policy := ""
	if step.Action != nil {
		policy = step.Action.FailurePolicy
	}
	switch {
	case strings.HasPrefix(policy, "compensate"):
		if step.Action == nil || step.Action.CompensationActionID == nil {
			_, err := o.transition(ctx, ledger.CaseID, stepID, ActionHumanTakeover, reason+"; compensate policy but no compensation action", nil)
			return err
		}
		_, err := o.transition(ctx, ledger.CaseID, stepID, ActionCompensating, reason+"; compensating", nil)
		return err
	case strings.HasPrefix(policy, "replan"):
		_, err := o.transition(ctx, ledger.CaseID, stepID, ActionReplanning, reason+"; replanning", nil)
		return err
	case strings.HasPrefix(policy, "retry"):
		return o.retryOrEscalate(ctx, ledger.CaseID, stepID, reason+"; retrying")
	default:
		_, err := o.transition(ctx, ledger.CaseID, stepID, ActionHumanTakeover, reason+"; human takeover", nil)
		return err
	}
}

// retryOrEscalate re-dispatches (ActionRetrying) when attempts remain, else
// escalates to a human. The retry re-uses the same deterministic idempotency
// key, so AgentNexus deduplicates a re-dispatched side effect.
func (o *Orchestrator) retryOrEscalate(ctx context.Context, caseID, stepID, reason string) error {
	ledger, err := o.runs.Load(ctx, caseID)
	if err != nil {
		return err
	}
	run, ok := runByID(ledger, stepID)
	if !ok {
		return ErrRunNotFound
	}
	if run.Attempts >= o.maxAttempts {
		_, e := o.transition(ctx, caseID, stepID, ActionHumanTakeover, reason+"; attempts exhausted", nil)
		return e
	}
	_, e := o.transition(ctx, caseID, stepID, ActionRetrying, reason, nil)
	return e
}

// completeStep records a verified, successful receipt: it reflects the Step as
// completed in the WorkCase (WITHOUT completing the case), marks the run
// completed with an unverified business outcome, records the receipt id, and
// ENQUEUES the declared verification needs for Task 0G/0H.
func (o *Orchestrator) completeStep(ctx context.Context, ledger RunLedger, stepID string, req nexus.ActionRequest, receipt nexus.ActionReceipt) error {
	ev := sdkworkcase.EvidenceRef{Handle: receipt.ReceiptID, ContentHash: receipt.ParameterHash, Authority: receipt.ExecutorIdentity}
	if err := o.markStepCompleted(ctx, ledger, stepID, ev); err != nil {
		return err
	}
	needs := req.VerificationNeeds
	actionID := req.ActionID
	paramHash := req.ParameterHash
	_, err := o.transitionWith(ctx, ledger.CaseID, stepID, ActionCompleted, "verified receipt "+receipt.ReceiptID, func(l *RunLedger, i int) {
		l.Runs[i].ReceiptID = receipt.ReceiptID
		l.Runs[i].Outcome = OutcomeUnverified
		for _, n := range needs {
			if verifyQueued(*l, stepID, n.NeedID) {
				continue
			}
			l.Verify = append(l.Verify, QueuedVerificationNeed{
				StepID: stepID, ActionID: actionID, NeedID: n.NeedID,
				Source: n.Source, Authority: n.Authority, ParameterHash: paramHash,
			})
		}
	})
	return err
}

func verifyQueued(l RunLedger, stepID, needID string) bool {
	for _, q := range l.Verify {
		if q.StepID == stepID && q.NeedID == needID {
			return true
		}
	}
	return false
}

// compensateStep runs a step's declared compensation (reversal) Action under a
// verified receipt. It applies the SAME atomic-claim pattern as dispatchStep:
// it wins compensating -> compensation_dispatched inside the ledger transaction
// BEFORE the reversal side effect, and dispatches only if it won. A concurrent
// Advance that observes the run still at compensating and loses the claim (or
// observes it already at compensation_dispatched) never re-dispatches the
// reversal -- a double reversal (revert/refund/cancel) would be directly
// harmful.
func (o *Orchestrator) compensateStep(ctx context.Context, wc sdkworkcase.WorkCase, ledger RunLedger, run ActionRun) (bool, error) {
	step, ok := o.stepByID(wc, run.StepID)
	if !ok || step.Action == nil || step.Action.CompensationActionID == nil {
		_, err := o.transition(ctx, ledger.CaseID, run.StepID, ActionHumanTakeover, "no compensation action to run", nil)
		return true, err
	}
	req, err := o.gov.PrepareCompensation(ctx, wc, ledger.PlanRevision, step)
	if err != nil {
		_, e := o.transition(ctx, ledger.CaseID, run.StepID, ActionHumanTakeover, "governor could not prepare compensation", nil)
		return true, e
	}
	// ATOMIC COMPENSATION CLAIM: only one Advance wins compensating ->
	// compensation_dispatched; a loser is a no-op and does NOT dispatch.
	applied, err := o.transition(ctx, ledger.CaseID, run.StepID, ActionCompensationDispatched, "dispatching compensation", nil)
	if err != nil {
		return true, err
	}
	if !applied {
		return false, nil // another Advance owns the compensation; do not dispatch
	}
	receipt, derr := o.gateway.Dispatch(ctx, req)
	if derr != nil {
		_, e := o.transition(ctx, ledger.CaseID, run.StepID, ActionHumanTakeover, "compensation dispatch failed: "+derr.Error(), nil)
		return true, e
	}
	if err := nexus.VerifyActionReceipt(req, receipt, o.trustKeyID, o.trustKey); err != nil {
		_, e := o.transition(ctx, ledger.CaseID, run.StepID, ActionHumanTakeover, "compensation receipt failed verification: "+err.Error(), nil)
		return true, e
	}
	if receipt.ResultCode != "succeeded" {
		_, e := o.transition(ctx, ledger.CaseID, run.StepID, ActionHumanTakeover, "compensation reported result_code="+receipt.ResultCode, nil)
		return true, e
	}
	_, e := o.transitionWith(ctx, ledger.CaseID, run.StepID, ActionCompensated, "compensation verified "+receipt.ReceiptID, func(l *RunLedger, i int) {
		l.Runs[i].ReceiptID = receipt.ReceiptID
	})
	return true, e
}

// compensationDispatchedRest handles a run FOUND at compensation_dispatched by a
// concurrent/fresh Advance (the claim winner is mid-reversal, or a crash left it
// here). A reversal is never re-dispatched on ambiguity: rest, and escalate to a
// human after bounded observations so a genuinely stuck reversal is resolved.
func (o *Orchestrator) compensationDispatchedRest(ctx context.Context, ledger RunLedger, run ActionRun) (bool, error) {
	exhausted, terr := o.bumpReconcile(ctx, ledger.CaseID, run.StepID)
	if terr != nil {
		return true, terr
	}
	if exhausted {
		_, e := o.transition(ctx, ledger.CaseID, run.StepID, ActionHumanTakeover, "compensation unresolved; not safe to auto-retry", nil)
		return true, e
	}
	return false, nil
}

// replanStep asks the (optional) Planner to propose a new plan, VALIDATES it,
// and persists it as a new WorkPlan revision through the Service. The model
// only proposes; the orchestrator decides and persists. syncPlan then adopts
// the new revision on the next loop.
func (o *Orchestrator) replanStep(ctx context.Context, wc sdkworkcase.WorkCase, ledger RunLedger, run ActionRun) (bool, error) {
	if o.planner == nil || ledger.Replans >= o.maxAttempts {
		_, err := o.transition(ctx, ledger.CaseID, run.StepID, ActionHumanTakeover, "replanning unavailable or exhausted", nil)
		return true, err
	}
	proposed, err := o.planner.ProposePlan(ctx, wc)
	if err != nil {
		_, e := o.transition(ctx, ledger.CaseID, run.StepID, ActionHumanTakeover, "planner proposal failed: "+err.Error(), nil)
		return true, e
	}
	if err := sdkworkcase.ValidatePlan(proposed); err != nil {
		_, e := o.transition(ctx, ledger.CaseID, run.StepID, ActionHumanTakeover, "proposed plan is invalid: "+err.Error(), nil)
		return true, e
	}
	key := "replan-" + shortHash(ledger.CaseID+":"+strconv.FormatUint(wc.Revision, 10))
	if _, err := o.svc.ProposePlan(ctx, ProposePlanCommand{
		Command: Command{EnterpriseID: ledger.EnterpriseID, OrgScope: ledger.OrgScope, ActorRef: ledger.ActorRef, CaseID: ledger.CaseID, ExpectedRevision: wc.Revision, IdempotencyKey: key},
		Plan:    proposed,
	}); err != nil {
		_, e := o.transition(ctx, ledger.CaseID, run.StepID, ActionHumanTakeover, "persisting replan failed: "+err.Error(), nil)
		return true, e
	}
	// Record the replan; syncPlan re-seeds runs to the new revision next loop.
	_, err = o.runs.Transform(ctx, ledger.CaseID, func(l RunLedger) (RunLedger, error) {
		l.Replans++
		return l, nil
	})
	return true, err
}

// --- WorkCase reflection (never completes the case) ------------------------

func (o *Orchestrator) markStepRunning(ctx context.Context, ledger RunLedger, stepID string) error {
	wc, err := o.svc.Get(ctx, ledger.EnterpriseID, ledger.OrgScope, ledger.CaseID)
	if err != nil {
		return err
	}
	if stepStatus(wc, stepID) != sdkworkcase.StepPending {
		return nil // already running/completed -- idempotent
	}
	_, err = o.advanceStep(ctx, ledger, wc.Revision, stepID, sdkworkcase.StepRunning, nil)
	return err
}

func (o *Orchestrator) markStepCompleted(ctx context.Context, ledger RunLedger, stepID string, ev sdkworkcase.EvidenceRef) error {
	wc, err := o.svc.Get(ctx, ledger.EnterpriseID, ledger.OrgScope, ledger.CaseID)
	if err != nil {
		return err
	}
	cur := stepStatus(wc, stepID)
	if cur == sdkworkcase.StepCompleted {
		return nil
	}
	if cur == sdkworkcase.StepPending {
		wc, err = o.advanceStep(ctx, ledger, wc.Revision, stepID, sdkworkcase.StepRunning, nil)
		if err != nil {
			return err
		}
	}
	_, err = o.advanceStep(ctx, ledger, wc.Revision, stepID, sdkworkcase.StepCompleted, []sdkworkcase.EvidenceRef{ev})
	return err
}

func (o *Orchestrator) advanceStep(ctx context.Context, ledger RunLedger, expectedRev uint64, stepID string, target sdkworkcase.StepStatus, ev []sdkworkcase.EvidenceRef) (sdkworkcase.WorkCase, error) {
	return o.svc.AdvanceStep(ctx, AdvanceStepCommand{
		Command: Command{
			EnterpriseID: ledger.EnterpriseID, OrgScope: ledger.OrgScope, ActorRef: ledger.ActorRef,
			CaseID: ledger.CaseID, ExpectedRevision: expectedRev,
			IdempotencyKey: "adv-" + shortHash(ledger.CaseID+":"+stepID+":"+string(target)+":"+strconv.FormatUint(expectedRev, 10)),
		},
		StepID: stepID, Status: target, Evidence: ev,
	})
}

func stepStatus(wc sdkworkcase.WorkCase, stepID string) sdkworkcase.StepStatus {
	_, steps := topPlan(wc)
	for _, st := range steps {
		if st.ID == stepID {
			if st.Status == "" {
				return sdkworkcase.StepPending
			}
			return st.Status
		}
	}
	return sdkworkcase.StepPending
}

// --- transitions (persist run state + append an audited decision) ----------

// transition and transitionWith apply a run-state change and append an audited
// DecisionRecord, returning applied=false (a no-op) when the transition is not
// legal from the FRESHLY-LOADED state. This is the correctness gate for
// concurrency and idempotency: the legality (CanTransitionTo) is checked, and
// dec.From is computed, inside the ledger transaction against the current state
// -- so two concurrent Advances cannot both drive pending -> dispatched, and a
// re-application of an already-applied edge is rejected rather than double-run.

func (o *Orchestrator) transition(ctx context.Context, caseID, stepID string, to ActionState, reason string, mutate func(*ActionRun)) (bool, error) {
	return o.transitionWith(ctx, caseID, stepID, to, reason, func(l *RunLedger, i int) {
		if mutate != nil {
			mutate(&l.Runs[i])
		}
	})
}

func (o *Orchestrator) transitionWith(ctx context.Context, caseID, stepID string, to ActionState, reason string, apply func(l *RunLedger, runIdx int)) (bool, error) {
	applied := false
	_, err := o.runs.Transform(ctx, caseID, func(l RunLedger) (RunLedger, error) {
		i := indexRun(l, stepID)
		if i < 0 {
			return l, ErrRunNotFound
		}
		from := l.Runs[i].State
		if !from.CanTransitionTo(to) {
			// Illegal from the freshly-loaded state: no-op. A concurrent/duplicate
			// Advance that finds the run already past `from` lands here and makes
			// no change and no decision -- so it cannot double-dispatch.
			return l, nil
		}
		l.Runs[i].State = to
		if apply != nil {
			apply(&l, i)
		}
		dec := DecisionRecord{Seq: len(l.Decisions) + 1, StepID: stepID, From: from, To: to, Reason: reason, At: o.now().UTC()}
		// The audit reference is committed atomically with the decision (same
		// ledger transaction). MemoryRunStore invokes this closure exactly once
		// per committed transition; a future optimistic-retry RunStore must keep
		// the AuditSink idempotent.
		ref, aerr := o.recordAudit(ctx, caseID, dec)
		if aerr != nil {
			return l, aerr
		}
		dec.AuditRef = ref
		l.Decisions = append(l.Decisions, dec)
		applied = true
		return l, nil
	})
	if err != nil {
		return false, err
	}
	// Reflect a terminal failure onto the WorkCase Step so a consumer never sees
	// an escalated/undone step as perpetually "running". Only a running
	// (dispatched) step is marked failed; a never-dispatched step stays pending.
	if applied {
		switch to {
		case ActionHumanTakeover, ActionTerminated, ActionCompensated:
			if ferr := o.markStepFailed(ctx, caseID, stepID); ferr != nil {
				return applied, ferr
			}
		}
	}
	return applied, nil
}

// markStepFailed reflects a terminal-failure run onto its WorkCase Step
// (StepRunning -> StepFailed) WITHOUT driving the case terminal (AdvanceStep
// leaves Status untouched). A step that never reached running (e.g. an approval
// failure before dispatch) stays pending.
func (o *Orchestrator) markStepFailed(ctx context.Context, caseID, stepID string) error {
	l, err := o.runs.Load(ctx, caseID)
	if err != nil {
		return err
	}
	wc, err := o.svc.Get(ctx, l.EnterpriseID, l.OrgScope, caseID)
	if err != nil {
		return err
	}
	if stepStatus(wc, stepID) != sdkworkcase.StepRunning {
		return nil
	}
	_, err = o.advanceStep(ctx, l, wc.Revision, stepID, sdkworkcase.StepFailed, nil)
	return err
}

func (o *Orchestrator) recordAudit(ctx context.Context, caseID string, dec DecisionRecord) (string, error) {
	if o.audit != nil {
		return o.audit.RecordDecision(ctx, caseID, dec)
	}
	return "audit-" + shortHash(caseID+":"+dec.StepID+":"+string(dec.To)+":"+dec.At.Format(time.RFC3339Nano)), nil
}

func runByID(l RunLedger, stepID string) (ActionRun, bool) {
	i := indexRun(l, stepID)
	if i < 0 {
		return ActionRun{}, false
	}
	return l.Runs[i], true
}

func indexRun(l RunLedger, stepID string) int {
	for i := range l.Runs {
		if l.Runs[i].StepID == stepID {
			return i
		}
	}
	return -1
}

// govRisk maps the nexus risk vocabulary (low/medium/high) onto the two-tier
// governance risk the UpwardReviewPolicy uses. Medium maps to high (more
// review) -- the plan defines only low (one reviewer) and high (two).
func govRisk(r nexus.RiskLevel) model.RiskLevel {
	if r == nexus.RiskLow {
		return model.RiskLow
	}
	return model.RiskHigh
}

func shortHash(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:16])
}
