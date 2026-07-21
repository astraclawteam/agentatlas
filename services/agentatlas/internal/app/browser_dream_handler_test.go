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
	"time"

	"github.com/astraclawteam/agentatlas/sdk/go/nexus"
	db "github.com/astraclawteam/agentatlas/services/agentatlas/db/generated"
	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/browsersession"
	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/dream"
	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/nexusclient"
	nexusruntime "github.com/astraclawteam/agentnexus/sdk/go/runtime"
	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
)

func browserDreamRequest(method, target string, session browsersession.Session, body string) *http.Request {
	req := httptest.NewRequest(method, target, stringsReader(body))
	return req.WithContext(context.WithValue(req.Context(), browserActorKey{}, session))
}

func TestBrowserDreamReadRequiresExactSessionAndNexusOrganization(t *testing.T) {
	run := dreamRunFixture()
	store := &fakeDreamRunStore{run: run, runs: []db.DreamRun{{ID: run.ID, EnterpriseID: run.EnterpriseID, OrgUnitID: run.OrgUnitID}}}
	auth := &fakeBrowserAuthorizer{}
	h := &browserDreamHandler{store: store, authorizer: auth, handles: browserDreamTestCodec(t)}
	session := browsersession.Session{Identity: browsersession.Identity{EnterpriseID: "ent-1", UserID: "manager-1", OrgVersion: 7, OrgUnitIDs: []string{"department:rd"}, Permissions: []string{"dream:read"}}, FamilyID: "family-read", UpstreamAccessToken: "bearer-session"}

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

func TestBrowserDreamBasicListReturnsOnlySanitizedShapeAndSessionBoundHandle(t *testing.T) {
	run := dreamRunFixture()
	store := &fakeDreamRunStore{run: run, runs: []db.DreamRun{{ID: run.ID, EnterpriseID: run.EnterpriseID, OrgUnitID: run.OrgUnitID}}}
	auth := &fakeBrowserAuthorizer{}
	h := &browserDreamHandler{store: store, authorizer: auth, handles: browserDreamTestCodec(t)}
	session := browsersession.Session{Identity: browsersession.Identity{EnterpriseID: "ent-1", UserID: "manager-1", OrgVersion: 7, OrgUnitIDs: []string{"department:rd"}, Permissions: []string{"dream:read"}}, FamilyID: "family-1", UpstreamAccessToken: "bearer-session"}

	rec := httptest.NewRecorder()
	h.list(rec, browserDreamRequest(http.MethodGet, "/api/dream/runs?org_unit_id=department:rd", session, ""))
	if rec.Code != http.StatusOK {
		t.Fatalf("list=%d body=%s", rec.Code, rec.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"run_id", "org_unit_id", "policy_version", "workflow", "parent_run_ids", "source_id", "evidence_pointer_id", "idempotency_key", "model_route", "model_version", "timezone", "schedule", "masking_rules"} {
		if jsonContainsKey(body, forbidden) {
			t.Fatalf("basic response leaked %q: %s", forbidden, rec.Body.String())
		}
	}
	if strings.Contains(rec.Body.String(), run.ID) || strings.Contains(rec.Body.String(), run.EvidencePointerID.String) {
		t.Fatalf("basic response leaked a raw identifier: %s", rec.Body.String())
	}
	runs, _ := body["runs"].([]any)
	if len(runs) != 1 {
		t.Fatalf("runs=%#v", body["runs"])
	}
	handle, _ := runs[0].(map[string]any)["handle"].(string)
	if len(handle) < 32 {
		t.Fatalf("missing opaque handle: %s", rec.Body.String())
	}

	other := session
	other.FamilyID = "family-2"
	req := browserDreamRequest(http.MethodGet, "/api/dream/runs/"+handle, other, "")
	route := chi.NewRouteContext()
	route.URLParams.Add("id", handle)
	req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, route))
	detail := httptest.NewRecorder()
	h.detail(detail, req)
	if detail.Code != http.StatusNotFound {
		t.Fatalf("cross-session handle status=%d body=%s", detail.Code, detail.Body.String())
	}
}

func jsonContainsKey(value any, target string) bool {
	switch typed := value.(type) {
	case map[string]any:
		for key, child := range typed {
			if key == target || jsonContainsKey(child, target) {
				return true
			}
		}
	case []any:
		for _, child := range typed {
			if jsonContainsKey(child, target) {
				return true
			}
		}
	}
	return false
}

func browserDreamTestCodec(t *testing.T) *browserDreamHandleCodec {
	t.Helper()
	protector, err := browsersession.NewProtector(bytes.Repeat([]byte{9}, 32))
	if err != nil {
		t.Fatal(err)
	}
	return newBrowserDreamHandleCodec(protector, func() time.Time { return time.Date(2026, 7, 13, 8, 0, 0, 0, time.UTC) })
}

func browserBasicPolicyBody(t *testing.T, h *browserDreamHandler, session browsersession.Session) []byte {
	t.Helper()
	binding := publishedDreamWorkflowBinding{WorkflowID: "wf-dream", WorkflowVersion: 3, WorkflowName: "企业梦境", OutputSpaceID: "spc_1", OutputName: "研发知识"}
	if h.bindings == nil {
		h.bindings = fakePublishedDreamBindingLister{items: []publishedDreamWorkflowBinding{binding}}
	}
	raw, _ := json.Marshal(binding)
	handle, err := h.handles.issue(session, "binding", "pg_mes", string(raw))
	if err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(map[string]any{"org_unit_id": "pg_mes", "cadence": "nightly", "input_sources": []string{"work_brief"}, "visibility": "members", "confirmation": "high_risk_only", "binding_handle": handle})
	return body
}

