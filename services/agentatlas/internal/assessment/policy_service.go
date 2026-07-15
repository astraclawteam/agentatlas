package assessment

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	model "github.com/astraclawteam/agentatlas/sdk/go/assessment"
	govmodel "github.com/astraclawteam/agentatlas/sdk/go/governance"
	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/governance"
	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/outcomegraph"
)

// --- ports -----------------------------------------------------------------

// GraphQuerier is the ONLY way this package reads the Outcome Graph: the closed,
// typed Task 0I operation set. It is satisfied by *outcomegraph.QueryService.
// There is deliberately no method that accepts raw Cypher or a graph name, so a
// policy draft can only ground itself through the bounded, tenant/org-scoped,
// typed surface — never arbitrary history.
type GraphQuerier interface {
	Run(ctx context.Context, req outcomegraph.Request) (outcomegraph.QueryResult, error)
}

// GovernanceDrafts is the EXISTING governed draft path (Task 0C) a publication
// review is filed through. It exposes ONLY Suggest — a suggestion-only draft
// that a human/governed review must adopt. This package holds NO publish, decide
// or approve capability at all, so an assessment policy can never publish itself:
// automatic publication is structurally impossible, not merely policy-checked.
type GovernanceDrafts interface {
	Suggest(ctx context.Context, actor governance.Actor, in governance.SuggestionInput) (govmodel.ChangeDraft, error)
}

// GovernanceReviews reports whether a governed change reached an approved
// terminal state — the human review outcome that gates publication. The
// assessment service can never produce this itself (it holds only Suggest, above,
// and this read), so publication is impossible without an external governed
// approval. Production wires this over the governance service's record state;
// tests supply the review outcome directly.
type GovernanceReviews interface {
	ChangeApproved(ctx context.Context, enterpriseID, changeID string) (bool, error)
}

// --- service ---------------------------------------------------------------

// Service governs the B2 assessment-policy lifecycle: a graph-bound,
// evidence-grounded draft; the mandatory shadow-cycle publication gate; and a
// governed, never-self-publishing transition to published. It is fail-closed at
// every step.
type Service struct {
	graph   GraphQuerier
	gov     GovernanceDrafts
	reviews GovernanceReviews
	store   PolicyStore
	now     func() time.Time
}

// NewService constructs a Service. now defaults to time.Now.
func NewService(graph GraphQuerier, gov GovernanceDrafts, reviews GovernanceReviews, store PolicyStore, now func() time.Time) *Service {
	if now == nil {
		now = time.Now
	}
	return &Service{graph: graph, gov: gov, reviews: reviews, store: store, now: now}
}

// Service sentinel errors. Callers compare with errors.Is.
var (
	// ErrInvalidState rejects a lifecycle operation attempted from a status that
	// does not permit it.
	ErrInvalidState = errors.New("assessment: invalid policy state for this operation")
	// ErrStaleGraph blocks a draft or shadow completion when the Outcome Graph
	// advanced mid-read (an inconsistent projection watermark): the policy pauses
	// rather than silently distilling from partial history.
	ErrStaleGraph = errors.New("assessment: outcome graph advanced mid-read (stale); blocking rather than distilling from partial history")
	// ErrNoThirdCycle rejects a third shadow cycle: a failed second cycle returns
	// the policy to draft and there is never an automatic third cycle.
	ErrNoThirdCycle = errors.New("assessment: no automatic third shadow cycle; a failed second cycle returns the policy to draft")
	// ErrPublicationNotApproved blocks publication until a governed human review
	// approves — the policy never self-publishes.
	ErrPublicationNotApproved = errors.New("assessment: publication requires an approved governed review; a policy never self-publishes")
	// ErrPolicyExists rejects bootstrapping a draft when a policy already exists
	// for the key (edit it via a new revision instead).
	ErrPolicyExists = errors.New("assessment: a policy already exists for this key; revise it via a new revision")
	// ErrCycleInProgress rejects opening a new shadow cycle while one is open.
	ErrCycleInProgress = errors.New("assessment: a shadow cycle is already in progress for this revision")
	// ErrNoOpenCycle rejects completing a cycle that was never opened.
	ErrNoOpenCycle = errors.New("assessment: no open shadow cycle to complete")
	// ErrNoRuleChange rejects a revision whose rule set is identical to the current
	// head: a no-op is not an edit, so it must not mint a fresh revision that would
	// reset the shadow count.
	ErrNoRuleChange = errors.New("assessment: a revision must change the rule set; the proposed rules are identical to the current head")
)

