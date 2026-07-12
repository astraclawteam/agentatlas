-- +goose Up

-- Dream runs are historical execution records. Lifecycle fields may advance,
-- but identity, policy/workflow versions, windows, and sanitized snapshots are
-- fixed at insertion time so reruns create new rows instead of rewriting history.
ALTER TABLE dream_policies
    ADD CONSTRAINT dream_policies_enterprise_id_id_uniq UNIQUE (enterprise_id, id);

ALTER TABLE workflows
    ADD CONSTRAINT workflows_enterprise_id_id_uniq UNIQUE (enterprise_id, id);

ALTER TABLE knowledge_spaces
    ADD CONSTRAINT knowledge_spaces_enterprise_id_id_uniq UNIQUE (enterprise_id, id);

ALTER TABLE org_scope_bindings
    ADD CONSTRAINT org_scope_bindings_enterprise_space_fk
    FOREIGN KEY (enterprise_id, space_id) REFERENCES knowledge_spaces (enterprise_id, id);

ALTER TABLE dream_runs
    ADD COLUMN org_unit_id text NOT NULL DEFAULT '',
    ADD COLUMN policy_version integer NOT NULL DEFAULT 1 CHECK (policy_version > 0),
    ADD COLUMN workflow_id text,
    ADD COLUMN workflow_version integer CHECK (workflow_version > 0),
    ADD COLUMN timezone text NOT NULL DEFAULT 'UTC',
    ADD COLUMN input_snapshot jsonb NOT NULL DEFAULT '{"source_counts":[],"sanitized_input_ids":[]}',
    ADD COLUMN visibility_snapshot jsonb NOT NULL DEFAULT '{"visibility_level":"members","org_unit_ids":[],"masked_field_count":0}',
    ADD COLUMN model_route text NOT NULL DEFAULT '',
    ADD COLUMN model_version text NOT NULL DEFAULT '',
    ADD COLUMN attempt integer NOT NULL DEFAULT 1 CHECK (attempt BETWEEN 1 AND 20),
    ADD COLUMN rerun_of_run_id text,
    ADD COLUMN coverage jsonb NOT NULL DEFAULT '{"expected_children":0,"completed_children":0,"input_count":0}',
    ADD COLUMN missing_inputs jsonb NOT NULL DEFAULT '[]',
    ADD COLUMN idempotency_key text NOT NULL DEFAULT '',
    ADD CONSTRAINT dream_runs_enterprise_id_id_uniq UNIQUE (enterprise_id, id),
    ADD CONSTRAINT dream_runs_enterprise_policy_fk
        FOREIGN KEY (enterprise_id, policy_id) REFERENCES dream_policies (enterprise_id, id),
    ADD CONSTRAINT dream_runs_enterprise_workflow_fk
        FOREIGN KEY (enterprise_id, workflow_id) REFERENCES workflows (enterprise_id, id),
    ADD CONSTRAINT dream_runs_workflow_version_fk
        FOREIGN KEY (workflow_id, workflow_version) REFERENCES workflow_versions (workflow_id, version),
    ADD CONSTRAINT dream_runs_rerun_enterprise_fk
        FOREIGN KEY (enterprise_id, rerun_of_run_id) REFERENCES dream_runs (enterprise_id, id),
    ADD CONSTRAINT dream_runs_workflow_pair_check
        CHECK ((workflow_id IS NULL) = (workflow_version IS NULL));

UPDATE dream_runs AS runs
SET policy_version = runs.version,
    org_unit_id = policies.org_scope
FROM dream_policies AS policies
WHERE policies.id = runs.policy_id;

ALTER TABLE dream_runs
    ADD CONSTRAINT dream_runs_policy_version_fk
    FOREIGN KEY (policy_id, policy_version) REFERENCES dream_policy_versions (policy_id, version)
    NOT VALID;

ALTER TABLE dream_runs DROP CONSTRAINT dream_runs_status_check;
ALTER TABLE dream_runs
    ADD CONSTRAINT dream_runs_status_check
    CHECK (status IN ('pending','running','waiting_confirmation','succeeded','failed'));

CREATE UNIQUE INDEX dream_runs_idempotency_uniq
    ON dream_runs (enterprise_id, idempotency_key)
    WHERE idempotency_key <> '';

