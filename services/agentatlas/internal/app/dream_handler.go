package app

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	sdkdream "github.com/astraclawteam/agentatlas/sdk/go/dream"
	"github.com/astraclawteam/agentatlas/sdk/go/governance"
	"github.com/astraclawteam/agentatlas/sdk/go/nexus"
	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/dream"
	governanceinternal "github.com/astraclawteam/agentatlas/services/agentatlas/internal/governance"
	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/nexusclient"
	nexusruntime "github.com/astraclawteam/agentnexus/sdk/go/runtime"
	"github.com/go-chi/chi/v5"
)

func operationKey(r *http.Request) (string, error) {
	key := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
	if len(key) < 16 || len(key) > 128 {
		return "", fmt.Errorf("Idempotency-Key must contain 16..128 characters")
	}
	return key, nil
}
func operationHash(v any) string {
	raw, _ := json.Marshal(v)
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}
func policyIDForOperation(enterprise, key string) string {
	sum := sha256.Sum256([]byte(enterprise + "\x00" + key))
	return "pol_" + hex.EncodeToString(sum[:8])
}
func auditIdempotencyKey(enterprise, operationKey string) string {
	sum := sha256.Sum256([]byte(enterprise + "\x00audit\x00" + operationKey))
	return "audit_" + hex.EncodeToString(sum[:])
}
func (h *dreamPolicyHandler) beginOperation(w http.ResponseWriter, r *http.Request, kind, policyID string, payload any) (dream.Operation, bool) {
	actor, _ := actorFrom(r.Context())
	key, err := operationKey(r)
	if err != nil {
		writeError(w, 400, "bad_request", err.Error())
		return dream.Operation{}, false
	}
	op, err := h.deps.Dreams.BeginOperation(r.Context(), actor.Ticket.EnterpriseID, key, kind, policyID, actor.Ticket.ActorUserID, operationHash(payload))
	if err != nil {
		writeError(w, 409, "idempotency_conflict", err.Error())
		return dream.Operation{}, false
	}
	if op.Replay != nil {
		writeJSON(w, http.StatusOK, *op.Replay)
		return op, false
	}
	return op, true
}
func (h *dreamPolicyHandler) operationAudit(w http.ResponseWriter, r *http.Request, op dream.Operation, action nexus.AuditAction, id, phase string, details map[string]any) (string, bool) {
	if op.Row.AuditRefID.Valid {
		return op.Row.AuditRefID.String, true
	}
	details["phase"] = phase + "_attempt"
	details["operation_key"] = op.Row.OperationKey
	ref, ok := h.auditWithKey(w, r, action, id, auditIdempotencyKey(op.Row.EnterpriseID, op.Row.OperationKey), details)
	if !ok {
		return "", false
	}
	actor, _ := actorFrom(r.Context())
	recorded, err := h.deps.Dreams.RecordOperationAudit(r.Context(), actor.Ticket.EnterpriseID, op.Row.OperationKey, ref)
	if err != nil {
		writeError(w, 500, "operation_receipt_failed", err.Error())
		return "", false
	}
	return recorded.AuditRefID.String, true
}
func (h *dreamPolicyHandler) finishOperation(w http.ResponseWriter, r *http.Request, op dream.Operation, view dream.LifecycleView, status int) {
	actor, _ := actorFrom(r.Context())
	view, err := h.deps.Dreams.CompleteOperation(r.Context(), actor.Ticket.EnterpriseID, op.Row.OperationKey, view)
	if err != nil {
		writeError(w, 500, "operation_receipt_failed", err.Error())
		return
	}
	writeJSON(w, status, view)
}

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
	if !hasScope(actor.Ticket.Scopes, "edit") && !hasScope(actor.Ticket.Scopes, "service_mode") && !hasScope(actor.Ticket.Scopes, "admin") {
		writeError(w, 403, "forbidden", "edit or service_mode is required")
		return
	}
	policies, err := h.deps.Dreams.ListPublishedBounded(r.Context(), actor.Ticket.EnterpriseID, 101)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "list_failed", err.Error())
		return
	}
	if len(policies) > 100 {
		writeError(w, 409, "policy_list_unbounded", "Dream policy list exceeds bound")
		return
	}
	action := "edit"
	if !hasScope(actor.Ticket.Scopes, "edit") && !hasScope(actor.Ticket.Scopes, "admin") {
		action = "service_mode"
	}
	visible := make([]dream.PublishedPolicy, 0, len(policies))
	for _, policy := range policies {
		allowed, err := h.canOrg(r, action, policy.Policy.OrgUnitID)
		if err != nil {
			writeError(w, 502, "authorization_failed", err.Error())
			return
		}
		if allowed {
			visible = append(visible, policy)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"dream_policies": visible})
}

