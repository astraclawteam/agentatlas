package assessment

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	model "github.com/astraclawteam/agentatlas/sdk/go/assessment"
	govmodel "github.com/astraclawteam/agentatlas/sdk/go/governance"
	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/governance"
	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/outcomegraph"
)

const (
	tnt       = "ent-b2"
	org       = "org-line-1"
	policyKey = "assess.assembly.operator"
	goalKey   = "goal.assembly.throughput"
	goalVer   = 2
)

func fixedNow() time.Time { return time.Date(2026, 7, 15, 9, 0, 0, 0, time.UTC) }

// --- fake typed graph querier ----------------------------------------------

// fakeGraph serves ONLY the closed typed operation set (it rejects anything
// outside it, proving the service never reaches for raw Cypher). advanceEachCall
// simulates the projection advancing mid-read (a stale watermark), and
// unavailable simulates an AGE outage.
type fakeGraph struct {
	nodes           []outcomegraph.ResultNode
	wm              uint64
	unavailable     bool
	advanceEachCall bool
	calls           []outcomegraph.Request
}

func (f *fakeGraph) Run(_ context.Context, req outcomegraph.Request) (outcomegraph.QueryResult, error) {
	f.calls = append(f.calls, req)
	if !req.Operation.Valid() {
		return outcomegraph.QueryResult{}, outcomegraph.ErrArbitraryCypher
	}
	if f.unavailable {
		return outcomegraph.QueryResult{}, outcomegraph.ErrGraphUnavailable
	}
	wm := f.wm
	if f.advanceEachCall {
		f.wm++
	}
	return outcomegraph.QueryResult{Nodes: f.nodes, Watermark: wm, SourceRevision: req.Anchor.Revision}, nil
}

func groundedGraph() *fakeGraph {
	return &fakeGraph{
		wm: 42,
		nodes: []outcomegraph.ResultNode{
			{Label: outcomegraph.LabelOutcome, BusinessID: "outcome-1", Revision: 3, Summary: "satisfied throughput outcome"},
			{Label: outcomegraph.LabelContributor, BusinessID: "method-x", Revision: 1, Summary: "two-step verify"},
		},
	}
}

// --- governance doubles ----------------------------------------------------

func newGov() (*governance.Service, *governance.MemoryPublisher, governance.Actor) {
	store := governance.NewMemoryStore(fixedNow)
	pub := governance.NewMemoryPublisher()
	svc := governance.NewService(store, nil, &governance.MemoryAuditAppender{}, pub, fixedNow)
	actor := governance.Actor{EnterpriseID: tnt, UserID: "system:assessment", OrgUnitIDs: []string{org}, Permissions: []string{"suggest"}}
	return svc, pub, actor
}

// stubReviews stands in for the governed human review outcome the assessment
// service can never produce itself (it holds no approve/publish capability).
type stubReviews struct {
	approved bool
	calls    int
}

func (r *stubReviews) ChangeApproved(_ context.Context, _, _ string) (bool, error) {
	r.calls++
	return r.approved, nil
}

// --- fixtures --------------------------------------------------------------

func sources() model.SourceBinding {
	return model.SourceBinding{
		OrgVersion:        3,
		JobResponsibility: model.VersionedRef{Key: "resp.assembly.operator", Version: 4},
		OrgGoal:           model.VersionedRef{Key: goalKey, Version: goalVer},
		SOPs:              []model.VersionedRef{{Key: "sop.assembly.safety", Version: 7}},
		AcceptanceReview:  model.VersionedRef{Key: "review.assembly.qc", Version: 1},
	}
}

func newService(t *testing.T, graph GraphQuerier, reviews GovernanceReviews) (*Service, *governance.MemoryPublisher, governance.Actor) {
	t.Helper()
	gov, pub, actor := newGov()
	svc := NewService(graph, gov, reviews, NewMemoryPolicyStore(fixedNow), fixedNow)
	return svc, pub, actor
}

func draftReq(actor governance.Actor) DraftRequest {
	return DraftRequest{Tenant: tnt, Org: org, PolicyKey: policyKey, Sources: sources(), Actor: actor}
}

