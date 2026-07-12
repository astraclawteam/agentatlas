package workflow

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5/pgtype"
	"go.opentelemetry.io/otel/attribute"

	sdkworkflow "github.com/astraclawteam/agentatlas/sdk/go/workflow"
	db "github.com/astraclawteam/agentatlas/services/agentatlas/db/generated"
	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/observability"
)

// Runtime executes published workflow versions as a persisted state machine.
// Runs pause at human.confirm nodes and resume on an explicit decision.
type Runtime struct {
	store          Store
	service        *Service
	registry       Registry
	metrics        *observability.Metrics
	dreamLifecycle func(context.Context, RunResult) error
}

type dreamWorkflowStore interface {
	CreateBoundDreamWorkflowRun(context.Context, db.CreateBoundDreamWorkflowRunParams) (db.CreateBoundDreamWorkflowRunRow, error)
	TransitionDreamWorkflowRun(context.Context, db.TransitionDreamWorkflowRunParams) (int64, error)
}

// ErrRunForbidden marks a run operation attempted by an actor outside the
// run's enterprise — callers map it to 403/404, never execute.
var ErrRunForbidden = errors.New("run belongs to a different enterprise")

// ErrWorkflowForbidden marks a workflow operation attempted by an actor
// outside the workflow's enterprise.
var ErrWorkflowForbidden = errors.New("workflow belongs to a different enterprise")

func NewRuntime(store Store, service *Service, registry Registry) *Runtime {
	return &Runtime{store: store, service: service, registry: registry}
}

// SetMetrics wires the optional Prometheus surface (WorkflowRuns by terminal
// status).
func (r *Runtime) SetMetrics(m *observability.Metrics) { r.metrics = m }
func (r *Runtime) SetDreamLifecycleHook(h func(context.Context, RunResult) error) {
	r.dreamLifecycle = h
}

// runState is persisted in workflow_runs.output while a run is in flight.
type runState struct {
	NextIndex int                       `json:"next_index"`
	Outputs   map[string]map[string]any `json:"outputs"`
	Input     map[string]any            `json:"input"`
	Dream     *VerifiedDreamContext     `json:"dream,omitempty"`
}

// RunResult is the bounded execution surface consumed by typed orchestrators.
// Outputs are the persisted outputs of the exact published workflow version.
type RunResult struct {
	RunID           string
	Status          string
	Outputs         map[string]map[string]any
	AggregateNodeID string
	EnterpriseID    string
	Input           map[string]any
	Dream           *VerifiedDreamContext
	Error           string
}

// RunPublished executes one exact immutable workflow version and returns its
// node outputs. It never substitutes the latest version.
func (r *Runtime) RunPublished(ctx context.Context, enterpriseID, workflowID string, version int32, input map[string]any) (RunResult, error) {
	return r.runPublished(ctx, enterpriseID, workflowID, version, input, nil)
}

func (r *Runtime) RunDreamPublished(ctx context.Context, enterpriseID, workflowID string, version int32, input map[string]any, dream VerifiedDreamContext) (RunResult, error) {
	if dream.EnterpriseID != enterpriseID || dream.DreamRunID == "" || dream.PolicyID == "" || dream.WorkflowID != workflowID || dream.WorkflowVersion != version {
		return RunResult{}, fmt.Errorf("invalid verified Dream context")
	}
	return r.runPublished(ctx, enterpriseID, workflowID, version, input, &dream)
}

func (r *Runtime) DreamResult(ctx context.Context, runID string, expected VerifiedDreamContext) (RunResult, error) {
	run, err := r.store.GetWorkflowRun(ctx, runID)
	if err != nil {
		return RunResult{}, fmt.Errorf("load Dream workflow run: %w", err)
	}
	var state runState
	if len(run.Output) > 0 {
		if err := json.Unmarshal(run.Output, &state); err != nil {
			return RunResult{}, fmt.Errorf("decode Dream workflow state: %w", err)
		}
	}
	if state.Dream == nil || state.Dream.EnterpriseID != expected.EnterpriseID || run.EnterpriseID != expected.EnterpriseID || state.Dream.DreamRunID != expected.DreamRunID || state.Dream.PolicyID != expected.PolicyID || state.Dream.PolicyVersion != expected.PolicyVersion || run.WorkflowID != expected.WorkflowID || run.Version != expected.WorkflowVersion {
		return RunResult{}, fmt.Errorf("workflow run is not bound to verified Dream context")
	}
	def, err := r.service.VersionDefinition(ctx, run.WorkflowID, run.Version)
	if err != nil {
		return RunResult{}, err
	}
	return RunResult{RunID: run.ID, Status: run.Status, EnterpriseID: run.EnterpriseID, Outputs: state.Outputs, Input: state.Input, Dream: state.Dream, AggregateNodeID: aggregateNodeID(def)}, nil
}

