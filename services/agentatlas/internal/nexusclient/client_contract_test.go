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
)

// contractServer implements the agentnexus-client.yaml surface.
func contractServer(t *testing.T, serviceSecret string) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()

	mux.HandleFunc("POST /v1/tickets/verify", func(w http.ResponseWriter, r *http.Request) {
		assertNoServiceAuthorization(t, r)
		var req nexus.VerifyTicketRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		resp := nexus.VerifyTicketResponse{Valid: false}
		if req.TicketID == "tick_ok" {
			resp = nexus.VerifyTicketResponse{
				Valid: true, EnterpriseID: "ent_1", ActorUserID: "u_zhang",
				Scopes: []string{"space.read"}, ExpiresAt: time.Now().Add(time.Hour),
			}
		}
		_ = json.NewEncoder(w).Encode(resp)
	})

	mux.HandleFunc("POST /v1/evidence/locate", func(w http.ResponseWriter, r *http.Request) {
		assertNoServiceAuthorization(t, r)
		_ = json.NewEncoder(w).Encode(nexus.LocateEvidenceResponse{
			ResourceURI: "fs://briefs/2026-07-06.md", SourceSystem: "filesystem",
		})
	})

	mux.HandleFunc("POST /v1/evidence/read", func(w http.ResponseWriter, r *http.Request) {
		assertNoServiceAuthorization(t, r)
		var req nexus.ReadEvidenceRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		if req.TicketID == "tick_denied" {
			http.Error(w, `{"code":"forbidden","message":"scope missing"}`, http.StatusForbidden)
			return
		}
		_ = json.NewEncoder(w).Encode(nexus.ReadEvidenceResponse{
			GrantID: "grant_1", ContentType: "text/plain",
			SanitizedExcerpt: "完成分拣规则联调", ContentHash: "sha256:x",
		})
	})

	mux.HandleFunc("POST /v1/audit/evidence", func(w http.ResponseWriter, r *http.Request) {
		clientID, secret, ok := r.BasicAuth()
		if !ok || clientID != "agentatlas" || secret != serviceSecret {
			t.Errorf("audit Basic credentials client=%q ok=%t", clientID, ok)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
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
	c, err := New(srv.URL, 5*time.Second, "agentatlas", writeServiceSecret(t, serviceSecret))
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

	loc, err := c.LocateEvidence(ctx, nexus.LocateEvidenceRequest{TicketID: "tick_ok", EnterpriseID: "ent_1", EvidencePointerID: "ev_1"})
	if err != nil || loc.SourceSystem != "filesystem" {
		t.Fatalf("locate: %+v err=%v", loc, err)
	}

	read, err := c.ReadEvidence(ctx, nexus.ReadEvidenceRequest{TicketID: "tick_ok", EnterpriseID: "ent_1", ResourceURI: loc.ResourceURI})
	if err != nil || read.GrantID != "grant_1" || read.SanitizedExcerpt == "" {
		t.Fatalf("read: %+v err=%v", read, err)
	}

	_, err = c.ReadEvidence(ctx, nexus.ReadEvidenceRequest{TicketID: "tick_denied", EnterpriseID: "ent_1", ResourceURI: loc.ResourceURI})
	if !errors.Is(err, ErrDenied) {
		t.Fatalf("denied read must map to ErrDenied, got %v", err)
	}

	audit, err := c.AppendAuditEvidence(ctx, nexus.AppendAuditEvidenceRequest{
		TicketID: "tick_ok", EnterpriseID: "ent_1", Action: nexus.AuditEvidenceRead,
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
			if _, err := New("https://nexus.example", time.Second, values[0], values[1]); err == nil {
				t.Fatal("unsafe service credential configuration accepted")
			}
		})
	}
	if runtime.GOOS != "windows" {
		if err := os.Chmod(good, 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := New("https://nexus.example", time.Second, "agentatlas", good); err == nil {
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
	client, err := New(server.URL, time.Second, "agentatlas", writeServiceSecret(t, "AgentAtlas-Nexus-Service-Q7mV2xK9pR4tY8dF3"))
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
	if _, err := m.ReadEvidence(ctx, nexus.ReadEvidenceRequest{ResourceURI: "fs://missing"}); !errors.Is(err, ErrDenied) {
		t.Fatalf("mock unknown read must deny, got %v", err)
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
