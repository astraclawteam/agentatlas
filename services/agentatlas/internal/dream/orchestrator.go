package dream

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	sdkdream "github.com/astraclawteam/agentatlas/sdk/go/dream"
	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/workflow"
)

var ErrDreamWorkflowPaused = errors.New("Dream workflow is waiting for human confirmation")

const (
	maxDreamSignals        = 500
	maxDreamDisplayRunes   = 10000
	maxDreamRetrievalRunes = 20000
	maxDreamSealedRunes    = 100000
)

type WorkflowInput struct {
	OrgUnitID       string          `json:"org_unit_id"`
	WindowStart     time.Time       `json:"window_start"`
	WindowEnd       time.Time       `json:"window_end"`
	Inputs          []ResolvedInput `json:"inputs"`
	Coverage        Coverage        `json:"coverage"`
	Missing         []MissingInput  `json:"missing"`
	RiskSignalRules []string        `json:"risk_signal_rules"`
}

type StructuredSignal = sdkdream.StructuredSignal

type DreamOutput struct {
	Display      string             `json:"display"`
	Retrieval    string             `json:"retrieval"`
	SealedDetail string             `json:"sealed_detail"`
	Facts        []StructuredSignal `json:"facts"`
	Themes       []StructuredSignal `json:"themes"`
	Trends       []StructuredSignal `json:"trends"`
	Risks        []StructuredSignal `json:"risks"`
	Todos        []StructuredSignal `json:"todos"`
	Source       string             `json:"source"`
	ModelRoute   string             `json:"model_route"`
	ModelVersion string             `json:"model_version"`
}

type DreamExecution struct {
	EnterpriseID  string
	DreamRunID    string
	PolicyID      string
	PolicyVersion int32
	Workflow      sdkdream.WorkflowRef
	Input         WorkflowInput
}

type ExecutionResult struct {
	WorkflowRunID string
	Status        string
	Output        DreamOutput
}

type publishedRuntime interface {
	RunPublished(context.Context, string, string, int32, map[string]any) (workflow.RunResult, error)
}

type Orchestrator struct{ runtime publishedRuntime }

func NewOrchestrator(runtime publishedRuntime) *Orchestrator { return &Orchestrator{runtime: runtime} }

func (o *Orchestrator) Run(ctx context.Context, execution DreamExecution) (ExecutionResult, error) {
	if o == nil || o.runtime == nil {
		return ExecutionResult{}, fmt.Errorf("Dream orchestrator requires workflow runtime")
	}
	if execution.EnterpriseID == "" || execution.PolicyID == "" || execution.PolicyVersion < 1 {
		return ExecutionResult{}, fmt.Errorf("Dream execution enterprise and pinned policy are required")
	}
	if execution.Workflow.ID == "" || execution.Workflow.Version < 1 {
		return ExecutionResult{}, fmt.Errorf("Dream execution requires a pinned published workflow")
	}
	if err := validateWorkflowInput(execution.Input); err != nil {
		return ExecutionResult{}, err
	}
	input, err := workflowInputMap(execution)
	if err != nil {
		return ExecutionResult{}, err
	}
	result, err := o.runtime.RunPublished(ctx, execution.EnterpriseID, execution.Workflow.ID, execution.Workflow.Version, input)
	out := ExecutionResult{WorkflowRunID: result.RunID, Status: result.Status}
	if err != nil {
		return out, fmt.Errorf("run Dream workflow %s@%d: %w", execution.Workflow.ID, execution.Workflow.Version, err)
	}
	if result.Status == workflow.RunWaitingConfirmation {
		return out, nil
	}
	if result.Status != workflow.RunSucceeded {
		return out, fmt.Errorf("Dream workflow %s finished with status %q", result.RunID, result.Status)
	}
	dreamOut, err := decodeDreamOutput(result.Outputs)
	if err != nil {
		return out, err
	}
	out.Output = dreamOut
	return out, nil
}

func workflowInputMap(execution DreamExecution) (map[string]any, error) {
	raw, err := json.Marshal(execution.Input)
	if err != nil {
		return nil, fmt.Errorf("encode Dream workflow input: %w", err)
	}
	var input map[string]any
	if err := json.Unmarshal(raw, &input); err != nil {
		return nil, fmt.Errorf("decode Dream workflow input: %w", err)
	}
	input["dream_run_id"] = execution.DreamRunID
	input["dream_policy_id"] = execution.PolicyID
	input["dream_policy_version"] = execution.PolicyVersion
	input["workflow_id"] = execution.Workflow.ID
	input["workflow_version"] = execution.Workflow.Version
	evidence := make([]string, 0, len(execution.Input.Inputs))
	parents := make([]string, 0, len(execution.Input.Inputs))
	for _, item := range execution.Input.Inputs {
		if item.EvidencePointerID != "" {
			evidence = append(evidence, item.EvidencePointerID)
		}
		if item.ParentRunID != "" {
			parents = append(parents, item.ParentRunID)
		}
	}
	input["evidence_pointer_ids"] = evidence
	input["parent_dream_run_ids"] = parents
	return input, nil
}