func (r *Runtime) runPublished(ctx context.Context, enterpriseID, workflowID string, version int32, input map[string]any, dream *VerifiedDreamContext) (RunResult, error) {
	wf, err := r.store.GetWorkflow(ctx, workflowID)
	if err != nil {
		return RunResult{}, fmt.Errorf("load workflow %s: %w", workflowID, err)
	}
	if wf.EnterpriseID != enterpriseID {
		return RunResult{}, ErrWorkflowForbidden
	}
	def, err := r.service.VersionDefinition(ctx, workflowID, version)
	if err != nil {
		return RunResult{}, err
	}
	runID, status, runErr := r.startRun(ctx, enterpriseID, workflowID, version, input, dream)
	result := RunResult{RunID: runID, Status: status, EnterpriseID: enterpriseID, Outputs: map[string]map[string]any{}, AggregateNodeID: aggregateNodeID(def)}
	if runID != "" {
		run, err := r.store.GetWorkflowRun(ctx, runID)
		if err != nil {
			return result, errors.Join(runErr, fmt.Errorf("load completed run %s: %w", runID, err))
		}
		if len(run.Output) > 0 {
			var state runState
			if err := json.Unmarshal(run.Output, &state); err != nil {
				return result, errors.Join(runErr, fmt.Errorf("decode completed run %s: %w", runID, err))
			}
			if state.Outputs != nil {
				result.Outputs = state.Outputs
			}
		}
	}
	return result, runErr
}

func aggregateNodeID(def Definition) string {
	var id string
	for _, node := range def.Nodes {
		if node.Type == sdkworkflow.NodeDreamAggregate {
			if id != "" {
				return ""
			}
			id = node.ID
		}
	}
	return id
}

// StartRun creates a run bound to a published version and executes until
// completion, failure, or a human.confirm pause.
func (r *Runtime) StartRun(ctx context.Context, enterpriseID, workflowID string, version int32, input map[string]any) (string, string, error) {
	return r.startRun(ctx, enterpriseID, workflowID, version, input, nil)
}

func (r *Runtime) startRun(ctx context.Context, enterpriseID, workflowID string, version int32, input map[string]any, dream *VerifiedDreamContext) (string, string, error) {
	def, err := r.service.VersionDefinition(ctx, workflowID, version)
	if err != nil {
		return "", "", err
	}
	order, err := topoOrder(def)
	if err != nil {
		return "", "", err
	}
	if input == nil {
		input = map[string]any{}
	}
	rawInput, err := json.Marshal(input)
	if err != nil {
		return "", "", fmt.Errorf("encode run input: %w", err)
	}
	runID := newID("run")
	state := runState{NextIndex: 0, Outputs: map[string]map[string]any{}, Input: input, Dream: dream}
	if dream == nil {
		if _, err := r.store.CreateWorkflowRun(ctx, db.CreateWorkflowRunParams{
			ID: runID, WorkflowID: workflowID, Version: version,
			EnterpriseID: enterpriseID, Status: RunRunning, Input: rawInput,
		}); err != nil {
			return "", "", fmt.Errorf("create run: %w", err)
		}
	} else {
		dreamStore, ok := r.store.(dreamWorkflowStore)
		if !ok {
			return "", "", fmt.Errorf("workflow store cannot atomically bind Dream runs")
		}
		rawState, err := json.Marshal(state)
		if err != nil {
			return "", "", fmt.Errorf("encode initial Dream workflow state: %w", err)
		}
		if _, err := dreamStore.CreateBoundDreamWorkflowRun(ctx, db.CreateBoundDreamWorkflowRunParams{
			DreamRunID: dream.DreamRunID, EnterpriseID: enterpriseID, PolicyID: dream.PolicyID,
			PolicyVersion: dream.PolicyVersion, OrgUnitID: dream.OrgUnitID,
			WorkflowID: pgtype.Text{String: workflowID, Valid: true}, Version: pgtype.Int4{Int32: version, Valid: true},
			ID: runID, Status: RunRunning, Input: rawInput, Output: rawState,
		}); err != nil {
			return runID, RunFailed, fmt.Errorf("create and bind Dream workflow run: %w", err)
		}
	}
	status, err := r.execute(ctx, runID, enterpriseID, def, order, state)
	return runID, status, err
}

