package llmroutermodel

import (
	"strings"
	"testing"

	"google.golang.org/adk/model"
	"google.golang.org/genai"
)

func TestBuildChatRequestFullConversation(t *testing.T) {
	req := &model.LLMRequest{
		Model: "deepseek-v4",
		Config: &genai.GenerateContentConfig{
			SystemInstruction: &genai.Content{Parts: []*genai.Part{genai.NewPartFromText("你是知识中枢")}},
			Tools: []*genai.Tool{{
				FunctionDeclarations: []*genai.FunctionDeclaration{{
					Name:        "draft_workflow",
					Description: "生成工作流草稿",
					Parameters: &genai.Schema{
						Type: genai.TypeObject,
						Properties: map[string]*genai.Schema{
							"workflow_id": {Type: genai.TypeString},
						},
						Required: []string{"workflow_id"},
					},
				}},
			}},
		},
		Contents: []*genai.Content{
			{Role: "user", Parts: []*genai.Part{genai.NewPartFromText("把文档做成 SOP")}},
			{Role: "model", Parts: []*genai.Part{
				{FunctionCall: &genai.FunctionCall{ID: "call_1", Name: "draft_workflow", Args: map[string]any{"workflow_id": "wf_1"}}},
			}},
			{Role: "user", Parts: []*genai.Part{
				{FunctionResponse: &genai.FunctionResponse{ID: "call_1", Name: "draft_workflow", Response: map[string]any{"ok": true}}},
			}},
		},
	}

	out, err := buildChatRequest(req, "fallback")
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if out.Model != "deepseek-v4" {
		t.Fatalf("model = %q", out.Model)
	}
	if len(out.Messages) != 4 {
		t.Fatalf("messages = %d, want 4 (system,user,assistant,tool)", len(out.Messages))
	}
	if out.Messages[0].Role != "system" || out.Messages[2].Role != "assistant" {
		t.Fatalf("roles = %v", []string{out.Messages[0].Role, out.Messages[1].Role, out.Messages[2].Role, out.Messages[3].Role})
	}
	if len(out.Messages[2].ToolCalls) != 1 || out.Messages[2].ToolCalls[0].ID != "call_1" {
		t.Fatalf("assistant tool_calls = %+v", out.Messages[2].ToolCalls)
	}
	if out.Messages[3].Role != "tool" || out.Messages[3].ToolCallID != "call_1" {
		t.Fatalf("tool message = %+v", out.Messages[3])
	}
	if len(out.Tools) != 1 || out.Tools[0].Function.Name != "draft_workflow" {
		t.Fatalf("tools = %+v", out.Tools)
	}
	params := out.Tools[0].Function.Parameters
	if params["type"] != "object" {
		t.Fatalf("schema type = %v", params["type"])
	}
}

func TestBuildChatRequestSynthesizesMissingCallID(t *testing.T) {
	req := &model.LLMRequest{
		Contents: []*genai.Content{
			{Role: "model", Parts: []*genai.Part{
				{FunctionCall: &genai.FunctionCall{Name: "draft_workflow", Args: map[string]any{}}},
			}},
		},
	}
	out, err := buildChatRequest(req, "m")
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	id := out.Messages[0].ToolCalls[0].ID
	if id == "" || !strings.Contains(id, "draft_workflow") {
		t.Fatalf("missing id must be synthesized deterministically, got %q", id)
	}
}

func TestResponseToLLMParsesToolCalls(t *testing.T) {
	resp := &chatResponse{
		Model: "deepseek-v4",
		Choices: []chatChoice{{
			Message: &chatMessage{
				Role:    "assistant",
				Content: "",
				ToolCalls: []toolCall{{
					ID: "call_9", Type: "function",
					Function: functionCall{Name: "draft_retrieval_plan", Arguments: `{"query":"我的工作内容"}`},
				}},
			},
			FinishReason: "tool_calls",
		}},
		Usage: &chatUsage{PromptTokens: 10, CompletionTokens: 5, TotalTokens: 15},
	}
	out, err := responseToLLM(resp)
	if err != nil {
		t.Fatalf("map: %v", err)
	}
	if len(out.Content.Parts) != 1 || out.Content.Parts[0].FunctionCall == nil {
		t.Fatalf("parts = %+v", out.Content.Parts)
	}
	fc := out.Content.Parts[0].FunctionCall
	if fc.ID != "call_9" || fc.Name != "draft_retrieval_plan" || fc.Args["query"] != "我的工作内容" {
		t.Fatalf("function call = %+v", fc)
	}
	if out.UsageMetadata.TotalTokenCount != 15 {
		t.Fatalf("usage = %+v", out.UsageMetadata)
	}
	if !out.TurnComplete {
		t.Fatal("non-stream response must be TurnComplete")
	}
}