ALTER TABLE dream_summaries
    ADD COLUMN facts jsonb NOT NULL DEFAULT '[]',
    ADD COLUMN themes jsonb NOT NULL DEFAULT '[]',
    ADD COLUMN trends jsonb NOT NULL DEFAULT '[]',
    ADD COLUMN todos jsonb NOT NULL DEFAULT '[]';

ALTER TABLE dream_summaries
    ADD CONSTRAINT dream_summaries_enterprise_run_fk
        FOREIGN KEY (enterprise_id, run_id) REFERENCES dream_runs (enterprise_id, id),
    ADD CONSTRAINT dream_summaries_enterprise_space_fk
        FOREIGN KEY (enterprise_id, space_id) REFERENCES knowledge_spaces (enterprise_id, id);

-- +goose StatementBegin
CREATE FUNCTION reject_immutable_dream_policy_version_update() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'Published Dream policy versions are immutable';
END;
$$;
-- +goose StatementEnd

CREATE TRIGGER dream_policy_versions_immutable
BEFORE UPDATE OR DELETE ON dream_policy_versions
FOR EACH ROW EXECUTE FUNCTION reject_immutable_dream_policy_version_update();

-- +goose StatementBegin
CREATE FUNCTION reject_immutable_workflow_version_update() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'Published workflow versions are immutable';
END;
$$;
-- +goose StatementEnd

CREATE TRIGGER workflow_versions_immutable
BEFORE UPDATE OR DELETE ON workflow_versions
FOR EACH ROW EXECUTE FUNCTION reject_immutable_workflow_version_update();

-- +goose StatementBegin
CREATE FUNCTION reject_immutable_dream_run_update() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.id IS DISTINCT FROM OLD.id
       OR NEW.policy_id IS DISTINCT FROM OLD.policy_id
       OR NEW.version IS DISTINCT FROM OLD.version
       OR NEW.enterprise_id IS DISTINCT FROM OLD.enterprise_id
       OR NEW.window_start IS DISTINCT FROM OLD.window_start
       OR NEW.window_end IS DISTINCT FROM OLD.window_end
       OR NEW.created_at IS DISTINCT FROM OLD.created_at
       OR NEW.org_unit_id IS DISTINCT FROM OLD.org_unit_id
       OR NEW.policy_version IS DISTINCT FROM OLD.policy_version
       OR NEW.workflow_id IS DISTINCT FROM OLD.workflow_id
       OR NEW.workflow_version IS DISTINCT FROM OLD.workflow_version
       OR NEW.timezone IS DISTINCT FROM OLD.timezone
       OR NEW.input_snapshot IS DISTINCT FROM OLD.input_snapshot
       OR NEW.visibility_snapshot IS DISTINCT FROM OLD.visibility_snapshot
       OR NEW.model_route IS DISTINCT FROM OLD.model_route
       OR NEW.model_version IS DISTINCT FROM OLD.model_version
       OR NEW.attempt IS DISTINCT FROM OLD.attempt
       OR NEW.rerun_of_run_id IS DISTINCT FROM OLD.rerun_of_run_id
       OR NEW.coverage IS DISTINCT FROM OLD.coverage
       OR NEW.missing_inputs IS DISTINCT FROM OLD.missing_inputs
       OR NEW.idempotency_key IS DISTINCT FROM OLD.idempotency_key THEN
        RAISE EXCEPTION 'Dream run identity, snapshots, versions, and windows are immutable';
    END IF;
    RETURN NEW;
END;
$$;
-- +goose StatementEnd

CREATE TRIGGER dream_runs_immutable_fields
BEFORE UPDATE ON dream_runs
FOR EACH ROW EXECUTE FUNCTION reject_immutable_dream_run_update();

CREATE TABLE dream_run_lineage (
    run_id        text NOT NULL REFERENCES dream_runs(id),
    parent_run_id text NOT NULL REFERENCES dream_runs(id),
    relation      text NOT NULL CHECK (relation = 'child_summary'),
    created_at    timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (run_id, parent_run_id, relation),
    CHECK (run_id <> parent_run_id)
);

-- +goose StatementBegin
CREATE FUNCTION validate_dream_run_lineage() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
    run_enterprise text;
    child_enterprise text;
