package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	sdkdream "github.com/astraclawteam/agentatlas/sdk/go/dream"
	sdkgovernance "github.com/astraclawteam/agentatlas/sdk/go/governance"
	"github.com/astraclawteam/agentatlas/sdk/go/nexus"
	db "github.com/astraclawteam/agentatlas/services/agentatlas/db/generated"
	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/browsersession"
	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/dream"
	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/nexusclient"
	"github.com/go-chi/chi/v5"
)

type browserDreamEvidenceClient interface {
	LocateEvidenceWithBearer(context.Context, string, nexus.LocateEvidenceRequest) (nexus.LocateEvidenceResponse, error)
	ReadEvidenceWithBearer(context.Context, string, nexus.ReadEvidenceRequest) (nexus.ReadEvidenceResponse, error)
	AppendAuditEvidenceWithBearer(context.Context, string, nexus.AppendAuditEvidenceRequest) (nexus.AppendAuditEvidenceResponse, error)
}

type browserDreamHandler struct {
	store      dreamRunStore
	authorizer nexus.BrowserBFFClient
	evidence   browserDreamEvidenceClient
	rerun      dreamRerunner
	backfill   dreamBackfiller
	operations *dream.PolicyService
}

func (h *browserDreamHandler) list(w http.ResponseWriter, r *http.Request) {
	session, org, ok := h.authorizeQuery(w, r, "dream:read", "dream.read")
	if !ok {
		return
	}
	if h.store == nil {
		writeError(w, http.StatusServiceUnavailable, "dream_unavailable", "Dream workspace is unavailable")
		return
	}
	runs, err := h.store.ListDreamRunsByOrg(r.Context(), db.ListDreamRunsByOrgParams{EnterpriseID: session.EnterpriseID, OrgUnitID: org, ResultLimit: maxDreamRunList + 1})
	if err != nil || len(runs) > maxDreamRunList {
		writeError(w, http.StatusServiceUnavailable, "dream_unavailable", "Dream workspace is unavailable")
		return
	}
	views := make([]sdkdream.DreamRunView, 0, len(runs))
	window := strings.TrimSpace(r.URL.Query().Get("window"))
	for _, run := range runs {
		row, err := h.store.GetDreamRunView(r.Context(), db.GetDreamRunViewParams{EnterpriseID: session.EnterpriseID, RunID: run.ID})
		if err != nil {
			writeError(w, http.StatusServiceUnavailable, "dream_unavailable", "Dream workspace is unavailable")
			return
		}
		view, err := dreamRunView(row)
		if err != nil {
			writeError(w, http.StatusServiceUnavailable, "dream_unavailable", "Dream workspace is unavailable")
			return
		}
		if window == "" || view.WindowEnd.Format("2006-01-02") == window {
			views = append(views, view)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"runs": views})
}

func (h *browserDreamHandler) detail(w http.ResponseWriter, r *http.Request) {
	_, view, ok := h.loadAuthorized(w, r, "dream:read", "dream.read")
	if ok {
		writeJSON(w, http.StatusOK, view)
	}
}

