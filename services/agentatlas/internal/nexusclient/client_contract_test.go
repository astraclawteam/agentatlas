package nexusclient

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/astraclawteam/agentatlas/sdk/go/nexus"
)

// contractServer implements the agentnexus-client.yaml proposal surface.
func contractServer(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()

	mux.HandleFunc("POST /v1/tickets/verify", func(w http.ResponseWriter, r *http.Request) {
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
		_ = json.NewEncoder(w).Encode(nexus.LocateEvidenceResponse{
			ResourceURI: "fs://briefs/2026-07-06.md", SourceSystem: "filesystem",
		})
	})

	mux.HandleFunc("POST /v1/evidence/read", func(w http.ResponseWriter, r *http.Request) {
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
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(nexus.AppendAuditEvidenceResponse{AuditRefID: "audit_1"})
	})

	mux.HandleFunc("GET /v1/org-events", func(w http.ResponseWriter, r *http.Request) {
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

func TestHTTPClientContract(t *testing.T) {
	srv := contractServer(t)
	c := New(srv.URL, 5*time.Second)
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
