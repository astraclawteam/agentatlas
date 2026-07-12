-- +goose Up
CREATE TABLE atlas_browser_login_attempts (
    state_hash text PRIMARY KEY CHECK (state_hash ~ '^[0-9a-f]{64}$'),
    nonce text NOT NULL CHECK (char_length(nonce) BETWEEN 16 AND 256),
    pkce_verifier_ciphertext bytea NOT NULL,
    return_to text NOT NULL CHECK (char_length(return_to) BETWEEN 1 AND 2048),
    created_at timestamptz NOT NULL DEFAULT now(),
    expires_at timestamptz NOT NULL,
    CHECK (expires_at > created_at)
);
CREATE INDEX atlas_browser_login_attempts_expiry_idx ON atlas_browser_login_attempts(expires_at);

CREATE TABLE atlas_browser_sessions (
    session_hash text PRIMARY KEY CHECK (session_hash ~ '^[0-9a-f]{64}$'),
    enterprise_id text NOT NULL REFERENCES enterprises(id),
    enterprise_user_id text NOT NULL,
    display_name text NOT NULL DEFAULT '',
    org_version bigint NOT NULL CHECK (org_version > 0),
    org_unit_ids jsonb NOT NULL CHECK (jsonb_typeof(org_unit_ids)='array' AND jsonb_array_length(org_unit_ids) <= 1000),
    permissions jsonb NOT NULL CHECK (jsonb_typeof(permissions)='array' AND jsonb_array_length(permissions) <= 100),
    advanced_mode_allowed boolean NOT NULL DEFAULT false,
    upstream_access_token_ciphertext bytea NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    last_seen_at timestamptz NOT NULL DEFAULT now(),
    idle_expires_at timestamptz NOT NULL,
    absolute_expires_at timestamptz NOT NULL,
    revoked_at timestamptz,
    CHECK (idle_expires_at <= absolute_expires_at)
);
CREATE INDEX atlas_browser_sessions_expiry_idx ON atlas_browser_sessions(idle_expires_at,absolute_expires_at) WHERE revoked_at IS NULL;

ALTER TABLE publish_operations
    ADD COLUMN request_hash text CHECK (request_hash IS NULL OR request_hash ~ '^[0-9a-f]{64}$'),
    ADD COLUMN audit_ref_id text;
CREATE UNIQUE INDEX publish_operations_audit_ref_uniq ON publish_operations(audit_ref_id) WHERE audit_ref_id IS NOT NULL;

ALTER TABLE change_reviews DROP CONSTRAINT change_reviews_check;
ALTER TABLE change_reviews ADD CONSTRAINT change_reviews_check CHECK (
    (review_mode='single_confirmation' AND risk_level='low' AND reviewer_user_id IS NULL AND queue IS NULL)
    OR (review_mode='upward_review' AND risk_level IN ('low','high') AND reviewer_user_id IS NOT NULL AND queue IS NULL AND jsonb_array_length(org_path)>0)
    OR (review_mode='enterprise_knowledge_admin_queue' AND risk_level IN ('low','high') AND reviewer_user_id IS NULL AND queue IS NOT NULL AND queue<>'')
);

CREATE TABLE published_resource_pointers (
    enterprise_id text NOT NULL REFERENCES enterprises(id),
    resource_type text NOT NULL CHECK (resource_type IN ('knowledge_entry','sop','workflow','dream_policy','method_outline')),
    resource_id text NOT NULL,
    change_id text NOT NULL,
    change_version integer NOT NULL CHECK (change_version > 0),
	resource_version integer NOT NULL CHECK (resource_version > 0),
    audit_ref_id text NOT NULL CHECK (audit_ref_id <> ''),
    updated_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (enterprise_id,resource_type,resource_id),
    FOREIGN KEY (enterprise_id,change_id,change_version) REFERENCES change_versions(enterprise_id,change_id,version)
);

-- +goose Down
DROP TABLE published_resource_pointers;
ALTER TABLE change_reviews DROP CONSTRAINT change_reviews_check;
ALTER TABLE change_reviews ADD CONSTRAINT change_reviews_check CHECK (
    (review_mode='single_confirmation' AND risk_level='low' AND reviewer_user_id IS NULL AND queue IS NULL)
    OR (review_mode='upward_review' AND risk_level='high' AND reviewer_user_id IS NOT NULL AND queue IS NULL AND jsonb_array_length(org_path)>0)
    OR (review_mode='enterprise_knowledge_admin_queue' AND risk_level='high' AND reviewer_user_id IS NULL AND queue IS NOT NULL AND queue<>'')
);
DROP INDEX publish_operations_audit_ref_uniq;
ALTER TABLE publish_operations DROP COLUMN audit_ref_id, DROP COLUMN request_hash;
DROP TABLE atlas_browser_sessions;
DROP TABLE atlas_browser_login_attempts;
