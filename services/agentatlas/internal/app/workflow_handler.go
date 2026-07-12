package app

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"

	governancemodel "github.com/astraclawteam/agentatlas/sdk/go/governance"
	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/governance"
	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/workflow"
)

type workflowHandler struct{ deps AgentRouterDeps }
type createWorkflowRequest struct {
	Name       string              `json:"name"`
	Definition workflow.Definition `json:"definition"`
}

func (h *workflowHandler) create(w http.ResponseWriter, r *http.Request) {
	actor, ok := actorFrom(r.Context())
	if !ok {
		writeError(w, 401, "unauthorized", "no verified actor")
		return
	}
	var req createWorkflowRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, 400, "bad_request", err.Error())
		return
	}
	if req.Name == "" {
		writeError(w, 400, "bad_request", "name is required")
		return
	}
	id, err := h.deps.Workflows.CreateDraft(r.Context(), actor.Ticket.EnterpriseID, req.Name, actor.Ticket.ActorUserID, req.Definition)
	if err != nil {
		writeError(w, 422, "invalid_workflow", err.Error())
		return
	}
	writeJSON(w, 201, map[string]any{"workflow_id": id})
}
func (h *workflowHandler) list(w http.ResponseWriter, r *http.Request) {
	actor, ok := actorFrom(r.Context())
	if !ok {
		writeError(w, 401, "unauthorized", "no verified actor")
		return
	}
	items, err := h.deps.Workflows.ListDrafts(r.Context(), actor.Ticket.EnterpriseID, 100)
	if err != nil {
		writeError(w, 422, "list_failed", err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"items": items})
}
func (h *workflowHandler) get(w http.ResponseWriter, r *http.Request) {
	actor, ok := actorFrom(r.Context())
	if !ok {
		writeError(w, 401, "unauthorized", "no verified actor")
		return
	}
	item, err := h.deps.Workflows.GetDraft(r.Context(), actor.Ticket.EnterpriseID, chi.URLParam(r, "id"))
	if err != nil {
		writeError(w, 404, "not_found", "workflow not found")
		return
	}
	writeJSON(w, 200, item)
}
func (h *workflowHandler) update(w http.ResponseWriter, r *http.Request) {
	actor, ok := actorFrom(r.Context())
	if !ok {
		writeError(w, 401, "unauthorized", "no verified actor")
		return
	}
	var req createWorkflowRequest
	if json.NewDecoder(r.Body).Decode(&req) != nil {
		writeError(w, 400, "bad_request", "invalid JSON")
		return
	}
	if err := h.deps.Workflows.UpdateDraft(r.Context(), actor.Ticket.EnterpriseID, chi.URLParam(r, "id"), req.Definition); err != nil {
		writeError(w, 422, "update_failed", err.Error())
		return
	}
	item, _ := h.deps.Workflows.GetDraft(r.Context(), actor.Ticket.EnterpriseID, chi.URLParam(r, "id"))
	writeJSON(w, 200, item)
}
func (h *workflowHandler) diff(w http.ResponseWriter, r *http.Request) {
	actor, ok := actorFrom(r.Context())
	if !ok {
		writeError(w, 401, "unauthorized", "no verified actor")
		return
	}
	item, err := h.deps.Workflows.GetDraft(r.Context(), actor.Ticket.EnterpriseID, chi.URLParam(r, "id"))
	if err != nil {
		writeError(w, 404, "not_found", "workflow not found")
		return
	}
	before := json.RawMessage(`null`)
	if item.Definition.Version > 0 {
		if latest, err := h.deps.Workflows.VersionDefinition(r.Context(), item.ID, int32(item.Definition.Version)); err == nil {
			before, _ = json.Marshal(latest)
		}
	}
	after, _ := json.Marshal(item.Definition)
	writeJSON(w, 200, map[string]any{"before": before, "after": json.RawMessage(after), "changed": !bytes.Equal(before, after)})
}

type startRunRequest struct {
	Version int32          `json:"version"`
	Input   map[string]any `json:"input,omitempty"`
}

