package assessment

// evaluator.go (Task 18C) produces a versioned, immutable, evidence-grounded
// WorkAssessment from GOVERNED, VERIFIED facts — deterministically. It mirrors
// Task 0H's Decide discipline: the deterministic core reads NO wall clock beyond
// the governed decision instant, NO randomness, and NO map iteration order (every
// collection is normalized), and it NEVER invokes a live model. The flow is
// strictly VERIFY-THEN-SCORE:
//
//  1. Read the Outcome Graph ONLY through the closed, typed, tenant/org-scoped,
//     budget-bounded operation set (the same GraphQuerier port 18B uses) to find
//     candidate Outcomes and bind the EXACT projection watermark. A stale read (the
//     watermark advanced mid-read) BLOCKS: every dimension becomes not_assessable
//     rather than scoring from an inconsistent projection.
//  2. VERIFY each candidate against the AUTHORITATIVE Outcome store (Task 0G/0H):
//     resolve to the un-superseded head, reuse the 0H disputed/superseded/unknown
//     semantics. Missing, unverifiable or conflicting evidence yields
//     not_assessable — NEVER an invented or guessed score.
//  3. Compute the evidence tier from the verified facts, classify blockers/delay
//     deterministically (blockers.go), compute the confidence explanation
//     (confidence.go), and honor cross-level attribution so a shared Outcome is
//     counted ONCE across a hierarchy.
//
// The LLM is a NARRATION PORT only (Narrator): it receives the already-computed
// facts and returns ONLY text, stored in the assessment's narrative field. It is
// never invoked in the deterministic core — Evaluate needs no Narrator at all, so
// the deterministic result is computed BEFORE and INDEPENDENT of any narration.
//
// Formal scoring requires a PUBLISHED policy (18B FormalScoringActive) and manager
// confirmation. No Apache AGE access, no live llmrouter/model, no AgentNexus.

import (
	"context"
	"errors"
	"fmt"
	"time"

	model "github.com/astraclawteam/agentatlas/sdk/go/assessment"
	omodel "github.com/astraclawteam/agentatlas/sdk/go/outcome"
	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/outcomegraph"
)

// OutcomeReader is the AUTHORITATIVE Outcome read port the evaluator VERIFIES
// candidates against. It is satisfied by *internal/outcome.MemoryStore and
// *internal/outcome.PostgresStore (the 0G/0H authoritative store). The evaluator
// never trusts the graph projection for truth — the graph finds candidates; this
// port confirms them.
type OutcomeReader interface {
	LatestOutcome(ctx context.Context, tenant, outcomeKey string) (omodel.Outcome, error)
	GetOutcome(ctx context.Context, tenant, outcomeKey string, revision uint64) (omodel.Outcome, error)
}

// Narrator is the LLM NARRATION PORT. It receives the already-computed facts and
// returns ONLY a human narrative. It is an interface (never a live model in the
// core) and is applied by Narrate AFTER the deterministic result exists; it can
// never create graph edges or change a tier, attribution, dimension result or
// confidence.
type Narrator interface {
	Narrate(ctx context.Context, facts NarrationFacts) (string, error)
}

// NarrationFacts is the read-only view of the computed assessment handed to the
// narration port. It carries the structured result the port must stay faithful
// to; the port cannot mutate it (it returns only text).
type NarrationFacts struct {
	Assessment model.WorkAssessment
}

// Evaluator produces WorkAssessments from a typed graph port and the authoritative
// Outcome store.
type Evaluator struct {
	graph    GraphQuerier
	outcomes OutcomeReader
	now      func() time.Time
}

// NewEvaluator constructs an Evaluator. now defaults to time.Now.
func NewEvaluator(graph GraphQuerier, outcomes OutcomeReader, now func() time.Time) (*Evaluator, error) {
	if graph == nil || outcomes == nil {
		return nil, errors.New("assessment: evaluator requires a graph querier and an outcome reader")
	}
	if now == nil {
		now = time.Now
	}
	return &Evaluator{graph: graph, outcomes: outcomes, now: now}, nil
}

// Sentinel errors. Callers compare with errors.Is.
var (
	// ErrManagerConfirmationRequired blocks a FORMAL assessment that is not
	// confirmed by the responsible manager.
	ErrManagerConfirmationRequired = errors.New("assessment: a formal work assessment requires manager confirmation")
)

