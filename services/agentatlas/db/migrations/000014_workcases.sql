-- +goose Up

-- workcases is the current-snapshot projection of the WorkCase aggregate
-- (github.com/astraclawteam/agentatlas/sdk/go/workcase, frozen by Task 0A):
-- one row per governed case of work, addressed by (enterprise_id,
-- org_scope, id). plans is the case's full WorkPlan revision history,
-- stored as JSONB rather than normalized child tables because a case's
-- plans round-trip whole through the frozen sdk/go/workcase JSON shape
-- (one json.Marshal/Unmarshal, no hand-maintained per-column mapping to
-- drift) and every reader wants the whole case (no SQL-level filtering on
-- individual steps).
CREATE TABLE workcases (
    id            text PRIMARY KEY,
    enterprise_id text NOT NULL REFERENCES enterprises(id),
    org_scope     text NOT NULL CHECK (org_scope <> ''),
    actor_ref     text NOT NULL CHECK (actor_ref <> ''),
    status        text NOT NULL CHECK (status IN ('draft','reviewing','executing','completed','terminated')),
    revision      bigint NOT NULL CHECK (revision >= 1),
    plans         jsonb NOT NULL DEFAULT '[]'::jsonb,
    created_at    timestamptz NOT NULL DEFAULT now(),
    updated_at    timestamptz NOT NULL DEFAULT now()
);

-- workcase_events is the append-only event log: one row per command
-- successfully applied to a case, written in the SAME transaction as the
-- workcases snapshot update it produced (see internal/workcase/postgres.go
-- Apply). seq is defined to equal the WorkCase.Revision the event produced,
-- so (case_id, seq) is simultaneously the natural per-event primary key and
-- a second, independent enforcement of gapless, non-duplicated revision
-- ordering alongside the snapshot's own `WHERE revision = $expected` guard.
-- payload carries the full resulting WorkCase (not a delta) so the log can
-- be replayed/rebuilt, and so an idempotent command replay can hand back
-- the exact original result, by reading a single row.
--
-- Cost note for later tasks (0E/0F): because payload is the FULL resulting
-- WorkCase, each event's size grows with the case's accumulated plan
-- revisions x steps, so a case's total log size is ~quadratic in the
-- number of plan-mutating commands under pathological unbounded
-- replanning. Acceptable for governed, human-scale cases (bounded plans,
-- bounded commands); revisit (delta payloads, or pruning plans from older
-- event payloads) before building any workload that replans unboundedly.
--
-- Constraint names here are load-bearing: internal/workcase/postgres.go
-- (mapPgError) switches on them to translate unique violations into the
-- package's typed sentinel errors. workcases_pkey / workcase_events_pkey
-- are PostgreSQL's default primary-key names; the idempotency constraint
-- is named explicitly.
CREATE TABLE workcase_events (
    case_id         text NOT NULL REFERENCES workcases(id),
    seq             bigint NOT NULL CHECK (seq >= 1),
    enterprise_id   text NOT NULL REFERENCES enterprises(id),
    event_type      text NOT NULL CHECK (event_type IN ('case_created','plan_proposed','review_started','execution_started','step_transitioned')),
    payload         jsonb NOT NULL,
    idempotency_key text NOT NULL CHECK (char_length(idempotency_key) BETWEEN 16 AND 128),
    created_at      timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (case_id, seq),
    -- Idempotency keys are scoped per enterprise (not per case): a Create
    -- command has no case_id to scope against until after it succeeds, so
    -- enterprise-scoped uniqueness is the one rule that applies uniformly
    -- to every command type, including Create. Violating this constraint
    -- is also how the loser of a concurrent same-key race is detected
    -- (mapPgError -> ErrIdempotencyKeyReused).
    CONSTRAINT workcase_events_idempotency_uniq UNIQUE (enterprise_id, idempotency_key)
);

-- +goose Down
DROP TABLE workcase_events;
DROP TABLE workcases;
