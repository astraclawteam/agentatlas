package app

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/astraclawteam/agentatlas/sdk/go/governance"
	"github.com/astraclawteam/agentatlas/sdk/go/nexus"
	db "github.com/astraclawteam/agentatlas/services/agentatlas/db/generated"
	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/dream"
)

func TestDreamPolicyLifecycleCreateIsRevisionedVersionZeroDraft(t *testing.T) {
	mock := adminMock()
	mock.Tickets["tick_editor"] = nexus.VerifyTicketResponse{Valid: true, EnterpriseID: "ent_1", ActorUserID: "editor", Scopes: []string{"edit", "publish_low_risk"}}
	srv, _, store := newAgentTestServerWithPolicyStore(t, mock)

	resp := postJSONReq(t, srv.URL+"/v1/dream-policies", "tick_editor", canonicalDreamPolicyBody())
	if resp.StatusCode != 201 {
		t.Fatalf("create = %d", resp.StatusCode)
	}
	created := decodeBody(t, resp)
	if created["status"] != "draft" || created["version"] != float64(0) || created["revision"] != float64(0) {
		t.Fatalf("created lifecycle = %#v", created)
	}
	if len(store.versions) != 0 {
		t.Fatalf("draft creation published versions: %#v", store.versions)
	}
}

func TestDreamPolicyLifecycleSuggestionCannotModifyOrPublish(t *testing.T) {
	mock := adminMock()
	mock.Tickets["tick_employee"] = nexus.VerifyTicketResponse{Valid: true, EnterpriseID: "ent_1", ActorUserID: "employee", Scopes: []string{"suggest"}}
	srv, _, store := newAgentTestServerWithPolicyStore(t, mock)
	created := decodeBody(t, postJSONReq(t, srv.URL+"/v1/dream-policies", "tick_employee", canonicalDreamPolicyBody()))
	id := created["dream_policy_id"].(string)
	if created["permission_mode"] != "suggestion_only" {
		t.Fatalf("suggestion=%#v", created)
	}
	resp := doLifecycleJSON(t, http.MethodPut, srv.URL+"/v1/dream-policies/"+id, "tick_employee", "", map[string]any{"revision": 0, "policy": canonicalDreamPolicyBody()})
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("suggestion update=%d", resp.StatusCode)
	}
	resp.Body.Close()
	resp = doLifecycleJSON(t, http.MethodPost, srv.URL+"/v1/dream-policies/"+id+"/publish", "tick_employee", "", map[string]any{"revision": 0})
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("suggestion publish=%d", resp.StatusCode)
	}
	resp.Body.Close()
	if store.policies[id].Status != "draft" || len(store.versions) != 0 {
		t.Fatalf("suggestion mutated=%+v versions=%v", store.policies[id], store.versions)
	}
}

func TestDreamPolicyLifecycleAuditFailurePreventsUpdate(t *testing.T) {
	base := adminMock()
	base.Tickets["tick_editor"] = nexus.VerifyTicketResponse{Valid: true, EnterpriseID: "ent_1", ActorUserID: "editor", Scopes: []string{"edit"}}
	store := newFakePolicyStore()
	raw, _ := json.Marshal(canonicalDreamPolicyBody())
	store.policies["policy-audit"] = db.DreamPolicy{ID: "policy-audit", EnterpriseID: "ent_1", OrgScope: "pg_mes", Status: "draft", Draft: raw, RequesterUserID: "editor", PermissionMode: "direct_edit", RiskReasons: []byte(`[]`), ReviewOrgPath: []byte(`[]`)}
	router := NewAgentRouter(AgentRouterDeps{Nexus: &failingAuditNexus{Mock: base}, Dreams: dream.NewPolicyService(store)})
	srv := httptest.NewServer(router)
	defer srv.Close()
	resp := doLifecycleJSON(t, http.MethodPut, srv.URL+"/v1/dream-policies/policy-audit", "tick_editor", "", map[string]any{"revision": 0, "policy": canonicalDreamPolicyBody()})
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("audit failure update=%d", resp.StatusCode)
	}
	resp.Body.Close()
	if store.policies["policy-audit"].Revision != 0 || store.policies["policy-audit"].Status != "draft" {
		t.Fatalf("audit failure mutated=%+v", store.policies["policy-audit"])
	}
}

