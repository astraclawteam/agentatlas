package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	sdkdream "github.com/astraclawteam/agentatlas/sdk/go/dream"
	"github.com/astraclawteam/agentatlas/sdk/go/nexus"
	db "github.com/astraclawteam/agentatlas/services/agentatlas/db/generated"
	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/dream"
	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/nexusclient"
	nexusruntime "github.com/astraclawteam/agentnexus/sdk/go/runtime"
	"github.com/go-chi/chi/v5"
)

const maxDreamRunList = 100

type dreamRunStore interface {
	GetDreamRunView(context.Context, db.GetDreamRunViewParams) (db.GetDreamRunViewRow, error)
	ListDreamRunsByOrg(context.Context, db.ListDreamRunsByOrgParams) ([]db.DreamRun, error)
	CreateDreamAnnotation(context.Context, db.CreateDreamAnnotationParams) (db.DreamRunAnnotation, error)
	ListDreamRunAnnotationsByRunBounded(context.Context, db.ListDreamRunAnnotationsByRunBoundedParams) ([]db.DreamRunAnnotation, error)
	ListDreamRunChildrenByParentBounded(context.Context, db.ListDreamRunChildrenByParentBoundedParams) ([]db.DreamRun, error)
}

type dreamRerunner interface {
	Rerun(context.Context, string, string, string, string) (string, error)
	LookupRerun(context.Context, string, string, string) (string, bool, error)
}

type dreamRunHandler struct {
	store dreamRunStore
	// evidence is the frozen-contract evidence surface.
	evidence FrozenEvidenceClient
	nexus    nexus.Client
	// orgAuthorization answers "may this actor act on this org unit?". It is a
	// separate dependency from nexus because that question belongs to the
	// authorization surface, not to evidence lookup.
	orgAuthorization nexus.OrgAuthorizationClient
	rerun            dreamRerunner
	operations       *dream.PolicyService
}

func (h *dreamRunHandler) detail(w http.ResponseWriter, r *http.Request) {
	actor, _ := actorFrom(r.Context())
	if !hasScope(actor.Ticket.Scopes, "dream:read") {
		writeError(w, http.StatusForbidden, "forbidden", "dream:read is required")
		return
	}
	view, err := h.load(r, chi.URLParam(r, "id"))
	if err != nil {
		writeError(w, http.StatusNotFound, "dream_run_not_found", "Dream run not found")
		return
	}
	if !h.authorizeOrg(w, r, "dream:read", view.OrgUnitID) {
		return
	}
	writeJSON(w, http.StatusOK, view)
}

func (h *dreamRunHandler) overview(w http.ResponseWriter, r *http.Request) { h.list(w, r) }

func (h *dreamRunHandler) list(w http.ResponseWriter, r *http.Request) {
	if h.store == nil {
		writeError(w, http.StatusServiceUnavailable, "dream_runs_unavailable", "Dream run store unavailable")
		return
	}
	actor, _ := actorFrom(r.Context())
	org := strings.TrimSpace(r.URL.Query().Get("org_unit_id"))
	if org == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "org_unit_id is required")
		return
	}
	if !h.authorizeOrg(w, r, "dream:read", org) {
		return
	}
	runs, err := h.store.ListDreamRunsByOrg(r.Context(), db.ListDreamRunsByOrgParams{EnterpriseID: actor.Ticket.EnterpriseID, OrgUnitID: org, ResultLimit: maxDreamRunList + 1})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "dream_runs_failed", err.Error())
		return
	}
	if len(runs) > maxDreamRunList {
		writeError(w, http.StatusConflict, "dream_runs_unbounded", "Dream run list exceeds bound")
		return
	}
	window := strings.TrimSpace(r.URL.Query().Get("window"))
	views := make([]sdkdream.DreamRunView, 0, len(runs))
	for _, run := range runs {
		if window != "" && run.WindowEnd.Valid && run.WindowEnd.Time.Format("2006-01-02") != window {
			continue
		}
		row, err := h.store.GetDreamRunView(r.Context(), db.GetDreamRunViewParams{EnterpriseID: actor.Ticket.EnterpriseID, RunID: run.ID})
		if err != nil {
			writeError(w, http.StatusInternalServerError, "dream_run_failed", err.Error())
			return
		}
		view, err := dreamRunView(row)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "dream_run_invalid", err.Error())
			return
		}
		views = append(views, view)
	}
	writeJSON(w, http.StatusOK, map[string]any{"runs": views})
}

