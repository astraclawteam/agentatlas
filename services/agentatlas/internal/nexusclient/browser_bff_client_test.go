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
