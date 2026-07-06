package app

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/astraclawteam/agentatlas/sdk/go/nexus"
	db "github.com/astraclawteam/agentatlas/services/agentatlas/db/generated"
	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/retrieval"
)

// PlanStepLister is the slice of the store the plan route needs (fakeable in
// unit tests; *db.Queries satisfies it).
type PlanStepLister interface {
	ListRetrievalPlanSteps(ctx context.Context, planID string) ([]db.RetrievalPlanStep, error)
}

// retrievalHandler serves Retrieval Plan creation (POST /v1/retrieval/plans).
type retrievalHandler struct {
	nexus     nexus.Client
	retrieval *retrieval.Service
	store     PlanStepLister
}

type createPlanRequest struct {
	Query        string   `json:"query"`
	SpaceIDs     []string `json:"space_ids,omitempty"`
	OrgScopes    []string `json:"org_scopes,omitempty"`
	SourceTypes  []string `json:"source_types,omitempty"`
	MaxRiskLevel string   `json:"max_risk_level,omitempty"`
	TopK         int      `json:"top_k,omitempty"`
	From         *string  `json:"from,omitempty"`
	To           *string  `json:"to,omitempty"`
}

func (h *retrievalHandler) createPlan(w http.ResponseWriter, r *http.Request) {
	_, ticket, err := verifyTicket(r.Context(), h.nexus, r)
	if err != nil {
		writeError(w, http.StatusUnauthorized, "unauthorized", err.Error())
		return
	}
	var req createPlanRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	if req.Query == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "query is required")
		return
	}
	q := retrieval.Query{
		EnterpriseID: ticket.EnterpriseID, Text: req.Query,
		SpaceIDs: req.SpaceIDs, OrgScopes: req.OrgScopes,
		SourceTypes: req.SourceTypes, MaxRiskLevel: req.MaxRiskLevel, TopK: req.TopK,
	}
	if req.From != nil {
		if t, err := time.Parse(time.RFC3339, *req.From); err == nil {
			q.From = &t
		}
	}
	if req.To != nil {
		if t, err := time.Parse(time.RFC3339, *req.To); err == nil {
			q.To = &t
		}
	}
	planID, err := h.retrieval.CreatePlan(r.Context(), q)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "plan_failed", err.Error())
		return
	}
	var steps []db.RetrievalPlanStep
	if h.store != nil {
		steps, err = h.store.ListRetrievalPlanSteps(r.Context(), planID)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "plan_steps_failed", err.Error())
			return
		}
	}
	outSteps := make([]map[string]any, 0, len(steps))
	for _, s := range steps {
		outSteps = append(outSteps, map[string]any{
			"step_no": s.StepNo, "kind": s.Kind, "params": json.RawMessage(s.Params),
		})
	}
	writeJSON(w, http.StatusCreated, map[string]any{"plan_id": planID, "steps": outSteps})
}