func (h *workflowHandler) startRun(w http.ResponseWriter, r *http.Request) {
	actor, ok := actorFrom(r.Context())
	if !ok {
		writeError(w, 401, "unauthorized", "no verified actor")
		return
	}
	if h.deps.Runtime == nil || h.deps.Runner == nil {
		writeError(w, 503, "runtime_unavailable", "workflow runtime/queue not configured")
		return
	}
	var req startRunRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, 400, "bad_request", err.Error())
		return
	}
	if req.Version <= 0 {
		writeError(w, 400, "bad_request", "version (>0, published) is required")
		return
	}
	runID, err := h.deps.Runtime.CreatePending(r.Context(), actor.Ticket.EnterpriseID, chi.URLParam(r, "id"), req.Version, req.Input)
	if err != nil {
		if errors.Is(err, workflow.ErrWorkflowForbidden) {
			writeError(w, 404, "not_found", "workflow not found")
			return
		}
		writeError(w, 422, "run_failed", err.Error())
		return
	}
	if err := h.deps.Runner.Enqueue(r.Context(), workflow.JobTypeRun, runID); err != nil {
		writeError(w, 500, "enqueue_failed", err.Error())
		return
	}
	writeJSON(w, 202, map[string]any{"run_id": runID, "status": workflow.RunPending})
}

func (h *workflowHandler) publish(w http.ResponseWriter, r *http.Request) {
	actor, ok := actorFrom(r.Context())
	if !ok {
		writeError(w, 401, "unauthorized", "no verified actor")
		return
	}
	if h.deps.Changes == nil {
		writeError(w, 503, "governance_unavailable", "workflow publishing requires governance")
		return
	}
	workflowID := chi.URLParam(r, "id")
	var req struct {
		ChangeID  string `json:"change_id"`
		OrgUnitID string `json:"org_unit_id"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	gactor := governance.Actor{EnterpriseID: actor.Ticket.EnterpriseID, UserID: actor.Ticket.ActorUserID, OrgUnitIDs: []string{req.OrgUnitID}, Permissions: ticketPermissions(actor.Ticket.Scopes)}
	if req.ChangeID != "" {
		result, err := h.deps.Changes.Publish(r.Context(), gactor, req.ChangeID, r.Header.Get("Idempotency-Key"))
		if err != nil {
			writeChangeResult(w, 0, nil, err)
			return
		}
		writeJSON(w, 200, result)
		return
	}
	if req.OrgUnitID == "" {
		writeError(w, 400, "bad_request", "org_unit_id is required")
		return
	}
	draft, err := h.deps.Workflows.GetDraft(r.Context(), actor.Ticket.EnterpriseID, workflowID)
	if err != nil {
		writeError(w, 404, "not_found", "workflow not found")
		return
	}
	content, _ := json.Marshal(draft.Definition)
	change, err := h.deps.Changes.CreateDraft(r.Context(), gactor, governance.SuggestionInput{OrgUnitID: req.OrgUnitID, ResourceType: governancemodel.ResourceWorkflow, ResourceID: workflowID, Action: governancemodel.ActionPublish, BaseVersion: draft.LatestVersion, ProposedContent: content})
	if err != nil {
		writeChangeResult(w, 0, nil, err)
		return
	}
	route, err := h.deps.Changes.Submit(r.Context(), gactor, change.ChangeID)
	if err != nil {
		writeChangeResult(w, 0, nil, err)
		return
	}
	writeJSON(w, 202, map[string]any{"change": change, "review_route": route})
}
func ticketPermissions(scopes []string) []string {
	out := []string{}
	for _, scope := range scopes {
		switch scope {
		case "admin":
			out = append(out, "edit", "publish_low_risk", "approve_high_risk")
		case "edit", "workflow.edit":
			out = append(out, "edit")
		case "publish_low_risk", "workflow.publish_low_risk":
			out = append(out, "publish_low_risk")
		case "approve_high_risk", "workflow.approve_high_risk":
			out = append(out, "approve_high_risk")
		}
	}
	return out
}
