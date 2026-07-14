// Package outcome persists the Outcome aggregate frozen by Task 0G
// (github.com/astraclawteam/agentatlas/sdk/go/outcome): immutable, versioned
// governed result records, their append-only lineage facts (nodes and edges)
// and the authoritative projection outbox a later Apache AGE read model (Task
// 0I) rebuilds from. This package is the deterministic persistence layer only
// -- opaque handles/hashes and permitted summaries, no connector endpoints,
// credentials, raw enterprise content, LLM calls, and NO Apache AGE access.
//
// Two implementations satisfy Store: an in-memory MemoryStore (for fast
// business-logic tests) and a PostgresStore (the authoritative store). A shared
// conformance suite (postgres_integration_test.go) pins both to one contract so
// their behavior cannot drift.
package outcome

import (
	"context"
	"errors"
	"sync"
	"time"

	model "github.com/astraclawteam/agentatlas/sdk/go/outcome"
)

// Sentinel errors returned by Store implementations. Callers compare with
// errors.Is; validation failures surface the sdk/go/outcome sentinels
// (model.ErrCrossTenantEdge, model.ErrDuplicateContribution, ...) directly.
var (
	// ErrNotFound is returned when no Outcome exists for the given tenant,
	// outcome_key and revision. A record under a different tenant is
	// indistinguishable from one that does not exist: both return ErrNotFound.
	ErrNotFound = errors.New("outcome: not found")
	// ErrRevisionExists is returned when a revision that already exists is
	// re-appended -- the append-only refusal of any attempt to overwrite
	// history in place. A changed conclusion must be a NEW, higher revision.
	ErrRevisionExists = errors.New("outcome: revision already exists (history is append-only; append a new revision)")
	// ErrBrokenSupersession is returned when a correction revision does not
	// supersede the current head revision (a gap, a stale predecessor, or a
	// mismatched predecessor id).
	ErrBrokenSupersession = errors.New("outcome: correction must supersede the current head revision")
	// ErrWatermarkRegression is returned when AdvanceWatermark is asked to move
	// a tenant's projection watermark backwards.
	ErrWatermarkRegression = errors.New("outcome: projection watermark must not move backwards")
)

// Store is the persistence port for the Outcome aggregate and its lineage.
type Store interface {
	// AppendOutcome validates o, assigns its deterministic id and persists it
	// as a new immutable revision. Revision 1 must not supersede; a correction
	// (revision N) must supersede the current head (revision N-1) by its exact
	// id. Re-appending an existing revision is ErrRevisionExists; an
	// out-of-sequence correction is ErrBrokenSupersession. A validation failure
	// (satisfied without an authoritative observation, an ActionReceipt used as
	// evidence, duplicate credit, connector-shaped content, ...) persists
	// nothing and returns the sdk/go/outcome sentinel.
	AppendOutcome(ctx context.Context, o model.Outcome) (model.Outcome, error)

	// GetOutcome returns the exact (tenant, outcomeKey, revision) revision, or
	// ErrNotFound. A cross-tenant read is reported as ErrNotFound.
	GetOutcome(ctx context.Context, tenant, outcomeKey string, revision uint64) (model.Outcome, error)

	// LatestOutcome returns the highest revision for (tenant, outcomeKey), or
	// ErrNotFound.
	LatestOutcome(ctx context.Context, tenant, outcomeKey string) (model.Outcome, error)

	// AppendLineage validates and appends immutable lineage nodes and edges.
	// Re-appending an identical fact is idempotent. An unversioned endpoint or
	// a cross-tenant edge persists nothing and returns the sdk sentinel.
	AppendLineage(ctx context.Context, nodes []model.LineageNode, edges []model.LineageEdge) error

	// AppendProjectionEvent validates ev, assigns the next per-tenant sequence
	// and appends it immutably. A tombstone/reevaluation is an ordinary append
	// naming the sequence it supersedes -- never an in-place edit.
	AppendProjectionEvent(ctx context.Context, ev model.ProjectionEvent) (model.ProjectionEvent, error)

	// Watermark returns the tenant's projection watermark, or ErrNotFound.
	Watermark(ctx context.Context, tenant string) (model.ProjectionWatermark, error)

	// AdvanceWatermark moves the tenant's projection watermark to toSequence.
	// Moving backwards is ErrWatermarkRegression; re-setting the same value is
	// idempotent.
	AdvanceWatermark(ctx context.Context, tenant string, toSequence uint64) (model.ProjectionWatermark, error)
}

// --- deep copy -------------------------------------------------------------

func cloneOutcome(o model.Outcome) model.Outcome {
	out := o
	out.Claim.Evidence = append([]model.EvidenceRef(nil), o.Claim.Evidence...)
	out.Claim.Observations = append([]model.ObservationRef(nil), o.Claim.Observations...)
	out.Claim.Blockers = append([]model.BlockerRef(nil), o.Claim.Blockers...)
	out.Claim.Confidence = append([]model.ConfidenceComponent(nil), o.Claim.Confidence...)
	out.ActionReceipts = append([]model.ReceiptRef(nil), o.ActionReceipts...)
	out.Contributions = append([]model.ContributionRef(nil), o.Contributions...)
	if o.Supersedes != nil {
		s := *o.Supersedes
		out.Supersedes = &s
	}
	return out
}