func (h *browserDreamHandler) annotate(w http.ResponseWriter, r *http.Request) {
	session, view, ok := h.loadAuthorized(w, r, "dream:annotate", "dream.annotate")
	if !ok {
		return
	}
	var input struct {
		Action  string `json:"action"`
		Comment string `json:"comment"`
	}
	if !decodeJSON(w, r, &input) {
		return
	}
	if !validAnnotation(input.Action) || len([]rune(input.Comment)) > 4000 || (input.Action == "comment" && strings.TrimSpace(input.Comment) == "") {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid bounded annotation")
		return
	}
	key, err := operationKey(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	row, err := h.store.CreateDreamAnnotation(r.Context(), db.CreateDreamAnnotationParams{ID: browserDreamID("dann", session.EnterpriseID, view.RunID, key), EnterpriseID: session.EnterpriseID, RunID: view.RunID, AnnotationType: input.Action, Body: input.Comment, CreatedBy: session.UserID})
	if err != nil {
		writeError(w, http.StatusConflict, "annotation_failed", "The annotation was already recorded or could not be appended")
		return
	}
	writeJSON(w, http.StatusCreated, row)
}

func (h *browserDreamHandler) evidenceAccess(w http.ResponseWriter, r *http.Request) {
	session, view, ok := h.loadAuthorized(w, r, "dream:evidence:read", "dream.evidence.read")
	if !ok {
		return
	}
	if view.EvidencePointerID == "" {
		writeError(w, http.StatusNotFound, "dream_evidence_not_found", "Dream evidence was not found")
		return
	}
	if h.evidence == nil {
		writeError(w, http.StatusServiceUnavailable, "evidence_unavailable", "Evidence authorization is unavailable")
		return
	}
	loc, err := h.evidence.LocateEvidenceWithBearer(r.Context(), session.UpstreamAccessToken, nexus.LocateEvidenceRequest{EnterpriseID: session.EnterpriseID, EvidencePointerID: view.EvidencePointerID, QueryIntent: "dream evidence drill-down"})
	if err != nil {
		h.evidenceError(w, err)
		return
	}
	read, err := h.evidence.ReadEvidenceWithBearer(r.Context(), session.UpstreamAccessToken, nexus.ReadEvidenceRequest{EnterpriseID: session.EnterpriseID, ResourceURI: loc.ResourceURI, EvidencePointerID: view.EvidencePointerID, MaxBytes: 100000})
	if err != nil {
		h.evidenceError(w, err)
		return
	}
	if strings.TrimSpace(read.GrantID) == "" {
		writeError(w, http.StatusForbidden, "grant_required", "AgentNexus returned no bound Step Grant")
		return
	}
	audit, err := h.evidence.AppendAuditEvidenceWithBearer(r.Context(), session.UpstreamAccessToken, nexus.AppendAuditEvidenceRequest{IdempotencyKey: browserDreamID("audit", session.EnterpriseID, view.RunID, read.GrantID), EnterpriseID: session.EnterpriseID, OrgVersion: session.OrgVersion, OrgUnitID: view.OrgUnitID, AuthorizedAction: "dream.evidence.read", Action: nexus.AuditEvidenceRead, ResourceType: "dream_run", ResourceID: view.RunID, Details: map[string]any{"evidence_pointer_id": view.EvidencePointerID, "grant_id": read.GrantID, "actor_user_id": session.UserID}})
	if err != nil || strings.TrimSpace(audit.AuditRefID) == "" {
		writeError(w, http.StatusServiceUnavailable, "audit_failed", "Evidence access could not be audited")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"content_type": read.ContentType, "sanitized_detail": read.SanitizedExcerpt, "content_hash": read.ContentHash})
}

func (h *browserDreamHandler) rerunRun(w http.ResponseWriter, r *http.Request) {
	session, view, ok := h.loadAuthorized(w, r, "dream:rerun", "dream.rerun")
	if !ok {
		return
	}
	if h.rerun == nil || h.operations == nil || h.evidence == nil {
		writeError(w, http.StatusServiceUnavailable, "rerun_unavailable", "Dream rerun is unavailable")
		return
	}
	key, err := operationKey(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	op, err := h.operations.BeginReceipt(r.Context(), session.EnterpriseID, key, "rerun", view.RunID, session.UserID, operationHash(map[string]any{"source_run_id": view.RunID}))
	if err != nil {
		writeError(w, http.StatusConflict, "rerun_failed", err.Error())
		return
	}
	if len(op.Replay) > 0 {
		var result map[string]string
		if json.Unmarshal(op.Replay, &result) != nil {
			writeError(w, http.StatusServiceUnavailable, "rerun_failed", "Dream rerun receipt is unavailable")
			return
		}
		writeJSON(w, http.StatusAccepted, result)
		return
	}
	if id, found, lookupErr := h.rerun.LookupRerun(r.Context(), session.EnterpriseID, view.RunID, key); lookupErr != nil {
		writeError(w, http.StatusConflict, "rerun_failed", lookupErr.Error())
		return
	} else if found {
		result := map[string]string{"run_id": id}
		if _, err := h.operations.CompleteReceipt(r.Context(), session.EnterpriseID, key, result); err != nil {
			writeError(w, http.StatusServiceUnavailable, "rerun_failed", err.Error())
			return
		}
		writeJSON(w, http.StatusAccepted, result)
		return
	}
	audit, err := h.evidence.AppendAuditEvidenceWithBearer(r.Context(), session.UpstreamAccessToken, nexus.AppendAuditEvidenceRequest{IdempotencyKey: auditIdempotencyKey(session.EnterpriseID, key), EnterpriseID: session.EnterpriseID, OrgVersion: session.OrgVersion, OrgUnitID: view.OrgUnitID, AuthorizedAction: "dream.rerun", Action: nexus.AuditDreamJobRun, ResourceType: "dream_run", ResourceID: view.RunID, Details: map[string]any{"org_unit_id": view.OrgUnitID, "idempotency_key": key, "phase": "manual_rerun_attempt"}})
	if err != nil || strings.TrimSpace(audit.AuditRefID) == "" {
		writeError(w, http.StatusServiceUnavailable, "audit_failed", "Dream rerun could not be audited")
		return
	}
	if _, err := h.operations.RecordOperationAudit(r.Context(), session.EnterpriseID, key, audit.AuditRefID); err != nil {
		writeError(w, http.StatusServiceUnavailable, "rerun_failed", err.Error())
		return
	}
	id, err := h.rerun.Rerun(r.Context(), session.EnterpriseID, view.RunID, key, audit.AuditRefID)
	if err != nil {
		writeError(w, http.StatusConflict, "rerun_failed", err.Error())
		return
	}
	result := map[string]string{"run_id": id}
	if _, err := h.operations.CompleteReceipt(r.Context(), session.EnterpriseID, key, result); err != nil {
		writeError(w, http.StatusServiceUnavailable, "rerun_failed", err.Error())
		return
	}
	writeJSON(w, http.StatusAccepted, result)
}

func (h *browserDreamHandler) listPolicies(w http.ResponseWriter, r *http.Request) {
	session, org, ok := h.authorizeQuery(w, r, "dream:read", "dream.policy.read")
	if !ok {
		return
	}
	if h.operations == nil {
		writeError(w, http.StatusServiceUnavailable, "dream_policy_unavailable", "Dream policies are unavailable")
		return
	}
	items, err := h.operations.ListLifecycleByOrgBounded(r.Context(), session.EnterpriseID, org, 1001)
	if err != nil || len(items) > 1000 {
		writeError(w, http.StatusServiceUnavailable, "dream_policy_unavailable", "Dream policies are unavailable")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"dream_policies": items})
}

