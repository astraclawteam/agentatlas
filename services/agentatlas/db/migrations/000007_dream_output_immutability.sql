-- +goose Up

-- Dream output and human interpretation are historical records. They may be
-- inserted once, but never rewritten or removed in place.
-- +goose StatementBegin
CREATE FUNCTION reject_immutable_dream_output_change() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION '% records are immutable and append-only', TG_TABLE_NAME;
END;
$$;
-- +goose StatementEnd

CREATE TRIGGER dream_summaries_immutable
BEFORE UPDATE OR DELETE ON dream_summaries
FOR EACH ROW EXECUTE FUNCTION reject_immutable_dream_output_change();

CREATE TRIGGER dream_evidence_pointers_immutable
BEFORE UPDATE OR DELETE ON dream_evidence_pointers
FOR EACH ROW EXECUTE FUNCTION reject_immutable_dream_output_change();

CREATE TRIGGER dream_run_annotations_immutable
BEFORE UPDATE OR DELETE ON dream_run_annotations
FOR EACH ROW EXECUTE FUNCTION reject_immutable_dream_output_change();

-- +goose Down

DROP TRIGGER dream_run_annotations_immutable ON dream_run_annotations;
DROP TRIGGER dream_evidence_pointers_immutable ON dream_evidence_pointers;
DROP TRIGGER dream_summaries_immutable ON dream_summaries;
DROP FUNCTION reject_immutable_dream_output_change();