func TestBrowserDreamPolicyRejectsStalePublishedBindingHandle(t *testing.T) {
	nx := &fakeBrowserDreamNexus{}
	h := &browserDreamHandler{authorizer: nx, evidence: nx, operations: dream.NewPolicyService(newFakePolicyStore()), handles: browserDreamTestCodec(t)}
	session := browsersession.Session{Identity: browsersession.Identity{EnterpriseID: "ent-1", UserID: "manager-1", OrgVersion: 7, OrgUnitIDs: []string{"pg_mes"}, Permissions: []string{"edit"}}, FamilyID: "family-policy-stale", UpstreamAccessToken: "bearer-session"}
	body := browserBasicPolicyBody(t, h, session)
	h.bindings = fakePublishedDreamBindingLister{}

	rec := callBrowserPolicy(t, h.createPolicy, http.MethodPost, "/api/dream/policies", session, "", "stale-binding-create", string(body))
	if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), "stale") {
		t.Fatalf("stale binding status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestBrowserDreamBasicUpdatePreservesAdvancedFieldsAndOrganization(t *testing.T) {
	nx := &fakeBrowserDreamNexus{}
	store := &browserPolicyStore{fakePolicyStore: newFakePolicyStore()}
	h := &browserDreamHandler{authorizer: nx, evidence: nx, operations: dream.NewPolicyService(store), handles: browserDreamTestCodec(t)}
	session := browsersession.Session{Identity: browsersession.Identity{EnterpriseID: "ent-1", UserID: "manager-1", OrgVersion: 7, OrgUnitIDs: []string{"pg_mes", "pg_other"}, Permissions: []string{"edit"}}, FamilyID: "family-policy-merge", UpstreamAccessToken: "bearer-session"}
	body := browserBasicPolicyBody(t, h, session)
	created := callBrowserPolicy(t, h.createPolicy, http.MethodPost, "/api/dream/policies", session, "", "merge-create-0001", string(body))
	var summary browserDreamPolicySummary
	if created.Code != http.StatusCreated || json.Unmarshal(created.Body.Bytes(), &summary) != nil {
		t.Fatalf("create=%d body=%s", created.Code, created.Body.String())
	}
	claim, err := h.handles.resolve(session, "policy", summary.Handle)
	if err != nil {
		t.Fatal(err)
	}
	row := store.policies[claim.ResourceID]
	var canonical dream.Policy
	if err := json.Unmarshal(row.Draft, &canonical); err != nil {
		t.Fatal(err)
	}
	canonical.Timezone = "Pacific/Auckland"
	canonical.MaskingRules = []string{"secret-[0-9]+"}
	canonical.RiskSignalRules = []string{"critical"}
	canonical.EvidenceRetention = "pointer_plus_display_summary"
	canonical.MaxAttempts = 9
	canonical.AllowPartialChildren = true
	row.Draft, _ = json.Marshal(canonical)
	store.policies[row.ID] = row

	var basic map[string]any
	_ = json.Unmarshal(body, &basic)
	basic["org_unit_id"] = "pg_other"
	wrongOrg, _ := json.Marshal(basic)
	denied := callBrowserPolicy(t, h.updatePolicy, http.MethodPut, "/api/dream/policies/"+summary.Handle, session, summary.Handle, "merge-wrong-org-01", string(wrongOrg))
	if denied.Code != http.StatusUnprocessableEntity {
		t.Fatalf("cross-org update=%d body=%s", denied.Code, denied.Body.String())
	}

	basic["org_unit_id"] = "pg_mes"
	basic["cadence"] = "weekly"
	updateBody, _ := json.Marshal(basic)
	updated := callBrowserPolicy(t, h.updatePolicy, http.MethodPut, "/api/dream/policies/"+summary.Handle, session, summary.Handle, "merge-update-0001", string(updateBody))
	if updated.Code != http.StatusOK {
		t.Fatalf("update=%d body=%s", updated.Code, updated.Body.String())
	}
	row = store.policies[claim.ResourceID]
	if err := json.Unmarshal(row.Draft, &canonical); err != nil {
		t.Fatal(err)
	}
	if canonical.OrgUnitID != "pg_mes" || canonical.Schedule != "0 22 * * 0" || canonical.Timezone != "Pacific/Auckland" || len(canonical.MaskingRules) != 1 || len(canonical.RiskSignalRules) != 1 || canonical.EvidenceRetention != "pointer_plus_display_summary" || canonical.MaxAttempts != 9 || !canonical.AllowPartialChildren {
		t.Fatalf("basic update erased or moved advanced policy fields: %+v", canonical)
	}
}

func TestBrowserDreamAdvancedPolicyRequiresEntitlementAndExplicitMode(t *testing.T) {
	nx := &fakeBrowserDreamNexus{}
	store := &browserPolicyStore{fakePolicyStore: newFakePolicyStore()}
	h := &browserDreamHandler{authorizer: nx, evidence: nx, operations: dream.NewPolicyService(store), handles: browserDreamTestCodec(t)}
	session := browsersession.Session{Identity: browsersession.Identity{EnterpriseID: "ent-1", UserID: "provider-1", OrgVersion: 7, OrgUnitIDs: []string{"pg_mes"}, Permissions: []string{"edit"}, AdvancedModeAllowed: true}, FamilyID: "family-advanced", UpstreamAccessToken: "bearer-session"}
	created := callBrowserPolicy(t, h.createPolicy, http.MethodPost, "/api/dream/policies", session, "", "advanced-create-001", string(browserBasicPolicyBody(t, h, session)))
	var summary browserDreamPolicySummary
	_ = json.Unmarshal(created.Body.Bytes(), &summary)

	request := browserDreamRequest(http.MethodGet, "/api/dream/policies/"+summary.Handle+"/advanced", session, "")
	route := chi.NewRouteContext()
	route.URLParams.Add("id", summary.Handle)
	request = request.WithContext(context.WithValue(request.Context(), chi.RouteCtxKey, route))
	denied := httptest.NewRecorder()
	h.getAdvancedPolicy(denied, request)
	if denied.Code != http.StatusForbidden {
		t.Fatalf("inactive advanced mode=%d body=%s", denied.Code, denied.Body.String())
	}
	request.Header.Set("X-Atlas-Advanced-Mode", "enabled")
	allowed := httptest.NewRecorder()
	h.getAdvancedPolicy(allowed, request)
	if allowed.Code != http.StatusOK || !strings.Contains(allowed.Body.String(), `"timezone":"Asia/Shanghai"`) || nx.last.Action != "dream.policy.advanced.read" {
		t.Fatalf("advanced get=%d body=%s auth=%+v", allowed.Code, allowed.Body.String(), nx.last)
	}
	var advanced browserDreamAdvancedPolicy
	_ = json.Unmarshal(allowed.Body.Bytes(), &advanced)
	advanced.Timezone = "UTC"
	raw, _ := json.Marshal(advanced)
	update := browserDreamRequest(http.MethodPut, "/api/dream/policies/"+summary.Handle+"/advanced", session, string(raw))
	update.Header.Set("X-Atlas-Advanced-Mode", "enabled")
	update.Header.Set("Idempotency-Key", "advanced-update-0001")
	update = update.WithContext(context.WithValue(update.Context(), chi.RouteCtxKey, route))
	updated := httptest.NewRecorder()
	h.putAdvancedPolicy(updated, update)
	if updated.Code != http.StatusOK || nx.last.Action != "dream.policy.advanced.edit" || nx.lastAudit.AuthorizedAction != "dream.policy.advanced.edit" {
		t.Fatalf("advanced update=%d body=%s auth=%+v audit=%+v", updated.Code, updated.Body.String(), nx.last, nx.lastAudit)
	}
}

func TestBrowserDreamMutationIsCSRFAndIdempotencyReadyAndAppendOnly(t *testing.T) {
	run := dreamRunFixture()
	run.ParentRunIds = nil
	store := &fakeDreamRunStore{run: run}
	auth := &fakeBrowserAuthorizer{}
	h := &browserDreamHandler{store: store, authorizer: auth, handles: browserDreamTestCodec(t)}
	session := browsersession.Session{Identity: browsersession.Identity{EnterpriseID: "ent-1", UserID: "manager-1", OrgVersion: 7, OrgUnitIDs: []string{"department:rd"}, Permissions: []string{"dream:annotate", "dream:read"}}, FamilyID: "family-annotation", UpstreamAccessToken: "bearer-session"}

	handle, _ := h.handles.issue(session, "run", run.OrgUnitID, run.ID)
	req := browserDreamRequest(http.MethodPost, "/api/dream/runs/"+handle+"/annotations", session, `{"action":"mark_incorrect","comment":"wrong"}`)
	req.Header.Set("Idempotency-Key", "browser-annotation-0001")
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("id", handle)
	req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))
	rec := httptest.NewRecorder()
	h.annotate(rec, req)
	if rec.Code != http.StatusCreated || len(store.annotations) != 1 || store.annotations[0].AnnotationType != "mark_incorrect" || store.run.DisplaySummary != "safe display" {
		t.Fatalf("annotation=%d body=%s rows=%+v", rec.Code, rec.Body.String(), store.annotations)
	}
	var response any
	if json.Unmarshal(rec.Body.Bytes(), &response) != nil || jsonContainsKey(response, "id") || jsonContainsKey(response, "enterprise_id") || jsonContainsKey(response, "run_id") || jsonContainsKey(response, "created_by") {
		t.Fatalf("annotation response leaked storage fields: %s", rec.Body.String())
	}
	if auth.last.Action != "dream.annotate" || auth.last.ResourceID != "run-1" || auth.last.OrgUnitID != "department:rd" {
		t.Fatalf("annotation auth=%+v", auth.last)
	}
	detailReq := browserDreamRequest(http.MethodGet, "/api/dream/runs/"+handle, session, "")
	detailRoute := chi.NewRouteContext()
	detailRoute.URLParams.Add("id", handle)
	detailReq = detailReq.WithContext(context.WithValue(detailReq.Context(), chi.RouteCtxKey, detailRoute))
	detail := httptest.NewRecorder()
	h.detail(detail, detailReq)
	if detail.Code != http.StatusOK || !strings.Contains(detail.Body.String(), `"annotations"`) || !strings.Contains(detail.Body.String(), "wrong") {
		t.Fatalf("persisted annotation missing after reload: %d %s", detail.Code, detail.Body.String())
	}
}

