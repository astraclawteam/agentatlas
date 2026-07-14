package workcase

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"

	"github.com/astraclawteam/agentatlas/sdk/go/workcase"
)

const (
	minIdempotencyKeyLen = 16
	maxIdempotencyKeyLen = 128
)

// Service is the deterministic aggregate service for WorkCase: it owns the
// state-machine rules (valid Status/StepStatus transitions and WorkPlan
// revision immutability) and delegates concurrency, tenancy and durability
// to a Store.
type Service struct {
	store  Store
	nextID func(enterpriseID, idempotencyKey string) (string, error)
}

// NewService constructs a Service over store. newID generates a CaseID for
// a CreateCommand that does not supply one; a nil newID uses a
// deterministic default (see deriveCaseID). newID MUST be a pure function
// of (enterpriseID, idempotencyKey) -- a random or otherwise
// non-deterministic generator would break idempotent replay: two Create
// calls carrying the same IdempotencyKey have to land on the same CaseID
// *before* either one ever reaches the Store, since Store's own
// idempotency check only fires once it already has a CaseID to compare
// against.
func NewService(store Store, newID func(enterpriseID, idempotencyKey string) (string, error)) (*Service, error) {
	if store == nil {
		return nil, errors.New("workcase: service requires a store")
	}
	if newID == nil {
		newID = deriveCaseID
	}
	return &Service{store: store, nextID: newID}, nil
}

// deriveCaseID deterministically derives a CaseID from (enterpriseID,
// idempotencyKey) so a replayed Create (same enterprise, same idempotency
// key, no caller-supplied CaseID) always targets the same case.
func deriveCaseID(enterpriseID, idempotencyKey string) (string, error) {
	sum := sha256.Sum256([]byte(enterpriseID + "\x00" + idempotencyKey))
	return "case_" + hex.EncodeToString(sum[:16]), nil
}

func validateCommand(cmd Command) error {
	if cmd.EnterpriseID == "" || cmd.OrgScope == "" || cmd.ActorRef == "" {
		return fmt.Errorf("%w: enterprise_id, org_scope and actor_ref are required", ErrInvalidCommand)
	}
	if len(cmd.IdempotencyKey) < minIdempotencyKeyLen || len(cmd.IdempotencyKey) > maxIdempotencyKeyLen {
		return fmt.Errorf("%w: idempotency_key must be %d-%d characters", ErrInvalidCommand, minIdempotencyKeyLen, maxIdempotencyKeyLen)
	}
	return nil
}

// Create starts a new WorkCase in workcase.StatusDraft at revision 1.
func (s *Service) Create(ctx context.Context, cmd CreateCommand) (workcase.WorkCase, error) {
	if err := validateCommand(cmd.Command); err != nil {
		return workcase.WorkCase{}, err
	}
	if cmd.ExpectedRevision != 0 {
		return workcase.WorkCase{}, fmt.Errorf("%w: create expects revision 0", ErrInvalidCommand)
	}
	caseID := cmd.CaseID
	if caseID == "" {
		id, err := s.nextID(cmd.EnterpriseID, cmd.IdempotencyKey)
		if err != nil {
			return workcase.WorkCase{}, err
		}
		caseID = id
	}
	cmd.CaseID = caseID
	enterpriseID, orgScope, actorRef := cmd.EnterpriseID, cmd.OrgScope, cmd.ActorRef
	return s.store.Apply(ctx, cmd.Command, EventCaseCreated, func(workcase.WorkCase) (workcase.WorkCase, error) {
		return workcase.WorkCase{
			ID:           caseID,
			EnterpriseID: enterpriseID,
			OrgScope:     orgScope,
			ActorRef:     actorRef,
			Status:       workcase.StatusDraft,
			Revision:     1,
		}, nil
	})
}