func (h *dreamRunHandler) annotate(w http.ResponseWriter, r *http.Request) {
	if h.store == nil {
		writeError(w, http.StatusServiceUnavailable, "dream_runs_unavailable", "Dream run store unavailable")
		return
	}
	actor, _ := actorFrom(r.Context())
	if !hasScope(actor.Ticket.Scopes, "dream:annotate") {
		writeError(w, http.StatusForbidden, "forbidden", "dream:annotate is required")
		return
	}
	view, err := h.load(r, chi.URLParam(r, "id"))
	if err != nil {
		writeError(w, http.StatusNotFound, "dream_run_not_found", "Dream run not found")
		return
	}
	if !h.authorizeOrg(w, r, "dream:annotate", view.OrgUnitID) {
		return
	}
	var req struct {
		Action  string `json:"action"`
		Comment string `json:"comment"`
	}
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	if !validAnnotation(req.Action) || len([]rune(req.Comment)) > 4000 || (req.Action == "comment" && strings.TrimSpace(req.Comment) == "") {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid bounded annotation")
		return
	}
	row, err := h.store.CreateDreamAnnotation(r.Context(), db.CreateDreamAnnotationParams{ID: newID("dann"), EnterpriseID: actor.Ticket.EnterpriseID, RunID: chi.URLParam(r, "id"), AnnotationType: req.Action, Body: req.Comment, CreatedBy: actor.Ticket.ActorUserID})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "annotation_failed", err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, row)
}

