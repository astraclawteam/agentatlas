package workcase

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/astraclawteam/agentatlas/sdk/go/workcase"
	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/outcomegraph"
)

// pgUniqueViolation is the PostgreSQL SQLSTATE for a unique/primary-key
// constraint violation. See db/migrations/000014_workcases.sql.
const pgUniqueViolation = "23505"

// Constraint names from db/migrations/000014_workcases.sql that Apply's
// writes can violate. The primary keys carry PostgreSQL's default names;
// the idempotency constraint is named explicitly in the migration, which
// documents these names as load-bearing.
const (
	constraintWorkcasesPK       = "workcases_pkey"
	constraintEventsPK          = "workcase_events_pkey"
	constraintEventsIdempotency = "workcase_events_idempotency_uniq"
)

// mapPgError centralizes the translation of unique-constraint violations
// raised by Apply's writes into this package's typed sentinels; every
// other error (including non-unique constraint classes) passes through
// unchanged.
func mapPgError(err error) error {
	var pgErr *pgconn.PgError
	if err == nil || !errors.As(err, &pgErr) || pgErr.Code != pgUniqueViolation {
		return err
	}
	switch pgErr.ConstraintName {
	case constraintWorkcasesPK:
		// A create-create race for the same CaseID between Apply's
		// FOR UPDATE probe (no row yet) and its INSERT, or a
		// caller-supplied CaseID already taken under any enterprise/org
		// (the id column is a global primary key): the caller's implicit
		// expectation of revision 0 no longer matches reality, same
		// contract as the explicit revision guard.
		return ErrStaleRevision
	case constraintEventsIdempotency:
		// Closes the idempotency TOCTOU window: two concurrent commands
		// carrying the SAME key both missed the replay SELECT (nothing
		// committed yet) and raced to the event INSERT. The loser must
		// see the same typed conflict a sequential caller gets from the
		// replay branch, not a raw driver error.
		return ErrIdempotencyKeyReused
	case constraintEventsPK:
		// The snapshot write succeeded at a revision whose event seq row
		// already exists: snapshot and event log disagree. Internal
		// corruption (or an out-of-band write into workcase_events),
		// never a caller-correctable condition.
		return fmt.Errorf("%w: %s", ErrEventLogConflict, pgErr.Message)
	}
	return err
}

// PostgresStore is the production Store: one workcases snapshot row per
// case plus an append-only workcase_events row per applied command,
// written together in one transaction per Apply call.
type PostgresStore struct {
	pool *pgxpool.Pool
	now  func() time.Time
}

// NewPostgresStore constructs a PostgresStore over pool. now defaults to
// time.Now.
func NewPostgresStore(pool *pgxpool.Pool, now func() time.Time) (*PostgresStore, error) {
	if pool == nil {
		return nil, errors.New("workcase postgres store requires pool")
	}
	if now == nil {
		now = time.Now
	}
	return &PostgresStore{pool: pool, now: now}, nil
}

type rowScanner interface{ Scan(...any) error }

func scanCase(row rowScanner) (workcase.WorkCase, error) {
	var out workcase.WorkCase
	var status string
	var plans []byte
	if err := row.Scan(&out.ID, &out.EnterpriseID, &out.OrgScope, &out.ActorRef, &status, &out.Revision, &plans); err != nil {
		return workcase.WorkCase{}, err
	}
	out.Status = workcase.Status(status)
	if len(plans) > 0 {
		if err := json.Unmarshal(plans, &out.Plans); err != nil {
			return workcase.WorkCase{}, fmt.Errorf("workcase: decode plans: %w", err)
		}
	}
	return out, nil
}

const selectCaseColumns = `id,enterprise_id,org_scope,actor_ref,status,revision,plans`

func (s *PostgresStore) Get(ctx context.Context, enterpriseID, orgScope, caseID string) (workcase.WorkCase, error) {
	row := s.pool.QueryRow(ctx, `SELECT `+selectCaseColumns+` FROM workcases WHERE id=$1 AND enterprise_id=$2 AND org_scope=$3`, caseID, enterpriseID, orgScope)
	out, err := scanCase(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return workcase.WorkCase{}, ErrNotFound
	}
	if err != nil {
		return workcase.WorkCase{}, err
	}
	return out, nil
}

