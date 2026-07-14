package outcome

// evaluator.go (Task 0H) turns governed, verified facts into an immutable
// Outcome and closes a WorkCase deterministically. Two things live here:
//
//   - Evaluator/Decide: a DETERMINISTIC, fail-closed decision. Decide is a pure
//     function of its EvaluateCommand -- it reads no wall clock (the governed
//     DecidedAt is an explicit input), no randomness, and no map iteration order
//     (every collection is sorted). It consumes ONLY signature-verified
//     nexus.ActionReceipt/ObservationReceipt refs (reusing the 0D nexus.Verify*
//     path) and enforces observation freshness (VerificationNeed.MaxAge vs
//     ObservedAt) that 0D/0E deferred. A model may PROPOSE missing verification
//     needs, but it can never set a status, publish an Outcome or supply
//     evidence: the command carries only governed facts. Evaluate = Decide +
//     append-only persistence (+ lineage facts + a projection event).
//
//   - ClosureService: the FIRST and ONLY production path to WorkCase completion.
//     Apply reads the referenced Outcome, verifies it is `satisfied` AT READ TIME
//     (never trusting the caller), and drives the Outcome-gated completion under
//     optimistic concurrency + idempotency, so a duplicate/redelivered closure is
//     a no-op rather than a double transition.
//
// No Apache AGE access (Task 0I), no AgentNexus connector or imported AgentNexus
// code: receipts cross this boundary as opaque, signed handles+hashes only.

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"

	nexus "github.com/astraclawteam/agentatlas/sdk/go/nexus"
	model "github.com/astraclawteam/agentatlas/sdk/go/outcome"
	sdkworkcase "github.com/astraclawteam/agentatlas/sdk/go/workcase"
)

// Sentinel errors surfaced by the evaluator and closure service.
var (
	// ErrPolicyInvalid wraps a published-policy validation failure.
	ErrPolicyInvalid = errors.New("outcome: evaluation policy is invalid")
	// ErrEvaluateCommand rejects a structurally-invalid EvaluateCommand (a
	// missing decision instant or trusted key, for example).
	ErrEvaluateCommand = errors.New("outcome: evaluate command is invalid")
	// ErrNotSatisfiedForClosure rejects closing a WorkCase whose referenced
	// Outcome is not `satisfied`. A WorkCase completes ONLY through a satisfied
	// terminal Outcome; unsatisfied replans/terminates and disputed/blocked/
	// unknown retain explicit human/reconciliation paths.
	ErrNotSatisfiedForClosure = errors.New("outcome: closure requires a satisfied Outcome (a WorkCase completes only through a satisfied terminal Outcome)")
	// ErrClosureCaseMismatch rejects a closure whose ref names a different case
	// than the Outcome it points at binds.
	ErrClosureCaseMismatch = errors.New("outcome: closure ref case does not match the referenced Outcome's work_case_id")
	// ErrOutcomeSuperseded rejects a closure whose ref revision is no longer the
	// current head of the Outcome chain -- a revision that was once satisfied stays
	// immutably satisfied, but a correction may have superseded it with a
	// not-satisfied revision. Closure requires the CURRENT satisfied head.
	ErrOutcomeSuperseded = errors.New("outcome: closure ref revision is not the current head (a superseding correction exists); closure requires the current satisfied head")
)

// ObservationInput pairs a signed nexus.ObservationReceipt with the
// nexus.ActionRequest it claims to satisfy, so the evaluator can fail-closed
// re-verify it (nexus.VerifyObservationReceipt): identity, Action/parameter-hash
// binding, declared need/postcondition and the trusted signature.
type ObservationInput struct {
	Request nexus.ActionRequest
	Receipt nexus.ObservationReceipt
}

// ActionInput pairs a signed nexus.ActionReceipt with its request. An
// ActionReceipt is bound to an Outcome for AUDIT LINEAGE ONLY -- technical
// execution proof can never justify a satisfied conclusion.
type ActionInput struct {
	Request nexus.ActionRequest
	Receipt nexus.ActionReceipt
}

