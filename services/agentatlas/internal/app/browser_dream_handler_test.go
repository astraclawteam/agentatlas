package app

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/astraclawteam/agentatlas/sdk/go/nexus"
	db "github.com/astraclawteam/agentatlas/services/agentatlas/db/generated"
	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/browsersession"
	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/dream"
	"github.com/go-chi/chi/v5"
)

func browserDreamRequest(method, target string, session browsersession.Session, body string) *http.Request {
	req := httptest.NewRequest(method, target, stringsReader(body))
	return req.WithContext(context.WithValue(req.Context(), browserActorKey{}, session))
}

func TestBrowserDreamReadRequiresExactSessionAndNexusOrganization(t *testing.T) {
	run := dreamRunFixture()
	store := &fakeDreamRunStore{run: run, runs: []db.DreamRun{{ID: run.ID, EnterpriseID: run.EnterpriseID, OrgUnitID: run.OrgUnitID}}}
	auth := &fakeBrowserAuthorizer{}
	h := &browserDreamHandler{store: store, authorizer: auth}
	session := browsersession.Session{Identity: browsersession.Identity{EnterpriseID: "ent-1", UserID: "manager-1", OrgVersion: 7, OrgUnitIDs: []string{"department:rd"}, Permissions: []string{"dream:read"}}, UpstreamAccessToken: "bearer-session"}

	rec := httptest.NewRecorder()
	h.list(rec, browserDreamRequest(http.MethodGet, "/api/dream/runs?org_unit_id=department:rd", session, ""))
	if rec.Code != http.StatusOK || store.listCalls != 1 {
		t.Fatalf("allowed=%d calls=%d body=%s", rec.Code, store.listCalls, rec.Body.String())
	}
	if auth.last.Action != "dream.read" || auth.last.ResourceType != "dream_run" || auth.last.ResourceID != "department:rd" || auth.last.OrgVersion != 7 || auth.last.OrgUnitID != "department:rd" {
		t.Fatalf("authorization binding=%+v", auth.last)
	}

	store.listCalls = 0
	rec = httptest.NewRecorder()
	h.list(rec, browserDreamRequest(http.MethodGet, "/api/dream/runs?org_unit_id=department:other", session, ""))
	if rec.Code != http.StatusForbidden || store.listCalls != 0 {
		t.Fatalf("cross org=%d calls=%d", rec.Code, store.listCalls)
	}
}

func TestBrowserDreamMutationIsCSRFAndIdempotencyReadyAndAppendOnly(t *testing.T) {
	run := dreamRunFixture()
	store := &fakeDreamRunStore{run: run}
	auth := &fakeBrowserAuthorizer{}
	h := &browserDreamHandler{store: store, authorizer: auth}
	session := browsersession.Session{Identity: browsersession.Identity{EnterpriseID: "ent-1", UserID: "manager-1", OrgVersion: 7, OrgUnitIDs: []string{"department:rd"}, Permissions: []string{"dream:annotate"}}, UpstreamAccessToken: "bearer-session"}

	req := browserDreamRequest(http.MethodPost, "/api/dream/runs/run-1/annotations", session, `{"action":"mark_incorrect","comment":"wrong"}`)
	req.Header.Set("Idempotency-Key", "browser-annotation-0001")
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("id", "run-1")
	req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))
	rec := httptest.NewRecorder()
	h.annotate(rec, req)
	if rec.Code != http.StatusCreated || len(store.annotations) != 1 || store.annotations[0].AnnotationType != "mark_incorrect" || store.run.DisplaySummary != "safe display" {
		t.Fatalf("annotation=%d body=%s rows=%+v", rec.Code, rec.Body.String(), store.annotations)
	}
	if auth.last.Action != "dream.annotate" || auth.last.ResourceID != "run-1" || auth.last.OrgUnitID != "department:rd" {
		t.Fatalf("annotation auth=%+v", auth.last)
	}
}