BEGIN
    SELECT enterprise_id INTO run_enterprise FROM dream_runs WHERE id = NEW.run_id;
    SELECT enterprise_id INTO child_enterprise FROM dream_runs WHERE id = NEW.parent_run_id;
    IF run_enterprise IS NULL OR child_enterprise IS NULL OR run_enterprise <> child_enterprise THEN
        RAISE EXCEPTION 'Dream lineage must remain within one enterprise';
    END IF;
    RETURN NEW;
END;
$$;
-- +goose StatementEnd

CREATE TRIGGER dream_run_lineage_enterprise_guard
BEFORE INSERT OR UPDATE ON dream_run_lineage
FOR EACH ROW EXECUTE FUNCTION validate_dream_run_lineage();

-- +goose StatementBegin
CREATE FUNCTION reject_immutable_dream_run_lineage_change() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'Dream run lineage is immutable';
END;
$$;
-- +goose StatementEnd

CREATE TRIGGER dream_run_lineage_immutable
BEFORE UPDATE OR DELETE ON dream_run_lineage
FOR EACH ROW EXECUTE FUNCTION reject_immutable_dream_run_lineage_change();

CREATE TABLE dream_run_annotations (
    id              text PRIMARY KEY,
    enterprise_id   text NOT NULL REFERENCES enterprises(id),
    run_id          text NOT NULL,
    annotation_type text NOT NULL,
    body            text NOT NULL CHECK (char_length(body) <= 4000),
    created_by      text NOT NULL,
    created_at      timestamptz NOT NULL DEFAULT now(),
    UNIQUE (enterprise_id, id),
    FOREIGN KEY (enterprise_id, run_id) REFERENCES dream_runs (enterprise_id, id)
);

CREATE TABLE change_drafts (
    id                text PRIMARY KEY,
    enterprise_id     text NOT NULL REFERENCES enterprises(id),
    org_unit_id       text NOT NULL,
    resource_type     text NOT NULL CHECK (resource_type IN ('knowledge_entry','sop','workflow','dream_policy','method_outline')),
    resource_id       text NOT NULL,
    action            text NOT NULL CHECK (action IN ('create','update','publish','disable','delete')),
    requester_user_id text NOT NULL,
    origin            text NOT NULL CHECK (origin IN ('direct_edit','employee_suggestion')),
    permission_mode   text NOT NULL CHECK (permission_mode IN ('direct_edit','suggestion_only')),
    revision          integer NOT NULL DEFAULT 1 CHECK (revision > 0),
    state             text NOT NULL CHECK (state IN ('draft','submitted','approved','rejected','published','withdrawn')),
    base_version      integer NOT NULL DEFAULT 0 CHECK (base_version >= 0),
    proposed_content  jsonb NOT NULL,
    created_at        timestamptz NOT NULL DEFAULT now(),
    updated_at        timestamptz NOT NULL DEFAULT now(),
    UNIQUE (enterprise_id, id),
    CHECK (
        (origin = 'direct_edit' AND permission_mode = 'direct_edit')
        OR
        (origin = 'employee_suggestion' AND permission_mode = 'suggestion_only' AND action <> 'publish')
    )
);

CREATE TABLE change_versions (
    id            text PRIMARY KEY,
    enterprise_id text NOT NULL REFERENCES enterprises(id),
    change_id     text NOT NULL,
    version       integer NOT NULL CHECK (version > 0),
    content       jsonb NOT NULL,
    published_by  text NOT NULL,
    published_at  timestamptz NOT NULL DEFAULT now(),
    UNIQUE (enterprise_id, change_id, version),
    FOREIGN KEY (enterprise_id, change_id) REFERENCES change_drafts (enterprise_id, id)
);

-- +goose StatementBegin
CREATE FUNCTION reject_immutable_change_version_update() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'Published change versions are immutable';
END;
$$;
-- +goose StatementEnd

CREATE TRIGGER change_versions_immutable
BEFORE UPDATE OR DELETE ON change_versions
FOR EACH ROW EXECUTE FUNCTION reject_immutable_change_version_update();