// --- requests --------------------------------------------------------------

// DraftRequest bootstraps a policy for a company with no rubric. The six built-in
// candidate dimensions are distilled from the versioned governed sources (job
// responsibility, org goal, SOPs, acceptance/review) AND the typed historical
// Outcome Graph, bound to the exact watermark. Confidence optionally overrides a
// dimension's declared confidence level (default medium); ExtraDimensions add
// governed, non-forbidden dimensions.
type DraftRequest struct {
	Tenant          string
	Org             string
	PolicyKey       string
	Sources         model.SourceBinding
	Confidence      map[model.DimensionKey]model.ConfidenceLevel
	ExtraDimensions []model.Dimension
	Actor           governance.Actor
}

// ReviseRequest edits a policy's rules, creating a NEW immutable revision that
// restarts the shadow count. The full replacement rule set is supplied (a
// revision is a complete, immutable rubric).
type ReviseRequest struct {
	Tenant           string
	PolicyKey        string
	Dimensions       []model.Dimension
	EvidenceRules    []model.EvidenceRule
	AttributionRules []model.AttributionRule
	ConfidenceRules  []model.ConfidenceRule
	Actor            governance.Actor
}

// BeginShadowRequest opens a shadow cycle over one complete assessment period.
type BeginShadowRequest struct {
	Tenant      string
	PolicyKey   string
	Revision    int
	PeriodStart time.Time
	PeriodEnd   time.Time
	Actor       governance.Actor
}

// CompleteShadowRequest closes an open shadow cycle with the deterministic gate
// inputs observed for the completed period.
type CompleteShadowRequest struct {
	Tenant                 string
	PolicyKey              string
	Revision               int
	Cycle                  int
	EvidenceCoverage       float64
	UnresolvedCorrections  int
	UnresolvedCalibrations int
	RuleChangedAfterStart  bool
	ManagerConfirmed       bool
	PolicyOwnerConfirmed   bool
	Actor                  governance.Actor
}

// ConfirmPublicationRequest completes publication once the governed review has
// approved (never before).
type ConfirmPublicationRequest struct {
	Tenant    string
	PolicyKey string
	Revision  int
	Actor     governance.Actor
}

// RetireRequest retires a published policy revision.
type RetireRequest struct {
	Tenant    string
	PolicyKey string
	Revision  int
	Actor     governance.Actor
}

// --- Draft -----------------------------------------------------------------