func TestBrowserDreamEvidenceFailsClosedBeforeReturningSanitizedDetail(t *testing.T) {
	run := dreamRunFixture()
	store := &fakeDreamRunStore{run: run}
	nx := &fakeBrowserDreamNexus{detail: "sanitized detail"}
	h := &browserDreamHandler{store: store, authorizer: nx, evidence: nx}
	session := browsersession.Session{Identity: browsersession.Identity{EnterpriseID: "ent-1", UserID: "manager-1", OrgVersion: 7, OrgUnitIDs: []string{"department:rd"}, Permissions: []string{"dream:evidence:read"}}, UpstreamAccessToken: "bearer-session"}

	req := browserDreamRequest(http.MethodPost, "/api/dream/runs/run-1/evidence-access", session, "{}")
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("id", "run-1")
	req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))
	rec := httptest.NewRecorder()
	h.evidenceAccess(rec, req)
	if rec.Code != http.StatusOK || !json.Valid(rec.Body.Bytes()) || !containsBytes(rec.Body.Bytes(), []byte("sanitized detail")) || nx.locates != 1 || nx.reads != 1 || nx.audits != 1 {
		t.Fatalf("evidence=%d body=%s calls=%d/%d/%d", rec.Code, rec.Body.String(), nx.locates, nx.reads, nx.audits)
	}
	if containsBytes(rec.Body.Bytes(), []byte("ev-sealed")) || containsBytes(rec.Body.Bytes(), []byte("grant-secret")) {
		t.Fatalf("raw authorization internals leaked: %s", rec.Body.String())
	}
}

func TestBrowserDreamRerunUsesBearerAuditAndPinnedIdempotency(t *testing.T) {
	run := dreamRunFixture()
	store := &fakeDreamRunStore{run: run}
	nx := &fakeBrowserDreamNexus{}
	rerunner := &fakeDreamRerunner{}
	operations := dream.NewPolicyService(newFakePolicyStore())
	h := &browserDreamHandler{store: store, authorizer: nx, evidence: nx, rerun: rerunner, operations: operations}
	session := browsersession.Session{Identity: browsersession.Identity{EnterpriseID: "ent-1", UserID: "manager-1", OrgVersion: 7, OrgUnitIDs: []string{"department:rd"}, Permissions: []string{"dream:rerun"}}, UpstreamAccessToken: "bearer-session"}
	req := browserDreamRequest(http.MethodPost, "/api/dream/runs/run-1/reruns", session, "{}")
	req.Header.Set("Idempotency-Key", "browser-rerun-key-0001")
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("id", "run-1")
	req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))
	rec := httptest.NewRecorder()
	h.rerunRun(rec, req)
	if rec.Code != http.StatusAccepted || rerunner.calls != 1 || rerunner.sourceRun != "run-1" || rerunner.key != "browser-rerun-key-0001" || nx.audits != 1 {
		t.Fatalf("rerun=%d body=%s calls=%d audit=%d", rec.Code, rec.Body.String(), rerunner.calls, nx.audits)
	}
}

func TestBrowserDreamPolicyCreateIsDraftOnlyAuthorizedAndAudited(t *testing.T) {
	nx := &fakeBrowserDreamNexus{}
	policies := dream.NewPolicyService(newFakePolicyStore())
	h := &browserDreamHandler{authorizer: nx, evidence: nx, operations: policies}
	session := browsersession.Session{Identity: browsersession.Identity{EnterpriseID: "ent-1", UserID: "manager-1", OrgVersion: 7, OrgUnitIDs: []string{"pg_mes"}, Permissions: []string{"edit"}}, UpstreamAccessToken: "bearer-session"}
	body, _ := json.Marshal(canonicalDreamPolicyBody())
	req := browserDreamRequest(http.MethodPost, "/api/dream/policies", session, string(body))
	req.Header.Set("Idempotency-Key", "browser-policy-key-0001")
	rec := httptest.NewRecorder()
	h.createPolicy(rec, req)
	if rec.Code != http.StatusCreated || nx.audits != 1 || !containsBytes(rec.Body.Bytes(), []byte(`"status":"draft"`)) || containsBytes(rec.Body.Bytes(), []byte(`"status":"published"`)) {
		t.Fatalf("policy=%d body=%s audits=%d", rec.Code, rec.Body.String(), nx.audits)
	}
}