func (h *browserDreamHandler) createPolicy(w http.ResponseWriter, r *http.Request) {
	session, ok := browserActorFrom(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "unauthorized", "no active browser session")
		return
	}
	if h.operations == nil || h.evidence == nil {
		writeError(w, http.StatusServiceUnavailable, "dream_policy_unavailable", "Dream policies are unavailable")
		return
	}
	var input sdkdream.DreamPolicyDefinition
	if !decodeJSON(w, r, &input) {
		return
	}
	policy := dream.Policy(input)
	if err := policy.Validate(); err != nil {
		writeError(w, http.StatusUnprocessableEntity, "invalid_policy", err.Error())
		return
	}
	mode, permission, action := sdkgovernance.PermissionSuggestionOnly, "suggest", "dream.policy.suggest"
	if browserDreamPermission(session.Permissions, "edit") {
		mode, permission, action = sdkgovernance.PermissionDirectEdit, "edit", "dream.policy.edit"
	}
	if !browserDreamPermission(session.Permissions, permission) || !containsExactOrganization(session.OrgUnitIDs, input.OrgUnitID) {
		writeError(w, http.StatusForbidden, "forbidden", "Dream policy is not authorized")
		return
	}
	if !h.authorize(w, r, session, input.OrgUnitID, "dream_policy", input.OrgUnitID, action) {
		return
	}
	key, err := operationKey(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	policyID := policyIDForOperation(session.EnterpriseID, key)
	op, err := h.operations.BeginOperation(r.Context(), session.EnterpriseID, key, "create", policyID, session.UserID, operationHash(map[string]any{"policy": input, "permission_mode": mode}))
	if err != nil {
		writeError(w, http.StatusConflict, "policy_conflict", err.Error())
		return
	}
	if op.Replay != nil {
		writeJSON(w, http.StatusCreated, op.Replay)
		return
	}
	audit, err := h.evidence.AppendAuditEvidenceWithBearer(r.Context(), session.UpstreamAccessToken, nexus.AppendAuditEvidenceRequest{IdempotencyKey: auditIdempotencyKey(session.EnterpriseID, key), EnterpriseID: session.EnterpriseID, OrgVersion: session.OrgVersion, OrgUnitID: input.OrgUnitID, AuthorizedAction: action, Action: nexus.AuditDreamPolicyCreateRequested, ResourceType: "dream_policy", ResourceID: policyID, Details: map[string]any{"phase": "create", "org_unit_id": input.OrgUnitID}})
	if err != nil || strings.TrimSpace(audit.AuditRefID) == "" {
		writeError(w, http.StatusServiceUnavailable, "audit_failed", "Dream policy creation could not be audited")
		return
	}
	if _, err := h.operations.RecordOperationAudit(r.Context(), session.EnterpriseID, key, audit.AuditRefID); err != nil {
		writeError(w, http.StatusServiceUnavailable, "policy_failed", err.Error())
		return
	}
	view, err := h.operations.CreateGovernedDraft(r.Context(), session.EnterpriseID, policyID, session.UserID, mode, key, policy)
	if err != nil {
		writeError(w, http.StatusUnprocessableEntity, "invalid_policy", err.Error())
		return
	}
	view, err = h.operations.CompleteOperation(r.Context(), session.EnterpriseID, key, view)
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "policy_failed", err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, view)
}

