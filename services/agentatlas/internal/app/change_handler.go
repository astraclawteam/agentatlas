package app

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"

	model "github.com/astraclawteam/agentatlas/sdk/go/governance"
	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/governance"
	"github.com/go-chi/chi/v5"
)

type changeHandler struct{ service *governance.Service }

func changeAvailability(service *governance.Service) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if service == nil {
				writeError(w, http.StatusServiceUnavailable, "governance_unavailable", "change governance not configured")
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

func changeActor(r *http.Request) (governance.Actor, bool) {
	s, ok := browserActorFrom(r.Context())
	return governance.Actor{EnterpriseID: s.EnterpriseID, UserID: s.UserID, DisplayName: s.DisplayName, UpstreamAccessToken: s.UpstreamAccessToken, OrgVersion: s.OrgVersion, OrgUnitIDs: s.OrgUnitIDs, Permissions: s.Permissions}, ok
}

type changeInput struct {
	OrgUnitID       string             `json:"org_unit_id"`
	ResourceType    model.ResourceType `json:"resource_type"`
	ResourceID      string             `json:"resource_id"`
	Action          model.Action       `json:"action"`
	BaseVersion     int32              `json:"base_version"`
	Revision        int64              `json:"revision"`
	ProposedContent json.RawMessage    `json:"proposed_content"`
	Decision        string             `json:"decision"`
	Comment         string             `json:"comment"`
}

func decodeChangeInput(w http.ResponseWriter, r *http.Request, out *changeInput) error {
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(out); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return errors.New("one JSON object required")
	}
	return nil
}

func (h *changeHandler) create(w http.ResponseWriter, r *http.Request) {
	a, _ := changeActor(r)
	var in changeInput
	if decodeChangeInput(w, r, &in) != nil {
		writeError(w, 400, "bad_request", "invalid JSON")
		return
	}
	d, err := h.service.CreateDraft(r.Context(), a, governance.SuggestionInput{OrgUnitID: in.OrgUnitID, ResourceType: in.ResourceType, ResourceID: in.ResourceID, Action: in.Action, BaseVersion: in.BaseVersion, ProposedContent: in.ProposedContent})
	writeChangeResult(w, http.StatusCreated, d, err)
}

func (h *changeHandler) suggest(w http.ResponseWriter, r *http.Request) {
	a, _ := changeActor(r)
	var in changeInput
	if decodeChangeInput(w, r, &in) != nil {
		writeError(w, 400, "bad_request", "invalid JSON")
		return
	}
	d, err := h.service.Suggest(r.Context(), a, governance.SuggestionInput{OrgUnitID: in.OrgUnitID, ResourceType: in.ResourceType, ResourceID: in.ResourceID, Action: in.Action, BaseVersion: in.BaseVersion, ProposedContent: in.ProposedContent})
	writeChangeResult(w, http.StatusCreated, d, err)
}
func (h *changeHandler) update(w http.ResponseWriter, r *http.Request) {
	a, _ := changeActor(r)
	var in changeInput
	if decodeChangeInput(w, r, &in) != nil {
		writeError(w, 400, "bad_request", "invalid JSON")
		return
	}
	d, err := h.service.UpdateDraft(r.Context(), a, chi.URLParam(r, "id"), in.Revision, in.ProposedContent)
	writeChangeResult(w, 200, d, err)
}
func (h *changeHandler) get(w http.ResponseWriter, r *http.Request) {
	a, _ := changeActor(r)
	rec, err := h.service.Get(r.Context(), a, chi.URLParam(r, "id"))
	writeChangeResult(w, 200, rec, err)
}
func (h *changeHandler) list(w http.ResponseWriter, r *http.Request) {
	a, _ := changeActor(r)
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	items, err := h.service.List(r.Context(), a, r.URL.Query().Get("org_unit_id"), limit)
	writeChangeResult(w, 200, map[string]any{"items": items}, err)
}
func (h *changeHandler) diff(w http.ResponseWriter, r *http.Request) {
	a, _ := changeActor(r)
	diff, err := h.service.Diff(r.Context(), a, chi.URLParam(r, "id"))
	if err != nil {
		writeChangeResult(w, 0, nil, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(200)
	_, _ = w.Write(diff)
}
func (h *changeHandler) assess(w http.ResponseWriter, r *http.Request) {
	a, _ := changeActor(r)
	v, err := h.service.Assess(r.Context(), a, chi.URLParam(r, "id"))
	writeChangeResult(w, 200, v, err)
}
func (h *changeHandler) submit(w http.ResponseWriter, r *http.Request) {
	a, _ := changeActor(r)
	v, err := h.service.Submit(r.Context(), a, chi.URLParam(r, "id"))
	writeChangeResult(w, 200, v, err)
}
func (h *changeHandler) decide(w http.ResponseWriter, r *http.Request) {
	a, _ := changeActor(r)
	var in changeInput
	if decodeChangeInput(w, r, &in) != nil {
		writeError(w, 400, "bad_request", "invalid JSON")
		return
	}
	err := h.service.Decide(r.Context(), a, chi.URLParam(r, "id"), governance.DecisionInput{Decision: in.Decision, Comment: in.Comment})
	writeChangeResult(w, 204, nil, err)
}
func (h *changeHandler) publish(w http.ResponseWriter, r *http.Request) {
	a, _ := changeActor(r)
	v, err := h.service.Publish(r.Context(), a, chi.URLParam(r, "id"), r.Header.Get("Idempotency-Key"))
	writeChangeResult(w, 200, v, err)
}
func writeChangeResult(w http.ResponseWriter, status int, v any, err error) {
	if err == nil {
		if status == 204 {
			w.WriteHeader(status)
		} else {
			writeJSON(w, status, v)
		}
		return
	}
	var conflict *governance.ConflictError
	switch {
	case errors.As(err, &conflict):
		writeJSON(w, http.StatusConflict, map[string]any{"error": "revision_conflict", "current_revision": conflict.CurrentRevision, "diff": conflict.Diff})
	case errors.Is(err, governance.ErrForbidden):
		writeError(w, 403, "forbidden", err.Error())
	case errors.Is(err, governance.ErrNotFound):
		writeError(w, 404, "not_found", err.Error())
	default:
		writeError(w, 422, "change_failed", err.Error())
	}
}
