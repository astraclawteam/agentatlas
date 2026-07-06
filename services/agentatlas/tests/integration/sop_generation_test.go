// Integration test: a real llmrouter model extracts a SOP workflow draft from
// the MES fixture document through the Knowledge Agent. Run:
//
//	LLMROUTER_BASE_URL=https://.../v1 LLMROUTER_API_KEY=sk-... \
//	  go test ./tests/integration -run TestSOPGeneration
package integration

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	sdkworkflow "github.com/astraclawteam/agentatlas/sdk/go/workflow"
	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/agent"
	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/llmroutermodel"
)

func TestSOPGeneration(t *testing.T) {
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

	raw, err := os.ReadFile(filepath.Join("..", "fixtures", "documents", "mes_work_order_sop.md"))
	if err != nil {
		t.Fatalf("fixture: %v", err)
	}
	sections := strings.Split(string(raw), "\n\n")

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	wf, err := agent.ExtractSOP(ctx, r, agent.SOPDocument{
		Title:    "MES 异常工单排查",
		Sections: sections,
	})
	if err != nil {
		t.Fatalf("extract sop: %v", err)
	}
	if len(wf.Nodes) < 3 {
		t.Fatalf("nodes = %d, want >=3", len(wf.Nodes))
	}
	var human bool
	for _, n := range wf.Nodes {
		if n.Type == sdkworkflow.NodeHumanConfirm || n.Type == sdkworkflow.NodeInputEvidencePointer {
			human = true
		}
	}
	if !human {
		t.Fatalf("sop draft missing human.confirm / evidence node: %+v", wf.Nodes)
	}
	t.Logf("sop draft: id=%s risk=%s nodes=%d edges=%d", wf.WorkflowID, wf.RiskLevel, len(wf.Nodes), len(wf.Edges))
	for _, n := range wf.Nodes {
		t.Logf("  node %s type=%s name=%s", n.ID, n.Type, n.Name)
	}
}