// --- request shapes ---------------------------------------------------------

// EvaluateRequest is the complete, governed input to a deterministic assessment.
// The graph anchor is the policy's org goal; the per-dimension governed facts
// (candidate Outcome keys, deliverables, human reports, blocker signals and
// contributions) mirror how 0H's EvaluateCommand carries only governed facts.
type EvaluateRequest struct {
	Tenant  string
	Org     string
	Subject string
	Level   model.AssessmentLevel

	Policy model.Policy
	Period model.Period
	Formal bool
	// Version is the assessment version for (subject, policy revision, period). A
	// re-assessment increments it (a NEW version, never an in-place edit); it
	// defaults to 1 when unset.
	Version int

	Manager model.ManagerConfirmation

	DimensionInputs map[model.DimensionKey]DimensionInput
	SubAssessments  []model.WorkAssessment

	Budget    outcomegraph.Budget
	DecidedAt time.Time
}

// DimensionInput carries the governed facts grounding ONE dimension. OutcomeCandidates
// are the Outcome keys the caller maps to this dimension; the evaluator counts one
// only if it is BOTH a typed-graph candidate AND verifies against the authoritative
// store. Attribution overrides the policy's attribution rule for the dimension when
// set.
type DimensionInput struct {
	OutcomeCandidates []string
	Deliverables      []DeliverableFact
	HumanReports      []HumanReportFact
	Blockers          []BlockerFact
	Contributions     []model.Contribution
	Attribution       model.AttributionMode
}

// DeliverableFact is a tier-2 candidate: an accepted deliverable/milestone/review/
// dependency with (when present) signed execution evidence.
type DeliverableFact struct {
	Handle         string
	Kind           string
	SignatureKeyID string
	Authority      string
	Summary        string
}

// HumanReportFact is a tier-3 candidate: a named human report.
type HumanReportFact struct {
	Handle    string
	Authority string
	Summary   string
}

// BlockerFact is one blocker plus the governed signals its classification is
// computed from (blockers.go).
type BlockerFact struct {
	Handle  string
	Kind    string
	Summary string
	Signals BlockerSignals
}

// --- Evaluate ---------------------------------------------------------------

// Evaluate produces the deterministic, normalized WorkAssessment. It fails closed:
// a formal assessment requires a published policy and manager confirmation; an
// unavailable graph errors; a stale graph blocks every dimension as not_assessable.
func (e *Evaluator) Evaluate(ctx context.Context, req EvaluateRequest) (model.WorkAssessment, error) {
	if req.Tenant == "" || req.Subject == "" {
		return model.WorkAssessment{}, errors.New("assessment: evaluate requires a tenant and subject")
	}
	if err := req.Policy.Validate(); err != nil {
		return model.WorkAssessment{}, fmt.Errorf("assessment: evaluate policy: %w", err)
	}
	if req.Formal {
		if err := req.Policy.AssertFormalScoringAllowed(); err != nil {
			return model.WorkAssessment{}, err
		}
		if !req.Manager.Confirmed {
			return model.WorkAssessment{}, ErrManagerConfirmationRequired
		}
	}

	budget := req.Budget
	if (budget == outcomegraph.Budget{}) {
		budget = outcomegraph.DefaultBudget()
	}
	cands, watermark, stale, truncated, err := e.readGraph(ctx, req.Tenant, req.Org, req.Policy.Sources.OrgGoal, budget)
	if err != nil {
		return model.WorkAssessment{}, err
	}

	// The set of Outcome keys already counted by a lower level: a shared Outcome
	// is counted ONCE across the hierarchy, so the parent excludes them.
	childCounted := map[string]bool{}
	for _, sub := range req.SubAssessments {
		for _, k := range sub.CountedOutcomeKeys() {
			childCounted[k] = true
		}
	}

	attrByDim := map[model.DimensionKey]model.AttributionMode{}
	for _, r := range req.Policy.AttributionRules {
		attrByDim[r.Dimension] = r.Mode
	}
	confByDim := map[model.DimensionKey]model.ConfidenceLevel{}
	for _, r := range req.Policy.ConfidenceRules {
		confByDim[r.Dimension] = r.Level
	}

	decidedAt := req.DecidedAt
	if decidedAt.IsZero() {
		decidedAt = e.now()
	}
	version := req.Version
	if version < 1 {
		version = 1
	}

	dims := make([]model.DimensionResult, 0, len(req.Policy.Dimensions))
	for _, d := range req.Policy.Dimensions {
		attr := attrByDim[d.Key]
		if di, ok := req.DimensionInputs[d.Key]; ok && di.Attribution != "" {
			attr = di.Attribution
		}
		if !attr.Valid() {
			attr = model.AttrSharedOnce
		}
		dims = append(dims, e.assessDimension(ctx, req.Tenant, d.Key, req.DimensionInputs[d.Key], attr, confByDim[d.Key], cands, stale, truncated, childCounted))
	}

	wa := model.WorkAssessment{
		Tenant:         req.Tenant,
		Org:            req.Org,
		Subject:        req.Subject,
		Level:          req.Level,
		PolicyKey:      req.Policy.PolicyKey,
		PolicyRevision: req.Policy.Revision,
		Version:        version,
		Formal:         req.Formal,
		OrgVersion:     req.Policy.Sources.OrgVersion,
		Sources:        req.Policy.Sources,
		Period:         req.Period,
		Graph: model.GraphSchema{
			Provider:        outcomegraph.ProviderName,
			SchemaVersion:   outcomegraph.GraphSchemaVersion,
			ProtocolVersion: outcomegraph.ProjectionProtocolVersion,
			GraphName:       outcomegraph.DefaultGraphName,
			Watermark:       watermark,
		},
		Dimensions:     dims,
		SubAssessments: req.SubAssessments,
		Manager:        req.Manager,
		CreatedAt:      decidedAt.UTC(),
	}
	wa.ID = wa.DerivedID()
	out := wa.Normalize()
	out.ID = out.DerivedID()
	if err := out.Validate(); err != nil {
		return model.WorkAssessment{}, fmt.Errorf("assessment: produced an invalid assessment: %w", err)
	}
	return out, nil
}