func TestBrowserDreamMutationRejectsOversizedAndMultipleJSONBodies(t *testing.T) {
	run := dreamRunFixture()
	store := &fakeDreamRunStore{run: run}
	auth := &fakeBrowserAuthorizer{}
	h := &browserDreamHandler{store: store, authorizer: auth, handles: browserDreamTestCodec(t)}
	session := browsersession.Session{Identity: browsersession.Identity{EnterpriseID: "ent-1", UserID: "manager-1", OrgVersion: 7, OrgUnitIDs: []string{"department:rd"}, Permissions: []string{"dream:annotate"}}, FamilyID: "family-body", UpstreamAccessToken: "bearer-session"}
	handle, _ := h.handles.issue(session, "run", run.OrgUnitID, run.ID)

	for name, body := range map[string]struct {
		body string
		want int
	}{
		"oversized": {`{"action":"comment","comment":"` + strings.Repeat("x", (1<<20)+1) + `"}`, http.StatusRequestEntityTooLarge},
		"multiple":  {`{"action":"confirm","comment":""} {"action":"reject","comment":""}`, http.StatusBadRequest},
	} {
		req := browserDreamRequest(http.MethodPost, "/api/dream/runs/"+handle+"/annotations", session, body.body)
		route := chi.NewRouteContext()
		route.URLParams.Add("id", handle)
		req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, route))
		rec := httptest.NewRecorder()
		h.annotate(rec, req)
		if rec.Code != body.want {
			t.Fatalf("%s status=%d want=%d body=%s", name, rec.Code, body.want, rec.Body.String())
		}
	}
}

func TestBrowserDreamEvidenceFailsClosedBeforeReturningSanitizedDetail(t *testing.T) {
	run := dreamRunFixture()
	store := &fakeDreamRunStore{run: run}
	nx := &fakeBrowserDreamNexus{detail: "sanitized detail"}
	h := &browserDreamHandler{store: store, authorizer: nx, evidence: nx, handles: browserDreamTestCodec(t)}
	session := browsersession.Session{Identity: browsersession.Identity{EnterpriseID: "ent-1", UserID: "manager-1", OrgVersion: 7, OrgUnitIDs: []string{"department:rd"}, Permissions: []string{"dream:evidence:read"}}, FamilyID: "family-evidence", UpstreamAccessToken: "bearer-session"}

	handle, _ := h.handles.issue(session, "run", run.OrgUnitID, run.ID)
	req := browserDreamRequest(http.MethodPost, "/api/dream/runs/"+handle+"/evidence-access", session, "{}")
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("id", handle)
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

func TestBrowserDreamDetailUsesHumanOrganizationLineageWithoutRawIDs(t *testing.T) {
	run := dreamRunFixture()
	run.ParentRunIds = []string{"child-secret"}
	child := dreamRunFixture()
	child.ID = "child-secret"
	child.OrgUnitID = "team:child-secret"
	store := &fakeDreamRunStore{run: run, views: map[string]db.GetDreamRunViewRow{child.ID: child}}
	orgs := &fakeBrowserOrgStore{spaces: []db.KnowledgeSpace{{ID: "space-child", EnterpriseID: "ent-1", Name: "研发二组", OrgScope: "team:child-secret", OrgVersion: 7}}}
	h := &browserDreamHandler{store: store, orgs: orgs, authorizer: &fakeBrowserAuthorizer{}, handles: browserDreamTestCodec(t)}
	session := browsersession.Session{Identity: browsersession.Identity{EnterpriseID: "ent-1", UserID: "manager-1", OrgVersion: 7, OrgUnitIDs: []string{"department:rd", "team:child-secret"}, Permissions: []string{"dream:read"}}, FamilyID: "family-lineage", UpstreamAccessToken: "bearer"}
	handle, _ := h.handles.issue(session, "run", run.OrgUnitID, run.ID)
	req := browserDreamRequest(http.MethodGet, "/api/dream/runs/"+handle, session, "")
	route := chi.NewRouteContext()
	route.URLParams.Add("id", handle)
	req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, route))
	rec := httptest.NewRecorder()
	h.detail(rec, req)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "研发二组") || strings.Contains(rec.Body.String(), "child-secret") || !strings.Contains(rec.Body.String(), `"input_organizations"`) {
		t.Fatalf("detail lineage=%d %s", rec.Code, rec.Body.String())
	}
}

