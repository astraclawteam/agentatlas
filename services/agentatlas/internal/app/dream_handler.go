package app

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	sdkdream "github.com/astraclawteam/agentatlas/sdk/go/dream"
	"github.com/astraclawteam/agentatlas/sdk/go/governance"
	"github.com/astraclawteam/agentatlas/sdk/go/nexus"
	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/dream"
	"github.com/go-chi/chi/v5"
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
	mode := governance.PermissionSuggestionOnly
	if hasScope(actor.Ticket.Scopes, "edit") || hasScope(actor.Ticket.Scopes, "admin") {
		mode = governance.PermissionDirectEdit
	} else if !hasScope(actor.Ticket.Scopes, "suggest") {
		writeError(w, http.StatusForbidden, "forbidden", "edit or suggest is required")
		return
	}
	if mode == governance.PermissionDirectEdit {
		if !h.authorizeOrg(w, r, "edit", req.OrgUnitID) {
			return
		}
	} else if !h.authorizeOrg(w, r, "suggest", req.OrgUnitID) {
		return
	}
	// Record the create request before persistence. A failed audit
	// therefore cannot leave an unaudited draft in the local store.
	audit, err := h.deps.Nexus.AppendAuditEvidence(r.Context(), nexus.AppendAuditEvidenceRequest{
		TicketID: actor.TicketID, EnterpriseID: actor.Ticket.EnterpriseID,
		Action: nexus.AuditDreamPolicyCreateRequested, ResourceType: "dream_policy", ResourceID: policyID,
		Details: map[string]any{
			"dream_policy_id": policyID, "org_unit_id": req.OrgUnitID,
			"visibility_level": req.VisibilityLevel, "phase": "create_requested",
		},
	})
	if err != nil || strings.TrimSpace(audit.AuditRefID) == "" {
		if err == nil {
			err = fmt.Errorf("AgentNexus returned no durable audit reference")
		}
		writeError(w, http.StatusInternalServerError, "audit_failed", err.Error())
		return
	}
	view, err := h.deps.Dreams.CreateGovernedDraft(r.Context(), actor.Ticket.EnterpriseID, policyID, actor.Ticket.ActorUserID, mode, audit.AuditRefID, policy)
	if err != nil {
		writeError(w, http.StatusUnprocessableEntity, "invalid_policy", err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, view)
}

