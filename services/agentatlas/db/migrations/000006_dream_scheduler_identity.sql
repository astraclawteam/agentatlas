-- +goose Up

ALTER TABLE dream_runs
    ADD COLUMN org_version bigint NOT NULL DEFAULT 1 CHECK (org_version > 0),
    ADD COLUMN operation_kind text NOT NULL DEFAULT 'scheduled'
        CHECK (operation_kind IN ('scheduled','automatic_retry','manual_rerun','backfill'));

UPDATE dream_runs
SET operation_kind = 'manual_rerun'
WHERE rerun_of_run_id IS NOT NULL;

UPDATE dream_runs AS runs
SET org_version = GREATEST(COALESCE((
    SELECT max(spaces.org_version)
    FROM knowledge_spaces AS spaces
    WHERE spaces.enterprise_id = runs.enterprise_id
), 1), 1);

-- +goose StatementBegin
CREATE FUNCTION protect_dream_scheduler_identity() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.org_version IS DISTINCT FROM OLD.org_version
       OR NEW.operation_kind IS DISTINCT FROM OLD.operation_kind THEN
        RAISE EXCEPTION 'Dream scheduler snapshot identity is immutable';
    END IF;
    RETURN NEW;
END;
$$;
-- +goose StatementEnd

CREATE TRIGGER dream_scheduler_identity_immutable
BEFORE UPDATE ON dream_runs
FOR EACH ROW EXECUTE FUNCTION protect_dream_scheduler_identity();

-- +goose Down

DROP TRIGGER dream_scheduler_identity_immutable ON dream_runs;
DROP FUNCTION protect_dream_scheduler_identity();
ALTER TABLE dream_runs
    DROP COLUMN operation_kind,
    DROP COLUMN org_version;