func passingComplete(actor governance.Actor, cycle int) CompleteShadowRequest {
	return CompleteShadowRequest{
		Tenant: tnt, PolicyKey: policyKey, Revision: 1, Cycle: cycle,
		EvidenceCoverage: 0.95, ManagerConfirmed: true, PolicyOwnerConfirmed: true,
		Actor: actor,
	}
}

// beginReq opens a shadow cycle over a COMPLETED (past) assessment period, so a
// completion stamped at fixedNow() legitimately falls after the period end.
func beginReq(actor governance.Actor, revision int) BeginShadowRequest {
	return BeginShadowRequest{
		Tenant: tnt, PolicyKey: policyKey, Revision: revision,
		PeriodStart: fixedNow().Add(-720 * time.Hour), PeriodEnd: fixedNow().Add(-time.Hour),
		Actor: actor,
	}
}

// --- lifecycle: draft -> shadow -> reviewing -> published -> retired --------

func TestPolicyLifecycleHappyPath(t *testing.T) {
	ctx := context.Background()
	graph := groundedGraph()
	reviews := &stubReviews{}
	svc, pub, actor := newService(t, graph, reviews)

	// A company with no rubric receives an evidence-linked DRAFT.
	p, err := svc.Draft(ctx, draftReq(actor))
	if err != nil {
		t.Fatalf("Draft: %v", err)
	}
	if p.Status != model.StatusDraft {
		t.Fatalf("draft status = %q", p.Status)
	}
	if p.Revision != 1 {
		t.Fatalf("first revision = %d", p.Revision)
	}
	if len(p.Dimensions) != 6 {
		t.Fatalf("draft must propose the six built-in candidate dimensions, got %d", len(p.Dimensions))
	}
	if p.Watermark != 42 {
		t.Fatalf("draft must bind the exact graph watermark, got %d", p.Watermark)
	}
	for _, d := range p.Dimensions {
		if len(d.Evidence) == 0 || d.Rationale == "" {
			t.Fatalf("dimension %q must be evidence-linked with a rationale (WHY)", d.Key)
		}
	}
	// Cannot issue formal scores before cycle 1.
	if p.FormalScoringActive() {
		t.Fatal("a draft policy must not activate formal scoring")
	}
	if err := p.AssertFormalScoringAllowed(); err == nil {
		t.Fatal("a draft policy must reject formal scoring")
	}

	// Begin shadow cycle 1 (mandatory).
	p, cyc, err := svc.BeginShadowCycle(ctx, beginReq(actor, 1))
	if err != nil {
		t.Fatalf("BeginShadowCycle: %v", err)
	}
	if p.Status != model.StatusShadow {
		t.Fatalf("after begin, status = %q", p.Status)
	}
	if cyc.Cycle != 1 || cyc.IsFormal() {
		t.Fatalf("cycle 1 must be non-formal, got cycle=%d formal=%v", cyc.Cycle, cyc.IsFormal())
	}

	// Complete cycle 1 with every gate satisfied -> reviewing + governed draft.
	p, err = svc.CompleteShadowCycle(ctx, passingComplete(actor, 1))
	if err != nil {
		t.Fatalf("CompleteShadowCycle: %v", err)
	}
	if p.Status != model.StatusReviewing {
		t.Fatalf("passing cycle must move to reviewing, got %q", p.Status)
	}
	if p.GovernanceChangeID == "" {
		t.Fatal("a passing cycle must file a governed suggestion draft")
	}
	if pub.Count() != 0 {
		t.Fatal("the assessment service must never self-publish (governance publisher called)")
	}

	// No silent publication: confirmation fails while the governed review has not
	// approved (the assessment service cannot approve its own draft).
	reviews.approved = false
	if _, err := svc.ConfirmPublication(ctx, ConfirmPublicationRequest{Tenant: tnt, PolicyKey: policyKey, Revision: 1, Actor: actor}); !errors.Is(err, ErrPublicationNotApproved) {
		t.Fatalf("ConfirmPublication without approval = %v, want ErrPublicationNotApproved", err)
	}

	// After the governed review approves, publication completes.
	reviews.approved = true
	p, err = svc.ConfirmPublication(ctx, ConfirmPublicationRequest{Tenant: tnt, PolicyKey: policyKey, Revision: 1, Actor: actor})
	if err != nil {
		t.Fatalf("ConfirmPublication after approval: %v", err)
	}
	if p.Status != model.StatusPublished {
		t.Fatalf("confirmed status = %q, want published", p.Status)
	}
	if !p.FormalScoringActive() {
		t.Fatal("a published policy must activate formal scoring")
	}
	if pub.Count() != 0 {
		t.Fatal("publication must go through the governed review, never the assessment service")
	}

	// A published revision is immutable: no shadow cycle may reopen on it.
	if _, _, err := svc.BeginShadowCycle(ctx, beginReq(actor, 1)); err == nil {
		t.Fatal("a published revision must be frozen against new shadow cycles")
	}

	// Retire the published revision (published -> retired); then it is fully frozen.
	p, err = svc.Retire(ctx, RetireRequest{Tenant: tnt, PolicyKey: policyKey, Revision: 1, Actor: actor})
	if err != nil {
		t.Fatalf("Retire: %v", err)
	}
	if p.Status != model.StatusRetired {
		t.Fatalf("retired status = %q", p.Status)
	}
	if _, err := svc.Retire(ctx, RetireRequest{Tenant: tnt, PolicyKey: policyKey, Revision: 1, Actor: actor}); err == nil {
		t.Fatal("a retired revision must be fully frozen")
	}
}

