package outcome

// policy_test.go (RED first, Task 0H) exercises the PURE, published-rule
// combination lattice in policy.go in isolation from receipt verification and
// persistence: given per-need verdicts plus blocker / human-dispute signals it
// must map deterministically onto the closed OutcomeStatus lattice, with a
// fixed, documented precedence. This is an internal (package outcome) test so it
// can assert on the unexported lattice directly -- the crypto/freshness/store
// behavior is proven end-to-end in evaluator_test.go.

import (
	"testing"

	model "github.com/astraclawteam/agentatlas/sdk/go/outcome"
)

func TestPolicyValidateRejectsEmptyVersionAndDuplicateNeed(t *testing.T) {
	if err := (Policy{Version: ""}).Validate(); err == nil {
		t.Fatal("a policy with no published version must be rejected (rule_version binds the exact policy)")
	}
	dup := Policy{Version: "outcome-policy-rev-7", Required: []RequiredPostcondition{
		{NeedID: "n1", PostconditionID: "p1", Authority: "erp-system-of-record"},
		{NeedID: "n1", PostconditionID: "p2", Authority: "erp-system-of-record"},
	}}
	if err := dup.Validate(); err == nil {
		t.Fatal("a policy declaring the same need twice must be rejected (ambiguous coverage)")
	}
	ok := Policy{Version: "outcome-policy-rev-7", Required: []RequiredPostcondition{
		{NeedID: "n1", PostconditionID: "p1", Authority: "erp-system-of-record"},
	}}
	if err := ok.Validate(); err != nil {
		t.Fatalf("a well-formed policy must validate: %v", err)
	}
}

// TestCombineStatusPrecedence pins the published lattice precedence:
//
//	disputed > blocked > unsatisfied > unknown > satisfied(all) > unverified
//
// This precedence is the load-bearing safety ordering: a genuine authority
// conflict or a human dispute is never silently downgraded to satisfied, an
// external blocker never masquerades as a business result, and satisfied is only
// ever reached when EVERY required need is independently satisfied.
func TestCombineStatusPrecedence(t *testing.T) {
	cases := []struct {
		name          string
		verdicts      []needVerdict
		blocker       bool
		humanDispute  bool
		requiredCount int
		want          model.OutcomeStatus
	}{
		{"all-satisfied", []needVerdict{needSatisfied, needSatisfied}, false, false, 2, model.OutcomeSatisfied},
		{"no-required-needs-is-unverified", nil, false, false, 0, model.OutcomeUnverified},
		{"a-single-unknown-blocks-satisfied", []needVerdict{needSatisfied, needUnknown}, false, false, 2, model.OutcomeUnknown},
		{"a-single-unsatisfied-dominates-unknown", []needVerdict{needUnsatisfied, needUnknown}, false, false, 2, model.OutcomeUnsatisfied},
		{"a-dispute-dominates-unsatisfied", []needVerdict{needDisputed, needUnsatisfied}, false, false, 2, model.OutcomeDisputed},
		{"a-blocker-dominates-unsatisfied", []needVerdict{needUnsatisfied}, true, false, 1, model.OutcomeBlocked},
		{"a-dispute-dominates-a-blocker", []needVerdict{needSatisfied}, true, true, 1, model.OutcomeDisputed},
		{"a-human-dispute-alone-is-disputed", []needVerdict{needSatisfied}, false, true, 1, model.OutcomeDisputed},
		{"a-blocker-with-all-satisfied-is-blocked", []needVerdict{needSatisfied}, true, false, 1, model.OutcomeBlocked},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := combineStatus(tc.verdicts, tc.blocker, tc.humanDispute, tc.requiredCount)
			if got != tc.want {
				t.Fatalf("combineStatus(%v, blocker=%v, humanDispute=%v, req=%d) = %q, want %q",
					tc.verdicts, tc.blocker, tc.humanDispute, tc.requiredCount, got, tc.want)
			}
		})
	}
}

// TestCombineStatusNeverSatisfiedWithoutRequired proves the crux: satisfied is
// unreachable unless there is at least one required need AND every verdict is
// satisfied. A verdicts list that is empty (nothing required, nothing observed)
// can never be satisfied -- it is at most unverified.
func TestCombineStatusNeverSatisfiedWithoutRequired(t *testing.T) {
	if got := combineStatus(nil, false, false, 0); got == model.OutcomeSatisfied {
		t.Fatal("satisfied must be unreachable with zero required needs")
	}
	// One required need but its verdict is merely unverified/unknown -> not satisfied.
	if got := combineStatus([]needVerdict{needUnknown}, false, false, 1); got == model.OutcomeSatisfied {
		t.Fatal("satisfied must be unreachable while any required need is unresolved")
	}
}