func (h *browserDreamHandler) updatePolicy(w http.ResponseWriter, r *http.Request) {
	session, current, ok := h.authorizePolicy(w, r, "edit", "dream.policy.edit")
	if !ok {
		return
	}
	if current.PermissionMode == sdkgovernance.PermissionSuggestionOnly {
		writeError(w, http.StatusForbidden, "suggestion_only", "Suggestions cannot be directly modified")
		return
	}
	var input struct {
		Revision int32                          `json:"revision"`
		Policy   sdkdream.DreamPolicyDefinition `json:"policy"`
	}
	if !decodeJSON(w, r, &input) {
		return
	}
	if !containsExactOrganization(session.OrgUnitIDs, input.Policy.OrgUnitID) || !h.authorize(w, r, session, input.Policy.OrgUnitID, "dream_policy", current.ID, "dream.policy.edit") {
		return
	}
	op, key, ok := h.beginPolicyOperation(w, r, session, "update", current.ID, input)
	if !ok {
		return
	}
	if op.Replay != nil {
		writeJSON(w, http.StatusOK, op.Replay)
		return
	}
	audit, ok := h.auditPolicy(w, r, session, op, current, "update", map[string]any{"revision": input.Revision})
	if !ok {
		return
	}
	view, err := h.operations.UpdateGovernedDraft(r.Context(), session.EnterpriseID, current.ID, session.UserID, audit, key, input.Revision, dream.Policy(input.Policy))
	if err != nil {
		writeError(w, http.StatusConflict, "revision_conflict", err.Error())
		return
	}
	h.finishPolicyOperation(w, r, session, key, view, http.StatusOK)
}

func (h *browserDreamHandler) checkPolicy(w http.ResponseWriter, r *http.Request) {
	session, current, ok := h.authorizePolicy(w, r, "edit", "dream.policy.edit")
	if !ok {
		return
	}
	var input struct {
		Revision int32 `json:"revision"`
	}
	if !decodeJSON(w, r, &input) {
		return
	}
	assessment, changed, view, err := h.operations.Assess(r.Context(), session.EnterpriseID, current.ID)
	if err != nil {
		writeError(w, http.StatusNotFound, "not_found", "Dream policy was not found")
		return
	}
	if view.Revision != input.Revision {
		writeError(w, http.StatusConflict, "revision_conflict", "stale Dream policy revision")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"revision": view.Revision, "risk_level": assessment.RiskLevel, "risk_reasons": assessment.RiskReasons, "changed_fields": changed})
}