func TestBrowserDreamRerunUsesBearerAuditAndPinnedIdempotency(t *testing.T) {
	run := dreamRunFixture()
	store := &fakeDreamRunStore{run: run}
	nx := &fakeBrowserDreamNexus{}
	rerunner := &fakeDreamRerunner{}
	operations := dream.NewPolicyService(newFakePolicyStore())
	h := &browserDreamHandler{store: store, authorizer: nx, evidence: nx, rerun: rerunner, operations: operations, handles: browserDreamTestCodec(t)}
	session := browsersession.Session{Identity: browsersession.Identity{EnterpriseID: "ent-1", UserID: "manager-1", OrgVersion: 7, OrgUnitIDs: []string{"department:rd"}, Permissions: []string{"dream:rerun"}}, FamilyID: "family-rerun", UpstreamAccessToken: "bearer-session"}
	handle, _ := h.handles.issue(session, "run", run.OrgUnitID, run.ID)
	req := browserDreamRequest(http.MethodPost, "/api/dream/runs/"+handle+"/reruns", session, "{}")
	req.Header.Set("Idempotency-Key", "browser-rerun-key-0001")
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("id", handle)
	req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))
	rec := httptest.NewRecorder()
	h.rerunRun(rec, req)
	if rec.Code != http.StatusAccepted || rerunner.calls != 1 || rerunner.sourceRun != "run-1" || rerunner.key != "browser-rerun-key-0001" || nx.audits != 1 {
		t.Fatalf("rerun=%d body=%s calls=%d audit=%d", rec.Code, rec.Body.String(), rerunner.calls, nx.audits)
	}
	if strings.Contains(rec.Body.String(), "rerun-1") || !strings.Contains(rec.Body.String(), `"handle"`) || strings.Contains(rec.Body.String(), `"run_id"`) {
		t.Fatalf("rerun response must return only an opaque handle: %s", rec.Body.String())
	}
}

func TestBrowserDreamPolicyCreateIsDraftOnlyAuthorizedAndAudited(t *testing.T) {
	nx := &fakeBrowserDreamNexus{}
	policies := dream.NewPolicyService(newFakePolicyStore())
	h := &browserDreamHandler{authorizer: nx, evidence: nx, operations: policies, handles: browserDreamTestCodec(t)}
	session := browsersession.Session{Identity: browsersession.Identity{EnterpriseID: "ent-1", UserID: "manager-1", OrgVersion: 7, OrgUnitIDs: []string{"pg_mes"}, Permissions: []string{"edit"}}, FamilyID: "family-policy", UpstreamAccessToken: "bearer-session"}
	body := browserBasicPolicyBody(t, h, session)
	req := browserDreamRequest(http.MethodPost, "/api/dream/policies", session, string(body))
	req.Header.Set("Idempotency-Key", "browser-policy-key-0001")
	rec := httptest.NewRecorder()
	h.createPolicy(rec, req)
	if rec.Code != http.StatusCreated || nx.audits != 1 || !containsBytes(rec.Body.Bytes(), []byte(`"status":"draft"`)) || containsBytes(rec.Body.Bytes(), []byte(`"status":"published"`)) {
		t.Fatalf("policy=%d body=%s audits=%d", rec.Code, rec.Body.String(), nx.audits)
	}
	var response any
	if json.Unmarshal(rec.Body.Bytes(), &response) != nil {
		t.Fatalf("invalid response: %s", rec.Body.String())
	}
	for _, forbidden := range []string{"dream_policy_id", "requester_user_id", "reviewer_user_id", "org_path", "org_unit_id", "timezone", "schedule", "workflow", "output_space_id", "masking_rules", "risk_signal_rules"} {
		if jsonContainsKey(response, forbidden) {
			t.Fatalf("basic policy response leaked %q: %s", forbidden, rec.Body.String())
		}
	}
	if !jsonContainsKey(response, "handle") {
		t.Fatalf("policy response has no opaque handle: %s", rec.Body.String())
	}
}

func TestBrowserDreamPublishedBindingListReturnsHumanLabelsAndOpaqueHandlesOnly(t *testing.T) {
	auth := &fakeBrowserAuthorizer{}
	h := &browserDreamHandler{authorizer: auth, handles: browserDreamTestCodec(t), bindings: fakePublishedDreamBindingLister{items: []publishedDreamWorkflowBinding{{WorkflowID: "workflow-secret", WorkflowVersion: 7, WorkflowName: "企业梦境整理", OutputSpaceID: "space-secret", OutputName: "研发知识"}}}}
	session := browsersession.Session{Identity: browsersession.Identity{EnterpriseID: "ent-1", UserID: "editor-1", OrgVersion: 7, OrgUnitIDs: []string{"pg_mes"}, Permissions: []string{"dream:read"}}, FamilyID: "binding-family", UpstreamAccessToken: "bearer"}
	rec := httptest.NewRecorder()
	h.listWorkflowBindings(rec, browserDreamRequest(http.MethodGet, "/api/dream/workflow-bindings?org_unit_id=pg_mes", session, ""))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "企业梦境整理") || !strings.Contains(rec.Body.String(), "研发知识") || !strings.Contains(rec.Body.String(), `"handle"`) || strings.Contains(rec.Body.String(), "workflow-secret") || strings.Contains(rec.Body.String(), "space-secret") {
		t.Fatalf("bindings=%d %s", rec.Code, rec.Body.String())
	}
	if auth.last.ResourceType != "workflow" || auth.last.ResourceID != "pg_mes" || auth.last.Action != "workflow.read" {
		t.Fatalf("binding authorization tuple=%+v", auth.last)
	}
}

