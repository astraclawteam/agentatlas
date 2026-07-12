package app

import (
	"bytes"
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
	_, err := store.CreateDreamPolicyLifecycle(t.Context(), db.CreateDreamPolicyLifecycleParams{ID: "policy-audit", EnterpriseID: "ent_1", OrgScope: "pg_mes", Draft: raw, RequesterUserID: "editor", PermissionMode: "direct_edit", AuditRefID: "audit-create"})
	if err != nil {
		t.Fatal(err)
	}
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
	self := doLifecycleJSON(t, http.MethodPost, srv.URL+"/v1/dream-policies/"+id+"/decisions", "tick_editor", "", map[string]any{"revision": 0, "decision": "approve"})
	if self.StatusCode != http.StatusConflict {
		t.Fatalf("self approval=%d", self.StatusCode)
	}
	self.Body.Close()
	decision := doLifecycleJSON(t, http.MethodPost, srv.URL+"/v1/dream-policies/"+id+"/decisions", "tick_reviewer", "", map[string]any{"revision": 0, "decision": "approve"})
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
	updatedPolicy := canonicalDreamPolicyBody()
	updatedPolicy["schedule"] = "15 22 * * *"
	updated := doLifecycleJSON(t, http.MethodPut, srv.URL+"/v1/dream-policies/"+id, "tick_editor", "", map[string]any{"revision": 1, "policy": updatedPolicy})
	if updated.StatusCode != 200 {
		t.Fatalf("low-risk update=%d %v", updated.StatusCode, decodeBody(t, updated))
	}
	updated.Body.Close()
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
