-- +goose Up

-- Existing installations may contain duplicates that were legal before this
-- migration. Fail with a named error instead of choosing lineage arbitrarily.
-- +goose StatementBegin
DO $$ BEGIN
  IF EXISTS (SELECT 1 FROM dream_inputs GROUP BY run_id,source_type,source_id HAVING count(*)>1) THEN
    RAISE EXCEPTION '000005 duplicate dream_inputs require deterministic operator reconciliation';
  END IF;
  IF EXISTS (SELECT 1 FROM dream_summaries GROUP BY enterprise_id,run_id,layer HAVING count(*)>1) THEN
    RAISE EXCEPTION '000005 duplicate dream_summaries require deterministic operator reconciliation';
  END IF;
END $$;
-- +goose StatementEnd

ALTER TABLE workflow_runs ADD CONSTRAINT workflow_runs_enterprise_id_id_uniq UNIQUE (enterprise_id, id);
ALTER TABLE workflow_runs ADD CONSTRAINT workflow_runs_dream_pin_uniq UNIQUE (enterprise_id, id, workflow_id, version);
ALTER TABLE dream_runs ADD COLUMN workflow_run_id text;
ALTER TABLE dream_runs ADD COLUMN output_hash text;
ALTER TABLE dream_runs ADD CONSTRAINT dream_runs_workflow_run_uniq UNIQUE (workflow_run_id);
ALTER TABLE dream_runs ADD CONSTRAINT dream_runs_workflow_run_enterprise_fk
    FOREIGN KEY (enterprise_id, workflow_run_id) REFERENCES workflow_runs (enterprise_id, id);
ALTER TABLE dream_runs ADD CONSTRAINT dream_runs_workflow_run_pin_fk
    FOREIGN KEY (enterprise_id, workflow_run_id, workflow_id, workflow_version)
    REFERENCES workflow_runs (enterprise_id, id, workflow_id, version);
ALTER TABLE dream_summaries ADD CONSTRAINT dream_summaries_run_layer_uniq UNIQUE (enterprise_id, run_id, layer);
ALTER TABLE dream_inputs ADD CONSTRAINT dream_inputs_run_source_uniq UNIQUE (run_id, source_type, source_id);

-- +goose StatementBegin
CREATE FUNCTION protect_dream_workflow_run_binding() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF (OLD.workflow_run_id IS NOT NULL AND NEW.workflow_run_id IS DISTINCT FROM OLD.workflow_run_id)
       OR (OLD.output_hash IS NOT NULL AND NEW.output_hash IS DISTINCT FROM OLD.output_hash) THEN
        RAISE EXCEPTION 'Dream workflow run binding and output hash are immutable';
    END IF;
    RETURN NEW;
END;
$$;
-- +goose StatementEnd
CREATE TRIGGER dream_workflow_run_binding_immutable BEFORE UPDATE ON dream_runs
FOR EACH ROW EXECUTE FUNCTION protect_dream_workflow_run_binding();

-- +goose Down

DROP TRIGGER dream_workflow_run_binding_immutable ON dream_runs;
DROP FUNCTION protect_dream_workflow_run_binding();
ALTER TABLE dream_inputs DROP CONSTRAINT dream_inputs_run_source_uniq;
ALTER TABLE dream_summaries DROP CONSTRAINT dream_summaries_run_layer_uniq;
ALTER TABLE dream_runs DROP CONSTRAINT dream_runs_workflow_run_pin_fk;
ALTER TABLE dream_runs DROP CONSTRAINT dream_runs_workflow_run_enterprise_fk;
ALTER TABLE dream_runs DROP CONSTRAINT dream_runs_workflow_run_uniq;
ALTER TABLE dream_runs DROP COLUMN workflow_run_id;
ALTER TABLE dream_runs DROP COLUMN output_hash;
ALTER TABLE workflow_runs DROP CONSTRAINT workflow_runs_dream_pin_uniq;
ALTER TABLE workflow_runs DROP CONSTRAINT workflow_runs_enterprise_id_id_uniq;
