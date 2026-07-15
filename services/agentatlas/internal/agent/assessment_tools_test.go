package agent_test

// This is a BLACK-BOX test (package agent_test) so it can exercise the manager
// assessment-query tool against the REAL deterministic visibility service
// (internal/assessment) without an import cycle: the agent package itself never
// imports internal/assessment.

import (
	"context"
	"errors"
	"testing"
	"time"

	assessmentmodel "github.com/astraclawteam/agentatlas/sdk/go/assessment"
	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/agent"
	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/assessment"
	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/governance"
)

func toolFixedNow() time.Time { return time.Date(2026, 7, 15, 9, 0, 0, 0, time.UTC) }

func toolConfidence() assessmentmodel.ConfidenceExplanation {
	comps := make([]assessmentmodel.ConfidenceComponent, 0, len(assessmentmodel.ConfidenceKinds()))
	for _, k := range assessmentmodel.ConfidenceKinds() {
		comps = append(comps, assessmentmodel.ConfidenceComponent{Kind: k, Score: 0.9})
	}
	return assessmentmodel.ConfidenceExplanation{Level: assessmentmodel.ConfidenceHigh, Components: comps}
}

func toolAssessment(tenant, subject, policyKey string) assessmentmodel.WorkAssessment {
	wa := assessmentmodel.WorkAssessment{
		Tenant: tenant, Org: "org:team-a", Subject: subject, Level: assessmentmodel.LevelIndividual,
		PolicyKey: policyKey, PolicyRevision: 1, Version: 1, Formal: true, OrgVersion: 3,
		Sources: assessmentmodel.SourceBinding{
			OrgVersion:        3,
			JobResponsibility: assessmentmodel.VersionedRef{Key: "resp.op", Version: 1},
			OrgGoal:           assessmentmodel.VersionedRef{Key: "goal.throughput", Version: 1},
			AcceptanceReview:  assessmentmodel.VersionedRef{Key: "review.qc", Version: 1},
		},
		Period: assessmentmodel.Period{Start: toolFixedNow().Add(-720 * time.Hour), End: toolFixedNow().Add(-time.Hour)},
		Graph:  assessmentmodel.GraphSchema{Provider: "apache_age", SchemaVersion: "v1", ProtocolVersion: "v1", GraphName: "outcome_graph", Watermark: 42},
		Dimensions: []assessmentmodel.DimensionResult{{
			Dimension: assessmentmodel.DimOutcomeCompletion, State: assessmentmodel.StateAssessed, Attribution: assessmentmodel.AttrSharedOnce,
			CountedOutcomeKeys: []string{"outcome-a"}, SatisfiedOutcomes: 1,
			Evidence:   []assessmentmodel.Evidence{{Tier: assessmentmodel.TierVerifiedOutcome, Handle: "outcome-a", Kind: "outcome", Revision: 1, Authority: "mes.official", SignatureKeyID: "sig-1", Verified: true}},
			Confidence: toolConfidence(),
		}},
		Manager:   assessmentmodel.ManagerConfirmation{Confirmed: true, Manager: "mgr-line-lead", ConfirmedAt: toolFixedNow()},
		CreatedAt: toolFixedNow(),
	}
	wa = wa.Normalize()
	wa.ID = wa.DerivedID()
	return wa
}

// fakeOrg is a deterministic OrgResolver double: the org facts come from the
// trusted structure, never from the LLM.
type fakeOrg struct {
	parties map[string]governance.Party
	orgVer  int64
	owned   map[string][]string
}

func (f fakeOrg) PartyOf(_ context.Context, _, userID string) (governance.Party, error) {
	return f.parties[userID], nil
}
func (f fakeOrg) CurrentOrgVersion(_ context.Context, _ string) (int64, error) { return f.orgVer, nil }
func (f fakeOrg) OwnedPolicyKeys(_ context.Context, _, managerID string) ([]string, error) {
	return f.owned[managerID], nil
}

// boundPort binds a TRUSTED (tenant, managerID) to the deterministic visibility
// service and adapts it to the tool's SDK-typed AssessmentQueryPort; the LLM args
// can never change the bound identity.
type boundPort struct {
	svc               *assessment.ManagerVisibilityService
	tenant, managerID string
}

