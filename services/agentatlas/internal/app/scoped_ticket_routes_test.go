package app

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	sdknexus "github.com/astraclawteam/agentatlas/sdk/go/nexus"
	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/governance"
	nexusclient "github.com/astraclawteam/agentatlas/services/agentatlas/internal/nexusclient"
	"github.com/go-chi/chi/v5"
)

func scopedWorkflowTicket(resourceID string, actions ...string) *nexusclient.Mock {
	mock := nexusclient.NewMock()
	mock.Tickets["scoped"] = sdknexus.VerifyTicketResponse{
		Valid: true, EnterpriseID: "ent_1", ActorUserID: "editor", Scopes: []string{"workflow_edit"},
		OrgVersion: 2, OrgUnitIDs: []string{"team"}, ResourceType: "workflow", ResourceID: resourceID,
		AllowedActions: actions, ExpiresAt: time.Now().Add(time.Hour),
	}
	return mock
}

func TestDecisionRequiresIdempotencyKey(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/api/changes/chg/decisions", strings.NewReader(`{"decision":"approve"}`))
	rr := httptest.NewRecorder()
	(&changeHandler{}).decide(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rr.Code)
	}
}

func TestScopedTicketCannotReachUnrelatedAgentRoutes(t *testing.T) {
	router := NewAgentRouter(AgentRouterDeps{Nexus: scopedWorkflowTicket("wf-bound", "workflow.edit", "publish")})
	paths := []struct{ method, path string }{
		{http.MethodPost, "/v1/workflows"}, {http.MethodGet, "/v1/workflows"},
		{http.MethodGet, "/v1/workflows/wf-bound"}, {http.MethodPut, "/v1/workflows/wf-bound"},
		{http.MethodGet, "/v1/workflows/wf-bound/diff"}, {http.MethodPost, "/v1/workflows/wf-bound/runs"},
		{http.MethodPost, "/v1/dream-policies"}, {http.MethodGet, "/v1/dream-policies"},
		{http.MethodPost, "/v1/dream-policies/dp/check"}, {http.MethodGet, "/v1/dream/overview"},
		{http.MethodGet, "/v1/dream/runs"}, {http.MethodPost, "/v1/dream/runs/run/annotations"},
		{http.MethodPost, "/v1/method-outlines"}, {http.MethodGet, "/v1/method-outlines"},
		{http.MethodPost, "/v1/agent/runs"}, {http.MethodPost, "/v1/agent/runs/run/messages"},
		{http.MethodPost, "/v1/agent/runs/run/confirmations"},
	}
	for _, tc := range paths {
		req := httptest.NewRequest(tc.method, tc.path, nil)
		req.Header.Set("X-Nexus-Ticket", "scoped")
		rr := httptest.NewRecorder()
		router.ServeHTTP(rr, req)
		if rr.Code != http.StatusForbidden {
			t.Errorf("%s %s = %d, want 403", tc.method, tc.path, rr.Code)
		}
	}
}

func TestScopedWorkflowPublishRequiresExactResourceAndActions(t *testing.T) {
	for _, tc := range []struct {
		name, resource string
		actions        []string
	}{
		{name: "wrong resource", resource: "wf-other", actions: []string{"workflow.edit", "publish"}},
		{name: "missing edit", resource: "wf-bound", actions: []string{"publish"}},
		{name: "missing publish", resource: "wf-bound", actions: []string{"workflow.edit"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			router := NewAgentRouter(AgentRouterDeps{Nexus: scopedWorkflowTicket(tc.resource, tc.actions...)})
			req := httptest.NewRequest(http.MethodPost, "/v1/workflows/wf-bound/publish", nil)
			req.Header.Set("X-Nexus-Ticket", "scoped")
			rr := httptest.NewRecorder()
			router.ServeHTTP(rr, req)
			if rr.Code != http.StatusForbidden {
				t.Fatalf("status = %d, want 403", rr.Code)
			}
		})
	}
}

func TestScopedAndLegacyRouteMiddlewareBoundaries(t *testing.T) {
	called := false
	next := http.HandlerFunc(func(http.ResponseWriter, *http.Request) { called = true })
	legacy := context.WithValue(context.Background(), actorCtxKey{}, actorContext{Ticket: sdknexus.VerifyTicketResponse{Valid: true, EnterpriseID: "ent", ActorUserID: "legacy"}})
	rejectScopedTicket(next).ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil).WithContext(legacy))
	if !called {
		t.Fatal("legacy unscoped ticket was rejected")
	}
}

func TestExactScopedWorkflowPublishMiddlewareAllowsBoundTicket(t *testing.T) {
	called := false
	next := http.HandlerFunc(func(http.ResponseWriter, *http.Request) { called = true })
	ticket := sdknexus.VerifyTicketResponse{Valid: true, EnterpriseID: "ent", ActorUserID: "editor", OrgVersion: 3, OrgUnitIDs: []string{"team"}, ResourceType: "workflow", ResourceID: "wf-bound", AllowedActions: []string{"workflow.edit", "publish"}}
	ctx := context.WithValue(context.Background(), actorCtxKey{}, actorContext{Ticket: ticket})
	routeCtx := chi.NewRouteContext()
	routeCtx.URLParams.Add("id", "wf-bound")
	ctx = context.WithValue(ctx, chi.RouteCtxKey, routeCtx)
	exactScopedWorkflowPublish(next).ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/v1/workflows/wf-bound/publish", nil).WithContext(ctx))
	if !called {
		t.Fatal("exact scoped workflow ticket was rejected")
	}
}

func TestDecisionConflictMapsToHTTP409(t *testing.T) {
	rr := httptest.NewRecorder()
	writeChangeResult(rr, 0, nil, governance.ErrConflict)
	if rr.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409", rr.Code)
	}
}
