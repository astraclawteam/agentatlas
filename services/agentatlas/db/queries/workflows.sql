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

-- name: CreateBoundDreamWorkflowRun :one
WITH target AS (
    SELECT dream.id
    FROM dream_runs AS dream
    WHERE dream.id = sqlc.arg(dream_run_id)
      AND dream.enterprise_id = sqlc.arg(enterprise_id)
      AND dream.policy_id = sqlc.arg(policy_id)
      AND dream.policy_version = sqlc.arg(policy_version)
      AND dream.org_unit_id = sqlc.arg(org_unit_id)
      AND dream.workflow_id = sqlc.arg(workflow_id)
      AND dream.workflow_version = sqlc.arg(version)
      AND dream.status = 'running'
      AND dream.workflow_run_id IS NULL
    FOR UPDATE
), created AS (
    INSERT INTO workflow_runs (id, workflow_id, version, enterprise_id, status, input, output)
    SELECT sqlc.arg(id), sqlc.arg(workflow_id), sqlc.arg(version), sqlc.arg(enterprise_id),
           sqlc.arg(status), sqlc.arg(input), sqlc.arg(output)
    FROM target
    RETURNING *
), bound AS (
    UPDATE dream_runs AS dream
    SET workflow_run_id = created.id
    FROM created, target
    WHERE dream.id = target.id
    RETURNING dream.workflow_run_id
)
SELECT created.* FROM created JOIN bound ON bound.workflow_run_id = created.id;

-- name: UpdateWorkflowRunStatus :execrows
UPDATE workflow_runs
SET status = $2, output = COALESCE($3, output),
    finished_at = CASE WHEN $2 IN ('succeeded','failed','cancelled') THEN now() ELSE finished_at END
WHERE id = $1;

-- name: TransitionDreamWorkflowRun :execrows
WITH changed AS (
    UPDATE workflow_runs AS run
    SET status = sqlc.arg(status), output = sqlc.arg(run_output),
        finished_at = CASE WHEN sqlc.arg(status) IN ('succeeded','failed','cancelled') THEN now() ELSE run.finished_at END
    WHERE run.id = sqlc.arg(id) AND run.enterprise_id = sqlc.arg(enterprise_id)
    RETURNING run.id, run.enterprise_id, run.status
)
INSERT INTO dream_workflow_lifecycle_outbox (
    enterprise_id, dream_run_id, workflow_run_id, status, lifecycle_error
)
SELECT changed.enterprise_id, dream.id, changed.id, changed.status, sqlc.arg(lifecycle_error)
FROM changed
JOIN dream_runs AS dream
  ON dream.enterprise_id = changed.enterprise_id AND dream.workflow_run_id = changed.id
WHERE changed.status IN ('waiting_confirmation','succeeded','failed','cancelled')
ON CONFLICT (workflow_run_id, status) DO UPDATE
SET lifecycle_error = EXCLUDED.lifecycle_error, updated_at = now();

-- name: GetWorkflowRun :one
SELECT * FROM workflow_runs WHERE id = $1;

-- name: InsertWorkflowRunEvent :one
INSERT INTO workflow_run_events (run_id, node_id, status, detail)
VALUES ($1, $2, $3, $4)
RETURNING *;

-- name: ListWorkflowRunEvents :many
SELECT * FROM workflow_run_events WHERE run_id = $1 ORDER BY id;