// HumanStatement is a governed human input. A Disputed statement forces the
// Outcome onto the disputed (human/reconciliation) path; a human statement can
// never author a satisfied conclusion -- only signed authoritative observations
// can. Summary is a bounded, permitted note (never raw content).
type HumanStatement struct {
	Handle    string
	Authority string
	Kind      string
	Disputed  bool
	Summary   string
}

// EvaluateCommand is the complete, governed, versioned input to a decision. It
// binds the exact Goal/WorkCase/WorkPlan/Operating Map/org versions, the
// published Policy (whose Version becomes rule_version), the signed evidence and
// the governed decision instant. Decide is a pure function of this value.
type EvaluateCommand struct {
	Tenant     string
	OutcomeKey string
	Revision   uint64
	// Supersedes must be set for a correction (Revision > 1): it names the exact
	// prior revision this decision supersedes. A re-evaluation that changes the
	// conclusion APPENDS a superseding revision; it never mutates a prior one.
	Supersedes *model.OutcomeRevisionRef

	Goal                model.GoalRef
	WorkCaseID          string
	WorkCaseRevision    uint64
	WorkPlanRevision    uint64
	OperatingMapVersion int64
	OrgVersion          int64

	Policy Policy
	// DecidedAt is the governed decision instant: the freshness reference AND the
	// stamp on the produced Outcome. The evaluator reads no other clock, so a
	// replay with an identical command reproduces an identical decision.
	DecidedAt time.Time

	TrustedKeyID string
	TrustedKey   ed25519.PublicKey

	Observations    []ObservationInput
	Actions         []ActionInput
	Blockers        []model.BlockerRef
	HumanStatements []HumanStatement
	Contributions   []model.ContributionRef

	// RevokedObservationHandles lists ObservationReceipt handles (ObservationID)
	// that have been revoked/deleted since a prior decision. They are EXCLUDED
	// from this (re-)evaluation, so a prior `satisfied` that loses its evidence
	// is re-decided to a not-satisfied conclusion via a correction revision.
	RevokedObservationHandles []string
}

