-- name: CreateAnswerTrace :one
INSERT INTO answer_traces (id, enterprise_id, case_ticket_id, actor_user_id, question_hash, sanitized_question_summary, workflow_run_id, space_ids, retrieval_plan_id, evidence_pointer_ids, agentnexus_read_grant_ids, model_route, answer_hash)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13)
RETURNING *;

-- name: GetAnswerTrace :one
SELECT * FROM answer_traces WHERE id = $1;

-- name: ListAnswerTracesByTicket :many
SELECT * FROM answer_traces WHERE enterprise_id = $1 AND case_ticket_id = $2 ORDER BY created_at DESC;

-- name: InsertAnswerTraceStep :exec
INSERT INTO answer_trace_steps (trace_id, step_no, kind, detail)
VALUES ($1, $2, $3, $4);

-- name: ListAnswerTraceSteps :many
SELECT * FROM answer_trace_steps WHERE trace_id = $1 ORDER BY step_no;

-- name: InsertAnswerTraceEvidence :exec
INSERT INTO answer_trace_evidence (trace_id, evidence_pointer_id, grant_id)
VALUES ($1, $2, $3)
ON CONFLICT DO NOTHING;

-- name: InsertAnswerTraceModelEvent :exec
INSERT INTO answer_trace_model_events (trace_id, model_route, prompt_hash, usage, latency_ms)
VALUES ($1, $2, $3, $4, $5);

-- name: InsertAnswerTraceAuditRef :exec
INSERT INTO answer_trace_audit_refs (trace_id, audit_ref_id)
VALUES ($1, $2)
ON CONFLICT DO NOTHING;

-- name: ListRecentAnswerTraces :many
SELECT * FROM answer_traces
WHERE enterprise_id = $1
ORDER BY created_at DESC
LIMIT 20;
