package app

import (
	"context"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/astraclawteam/agentatlas/sdk/go/nexus"
	db "github.com/astraclawteam/agentatlas/services/agentatlas/db/generated"
	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/agent"
	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/dream"
	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/observability"
	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/tasks"
	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/workflow"
)

// AgentRouterDeps is the composition surface for the atlas-agent control
// plane (api/openapi/atlas-agent.yaml): Knowledge Agent runs, workflow
// draft/publish, dream policies, confirmations.
type AgentRouterDeps struct {
	Nexus     nexus.Client
	Agent     *agent.Runner
	Workflows *workflow.Service
	Runtime   *workflow.Runtime
	Dreams    *dream.PolicyService
	Store     *db.Queries
	Runner    *tasks.Runner
	Metrics   *observability.Metrics // optional; enables /metrics + latency histograms
}

// actorContext carries the verified ticket identity through the request.
type actorContext struct {
	TicketID string
	Ticket   nexus.VerifyTicketResponse
}

type actorCtxKey struct{}

// actorFrom returns the verified actor stashed by ticketGuard.
func actorFrom(ctx context.Context) (actorContext, bool) {
	a, ok := ctx.Value(actorCtxKey{}).(actorContext)
	return a, ok
}

// ticketGuard enforces X-Nexus-Ticket on every control-plane route: missing or
// invalid tickets fail closed with 401 before any handler logic runs. The
// verified actor is stashed in the request context for handlers.
func ticketGuard(client nexus.Client) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ticketID, resp, err := verifyTicket(r.Context(), client, r)
			if err != nil {
				writeError(w, http.StatusUnauthorized, "unauthorized", err.Error())
				return
			}
			ctx := context.WithValue(r.Context(), actorCtxKey{}, actorContext{TicketID: ticketID, Ticket: resp})
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// NewAgentRouter builds the atlas-agent HTTP surface. The /v1 route group is
// ticket-guarded (fail closed); handlers are backed by the real services.
func NewAgentRouter(deps AgentRouterDeps) *chi.Mux {
	r := chi.NewRouter()
	if deps.Metrics != nil {
		r.Use(deps.Metrics.Middleware)
		r.Method(http.MethodGet, "/metrics", deps.Metrics.Handler())
	}
	r.Get("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, NewHealthStatus("atlas-agent",
			"postgres", "nats", "agentnexus", "llmrouter").MarkReady(true))
	})

	wf := &workflowHandler{deps: deps}
	dp := &dreamPolicyHandler{deps: deps}
	ar := newAgentRunHandler(deps)

	r.Route("/v1", func(r chi.Router) {
		r.Use(ticketGuard(deps.Nexus))
		r.Post("/workflows", wf.create)
		r.Post("/workflows/{id}/publish", wf.publish)
		r.Post("/workflows/{id}/runs", wf.startRun)
		r.Post("/dream-policies", dp.create)
		r.Post("/agent/runs", ar.start)
		r.Post("/agent/runs/{id}/messages", ar.message)
		r.Post("/agent/runs/{id}/confirmations", ar.confirm)
	})
	return r
}
