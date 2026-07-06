package app

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	db "github.com/astraclawteam/agentatlas/services/agentatlas/db/generated"
	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/retrieval"
)

// MethodOutlineStore is the store slice the outline routes need (fakeable in
// unit tests; *db.Queries satisfies it).
type MethodOutlineStore interface {
	CreateMethodOutline(ctx context.Context, arg db.CreateMethodOutlineParams) (db.MethodOutline, error)
	ListMethodOutlines(ctx context.Context, enterpriseID string) ([]db.MethodOutline, error)
	CreateIndexJob(ctx context.Context, arg db.CreateIndexJobParams) (db.IndexJob, error)
}

// methodOutlineHandler serves Method Outline import + listing (§5.5 产品设计:
// 比 SOP 更灵活的“做某件事的方法结构”). Outlines index into OpenSearch so the
// Knowledge Agent retrieves them alongside SOP/dream/timeline sources.
type methodOutlineHandler struct {
	deps  AgentRouterDeps
	store MethodOutlineStore
}

type createMethodOutlineRequest struct {
	Title    string          `json:"title"`
	Outline  json.RawMessage `json:"outline"` // sections/steps of arbitrary nesting
	OrgScope string          `json:"org_scope,omitempty"`
}

func (h *methodOutlineHandler) create(w http.ResponseWriter, r *http.Request) {
	actor, ok := actorFrom(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "unauthorized", "no verified actor")
		return
	}
	if h.store == nil || h.deps.Runner == nil {
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", "method outline store/queue not configured")
		return
	}
	var req createMethodOutlineRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	if req.Title == "" || len(req.Outline) == 0 {
		writeError(w, http.StatusBadRequest, "bad_request", "title and outline are required")
		return
	}
	if !json.Valid(req.Outline) {
		writeError(w, http.StatusBadRequest, "bad_request", "outline must be valid JSON (sections/steps)")
		return
	}
	row, err := h.store.CreateMethodOutline(r.Context(), db.CreateMethodOutlineParams{
		ID: newID("mo"), EnterpriseID: actor.Ticket.EnterpriseID,
		Title: req.Title, Outline: req.Outline, OrgScope: req.OrgScope,
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "create_failed", err.Error())
		return
	}
	// make it retrievable: enqueue an index job (consumed by atlas-worker)
	idx, err := h.store.CreateIndexJob(r.Context(), db.CreateIndexJobParams{
		ID: newID("idx"), EnterpriseID: actor.Ticket.EnterpriseID,
		SourceType: "method_outline", SourceID: row.ID, Status: "pending",
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "index_job_failed", err.Error())
		return
	}
	if err := h.deps.Runner.Enqueue(r.Context(), retrieval.JobTypeIndex, idx.ID); err != nil {
		writeError(w, http.StatusInternalServerError, "enqueue_failed", err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{
		"method_outline_id": row.ID, "title": row.Title, "org_scope": row.OrgScope,
	})
}

func (h *methodOutlineHandler) list(w http.ResponseWriter, r *http.Request) {
	actor, ok := actorFrom(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "unauthorized", "no verified actor")
		return
	}
	rows, err := h.store.ListMethodOutlines(r.Context(), actor.Ticket.EnterpriseID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "list_failed", err.Error())
		return
	}
	out := make([]map[string]any, 0, len(rows))
	for _, row := range rows {
		out = append(out, map[string]any{
			"method_outline_id": row.ID, "title": row.Title,
			"org_scope": row.OrgScope, "outline": json.RawMessage(row.Outline),
			"created_at": row.CreatedAt.Time.Format(time.RFC3339),
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"method_outlines": out})
}
