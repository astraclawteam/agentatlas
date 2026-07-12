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

	sdkdream "github.com/astraclawteam/agentatlas/sdk/go/dream"
	"github.com/astraclawteam/agentatlas/sdk/go/nexus"
	db "github.com/astraclawteam/agentatlas/services/agentatlas/db/generated"
	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/nexusclient"
	"github.com/go-chi/chi/v5"
)

const maxDreamRunList = 100

type dreamRunStore interface {
	GetDreamRunView(context.Context, db.GetDreamRunViewParams) (db.GetDreamRunViewRow, error)
	ListDreamRunsByOrg(context.Context, db.ListDreamRunsByOrgParams) ([]db.DreamRun, error)
	CreateDreamAnnotation(context.Context, db.CreateDreamAnnotationParams) (db.DreamRunAnnotation, error)
}

type dreamRerunner interface {
	Rerun(context.Context, string, string, string) (string, error)
}

type dreamRunHandler struct {
	store dreamRunStore
	nexus nexus.Client
	rerun dreamRerunner
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
	key := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
	if key == "" || len(key) > 256 {
		writeError(w, http.StatusBadRequest, "bad_request", "bounded Idempotency-Key is required")
		return
	}
	audit, err := h.nexus.AppendAuditEvidence(r.Context(), nexus.AppendAuditEvidenceRequest{TicketID: actor.TicketID, EnterpriseID: actor.Ticket.EnterpriseID, Action: nexus.AuditDreamJobRun, ResourceType: "dream_run", ResourceID: view.RunID, Details: map[string]any{"org_unit_id": view.OrgUnitID, "idempotency_key": key, "phase": "manual_rerun"}})
	if err != nil || strings.TrimSpace(audit.AuditRefID) == "" {
		if err == nil {
			err = fmt.Errorf("AgentNexus returned no durable audit reference")
		}
		writeError(w, http.StatusInternalServerError, "audit_failed", err.Error())
		return
	}
	id, err := h.rerun.Rerun(r.Context(), actor.Ticket.EnterpriseID, chi.URLParam(r, "id"), key)
	if err != nil {
		writeError(w, http.StatusConflict, "rerun_failed", err.Error())
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]string{"run_id": id})
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
	loc, err := h.nexus.LocateEvidence(r.Context(), nexus.LocateEvidenceRequest{TicketID: actor.TicketID, EnterpriseID: actor.Ticket.EnterpriseID, EvidencePointerID: view.EvidencePointerID, QueryIntent: "dream evidence drill-down"})
	if err != nil {
		h.writeNexusEvidenceError(w, err)
		return
	}
	read, err := h.nexus.ReadEvidence(r.Context(), nexus.ReadEvidenceRequest{TicketID: actor.TicketID, EnterpriseID: actor.Ticket.EnterpriseID, ResourceURI: loc.ResourceURI, EvidencePointerID: view.EvidencePointerID, MaxBytes: 100000})
	if err != nil {
		h.writeNexusEvidenceError(w, err)
		return
	}
	if strings.TrimSpace(read.GrantID) == "" {
		writeError(w, http.StatusForbidden, "grant_required", "AgentNexus returned no bound Step Grant")
		return
	}
	audit, err := h.nexus.AppendAuditEvidence(r.Context(), nexus.AppendAuditEvidenceRequest{TicketID: actor.TicketID, EnterpriseID: actor.Ticket.EnterpriseID, Action: nexus.AuditEvidenceRead, ResourceType: "dream_run", ResourceID: view.RunID, Details: map[string]any{"evidence_pointer_id": view.EvidencePointerID, "grant_id": read.GrantID, "actor_user_id": actor.Ticket.ActorUserID}})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "audit_failed", err.Error())
		return
	}
	if strings.TrimSpace(audit.AuditRefID) == "" {
		writeError(w, http.StatusInternalServerError, "audit_failed", "AgentNexus returned no durable audit reference")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"grant_id": read.GrantID, "audit_ref_id": audit.AuditRefID, "content_type": read.ContentType, "sanitized_detail": read.SanitizedExcerpt, "content_hash": read.ContentHash})
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
	if h.nexus == nil || actor.Ticket.EnterpriseID == "" || strings.TrimSpace(orgUnitID) == "" {
		writeError(w, http.StatusServiceUnavailable, "authorization_unavailable", "AgentNexus organization authorization unavailable")
		return false
	}
	resourceURI := dreamOrgResourceURI(actor.Ticket.EnterpriseID, orgUnitID)
	located, err := h.nexus.LocateEvidence(r.Context(), nexus.LocateEvidenceRequest{TicketID: actor.TicketID, EnterpriseID: actor.Ticket.EnterpriseID, ResourceURI: resourceURI, QueryIntent: action})
	if errors.Is(err, nexusclient.ErrDenied) {
		writeError(w, http.StatusForbidden, "forbidden", "AgentNexus denied organization access")
		return false
	}
	if err != nil {
		writeError(w, http.StatusBadGateway, "authorization_failed", err.Error())
		return false
	}
	if located.ResourceURI != resourceURI {
		writeError(w, http.StatusForbidden, "forbidden", "AgentNexus organization binding mismatch")
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