type fakePublishedDreamBindingLister struct {
	items []publishedDreamWorkflowBinding
}

func (f fakePublishedDreamBindingLister) ListPublishedDreamWorkflowBindings(context.Context, string, string, int32) ([]publishedDreamWorkflowBinding, error) {
	return f.items, nil
}

func TestBrowserDreamPolicyGovernedLifecycleAndBackfill(t *testing.T) {
	nx := &fakeBrowserDreamNexus{route: nexus.ApprovalRoute{Mode: "upward_review", RiskLevel: "high", RiskReasons: []string{"high_risk_field:workflow"}, RequesterUserID: "creator-1", ReviewerUserID: "manager-1", OrgPath: []string{"pg_mes", "department:rd"}}}
	policyStore := &browserPolicyStore{fakePolicyStore: newFakePolicyStore()}
	policies := dream.NewPolicyService(policyStore)
	backfiller := &fakeDreamRerunner{}
	h := &browserDreamHandler{authorizer: nx, evidence: nx, operations: policies, backfill: backfiller, handles: browserDreamTestCodec(t)}
	creator := browsersession.Session{Identity: browsersession.Identity{EnterpriseID: "ent_1", UserID: "creator-1", OrgVersion: 7, OrgUnitIDs: []string{"pg_mes"}, Permissions: []string{"edit", "dream:read"}, AdvancedModeAllowed: true}, FamilyID: "family-lifecycle", UpstreamAccessToken: "bearer-session"}
	reviewer := browsersession.Session{Identity: browsersession.Identity{EnterpriseID: "ent_1", UserID: "manager-1", OrgVersion: 7, OrgUnitIDs: []string{"pg_mes"}, Permissions: []string{"approve_high_risk"}}, FamilyID: "family-lifecycle", UpstreamAccessToken: "bearer-reviewer"}
	body := browserBasicPolicyBody(t, h, creator)
	created := callBrowserPolicy(t, h.createPolicy, http.MethodPost, "/api/dream/policies", creator, "", "lifecycle-create-0001", string(body))
	var summary browserDreamPolicySummary
	if json.Unmarshal(created.Body.Bytes(), &summary) != nil || summary.Handle == "" || summary.Status != "draft" {
		t.Fatalf("create=%d %s", created.Code, created.Body.String())
	}
	claim, err := h.handles.resolve(creator, "policy", summary.Handle)
	if err != nil {
		t.Fatal(err)
	}
	policyHandle := summary.Handle
	nonAdvanced := creator
	nonAdvanced.AdvancedModeAllowed = false
	deniedAdvanced := callBrowserPolicy(t, h.backfillPolicy, http.MethodPost, "/api/dream/policies/"+policyHandle+"/backfills", nonAdvanced, policyHandle, "denied-advanced-backfill", `{"window_start":"2026-07-01T00:00:00Z","window_end":"2026-07-02T00:00:00Z"}`)
	if deniedAdvanced.Code != http.StatusForbidden || backfiller.backfillCalls != 0 {
		t.Fatalf("advanced gate status=%d calls=%d body=%s", deniedAdvanced.Code, backfiller.backfillCalls, deniedAdvanced.Body.String())
	}
	deniedInactive := callBrowserPolicy(t, h.backfillPolicy, http.MethodPost, "/api/dream/policies/"+policyHandle+"/backfills", creator, policyHandle, "denied-inactive-backfill", `{"window_start":"2026-07-01T00:00:00Z","window_end":"2026-07-02T00:00:00Z"}`)
	if deniedInactive.Code != http.StatusForbidden || backfiller.backfillCalls != 0 {
		t.Fatalf("inactive mode status=%d calls=%d body=%s", deniedInactive.Code, backfiller.backfillCalls, deniedInactive.Body.String())
	}
	view, err := policies.GetLifecycle(context.Background(), creator.EnterpriseID, claim.ResourceID)
	if err != nil {
		t.Fatal(err)
	}
	listed := callBrowserPolicy(t, h.listPolicies, http.MethodGet, "/api/dream/policies?org_unit_id=pg_mes", creator, "", "", "")
	if listed.Code != http.StatusOK || !containsBytes(listed.Body.Bytes(), []byte(`"status":"draft"`)) {
		t.Fatalf("draft list=%d %s", listed.Code, listed.Body.String())
	}
	if nx.last.ResourceType != "dream_policy" || nx.last.ResourceID != "pg_mes" || nx.last.Action != "dream.policy.read" {
		t.Fatalf("policy list authorization tuple=%+v", nx.last)
	}
	intruder := browsersession.Session{Identity: browsersession.Identity{EnterpriseID: "ent_1", UserID: "viewer-1", OrgVersion: 7, OrgUnitIDs: []string{"pg_mes"}, Permissions: []string{"dream:read"}}, UpstreamAccessToken: "bearer-viewer"}
	intruder.FamilyID = creator.FamilyID
	deniedBackfill := callBrowserPolicy(t, h.backfillPolicy, http.MethodPost, "/api/dream/policies/"+policyHandle+"/backfills", intruder, policyHandle, "denied-backfill-0001", `{"window_start":"2026-07-01T00:00:00Z","window_end":"2026-07-02T00:00:00Z"}`)
	deniedDisable := callBrowserPolicy(t, h.disablePolicy, http.MethodPost, "/api/dream/policies/"+policyHandle+"/disable", intruder, policyHandle, "denied-disable-00001", `{"revision":0}`)
	if deniedBackfill.Code != http.StatusForbidden || deniedDisable.Code != http.StatusForbidden || backfiller.backfillCalls != 0 {
		t.Fatalf("authorization backfill=%d disable=%d calls=%d", deniedBackfill.Code, deniedDisable.Code, backfiller.backfillCalls)
	}
	reviewed := callBrowserPolicy(t, h.reviewPolicy, http.MethodPost, "/api/dream/policies/"+policyHandle+"/review", creator, policyHandle, "lifecycle-review-0001", `{"revision":0,"action":"publish"}`)
	if json.Unmarshal(reviewed.Body.Bytes(), &summary) != nil || summary.ReviewState != "pending" {
		t.Fatalf("review=%d %s", reviewed.Code, reviewed.Body.String())
	}
	decided := callBrowserPolicy(t, h.decidePolicy, http.MethodPost, "/api/dream/policies/"+policyHandle+"/decisions", reviewer, policyHandle, "lifecycle-decision-01", `{"revision":0,"decision":"approve"}`)
	if json.Unmarshal(decided.Body.Bytes(), &summary) != nil || summary.ReviewState != "approved" {
		t.Fatalf("decision=%d %s", decided.Code, decided.Body.String())
	}
	published := callBrowserPolicy(t, h.publishPolicy, http.MethodPost, "/api/dream/policies/"+policyHandle+"/publish", creator, policyHandle, "lifecycle-publish-001", `{"revision":0}`)
	if json.Unmarshal(published.Body.Bytes(), &summary) != nil || summary.Status != "published" {
		t.Fatalf("publish=%d %s", published.Code, published.Body.String())
	}
	backfill := callBrowserAdvancedPolicy(t, h.backfillPolicy, http.MethodPost, "/api/dream/policies/"+policyHandle+"/backfills", creator, policyHandle, "lifecycle-backfill-01", `{"window_start":"2026-07-01T00:00:00Z","window_end":"2026-07-02T00:00:00Z"}`)
	if backfill.Code != http.StatusAccepted || backfiller.backfillCalls != 1 {
		t.Fatalf("backfill=%d %s calls=%d", backfill.Code, backfill.Body.String(), backfiller.backfillCalls)
	}
	view, _ = policies.GetLifecycle(context.Background(), creator.EnterpriseID, claim.ResourceID)
	disableReview := callBrowserPolicy(t, h.reviewPolicy, http.MethodPost, "/api/dream/policies/"+policyHandle+"/review", creator, policyHandle, "lifecycle-disable-review", fmt.Sprintf(`{"revision":%d,"action":"disable"}`, view.Revision))
	if json.Unmarshal(disableReview.Body.Bytes(), &summary) != nil || summary.PendingAction != "disable" {
		t.Fatalf("disable review=%d %s", disableReview.Code, disableReview.Body.String())
	}
	disableDecision := callBrowserPolicy(t, h.decidePolicy, http.MethodPost, "/api/dream/policies/"+policyHandle+"/decisions", reviewer, policyHandle, "lifecycle-disable-decision", fmt.Sprintf(`{"revision":%d,"decision":"approve"}`, view.Revision))
	if json.Unmarshal(disableDecision.Body.Bytes(), &summary) != nil || summary.ReviewState != "approved" {
		t.Fatalf("disable decision=%d %s", disableDecision.Code, disableDecision.Body.String())
	}
	disabled := callBrowserPolicy(t, h.disablePolicy, http.MethodPost, "/api/dream/policies/"+policyHandle+"/disable", creator, policyHandle, "lifecycle-disable-final", fmt.Sprintf(`{"revision":%d}`, view.Revision))
	if json.Unmarshal(disabled.Body.Bytes(), &summary) != nil || summary.Status != "disabled" {
		t.Fatalf("disable=%d %s", disabled.Code, disabled.Body.String())
	}
}