// Resume records the human.confirm decision for a paused run. approve=false
// cancels it; approve=true advances past the confirm node and puts the run
// back to pending — the CALLER must re-enqueue it so atlas-worker (the only
// process with the fully wired registry) executes the remaining nodes.
// enterpriseID must match the run's enterprise (fail closed, ErrRunForbidden).
func (r *Runtime) Resume(ctx context.Context, runID, enterpriseID string, approve bool, comment string) (string, error) {
	run, err := r.store.GetWorkflowRun(ctx, runID)
	if err != nil {
		return "", fmt.Errorf("load run %s: %w", runID, err)
	}
	if run.EnterpriseID != enterpriseID {
		return "", ErrRunForbidden
	}
	if run.Status != RunWaitingConfirmation {
		return "", fmt.Errorf("run %s is %s, not waiting_confirmation", runID, run.Status)
	}
	var state runState
	if err := json.Unmarshal(run.Output, &state); err != nil {
		return "", fmt.Errorf("decode run state: %w", err)
	}
	def, err := r.service.VersionDefinition(ctx, run.WorkflowID, run.Version)
	if err != nil {
		return "", err
	}
	order, err := topoOrder(def)
	if err != nil {
		return "", err
	}
	if state.NextIndex >= len(order) {
		return "", fmt.Errorf("run %s paused position out of range", runID)
	}
	confirmNode := order[state.NextIndex]

	if !approve {
		if err := writeEvent(ctx, r.store, runID, confirmNode.ID, NodeFailed, map[string]any{"decision": "reject", "comment": comment}); err != nil {
			return "", err
		}
		if err := r.setRunStatus(ctx, runID, RunCancelled, &state, "human confirmation rejected"); err != nil {
			return "", err
		}
		return RunCancelled, nil
	}

	if err := writeEvent(ctx, r.store, runID, confirmNode.ID, NodeSucceeded, map[string]any{"decision": "approve", "comment": comment}); err != nil {
		return "", err
	}
	state.Outputs[confirmNode.ID] = map[string]any{"approved": true}
	state.NextIndex++
	if err := r.setRunStatus(ctx, runID, RunPending, &state, ""); err != nil {
		return "", err
	}
	return RunPending, nil
}

func (r *Runtime) execute(ctx context.Context, runID, enterpriseID string, def Definition, order []sdkworkflow.Node, state runState) (string, error) {
	ctx, span := observability.Tracer("workflow").Start(ctx, "workflow.run")
	span.SetAttributes(attribute.String("run_id", runID), attribute.String("enterprise_id", enterpriseID), attribute.String("workflow_id", def.WorkflowID))
	defer span.End()

	runCtx := &RunContext{RunID: runID, EnterpriseID: enterpriseID, Input: state.Input, Outputs: state.Outputs, Dream: state.Dream}

	for state.NextIndex < len(order) {
		node := order[state.NextIndex]

		if node.Type == sdkworkflow.NodeHumanConfirm || node.RequiresConfirmation {
			if err := writeEvent(ctx, r.store, runID, node.ID, NodeWaitingConfirmation, nil); err != nil {
				return "", err
			}
			if err := r.setRunStatus(ctx, runID, RunWaitingConfirmation, &state, ""); err != nil {
				return "", err
			}
			return RunWaitingConfirmation, nil
		}

		if err := writeEvent(ctx, r.store, runID, node.ID, NodeRunning, nil); err != nil {
			return "", err
		}
		exec, ok := r.registry[node.Type]
		if !ok {
			exec = notWired(node.Type)
		}
		out, err := exec.Execute(ctx, node, runCtx)
		if err != nil {
			_ = writeEvent(ctx, r.store, runID, node.ID, NodeFailed, map[string]any{"error": err.Error()})
			if serr := r.setRunStatus(ctx, runID, RunFailed, &state, err.Error()); serr != nil {
				return "", errors.Join(err, serr)
			}
			return RunFailed, err
		}
		if out == nil {
			out = map[string]any{}
		}
		state.Outputs[node.ID] = out
		if err := writeEvent(ctx, r.store, runID, node.ID, NodeSucceeded, nil); err != nil {
			return "", err
		}
		state.NextIndex++
	}

	if err := r.setRunStatus(ctx, runID, RunSucceeded, &state, ""); err != nil {
		return "", err
	}
	return RunSucceeded, nil
}