func TestDreamPolicyLifecycleAdminQueueRefreshesFromNexus(t *testing.T) {
	mock := adminMock()
	mock.Tickets["tick_editor"] = nexus.VerifyTicketResponse{Valid: true, EnterpriseID: "ent_1", ActorUserID: "editor", Scopes: []string{"edit"}}
	mock.Tickets["tick_manager"] = nexus.VerifyTicketResponse{Valid: true, EnterpriseID: "ent_1", ActorUserID: "manager", Scopes: []string{"approve_high_risk"}}
	mock.ApprovalRoute = nexus.ApprovalRoute{Mode: string(governance.ReviewAdminQueue), RiskLevel: "high", RiskReasons: []string{"workflow_binding"}, RequesterUserID: "editor", OrgPath: []string{"pg_mes"}, Queue: "enterprise_knowledge_admin"}
	srv, _, _ := newAgentTestServerWithPolicyStore(t, mock)
	created := decodeBody(t, postJSONReq(t, srv.URL+"/v1/dream-policies", "tick_editor", canonicalDreamPolicyBody()))
	id := created["dream_policy_id"].(string)
	queued := doLifecycleJSON(t, http.MethodPost, srv.URL+"/v1/dream-policies/"+id+"/review", "tick_editor", "queue-review-0001", map[string]any{"revision": 0, "action": "publish"})
	if queued.StatusCode != 200 {
		t.Fatalf("queue=%d", queued.StatusCode)
	}
	queueBody := decodeBody(t, queued)
	if queueBody["review_mode"] != "enterprise_knowledge_admin_queue" {
		t.Fatalf("queue=%#v", queueBody)
	}
	mock.ApprovalRoute = nexus.ApprovalRoute{Mode: string(governance.ReviewUpward), RiskLevel: "high", RiskReasons: []string{"workflow_binding"}, RequesterUserID: "editor", ReviewerUserID: "manager", OrgPath: []string{"pg_mes", "dept"}}
	refreshed := doLifecycleJSON(t, http.MethodPost, srv.URL+"/v1/dream-policies/"+id+"/review", "tick_editor", "queue-refresh-001", map[string]any{"revision": 0, "action": "publish"})
	if refreshed.StatusCode != 200 {
		t.Fatalf("refresh=%d %v", refreshed.StatusCode, decodeBody(t, refreshed))
	}
	refreshBody := decodeBody(t, refreshed)
	if refreshBody["reviewer_user_id"] != "manager" {
		t.Fatalf("refresh=%#v", refreshBody)
	}
	decision := doLifecycleJSON(t, http.MethodPost, srv.URL+"/v1/dream-policies/"+id+"/decisions", "tick_manager", "queue-decision-01", map[string]any{"revision": 0, "decision": "approve"})
	if decision.StatusCode != 200 {
		t.Fatalf("queue decision=%d", decision.StatusCode)
	}
	decision.Body.Close()
}

