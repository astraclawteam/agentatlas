package outcomelearning

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	govmodel "github.com/astraclawteam/agentatlas/sdk/go/governance"
	outcomemodel "github.com/astraclawteam/agentatlas/sdk/go/outcome"
	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/governance"
	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/outcome"
	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/outcomegraph"
)

// --- ports -----------------------------------------------------------------

// GraphQuerier is the ONLY way this package reads the Outcome Graph: the closed,
// typed 0I operation set. It is satisfied by *outcomegraph.QueryService. There
// is deliberately no method that accepts raw Cypher or a graph name — the type
// itself forbids bypassing the bounded, tenant/org-scoped surface.
type GraphQuerier interface {
	Run(ctx context.Context, req outcomegraph.Request) (outcomegraph.QueryResult, error)
}

// OutcomeVersions is the read-only, immutable historical Outcome store (Tasks
// 0G/0H) replay reads against. It is satisfied by *outcome.MemoryStore and
// *outcome.PostgresStore. This package never writes an Outcome.
type OutcomeVersions interface {
	GetOutcome(ctx context.Context, tenant, outcomeKey string, revision uint64) (outcomemodel.Outcome, error)
	LatestOutcome(ctx context.Context, tenant, outcomeKey string) (outcomemodel.Outcome, error)
}

// GovernanceDrafts is the EXISTING governed draft path (Task 0C) accepted
// candidates hand off to. It exposes ONLY Suggest — creating a suggestion-only
// draft that requires human/governed review. This package holds NO publish,
// decide or approve capability at all, so a candidate can never publish itself:
// automatic publication is structurally impossible, not merely policy-checked.
type GovernanceDrafts interface {
	Suggest(ctx context.Context, actor governance.Actor, in governance.SuggestionInput) (govmodel.ChangeDraft, error)
}

// --- service ---------------------------------------------------------------

// Service distills governed candidates from the Outcome Graph and hands accepted
// ones to governance for review. It is fail-closed at every step.
type Service struct {
	graph     GraphQuerier
	outcomes  OutcomeVersions
	gov       GovernanceDrafts
	distiller Distiller
	store     CandidateStore
	now       func() time.Time
}

// NewService constructs a Service. now defaults to time.Now.
func NewService(graph GraphQuerier, outcomes OutcomeVersions, gov GovernanceDrafts, distiller Distiller, store CandidateStore, now func() time.Time) *Service {
	if now == nil {
		now = time.Now
	}
	return &Service{graph: graph, outcomes: outcomes, gov: gov, distiller: distiller, store: store, now: now}
}

// DistillRequest names one candidate to distill: its kind, tenant/org scope, the
// graph anchor (a Goal for goal-anchored kinds, a Blocker for impact-anchored
// kinds), the subject reference, the governed actor the suggestion is filed
// under, and the distillation policy.
type DistillRequest struct {
	Tenant  string
	Org     string
	Kind    CandidateKind
	Anchor  outcomegraph.NodeRef
	Subject SourceRef
	Actor   governance.Actor
	Policy  Policy
}

// Policy bounds a distillation: the generation policy + model versions bound
// into every candidate, the minimum evidence coverage, the minimum recurrence
// (for impact-anchored patterns) and the shadow sample budget.
type Policy struct {
	GenerationVersion string
	ModelVersion      string
	MinCoverage       float64
	MinRecurrence     int
	ShadowBudget      int
}

func (r DistillRequest) validate() error {
	if r.Tenant == "" {
		return errors.New("outcomelearning: request tenant is required")
	}
	if !r.Kind.Valid() {
		return fmt.Errorf("outcomelearning: request kind %q is not valid", r.Kind)
	}
	if r.Policy.GenerationVersion == "" || r.Policy.ModelVersion == "" {
		return errors.New("outcomelearning: policy must pin the generation and model versions")
	}
	return nil
}

// evidence is the opaque bundle gathered from the typed graph surface for one
// distillation: the consistent projection watermark, the subject's source set,
// the shadow window and the read-model revoked/superseded flags.
type evidence struct {
	watermark    uint64
	watermarkOK  bool // every query reported the same watermark
	subjectSrc   []SourceRef
	windowSrc    []SourceRef
	graphRevoked map[string]bool
	recurrence   int
}

