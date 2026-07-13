package app

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"google.golang.org/adk/model"

	"github.com/astraclawteam/agentatlas/sdk/go/nexus"
	db "github.com/astraclawteam/agentatlas/services/agentatlas/db/generated"
	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/artifacts"
	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/observability"
	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/retrieval"
	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/tasks"
	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/trace"
	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/transportsecurity"
)

// healthzResponse embeds HealthStatus (internal/app/health.go) so the
// top-level JSON shape (service/version/ready/dependencies) is byte-for-byte
// unchanged for existing consumers, and adds an optional "tls" field: a
// certificate-lifecycle probe that is DISTINCT from dependency reachability
// (which this handler does not otherwise check — see health.go), and that
// never includes key material (transportsecurity.Status's own contract).
// Omitted entirely when TLS is not configured for this service's own
// server identity (deps.TLS == nil), so a plaintext deployment's /healthz
// body is identical to before this task.
type healthzResponse struct {
	HealthStatus
	TLS *transportsecurity.Status `json:"tls,omitempty"`
}

func newID(prefix string) string {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return prefix + "_" + hex.EncodeToString(b)
}

// RouterDeps is the composition surface for the runtime API.
type RouterDeps struct {
	Nexus     nexus.Client
	Retrieval *retrieval.Service
	Traces    *trace.Service
	LLM       model.LLM
	Store     *db.Queries
	Runner    *tasks.Runner
	Artifacts *artifacts.Service     // optional; enables POST /v1/artifacts/jobs
	PlanSteps PlanStepLister         // optional; defaults to Store
	Metrics   *observability.Metrics // optional; enables /metrics + latency histograms
	// TLS is this service's own server-identity Manager (the "AgentAtlas"
	// link); optional. When set, /healthz surfaces certificate lifecycle
	// status distinctly from readiness, without leaking key material — see
	// healthzResponse's doc comment.
	TLS *transportsecurity.Manager
}

