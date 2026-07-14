package workcase

// action_state.go defines the fine-grained, deterministic Action-execution
// state machine the Orchestrator (Task 0E) advances over. These states are
// DISTINCT from the frozen Task 0A workcase.StepStatus (pending/running/
// completed/failed) and from the frozen AgentNexus nexus.ActionStatus: they
// capture AgentAtlas's own recovery vocabulary -- retrying, replanning,
// compensating, human takeover, termination and (critically) the
// result_unknown state a transport timeout produces. None of these ever drive a
// WorkCase to the forbidden-in-0E "completed" status; completion is gated by a
// satisfied Outcome that Task 0H publishes.

// ActionState is the execution state of a single WorkPlan Step's Action as
// tracked in the Orchestrator's run ledger.
type ActionState string

const (
	// ActionPending: the step's Action has not been dispatched yet.
	ActionPending ActionState = "pending"
	// ActionDispatched: an ActionRequest has been sent for execution and a
	// receipt is being obtained/verified. Persisted BEFORE the side effect so a
	// crash leaves a reconcilable record rather than a silent gap.
	ActionDispatched ActionState = "dispatched"
	// ActionRetrying: a dispatch that provably did NOT execute (rejected before
	// any side effect, or a safe read timeout) may be re-dispatched under the
	// SAME idempotency key. Never used to blind-retry a side effect whose result
	// is unknown.
	ActionRetrying ActionState = "retrying"
	// ActionReplanning: the current plan cannot proceed; a new WorkPlan revision
	// is being proposed (an LLM may propose it, but the Orchestrator validates
	// and persists it -- the model never mutates state).
	ActionReplanning ActionState = "replanning"
	// ActionCompensating: a side effect that executed but failed needs to be
	// undone via its declared compensation Action; the compensation has not yet
	// been claimed for dispatch.
	ActionCompensating ActionState = "compensating"
	// ActionCompensationDispatched: exactly one Advance has ATOMICALLY claimed
	// the compensation for dispatch (compensating -> compensation_dispatched,
	// inside the ledger transaction, BEFORE the reversal side effect). This is
	// the compensation analogue of ActionDispatched: it prevents a second
	// concurrent Advance from double-dispatching a reversal (revert/refund/
	// cancel), which would be directly harmful.
	ActionCompensationDispatched ActionState = "compensation_dispatched"
	// ActionCompensated: the compensation Action completed under a verified
	// receipt; the original side effect is considered reversed. Not a success.
	ActionCompensated ActionState = "compensated"
	// ActionHumanTakeover: the Orchestrator has exhausted its deterministic
	// options and handed the step to a human. Terminal for the automated loop.
	ActionHumanTakeover ActionState = "human_takeover"
	// ActionResultUnknown: a transport timeout (or an unverifiable claim) means
	// we cannot tell whether the side effect executed. Requires explicit
	// reconciliation -- never optimistic success and never a blind retry.
	ActionResultUnknown ActionState = "result_unknown"
	// ActionCompleted: the Action step completed under a verified, signed
	// ActionReceipt. The receipt proves TECHNICAL execution only; the business
	// outcome remains OutcomeUnverified until Task 0G/0H.
	ActionCompleted ActionState = "completed"
	// ActionTerminated: the step reached an unrecoverable terminal state.
	ActionTerminated ActionState = "terminated"
)

// ActionStates returns every valid ActionState in a stable order.
func ActionStates() []ActionState {
	return []ActionState{
		ActionPending, ActionDispatched, ActionRetrying, ActionReplanning,
		ActionCompensating, ActionCompensationDispatched, ActionCompensated, ActionHumanTakeover,
		ActionResultUnknown, ActionCompleted, ActionTerminated,
	}
}

