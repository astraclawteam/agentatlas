package app

import (
	"context"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/astraclawteam/agentatlas/sdk/go/nexus"
	db "github.com/astraclawteam/agentatlas/services/agentatlas/db/generated"
	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/agent"
	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/assessment"
	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/browsersession"
	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/dream"
	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/governance"
	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/observability"
	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/tasks"
	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/transportsecurity"
	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/workflow"
)

// AgentRouterDeps is the composition surface for the atlas-agent control
// plane (api/openapi/atlas-agent.yaml): Knowledge Agent runs, workflow
// draft/publish, dream policies, confirmations.
type AgentRouterDeps struct {
	Nexus nexus.Client
	// OrgAuthorization answers organization-scope authorization questions. It
	// is separate from Nexus because that question belongs to the
	// authorization surface, not to evidence lookup.
	OrgAuthorization nexus.OrgAuthorizationClient
	// ApprovalTransmitter delivers authored approval plans; ApprovalAuthority
	// names the deployment's approval authority (the customer OA/BPM system).
	ApprovalTransmitter governance.ApprovalTransmitter
	ApprovalAuthority   string
	// WorkCaseContextFor reports the WorkCase handle a governed change belongs
	// to. Transmission needs one because the frozen ApprovalRequest requires an
	// opaque wc_* business context, and dream policy ids are pol_*, so with the
	// default this is dormant until C1 makes governed changes WorkCase-backed.
	// It is a dependency rather than a direct call so both branches stay
	// exercisable: a test can supply a WorkCase-backed change and drive the
	// real transmit-and-refresh path that ships.
	WorkCaseContextFor func(changeID string) (string, bool)
	// Evidence is the frozen-contract evidence surface.
	Evidence               FrozenEvidenceClient
	Agent                  *agent.Runner
	Workflows              *workflow.Service
	Runtime                *workflow.Runtime
	Dreams                 *dream.PolicyService
	DreamRuns              dreamRunStore
	DreamRerun             dreamRerunner
	Store                  *db.Queries
	Outlines               MethodOutlineStore // optional; defaults to Store
	Runner                 *tasks.Runner
	Metrics                *observability.Metrics               // optional; enables /metrics + latency histograms
	BrowserSessions        *browsersession.Service              // optional; enables Console BFF routes
	BrowserHandleProtector *browsersession.Protector            // required for restart-safe opaque Console resource handles
	BrowserOrgStore        browserSessionOrgStore               // optional; defaults to Store
	BrowserKnowledgeStore  browserKnowledgeStore                // optional; defaults to Store
	BrowserAuthorizer      nexus.BrowserBFFClient               // optional; required by advanced legacy BFF routes
	Changes                *governance.Service                  // optional; enables governed maintenance routes
	Assessments            *assessment.ManagerVisibilityService // optional; enables the Task 18D management assessment detail route
	// TLS is this service's own server-identity Manager (the "AgentAtlas"
	// link); optional. When set, /healthz surfaces certificate lifecycle
	// status distinctly from readiness, without leaking key material — see
	// healthzResponse's doc comment in routes.go.
	TLS *transportsecurity.Manager
}

type dreamBackfiller interface {
	Backfill(context.Context, dream.BackfillRequest) (string, error)
	LookupBackfill(context.Context, dream.BackfillRequest) (string, bool, error)
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

func rejectScopedTicket(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		actor, ok := actorFrom(r.Context())
		if !ok {
			writeError(w, http.StatusUnauthorized, "unauthorized", "no verified actor")
			return
		}
		if ticketIsScoped(actor.Ticket) {
			writeError(w, http.StatusForbidden, "ticket_scope_denied", "scoped ticket is not valid for this route")
			return
		}
		next.ServeHTTP(w, r)
	})
}

func exactScopedWorkflowPublish(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		actor, ok := actorFrom(r.Context())
		if !ok {
			writeError(w, http.StatusUnauthorized, "unauthorized", "no verified actor")
			return
		}
		ticket := actor.Ticket
		if !ticketIsScoped(ticket) || ticket.OrgVersion < 1 || ticket.ResourceType != "workflow" || ticket.ResourceID != chi.URLParam(r, "id") || len(ticket.OrgUnitIDs) == 0 || !containsTicketAction(ticket.AllowedActions, "workflow.edit") || !containsTicketAction(ticket.AllowedActions, "publish") {
			writeError(w, http.StatusForbidden, "ticket_scope_denied", "ticket is not bound to this workflow publication")
			return
		}
		next.ServeHTTP(w, r)
	})
}