// ProposePlan appends a new, immutable WorkPlan revision to case CaseID.
// Existing WorkPlan revisions are never mutated in place: ProposePlan
// always appends, so a plan already reviewing/executing is trivially left
// untouched, satisfied by construction rather than by a conditional check.
// ProposePlan is rejected once the case is workcase.StatusCompleted or
// workcase.StatusTerminated (nothing further can be proposed for a
// finished case).
func (s *Service) ProposePlan(ctx context.Context, cmd ProposePlanCommand) (workcase.WorkCase, error) {
	if err := validateCommand(cmd.Command); err != nil {
		return workcase.WorkCase{}, err
	}
	if cmd.CaseID == "" {
		return workcase.WorkCase{}, fmt.Errorf("%w: case_id is required", ErrInvalidCommand)
	}
	if err := workcase.ValidatePlan(cmd.Plan); err != nil {
		return workcase.WorkCase{}, fmt.Errorf("%w: %v", ErrInvalidCommand, err)
	}
	plan := clonePlan(cmd.Plan)
	return s.store.Apply(ctx, cmd.Command, EventPlanProposed, func(current workcase.WorkCase) (workcase.WorkCase, error) {
		if current.Status == workcase.StatusCompleted || current.Status == workcase.StatusTerminated {
			return workcase.WorkCase{}, fmt.Errorf("%w: case %s is %s", ErrInvalidTransition, current.ID, current.Status)
		}
		next := cloneCase(current)
		nextPlanRevision := uint64(1)
		if n := len(next.Plans); n > 0 {
			nextPlanRevision = next.Plans[n-1].Revision + 1
		}
		proposed := plan
		proposed.Revision = nextPlanRevision
		next.Plans = append(next.Plans, proposed)
		next.Revision = current.Revision + 1
		return next, nil
	})
}

// StartReview transitions a case from workcase.StatusDraft to
// workcase.StatusReviewing. The case must already have at least one
// proposed WorkPlan.
func (s *Service) StartReview(ctx context.Context, cmd StartReviewCommand) (workcase.WorkCase, error) {
	if err := validateCommand(cmd.Command); err != nil {
		return workcase.WorkCase{}, err
	}
	if cmd.CaseID == "" {
		return workcase.WorkCase{}, fmt.Errorf("%w: case_id is required", ErrInvalidCommand)
	}
	return s.store.Apply(ctx, cmd.Command, EventReviewStarted, func(current workcase.WorkCase) (workcase.WorkCase, error) {
		if current.Status != workcase.StatusDraft {
			return workcase.WorkCase{}, fmt.Errorf("%w: case %s is %s, not draft", ErrInvalidTransition, current.ID, current.Status)
		}
		if len(current.Plans) == 0 {
			return workcase.WorkCase{}, fmt.Errorf("%w: case %s has no proposed plan", ErrInvalidTransition, current.ID)
		}
		next := cloneCase(current)
		next.Status = workcase.StatusReviewing
		next.Revision = current.Revision + 1
		return next, nil
	})
}

// StartExecution transitions a case from workcase.StatusReviewing to
// workcase.StatusExecuting.
func (s *Service) StartExecution(ctx context.Context, cmd StartExecutionCommand) (workcase.WorkCase, error) {
	if err := validateCommand(cmd.Command); err != nil {
		return workcase.WorkCase{}, err
	}
	if cmd.CaseID == "" {
		return workcase.WorkCase{}, fmt.Errorf("%w: case_id is required", ErrInvalidCommand)
	}
	return s.store.Apply(ctx, cmd.Command, EventExecutionStarted, func(current workcase.WorkCase) (workcase.WorkCase, error) {
		if current.Status != workcase.StatusReviewing {
			return workcase.WorkCase{}, fmt.Errorf("%w: case %s is %s, not reviewing", ErrInvalidTransition, current.ID, current.Status)
		}
		next := cloneCase(current)
		next.Status = workcase.StatusExecuting
		next.Revision = current.Revision + 1
		return next, nil
	})
}

// legalStepTransitions is the closed StepStatus state machine: pending is
// the implicit zero value (Task 0A's Step.Status is "omitempty"), running
// may complete or fail, and completed/failed are terminal.
var legalStepTransitions = map[workcase.StepStatus]map[workcase.StepStatus]bool{
	workcase.StepPending: {workcase.StepRunning: true},
	workcase.StepRunning: {workcase.StepCompleted: true, workcase.StepFailed: true},
}

// AdvanceStepCommand records a Step status transition on behalf of the Task 0E
// Orchestrator. It is the SAME shape as TransitionStepCommand but is applied
// through Service.AdvanceStep, which deliberately does NOT drive the WorkCase's
// own Status: see AdvanceStep. It lives here (rather than in store.go with the
// other commands) because it is an orchestration-facing entry point added in
// Task 0E.
type AdvanceStepCommand struct {
	Command
	StepID   string
	Status   workcase.StepStatus
	Evidence []workcase.EvidenceRef
}

