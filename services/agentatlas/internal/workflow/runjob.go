package workflow

import (
	"context"
	"encoding/json"
	"fmt"

	db "github.com/astraclawteam/agentatlas/services/agentatlas/db/generated"
	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/tasks"
)

// JobTypeRun is the async job type for executing workflow runs on the worker.
const JobTypeRun = "workflow_run"

// CreatePending registers a run bound to a published version WITHOUT executing
// it — the serving plane enqueues, atlas-worker claims and executes. The
// workflow must belong to the caller's enterprise (fail closed): version
// definitions are looked up by id+version only, so ownership is checked here.
func (r *Runtime) CreatePending(ctx context.Context, enterpriseID, workflowID string, version int32, input map[string]any) (string, error) {
	wf, err := r.store.GetWorkflow(ctx, workflowID)
	if err != nil {
		return "", fmt.Errorf("load workflow %s: %w", workflowID, err)
	}
	if wf.EnterpriseID != enterpriseID {
		return "", ErrWorkflowForbidden
	}
	def, err := r.service.VersionDefinition(ctx, workflowID, version)
	if err != nil {
		return "", err
	}
	if _, err := topoOrder(def); err != nil {
		return "", err // fail early: never enqueue an unexecutable definition
	}
	if input == nil {
		input = map[string]any{}
	}
	rawInput, err := json.Marshal(input)
	if err != nil {
		return "", fmt.Errorf("encode run input: %w", err)
	}
	runID := newID("run")
	if _, err := r.store.CreateWorkflowRun(ctx, db.CreateWorkflowRunParams{
		ID: runID, WorkflowID: workflowID, Version: version,
		EnterpriseID: enterpriseID, Status: RunPending, Input: rawInput,
	}); err != nil {
		return "", fmt.Errorf("create run: %w", err)
	}
	return runID, nil
}

// ExecuteClaimed runs a claimed (status=running) run. Fresh runs start at
// node 0; a run resumed after human.confirm approval carries its persisted
// state in workflow_runs.output (the Claim UPDATE keeps output via COALESCE)
// and continues from the node after the confirm gate.
func (r *Runtime) ExecuteClaimed(ctx context.Context, runID string) (string, error) {
	run, err := r.store.GetWorkflowRun(ctx, runID)
	if err != nil {
		return "", fmt.Errorf("load run %s: %w", runID, err)
	}
	def, err := r.service.VersionDefinition(ctx, run.WorkflowID, run.Version)
	if err != nil {
		return "", err
	}
	order, err := topoOrder(def)
	if err != nil {
		return "", err
	}
	input := map[string]any{}
	if len(run.Input) > 0 {
		if err := json.Unmarshal(run.Input, &input); err != nil {
			return "", fmt.Errorf("decode run input: %w", err)
		}
	}
	state := runState{NextIndex: 0, Outputs: map[string]map[string]any{}, Input: input}
	if len(run.Output) > 0 {
		var saved runState
		if err := json.Unmarshal(run.Output, &saved); err != nil {
			return "", fmt.Errorf("decode persisted run state %s: %w", runID, err)
		}
		if saved.Outputs != nil {
			saved.Input = input
			state = saved
		}
	}
	return r.execute(ctx, runID, run.EnterpriseID, def, order, state)
}

// RunJobHandler consumes workflow-run jobs on the worker's task runner
// (worker.Deps.Extra registrar).
type RunJobHandler struct {
	runtime *Runtime
	store   Store
	tasks   *tasks.Runner
}

func NewRunJobHandler(runtime *Runtime, store Store, taskRunner *tasks.Runner) *RunJobHandler {
	return &RunJobHandler{runtime: runtime, store: store, tasks: taskRunner}
}

func (h *RunJobHandler) RegisterJobHandler() error {
	return h.tasks.Register(JobTypeRun, tasks.Handler{
		Claim: func(ctx context.Context, runID string) (bool, error) {
			run, err := h.store.GetWorkflowRun(ctx, runID)
			if err != nil {
				return false, err
			}
			if run.Status != RunPending {
				return false, nil // already claimed / finished — idempotent redelivery
			}
			rows, err := h.store.UpdateWorkflowRunStatus(ctx, db.UpdateWorkflowRunStatusParams{
				ID: runID, Status: RunRunning, Output: nil,
			})
			return rows > 0, err
		},
		Execute: func(ctx context.Context, runID string) error {
			// execute persists the terminal status (succeeded / failed /
			// waiting_confirmation) itself; a pause is not an error.
			_, err := h.runtime.ExecuteClaimed(ctx, runID)
			return err
		},
		Complete: func(ctx context.Context, runID string, execErr error) error {
			// Terminal statuses are already persisted by the runtime; nothing
			// to reconcile here. Errors were recorded as failed run events.
			return nil
		},
	})
}
