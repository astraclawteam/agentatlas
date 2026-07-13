// Package workcase persists the WorkCase aggregate frozen by Task 0A
// (github.com/astraclawteam/agentatlas/sdk/go/workcase): one governed case
// of work per enterprise, its immutable WorkPlan revisions, and the
// append-only event log that produced its current snapshot. This package
// is the deterministic persistence and state-machine layer only -- no
// connector endpoints, credentials, raw enterprise content or HR
// vocabulary, and no LLM calls.
package workcase

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/astraclawteam/agentatlas/sdk/go/workcase"
)

// Sentinel errors returned by Service and Store. Callers should compare
// with errors.Is; concrete implementations may wrap these with additional
// context.
var (
	// ErrNotFound is returned when a case does not exist for the given
	// enterprise and exact org scope. A case that exists under a different
	// enterprise or a different org scope is indistinguishable from one
	// that does not exist at all: both return ErrNotFound, never a
	// separate forbidden/leaked-existence error.
	ErrNotFound = errors.New("workcase: not found")
	// ErrStaleRevision is returned when a command's ExpectedRevision does
	// not match the aggregate's current Revision. This also covers a
	// Create command targeting a CaseID that already exists -- under ANY
	// enterprise or org scope, because CaseIDs are globally unique,
	// mirroring the workcases table's global primary key (the caller's
	// implicit expectation of revision 0 no longer matches reality) -- and
	// a concurrent writer that lost a race against another command applied
	// at the same expected revision.
	ErrStaleRevision = errors.New("workcase: stale revision")
	// ErrInvalidCommand is returned when a command fails basic shape
	// validation: a missing EnterpriseID/OrgScope/ActorRef/CaseID, an
	// out-of-bounds IdempotencyKey, or (for ProposePlan) a WorkPlan that
	// fails workcase.ValidatePlan.
	ErrInvalidCommand = errors.New("workcase: invalid command")
	// ErrInvalidTransition is returned when a command is well-formed and
	// targets a real, correctly-scoped, non-stale case, but the case's
	// current Status (or a Step's current StepStatus) does not permit the
	// requested transition.
	ErrInvalidTransition = errors.New("workcase: invalid state transition")
	// ErrIdempotencyKeyReused is returned when an IdempotencyKey already
	// recorded against a different CaseID is presented again from the
	// recorded case's own org scope (a wrong-org caller sees ErrNotFound
	// instead -- see Store.Apply), whether sequentially or as the loser of
	// two concurrent commands racing the same brand-new key. This is a
	// genuine conflict and is never silently treated as a replay.
	ErrIdempotencyKeyReused = errors.New("workcase: idempotency key reused for a different command")
	// ErrEventLogConflict signals snapshot/event-log divergence inside a
	// single Apply: the snapshot write succeeded at a revision whose
	// append-only event seq row already exists. Because seq is defined to
	// equal the revision the event produced and both writes share one
	// transaction, this can only mean an out-of-band write into
	// workcase_events or genuine corruption -- unlike ErrStaleRevision or
	// ErrIdempotencyKeyReused it is never caller-correctable (retrying
	// cannot help), so it is kept distinct from both.
	ErrEventLogConflict = errors.New("workcase: event log seq conflict (snapshot/event divergence)")
)

// EventType identifies the kind of fact recorded in the append-only
// workcase_events log.
type EventType string

const (
	EventCaseCreated      EventType = "case_created"
	EventPlanProposed     EventType = "plan_proposed"
	EventReviewStarted    EventType = "review_started"
	EventExecutionStarted EventType = "execution_started"
	EventStepTransitioned EventType = "step_transitioned"
)

// Command is the envelope every mutating Service call requires: the exact
// tenant/org scope being addressed, the acting identity, the aggregate id,
// the caller's expected current revision (optimistic concurrency) and an
// idempotency key for safe command replay.
type Command struct {
	EnterpriseID     string
	OrgScope         string
	ActorRef         string
	CaseID           string
	ExpectedRevision uint64
	IdempotencyKey   string
}

// CreateCommand starts a new WorkCase. CaseID is optional: a blank CaseID
// asks the Service to generate one. ExpectedRevision must be 0 (the caller
// asserts no case exists yet at CaseID).
type CreateCommand struct {
	Command
}

// ProposePlanCommand appends a new, immutable WorkPlan revision to a case.
type ProposePlanCommand struct {
	Command
	Plan workcase.WorkPlan
}