// Decide is the pure, deterministic decision. It performs no I/O and no wall-clock
// read; identical input yields an identical Outcome (hence an identical
// ContentHash). It returns a fully-formed, SDK-valid Outcome (unpersisted).
func Decide(cmd EvaluateCommand) (model.Outcome, error) {
	if err := cmd.Policy.Validate(); err != nil {
		return model.Outcome{}, fmt.Errorf("%w: %v", ErrPolicyInvalid, err)
	}
	if cmd.DecidedAt.IsZero() {
		return model.Outcome{}, fmt.Errorf("%w: decided_at is required (the governed decision instant; the evaluator reads no wall clock)", ErrEvaluateCommand)
	}
	if len(cmd.TrustedKey) != ed25519.PublicKeySize {
		return model.Outcome{}, fmt.Errorf("%w: a %d-byte ed25519 trusted key is required for fail-closed verification", ErrEvaluateCommand, ed25519.PublicKeySize)
	}

	revoked := map[string]bool{}
	for _, h := range cmd.RevokedObservationHandles {
		revoked[h] = true
	}

	// Verify + bucket observations by need. A revoked, forged, unsigned or
	// detached observation is dropped here and contributes NOTHING downstream.
	type verifiedObs struct {
		receipt nexus.ObservationReceipt
		fresh   bool
	}
	byNeed := map[string][]verifiedObs{}
	for _, oi := range cmd.Observations {
		if revoked[oi.Receipt.ObservationID] {
			continue
		}
		if err := nexus.VerifyObservationReceipt(oi.Request, oi.Receipt, cmd.TrustedKeyID, cmd.TrustedKey); err != nil {
			continue
		}
		var maxAge time.Duration
		for _, n := range oi.Request.VerificationNeeds {
			if n.NeedID == oi.Receipt.VerificationNeedID {
				maxAge = n.MaxAge
				break
			}
		}
		byNeed[oi.Receipt.VerificationNeedID] = append(byNeed[oi.Receipt.VerificationNeedID], verifiedObs{receipt: oi.Receipt, fresh: freshnessOf(oi.Receipt, maxAge, cmd.DecidedAt)})
	}

	verdicts := make([]needVerdict, 0, len(cmd.Policy.Required))
	var (
		authoritativeCovered int
		freshCount           int
		conflicts            int
		attachedObs          []model.ObservationRef
	)
	seenObsHandle := map[string]bool{}

	for _, rp := range cmd.Policy.Required {
		var authFresh []verifiedObs
		anyAuthoritative := false
		for _, c := range byNeed[rp.NeedID] {
			if c.receipt.PostconditionID != rp.PostconditionID || c.receipt.Authority != rp.Authority {
				continue
			}
			anyAuthoritative = true
			// Attach every authoritative, verified observation for this need for
			// lineage/audit (deduped by handle), regardless of freshness.
			ref := toObservationRef(c.receipt)
			if !seenObsHandle[ref.Handle] {
				seenObsHandle[ref.Handle] = true
				attachedObs = append(attachedObs, ref)
			}
			if c.fresh {
				authFresh = append(authFresh, c)
			}
		}

		verdict := needUnknown
		switch {
		case len(authFresh) > 0:
			authoritativeCovered++
			freshCount++
			hashes := map[string]bool{}
			for _, c := range authFresh {
				hashes[c.receipt.NormalizedObservationHash] = true
			}
			switch {
			case len(hashes) > 1:
				// Two or more fresh authoritative observations report DIFFERENT
				// states for the same postcondition: the authorities disagree about
				// what happened -- a genuine conflict, independent of the accepting
				// definition. Route to the human/reconciliation path.
				verdict = needDisputed
				conflicts++
			case !rp.hasAcceptingDefinition():
				// A single consistent observed state, but the policy declares no
				// accepting definition: we observed SOMETHING but cannot confirm it
				// is success. Observation presence is not confirmation -> unknown
				// (fail-closed; the case cannot complete on a bare observation).
				verdict = needUnknown
			default:
				// Exactly one observed state and an accepting definition: confirm it.
				h := ""
				for k := range hashes {
					h = k
				}
				if rp.accepts(h) {
					verdict = needSatisfied
				} else {
					verdict = needUnsatisfied
				}
			}
		case anyAuthoritative:
			// An authoritative observation exists but is STALE: insufficient fresh
			// evidence -> unknown (a stale observation can never satisfy).
			authoritativeCovered++
			verdict = needUnknown
		default:
			// No authoritative observation for a required postcondition -> unknown.
			verdict = needUnknown
		}
		verdicts = append(verdicts, verdict)
	}

	hasBlocker := len(cmd.Blockers) > 0
	hasHumanDispute := false
	for _, hs := range cmd.HumanStatements {
		if hs.Disputed {
			hasHumanDispute = true
		}
	}
	status := combineStatus(verdicts, hasBlocker, hasHumanDispute, len(cmd.Policy.Required))

	// Canonicalize every collection so the marshaled Outcome (and its content
	// hash) is order-independent -- no map-order input to the decision.
	sort.Slice(attachedObs, func(i, j int) bool { return attachedObs[i].Handle < attachedObs[j].Handle })

	receiptRefs := verifiedReceiptRefs(cmd)
	blockers := append([]model.BlockerRef(nil), cmd.Blockers...)
	sort.Slice(blockers, func(i, j int) bool { return blockers[i].Handle < blockers[j].Handle })
	contributions := append([]model.ContributionRef(nil), cmd.Contributions...)
	sort.Slice(contributions, func(i, j int) bool {
		if contributions[i].ContributorID != contributions[j].ContributorID {
			return contributions[i].ContributorID < contributions[j].ContributorID
		}
		return contributions[i].Kind < contributions[j].Kind
	})

	conf := confidenceComponents(confidenceInputs{
		requiredCount:        len(cmd.Policy.Required),
		authoritativeCovered: authoritativeCovered,
		freshCount:           freshCount,
		conflicts:            conflicts,
		contributions:        len(contributions),
	})

	o := model.Outcome{
		Tenant:     cmd.Tenant,
		OutcomeKey: cmd.OutcomeKey,
		Revision:   cmd.Revision,
		Claim: model.OutcomeClaim{
			Goal:         cmd.Goal,
			Status:       status,
			RuleVersion:  cmd.Policy.Version,
			Observations: attachedObs,
			Blockers:     blockers,
			Confidence:   conf,
		},
		WorkCaseID:          cmd.WorkCaseID,
		WorkCaseRevision:    cmd.WorkCaseRevision,
		WorkPlanRevision:    cmd.WorkPlanRevision,
		OperatingMapVersion: cmd.OperatingMapVersion,
		OrgVersion:          cmd.OrgVersion,
		ActionReceipts:      receiptRefs,
		Contributions:       contributions,
		DecidedAt:           cmd.DecidedAt.UTC(),
		Supersedes:          cmd.Supersedes,
	}
	o.ID = o.DerivedID()
	if err := o.Validate(); err != nil {
		return model.Outcome{}, err
	}
	return o, nil
}

