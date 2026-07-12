-- +goose Up
ALTER TABLE dream_policies DROP CONSTRAINT dream_policies_status_check;
ALTER TABLE dream_policies
    ADD COLUMN revision integer NOT NULL DEFAULT 0 CHECK (revision >= 0),
    ADD COLUMN requester_user_id text NOT NULL DEFAULT '',
    ADD COLUMN permission_mode text NOT NULL DEFAULT 'direct_edit'
        CHECK (permission_mode IN ('direct_edit','suggestion_only')),
    ADD COLUMN risk_level text NOT NULL DEFAULT '' CHECK (risk_level IN ('','low','high')),
    ADD COLUMN risk_reasons jsonb NOT NULL DEFAULT '[]'::jsonb,
    ADD COLUMN review_mode text NOT NULL DEFAULT ''
        CHECK (review_mode IN ('','single_confirmation','upward_review','enterprise_knowledge_admin_queue')),
    ADD COLUMN reviewer_user_id text,
    ADD COLUMN review_org_path jsonb NOT NULL DEFAULT '[]'::jsonb,
    ADD COLUMN review_queue text,
    ADD COLUMN decision text NOT NULL DEFAULT '' CHECK (decision IN ('','approve','reject')),
    ADD COLUMN audit_ref_id text NOT NULL DEFAULT '';
ALTER TABLE dream_policies ADD CONSTRAINT dream_policies_status_check
    CHECK (status IN ('draft','review_pending','approved','rejected','published','disabled'));

CREATE TABLE dream_policy_transition_audits (
    enterprise_id text NOT NULL REFERENCES enterprises(id),
    policy_id text NOT NULL REFERENCES dream_policies(id),
    revision integer NOT NULL CHECK (revision >= 0),
    transition text NOT NULL,
    audit_ref_id text NOT NULL CHECK (audit_ref_id <> ''),
    actor_user_id text NOT NULL CHECK (actor_user_id <> ''),
    created_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (enterprise_id, policy_id, revision, transition),
    UNIQUE (audit_ref_id)
);

-- +goose Down
DROP TABLE dream_policy_transition_audits;
ALTER TABLE dream_policies DROP CONSTRAINT dream_policies_status_check;
ALTER TABLE dream_policies
    DROP COLUMN audit_ref_id,
    DROP COLUMN decision,
    DROP COLUMN review_queue,
    DROP COLUMN review_org_path,
    DROP COLUMN reviewer_user_id,
    DROP COLUMN review_mode,
    DROP COLUMN risk_reasons,
    DROP COLUMN risk_level,
    DROP COLUMN permission_mode,
    DROP COLUMN requester_user_id,
    DROP COLUMN revision;
ALTER TABLE dream_policies ADD CONSTRAINT dream_policies_status_check
    CHECK (status IN ('draft','published','disabled'));
