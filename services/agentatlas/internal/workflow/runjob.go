package workflow

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	db "github.com/astraclawteam/agentatlas/services/agentatlas/db/generated"
	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/tasks"
)

const JobTypeRun = "workflow_run"

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
		return "", err
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

func (r *Runtime) ExecuteClaimed(ctx context.Context, runID string, executionOwner pgtype.Text) (string, error) {
	run, err := r.store.GetWorkflowRun(ctx, runID)
	if err != nil {
		return "", fmt.Errorf("load run %s: %w", runID, err)
	}
	if !run.ExecutionOwner.Valid || run.ExecutionOwner != executionOwner || !run.ExecutionLeaseExpiresAt.Valid || !run.ExecutionLeaseExpiresAt.Time.After(time.Now()) {
		return "", fmt.Errorf("workflow run %s execution ownership lost", runID)
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
	return r.execute(ctx, runID, run.EnterpriseID, def, order, state, executionOwner, run.StateRevision)
}

type RunJobHandler struct {
	runtime        *Runtime
	store          Store
	tasks          *tasks.Runner
	executionOwner pgtype.Text
}

func NewRunJobHandler(runtime *Runtime, store Store, taskRunner *tasks.Runner) *RunJobHandler {
	return &RunJobHandler{runtime: runtime, store: store, tasks: taskRunner,
		executionOwner: pgtype.Text{String: newID("workflow-worker"), Valid: true}}
}

func (h *RunJobHandler) RegisterJobHandler() error {
	return h.tasks.Register(JobTypeRun, tasks.Handler{
		Claim: func(ctx context.Context, runID string) (bool, error) {
			claimer, ok := h.store.(interface {
				ClaimWorkflowRunLease(context.Context, db.ClaimWorkflowRunLeaseParams) (db.WorkflowRun, error)
			})
			if !ok {
				return false, fmt.Errorf("workflow store cannot atomically claim runs")
			}
			_, err := claimer.ClaimWorkflowRunLease(ctx, db.ClaimWorkflowRunLeaseParams{ID: runID, ExecutionOwner: h.executionOwner})
			if errors.Is(err, pgx.ErrNoRows) {
				return false, nil
			}
			return err == nil, err
		},
		Execute: func(ctx context.Context, runID string) error {
			return h.executeWithLease(ctx, runID)
		},
		Complete: func(context.Context, string, error) error { return nil },
	})
}

func (h *RunJobHandler) executeWithLease(ctx context.Context, runID string) error {
	renewer, ok := h.store.(interface {
		RenewWorkflowRunLease(context.Context, db.RenewWorkflowRunLeaseParams) (int64, error)
	})
	if !ok {
		return fmt.Errorf("workflow store cannot renew execution leases")
	}
	leaseCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	done := make(chan struct{})
	renewed := make(chan error, 1)
	go func() {
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				renewed <- nil
				return
			case <-leaseCtx.Done():
				renewed <- leaseCtx.Err()
				return
			case <-ticker.C:
				rows, err := renewer.RenewWorkflowRunLease(leaseCtx, db.RenewWorkflowRunLeaseParams{ID: runID, ExecutionOwner: h.executionOwner})
				if err != nil || rows != 1 {
					if err == nil {
						err = fmt.Errorf("workflow execution lease ownership lost")
					}
					cancel()
					renewed <- err
					return
				}
			}
		}
	}()
	_, execErr := h.runtime.ExecuteClaimed(leaseCtx, runID, h.executionOwner)
	close(done)
	return errors.Join(execErr, <-renewed)
}

// RecoverPendingRuns makes the PostgreSQL pending state the durable workflow
// dispatch record. It recovers expired running leases and republishes pending
// approvals/idempotent redeliveries after process or NATS failures.
func (h *RunJobHandler) RecoverPendingRuns(ctx context.Context) error {
	store, ok := h.store.(interface {
		RecoverExpiredWorkflowRuns(context.Context, int32) ([]string, error)
		ListPendingWorkflowRuns(context.Context, int32) ([]string, error)
	})
	if !ok {
		return fmt.Errorf("workflow store cannot recover durable pending runs")
	}
	if _, err := store.RecoverExpiredWorkflowRuns(ctx, 100); err != nil {
		return fmt.Errorf("recover expired workflow runs: %w", err)
	}
	ids, err := store.ListPendingWorkflowRuns(ctx, 100)
	if err != nil {
		return fmt.Errorf("list pending workflow runs: %w", err)
	}
	var failures []error
	for _, id := range ids {
		if err := h.tasks.Enqueue(ctx, JobTypeRun, id); err != nil {
			failures = append(failures, fmt.Errorf("redispatch workflow %s: %w", id, err))
		}
	}
	return errors.Join(failures...)
}
