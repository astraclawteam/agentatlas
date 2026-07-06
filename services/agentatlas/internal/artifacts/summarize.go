package artifacts

import (
	"context"
	"fmt"
	"strings"

	"google.golang.org/adk/model"

	"github.com/astraclawteam/agentatlas/sdk/go/atlasdocument"
	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/llmutil"
	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/parsergateway"
)

// maxSectionInputRunes bounds the content one model call may receive — long
// documents are summarized hierarchically (per section, then rolled up) so a
// single call never sees the whole document.
const maxSectionInputRunes = 3000

// Summarizer produces LLM-grade display/retrieval summaries from parsed
// content. A nil Summarizer (or nil model) is the explicit degraded mode: it
// delegates to the deterministic buildSummaries.
type Summarizer struct {
	llm model.LLM
}

// NewSummarizer wraps a model. Passing nil yields the deterministic mode.
func NewSummarizer(llm model.LLM) *Summarizer { return &Summarizer{llm: llm} }

const sectionInstruction = `你是文档分节摘要器。只根据给定内容，用中文概括该节要点（2~4 句），不得引入外部信息，不得虚构。直接输出摘要文本，不要任何前后缀。`

const docInstruction = `你是文档摘要器。只根据用户提供的分节摘要（或全文）生成文档级摘要，不得引入外部信息，不得虚构。严格输出一个 JSON 对象（不要 markdown 代码块，不要多余文本），字段：
{"display":"不超过200字、可直接展示的中文文档摘要","retrieval":"不超过1000字、面向检索的中文详细摘要"}`

type docSummaryOut struct {
	Display   string `json:"display"`
	Retrieval string `json:"retrieval"`
}

// Summarize returns the document summaries plus their source
// ("llm" | "deterministic"). Model failures fail loud — the artifact job fails
// instead of silently downgrading.
func (s *Summarizer) Summarize(ctx context.Context, out parsergateway.ParseOutput) ([]atlasdocument.Summary, string, error) {
	if s == nil || s.llm == nil {
		return buildSummaries(out), "deterministic", nil
	}
	sections := sectionize(out)
	if len(sections) == 0 {
		// Nothing textual to feed a model (e.g. image-only artifact).
		return buildSummaries(out), "deterministic", nil
	}

	rollupInput := sections[0]
	if len(sections) > 1 {
		// Hierarchical pass: one call per section, then roll the section
		// summaries up into the document-level layers.
		parts := make([]string, 0, len(sections))
		for i, sec := range sections {
			sum, err := llmutil.CompleteText(ctx, s.llm, sectionInstruction, sec)
			if err != nil {
				return nil, "", fmt.Errorf("summarize section %d/%d: %w", i+1, len(sections), err)
			}
			parts = append(parts, strings.TrimSpace(sum))
		}
		rollupInput = strings.Join(parts, "\n---\n")
	}

	raw, err := llmutil.CompleteText(ctx, s.llm, docInstruction, rollupInput)
	if err != nil {
		return nil, "", fmt.Errorf("document summary model call: %w", err)
	}
	var parsed docSummaryOut
	if err := llmutil.ParseJSON(raw, &parsed); err != nil {
		return nil, "", fmt.Errorf("document summary output not valid JSON: %w (raw=%s)", err, truncateRunes(raw, 300))
	}
	display := truncateRunes(strings.TrimSpace(parsed.Display), 200)
	retrieval := truncateRunes(strings.TrimSpace(parsed.Retrieval), 1000)
	if display == "" || retrieval == "" {
		return nil, "", fmt.Errorf("document summary returned empty display/retrieval layer")
	}
	return []atlasdocument.Summary{
		{SummaryID: newID("sum"), Level: atlasdocument.SummaryDisplay, Text: display},
		{SummaryID: newID("sum"), Level: atlasdocument.SummaryRetrieval, Text: retrieval},
	}, "llm", nil
}

// sectionize groups the parsed textual content into model-call-sized sections:
// a new section at every heading, plus budget splits inside oversized
// sections. Audio transcripts form their own trailing sections.
func sectionize(out parsergateway.ParseOutput) []string {
	var sections []string
	var cur strings.Builder
	flush := func() {
		if strings.TrimSpace(cur.String()) != "" {
			sections = append(sections, cur.String())
		}
		cur.Reset()
	}
	add := func(text string) {
		for _, chunk := range splitRunes(text, maxSectionInputRunes) {
			if len([]rune(cur.String()))+len([]rune(chunk)) > maxSectionInputRunes {
				flush()
			}
			cur.WriteString(chunk)
			cur.WriteString("\n")
		}
	}
	for _, b := range out.Blocks {
		switch b.Type {
		case atlasdocument.BlockHeading:
			flush()
			add(b.Text)
		case atlasdocument.BlockText:
			add(b.Text)
		}
	}
	flush()
	if len(out.AudioSegments) > 0 {
		var tr strings.Builder
		tr.WriteString("语音转写：\n")
		for _, seg := range out.AudioSegments {
			tr.WriteString(seg.Text)
			tr.WriteString("\n")
		}
		for _, chunk := range splitRunes(tr.String(), maxSectionInputRunes) {
			sections = append(sections, chunk)
		}
	}
	return sections
}

func splitRunes(s string, max int) []string {
	r := []rune(s)
	if len(r) <= max {
		return []string{s}
	}
	var out []string
	for start := 0; start < len(r); start += max {
		end := start + max
		if end > len(r) {
			end = len(r)
		}
		out = append(out, string(r[start:end]))
	}
	return out
}