func (h *browserDreamHandler) reviewPolicy(w http.ResponseWriter, r *http.Request) {
	session, current, ok := h.authorizePolicy(w, r, "edit", "dream.policy.edit")
	if !ok {
		return
	}
	var input struct {
		Revision int32  `json:"revision"`
		Action   string `json:"action"`
	}
	if !decodeJSON(w, r, &input) {
		return
	}
	if input.Action == "" {
		input.Action = "publish"
	}
	if input.Action != "publish" && input.Action != "disable" {
		writeError(w, http.StatusBadRequest, "bad_request", "action must be publish or disable")
		return
	}
	if current.Revision != input.Revision || current.RequesterUserID != session.UserID {
		writeError(w, http.StatusForbidden, "requester_required", "Only the policy requester may submit review")
		return
	}
	refresh := current.ReviewState == "pending" && current.ReviewMode == sdkgovernance.ReviewAdminQueue && current.PendingAction == input.Action
	if !refresh && ((input.Action == "publish" && current.Status != "draft") || (input.Action == "disable" && current.Status != "published")) {
		writeError(w, http.StatusConflict, "invalid_state", "Action is not valid for the current policy state")
		return
	}
	assessment, changed, _, err := h.operations.Assess(r.Context(), session.EnterpriseID, current.ID)
	if err != nil {
		writeError(w, http.StatusNotFound, "not_found", "Dream policy was not found")
		return
	}
	if input.Action == "disable" {
		assessment = sdkgovernance.RiskAssessment{RiskLevel: sdkgovernance.RiskHigh, RiskReasons: []string{"high_risk_field:status"}}
		changed = []string{"status"}
	}
	op, key, ok := h.beginPolicyOperation(w, r, session, "review", current.ID, input)
	if !ok {
		return
	}
	if op.Replay != nil {
		writeJSON(w, http.StatusOK, op.Replay)
		return
	}
	resolved, err := h.authorizer.ResolveApprovalRouteWithBearer(r.Context(), session.UpstreamAccessToken, nexus.ApprovalResolveRequest{EnterpriseID: session.EnterpriseID, ActorUserID: current.RequesterUserID, IdempotencyKey: key, OrgVersion: session.OrgVersion, OrgUnitID: current.Policy.OrgUnitID, ResourceType: "dream_policy", ResourceID: current.ID, Action: "dream_policy." + input.Action, ChangedFields: changed, ImpactedOrgUnitIDs: []string{current.Policy.OrgUnitID}, RequestedRisk: string(assessment.RiskLevel), FactsIssuedAt: op.Row.FactsIssuedAt.Time, FactsExpiresAt: op.Row.FactsExpiresAt.Time, FactsNonce: op.Row.FactsNonce})
	if err != nil {
		writeError(w, http.StatusBadGateway, "governance_failed", "Review route could not be resolved")
		return
	}
	if resolved.RequesterUserID != current.RequesterUserID || (assessment.RiskLevel == sdkgovernance.RiskHigh && resolved.RiskLevel != "high") {
		writeError(w, http.StatusBadGateway, "governance_binding_mismatch", "Review route binding mismatch")
		return
	}
	level := sdkgovernance.RiskLevel(resolved.RiskLevel)
	if level != sdkgovernance.RiskLow {
		level = sdkgovernance.RiskHigh
	}
	route := sdkgovernance.ReviewRoute{ChangeID: current.ID, ResourceType: sdkgovernance.ResourceDreamPolicy, ResourceID: current.ID, RequesterUserID: current.RequesterUserID, ReviewerUserID: resolved.ReviewerUserID, RiskLevel: level, Mode: sdkgovernance.ReviewMode(resolved.Mode), State: sdkgovernance.RoutePending, OrgPath: resolved.OrgPath, Queue: resolved.Queue}
	if route.RiskLevel == sdkgovernance.RiskHigh && route.ReviewerUserID == current.RequesterUserID {
		writeError(w, http.StatusForbidden, "self_review_denied", "Requester cannot review their own high-risk change")
		return
	}
	audit, ok := h.auditPolicy(w, r, session, op, current, "review:"+input.Action, map[string]any{"revision": input.Revision, "risk_level": route.RiskLevel, "reviewer_user_id": route.ReviewerUserID})
	if !ok {
		return
	}
	view, err := h.operations.SubmitReview(r.Context(), session.EnterpriseID, current.ID, session.UserID, audit, key, input.Action, input.Revision, route, resolved.RiskReasons)
	if err != nil {
		writeError(w, http.StatusConflict, "review_failed", err.Error())
		return
	}
	h.finishPolicyOperation(w, r, session, key, view, http.StatusOK)
}

func (h *browserDreamHandler) decidePolicy(w http.ResponseWriter, r *http.Request) {
	session, current, ok := h.loadPolicy(w, r)
	if !ok {
		return
	}
	var input struct {
		Revision int32  `json:"revision"`
		Decision string `json:"decision"`
		Comment  string `json:"comment"`
	}
	if !decodeJSON(w, r, &input) {
		return
	}
	if current.Revision != input.Revision || current.ReviewState != "pending" {
		writeError(w, http.StatusConflict, "decision_failed", "Policy is not pending this revision")
		return
	}
	if current.ReviewMode == sdkgovernance.ReviewAdminQueue {
		writeError(w, http.StatusConflict, "reviewer_unassigned", "Refresh the review route before deciding")
		return
	}
	if current.ReviewMode == sdkgovernance.ReviewUpward && (current.ReviewerUserID != session.UserID || current.RequesterUserID == session.UserID) {
		writeError(w, http.StatusForbidden, "wrong_reviewer", "Actor is not the assigned reviewer")
		return
	}
	if current.ReviewMode == sdkgovernance.ReviewSingleConfirmation && current.RequesterUserID != session.UserID {
		writeError(w, http.StatusForbidden, "wrong_confirmer", "Requester confirmation is required")
		return
	}
	permission := "publish_low_risk"
	if current.RiskLevel == sdkgovernance.RiskHigh {
		permission = "approve_high_risk"
	}
	if !browserDreamPermission(session.Permissions, permission) {
		writeError(w, http.StatusForbidden, "forbidden", "Dream policy decision is not authorized")
		return
	}
	if !h.authorize(w, r, session, current.Policy.OrgUnitID, "dream_policy", current.ID, "dream.policy.decide") {
		return
	}
	op, key, ok := h.beginPolicyOperation(w, r, session, "decision", current.ID, input)
	if !ok {
		return
	}
	if op.Replay != nil {
		writeJSON(w, http.StatusOK, op.Replay)
		return
	}
	audit, ok := h.auditPolicy(w, r, session, op, current, "decision", map[string]any{"revision": input.Revision, "decision": input.Decision, "comment": input.Comment})
	if !ok {
		return
	}
	view, err := h.operations.Decide(r.Context(), session.EnterpriseID, current.ID, session.UserID, audit, key, input.Decision, input.Revision)
	if err != nil {
		writeError(w, http.StatusConflict, "decision_failed", err.Error())
		return
	}
	h.finishPolicyOperation(w, r, session, key, view, http.StatusOK)
}

