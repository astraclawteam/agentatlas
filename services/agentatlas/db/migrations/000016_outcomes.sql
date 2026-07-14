-- +goose Up

-- outcome_observations_all_signed reports whether a JSONB observations array is
-- non-empty AND every element carries a non-empty authority AND a non-empty
-- signature_key_id -- the exact structural precondition sdk/go/outcome treats as
-- an authoritative, signed observation (ObservationRef.IsAuthoritative). The
-- outcomes satisfied CHECK calls it so the authoritative store enforces
-- satisfaction the SAME way sdk/go/outcome.Outcome.Validate and the JSON Schema
-- do -- not merely "some observation element is present". A NULL/missing or
-- non-array argument returns false (fail-closed).
-- +goose StatementBegin
CREATE FUNCTION outcome_observations_all_signed(observations jsonb) RETURNS boolean
LANGUAGE sql IMMUTABLE AS $$
    SELECT observations IS NOT NULL
       AND jsonb_typeof(observations) = 'array'
       AND jsonb_array_length(observations) >= 1
       AND NOT EXISTS (
           SELECT 1 FROM jsonb_array_elements(observations) AS o
           WHERE coalesce(o->>'authority', '') = ''
              OR coalesce(o->>'signature_key_id', '') = ''
       );
$$;
-- +goose StatementEnd

-- outcomes is the authoritative, append-only history of immutable Outcome
-- revisions (github.com/astraclawteam/agentatlas/sdk/go/outcome, frozen by
-- Task 0G): one row per (tenant, outcome_key, revision). A changed conclusion
-- is a NEW revision that names the prior one via supersedes_id; a row is never
-- rewritten. content is the full sdk/go/outcome.Outcome as JSONB (one
-- json.Marshal/Unmarshal, no per-column drift); the promoted scalar columns
-- exist for indexing, tenant isolation and the CHECK constraints below. content
-- carries opaque handles, hashes and permitted summaries only -- never a
-- connector endpoint, credential, raw enterprise content or full receipt body.
--
-- Authoritative enforcement, per invariant (sdk/go/outcome.Validate remains the
-- primary gate; the DB is the fail-closed backstop for any raw-SQL path):
--   * DB-ENFORCED here (genuine mirrors of Validate/schema): status enum,
--     goal/work/operating-map/org version binding, promoted-column vs content
--     agreement, supersession coherence, and the satisfied-requires-authoritative-
--     signed-observation gate (via outcome_observations_all_signed).
--   * Go-ONLY (NOT mirrored in this schema): duplicate-credit rejection and the
--     opaque-content (NoRawContentLeak) scan -- sdk/go/outcome.Outcome.Validate
--     is the sole authoritative enforcer of those two.
CREATE TABLE outcomes (
    id                     text PRIMARY KEY,
    tenant                 text NOT NULL REFERENCES enterprises(id),
    outcome_key            text NOT NULL CHECK (outcome_key <> ''),
    revision               bigint NOT NULL CHECK (revision >= 1),
    status                 text NOT NULL CHECK (status IN ('unverified','satisfied','unsatisfied','disputed','blocked','unknown')),
    goal_tenant            text NOT NULL CHECK (goal_tenant <> ''),
    goal_key               text NOT NULL CHECK (goal_key <> ''),
    goal_version           bigint NOT NULL CHECK (goal_version >= 1),
    rule_version           text NOT NULL CHECK (rule_version <> ''),
    work_case_id           text NOT NULL CHECK (work_case_id <> ''),
    work_case_revision     bigint NOT NULL CHECK (work_case_revision >= 1),
    work_plan_revision     bigint NOT NULL CHECK (work_plan_revision >= 1),
    operating_map_version  bigint NOT NULL CHECK (operating_map_version >= 1),
    org_version            bigint NOT NULL CHECK (org_version >= 0),
    decided_at             timestamptz NOT NULL,
    supersedes_id          text,
    supersedes_revision    bigint,
    content                jsonb NOT NULL,
    created_at             timestamptz NOT NULL DEFAULT now(),
    UNIQUE (tenant, outcome_key, revision),
    -- The goal must live in the outcome's own tenant.
    CHECK (goal_tenant = tenant),
    -- Promoted scalar columns must NEVER disagree with content: the store reads
    -- and filters on these columns while GetOutcome unmarshals content, so a raw
    -- row whose column and content diverge (e.g. a 'satisfied' content under an
    -- 'unsatisfied' column) would be returned/queried inconsistently. coalesce
    -- to ''/-1 makes a MISSING content field fail closed too. (decided_at is
    -- intentionally excluded: timestamptz microsecond storage can differ from
    -- the JSON nanosecond text, and the store never reads it for logic.)
    CHECK (status = coalesce(content->'claim'->>'status', '')),
    CHECK (tenant = coalesce(content->>'tenant', '')),
    CHECK (outcome_key = coalesce(content->>'outcome_key', '')),
    CHECK (revision = coalesce((content->>'revision')::bigint, -1)),
    CHECK (goal_tenant = coalesce(content->'claim'->'goal'->>'tenant', '')),
    CHECK (goal_key = coalesce(content->'claim'->'goal'->>'goal_key', '')),
    CHECK (goal_version = coalesce((content->'claim'->'goal'->>'goal_version')::bigint, -1)),
    CHECK (rule_version = coalesce(content->'claim'->>'rule_version', '')),
    CHECK (work_case_id = coalesce(content->>'work_case_id', '')),
    CHECK (work_case_revision = coalesce((content->>'work_case_revision')::bigint, -1)),
    CHECK (work_plan_revision = coalesce((content->>'work_plan_revision')::bigint, -1)),
    CHECK (operating_map_version = coalesce((content->>'operating_map_version')::bigint, -1)),
    CHECK (org_version = coalesce((content->>'org_version')::bigint, -1)),
    -- Revision 1 has no predecessor; every later revision must name the
    -- immediately prior revision it supersedes. A "correction" that tried to
    -- overwrite history would either collide on the UNIQUE key above (same
    -- revision) or violate this coherence rule (missing/mismatched predecessor).
    CHECK (
        (revision = 1 AND supersedes_id IS NULL AND supersedes_revision IS NULL)
        OR
        (revision > 1 AND supersedes_id IS NOT NULL AND supersedes_revision = revision - 1)
    ),
    -- The promoted supersession pointer must also agree with content.supersedes.
    CHECK (
        (supersedes_id IS NULL AND (content->'supersedes') IS NULL)
        OR (supersedes_id = content->'supersedes'->>'outcome_id'
            AND supersedes_revision = (content->'supersedes'->>'revision')::bigint)
    ),
    -- A satisfied Outcome must bind at least one AUTHORITATIVE, SIGNED
    -- observation in content: every observation element must carry a non-empty
    -- authority AND signature_key_id (an ActionReceipt, and an unsigned or
    -- authority-less observation, can never stand in). Genuine mirror of
    -- sdk/go/outcome.Outcome.Validate + the JSON Schema's satisfied gate.
    CHECK (status <> 'satisfied' OR outcome_observations_all_signed(content->'claim'->'observations'))
);

