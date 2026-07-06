package dream

import (
	"context"
	"iter"
	"strings"
	"testing"
	"time"

	"google.golang.org/adk/model"
	"google.golang.org/genai"

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
	if agg.Source != "deterministic" || empty.Source != "deterministic" {
		t.Fatalf("deterministic source not marked: %q / %q", agg.Source, empty.Source)
	}
}

// capturingLLM records the full request text and yields a canned reply.
type capturingLLM struct {
	prompt string
	reply  string
}

func (c *capturingLLM) Name() string { return "test/capture" }

func (c *capturingLLM) GenerateContent(_ context.Context, req *model.LLMRequest, _ bool) iter.Seq2[*model.LLMResponse, error] {
	var b strings.Builder
	if req.Config != nil && req.Config.SystemInstruction != nil {
		for _, p := range req.Config.SystemInstruction.Parts {
			b.WriteString(p.Text)
		}
	}
	for _, ct := range req.Contents {
		for _, p := range ct.Parts {
			b.WriteString(p.Text)
		}
	}
	c.prompt = b.String()
	return func(yield func(*model.LLMResponse, error) bool) {
		yield(&model.LLMResponse{
			Content:      genai.NewContentFromText(c.reply, "model"),
			TurnComplete: true,
		}, nil)
	}
}

func synthFixture() ([]db.WorkBrief, *Masker, *RiskExtractor, time.Time) {
	masker, _ := NewMasker([]string{`1[3-9]\d{9}`})
	risks, _ := NewRiskExtractor([]string{`风险[:：]\S+`})
	window := time.Date(2026, 7, 6, 22, 0, 0, 0, time.UTC)
	briefs := []db.WorkBrief{
		{ID: "wb1", EmployeeUserID: "u_zhang", Summary: "完成分拣规则联调，风险:接口限流未定，电话13800138000。"},
		{ID: "wb2", EmployeeUserID: "u_li", Summary: "整理 MES 工单模板。"},
	}
	return briefs, masker, risks, window
}

func TestSynthesizerMasksBeforeModelAndCapsLayers(t *testing.T) {
	briefs, masker, risks, window := synthFixture()
	longDisplay := strings.Repeat("很长的展示摘要", 40) // 280 runes > 200 cap
	cap := &capturingLLM{reply: `{"display":"` + longDisplay + `","retrieval":"模型检索摘要：MES 工单联调推进，限流风险待定。","risks":["接口限流未定"],"trends":["联调收尾"],"todos":["确认限流方案"]}`}

	agg, err := NewSynthesizer(cap).Aggregate(context.Background(), "MES 专项组", window.Add(-24*time.Hour), window, briefs, masker, risks)
	if err != nil {
		t.Fatalf("aggregate: %v", err)
	}
	if strings.Contains(cap.prompt, "13800138000") {
		t.Fatal("raw (unmasked) brief text reached the model request")
	}
	if !strings.Contains(cap.prompt, "▇▇") {
		t.Fatalf("masked text missing from model request: %s", cap.prompt)
	}
	if agg.Source != "llm" {
		t.Fatalf("source = %q, want llm", agg.Source)
	}
	if got := len([]rune(agg.Display)); got > 200 {
		t.Fatalf("display cap violated: %d runes", got)
	}
	if !strings.Contains(agg.Retrieval, "模型检索摘要") {
		t.Fatalf("retrieval not model-derived: %s", agg.Retrieval)
	}
	if !strings.Contains(agg.SealedDetail, "## u_zhang") || !strings.Contains(agg.SealedDetail, "## 模型综合") {
		t.Fatalf("sealed must keep structure + model digest: %s", agg.SealedDetail)
	}
	if len(agg.RiskSignals) != 1 || !strings.Contains(agg.RiskSignals[0], "接口限流") {
		t.Fatalf("risk signals = %v", agg.RiskSignals)
	}
}

func TestSynthesizerNilModelFallsBackDeterministic(t *testing.T) {
	briefs, masker, risks, window := synthFixture()
	var nilSynth *Synthesizer
	for name, s := range map[string]*Synthesizer{"nil_synth": nilSynth, "nil_model": NewSynthesizer(nil)} {
		agg, err := s.Aggregate(context.Background(), "组", window.Add(-24*time.Hour), window, briefs, masker, risks)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if agg.Source != "deterministic" {
			t.Fatalf("%s: source = %q, want deterministic", name, agg.Source)
		}
		if !strings.Contains(agg.Display, "2 名成员") {
			t.Fatalf("%s: deterministic display lost: %s", name, agg.Display)
		}
	}
}

func TestSynthesizerFailsLoudOnBadModelOutput(t *testing.T) {
	briefs, masker, risks, window := synthFixture()
	for name, reply := range map[string]string{
		"not_json":      "这不是 JSON",
		"empty_display": `{"display":"","retrieval":"x"}`,
	} {
		_, err := NewSynthesizer(&capturingLLM{reply: reply}).Aggregate(
			context.Background(), "组", window.Add(-24*time.Hour), window, briefs, masker, risks)
		if err == nil {
			t.Fatalf("%s: bad model output must fail loud", name)
		}
	}
}
