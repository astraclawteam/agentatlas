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
type Aggregation struct {
	Display      string
	Retrieval    string
	SealedDetail string
	RiskSignals  []string
	InputCount   int
}

func truncateRunes(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max])
}

// AggregateBriefs synthesizes the window's briefs structurally (LLM-grade
// narrative lands with the answer.generate / transform.summarize nodes; this
// keeps dreams runnable without model access and fully deterministic).
func AggregateBriefs(scopeName string, windowStart, windowEnd time.Time, briefs []db.WorkBrief, masker *Masker, risks *RiskExtractor) Aggregation {
	if len(briefs) == 0 {
		return Aggregation{
			Display:    fmt.Sprintf("%s：本期无工作简报输入。", scopeName),
			Retrieval:  fmt.Sprintf("%s %s 至 %s 无简报。", scopeName, windowStart.Format("01-02"), windowEnd.Format("01-02")),
			InputCount: 0,
		}
	}

	texts := make([]string, 0, len(briefs))
	var sealed strings.Builder
	fmt.Fprintf(&sealed, "# %s 梦境汇总（%s ~ %s）\n\n", scopeName,
		windowStart.Format("2006-01-02"), windowEnd.Format("2006-01-02"))
	byEmployee := map[string][]string{}
	for _, b := range briefs {
		masked := masker.Apply(b.Summary)
		texts = append(texts, masked)
		byEmployee[b.EmployeeUserID] = append(byEmployee[b.EmployeeUserID], masked)
	}
	for user, items := range byEmployee {
		fmt.Fprintf(&sealed, "## %s\n", user)
		for _, item := range items {
			fmt.Fprintf(&sealed, "- %s\n", item)
		}
	}
	signals := risks.Extract(texts)
	if len(signals) > 0 {
		fmt.Fprintf(&sealed, "\n## 风险信号\n")
		for _, s := range signals {
			fmt.Fprintf(&sealed, "- %s\n", s)
		}
	}

	display := fmt.Sprintf("%s：%d 名成员提交 %d 条简报", scopeName, len(byEmployee), len(briefs))
	if len(signals) > 0 {
		display += fmt.Sprintf("，风险信号 %d 项（%s）", len(signals), truncateRunes(strings.Join(signals, "、"), 80))
	} else {
		display += "，无风险信号"
	}

	return Aggregation{
		Display:      truncateRunes(display, 200),
		Retrieval:    truncateRunes(strings.Join(texts, "；"), 1000),
		SealedDetail: sealed.String(),
		RiskSignals:  signals,
		InputCount:   len(briefs),
	}
}