func (h *browserDreamHandler) publishPolicy(w http.ResponseWriter, r *http.Request) {
	h.finalizePolicy(w, r, "publish")
}
func (h *browserDreamHandler) disablePolicy(w http.ResponseWriter, r *http.Request) {
	h.finalizePolicy(w, r, "disable")
}
func (h *browserDreamHandler) finalizePolicy(w http.ResponseWriter, r *http.Request, action string) {
	session, current, ok := h.authorizePolicy(w, r, "edit", "dream.policy."+action)
	if !ok {
		return
	}
	var input struct {
		Revision int32 `json:"revision"`
	}
	if !decodeJSON(w, r, &input) {
		return
	}
	if current.PermissionMode == sdkgovernance.PermissionSuggestionOnly || current.Revision != input.Revision || current.ReviewState != "approved" || current.PendingAction != action {
		writeError(w, http.StatusConflict, action+"_failed", "Approved decision is required")
		return
	}
	op, key, ok := h.beginPolicyOperation(w, r, session, action, current.ID, input)
	if !ok {
		return
	}
	if op.Replay != nil {
		writeJSON(w, http.StatusOK, op.Replay)
		return
	}
	audit, ok := h.auditPolicy(w, r, session, op, current, action, map[string]any{"revision": input.Revision})
	if !ok {
		return
	}
	var view dream.LifecycleView
	var err error
	if action == "publish" {
		view, err = h.operations.PublishGoverned(r.Context(), session.EnterpriseID, current.ID, session.UserID, audit, key, input.Revision)
	} else {
		view, err = h.operations.Disable(r.Context(), session.EnterpriseID, current.ID, session.UserID, audit, key, input.Revision)
	}
	if err != nil {
		writeError(w, http.StatusConflict, action+"_failed", err.Error())
		return
	}
	h.finishPolicyOperation(w, r, session, key, view, http.StatusOK)
}