// --- graph-bound: stale/unavailable BLOCKS ---------------------------------

func TestPolicyDraftBlocksOnUnavailableGraph(t *testing.T) {
	ctx := context.Background()
	graph := groundedGraph()
	graph.unavailable = true
	svc, _, actor := newService(t, graph, &stubReviews{})
	if _, err := svc.Draft(ctx, draftReq(actor)); !errors.Is(err, outcomegraph.ErrGraphUnavailable) {
		t.Fatalf("Draft on an unavailable graph = %v, want ErrGraphUnavailable (block, no partial history)", err)
	}
}

func TestPolicyShadowCompletionBlocksOnStaleGraph(t *testing.T) {
	ctx := context.Background()
	graph := groundedGraph()
	svc, _, actor := newService(t, graph, &stubReviews{})
	if _, err := svc.Draft(ctx, draftReq(actor)); err != nil {
		t.Fatalf("Draft: %v", err)
	}
	if _, _, err := svc.BeginShadowCycle(ctx, beginReq(actor, 1)); err != nil {
		t.Fatalf("BeginShadowCycle: %v", err)
	}
	// The graph advances mid-read during completion -> stale -> completion blocks.
	graph.advanceEachCall = true
	if _, err := svc.CompleteShadowCycle(ctx, passingComplete(actor, 1)); !errors.Is(err, ErrStaleGraph) {
		t.Fatalf("CompleteShadowCycle on a stale graph = %v, want ErrStaleGraph", err)
	}
}

// --- forbidden dimensions hard-rejected by the service ----------------------