// graphCand is a typed-graph Outcome candidate: the projected revision and whether
// the projection flagged it superseded.
type graphCand struct {
	revision   uint64
	superseded bool
}

// readGraph reads candidates and the exact watermark through the closed typed
// operation set ONLY (mirroring 18B's Service.ground): two typed reads whose
// watermarks must agree, else the projection advanced mid-read (stale). An
// unavailable graph propagates ErrGraphUnavailable (a block, never a silent wrong
// answer).
func (e *Evaluator) readGraph(ctx context.Context, tenant, org string, goal model.VersionedRef, budget outcomegraph.Budget) (map[string]graphCand, uint64, bool, bool, error) {
	anchor := outcomegraph.NodeRef{Label: outcomegraph.LabelGoal, BusinessID: goal.Key, Revision: goalRevision(goal.Version)}
	compare, err := e.graph.Run(ctx, outcomegraph.Request{Operation: outcomegraph.OpComparePlanOutcomes, Tenant: tenant, Org: org, Anchor: anchor, Budget: budget})
	if err != nil {
		return nil, 0, false, false, err
	}
	methods, err := e.graph.Run(ctx, outcomegraph.Request{Operation: outcomegraph.OpFindEffectiveMethods, Tenant: tenant, Org: org, Anchor: anchor, Budget: budget})
	if err != nil {
		return nil, 0, false, false, err
	}
	stale := compare.Watermark != methods.Watermark
	cands := map[string]graphCand{}
	for _, n := range compare.Nodes {
		if n.Label == outcomegraph.LabelOutcome {
			cands[n.BusinessID] = graphCand{revision: n.Revision, superseded: n.Superseded}
		}
	}
	for _, n := range methods.Nodes {
		if n.Label == outcomegraph.LabelOutcome {
			if _, ok := cands[n.BusinessID]; !ok {
				cands[n.BusinessID] = graphCand{revision: n.Revision, superseded: n.Superseded}
			}
		}
	}
	return cands, compare.Watermark, stale, compare.Truncated || methods.Truncated, nil
}

// countedOutcome is one verified authoritative Outcome head counted for a
// dimension, retained through cross-level de-duplication.
type countedOutcome struct {
	key       string
	satisfied bool
	evidence  *model.Evidence // tier-1, when backed by a signed authoritative observation
}

