package outcomegraph

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/tasks"
)

// OutboxReader reads the immutable, append-only shared projection outbox. It is
// STRICTLY read-only: no projection event is ever pruned or edited (a
// tombstone/reevaluation is a NEW appended event). The projector consumes events
// in per-tenant sequence order.
type OutboxReader interface {
	// ReadAfter returns up to limit events for tenant with sequence > afterSeq,
	// in ascending sequence order.
	ReadAfter(ctx context.Context, tenant string, afterSeq uint64, limit int) ([]ProjectionEvent, error)
	// ReadAll returns every event for tenant in ascending sequence order (used by
	// Rebuild).
	ReadAll(ctx context.Context, tenant string) ([]ProjectionEvent, error)
	// Tenants lists the tenants that have outbox events (used by periodic
	// catch-up).
	Tenants(ctx context.Context) ([]string, error)
	// Head returns the highest sequence appended for tenant (0 when the tenant
	// has no events). It is the counterpart to the graph's watermark: the two
	// together are the only way to answer "is the projection advancing", which
	// is the question this service had no way to answer at all.
	Head(ctx context.Context, tenant string) (uint64, error)
}

// TenantLag reports how far one tenant's graph read model trails its
// authoritative outbox.
//
// Lag is the deliverable, not the endpoint that serves it. This service used to
// expose no HTTP listener whatsoever -- no /healthz, no /readyz, no metrics port
// -- and its runtime image carries a single static binary with no psql, so
// nothing inside the container could answer whether the projection was moving.
// It degrades silently by design: ProjectAll failures are logged at WARN and the
// watermark simply stops advancing. A probe that reported only "the process
// exists" would have been green throughout that, which is why the number below
// is the point.
type TenantLag struct {
	Tenant    string
	Watermark uint64
	Head      uint64
	// Events is Head - Watermark: the number of committed domain facts the graph
	// has not applied yet. Zero means fully caught up.
	Events uint64
}

// JobProject is the NATS job type/subject the projector consumes; the jobID is
// the tenant to catch up. Reuses internal/tasks (MemBus in tests, NATSBus in
// production) — no new broker.
const JobProject = "outcomegraph.project"

// Projector is the resumable, idempotent Outcome-Graph projector. It consumes
// the shared outbox through NATS/outbox, applies each tenant's events to the
// graph via idempotent MERGE, and advances the per-tenant watermark ONLY after
// the graph write commits. It is correct under duplicate, out-of-order and
// redelivered delivery and across a crash before OR after the graph commit.
type Projector struct {
	store     OutcomeGraphStore
	outbox    OutboxReader
	batchSize int
	now       func() time.Time
}

// ProjectorOption configures a Projector.
type ProjectorOption func(*Projector)

// WithBatchSize sets the max events applied per graph transaction.
func WithBatchSize(n int) ProjectorOption {
	return func(p *Projector) {
		if n > 0 {
			p.batchSize = n
		}
	}
}

// NewProjector builds a Projector over a graph store and an outbox reader.
func NewProjector(store OutcomeGraphStore, outbox OutboxReader, opts ...ProjectorOption) *Projector {
	p := &Projector{store: store, outbox: outbox, batchSize: 256, now: time.Now}
	for _, o := range opts {
		o(p)
	}
	return p
}

// ProjectTenant catches a tenant's graph up to the head of its outbox and
// returns the watermark reached. The sequence of operations is load-bearing:
//
//  1. read the current watermark;
//  2. read the next batch of outbox events after it;
//  3. apply the batch to the graph in ONE transaction (idempotent MERGE);
//  4. ONLY after that commit, advance the watermark.
//
// A crash BEFORE step 3 commits leaves the watermark unmoved: the same events
// replay cleanly. A crash AFTER step 3 but BEFORE step 4 also leaves the
// watermark unmoved: the deterministic merge keys make the replayed MERGE a
// no-op, so no duplicate edges appear. If the graph is unavailable (AGE down),
// steps 1/3 return ErrGraphUnavailable, the watermark never advances, and the
// error surfaces EXPLICITLY (graph-dependent planning degrades, never a silent
// wrong answer). On recovery the next call catches up from the last committed
// watermark.
func (p *Projector) ProjectTenant(ctx context.Context, tenant string) (uint64, error) {
	wm, err := p.store.Watermark(ctx, tenant)
	if err != nil {
		return 0, fmt.Errorf("outcomegraph: read watermark: %w", err)
	}
	for {
		if err := ctx.Err(); err != nil {
			return wm, err
		}
		events, err := p.outbox.ReadAfter(ctx, tenant, wm, p.batchSize)
		if err != nil {
			return wm, fmt.Errorf("outcomegraph: read outbox: %w", err)
		}
		if len(events) == 0 {
			return wm, nil
		}
		// ProjectBatch applies + advances the watermark as ONE per-tenant-serialized
		// unit (advisory lock in the graph store): concurrent projectors for this
		// tenant (the periodic ProjectAll loop and the NATS subscription callback,
		// and any other replica) run strictly serially, so the idempotent MERGE
		// never races into duplicate vertices/edges. On AGE down / apply failure it
		// returns an explicit error and the watermark does not advance, so the
		// caller degrades rather than trusting a stale graph.
		newWM, err := p.store.ProjectBatch(ctx, tenant, events)
		if err != nil {
			return wm, fmt.Errorf("outcomegraph: project batch: %w", err)
		}
		batchLen := len(events)
		wm = newWM
		if batchLen < p.batchSize {
			return wm, nil
		}
	}
}

