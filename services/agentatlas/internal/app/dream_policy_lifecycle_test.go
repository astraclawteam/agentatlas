package app

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/astraclawteam/agentatlas/sdk/go/nexus"
	db "github.com/astraclawteam/agentatlas/services/agentatlas/db/generated"
	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/dream"
	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/nexusclient"
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
	router := NewAgentRouter(AgentRouterDeps{OrgAuthorization: &allowOrgAuthorization{}, ApprovalTransmitter: &fakeApprovalTransmitter{decision: nexusclient.ApprovalApproved, authority: "oa.example"}, ApprovalAuthority: "oa.example", Nexus: &failingAuditNexus{Mock: base}, Dreams: dream.NewPolicyService(store)})
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

func TestDreamPolicyLifecycleMapsNexusAuditConflictTo409(t *testing.T) {
	base := adminMock()
	base.Tickets["tick_editor"] = nexus.VerifyTicketResponse{Valid: true, EnterpriseID: "ent_1", ActorUserID: "editor", Scopes: []string{"edit"}}
	store := newFakePolicyStore()
	raw, _ := json.Marshal(canonicalDreamPolicyBody())
	store.policies["policy-conflict"] = db.DreamPolicy{ID: "policy-conflict", EnterpriseID: "ent_1", OrgScope: "pg_mes", Status: "draft", Draft: raw, RequesterUserID: "editor", PermissionMode: "direct_edit", RiskReasons: []byte(`[]`), ReviewOrgPath: []byte(`[]`)}
	router := NewAgentRouter(AgentRouterDeps{OrgAuthorization: &allowOrgAuthorization{}, ApprovalTransmitter: &fakeApprovalTransmitter{decision: nexusclient.ApprovalApproved, authority: "oa.example"}, ApprovalAuthority: "oa.example", Nexus: &conflictAuditNexus{Mock: base}, Dreams: dream.NewPolicyService(store)})
	srv := httptest.NewServer(router)
	defer srv.Close()
	resp := doLifecycleJSON(t, http.MethodPut, srv.URL+"/v1/dream-policies/policy-conflict", "tick_editor", "audit-conflict-op-0001", map[string]any{"revision": 0, "policy": canonicalDreamPolicyBody()})
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("status=%d body=%v", resp.StatusCode, decodeBody(t, resp))
	}
	resp.Body.Close()
	if store.policies["policy-conflict"].Revision != 0 {
		t.Fatal("audit conflict mutated policy")
	}
}

func TestDreamPolicyLifecycleRecoversAfterRemoteAuditReceiptPersistenceFailure(t *testing.T) {
	mock := adminMock()
	mock.Tickets["tick_editor"] = nexus.VerifyTicketResponse{Valid: true, EnterpriseID: "ent_1", ActorUserID: "editor", Scopes: []string{"edit"}}
	store := newFakePolicyStore()
	store.failRecordOnce = true
	srv, _, _ := newAgentTestServerWithProvidedPolicyStore(t, mock, store)
	first := doLifecycleJSON(t, http.MethodPost, srv.URL+"/v1/dream-policies", "tick_editor", "audit-record-failure", canonicalDreamPolicyBody())
	if first.StatusCode != http.StatusInternalServerError {
		t.Fatalf("first=%d", first.StatusCode)
	}
	first.Body.Close()
	retry := doLifecycleJSON(t, http.MethodPost, srv.URL+"/v1/dream-policies", "tick_editor", "audit-record-failure", canonicalDreamPolicyBody())
	if retry.StatusCode != http.StatusCreated {
		t.Fatalf("retry=%d body=%v", retry.StatusCode, decodeBody(t, retry))
	}
	retry.Body.Close()
	if len(mock.AuditLog) != 1 {
		t.Fatalf("semantic remote audits=%d", len(mock.AuditLog))
	}
}

func TestDreamPolicyLifecycleReconcilesTransitionAfterCompletionFailure(t *testing.T) {
	mock := adminMock()
	mock.Tickets["tick_editor"] = nexus.VerifyTicketResponse{Valid: true, EnterpriseID: "ent_1", ActorUserID: "editor", Scopes: []string{"edit"}}
	store := newFakePolicyStore()
	srv, _, _ := newAgentTestServerWithProvidedPolicyStore(t, mock, store)
	created := decodeBody(t, postJSONReq(t, srv.URL+"/v1/dream-policies", "tick_editor", canonicalDreamPolicyBody()))
	id := created["dream_policy_id"].(string)
	store.failCompleteOnce = true
	updatedPolicy := canonicalDreamPolicyBody()
	updatedPolicy["schedule"] = "15 22 * * *"
	first := doLifecycleJSON(t, http.MethodPut, srv.URL+"/v1/dream-policies/"+id, "tick_editor", "transition-complete-failure", map[string]any{"revision": 0, "policy": updatedPolicy})
	if first.StatusCode != http.StatusInternalServerError || store.policies[id].Revision != 1 {
		t.Fatalf("first=%d revision=%d", first.StatusCode, store.policies[id].Revision)
	}
	first.Body.Close()
	retry := doLifecycleJSON(t, http.MethodPut, srv.URL+"/v1/dream-policies/"+id, "tick_editor", "transition-complete-failure", map[string]any{"revision": 0, "policy": updatedPolicy})
	if retry.StatusCode != http.StatusOK {
		t.Fatalf("retry=%d body=%v", retry.StatusCode, decodeBody(t, retry))
	}
	retry.Body.Close()
	if len(mock.AuditLog) != 2 { // create + exactly one update attempt
		t.Fatalf("remote audits=%d", len(mock.AuditLog))
	}
}