// verifiedReceiptRefs fail-closed verifies each ActionReceipt and returns its
// opaque ReceiptRef (audit lineage only), deduped by handle and sorted.
func verifiedReceiptRefs(cmd EvaluateCommand) []model.ReceiptRef {
	var refs []model.ReceiptRef
	seen := map[string]bool{}
	for _, ai := range cmd.Actions {
		if err := nexus.VerifyActionReceipt(ai.Request, ai.Receipt, cmd.TrustedKeyID, cmd.TrustedKey); err != nil {
			continue
		}
		keyID := ""
		if ai.Receipt.Signature != nil {
			keyID = ai.Receipt.Signature.KeyID
		}
		ref := model.ReceiptRef{Handle: ai.Receipt.ReceiptID, ReceiptHash: ai.Receipt.ParameterHash, SignatureKeyID: keyID}
		if seen[ref.Handle] {
			continue
		}
		seen[ref.Handle] = true
		refs = append(refs, ref)
	}
	sort.Slice(refs, func(i, j int) bool { return refs[i].Handle < refs[j].Handle })
	return refs
}

// freshnessOf reports whether receipt is fresh enough to help satisfy a need at
// decidedAt. The bound is the TIGHTER of the need's MaxAge and the receipt's own
// FreshnessWindow. FAIL-CLOSED: a zero bound (no enforceable freshness declared
// by either the need or the receipt) is NOT fresh -- an undeclared freshness
// bound must not silently disable the gate, so a satisfiable need requires an
// enforceable MaxAge/FreshnessWindow. Within a declared bound the check is
// inclusive (age == bound is fresh); an observation stamped after the decision
// instant is fresh (age clamped at 0).
func freshnessOf(r nexus.ObservationReceipt, maxAge time.Duration, decidedAt time.Time) bool {
	bound := maxAge
	if r.FreshnessWindow > 0 && (bound == 0 || r.FreshnessWindow < bound) {
		bound = r.FreshnessWindow
	}
	if bound <= 0 {
		return false // no enforceable freshness bound -> fail-closed (never fresh)
	}
	age := decidedAt.Sub(r.ObservedAt)
	return age <= bound
}

func toObservationRef(r nexus.ObservationReceipt) model.ObservationRef {
	keyID := ""
	if r.Signature != nil {
		keyID = r.Signature.KeyID
	}
	return model.ObservationRef{
		Handle:          r.ObservationID,
		ObservationHash: r.NormalizedObservationHash,
		Authority:       r.Authority,
		SignatureKeyID:  keyID,
		ObservedAt:      r.ObservedAt.UTC(),
	}
}

// ContentHash is the deterministic content hash used for replay equality. It is
// computed over the canonical JSON of the Outcome with the service-assigned ID
// and the DecidedAt stamp ZEROED, so identical versioned input yields an
// identical hash and a differently-stamped decided_at (with an unchanged
// freshness verdict) does not change the hash -- while any change to the DECISION
// (status, evidence, versions, blockers, contributions, supersession) does.
func ContentHash(o model.Outcome) string {
	o.ID = ""
	o.DecidedAt = time.Time{}
	raw, _ := json.Marshal(o)
	sum := sha256.Sum256(raw)
	return "oc_" + hex.EncodeToString(sum[:])
}

// Evaluator produces and persists Outcomes over a Store.
type Evaluator struct {
	store Store
}

