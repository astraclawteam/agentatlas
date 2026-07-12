-- +goose Up

ALTER TABLE timeline_nodes
    DROP CONSTRAINT timeline_nodes_source_type_check,
    ADD CONSTRAINT timeline_nodes_source_type_check CHECK (source_type IN (
        'work_brief', 'dream_summary', 'sop_update', 'project_event',
        'project_record', 'external_evidence', 'agent_answer',
        'completed_task', 'risk_event'
    ));

-- +goose Down

-- Rollback intentionally fails closed while rows use source types that the
-- previous constraint cannot represent; operators must migrate those rows
-- explicitly before retrying the down migration.
-- +goose StatementBegin
DO $$
BEGIN
    IF EXISTS (
        SELECT 1 FROM timeline_nodes
        WHERE source_type IN ('project_record', 'completed_task', 'risk_event')
    ) THEN
        RAISE EXCEPTION 'cannot roll back migration 000003 while extended Dream timeline source rows exist';
    END IF;
END $$;
-- +goose StatementEnd

ALTER TABLE timeline_nodes
    DROP CONSTRAINT timeline_nodes_source_type_check,
    ADD CONSTRAINT timeline_nodes_source_type_check CHECK (source_type IN (
        'work_brief', 'dream_summary', 'sop_update', 'project_event',
        'external_evidence', 'agent_answer'
    ));