func TestBrowserDreamPolicyGovernedLifecycleAndBackfill(t *testing.T) {
	nx := &fakeBrowserDreamNexus{route: nexus.ApprovalRoute{Mode: "upward_review", RiskLevel: "high", RiskReasons: []string{"high_risk_field:workflow"}, RequesterUserID: "creator-1", ReviewerUserID: "manager-1", OrgPath: []string{"pg_mes", "department:rd"}}}
	policyStore := &browserPolicyStore{fakePolicyStore: newFakePolicyStore()}
	policies := dream.NewPolicyService(policyStore)
	backfiller := &fakeDreamRerunner{}
	h := &browserDreamHandler{authorizer: nx, evidence: nx, operations: policies, backfill: backfiller}
	creator := browsersession.Session{Identity: browsersession.Identity{EnterpriseID: "ent_1", UserID: "creator-1", OrgVersion: 7, OrgUnitIDs: []string{"pg_mes"}, Permissions: []string{"edit", "dream:read"}}, UpstreamAccessToken: "bearer-session"}
	reviewer := browsersession.Session{Identity: browsersession.Identity{EnterpriseID: "ent_1", UserID: "manager-1", OrgVersion: 7, OrgUnitIDs: []string{"pg_mes"}, Permissions: []string{"approve_high_risk"}}, UpstreamAccessToken: "bearer-reviewer"}
	body, _ := json.Marshal(canonicalDreamPolicyBody())
	created := callBrowserPolicy(t, h.createPolicy, http.MethodPost, "/api/dream/policies", creator, "", "lifecycle-create-0001", string(body))
	var view dream.LifecycleView
	if json.Unmarshal(created.Body.Bytes(), &view) != nil || view.ID == "" || view.Status != "draft" {
		t.Fatalf("create=%d %s", created.Code, created.Body.String())
	}
	listed := callBrowserPolicy(t, h.listPolicies, http.MethodGet, "/api/dream/policies?org_unit_id=pg_mes", creator, "", "", "")
	if listed.Code != http.StatusOK || !containsBytes(listed.Body.Bytes(), []byte(`"status":"draft"`)) {
		t.Fatalf("draft list=%d %s", listed.Code, listed.Body.String())
	}
	intruder := browsersession.Session{Identity: browsersession.Identity{EnterpriseID: "ent_1", UserID: "viewer-1", OrgVersion: 7, OrgUnitIDs: []string{"pg_mes"}, Permissions: []string{"dream:read"}}, UpstreamAccessToken: "bearer-viewer"}
	deniedBackfill := callBrowserPolicy(t, h.backfillPolicy, http.MethodPost, "/api/dream/policies/"+view.ID+"/backfills", intruder, view.ID, "denied-backfill-0001", `{"window_start":"2026-07-01T00:00:00Z","window_end":"2026-07-02T00:00:00Z"}`)
	deniedDisable := callBrowserPolicy(t, h.disablePolicy, http.MethodPost, "/api/dream/policies/"+view.ID+"/disable", intruder, view.ID, "denied-disable-00001", `{"revision":0}`)
	if deniedBackfill.Code != http.StatusForbidden || deniedDisable.Code != http.StatusForbidden || backfiller.backfillCalls != 0 {
		t.Fatalf("authorization backfill=%d disable=%d calls=%d", deniedBackfill.Code, deniedDisable.Code, backfiller.backfillCalls)
	}
	reviewed := callBrowserPolicy(t, h.reviewPolicy, http.MethodPost, "/api/dream/policies/"+view.ID+"/review", creator, view.ID, "lifecycle-review-0001", `{"revision":0,"action":"publish"}`)
	if json.Unmarshal(reviewed.Body.Bytes(), &view) != nil || view.ReviewState != "pending" || view.ReviewerUserID != "manager-1" {
		t.Fatalf("review=%d %s", reviewed.Code, reviewed.Body.String())
	}
	decided := callBrowserPolicy(t, h.decidePolicy, http.MethodPost, "/api/dream/policies/"+view.ID+"/decisions", reviewer, view.ID, "lifecycle-decision-01", `{"revision":0,"decision":"approve"}`)
	if json.Unmarshal(decided.Body.Bytes(), &view) != nil || view.ReviewState != "approved" {
		t.Fatalf("decision=%d %s", decided.Code, decided.Body.String())
	}
	published := callBrowserPolicy(t, h.publishPolicy, http.MethodPost, "/api/dream/policies/"+view.ID+"/publish", creator, view.ID, "lifecycle-publish-001", `{"revision":0}`)
	if json.Unmarshal(published.Body.Bytes(), &view) != nil || view.Status != "published" {
		t.Fatalf("publish=%d %s", published.Code, published.Body.String())
	}
	backfill := callBrowserPolicy(t, h.backfillPolicy, http.MethodPost, "/api/dream/policies/"+view.ID+"/backfills", creator, view.ID, "lifecycle-backfill-01", `{"window_start":"2026-07-01T00:00:00Z","window_end":"2026-07-02T00:00:00Z"}`)
	if backfill.Code != http.StatusAccepted || backfiller.backfillCalls != 1 {
		t.Fatalf("backfill=%d %s calls=%d", backfill.Code, backfill.Body.String(), backfiller.backfillCalls)
	}
	disableReview := callBrowserPolicy(t, h.reviewPolicy, http.MethodPost, "/api/dream/policies/"+view.ID+"/review", creator, view.ID, "lifecycle-disable-review", fmt.Sprintf(`{"revision":%d,"action":"disable"}`, view.Revision))
	if json.Unmarshal(disableReview.Body.Bytes(), &view) != nil || view.PendingAction != "disable" {
		t.Fatalf("disable review=%d %s", disableReview.Code, disableReview.Body.String())
	}
	disableDecision := callBrowserPolicy(t, h.decidePolicy, http.MethodPost, "/api/dream/policies/"+view.ID+"/decisions", reviewer, view.ID, "lifecycle-disable-decision", fmt.Sprintf(`{"revision":%d,"decision":"approve"}`, view.Revision))
	if json.Unmarshal(disableDecision.Body.Bytes(), &view) != nil || view.ReviewState != "approved" {
		t.Fatalf("disable decision=%d %s", disableDecision.Code, disableDecision.Body.String())
	}
	disabled := callBrowserPolicy(t, h.disablePolicy, http.MethodPost, "/api/dream/policies/"+view.ID+"/disable", creator, view.ID, "lifecycle-disable-final", fmt.Sprintf(`{"revision":%d}`, view.Revision))
	if json.Unmarshal(disabled.Body.Bytes(), &view) != nil || view.Status != "disabled" {
		t.Fatalf("disable=%d %s", disabled.Code, disabled.Body.String())
	}
}

