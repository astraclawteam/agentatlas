package assessment

// confidence.go (Task 18C) computes the seven EXPLICIT, NAMED confidence
// components the plan requires — coverage, authority, freshness, consistency,
// attribution clarity, projection freshness and rule clarity — plus the overall
// level. Confidence is DETERMINISTIC METADATA that EXPLAINS how well-supported a
// dimension result is; it never by itself flips a dimension result (that is the
// verify-then-score logic in evaluator.go). Every score is a pure function of
// decision-level facts (counts and flags), bounded to [0,1]; no wall clock, no
// randomness, no LLM.

import (
	model "github.com/astraclawteam/agentatlas/sdk/go/assessment"
)

func clamp01(f float64) float64 {
	switch {
	case f < 0:
		return 0
	case f > 1:
		return 1
	default:
		return f
	}
}

// ratio returns n/d clamped to [0,1]; a zero denominator yields 0 (no evidence
// means no confidence, never a vacuous 1).
func ratio(n, d int) float64 {
	if d <= 0 {
		return 0
	}
	return clamp01(float64(n) / float64(d))
}

// confidenceInputs is the deterministic trace the evaluator threads into the
// component computation for ONE dimension. Every field is a decision-level count
// or flag, never a continuous function of the wall clock.
type confidenceInputs struct {
	candidates        int                   // evidence candidates considered (verified + failed)
	counted           int                   // candidates that verified into counted evidence
	authoritative     int                   // counted evidence that is tier-1 (verified outcome + signed observation)
	fresh             int                   // counted evidence backed by a fresh authoritative observation
	conflicts         int                   // conflicting (disputed) signals
	contributions     int                   // credited contributions
	attributedContrib int                   // contributions carrying a clear (non-empty) attribution
	graphStale        bool                  // the projection watermark advanced mid-read
	graphTruncated    bool                  // a graph read hit its row budget
	ruleLevel         model.ConfidenceLevel // the policy's confidence rule for this dimension
}

// computeConfidence produces the seven named, bounded components and the overall
// level. It is a pure function of its input, so identical governed facts yield an
// identical explanation.
func computeConfidence(in confidenceInputs) model.ConfidenceExplanation {
	// coverage: fraction of considered candidates that verified into evidence.
	coverage := ratio(in.counted, in.candidates)
	// authority: fraction of counted evidence that is authoritative (tier-1).
	authority := ratio(in.authoritative, in.counted)
	// freshness: fraction of counted evidence backed by a fresh observation.
	freshness := ratio(in.fresh, in.counted)
	// consistency: 1 minus the fraction of considered candidates in conflict.
	consistency := clamp01(1 - ratio(in.conflicts, in.candidates))
	if in.candidates == 0 {
		consistency = 1
	}
	// attribution clarity: fraction of contributions with a clear attribution
	// (vacuously clear when there are none to attribute).
	attribution := 1.0
	if in.contributions > 0 {
		attribution = ratio(in.attributedContrib, in.contributions)
	}
	// projection freshness: a stale read is 0, a budget-truncated read is halved,
	// a clean read is 1 — the projection watermark's contribution to confidence.
	projection := 1.0
	switch {
	case in.graphStale:
		projection = 0
	case in.graphTruncated:
		projection = 0.5
	}
	// rule clarity: how confidently the governed policy scores this dimension.
	rule := ruleClarity(in.ruleLevel)

	components := []model.ConfidenceComponent{
		{Kind: model.ConfCoverage, Score: coverage},
		{Kind: model.ConfAuthority, Score: authority},
		{Kind: model.ConfFreshness, Score: freshness},
		{Kind: model.ConfConsistency, Score: consistency},
		{Kind: model.ConfAttributionClarity, Score: attribution},
		{Kind: model.ConfProjectionFreshness, Score: projection},
		{Kind: model.ConfRuleClarity, Score: rule},
	}
	return model.ConfidenceExplanation{Level: overallLevel(components), Components: components}
}

func ruleClarity(level model.ConfidenceLevel) float64 {
	switch level {
	case model.ConfidenceHigh:
		return 1
	case model.ConfidenceMedium:
		return 0.67
	case model.ConfidenceLow:
		return 0.34
	default:
		// An unset/unknown rule level is treated as medium clarity (the policy
		// default) rather than zero — the dimension still has a governed rule.
		return 0.67
	}
}

// overallLevel maps the seven components onto a closed ConfidenceLevel. It is
// fail-closed: any of the four CRITICAL components (coverage, authority,
// consistency, projection freshness) at zero forces low, because a result with no
// coverage, no authority, an unresolved conflict or a stale projection can never
// be high/medium confidence regardless of the others.
func overallLevel(components []model.ConfidenceComponent) model.ConfidenceLevel {
	critical := map[string]bool{
		model.ConfCoverage: true, model.ConfAuthority: true,
		model.ConfConsistency: true, model.ConfProjectionFreshness: true,
	}
	sum := 0.0
	for _, c := range components {
		sum += c.Score
		if critical[c.Kind] && c.Score == 0 {
			return model.ConfidenceLow
		}
	}
	avg := sum / float64(len(components))
	switch {
	case avg >= 0.8:
		return model.ConfidenceHigh
	case avg >= 0.5:
		return model.ConfidenceMedium
	default:
		return model.ConfidenceLow
	}
}
