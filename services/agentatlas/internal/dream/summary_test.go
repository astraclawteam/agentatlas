package dream

import (
	"strings"
	"testing"
	"time"

	db "github.com/astraclawteam/agentatlas/services/agentatlas/db/generated"
)

func TestMaskerStripsSensitiveSpans(t *testing.T) {
	m, err := NewMasker([]string{`1[3-9]\d{9}`, `身份证\d+`})
	if err != nil {
		t.Fatalf("masker: %v", err)
	}
	out := m.Apply("联系张予安 13800138000，身份证440300xxxx 已归档。")
	if strings.Contains(out, "13800138000") || strings.Contains(out, "身份证440300") {
		t.Fatalf("masking failed: %s", out)
	}
	if !strings.Contains(out, "▇▇") {
		t.Fatalf("mask marker missing: %s", out)
	}
}

func TestAggregateBriefsLayersAndRisks(t *testing.T) {
	masker, _ := NewMasker([]string{`1[3-9]\d{9}`})
	risks, _ := NewRiskExtractor([]string{`风险[:：]\S+`})
	window := time.Date(2026, 7, 6, 22, 0, 0, 0, time.UTC)

	briefs := []db.WorkBrief{
		{ID: "wb1", EmployeeUserID: "u_zhang", Summary: "完成分拣规则联调，风险:接口限流未定，电话13800138000。"},
		{ID: "wb2", EmployeeUserID: "u_li", Summary: "整理 MES 工单模板。"},
	}
	agg := AggregateBriefs("MES 专项组", window.Add(-24*time.Hour), window, briefs, masker, risks)

	if agg.InputCount != 2 {
		t.Fatalf("input count = %d", agg.InputCount)
	}
	if !strings.Contains(agg.Display, "2 名成员") || !strings.Contains(agg.Display, "风险信号 1 项") {
		t.Fatalf("display = %s", agg.Display)
	}
	if len([]rune(agg.Display)) > 200 || len([]rune(agg.Retrieval)) > 1000 {
		t.Fatal("layer length limits violated")
	}
	// masked phone must not appear in ANY layer
	for name, layer := range map[string]string{"display": agg.Display, "retrieval": agg.Retrieval, "sealed": agg.SealedDetail} {
		if strings.Contains(layer, "13800138000") {
			t.Fatalf("masked span leaked into %s layer", name)
		}
	}
	if len(agg.RiskSignals) != 1 || !strings.Contains(agg.RiskSignals[0], "接口限流") {
		t.Fatalf("risk signals = %v", agg.RiskSignals)
	}
	if !strings.Contains(agg.SealedDetail, "## u_zhang") || !strings.Contains(agg.SealedDetail, "## 风险信号") {
		t.Fatalf("sealed structure: %s", agg.SealedDetail)
	}

	empty := AggregateBriefs("组", window.Add(-24*time.Hour), window, nil, masker, risks)
	if empty.InputCount != 0 || !strings.Contains(empty.Display, "无工作简报") {
		t.Fatalf("empty aggregation: %+v", empty)
	}
}