-- No separate index: UNIQUE (tenant, outcome_key, revision) already builds the
-- btree the store's point lookups (GetOutcome) and head scan (LatestOutcome,
-- AppendOutcome) use.

-- Append-only lineage nodes: one immutable, versioned node per
-- (tenant, node_type, business_id, revision). id derives ONLY from those four
-- (sdk/go/outcome.StableID) -- a graph-provider (AGE/Cypher) internal id never
-- appears here or in the public contract.
CREATE TABLE outcome_lineage_nodes (
    id           text PRIMARY KEY,
    tenant       text NOT NULL REFERENCES enterprises(id),
    node_type    text NOT NULL CHECK (node_type IN ('outcome','goal','work_case','observation','action_receipt','blocker','contributor','evidence')),
    business_id  text NOT NULL CHECK (business_id <> ''),
    revision     bigint NOT NULL CHECK (revision >= 1),
    summary      text,
    created_at   timestamptz NOT NULL DEFAULT now(),
    UNIQUE (tenant, node_type, business_id, revision)
);

-- Append-only lineage edges. Both endpoints bind the edge's own tenant: a
-- cross-tenant edge is refused by the CHECK below (defense-in-depth mirror of
-- sdk/go/outcome.LineageEdge.Validate). Endpoints are always versioned
-- (revision >= 1).
CREATE TABLE outcome_lineage_edges (
    id               text PRIMARY KEY,
    tenant           text NOT NULL REFERENCES enterprises(id),
    edge_type        text NOT NULL CHECK (edge_type IN ('concerns','supersedes','corrects','contributes_to','evidences','observes','blocked_by','derived_from')),
    from_tenant      text NOT NULL,
    from_type        text NOT NULL,
    from_business_id text NOT NULL CHECK (from_business_id <> ''),
    from_revision    bigint NOT NULL CHECK (from_revision >= 1),
    to_tenant        text NOT NULL,
    to_type          text NOT NULL,
    to_business_id   text NOT NULL CHECK (to_business_id <> ''),
    to_revision      bigint NOT NULL CHECK (to_revision >= 1),
    created_at       timestamptz NOT NULL DEFAULT now(),
    CHECK (from_tenant = tenant AND to_tenant = tenant)
);

-- Authoritative projection outbox: immutable per-tenant rebuild inputs the
-- Task 0I AGE projector will consume. A source deletion/revocation APPENDS a
-- versioned tombstone (+ a bounded reevaluation), naming the sequence it
-- supersedes; it never edits an existing event. sequence is per-tenant
-- monotonic.
CREATE TABLE outcome_projection_events (
    tenant              text NOT NULL REFERENCES enterprises(id),
    sequence            bigint NOT NULL CHECK (sequence >= 1),
    kind                text NOT NULL CHECK (kind IN ('outcome_revision','lineage_fact','tombstone','reevaluation')),
    subject_type        text NOT NULL CHECK (subject_type IN ('outcome','goal','work_case','observation','action_receipt','blocker','contributor','evidence')),
    subject_id          text NOT NULL CHECK (subject_id <> ''),
    subject_revision    bigint NOT NULL CHECK (subject_revision >= 1),
    payload_hash        text NOT NULL CHECK (payload_hash <> ''),
    supersedes_sequence bigint,
    recorded_at         timestamptz NOT NULL,
    created_at          timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant, sequence)
);