func (s *PostgresStore) Apply(ctx context.Context, cmd Command, eventType EventType, mutate Mutate) (workcase.WorkCase, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return workcase.WorkCase{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// Idempotency-first: a repeated key short-circuits before any
	// tenancy, revision or domain-rule check runs, so replaying a command
	// after the aggregate has moved on (to a state where a FRESH
	// application would now fail those checks) still returns the original
	// result instead of a spurious error.
	var existingCaseID string
	var existingPayload []byte
	err = tx.QueryRow(ctx, `SELECT case_id,payload FROM workcase_events WHERE enterprise_id=$1 AND idempotency_key=$2`, cmd.EnterpriseID, cmd.IdempotencyKey).Scan(&existingCaseID, &existingPayload)
	switch {
	case err == nil:
		var replay workcase.WorkCase
		if jsonErr := json.Unmarshal(existingPayload, &replay); jsonErr != nil {
			return workcase.WorkCase{}, fmt.Errorf("workcase: decode replayed event: %w", jsonErr)
		}
		// Org scope gates the replay before anything about the recorded
		// command is disclosed: a caller addressing this key from the
		// wrong org scope gets the same ErrNotFound any other
		// wrongly-scoped access gets -- never a replay, and never a
		// key-reuse conflict that would leak the key's existence.
		if replay.OrgScope != cmd.OrgScope {
			return workcase.WorkCase{}, ErrNotFound
		}
		if existingCaseID != cmd.CaseID {
			return workcase.WorkCase{}, ErrIdempotencyKeyReused
		}
		return replay, nil
	case errors.Is(err, pgx.ErrNoRows):
		// Not a replay; fall through to the normal path below.
	default:
		return workcase.WorkCase{}, err
	}

	row := tx.QueryRow(ctx, `SELECT `+selectCaseColumns+` FROM workcases WHERE id=$1 AND enterprise_id=$2 AND org_scope=$3 FOR UPDATE`, cmd.CaseID, cmd.EnterpriseID, cmd.OrgScope)
	current, err := scanCase(row)
	exists := true
	switch {
	case err == nil:
	case errors.Is(err, pgx.ErrNoRows):
		exists = false
		current = workcase.WorkCase{}
	default:
		return workcase.WorkCase{}, err
	}

	if eventType == EventCaseCreated {
		if exists {
			return workcase.WorkCase{}, ErrStaleRevision
		}
	} else if !exists {
		return workcase.WorkCase{}, ErrNotFound
	}
	if current.Revision != cmd.ExpectedRevision {
		return workcase.WorkCase{}, ErrStaleRevision
	}

	next, err := mutate(current)
	if err != nil {
		return workcase.WorkCase{}, err
	}

	plansJSON, err := json.Marshal(next.Plans)
	if err != nil {
		return workcase.WorkCase{}, err
	}
	now := s.now().UTC()
	if eventType == EventCaseCreated {
		if _, err = tx.Exec(ctx, `INSERT INTO workcases(id,enterprise_id,org_scope,actor_ref,status,revision,plans,created_at,updated_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$8)`,
			next.ID, next.EnterpriseID, next.OrgScope, next.ActorRef, string(next.Status), next.Revision, plansJSON, now); err != nil {
			return workcase.WorkCase{}, mapPgError(err)
		}
	} else {
		tag, execErr := tx.Exec(ctx, `UPDATE workcases SET status=$1,revision=$2,plans=$3,updated_at=$4 WHERE id=$5 AND enterprise_id=$6 AND org_scope=$7 AND revision=$8`,
			string(next.Status), next.Revision, plansJSON, now, cmd.CaseID, cmd.EnterpriseID, cmd.OrgScope, cmd.ExpectedRevision)
		if execErr != nil {
			return workcase.WorkCase{}, mapPgError(execErr)
		}
		if tag.RowsAffected() == 0 {
			// Lost a race against another writer between our FOR UPDATE
			// read and this UPDATE (should be rare given the row lock, but
			// guarded regardless -- defense in depth, no different a
			// contract than the revision check above).
			return workcase.WorkCase{}, ErrStaleRevision
		}
	}

	payload, err := json.Marshal(next)
	if err != nil {
		return workcase.WorkCase{}, err
	}
	if _, err = tx.Exec(ctx, `INSERT INTO workcase_events(case_id,seq,enterprise_id,event_type,payload,idempotency_key,created_at) VALUES($1,$2,$3,$4,$5,$6,$7)`,
		cmd.CaseID, next.Revision, cmd.EnterpriseID, string(eventType), payload, cmd.IdempotencyKey, now); err != nil {
		return workcase.WorkCase{}, mapPgError(err)
	}

	// Task 0I transactional outbox: append the WorkCase revision as a canonical
	// projection event to the ONE shared Outcome-Graph outbox, in THIS
	// transaction. The only synchronous write is to authoritative PostgreSQL
	// (snapshot + event + outbox row, atomically); AGE is projected async.
	if _, err := outcomegraph.AppendOutboxTx(ctx, tx, outcomegraph.MapWorkCase(next), now); err != nil {
		return workcase.WorkCase{}, err
	}

	if err := tx.Commit(ctx); err != nil {
		return workcase.WorkCase{}, err
	}
	return next, nil
}
