package dream

import (
	"context"
	"strings"
	"testing"
	"time"

	sdkdream "github.com/astraclawteam/agentatlas/sdk/go/dream"
	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/workflow"
)

type recordingDreamRuntime struct {
	startedEnterprise string
	startedWorkflow   string
	startedVersion    int32
	input             map[string]any
	result            workflow.RunResult
	err               error
}

func (r *recordingDreamRuntime) RunPublished(_ context.Context, enterpriseID, workflowID string, version int32, input map[string]any) (workflow.RunResult, error) {
	r.startedEnterprise = enterpriseID
	r.startedWorkflow = workflowID
	r.startedVersion = version
	r.input = input
	return r.result, r.err
}

func validDreamWorkflowOutput() map[string]any {
	return map[string]any{
		"display": "display", "retrieval": "retrieval", "sealed_detail": "sealed",
		"facts": []any{}, "themes": []any{}, "trends": []any{}, "risks": []any{}, "todos": []any{},
		"source": "deterministic", "model_route": "workflow/dream", "model_version": "v3",
	}
}

func TestOrchestratorRunsExactlyPinnedPublishedWorkflow(t *testing.T) {
	runtime := &recordingDreamRuntime{result: workflow.RunResult{
		RunID: "wrun-1", Status: workflow.RunSucceeded,
		Outputs: map[string]map[string]any{"aggregate": validDreamWorkflowOutput()},
	}}
	orchestrator := NewOrchestrator(runtime)
	start := time.Date(2026, 7, 11, 0, 0, 0, 0, time.UTC)
	out, err := orchestrator.Run(context.Background(), DreamExecution{
		EnterpriseID: "ent-1", PolicyID: "policy-1", PolicyVersion: 7,
		Workflow: sdkdream.WorkflowRef{ID: "wf-dream", Version: 3},
		Input: WorkflowInput{OrgUnitID: "department:rd", WindowStart: start, WindowEnd: start.Add(24 * time.Hour),
			Inputs:   []ResolvedInput{{SourceType: sdkdream.SourceWorkBrief, SourceID: "brief-1", OrgUnitID: "department:rd", SanitizedText: "sanitized", Visibility: []string{"managers"}}},
			Coverage: Coverage{InputCount: 1}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if runtime.startedEnterprise != "ent-1" || runtime.startedWorkflow != "wf-dream" || runtime.startedVersion != 3 {
		t.Fatalf("runtime binding = %s %s@%d", runtime.startedEnterprise, runtime.startedWorkflow, runtime.startedVersion)
	}
	if out.WorkflowRunID != "wrun-1" || out.Output.Display != "display" {
		t.Fatalf("out = %+v", out)
	}
	if runtime.input["dream_policy_id"] != "policy-1" || runtime.input["workflow_id"] != "wf-dream" {
		t.Fatalf("lineage input missing: %v", runtime.input)
	}
}

func TestOrchestratorRejectsMalformedOrAmbiguousOutput(t *testing.T) {
	for name, outputs := range map[string]map[string]map[string]any{
		"missing":   {"other": {"value": "not dream output"}},
		"malformed": {"aggregate": {"display": "", "retrieval": "retrieval"}},
		"ambiguous": {"aggregate-a": validDreamWorkflowOutput(), "aggregate-b": validDreamWorkflowOutput()},
	} {
		t.Run(name, func(t *testing.T) {
			runtime := &recordingDreamRuntime{result: workflow.RunResult{RunID: "wrun", Status: workflow.RunSucceeded, Outputs: outputs}}
			_, err := NewOrchestrator(runtime).Run(context.Background(), validDreamExecution())
			if err == nil {
				t.Fatal("invalid workflow output must fail closed")
			}
		})
	}
}

func TestOrchestratorPreservesHumanConfirmationPause(t *testing.T) {
	runtime := &recordingDreamRuntime{result: workflow.RunResult{RunID: "wrun-pause", Status: workflow.RunWaitingConfirmation}}
	out, err := NewOrchestrator(runtime).Run(context.Background(), validDreamExecution())
	if err != nil {
		t.Fatal(err)
	}
	if out.WorkflowRunID != "wrun-pause" || out.Status != workflow.RunWaitingConfirmation || out.Output.Display != "" {
		t.Fatalf("pause result = %+v", out)
	}
}

func TestOrchestratorRejectsUnboundedTypedInputBeforeRuntime(t *testing.T) {
	exec := validDreamExecution()
	exec.Input.Inputs = make([]ResolvedInput, maxResolvedInputs+1)
	runtime := &recordingDreamRuntime{}
	_, err := NewOrchestrator(runtime).Run(context.Background(), exec)
	if err == nil || !strings.Contains(err.Error(), "bound") {
		t.Fatalf("oversized input error = %v", err)
	}
	if runtime.startedWorkflow != "" {
		t.Fatal("invalid input reached workflow runtime")
	}
}

func TestOrchestratorEncodesTypedInputWithStableWireFields(t *testing.T) {
	runtime := &recordingDreamRuntime{result: workflow.RunResult{RunID: "wrun", Status: workflow.RunWaitingConfirmation}}
	exec := validDreamExecution()
	exec.Input.Inputs = []ResolvedInput{{SourceType: sdkdream.SourceWorkBrief, SourceID: "brief-1", OrgUnitID: "department:rd", SanitizedText: "safe", Visibility: []string{"managers"}}}
	exec.Input.Coverage = Coverage{InputCount: 1}
	exec.Input.RiskSignalRules = []string{"risk"}
	if _, err := NewOrchestrator(runtime).Run(context.Background(), exec); err != nil {
		t.Fatal(err)
	}
	items := runtime.input["inputs"].([]any)
	item := items[0].(map[string]any)
	coverage := runtime.input["coverage"].(map[string]any)
	if item["sanitized_text"] != "safe" || coverage["input_count"] != float64(1) || runtime.input["risk_signal_rules"].([]any)[0] != "risk" {
		t.Fatalf("unstable typed wire input: item=%v coverage=%v", item, coverage)
	}
}

func validDreamExecution() DreamExecution {
	start := time.Date(2026, 7, 11, 0, 0, 0, 0, time.UTC)
	return DreamExecution{EnterpriseID: "ent-1", PolicyID: "policy-1", PolicyVersion: 1,
		Workflow: sdkdream.WorkflowRef{ID: "wf-dream", Version: 3},
		Input:    WorkflowInput{OrgUnitID: "department:rd", WindowStart: start, WindowEnd: start.Add(time.Hour), Inputs: []ResolvedInput{}, Missing: []MissingInput{}}}
}
