// Integration test: a real llmrouter model drives the ADK Knowledge Agent
// loop and invokes at least one function tool. Run:
//
//	LLMROUTER_BASE_URL=https://.../v1 LLMROUTER_API_KEY=sk-... LLMROUTER_MODEL=deepseek-v4-flash \
//	  go test ./tests/integration -run TestAgentLoop
package integration

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/agent"
	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/llmroutermodel"
)

func TestAgentLoop(t *testing.T) {
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
		BaseURL: baseURL, APIKey: apiKey, DefaultModel: modelName, Timeout: 120 * time.Second,
	})
	if err != nil {
		t.Fatalf("new model: %v", err)
	}
	r, err := agent.NewRunner(m)
	if err != nil {
		t.Fatalf("new runner: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	res, err := r.Run(ctx, "u_it", "s_it",
		"请把下面三步整理成一个 SOP 工作流草稿（调用 draft_workflow，workflow_id 用 wf_it_demo，kind 用 sop）：1) 接收 MES 异常工单；2) 排查异常原因；3) 管理员确认后归档。")
	if err != nil {
		t.Fatalf("agent run: %v", err)
	}
	if len(res.ToolCalls) < 1 {
		t.Fatalf("expected >=1 tool call, got none (text=%s)", res.Text)
	}
	if res.Text == "" {
		t.Fatal("final text empty")
	}
	for _, tc := range res.ToolCalls {
		t.Logf("tool call: %s args=%s result=%s", tc.Name, tc.Args, tc.Result)
	}
	t.Logf("final: %s", res.Text)
}
