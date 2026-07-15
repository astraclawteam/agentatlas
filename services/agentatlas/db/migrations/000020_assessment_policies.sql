-- +goose Up

-- assessment_policies is the append-only, governed store of B2 employee
-- work-assessment POLICY revisions distilled and governed by Task 18B
-- (internal/assessment) from the versioned governed sources (job responsibility,
-- organization goal, SOP, acceptance/review) and the provable Outcome lineage
-- (0G/0H) as projected by the 0I Outcome Graph.
--
-- Each row is one immutable, versioned policy revision. A revision's RULE SET
-- (dimensions + evidence/attribution/confidence rules) is immutable in place: a
-- rule edit APPENDS a NEW revision (supersedes_id), restarting the shadow count.
-- A revision advances through the closed lifecycle {draft, shadow, reviewing,
-- published, retired}; a published revision is frozen except for the single
-- terminal published -> retired transition, and a retired revision is fully
-- frozen. This mirrors the append-only-immutable + down-guard discipline of the
-- Outcome stores (migrations 000016/000017/000018).
--
-- FAIRNESS and NO-SILENT-PUBLICATION are structural: forbidden dimensions are
-- rejected before persistence (SDK + service), and the governed_publication
-- CHECK enforces that any reviewing/published/retired revision names the
-- governed review (governance_change_id) it flowed through — a policy never
-- self-publishes. The table holds only opaque, classified metadata: versioned
-- business keys, opaque evidence handles, bounded permitted rationales and the
-- deterministic revision id — never raw enterprise content, a connector
-- endpoint or a credential.
--
-- id is the deterministic, provider-neutral revision id
-- (sdk/go/assessment.Policy.DerivedID over tenant/policy_key/revision);
-- (tenant, id) is the primary key and (tenant, policy_key, revision) is unique.
--
-- NOTE ON NUMBERING: 000019 is deliberately skipped. 000019_agent_sessions.sql
-- belongs to the ELC-LLMROUTER-blocked Task 3 (not yet landed); goose runs
-- migrations in numeric order and tolerates the gap, which Task 3 fills later.
CREATE TABLE assessment_policies (
    tenant               text    NOT NULL REFERENCES enterprises(id),
    id                   text    NOT NULL CHECK (id <> ''),
    policy_key           text    NOT NULL CHECK (policy_key <> ''),
    revision             integer NOT NULL CHECK (revision >= 1),
    org                  text,
    status               text    NOT NULL CHECK (status IN ('draft','shadow','reviewing','published','retired')),
    sources              jsonb   NOT NULL,
    watermark            bigint  NOT NULL CHECK (watermark >= 0),
    dimensions           jsonb   NOT NULL,
    evidence_rules       jsonb   NOT NULL,
    attribution_rules    jsonb   NOT NULL,
    confidence_rules     jsonb   NOT NULL,
    shadow_cycles        jsonb   NOT NULL DEFAULT '[]'::jsonb,
    rule_digest          text    NOT NULL CHECK (rule_digest <> ''),
    governance_change_id text    NOT NULL DEFAULT '',
    supersedes_id        text    NOT NULL DEFAULT '',
    created_at           timestamptz NOT NULL,
    updated_at           timestamptz NOT NULL,
    recorded_at          timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant, id),
    UNIQUE (tenant, policy_key, revision),

    -- No silent publication: a reviewing/published/retired revision must name the
    -- governed review it flowed through. A policy can never reach a published
    -- lineage without a governance change id.
    CONSTRAINT assessment_policies_governed_publication CHECK (
        status NOT IN ('reviewing','published','retired') OR governance_change_id <> ''
    )
);

-- Head-revision lookups and status scans.
CREATE INDEX assessment_policies_key ON assessment_policies (tenant, policy_key);
CREATE INDEX assessment_policies_status ON assessment_policies (tenant, status);

-- Immutability: a policy revision is governed history. It is never deleted or
-- truncated; its rule set never changes in place (a rule edit is a new revision);
-- a published revision may make only the single published -> retired transition;
-- a retired revision is fully frozen. Row-level triggers do not fire on TRUNCATE,
-- so a statement-level guard is added too (mirrors 000016/000017/000018).
-- +goose StatementBegin
CREATE FUNCTION reject_immutable_assessment_policy_change() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP = 'TRUNCATE' THEN
        RAISE EXCEPTION '% is append-only governed policy history; it is never truncated', TG_TABLE_NAME;
    END IF;
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION '% is append-only governed policy history; a revision is never deleted', TG_TABLE_NAME;
    END IF;
    -- UPDATE guards.
    IF OLD.status = 'retired' THEN
        RAISE EXCEPTION 'a retired assessment policy revision is immutable';
    END IF;
    IF OLD.status = 'published' AND NEW.status <> 'retired' THEN
        RAISE EXCEPTION 'a published assessment policy revision is immutable except for retirement';
    END IF;
    IF NEW.tenant <> OLD.tenant OR NEW.policy_key <> OLD.policy_key OR NEW.revision <> OLD.revision THEN
        RAISE EXCEPTION 'assessment policy identity (tenant, policy_key, revision) is immutable';
    END IF;
    IF NEW.rule_digest <> OLD.rule_digest
       OR NEW.dimensions::text <> OLD.dimensions::text
       OR NEW.evidence_rules::text <> OLD.evidence_rules::text
       OR NEW.attribution_rules::text <> OLD.attribution_rules::text
       OR NEW.confidence_rules::text <> OLD.confidence_rules::text THEN
        RAISE EXCEPTION 'assessment policy rules are immutable within a revision; a rule edit must create a new revision';
    END IF;
    RETURN NEW;
END;
$$;
-- +goose StatementEnd

CREATE TRIGGER assessment_policies_immutable
BEFORE UPDATE OR DELETE ON assessment_policies
FOR EACH ROW EXECUTE FUNCTION reject_immutable_assessment_policy_change();
CREATE TRIGGER assessment_policies_no_truncate
BEFORE TRUNCATE ON assessment_policies
FOR EACH STATEMENT EXECUTE FUNCTION reject_immutable_assessment_policy_change();

-- +goose Down

-- Defensive guard mirroring 000016/000017/000018: the assessment-policy store is
-- append-only governed history, so refuse to drop it silently once history exists.
-- +goose StatementBegin
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM assessment_policies) THEN
        RAISE EXCEPTION 'cannot downgrade 000020: assessment_policies history exists';
    END IF;
END $$;
-- +goose StatementEnd

DROP TRIGGER assessment_policies_no_truncate ON assessment_policies;
DROP TRIGGER assessment_policies_immutable ON assessment_policies;
DROP FUNCTION reject_immutable_assessment_policy_change();
DROP INDEX assessment_policies_status;
DROP INDEX assessment_policies_key;
DROP TABLE assessment_policies;