// StartReviewCommand transitions a case from draft to reviewing.
type StartReviewCommand struct {
	Command
}

// StartExecutionCommand transitions a case from reviewing to executing.
type StartExecutionCommand struct {
	Command
}

// TransitionStepCommand records a Step status transition (and any new
// Evidence) within an executing case's current WorkPlan.
type TransitionStepCommand struct {
	Command
	StepID   string
	Status   workcase.StepStatus
	Evidence []workcase.EvidenceRef
}

// Mutate computes the next aggregate state from the current one. For an
// EventCaseCreated application current is the zero value (there is nothing
// to read yet); for every other EventType, Store guarantees current is the
// real, tenant/org-scoped, revision-matched snapshot. Mutate returns a
// domain error (for example ErrInvalidTransition) to reject the command
// with no persistence side effect at all.
type Mutate func(current workcase.WorkCase) (workcase.WorkCase, error)

// Store is the persistence port for the WorkCase aggregate: one snapshot
// row per case plus one append-only event per successfully applied
// command, written together in a single transaction.
type Store interface {
	// Get returns the current snapshot for caseID, scoped to enterpriseID
	// and the exact orgScope. See ErrNotFound for the cross-tenant
	// contract: a case under a different enterprise or org scope is
	// reported identically to a case that does not exist.
	Get(ctx context.Context, enterpriseID, orgScope, caseID string) (workcase.WorkCase, error)

	// Apply loads the current snapshot for cmd (tenant/org-scoped; the
	// zero value when eventType is EventCaseCreated), enforces optimistic
	// concurrency (the stored revision, or 0 for a not-yet-existing case,
	// must equal cmd.ExpectedRevision -- otherwise ErrStaleRevision), calls
	// mutate to compute the next aggregate state, and persists next plus
	// one new Event atomically: both commit together or neither does.
	//
	// Idempotency: a previously recorded cmd.IdempotencyKey short-circuits
	// all of this, before mutate is ever invoked, and replays the original
	// recorded result -- provided the replay is addressed consistently.
	// cmd.OrgScope must match the recorded case's org scope (a mismatch is
	// ErrNotFound, the same answer any other wrongly-scoped access gets,
	// so a wrong-org caller cannot even learn the key exists), and
	// cmd.CaseID must match the recorded case (a different CaseID from the
	// correct org scope is ErrIdempotencyKeyReused). Two concurrent
	// commands racing the SAME brand-new key are also safe: at most one
	// wins; a loser targeting a different CaseID gets
	// ErrIdempotencyKeyReused, and a loser targeting the same CaseID gets
	// ErrStaleRevision (its retry then replays normally).
	//
	// CaseIDs are globally unique across all enterprises and org scopes
	// (mirroring the workcases table's global primary key): an
	// EventCaseCreated command whose CaseID exists anywhere fails with
	// ErrStaleRevision, even when the existing case is invisible to the
	// caller's own enterprise/org scope.
	Apply(ctx context.Context, cmd Command, eventType EventType, mutate Mutate) (workcase.WorkCase, error)
}

// cloneCase returns a deep copy of c so callers holding next/current values
// across a Mutate boundary (or across a MemoryStore read/write) never alias
// slices with the store's own state or with each other.
func cloneCase(c workcase.WorkCase) workcase.WorkCase {
	out := c
	if c.Plans != nil {
		out.Plans = make([]workcase.WorkPlan, len(c.Plans))
		for i, p := range c.Plans {
			out.Plans[i] = clonePlan(p)
		}
	}
	return out
}

func clonePlan(p workcase.WorkPlan) workcase.WorkPlan {
	out := p
	if p.Steps != nil {
		out.Steps = make([]workcase.Step, len(p.Steps))
		for i, st := range p.Steps {
			out.Steps[i] = cloneStep(st)
		}
	}
	return out
}

func cloneStep(st workcase.Step) workcase.Step {
	out := st
	if st.Evidence != nil {
		out.Evidence = append([]workcase.EvidenceRef(nil), st.Evidence...)
	}
	if st.Action != nil {
		a := *st.Action
		if st.Action.Preconditions != nil {
			a.Preconditions = append([]workcase.Precondition(nil), st.Action.Preconditions...)
		}
		if st.Action.CompensationActionID != nil {
			id := *st.Action.CompensationActionID
			a.CompensationActionID = &id
		}
		out.Action = &a
	}
	return out
}