func TestDreamPolicyLifecycleStuckPendingTransitionNeverReconstructsLaterView(t *testing.T) {
	mock := adminMock()
	mock.Tickets["tick_editor"] = nexus.VerifyTicketResponse{Valid: true, EnterpriseID: "ent_1", ActorUserID: "editor", Scopes: []string{"edit"}}
	store := newFakePolicyStore()
	srv, _, _ := newAgentTestServerWithProvidedPolicyStore(t, mock, store)
	created := decodeBody(t, postJSONReq(t, srv.URL+"/v1/dream-policies", "tick_editor", canonicalDreamPolicyBody()))
	id := created["dream_policy_id"].(string)
	store.leaveTransitionPendingOnce = true
	policyA := canonicalDreamPolicyBody()
	policyA["schedule"] = "15 22 * * *"
	const operationA = "legacy-stuck-operation-a"
	firstA := doLifecycleJSON(t, http.MethodPut, srv.URL+"/v1/dream-policies/"+id, "tick_editor", operationA, map[string]any{"revision": 0, "policy": policyA})
	if firstA.StatusCode != http.StatusConflict || store.policies[id].Revision != 1 {
		t.Fatalf("A status=%d revision=%d body=%v", firstA.StatusCode, store.policies[id].Revision, decodeBody(t, firstA))
	}
	firstA.Body.Close()
	policyB := canonicalDreamPolicyBody()
	policyB["schedule"] = "30 22 * * *"
	secondB := doLifecycleJSON(t, http.MethodPut, srv.URL+"/v1/dream-policies/"+id, "tick_editor", "operation-b-after-stuck-a", map[string]any{"revision": 1, "policy": policyB})
	if secondB.StatusCode != http.StatusOK {
		t.Fatalf("B status=%d body=%v", secondB.StatusCode, decodeBody(t, secondB))
	}
	secondB.Body.Close()
	auditsBeforeRetry := len(mock.AuditLog)
	retryA := doLifecycleJSON(t, http.MethodPut, srv.URL+"/v1/dream-policies/"+id, "tick_editor", operationA, map[string]any{"revision": 0, "policy": policyA})
	if retryA.StatusCode != http.StatusConflict {
		t.Fatalf("retry A status=%d body=%v", retryA.StatusCode, decodeBody(t, retryA))
	}
	retryA.Body.Close()
	opA := store.operations["ent_1\x00"+operationA]
	if opA.Status != "pending" || len(opA.Result) != 0 || store.policies[id].Revision != 2 || len(mock.AuditLog) != auditsBeforeRetry {
		t.Fatalf("A receipt/status drift op=%+v revision=%d audits=%d/%d", opA, store.policies[id].Revision, len(mock.AuditLog), auditsBeforeRetry)
	}
}

func TestDreamPolicyLifecycleAdminQueueRefreshesFromNexus(t *testing.T) {
	mock := adminMock()
	mock.Tickets["tick_editor"] = nexus.VerifyTicketResponse{Valid: true, EnterpriseID: "ent_1", ActorUserID: "editor", Scopes: []string{"edit"}}
	mock.Tickets["tick_manager"] = nexus.VerifyTicketResponse{Valid: true, EnterpriseID: "ent_1", ActorUserID: "manager", Scopes: []string{"approve_high_risk"}}
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
	refreshed := doLifecycleJSON(t, http.MethodPost, srv.URL+"/v1/dream-policies/"+id+"/review", "tick_editor", "queue-refresh-001", map[string]any{"revision": 0, "action": "publish"})
	if refreshed.StatusCode != 200 {
		t.Fatalf("refresh=%d %v", refreshed.StatusCode, decodeBody(t, refreshed))
	}
	refreshBody := decodeBody(t, refreshed)
	// The reviewer is whoever the approval AUTHORITY reported, not someone
	// AgentAtlas chose: it no longer selects approvers.
	if refreshBody["reviewer_user_id"] != "oa.example" || refreshBody["review_mode"] != "upward_review" {
		t.Fatalf("refresh=%#v", refreshBody)
	}
	// No second decision call: the authority already approved in its own
	// system, and refresh applied that. Asking a human to click approve again
	// here would put the same decision in two places, where they can disagree.
	if refreshBody["review_state"] != "approved" {
		t.Fatalf("refresh did not apply the authority decision: %#v", refreshBody)
	}
}