// NewEvaluator constructs an Evaluator over store.
func NewEvaluator(store Store) (*Evaluator, error) {
	if store == nil {
		return nil, errors.New("outcome: evaluator requires a store")
	}
	return &Evaluator{store: store}, nil
}

// Evaluate decides the Outcome (Decide) and persists it append-only, then records
// its lineage facts and a projection event. The store's three-layer invariant
// (a satisfied Outcome must carry an authoritative signed observation) is the
// final backstop: a decision that claims satisfied without one is rejected.
func (e *Evaluator) Evaluate(ctx context.Context, cmd EvaluateCommand) (model.Outcome, error) {
	o, err := Decide(cmd)
	if err != nil {
		return model.Outcome{}, err
	}
	persisted, err := e.store.AppendOutcome(ctx, o)
	if err != nil {
		return model.Outcome{}, err
	}
	if err := e.appendLineageAndProjection(ctx, persisted); err != nil {
		return model.Outcome{}, err
	}
	return persisted, nil
}

// appendLineageAndProjection records the immutable lineage facts (outcome, goal,
// work case, observation and blocker nodes, and the edges between them) and one
// authoritative projection event for the Outcome revision. Facts are idempotent
// (append-only, first-write-wins); a re-run appends nothing new.
func (e *Evaluator) appendLineageAndProjection(ctx context.Context, o model.Outcome) error {
	tenant := o.Tenant
	outcomeNode := model.LineageNode{Tenant: tenant, Type: model.NodeOutcome, BusinessID: o.OutcomeKey, Revision: o.Revision}
	goalNode := model.LineageNode{Tenant: tenant, Type: model.NodeGoal, BusinessID: o.Claim.Goal.GoalKey, Revision: uint64(o.Claim.Goal.GoalVersion)}
	caseNode := model.LineageNode{Tenant: tenant, Type: model.NodeWorkCase, BusinessID: o.WorkCaseID, Revision: o.WorkCaseRevision}
	nodes := []model.LineageNode{outcomeNode, goalNode, caseNode}
	edges := []model.LineageEdge{
		{Tenant: tenant, Type: model.EdgeConcerns, From: endpoint(outcomeNode), To: endpoint(goalNode)},
		{Tenant: tenant, Type: model.EdgeDerivedFrom, From: endpoint(outcomeNode), To: endpoint(caseNode)},
	}
	for _, obs := range o.Claim.Observations {
		n := model.LineageNode{Tenant: tenant, Type: model.NodeObservation, BusinessID: obs.Handle, Revision: 1, Summary: obs.Authority}
		nodes = append(nodes, n)
		edges = append(edges, model.LineageEdge{Tenant: tenant, Type: model.EdgeObserves, From: endpoint(outcomeNode), To: endpoint(n)})
	}
	for _, blk := range o.Claim.Blockers {
		n := model.LineageNode{Tenant: tenant, Type: model.NodeBlocker, BusinessID: blk.Handle, Revision: 1, Summary: blk.Kind}
		nodes = append(nodes, n)
		edges = append(edges, model.LineageEdge{Tenant: tenant, Type: model.EdgeBlockedBy, From: endpoint(outcomeNode), To: endpoint(n)})
	}
	if o.Supersedes != nil {
		prior := model.LineageNode{Tenant: tenant, Type: model.NodeOutcome, BusinessID: o.Supersedes.OutcomeKey, Revision: o.Supersedes.Revision}
		nodes = append(nodes, prior)
		edges = append(edges, model.LineageEdge{Tenant: tenant, Type: model.EdgeSupersedes, From: endpoint(outcomeNode), To: endpoint(prior)})
	}
	if err := e.store.AppendLineage(ctx, nodes, edges); err != nil {
		return err
	}
	_, err := e.store.AppendProjectionEvent(ctx, model.ProjectionEvent{
		Tenant:          tenant,
		Kind:            model.ProjectionOutcomeRevision,
		SubjectType:     model.NodeOutcome,
		SubjectID:       o.OutcomeKey,
		SubjectRevision: o.Revision,
		PayloadHash:     ContentHash(o),
		RecordedAt:      o.DecidedAt.UTC(),
	})
	return err
}

