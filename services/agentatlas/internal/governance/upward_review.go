package governance

// upward_review.go defines the shared UpwardReviewPolicy (GA Task 0E): the
// deterministic rule for how many, and which, upward reviewers a governed side
// effect requires. It builds on the same org/actor concepts as the rest of this
// package (an Actor's OrgVersion and org-unit membership) but expresses them as
// an explicit, self-contained Party value so the policy can be evaluated
// without a live org-graph service.
//
// The rule (from the plan):
//   - a low-risk side effect requires ONE distinct eligible upward reviewer;
//   - a high-risk side effect requires TWO distinct eligible upward reviewers;
//   - a decision by the submitter, a peer, a subordinate, a duplicate reviewer,
//     or a reviewer/submitter deciding against a stale org snapshot fails.

import (
	"errors"
	"fmt"

	model "github.com/astraclawteam/agentatlas/sdk/go/governance"
)

// Rejection reasons. Each is a distinct sentinel so callers/tests can assert
// exactly which eligibility rule rejected a decision.
var (
	// ErrNoUpwardReviewer: fewer than the required number of distinct eligible
	// upward reviewers were supplied.
	ErrNoUpwardReviewer = errors.New("governance: insufficient distinct eligible upward reviewers")
	// ErrReviewerIsSubmitter: the submitter cannot review their own side effect.
	ErrReviewerIsSubmitter = errors.New("governance: submitter cannot review their own side effect")
	// ErrReviewerIsPeer: a reviewer in the submitter's own org unit is a peer,
	// not upward.
	ErrReviewerIsPeer = errors.New("governance: reviewer is a peer, not upward")
	// ErrReviewerIsSubordinate: a reviewer below the submitter in the org
	// hierarchy is a subordinate.
	ErrReviewerIsSubordinate = errors.New("governance: reviewer is a subordinate, not upward")
	// ErrDuplicateReviewer: the same reviewer identity was counted more than
	// once (distinct reviewers are required).
	ErrDuplicateReviewer = errors.New("governance: duplicate reviewer identity")
	// ErrStaleOrgReviewer: a reviewer or the submitter decided against an
	// out-of-date org snapshot (OrgVersion != CurrentOrgVersion).
	ErrStaleOrgReviewer = errors.New("governance: decision made against a stale org version")
	// ErrReviewerNotUpward: a reviewer in an unrelated branch is neither an
	// ancestor nor a descendant of the submitter, so not an upward reviewer.
	ErrReviewerNotUpward = errors.New("governance: reviewer is not upward of the submitter")
	// ErrUnknownRisk: the risk tier is neither low nor high.
	ErrUnknownRisk = errors.New("governance: unknown risk tier for upward review")
)

// Party is one participant in an upward-review decision, reduced to exactly the
// org facts the policy needs. OrgPath is the strict ancestor chain of OrgUnitID
// (root-first, EXCLUDING OrgUnitID itself); an empty OrgPath means the unit is
// at the org root.
type Party struct {
	UserID     string
	OrgUnitID  string
	OrgPath    []string
	OrgVersion int64
}

// ApprovalParties bundles the submitter and the reviewers whose decisions are
// being evaluated against an UpwardReviewPolicy.
type ApprovalParties struct {
	Submitter Party
	Reviewers []Party
}

// UpwardReviewPolicy is the shared, deterministic upward-review rule. It is
// pure data: the authoritative current org version. Every Party is checked
// against it for staleness.
type UpwardReviewPolicy struct {
	CurrentOrgVersion int64
}

// Required returns how many distinct eligible upward reviewers a side effect at
// the given risk tier needs: one for low, two for high.
func (p UpwardReviewPolicy) Required(risk model.RiskLevel) (int, error) {
	switch risk {
	case model.RiskLow:
		return 1, nil
	case model.RiskHigh:
		return 2, nil
	default:
		return 0, fmt.Errorf("%w: %q", ErrUnknownRisk, risk)
	}
}

// isAncestorOf reports whether unit appears in path (i.e. unit is a strict
// ancestor of the party whose ancestor chain is path).
func isAncestorOf(unit string, path []string) bool {
	for _, u := range path {
		if u == unit {
			return true
		}
	}
	return false
}

// Evaluate applies the upward-review rule to parties at the given risk tier. It
// returns nil only when at least the required number of DISTINCT, eligible,
// current-org upward reviewers are present AND every supplied reviewer is
// individually eligible (a single bad reviewer -- submitter/peer/subordinate/
// duplicate/stale/unrelated -- fails the whole decision, fail-closed).
func (p UpwardReviewPolicy) Evaluate(parties ApprovalParties, risk model.RiskLevel) error {
	required, err := p.Required(risk)
	if err != nil {
		return err
	}
	if p.CurrentOrgVersion <= 0 {
		return fmt.Errorf("%w: policy has no authoritative org version", ErrStaleOrgReviewer)
	}
	submitter := parties.Submitter
	// A stale submitter snapshot makes the whole hierarchy judgement
	// untrustworthy.
	if submitter.OrgVersion != p.CurrentOrgVersion {
		return fmt.Errorf("%w: submitter %s at org version %d, current %d", ErrStaleOrgReviewer, submitter.UserID, submitter.OrgVersion, p.CurrentOrgVersion)
	}

	seen := make(map[string]bool, len(parties.Reviewers))
	eligible := 0
	for _, r := range parties.Reviewers {
		if r.UserID == submitter.UserID {
			return fmt.Errorf("%w: %s", ErrReviewerIsSubmitter, r.UserID)
		}
		if seen[r.UserID] {
			return fmt.Errorf("%w: %s", ErrDuplicateReviewer, r.UserID)
		}
		seen[r.UserID] = true
		if r.OrgVersion != p.CurrentOrgVersion {
			return fmt.Errorf("%w: reviewer %s at org version %d, current %d", ErrStaleOrgReviewer, r.UserID, r.OrgVersion, p.CurrentOrgVersion)
		}
		switch {
		case r.OrgUnitID == submitter.OrgUnitID:
			return fmt.Errorf("%w: %s", ErrReviewerIsPeer, r.UserID)
		case isAncestorOf(submitter.OrgUnitID, r.OrgPath):
			// submitter's unit is an ancestor of the reviewer's unit -> reviewer
			// is below the submitter.
			return fmt.Errorf("%w: %s", ErrReviewerIsSubordinate, r.UserID)
		case isAncestorOf(r.OrgUnitID, submitter.OrgPath):
			// reviewer's unit is an ancestor of the submitter's unit -> upward.
			eligible++
		default:
			return fmt.Errorf("%w: %s", ErrReviewerNotUpward, r.UserID)
		}
	}
	if eligible < required {
		return fmt.Errorf("%w: have %d, need %d for risk %q", ErrNoUpwardReviewer, eligible, required, risk)
	}
	return nil
}