CREATE TABLE change_reviews (
    id                 text PRIMARY KEY,
    enterprise_id      text NOT NULL REFERENCES enterprises(id),
    change_id          text NOT NULL,
    change_revision    integer NOT NULL CHECK (change_revision > 0),
    reviewer_user_id   text,
    risk_level         text NOT NULL CHECK (risk_level IN ('low','high')),
    risk_reasons       jsonb NOT NULL DEFAULT '[]',
    review_mode        text NOT NULL CHECK (review_mode IN ('single_confirmation','upward_review','enterprise_knowledge_admin_queue')),
    state              text NOT NULL CHECK (state IN ('pending','approved','rejected','cancelled')),
    org_path           jsonb NOT NULL DEFAULT '[]',
    queue              text,
    decision           text NOT NULL DEFAULT '',
    comment            text NOT NULL DEFAULT '' CHECK (char_length(comment) <= 4000),
    created_at         timestamptz NOT NULL DEFAULT now(),
    UNIQUE (enterprise_id, change_id, change_revision, reviewer_user_id),
    FOREIGN KEY (enterprise_id, change_id) REFERENCES change_drafts (enterprise_id, id),
    CHECK (
        (review_mode = 'single_confirmation' AND risk_level = 'low'
            AND reviewer_user_id IS NULL AND queue IS NULL)
        OR
        (review_mode = 'upward_review' AND risk_level = 'high'
            AND reviewer_user_id IS NOT NULL AND queue IS NULL
            AND jsonb_array_length(org_path) > 0)
        OR
        (review_mode = 'enterprise_knowledge_admin_queue' AND risk_level = 'high'
            AND reviewer_user_id IS NULL AND queue IS NOT NULL AND queue <> '')
    )
);

-- +goose StatementBegin
CREATE FUNCTION validate_change_review_revision() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
    current_revision integer;
    requester text;
BEGIN
    SELECT revision, requester_user_id
      INTO current_revision, requester
      FROM change_drafts
     WHERE enterprise_id = NEW.enterprise_id AND id = NEW.change_id;
    IF current_revision IS NULL OR NEW.change_revision <> current_revision THEN
        RAISE EXCEPTION 'Change review revision is stale or outside the enterprise';
    END IF;
    IF NEW.review_mode = 'upward_review' AND NEW.reviewer_user_id = requester THEN
        RAISE EXCEPTION 'Requester cannot approve their own upward review';
    END IF;
    RETURN NEW;
END;
$$;
-- +goose StatementEnd

CREATE TRIGGER change_reviews_revision_guard
BEFORE INSERT OR UPDATE ON change_reviews
FOR EACH ROW EXECUTE FUNCTION validate_change_review_revision();

CREATE TABLE publish_operations (
    id               text PRIMARY KEY,
    enterprise_id    text NOT NULL REFERENCES enterprises(id),
    change_id        text NOT NULL,
    change_revision  integer NOT NULL CHECK (change_revision > 0),
    idempotency_key  text NOT NULL CHECK (idempotency_key <> ''),
    status           text NOT NULL CHECK (status IN ('pending','succeeded','failed')),
    result            jsonb,
    created_at        timestamptz NOT NULL DEFAULT now(),
    finished_at       timestamptz,
    UNIQUE (enterprise_id, idempotency_key),
    FOREIGN KEY (enterprise_id, change_id) REFERENCES change_drafts (enterprise_id, id)
);

-- +goose StatementBegin
CREATE FUNCTION validate_publish_operation_revision() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
    current_revision integer;
BEGIN
    SELECT revision
      INTO current_revision
      FROM change_drafts
     WHERE enterprise_id = NEW.enterprise_id AND id = NEW.change_id;
    IF current_revision IS NULL OR NEW.change_revision <> current_revision THEN
        RAISE EXCEPTION 'Publish operation revision is stale or outside the enterprise';
    END IF;
    RETURN NEW;
END;
$$;
-- +goose StatementEnd

CREATE TRIGGER publish_operations_revision_guard
AFTER INSERT ON publish_operations
FOR EACH ROW EXECUTE FUNCTION validate_publish_operation_revision();

-- +goose StatementBegin
CREATE FUNCTION protect_publish_operation_identity_update() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.id IS DISTINCT FROM OLD.id
       OR NEW.enterprise_id IS DISTINCT FROM OLD.enterprise_id
       OR NEW.change_id IS DISTINCT FROM OLD.change_id
       OR NEW.change_revision IS DISTINCT FROM OLD.change_revision
       OR NEW.idempotency_key IS DISTINCT FROM OLD.idempotency_key
       OR NEW.created_at IS DISTINCT FROM OLD.created_at THEN
        RAISE EXCEPTION 'Publish operation identity is immutable';
    END IF;
    RETURN NEW;