// Valid reports whether s is one of the closed ActionState values. Note that a
// nexus.ActionStatus value (e.g. "succeeded") or the WorkCase "completed"-case
// status is NOT a valid ActionState.
func (s ActionState) Valid() bool {
	switch s {
	case ActionPending, ActionDispatched, ActionRetrying, ActionReplanning,
		ActionCompensating, ActionCompensationDispatched, ActionCompensated, ActionHumanTakeover,
		ActionResultUnknown, ActionCompleted, ActionTerminated:
		return true
	}
	return false
}

// legalActionTransitions is the closed transition graph. A missing edge is a
// safety property: for example result_unknown -> dispatched is absent so a
// timed-out side effect can never be optimistically re-run as if it had
// succeeded; it must go through reconciliation (result_unknown -> retrying only
// after an authoritative non-execution is confirmed, or -> completed once a
// signed receipt is reconciled).
var legalActionTransitions = map[ActionState]map[ActionState]bool{
	ActionPending: {
		ActionDispatched:    true,
		ActionReplanning:    true,
		ActionHumanTakeover: true,
	},
	ActionDispatched: {
		ActionCompleted:     true,
		ActionResultUnknown: true,
		ActionRetrying:      true,
		ActionCompensating:  true,
		ActionReplanning:    true,
		ActionHumanTakeover: true,
	},
	ActionRetrying: {
		ActionDispatched:    true,
		ActionHumanTakeover: true,
	},
	ActionResultUnknown: {
		ActionCompleted:    true,
		ActionRetrying:     true,
		ActionCompensating: true,
		// A reconciled receipt that reports a technical failure may legitimately
		// drive a replan under a replan_then_human policy.
		ActionReplanning:    true,
		ActionHumanTakeover: true,
	},
	ActionReplanning: {
		ActionPending:       true,
		ActionHumanTakeover: true,
		ActionTerminated:    true,
	},
	ActionCompensating: {
		// The reversal must be atomically CLAIMED before it is dispatched, so a
		// concurrent Advance cannot double-dispatch it (there is no direct
		// compensating -> compensated edge).
		ActionCompensationDispatched: true,
		ActionHumanTakeover:          true,
	},
	ActionCompensationDispatched: {
		ActionCompensated:   true,
		ActionHumanTakeover: true,
	},
	ActionCompensated: {
		ActionTerminated: true,
	},
	// ActionCompleted, ActionHumanTakeover, ActionTerminated are terminal.
}

// CanTransitionTo reports whether s -> next is a legal deterministic edge.
func (s ActionState) CanTransitionTo(next ActionState) bool {
	return legalActionTransitions[s][next]
}

// Terminal reports whether the Orchestrator performs no further automatic work
// from s. ActionCompleted, ActionTerminated and ActionHumanTakeover rest here;
// ActionCompensated is a resting state that only a human/replan (out of 0E
// scope) advances, so it is not treated as a self-driving terminal.
func (s ActionState) Terminal() bool {
	switch s {
	case ActionCompleted, ActionTerminated, ActionHumanTakeover:
		return true
	}
	return false
}

// ActionOutcome is the business-verification status of a completed Action step.
// In Task 0E the ONLY reachable value is OutcomeUnverified: a signed
// ActionReceipt proves technical execution but never that the WorkCase Goal was
// achieved. The satisfied/unsatisfied/disputed/blocked/unknown Outcome
// vocabulary is frozen by Task 0G and evaluated by Task 0H -- it is deliberately
// absent here so 0E can never manufacture a business Outcome.
type ActionOutcome string

const (
	// OutcomeUnknown is the zero value: the step has not completed, so no
	// outcome has been recorded.
	OutcomeUnknown ActionOutcome = ""
	// OutcomeUnverified marks a technically-completed Action whose business
	// Outcome has not been (and cannot be, in 0E) verified.
	OutcomeUnverified ActionOutcome = "outcome_unverified"
)

// Valid reports whether o is one of the outcomes 0E can express. Any
// business-satisfaction value (e.g. "satisfied") is intentionally invalid here.
func (o ActionOutcome) Valid() bool {
	switch o {
	case OutcomeUnknown, OutcomeUnverified:
		return true
	}
	return false
}