func (h *browserDreamHandler) backfillPolicy(w http.ResponseWriter, r *http.Request) {
	session, current, ok := h.authorizePolicy(w, r, "edit", "dream.policy.backfill")
	if !ok {
		return
	}
	if h.backfill == nil {
		writeError(w, http.StatusServiceUnavailable, "backfill_unavailable", "Dream backfill is unavailable")
		return
	}
	var input struct {
		WindowStart  time.Time `json:"window_start"`
		WindowEnd    time.Time `json:"window_end"`
		RerunOfRunID string    `json:"rerun_of_run_id"`
	}
	if !decodeJSON(w, r, &input) {
		return
	}
	key, err := operationKey(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	request := dream.BackfillRequest{EnterpriseID: session.EnterpriseID, PolicyID: current.ID, WindowStart: input.WindowStart, WindowEnd: input.WindowEnd, RerunOfRunID: input.RerunOfRunID, IdempotencyKey: key}
	op, err := h.operations.BeginReceipt(r.Context(), session.EnterpriseID, key, "backfill", current.ID, session.UserID, operationHash(map[string]any{"policy_id": current.ID, "window_start": input.WindowStart, "window_end": input.WindowEnd, "rerun_of_run_id": input.RerunOfRunID}))
	if err != nil {
		writeError(w, http.StatusConflict, "backfill_failed", err.Error())
		return
	}
	if len(op.Replay) > 0 {
		var result map[string]string
		if json.Unmarshal(op.Replay, &result) != nil {
			writeError(w, http.StatusServiceUnavailable, "backfill_failed", "Backfill receipt is unavailable")
			return
		}
		writeJSON(w, http.StatusAccepted, result)
		return
	}
	if id, found, err := h.backfill.LookupBackfill(r.Context(), request); err != nil {
		writeError(w, http.StatusConflict, "backfill_failed", err.Error())
		return
	} else if found {
		result := map[string]string{"run_id": id}
		if _, err := h.operations.CompleteReceipt(r.Context(), session.EnterpriseID, key, result); err != nil {
			writeError(w, http.StatusServiceUnavailable, "backfill_failed", err.Error())
			return
		}
		writeJSON(w, http.StatusAccepted, result)
		return
	}
	audit, err := h.evidence.AppendAuditEvidenceWithBearer(r.Context(), session.UpstreamAccessToken, nexus.AppendAuditEvidenceRequest{IdempotencyKey: auditIdempotencyKey(session.EnterpriseID, key), EnterpriseID: session.EnterpriseID, OrgVersion: session.OrgVersion, OrgUnitID: current.Policy.OrgUnitID, AuthorizedAction: "dream.policy.backfill", Action: nexus.AuditDreamJobRun, ResourceType: "dream_policy", ResourceID: current.ID, Details: map[string]any{"window_start": input.WindowStart, "window_end": input.WindowEnd, "rerun_of_run_id": input.RerunOfRunID, "phase": "backfill_attempt"}})
	if err != nil || strings.TrimSpace(audit.AuditRefID) == "" {
		writeError(w, http.StatusServiceUnavailable, "audit_failed", "Backfill could not be audited")
		return
	}
	if _, err := h.operations.RecordOperationAudit(r.Context(), session.EnterpriseID, key, audit.AuditRefID); err != nil {
		writeError(w, http.StatusServiceUnavailable, "backfill_failed", err.Error())
		return
	}
	request.AuditRefID = audit.AuditRefID
	id, err := h.backfill.Backfill(r.Context(), request)
	if err != nil {
		writeError(w, http.StatusConflict, "backfill_failed", err.Error())
		return
	}
	result := map[string]string{"run_id": id}
	if _, err := h.operations.CompleteReceipt(r.Context(), session.EnterpriseID, key, result); err != nil {
		writeError(w, http.StatusServiceUnavailable, "backfill_failed", err.Error())
		return
	}
	writeJSON(w, http.StatusAccepted, result)
}

func (h *browserDreamHandler) loadPolicy(w http.ResponseWriter, r *http.Request) (browsersession.Session, dream.LifecycleView, bool) {
	session, ok := browserActorFrom(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "unauthorized", "no active browser session")
		return session, dream.LifecycleView{}, false
	}
	if h.operations == nil {
		writeError(w, http.StatusServiceUnavailable, "dream_policy_unavailable", "Dream policies are unavailable")
		return session, dream.LifecycleView{}, false
	}
	view, err := h.operations.GetLifecycle(r.Context(), session.EnterpriseID, chi.URLParam(r, "id"))
	if err != nil {
		writeError(w, http.StatusNotFound, "not_found", "Dream policy was not found")
		return session, view, false
	}
	if !containsExactOrganization(session.OrgUnitIDs, view.Policy.OrgUnitID) {
		writeError(w, http.StatusForbidden, "forbidden", "Dream policy organization is not authorized")
		return session, view, false
	}
	return session, view, true
}
func (h *browserDreamHandler) authorizePolicy(w http.ResponseWriter, r *http.Request, permission, action string) (browsersession.Session, dream.LifecycleView, bool) {
	session, view, ok := h.loadPolicy(w, r)
	if !ok {
		return session, view, false
	}
	if !browserDreamPermission(session.Permissions, permission) {
		writeError(w, http.StatusForbidden, "forbidden", "Dream policy operation is not authorized")
		return session, view, false
	}
	if !h.authorize(w, r, session, view.Policy.OrgUnitID, "dream_policy", view.ID, action) {
		return session, view, false
	}
	return session, view, true
}
func (h *browserDreamHandler) beginPolicyOperation(w http.ResponseWriter, r *http.Request, session browsersession.Session, kind, id string, input any) (dream.Operation, string, bool) {
	key, err := operationKey(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return dream.Operation{}, "", false
	}
	op, err := h.operations.BeginOperation(r.Context(), session.EnterpriseID, key, kind, id, session.UserID, operationHash(input))
	if err != nil {
		writeError(w, http.StatusConflict, kind+"_failed", err.Error())
		return dream.Operation{}, key, false
	}
	return op, key, true
}
func (h *browserDreamHandler) auditPolicy(w http.ResponseWriter, r *http.Request, session browsersession.Session, op dream.Operation, view dream.LifecycleView, phase string, details map[string]any) (string, bool) {
	details["phase"] = phase
	audit, err := h.evidence.AppendAuditEvidenceWithBearer(r.Context(), session.UpstreamAccessToken, nexus.AppendAuditEvidenceRequest{IdempotencyKey: auditIdempotencyKey(session.EnterpriseID, op.Row.OperationKey), EnterpriseID: session.EnterpriseID, OrgVersion: session.OrgVersion, OrgUnitID: view.Policy.OrgUnitID, AuthorizedAction: "dream.policy." + strings.Split(phase, ":")[0], Action: nexus.AuditDreamPolicyCreated, ResourceType: "dream_policy", ResourceID: view.ID, Details: details})
	if err != nil || strings.TrimSpace(audit.AuditRefID) == "" {
		writeError(w, http.StatusServiceUnavailable, "audit_failed", "Dream policy operation could not be audited")
		return "", false
	}
	if _, err := h.operations.RecordOperationAudit(r.Context(), session.EnterpriseID, op.Row.OperationKey, audit.AuditRefID); err != nil {
		writeError(w, http.StatusServiceUnavailable, "audit_failed", err.Error())
		return "", false
	}
	return audit.AuditRefID, true
}
func (h *browserDreamHandler) finishPolicyOperation(w http.ResponseWriter, r *http.Request, session browsersession.Session, key string, view dream.LifecycleView, status int) {
	completed, err := h.operations.CompleteOperation(r.Context(), session.EnterpriseID, key, view)
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "operation_failed", err.Error())
		return
	}
	writeJSON(w, status, completed)
}