// ProjectAll catches up every tenant with outbox events. Used by the periodic
// reconcile loop so projection is eventually complete even if a NATS wakeup was
// missed — the outbox is the source of truth, NATS is only a nudge.
func (p *Projector) ProjectAll(ctx context.Context) error {
	tenants, err := p.outbox.Tenants(ctx)
	if err != nil {
		return err
	}
	var errs []error
	for _, t := range tenants {
		if _, err := p.ProjectTenant(ctx, t); err != nil {
			errs = append(errs, fmt.Errorf("tenant %s: %w", t, err))
		}
	}
	return errors.Join(errs...)
}

// Lag reports, for every tenant with outbox events, how far the graph read
// model trails the authoritative outbox.
//
// It reads the watermark BEFORE the head, deliberately. The other order can
// observe a watermark that already includes events appended after the head was
// read, which underflows to an enormous lag on a perfectly healthy projector --
// a false alarm on the one number an operator is being asked to trust. This
// order can only ever over-report by the events appended in between, which is
// the honest direction to be wrong in.
func (p *Projector) Lag(ctx context.Context) ([]TenantLag, error) {
	tenants, err := p.outbox.Tenants(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]TenantLag, 0, len(tenants))
	var errs []error
	for _, tenant := range tenants {
		watermark, err := p.store.Watermark(ctx, tenant)
		if err != nil {
			errs = append(errs, fmt.Errorf("tenant %s watermark: %w", tenant, err))
			continue
		}
		head, err := p.outbox.Head(ctx, tenant)
		if err != nil {
			errs = append(errs, fmt.Errorf("tenant %s head: %w", tenant, err))
			continue
		}
		lag := TenantLag{Tenant: tenant, Watermark: watermark, Head: head}
		if head > watermark {
			lag.Events = head - watermark
		}
		out = append(out, lag)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Tenant < out[j].Tenant })
	return out, errors.Join(errs...)
}

// Rebuild drops the tenant's graph and replays the ENTIRE outbox, producing a
// graph identical to the incrementally-projected one (deterministic merge keys)
// and setting the watermark to the last replayed sequence.
func (p *Projector) Rebuild(ctx context.Context, tenant string) error {
	events, err := p.outbox.ReadAll(ctx, tenant)
	if err != nil {
		return fmt.Errorf("outcomegraph: read outbox for rebuild: %w", err)
	}
	return p.store.Rebuild(ctx, tenant, events)
}

// Subscribe wires the projector to a task bus: each message's body is a tenant
// to catch up. Idempotency is inherent (watermark + idempotent MERGE), so a
// redelivered or duplicated wakeup is harmless.
func (p *Projector) Subscribe(ctx context.Context, bus tasks.Bus) (func(), error) {
	return bus.Subscribe(ctx, JobProject, func(ctx context.Context, tenant string) {
		_, _ = p.ProjectTenant(ctx, tenant)
	})
}

// Notify publishes a projection wakeup for a tenant (best-effort). The periodic
// ProjectAll loop is the reliable path; this only reduces latency.
func Notify(ctx context.Context, bus tasks.Bus, tenant string) error {
	return bus.Publish(ctx, JobProject, tenant)
}

// --- in-memory outbox (tests, single-process) ------------------------------

// MemOutbox is an in-memory OutboxReader/appender mirroring the shared outbox's
// per-tenant monotonic sequence and append-only semantics.
type MemOutbox struct {
	mu     sync.Mutex
	events map[string][]ProjectionEvent
}

// NewMemOutbox constructs an empty in-memory outbox.
func NewMemOutbox() *MemOutbox {
	return &MemOutbox{events: map[string][]ProjectionEvent{}}
}

