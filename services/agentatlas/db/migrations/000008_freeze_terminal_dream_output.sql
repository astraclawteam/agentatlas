-- +goose Up

-- Output membership is open only during the runner-owned production phase.
-- Locking the run row serializes these inserts against the terminal status
-- transition, so no transaction can append a layer or evidence link after
-- completion wins the lock.
-- +goose StatementBegin
CREATE FUNCTION require_running_dream_for_summary_insert() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
    run_status text;
BEGIN
    SELECT status INTO run_status
    FROM dream_runs
    WHERE id = NEW.run_id
    FOR UPDATE;
    IF run_status IS DISTINCT FROM 'running' THEN
        RAISE EXCEPTION 'Dream summary layers may only be inserted while the run is running';
    END IF;
    RETURN NEW;
END;
$$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE FUNCTION require_running_dream_for_evidence_insert() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
    run_status text;
BEGIN
    SELECT runs.status INTO run_status
    FROM dream_summaries AS summaries
    JOIN dream_runs AS runs
      ON runs.id = summaries.run_id
     AND runs.enterprise_id = summaries.enterprise_id
    WHERE summaries.id = NEW.dream_summary_id
    FOR UPDATE OF runs;
    IF run_status IS DISTINCT FROM 'running' THEN
        RAISE EXCEPTION 'Dream evidence lineage may only be inserted while the run is running';
    END IF;
    RETURN NEW;
END;
$$;
-- +goose StatementEnd

CREATE TRIGGER dream_summaries_insert_running_guard
BEFORE INSERT ON dream_summaries
FOR EACH ROW EXECUTE FUNCTION require_running_dream_for_summary_insert();

CREATE TRIGGER dream_evidence_pointers_insert_running_guard
BEFORE INSERT ON dream_evidence_pointers
FOR EACH ROW EXECUTE FUNCTION require_running_dream_for_evidence_insert();

-- +goose Down

DROP TRIGGER dream_evidence_pointers_insert_running_guard ON dream_evidence_pointers;
DROP TRIGGER dream_summaries_insert_running_guard ON dream_summaries;
DROP FUNCTION require_running_dream_for_evidence_insert();
DROP FUNCTION require_running_dream_for_summary_insert();
