package outcomegraph

import (
	"context"
	"errors"
	"fmt"
)

// query.go is the bounded, typed query surface of the Outcome Graph. It exposes
// ONLY a closed set of typed, tenant/org-scoped, budget-bounded operations. It
// has NO method that accepts a raw or partial Cypher string, so a caller can
// never submit arbitrary Cypher; the underlying provider always binds caller
// values as parameters (never string-concatenated into a query), so an injection
// attempt via any id/label parameter cannot escape the fixed template. Every
// response carries the source revision and projection watermark of the read
// model it came from.

// Operation is the closed set of typed Outcome-Graph server operations. Anything
// outside this set is rejected as arbitrary Cypher.
type Operation string

const (
	OpTraceOutcomeBasis       Operation = "trace_outcome_basis"
	OpFindSimilarWorkcases    Operation = "find_similar_workcases"
	OpTraceBlockerImpact      Operation = "trace_blocker_impact"
	OpFindEffectiveMethods    Operation = "find_effective_methods"
	OpExplainContribution     Operation = "explain_contribution"
	OpFindAffectedConclusions Operation = "find_affected_conclusions"
	OpComparePlanOutcomes     Operation = "compare_plan_outcomes"
)

var operations = map[Operation]NodeLabel{
	OpTraceOutcomeBasis:       LabelOutcome,
	OpFindSimilarWorkcases:    LabelWorkCase,
	OpTraceBlockerImpact:      LabelBlocker,
	OpFindEffectiveMethods:    LabelGoal,
	OpExplainContribution:     LabelContributor,
	OpFindAffectedConclusions: LabelEvidence,
	OpComparePlanOutcomes:     LabelGoal,
}

// Valid reports whether o is a known typed operation.
func (o Operation) Valid() bool { _, ok := operations[o]; return ok }

// Operations returns the closed set in a stable order.
func Operations() []Operation {
	return []Operation{
		OpTraceOutcomeBasis, OpFindSimilarWorkcases, OpTraceBlockerImpact,
		OpFindEffectiveMethods, OpExplainContribution, OpFindAffectedConclusions,
		OpComparePlanOutcomes,
	}
}

// ErrArbitraryCypher rejects any attempt to run something outside the closed set
// of typed operations. There is no arbitrary-Cypher entry point at all; this is
// the defensive backstop for the typed dispatcher.
var ErrArbitraryCypher = errors.New("outcomegraph: only typed parameterized operations are permitted; arbitrary Cypher is rejected")

// Request is a fully-typed query request: an operation, tenant/org scope, a
// versioned anchor node, and a budget. There is deliberately NO raw-query field.
type Request struct {
	Operation Operation
	Tenant    string
	Org       string
	Anchor    NodeRef
	Budget    Budget
}

// QueryService serves the typed operations over any OutcomeGraphStore provider.
type QueryService struct {
	store OutcomeGraphStore
}

// NewQueryService builds a QueryService over a graph store.
func NewQueryService(store OutcomeGraphStore) *QueryService { return &QueryService{store: store} }

// Run validates and dispatches a typed request. It rejects an unknown operation
// (ErrArbitraryCypher), an anchor whose label does not match the operation, a
// missing tenant, and an over-budget request (ErrBudgetExceeded) — before any
// provider call.
func (s *QueryService) Run(ctx context.Context, req Request) (QueryResult, error) {
	wantLabel, ok := operations[req.Operation]
	if !ok {
		return QueryResult{}, ErrArbitraryCypher
	}
	if req.Tenant == "" {
		return QueryResult{}, fmt.Errorf("%w: tenant required", ErrInvalidQuery)
	}
	if req.Anchor.Label != wantLabel {
		return QueryResult{}, fmt.Errorf("%w: %s requires a %s anchor, got %s", ErrInvalidQuery, req.Operation, wantLabel, req.Anchor.Label)
	}
	if err := req.Anchor.Validate(); err != nil {
		return QueryResult{}, fmt.Errorf("%w: anchor: %v", ErrInvalidQuery, err)
	}
	// Enforce the budget ceilings up front (a refusal, never an unbounded run).
	budget, err := req.Budget.Resolve()
	if err != nil {
		return QueryResult{}, err
	}
	switch req.Operation {
	case OpTraceOutcomeBasis:
		return s.store.GetLineage(ctx, LineageQuery{Tenant: req.Tenant, Org: req.Org, Anchor: req.Anchor, Direction: DirOut, Budget: budget})
	case OpExplainContribution:
		return s.store.GetLineage(ctx, LineageQuery{Tenant: req.Tenant, Org: req.Org, Anchor: req.Anchor, Direction: DirIn, Budget: budget})
	case OpComparePlanOutcomes:
		return s.store.GetLineage(ctx, LineageQuery{Tenant: req.Tenant, Org: req.Org, Anchor: req.Anchor, Direction: DirIn, Budget: budget})
	case OpFindSimilarWorkcases:
		return s.store.FindRelatedCases(ctx, RelatedCasesQuery{Tenant: req.Tenant, Org: req.Org, Anchor: req.Anchor, Budget: budget})
	case OpTraceBlockerImpact, OpFindAffectedConclusions:
		return s.store.TraceImpact(ctx, ImpactQuery{Tenant: req.Tenant, Org: req.Org, Anchor: req.Anchor, Budget: budget})
	case OpFindEffectiveMethods:
		return s.store.QueryMethods(ctx, MethodsQuery{Tenant: req.Tenant, Org: req.Org, Goal: req.Anchor, OnlySatisfied: true, Budget: budget})
	}
	// Unreachable: operations map is exhaustive above.
	return QueryResult{}, ErrArbitraryCypher
}