func (r *Runtime) setRunStatus(ctx context.Context, runID, status string, state *runState, lifecycleError string) error {
	var raw []byte
	if state != nil {
		var err error
		raw, err = json.Marshal(state)
		if err != nil {
			return fmt.Errorf("encode run state: %w", err)
		}
	}
	var rows int64
	var err error
	if state != nil && state.Dream != nil && status != RunPending && status != RunRunning {
		dreamStore, ok := r.store.(dreamWorkflowStore)
		if !ok {
			return fmt.Errorf("workflow store cannot atomically transition Dream runs")
		}
		rows, err = dreamStore.TransitionDreamWorkflowRun(ctx, db.TransitionDreamWorkflowRunParams{
			LifecycleError: truncateWorkflowError(lifecycleError), Status: status, RunOutput: raw,
			ID: runID, EnterpriseID: state.Dream.EnterpriseID,
		})
	} else {
		rows, err = r.store.UpdateWorkflowRunStatus(ctx, db.UpdateWorkflowRunStatusParams{
			ID: runID, Status: status, Output: raw,
		})
	}
	if err != nil {
		return fmt.Errorf("set run %s -> %s: %w", runID, status, err)
	}
	if rows == 0 {
		return fmt.Errorf("run %s not found", runID)
	}
	if r.metrics != nil {
		switch status {
		case RunSucceeded, RunFailed, RunCancelled:
			r.metrics.WorkflowRuns.WithLabelValues(status).Inc()
		}
	}
	if state != nil && state.Dream != nil && r.dreamLifecycle != nil && status != RunPending && status != RunRunning {
		_ = r.dreamLifecycle(ctx, RunResult{RunID: runID, EnterpriseID: state.Dream.EnterpriseID, Status: status, Dream: state.Dream, Error: lifecycleError})
	}
	return nil
}

func truncateWorkflowError(value string) string {
	runes := []rune(value)
	if len(runes) > 1000 {
		return string(runes[:1000])
	}
	return value
}

// topoOrder returns a deterministic topological order (Kahn's algorithm,
// ties broken by definition order). Cycles fail loud.
func topoOrder(def Definition) ([]sdkworkflow.Node, error) {
	indegree := map[string]int{}
	adjacency := map[string][]string{}
	byID := map[string]sdkworkflow.Node{}
	for _, n := range def.Nodes {
		byID[n.ID] = n
		indegree[n.ID] = 0
	}
	for _, e := range def.Edges {
		if _, ok := byID[e.From]; !ok {
			return nil, fmt.Errorf("edge from unknown node %q", e.From)
		}
		if _, ok := byID[e.To]; !ok {
			return nil, fmt.Errorf("edge to unknown node %q", e.To)
		}
		adjacency[e.From] = append(adjacency[e.From], e.To)
		indegree[e.To]++
	}

	var order []sdkworkflow.Node
	queue := make([]string, 0, len(def.Nodes))
	for _, n := range def.Nodes {
		if indegree[n.ID] == 0 {
			queue = append(queue, n.ID)
		}
	}
	for len(queue) > 0 {
		id := queue[0]
		queue = queue[1:]
		order = append(order, byID[id])
		for _, next := range adjacency[id] {
			indegree[next]--
			if indegree[next] == 0 {
				queue = append(queue, next)
			}
		}
	}
	if len(order) != len(def.Nodes) {
		return nil, fmt.Errorf("workflow %s contains a cycle", def.WorkflowID)
	}
	return order, nil
}
