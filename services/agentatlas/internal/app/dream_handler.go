package app

import (
	"encoding/json"
	"net/http"

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

type createDreamPolicyRequest struct {
	OrgScope          string   `json:"org_scope"`
	Schedule          string   `json:"schedule"`
	InputSources      []string `json:"input_sources"`
	VisibilityLevel   string   `json:"visibility_level"`
	MaskingRules      []string `json:"masking_rules"`
	RiskSignalRules   []string `json:"risk_signal_rules"`
	EvidenceRetention string   `json:"evidence_retention"`
	OutputSpaceID     string   `json:"output_space_id"`
}

func (h *dreamPolicyHandler) create(w http.ResponseWriter, r *http.Request) {
	actor, ok := actorFrom(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "unauthorized", "no verified actor")
		return
	}
	var req createDreamPolicyRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	policyID, err := h.deps.Dreams.CreateDraft(r.Context(), actor.Ticket.EnterpriseID, dream.Policy{
		OrgScope: req.OrgScope, Schedule: req.Schedule,
		InputSources: req.InputSources, VisibilityLevel: req.VisibilityLevel,
		MaskingRules: req.MaskingRules, RiskSignalRules: req.RiskSignalRules,
		EvidenceRetention: req.EvidenceRetention, OutputSpaceID: req.OutputSpaceID,
	})
	if err != nil {
		writeError(w, http.StatusUnprocessableEntity, "invalid_policy", err.Error())
		return
	}
	version, err := h.deps.Dreams.Publish(r.Context(), policyID)
	if err != nil {
		writeError(w, http.StatusUnprocessableEntity, "publish_failed", err.Error())
		return
	}
	// Policy creation governs data visibility — audit append is mandatory.
	if _, err := h.deps.Nexus.AppendAuditEvidence(r.Context(), nexus.AppendAuditEvidenceRequest{
		TicketID: actor.TicketID, EnterpriseID: actor.Ticket.EnterpriseID,
		Action: nexus.AuditDreamPolicyCreated,
		Details: map[string]any{
			"dream_policy_id": policyID, "org_scope": req.OrgScope,
			"visibility_level": req.VisibilityLevel,
		},
	}); err != nil {
		writeError(w, http.StatusInternalServerError, "audit_failed", err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"dream_policy_id": policyID, "version": version})
}