// Distill runs one distillation end to end: typed graph reads, evidence
// grounding, deterministic replay + shadow, and — only if accepted — a handoff
// to the governance draft path. If the graph is unavailable it PAUSES
// (ErrGraphUnavailable, no candidate emitted). Any quarantine is recorded but
// never handed to governance.
func (s *Service) Distill(ctx context.Context, req DistillRequest) (Candidate, error) {
	if err := req.validate(); err != nil {
		return Candidate{}, err
	}
	ev, err := s.gather(ctx, req)
	if err != nil {
		// ErrGraphUnavailable propagates as a PAUSE: no candidate is emitted
		// from stale/absent data; the caller resumes when the graph recovers.
		return Candidate{}, err
	}
	prop, err := s.distiller.Propose(ctx, ev.proposalInput(req))
	if err != nil {
		return Candidate{}, fmt.Errorf("outcomelearning: distiller proposer failed: %w", err)
	}
	cand, err := s.assess(ctx, req, ev, prop)
	if err != nil {
		return Candidate{}, err
	}
	cand.ID = cand.deriveID()
	return s.finalize(ctx, req, cand)
}

// finalize is idempotent: re-distilling identical evidence (same deterministic
// id) returns the already-stored candidate WITHOUT a second governance handoff,
// so a candidate is handed to review exactly once. A never-before-seen accepted
// candidate is handed to the governance draft path; every candidate is appended
// to the immutable store.
func (s *Service) finalize(ctx context.Context, req DistillRequest, cand Candidate) (Candidate, error) {
	if existing, ok, err := s.store.Get(ctx, cand.Tenant, cand.ID); err == nil && ok {
		return existing, nil
	}
	if cand.Disposition == DispositionAccepted {
		changeID, err := s.handoff(ctx, req, cand)
		if err != nil {
			return Candidate{}, fmt.Errorf("outcomelearning: governance handoff failed: %w", err)
		}
		cand.GovernanceChangeID = changeID
	}
	if err := s.store.Append(ctx, cand); err != nil && !errors.Is(err, ErrCandidateExists) {
		return Candidate{}, err
	}
	return cand, nil
}

// Reevaluate re-derives a prior candidate against the CURRENT graph and
// immutable history. A candidate whose source was revoked or superseded since
// distillation is re-evaluated to a NEW quarantined candidate that supersedes
// the prior one — never left as a live recommendation on withdrawn evidence.
func (s *Service) Reevaluate(ctx context.Context, prior Candidate) (Candidate, error) {
	req := DistillRequest{
		Tenant: prior.Tenant, Org: prior.Org, Kind: prior.Kind,
		Anchor:  reanchor(prior),
		Subject: subjectOf(prior),
		Policy:  Policy{GenerationVersion: prior.GenerationPolicy, ModelVersion: prior.ModelVersion},
	}
	ev, err := s.gather(ctx, req)
	if err != nil {
		return Candidate{}, err
	}
	prop := Proposal{Summary: prior.Summary, Change: prior.Proposal}
	cand, err := s.assess(ctx, req, ev, prop)
	if err != nil {
		return Candidate{}, err
	}
	cand.SupersedesID = prior.ID
	cand.ID = cand.deriveID()
	// A re-evaluation never re-publishes: it only re-files (as a fresh suggestion
	// draft) if it is STILL accepted; a quarantined re-evaluation never reaches
	// governance. finalize keeps the handoff idempotent.
	return s.finalize(ctx, DistillRequest{Tenant: prior.Tenant, Org: prior.Org, Kind: prior.Kind, Actor: reevalActor(prior)}, cand)
}

// --- gather (typed graph reads only) ---------------------------------------

func (s *Service) gather(ctx context.Context, req DistillRequest) (evidence, error) {
	if req.Kind.strategy() == strategyBlocker {
		return s.gatherBlocker(ctx, req)
	}
	return s.gatherGoal(ctx, req)
}