func (h *dreamRunHandler) rerunRun(w http.ResponseWriter, r *http.Request) {
	actor, _ := actorFrom(r.Context())
	if h.rerun == nil {
		writeError(w, http.StatusServiceUnavailable, "rerun_unavailable", "Dream rerun service unavailable")
		return
	}
	if !hasScope(actor.Ticket.Scopes, "dream:rerun") {
		writeError(w, http.StatusForbidden, "forbidden", "dream:rerun is required")
		return
	}
	view, err := h.load(r, chi.URLParam(r, "id"))
	if err != nil {
		writeError(w, http.StatusNotFound, "dream_run_not_found", "Dream run not found")
		return
	}
	if !h.authorizeOrg(w, r, "dream:rerun", view.OrgUnitID) {
		return
	}
	if h.operations == nil {
		writeError(w, http.StatusServiceUnavailable, "rerun_unavailable", "Dream operation receipt service unavailable")
		return
	}
	key, err := operationKey(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	op, err := h.operations.BeginReceipt(r.Context(), actor.Ticket.EnterpriseID, key, "rerun", view.RunID, actor.Ticket.ActorUserID, operationHash(map[string]any{"source_run_id": view.RunID}))
	if err != nil {
		writeError(w, http.StatusConflict, "rerun_failed", err.Error())
		return
	}
	if len(op.Replay) > 0 {
		var result map[string]string
		if err := json.Unmarshal(op.Replay, &result); err != nil {
			writeError(w, http.StatusInternalServerError, "rerun_failed", err.Error())
			return
		}
		writeJSON(w, http.StatusAccepted, result)
		return
	}
	if id, found, err := h.rerun.LookupRerun(r.Context(), actor.Ticket.EnterpriseID, chi.URLParam(r, "id"), key); err != nil {
		writeError(w, http.StatusConflict, "rerun_failed", err.Error())
		return
	} else if found {
		result := map[string]string{"run_id": id}
		if _, err := h.operations.CompleteReceipt(r.Context(), actor.Ticket.EnterpriseID, key, result); err != nil {
			writeError(w, http.StatusInternalServerError, "rerun_failed", err.Error())
			return
		}
		writeJSON(w, http.StatusAccepted, result)
		return
	}
	audit, err := h.nexus.AppendAuditEvidence(r.Context(), nexus.AppendAuditEvidenceRequest{IdempotencyKey: auditIdempotencyKey(actor.Ticket.EnterpriseID, key), TicketID: actor.TicketID, EnterpriseID: actor.Ticket.EnterpriseID, Action: nexus.AuditDreamJobRun, ResourceType: "dream_run", ResourceID: view.RunID, Details: map[string]any{"org_unit_id": view.OrgUnitID, "idempotency_key": key, "phase": "manual_rerun_attempt"}})
	if errors.Is(err, nexusclient.ErrConflict) {
		writeError(w, http.StatusConflict, "idempotency_conflict", err.Error())
		return
	}
	if err != nil || strings.TrimSpace(audit.AuditRefID) == "" {
		if err == nil {
			err = fmt.Errorf("AgentNexus returned no durable audit reference")
		}
		writeError(w, http.StatusInternalServerError, "audit_failed", err.Error())
		return
	}
	if _, err := h.operations.RecordOperationAudit(r.Context(), actor.Ticket.EnterpriseID, key, audit.AuditRefID); err != nil {
		writeError(w, http.StatusInternalServerError, "operation_receipt_failed", err.Error())
		return
	}
	id, err := h.rerun.Rerun(r.Context(), actor.Ticket.EnterpriseID, chi.URLParam(r, "id"), key, audit.AuditRefID)
	if err != nil {
		writeError(w, http.StatusConflict, "rerun_failed", err.Error())
		return
	}
	result := map[string]string{"run_id": id}
	if _, err := h.operations.CompleteReceipt(r.Context(), actor.Ticket.EnterpriseID, key, result); err != nil {
		writeError(w, http.StatusInternalServerError, "operation_receipt_failed", err.Error())
		return
	}
	writeJSON(w, http.StatusAccepted, result)
}

func (h *dreamRunHandler) evidenceAccess(w http.ResponseWriter, r *http.Request) {
	actor, _ := actorFrom(r.Context())
	if !hasScope(actor.Ticket.Scopes, "dream:evidence:read") {
		writeError(w, http.StatusForbidden, "forbidden", "dream:evidence:read is required")
		return
	}
	view, err := h.load(r, chi.URLParam(r, "id"))
	if err != nil || view.EvidencePointerID == "" {
		writeError(w, http.StatusNotFound, "dream_evidence_not_found", "Dream evidence not found")
		return
	}
	if !h.authorizeOrg(w, r, "dream:evidence:read", view.OrgUnitID) {
		return
	}
	if h.evidence == nil {
		writeError(w, http.StatusServiceUnavailable, "evidence_unavailable", "AgentNexus evidence surface unavailable")
		return
	}
	// Declare WHAT is needed and receive opaque handles. Identity comes from
	// the verified service credential at ingress, so no ticket or enterprise
	// identifier travels in the body, and no resource URI is put on the wire.
	located, err := h.evidence.Locate(r.Context(), nexusruntime.EvidenceRequest{
		RequestID: "dream-evidence-" + view.RunID,
		Purpose:   dreamEvidencePurpose,
		DataNeeds: []nexusruntime.DataNeed{{
			NeedID:    view.EvidencePointerID,
			DataClass: dreamEvidenceDataClass,
			Purpose:   dreamEvidencePurpose,
		}},
		ExpiresAt: time.Now().Add(evidenceRequestTTL).UTC(),
	})
	if err != nil {
		h.writeNexusEvidenceError(w, err)
		return
	}
	if len(located.Evidence) == 0 {
		writeError(w, http.StatusNotFound, "dream_evidence_not_found", "AgentNexus located no evidence for this pointer")
		return
	}
	// Fields is deliberately omitted: the retired surface read a byte-capped
	// blob, and inventing a field list here would silently narrow what the
	// drill-down returns. Omitted means every field the policy permits.
	read, err := h.evidence.Read(r.Context(), nexusruntime.EvidenceReadRequest{
		RequestID:          "dream-evidence-read-" + view.RunID,
		BusinessContextRef: located.BusinessContextRef,
		EvidenceRef:        located.Evidence[0].EvidenceRef,
		Purpose:            dreamEvidencePurpose,
		ExpiresAt:          time.Now().Add(evidenceRequestTTL).UTC(),
	})
	if err != nil {
		h.writeNexusEvidenceError(w, err)
		return
	}
	if strings.TrimSpace(read.GrantRef) == "" {
		writeError(w, http.StatusForbidden, "grant_required", "AgentNexus returned no bound Step Grant")
		return
	}
	audit, err := h.nexus.AppendAuditEvidence(r.Context(), nexus.AppendAuditEvidenceRequest{TicketID: actor.TicketID, EnterpriseID: actor.Ticket.EnterpriseID, Action: nexus.AuditEvidenceRead, ResourceType: "dream_run", ResourceID: view.RunID, Details: map[string]any{"evidence_pointer_id": view.EvidencePointerID, "grant_ref": read.GrantRef, "actor_user_id": actor.Ticket.ActorUserID}})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "audit_failed", err.Error())
		return
	}
	if strings.TrimSpace(audit.AuditRefID) == "" {
		writeError(w, http.StatusInternalServerError, "audit_failed", "AgentNexus returned no durable audit reference")
		return
	}
	// The freshness trio is carried through rather than dropped. The frozen
	// contract states it on every allowed read precisely so a cached answer can
	// never masquerade as a live one; swallowing it here would re-create that
	// ambiguity one layer up.
	writeJSON(w, http.StatusOK, map[string]any{
		"grant_ref":         read.GrantRef,
		"audit_ref_id":      audit.AuditRefID,
		"decision":          read.Decision,
		"data":              read.Data,
		"receipt_ref":       read.ReceiptRef,
		"source_version":    read.SourceVersion,
		"as_of":             read.AsOf,
		"served_from_cache": read.ServedFromCache,
	})
}

