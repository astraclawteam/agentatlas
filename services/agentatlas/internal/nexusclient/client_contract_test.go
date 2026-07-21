package nexusclient

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/astraclawteam/agentatlas/sdk/go/nexus"
	nexusruntime "github.com/astraclawteam/agentnexus/sdk/go/runtime"
)

// contractServer implements the agentnexus-client.yaml surface.
func contractServer(t *testing.T, serviceSecret string) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()

	mux.HandleFunc("POST /v1/tickets/verify", func(w http.ResponseWriter, r *http.Request) {
		clientID, secret, ok := r.BasicAuth()
		if !ok || clientID != "agentatlas" || secret != serviceSecret {
			t.Errorf("ticket verify Basic credentials client=%q ok=%t", clientID, ok)
		}
		var req nexus.VerifyTicketRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		resp := nexus.VerifyTicketResponse{Valid: false}
		if req.TicketID == "tick_ok" {
			resp = nexus.VerifyTicketResponse{
				Valid: true, EnterpriseID: "ent_1", ActorUserID: "u_zhang",
				Scopes: []string{"space.read"}, OrgVersion: 7, OrgUnitIDs: []string{"team"}, ExpiresAt: time.Now().Add(time.Hour),
			}
		}
		_ = json.NewEncoder(w).Encode(resp)
	})

	// The frozen evidence surface. Denial is keyed by the DECLARED NEED now:
	// there is no ticket in the body to key it by.
	mux.HandleFunc("POST /v1/runtime/locate", func(w http.ResponseWriter, r *http.Request) {
		assertNoServiceAuthorization(t, r)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"business_context_ref": "wc_0123456789abcdef0123",
			"evidence": []map[string]any{{
				"evidence_ref": "evd_0123456789abcdef0123", "data_class": "test.evidence",
			}},
		})
	})

	mux.HandleFunc("POST /v1/runtime/read", func(w http.ResponseWriter, r *http.Request) {
		assertNoServiceAuthorization(t, r)
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if ref, _ := body["evidence_ref"].(string); ref == "evd_denied0000000000" {
			http.Error(w, `{"code":"forbidden","message":"scope missing"}`, http.StatusForbidden)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"decision": "allow", "grant_ref": "grn_0123456789abcdef",
			"data":           map[string]any{"detail": "ok"},
			"source_version": 1, "as_of": "2026-07-21T00:00:00Z", "served_from_cache": false,
		})
	})

	mux.HandleFunc("POST /v1/audit/evidence", func(w http.ResponseWriter, r *http.Request) {
		clientID, secret, ok := r.BasicAuth()
		if !ok || clientID != "agentatlas" || secret != serviceSecret {
			t.Errorf("audit Basic credentials client=%q ok=%t", clientID, ok)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		if r.Header.Get("Idempotency-Key") != "audit-contract-key-0001" {
			t.Errorf("audit idempotency header=%q", r.Header.Get("Idempotency-Key"))
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		if body["action"] != "evidence_read" || body["resource_type"] != "answer_trace" || body["resource_id"] != "trace-1" {
			t.Errorf("audit body=%v", body)
		}
		if _, exists := body["workflow_run_id"]; exists {
			t.Errorf("audit body silently contains workflow_run_id: %v", body)
		}
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(nexus.AppendAuditEvidenceResponse{AuditRefID: "audit_1"})
	})

	mux.HandleFunc("GET /v1/org-events", func(w http.ResponseWriter, r *http.Request) {
		assertNoServiceAuthorization(t, r)
		if r.URL.Query().Get("enterprise_id") == "" {
			http.Error(w, "enterprise_id required", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		flusher := w.(http.Flusher)
		for i := 1; i <= 2; i++ {
			ev := nexus.OrgEvent{
				EventID: fmt.Sprintf("evt_%d", i), EnterpriseID: "ent_1",
				OrgVersion: int64(i), Type: nexus.OrgEmployeeUpserted,
				Scope:      nexus.OrgScope{Kind: nexus.ScopeEmployee, ID: fmt.Sprintf("u_%d", i), Name: "员工"},
				OccurredAt: time.Unix(1750000000, 0),
			}
			raw, _ := json.Marshal(ev)
			fmt.Fprintf(w, "data: %s\n\n", raw)
			flusher.Flush()
		}
	})

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func assertNoServiceAuthorization(t *testing.T, r *http.Request) {
	t.Helper()
	if authorization := r.Header.Get("Authorization"); authorization != "" {
		t.Errorf("service credential leaked to %s: authorization header present", r.URL.Path)
	}
}

func writeServiceSecret(t *testing.T, value string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "agentnexus-service.secret")
	if err := os.WriteFile(path, []byte(value), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestHTTPClientContract(t *testing.T) {
	const serviceSecret = "AgentAtlas-Nexus-Service-Q7mV2xK9pR4tY8dF3"
	srv := contractServer(t, serviceSecret)
	c, err := New(srv.URL, 5*time.Second, "agentatlas", writeServiceSecret(t, serviceSecret), nil)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(fmt.Sprintf("%+v %#v", c, c), serviceSecret) {
		t.Fatal("HTTP client formatting exposes the service secret")
	}
	ctx := context.Background()

	verify, err := c.VerifyTicket(ctx, nexus.VerifyTicketRequest{TicketID: "tick_ok"})
	if err != nil || !verify.Valid || verify.EnterpriseID != "ent_1" {
		t.Fatalf("verify: %+v err=%v", verify, err)
	}
	invalid, err := c.VerifyTicket(ctx, nexus.VerifyTicketRequest{TicketID: "nope"})
	if err != nil || invalid.Valid {
		t.Fatalf("invalid ticket must be valid=false without transport error: %+v err=%v", invalid, err)
	}

	sample := func(ref string) nexusruntime.EvidenceReadRequest {
		return nexusruntime.EvidenceReadRequest{
			RequestID: "contract-read-0001", BusinessContextRef: "wc_0123456789abcdef0123",
			EvidenceRef: ref, Purpose: "contract_test",
			ExpiresAt: time.Now().Add(time.Minute).UTC(),
		}
	}
	located, err := c.Locate(ctx, nexusruntime.EvidenceRequest{
		RequestID: "contract-locate-0001", Purpose: "contract_test",
		DataNeeds: []nexusruntime.DataNeed{{NeedID: "need-1", DataClass: "test.evidence", Purpose: "contract_test"}},
		ExpiresAt: time.Now().Add(time.Minute).UTC(),
	})
	if err != nil || len(located.Evidence) != 1 || located.Evidence[0].EvidenceRef == "" {
		t.Fatalf("locate: %+v err=%v", located, err)
	}

	read, err := c.Read(ctx, sample(located.Evidence[0].EvidenceRef))
	if err != nil || read.GrantRef == "" || read.Decision != "allow" {
		t.Fatalf("read: %+v err=%v", read, err)
	}
	// The freshness disclosure must survive decoding: a caller that cannot see
	// served_from_cache cannot tell a staged answer from a live one.
	if read.AsOf == "" || read.SourceVersion == 0 {
		t.Fatalf("freshness disclosure lost: %+v", read)
	}

	if _, err := c.Read(ctx, sample("evd_denied0000000000")); !errors.Is(err, ErrDenied) {
		t.Fatalf("denied read must map to ErrDenied, got %v", err)
	}

	audit, err := c.AppendAuditEvidence(ctx, nexus.AppendAuditEvidenceRequest{
		IdempotencyKey: "audit-contract-key-0001",
		TicketID:       "tick_ok", EnterpriseID: "ent_1", Action: nexus.AuditEvidenceRead,
		ResourceType: "answer_trace", ResourceID: "trace-1",
	})
	if err != nil || audit.AuditRefID != "audit_1" {
		t.Fatalf("audit: %+v err=%v", audit, err)
	}

	var got []nexus.OrgEvent
	err = c.SubscribeOrgEvents(ctx, "ent_1", 0, func(_ context.Context, ev nexus.OrgEvent) error {
		got = append(got, ev)
		return nil
	})
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	if len(got) != 2 || got[1].OrgVersion != 2 {
		t.Fatalf("org events = %+v", got)
	}
}

func TestHTTPClientServiceCredentialsFailClosed(t *testing.T) {
	good := writeServiceSecret(t, "AgentAtlas-Nexus-Service-Q7mV2xK9pR4tY8dF3")
	for name, values := range map[string][2]string{
		"wrong client id": {"console", good},
		"missing file":    {"agentatlas", filepath.Join(t.TempDir(), "missing")},
		"relative path":   {"agentatlas", "relative.secret"},
		"weak secret":     {"agentatlas", writeServiceSecret(t, "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")},
		"whitespace":      {"agentatlas", writeServiceSecret(t, "AgentAtlas-Nexus-Service-Q7mV2xK9pR4tY8dF3\n")},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := New("https://nexus.example", time.Second, values[0], values[1], nil); err == nil {
				t.Fatal("unsafe service credential configuration accepted")
			}
		})
	}
	if runtime.GOOS != "windows" {
		if err := os.Chmod(good, 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := New("https://nexus.example", time.Second, "agentatlas", good, nil); err == nil {
			t.Fatal("broad service secret permissions accepted")
		}
	}
}

func TestHTTPClientNeverForwardsServiceCredentialOnRedirect(t *testing.T) {
	leaked := false
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/audit/evidence", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/redirect-target", http.StatusTemporaryRedirect)
	})
	mux.HandleFunc("POST /redirect-target", func(w http.ResponseWriter, r *http.Request) {
		_, _, leaked = r.BasicAuth()
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(nexus.AppendAuditEvidenceResponse{AuditRefID: "unexpected"})
	})
	server := httptest.NewServer(mux)
	defer server.Close()
	client, err := New(server.URL, time.Second, "agentatlas", writeServiceSecret(t, "AgentAtlas-Nexus-Service-Q7mV2xK9pR4tY8dF3"), nil)
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.AppendAuditEvidence(context.Background(), nexus.AppendAuditEvidenceRequest{TicketID: "ticket", EnterpriseID: "ent", Action: nexus.AuditEvidenceRead, ResourceType: "answer_trace", ResourceID: "trace"})
	if err == nil {
		t.Fatal("audit redirect accepted")
	}
	if leaked {
		t.Fatal("service credential leaked across audit redirect")
	}
}

func TestHTTPClientMapsAuditPayloadMismatchToConflict(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/audit/evidence" || r.Header.Get("Idempotency-Key") != "audit-conflict-key-0001" {
			t.Fatalf("request path=%s key=%q", r.URL.Path, r.Header.Get("Idempotency-Key"))
		}
		http.Error(w, `{"error":"idempotency_conflict"}`, http.StatusConflict)
	}))
	defer server.Close()
	client, err := New(server.URL, time.Second, "agentatlas", writeServiceSecret(t, "AgentAtlas-Nexus-Service-Q7mV2xK9pR4tY8dF3"), nil)
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.AppendAuditEvidence(context.Background(), nexus.AppendAuditEvidenceRequest{IdempotencyKey: "audit-conflict-key-0001", TicketID: "ticket", EnterpriseID: "ent", Action: nexus.AuditDreamPolicyCreated, ResourceType: "dream_policy", ResourceID: "policy"})
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("error=%v", err)
	}
}