func TestForbiddenDimensionRejectedByService(t *testing.T) {
	ctx := context.Background()
	svc, _, actor := newService(t, groundedGraph(), &stubReviews{})

	forbidden := []model.Dimension{
		{Key: "personality", Title: "Personality fit", Formal: true, Rationale: "team culture"},
		{Key: "loyalty", Title: "Company loyalty", Formal: true, Rationale: "retention"},
		{Key: "private_life", Title: "Private life", Formal: true, Rationale: "off hours"},
		{Key: model.DimQuality, Title: "Quality", Hidden: true, Formal: true, Rationale: "grounded in outcomes"},
	}
	for _, d := range forbidden {
		req := draftReq(actor)
		req.ExtraDimensions = []model.Dimension{d}
		if _, err := svc.Draft(ctx, req); !errors.Is(err, model.ErrForbiddenDimension) {
			t.Fatalf("Draft with forbidden dimension %q = %v, want ErrForbiddenDimension", d.Key, err)
		}
	}

	// A clean draft first, then a forbidden rule edit is also rejected.
	if _, err := svc.Draft(ctx, draftReq(actor)); err != nil {
		t.Fatalf("clean Draft: %v", err)
	}
	head, err := svc.Get(ctx, tnt, policyKey, 1)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	badDims := append(append([]model.Dimension(nil), head.Dimensions...),
		model.Dimension{Key: "loyalty", Title: "Loyalty", Formal: true, Rationale: "retention risk"})
	_, err = svc.ReviseRules(ctx, ReviseRequest{
		Tenant: tnt, PolicyKey: policyKey, Actor: actor,
		Dimensions: badDims, EvidenceRules: head.EvidenceRules,
		AttributionRules: head.AttributionRules, ConfidenceRules: head.ConfidenceRules,
	})
	if !errors.Is(err, model.ErrForbiddenDimension) {
		t.Fatalf("ReviseRules with a forbidden dimension = %v, want ErrForbiddenDimension", err)
	}
}

// --- shadow-cycle gate: the strict deterministic state machine --------------

func TestShadowCycleGateStateMachine(t *testing.T) {
	// Each case runs a fresh draft, opens cycle 1, and completes it with the
	// mutated gate inputs; wantStatus is the resulting policy status.
	cases := []struct {
		name       string
		mutate     func(*CompleteShadowRequest)
		wantStatus model.Status // shadow => needs a mandatory cycle 2; reviewing => publishable
	}{
		{"all_gates_pass", func(r *CompleteShadowRequest) {}, model.StatusReviewing},
		{"coverage_at_threshold", func(r *CompleteShadowRequest) { r.EvidenceCoverage = model.MinEvidenceCoverage }, model.StatusReviewing},
		{"coverage_below_threshold", func(r *CompleteShadowRequest) { r.EvidenceCoverage = model.MinEvidenceCoverage - 0.001 }, model.StatusShadow},
		{"manager_not_confirmed", func(r *CompleteShadowRequest) { r.ManagerConfirmed = false }, model.StatusShadow},
		{"owner_not_confirmed", func(r *CompleteShadowRequest) { r.PolicyOwnerConfirmed = false }, model.StatusShadow},
		{"unresolved_correction", func(r *CompleteShadowRequest) { r.UnresolvedCorrections = 1 }, model.StatusShadow},
		{"unresolved_calibration", func(r *CompleteShadowRequest) { r.UnresolvedCalibrations = 1 }, model.StatusShadow},
		{"rule_changed_after_start", func(r *CompleteShadowRequest) { r.RuleChangedAfterStart = true }, model.StatusShadow},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			svc, _, actor := newService(t, groundedGraph(), &stubReviews{})
			if _, err := svc.Draft(ctx, draftReq(actor)); err != nil {
				t.Fatalf("Draft: %v", err)
			}
			if _, _, err := svc.BeginShadowCycle(ctx, beginReq(actor, 1)); err != nil {
				t.Fatalf("Begin: %v", err)
			}
			req := passingComplete(actor, 1)
			tc.mutate(&req)
			p, err := svc.CompleteShadowCycle(ctx, req)
			if err != nil {
				t.Fatalf("Complete: %v", err)
			}
			if p.Status != tc.wantStatus {
				t.Fatalf("status = %q, want %q", p.Status, tc.wantStatus)
			}
		})
	}
}

func TestShadowCycleLowConfidenceFormalBlocks(t *testing.T) {
	ctx := context.Background()
	svc, _, actor := newService(t, groundedGraph(), &stubReviews{})
	req := draftReq(actor)
	req.Confidence = map[model.DimensionKey]model.ConfidenceLevel{model.DimQuality: model.ConfidenceLow}
	if _, err := svc.Draft(ctx, req); err != nil {
		t.Fatalf("Draft: %v", err)
	}
	if _, _, err := svc.BeginShadowCycle(ctx, beginReq(actor, 1)); err != nil {
		t.Fatalf("Begin: %v", err)
	}
	p, err := svc.CompleteShadowCycle(ctx, passingComplete(actor, 1))
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if p.Status != model.StatusShadow {
		t.Fatalf("a formal dimension with low confidence must block publication, got %q", p.Status)
	}
}

