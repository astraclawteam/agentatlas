package dream

import (
	"context"
	"github.com/jackc/pgx/v5/pgtype"
	"strings"
	"testing"
	"time"

	sdkdream "github.com/astraclawteam/agentatlas/sdk/go/dream"
	db "github.com/astraclawteam/agentatlas/services/agentatlas/db/generated"
	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/workflow"
)

type recordingDreamRuntime struct {
	startedEnterprise string
	startedWorkflow   string
	startedVersion    int32
	input             map[string]any
	result            workflow.RunResult
	err               error
	verified          workflow.VerifiedDreamContext
	resumedRunID      string
}

func (r *recordingDreamRuntime) DreamResult(_ context.Context, runID string, verified workflow.VerifiedDreamContext) (workflow.RunResult, error) {
	r.resumedRunID = runID
	r.verified = verified
	if r.result.Input == nil {
		r.result.Input, _ = workflowInputMap(validDreamExecution())
		r.result.Dream = &workflow.VerifiedDreamContext{EnterpriseID: "ent-1", DreamRunID: verified.DreamRunID, PolicyID: verified.PolicyID, PolicyVersion: verified.PolicyVersion, WorkflowID: verified.WorkflowID, WorkflowVersion: verified.WorkflowVersion, OrgUnitID: "department:rd"}
	}
	return r.result, r.err
}

func TestOrchestratorReconcilesExactExistingWorkflowRun(t *testing.T) {
	runtime := &recordingDreamRuntime{result: workflow.RunResult{RunID: "wrun-pinned", Status: workflow.RunSucceeded, AggregateNodeID: "aggregate", Outputs: map[string]map[string]any{"aggregate": validDreamWorkflowOutput()}}}
	exec := validDreamExecution()
	exec.ExistingWorkflowRunID = "wrun-pinned"
	out, err := NewOrchestrator(runtime).Run(context.Background(), exec)
	if err != nil || runtime.resumedRunID != "wrun-pinned" || runtime.startedWorkflow != "" || out.WorkflowRunID != "wrun-pinned" {
		t.Fatalf("out=%+v runtime=%+v err=%v", out, runtime, err)
	}
}

func TestOrchestratorUsesRuntimeVerifiedAggregateNode(t *testing.T) {
	runtime := &recordingDreamRuntime{result: workflow.RunResult{RunID: "wrun", Status: workflow.RunSucceeded, AggregateNodeID: "aggregate", Outputs: map[string]map[string]any{"aggregate": validDreamWorkflowOutput(), "spoof": validDreamWorkflowOutput()}}}
	out, err := NewOrchestrator(runtime).Run(context.Background(), validDreamExecution())
	if err != nil || out.Output.Display != "display" {
		t.Fatalf("out=%+v err=%v", out, err)
	}
}

func (r *recordingDreamRuntime) RunDreamPublished(_ context.Context, enterpriseID, workflowID string, version int32, input map[string]any, verified workflow.VerifiedDreamContext) (workflow.RunResult, error) {
	r.startedEnterprise = enterpriseID
	r.startedWorkflow = workflowID
	r.startedVersion = version
	r.input = input
	r.verified = verified
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
		Outputs: map[string]map[string]any{"aggregate": validDreamWorkflowOutput()}, AggregateNodeID: "aggregate",
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
	if runtime.verified.PolicyID != "policy-1" || runtime.verified.WorkflowID != "wf-dream" {
		t.Fatalf("verified lineage missing: %+v", runtime.verified)
	}
}