func TestMockImplementsContract(t *testing.T) {
	m := NewMock()
	m.Tickets["tick_ok"] = nexus.VerifyTicketResponse{Valid: true, EnterpriseID: "ent_1"}
	m.OrgEvents = []nexus.OrgEvent{
		{EventID: "e1", EnterpriseID: "ent_1", OrgVersion: 1, Type: nexus.OrgEmployeeUpserted,
			Scope: nexus.OrgScope{Kind: nexus.ScopeEmployee, ID: "u1", Name: "员工"}},
	}
	ctx := context.Background()

	if resp, _ := m.VerifyTicket(ctx, nexus.VerifyTicketRequest{TicketID: "tick_ok"}); !resp.Valid {
		t.Fatal("mock verify failed")
	}
	var n int
	if err := m.SubscribeOrgEvents(ctx, "ent_1", 0, func(context.Context, nexus.OrgEvent) error {
		n++
		return nil
	}); err != nil || n != 1 {
		t.Fatalf("mock subscribe n=%d err=%v", n, err)
	}
	if _, err := m.AppendAuditEvidence(ctx, nexus.AppendAuditEvidenceRequest{Action: nexus.AuditEvidenceRead}); err != nil {
		t.Fatal(err)
	}
	if len(m.AuditLog) != 1 {
		t.Fatalf("audit log = %d", len(m.AuditLog))
	}
}