func dreamOrgResourceURI(enterpriseID, orgUnitID string) string {
	return "agentatlas://dream/enterprises/" + url.PathEscape(enterpriseID) + "/org-units/" + url.PathEscape(orgUnitID)
}

func (h *dreamRunHandler) authorizeOrg(w http.ResponseWriter, r *http.Request, action, orgUnitID string) bool {
	actor, ok := actorFrom(r.Context())
	if !ok || !hasScope(actor.Ticket.Scopes, action) {
		writeError(w, http.StatusForbidden, "forbidden", action+" is required")
		return false
	}
	if h.orgAuthorization == nil || actor.Ticket.EnterpriseID == "" || strings.TrimSpace(orgUnitID) == "" {
		writeError(w, http.StatusServiceUnavailable, "authorization_unavailable", "AgentNexus organization authorization unavailable")
		return false
	}
	// Ask the authorization surface for a decision instead of inferring one
	// from an evidence lookup echoing back a synthesized resource URI.
	decision, err := h.orgAuthorization.AuthorizeTicketOperation(r.Context(), actor.TicketID, nexus.BrowserAuthorizationRequest{
		OrgUnitID:    orgUnitID,
		OrgVersion:   actor.Ticket.OrgVersion,
		ResourceType: "dream_run",
		ResourceID:   orgUnitID,
		Action:       action,
	})
	if errors.Is(err, nexusclient.ErrDenied) {
		writeError(w, http.StatusForbidden, "forbidden", "AgentNexus denied organization access")
		return false
	}
	if err != nil {
		writeError(w, http.StatusBadGateway, "authorization_failed", err.Error())
		return false
	}
	// Anything that is not an explicit allow is a denial: an unrecognised
	// decision value must never read as permission.
	if decision.Decision != "allow" {
		writeError(w, http.StatusForbidden, "forbidden", "AgentNexus denied organization access")
		return false
	}
	return true
}

func (h *dreamRunHandler) writeNexusEvidenceError(w http.ResponseWriter, err error) {
	if errors.Is(err, nexusclient.ErrDenied) {
		writeError(w, http.StatusForbidden, "evidence_denied", "AgentNexus denied evidence access")
		return
	}
	writeError(w, http.StatusBadGateway, "nexus_failed", err.Error())
}

