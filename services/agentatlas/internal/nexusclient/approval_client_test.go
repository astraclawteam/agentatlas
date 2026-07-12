package nexusclient

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/astraclawteam/agentatlas/sdk/go/nexus"
)

func TestApprovalClientUsesFrozenHeadersAndRejectsAutoPublish(t *testing.T) {
	secret := "0123456789abcdefghijklmnopqrstuv"
	auto := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/approvals/resolve" || r.Header.Get("X-Case-Ticket") != "ticket-1" || r.Header.Get("Idempotency-Key") != "approval-key-0001" || r.Header.Get("X-Approval-Facts-Attestation") == "" {
			t.Errorf("approval request path=%s headers=%v", r.URL.Path, r.Header)
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		if _, leaked := body["ticket_id"]; leaked {
			t.Error("ticket leaked into frozen body")
		}
		_ = json.NewEncoder(w).Encode(nexus.ApprovalRoute{Mode: "upward_review", RiskLevel: "high", RiskReasons: []string{"workflow_binding"}, RequesterUserID: "editor", ReviewerUserID: "manager", OrgPath: []string{"team", "department"}, AutoPublish: auto})
	}))
	defer server.Close()
	client := &HTTPClient{baseURL: server.URL, http: server.Client(), serviceClientID: "agentatlas", serviceSecret: "different-service-credential-123456", approvalFactsSecret: secret}
	now := time.Date(2026, 7, 12, 1, 2, 3, 0, time.UTC)
	route, err := client.ResolveApprovalRoute(t.Context(), nexus.ApprovalResolveRequest{TicketID: "ticket-1", EnterpriseID: "enterprise-1", ActorUserID: "editor", IdempotencyKey: "approval-key-0001", OrgVersion: 1, OrgUnitID: "team", ResourceType: "dream_policy", ResourceID: "policy-1", Action: "publish", ChangedFields: []string{"workflow"}, ImpactedOrgUnitIDs: []string{"team"}, RequestedRisk: "high", FactsIssuedAt: now, FactsExpiresAt: now.Add(5 * time.Minute), FactsNonce: "nonce-0000000001"})
	if err != nil {
		t.Fatal(err)
	}
	if route.ReviewerUserID != "manager" {
		t.Fatalf("route=%+v", route)
	}
}
