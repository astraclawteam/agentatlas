-- +goose Up

ALTER TABLE timeline_nodes
    DROP CONSTRAINT timeline_nodes_source_type_check,
    ADD CONSTRAINT timeline_nodes_source_type_check CHECK (source_type IN (
        'work_brief', 'dream_summary', 'sop_update', 'project_event',
        'project_record', 'external_evidence', 'agent_answer',
        'completed_task', 'risk_event'
    ));

-- +goose Down

ALTER TABLE timeline_nodes
    DROP CONSTRAINT timeline_nodes_source_type_check,
    ADD CONSTRAINT timeline_nodes_source_type_check CHECK (source_type IN (
        'work_brief', 'dream_summary', 'sop_update', 'project_event',
        'external_evidence', 'agent_answer'
    ));
