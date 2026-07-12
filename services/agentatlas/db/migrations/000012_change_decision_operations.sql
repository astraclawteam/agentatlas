-- +goose Up
CREATE TABLE change_decision_operations (
    id text PRIMARY KEY,
    enterprise_id text NOT NULL REFERENCES enterprises(id),
    idempotency_key text NOT NULL CHECK (char_length(idempotency_key) BETWEEN 16 AND 128),
    change_id text NOT NULL,
    change_revision integer NOT NULL CHECK (change_revision > 0),
    actor_user_id text NOT NULL CHECK (actor_user_id <> ''),
    decision text NOT NULL CHECK (decision IN ('approve','reject')),
    request_hash text NOT NULL CHECK (request_hash ~ '^[0-9a-f]{64}$'),
    status text NOT NULL CHECK (status IN ('pending','succeeded')),
    audit_ref_id text,
    created_at timestamptz NOT NULL DEFAULT now(),
    finished_at timestamptz,
    UNIQUE (enterprise_id,idempotency_key),
    UNIQUE (enterprise_id,change_id,change_revision),
    FOREIGN KEY (enterprise_id,change_id) REFERENCES change_drafts(enterprise_id,id)
);

CREATE TABLE change_decision_audits (
    id text PRIMARY KEY,
    enterprise_id text NOT NULL REFERENCES enterprises(id),
    operation_id text NOT NULL REFERENCES change_decision_operations(id),
    change_id text NOT NULL,
    change_revision integer NOT NULL CHECK (change_revision > 0),
    actor_user_id text NOT NULL CHECK (actor_user_id <> ''),
    decision text NOT NULL CHECK (decision IN ('approve','reject')),
    audit_ref_id text NOT NULL CHECK (audit_ref_id <> ''),
    created_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (enterprise_id,change_id,change_revision),
    UNIQUE (enterprise_id,audit_ref_id),
    FOREIGN KEY (enterprise_id,change_id) REFERENCES change_drafts(enterprise_id,id)
);

-- +goose Down
DROP TABLE change_decision_audits;
DROP TABLE change_decision_operations;
