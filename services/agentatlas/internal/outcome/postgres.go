package outcome

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	model "github.com/astraclawteam/agentatlas/sdk/go/outcome"
	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/outcomegraph"
)

// pgUniqueViolation is the PostgreSQL SQLSTATE for a unique/primary-key
// violation. See db/migrations/000016_outcomes.sql.
const pgUniqueViolation = "23505"

// PostgresStore is the authoritative Store: append-only outcome revisions,
// lineage nodes/edges and projection events, plus a forward-only per-tenant
// watermark. It follows the same direct-pgx idiom as
// internal/operatingmap/postgres.go and internal/workcase/postgres.go
// (pgxpool, tenant-scoped queries, per-key advisory lock, no Apache AGE
// access). content stores the whole sdk/go/outcome.Outcome as JSONB; the
// promoted scalar columns exist for indexing, tenant isolation and the CHECK
// constraints.
type PostgresStore struct {
	pool *pgxpool.Pool
	now  func() time.Time
}

// NewPostgresStore constructs a PostgresStore over pool. now defaults to
// time.Now.
func NewPostgresStore(pool *pgxpool.Pool, now func() time.Time) (*PostgresStore, error) {
	if pool == nil {
		return nil, errors.New("outcome postgres store requires a pool")
	}
	if now == nil {
		now = time.Now
	}
	return &PostgresStore{pool: pool, now: now}, nil
}

// isUniqueViolation reports whether err is a PostgreSQL unique/PK violation.
func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == pgUniqueViolation
}

