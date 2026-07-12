package nexusclient

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/astraclawteam/agentatlas/sdk/go/nexus"
)

func TestBrowserBFFAuditUsesBearerAndOmitsCaseTicket(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/audit/evidence" || r.Header.Get("Authorization") != "Bearer upstream-browser-access-token" || r.Header.Get("Idempotency-Key") != "browser-audit-key-1234" {
			t.Fatalf("request path=%s auth=%q idem=%q", r.URL.Path, r.Header.Get("Authorization"), r.Header.Get("Idempotency-Key"))
		}
		var body map[string]any
		if json.NewDecoder(r.Body).Decode(&body) != nil {
			t.Fatal("decode")
		}
		if _, exists := body["ticket_id"]; exists {
			t.Fatalf("browser audit sent ticket_id: %v", body)
		}
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(nexus.AppendAuditEvidenceResponse{AuditRefID: "audit-browser-1"})
	}))
	defer server.Close()
	client := &HTTPClient{baseURL: server.URL, http: server.Client()}
	out, err := client.AppendAuditEvidenceWithBearer(context.Background(), "upstream-browser-access-token", nexus.AppendAuditEvidenceRequest{IdempotencyKey: "browser-audit-key-1234", EnterpriseID: "ent-1", Action: nexus.AuditWorkflowVersionPublished, ResourceType: "workflow", ResourceID: "wf-1"})
	if err != nil || out.AuditRefID != "audit-browser-1" {
		t.Fatalf("out=%+v err=%v", out, err)
	}
}

func TestTicketGovernanceAuthorizationUsesServiceCredentialsAndOriginalTicket(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, password, ok := r.BasicAuth()
		if r.URL.Path != "/v1/authorization/decisions" || !ok || user != "agentatlas" || password != "service-secret" {
			t.Fatalf("request path=%s basic=%v user=%q password=%q", r.URL.Path, ok, user, password)
		}
		var body nexus.BrowserAuthorizationRequest
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if body.TicketID != "case-ticket" || body.ResourceType != "workflow" || body.ResourceID != "wf-1" || body.Action != "workflow.edit" {
			t.Fatalf("unbound authorization: %+v", body)
		}
		_ = json.NewEncoder(w).Encode(nexus.BrowserAuthorizationDecision{Decision: "allow", OrgVersion: 7, OrgUnitIDs: []string{"team"}})
	}))
	defer server.Close()
	client := &HTTPClient{baseURL: server.URL, http: server.Client(), serviceClientID: "agentatlas", serviceSecret: "service-secret"}
	out, err := client.AuthorizeTicketOperation(context.Background(), "case-ticket", nexus.BrowserAuthorizationRequest{OrgUnitID: "team", OrgVersion: 7, ResourceType: "workflow", ResourceID: "wf-1", Action: "workflow.edit"})
	if err != nil || out.Decision != "allow" {
		t.Fatalf("out=%+v err=%v", out, err)
	}
}