// Draft bootstraps an evidence-linked DRAFT policy from the versioned governed
// sources and the typed Outcome Graph. It BLOCKS (returns the graph error) when
// the graph is unavailable or stale rather than silently using partial history.
// The proposed dimensions are SUGGESTIONS (non-formal until published); a
// forbidden or hidden dimension is hard-rejected here and again at Validate.
func (s *Service) Draft(ctx context.Context, req DraftRequest) (model.Policy, error) {
	if req.Tenant == "" || req.PolicyKey == "" {
		return model.Policy{}, errors.New("assessment: draft requires a tenant and policy_key")
	}
	if err := req.Sources.Validate(); err != nil {
		return model.Policy{}, fmt.Errorf("assessment: draft sources: %w", err)
	}
	if _, err := s.store.Head(ctx, req.Tenant, req.PolicyKey); err == nil {
		return model.Policy{}, ErrPolicyExists
	} else if !errors.Is(err, ErrNotFound) {
		return model.Policy{}, err
	}

	watermark, links, err := s.ground(ctx, req.Tenant, req.Org, req.Sources.OrgGoal)
	if err != nil {
		return model.Policy{}, err
	}

	dims := builtinCandidateDimensions(req.Sources, links)
	dims = append(dims, req.ExtraDimensions...)
	if err := rejectForbidden(dims); err != nil {
		return model.Policy{}, err
	}
	ev, at, cf := defaultRules(dims, req.Confidence)

	now := s.now().UTC()
	p := model.Policy{
		Tenant: req.Tenant, Org: req.Org, PolicyKey: req.PolicyKey, Revision: 1,
		Status: model.StatusDraft, Sources: req.Sources, Watermark: watermark,
		Dimensions: dims, EvidenceRules: ev, AttributionRules: at, ConfidenceRules: cf,
		CreatedAt: now, UpdatedAt: now,
	}
	if err := p.Validate(); err != nil {
		return model.Policy{}, err
	}
	return s.store.CreateRevision(ctx, p)
}

// --- ReviseRules -----------------------------------------------------------

// ReviseRules edits a policy's rules by creating a NEW immutable revision in
// draft, restarting the shadow count. The prior revision is untouched
// (immutable). It re-grounds the watermark and re-rejects forbidden dimensions.
func (s *Service) ReviseRules(ctx context.Context, req ReviseRequest) (model.Policy, error) {
	head, err := s.store.Head(ctx, req.Tenant, req.PolicyKey)
	if err != nil {
		return model.Policy{}, err
	}
	if err := rejectForbidden(req.Dimensions); err != nil {
		return model.Policy{}, err
	}
	watermark, _, err := s.ground(ctx, req.Tenant, head.Org, head.Sources.OrgGoal)
	if err != nil {
		return model.Policy{}, err
	}
	now := s.now().UTC()
	p := model.Policy{
		Tenant: head.Tenant, Org: head.Org, PolicyKey: head.PolicyKey, Revision: head.Revision + 1,
		Status: model.StatusDraft, Sources: head.Sources, Watermark: watermark,
		Dimensions: req.Dimensions, EvidenceRules: req.EvidenceRules,
		AttributionRules: req.AttributionRules, ConfidenceRules: req.ConfidenceRules,
		SupersedesID: head.ID, CreatedAt: now, UpdatedAt: now,
	}
	// A no-op is not an edit: the plan restarts the shadow count on any rule edit,
	// so an identical rule set must NOT mint a fresh revision (which would reset
	// the count) — it is rejected instead.
	if p.RuleDigest() == head.RuleDigest() {
		return model.Policy{}, ErrNoRuleChange
	}
	if err := p.Validate(); err != nil {
		return model.Policy{}, err
	}
	return s.store.CreateRevision(ctx, p)
}

// --- shadow cycles ---------------------------------------------------------

