package dream

import (
	"fmt"
	"strings"
	"time"

	db "github.com/astraclawteam/agentatlas/services/agentatlas/db/generated"
)

// Layered output of one aggregation:
//   - Display: card/timeline text, always safe to show.
//   - Retrieval: agent-searchable summary (visibility checked by AgentNexus).
//   - SealedDetail: full aggregation, object storage only, pointer in DB.
//   - Source: "llm" when a model synthesized the layers, "deterministic" for
//     the no-model degraded mode.
type Aggregation struct {
	Display      string
	Retrieval    string
	SealedDetail string
	RiskSignals  []string
	InputCount   int
	Source       string
}

func truncateRunes(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max])
}

// maskBriefs applies the policy masker to every brief BEFORE anything else
// consumes the text — no unmasked content may leave this function.
func maskBriefs(briefs []db.WorkBrief, masker *Masker) (texts []string, byEmployee map[string][]string) {
	texts = make([]string, 0, len(briefs))
	byEmployee = map[string][]string{}
	for _, b := range briefs {
		masked := masker.Apply(b.Summary)
		texts = append(texts, masked)
		byEmployee[b.EmployeeUserID] = append(byEmployee[b.EmployeeUserID], masked)
	}
	return texts, byEmployee
}

// sealedStructural renders the per-employee masked briefs + risk signals — the
// structural backbone of the sealed detailed summary.
func sealedStructural(scopeName string, windowStart, windowEnd time.Time, byEmployee map[string][]string, signals []string) string {
	var sealed strings.Builder
	fmt.Fprintf(&sealed, "# %s 梦境汇总（%s ~ %s）\n\n", scopeName,
		windowStart.Format("2006-01-02"), windowEnd.Format("2006-01-02"))
	for user, items := range byEmployee {
		fmt.Fprintf(&sealed, "## %s\n", user)
		for _, item := range items {
			fmt.Fprintf(&sealed, "- %s\n", item)
		}
	}
	if len(signals) > 0 {
		fmt.Fprintf(&sealed, "\n## 风险信号\n")
		for _, s := range signals {
			fmt.Fprintf(&sealed, "- %s\n", s)
		}
	}
	return sealed.String()
}

// AggregateBriefs synthesizes the window's briefs structurally. It is the
// explicit no-model degraded mode (Source="deterministic"); LLM-grade
// narrative comes from Synthesizer.Aggregate.
func AggregateBriefs(scopeName string, windowStart, windowEnd time.Time, briefs []db.WorkBrief, masker *Masker, risks *RiskExtractor) Aggregation {
	if len(briefs) == 0 {
		return Aggregation{
			Display:    fmt.Sprintf("%s：本期无工作简报输入。", scopeName),
			Retrieval:  fmt.Sprintf("%s %s 至 %s 无简报。", scopeName, windowStart.Format("01-02"), windowEnd.Format("01-02")),
			InputCount: 0,
			Source:     "deterministic",
		}
	}

	texts, byEmployee := maskBriefs(briefs, masker)
	signals := risks.Extract(texts)

	display := fmt.Sprintf("%s：%d 名成员提交 %d 条简报", scopeName, len(byEmployee), len(briefs))
	if len(signals) > 0 {
		display += fmt.Sprintf("，风险信号 %d 项（%s）", len(signals), truncateRunes(strings.Join(signals, "、"), 80))
	} else {
		display += "，无风险信号"
	}

	return Aggregation{
		Display:      truncateRunes(display, 200),
		Retrieval:    truncateRunes(strings.Join(texts, "；"), 1000),
		SealedDetail: sealedStructural(scopeName, windowStart, windowEnd, byEmployee, signals),
		RiskSignals:  signals,
		InputCount:   len(briefs),
		Source:       "deterministic",
	}
}
