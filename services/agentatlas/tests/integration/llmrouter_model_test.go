// Integration test against a real OpenAI-compatible gateway (llmrouter).
// Run:
//
//	LLMROUTER_BASE_URL=https://.../v1 LLMROUTER_API_KEY=sk-... LLMROUTER_MODEL=deepseek-v4-flash \
//	  go test ./tests/integration -run TestLLMRouterModel
package integration

import (
	"context"
	"os"
	"testing"
	"time"

	adkmodel "google.golang.org/adk/model"
	"google.golang.org/genai"

	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/llmroutermodel"
)

func TestLLMRouterModel(t *testing.T) {
	baseURL := os.Getenv("LLMROUTER_BASE_URL")
	apiKey := os.Getenv("LLMROUTER_API_KEY")
	if baseURL == "" || apiKey == "" {
		t.Skip("set LLMROUTER_BASE_URL and LLMROUTER_API_KEY")
	}
	modelName := os.Getenv("LLMROUTER_MODEL")
	if modelName == "" {
		modelName = "deepseek-v4-flash"
	}

	m, err := llmroutermodel.New(llmroutermodel.Config{
		BaseURL: baseURL, APIKey: apiKey, DefaultModel: modelName, Timeout: 90 * time.Second,
	})
	if err != nil {
		t.Fatalf("new model: %v", err)
	}
	ctx := context.Background()

	// 1) plain completion
	req := &adkmodel.LLMRequest{
		Contents: []*genai.Content{
			{Role: "user", Parts: []*genai.Part{genai.NewPartFromText("用一句话回答：AgentAtlas 是什么？")}},
		},
	}
	var text string
	for resp, err := range m.GenerateContent(ctx, req, false) {
		if err != nil {
			t.Fatalf("complete: %v", err)
		}
		if resp.Content != nil {
			for _, p := range resp.Content.Parts {
				text += p.Text
			}
		}
	}
	if text == "" {
		t.Fatal("empty completion")
	}
	t.Logf("completion: %s", text)

	// 2) tool call event through streaming
	toolReq := &adkmodel.LLMRequest{
		Config: &genai.GenerateContentConfig{
			SystemInstruction: &genai.Content{Parts: []*genai.Part{genai.NewPartFromText(
				"你必须调用 report_answer 工具提交答案，不要直接输出文本。")}},
			Tools: []*genai.Tool{{FunctionDeclarations: []*genai.FunctionDeclaration{{
				Name:        "report_answer",
				Description: "提交最终答案",
				Parameters: &genai.Schema{
					Type:       genai.TypeObject,
					Properties: map[string]*genai.Schema{"answer": {Type: genai.TypeString}},
					Required:   []string{"answer"},
				},
			}}}},
		},
		Contents: []*genai.Content{
			{Role: "user", Parts: []*genai.Part{genai.NewPartFromText("1+1 等于几？调用工具提交。")}},
		},
	}
	var sawCall bool
	for resp, err := range m.GenerateContent(ctx, toolReq, true) {
		if err != nil {
			t.Fatalf("stream: %v", err)
		}
		if resp.Partial || resp.Content == nil {
			continue
		}
		for _, p := range resp.Content.Parts {
			if p.FunctionCall != nil {
				sawCall = true
				if p.FunctionCall.ID == "" || p.FunctionCall.Name == "" {
					t.Fatalf("tool call identity incomplete: %+v", p.FunctionCall)
				}
				t.Logf("tool call: %s(%v) id=%s", p.FunctionCall.Name, p.FunctionCall.Args, p.FunctionCall.ID)
			}
		}
	}
	if !sawCall {
		t.Fatal("expected a streamed tool call event with complete identity")
	}
}