func TestDreamPolicyLifecycleHighRiskUsesUpwardDifferentReviewer(t *testing.T) {
	mock := adminMock()
	mock.Tickets["tick_editor"] = nexus.VerifyTicketResponse{Valid: true, EnterpriseID: "ent_1", ActorUserID: "editor", Scopes: []string{"edit", "approve_high_risk", "publish_low_risk"}}
	mock.Tickets["tick_reviewer"] = nexus.VerifyTicketResponse{Valid: true, EnterpriseID: "ent_1", ActorUserID: "manager", Scopes: []string{"approve_high_risk"}}
	mock.ApprovalRoute = nexus.ApprovalRoute{Mode: string(governance.ReviewUpward), RiskLevel: "high", RiskReasons: []string{"workflow_binding"}, RequesterUserID: "editor", ReviewerUserID: "manager", OrgPath: []string{"pg_mes", "dept_rnd"}}
	srv, _, store := newAgentTestServerWithPolicyStore(t, mock)
	created := decodeBody(t, postJSONReq(t, srv.URL+"/v1/dream-policies", "tick_editor", canonicalDreamPolicyBody()))
	id := created["dream_policy_id"].(string)
	checked := doLifecycleJSON(t, http.MethodPost, srv.URL+"/v1/dream-policies/"+id+"/check", "tick_editor", "", map[string]any{"revision": 0})
	if checked.StatusCode != 200 {
		t.Fatalf("check=%d", checked.StatusCode)
	}
	checkBody := decodeBody(t, checked)
	if checkBody["risk_level"] != "high" || store.policies[id].Status != "draft" {
		t.Fatalf("check body/state=%#v %+v", checkBody, store.policies[id])
	}
	review := doLifecycleJSON(t, http.MethodPost, srv.URL+"/v1/dream-policies/"+id+"/review", "tick_editor", "dream-review-0001", map[string]any{"revision": 0})
	if review.StatusCode != 200 {
		t.Fatalf("review=%d %v", review.StatusCode, decodeBody(t, review))
	}
	reviewed := decodeBody(t, review)
	if reviewed["status"] != "review_pending" || reviewed["risk_level"] != "high" || reviewed["reviewer_user_id"] != "manager" {
		t.Fatalf("reviewed=%#v", reviewed)
	}
	auditsBeforeWrongReviewer := len(mock.AuditLog)
	self := doLifecycleJSON(t, http.MethodPost, srv.URL+"/v1/dream-policies/"+id+"/decisions", "tick_editor", "self-decision-0001", map[string]any{"revision": 0, "decision": "approve"})
	if self.StatusCode != http.StatusForbidden {
		t.Fatalf("self approval=%d", self.StatusCode)
	}
	self.Body.Close()
	if len(mock.AuditLog) != auditsBeforeWrongReviewer {
		t.Fatal("wrong reviewer emitted a committed-looking remote audit")
	}
	decision := doLifecycleJSON(t, http.MethodPost, srv.URL+"/v1/dream-policies/"+id+"/decisions", "tick_reviewer", "reviewer-decision-1", map[string]any{"revision": 0, "decision": "approve"})
	if decision.StatusCode != 200 {
		t.Fatalf("decision=%d %v", decision.StatusCode, decodeBody(t, decision))
	}
	decision.Body.Close()
	published := doLifecycleJSON(t, http.MethodPost, srv.URL+"/v1/dream-policies/"+id+"/publish", "tick_editor", "", map[string]any{"revision": 0})
	if published.StatusCode != 200 {
		t.Fatalf("publish=%d %v", published.StatusCode, decodeBody(t, published))
	}
	out := decodeBody(t, published)
	if out["status"] != "published" || out["version"] != float64(1) || out["revision"] != float64(1) {
		t.Fatalf("published=%#v", out)
	}
	if len(store.versions) != 1 {
		t.Fatalf("versions=%#v", store.versions)
	}
	auditsAfterPublish := len(mock.AuditLog)
	replayedPublish := doLifecycleJSON(t, http.MethodPost, srv.URL+"/v1/dream-policies/"+id+"/publish", "tick_editor", "", map[string]any{"revision": 0})
	if replayedPublish.StatusCode != 200 {
		t.Fatalf("publish replay=%d", replayedPublish.StatusCode)
	}
	replayedPublish.Body.Close()
	if len(mock.AuditLog) != auditsAfterPublish {
		t.Fatal("publish replay duplicated remote audit")
	}
	updatedPolicy := canonicalDreamPolicyBody()
	updatedPolicy["schedule"] = "15 22 * * *"
	updated := doLifecycleJSON(t, http.MethodPut, srv.URL+"/v1/dream-policies/"+id, "tick_editor", "schedule-update-01", map[string]any{"revision": 1, "policy": updatedPolicy})
	if updated.StatusCode != 200 {
		t.Fatalf("low-risk update=%d %v", updated.StatusCode, decodeBody(t, updated))
	}
	updated.Body.Close()
	replayedUpdate := doLifecycleJSON(t, http.MethodPut, srv.URL+"/v1/dream-policies/"+id, "tick_editor", "schedule-update-01", map[string]any{"revision": 1, "policy": updatedPolicy})
	if replayedUpdate.StatusCode != 200 {
		t.Fatalf("update replay=%d", replayedUpdate.StatusCode)
	}
	replayedUpdate.Body.Close()
	mismatch := canonicalDreamPolicyBody()
	mismatch["schedule"] = "30 22 * * *"
	mismatchResp := doLifecycleJSON(t, http.MethodPut, srv.URL+"/v1/dream-policies/"+id, "tick_editor", "schedule-update-01", map[string]any{"revision": 1, "policy": mismatch})
	if mismatchResp.StatusCode != 409 {
		t.Fatalf("idempotency mismatch=%d", mismatchResp.StatusCode)
	}
	mismatchResp.Body.Close()
	mock.ApprovalRoute = nexus.ApprovalRoute{Mode: string(governance.ReviewSingleConfirmation), RiskLevel: "low", RiskReasons: []string{}, RequesterUserID: "editor", OrgPath: []string{}}
	lowReview := doLifecycleJSON(t, http.MethodPost, srv.URL+"/v1/dream-policies/"+id+"/review", "tick_editor", "dream-review-0002", map[string]any{"revision": 2})
	if lowReview.StatusCode != 200 {
		t.Fatalf("low review=%d %v", lowReview.StatusCode, decodeBody(t, lowReview))
	}
	low := decodeBody(t, lowReview)
	if low["risk_level"] != "low" || low["review_mode"] != "single_confirmation" {
		t.Fatalf("low route=%#v", low)
	}
	lowDecision := doLifecycleJSON(t, http.MethodPost, srv.URL+"/v1/dream-policies/"+id+"/decisions", "tick_editor", "", map[string]any{"revision": 2, "decision": "approve"})
	if lowDecision.StatusCode != 200 {
		t.Fatalf("low decision=%d", lowDecision.StatusCode)
	}
	lowDecision.Body.Close()
	second := doLifecycleJSON(t, http.MethodPost, srv.URL+"/v1/dream-policies/"+id+"/publish", "tick_editor", "", map[string]any{"revision": 2})
	if second.StatusCode != 200 {
		t.Fatalf("second publish=%d %v", second.StatusCode, decodeBody(t, second))
	}
	secondOut := decodeBody(t, second)
	if secondOut["version"] != float64(2) || secondOut["revision"] != float64(3) {
		t.Fatalf("second publish=%#v", secondOut)
	}
}

func doLifecycleJSON(t *testing.T, method, url, ticket, key string, body any) *http.Response {
	t.Helper()
	raw, _ := json.Marshal(body)
	if key == "" {
		sum := sha256.Sum256(append([]byte(method+url), raw...))
		key = "test-op-" + hex.EncodeToString(sum[:8])
	}
	req, _ := http.NewRequest(method, url, bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Nexus-Ticket", ticket)
	if key != "" {
		req.Header.Set("Idempotency-Key", key)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func canonicalDreamPolicyBody() map[string]any {
	return map[string]any{
		"org_unit_id": "pg_mes", "timezone": "Asia/Shanghai", "schedule": "0 22 * * *",
		"input_sources": []string{"work_brief"}, "workflow": map[string]any{"id": "wf-dream", "version": 3},
		"visibility_level": "members", "confirmation_mode": "high_risk_only",
		"masking_rules": []string{}, "risk_signal_rules": []string{},
		"evidence_retention": "pointer_only", "output_space_id": "spc_1",
	}
}
