package agent

import (
	"context"
	"iter"
	"strings"
	"testing"

	"google.golang.org/adk/model"
	"google.golang.org/genai"

	"github.com/astraclawteam/agentatlas/sdk/go/workflow"
)

func sopDoc() SOPDocument {
	return SOPDocument{
		Title:   "MES 异常工单排查",
		Summary: "确认工单编号、排查异常原因、管理员确认后归档。",
		Sections: []string{
			"第一步：确认工单编号并核对产线。",
			"第二步：排查异常原因，接口限流风险需上报。",
			"第三步：管理员确认后归档。",
		},
	}
}

func TestExtractSOPProducesSchemaValidDraft(t *testing.T) {
	// toolCallLLM (runner_test.go) emits a draft_workflow call for a 3-step SOP
	// with a human.confirm node, then the final text.
	r, err := NewRunner(toolCallLLM{})
	if err != nil {
		t.Fatalf("new runner: %v", err)
	}
	wf, err := ExtractSOP(context.Background(), r, sopDoc())
	if err != nil {
		t.Fatalf("extract sop: %v", err)
	}
	if wf.Kind != workflow.KindSOP || wf.Version != 0 {
		t.Fatalf("draft = kind %q version %d", wf.Kind, wf.Version)
	}
	if len(wf.Nodes) < 3 {
		t.Fatalf("nodes = %d, want >=3", len(wf.Nodes))
	}
	var confirm bool
	for _, n := range wf.Nodes {
		if !workflow.IsBuiltinNodeType(n.Type) {
			t.Fatalf("non-builtin node type: %s", n.Type)
		}
		if n.Type == workflow.NodeHumanConfirm {
			confirm = true
		}
	}
	if !confirm {
		t.Fatal("sop draft missing human.confirm node")
	}
}

// textOnlyLLM never calls a tool — extraction must fail loud.
type textOnlyLLM struct{}

func (textOnlyLLM) Name() string { return "test/text-only" }

func (textOnlyLLM) GenerateContent(_ context.Context, _ *model.LLMRequest, _ bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		yield(&model.LLMResponse{
			Content:      genai.NewContentFromText("我觉得这个文档挺好的。", "model"),
			TurnComplete: true,
		}, nil)
	}
}

func TestExtractSOPFailsLoudWithoutToolCall(t *testing.T) {
	r, err := NewRunner(textOnlyLLM{})
	if err != nil {
		t.Fatalf("new runner: %v", err)
	}
	_, err = ExtractSOP(context.Background(), r, sopDoc())
	if err == nil || !strings.Contains(err.Error(), "draft_workflow") {
		t.Fatalf("must fail loud when the agent skips draft_workflow, got %v", err)
	}
}

// wrongKindLLM drafts an ingestion workflow instead of a SOP.
type wrongKindLLM struct{}

func (wrongKindLLM) Name() string { return "test/wrong-kind" }

func (wrongKindLLM) GenerateContent(_ context.Context, req *model.LLMRequest, _ bool) iter.Seq2[*model.LLMResponse, error] {
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
						ID:   "call_bad",
						Name: "draft_workflow",
						Args: map[string]any{
							"workflow_id": "wf_bad", "kind": "ingestion", "risk_level": "low",
							"steps": []any{map[string]any{"id": "a", "type": "input.manual"}},
							"edges": []any{},
						},
					},
				}}},
				TurnComplete: true,
			}, nil)
			return
		}
		yield(&model.LLMResponse{
			Content:      genai.NewContentFromText("完成。", "model"),
			TurnComplete: true,
		}, nil)
	}
}

func TestExtractSOPRejectsNonSOPKind(t *testing.T) {
	r, err := NewRunner(wrongKindLLM{})
	if err != nil {
		t.Fatalf("new runner: %v", err)
	}
	_, err = ExtractSOP(context.Background(), r, sopDoc())
	if err == nil || !strings.Contains(err.Error(), "want sop") {
		t.Fatalf("non-sop kind must fail loud, got %v", err)
	}
}