func TestOrchestratorRejectsMalformedOrAmbiguousOutput(t *testing.T) {
	for name, outputs := range map[string]map[string]map[string]any{
		"missing":   {"other": {"value": "not dream output"}},
		"malformed": {"aggregate": {"display": "", "retrieval": "retrieval"}},
		"ambiguous": {"aggregate-a": validDreamWorkflowOutput(), "aggregate-b": validDreamWorkflowOutput()},
	} {
		t.Run(name, func(t *testing.T) {
			runtime := &recordingDreamRuntime{result: workflow.RunResult{RunID: "wrun", Status: workflow.RunSucceeded, Outputs: outputs, AggregateNodeID: "aggregate"}}
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

func TestDreamRunPolicySnapshotAgreementFailsClosed(t *testing.T) {
	p := validPolicy()
	run := db.DreamRun{ID: "dr", PolicyID: "policy", Version: 2, PolicyVersion: 2, EnterpriseID: "ent-1", OrgUnitID: p.OrgUnitID, Timezone: p.Timezone, WorkflowID: pgtype.Text{String: p.Workflow.ID, Valid: true}, WorkflowVersion: pgtype.Int4{Int32: p.Workflow.Version, Valid: true}, ModelRoute: "workflow/" + p.Workflow.ID, ModelVersion: "v3", WindowStart: pgtype.Timestamptz{Time: time.Now(), Valid: true}, WindowEnd: pgtype.Timestamptz{Time: time.Now().Add(time.Hour), Valid: true}, InputSnapshot: []byte(`{"source_counts":[{"source_type":"work_brief","count":0}],"sanitized_input_ids":[]}`), VisibilitySnapshot: []byte(`{"visibility_level":"members","org_unit_ids":["department:rd-1"],"masked_field_count":0}`)}
	if err := validateRunPolicySnapshot(run, "ent-1", p); err != nil {
		t.Fatal(err)
	}
	mutations := map[string]func(*db.DreamRun){
		"enterprise":     func(r *db.DreamRun) { r.EnterpriseID = "ent-2" },
		"legacy_version": func(r *db.DreamRun) { r.Version++ },
		"org":            func(r *db.DreamRun) { r.OrgUnitID = "department:other" },
		"timezone":       func(r *db.DreamRun) { r.Timezone = "UTC" },
		"route":          func(r *db.DreamRun) { r.ModelRoute = "forged" },
		"visibility": func(r *db.DreamRun) {
			r.VisibilitySnapshot = []byte(`{"visibility_level":"members","org_unit_ids":["other"],"masked_field_count":0}`)
		},
		"visibility_extra": func(r *db.DreamRun) {
			r.VisibilitySnapshot = []byte(`{"visibility_level":"members","org_unit_ids":["department:rd-1"],"masked_field_count":0,"extra":true}`)
		},
		"visibility_duplicate_org": func(r *db.DreamRun) {
			r.VisibilitySnapshot = []byte(`{"visibility_level":"members","org_unit_ids":["department:rd-1","department:rd-1"],"masked_field_count":0}`)
		},
		"visibility_additional_org": func(r *db.DreamRun) {
			r.VisibilitySnapshot = []byte(`{"visibility_level":"members","org_unit_ids":["department:rd-1","department:other"],"masked_field_count":0}`)
		},
		"visibility_masked_count": func(r *db.DreamRun) {
			r.VisibilitySnapshot = []byte(`{"visibility_level":"members","org_unit_ids":["department:rd-1"],"masked_field_count":1}`)
		},
		"input_null_counts": func(r *db.DreamRun) {
			r.InputSnapshot = []byte(`{"source_counts":null,"sanitized_input_ids":[]}`)
		},
		"input_count_mismatch": func(r *db.DreamRun) {
			r.InputSnapshot = []byte(`{"source_counts":[{"source_type":"work_brief","count":1}],"sanitized_input_ids":[]}`)
		},
		"input_nonzero_scheduler_snapshot": func(r *db.DreamRun) {
			r.InputSnapshot = []byte(`{"source_counts":[{"source_type":"work_brief","count":1}],"sanitized_input_ids":["brief-1"]}`)
		},
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			copy := run
			mutate(&copy)
			if validateRunPolicySnapshot(copy, "ent-1", p) == nil {
				t.Fatal("must reject")
			}
		})
	}
}

func TestPersistedDreamInputsMustExactlyMatchWorkflowInput(t *testing.T) {
	input := WorkflowInput{Inputs: []ResolvedInput{{SourceType: sdkdream.SourceWorkBrief, SourceID: "brief-1"}}}
	rows := []db.DreamInput{{RunID: "dr", SourceType: string(sdkdream.SourceWorkBrief), SourceID: "brief-1"}}
	if err := validatePersistedDreamInputs(input, rows); err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func([]db.DreamInput) []db.DreamInput{
		"missing": func([]db.DreamInput) []db.DreamInput { return nil },
		"extra": func(rows []db.DreamInput) []db.DreamInput {
			return append(rows, db.DreamInput{RunID: "dr", SourceType: string(sdkdream.SourceWorkBrief), SourceID: "brief-2"})
		},
		"type": func(rows []db.DreamInput) []db.DreamInput {
			rows[0].SourceType = string(sdkdream.SourceRiskEvent)
			return rows
		},
	} {
		t.Run(name, func(t *testing.T) {
			copyRows := append([]db.DreamInput(nil), rows...)
			if validatePersistedDreamInputs(input, mutate(copyRows)) == nil {
				t.Fatal("persisted input mismatch accepted")
			}
		})
	}
}