func TestBrowserDreamBackfillFailsBeforeWorkWithoutAuditClient(t *testing.T) {
	policies := dream.NewPolicyService(newFakePolicyStore())
	nx := &fakeBrowserDreamNexus{}
	runner := &fakeDreamRerunner{}
	h := &browserDreamHandler{authorizer: nx, evidence: nx, operations: policies, backfill: runner, handles: browserDreamTestCodec(t)}
	session := browsersession.Session{Identity: browsersession.Identity{EnterpriseID: "ent-1", UserID: "editor-1", OrgVersion: 7, OrgUnitIDs: []string{"pg_mes"}, Permissions: []string{"edit"}, AdvancedModeAllowed: true}, FamilyID: "family-no-audit", UpstreamAccessToken: "bearer"}
	body := browserBasicPolicyBody(t, h, session)
	created := callBrowserPolicy(t, h.createPolicy, http.MethodPost, "/api/dream/policies", session, "", "create-no-audit-0001", string(body))
	var summary browserDreamPolicySummary
	if json.Unmarshal(created.Body.Bytes(), &summary) != nil || summary.Handle == "" {
		t.Fatalf("create=%d %s", created.Code, created.Body.String())
	}
	h.evidence = nil
	handle := summary.Handle
	rec := callBrowserAdvancedPolicy(t, h.backfillPolicy, http.MethodPost, "/api/dream/policies/"+handle+"/backfills", session, handle, "no-audit-backfill", `{"window_start":"2026-07-01T00:00:00Z","window_end":"2026-07-02T00:00:00Z"}`)
	if rec.Code != http.StatusServiceUnavailable || runner.backfillCalls != 0 {
		t.Fatalf("status=%d calls=%d body=%s", rec.Code, runner.backfillCalls, rec.Body.String())
	}
}

