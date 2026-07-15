package app

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"

	assessmentmodel "github.com/astraclawteam/agentatlas/sdk/go/assessment"
	"github.com/astraclawteam/agentatlas/sdk/go/nexus"
	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/assessment"
)

// assessment_handler.go serves the Task 18D assessment surfaces:
//
//   - the EMPLOYEE (Xiaozhi/ticket runtime, atlas-runtime.yaml): read your OWN
//     assessment (fail-closed projection, no score/level/manager-notes), submit a
//     correction, and read its outcome. Identity is the verified Nexus ticket.
//   - the MANAGER (Console BFF, atlas-agent.yaml): read the authorized assessment
//     detail (score/level + manager notes) only after the exact hierarchy +
//     policy authorization. Identity is the verified browser session.
//
// Both handlers are registered UNCONDITIONALLY (so the contract-drift walk always
// sees them) and fail closed to 503 when their backing service is not composed.

// --- employee runtime surface (atlas-runtime.yaml) ---------------------------

type assessmentRuntimeHandler struct {
	nexus       nexus.Client
	results     assessment.ResultStore
	corrections *assessment.CorrectionService
}

// getForEmployee returns the fail-closed employee projection of the caller's OWN
// assessment. A cross-tenant or non-subject read is 404 (an assessment is never
// enumerable to anyone but its subject).
func (h *assessmentRuntimeHandler) getForEmployee(w http.ResponseWriter, req *http.Request) {
	if h.results == nil {
		writeError(w, http.StatusServiceUnavailable, "assessment_unavailable", "assessment surface not configured")
		return
	}
	_, ticket, err := verifyTicket(req.Context(), h.nexus, req)
	if err != nil {
		writeError(w, http.StatusUnauthorized, "unauthorized", err.Error())
		return
	}
	wa, err := h.results.GetAssessment(req.Context(), ticket.EnterpriseID, chi.URLParam(req, "id"))
	if err != nil {
		writeError(w, http.StatusNotFound, "not_found", "assessment not found")
		return
	}
	view, err := assessment.ProjectForEmployee(wa, ticket.ActorUserID)
	if err != nil {
		// A non-subject read must not reveal that the assessment exists.
		writeError(w, http.StatusNotFound, "not_found", "assessment not found")
		return
	}
	writeJSON(w, http.StatusOK, view)
}

type submitCorrectionBody struct {
	Kind             string `json:"kind"`
	Dimension        string `json:"dimension"`
	ChallengedHandle string `json:"challenged_handle,omitempty"`
	Rationale        string `json:"rationale"`
	AddedEvidence    []struct {
		Handle  string `json:"handle"`
		Kind    string `json:"kind,omitempty"`
		Summary string `json:"summary,omitempty"`
	} `json:"added_evidence,omitempty"`
}

// submitCorrection records an employee's correction against their OWN assessment.
// Employee-added evidence is recorded as a human_report authored by the employee
// (re-verified on re-evaluation); it can never be a self-signed receipt.
func (h *assessmentRuntimeHandler) submitCorrection(w http.ResponseWriter, req *http.Request) {
	if h.corrections == nil {
		writeError(w, http.StatusServiceUnavailable, "assessment_unavailable", "assessment surface not configured")
		return
	}
	_, ticket, err := verifyTicket(req.Context(), h.nexus, req)
	if err != nil {
		writeError(w, http.StatusUnauthorized, "unauthorized", err.Error())
		return
	}
	var body submitCorrectionBody
	if err := json.NewDecoder(http.MaxBytesReader(w, req.Body, 1<<20)).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "correction body is invalid")
		return
	}
	var added []assessmentmodel.Evidence
	for _, e := range body.AddedEvidence {
		kind := e.Kind
		if kind == "" {
			kind = "report"
		}
		added = append(added, assessmentmodel.Evidence{
			Tier: assessmentmodel.TierHumanReport, Handle: e.Handle, Kind: kind,
			Authority: ticket.ActorUserID, Summary: e.Summary,
		})
	}
	c, err := h.corrections.Submit(req.Context(), assessment.SubmitCorrectionRequest{
		Tenant:             ticket.EnterpriseID,
		EmployeeID:         ticket.ActorUserID,
		TargetAssessmentID: chi.URLParam(req, "id"),
		Kind:               assessment.CorrectionKind(body.Kind),
		Dimension:          assessmentmodel.DimensionKey(body.Dimension),
		AddedEvidence:      added,
		ChallengedHandle:   body.ChallengedHandle,
		Rationale:          body.Rationale,
	})
	if err != nil {
		switch {
		case errors.Is(err, assessment.ErrNotAssessmentSubject), errors.Is(err, assessment.ErrNotFound):
			writeError(w, http.StatusNotFound, "not_found", "assessment not found")
		default:
			writeError(w, http.StatusBadRequest, "invalid_correction", err.Error())
		}
		return
	}
	writeJSON(w, http.StatusCreated, assessment.ProjectCorrectionForEmployee(c))
}

// getCorrection returns the employee's OWN correction submission + outcome.
func (h *assessmentRuntimeHandler) getCorrection(w http.ResponseWriter, req *http.Request) {
	if h.corrections == nil {
		writeError(w, http.StatusServiceUnavailable, "assessment_unavailable", "assessment surface not configured")
		return
	}
	_, ticket, err := verifyTicket(req.Context(), h.nexus, req)
	if err != nil {
		writeError(w, http.StatusUnauthorized, "unauthorized", err.Error())
		return
	}
	c, err := h.corrections.Get(req.Context(), ticket.EnterpriseID, ticket.ActorUserID, chi.URLParam(req, "correctionId"))
	if err != nil {
		writeError(w, http.StatusNotFound, "not_found", "correction not found")
		return
	}
	writeJSON(w, http.StatusOK, assessment.ProjectCorrectionForEmployee(c))
}

// --- manager BFF surface (atlas-agent.yaml) ----------------------------------

type assessmentAgentHandler struct {
	manager *assessment.ManagerVisibilityService
}

// detail returns the authorized manager assessment detail (score/level + manager
// notes) for a subject within the manager's exact hierarchy scope and under a
// policy they own. An out-of-scope or unowned read is 403; a missing one 404.
func (h *assessmentAgentHandler) detail(w http.ResponseWriter, req *http.Request) {
	if h.manager == nil {
		writeError(w, http.StatusServiceUnavailable, "assessment_unavailable", "assessment surface not configured")
		return
	}
	session, ok := browserActorFrom(req.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "unauthorized", "no active browser session")
		return
	}
	view, err := h.manager.ManagerView(req.Context(), session.EnterpriseID, session.UserID, chi.URLParam(req, "id"))
	if err != nil {
		switch {
		case errors.Is(err, assessment.ErrNotFound):
			writeError(w, http.StatusNotFound, "not_found", "assessment not found")
		case errors.Is(err, assessment.ErrManagerNotAuthorized), errors.Is(err, assessment.ErrPolicyNotOwned), errors.Is(err, assessment.ErrSubjectPartyMismatch):
			writeError(w, http.StatusForbidden, "assessment_access_denied", "not authorized to read this assessment")
		default:
			writeError(w, http.StatusInternalServerError, "assessment_failed", "assessment read failed")
		}
		return
	}
	writeJSON(w, http.StatusOK, view)
}
