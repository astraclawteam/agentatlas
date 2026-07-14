package outcome

// policy.go (Task 0H) is the PUBLISHED, versioned Outcome-evaluation rule and
// the deterministic combination lattice that turns per-need verdicts into a
// single closed OutcomeStatus. It contains NO crypto, NO wall-clock read and NO
// persistence: the receipt-verification and freshness enforcement live in
// evaluator.go, and this file is the pure business rule they feed. Keeping the
// lattice here (and unexported) lets policy_test.go pin the precedence directly.

import (
	"fmt"

	model "github.com/astraclawteam/agentatlas/sdk/go/outcome"
)

// RequiredPostcondition is one governed postcondition whose confirmation is
// necessary for a Goal to be satisfied: the VerificationNeed that confirms it,
// the PostconditionSpec it belongs to, the authoritative source that must supply
// the observation, and -- REQUIRED for a postcondition to ever be satisfiable --
// the ACCEPTING DEFINITION (what an observed state confirming success looks
// like). It is PUBLISHED policy content -- never model prose: an LLM can propose
// that such a need exists, but only governed policy authorship (a published
// Operating Map / rule version) can require it AND declare what confirms it.
//
// An ObservationReceipt attests an observed STATE via its NormalizedObservationHash;
// it carries NO confirm/refute flag. Confirmation therefore requires comparing
// that hash to the accepting definition here. A postcondition with NO accepting
// definition is UNCONFIRMABLE: the evaluator resolves it to `unknown` (the case
// cannot complete), never satisfied-by-presence.
//
// The accepting definition is the union of the single ExpectedObservationHash
// (convenience) and the AcceptingObservationHashes set; a fresh authoritative
// observation SATISFIES the postcondition iff its hash is in that union.
type RequiredPostcondition struct {
	NeedID                     string
	PostconditionID            string
	Authority                  string
	ExpectedObservationHash    string
	AcceptingObservationHashes []string
}

// hasAcceptingDefinition reports whether the policy declares what a confirming
// observation looks like for this postcondition. Absent ⇒ unconfirmable.
func (rp RequiredPostcondition) hasAcceptingDefinition() bool {
	return rp.ExpectedObservationHash != "" || len(rp.AcceptingObservationHashes) > 0
}

// accepts reports whether an observed normalized-observation hash is in this
// postcondition's declared accepting definition.
func (rp RequiredPostcondition) accepts(hash string) bool {
	if rp.ExpectedObservationHash != "" && hash == rp.ExpectedObservationHash {
		return true
	}
	for _, h := range rp.AcceptingObservationHashes {
		if h == hash {
			return true
		}
	}
	return false
}

// Policy is one published, versioned Outcome-evaluation policy. Version is bound
// verbatim into every produced Outcome as its rule_version (the exact evaluation
// rule the decision was made under). Required enumerates the postconditions whose
// confirmation defines Goal satisfaction (the coverage requirement). A Goal with
// zero required postconditions can never be "satisfied" -- at most "unverified".
type Policy struct {
	Version  string
	Required []RequiredPostcondition
}

// Validate enforces that the policy names a version and that each required
// postcondition is well-formed and declared at most once (ambiguous coverage is
// rejected -- one need maps to exactly one required postcondition).
func (p Policy) Validate() error {
	if p.Version == "" {
		return fmt.Errorf("outcome policy: a published version is required (it binds the Outcome's rule_version)")
	}
	seen := map[string]bool{}
	for i, rp := range p.Required {
		if rp.NeedID == "" || rp.PostconditionID == "" {
			return fmt.Errorf("outcome policy: required[%d] needs both a need_id and a postcondition_id", i)
		}
		if rp.Authority == "" {
			return fmt.Errorf("outcome policy: required[%d] needs an authoritative source", i)
		}
		if seen[rp.NeedID] {
			return fmt.Errorf("outcome policy: required need %q declared more than once (ambiguous coverage)", rp.NeedID)
		}
		seen[rp.NeedID] = true
	}
	return nil
}

// needVerdict is the per-required-postcondition verdict the evaluator computes
// from verified, fresh, authoritative observations before the lattice combines
// them. It is deliberately narrower than OutcomeStatus (no blocked/unverified):
// blocked comes from external blockers and unverified is the vacuous default,
// both applied in combineStatus.
type needVerdict string

const (
	needSatisfied   needVerdict = "satisfied"
	needUnsatisfied needVerdict = "unsatisfied"
	needDisputed    needVerdict = "disputed"
	needUnknown     needVerdict = "unknown"
)

// combineStatus is the PUBLISHED lattice. Precedence, highest first:
//
//	disputed > blocked > unsatisfied > unknown > satisfied(all) > unverified
//
// The ordering is the load-bearing safety rule: a genuine authority conflict or a
// human dispute is never silently downgraded (disputed dominates); an external
// blocker never masquerades as a business result (blocked dominates any positive
// need); a single refuted required postcondition sinks the Outcome (unsatisfied
// dominates unknown); any unresolved required postcondition prevents satisfied
// (unknown dominates satisfied); and satisfied is reached ONLY when there is at
// least one required postcondition and EVERY one is independently satisfied.
func combineStatus(verdicts []needVerdict, hasBlocker, hasHumanDispute bool, requiredCount int) model.OutcomeStatus {
	anyDisputed, anyUnsatisfied, anyUnknown := false, false, false
	satisfiedCount := 0
	for _, v := range verdicts {
		switch v {
		case needDisputed:
			anyDisputed = true
		case needUnsatisfied:
			anyUnsatisfied = true
		case needUnknown:
			anyUnknown = true
		case needSatisfied:
			satisfiedCount++
		}
	}
	switch {
	case anyDisputed || hasHumanDispute:
		return model.OutcomeDisputed
	case hasBlocker:
		return model.OutcomeBlocked
	case anyUnsatisfied:
		return model.OutcomeUnsatisfied
	case anyUnknown:
		return model.OutcomeUnknown
	case requiredCount > 0 && satisfiedCount == requiredCount:
		return model.OutcomeSatisfied
	default:
		return model.OutcomeUnverified
	}
}