func TestBrowserDreamSuggestionAdoptionCreatesImmutableDirectEditLineageAndReplays(t *testing.T) {
	nx := &fakeBrowserDreamNexus{}
	store := &browserPolicyStore{fakePolicyStore: newFakePolicyStore()}
	policies := dream.NewPolicyService(store)
	h := &browserDreamHandler{authorizer: nx, evidence: nx, operations: policies, handles: browserDreamTestCodec(t)}
	employee := browsersession.Session{Identity: browsersession.Identity{EnterpriseID: "ent-1", UserID: "employee-1", OrgVersion: 7, OrgUnitIDs: []string{"pg_mes"}, Permissions: []string{"suggest", "dream:read"}}, FamilyID: "employee-family", UpstreamAccessToken: "employee-bearer"}
	editor := browsersession.Session{Identity: browsersession.Identity{EnterpriseID: "ent-1", UserID: "editor-1", OrgVersion: 7, OrgUnitIDs: []string{"pg_mes"}, Permissions: []string{"edit", "dream:read"}}, FamilyID: "editor-family", UpstreamAccessToken: "editor-bearer"}
	created := callBrowserPolicy(t, h.createPolicy, http.MethodPost, "/api/dream/policies", employee, "", "suggestion-create-0001", string(browserBasicPolicyBody(t, h, employee)))
	if created.Code != http.StatusCreated {
		t.Fatalf("suggest=%d %s", created.Code, created.Body.String())
	}
	var employeeView browserDreamPolicySummary
	_ = json.Unmarshal(created.Body.Bytes(), &employeeView)
	sourceClaim, _ := h.handles.resolve(employee, "policy", employeeView.Handle)

	listed := callBrowserPolicy(t, h.listPolicies, http.MethodGet, "/api/dream/policies?org_unit_id=pg_mes", editor, "", "", "")
	var listBody struct {
		DreamPolicies []browserDreamPolicySummary `json:"dream_policies"`
	}
	if json.Unmarshal(listed.Body.Bytes(), &listBody) != nil || len(listBody.DreamPolicies) != 1 || !listBody.DreamPolicies[0].CanAdopt {
		t.Fatalf("editor list=%d %s", listed.Code, listed.Body.String())
	}
	handle := listBody.DreamPolicies[0].Handle
	adopt := callBrowserPolicy(t, h.adoptPolicy, http.MethodPost, "/api/dream/policies/"+handle+"/adoptions", editor, handle, "suggestion-adopt-0001", `{"revision":0}`)
	var adopted browserDreamPolicySummary
	if adopt.Code != http.StatusCreated || json.Unmarshal(adopt.Body.Bytes(), &adopted) != nil || adopted.PermissionMode != "direct_edit" || adopted.Handle == handle {
		t.Fatalf("adopt=%d %s", adopt.Code, adopt.Body.String())
	}
	if nx.last.ResourceType != "dream_policy" || nx.last.ResourceID != sourceClaim.ResourceID || nx.last.Action != "dream.policy.adopt" {
		t.Fatalf("adoption authorization tuple=%+v", nx.last)
	}
	if source := store.policies[sourceClaim.ResourceID]; source.PermissionMode != "suggestion_only" || source.RequesterUserID != "employee-1" {
		t.Fatalf("source suggestion mutated: %+v", source)
	}
	if len(store.adoptions) != 1 {
		t.Fatalf("lineage=%+v", store.adoptions)
	}
	replay := callBrowserPolicy(t, h.adoptPolicy, http.MethodPost, "/api/dream/policies/"+handle+"/adoptions", editor, handle, "suggestion-adopt-0001", `{"revision":0}`)
	if replay.Code != http.StatusCreated || len(store.adoptions) != 1 {
		t.Fatalf("replay=%d %s lineage=%d", replay.Code, replay.Body.String(), len(store.adoptions))
	}
}