// BeginShadowCycle opens the next shadow cycle over one complete assessment
// period. Cycle 1 is mandatory (draft -> shadow); exactly one cycle 2 is allowed
// after a failed cycle 1; a third cycle is refused (ErrNoThirdCycle). It binds
// the exact graph watermark and BLOCKS on an unavailable/stale graph.
func (s *Service) BeginShadowCycle(ctx context.Context, req BeginShadowRequest) (model.Policy, model.ShadowCycle, error) {
	p, err := s.store.GetRevision(ctx, req.Tenant, req.PolicyKey, req.Revision)
	if err != nil {
		return model.Policy{}, model.ShadowCycle{}, err
	}
	if p.Status != model.StatusDraft && p.Status != model.StatusShadow {
		return model.Policy{}, model.ShadowCycle{}, fmt.Errorf("%w: cannot open a shadow cycle from %q", ErrInvalidState, p.Status)
	}
	if n := len(p.ShadowCycles); n > 0 && p.ShadowCycles[n-1].CompletedAt.IsZero() {
		return model.Policy{}, model.ShadowCycle{}, ErrCycleInProgress
	}
	cycleNum := len(p.ShadowCycles) + 1
	if cycleNum > model.MaxShadowCycles {
		return model.Policy{}, model.ShadowCycle{}, ErrNoThirdCycle
	}
	if cycleNum == 2 {
		prev := p.ShadowCycles[0]
		if prev.CompletedAt.IsZero() || prev.Passed {
			return model.Policy{}, model.ShadowCycle{}, fmt.Errorf("%w: a second cycle is mandatory only after a failed first cycle", ErrInvalidState)
		}
	}

	watermark, _, err := s.ground(ctx, req.Tenant, p.Org, p.Sources.OrgGoal)
	if err != nil {
		return model.Policy{}, model.ShadowCycle{}, err
	}
	now := s.now().UTC()
	cyc := model.ShadowCycle{
		Cycle: cycleNum, Revision: p.Revision, RuleDigest: p.RuleDigest(),
		PeriodStart: req.PeriodStart.UTC(), PeriodEnd: req.PeriodEnd.UTC(),
		StartedAt: now, Watermark: watermark,
	}
	p.ShadowCycles = append(p.ShadowCycles, cyc)
	p.Status = model.StatusShadow
	p.UpdatedAt = now
	stored, err := s.store.UpdateRevision(ctx, p)
	if err != nil {
		return model.Policy{}, model.ShadowCycle{}, err
	}
	return stored, cyc, nil
}

// CompleteShadowCycle closes an open cycle with its observed gate inputs and
// applies the STRICT deterministic publication gate. On a pass the policy moves
// to reviewing and files a suggestion-only governance draft; on a failure it
// stays in shadow awaiting the mandatory cycle 2, or returns to draft if the
// second cycle failed. It re-grounds the completion watermark and BLOCKS on an
// unavailable/stale graph (never completes from partial history).
func (s *Service) CompleteShadowCycle(ctx context.Context, req CompleteShadowRequest) (model.Policy, error) {
	p, err := s.store.GetRevision(ctx, req.Tenant, req.PolicyKey, req.Revision)
	if err != nil {
		return model.Policy{}, err
	}
	if p.Status != model.StatusShadow {
		return model.Policy{}, fmt.Errorf("%w: cannot complete a shadow cycle from %q", ErrInvalidState, p.Status)
	}
	idx := -1
	for i := range p.ShadowCycles {
		if p.ShadowCycles[i].Cycle == req.Cycle && p.ShadowCycles[i].CompletedAt.IsZero() {
			idx = i
			break
		}
	}
	if idx == -1 {
		return model.Policy{}, ErrNoOpenCycle
	}

	watermark, _, err := s.ground(ctx, req.Tenant, p.Org, p.Sources.OrgGoal)
	if err != nil {
		return model.Policy{}, err
	}

	now := s.now().UTC()
	c := &p.ShadowCycles[idx]
	c.CompletedAt = now
	c.Watermark = watermark
	c.EvidenceCoverage = req.EvidenceCoverage
	c.UnresolvedCorrections = req.UnresolvedCorrections
	c.UnresolvedCalibrations = req.UnresolvedCalibrations
	c.RuleChangedAfterStart = req.RuleChangedAfterStart
	c.ManagerConfirmed = req.ManagerConfirmed
	c.PolicyOwnerConfirmed = req.PolicyOwnerConfirmed

	reasons := model.EvaluatePublicationGate(p, *c)
	c.Passed = len(reasons) == 0
	c.FailReasons = reasons
	p.UpdatedAt = now

	if c.Passed {
		// Governed publication path: file a suggestion-only draft and move to
		// reviewing. The policy does NOT publish here — publication completes only
		// after the governed review approves (ConfirmPublication).
		changeID, err := s.handoff(ctx, req.Actor, p, *c)
		if err != nil {
			return model.Policy{}, err
		}
		p.GovernanceChangeID = changeID
		p.Status = model.StatusReviewing
	} else if req.Cycle >= model.MaxShadowCycles {
		// A failed second cycle returns the policy to draft; no automatic third.
		p.Status = model.StatusDraft
	} else {
		// A failed first cycle keeps the policy in shadow; cycle 2 is mandatory.
		p.Status = model.StatusShadow
	}
	return s.store.UpdateRevision(ctx, p)
}