-- Per-tenant projection progress. This is the ONE mutable record in the 0G
-- schema (a watermark advances), so it deliberately carries NO append-only
-- immutability trigger. It only ever moves forward -- the store's
-- AdvanceWatermark refuses a regression.
CREATE TABLE outcome_projection_watermarks (
    tenant        text PRIMARY KEY REFERENCES enterprises(id),
    last_sequence bigint NOT NULL CHECK (last_sequence >= 0),
    updated_at    timestamptz NOT NULL
);

-- Governed result history is insertable once, never rewritten or removed in
-- place (mirrors reject_immutable_operating_map_change in migration 000015,
-- scoped to this task's own tables). Row-level triggers do not fire on
-- TRUNCATE, so append-only also needs the statement-level guard.
-- +goose StatementBegin
CREATE FUNCTION reject_immutable_outcome_change() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION '% records are immutable and append-only', TG_TABLE_NAME;
END;
$$;
-- +goose StatementEnd

CREATE TRIGGER outcomes_immutable
BEFORE UPDATE OR DELETE ON outcomes
FOR EACH ROW EXECUTE FUNCTION reject_immutable_outcome_change();
CREATE TRIGGER outcomes_no_truncate
BEFORE TRUNCATE ON outcomes
FOR EACH STATEMENT EXECUTE FUNCTION reject_immutable_outcome_change();

CREATE TRIGGER outcome_lineage_nodes_immutable
BEFORE UPDATE OR DELETE ON outcome_lineage_nodes
FOR EACH ROW EXECUTE FUNCTION reject_immutable_outcome_change();
CREATE TRIGGER outcome_lineage_nodes_no_truncate
BEFORE TRUNCATE ON outcome_lineage_nodes
FOR EACH STATEMENT EXECUTE FUNCTION reject_immutable_outcome_change();

CREATE TRIGGER outcome_lineage_edges_immutable
BEFORE UPDATE OR DELETE ON outcome_lineage_edges
FOR EACH ROW EXECUTE FUNCTION reject_immutable_outcome_change();
CREATE TRIGGER outcome_lineage_edges_no_truncate
BEFORE TRUNCATE ON outcome_lineage_edges
FOR EACH STATEMENT EXECUTE FUNCTION reject_immutable_outcome_change();

CREATE TRIGGER outcome_projection_events_immutable
BEFORE UPDATE OR DELETE ON outcome_projection_events
FOR EACH ROW EXECUTE FUNCTION reject_immutable_outcome_change();
CREATE TRIGGER outcome_projection_events_no_truncate
BEFORE TRUNCATE ON outcome_projection_events
FOR EACH STATEMENT EXECUTE FUNCTION reject_immutable_outcome_change();

-- +goose Down

-- Defensive guard mirroring 000015: governed Outcome/lineage/projection history
-- is an authoritative append-only record, so refuse to drop it silently on
-- downgrade. This migration owns FOUR governed tables and AppendLineage /
-- AppendProjectionEvent do NOT require an outcome to exist, so the guard must
-- cover EVERY governed table it is about to drop -- not only `outcomes` (a
-- lineage/projection-only history with zero outcomes is reachable, and dropping
-- it would destroy authoritative history).
-- +goose StatementBegin
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM outcomes)
       OR EXISTS (SELECT 1 FROM outcome_lineage_nodes)
       OR EXISTS (SELECT 1 FROM outcome_lineage_edges)
       OR EXISTS (SELECT 1 FROM outcome_projection_events) THEN
        RAISE EXCEPTION 'cannot downgrade 000016: governed Outcome/lineage/projection history exists';
    END IF;
END $$;
-- +goose StatementEnd

DROP TRIGGER outcome_projection_events_no_truncate ON outcome_projection_events;
DROP TRIGGER outcome_projection_events_immutable ON outcome_projection_events;
DROP TRIGGER outcome_lineage_edges_no_truncate ON outcome_lineage_edges;
DROP TRIGGER outcome_lineage_edges_immutable ON outcome_lineage_edges;
DROP TRIGGER outcome_lineage_nodes_no_truncate ON outcome_lineage_nodes;
DROP TRIGGER outcome_lineage_nodes_immutable ON outcome_lineage_nodes;
DROP TRIGGER outcomes_no_truncate ON outcomes;
DROP TRIGGER outcomes_immutable ON outcomes;
DROP FUNCTION reject_immutable_outcome_change();

DROP TABLE outcome_projection_watermarks;
DROP TABLE outcome_projection_events;
DROP TABLE outcome_lineage_edges;
DROP TABLE outcome_lineage_nodes;
DROP TABLE outcomes;

DROP FUNCTION outcome_observations_all_signed(jsonb);
