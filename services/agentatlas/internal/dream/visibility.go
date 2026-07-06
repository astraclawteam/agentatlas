package dream

import (
	"fmt"
	"regexp"
	"strings"
)

// Masker applies policy masking rules; matches are replaced with ▇▇ so
// summaries never leak the masked spans into any layer.
type Masker struct {
	rules []*regexp.Regexp
}

func NewMasker(rules []string) (*Masker, error) {
	m := &Masker{}
	for _, r := range rules {
		re, err := regexp.Compile(r)
		if err != nil {
			return nil, fmt.Errorf("masking rule %q: %w", r, err)
		}
		m.rules = append(m.rules, re)
	}
	return m, nil
}

func (m *Masker) Apply(text string) string {
	for _, re := range m.rules {
		text = re.ReplaceAllString(text, "▇▇")
	}
	return text
}

// RiskExtractor collects risk signals by rule; company-level summaries keep
// the signal, not the raw sentence.
type RiskExtractor struct {
	rules []*regexp.Regexp
}

func NewRiskExtractor(rules []string) (*RiskExtractor, error) {
	r := &RiskExtractor{}
	for _, rule := range rules {
		re, err := regexp.Compile(rule)
		if err != nil {
			return nil, fmt.Errorf("risk rule %q: %w", rule, err)
		}
		r.rules = append(r.rules, re)
	}
	return r, nil
}

func (r *RiskExtractor) Extract(texts []string) []string {
	seen := map[string]bool{}
	var signals []string
	for _, text := range texts {
		for _, re := range r.rules {
			for _, match := range re.FindAllString(text, -1) {
				match = strings.TrimSpace(match)
				if match != "" && !seen[match] {
					seen[match] = true
					signals = append(signals, match)
				}
			}
		}
	}
	return signals
}
