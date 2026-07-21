package app

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	assessmentmodel "github.com/astraclawteam/agentatlas/sdk/go/assessment"
	"github.com/astraclawteam/agentatlas/sdk/go/nexus"
	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/assessment"
	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/browsersession"
	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/governance"
	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/nexusclient"
)

const (
	assessTenant  = "ent-18d"
	assessSubject = "actor-op-7"
	assessPolicy  = "assess.op"
	assessOrgVer  = 7
	assessNarr    = "MANAGER-ONLY narrative sentinel B plus"
	assessMgrID   = "mgr-secret-lead"
)

func assessNow() time.Time { return time.Date(2026, 7, 15, 9, 0, 0, 0, time.UTC) }

func appConfidence() assessmentmodel.ConfidenceExplanation {
	comps := make([]assessmentmodel.ConfidenceComponent, 0, len(assessmentmodel.ConfidenceKinds()))
	for _, k := range assessmentmodel.ConfidenceKinds() {
		comps = append(comps, assessmentmodel.ConfidenceComponent{Kind: k, Score: 0.9})
	}
	return assessmentmodel.ConfidenceExplanation{Level: assessmentmodel.ConfidenceHigh, Components: comps}
}

func appAssessment(subject string, version int) assessmentmodel.WorkAssessment {
	wa := assessmentmodel.WorkAssessment{
		Tenant: assessTenant, Org: "org:team-a", Subject: subject, Level: assessmentmodel.LevelIndividual,
		PolicyKey: assessPolicy, PolicyRevision: 1, Version: version, Formal: true, OrgVersion: 3,
		Sources: assessmentmodel.SourceBinding{
			OrgVersion:        3,
			JobResponsibility: assessmentmodel.VersionedRef{Key: "resp.op", Version: 1},
			OrgGoal:           assessmentmodel.VersionedRef{Key: "goal.throughput", Version: 1},
			AcceptanceReview:  assessmentmodel.VersionedRef{Key: "review.qc", Version: 1},
		},
		Period: assessmentmodel.Period{Start: assessNow().Add(-720 * time.Hour), End: assessNow().Add(-time.Hour)},
		Graph:  assessmentmodel.GraphSchema{Provider: "apache_age", SchemaVersion: "v1", ProtocolVersion: "v1", GraphName: "outcome_graph", Watermark: 42},
		Dimensions: []assessmentmodel.DimensionResult{{
			Dimension: assessmentmodel.DimOutcomeCompletion, State: assessmentmodel.StateAssessed, Attribution: assessmentmodel.AttrSharedOnce,
			CountedOutcomeKeys: []string{"outcome-a"}, SatisfiedOutcomes: 1,
			Evidence:   []assessmentmodel.Evidence{{Tier: assessmentmodel.TierVerifiedOutcome, Handle: "outcome-a", Kind: "outcome", Revision: 1, Authority: "mes.official", SignatureKeyID: "sig-1", Verified: true}},
			Confidence: appConfidence(),
		}},
		Manager:   assessmentmodel.ManagerConfirmation{Confirmed: true, Manager: assessMgrID, ConfirmedAt: assessNow()},
		CreatedAt: assessNow(),
		Narrative: assessNarr,
	}
	wa = wa.Normalize()
	wa.ID = wa.DerivedID()
	return wa
}

// appFakeOrg is a deterministic OrgResolver double for the app tests.
type appFakeOrg struct {
	parties map[string]governance.Party
	owned   map[string][]string
}

func (f appFakeOrg) PartyOf(_ context.Context, _, userID string) (governance.Party, error) {
	return f.parties[userID], nil
}
func (f appFakeOrg) CurrentOrgVersion(_ context.Context, _ string) (int64, error) {
	return assessOrgVer, nil
}
func (f appFakeOrg) OwnedPolicyKeys(_ context.Context, _, managerID string) ([]string, error) {
	return f.owned[managerID], nil
}

func appOrgResolver() appFakeOrg {
	return appFakeOrg{
		parties: map[string]governance.Party{
			assessSubject: {UserID: assessSubject, OrgUnitID: "org:team-a", OrgPath: []string{"org:root", "org:div"}, OrgVersion: assessOrgVer},
			"u:div-lead":  {UserID: "u:div-lead", OrgUnitID: "org:div", OrgPath: []string{"org:root"}, OrgVersion: assessOrgVer},
			"u:peer":      {UserID: "u:peer", OrgUnitID: "org:team-a", OrgPath: []string{"org:root", "org:div"}, OrgVersion: assessOrgVer},
		},
		owned: map[string][]string{"u:div-lead": {assessPolicy}, "u:peer": {assessPolicy}},
	}
}