// Append assigns the next per-tenant sequence and stores ev immutably.
func (m *MemOutbox) Append(ev ProjectionEvent) (ProjectionEvent, error) {
	if ev.PayloadHash == "" {
		ev.PayloadHash = ev.Delta.Hash()
	}
	if ev.RecordedAt.IsZero() {
		ev.RecordedAt = time.Unix(0, 0).UTC()
	}
	if err := ev.Validate(); err != nil {
		return ProjectionEvent{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	ev.Sequence = uint64(len(m.events[ev.Tenant])) + 1
	m.events[ev.Tenant] = append(m.events[ev.Tenant], ev)
	return ev, nil
}

func (m *MemOutbox) ReadAfter(_ context.Context, tenant string, afterSeq uint64, limit int) ([]ProjectionEvent, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []ProjectionEvent
	for _, e := range m.events[tenant] {
		if e.Sequence > afterSeq {
			out = append(out, e)
			if len(out) >= limit {
				break
			}
		}
	}
	return out, nil
}

func (m *MemOutbox) ReadAll(_ context.Context, tenant string) ([]ProjectionEvent, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]ProjectionEvent(nil), m.events[tenant]...), nil
}

func (m *MemOutbox) Head(_ context.Context, tenant string) (uint64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	events := m.events[tenant]
	if len(events) == 0 {
		return 0, nil
	}
	return events[len(events)-1].Sequence, nil
}

func (m *MemOutbox) Tenants(_ context.Context) ([]string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]string, 0, len(m.events))
	for t := range m.events {
		out = append(out, t)
	}
	sort.Strings(out)
	return out, nil
}

var _ OutboxReader = (*MemOutbox)(nil)

// --- Postgres outbox (production) ------------------------------------------

// PostgresOutbox reads the authoritative shared outbox table. It is read-only:
// the append side is AppendOutboxTx, executed inside each authoritative store's
// own transaction.
type PostgresOutbox struct {
	pool *pgxpool.Pool
}

// NewPostgresOutbox constructs a PostgresOutbox over the authoritative pool.
func NewPostgresOutbox(pool *pgxpool.Pool) *PostgresOutbox { return &PostgresOutbox{pool: pool} }

func (o *PostgresOutbox) scan(ctx context.Context, query string, args ...any) ([]ProjectionEvent, error) {
	rows, err := o.pool.Query(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ProjectionEvent
	for rows.Next() {
		var ev ProjectionEvent
		var org *string
		var source, kind, subjectLabel string
		var seq, subjectRev int64
		var supersedes *int64
		var payload []byte
		if err := rows.Scan(&ev.Tenant, &seq, &org, &source, &kind, &subjectLabel, &ev.SubjectID, &subjectRev, &payload, &ev.PayloadHash, &supersedes, &ev.RecordedAt); err != nil {
			return nil, err
		}
		ev.Sequence = uint64(seq)
		if org != nil {
			ev.Org = *org
		}
		ev.Source = Source(source)
		ev.Kind = EventKind(kind)
		ev.SubjectLabel = NodeLabel(subjectLabel)
		ev.SubjectRevision = uint64(subjectRev)
		if supersedes != nil {
			ev.SupersedesSequence = uint64(*supersedes)
		}
		if err := json.Unmarshal(payload, &ev.Delta); err != nil {
			return nil, fmt.Errorf("outcomegraph: decode outbox payload seq %d: %w", seq, err)
		}
		out = append(out, ev)
	}
	return out, rows.Err()
}

const outboxColumns = `tenant,sequence,org,source,kind,subject_label,subject_id,subject_revision,payload,payload_hash,supersedes_sequence,recorded_at`

func (o *PostgresOutbox) ReadAfter(ctx context.Context, tenant string, afterSeq uint64, limit int) ([]ProjectionEvent, error) {
	return o.scan(ctx, `SELECT `+outboxColumns+` FROM outcome_graph_outbox WHERE tenant=$1 AND sequence>$2 ORDER BY sequence ASC LIMIT $3`, tenant, int64(afterSeq), limit)
}

func (o *PostgresOutbox) ReadAll(ctx context.Context, tenant string) ([]ProjectionEvent, error) {
	return o.scan(ctx, `SELECT `+outboxColumns+` FROM outcome_graph_outbox WHERE tenant=$1 ORDER BY sequence ASC`, tenant)
}

func (o *PostgresOutbox) Head(ctx context.Context, tenant string) (uint64, error) {
	// COALESCE, not a nullable scan: a tenant that appears in Tenants() between
	// this call and a truncation would otherwise error out the whole lag read.
	var head int64
	err := o.pool.QueryRow(ctx,
		`SELECT COALESCE(MAX(sequence),0) FROM outcome_graph_outbox WHERE tenant=$1`, tenant).Scan(&head)
	if err != nil {
		return 0, err
	}
	return uint64(head), nil
}

func (o *PostgresOutbox) Tenants(ctx context.Context) ([]string, error) {
	rows, err := o.pool.Query(ctx, `SELECT DISTINCT tenant FROM outcome_graph_outbox ORDER BY tenant`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var t string
		if err := rows.Scan(&t); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

var _ OutboxReader = (*PostgresOutbox)(nil)