func callBrowserPolicy(t *testing.T, handler http.HandlerFunc, method, target string, session browsersession.Session, id, key, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := browserDreamRequest(method, target, session, body)
	req.Header.Set("Idempotency-Key", key)
	if id != "" {
		route := chi.NewRouteContext()
		route.URLParams.Add("id", id)
		req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, route))
	}
	rec := httptest.NewRecorder()
	handler(rec, req)
	return rec
}

type fakeBrowserDreamNexus struct {
	detail                 string
	route                  nexus.ApprovalRoute
	locates, reads, audits int
}

type browserPolicyStore struct{ *fakePolicyStore }

func (s *browserPolicyStore) ListDreamPolicyLifecyclesByOrgBounded(_ context.Context, input db.ListDreamPolicyLifecyclesByOrgBoundedParams) ([]db.DreamPolicy, error) {
	items := make([]db.DreamPolicy, 0)
	for _, policy := range s.policies {
		if policy.EnterpriseID == input.EnterpriseID && policy.OrgScope == input.OrgScope {
			items = append(items, policy)
		}
	}
	return items, nil
}

func (f *fakeBrowserDreamNexus) AuthorizeBrowserOperation(_ context.Context, _ string, req nexus.BrowserAuthorizationRequest) (nexus.BrowserAuthorizationDecision, error) {
	return nexus.BrowserAuthorizationDecision{Decision: "allow", OrgVersion: req.OrgVersion, OrgUnitIDs: []string{req.OrgUnitID}}, nil
}
func (f *fakeBrowserDreamNexus) ResolveApprovalRouteWithBearer(context.Context, string, nexus.ApprovalResolveRequest) (nexus.ApprovalRoute, error) {
	return f.route, nil
}
func (f *fakeBrowserDreamNexus) AppendAuditEvidenceWithBearer(context.Context, string, nexus.AppendAuditEvidenceRequest) (nexus.AppendAuditEvidenceResponse, error) {
	f.audits++
	return nexus.AppendAuditEvidenceResponse{AuditRefID: "audit-secret"}, nil
}
func (f *fakeBrowserDreamNexus) LocateEvidenceWithBearer(context.Context, string, nexus.LocateEvidenceRequest) (nexus.LocateEvidenceResponse, error) {
	f.locates++
	return nexus.LocateEvidenceResponse{ResourceURI: "sealed://detail"}, nil
}
func (f *fakeBrowserDreamNexus) ReadEvidenceWithBearer(context.Context, string, nexus.ReadEvidenceRequest) (nexus.ReadEvidenceResponse, error) {
	f.reads++
	return nexus.ReadEvidenceResponse{GrantID: "grant-secret", SanitizedExcerpt: f.detail, ContentType: "text/plain", ContentHash: "sha256:test"}, nil
}

func stringsReader(value string) *strings.Reader { return strings.NewReader(value) }
func containsBytes(value, match []byte) bool     { return bytes.Contains(value, match) }