// assessDimension verifies a dimension's candidates and produces its result. A
// stale graph blocks (not_assessable/stale); conflicting evidence blocks
// (not_assessable/conflicting); no verifiable evidence blocks
// (not_assessable/unverifiable or /insufficient). Otherwise it is assessed, with
// the counted un-superseded heads de-duplicated across levels.
func (e *Evaluator) assessDimension(ctx context.Context, tenant string, dimKey model.DimensionKey, di DimensionInput, attr model.AttributionMode, ruleLevel model.ConfidenceLevel, cands map[string]graphCand, stale, truncated bool, childCounted map[string]bool) model.DimensionResult {
	if stale {
		return notAssessable(dimKey, attr, model.ReasonStaleGraph, confidenceInputs{graphStale: true, ruleLevel: ruleLevel})
	}

	var (
		candidates int
		conflicts  int
		failed     int
		counted    []countedOutcome
		evidence   []model.Evidence
		tier2      int
		tier3      int
	)

	// (1) Outcome candidates: verify each graph candidate against the authoritative
	// store, resolving to the un-superseded head.
	for _, key := range di.OutcomeCandidates {
		cand, ok := cands[key]
		if !ok {
			// Not a typed-graph candidate: absent, not a verifiable candidate.
			continue
		}
		candidates++
		head, err := e.outcomes.LatestOutcome(ctx, tenant, key)
		if err != nil {
			// A graph candidate with no authoritative Outcome behind it.
			failed++
			continue
		}
		switch head.Claim.Status {
		case omodel.OutcomeDisputed:
			conflicts++
		case omodel.OutcomeSatisfied, omodel.OutcomeUnsatisfied, omodel.OutcomeBlocked:
			if attr == model.AttrBlockerExcluded && head.Claim.Status == omodel.OutcomeBlocked {
				// A blocked outcome is excluded from the assessed party.
				continue
			}
			co := countedOutcome{key: key, satisfied: head.Claim.Status == omodel.OutcomeSatisfied}
			if obs, ok := authoritativeObservation(head); ok {
				ev := model.Evidence{
					Tier: model.TierVerifiedOutcome, Handle: key, Kind: "outcome", Revision: head.Revision,
					Authority: obs.Authority, SignatureKeyID: obs.SignatureKeyID, Verified: true,
					Superseded: cand.superseded || cand.revision != head.Revision,
				}
				co.evidence = &ev
			}
			counted = append(counted, co)
		default:
			// unknown / unverified: an inconclusive head is insufficient signal
			// (0H's unknown), not a verified conclusion — do not count.
		}
	}

	// (2) Tier-2 deliverables: signed execution evidence.
	for _, d := range di.Deliverables {
		candidates++
		if d.SignatureKeyID == "" {
			failed++
			continue
		}
		evidence = append(evidence, model.Evidence{
			Tier: model.TierAcceptedDeliverable, Handle: d.Handle, Kind: nonEmpty(d.Kind, "deliverable"),
			Authority: d.Authority, SignatureKeyID: d.SignatureKeyID, Verified: true, Summary: bounded(d.Summary),
		})
		tier2++
	}

	// (3) Tier-3 human reports: a named human source (lowest tier).
	for _, r := range di.HumanReports {
		candidates++
		if r.Authority == "" {
			failed++
			continue
		}
		evidence = append(evidence, model.Evidence{
			Tier: model.TierHumanReport, Handle: r.Handle, Kind: "report", Authority: r.Authority, Summary: bounded(r.Summary),
		})
		tier3++
	}

	// Verify-then-score decision (fail-closed).
	if conflicts > 0 {
		return notAssessable(dimKey, attr, model.ReasonConflicting, confidenceInputs{
			candidates: candidates, conflicts: conflicts, contributions: len(di.Contributions),
			attributedContrib: len(di.Contributions), graphTruncated: truncated, ruleLevel: ruleLevel,
		})
	}
	if len(counted) == 0 && tier2 == 0 && tier3 == 0 {
		reason := model.ReasonInsufficientData
		if failed > 0 {
			reason = model.ReasonUnverifiable
		}
		return notAssessable(dimKey, attr, reason, confidenceInputs{
			candidates: candidates, contributions: len(di.Contributions),
			attributedContrib: len(di.Contributions), graphTruncated: truncated, ruleLevel: ruleLevel,
		})
	}

	// Confidence is computed from the RAW verification (evidence quality), before
	// cross-level de-duplication reassigns credit.
	tier1 := 0
	for _, c := range counted {
		if c.evidence != nil {
			tier1++
		}
	}
	conf := computeConfidence(confidenceInputs{
		candidates: candidates, counted: len(counted) + tier2 + tier3, authoritative: tier1, fresh: tier1,
		conflicts: 0, contributions: len(di.Contributions), attributedContrib: len(di.Contributions),
		graphStale: false, graphTruncated: truncated, ruleLevel: ruleLevel,
	})

	// Cross-level de-duplication: a shared Outcome counted at a lower level is not
	// re-counted here (the credit stays where it was first counted).
	var countedKeys []string
	satisfied := 0
	seen := map[string]bool{}
	for _, c := range counted {
		if childCounted[c.key] {
			continue // counted once, below
		}
		if seen[c.key] {
			continue
		}
		seen[c.key] = true
		countedKeys = append(countedKeys, c.key)
		if c.satisfied {
			satisfied++
		}
		if c.evidence != nil {
			evidence = append(evidence, *c.evidence)
		}
	}

	blockers := make([]model.Blocker, 0, len(di.Blockers))
	for _, b := range di.Blockers {
		bc, bd := ClassifyBlocker(b.Signals)
		blockers = append(blockers, model.Blocker{Handle: b.Handle, Kind: nonEmpty(b.Kind, "blocker"), Confidence: bc, Delay: bd, Summary: bounded(b.Summary)})
	}

	contributions := append([]model.Contribution(nil), di.Contributions...)

	return model.DimensionResult{
		Dimension:          dimKey,
		State:              model.StateAssessed,
		Attribution:        attr,
		CountedOutcomeKeys: countedKeys,
		SatisfiedOutcomes:  satisfied,
		Evidence:           evidence,
		Blockers:           blockers,
		Contributions:      contributions,
		Confidence:         conf,
	}
}