// --- governed publication --------------------------------------------------

// handoff files a suggestion-only governance draft for the publication review.
// The draft carries a non-publish action (ActionUpdate) and origin
// employee-suggestion, so governance.ChangeDraft.Validate structurally forbids
// it from carrying publish — the policy cannot self-publish through this path.
//
// The governed human reviewer is the AUTHORITATIVE fairness gate, so the body
// carries the full REVIEWABLE rubric — every dimension (key/title/formal/
// rationale) and compact evidence/attribution/confidence summaries — not an
// opaque count. Every field is a key/title/rationale/enum already length-bounded
// by the SDK (title<=256, rationale<=512), so the body stays well within the
// governance draft's <=100 top-level-property shape and carries no raw enterprise
// content (it never leaves the opaque-content boundary).
func (s *Service) handoff(ctx context.Context, actor governance.Actor, p model.Policy, c model.ShadowCycle) (string, error) {
	dims := make([]map[string]any, 0, len(p.Dimensions))
	for _, d := range p.Dimensions {
		dims = append(dims, map[string]any{
			"key":       d.Key,
			"title":     d.Title,
			"formal":    d.Formal,
			"rationale": d.Rationale,
		})
	}
	evidence := make([]map[string]any, 0, len(p.EvidenceRules))
	for _, r := range p.EvidenceRules {
		evidence = append(evidence, map[string]any{"dimension": r.Dimension, "tier": r.Tier})
	}
	attribution := make([]map[string]any, 0, len(p.AttributionRules))
	for _, r := range p.AttributionRules {
		attribution = append(attribution, map[string]any{"dimension": r.Dimension, "mode": r.Mode})
	}
	confidence := make([]map[string]any, 0, len(p.ConfidenceRules))
	for _, r := range p.ConfidenceRules {
		confidence = append(confidence, map[string]any{"dimension": r.Dimension, "level": r.Level})
	}
	body := map[string]any{
		"assessment_policy_id":   p.ID,
		"policy_key":             p.PolicyKey,
		"revision":               p.Revision,
		"watermark":              c.Watermark,
		"shadow_cycle":           c.Cycle,
		"evidence_coverage":      c.EvidenceCoverage,
		"manager_confirmed":      c.ManagerConfirmed,
		"policy_owner_confirmed": c.PolicyOwnerConfirmed,
		"publication_requested":  true,
		"review_required":        true,
		"dimensions":             dims,
		"evidence_rules":         evidence,
		"attribution_rules":      attribution,
		"confidence_rules":       confidence,
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return "", err
	}
	in := governance.SuggestionInput{
		OrgUnitID:       p.Org,
		ResourceType:    govmodel.ResourceKnowledgeEntry,
		ResourceID:      p.PolicyKey,
		Action:          govmodel.ActionUpdate, // never publish — a suggestion cannot self-publish
		BaseVersion:     int32(p.Revision - 1),
		ProposedContent: raw,
	}
	draft, err := s.gov.Suggest(ctx, actor, in)
	if err != nil {
		return "", fmt.Errorf("assessment: governed publication draft failed: %w", err)
	}
	return draft.ChangeID, nil
}

