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

// TransitionStep records a Step's new StepStatus (and any new Evidence)
// within the current (most recently proposed) WorkPlan of an executing
// case. It requires the case to be workcase.StatusExecuting and the
// transition to be legal per legalStepTransitions. When this transition
// leaves every Step in the current plan workcase.StepCompleted, the case
// becomes workcase.StatusCompleted; a Step transitioning to
// workcase.StepFailed terminates the case (workcase.StatusTerminated).
// These are the only two ways a WorkCase reaches a terminal Status, since
// the closed command inventory has no separate Complete/Terminate command.
func (s *Service) TransitionStep(ctx context.Context, cmd TransitionStepCommand) (workcase.WorkCase, error) {
	if err := validateCommand(cmd.Command); err != nil {
		return workcase.WorkCase{}, err
	}
	if cmd.CaseID == "" || cmd.StepID == "" {
		return workcase.WorkCase{}, fmt.Errorf("%w: case_id and step_id are required", ErrInvalidCommand)
	}
	switch cmd.Status {
	case workcase.StepRunning, workcase.StepCompleted, workcase.StepFailed:
	default:
		return workcase.WorkCase{}, fmt.Errorf("%w: unsupported target step status %q", ErrInvalidCommand, cmd.Status)
	}
	evidence := append([]workcase.EvidenceRef(nil), cmd.Evidence...)

	return s.store.Apply(ctx, cmd.Command, EventStepTransitioned, func(current workcase.WorkCase) (workcase.WorkCase, error) {
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
			if st.ID == cmd.StepID {
				found = i
				break
			}
		}
		if found < 0 {
			return workcase.WorkCase{}, fmt.Errorf("%w: case %s has no step %s in its current plan", ErrInvalidTransition, current.ID, cmd.StepID)
		}
		fromStatus := steps[found].Status
		if fromStatus == "" {
			fromStatus = workcase.StepPending
		}
		if !legalStepTransitions[fromStatus][cmd.Status] {
			return workcase.WorkCase{}, fmt.Errorf("%w: step %s cannot go from %s to %s", ErrInvalidTransition, cmd.StepID, fromStatus, cmd.Status)
		}
		steps[found].Status = cmd.Status
		steps[found].Evidence = append(steps[found].Evidence, evidence...)
		next.Revision = current.Revision + 1

		allCompleted, anyFailed := true, false
		for _, st := range steps {
			if st.Status == workcase.StepFailed {
				anyFailed = true
			}
			if st.Status != workcase.StepCompleted {
				allCompleted = false
			}
		}
		switch {
		case anyFailed:
			next.Status = workcase.StatusTerminated
		case allCompleted:
			next.Status = workcase.StatusCompleted
		}
		return next, nil
	})
}

// Get returns the current snapshot for caseID. It is a thin passthrough to
// Store.Get; see Store.Get for the cross-tenant contract.
func (s *Service) Get(ctx context.Context, enterpriseID, orgScope, caseID string) (workcase.WorkCase, error) {
	return s.store.Get(ctx, enterpriseID, orgScope, caseID)
}
