-- +goose Up

-- work_assessments is the append-only, immutable, VERSIONED store of B2 employee
-- work-assessment RESULTS (github.com/astraclawteam/agentatlas/sdk/go/assessment
-- WorkAssessment, Task 18C): one row per (tenant, subject, policy_key,
-- policy_revision, version, period). Each row is a frozen, evidence-grounded
-- assessment produced deterministically by internal/assessment.Evaluator from the
-- authoritative WorkCases/Outcomes (0G/0H) as found through the exact 0I Outcome
-- Graph watermark bound in the row.
--
-- A WorkAssessment is fully IMMUTABLE once written: it has no lifecycle
-- transitions (unlike an assessment POLICY, migration 000020). A re-assessment is
-- a NEW version (a new row and a new deterministic id), never an in-place edit,
-- and running the same inputs again reproduces the same version's content exactly.
-- This mirrors the append-only-immutable + down-guard discipline of the Outcome
-- store (migration 000016) and the assessment-policy store (000020).
--
-- content is the full sdk/go/assessment.WorkAssessment as JSONB (one
-- json.Marshal/Unmarshal, no per-column drift); the promoted scalar columns exist
-- for indexing, tenant isolation and the CHECK constraints. content carries only
-- opaque handles, versioned keys, bounded permitted summaries and enums -- never a
-- connector endpoint, a credential or raw enterprise content (the SDK's
-- NoRawContentLeak is the authoritative enforcer of the opaque-content boundary).
--
-- id is the deterministic, provider-neutral id
-- (sdk/go/assessment.WorkAssessment.DerivedID over tenant/subject/policy_key/
-- policy_revision/version/period); (tenant, id) is the primary key.
--
-- NOTE ON NUMBERING: 000019 remains deliberately skipped (the ELC-blocked Task 3
-- agent_sessions migration fills it later); goose runs migrations in numeric order
-- and tolerates the gap. 000020 is the assessment POLICY store (18B); this is its
-- 18C result sibling.
CREATE TABLE work_assessments (
    tenant          text    NOT NULL REFERENCES enterprises(id),
    id              text    NOT NULL CHECK (id <> ''),
    subject         text    NOT NULL CHECK (subject <> ''),
    level           text    NOT NULL CHECK (level IN ('individual','group','department','business_unit','company')),
    policy_key      text    NOT NULL CHECK (policy_key <> ''),
    policy_revision integer NOT NULL CHECK (policy_revision >= 1),
    version         integer NOT NULL CHECK (version >= 1),
    org             text,
    formal          boolean NOT NULL,
    org_version     bigint  NOT NULL CHECK (org_version >= 0),
    period_start    timestamptz NOT NULL,
    period_end      timestamptz NOT NULL CHECK (period_end > period_start),
    graph_watermark bigint  NOT NULL CHECK (graph_watermark >= 0),
    digest          text    NOT NULL CHECK (digest <> ''),
    content         jsonb   NOT NULL,
    created_at      timestamptz NOT NULL,
    recorded_at     timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant, id),
    -- One assessment per (subject, policy revision, version, period): a
    -- re-assessment is a new VERSION, never an overwrite.
    UNIQUE (tenant, subject, policy_key, policy_revision, version, period_start, period_end),

    -- Promoted scalar columns must NEVER disagree with content (the store filters
    -- on the columns while Get unmarshals content). coalesce to ''/-1 makes a
    -- MISSING content field fail closed. (period_start/end excluded: timestamptz
    -- microsecond storage can differ from the JSON nanosecond text, matching the
    -- decided_at exclusion in migration 000016.)
    CHECK (tenant = coalesce(content->>'tenant', '')),
    CHECK (subject = coalesce(content->>'subject', '')),
    CHECK (level = coalesce(content->>'level', '')),
    CHECK (policy_key = coalesce(content->>'policy_key', '')),
    CHECK (policy_revision = coalesce((content->>'policy_revision')::bigint, -1)),
    CHECK (version = coalesce((content->>'version')::bigint, -1)),
    CHECK (formal = coalesce((content->>'formal')::boolean, NOT formal)),
    CHECK (org_version = coalesce((content->>'org_version')::bigint, -1)),
    CHECK (graph_watermark = coalesce((content->'graph'->>'watermark')::bigint, -1))
);

-- Subject-history and status scans.
CREATE INDEX work_assessments_subject ON work_assessments (tenant, subject, policy_key);
CREATE INDEX work_assessments_policy ON work_assessments (tenant, policy_key, policy_revision);

-- Immutability: a work assessment is governed history. It is never updated,
-- deleted or truncated in place; a re-assessment is a new version (a new row).
-- Row-level triggers do not fire on TRUNCATE, so a statement-level guard is added
-- too (mirrors migration 000016's reject_immutable_outcome_change).
-- +goose StatementBegin
CREATE FUNCTION reject_immutable_work_assessment_change() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION '% records are immutable and append-only; a re-assessment is a new version', TG_TABLE_NAME;
END;
$$;
-- +goose StatementEnd

CREATE TRIGGER work_assessments_immutable
BEFORE UPDATE OR DELETE ON work_assessments
FOR EACH ROW EXECUTE FUNCTION reject_immutable_work_assessment_change();
CREATE TRIGGER work_assessments_no_truncate
BEFORE TRUNCATE ON work_assessments
FOR EACH STATEMENT EXECUTE FUNCTION reject_immutable_work_assessment_change();

-- +goose Down

-- Defensive guard mirroring 000016/000020: the work-assessment store is
-- append-only governed history, so refuse to drop it silently once history exists.
-- +goose StatementBegin
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM work_assessments) THEN
        RAISE EXCEPTION 'cannot downgrade 000021: work_assessments history exists';
    END IF;
END $$;
-- +goose StatementEnd

DROP TRIGGER work_assessments_no_truncate ON work_assessments;
DROP TRIGGER work_assessments_immutable ON work_assessments;
DROP FUNCTION reject_immutable_work_assessment_change();
DROP INDEX work_assessments_policy;
DROP INDEX work_assessments_subject;
DROP TABLE work_assessments;