func validateWorkflowInput(input WorkflowInput) error {
	if strings.TrimSpace(input.OrgUnitID) == "" || input.WindowStart.IsZero() || input.WindowEnd.IsZero() || !input.WindowEnd.After(input.WindowStart) {
		return fmt.Errorf("Dream workflow input requires org unit and increasing window")
	}
	if len(input.Inputs) > maxResolvedInputs || len(input.Missing) > maxResolvedInputs {
		return fmt.Errorf("Dream workflow input exceeds bound %d", maxResolvedInputs)
	}
	if len(input.RiskSignalRules) > 100 {
		return fmt.Errorf("Dream workflow risk rules exceed bound 100")
	}
	for _, rule := range input.RiskSignalRules {
		if strings.TrimSpace(rule) == "" || len([]rune(rule)) > 256 {
			return fmt.Errorf("Dream workflow input contains invalid risk rule")
		}
	}
	if input.Coverage.ExpectedChildren < 0 || input.Coverage.CompletedChildren < 0 || input.Coverage.InputCount < 0 || input.Coverage.CompletedChildren > input.Coverage.ExpectedChildren || input.Coverage.InputCount != len(input.Inputs) {
		return fmt.Errorf("Dream workflow input has invalid coverage")
	}
	for _, item := range input.Inputs {
		if item.SourceType == "" || item.SourceID == "" || item.OrgUnitID == "" || item.SanitizedText == "" || len([]rune(item.SanitizedText)) > maxResolvedTextRunes || len(item.Visibility) == 0 || len(item.Visibility) > maxVisibilityEntries {
			return fmt.Errorf("Dream workflow input contains invalid or unbounded resolved input %q", item.SourceID)
		}
	}
	for _, item := range input.Missing {
		if item.SourceType == "" || item.SourceID == "" || !validMissingReason(item.Reason) {
			return fmt.Errorf("Dream workflow input contains invalid missing input")
		}
	}
	return nil
}

func decodeDreamOutput(outputs map[string]map[string]any) (DreamOutput, error) {
	var candidates []DreamOutput
	for _, nodeOut := range outputs {
		if _, ok := nodeOut["display"]; !ok {
			continue
		}
		raw, err := json.Marshal(nodeOut)
		if err != nil {
			return DreamOutput{}, fmt.Errorf("encode Dream workflow output: %w", err)
		}
		var out DreamOutput
		decoder := json.NewDecoder(bytes.NewReader(raw))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&out); err != nil {
			return DreamOutput{}, fmt.Errorf("malformed Dream workflow output: %w", err)
		}
		if err := validateDreamOutput(out); err != nil {
			return DreamOutput{}, err
		}
		candidates = append(candidates, out)
	}
	if len(candidates) != 1 {
		return DreamOutput{}, fmt.Errorf("Dream workflow must produce exactly one typed output, got %d", len(candidates))
	}
	return candidates[0], nil
}

func validateDreamOutput(out DreamOutput) error {
	if strings.TrimSpace(out.Display) == "" || strings.TrimSpace(out.Retrieval) == "" || strings.TrimSpace(out.SealedDetail) == "" || strings.TrimSpace(out.Source) == "" || strings.TrimSpace(out.ModelRoute) == "" || strings.TrimSpace(out.ModelVersion) == "" {
		return fmt.Errorf("Dream workflow output is missing required typed fields")
	}
	if len([]rune(out.Display)) > maxDreamDisplayRunes || len([]rune(out.Retrieval)) > maxDreamRetrievalRunes || len([]rune(out.SealedDetail)) > maxDreamSealedRunes {
		return fmt.Errorf("Dream workflow output text exceeds bound")
	}
	for _, group := range [][]StructuredSignal{out.Facts, out.Themes, out.Trends, out.Risks, out.Todos} {
		if len(group) > maxDreamSignals {
			return fmt.Errorf("Dream workflow output signals exceed bound %d", maxDreamSignals)
		}
		for _, signal := range group {
			if signal.ID == "" || signal.Title == "" || signal.Detail == "" || signal.EvidencePointerID == "" || (signal.Severity != sdkdream.SignalSeverityInfo && signal.Severity != sdkdream.SignalSeverityWarning && signal.Severity != sdkdream.SignalSeverityCritical) || len([]rune(signal.Title)) > 200 || len([]rune(signal.Detail)) > 2000 {
				return fmt.Errorf("Dream workflow output contains malformed structured signal")
			}
		}
	}
	return nil
}