func (s *PostgresStore) AppendOutcome(ctx context.Context, o model.Outcome) (model.Outcome, error) {
	if err := o.Validate(); err != nil {
		return model.Outcome{}, err
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return model.Outcome{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// Serialize concurrent appends per (tenant, outcome_key) so head detection,
	// the supersession check and the insert are atomic against each other. The
	// key length-prefixes the tenant so distinct (tenant, key) pairs can never
	// alias one lock word (e.g. "a"+"b|c" vs "a|b"+"c"), and it uses no NUL byte
	// (invalid in a Postgres text parameter). The lock is only an optimization;
	// UNIQUE (tenant, outcome_key, revision) is the real serialization backstop.
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`,
		fmt.Sprintf("outcome:%d:%s:%s", len(o.Tenant), o.Tenant, o.OutcomeKey)); err != nil {
		return model.Outcome{}, err
	}

	var head uint64
	var headID string
	row := tx.QueryRow(ctx, `SELECT id, revision FROM outcomes WHERE tenant=$1 AND outcome_key=$2 ORDER BY revision DESC LIMIT 1`, o.Tenant, o.OutcomeKey)
	switch err := row.Scan(&headID, &head); {
	case err == nil:
	case errors.Is(err, pgx.ErrNoRows):
		head, headID = 0, ""
	default:
		return model.Outcome{}, err
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
	content, err := json.Marshal(o)
	if err != nil {
		return model.Outcome{}, err
	}
	var supersedesID any
	var supersedesRevision any
	if o.Supersedes != nil {
		supersedesID = o.Supersedes.OutcomeID
		supersedesRevision = int64(o.Supersedes.Revision)
	}

	if _, err := tx.Exec(ctx, `INSERT INTO outcomes
		(id,tenant,outcome_key,revision,status,goal_tenant,goal_key,goal_version,rule_version,
		 work_case_id,work_case_revision,work_plan_revision,operating_map_version,org_version,
		 decided_at,supersedes_id,supersedes_revision,content)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18)`,
		o.ID, o.Tenant, o.OutcomeKey, int64(o.Revision), string(o.Claim.Status),
		o.Claim.Goal.Tenant, o.Claim.Goal.GoalKey, o.Claim.Goal.GoalVersion, o.Claim.RuleVersion,
		o.WorkCaseID, int64(o.WorkCaseRevision), int64(o.WorkPlanRevision), o.OperatingMapVersion, o.OrgVersion,
		o.DecidedAt.UTC(), supersedesID, supersedesRevision, content); err != nil {
		if isUniqueViolation(err) {
			return model.Outcome{}, ErrRevisionExists
		}
		return model.Outcome{}, err
	}

	if err := tx.Commit(ctx); err != nil {
		if isUniqueViolation(err) {
			return model.Outcome{}, ErrRevisionExists
		}
		return model.Outcome{}, err
	}
	return o, nil
}

func scanOutcome(row interface{ Scan(...any) error }) (model.Outcome, error) {
	var content []byte
	if err := row.Scan(&content); err != nil {
		return model.Outcome{}, err
	}
	var o model.Outcome
	if err := json.Unmarshal(content, &o); err != nil {
		return model.Outcome{}, err
	}
	return o, nil
}

func (s *PostgresStore) GetOutcome(ctx context.Context, tenant, outcomeKey string, revision uint64) (model.Outcome, error) {
	row := s.pool.QueryRow(ctx, `SELECT content FROM outcomes WHERE tenant=$1 AND outcome_key=$2 AND revision=$3`, tenant, outcomeKey, int64(revision))
	o, err := scanOutcome(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return model.Outcome{}, ErrNotFound
	}
	return o, err
}

func (s *PostgresStore) LatestOutcome(ctx context.Context, tenant, outcomeKey string) (model.Outcome, error) {
	row := s.pool.QueryRow(ctx, `SELECT content FROM outcomes WHERE tenant=$1 AND outcome_key=$2 ORDER BY revision DESC LIMIT 1`, tenant, outcomeKey)
	o, err := scanOutcome(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return model.Outcome{}, ErrNotFound
	}
	return o, err
}

func (s *PostgresStore) AppendLineage(ctx context.Context, nodes []model.LineageNode, edges []model.LineageEdge) error {
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
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	for _, n := range nodes {
		id := n.DerivedID()
		var summary any
		if n.Summary != "" {
			summary = n.Summary
		}
		if _, err := tx.Exec(ctx, `INSERT INTO outcome_lineage_nodes(id,tenant,node_type,business_id,revision,summary)
			VALUES ($1,$2,$3,$4,$5,$6) ON CONFLICT (id) DO NOTHING`,
			id, n.Tenant, string(n.Type), n.BusinessID, int64(n.Revision), summary); err != nil {
			return err
		}
	}
	for _, e := range edges {
		id := e.DerivedID()
		if _, err := tx.Exec(ctx, `INSERT INTO outcome_lineage_edges
			(id,tenant,edge_type,from_tenant,from_type,from_business_id,from_revision,to_tenant,to_type,to_business_id,to_revision)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11) ON CONFLICT (id) DO NOTHING`,
			id, e.Tenant, string(e.Type),
			e.From.Tenant, string(e.From.Type), e.From.BusinessID, int64(e.From.Revision),
			e.To.Tenant, string(e.To.Type), e.To.BusinessID, int64(e.To.Revision)); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

// AppendProjectionEvent is re-pointed by Task 0I onto the ONE shared
// Outcome-Graph projection outbox (outcome_graph_outbox), unifying the 0G
// outcome-only projection path so there is exactly one physical outbox for all
// four authoritative domains (workcase, operatingmap, governance, outcome) — the
// 0G-era outcome_projection_events table is retired (no longer written). The
// per-tenant monotonic Sequence, tombstone/reevaluation-as-append and
// never-edit-in-place semantics the 0G contract requires are preserved by the
// shared outbox. A rich, self-contained graph delta is built from the
// authoritative Outcome when present; otherwise a minimal subject upsert is used.
func (s *PostgresStore) AppendProjectionEvent(ctx context.Context, ev model.ProjectionEvent) (model.ProjectionEvent, error) {
	if err := ev.Validate(); err != nil {
		return model.ProjectionEvent{}, err
	}
	var rich *outcomegraph.GraphDelta
	if (ev.Kind == model.ProjectionOutcomeRevision || ev.Kind == model.ProjectionLineageFact) && ev.SubjectType == model.NodeOutcome {
		switch o, err := s.GetOutcome(ctx, ev.Tenant, ev.SubjectID, ev.SubjectRevision); {
		case err == nil:
			d := outcomegraph.MapOutcome(o)
			rich = &d
		case errors.Is(err, ErrNotFound):
			// No authoritative Outcome (e.g. a raw projection append): fall back to
			// a minimal subject upsert.
		default:
			return model.ProjectionEvent{}, err
		}
	}
	ogEvent := outcomegraph.MapOutcomeMarker(ev, rich)

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return model.ProjectionEvent{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	seq, err := outcomegraph.AppendOutboxTx(ctx, tx, ogEvent, s.now())
	if err != nil {
		return model.ProjectionEvent{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return model.ProjectionEvent{}, err
	}
	ev.Sequence = seq
	return ev, nil
}

func (s *PostgresStore) Watermark(ctx context.Context, tenant string) (model.ProjectionWatermark, error) {
	var w model.ProjectionWatermark
	var last int64
	err := s.pool.QueryRow(ctx, `SELECT last_sequence, updated_at FROM outcome_projection_watermarks WHERE tenant=$1`, tenant).Scan(&last, &w.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return model.ProjectionWatermark{}, ErrNotFound
	}
	if err != nil {
		return model.ProjectionWatermark{}, err
	}
	w.Tenant = tenant
	w.LastSequence = uint64(last)
	return w, nil
}

func (s *PostgresStore) AdvanceWatermark(ctx context.Context, tenant string, toSequence uint64) (model.ProjectionWatermark, error) {
	now := s.now().UTC()
	// ON CONFLICT ... WHERE last_sequence <= EXCLUDED.last_sequence lets an
	// equal (idempotent) or forward move through and blocks a regression (the
	// blocked update affects zero rows).
	tag, err := s.pool.Exec(ctx, `INSERT INTO outcome_projection_watermarks(tenant,last_sequence,updated_at)
		VALUES ($1,$2,$3)
		ON CONFLICT (tenant) DO UPDATE SET last_sequence=EXCLUDED.last_sequence, updated_at=EXCLUDED.updated_at
		WHERE outcome_projection_watermarks.last_sequence <= EXCLUDED.last_sequence`,
		tenant, int64(toSequence), now)
	if err != nil {
		return model.ProjectionWatermark{}, err
	}
	if tag.RowsAffected() == 0 {
		return model.ProjectionWatermark{}, ErrWatermarkRegression
	}
	return model.ProjectionWatermark{Tenant: tenant, LastSequence: toSequence, UpdatedAt: now}, nil
}
