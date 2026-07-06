-- name: CreateWorkflowDraft :one
INSERT INTO workflows (id, enterprise_id, name, kind, created_by, draft)
VALUES ($1, $2, $3, $4, $5, $6)
RETURNING *;

-- name: UpdateWorkflowDraft :execrows
UPDATE workflows SET draft = $3, draft_updated_at = now()
WHERE id = $1 AND enterprise_id = $2;

-- name: GetWorkflow :one
SELECT * FROM workflows WHERE id = $1;

-- name: PublishWorkflowVersion :one
INSERT INTO workflow_versions (workflow_id, version, definition, risk_level, published_by)
VALUES ($1, $2, $3, $4, $5)
RETURNING *;

-- name: GetWorkflowVersion :one
SELECT * FROM workflow_versions WHERE workflow_id = $1 AND version = $2;

-- name: GetLatestWorkflowVersion :one
SELECT * FROM workflow_versions WHERE workflow_id = $1 ORDER BY version DESC LIMIT 1;

-- name: InsertWorkflowNode :exec
INSERT INTO workflow_nodes (workflow_id, version, node_id, node_type, name, config, requires_confirmation)
VALUES ($1, $2, $3, $4, $5, $6, $7);

-- name: InsertWorkflowEdge :exec
INSERT INTO workflow_edges (workflow_id, version, from_node, to_node, condition)
VALUES ($1, $2, $3, $4, $5);

-- name: ListWorkflowNodes :many
SELECT * FROM workflow_nodes WHERE workflow_id = $1 AND version = $2 ORDER BY node_id;

-- name: CreateWorkflowRun :one
INSERT INTO workflow_runs (id, workflow_id, version, enterprise_id, status, input)
VALUES ($1, $2, $3, $4, $5, $6)
RETURNING *;

-- name: UpdateWorkflowRunStatus :execrows
UPDATE workflow_runs
SET status = $2, output = COALESCE($3, output),
    finished_at = CASE WHEN $2 IN ('succeeded','failed','cancelled') THEN now() ELSE finished_at END
WHERE id = $1;

-- name: GetWorkflowRun :one
SELECT * FROM workflow_runs WHERE id = $1;

-- name: InsertWorkflowRunEvent :one
INSERT INTO workflow_run_events (run_id, node_id, status, detail)
VALUES ($1, $2, $3, $4)
RETURNING *;

-- name: ListWorkflowRunEvents :many
SELECT * FROM workflow_run_events WHERE run_id = $1 ORDER BY id;