func (h *browserDreamHandler) authorizeQuery(w http.ResponseWriter, r *http.Request, permission, action string) (browsersession.Session, string, bool) {
	session, ok := browserActorFrom(r.Context())
	org := strings.TrimSpace(r.URL.Query().Get("org_unit_id"))
	if !ok {
		writeError(w, http.StatusUnauthorized, "unauthorized", "no active browser session")
		return session, org, false
	}
	if !containsExactOrganization(session.OrgUnitIDs, org) || !browserDreamPermission(session.Permissions, permission) {
		writeError(w, http.StatusForbidden, "forbidden", "Dream organization is not authorized")
		return session, org, false
	}
	if !h.authorize(w, r, session, org, "dream_run", org, action) {
		return session, org, false
	}
	return session, org, true
}

func (h *browserDreamHandler) loadAuthorized(w http.ResponseWriter, r *http.Request, permission, action string) (browsersession.Session, sdkdream.DreamRunView, bool) {
	session, ok := browserActorFrom(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "unauthorized", "no active browser session")
		return session, sdkdream.DreamRunView{}, false
	}
	if !browserDreamPermission(session.Permissions, permission) {
		writeError(w, http.StatusForbidden, "forbidden", "Dream operation is not authorized")
		return session, sdkdream.DreamRunView{}, false
	}
	if h.store == nil {
		writeError(w, http.StatusServiceUnavailable, "dream_unavailable", "Dream workspace is unavailable")
		return session, sdkdream.DreamRunView{}, false
	}
	row, err := h.store.GetDreamRunView(r.Context(), db.GetDreamRunViewParams{EnterpriseID: session.EnterpriseID, RunID: chi.URLParam(r, "id")})
	if err != nil {
		writeError(w, http.StatusNotFound, "dream_run_not_found", "Dream run was not found")
		return session, sdkdream.DreamRunView{}, false
	}
	view, err := dreamRunView(row)
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "dream_unavailable", "Dream workspace is unavailable")
		return session, sdkdream.DreamRunView{}, false
	}
	if !containsExactOrganization(session.OrgUnitIDs, view.OrgUnitID) {
		writeError(w, http.StatusForbidden, "forbidden", "Dream organization is not authorized")
		return session, sdkdream.DreamRunView{}, false
	}
	if !h.authorize(w, r, session, view.OrgUnitID, "dream_run", view.RunID, action) {
		return session, sdkdream.DreamRunView{}, false
	}
	return session, view, true
}

func (h *browserDreamHandler) authorize(w http.ResponseWriter, r *http.Request, session browsersession.Session, org, resourceType, resourceID, action string) bool {
	if session.OrgVersion < 1 || session.UpstreamAccessToken == "" || h.authorizer == nil {
		writeError(w, http.StatusServiceUnavailable, "authorization_unavailable", "Dream authorization is unavailable")
		return false
	}
	decision, err := h.authorizer.AuthorizeBrowserOperation(r.Context(), session.UpstreamAccessToken, nexus.BrowserAuthorizationRequest{OrgUnitID: org, OrgVersion: session.OrgVersion, ResourceType: resourceType, ResourceID: resourceID, Action: action})
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "authorization_unavailable", "Dream authorization is unavailable")
		return false
	}
	if decision.Decision != "allow" || decision.OrgVersion != session.OrgVersion || !containsExactOrganization(decision.OrgUnitIDs, org) {
		writeError(w, http.StatusForbidden, "forbidden", "AgentNexus denied this Dream operation")
		return false
	}
	return true
}

func (h *browserDreamHandler) evidenceError(w http.ResponseWriter, err error) {
	if errors.Is(err, nexusclient.ErrDenied) {
		writeError(w, http.StatusForbidden, "forbidden", "AgentNexus denied evidence access")
		return
	}
	writeError(w, http.StatusServiceUnavailable, "evidence_unavailable", "Evidence access is unavailable")
}
func browserDreamPermission(values []string, required string) bool {
	for _, value := range values {
		if value == required || value == "admin" {
			return true
		}
	}
	return false
}
func browserDreamID(prefix string, values ...string) string {
	digest := sha256.Sum256([]byte(strings.Join(values, "\x00")))
	return fmt.Sprintf("%s_%s", prefix, hex.EncodeToString(digest[:16]))
}