// ConfirmPublication completes publication (reviewing -> published) ONLY when the
// governed review has approved. It fails closed (ErrPublicationNotApproved)
// otherwise: the assessment service cannot approve its own draft, so a policy
// never self-publishes.
func (s *Service) ConfirmPublication(ctx context.Context, req ConfirmPublicationRequest) (model.Policy, error) {
	p, err := s.store.GetRevision(ctx, req.Tenant, req.PolicyKey, req.Revision)
	if err != nil {
		return model.Policy{}, err
	}
	if p.Status != model.StatusReviewing || p.GovernanceChangeID == "" {
		return model.Policy{}, fmt.Errorf("%w: publication can only be confirmed from reviewing with a governed draft", ErrInvalidState)
	}
	approved, err := s.reviews.ChangeApproved(ctx, req.Tenant, p.GovernanceChangeID)
	if err != nil {
		return model.Policy{}, err
	}
	if !approved {
		return model.Policy{}, ErrPublicationNotApproved
	}
	p.Status = model.StatusPublished
	p.UpdatedAt = s.now().UTC()
	return s.store.UpdateRevision(ctx, p)
}

// Retire retires a published policy revision (the single published -> retired
// transition). A retired revision is fully frozen.
func (s *Service) Retire(ctx context.Context, req RetireRequest) (model.Policy, error) {
	p, err := s.store.GetRevision(ctx, req.Tenant, req.PolicyKey, req.Revision)
	if err != nil {
		return model.Policy{}, err
	}
	if p.Status != model.StatusPublished {
		return model.Policy{}, fmt.Errorf("%w: only a published policy can be retired", ErrInvalidState)
	}
	p.Status = model.StatusRetired
	p.UpdatedAt = s.now().UTC()
	return s.store.UpdateRevision(ctx, p)
}

// Get returns the exact policy revision.
func (s *Service) Get(ctx context.Context, tenant, policyKey string, revision int) (model.Policy, error) {
	return s.store.GetRevision(ctx, tenant, policyKey, revision)
}

// --- graph grounding (typed reads only) ------------------------------------

// ground reads the Outcome Graph ONLY through the closed typed operation set to
// distill candidate dimensions and bind the exact watermark. It BLOCKS on an
// unavailable graph (propagating ErrGraphUnavailable) and on a stale graph (an
// inconsistent watermark across the reads => ErrStaleGraph), never silently
// distilling from partial history. An empty-but-available graph is fine (a
// brand-new company has sparse history); the source binding still grounds the
// draft.
func (s *Service) ground(ctx context.Context, tenant, org string, goal model.VersionedRef) (uint64, []model.EvidenceLink, error) {
	anchor := outcomegraph.NodeRef{Label: outcomegraph.LabelGoal, BusinessID: goal.Key, Revision: goalRevision(goal.Version)}
	compare, err := s.graph.Run(ctx, outcomegraph.Request{
		Operation: outcomegraph.OpComparePlanOutcomes, Tenant: tenant, Org: org,
		Anchor: anchor, Budget: outcomegraph.DefaultBudget(),
	})
	if err != nil {
		return 0, nil, err
	}
	methods, err := s.graph.Run(ctx, outcomegraph.Request{
		Operation: outcomegraph.OpFindEffectiveMethods, Tenant: tenant, Org: org,
		Anchor: anchor, Budget: outcomegraph.DefaultBudget(),
	})
	if err != nil {
		return 0, nil, err
	}
	if compare.Watermark != methods.Watermark {
		return 0, nil, ErrStaleGraph
	}
	links := graphLinks(compare.Nodes)
	links = append(links, graphLinks(methods.Nodes)...)
	return compare.Watermark, links, nil
}

func goalRevision(version int64) uint64 {
	if version < 1 {
		return 1
	}
	return uint64(version)
}

// graphLinks maps opaque graph result nodes onto bounded evidence links (a
// handle, a business kind and a permitted summary) — never raw content.
func graphLinks(nodes []outcomegraph.ResultNode) []model.EvidenceLink {
	var out []model.EvidenceLink
	for _, n := range nodes {
		handle := fmt.Sprintf("%s:%s@%d", n.Label, n.BusinessID, n.Revision)
		if len(handle) > 256 {
			handle = handle[:256]
		}
		out = append(out, model.EvidenceLink{
			Handle:  handle,
			Kind:    strings.ToLower(string(n.Label)),
			Summary: boundedSummary(n.Summary),
		})
	}
	return out
}

