package app

import (
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
	Dreams    *dream.PolicyService
	Store     *db.Queries
	Runner    *tasks.Runner
	Metrics   *observability.Metrics // optional; enables /metrics + latency histograms
}

// ticketGuard enforces X-Nexus-Ticket on every control-plane route: missing or
// invalid tickets fail closed with 401 before any handler logic runs.
func ticketGuard(client nexus.Client) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if _, _, err := verifyTicket(r.Context(), client, r); err != nil {
				writeError(w, http.StatusUnauthorized, "unauthorized", err.Error())
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// NewAgentRouter builds the atlas-agent HTTP surface. The /v1 route group is
// ticket-guarded; the concrete agent/admin handlers are mounted by
// mountAgentRoutes (Goal B4) — until then guarded routes answer 501 so the
// serving skeleton (auth, health, metrics) is real from day one.
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

	r.Route("/v1", func(r chi.Router) {
		r.Use(ticketGuard(deps.Nexus))
		mountAgentRoutes(r, deps)
	})
	return r
}

// mountAgentRoutes registers the control-plane endpoints. Goal B4 replaces the
// 501 stubs with real handlers backed by the services in AgentRouterDeps.
func mountAgentRoutes(r chi.Router, _ AgentRouterDeps) {
	notImplemented := func(w http.ResponseWriter, _ *http.Request) {
		writeError(w, http.StatusNotImplemented, "not_implemented", "lands with Goal B4")
	}
	r.Post("/agent/runs", notImplemented)
	r.Post("/agent/runs/{id}/messages", notImplemented)
	r.Post("/agent/runs/{id}/confirmations", notImplemented)
	r.Post("/workflows", notImplemented)
	r.Post("/workflows/{id}/publish", notImplemented)
	r.Post("/dream-policies", notImplemented)
}