// stepTransition is the shared core of TransitionStep and AdvanceStep. It
// validates and applies a single Step status change within the current
// (most-recently-proposed) WorkPlan of an executing case and NEVER moves the
// WorkCase's own Status.
//
// Task 0H unification: a step transition can no longer complete or terminate a
// case. Completion is reachable ONLY through Service.CompleteCase (the
// Outcome-gated path), and termination ONLY through Service.TerminateCase (the
// governed path). This makes "completed" reachable by exactly ONE route, and
// that route always requires a satisfied Outcome (verified by
// outcome.ClosureService before it calls CompleteCase). TransitionStep and
// AdvanceStep are therefore now behaviourally identical; TransitionStep remains
// as the direct-command name used by the Task 0A/0B tests.
func (s *Service) stepTransition(ctx context.Context, cmd Command, stepID string, status workcase.StepStatus, evidence []workcase.EvidenceRef) (workcase.WorkCase, error) {
	if err := validateCommand(cmd); err != nil {
		return workcase.WorkCase{}, err
	}
	if cmd.CaseID == "" || stepID == "" {
		return workcase.WorkCase{}, fmt.Errorf("%w: case_id and step_id are required", ErrInvalidCommand)
	}
	switch status {
	case workcase.StepRunning, workcase.StepCompleted, workcase.StepFailed:
	default:
		return workcase.WorkCase{}, fmt.Errorf("%w: unsupported target step status %q", ErrInvalidCommand, status)
	}
	ev := append([]workcase.EvidenceRef(nil), evidence...)

	return s.store.Apply(ctx, cmd, EventStepTransitioned, func(current workcase.WorkCase) (workcase.WorkCase, error) {
		if current.Status != workcase.StatusExecuting {
			return workcase.WorkCase{}, fmt.Errorf("%w: case %s is %s, not executing", ErrInvalidTransition, current.ID, current.Status)
		}
		next := cloneCase(current)
		top := len(next.Plans) - 1
		if top < 0 {
			return workcase.WorkCase{}, fmt.Errorf("%w: case %s has no plan", ErrInvalidTransition, current.ID)
		}
		steps := next.Plans[top].Steps
		found := -1
		for i, st := range steps {
			if st.ID == stepID {
				found = i
				break
			}
		}
		if found < 0 {
			return workcase.WorkCase{}, fmt.Errorf("%w: case %s has no step %s in its current plan", ErrInvalidTransition, current.ID, stepID)
		}
		fromStatus := steps[found].Status
		if fromStatus == "" {
			fromStatus = workcase.StepPending
		}
		if !legalStepTransitions[fromStatus][status] {
			return workcase.WorkCase{}, fmt.Errorf("%w: step %s cannot go from %s to %s", ErrInvalidTransition, stepID, fromStatus, status)
		}
		steps[found].Status = status
		steps[found].Evidence = append(steps[found].Evidence, ev...)
		next.Revision = current.Revision + 1
		return next, nil
	})
}

// TransitionStep records a Step's new StepStatus (and any new Evidence) within
// the current (most recently proposed) WorkPlan of an executing case. It
// requires the case to be workcase.StatusExecuting and the transition to be
// legal per legalStepTransitions. As of Task 0H it NO LONGER drives the
// WorkCase's own Status: an all-completed plan leaves the case executing (the
// case completes only through the Outcome-gated Service.CompleteCase), and a
// StepFailed no longer auto-terminates (termination is the governed
// Service.TerminateCase). It is now behaviourally identical to AdvanceStep.
func (s *Service) TransitionStep(ctx context.Context, cmd TransitionStepCommand) (workcase.WorkCase, error) {
	return s.stepTransition(ctx, cmd.Command, cmd.StepID, cmd.Status, cmd.Evidence)
}

// AdvanceStep records a Step status transition for the Task 0E Orchestrator
// WITHOUT touching the WorkCase's own Status. An orchestrated plan whose every
// Step is workcase.StepCompleted leaves the case StatusExecuting -- the
// orchestrator can never reach StatusCompleted, which is gated on a satisfied
// Outcome (Task 0H). The step-status transition itself is validated exactly like
// TransitionStep (executing case, legal StepStatus edge).
func (s *Service) AdvanceStep(ctx context.Context, cmd AdvanceStepCommand) (workcase.WorkCase, error) {
	return s.stepTransition(ctx, cmd.Command, cmd.StepID, cmd.Status, cmd.Evidence)
}

