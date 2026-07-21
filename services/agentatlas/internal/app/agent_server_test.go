package app

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/astraclawteam/agentatlas/sdk/go/nexus"
	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/nexusclient"
)

func TestAgentServerHealthAndTicketGuard(t *testing.T) {
	mock := nexusclient.NewMock()
	mock.Tickets["tick_admin"] = nexus.VerifyTicketResponse{
		Valid: true, EnterpriseID: "ent_1", ActorUserID: "admin",
		Scopes: []string{"admin"}, ExpiresAt: time.Now().Add(time.Hour),
	}
	router := NewAgentRouter(AgentRouterDeps{OrgAuthorization: &allowOrgAuthorization{}, ApprovalTransmitter: &fakeApprovalTransmitter{decision: nexusclient.ApprovalApproved, authority: "oa.example"}, ApprovalAuthority: "oa.example", Nexus: mock})
	srv := httptest.NewServer(router)
	defer srv.Close()

	// health is public and ready
	resp, err := http.Get(srv.URL + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("healthz = %d", resp.StatusCode)
	}

	call := func(ticket string) int {
		req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/agent/runs", strings.NewReader(`{}`))
		req.Header.Set("Content-Type", "application/json")
		if ticket != "" {
			req.Header.Set("X-Nexus-Ticket", ticket)
		}
		r, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		r.Body.Close()
		return r.StatusCode
	}

	// every control-plane route fails closed without a ticket
	if got := call(""); got != http.StatusUnauthorized {
		t.Fatalf("ticketless = %d, want 401", got)
	}
	if got := call("tick_invalid"); got != http.StatusUnauthorized {
		t.Fatalf("invalid ticket = %d, want 401", got)
	}
	// valid ticket passes the guard; with no Agent runner configured the real
	// handler (Goal B4) answers 503 — the point here is it is NOT 401.
	if got := call("tick_admin"); got != http.StatusServiceUnavailable {
		t.Fatalf("authed = %d, want 503 (guard passed, agent unconfigured)", got)
	}
}