// canOrg asks the authorization surface whether the ticket holder may act on
// one organization unit.
//
// It used to synthesize a resource URI, call evidence LOCATE with it, and treat
// the echoed URI as permission. That put connector topology on the wire and
// inferred a grant from an echo; the decision belongs to
// /v1/authorization/decisions, which is what OrgAuthorization reaches.
func (h *dreamPolicyHandler) canOrg(r *http.Request, action, org string) (bool, error) {
	actor, _ := actorFrom(r.Context())
	if h.deps.OrgAuthorization == nil {
		return false, errors.New("AgentNexus organization authorization unavailable")
	}
	decision, err := h.deps.OrgAuthorization.AuthorizeTicketOperation(r.Context(), actor.TicketID, nexus.BrowserAuthorizationRequest{
		OrgUnitID:    org,
		OrgVersion:   actor.Ticket.OrgVersion,
		ResourceType: "dream_policy",
		ResourceID:   org,
		Action:       action,
	})
	if errors.Is(err, nexusclient.ErrDenied) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	// Only an explicit allow is permission.
	return decision.Decision == "allow", nil
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
	key, err := operationKey(r)
	if err != nil {
		writeError(w, 400, "bad_request", err.Error())
		return
	}
	policyID := policyIDForOperation(actor.Ticket.EnterpriseID, key)
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
	op, proceed := h.beginOperation(w, r, "create", policyID, map[string]any{"policy": req, "permission_mode": mode})
	if !proceed {
		return
	}
	audit, ok := h.operationAudit(w, r, op, nexus.AuditDreamPolicyCreateRequested, policyID, "create", map[string]any{"dream_policy_id": policyID, "org_unit_id": req.OrgUnitID, "visibility_level": req.VisibilityLevel})
	if !ok {
		return
	}
	_ = audit
	view, err := h.deps.Dreams.CreateGovernedDraft(r.Context(), actor.Ticket.EnterpriseID, policyID, actor.Ticket.ActorUserID, mode, op.Row.OperationKey, policy)
	if err != nil {
		writeError(w, http.StatusUnprocessableEntity, "invalid_policy", err.Error())
		return
	}
	h.finishOperation(w, r, op, view, http.StatusCreated)
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
	op, proceed := h.beginOperation(w, r, "update", id, req)
	if !proceed {
		return
	}
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
	audit, ok := h.operationAudit(w, r, op, nexus.AuditDreamPolicyCreated, id, "update", map[string]any{"revision": req.Revision})
	if !ok {
		return
	}
	view, err := h.deps.Dreams.UpdateGovernedDraft(r.Context(), actor.Ticket.EnterpriseID, id, actor.Ticket.ActorUserID, audit, op.Row.OperationKey, req.Revision, dream.Policy(req.Policy))
	if err != nil {
		writeError(w, 409, "revision_conflict", err.Error())
		return
	}
	h.finishOperation(w, r, op, view, 200)
}