func TestResponseToLLMRejectsBadArguments(t *testing.T) {
	resp := &chatResponse{
		Choices: []chatChoice{{
			Message: &chatMessage{
				ToolCalls: []toolCall{{ID: "c", Function: functionCall{Name: "f", Arguments: "{broken"}}},
			},
		}},
	}
	if _, err := responseToLLM(resp); err == nil {
		t.Fatal("invalid tool arguments must fail loud")
	}
}

func intPtr(v int) *int { return &v }

func TestStreamAccumulatorToolCallIdentity(t *testing.T) {
	acc := newStreamAccumulator()
	chunks := []*chatResponse{
		{Model: "deepseek-v4", Choices: []chatChoice{{Delta: &chatMessage{Role: "assistant"}}}},
		{Choices: []chatChoice{{Delta: &chatMessage{ToolCalls: []toolCall{
			{Index: intPtr(0), ID: "call_s1", Function: functionCall{Name: "draft_workflow"}},
		}}}}},
		// llmrouter historical bug shape: later chunks carry ONLY argument
		// deltas, no id / name. The accumulator must still bind them.
		{Choices: []chatChoice{{Delta: &chatMessage{ToolCalls: []toolCall{
			{Index: intPtr(0), Function: functionCall{Arguments: `{"workflow_`}},
		}}}}},
		{Choices: []chatChoice{{Delta: &chatMessage{ToolCalls: []toolCall{
			{Index: intPtr(0), Function: functionCall{Arguments: `id":"wf_1"}`}},
		}}}}},
		{Choices: []chatChoice{{FinishReason: "tool_calls"}}, Usage: &chatUsage{TotalTokens: 7}},
	}
	for _, c := range chunks {
		if _, err := acc.add(c); err != nil {
			t.Fatalf("add: %v", err)
		}
	}
	final, err := acc.final()
	if err != nil {
		t.Fatalf("final: %v", err)
	}
	if len(final.Content.Parts) != 1 {
		t.Fatalf("parts = %+v", final.Content.Parts)
	}
	fc := final.Content.Parts[0].FunctionCall
	if fc == nil || fc.ID != "call_s1" || fc.Name != "draft_workflow" || fc.Args["workflow_id"] != "wf_1" {
		t.Fatalf("streamed call identity broken: %+v", fc)
	}
	if final.UsageMetadata.TotalTokenCount != 7 || final.ModelVersion != "deepseek-v4" {
		t.Fatalf("metadata: %+v %q", final.UsageMetadata, final.ModelVersion)
	}
}

func TestStreamAccumulatorNamelessCallFailsLoud(t *testing.T) {
	acc := newStreamAccumulator()
	_, _ = acc.add(&chatResponse{Choices: []chatChoice{{Delta: &chatMessage{ToolCalls: []toolCall{
		{Index: intPtr(0), Function: functionCall{Arguments: `{}`}},
	}}}}})
	if _, err := acc.final(); err == nil {
		t.Fatal("a tool call that never received a name must fail loud, not emit an orphan")
	}
}

func TestStreamAccumulatorTextDeltas(t *testing.T) {
	acc := newStreamAccumulator()
	for _, s := range []string{"你好", "，", "世界"} {
		delta, err := acc.add(&chatResponse{Choices: []chatChoice{{Delta: &chatMessage{Content: s}}}})
		if err != nil || delta != s {
			t.Fatalf("delta = %q err=%v", delta, err)
		}
	}
	final, err := acc.final()
	if err != nil {
		t.Fatalf("final: %v", err)
	}
	if got := final.Content.Parts[0].Text; got != "你好，世界" {
		t.Fatalf("text = %q", got)
	}
}

func TestFinishReasonMapping(t *testing.T) {
	cases := map[string]genai.FinishReason{
		"stop": genai.FinishReasonStop, "tool_calls": genai.FinishReasonStop,
		"length": genai.FinishReasonMaxTokens, "content_filter": genai.FinishReasonSafety,
	}
	for in, want := range cases {
		if got := finishReason(in); got != want {
			t.Fatalf("finishReason(%q) = %v, want %v", in, got, want)
		}
	}
}
