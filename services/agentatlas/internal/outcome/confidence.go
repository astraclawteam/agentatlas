package outcome

// confidence.go (Task 0H) computes the five EXPLICIT, NAMED confidence
// components the plan requires -- authority, freshness, coverage, consistency,
// attribution. They EXPLAIN a decision (how well-supported it is); they never by
// themselves flip the status, which combineStatus decides from the published
// rule alone. Every score is deterministic and bounded to [0,1].

import (
	model "github.com/astraclawteam/agentatlas/sdk/go/outcome"
)

// Confidence component kind names. These are the stable vocabulary carried in
// each produced Outcome's ConfidenceComponent.Kind.
const (
	ConfidenceAuthority   = "authority"
	ConfidenceFreshness   = "freshness"
	ConfidenceCoverage    = "coverage"
	ConfidenceConsistency = "consistency"
	ConfidenceAttribution = "attribution"
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

func ratio(n, d int) float64 {
	if d <= 0 {
		return 1
	}
	return clamp01(float64(n) / float64(d))
}

// confidenceInputs is the deterministic trace the evaluator threads into the
// component computation. Every field is a DECISION-LEVEL fact (a count of
// verdicts), NOT a continuous function of the wall clock: freshness confidence is
// the fraction of required needs whose observation is fresh, so a different
// decided_at that does not flip any fresh/stale verdict leaves every component
// (and thus the Outcome content hash) unchanged.
type confidenceInputs struct {
	requiredCount        int // number of required postconditions
	authoritativeCovered int // required needs that had an authoritative observation (fresh or stale)
	freshCount           int // required needs with a FRESH authoritative observation
	conflicts            int // required needs with conflicting authorities
	contributions        int // credited contributions
}

// confidenceComponents computes the five named, bounded components. It is a pure
// function of its input, so identical governed facts yield identical components.
func confidenceComponents(in confidenceInputs) []model.ConfidenceComponent {
	// authority: fraction of required needs backed by an authoritative observation.
	authority := ratio(in.authoritativeCovered, in.requiredCount)

	// freshness: fraction of required needs whose authoritative observation is
	// fresh. Vacuously 1 when nothing is required. This keys only on the
	// fresh/stale verdict, never the exact remaining life.
	freshness := ratio(in.freshCount, in.requiredCount)

	// coverage: fraction of required needs actually observed authoritatively.
	coverage := ratio(in.authoritativeCovered, in.requiredCount)

	// consistency: 1 minus the fraction of required needs in authority conflict.
	consistency := clamp01(1 - ratio(in.conflicts, in.requiredCount))
	if in.requiredCount == 0 {
		consistency = 1
	}

	// attribution: whether the work carries any credited contribution.
	attribution := 0.0
	if in.contributions > 0 {
		attribution = 1.0
	}

	return []model.ConfidenceComponent{
		{Kind: ConfidenceAuthority, Score: authority},
		{Kind: ConfidenceFreshness, Score: freshness},
		{Kind: ConfidenceCoverage, Score: coverage},
		{Kind: ConfidenceConsistency, Score: consistency},
		{Kind: ConfidenceAttribution, Score: attribution},
	}
}