// MemoryStore is an in-memory Store used by service_test.go to exercise
// Service's business logic without a database. It enforces exactly the
// same contract as PostgresStore -- global CaseID uniqueness (the cases
// map is keyed by bare CaseID, mirroring workcases' global primary key,
// so a create whose id exists under ANY enterprise or org scope fails
// with ErrStaleRevision while non-create commands addressed outside their
// exact enterprise+org scope see ErrNotFound), idempotency-first replay
// gated on matching OrgScope, and optimistic concurrency -- so tests
// written against it exercise real Service behavior, not a simplified
// stand-in. The shared conformance suite in postgres_integration_test.go
// (TestPostgresAndMemoryStoreConformance) pins both implementations to
// identical typed outcomes.
type MemoryStore struct {
	mu          sync.Mutex
	now         func() time.Time
	cases       map[string]workcase.WorkCase // keyed by bare CaseID -- global, mirroring workcases_pkey
	idempotency map[string]idempotencyRecord // keyed by enterpriseID + key -- per-enterprise, mirroring workcase_events_idempotency_uniq
	events      map[string][]storedEvent     // keyed by bare CaseID
}

type idempotencyRecord struct {
	caseID string
	result workcase.WorkCase
}

type storedEvent struct {
	Seq       uint64
	Type      EventType
	Payload   workcase.WorkCase
	CreatedAt time.Time
}

// NewMemoryStore constructs an empty MemoryStore. now defaults to
// time.Now.
func NewMemoryStore(now func() time.Time) *MemoryStore {
	if now == nil {
		now = time.Now
	}
	return &MemoryStore{
		now:         now,
		cases:       map[string]workcase.WorkCase{},
		idempotency: map[string]idempotencyRecord{},
		events:      map[string][]storedEvent{},
	}
}

func memKey(enterpriseID, id string) string { return enterpriseID + "\x00" + id }

func (s *MemoryStore) Get(_ context.Context, enterpriseID, orgScope, caseID string) (workcase.WorkCase, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, ok := s.cases[caseID]
	if !ok || c.EnterpriseID != enterpriseID || c.OrgScope != orgScope {
		return workcase.WorkCase{}, ErrNotFound
	}
	return cloneCase(c), nil
}

func (s *MemoryStore) Apply(_ context.Context, cmd Command, eventType EventType, mutate Mutate) (workcase.WorkCase, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	idemKey := memKey(cmd.EnterpriseID, cmd.IdempotencyKey)
	if rec, ok := s.idempotency[idemKey]; ok {
		// Org scope gates the replay before anything about the recorded
		// command is disclosed (same rule as PostgresStore.Apply): a
		// wrong-org caller learns only not-found, never that the key
		// exists.
		if rec.result.OrgScope != cmd.OrgScope {
			return workcase.WorkCase{}, ErrNotFound
		}
		if rec.caseID != cmd.CaseID {
			return workcase.WorkCase{}, ErrIdempotencyKeyReused
		}
		return cloneCase(rec.result), nil
	}

	// The cases map is keyed by bare CaseID (global, like workcases'
	// primary key). "exists" is global existence; "visible" additionally
	// requires the caller's exact enterprise and org scope.
	current, exists := s.cases[cmd.CaseID]
	visible := exists && current.EnterpriseID == cmd.EnterpriseID && current.OrgScope == cmd.OrgScope

	if eventType == EventCaseCreated {
		if exists {
			// Taken anywhere is taken: identical to PostgresStore, where
			// the INSERT collides on the global workcases_pkey even when
			// the existing case belongs to another enterprise/org.
			return workcase.WorkCase{}, ErrStaleRevision
		}
	} else if !visible {
		// Hidden (wrong enterprise/org) reads exactly like absent.
		return workcase.WorkCase{}, ErrNotFound
	}
	if !visible {
		current = workcase.WorkCase{}
	}
	if current.Revision != cmd.ExpectedRevision {
		return workcase.WorkCase{}, ErrStaleRevision
	}

	next, err := mutate(cloneCase(current))
	if err != nil {
		return workcase.WorkCase{}, err
	}

	stored := cloneCase(next)
	s.cases[cmd.CaseID] = stored
	s.idempotency[idemKey] = idempotencyRecord{caseID: cmd.CaseID, result: cloneCase(stored)}
	s.events[cmd.CaseID] = append(s.events[cmd.CaseID], storedEvent{Seq: next.Revision, Type: eventType, Payload: cloneCase(stored), CreatedAt: s.now().UTC()})
	return cloneCase(stored), nil
}
