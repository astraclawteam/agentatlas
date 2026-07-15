package assessment

// blockers.go (Task 18C) is the DETERMINISTIC, FAIR blocker classifier. It turns
// the governed evidence SIGNALS backing a blocker into a closed classification
// confidence {verified, corroborated, reported, inferred} and a closed delay
// attribution {external, personal, process, resource, unattributed}. It is a pure
// function of its input: no wall clock, no randomness, no LLM, no map iteration.
//
// FAIRNESS is load-bearing (consistent with policy.go's stance): "personal"
// grades the assessed party's OWN work/process fact (e.g. a rework step) and is
// reached ONLY when no external, resource or process cause explains the delay —
// an outside cause always dominates, so a delay is never blamed on the person
// when the evidence points elsewhere. "unattributed" is the honest default when
// the signals support no attribution at all. The classifier never grades a
// person's character — only work/process facts.

import (
	model "github.com/astraclawteam/agentatlas/sdk/go/assessment"
)

// BlockerSignals is the governed, verified evidence backing one blocker. Every
// field is a fact derived upstream (a signed observation/receipt, a count of
// independent human reports, and the cause category the evidence points at) —
// never model prose.
type BlockerSignals struct {
	// Signed reports that the blocker is backed by a signed observation/receipt
	// (the highest-trust evidence).
	Signed bool
	// Reports is the number of INDEPENDENT human reports that name this blocker.
	Reports int
	// Cause signals (evidence-graded). More than one may be set; the fair
	// precedence below decides the attribution.
	External bool
	Process  bool
	Resource bool
	Personal bool
}

// ClassifyBlocker computes the deterministic (confidence, delay) for a blocker
// from its signals.
//
// Confidence (most to least trusted): a signed observation/receipt is VERIFIED;
// two or more independent reports are CORROBORATED; a single report is REPORTED;
// anything else is INFERRED (derived from indirect signals only).
//
// Delay attribution precedence (fair — external/system causes first): EXTERNAL
// (an outside dependency) dominates, then RESOURCE (a resource gap), then PROCESS
// (an SOP/process step), then PERSONAL (the assessed party's own work/process
// fact), else UNATTRIBUTED.
func ClassifyBlocker(s BlockerSignals) (model.BlockerConfidence, model.DelayAttribution) {
	return classifyConfidence(s), classifyDelay(s)
}

func classifyConfidence(s BlockerSignals) model.BlockerConfidence {
	switch {
	case s.Signed:
		return model.BlockerVerified
	case s.Reports >= 2:
		return model.BlockerCorroborated
	case s.Reports == 1:
		return model.BlockerReported
	default:
		return model.BlockerInferred
	}
}

func classifyDelay(s BlockerSignals) model.DelayAttribution {
	switch {
	case s.External:
		return model.DelayExternal
	case s.Resource:
		return model.DelayResource
	case s.Process:
		return model.DelayProcess
	case s.Personal:
		return model.DelayPersonal
	default:
		return model.DelayUnattributed
	}
}