// --- mandatory second cycle, failed second cycle -> draft, revision restart --

func TestShadowRequiresSecondCycleThenReturnsToDraft(t *testing.T) {
	ctx := context.Background()
	svc, _, actor := newService(t, groundedGraph(), &stubReviews{})
	if _, err := svc.Draft(ctx, draftReq(actor)); err != nil {
		t.Fatalf("Draft: %v", err)
	}
	if _, _, err := svc.BeginShadowCycle(ctx, beginReq(actor, 1)); err != nil {
		t.Fatalf("Begin1: %v", err)
	}
	// Cycle 1 fails a gate -> stays in shadow, mandatory cycle 2.
	fail := passingComplete(actor, 1)
	fail.ManagerConfirmed = false
	p, err := svc.CompleteShadowCycle(ctx, fail)
	if err != nil {
		t.Fatalf("Complete1: %v", err)
	}
	if p.Status != model.StatusShadow {
		t.Fatalf("failed cycle 1 status = %q, want shadow", p.Status)
	}
	// Cannot jump to publication after a failed cycle 1.
	if _, err := svc.ConfirmPublication(ctx, ConfirmPublicationRequest{Tenant: tnt, PolicyKey: policyKey, Revision: 1, Actor: actor}); err == nil {
		t.Fatal("must not publish after a failed cycle 1 without a passing cycle 2")
	}

	// Begin the mandatory cycle 2.
	_, cyc2, err := svc.BeginShadowCycle(ctx, beginReq(actor, 1))
	if err != nil {
		t.Fatalf("Begin2: %v", err)
	}
	if cyc2.Cycle != 2 {
		t.Fatalf("second cycle number = %d, want 2", cyc2.Cycle)
	}
	// Cycle 2 also fails -> returns to draft, and NO automatic third cycle.
	fail2 := passingComplete(actor, 2)
	fail2.EvidenceCoverage = 0.10
	p, err = svc.CompleteShadowCycle(ctx, fail2)
	if err != nil {
		t.Fatalf("Complete2: %v", err)
	}
	if p.Status != model.StatusDraft {
		t.Fatalf("failed cycle 2 status = %q, want draft", p.Status)
	}
	// No automatic third cycle: a third BeginShadowCycle on this revision is refused.
	if _, _, err := svc.BeginShadowCycle(ctx, beginReq(actor, 1)); !errors.Is(err, ErrNoThirdCycle) {
		t.Fatalf("third cycle = %v, want ErrNoThirdCycle", err)
	}
}