func (p boundPort) ManagerView(ctx context.Context, id string) (assessmentmodel.WorkAssessment, error) {
	view, err := p.svc.ManagerView(ctx, p.tenant, p.managerID, id)
	if err != nil {
		return assessmentmodel.WorkAssessment{}, err
	}
	return view.Assessment, nil
}

const (
	toolTenant  = "ent-tool"
	toolSubject = "actor-op-7"
	toolPolicy  = "assess.op"
	toolOrgVer  = 7
)

func newToolEnv(t *testing.T, managerID string) (boundPort, assessmentmodel.WorkAssessment) {
	t.Helper()
	results := assessment.NewMemoryResultStore(toolFixedNow)
	wa, err := results.AppendAssessment(context.Background(), toolAssessment(toolTenant, toolSubject, toolPolicy))
	if err != nil {
		t.Fatalf("seed assessment: %v", err)
	}
	org := fakeOrg{
		parties: map[string]governance.Party{
			toolSubject:  {UserID: toolSubject, OrgUnitID: "org:team-a", OrgPath: []string{"org:root", "org:div"}, OrgVersion: toolOrgVer},
			"u:div-lead": {UserID: "u:div-lead", OrgUnitID: "org:div", OrgPath: []string{"org:root"}, OrgVersion: toolOrgVer},
			"u:peer":     {UserID: "u:peer", OrgUnitID: "org:team-a", OrgPath: []string{"org:root", "org:div"}, OrgVersion: toolOrgVer},
		},
		orgVer: toolOrgVer,
		owned:  map[string][]string{"u:div-lead": {toolPolicy}, "u:peer": {toolPolicy}},
	}
	svc := assessment.NewManagerVisibilityService(results, org)
	return boundPort{svc: svc, tenant: toolTenant, managerID: managerID}, wa
}

// The tool returns the authorized deterministic projection VERBATIM (no rewrite).
func TestManagerAssessmentQueryReturnsAuthorizedProjection(t *testing.T) {
	ctx := context.Background()
	port, wa := newToolEnv(t, "u:div-lead")
	res, err := agent.ManagerAssessmentQuery(ctx, port, agent.ManagerAssessmentQueryArgs{AssessmentID: wa.ID})
	if err != nil {
		t.Fatalf("authorized query: %v", err)
	}
	if res.Assessment.Digest() != wa.Digest() {
		t.Fatalf("the tool must return the deterministic assessment digest unchanged (no rewrite)")
	}
	if res.Assessment.Dimensions[0].SatisfiedOutcomes != 1 {
		t.Fatalf("the tool must surface the deterministic score, got %d", res.Assessment.Dimensions[0].SatisfiedOutcomes)
	}
}

// The tool cannot broaden hierarchy scope: a peer (same unit, not upward) is
// refused by the deterministic authorization, not served the score.
func TestManagerAssessmentQueryEnforcesScope(t *testing.T) {
	ctx := context.Background()
	port, wa := newToolEnv(t, "u:peer")
	if _, err := agent.ManagerAssessmentQuery(ctx, port, agent.ManagerAssessmentQueryArgs{AssessmentID: wa.ID}); !errors.Is(err, assessment.ErrManagerNotAuthorized) {
		t.Fatalf("peer manager query = %v, want ErrManagerNotAuthorized (scope enforced, no broadening)", err)
	}
}

func TestManagerAssessmentQueryRequiresID(t *testing.T) {
	ctx := context.Background()
	port, _ := newToolEnv(t, "u:div-lead")
	if _, err := agent.ManagerAssessmentQuery(ctx, port, agent.ManagerAssessmentQueryArgs{AssessmentID: ""}); err == nil {
		t.Fatal("empty assessment_id must fail loud")
	}
}

// The tool is constructible as an ADK function tool (registration shape mirrors
// the existing tools).
func TestManagerAssessmentQueryToolConstructs(t *testing.T) {
	port, _ := newToolEnv(t, "u:div-lead")
	tl, err := agent.ManagerAssessmentQueryTool(port)
	if err != nil {
		t.Fatalf("ManagerAssessmentQueryTool: %v", err)
	}
	if tl.Name() != "query_work_assessment" {
		t.Fatalf("tool name = %q, want query_work_assessment", tl.Name())
	}
}