// --- typed convenience methods ---------------------------------------------

// TraceOutcomeBasis returns the goal, observations, evidence, blockers,
// contributors and receipts an Outcome revision was decided on.
func (s *QueryService) TraceOutcomeBasis(ctx context.Context, tenant, org, outcomeKey string, revision uint64, b Budget) (QueryResult, error) {
	return s.Run(ctx, Request{Operation: OpTraceOutcomeBasis, Tenant: tenant, Org: org, Anchor: NodeRef{Label: LabelOutcome, BusinessID: outcomeKey, Revision: revision}, Budget: b})
}

// FindSimilarWorkcases returns other WorkCases sharing a Goal with the anchor
// case.
func (s *QueryService) FindSimilarWorkcases(ctx context.Context, tenant, org, caseID string, revision uint64, b Budget) (QueryResult, error) {
	return s.Run(ctx, Request{Operation: OpFindSimilarWorkcases, Tenant: tenant, Org: org, Anchor: NodeRef{Label: LabelWorkCase, BusinessID: caseID, Revision: revision}, Budget: b})
}

// TraceBlockerImpact returns the Outcomes/cases affected by a Blocker.
func (s *QueryService) TraceBlockerImpact(ctx context.Context, tenant, org, blockerHandle string, b Budget) (QueryResult, error) {
	return s.Run(ctx, Request{Operation: OpTraceBlockerImpact, Tenant: tenant, Org: org, Anchor: NodeRef{Label: LabelBlocker, BusinessID: blockerHandle, Revision: 1}, Budget: b})
}

// FindEffectiveMethods returns the Contributor (method) nodes that contributed
// to satisfied Outcomes concerning a Goal.
func (s *QueryService) FindEffectiveMethods(ctx context.Context, tenant, org, goalKey string, goalVersion uint64, b Budget) (QueryResult, error) {
	return s.Run(ctx, Request{Operation: OpFindEffectiveMethods, Tenant: tenant, Org: org, Anchor: NodeRef{Label: LabelGoal, BusinessID: goalKey, Revision: goalVersion}, Budget: b})
}

// ExplainContribution returns the Outcomes a Contributor contributed to.
func (s *QueryService) ExplainContribution(ctx context.Context, tenant, org, contributorID string, b Budget) (QueryResult, error) {
	return s.Run(ctx, Request{Operation: OpExplainContribution, Tenant: tenant, Org: org, Anchor: NodeRef{Label: LabelContributor, BusinessID: contributorID, Revision: 1}, Budget: b})
}

// FindAffectedConclusions returns the Outcomes affected by a piece of Evidence.
func (s *QueryService) FindAffectedConclusions(ctx context.Context, tenant, org, evidenceHandle string, b Budget) (QueryResult, error) {
	return s.Run(ctx, Request{Operation: OpFindAffectedConclusions, Tenant: tenant, Org: org, Anchor: NodeRef{Label: LabelEvidence, BusinessID: evidenceHandle, Revision: 1}, Budget: b})
}

// ComparePlanOutcomes returns the Outcomes concerning a Goal (for comparison of
// their plans/statuses).
func (s *QueryService) ComparePlanOutcomes(ctx context.Context, tenant, org, goalKey string, goalVersion uint64, b Budget) (QueryResult, error) {
	return s.Run(ctx, Request{Operation: OpComparePlanOutcomes, Tenant: tenant, Org: org, Anchor: NodeRef{Label: LabelGoal, BusinessID: goalKey, Revision: goalVersion}, Budget: b})
}
