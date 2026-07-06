package app

import (
	"encoding/json"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/astraclawteam/agentatlas/sdk/go/nexus"
	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/workflow"
)

// workflowHandler serves workflow draft creation and immutable publish.
type workflowHandler struct {
	deps AgentRouterDeps
}

type createWorkflowRequest struct {
	Name       string              `json:"name"`
	Definition workflow.Definition `json:"definition"`
}

func (h *workflowHandler) create(w http.ResponseWriter, r *http.Request) {
	actor, ok := actorFrom(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "unauthorized", "no verified actor")
		return
	}
	var req createWorkflowRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	if req.Name == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "name is required")
		return
	}
	id, err := h.deps.Workflows.CreateDraft(r.Context(), actor.Ticket.EnterpriseID, req.Name, actor.Ticket.ActorUserID, req.Definition)
	if err != nil {
		writeError(w, http.StatusUnprocessableEntity, "invalid_workflow", err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"workflow_id": id})
}

func (h *workflowHandler) publish(w http.ResponseWriter, r *http.Request) {
	actor, ok := actorFrom(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "unauthorized", "no verified actor")
		return
	}
	workflowID := chi.URLParam(r, "id")
	version, err := h.deps.Workflows.Publish(r.Context(), actor.Ticket.EnterpriseID, workflowID, actor.Ticket.ActorUserID)
	if err != nil {
		writeError(w, http.StatusUnprocessableEntity, "publish_failed", err.Error())
		return
	}
	// Publish is a high-risk admin write: the audit append is mandatory and
	// its failure is surfaced loudly (the version is already immutable — ops
	// must reconcile the audit chain, not silently proceed).
	if _, err := h.deps.Nexus.AppendAuditEvidence(r.Context(), nexus.AppendAuditEvidenceRequest{
		TicketID: actor.TicketID, EnterpriseID: actor.Ticket.EnterpriseID,
		Action: nexus.AuditWorkflowVersionPublished,
		Details: map[string]any{"workflow_id": workflowID, "version": version},
	}); err != nil {
		writeError(w, http.StatusInternalServerError, "audit_failed", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"workflow_id": workflowID, "version": version})
}