func TestDreamPolicyLifecycleDifferentEditorCannotSubmitOrRefreshReview(t *testing.T) {
	mock := adminMock()
	mock.Tickets["tick_creator"] = nexus.VerifyTicketResponse{Valid: true, EnterpriseID: "ent_1", ActorUserID: "creator", Scopes: []string{"edit"}}
	mock.Tickets["tick_editor2"] = nexus.VerifyTicketResponse{Valid: true, EnterpriseID: "ent_1", ActorUserID: "editor-2", Scopes: []string{"edit"}}
	srv, _, _ := newAgentTestServerWithPolicyStore(t, mock)
	created := decodeBody(t, postJSONReq(t, srv.URL+"/v1/dream-policies", "tick_creator", canonicalDreamPolicyBody()))
	id := created["dream_policy_id"].(string)
	auditBefore := len(mock.AuditLog)
	denied := doLifecycleJSON(t, http.MethodPost, srv.URL+"/v1/dream-policies/"+id+"/review", "tick_editor2", "different-editor-review", map[string]any{"revision": 0, "action": "publish"})
	if denied.StatusCode != http.StatusForbidden {
		t.Fatalf("different editor submit=%d body=%v", denied.StatusCode, decodeBody(t, denied))
	}
	denied.Body.Close()
	// A denied editor must not have reached AgentNexus at all.
	if len(mock.AuditLog) != auditBefore {
		t.Fatal("denied editor emitted audit")
	}
	queued := doLifecycleJSON(t, http.MethodPost, srv.URL+"/v1/dream-policies/"+id+"/review", "tick_creator", "creator-queue-review", map[string]any{"revision": 0, "action": "publish"})
	if queued.StatusCode != http.StatusOK {
		t.Fatalf("creator queue=%d body=%v", queued.StatusCode, decodeBody(t, queued))
	}
	queued.Body.Close()
	auditBefore = len(mock.AuditLog)
	refreshed := doLifecycleJSON(t, http.MethodPost, srv.URL+"/v1/dream-policies/"+id+"/review", "tick_editor2", "different-editor-refresh", map[string]any{"revision": 0, "action": "publish"})
	if refreshed.StatusCode != http.StatusForbidden {
		t.Fatalf("different editor refresh=%d body=%v", refreshed.StatusCode, decodeBody(t, refreshed))
	}
	refreshed.Body.Close()
	if len(mock.AuditLog) != auditBefore {
		t.Fatal("denied refresh reached AgentNexus or emitted audit")
	}
}

func TestDreamPolicyLifecycleHighRiskUsesUpwardDifferentReviewer(t *testing.T) {
	mock := adminMock()
	mock.Tickets["tick_editor"] = nexus.VerifyTicketResponse{Valid: true, EnterpriseID: "ent_1", ActorUserID: "editor", Scopes: []string{"edit", "approve_high_risk", "publish_low_risk"}}
	mock.Tickets["tick_reviewer"] = nexus.VerifyTicketResponse{Valid: true, EnterpriseID: "ent_1", ActorUserID: "manager", Scopes: []string{"approve_high_risk"}}
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
	// First submit only QUEUES with the authority: it is delivery, not a
	// decision, so no reviewer is named yet.
	if reviewed["status"] != "review_pending" || reviewed["risk_level"] != "high" ||
		reviewed["review_mode"] != "enterprise_knowledge_admin_queue" || reviewed["reviewer_user_id"] != nil {
		t.Fatalf("reviewed=%#v", reviewed)
	}
	auditsBeforeWrongReviewer := len(mock.AuditLog)
	// Self-approval is now moot rather than merely refused: a route queued with
	// the authority cannot be decided in AgentAtlas by ANYONE, requester
	// included. The guard fires before identity is even considered.
	self := doLifecycleJSON(t, http.MethodPost, srv.URL+"/v1/dream-policies/"+id+"/decisions", "tick_editor", "self-decision-0001", map[string]any{"revision": 0, "decision": "approve"})
	if self.StatusCode != http.StatusConflict {
		t.Fatalf("self approval=%d", self.StatusCode)
	}
	self.Body.Close()
	if len(mock.AuditLog) != auditsBeforeWrongReviewer {
		t.Fatal("wrong reviewer emitted a committed-looking remote audit")
	}
	// Approval arrives from the authority, not from a click here: refreshing
	// reads its decision and applies it in one step.
	refreshed := doLifecycleJSON(t, http.MethodPost, srv.URL+"/v1/dream-policies/"+id+"/review", "tick_editor", "authority-refresh-01", map[string]any{"revision": 0})
	if refreshed.StatusCode != 200 {
		t.Fatalf("refresh=%d %v", refreshed.StatusCode, decodeBody(t, refreshed))
	}
	refreshedBody := decodeBody(t, refreshed)
	if refreshedBody["review_state"] != "approved" || refreshedBody["reviewer_user_id"] != "oa.example" {
		t.Fatalf("authority decision not applied: %#v", refreshedBody)
	}
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
