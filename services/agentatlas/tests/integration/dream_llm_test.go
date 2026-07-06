// Integration test: a real llmrouter model synthesizes the dream layers over
// masked fixture briefs. Run:
//
//	LLMROUTER_BASE_URL=https://.../v1 LLMROUTER_API_KEY=sk-... \
//	  go test ./tests/integration -run TestDreamLLM
package integration

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	db "github.com/astraclawteam/agentatlas/services/agentatlas/db/generated"
	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/dream"
	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/llmroutermodel"
)

func TestDreamLLM(t *testing.T) {
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

	masker, err := dream.NewMasker([]string{`1[3-9]\d{9}`})
	if err != nil {
		t.Fatal(err)
	}
	risks, err := dream.NewRiskExtractor([]string{`风险[:：]\S+`})
	if err != nil {
		t.Fatal(err)
	}
	window := time.Date(2026, 7, 6, 22, 0, 0, 0, time.UTC)
	briefs := []db.WorkBrief{
		{ID: "wb1", EmployeeUserID: "u_zhang", Summary: "完成 MES 分拣规则联调与版本复核，风险:接口限流方案未定，联系电话13800138000。"},
		{ID: "wb2", EmployeeUserID: "u_li", Summary: "整理 MES 异常工单排查模板并同步项目组。"},
		{ID: "wb3", EmployeeUserID: "u_zhang", Summary: "与供应商确认工单接口字段映射。"},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	agg, err := dream.NewSynthesizer(m).Aggregate(ctx, "MES 专项组", window.Add(-24*time.Hour), window, briefs, masker, risks)
	if err != nil {
		t.Fatalf("synthesize: %v", err)
	}
	if agg.Source != "llm" {
		t.Fatalf("source = %q, want llm", agg.Source)
	}
	if agg.Display == "" || agg.Retrieval == "" || agg.SealedDetail == "" {
		t.Fatalf("empty layer: %+v", agg)
	}
	if n := len([]rune(agg.Display)); n > 200 {
		t.Fatalf("display cap violated: %d", n)
	}
	if n := len([]rune(agg.Retrieval)); n > 1000 {
		t.Fatalf("retrieval cap violated: %d", n)
	}
	for name, layer := range map[string]string{"display": agg.Display, "retrieval": agg.Retrieval, "sealed": agg.SealedDetail} {
		if strings.Contains(layer, "13800138000") {
			t.Fatalf("masked span leaked into %s layer", name)
		}
	}
	t.Logf("display: %s", agg.Display)
	t.Logf("retrieval: %s", agg.Retrieval)
}
