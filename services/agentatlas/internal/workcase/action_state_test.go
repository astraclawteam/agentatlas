package workcase_test

import (
	"testing"

	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/workcase"
)

// TestActionStateValidRejectsUnknown pins the closed action-execution state
// vocabulary Task 0E introduces (distinct from the frozen Task 0A StepStatus):
// every named state is Valid, and anything else is not.
func TestActionStateValidRejectsUnknown(t *testing.T) {
	for _, s := range workcase.ActionStates() {
		if !s.Valid() {
			t.Errorf("state %q reported invalid but is in ActionStates()", s)
		}
	}
	if workcase.ActionState("succeeded").Valid() {
		t.Fatal("a nexus ActionStatus value must not be a workcase ActionState")
	}
	if workcase.ActionState("completed_case").Valid() {
		t.Fatal("unknown action state reported valid")
	}
	// The forbidden-in-0E WorkCase status must never leak into this vocabulary.
	if workcase.ActionState("completed").Valid() && !containsState(workcase.ActionStates(), workcase.ActionCompleted) {
		t.Fatal("action state vocabulary is inconsistent")
	}
}

func containsState(states []workcase.ActionState, want workcase.ActionState) bool {
	for _, s := range states {
		if s == want {
			return true
		}
	}
	return false
}

// TestActionStateExplicitStatesExist proves the six explicit states the plan
// (Task 0E) requires by name are all present and distinct.
func TestActionStateExplicitStatesExist(t *testing.T) {
	required := []workcase.ActionState{
		workcase.ActionRetrying,
		workcase.ActionReplanning,
		workcase.ActionCompensating,
		workcase.ActionHumanTakeover,
		workcase.ActionTerminated,
		workcase.ActionResultUnknown,
	}
	seen := map[workcase.ActionState]bool{}
	for _, s := range required {
		if !s.Valid() {
			t.Errorf("plan-required state %q is not valid", s)
		}
		if seen[s] {
			t.Errorf("state %q duplicated", s)
		}
		seen[s] = true
	}
}

// TestActionStateLegalTransitions locks the deterministic transition graph the
// orchestrator advances over. Each legal edge below is a fact the failure
// semantics depend on; each illegal edge is a safety property.
func TestActionStateLegalTransitions(t *testing.T) {
	legal := [][2]workcase.ActionState{
		{workcase.ActionPending, workcase.ActionDispatched},
		{workcase.ActionPending, workcase.ActionHumanTakeover},
		{workcase.ActionDispatched, workcase.ActionCompleted},
		{workcase.ActionDispatched, workcase.ActionResultUnknown},
		{workcase.ActionDispatched, workcase.ActionRetrying},
		{workcase.ActionDispatched, workcase.ActionCompensating},
		{workcase.ActionDispatched, workcase.ActionReplanning},
		{workcase.ActionDispatched, workcase.ActionHumanTakeover},
		{workcase.ActionRetrying, workcase.ActionDispatched},
		{workcase.ActionRetrying, workcase.ActionHumanTakeover},
		{workcase.ActionResultUnknown, workcase.ActionCompleted},
		{workcase.ActionResultUnknown, workcase.ActionRetrying},
		{workcase.ActionResultUnknown, workcase.ActionCompensating},
		// A reconciled receipt that reports failure may legitimately replan.
		{workcase.ActionResultUnknown, workcase.ActionReplanning},
		{workcase.ActionResultUnknown, workcase.ActionHumanTakeover},
		{workcase.ActionReplanning, workcase.ActionPending},
		{workcase.ActionReplanning, workcase.ActionHumanTakeover},
		// Compensation is atomically CLAIMED (compensating -> compensation_dispatched)
		// before the reversal is dispatched, then resolved.
		{workcase.ActionCompensating, workcase.ActionCompensationDispatched},
		{workcase.ActionCompensating, workcase.ActionHumanTakeover},
		{workcase.ActionCompensationDispatched, workcase.ActionCompensated},
		{workcase.ActionCompensationDispatched, workcase.ActionHumanTakeover},
		{workcase.ActionCompensated, workcase.ActionTerminated},
	}
	for _, e := range legal {
		if !e[0].CanTransitionTo(e[1]) {
			t.Errorf("expected legal transition %s -> %s to be permitted", e[0], e[1])
		}
	}

	illegal := [][2]workcase.ActionState{
		// A step can never complete without first being dispatched.
		{workcase.ActionPending, workcase.ActionCompleted},
		// A transport timeout must never be optimistically treated as success
		// by jumping straight back to dispatched then completed; result_unknown
		// must go through reconciliation, never blind success.
		{workcase.ActionResultUnknown, workcase.ActionDispatched},
		// Terminal states have no successors.
		{workcase.ActionCompleted, workcase.ActionDispatched},
		{workcase.ActionCompleted, workcase.ActionRetrying},
		{workcase.ActionTerminated, workcase.ActionDispatched},
		{workcase.ActionHumanTakeover, workcase.ActionCompleted},
		// Compensation is not a substitute for a verified completion.
		{workcase.ActionCompensating, workcase.ActionCompleted},
		// The reversal cannot skip its atomic claim: no direct compensating ->
		// compensated edge (it must pass through compensation_dispatched).
		{workcase.ActionCompensating, workcase.ActionCompensated},
		// A compensation claim is not a primary dispatch.
		{workcase.ActionCompensationDispatched, workcase.ActionDispatched},
	}
	for _, e := range illegal {
		if e[0].CanTransitionTo(e[1]) {
			t.Errorf("expected illegal transition %s -> %s to be forbidden", e[0], e[1])
		}
	}
}

// TestActionStateTerminal marks exactly the states from which the orchestrator
// performs no further automatic work.
func TestActionStateTerminal(t *testing.T) {
	terminal := map[workcase.ActionState]bool{
		workcase.ActionCompleted:     true,
		workcase.ActionTerminated:    true,
		workcase.ActionHumanTakeover: true,
	}
	for _, s := range workcase.ActionStates() {
		if s.Terminal() != terminal[s] {
			t.Errorf("state %q Terminal()=%v, want %v", s, s.Terminal(), terminal[s])
		}
	}
}

// TestActionOutcomeOnlyUnverifiedReachableInTask0E proves the heart of the 0E
// completion prohibition at the type level: the only completed-action outcome
// this package can express is outcome_unverified. There is deliberately no
// "satisfied"/"completed" business-outcome constant here -- that vocabulary is
// frozen by Task 0G and evaluated by Task 0H, never manufactured in 0E.
func TestActionOutcomeOnlyUnverifiedReachableInTask0E(t *testing.T) {
	if !workcase.OutcomeUnverified.Valid() {
		t.Fatal("outcome_unverified must be a valid action outcome")
	}
	if string(workcase.OutcomeUnverified) != "outcome_unverified" {
		t.Fatalf("outcome_unverified has unexpected wire value %q", workcase.OutcomeUnverified)
	}
	if !workcase.OutcomeUnknown.Valid() {
		t.Fatal("the zero-value (not-yet-completed) outcome must be valid")
	}
	// Any business-satisfaction vocabulary must be rejected here.
	for _, forbidden := range []workcase.ActionOutcome{"satisfied", "outcome_satisfied", "completed", "success"} {
		if forbidden.Valid() {
			t.Fatalf("business-outcome value %q must not be expressible as a 0E action outcome", forbidden)
		}
	}
}