func TestRuleRevisionRestartsAtCycleOne(t *testing.T) {
	ctx := context.Background()
	svc, _, actor := newService(t, groundedGraph(), &stubReviews{})
	if _, err := svc.Draft(ctx, draftReq(actor)); err != nil {
		t.Fatalf("Draft: %v", err)
	}
	if _, _, err := svc.BeginShadowCycle(ctx, beginReq(actor, 1)); err != nil {
		t.Fatalf("Begin: %v", err)
	}
	head, err := svc.Get(ctx, tnt, policyKey, 1)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}

	// A rule edit creates revision 2 (in draft), restarting the shadow count.
	editedConf := append([]model.ConfidenceRule(nil), head.ConfidenceRules...)
	editedConf[0].Rationale = "recalibrated after the first shadow window"
	rev2, err := svc.ReviseRules(ctx, ReviseRequest{
		Tenant: tnt, PolicyKey: policyKey, Actor: actor,
		Dimensions: head.Dimensions, EvidenceRules: head.EvidenceRules,
		AttributionRules: head.AttributionRules, ConfidenceRules: editedConf,
	})
	if err != nil {
		t.Fatalf("ReviseRules: %v", err)
	}
	if rev2.Revision != 2 || rev2.Status != model.StatusDraft {
		t.Fatalf("rule edit must create a fresh draft revision 2, got rev=%d status=%q", rev2.Revision, rev2.Status)
	}
	if len(rev2.ShadowCycles) != 0 {
		t.Fatalf("a new revision must restart the shadow count at zero, got %d cycles", len(rev2.ShadowCycles))
	}
	if rev2.RuleDigest() == head.RuleDigest() {
		t.Fatal("a rule edit must change the rule digest")
	}

	// Revision 1 remains immutable (its recorded cycle history is untouched).
	old, err := svc.Get(ctx, tnt, policyKey, 1)
	if err != nil {
		t.Fatalf("Get rev1: %v", err)
	}
	if len(old.ShadowCycles) != 1 {
		t.Fatalf("revision 1 shadow history mutated: %d cycles", len(old.ShadowCycles))
	}

	// The new revision's shadow cycle is numbered 1 (restarted).
	_, cyc, err := svc.BeginShadowCycle(ctx, BeginShadowRequest{
		Tenant: tnt, PolicyKey: policyKey, Revision: 2,
		PeriodStart: fixedNow(), PeriodEnd: fixedNow().Add(720 * time.Hour), Actor: actor,
	})
	if err != nil {
		t.Fatalf("Begin rev2: %v", err)
	}
	if cyc.Cycle != 1 {
		t.Fatalf("new revision must restart at cycle 1, got %d", cyc.Cycle)
	}
}

// The service reads the graph ONLY through the closed typed operation set.
func TestServiceUsesOnlyTypedGraphOperations(t *testing.T) {
	ctx := context.Background()
	graph := groundedGraph()
	svc, _, actor := newService(t, graph, &stubReviews{})
	if _, err := svc.Draft(ctx, draftReq(actor)); err != nil {
		t.Fatalf("Draft: %v", err)
	}
	if len(graph.calls) == 0 {
		t.Fatal("Draft must ground candidate dimensions via the typed graph surface")
	}
	for _, c := range graph.calls {
		if !c.Operation.Valid() {
			t.Fatalf("service issued a non-typed graph operation %q", c.Operation)
		}
		if c.Tenant != tnt {
			t.Fatalf("graph query tenant = %q, want %q (tenant-scoped)", c.Tenant, tnt)
		}
	}
}

// --- FIX-A2 / B3 / D helpers ------------------------------------------------

// newServiceWithStore wires a service over a concrete MemoryPolicyStore so a test
// can seed a revision into a chosen lifecycle status directly. The store enforces
// immutability; the legality of a transition is a service precondition, which is
// exactly what these tests exercise.
func newServiceWithStore(t *testing.T, graph GraphQuerier, reviews GovernanceReviews) (*Service, *MemoryPolicyStore, governance.Actor) {
	t.Helper()
	gov, _, actor := newGov()
	store := NewMemoryPolicyStore(fixedNow)
	svc := NewService(graph, gov, reviews, store, fixedNow)
	return svc, store, actor
}

// seedStatus inserts revision 1 of the standard policy and advances it directly
// to the requested status through the store. checkTransition permits any status
// change out of the non-terminal draft state as long as the rule set is
// unchanged, so one UpdateRevision reaches any status — which is precisely why
// the LEGALITY of a transition must be (and is) enforced in the service layer.
func seedStatus(t *testing.T, store *MemoryPolicyStore, status model.Status) {
	t.Helper()
	ctx := context.Background()
	if _, err := store.CreateRevision(ctx, minimalPolicy(tnt, policyKey, 1)); err != nil {
		t.Fatalf("seed create: %v", err)
	}
	if status == model.StatusDraft {
		return
	}
	got, err := store.GetRevision(ctx, tnt, policyKey, 1)
	if err != nil {
		t.Fatalf("seed get: %v", err)
	}
	got.Status = status
	if status == model.StatusReviewing || status == model.StatusPublished || status == model.StatusRetired {
		got.GovernanceChangeID = "chg-seed" // a governed lineage names its review
	}
	got.UpdatedAt = fixedNow().Add(time.Hour)
	if _, err := store.UpdateRevision(ctx, got); err != nil {
		t.Fatalf("seed transition to %q: %v", status, err)
	}
}