func (h *dreamPolicyHandler) review(w http.ResponseWriter, r *http.Request) {
	actor, _ := actorFrom(r.Context())
	var req struct {
		Revision int32  `json:"revision"`
		Action   string `json:"action"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	id := chi.URLParam(r, "id")
	if req.Action == "" {
		req.Action = "publish"
	}
	if req.Action != "publish" && req.Action != "disable" {
		writeError(w, 400, "bad_request", "action must be publish or disable")
		return
	}
	op, proceed := h.beginOperation(w, r, "review", id, req)
	if !proceed {
		return
	}
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
	if actor.Ticket.ActorUserID != view.RequesterUserID {
		writeError(w, http.StatusForbidden, "requester_required", "only the stored Dream policy requester may submit or refresh review")
		return
	}
	refresh := view.ReviewState == "pending" && view.ReviewMode == governance.ReviewAdminQueue && view.PendingAction == req.Action
	if !refresh && ((req.Action == "publish" && view.Status != "draft") || (req.Action == "disable" && view.Status != "published")) {
		writeError(w, 409, "invalid_state", "action is not valid for current policy state")
		return
	}
	if req.Action == "disable" {
		assessment = governance.RiskAssessment{RiskLevel: governance.RiskHigh, RiskReasons: []string{"high_risk_field:status"}}
		changed = []string{"status"}
	}
	if !h.authorizeOrg(w, r, "edit", view.Policy.OrgUnitID) {
		return
	}
	if h.deps.ApprovalTransmitter == nil {
		writeError(w, 503, "governance_unavailable", "AgentNexus approval transmission unavailable")
		return
	}
	// Only a change that actually needs approval goes to the authority. A
	// low-risk edit keeps its local single-confirmation fast path: sending
	// every small change to the customer's OA queue would be both unnecessary
	// and a good way to get the queue ignored.
	needsAuthority := assessment.RiskLevel != governance.RiskLow
	planHash := "sha256:" + operationHash(map[string]any{"change": id, "revision": req.Revision, "action": req.Action, "fields": changed})
	planRef := governanceinternal.ApprovalPlanRefFor(actor.Ticket.EnterpriseID, id, uint64(req.Revision))
	// The frozen ApprovalRequest requires a wc_* WorkCase handle as its business
	// context. A governed change does not have one until C1 builds the WorkCase
	// orchestrator, so transmission is DORMANT rather than broken: a high-risk
	// change still routes to the local administrator queue exactly as it did
	// before the transmission surface existed. Transmitting a policy id instead
	// would be rejected client-side by Validate() on every single call.
	workCaseContextFor := h.deps.WorkCaseContextFor
	if workCaseContextFor == nil {
		workCaseContextFor = governanceinternal.WorkCaseContextFor
	}
	businessContextRef, transmissible := workCaseContextFor(id)
	// reviewer stays empty until the authority has actually decided.
	reviewer := ""
	if !needsAuthority || !transmissible {
		// Nothing to transmit and nothing to wait for.
	} else if refresh {
		// Refresh READS the authority's decision; it does not re-submit. The
		// plan is already delivered, and asking again would duplicate it.
		status, err := h.deps.ApprovalTransmitter.GetApprovalTransmission(r.Context(), planRef)
		if err != nil {
			writeError(w, 502, "governance_failed", err.Error())
			return
		}
		if !status.Decided() {
			// Still with the authority. A pending decision is a state, not a
			// failure: the route stays queued and the caller retries later.
			// Note "delivered" is NOT decided - it only means the plan arrived.
			writeError(w, 409, "approval_pending", "the approval authority has not decided yet")
			return
		}
		if status.Decision != nexusclient.ApprovalApproved {
			// A denial is an answer, not an error, but it is not an approval:
			// nothing may proceed on it.
			writeError(w, 403, "approval_denied", "the approval authority did not approve this change")
			return
		}
		// The approver identity is deliberately absent from this contract, so
		// this records the AUTHORITY under which the decision was made - not a
		// person. Who clicked approve inside the customer's OA/BPM is that
		// system's record, not a field AgentAtlas may invent.
		reviewer = status.Authority
	} else if _, err := h.deps.ApprovalTransmitter.TransmitApprovalPlan(r.Context(), nexusruntime.ApprovalRequest{
		RequestID:          "aprq-" + op.Row.OperationKey,
		BusinessContextRef: businessContextRef,
		Capability:         "dream_policy." + req.Action,
		ParameterHash:      planHash,
		Purpose:            "governed_change_approval",
		Plan: nexusruntime.ApprovalPlanRef{
			PlanRef: planRef, PlanHash: planHash, Authority: h.deps.ApprovalAuthority,
		},
		ExpiresAt: time.Now().Add(24 * time.Hour).UTC(),
	}); err != nil {
		writeError(w, 502, "governance_failed", err.Error())
		return
	}
	// The risk verdict is local now; there is no returned route to cross-check
	// it against, and none to take a reviewer from.
	level := assessment.RiskLevel
	if level != governance.RiskLow {
		level = governance.RiskHigh
	}
	// Queued with the authority until it decides; Upward once it has. The
	// self-review guard in ReviewRoute.Validate fires as soon as a reviewer is
	// named, which is exactly when a decision has landed.
	mode, queue, orgPath := governance.ReviewAdminQueue, h.deps.ApprovalAuthority, []string{}
	switch {
	case !needsAuthority:
		mode, queue = governance.ReviewSingleConfirmation, ""
	case reviewer != "":
		mode, queue, orgPath = governance.ReviewUpward, "", []string{h.deps.ApprovalAuthority}
	}
	route := governance.ReviewRoute{ChangeID: id, ResourceType: governance.ResourceDreamPolicy, ResourceID: id, RequesterUserID: view.RequesterUserID, ReviewerUserID: reviewer, RiskLevel: level, Mode: mode, Queue: queue, State: governance.RoutePending, OrgPath: orgPath}
	audit, ok := h.operationAudit(w, r, op, nexus.AuditDreamPolicyCreated, id, "review:"+req.Action, map[string]any{"revision": req.Revision, "risk_level": route.RiskLevel, "reviewer_user_id": route.ReviewerUserID})
	if !ok {
		return
	}
	result, err := h.deps.Dreams.SubmitReview(r.Context(), actor.Ticket.EnterpriseID, id, actor.Ticket.ActorUserID, audit, op.Row.OperationKey, req.Action, req.Revision, route, assessment.RiskReasons)
	if err != nil {
		writeError(w, 409, "review_failed", err.Error())
		return
	}
	if reviewer != "" {
		// The authority already approved, in its own system. Applying that here
		// rather than waiting for someone to click approve a second time keeps
		// the decision in ONE place: a second click would be duplicate work, and
		// a decision that exists in two systems can disagree with itself.
		decided, err := h.deps.Dreams.Decide(r.Context(), actor.Ticket.EnterpriseID, id, reviewer, audit, op.Row.OperationKey, "approve", req.Revision)
		if err != nil {
			writeError(w, 409, "decision_failed", err.Error())
			return
		}
		result = decided
	}
	h.finishOperation(w, r, op, result, 200)
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
	op, proceed := h.beginOperation(w, r, "decision", id, req)
	if !proceed {
		return
	}
	view, err := h.deps.Dreams.GetLifecycle(r.Context(), actor.Ticket.EnterpriseID, id)
	if err != nil {
		writeError(w, 404, "not_found", err.Error())
		return
	}
	if view.Revision != req.Revision || view.ReviewState != "pending" {
		writeError(w, 409, "decision_failed", "policy is not pending this revision")
		return
	}
	if view.ReviewMode == governance.ReviewAdminQueue {
		writeError(w, 409, "reviewer_unassigned", "refresh the AgentNexus review route before deciding")
		return
	}
	if view.ReviewMode == governance.ReviewUpward && (view.ReviewerUserID != actor.Ticket.ActorUserID || view.RequesterUserID == actor.Ticket.ActorUserID) {
		writeError(w, 403, "wrong_reviewer", "actor is not the AgentNexus-assigned reviewer")
		return
	}
	if view.ReviewMode == governance.ReviewSingleConfirmation && view.RequesterUserID != actor.Ticket.ActorUserID {
		writeError(w, 403, "wrong_confirmer", "requester confirmation is required")
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
	audit, ok := h.operationAudit(w, r, op, nexus.AuditDreamPolicyCreated, id, "decision", map[string]any{"revision": req.Revision, "decision": req.Decision, "comment": req.Comment})
	if !ok {
		return
	}
	result, err := h.deps.Dreams.Decide(r.Context(), actor.Ticket.EnterpriseID, id, actor.Ticket.ActorUserID, audit, op.Row.OperationKey, req.Decision, req.Revision)
	if err != nil {
		writeError(w, 409, "decision_failed", err.Error())
		return
	}
	h.finishOperation(w, r, op, result, 200)
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
	op, proceed := h.beginOperation(w, r, "publish", id, req)
	if !proceed {
		return
	}
	view, err := h.deps.Dreams.GetLifecycle(r.Context(), actor.Ticket.EnterpriseID, id)
	if err != nil {
		writeError(w, 404, "not_found", err.Error())
		return
	}
	if view.PermissionMode == governance.PermissionSuggestionOnly {
		writeError(w, 403, "suggestion_only", "employee suggestions cannot publish")
		return
	}
	if view.Revision != req.Revision || view.ReviewState != "approved" || view.PendingAction != "publish" {
		writeError(w, 409, "publish_failed", "approved publish decision is required")
		return
	}
	if !hasScope(actor.Ticket.Scopes, "edit") && !hasScope(actor.Ticket.Scopes, "admin") {
		writeError(w, 403, "forbidden", "edit is required")
		return
	}
	if !h.authorizeOrg(w, r, "edit", view.Policy.OrgUnitID) {
		return
	}
	audit, ok := h.operationAudit(w, r, op, nexus.AuditDreamPolicyCreated, id, "publish", map[string]any{"revision": req.Revision})
	if !ok {
		return
	}
	result, err := h.deps.Dreams.PublishGoverned(r.Context(), actor.Ticket.EnterpriseID, id, actor.Ticket.ActorUserID, audit, op.Row.OperationKey, req.Revision)
	if err != nil {
		writeError(w, 409, "publish_failed", err.Error())
		return
	}
	h.finishOperation(w, r, op, result, 200)
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
	op, proceed := h.beginOperation(w, r, "disable", id, req)
	if !proceed {
		return
	}
	view, err := h.deps.Dreams.GetLifecycle(r.Context(), actor.Ticket.EnterpriseID, id)
	if err != nil {
		writeError(w, 404, "not_found", err.Error())
		return
	}
	if view.Revision != req.Revision || view.ReviewState != "approved" || view.PendingAction != "disable" {
		writeError(w, 409, "disable_failed", "approved disable decision is required")
		return
	}
	if !hasScope(actor.Ticket.Scopes, "edit") && !hasScope(actor.Ticket.Scopes, "admin") {
		writeError(w, 403, "forbidden", "edit is required")
		return
	}
	if !h.authorizeOrg(w, r, "edit", view.Policy.OrgUnitID) {
		return
	}
	audit, ok := h.operationAudit(w, r, op, nexus.AuditDreamPolicyCreated, id, "disable", map[string]any{"revision": req.Revision})
	if !ok {
		return
	}
	result, err := h.deps.Dreams.Disable(r.Context(), actor.Ticket.EnterpriseID, id, actor.Ticket.ActorUserID, audit, op.Row.OperationKey, req.Revision)
	if err != nil {
		writeError(w, 409, "disable_failed", err.Error())
		return
	}
	h.finishOperation(w, r, op, result, 200)
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
	key, err := operationKey(r)
	if err != nil {
		writeError(w, 400, "bad_request", err.Error())
		return
	}
	backfill := dream.BackfillRequest{
		EnterpriseID:   actor.Ticket.EnterpriseID,
		PolicyID:       id,
		WindowStart:    req.WindowStart,
		WindowEnd:      req.WindowEnd,
		RerunOfRunID:   req.RerunOfRunID,
		IdempotencyKey: key,
	}
	op, err := h.deps.Dreams.BeginReceipt(r.Context(), actor.Ticket.EnterpriseID, key, "backfill", id, actor.Ticket.ActorUserID, operationHash(map[string]any{"policy_id": id, "window_start": req.WindowStart, "window_end": req.WindowEnd, "rerun_of_run_id": req.RerunOfRunID}))
	if err != nil {
		writeError(w, 409, "backfill_failed", err.Error())
		return
	}
	if len(op.Replay) > 0 {
		var result map[string]string
		if err := json.Unmarshal(op.Replay, &result); err != nil {
			writeError(w, 500, "backfill_failed", err.Error())
			return
		}
		writeJSON(w, http.StatusAccepted, result)
		return
	}
	if runID, found, err := runner.LookupBackfill(r.Context(), backfill); err != nil {
		writeError(w, 409, "backfill_failed", err.Error())
		return
	} else if found {
		result := map[string]string{"run_id": runID}
		if _, err := h.deps.Dreams.CompleteReceipt(r.Context(), actor.Ticket.EnterpriseID, key, result); err != nil {
			writeError(w, 500, "operation_receipt_failed", err.Error())
			return
		}
		writeJSON(w, http.StatusAccepted, result)
		return
	}
	audit, ok := h.auditWithKey(w, r, nexus.AuditDreamJobRun, id, auditIdempotencyKey(actor.Ticket.EnterpriseID, key), map[string]any{"window_start": req.WindowStart, "window_end": req.WindowEnd, "rerun_of_run_id": req.RerunOfRunID, "idempotency_key": key, "phase": "backfill_attempt"})
	if !ok {
		return
	}
	if _, err := h.deps.Dreams.RecordOperationAudit(r.Context(), actor.Ticket.EnterpriseID, key, audit); err != nil {
		writeError(w, 500, "operation_receipt_failed", err.Error())
		return
	}
	backfill.AuditRefID = audit
	runID, err := runner.Backfill(r.Context(), backfill)
	if err != nil {
		writeError(w, 409, "backfill_failed", err.Error())
		return
	}
	result := map[string]string{"run_id": runID}
	if _, err := h.deps.Dreams.CompleteReceipt(r.Context(), actor.Ticket.EnterpriseID, key, result); err != nil {
		writeError(w, 500, "operation_receipt_failed", err.Error())
		return
	}
	writeJSON(w, http.StatusAccepted, result)
}

func (h *dreamPolicyHandler) authorizeOrg(w http.ResponseWriter, r *http.Request, action, org string) bool {
	return (&dreamRunHandler{nexus: h.deps.Nexus, orgAuthorization: h.deps.OrgAuthorization}).authorizeOrg(w, r, action, org)
}
func (h *dreamPolicyHandler) audit(w http.ResponseWriter, r *http.Request, action nexus.AuditAction, id string, details map[string]any) (string, bool) {
	return h.auditWithKey(w, r, action, id, "", details)
}
func (h *dreamPolicyHandler) auditWithKey(w http.ResponseWriter, r *http.Request, action nexus.AuditAction, id, idempotencyKey string, details map[string]any) (string, bool) {
	actor, _ := actorFrom(r.Context())
	resp, err := h.deps.Nexus.AppendAuditEvidence(r.Context(), nexus.AppendAuditEvidenceRequest{IdempotencyKey: idempotencyKey, TicketID: actor.TicketID, EnterpriseID: actor.Ticket.EnterpriseID, Action: action, ResourceType: "dream_policy", ResourceID: id, Details: details})
	if errors.Is(err, nexusclient.ErrConflict) {
		writeError(w, http.StatusConflict, "idempotency_conflict", err.Error())
		return "", false
	}
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