func (s *Service) gatherGoal(ctx context.Context, req DistillRequest) (evidence, error) {
	find, err := s.graph.Run(ctx, outcomegraph.Request{Operation: outcomegraph.OpFindEffectiveMethods, Tenant: req.Tenant, Org: req.Org, Anchor: req.Anchor, Budget: outcomegraph.DefaultBudget()})
	if err != nil {
		return evidence{}, err
	}
	explain, err := s.graph.Run(ctx, outcomegraph.Request{Operation: outcomegraph.OpExplainContribution, Tenant: req.Tenant, Org: req.Org, Anchor: outcomegraph.NodeRef{Label: outcomegraph.LabelContributor, BusinessID: req.Subject.BusinessID, Revision: max1(req.Subject.Revision)}, Budget: outcomegraph.DefaultBudget()})
	if err != nil {
		return evidence{}, err
	}
	compare, err := s.graph.Run(ctx, outcomegraph.Request{Operation: outcomegraph.OpComparePlanOutcomes, Tenant: req.Tenant, Org: req.Org, Anchor: req.Anchor, Budget: outcomegraph.DefaultBudget()})
	if err != nil {
		return evidence{}, err
	}
	ev := evidence{graphRevoked: map[string]bool{}}
	ev.watermark, ev.watermarkOK = consistentWatermark(find, explain, compare)
	ev.subjectSrc = outcomeSources(explain, ev.graphRevoked)
	ev.windowSrc = outcomeSources(compare, ev.graphRevoked)
	return ev, nil
}

func (s *Service) gatherBlocker(ctx context.Context, req DistillRequest) (evidence, error) {
	impact, err := s.graph.Run(ctx, outcomegraph.Request{Operation: outcomegraph.OpTraceBlockerImpact, Tenant: req.Tenant, Org: req.Org, Anchor: req.Anchor, Budget: outcomegraph.DefaultBudget()})
	if err != nil {
		return evidence{}, err
	}
	ev := evidence{graphRevoked: map[string]bool{}}
	ev.watermark, ev.watermarkOK = consistentWatermark(impact)
	ev.subjectSrc = outcomeSources(impact, ev.graphRevoked)
	ev.windowSrc = ev.subjectSrc
	ev.recurrence = len(ev.subjectSrc)
	return ev, nil
}

// outcomeSources extracts the Outcome result nodes as opaque source refs and
// records each node's read-model revoked/superseded flag.
func outcomeSources(res outcomegraph.QueryResult, revoked map[string]bool) []SourceRef {
	var out []SourceRef
	for _, n := range res.Nodes {
		if n.Label != outcomegraph.LabelOutcome {
			continue
		}
		out = append(out, SourceRef{Kind: SourceOutcome, BusinessID: n.BusinessID, Revision: n.Revision})
		if n.Revoked || n.Superseded {
			revoked[n.BusinessID] = true
		}
	}
	return out
}

// consistentWatermark returns the shared watermark of every query result and
// whether they all agreed. A disagreement means the graph advanced past the
// distillation watermark mid-read (a stale projection).
func consistentWatermark(results ...outcomegraph.QueryResult) (uint64, bool) {
	if len(results) == 0 {
		return 0, false
	}
	w := results[0].Watermark
	for _, r := range results[1:] {
		if r.Watermark != w {
			return w, false
		}
	}
	return w, true
}

// --- assess (deterministic grounding, replay, shadow) ----------------------

func (s *Service) assess(ctx context.Context, req DistillRequest, ev evidence, prop Proposal) (Candidate, error) {
	// Bind the immutable candidate skeleton first (sources + versions +
	// scrubbed proposer output), so even a quarantined candidate is a complete,
	// leak-free record.
	rt, action := req.Kind.governanceTarget()
	sources := append([]SourceRef{req.Subject}, ev.subjectSrc...)
	cand := Candidate{
		Tenant: req.Tenant, Org: req.Org, Kind: req.Kind,
		Summary:          scrubSummary(prop.Summary),
		Proposal:         ProposedChange{ResourceType: rt, Action: action, ResourceID: "", Content: scrubContent(prop.Change.Content)},
		Watermark:        ev.watermark,
		Anchor:           AnchorRef{Label: string(req.Anchor.Label), BusinessID: req.Anchor.BusinessID, Revision: req.Anchor.Revision},
		Sources:          sources,
		GenerationPolicy: req.Policy.GenerationVersion,
		ModelVersion:     req.Policy.ModelVersion,
		CreatedAt:        s.now().UTC(),
	}

	// Load the immutable authoritative view once (read-only, exact revisions).
	view, err := s.loadAuthView(ctx, req.Tenant, ev.subjectSrc, ev.windowSrc)
	if err != nil {
		return Candidate{}, err
	}

	// Compute replay + shadow deterministically for the record (independent of
	// which check ultimately quarantines).
	member := s.memberFn(req)
	cand.Replay = computeReplay(req.Subject.BusinessID, member, ev.subjectSrc, ev.graphRevoked, view)
	cand.Coverage = coverageOf(ev.subjectSrc, view)
	if req.Kind.strategy() == strategyGoal {
		cand.Shadow = computeShadow(req.Subject.BusinessID, ev.subjectSrc, ev.windowSrc, view, req.Policy.ShadowBudget)
	} else {
		cand.Shadow = notApplicableShadow()
	}

	reason := s.quarantineReason(req, ev, view, cand)
	if reason != ReasonNone {
		cand.Disposition = DispositionQuarantined
		cand.QuarantineReason = reason
		return cand, nil
	}
	cand.Disposition = DispositionAccepted
	return cand, nil
}

