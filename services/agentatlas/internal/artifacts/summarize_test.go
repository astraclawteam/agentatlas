package artifacts

import (
	"context"
	"iter"
	"strings"
	"testing"

	"google.golang.org/adk/model"
	"google.golang.org/genai"

	"github.com/astraclawteam/agentatlas/sdk/go/atlasdocument"
	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/parsergateway"
)

// scriptedLLM answers section calls with plain text and rollup calls (system
// prompt mentions JSON) with a canned JSON document summary.
type scriptedLLM struct {
	systems    []string
	prompts    []string
	rollupJSON string
}

func (s *scriptedLLM) Name() string { return "test/scripted" }

func (s *scriptedLLM) GenerateContent(_ context.Context, req *model.LLMRequest, _ bool) iter.Seq2[*model.LLMResponse, error] {
	system := ""
	if req.Config != nil && req.Config.SystemInstruction != nil {
		for _, p := range req.Config.SystemInstruction.Parts {
			system += p.Text
		}
	}
	user := ""
	for _, c := range req.Contents {
		for _, p := range c.Parts {
			user += p.Text
		}
	}
	s.systems = append(s.systems, system)
	s.prompts = append(s.prompts, user)
	reply := "节摘要：" + truncateRunes(strings.TrimSpace(user), 20)
	if strings.Contains(system, "JSON") {
		reply = s.rollupJSON
	}
	return func(yield func(*model.LLMResponse, error) bool) {
		yield(&model.LLMResponse{
			Content:      genai.NewContentFromText(reply, "model"),
			TurnComplete: true,
		}, nil)
	}
}

func twoSectionDoc() parsergateway.ParseOutput {
	return parsergateway.ParseOutput{
		ProviderID: "docling",
		Blocks: []atlasdocument.Block{
			{BlockID: "b1", Type: atlasdocument.BlockHeading, Text: "MES 异常工单排查", Order: 1},
			{BlockID: "b2", Type: atlasdocument.BlockText, Text: "第一步：确认工单编号并核对产线。", Order: 2},
			{BlockID: "b3", Type: atlasdocument.BlockHeading, Text: "风险点", Order: 3},
			{BlockID: "b4", Type: atlasdocument.BlockText, Text: "接口限流方案未定，需要与平台组确认。", Order: 4},
		},
	}
}

func TestSummarizerHierarchicalRollupAndCaps(t *testing.T) {
	longDisplay := strings.Repeat("展示摘要片段", 50) // 300 runes > 200 cap
	llm := &scriptedLLM{rollupJSON: `{"display":"` + longDisplay + `","retrieval":"面向检索的 MES 排查文档详细摘要。"}`}

	sums, source, err := NewSummarizer(llm).Summarize(context.Background(), twoSectionDoc())
	if err != nil {
		t.Fatalf("summarize: %v", err)
	}
	if source != "llm" {
		t.Fatalf("source = %q, want llm", source)
	}
	// 2 headings -> 2 sections -> 2 section calls + 1 rollup call.
	if len(llm.prompts) != 3 {
		t.Fatalf("model calls = %d, want 3 (prompts=%q)", len(llm.prompts), llm.prompts)
	}
	if !strings.Contains(llm.prompts[2], "节摘要：") {
		t.Fatalf("rollup must consume section summaries, got %q", llm.prompts[2])
	}
	if len(sums) != 2 {
		t.Fatalf("summaries = %d", len(sums))
	}
	byLevel := map[atlasdocument.SummaryLevel]string{}
	for _, s := range sums {
		byLevel[s.Level] = s.Text
	}
	if got := len([]rune(byLevel[atlasdocument.SummaryDisplay])); got > 200 {
		t.Fatalf("display cap violated: %d runes", got)
	}
	if !strings.Contains(byLevel[atlasdocument.SummaryRetrieval], "检索") {
		t.Fatalf("retrieval not model-derived: %q", byLevel[atlasdocument.SummaryRetrieval])
	}
}

func TestSummarizerBudgetSplitsOversizedSections(t *testing.T) {
	llm := &scriptedLLM{rollupJSON: `{"display":"大文档摘要","retrieval":"大文档检索摘要"}`}
	out := parsergateway.ParseOutput{
		ProviderID: "docling",
		Blocks: []atlasdocument.Block{
			{BlockID: "b1", Type: atlasdocument.BlockText, Text: strings.Repeat("长", 7000), Order: 1},
		},
	}
	_, source, err := NewSummarizer(llm).Summarize(context.Background(), out)
	if err != nil {
		t.Fatalf("summarize: %v", err)
	}
	if source != "llm" {
		t.Fatalf("source = %q", source)
	}
	// 7000 runes / 3000 budget -> 3 sections -> 3 section calls + 1 rollup.
	if len(llm.prompts) != 4 {
		t.Fatalf("model calls = %d, want 4", len(llm.prompts))
	}
	for i, p := range llm.prompts {
		if n := len([]rune(p)); n > maxSectionInputRunes+16 {
			t.Fatalf("call %d input %d runes exceeds per-call budget", i, n)
		}
	}
}

func TestSummarizerNilFallsBackDeterministic(t *testing.T) {
	out := twoSectionDoc()
	var nilSum *Summarizer
	for name, s := range map[string]*Summarizer{"nil_summarizer": nilSum, "nil_model": NewSummarizer(nil)} {
		sums, source, err := s.Summarize(context.Background(), out)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if source != "deterministic" {
			t.Fatalf("%s: source = %q", name, source)
		}
		if len(sums) != 2 || !strings.Contains(sums[0].Text, "MES 异常工单排查") {
			t.Fatalf("%s: deterministic summaries lost: %+v", name, sums)
		}
	}
}

func TestSummarizerFailsLoudOnBadModelOutput(t *testing.T) {
	for name, rollup := range map[string]string{
		"not_json":      "这不是 JSON",
		"empty_display": `{"display":"","retrieval":"x"}`,
	} {
		llm := &scriptedLLM{rollupJSON: rollup}
		_, _, err := NewSummarizer(llm).Summarize(context.Background(), twoSectionDoc())
		if err == nil {
			t.Fatalf("%s: bad model output must fail loud", name)
		}
	}
}