func (h *dreamRunHandler) load(r *http.Request, id string) (sdkdream.DreamRunView, error) {
	if h.store == nil {
		return sdkdream.DreamRunView{}, fmt.Errorf("Dream run store unavailable")
	}
	actor, ok := actorFrom(r.Context())
	if !ok || actor.Ticket.EnterpriseID == "" || id == "" {
		return sdkdream.DreamRunView{}, fmt.Errorf("missing actor or run")
	}
	row, err := h.store.GetDreamRunView(r.Context(), db.GetDreamRunViewParams{EnterpriseID: actor.Ticket.EnterpriseID, RunID: id})
	if err != nil {
		return sdkdream.DreamRunView{}, err
	}
	return dreamRunView(row)
}

func dreamRunView(row db.GetDreamRunViewRow) (sdkdream.DreamRunView, error) {
	var coverage sdkdream.Coverage
	var missing []sdkdream.MissingInput
	var facts, themes, trends, risks, todos []sdkdream.StructuredSignal
	var input sdkdream.InputSnapshotSummary
	var visibility sdkdream.VisibilitySnapshotSummary
	for _, item := range []struct {
		raw []byte
		dst any
	}{{row.Coverage, &coverage}, {row.MissingInputs, &missing}, {row.Facts, &facts}, {row.Themes, &themes}, {row.Trends, &trends}, {row.Risks, &risks}, {row.Todos, &todos}, {row.InputSnapshot, &input}, {row.VisibilitySnapshot, &visibility}} {
		if len(item.raw) == 0 {
			continue
		}
		dec := json.NewDecoder(bytes.NewReader(item.raw))
		dec.DisallowUnknownFields()
		if err := dec.Decode(item.dst); err != nil {
			return sdkdream.DreamRunView{}, err
		}
	}
	view := sdkdream.DreamRunView{RunID: row.ID, Status: sdkdream.RunStatus(row.Status), OrgUnitID: row.OrgUnitID, PolicyVersion: row.PolicyVersion, Workflow: sdkdream.WorkflowRef{ID: row.WorkflowID.String, Version: row.WorkflowVersion.Int32}, ParentRunIDs: nonNilStrings(row.ParentRunIds), InputCount: int32(row.InputCount), Coverage: coverage, MissingInputs: nonNilMissing(missing), Facts: nonNilSignals(facts), Themes: nonNilSignals(themes), Trends: nonNilSignals(trends), Risks: nonNilSignals(risks), Todos: nonNilSignals(todos), DisplaySummary: row.DisplaySummary, EvidencePointerID: row.EvidencePointerID.String, InputSnapshot: input, VisibilitySnapshot: visibility, ModelRoute: row.ModelRoute, ModelVersion: row.ModelVersion, Attempt: row.Attempt, IdempotencyKey: row.IdempotencyKey}
	if row.Status != string(sdkdream.RunSucceeded) {
		view.DisplaySummary = ""
		view.EvidencePointerID = ""
		view.Facts = []sdkdream.StructuredSignal{}
		view.Themes = []sdkdream.StructuredSignal{}
		view.Trends = []sdkdream.StructuredSignal{}
		view.Risks = []sdkdream.StructuredSignal{}
		view.Todos = []sdkdream.StructuredSignal{}
	}
	if row.WindowStart.Valid {
		view.WindowStart = row.WindowStart.Time
	}
	if row.WindowEnd.Valid {
		view.WindowEnd = row.WindowEnd.Time
	}
	if row.RerunOfRunID.Valid {
		view.RerunOfRunID = row.RerunOfRunID.String
	}
	return view, nil
}

func validAnnotation(v string) bool {
	switch v {
	case "confirm", "reject", "mark_incorrect", "comment":
		return true
	}
	return false
}
func hasScope(scopes []string, wanted string) bool {
	for _, s := range scopes {
		if s == wanted || s == "admin" {
			return true
		}
	}
	return false
}
func nonNilStrings(v []string) []string {
	if v == nil {
		return []string{}
	}
	return v
}
func nonNilSignals(v []sdkdream.StructuredSignal) []sdkdream.StructuredSignal {
	if v == nil {
		return []sdkdream.StructuredSignal{}
	}
	return v
}
func nonNilMissing(v []sdkdream.MissingInput) []sdkdream.MissingInput {
	if v == nil {
		return []sdkdream.MissingInput{}
	}
	return v
}