// quarantineReason applies the fail-closed checks in priority order and returns
// the FIRST reason to fire, or ReasonNone when the candidate is adoptable.
func (s *Service) quarantineReason(req DistillRequest, ev evidence, view authView, cand Candidate) QuarantineReason {
	if len(ev.subjectSrc) == 0 {
		return ReasonInsufficientEvidence
	}
	// 2. Stale projection watermark (the graph moved past the distillation
	// watermark mid-read).
	if !ev.watermarkOK {
		return ReasonStaleWatermark
	}
	// 3. Revoked/superseded source (read-model tombstone OR the authoritative
	// bound revision is no longer the head).
	for _, src := range ev.subjectSrc {
		if ev.graphRevoked[src.BusinessID] {
			return ReasonRevokedSource
		}
		if head := view.latestRevision[src.BusinessID]; head > src.Revision {
			return ReasonRevokedSource
		}
	}
	// 4. Replay mismatch (read-model claim disagrees with immutable history).
	if !cand.Replay.Matched {
		return ReasonReplayMismatch
	}
	// 5. Contradictory outcomes (an authoritative disputed conclusion).
	for _, src := range ev.subjectSrc {
		if o, ok := view.atRevision[src.BusinessID]; ok && o.Claim.Status == outcomemodel.OutcomeDisputed {
			return ReasonContradictory
		}
	}
	// 6. Low confidence (unknown/unverified conclusions).
	for _, src := range ev.subjectSrc {
		if o, ok := view.atRevision[src.BusinessID]; ok {
			if o.Claim.Status == outcomemodel.OutcomeUnknown || o.Claim.Status == outcomemodel.OutcomeUnverified {
				return ReasonLowConfidence
			}
		}
	}
	// 7. Insufficient evidence coverage.
	if cand.Coverage.Score < req.Policy.MinCoverage {
		return ReasonInsufficientEvidence
	}
	// 8. Impact-anchored patterns need a minimum recurrence to be a "pattern".
	if req.Kind.strategy() == strategyBlocker && req.Policy.MinRecurrence > 0 && ev.recurrence < req.Policy.MinRecurrence {
		return ReasonLowConfidence
	}
	// 9. Shadow regression (a proposed method/rule that does not beat the status
	// quo).
	if cand.Shadow.Applicable && cand.Shadow.Regression {
		return ReasonShadowRegression
	}
	return ReasonNone
}

// memberFn returns the authoritative-membership predicate replay uses for the
// request kind: contribution for goal-anchored (method) kinds, blocker binding
// for impact-anchored kinds.
func (s *Service) memberFn(req DistillRequest) func(outcomemodel.Outcome) bool {
	if req.Kind.strategy() == strategyBlocker {
		blocker := req.Subject.BusinessID
		return func(o outcomemodel.Outcome) bool { return blockedBy(o, blocker) }
	}
	subject := req.Subject.BusinessID
	return func(o outcomemodel.Outcome) bool { return contributes(o, subject) }
}

// loadAuthView reads the immutable authoritative Outcome view for every source
// at its exact bound revision, plus the current head revision for supersession
// detection. Read-only.
func (s *Service) loadAuthView(ctx context.Context, tenant string, sets ...[]SourceRef) (authView, error) {
	view := authView{atRevision: map[string]outcomemodel.Outcome{}, latestRevision: map[string]uint64{}, present: map[string]bool{}}
	for _, set := range sets {
		for _, src := range set {
			if _, done := view.present[src.BusinessID]; done {
				continue
			}
			o, err := s.outcomes.GetOutcome(ctx, tenant, src.BusinessID, src.Revision)
			if err != nil {
				if errors.Is(err, outcome.ErrNotFound) {
					view.present[src.BusinessID] = false
					continue
				}
				return authView{}, err
			}
			view.atRevision[src.BusinessID] = o
			view.present[src.BusinessID] = true
			latest, err := s.outcomes.LatestOutcome(ctx, tenant, src.BusinessID)
			if err == nil {
				view.latestRevision[src.BusinessID] = latest.Revision
			} else {
				view.latestRevision[src.BusinessID] = src.Revision
			}
		}
	}
	return view, nil
}