// --- MemoryStore -----------------------------------------------------------

// MemoryStore is an in-memory Store enforcing exactly the same contract as
// PostgresStore: immutable append-only revisions with explicit supersession,
// idempotent lineage facts, a per-tenant monotonic projection outbox and a
// forward-only watermark. Tests written against it exercise real behavior, and
// the shared conformance suite pins it to PostgresStore.
type MemoryStore struct {
	mu         sync.Mutex
	now        func() time.Time
	outcomes   map[string][]model.Outcome // keyed by tenant\x00outcome_key, revision-ordered
	nodes      map[string]model.LineageNode
	edges      map[string]model.LineageEdge
	events     map[string][]model.ProjectionEvent // keyed by tenant
	watermarks map[string]model.ProjectionWatermark
}

// NewMemoryStore constructs an empty MemoryStore. now defaults to time.Now.
func NewMemoryStore(now func() time.Time) *MemoryStore {
	if now == nil {
		now = time.Now
	}
	return &MemoryStore{
		now:        now,
		outcomes:   map[string][]model.Outcome{},
		nodes:      map[string]model.LineageNode{},
		edges:      map[string]model.LineageEdge{},
		events:     map[string][]model.ProjectionEvent{},
		watermarks: map[string]model.ProjectionWatermark{},
	}
}

func outcomeKeyOf(tenant, key string) string { return tenant + "\x00" + key }

func (s *MemoryStore) AppendOutcome(_ context.Context, o model.Outcome) (model.Outcome, error) {
	if err := o.Validate(); err != nil {
		return model.Outcome{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	k := outcomeKeyOf(o.Tenant, o.OutcomeKey)
	chain := s.outcomes[k]
	var head uint64
	var headID string
	if n := len(chain); n > 0 {
		head = chain[n-1].Revision
		headID = chain[n-1].ID
	}
	if o.Revision <= head {
		return model.Outcome{}, ErrRevisionExists
	}
	if o.Revision > head+1 {
		return model.Outcome{}, ErrBrokenSupersession
	}
	if o.Revision > 1 && (o.Supersedes == nil || o.Supersedes.OutcomeID != headID) {
		return model.Outcome{}, ErrBrokenSupersession
	}
	o.ID = o.DerivedID()
	s.outcomes[k] = append(chain, cloneOutcome(o))
	return cloneOutcome(o), nil
}

func (s *MemoryStore) GetOutcome(_ context.Context, tenant, outcomeKey string, revision uint64) (model.Outcome, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, o := range s.outcomes[outcomeKeyOf(tenant, outcomeKey)] {
		if o.Revision == revision {
			return cloneOutcome(o), nil
		}
	}
	return model.Outcome{}, ErrNotFound
}

func (s *MemoryStore) LatestOutcome(_ context.Context, tenant, outcomeKey string) (model.Outcome, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	chain := s.outcomes[outcomeKeyOf(tenant, outcomeKey)]
	if len(chain) == 0 {
		return model.Outcome{}, ErrNotFound
	}
	return cloneOutcome(chain[len(chain)-1]), nil
}

func (s *MemoryStore) AppendLineage(_ context.Context, nodes []model.LineageNode, edges []model.LineageEdge) error {
	for i := range nodes {
		if err := nodes[i].Validate(); err != nil {
			return err
		}
	}
	for i := range edges {
		if err := edges[i].Validate(); err != nil {
			return err
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	// First-write-wins, matching PostgresStore's ON CONFLICT (id) DO NOTHING: a
	// node/edge is an immutable append-only fact, and a node's StableID excludes
	// its summary, so re-appending the same id with a changed summary must NOT
	// overwrite the stored value.
	for _, n := range nodes {
		id := n.DerivedID()
		if _, ok := s.nodes[id]; !ok {
			n.ID = id
			s.nodes[id] = n
		}
	}
	for _, e := range edges {
		id := e.DerivedID()
		if _, ok := s.edges[id]; !ok {
			e.ID = id
			s.edges[id] = e
		}
	}
	return nil
}

func (s *MemoryStore) AppendProjectionEvent(_ context.Context, ev model.ProjectionEvent) (model.ProjectionEvent, error) {
	if err := ev.Validate(); err != nil {
		return model.ProjectionEvent{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	// RecordedAt is caller-supplied and required (ProjectionEvent.Validate
	// rejects a zero value above), so the store does not stamp it.
	ev.Sequence = uint64(len(s.events[ev.Tenant])) + 1
	s.events[ev.Tenant] = append(s.events[ev.Tenant], ev)
	return ev, nil
}

func (s *MemoryStore) Watermark(_ context.Context, tenant string) (model.ProjectionWatermark, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	w, ok := s.watermarks[tenant]
	if !ok {
		return model.ProjectionWatermark{}, ErrNotFound
	}
	return w, nil
}

func (s *MemoryStore) AdvanceWatermark(_ context.Context, tenant string, toSequence uint64) (model.ProjectionWatermark, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if cur, ok := s.watermarks[tenant]; ok && toSequence < cur.LastSequence {
		return model.ProjectionWatermark{}, ErrWatermarkRegression
	}
	w := model.ProjectionWatermark{Tenant: tenant, LastSequence: toSequence, UpdatedAt: s.now().UTC()}
	s.watermarks[tenant] = w
	return w, nil
}
