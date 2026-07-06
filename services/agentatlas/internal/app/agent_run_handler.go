package app

import (
	"encoding/json"
	"errors"
	"net/http"
	"sync"

	"github.com/go-chi/chi/v5"

	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/workflow"
)

// maxTrackedAgentRuns bounds the in-memory run-ownership table on the
// long-lived control plane: when full, an arbitrary stale entry is evicted
// (its run can no longer be continued — acceptable for a conversation cache,
// unlike unbounded growth).
const maxTrackedAgentRuns = 4096

// agentRunHandler serves Knowledge Agent runs. Run state (conversation
// history) lives in the ADK in-memory session service keyed by run id; this
// handler tracks run ownership so a run can only be continued by the same
// enterprise + actor that started it.
type agentRunHandler struct {
	deps AgentRouterDeps

	mu     sync.Mutex
	owners map[string]runOwner
}

type runOwner struct {
	EnterpriseID string
	ActorUserID  string
}

func newAgentRunHandler(deps AgentRouterDeps) *agentRunHandler {
	return &agentRunHandler{deps: deps, owners: map[string]runOwner{}}
}

type agentRunRequest struct {
	Message string `json:"message"`
}

type agentRunResponse struct {
	RunID     string          `json:"run_id"`
	Text      string          `json:"text"`
	ToolCalls []toolCallBrief `json:"tool_calls,omitempty"`
}

type toolCallBrief struct {
	Name   string          `json:"name"`
	Args   json.RawMessage `json:"args,omitempty"`
	Result json.RawMessage `json:"result,omitempty"`
}

func (h *agentRunHandler) start(w http.ResponseWriter, r *http.Request) {
	actor, ok := actorFrom(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "unauthorized", "no verified actor")
		return
	}
	if h.deps.Agent == nil {
		writeError(w, http.StatusServiceUnavailable, "agent_unavailable", "knowledge agent runner not configured")
		return
	}
	var req agentRunRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Message == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "message is required")
		return
	}
	runID := newID("arun")
	h.mu.Lock()
	if len(h.owners) >= maxTrackedAgentRuns {
		for k := range h.owners {
			delete(h.owners, k)
			break
		}
	}
	h.owners[runID] = runOwner{EnterpriseID: actor.Ticket.EnterpriseID, ActorUserID: actor.Ticket.ActorUserID}
	h.mu.Unlock()
	h.execute(w, r, runID, actor.Ticket.ActorUserID, req.Message, http.StatusCreated)
}

func (h *agentRunHandler) message(w http.ResponseWriter, r *http.Request) {
	actor, ok := actorFrom(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "unauthorized", "no verified actor")
		return
	}
	runID := chi.URLParam(r, "id")
	h.mu.Lock()
	owner, exists := h.owners[runID]
	h.mu.Unlock()
	if !exists {
		writeError(w, http.StatusNotFound, "not_found", "agent run not found")
		return
	}
	if owner.EnterpriseID != actor.Ticket.EnterpriseID || owner.ActorUserID != actor.Ticket.ActorUserID {
		// Cross-actor continuation is an access violation — fail closed.
		writeError(w, http.StatusForbidden, "forbidden", "run belongs to a different actor")
		return
	}
	var req agentRunRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Message == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "message is required")
		return
	}
	h.execute(w, r, runID, actor.Ticket.ActorUserID, req.Message, http.StatusOK)
}

func (h *agentRunHandler) execute(w http.ResponseWriter, r *http.Request, runID, userID, message string, okStatus int) {
	res, err := h.deps.Agent.Run(r.Context(), userID, runID, message)
	if err != nil {
		writeError(w, http.StatusBadGateway, "agent_failed", err.Error())
		return
	}
	out := agentRunResponse{RunID: runID, Text: res.Text}
	for _, tc := range res.ToolCalls {
		out.ToolCalls = append(out.ToolCalls, toolCallBrief{Name: tc.Name, Args: tc.Args, Result: tc.Result})
	}
	writeJSON(w, okStatus, out)
}

type confirmRequest struct {
	Approve bool   `json:"approve"`
	Comment string `json:"comment,omitempty"`
}

// confirm records the human.confirm decision for a paused workflow run —
// {id} is the workflow run id produced when the run paused. Approval puts the
// run back to pending and re-enqueues it for atlas-worker: this process only
// has the built-in registry, so post-gate nodes MUST execute on the worker.
func (h *agentRunHandler) confirm(w http.ResponseWriter, r *http.Request) {
	actor, ok := actorFrom(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "unauthorized", "no verified actor")
		return
	}
	if h.deps.Runtime == nil || h.deps.Runner == nil {
		writeError(w, http.StatusServiceUnavailable, "runtime_unavailable", "workflow runtime/queue not configured")
		return
	}
	runID := chi.URLParam(r, "id")
	var req confirmRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	status, err := h.deps.Runtime.Resume(r.Context(), runID, actor.Ticket.EnterpriseID, req.Approve, req.Comment)
	if err != nil {
		if errors.Is(err, workflow.ErrRunForbidden) {
			writeError(w, http.StatusForbidden, "forbidden", "run belongs to a different enterprise")
			return
		}
		writeError(w, http.StatusUnprocessableEntity, "resume_failed", err.Error())
		return
	}
	if status == workflow.RunPending {
		if err := h.deps.Runner.Enqueue(r.Context(), workflow.JobTypeRun, runID); err != nil {
			// The decision is recorded but execution is not scheduled — surface
			// loudly so the operator retries; silent success would strand the run.
			writeError(w, http.StatusInternalServerError, "enqueue_failed", err.Error())
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"run_id": runID, "status": status})
}