// capturingGov records the SuggestionInput the publication handoff files so a
// test can assert the reviewable rubric it carries.
type capturingGov struct{ inputs []governance.SuggestionInput }

func (g *capturingGov) Suggest(_ context.Context, _ governance.Actor, in governance.SuggestionInput) (govmodel.ChangeDraft, error) {
	g.inputs = append(g.inputs, in)
	return govmodel.ChangeDraft{ChangeID: "chg-captured"}, nil
}

// --- FIX-A2: illegal lifecycle states are rejected in the service -----------

func TestConfirmPublicationRejectsNonReviewing(t *testing.T) {
	for _, st := range []model.Status{model.StatusDraft, model.StatusShadow, model.StatusPublished, model.StatusRetired} {
		t.Run(string(st), func(t *testing.T) {
			svc, store, actor := newServiceWithStore(t, groundedGraph(), &stubReviews{approved: true})
			seedStatus(t, store, st)
			if _, err := svc.ConfirmPublication(context.Background(), ConfirmPublicationRequest{Tenant: tnt, PolicyKey: policyKey, Revision: 1, Actor: actor}); !errors.Is(err, ErrInvalidState) {
				t.Fatalf("ConfirmPublication from %q = %v, want ErrInvalidState", st, err)
			}
		})
	}
}

func TestRetireRejectsNonPublished(t *testing.T) {
	for _, st := range []model.Status{model.StatusDraft, model.StatusShadow, model.StatusReviewing} {
		t.Run(string(st), func(t *testing.T) {
			svc, store, actor := newServiceWithStore(t, groundedGraph(), &stubReviews{})
			seedStatus(t, store, st)
			if _, err := svc.Retire(context.Background(), RetireRequest{Tenant: tnt, PolicyKey: policyKey, Revision: 1, Actor: actor}); !errors.Is(err, ErrInvalidState) {
				t.Fatalf("Retire from %q = %v, want ErrInvalidState", st, err)
			}
		})
	}
}

func TestBeginShadowCycleRejectsIllegalStatus(t *testing.T) {
	for _, st := range []model.Status{model.StatusReviewing, model.StatusPublished, model.StatusRetired} {
		t.Run(string(st), func(t *testing.T) {
			svc, store, actor := newServiceWithStore(t, groundedGraph(), &stubReviews{})
			seedStatus(t, store, st)
			if _, _, err := svc.BeginShadowCycle(context.Background(), beginReq(actor, 1)); !errors.Is(err, ErrInvalidState) {
				t.Fatalf("BeginShadowCycle from %q = %v, want ErrInvalidState", st, err)
			}
		})
	}
}

func TestCompleteShadowCycleRejectsNonShadow(t *testing.T) {
	for _, st := range []model.Status{model.StatusDraft, model.StatusReviewing} {
		t.Run(string(st), func(t *testing.T) {
			svc, store, actor := newServiceWithStore(t, groundedGraph(), &stubReviews{})
			seedStatus(t, store, st)
			if _, err := svc.CompleteShadowCycle(context.Background(), passingComplete(actor, 1)); !errors.Is(err, ErrInvalidState) {
				t.Fatalf("CompleteShadowCycle from %q = %v, want ErrInvalidState", st, err)
			}
		})
	}
}

// --- FIX-B3: the publication handoff carries the full reviewable rubric ------