// notAssessable builds a not_assessable dimension result with a computed
// confidence explanation and NO invented score.
func notAssessable(dimKey model.DimensionKey, attr model.AttributionMode, reason model.NotAssessableReason, in confidenceInputs) model.DimensionResult {
	return model.DimensionResult{
		Dimension:           dimKey,
		State:               model.StateNotAssessable,
		NotAssessableReason: reason,
		Attribution:         attr,
		Confidence:          computeConfidence(in),
	}
}

// authoritativeObservation returns the first authoritative, signed observation on
// the Outcome head (the tier-1 backing), if any.
func authoritativeObservation(o omodel.Outcome) (omodel.ObservationRef, bool) {
	for _, obs := range o.Claim.Observations {
		if obs.IsAuthoritative() {
			return obs, true
		}
	}
	return omodel.ObservationRef{}, false
}

func nonEmpty(s, fallback string) string {
	if s == "" {
		return fallback
	}
	return s
}

func bounded(s string) string {
	const max = 512
	if len(s) > max {
		return s[:max]
	}
	return s
}

// --- narration port ---------------------------------------------------------

// Narrate applies the LLM narration port to an already-computed assessment. It
// changes ONLY the narrative field: the port receives the computed facts and
// returns text, which is stored bounded. Every structured fact (id, digest,
// dimension results, confidence) is untouched — the port cannot change a tier,
// attribution, dimension result or confidence, and this method is never called in
// the deterministic core.
func (e *Evaluator) Narrate(ctx context.Context, wa model.WorkAssessment, n Narrator) (model.WorkAssessment, error) {
	if n == nil {
		return wa, nil
	}
	text, err := n.Narrate(ctx, NarrationFacts{Assessment: wa.Normalize()})
	if err != nil {
		return model.WorkAssessment{}, err
	}
	out := wa
	out.Narrative = boundedNarrative(text)
	return out, nil
}

func boundedNarrative(s string) string {
	const max = 8192
	if len(s) <= max {
		return s
	}
	// Trim on a rune boundary.
	b := []byte(s[:max])
	for len(b) > 0 && b[len(b)-1]&0xC0 == 0x80 {
		b = b[:len(b)-1]
	}
	return string(b)
}
