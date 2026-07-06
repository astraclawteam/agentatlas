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
	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/retrieval"
	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/tasks"
	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/trace"
)

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
}

// NewRouter builds the atlas-api HTTP surface (api/openapi/atlas-runtime.yaml).
func NewRouter(deps RouterDeps) *chi.Mux {
	answer := &answerDeps{nexus: deps.Nexus, retrieval: deps.Retrieval, traces: deps.Traces, llm: deps.LLM}
	briefs := &briefDeps{nexus: deps.Nexus, store: deps.Store, runner: deps.Runner}

	r := chi.NewRouter()
	r.Get("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, NewHealthStatus("atlas-api",
			"postgres", "opensearch", "nats", "object-storage", "agentnexus", "llmrouter").MarkReady(true))
	})

	r.Route("/v1", func(r chi.Router) {
		r.Post("/answer", answer.handleAnswer)
		r.Post("/work-briefs", briefs.handleIngest)

		r.Get("/spaces/{id}", func(w http.ResponseWriter, req *http.Request) {
			if _, _, err := verifyTicket(req.Context(), deps.Nexus, req); err != nil {
				writeError(w, http.StatusUnauthorized, "unauthorized", err.Error())
				return
			}
			space, err := deps.Store.GetKnowledgeSpace(req.Context(), chi.URLParam(req, "id"))
			if err != nil {
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
			if _, _, err := verifyTicket(req.Context(), deps.Nexus, req); err != nil {
				writeError(w, http.StatusUnauthorized, "unauthorized", err.Error())
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
					"node_time": n.NodeTime.Time.Format(time.RFC3339),
					"source_type": n.SourceType, "summary_text": n.SummaryText,
					"tags": n.Tags, "evidence_pointer_id": textOrEmptyStr(n.EvidencePointerID),
				})
			}
			writeJSON(w, http.StatusOK, map[string]any{"nodes": out})
		})

		r.Get("/traces/{id}", func(w http.ResponseWriter, req *http.Request) {
			if _, _, err := verifyTicket(req.Context(), deps.Nexus, req); err != nil {
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
			writeJSON(w, http.StatusOK, map[string]any{
				"trace_id": row.ID, "enterprise_id": row.EnterpriseID,
				"case_ticket_id": row.CaseTicketID, "actor_user_id": row.ActorUserID,
				"question_hash": row.QuestionHash,
				"sanitized_question_summary": row.SanitizedQuestionSummary,
				"space_ids": row.SpaceIds, "retrieval_plan_id": textOrEmptyStr(row.RetrievalPlanID),
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