// coverageOf computes evidence coverage over the subject sources: a source is
// covered when its authoritative Outcome carries at least one EvidenceRef or an
// authoritative signed ObservationRef.
func coverageOf(subjectSrc []SourceRef, view authView) Coverage {
	covered := 0
	for _, src := range subjectSrc {
		o, ok := view.atRevision[src.BusinessID]
		if !ok {
			continue
		}
		if len(o.Claim.Evidence) > 0 {
			covered++
			continue
		}
		for _, obs := range o.Claim.Observations {
			if obs.IsAuthoritative() {
				covered++
				break
			}
		}
	}
	return newCoverage(covered, len(subjectSrc))
}

// --- governance handoff (draft only) ---------------------------------------

func (s *Service) handoff(ctx context.Context, req DistillRequest, cand Candidate) (string, error) {
	content, err := draftContent(cand)
	if err != nil {
		return "", err
	}
	rt, action := cand.Kind.governanceTarget()
	in := governance.SuggestionInput{
		OrgUnitID:       req.Org,
		ResourceType:    rt,
		ResourceID:      cand.ID,
		Action:          action, // create/update only — never publish
		BaseVersion:     0,
		ProposedContent: content,
	}
	draft, err := s.gov.Suggest(ctx, req.Actor, in)
	if err != nil {
		return "", err
	}
	return draft.ChangeID, nil
}

// draftContent builds the scrubbed, bounded JSON governance-suggestion body for
// a candidate. It carries the candidate's provenance and verdict plus the
// scrubbed proposed change — never raw content.
func draftContent(cand Candidate) (json.RawMessage, error) {
	body := map[string]any{
		"candidate_id":      cand.ID,
		"kind":              string(cand.Kind),
		"summary":           cand.Summary,
		"watermark":         cand.Watermark,
		"evidence_coverage": cand.Coverage.Score,
		"replay_matched":    cand.Replay.Matched,
		"shadow_regression": cand.Shadow.Regression,
		"proposed_change":   cand.Proposal.Content,
		"review_required":   true,
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	// Defense in depth: the assembled body must itself be leak-free.
	if err := outcomemodel.NoRawContentLeak(json.RawMessage(raw)); err != nil {
		safe, _ := json.Marshal(map[string]any{"candidate_id": cand.ID, "kind": string(cand.Kind), "summary": redacted, "review_required": true})
		return safe, nil
	}
	return raw, nil
}

// --- helpers ---------------------------------------------------------------

func (ev evidence) proposalInput(req DistillRequest) ProposalInput {
	sums := make([]EvidenceSummary, 0, len(ev.subjectSrc))
	for _, src := range ev.subjectSrc {
		sums = append(sums, EvidenceSummary{Ref: src})
	}
	return ProposalInput{Kind: req.Kind, Tenant: req.Tenant, Org: req.Org, Subject: req.Subject, Watermark: ev.watermark, Evidence: sums}
}

// reanchor rebuilds the typed graph anchor from the candidate's stored,
// closed-vocabulary AnchorRef so a re-evaluation re-runs the exact same query.
func reanchor(c Candidate) outcomegraph.NodeRef {
	return outcomegraph.NodeRef{Label: outcomegraph.NodeLabel(c.Anchor.Label), BusinessID: c.Anchor.BusinessID, Revision: max1(c.Anchor.Revision)}
}

func subjectOf(c Candidate) SourceRef {
	if len(c.Sources) > 0 {
		return c.Sources[0]
	}
	return SourceRef{}
}

func reevalActor(c Candidate) governance.Actor {
	return governance.Actor{EnterpriseID: c.Tenant, UserID: "system:outcome-learning", OrgUnitIDs: []string{c.Org}, Permissions: []string{"suggest"}}
}

func max1(v uint64) uint64 {
	if v < 1 {
		return 1
	}
	return v
}