END;
$$;
-- +goose StatementEnd

CREATE TRIGGER publish_operations_identity_guard
BEFORE UPDATE ON publish_operations
FOR EACH ROW EXECUTE FUNCTION protect_publish_operation_identity_update();

-- +goose Down

DROP TRIGGER publish_operations_identity_guard ON publish_operations;
DROP FUNCTION protect_publish_operation_identity_update();
DROP TRIGGER publish_operations_revision_guard ON publish_operations;
DROP FUNCTION validate_publish_operation_revision();
DROP TABLE publish_operations;
DROP TRIGGER change_reviews_revision_guard ON change_reviews;
DROP FUNCTION validate_change_review_revision();
DROP TABLE change_reviews;
DROP TRIGGER change_versions_immutable ON change_versions;
DROP FUNCTION reject_immutable_change_version_update();
DROP TABLE change_versions;
DROP TABLE change_drafts;
DROP TABLE dream_run_annotations;
DROP TRIGGER dream_run_lineage_immutable ON dream_run_lineage;
DROP FUNCTION reject_immutable_dream_run_lineage_change();
DROP TRIGGER dream_run_lineage_enterprise_guard ON dream_run_lineage;
DROP FUNCTION validate_dream_run_lineage();
DROP TABLE dream_run_lineage;
DROP TRIGGER dream_runs_immutable_fields ON dream_runs;
DROP FUNCTION reject_immutable_dream_run_update();
DROP TRIGGER dream_policy_versions_immutable ON dream_policy_versions;
DROP FUNCTION reject_immutable_dream_policy_version_update();
DROP TRIGGER workflow_versions_immutable ON workflow_versions;
DROP FUNCTION reject_immutable_workflow_version_update();

ALTER TABLE dream_summaries
    DROP CONSTRAINT dream_summaries_enterprise_space_fk,
    DROP CONSTRAINT dream_summaries_enterprise_run_fk;
ALTER TABLE dream_summaries
    DROP COLUMN todos,
    DROP COLUMN trends,
    DROP COLUMN themes,
    DROP COLUMN facts;

DROP INDEX dream_runs_idempotency_uniq;
ALTER TABLE dream_runs DROP CONSTRAINT dream_runs_status_check;
ALTER TABLE dream_runs
    ADD CONSTRAINT dream_runs_status_check
    CHECK (status IN ('pending','running','succeeded','failed'));
ALTER TABLE dream_runs
    DROP CONSTRAINT dream_runs_workflow_pair_check,
    DROP CONSTRAINT dream_runs_rerun_enterprise_fk,
    DROP CONSTRAINT dream_runs_workflow_version_fk,
    DROP CONSTRAINT dream_runs_enterprise_workflow_fk,
    DROP CONSTRAINT dream_runs_policy_version_fk,
    DROP CONSTRAINT dream_runs_enterprise_policy_fk,
    DROP CONSTRAINT dream_runs_enterprise_id_id_uniq,
    DROP COLUMN idempotency_key,
    DROP COLUMN missing_inputs,
    DROP COLUMN coverage,
    DROP COLUMN rerun_of_run_id,
    DROP COLUMN attempt,
    DROP COLUMN model_version,
    DROP COLUMN model_route,
    DROP COLUMN visibility_snapshot,
    DROP COLUMN input_snapshot,
    DROP COLUMN timezone,
    DROP COLUMN workflow_version,
    DROP COLUMN workflow_id,
    DROP COLUMN policy_version,
    DROP COLUMN org_unit_id;

ALTER TABLE workflows DROP CONSTRAINT workflows_enterprise_id_id_uniq;
ALTER TABLE dream_policies DROP CONSTRAINT dream_policies_enterprise_id_id_uniq;
ALTER TABLE org_scope_bindings DROP CONSTRAINT org_scope_bindings_enterprise_space_fk;
ALTER TABLE knowledge_spaces DROP CONSTRAINT knowledge_spaces_enterprise_id_id_uniq;