func boundedSummary(s string) string {
	if len(s) > 512 {
		return s[:512]
	}
	return s
}

// --- dimension / rule construction -----------------------------------------

var builtinTitles = map[model.DimensionKey]string{
	model.DimOutcomeCompletion:       "Outcome completion",
	model.DimQuality:                 "Delivered quality",
	model.DimTimeliness:              "Timeliness",
	model.DimCollaboration:           "Collaboration and handoff",
	model.DimRiskCompliance:          "Risk and compliance",
	model.DimImprovementContribution: "Improvement contribution",
}

var builtinRationales = map[model.DimensionKey]string{
	model.DimOutcomeCompletion:       "candidate distilled from the job responsibility and confirmed outcome completion over the assessment period",
	model.DimQuality:                 "candidate distilled from accepted-deliverable quality signals grounded in the outcome graph",
	model.DimTimeliness:              "candidate distilled from milestone timeliness across the assessment period",
	model.DimCollaboration:           "candidate distilled from cross-role handoff outcomes",
	model.DimRiskCompliance:          "candidate distilled from risk and compliance blockers recorded against the goal",
	model.DimImprovementContribution: "candidate distilled from improvement contributions to shared outcomes",
}

// builtinCandidateDimensions proposes the six built-in candidate dimensions,
// each evidence-linked (WHY) to the organization goal/job responsibility and the
// distilled graph descriptors. They are SUGGESTIONS with formal-scoring INTENT;
// the EFFECT stays non-formal until the policy publishes.
func builtinCandidateDimensions(src model.SourceBinding, graph []model.EvidenceLink) []model.Dimension {
	goalLink := model.EvidenceLink{
		Handle:  "org_goal:" + src.OrgGoal.Key,
		Kind:    "org_goal",
		Summary: "grounded in the organization goal and job responsibility",
	}
	var dims []model.Dimension
	for _, k := range model.BuiltinDimensionKeys() {
		evidence := append([]model.EvidenceLink{goalLink}, graph...)
		dims = append(dims, model.Dimension{
			Key:       k,
			Title:     builtinTitles[k],
			Formal:    true,
			Rationale: builtinRationales[k],
			Evidence:  evidence,
		})
	}
	return dims
}

// defaultRules builds one evidence/attribution/confidence rule per dimension. A
// dimension's confidence defaults to medium unless overridden; the publication
// gate later forbids a FORMAL dimension carrying low confidence.
func defaultRules(dims []model.Dimension, confidence map[model.DimensionKey]model.ConfidenceLevel) ([]model.EvidenceRule, []model.AttributionRule, []model.ConfidenceRule) {
	var ev []model.EvidenceRule
	var at []model.AttributionRule
	var cf []model.ConfidenceRule
	for _, d := range dims {
		level := model.ConfidenceMedium
		if override, ok := confidence[d.Key]; ok && override.Valid() {
			level = override
		}
		ev = append(ev, model.EvidenceRule{Dimension: d.Key, Tier: model.TierVerifiedOutcome, Rationale: "grounded in a verified outcome backed by a signed observation"})
		at = append(at, model.AttributionRule{Dimension: d.Key, Mode: model.AttrSharedOnce, Rationale: "a shared outcome is attributed once, preserving contribution and blocker links"})
		cf = append(cf, model.ConfidenceRule{Dimension: d.Key, Level: level, Rationale: "confidence derived from evidence coverage, authority and freshness"})
	}
	return ev, at, cf
}

// rejectForbidden hard-rejects any forbidden or hidden dimension at the service
// boundary (defense in depth over the SDK Dimension.Validate), returning the
// wrapped model.ErrForbiddenDimension.
func rejectForbidden(dims []model.Dimension) error {
	for _, d := range dims {
		if err := d.Validate(); err != nil && errors.Is(err, model.ErrForbiddenDimension) {
			return err
		}
	}
	return nil
}
