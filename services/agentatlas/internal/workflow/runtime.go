package workflow

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"go.opentelemetry.io/otel/attribute"

	sdkworkflow "github.com/astraclawteam/agentatlas/sdk/go/workflow"
	db "github.com/astraclawteam/agentatlas/services/agentatlas/db/generated"
	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/observability"
)

// Runtime executes published workflow versions as a persisted state machine.
// Runs pause at human.confirm nodes and resume on an explicit decision.
type Runtime struct {
	store    Store
	service  *Service
	registry Registry
	metrics  *observability.Metrics
}

func NewRuntime(store Store, service *Service, registry Registry) *Runtime {
	return &Runtime{store: store, service: service, registry: registry}
}

// SetMetrics wires the optional Prometheus surface (WorkflowRuns by terminal
// status).
func (r *Runtime) SetMetrics(m *observability.Metrics) { r.metrics = m }

// runState is persisted in workflow_runs.output while a run is in flight.
type runState struct {
	NextIndex int                       `json:"next_index"`
	Outputs   map[string]map[string]any `json:"outputs"`
	Input     map[string]any            `json:"input"`
}

// StartRun creates a run bound to a published version and executes until
// completion, failure, or a human.confirm pause.
func (r *Runtime) StartRun(ctx context.Context, enterpriseID, workflowID string, version int32, input map[string]any) (string, string, error) {
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
	if _, err := r.store.CreateWorkflowRun(ctx, db.CreateWorkflowRunParams{
		ID: runID, WorkflowID: workflowID, Version: version,
		EnterpriseID: enterpriseID, Status: RunRunning, Input: rawInput,
	}); err != nil {
		return "", "", fmt.Errorf("create run: %w", err)
	}

	state := runState{NextIndex: 0, Outputs: map[string]map[string]any{}, Input: input}
	status, err := r.execute(ctx, runID, enterpriseID, def, order, state)
	return runID, status, err
}

// Resume continues a run paused at human.confirm. approve=false cancels it.
func (r *Runtime) Resume(ctx context.Context, runID string, approve bool, comment string) (string, error) {
	run, err := r.store.GetWorkflowRun(ctx, runID)
	if err != nil {
		return "", fmt.Errorf("load run %s: %w", runID, err)
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
		if err := r.setRunStatus(ctx, runID, RunCancelled, nil); err != nil {
			return "", err
		}
		return RunCancelled, nil
	}

	if err := writeEvent(ctx, r.store, runID, confirmNode.ID, NodeSucceeded, map[string]any{"decision": "approve", "comment": comment}); err != nil {
		return "", err
	}
	state.Outputs[confirmNode.ID] = map[string]any{"approved": true}
	state.NextIndex++
	if err := r.setRunStatus(ctx, runID, RunRunning, &state); err != nil {
		return "", err
	}
	return r.execute(ctx, runID, run.EnterpriseID, def, order, state)
}

func (r *Runtime) execute(ctx context.Context, runID, enterpriseID string, def Definition, order []sdkworkflow.Node, state runState) (string, error) {
	ctx, span := observability.Tracer("workflow").Start(ctx, "workflow.run")
	span.SetAttributes(attribute.String("run_id", runID), attribute.String("enterprise_id", enterpriseID), attribute.String("workflow_id", def.WorkflowID))
	defer span.End()

	runCtx := &RunContext{RunID: runID, EnterpriseID: enterpriseID, Input: state.Input, Outputs: state.Outputs}

	for state.NextIndex < len(order) {
		node := order[state.NextIndex]

		if node.Type == sdkworkflow.NodeHumanConfirm || node.RequiresConfirmation {
			if err := writeEvent(ctx, r.store, runID, node.ID, NodeWaitingConfirmation, nil); err != nil {
				return "", err
			}
			if err := r.setRunStatus(ctx, runID, RunWaitingConfirmation, &state); err != nil {
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
			if serr := r.setRunStatus(ctx, runID, RunFailed, &state); serr != nil {
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

	if err := r.setRunStatus(ctx, runID, RunSucceeded, &state); err != nil {
		return "", err
	}
	return RunSucceeded, nil
}

func (r *Runtime) setRunStatus(ctx context.Context, runID, status string, state *runState) error {
	if r.metrics != nil {
		switch status {
		case RunSucceeded, RunFailed, RunCancelled:
			r.metrics.WorkflowRuns.WithLabelValues(status).Inc()
		}
	}
	var raw []byte
	if state != nil {
		var err error
		raw, err = json.Marshal(state)
		if err != nil {
			return fmt.Errorf("encode run state: %w", err)
		}
	}
	rows, err := r.store.UpdateWorkflowRunStatus(ctx, db.UpdateWorkflowRunStatusParams{
		ID: runID, Status: status, Output: raw,
	})
	if err != nil {
		return fmt.Errorf("set run %s -> %s: %w", runID, status, err)
	}
	if rows == 0 {
		return fmt.Errorf("run %s not found", runID)
	}
	return nil
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