func TestPublicationHandoffCarriesRubric(t *testing.T) {
	ctx := context.Background()
	gov := &capturingGov{}
	store := NewMemoryPolicyStore(fixedNow)
	actor := governance.Actor{EnterpriseID: tnt, UserID: "system:assessment", OrgUnitIDs: []string{org}, Permissions: []string{"suggest"}}
	svc := NewService(groundedGraph(), gov, &stubReviews{}, store, fixedNow)

	if _, err := svc.Draft(ctx, draftReq(actor)); err != nil {
		t.Fatalf("Draft: %v", err)
	}
	if _, _, err := svc.BeginShadowCycle(ctx, beginReq(actor, 1)); err != nil {
		t.Fatalf("Begin: %v", err)
	}
	p, err := svc.CompleteShadowCycle(ctx, passingComplete(actor, 1))
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if p.Status != model.StatusReviewing {
		t.Fatalf("a passing cycle must move to reviewing, got %q", p.Status)
	}
	if len(gov.inputs) != 1 {
		t.Fatalf("publication handoff must file exactly one governed suggestion, got %d", len(gov.inputs))
	}

	var body map[string]any
	if err := json.Unmarshal(gov.inputs[0].ProposedContent, &body); err != nil {
		t.Fatalf("rubric is not valid JSON: %v", err)
	}
	dims, ok := body["dimensions"].([]any)
	if !ok || len(dims) != 6 {
		t.Fatalf("rubric must carry the six dimensions, got %v", body["dimensions"])
	}
	keys := map[string]bool{}
	for _, d := range dims {
		dm, ok := d.(map[string]any)
		if !ok {
			t.Fatalf("dimension entry is not an object: %v", d)
		}
		key, _ := dm["key"].(string)
		title, _ := dm["title"].(string)
		if key == "" || title == "" {
			t.Fatalf("dimension entry missing key/title: %v", dm)
		}
		keys[key] = true
	}
	if !keys[string(model.DimOutcomeCompletion)] || !keys[string(model.DimRiskCompliance)] {
		t.Fatalf("rubric must expose the built-in dimension keys, got %v", keys)
	}
	for _, k := range []string{"evidence_rules", "attribution_rules", "confidence_rules"} {
		arr, ok := body[k].([]any)
		if !ok || len(arr) == 0 {
			t.Fatalf("rubric must carry %s summaries, got %v", k, body[k])
		}
	}
	// The opaque count must be gone (replaced by the reviewable rubric).
	if _, present := body["dimension_count"]; present {
		t.Fatal("handoff must replace the opaque dimension_count with the rubric")
	}
}

// --- FIX-D: a no-op revision is rejected ------------------------------------

func TestReviseRulesRejectsNoOp(t *testing.T) {
	ctx := context.Background()
	svc, _, actor := newService(t, groundedGraph(), &stubReviews{})
	if _, err := svc.Draft(ctx, draftReq(actor)); err != nil {
		t.Fatalf("Draft: %v", err)
	}
	head, err := svc.Get(ctx, tnt, policyKey, 1)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	// Byte-identical rules -> no-op -> rejected (must not mint a fresh revision
	// that would reset the shadow count).
	if _, err := svc.ReviseRules(ctx, ReviseRequest{
		Tenant: tnt, PolicyKey: policyKey, Actor: actor,
		Dimensions: head.Dimensions, EvidenceRules: head.EvidenceRules,
		AttributionRules: head.AttributionRules, ConfidenceRules: head.ConfidenceRules,
	}); !errors.Is(err, ErrNoRuleChange) {
		t.Fatalf("no-op ReviseRules = %v, want ErrNoRuleChange", err)
	}

	// A genuine change still succeeds and creates revision 2.
	editedConf := append([]model.ConfidenceRule(nil), head.ConfidenceRules...)
	editedConf[0].Rationale = "recalibrated after the first shadow window"
	rev2, err := svc.ReviseRules(ctx, ReviseRequest{
		Tenant: tnt, PolicyKey: policyKey, Actor: actor,
		Dimensions: head.Dimensions, EvidenceRules: head.EvidenceRules,
		AttributionRules: head.AttributionRules, ConfidenceRules: editedConf,
	})
	if err != nil {
		t.Fatalf("genuine ReviseRules: %v", err)
	}
	if rev2.Revision != 2 {
		t.Fatalf("genuine rule edit must create revision 2, got %d", rev2.Revision)
	}
}
