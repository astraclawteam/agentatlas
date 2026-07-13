-- +goose Up
ALTER TABLE dream_policy_operations DROP CONSTRAINT dream_policy_operations_operation_kind_check;
ALTER TABLE dream_policy_operations ADD CONSTRAINT dream_policy_operations_operation_kind_check
    CHECK (operation_kind IN ('create','update','review','decision','publish','disable','rerun','backfill','adopt'));

CREATE TABLE dream_policy_adoptions (
    enterprise_id             text NOT NULL REFERENCES enterprises(id),
    source_policy_id          text NOT NULL REFERENCES dream_policies(id),
    source_requester_user_id  text NOT NULL CHECK (source_requester_user_id <> ''),
    source_revision           integer NOT NULL CHECK (source_revision >= 0),
    target_policy_id          text NOT NULL REFERENCES dream_policies(id),
    adopter_user_id           text NOT NULL CHECK (adopter_user_id <> ''),
    audit_ref_id              text NOT NULL CHECK (audit_ref_id <> ''),
    operation_key             text NOT NULL,
    created_at                timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (enterprise_id, source_policy_id, source_revision),
    UNIQUE (enterprise_id, target_policy_id),
    UNIQUE (audit_ref_id),
    FOREIGN KEY (enterprise_id, operation_key) REFERENCES dream_policy_operations(enterprise_id, operation_key)
);

CREATE TRIGGER dream_policy_adoptions_immutable
BEFORE UPDATE OR DELETE ON dream_policy_adoptions
FOR EACH ROW EXECUTE FUNCTION reject_immutable_dream_output_change();

-- +goose Down
DELETE FROM dream_policy_transition_audits WHERE policy_id IN (SELECT target_policy_id FROM dream_policy_adoptions);
DROP TRIGGER dream_policy_adoptions_immutable ON dream_policy_adoptions;
DROP TABLE dream_policy_adoptions;
DELETE FROM dream_policy_operations WHERE operation_kind='adopt';
ALTER TABLE dream_policy_operations DROP CONSTRAINT dream_policy_operations_operation_kind_check;
ALTER TABLE dream_policy_operations ADD CONSTRAINT dream_policy_operations_operation_kind_check
    CHECK (operation_kind IN ('create','update','review','decision','publish','disable','rerun','backfill'));