func endpoint(n model.LineageNode) model.LineageEndpoint {
	return model.LineageEndpoint{Tenant: n.Tenant, Type: n.Type, BusinessID: n.BusinessID, Revision: n.Revision}
}

// --- closure ----------------------------------------------------------------

// CaseCloser is the port through which a satisfied Outcome completes its
// WorkCase. It is implemented by internal/workcase.Service.CompleteCase, the
// ONLY production writer of StatusCompleted. Defined here (not imported from
// workcase) so this package never depends on the workcase service -- the
// dependency runs workcase -> outcome, never the reverse.
type CaseCloser interface {
	CompleteCase(ctx context.Context, enterpriseID, orgScope, actorRef, caseID string, expectedRevision uint64, idempotencyKey, outcomeRef string) (sdkworkcase.WorkCase, error)
}

// OutcomeRef identifies the satisfied Outcome to apply AND the case coordinates
// to complete under. CaseID must equal the Outcome's WorkCaseID (verified).
type OutcomeRef struct {
	Tenant     string
	OutcomeKey string
	Revision   uint64
	OrgScope   string
	ActorRef   string
	CaseID     string
}

// ClosureResult reports the applied closure.
type ClosureResult struct {
	Completed bool
	Case      sdkworkcase.WorkCase
	Outcome   model.Outcome
}

// ClosureService applies a satisfied Outcome as a WorkCase completion.
type ClosureService struct {
	store  Store
	closer CaseCloser
}

// NewClosureService constructs a ClosureService over the Outcome store and the
// case-completion port.
func NewClosureService(store Store, closer CaseCloser) (*ClosureService, error) {
	if store == nil || closer == nil {
		return nil, errors.New("outcome: closure service requires a store and a case closer")
	}
	return &ClosureService{store: store, closer: closer}, nil
}

// Apply completes the WorkCase named by ref ONLY when the referenced Outcome is
// `satisfied` -- verified by RE-READING the Outcome from the authoritative store
// at read time, never trusting the caller. Completion runs under optimistic
// concurrency (expectedCaseRevision) + idempotency (idempotencyKey), so a
// duplicate or redelivered closure is a no-op, not a double transition.
func (c *ClosureService) Apply(ctx context.Context, ref OutcomeRef, expectedCaseRevision uint64, idempotencyKey string) (ClosureResult, error) {
	o, err := c.store.GetOutcome(ctx, ref.Tenant, ref.OutcomeKey, ref.Revision)
	if err != nil {
		return ClosureResult{}, err
	}
	// The referenced revision must be the CURRENT HEAD of the Outcome chain. An
	// immutable revision that was once satisfied stays satisfied forever, so a
	// correction that superseded it with a not-satisfied revision must NOT be
	// closeable -- a stale closure command minted while rev N was head can never
	// complete a case that has since been corrected.
	head, err := c.store.LatestOutcome(ctx, ref.Tenant, ref.OutcomeKey)
	if err != nil {
		return ClosureResult{}, err
	}
	if ref.Revision != head.Revision {
		return ClosureResult{Outcome: head}, fmt.Errorf("%w: ref revision %d, head %d", ErrOutcomeSuperseded, ref.Revision, head.Revision)
	}
	// Verify the Outcome is satisfied at read time -- never trust the caller.
	if o.Claim.Status != model.OutcomeSatisfied {
		return ClosureResult{Outcome: o}, fmt.Errorf("%w: %s/%s@%d is %q", ErrNotSatisfiedForClosure, ref.Tenant, ref.OutcomeKey, ref.Revision, o.Claim.Status)
	}
	if o.WorkCaseID != ref.CaseID {
		return ClosureResult{Outcome: o}, fmt.Errorf("%w: ref case %q vs outcome work_case_id %q", ErrClosureCaseMismatch, ref.CaseID, o.WorkCaseID)
	}
	wc, err := c.closer.CompleteCase(ctx, ref.Tenant, ref.OrgScope, ref.ActorRef, ref.CaseID, expectedCaseRevision, idempotencyKey, o.ID)
	if err != nil {
		return ClosureResult{Outcome: o}, err
	}
	return ClosureResult{Completed: true, Case: wc, Outcome: o}, nil
}