func TestBrowserDreamRouterRequiresSessionForEveryRouteAndCSRFForeveryMutation(t *testing.T) {
	now := time.Date(2026, 7, 12, 10, 0, 0, 0, time.UTC)
	identity := browsersession.Identity{EnterpriseID: "ent-1", UserID: "manager-1", DisplayName: "陈经理", OrgVersion: 7, OrgUnitIDs: []string{"department:rd", "pg_mes"}, Permissions: []string{"dream:read", "dream:annotate", "dream:rerun", "dream:evidence:read", "edit", "approve_high_risk"}, AdvancedModeAllowed: true}
	oidc := &fakeAtlasOIDC{profile: identity}
	sessions, err := browsersession.New(browsersession.Config{Issuer: "https://nexus.example", ClientID: "agentatlas", ClientSecret: "secret", RedirectURI: "https://atlas.example/auth/callback"}, browsersession.NewMemoryStore(func() time.Time { return now }), oidc, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	protector, _ := browsersession.NewProtector(bytes.Repeat([]byte{4}, 32))
	run := dreamRunFixture()
	run.ParentRunIds = nil
	store := &fakeDreamRunStore{run: run, runs: []db.DreamRun{{ID: run.ID, EnterpriseID: run.EnterpriseID, OrgUnitID: run.OrgUnitID}}}
	nx := &fakeBrowserDreamNexus{}
	router := NewAgentRouter(AgentRouterDeps{OrgAuthorization: &allowOrgAuthorization{}, Nexus: adminMock(), BrowserSessions: sessions, BrowserHandleProtector: protector, BrowserAuthorizer: nx, DreamRuns: store, Dreams: dream.NewPolicyService(&browserPolicyStore{fakePolicyStore: newFakePolicyStore()}), DreamRerun: &fakeDreamRerunner{}})
	login := httptest.NewRequest(http.MethodGet, "https://atlas.example/auth/login?return_to=%2Fdream", nil)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, login)
	callback := httptest.NewRequest(http.MethodGet, "https://atlas.example/auth/callback?state="+oidc.last.State+"&code=one-use", nil)
	rr = httptest.NewRecorder()
	router.ServeHTTP(rr, callback)
	cookie := rr.Result().Cookies()[0]

	routes := []struct{ method, path string }{
		{http.MethodGet, "/api/dream/runs?org_unit_id=department:rd"}, {http.MethodGet, "/api/dream/runs/opaque"}, {http.MethodGet, "/api/dream/policies?org_unit_id=pg_mes"}, {http.MethodGet, "/api/dream/workflow-bindings?org_unit_id=pg_mes"}, {http.MethodGet, "/api/dream/policies/opaque/advanced"},
		{http.MethodPost, "/api/dream/runs/opaque/annotations"}, {http.MethodPost, "/api/dream/runs/opaque/reruns"}, {http.MethodPost, "/api/dream/runs/opaque/evidence-access"}, {http.MethodPost, "/api/dream/policies"}, {http.MethodPut, "/api/dream/policies/opaque"}, {http.MethodPost, "/api/dream/policies/opaque/adoptions"}, {http.MethodPost, "/api/dream/policies/opaque/check"}, {http.MethodPost, "/api/dream/policies/opaque/review"}, {http.MethodPost, "/api/dream/policies/opaque/decisions"}, {http.MethodPost, "/api/dream/policies/opaque/publish"}, {http.MethodPost, "/api/dream/policies/opaque/disable"}, {http.MethodPost, "/api/dream/policies/opaque/backfills"},
	}
	for _, route := range routes {
		t.Run(route.method+" "+route.path, func(t *testing.T) {
			request := httptest.NewRequest(route.method, "https://atlas.example"+route.path, strings.NewReader(`{}`))
			response := httptest.NewRecorder()
			router.ServeHTTP(response, request)
			if response.Code != http.StatusUnauthorized {
				t.Fatalf("without session=%d body=%s", response.Code, response.Body.String())
			}
			if route.method != http.MethodGet {
				request = httptest.NewRequest(route.method, "https://atlas.example"+route.path, strings.NewReader(`{}`))
				request.AddCookie(cookie)
				response = httptest.NewRecorder()
				router.ServeHTTP(response, request)
				if response.Code != http.StatusForbidden {
					t.Fatalf("without same-origin CSRF=%d body=%s", response.Code, response.Body.String())
				}
			} else if strings.Contains(route.path, "/advanced") {
				request = httptest.NewRequest(route.method, "https://atlas.example"+route.path, nil)
				request.AddCookie(cookie)
				request.Header.Set("X-Atlas-Advanced-Mode", "enabled")
				response = httptest.NewRecorder()
				router.ServeHTTP(response, request)
				if response.Code == http.StatusForbidden {
					t.Fatalf("credentialed Advanced GET was incorrectly CSRF-gated: %s", response.Body.String())
				}
			}
		})
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

func callBrowserAdvancedPolicy(t *testing.T, handler http.HandlerFunc, method, target string, session browsersession.Session, id, key, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := browserDreamRequest(method, target, session, body)
	req.Header.Set("Idempotency-Key", key)
	req.Header.Set("X-Atlas-Advanced-Mode", "enabled")
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
	last                   nexus.BrowserAuthorizationRequest
	lastAudit              nexus.AppendAuditEvidenceRequest
}

type browserPolicyStore struct {
	*fakePolicyStore
	adoptions map[string]db.DreamPolicyAdoption
}

func (s *browserPolicyStore) AdoptDreamPolicySuggestion(_ context.Context, input db.AdoptDreamPolicySuggestionParams) (db.AdoptDreamPolicySuggestionRow, error) {
	source, ok := s.policies[input.SourcePolicyID]
	op := s.operations[input.EnterpriseID+"\x00"+input.OperationKey]
	if !ok || source.EnterpriseID != input.EnterpriseID || source.PermissionMode != "suggestion_only" || source.Status != "draft" || source.Revision != input.SourceRevision || op.OperationKind != "adopt" || !op.AuditRefID.Valid || op.AuditRefID.String != input.AuditRefID.String {
		return db.AdoptDreamPolicySuggestionRow{}, pgx.ErrNoRows
	}
	if s.adoptions == nil {
		s.adoptions = map[string]db.DreamPolicyAdoption{}
	}
	key := input.EnterpriseID + "\x00" + input.SourcePolicyID + fmt.Sprint(input.SourceRevision)
	if _, exists := s.adoptions[key]; exists {
		return db.AdoptDreamPolicySuggestionRow{}, pgx.ErrNoRows
	}
	target := db.DreamPolicy{ID: input.TargetPolicyID, EnterpriseID: source.EnterpriseID, OrgScope: source.OrgScope, Status: "draft", Draft: source.Draft, RequesterUserID: input.AdopterUserID, PermissionMode: "direct_edit", RiskReasons: []byte(`[]`), ReviewOrgPath: []byte(`[]`), AuditRefID: input.AuditRefID.String}
	s.policies[target.ID] = target
	s.adoptions[key] = db.DreamPolicyAdoption{EnterpriseID: input.EnterpriseID, SourcePolicyID: source.ID, SourceRequesterUserID: source.RequesterUserID, SourceRevision: source.Revision, TargetPolicyID: target.ID, AdopterUserID: input.AdopterUserID, AuditRefID: input.AuditRefID.String, OperationKey: input.OperationKey}
	s.recordTransition(input.EnterpriseID, input.OperationKey, target.ID, "adopt", 0)
	return db.AdoptDreamPolicySuggestionRow{ID: target.ID, EnterpriseID: target.EnterpriseID, OrgScope: target.OrgScope, Status: target.Status, Draft: target.Draft, RequesterUserID: target.RequesterUserID, PermissionMode: target.PermissionMode, RiskReasons: target.RiskReasons, ReviewOrgPath: target.ReviewOrgPath, AuditRefID: target.AuditRefID}, nil
}

func (s *browserPolicyStore) GetDreamPolicyAdoptionBySource(_ context.Context, input db.GetDreamPolicyAdoptionBySourceParams) (db.DreamPolicyAdoption, error) {
	row, ok := s.adoptions[input.EnterpriseID+"\x00"+input.SourcePolicyID+fmt.Sprint(input.SourceRevision)]
	if !ok {
		return db.DreamPolicyAdoption{}, pgx.ErrNoRows
	}
	return row, nil
}

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
	f.last = req
	return nexus.BrowserAuthorizationDecision{Decision: "allow", OrgVersion: req.OrgVersion, OrgUnitIDs: []string{req.OrgUnitID}}, nil
}
func (f *fakeBrowserDreamNexus) ResolveApprovalRouteWithBearer(context.Context, string, nexus.ApprovalResolveRequest) (nexus.ApprovalRoute, error) {
	return f.route, nil
}
func (f *fakeBrowserDreamNexus) AppendAuditEvidenceWithBearer(_ context.Context, _ string, request nexus.AppendAuditEvidenceRequest) (nexus.AppendAuditEvidenceResponse, error) {
	f.audits++
	f.lastAudit = request
	return nexus.AppendAuditEvidenceResponse{AuditRefID: "audit-secret"}, nil
}
func (f *fakeBrowserDreamNexus) LocateWithBearer(_ context.Context, _ string, _ nexusruntime.EvidenceRequest) (nexusclient.LocateEvidenceResult, error) {
	f.locates++
	return nexusclient.LocateEvidenceResult{
		BusinessContextRef: "wc_0123456789abcdef0123",
		Evidence:           []nexusruntime.EvidenceHandle{{EvidenceRef: "evd_0123456789abcdef0123", DataClass: dreamEvidenceDataClass}},
	}, nil
}
func (f *fakeBrowserDreamNexus) ReadWithBearer(_ context.Context, _ string, _ nexusruntime.EvidenceReadRequest) (nexusclient.ReadEvidenceResult, error) {
	f.reads++
	return nexusclient.ReadEvidenceResult{
		Decision: "allow", GrantRef: "grant-secret",
		Data:          map[string]any{"detail": f.detail},
		SourceVersion: 7, AsOf: "2026-07-21T00:00:00Z", ServedFromCache: false,
	}, nil
}

func stringsReader(value string) *strings.Reader { return strings.NewReader(value) }
func containsBytes(value, match []byte) bool     { return bytes.Contains(value, match) }