// NewRouter builds the atlas-api HTTP surface (api/openapi/atlas-runtime.yaml).
func NewRouter(deps RouterDeps) *chi.Mux {
	answer := &answerDeps{nexus: deps.Nexus, retrieval: deps.Retrieval, traces: deps.Traces, llm: deps.LLM}
	briefs := &briefDeps{nexus: deps.Nexus, store: deps.Store, runner: deps.Runner}
	artifactsH := &artifactHandler{nexus: deps.Nexus, artifacts: deps.Artifacts}
	planSteps := deps.PlanSteps
	if planSteps == nil && deps.Store != nil {
		planSteps = deps.Store
	}
	retrievalH := &retrievalHandler{nexus: deps.Nexus, retrieval: deps.Retrieval, store: planSteps}

	r := chi.NewRouter()
	r.Use(corsMiddleware)
	if deps.Metrics != nil {
		r.Use(deps.Metrics.Middleware)
		r.Method(http.MethodGet, "/metrics", deps.Metrics.Handler())
	}
	r.Get("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		status := NewHealthStatus("atlas-api",
			"postgres", "opensearch", "nats", "object-storage", "agentnexus", "llmrouter").MarkReady(true)
		resp := healthzResponse{HealthStatus: status}
		if deps.TLS != nil {
			tlsStatus := deps.TLS.Status()
			resp.TLS = &tlsStatus
			if !tlsStatus.Ready {
				// A certificate problem is reported distinctly (via the
				// "tls" field's own detail) from any other dependency
				// issue, but it still fails overall readiness closed.
				resp.HealthStatus = status.MarkReady(false)
			}
		}
		writeJSON(w, http.StatusOK, resp)
	})

	r.Route("/v1", func(r chi.Router) {
		r.Post("/answer", answer.handleAnswer)
		r.Post("/work-briefs", briefs.handleIngest)
		r.Post("/artifacts/jobs", artifactsH.createJob)
		r.Post("/retrieval/plans", retrievalH.createPlan)

		r.Get("/spaces", func(w http.ResponseWriter, req *http.Request) {
			_, ticket, err := verifyTicket(req.Context(), deps.Nexus, req)
			if err != nil {
				writeError(w, http.StatusUnauthorized, "unauthorized", err.Error())
				return
			}
			rows, err := deps.Store.ListKnowledgeSpacesByEnterprise(req.Context(), ticket.EnterpriseID)
			if err != nil {
				writeError(w, http.StatusInternalServerError, "spaces_failed", err.Error())
				return
			}
			out := make([]map[string]any, 0, len(rows))
			for _, s := range rows {
				out = append(out, map[string]any{
					"space_id": s.ID, "enterprise_id": s.EnterpriseID,
					"kind": s.Kind, "name": s.Name,
					"org_scope": s.OrgScope, "org_version": s.OrgVersion,
				})
			}
			writeJSON(w, http.StatusOK, map[string]any{"spaces": out})
		})

		r.Get("/traces", func(w http.ResponseWriter, req *http.Request) {
			_, ticket, err := verifyTicket(req.Context(), deps.Nexus, req)
			if err != nil {
				writeError(w, http.StatusUnauthorized, "unauthorized", err.Error())
				return
			}
			rows, err := deps.Store.ListRecentAnswerTraces(req.Context(), ticket.EnterpriseID)
			if err != nil {
				writeError(w, http.StatusInternalServerError, "traces_failed", err.Error())
				return
			}
			out := make([]map[string]any, 0, len(rows))
			for _, row := range rows {
				out = append(out, map[string]any{
					"trace_id": row.ID, "sanitized_question_summary": row.SanitizedQuestionSummary,
					"space_ids": row.SpaceIds, "evidence_pointer_ids": row.EvidencePointerIds,
					"agentnexus_read_grant_ids": row.AgentnexusReadGrantIds,
					"model_route":               row.ModelRoute, "answer_hash": row.AnswerHash,
					"created_at": row.CreatedAt.Time.Format(time.RFC3339),
				})
			}
			writeJSON(w, http.StatusOK, map[string]any{"traces": out})
		})

		r.Get("/spaces/{id}", func(w http.ResponseWriter, req *http.Request) {
			_, ticket, err := verifyTicket(req.Context(), deps.Nexus, req)
			if err != nil {
				writeError(w, http.StatusUnauthorized, "unauthorized", err.Error())
				return
			}
			space, err := deps.Store.GetKnowledgeSpace(req.Context(), chi.URLParam(req, "id"))
			// Cross-enterprise reads report not_found: space ids must not be
			// enumerable across tenants (isolation invariant).
			if err != nil || space.EnterpriseID != ticket.EnterpriseID {
				writeError(w, http.StatusNotFound, "not_found", "knowledge space not found")
				return
			}
			writeJSON(w, http.StatusOK, map[string]any{
				"space_id": space.ID, "enterprise_id": space.EnterpriseID,
				"kind": space.Kind, "name": space.Name,
				"org_scope": space.OrgScope, "org_version": space.OrgVersion,
			})
		})

		r.Get("/spaces/{id}/timeline", func(w http.ResponseWriter, req *http.Request) {
			_, ticket, err := verifyTicket(req.Context(), deps.Nexus, req)
			if err != nil {
				writeError(w, http.StatusUnauthorized, "unauthorized", err.Error())
				return
			}
			space, err := deps.Store.GetKnowledgeSpace(req.Context(), chi.URLParam(req, "id"))
			if err != nil || space.EnterpriseID != ticket.EnterpriseID {
				writeError(w, http.StatusNotFound, "not_found", "knowledge space not found")
				return
			}
			nodes, err := deps.Store.ListTimelineNodes(req.Context(), db.ListTimelineNodesParams{
				SpaceID: chi.URLParam(req, "id"),
				Column2: parseTime(req.URL.Query().Get("from")),
				Column3: parseTime(req.URL.Query().Get("to")),
				Limit:   50,
			})
			if err != nil {
				writeError(w, http.StatusInternalServerError, "timeline_failed", err.Error())
				return
			}
			out := make([]map[string]any, 0, len(nodes))
			for _, n := range nodes {
				out = append(out, map[string]any{
					"timeline_node_id": n.ID, "enterprise_id": n.EnterpriseID,
					"space_id": n.SpaceID, "org_scope": n.OrgScope,
					"node_time":   n.NodeTime.Time.Format(time.RFC3339),
					"source_type": n.SourceType, "summary_text": n.SummaryText,
					"tags": n.Tags, "evidence_pointer_id": textOrEmptyStr(n.EvidencePointerID),
				})
			}
			writeJSON(w, http.StatusOK, map[string]any{"nodes": out})
		})

		r.Get("/traces/{id}", func(w http.ResponseWriter, req *http.Request) {
			_, ticket, err := verifyTicket(req.Context(), deps.Nexus, req)
			if err != nil {
				writeError(w, http.StatusUnauthorized, "unauthorized", err.Error())
				return
			}
			row, err := deps.Traces.Get(req.Context(), chi.URLParam(req, "id"))
			if err != nil {
				if errors.Is(err, pgx.ErrNoRows) {
					writeError(w, http.StatusNotFound, "not_found", "trace not found")
					return
				}
				writeError(w, http.StatusInternalServerError, "trace_failed", err.Error())
				return
			}
			if row.EnterpriseID != ticket.EnterpriseID {
				// Cross-enterprise trace ids are not enumerable (isolation invariant).
				writeError(w, http.StatusNotFound, "not_found", "trace not found")
				return
			}
			writeJSON(w, http.StatusOK, map[string]any{
				"trace_id": row.ID, "enterprise_id": row.EnterpriseID,
				"case_ticket_id": row.CaseTicketID, "actor_user_id": row.ActorUserID,
				"question_hash":              row.QuestionHash,
				"sanitized_question_summary": row.SanitizedQuestionSummary,
				"space_ids":                  row.SpaceIds, "retrieval_plan_id": textOrEmptyStr(row.RetrievalPlanID),
				"evidence_pointer_ids":      row.EvidencePointerIds,
				"agentnexus_read_grant_ids": row.AgentnexusReadGrantIds,
				"model_route":               row.ModelRoute, "answer_hash": row.AnswerHash,
				"created_at": row.CreatedAt.Time.Format(time.RFC3339),
			})
		})
	})
	return r
}

func parseTime(v string) pgtype.Timestamptz {
	if v == "" {
		return pgtype.Timestamptz{}
	}
	t, err := time.Parse(time.RFC3339, v)
	if err != nil {
		return pgtype.Timestamptz{}
	}
	return pgtype.Timestamptz{Time: t, Valid: true}
}

func textOrEmptyStr(v pgtype.Text) string {
	if v.Valid {
		return v.String
	}
	return ""
}
