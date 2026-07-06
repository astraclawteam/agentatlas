package dream

import (
	"context"
	"fmt"
	"strings"
	"time"

	"google.golang.org/adk/model"

	db "github.com/astraclawteam/agentatlas/services/agentatlas/db/generated"
	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/llmutil"
)

// Synthesizer produces LLM-grade dream aggregations. Masking and risk
// extraction run BEFORE the model sees anything — by construction the model
// only ever receives masker output. A nil Synthesizer (or nil model) is the
// explicit degraded mode: it delegates to the deterministic AggregateBriefs.
type Synthesizer struct {
	llm model.LLM
}

// NewSynthesizer wraps a model. Passing nil yields the deterministic mode.
func NewSynthesizer(llm model.LLM) *Synthesizer { return &Synthesizer{llm: llm} }

const synthesisInstruction = `你是 AgentAtlas 的梦境汇总器。只根据用户提供的已脱敏简报与风险信号做汇总；不得引入外部信息，不得虚构内容，不得试图还原被脱敏（▇▇）的片段。严格输出一个 JSON 对象（不要 markdown 代码块，不要多余文本），字段：
{"display":"不超过200字的展示摘要","retrieval":"不超过1000字的检索摘要","risks":["风险要点"],"trends":["趋势要点"],"todos":["待办要点"]}`

type synthesisOut struct {
	Display   string   `json:"display"`
	Retrieval string   `json:"retrieval"`
	Risks     []string `json:"risks"`
	Trends    []string `json:"trends"`
	Todos     []string `json:"todos"`
}

// Aggregate synthesizes the window's briefs through the model. Model or
// output-format failures fail loud (the dream run fails and is retried by the
// task runner); only the no-model configuration degrades deterministically.
func (s *Synthesizer) Aggregate(ctx context.Context, scopeName string, windowStart, windowEnd time.Time, briefs []db.WorkBrief, masker *Masker, risks *RiskExtractor) (Aggregation, error) {
	if s == nil || s.llm == nil || len(briefs) == 0 {
		return AggregateBriefs(scopeName, windowStart, windowEnd, briefs, masker, risks), nil
	}

	// Mask first; the prompt is built exclusively from masker output.
	texts, byEmployee := maskBriefs(briefs, masker)
	signals := risks.Extract(texts)

	var prompt strings.Builder
	fmt.Fprintf(&prompt, "范围：%s（%s ~ %s）\n已脱敏简报（%d 条）：\n",
		scopeName, windowStart.Format("2006-01-02"), windowEnd.Format("2006-01-02"), len(texts))
	for i, t := range texts {
		fmt.Fprintf(&prompt, "%d. %s\n", i+1, t)
	}
	if len(signals) > 0 {
		prompt.WriteString("规则提取的风险信号：\n")
		for _, sg := range signals {
			fmt.Fprintf(&prompt, "- %s\n", sg)
		}
	}

	raw, err := llmutil.CompleteText(ctx, s.llm, synthesisInstruction, prompt.String())
	if err != nil {
		return Aggregation{}, fmt.Errorf("dream synthesis model call: %w", err)
	}
	var out synthesisOut
	if err := llmutil.ParseJSON(raw, &out); err != nil {
		return Aggregation{}, fmt.Errorf("dream synthesis output not valid JSON: %w (raw=%s)", err, truncateRunes(raw, 300))
	}
	display := truncateRunes(strings.TrimSpace(out.Display), 200)
	retrieval := truncateRunes(strings.TrimSpace(out.Retrieval), 1000)
	if display == "" || retrieval == "" {
		return Aggregation{}, fmt.Errorf("dream synthesis returned empty display/retrieval layer")
	}

	var digest strings.Builder
	digest.WriteString("\n## 模型综合\n")
	writeDigestSection(&digest, "风险", out.Risks)
	writeDigestSection(&digest, "趋势", out.Trends)
	writeDigestSection(&digest, "待办", out.Todos)

	return Aggregation{
		Display:      display,
		Retrieval:    retrieval,
		SealedDetail: sealedStructural(scopeName, windowStart, windowEnd, byEmployee, signals) + digest.String(),
		RiskSignals:  signals,
		InputCount:   len(briefs),
		Source:       "llm",
	}, nil
}

func writeDigestSection(b *strings.Builder, title string, items []string) {
	if len(items) == 0 {
		return
	}
	fmt.Fprintf(b, "### %s\n", title)
	for _, it := range items {
		fmt.Fprintf(b, "- %s\n", it)
	}
}
