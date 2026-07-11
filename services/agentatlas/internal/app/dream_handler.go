package app

import (
	"encoding/json"
	"net/http"

	sdkdream "github.com/astraclawteam/agentatlas/sdk/go/dream"
	"github.com/astraclawteam/agentatlas/sdk/go/nexus"
	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/dream"
)

// dreamPolicyHandler serves Dream Policy creation + publish (versioned) and
// the published-policy listing consumed by the console policy panel.
type dreamPolicyHandler struct {
	deps AgentRouterDeps
}

func (h *dreamPolicyHandler) list(w http.ResponseWriter, r *http.Request) {
	actor, ok := actorFrom(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "unauthorized", "no verified actor")
		return
	}
	policies, err := h.deps.Dreams.ListPublished(r.Context(), actor.Ticket.EnterpriseID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "list_failed", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"dream_policies": policies})
}

func (h *dreamPolicyHandler) create(w http.ResponseWriter, r *http.Request) {
	actor, ok := actorFrom(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "unauthorized", "no verified actor")
		return
	}
	var req sdkdream.DreamPolicyDefinition
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	policy := dream.Policy(req)
	if err := policy.Validate(); err != nil {
		writeError(w, http.StatusUnprocessableEntity, "invalid_policy", err.Error())
		return
	}
	policyID := dream.NewPolicyID()
	// Record the create request before persistence. A failed audit
	// therefore cannot leave an unaudited draft in the local store.
	if _, err := h.deps.Nexus.AppendAuditEvidence(r.Context(), nexus.AppendAuditEvidenceRequest{
		TicketID: actor.TicketID, EnterpriseID: actor.Ticket.EnterpriseID,
		Action: nexus.AuditDreamPolicyCreateRequested, ResourceType: "dream_policy", ResourceID: policyID,
		Details: map[string]any{
			"dream_policy_id": policyID, "org_unit_id": req.OrgUnitID,
			"visibility_level": req.VisibilityLevel, "phase": "create_requested",
		},
	}); err != nil {
		writeError(w, http.StatusInternalServerError, "audit_failed", err.Error())
		return
	}
	if _, err := h.deps.Dreams.CreateDraftWithID(r.Context(), actor.Ticket.EnterpriseID, policyID, policy); err != nil {
		writeError(w, http.StatusUnprocessableEntity, "invalid_policy", err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"dream_policy_id": policyID, "status": "draft"})
}