// stubReeval is a deterministic Reevaluator for the correction accept path (the
// full 18C evaluator's graph/outcome seeding lives in the assessment package's
// own tests). It returns a NEW version for the same subject.
type stubReeval struct {
	base assessmentmodel.WorkAssessment
}

func (s stubReeval) Evaluate(_ context.Context, req assessment.EvaluateRequest) (assessmentmodel.WorkAssessment, error) {
	wa := s.base
	wa.Subject = req.Subject
	wa.Version = req.Version
	wa.Period = req.Period
	wa = wa.Normalize()
	wa.ID = wa.DerivedID()
	return wa, nil
}

func assessTicketMock() *nexusclient.Mock {
	mock := nexusclient.NewMock()
	mock.Tickets["tick_emp"] = nexus.VerifyTicketResponse{Valid: true, EnterpriseID: assessTenant, ActorUserID: assessSubject}
	mock.Tickets["tick_other"] = nexus.VerifyTicketResponse{Valid: true, EnterpriseID: assessTenant, ActorUserID: "actor-other"}
	return mock
}

func managerCtx(req *http.Request, userID, id string) *http.Request {
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("id", id)
	ctx := context.WithValue(req.Context(), chi.RouteCtxKey, rctx)
	session := browsersession.Session{Identity: browsersession.Identity{EnterpriseID: assessTenant, UserID: userID, OrgVersion: assessOrgVer, OrgUnitIDs: []string{"org:div"}}}
	ctx = context.WithValue(ctx, browserActorKey{}, session)
	return req.WithContext(ctx)
}

// TestAssessmentContract proves the served assessment surfaces honor the
// visibility contract: the employee endpoint never leaks the score/level/notes,
// a non-subject cannot read, and the manager endpoint reveals the detail only
// after the exact hierarchy + policy authorization.
func TestAssessmentContract(t *testing.T) {
	ctx := context.Background()
	results := assessment.NewMemoryResultStore(assessNow)
	v1, err := results.AppendAssessment(ctx, appAssessment(assessSubject, 1))
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	corr := assessment.NewMemoryCorrectionStore(assessNow)
	corrections := assessment.NewCorrectionService(corr, results, stubReeval{base: appAssessment(assessSubject, 1)}, assessNow)

	router := NewRouter(RouterDeps{Evidence: &fakeFrozenEvidence{}, Nexus: assessTicketMock(), AssessmentResults: results, AssessmentCorrections: corrections})
	srv := httptest.NewServer(router)
	t.Cleanup(srv.Close)

	// (1) EMPLOYEE reads own assessment: facts, no score/level/manager notes.
	body := doGet(t, srv.URL+"/v1/assessments/"+v1.ID, "tick_emp")
	for _, forbidden := range []string{"satisfied_outcomes", "components", "narrative", assessNarr, assessMgrID} {
		if strings.Contains(body, forbidden) {
			t.Fatalf("employee endpoint leaked %q:\n%s", forbidden, body)
		}
	}
	if !strings.Contains(body, "completed_outcome_refs") {
		t.Fatalf("employee endpoint must surface completion facts:\n%s", body)
	}

	// (2) A non-subject cannot read another employee's assessment (404, not enumerable).
	if code := doGetStatus(t, srv.URL+"/v1/assessments/"+v1.ID, "tick_other"); code != http.StatusNotFound {
		t.Fatalf("cross-employee read status = %d, want 404", code)
	}

	// (3) MANAGER within scope + owning the policy sees the full detail (score + notes).
	managerSvc := assessment.NewManagerVisibilityService(results, appOrgResolver())
	h := &assessmentAgentHandler{manager: managerSvc}

	rec := httptest.NewRecorder()
	h.detail(rec, managerCtx(httptest.NewRequest(http.MethodGet, "/api/assessments/"+v1.ID, nil), "u:div-lead", v1.ID))
	if rec.Code != http.StatusOK {
		t.Fatalf("authorized manager status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	for _, must := range []string{"satisfied_outcomes", assessNarr, assessMgrID} {
		if !strings.Contains(rec.Body.String(), must) {
			t.Fatalf("authorized manager detail must include %q:\n%s", must, rec.Body.String())
		}
	}

	// (4) A peer (same org unit, not upward) is refused (403).
	rec = httptest.NewRecorder()
	h.detail(rec, managerCtx(httptest.NewRequest(http.MethodGet, "/api/assessments/"+v1.ID, nil), "u:peer", v1.ID))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("peer manager status = %d, want 403", rec.Code)
	}
}

