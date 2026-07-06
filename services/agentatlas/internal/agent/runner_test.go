package agent

import (
	"context"
	"encoding/json"
	"iter"
	"strings"
	"testing"

	"google.golang.org/adk/model"
	"google.golang.org/genai"

	"github.com/astraclawteam/agentatlas/sdk/go/workflow"
)

// toolCallLLM is a deterministic model.LLM that first issues a draft_workflow
// tool call and, once it sees the tool response in the request history, emits
// the final assistant text — exercising one full agent loop round-trip.
type toolCallLLM struct{}

func (toolCallLLM) Name() string { return "test/tool-call" }

func (toolCallLLM) GenerateContent(_ context.Context, req *model.LLMRequest, _ bool) iter.Seq2[*model.LLMResponse, error] {
	sawToolResult := false
	for _, c := range req.Contents {
		for _, p := range c.Parts {
			if p.FunctionResponse != nil {
				sawToolResult = true
			}
		}
	}
	return func(yield func(*model.LLMResponse, error) bool) {
		if !sawToolResult {
			yield(&model.LLMResponse{
				Content: &genai.Content{Role: "model", Parts: []*genai.Part{{
					FunctionCall: &genai.FunctionCall{
						ID:   "call_wf_1",
						Name: "draft_workflow",
						Args: map[string]any{
							"workflow_id": "wf_sop_demo",
							"kind":        "sop",
							"risk_level":  "medium",
							"steps": []any{
								map[string]any{"id": "in", "type": "input.evidence_pointer", "name": "接收文档"},
								map[string]any{"id": "extract", "type": "transform.extract_sop", "name": "抽取 SOP"},
								map[string]any{"id": "confirm", "type": "human.confirm", "name": "管理员确认", "requires_confirmation": true},
							},
							"edges": []any{
								map[string]any{"from": "in", "to": "extract"},
								map[string]any{"from": "extract", "to": "confirm"},
							},
						},
					},
				}}},
				TurnComplete: true,
			}, nil)
			return
		}
		yield(&model.LLMResponse{
			Content:      genai.NewContentFromText("SOP 工作流草稿已生成，等待管理员确认。", "model"),
			TurnComplete: true,
		}, nil)
	}
}

func TestRunnerExecutesDraftWorkflowTool(t *testing.T) {
	r, err := NewRunner(toolCallLLM{})
	if err != nil {
		t.Fatalf("new runner: %v", err)
	}
	res, err := r.Run(context.Background(), "u_test", "s_test", "把这三步做成 SOP 工作流草稿")
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if len(res.ToolCalls) == 0 {
		t.Fatal("agent loop executed no tool calls")
	}
	if res.ToolCalls[0].Name != "draft_workflow" {
		t.Fatalf("ToolCalls[0].Name = %q, want draft_workflow", res.ToolCalls[0].Name)
	}
	if res.Text == "" {
		t.Fatal("final text empty")
	}
	if len(res.ToolCalls[0].Result) == 0 {
		t.Fatal("tool result not captured")
	}

	// The captured result must contain the schema-valid workflow the tool built.
	var direct DraftWorkflowResult
	if err := json.Unmarshal(res.ToolCalls[0].Result, &direct); err != nil {
		t.Fatalf("unmarshal tool result: %v (raw=%s)", err, res.ToolCalls[0].Result)
	}
	wf := direct.Workflow
	if wf.WorkflowID == "" {
		// Some functiontool versions wrap struct results under "output".
		var wrapped struct {
			Output DraftWorkflowResult `json:"output"`
		}
		if err := json.Unmarshal(res.ToolCalls[0].Result, &wrapped); err != nil {
			t.Fatalf("unmarshal wrapped tool result: %v (raw=%s)", err, res.ToolCalls[0].Result)
		}
		wf = wrapped.Output.Workflow
	}
	if wf.WorkflowID != "wf_sop_demo" || wf.Kind != workflow.KindSOP || len(wf.Nodes) != 3 {
		t.Fatalf("workflow from tool result = %+v (raw=%s)", wf, res.ToolCalls[0].Result)
	}
	if wf.Version != 0 {
		t.Fatalf("draft must stay unpublished, version = %d", wf.Version)
	}
	for _, n := range wf.Nodes {
		if !workflow.IsBuiltinNodeType(n.Type) {
			t.Fatalf("non-builtin node type leaked: %s", n.Type)
		}
	}
}

// loopingLLM always demands another tool call — the runner guard must fail
// loud instead of spinning forever.
type loopingLLM struct{}

func (loopingLLM) Name() string { return "test/looping" }

func (loopingLLM) GenerateContent(_ context.Context, _ *model.LLMRequest, _ bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		yield(&model.LLMResponse{
			Content: &genai.Content{Role: "model", Parts: []*genai.Part{{
				FunctionCall: &genai.FunctionCall{
					Name: "draft_retrieval_plan",
					Args: map[string]any{"query": "loop"},
				},
			}}},
			TurnComplete: true,
		}, nil)
	}
}

func TestRunnerFailsLoudOnToolLoop(t *testing.T) {
	r, err := NewRunner(loopingLLM{})
	if err != nil {
		t.Fatalf("new runner: %v", err)
	}
	_, err = r.Run(context.Background(), "u_loop", "s_loop", "触发循环")
	if err == nil || !strings.Contains(err.Error(), "exceeded") {
		t.Fatalf("tool loop must fail loud with the event guard, got %v", err)
	}
}
