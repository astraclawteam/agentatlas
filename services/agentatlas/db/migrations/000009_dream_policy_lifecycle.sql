-- +goose Up
ALTER TABLE dream_policies
    ADD COLUMN revision integer NOT NULL DEFAULT 0 CHECK (revision >= 0),
    ADD COLUMN requester_user_id text NOT NULL DEFAULT '',
    ADD COLUMN permission_mode text NOT NULL DEFAULT 'direct_edit'
        CHECK (permission_mode IN ('direct_edit','suggestion_only')),
    ADD COLUMN pending_action text NOT NULL DEFAULT '' CHECK (pending_action IN ('','publish','disable')),
    ADD COLUMN review_state text NOT NULL DEFAULT '' CHECK (review_state IN ('','pending','approved','rejected')),
    ADD COLUMN risk_level text NOT NULL DEFAULT '' CHECK (risk_level IN ('','low','high')),
    ADD COLUMN risk_reasons jsonb NOT NULL DEFAULT '[]'::jsonb,
    ADD COLUMN review_mode text NOT NULL DEFAULT ''
        CHECK (review_mode IN ('','single_confirmation','upward_review','enterprise_knowledge_admin_queue')),
    ADD COLUMN reviewer_user_id text,
    ADD COLUMN review_org_path jsonb NOT NULL DEFAULT '[]'::jsonb,
    ADD COLUMN review_queue text,
    ADD COLUMN decision text NOT NULL DEFAULT '' CHECK (decision IN ('','approve','reject')),
    ADD COLUMN audit_ref_id text NOT NULL DEFAULT '';

ALTER TABLE dream_runs ADD COLUMN audit_ref_id text;

-- +goose StatementBegin
CREATE FUNCTION dream_policy_lifecycle_result(policy dream_policies, published_version integer)
RETURNS jsonb LANGUAGE sql STABLE AS $$
SELECT jsonb_strip_nulls(jsonb_build_object(
    'dream_policy_id', policy.id,
    'status', CASE policy.review_state WHEN 'pending' THEN 'review_pending' WHEN 'approved' THEN 'approved' WHEN 'rejected' THEN 'rejected' ELSE policy.status END,
    'revision', policy.revision,
    'version', published_version,
    'requester_user_id', policy.requester_user_id,
    'permission_mode', policy.permission_mode,
    'risk_level', NULLIF(policy.risk_level, ''),
    'risk_reasons', policy.risk_reasons,
    'review_mode', NULLIF(policy.review_mode, ''),
    'reviewer_user_id', policy.reviewer_user_id,
    'org_path', policy.review_org_path,
    'queue', policy.review_queue,
    'pending_action', NULLIF(policy.pending_action, ''),
    'review_state', NULLIF(policy.review_state, ''),
    'policy', policy.draft
));
$$;
-- +goose StatementEnd

CREATE TABLE dream_policy_operations (
    enterprise_id text NOT NULL REFERENCES enterprises(id),
    operation_key text NOT NULL CHECK (char_length(operation_key) BETWEEN 16 AND 128),
    operation_kind text NOT NULL CHECK (operation_kind IN ('create','update','review','decision','publish','disable','rerun','backfill')),
    policy_id text NOT NULL,
    actor_user_id text NOT NULL CHECK (actor_user_id <> ''),
    request_hash text NOT NULL CHECK (request_hash ~ '^[0-9a-f]{64}$'),
    facts_nonce text NOT NULL CHECK (char_length(facts_nonce) BETWEEN 16 AND 128),
    facts_issued_at timestamptz NOT NULL DEFAULT now(),
    facts_expires_at timestamptz NOT NULL DEFAULT (now() + interval '5 minutes'),
    audit_ref_id text,
    status text NOT NULL DEFAULT 'pending' CHECK (status IN ('pending','completed')),
    result jsonb,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (enterprise_id, operation_key),
    CHECK ((status='pending' AND result IS NULL) OR (status='completed' AND result IS NOT NULL))
);
CREATE UNIQUE INDEX dream_policy_operations_audit_ref_uniq
    ON dream_policy_operations(audit_ref_id) WHERE audit_ref_id IS NOT NULL;

CREATE TABLE dream_policy_transition_audits (
    enterprise_id text NOT NULL REFERENCES enterprises(id),
    policy_id text NOT NULL REFERENCES dream_policies(id),
    revision integer NOT NULL CHECK (revision >= 0),
    transition text NOT NULL,
    operation_key text NOT NULL,
    audit_ref_id text NOT NULL CHECK (audit_ref_id <> ''),
    actor_user_id text NOT NULL CHECK (actor_user_id <> ''),
    created_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (enterprise_id, policy_id, revision, transition),
    UNIQUE (audit_ref_id),
    FOREIGN KEY (enterprise_id, operation_key) REFERENCES dream_policy_operations(enterprise_id, operation_key)
);

ALTER TABLE dream_runs ADD CONSTRAINT dream_explicit_runs_require_audit
    CHECK (operation_kind NOT IN ('manual_rerun','backfill') OR audit_ref_id IS NOT NULL);

-- +goose Down
ALTER TABLE dream_runs DROP CONSTRAINT dream_explicit_runs_require_audit;
DROP TABLE dream_policy_transition_audits;
DROP TABLE dream_policy_operations;
ALTER TABLE dream_runs DROP COLUMN audit_ref_id;
DROP FUNCTION dream_policy_lifecycle_result(dream_policies, integer);
-- Defensive mapping permits rollback from prerelease v9 databases that used
-- lifecycle state in the status column.
ALTER TABLE dream_policies DROP CONSTRAINT IF EXISTS dream_policies_status_check;
UPDATE dream_policies SET status='draft' WHERE status IN ('review_pending','approved','rejected');
ALTER TABLE dream_policies
    DROP COLUMN audit_ref_id,
    DROP COLUMN decision,
    DROP COLUMN review_queue,
    DROP COLUMN review_org_path,
    DROP COLUMN reviewer_user_id,
    DROP COLUMN review_mode,
    DROP COLUMN risk_reasons,
    DROP COLUMN risk_level,
    DROP COLUMN review_state,
    DROP COLUMN pending_action,
    DROP COLUMN permission_mode,
    DROP COLUMN requester_user_id,
    DROP COLUMN revision;
ALTER TABLE dream_policies ADD CONSTRAINT dream_policies_status_check
    CHECK (status IN ('draft','published','disabled'));