// TestAssessmentAudit proves the correction audit trail through the served
// surface: an employee submits a correction (the old version is untouched), an
// accepted correction produces a NEW version, and the employee's correction
// outcome reflects it while the old version stays retrievable and immutable.
func TestAssessmentAudit(t *testing.T) {
	ctx := context.Background()
	results := assessment.NewMemoryResultStore(assessNow)
	v1, err := results.AppendAssessment(ctx, appAssessment(assessSubject, 1))
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	oldDigest := v1.Digest()
	corr := assessment.NewMemoryCorrectionStore(assessNow)
	corrections := assessment.NewCorrectionService(corr, results, stubReeval{base: appAssessment(assessSubject, 1)}, assessNow)

	router := NewRouter(RouterDeps{Evidence: &fakeFrozenEvidence{}, Nexus: assessTicketMock(), AssessmentResults: results, AssessmentCorrections: corrections})
	srv := httptest.NewServer(router)
	t.Cleanup(srv.Close)

	// Employee submits a correction (served). The old version must be unchanged.
	created := doPost(t, srv.URL+"/v1/assessments/"+v1.ID+"/corrections", "tick_emp",
		`{"kind":"challenge_attribution","dimension":"outcome_completion","challenged_handle":"outcome-a","rationale":"outcome-a is not mine"}`)
	correctionID, _ := created["id"].(string)
	if correctionID == "" {
		t.Fatalf("submit did not return a correction id: %v", created)
	}
	if again, _ := results.GetAssessment(ctx, assessTenant, v1.ID); again.Digest() != oldDigest {
		t.Fatal("submitting a correction must not mutate the old assessment")
	}

	// A manager/governed decision ACCEPTS it -> a new immutable version.
	_, newWA, err := corrections.Resolve(ctx, assessment.ResolveCorrectionRequest{
		Tenant: assessTenant, CorrectionID: correctionID, Accept: true, DecidedBy: "u:div-lead",
		Reason: "upheld", Reevaluation: assessment.EvaluateRequest{Policy: assessmentmodel.Policy{PolicyKey: assessPolicy, Revision: 1}},
	})
	if err != nil {
		t.Fatalf("resolve accept: %v", err)
	}
	if newWA.Version != 2 || newWA.ID == v1.ID {
		t.Fatalf("accepted correction must produce a new version, got v%d id=%s", newWA.Version, newWA.ID)
	}

	// The employee's correction outcome (served) reflects the acceptance + new
	// version reference — but not the manager identity.
	outcome := doGet(t, srv.URL+"/v1/assessments/"+v1.ID+"/corrections/"+correctionID, "tick_emp")
	if !strings.Contains(outcome, "accepted") || !strings.Contains(outcome, newWA.ID) {
		t.Fatalf("correction outcome must show accepted + the new version ref:\n%s", outcome)
	}
	if strings.Contains(outcome, "u:div-lead") {
		t.Fatalf("correction outcome must not leak the manager identity:\n%s", outcome)
	}

	// The OLD version stays retrievable and immutable through the served surface.
	if again, _ := results.GetAssessment(ctx, assessTenant, v1.ID); again.Digest() != oldDigest {
		t.Fatal("the old version must stay immutable after an accepted correction")
	}
	if code := doGetStatus(t, srv.URL+"/v1/assessments/"+v1.ID, "tick_emp"); code != http.StatusOK {
		t.Fatalf("old version must remain readable, status = %d", code)
	}
}

// --- HTTP helpers ------------------------------------------------------------

func doGet(t *testing.T, url, ticket string) string {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, url, nil)
	req.Header.Set("X-Nexus-Ticket", ticket)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s status = %d, want 200", url, resp.StatusCode)
	}
	raw, _ := io.ReadAll(resp.Body)
	return string(raw)
}

func doGetStatus(t *testing.T, url, ticket string) int {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, url, nil)
	req.Header.Set("X-Nexus-Ticket", ticket)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	resp.Body.Close()
	return resp.StatusCode
}

func doPost(t *testing.T, url, ticket, body string) map[string]any {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, url, strings.NewReader(body))
	req.Header.Set("X-Nexus-Ticket", ticket)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("POST %s status = %d, want 201: %s", url, resp.StatusCode, string(raw))
	}
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return out
}
