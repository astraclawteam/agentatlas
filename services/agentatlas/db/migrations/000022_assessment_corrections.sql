-- +goose Up

-- assessment_corrections is the append-only, immutable store of B2 employee
-- assessment CORRECTIONS (Task 18D). Each row is one employee's correction
-- against a SPECIFIC immutable WorkAssessment version (migration 000021): it ADDS
-- evidence or CHALLENGES a fact/attribution. A correction NEVER edits the old
-- assessment in place, NEVER deletes a signed receipt and NEVER mutates a
-- structured result. An ACCEPTED correction produces a NEW assessment VERSION
-- (a new work_assessments row via 18C re-evaluation) whose id is linked here in
-- new_assessment_id; the OLD version stays immutable and traceable (this row
-- binds the exact target id + digest).
--
-- The correction record itself is append-only: it is submitted once (pending)
-- and RESOLVED exactly once (accepted|rejected), and then frozen. Its immutable
-- CORE (target, kind, dimension, added evidence, challenged handle, rationale,
-- submitted_at) never changes in place; only the one-shot resolution is recorded.
-- This mirrors the append-only-immutable + one-transition + down-guard discipline
-- of the assessment-policy store (000020) and the work-assessment store (000021).
--
-- content is the full sdk-shaped AssessmentCorrectionCase as JSONB (one
-- json.Marshal/Unmarshal, no per-column drift); the promoted scalar columns exist
-- for indexing, tenant isolation and the CHECK constraints. content carries only
-- opaque handles, versioned keys, bounded permitted summaries and enums -- never a
-- connector endpoint, a credential or raw enterprise content (the SDK's
-- NoRawContentLeak is the authoritative enforcer of the opaque-content boundary).
--
-- id is the deterministic id derived from the correction's immutable core
-- (assessment.AssessmentCorrectionCase.DerivedID); (tenant, id) is the primary
-- key. (tenant, target_assessment_id) references the exact immutable assessment
-- version so a correction can never dangle from a non-existent result.
CREATE TABLE assessment_corrections (
    tenant               text    NOT NULL REFERENCES enterprises(id),
    id                   text    NOT NULL CHECK (id <> ''),
    subject              text    NOT NULL CHECK (subject <> ''),
    target_assessment_id text    NOT NULL CHECK (target_assessment_id <> ''),
    target_digest        text    NOT NULL CHECK (target_digest <> ''),
    kind                 text    NOT NULL CHECK (kind IN ('add_evidence','challenge_fact','challenge_attribution')),
    dimension            text    NOT NULL CHECK (dimension <> ''),
    resolution_state     text    NOT NULL CHECK (resolution_state IN ('pending','accepted','rejected')),
    new_assessment_id    text,
    content              jsonb   NOT NULL,
    submitted_at         timestamptz NOT NULL,
    decided_at           timestamptz,
    recorded_at          timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant, id),

    -- A correction is bound to a REAL immutable assessment version; it can never
    -- dangle from a non-existent result.
    FOREIGN KEY (tenant, target_assessment_id) REFERENCES work_assessments (tenant, id),

    -- Promoted scalar columns must NEVER disagree with content (the store filters
    -- on the columns while Get unmarshals content). coalesce to '' makes a MISSING
    -- content field fail closed.
    CHECK (tenant = coalesce(content->>'tenant', '')),
    CHECK (subject = coalesce(content->>'subject', '')),
    CHECK (target_assessment_id = coalesce(content->>'target_assessment_id', '')),
    CHECK (target_digest = coalesce(content->>'target_digest', '')),
    CHECK (kind = coalesce(content->>'kind', '')),
    CHECK (dimension = coalesce(content->>'dimension', '')),
    CHECK (resolution_state = coalesce(content->'resolution'->>'state', '')),
    -- A resolved correction must record who/when; a pending one must not.
    CHECK ((resolution_state = 'pending') = (decided_at IS NULL)),
    -- Only an ACCEPTED correction may link a new assessment version.
    CHECK (new_assessment_id IS NULL OR resolution_state = 'accepted')
);

-- Per-subject reads and the per-assessment audit trail.
CREATE INDEX assessment_corrections_subject ON assessment_corrections (tenant, subject);
CREATE INDEX assessment_corrections_target ON assessment_corrections (tenant, target_assessment_id);

-- Immutability: a correction is governed history. It is never deleted or
-- truncated; its CORE never changes in place; it resolves from pending to a
-- terminal state exactly once. Row-level triggers do not fire on TRUNCATE, so a
-- statement-level guard is added too (mirrors 000020/000021).
-- +goose StatementBegin
CREATE FUNCTION reject_immutable_assessment_correction_change() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP = 'TRUNCATE' THEN
        RAISE EXCEPTION '% is append-only correction history; it is never truncated', TG_TABLE_NAME;
    END IF;
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION '% is append-only correction history; a correction is never deleted', TG_TABLE_NAME;
    END IF;
    -- UPDATE: only the one-shot resolution may be recorded.
    IF OLD.resolution_state <> 'pending' THEN
        RAISE EXCEPTION 'a resolved assessment correction is immutable';
    END IF;
    IF NEW.resolution_state NOT IN ('accepted','rejected') THEN
        RAISE EXCEPTION 'an assessment correction resolves to accepted or rejected exactly once';
    END IF;
    IF NEW.tenant <> OLD.tenant OR NEW.id <> OLD.id
       OR NEW.subject <> OLD.subject
       OR NEW.target_assessment_id <> OLD.target_assessment_id
       OR NEW.target_digest <> OLD.target_digest
       OR NEW.kind <> OLD.kind
       OR NEW.dimension <> OLD.dimension
       OR NEW.submitted_at <> OLD.submitted_at THEN
        RAISE EXCEPTION 'assessment correction identity/core is immutable; only its one-shot resolution may be recorded';
    END IF;
    -- The full correction core (target, kind, dimension, added evidence, challenge,
    -- rationale) is immutable; only the resolution key of content may change.
    IF (NEW.content - 'resolution') IS DISTINCT FROM (OLD.content - 'resolution') THEN
        RAISE EXCEPTION 'assessment correction core is immutable; a new correction is a new record';
    END IF;
    RETURN NEW;
END;
$$;
-- +goose StatementEnd

CREATE TRIGGER assessment_corrections_immutable
BEFORE UPDATE OR DELETE ON assessment_corrections
FOR EACH ROW EXECUTE FUNCTION reject_immutable_assessment_correction_change();
CREATE TRIGGER assessment_corrections_no_truncate
BEFORE TRUNCATE ON assessment_corrections
FOR EACH STATEMENT EXECUTE FUNCTION reject_immutable_assessment_correction_change();

-- +goose Down

-- Defensive guard mirroring 000020/000021: the correction store is append-only
-- governed history, so refuse to drop it silently once history exists.
-- +goose StatementBegin
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM assessment_corrections) THEN
        RAISE EXCEPTION 'cannot downgrade 000022: assessment_corrections history exists';
    END IF;
END $$;
-- +goose StatementEnd

DROP TRIGGER assessment_corrections_no_truncate ON assessment_corrections;
DROP TRIGGER assessment_corrections_immutable ON assessment_corrections;
DROP FUNCTION reject_immutable_assessment_correction_change();
DROP INDEX assessment_corrections_target;
DROP INDEX assessment_corrections_subject;
DROP TABLE assessment_corrections;