func (h *dreamPolicyHandler) update(w http.ResponseWriter, r *http.Request) {
	actor, _ := actorFrom(r.Context())
	var req struct {
		Revision int32                          `json:"revision"`
		Policy   sdkdream.DreamPolicyDefinition `json:"policy"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	if !hasScope(actor.Ticket.Scopes, "edit") && !hasScope(actor.Ticket.Scopes, "admin") {
		writeError(w, 403, "forbidden", "edit is required")
		return
	}
	id := chi.URLParam(r, "id")
	current, err := h.deps.Dreams.GetLifecycle(r.Context(), actor.Ticket.EnterpriseID, id)
	if err != nil {
		writeError(w, 404, "not_found", "Dream policy not found")
		return
	}
	if current.PermissionMode == governance.PermissionSuggestionOnly {
		writeError(w, 403, "suggestion_only", "employee suggestions cannot be directly modified")
		return
	}
	if !h.authorizeOrg(w, r, "edit", current.Policy.OrgUnitID) || !h.authorizeOrg(w, r, "edit", req.Policy.OrgUnitID) {
		return
	}
	audit, ok := h.audit(w, r, nexus.AuditDreamPolicyCreated, id, map[string]any{"revision": req.Revision, "phase": "update"})
	if !ok {
		return
	}
	view, err := h.deps.Dreams.UpdateGovernedDraft(r.Context(), actor.Ticket.EnterpriseID, id, actor.Ticket.ActorUserID, audit, req.Revision, dream.Policy(req.Policy))
	if err != nil {
		writeError(w, 409, "revision_conflict", err.Error())
		return
	}
	writeJSON(w, 200, view)
}

func (h *dreamPolicyHandler) review(w http.ResponseWriter, r *http.Request) {
	actor, _ := actorFrom(r.Context())
	var req struct {
		Revision int32 `json:"revision"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	id := chi.URLParam(r, "id")
	if !hasScope(actor.Ticket.Scopes, "edit") && !hasScope(actor.Ticket.Scopes, "admin") {
		writeError(w, 403, "forbidden", "edit is required")
		return
	}
	assessment, changed, view, err := h.deps.Dreams.Assess(r.Context(), actor.Ticket.EnterpriseID, id)
	if err != nil {
		writeError(w, 404, "not_found", err.Error())
		return
	}
	if view.Revision != req.Revision {
		writeError(w, 409, "revision_conflict", "stale Dream policy revision")
		return
	}
	if !h.authorizeOrg(w, r, "edit", view.Policy.OrgUnitID) {
		return
	}
	client, ok := h.deps.Nexus.(nexus.ApprovalClient)
	if !ok {
		writeError(w, 503, "governance_unavailable", "AgentNexus approval resolver unavailable")
		return
	}
	orgVersion, err := h.deps.Dreams.OrgVersion(r.Context(), actor.Ticket.EnterpriseID)
	if err != nil {
		writeError(w, 503, "governance_unavailable", err.Error())
		return
	}
	key := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
	if len(key) < 16 || len(key) > 128 {
		writeError(w, 400, "bad_request", "bounded Idempotency-Key is required")
		return
	}
	now := time.Now().UTC()
	resolved, err := client.ResolveApprovalRoute(r.Context(), nexus.ApprovalResolveRequest{TicketID: actor.TicketID, EnterpriseID: actor.Ticket.EnterpriseID, ActorUserID: actor.Ticket.ActorUserID, IdempotencyKey: key, OrgVersion: orgVersion, OrgUnitID: view.Policy.OrgUnitID, ResourceType: "dream_policy", ResourceID: id, Action: "publish", ChangedFields: changed, ImpactedOrgUnitIDs: []string{view.Policy.OrgUnitID}, RequestedRisk: string(assessment.RiskLevel), FactsIssuedAt: now, FactsExpiresAt: now.Add(5 * time.Minute), FactsNonce: newID("facts")})
	if err != nil {
		writeError(w, 502, "governance_failed", err.Error())
		return
	}
	if resolved.RequesterUserID != actor.Ticket.ActorUserID || (assessment.RiskLevel == governance.RiskHigh && resolved.RiskLevel != "high") {
		writeError(w, http.StatusBadGateway, "governance_binding_mismatch", "AgentNexus approval route changed requester or downgraded deterministic risk")
		return
	}
	level := governance.RiskLevel(resolved.RiskLevel)
	if level != governance.RiskLow {
		level = governance.RiskHigh
	}
	route := governance.ReviewRoute{ChangeID: id, ResourceType: governance.ResourceDreamPolicy, ResourceID: id, RequesterUserID: actor.Ticket.ActorUserID, ReviewerUserID: resolved.ReviewerUserID, RiskLevel: level, Mode: governance.ReviewMode(resolved.Mode), State: governance.RoutePending, OrgPath: resolved.OrgPath, Queue: resolved.Queue}
	if route.RiskLevel == governance.RiskHigh && route.ReviewerUserID == actor.Ticket.ActorUserID {
		writeError(w, 403, "self_review_denied", "requester cannot review their own high-risk change")
		return
	}
	audit, ok := h.audit(w, r, nexus.AuditDreamPolicyCreated, id, map[string]any{"revision": req.Revision, "risk_level": route.RiskLevel, "reviewer_user_id": route.ReviewerUserID, "phase": "review"})
	if !ok {
		return
	}
	result, err := h.deps.Dreams.SubmitReview(r.Context(), actor.Ticket.EnterpriseID, id, actor.Ticket.ActorUserID, audit, req.Revision, route, resolved.RiskReasons)
	if err != nil {
		writeError(w, 409, "review_failed", err.Error())
		return
	}
	writeJSON(w, 200, result)
}

func (h *dreamPolicyHandler) check(w http.ResponseWriter, r *http.Request) {
	actor, _ := actorFrom(r.Context())
	var req struct {
		Revision int32 `json:"revision"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	if !hasScope(actor.Ticket.Scopes, "edit") && !hasScope(actor.Ticket.Scopes, "admin") {
		writeError(w, 403, "forbidden", "edit is required")
		return
	}
	assessment, changed, view, err := h.deps.Dreams.Assess(r.Context(), actor.Ticket.EnterpriseID, chi.URLParam(r, "id"))
	if err != nil {
		writeError(w, 404, "not_found", err.Error())
		return
	}
	if view.Revision != req.Revision {
		writeError(w, 409, "revision_conflict", "stale Dream policy revision")
		return
	}
	if !h.authorizeOrg(w, r, "edit", view.Policy.OrgUnitID) {
		return
	}
	writeJSON(w, 200, map[string]any{"revision": view.Revision, "risk_level": assessment.RiskLevel, "risk_reasons": assessment.RiskReasons, "changed_fields": changed})
}

func (h *dreamPolicyHandler) decide(w http.ResponseWriter, r *http.Request) {
	actor, _ := actorFrom(r.Context())
	var req struct {
		Revision int32  `json:"revision"`
		Decision string `json:"decision"`
		Comment  string `json:"comment"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	id := chi.URLParam(r, "id")
	view, err := h.deps.Dreams.GetLifecycle(r.Context(), actor.Ticket.EnterpriseID, id)
	if err != nil {
		writeError(w, 404, "not_found", err.Error())
		return
	}
	needed := "publish_low_risk"
	if view.RiskLevel == governance.RiskHigh {
		needed = "approve_high_risk"
	}
	if !hasScope(actor.Ticket.Scopes, needed) && !hasScope(actor.Ticket.Scopes, "admin") {
		writeError(w, 403, "forbidden", needed+" is required")
		return
	}
	if !h.authorizeOrg(w, r, needed, view.Policy.OrgUnitID) {
		return
	}
	audit, ok := h.audit(w, r, nexus.AuditDreamPolicyCreated, id, map[string]any{"revision": req.Revision, "decision": req.Decision, "comment": req.Comment, "phase": "decision"})
	if !ok {
		return
	}
	result, err := h.deps.Dreams.Decide(r.Context(), actor.Ticket.EnterpriseID, id, actor.Ticket.ActorUserID, audit, req.Decision, req.Revision)
	if err != nil {
		writeError(w, 409, "decision_failed", err.Error())
		return
	}
	writeJSON(w, 200, result)
}

func (h *dreamPolicyHandler) publish(w http.ResponseWriter, r *http.Request) {
	actor, _ := actorFrom(r.Context())
	var req struct {
		Revision int32 `json:"revision"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	id := chi.URLParam(r, "id")
	view, err := h.deps.Dreams.GetLifecycle(r.Context(), actor.Ticket.EnterpriseID, id)
	if err != nil {
		writeError(w, 404, "not_found", err.Error())
		return
	}
	if view.PermissionMode == governance.PermissionSuggestionOnly {
		writeError(w, 403, "suggestion_only", "employee suggestions cannot publish")
		return
	}
	if !hasScope(actor.Ticket.Scopes, "edit") && !hasScope(actor.Ticket.Scopes, "admin") {
		writeError(w, 403, "forbidden", "edit is required")
		return
	}
	if !h.authorizeOrg(w, r, "edit", view.Policy.OrgUnitID) {
		return
	}
	audit, ok := h.audit(w, r, nexus.AuditDreamPolicyCreated, id, map[string]any{"revision": req.Revision, "phase": "publish"})
	if !ok {
		return
	}
	result, err := h.deps.Dreams.PublishGoverned(r.Context(), actor.Ticket.EnterpriseID, id, actor.Ticket.ActorUserID, audit, req.Revision)
	if err != nil {
		writeError(w, 409, "publish_failed", err.Error())
		return
	}
	writeJSON(w, 200, result)
}

func (h *dreamPolicyHandler) disable(w http.ResponseWriter, r *http.Request) {
	actor, _ := actorFrom(r.Context())
	var req struct {
		Revision int32 `json:"revision"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	id := chi.URLParam(r, "id")
	view, err := h.deps.Dreams.GetLifecycle(r.Context(), actor.Ticket.EnterpriseID, id)
	if err != nil {
		writeError(w, 404, "not_found", err.Error())
		return
	}
	if !hasScope(actor.Ticket.Scopes, "edit") && !hasScope(actor.Ticket.Scopes, "admin") {
		writeError(w, 403, "forbidden", "edit is required")
		return
	}
	if !h.authorizeOrg(w, r, "edit", view.Policy.OrgUnitID) {
		return
	}
	audit, ok := h.audit(w, r, nexus.AuditDreamPolicyCreated, id, map[string]any{"revision": req.Revision, "phase": "disable"})
	if !ok {
		return
	}
	result, err := h.deps.Dreams.Disable(r.Context(), actor.Ticket.EnterpriseID, id, actor.Ticket.ActorUserID, audit, req.Revision)
	if err != nil {
		writeError(w, 409, "disable_failed", err.Error())
		return
	}
	writeJSON(w, 200, result)
}

func (h *dreamPolicyHandler) backfill(w http.ResponseWriter, r *http.Request) {
	actor, _ := actorFrom(r.Context())
	runner, ok := h.deps.DreamRerun.(dreamBackfiller)
	if !ok {
		writeError(w, 503, "backfill_unavailable", "Dream backfill service unavailable")
		return
	}
	var req struct {
		WindowStart  time.Time `json:"window_start"`
		WindowEnd    time.Time `json:"window_end"`
		RerunOfRunID string    `json:"rerun_of_run_id"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	id := chi.URLParam(r, "id")
	view, err := h.deps.Dreams.GetLifecycle(r.Context(), actor.Ticket.EnterpriseID, id)
	if err != nil {
		writeError(w, 404, "not_found", err.Error())
		return
	}
	if !hasScope(actor.Ticket.Scopes, "edit") && !hasScope(actor.Ticket.Scopes, "admin") {
		writeError(w, 403, "forbidden", "edit is required")
		return
	}
	if !h.authorizeOrg(w, r, "edit", view.Policy.OrgUnitID) {
		return
	}
	key := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
	if key == "" || len(key) > 256 {
		writeError(w, 400, "bad_request", "bounded Idempotency-Key is required")
		return
	}
	audit, ok := h.audit(w, r, nexus.AuditDreamJobRun, id, map[string]any{"window_start": req.WindowStart, "window_end": req.WindowEnd, "rerun_of_run_id": req.RerunOfRunID, "idempotency_key": key, "phase": "backfill"})
	if !ok {
		return
	}
	_ = audit
	runID, err := runner.Backfill(r.Context(), dream.BackfillRequest{EnterpriseID: actor.Ticket.EnterpriseID, PolicyID: id, WindowStart: req.WindowStart, WindowEnd: req.WindowEnd, RerunOfRunID: req.RerunOfRunID, IdempotencyKey: key})
	if err != nil {
		writeError(w, 409, "backfill_failed", err.Error())
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]string{"run_id": runID})
}

func (h *dreamPolicyHandler) authorizeOrg(w http.ResponseWriter, r *http.Request, action, org string) bool {
	return (&dreamRunHandler{nexus: h.deps.Nexus}).authorizeOrg(w, r, action, org)
}
func (h *dreamPolicyHandler) audit(w http.ResponseWriter, r *http.Request, action nexus.AuditAction, id string, details map[string]any) (string, bool) {
	actor, _ := actorFrom(r.Context())
	resp, err := h.deps.Nexus.AppendAuditEvidence(r.Context(), nexus.AppendAuditEvidenceRequest{TicketID: actor.TicketID, EnterpriseID: actor.Ticket.EnterpriseID, Action: action, ResourceType: "dream_policy", ResourceID: id, Details: details})
	if err != nil || strings.TrimSpace(resp.AuditRefID) == "" {
		if err == nil {
			err = fmt.Errorf("AgentNexus returned no durable audit reference")
		}
		writeError(w, 500, "audit_failed", err.Error())
		return "", false
	}
	return resp.AuditRefID, true
}
func decodeJSON(w http.ResponseWriter, r *http.Request, target any) bool {
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(target); err != nil {
		writeError(w, 400, "bad_request", err.Error())
		return false
	}
	return true
}