// CompleteCase transitions an executing WorkCase to workcase.StatusCompleted. It
// is the ONE and ONLY production path to StatusCompleted, and it is Outcome-gated:
// it requires a non-empty outcomeRef, and its sole authorized caller --
// outcome.ClosureService.Apply -- invokes it only AFTER re-reading the referenced
// Outcome and verifying it is `satisfied` at read time (never trusting the
// caller). Because the step-lifecycle transitions can no longer complete a case,
// a WorkCase reaches completed ONLY through a satisfied terminal Outcome.
//
// The transition is recorded as an EventStepTransitioned event -- the only
// event_type the frozen workcase_events CHECK admits (a dedicated
// case_completed event_type is deferred to the next task that adds a migration;
// Task 0H is core-code-only) -- whose payload is the completed WorkCase, so event
// replay reconstructs it faithfully. Optimistic concurrency (expectedRevision)
// and idempotency (idempotencyKey) make a duplicate/redelivered closure safe:
// the same key replays the recorded result rather than transitioning twice.
func (s *Service) CompleteCase(ctx context.Context, enterpriseID, orgScope, actorRef, caseID string, expectedRevision uint64, idempotencyKey, outcomeRef string) (workcase.WorkCase, error) {
	cmd := Command{EnterpriseID: enterpriseID, OrgScope: orgScope, ActorRef: actorRef, CaseID: caseID, ExpectedRevision: expectedRevision, IdempotencyKey: idempotencyKey}
	if err := validateCommand(cmd); err != nil {
		return workcase.WorkCase{}, err
	}
	if cmd.CaseID == "" {
		return workcase.WorkCase{}, fmt.Errorf("%w: case_id is required", ErrInvalidCommand)
	}
	if outcomeRef == "" {
		return workcase.WorkCase{}, fmt.Errorf("%w: an outcome reference is required (a case completes only through a satisfied Outcome)", ErrInvalidCommand)
	}
	return s.store.Apply(ctx, cmd, EventStepTransitioned, func(current workcase.WorkCase) (workcase.WorkCase, error) {
		if current.Status != workcase.StatusExecuting {
			return workcase.WorkCase{}, fmt.Errorf("%w: case %s is %s, not executing (only an executing case may complete)", ErrInvalidTransition, current.ID, current.Status)
		}
		next := cloneCase(current)
		next.Status = workcase.StatusCompleted
		next.Revision = current.Revision + 1
		return next, nil
	})
}

// TerminateCase transitions an executing WorkCase to workcase.StatusTerminated
// (governed termination). It is the governed path the orchestrator drives when a
// business Outcome is unsatisfied and replanning is unavailable/exhausted. It
// carries a required reason for the audit trail and, like CompleteCase, records
// an EventStepTransitioned event (the only admitted event_type) whose payload is
// the terminated WorkCase.
func (s *Service) TerminateCase(ctx context.Context, enterpriseID, orgScope, actorRef, caseID string, expectedRevision uint64, idempotencyKey, reason string) (workcase.WorkCase, error) {
	cmd := Command{EnterpriseID: enterpriseID, OrgScope: orgScope, ActorRef: actorRef, CaseID: caseID, ExpectedRevision: expectedRevision, IdempotencyKey: idempotencyKey}
	if err := validateCommand(cmd); err != nil {
		return workcase.WorkCase{}, err
	}
	if cmd.CaseID == "" {
		return workcase.WorkCase{}, fmt.Errorf("%w: case_id is required", ErrInvalidCommand)
	}
	if reason == "" {
		return workcase.WorkCase{}, fmt.Errorf("%w: a termination reason is required", ErrInvalidCommand)
	}
	return s.store.Apply(ctx, cmd, EventStepTransitioned, func(current workcase.WorkCase) (workcase.WorkCase, error) {
		if current.Status != workcase.StatusExecuting {
			return workcase.WorkCase{}, fmt.Errorf("%w: case %s is %s, not executing (only an executing case may be terminated)", ErrInvalidTransition, current.ID, current.Status)
		}
		next := cloneCase(current)
		next.Status = workcase.StatusTerminated
		next.Revision = current.Revision + 1
		return next, nil
	})
}

// Get returns the current snapshot for caseID. It is a thin passthrough to
// Store.Get; see Store.Get for the cross-tenant contract.
func (s *Service) Get(ctx context.Context, enterpriseID, orgScope, caseID string) (workcase.WorkCase, error) {
	return s.store.Get(ctx, enterpriseID, orgScope, caseID)
}