func ticketIsScoped(ticket nexus.VerifyTicketResponse) bool {
	return ticket.OrgVersion > 0 || len(ticket.OrgUnitIDs) > 0 || ticket.ResourceType != "" || ticket.ResourceID != "" || len(ticket.AllowedActions) > 0 || ticket.ReviewMode != "" || ticket.Queue != ""
}

func containsTicketAction(actions []string, want string) bool {
	for _, action := range actions {
		if action == want {
			return true
		}
	}
	return false
}

// NewAgentRouter builds the atlas-agent HTTP surface. The /v1 route group is
// ticket-guarded (fail closed); handlers are backed by the real services.
func NewAgentRouter(deps AgentRouterDeps) *chi.Mux {
	r := chi.NewRouter()
	r.Use(corsMiddleware)
	if deps.Metrics != nil {
		r.Use(deps.Metrics.Middleware)
		r.Method(http.MethodGet, "/metrics", deps.Metrics.Handler())
	}
	r.Get("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		status := NewHealthStatus("atlas-agent",
			"postgres", "nats", "agentnexus", "llmrouter").MarkReady(true)
		resp := healthzResponse{HealthStatus: status}
		if deps.TLS != nil {
			tlsStatus := deps.TLS.Status()
			resp.TLS = &tlsStatus
			if !tlsStatus.Ready {
				resp.HealthStatus = status.MarkReady(false)
			}
		}
		writeJSON(w, http.StatusOK, resp)
	})

	wf := &workflowHandler{deps: deps}
	dp := &dreamPolicyHandler{deps: deps}
	dreamRuns := deps.DreamRuns
	if dreamRuns == nil && deps.Store != nil {
		dreamRuns = deps.Store
	}
	dr := &dreamRunHandler{store: dreamRuns, evidence: deps.Evidence, nexus: deps.Nexus, orgAuthorization: deps.OrgAuthorization, rerun: deps.DreamRerun, operations: deps.Dreams}
	ar := newAgentRunHandler(deps)
	outlineStore := deps.Outlines
	if outlineStore == nil && deps.Store != nil {
		outlineStore = deps.Store
	}
	mo := &methodOutlineHandler{deps: deps, store: outlineStore}
	{
		orgStore := deps.BrowserOrgStore
		if orgStore == nil && deps.Store != nil {
			orgStore = deps.Store
		}
		browser := &browserSessionHandler{sessions: deps.BrowserSessions, orgs: orgStore}
		knowledgeStore := deps.BrowserKnowledgeStore
		if knowledgeStore == nil && deps.Store != nil {
			knowledgeStore = deps.Store
		}
		knowledge := &browserKnowledgeHandler{orgs: orgStore, store: knowledgeStore, authorizer: deps.BrowserAuthorizer}
		var browserDreamEvidence browserDreamEvidenceClient
		if candidate, ok := deps.BrowserAuthorizer.(browserDreamEvidenceClient); ok {
			browserDreamEvidence = candidate
		}
		var browserBackfill dreamBackfiller
		if candidate, ok := deps.DreamRerun.(dreamBackfiller); ok {
			browserBackfill = candidate
		}
		browserDream := &browserDreamHandler{store: dreamRuns, orgs: orgStore, authorizer: deps.BrowserAuthorizer, evidence: browserDreamEvidence, rerun: deps.DreamRerun, backfill: browserBackfill, operations: deps.Dreams, approvals: deps.ApprovalTransmitter, approvalAuthority: deps.ApprovalAuthority, workCaseContextFor: deps.WorkCaseContextFor, handles: newBrowserDreamHandleCodec(deps.BrowserHandleProtector, nil), bindings: workflowDreamBindingLister{workflows: deps.Workflows, orgs: orgStore}}
		if deps.Changes != nil {
			knowledge.changes = deps.Changes
		}
		var legacyWorkflows legacyWorkflowLister
		if deps.Workflows != nil {
			legacyWorkflows = deps.Workflows
		}
		var legacyDreams legacyDreamLister
		if deps.Dreams != nil {
			legacyDreams = deps.Dreams
		}
		var legacyTraces legacyTraceStore
		if deps.Store != nil {
			legacyTraces = deps.Store
		}
		legacy := &legacyBrowserHandler{authorizer: deps.BrowserAuthorizer, orgs: orgStore, workflows: legacyWorkflows, dreams: legacyDreams, traces: legacyTraces}
		r.Get("/auth/login", browser.login)
		r.Get("/auth/callback", browser.callback)
		r.Get("/api/session", browser.session)
		r.With(browser.sessionGuard).Get("/api/knowledge", knowledge.list)
		assessAgent := &assessmentAgentHandler{manager: deps.Assessments}
		r.With(browser.sessionGuard).Get("/api/assessments/{id}", assessAgent.detail)
		r.Route("/api/dream", func(r chi.Router) {
			r.Use(browser.sessionGuard)
			r.Get("/runs", browserDream.list)
			r.Get("/runs/{id}", browserDream.detail)
			r.Get("/policies", browserDream.listPolicies)
			r.Get("/workflow-bindings", browserDream.listWorkflowBindings)
			r.Get("/policies/{id}/advanced", browserDream.getAdvancedPolicy)
			r.Group(func(r chi.Router) {
				r.Use(sameOriginCSRF)
				r.Post("/runs/{id}/annotations", browserDream.annotate)
				r.Post("/runs/{id}/reruns", browserDream.rerunRun)
				r.Post("/runs/{id}/evidence-access", browserDream.evidenceAccess)
				r.Post("/policies", browserDream.createPolicy)
				r.Post("/policies/{id}/adoptions", browserDream.adoptPolicy)
				r.Put("/policies/{id}", browserDream.updatePolicy)
				r.Put("/policies/{id}/advanced", browserDream.putAdvancedPolicy)
				r.Post("/policies/{id}/check", browserDream.checkPolicy)
				r.Post("/policies/{id}/review", browserDream.reviewPolicy)
				r.Post("/policies/{id}/decisions", browserDream.decidePolicy)
				r.Post("/policies/{id}/publish", browserDream.publishPolicy)
				r.Post("/policies/{id}/disable", browserDream.disablePolicy)
				r.Post("/policies/{id}/backfills", browserDream.backfillPolicy)
			})
		})
		r.With(browser.sessionGuard, sameOriginCSRF).Post("/auth/logout", browser.logout)
		r.Route("/api/legacy", func(r chi.Router) {
			r.Use(browser.sessionGuard)
			for _, surface := range []string{"knowledge", "dream", "workflows", "evidence", "assistant"} {
				r.Get("/"+surface, legacy.read(surface))
			}
			r.With(sameOriginCSRF).Post("/assistant/attachments", legacy.uploadAttachments)
		})
		{
			changes := &changeHandler{service: deps.Changes}
			r.Route("/api/changes", func(r chi.Router) {
				r.Use(browser.sessionGuard)
				r.Use(changeAvailability(deps.Changes))
				r.Get("/", changes.list)
				r.Get("/{id}", changes.get)
				r.Get("/{id}/diff", changes.diff)
				r.Group(func(r chi.Router) {
					r.Use(sameOriginCSRF)
					r.Post("/", changes.create)
					r.Post("/suggestions", changes.suggest)
					r.Put("/{id}", changes.update)
					r.Post("/{id}/assess", changes.assess)
					r.Post("/{id}/submit", changes.submit)
					r.Post("/{id}/decisions", changes.decide)
					r.Post("/{id}/publish", changes.publish)
				})
			})
		}
	}

	r.Route("/v1", func(r chi.Router) {
		r.Use(ticketGuard(deps.Nexus))
		r.Group(func(r chi.Router) {
			r.Use(rejectScopedTicket)
			r.Post("/workflows", wf.create)
			r.Get("/workflows", wf.list)
			r.Get("/workflows/{id}", wf.get)
			r.Put("/workflows/{id}", wf.update)
			r.Get("/workflows/{id}/diff", wf.diff)
			r.Post("/workflows/{id}/runs", wf.startRun)
			r.Post("/dream-policies", dp.create)
			r.Get("/dream-policies", dp.list)
			r.Put("/dream-policies/{id}", dp.update)
			r.Post("/dream-policies/{id}/check", dp.check)
			r.Post("/dream-policies/{id}/review", dp.review)
			r.Post("/dream-policies/{id}/decisions", dp.decide)
			r.Post("/dream-policies/{id}/publish", dp.publish)
			r.Post("/dream-policies/{id}/disable", dp.disable)
			r.Post("/dream-policies/{id}/backfills", dp.backfill)
			r.Get("/dream/overview", dr.overview)
			r.Get("/dream/runs", dr.list)
			r.Get("/dream/runs/{id}", dr.detail)
			r.Post("/dream/runs/{id}/annotations", dr.annotate)
			r.Post("/dream/runs/{id}/reruns", dr.rerunRun)
			r.Post("/dream/runs/{id}/evidence-access", dr.evidenceAccess)
			r.Post("/method-outlines", mo.create)
			r.Get("/method-outlines", mo.list)
			r.Post("/agent/runs", ar.start)
			r.Post("/agent/runs/{id}/messages", ar.message)
			r.Post("/agent/runs/{id}/confirmations", ar.confirm)
		})
		r.With(exactScopedWorkflowPublish).Post("/workflows/{id}/publish", wf.publish)
	})
	return r
}
