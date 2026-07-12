-- name: CreateWorkflowDraft :one
INSERT INTO workflows (id, enterprise_id, name, kind, created_by, draft)
VALUES ($1, $2, $3, $4, $5, $6)
RETURNING *;

-- name: UpdateWorkflowDraft :execrows
UPDATE workflows SET draft = $3, draft_updated_at = now()
WHERE id = $1 AND enterprise_id = $2;

-- name: GetWorkflow :one
SELECT * FROM workflows WHERE id = $1;

-- name: ListWorkflowsByEnterprise :many
SELECT * FROM workflows WHERE enterprise_id = $1 ORDER BY draft_updated_at DESC, id LIMIT $2;

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
      AND dream.execution_owner = sqlc.arg(dream_execution_owner)
      AND dream.execution_lease_expires_at > now()
    FOR UPDATE
), persisted_inputs AS (
    INSERT INTO dream_inputs(run_id,source_type,source_id)
    SELECT target.id, item->>'source_type', item->>'source_id'
    FROM target, jsonb_array_elements(sqlc.arg(input)::jsonb->'inputs') AS item
    ON CONFLICT (run_id,source_type,source_id) DO NOTHING
    RETURNING run_id
), created AS (
    INSERT INTO workflow_runs (id, workflow_id, version, enterprise_id, status, input, output, execution_owner, execution_lease_expires_at)
    SELECT sqlc.arg(id), sqlc.arg(workflow_id), sqlc.arg(version), sqlc.arg(enterprise_id),
           sqlc.arg(status), sqlc.arg(input)::jsonb, sqlc.arg(output)::jsonb, sqlc.arg(execution_owner), now()+interval '2 minutes'
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

-- name: TransitionWorkflowRunWithEvent :one
WITH changed AS (
    UPDATE workflow_runs AS run
    SET status = sqlc.arg(status), output = sqlc.arg(run_output),
        finished_at = CASE WHEN sqlc.arg(status) IN ('succeeded','failed','cancelled') THEN now() ELSE run.finished_at END,
        state_revision=run.state_revision+1,
        execution_owner=CASE WHEN sqlc.arg(status)='running' THEN run.execution_owner ELSE NULL END,
        execution_lease_expires_at=CASE WHEN sqlc.arg(status)='running' THEN now()+interval '2 minutes' ELSE NULL END
    WHERE run.id = sqlc.arg(id) AND run.status=sqlc.arg(expected_status)
      AND run.state_revision=sqlc.arg(expected_revision)
      AND run.execution_owner IS NOT DISTINCT FROM sqlc.narg(expected_owner)
      AND (run.execution_owner IS NULL OR run.execution_lease_expires_at > now())
    RETURNING run.id, run.state_revision
), audit AS (
    INSERT INTO workflow_run_events (run_id, node_id, status, detail)
    SELECT changed.id, sqlc.arg(node_id), sqlc.arg(event_status), sqlc.arg(event_detail)
    FROM changed
    RETURNING run_id
)
SELECT changed.state_revision FROM changed JOIN audit ON audit.run_id = changed.id;

-- name: TransitionDreamWorkflowRun :one
WITH changed AS (
    UPDATE workflow_runs AS run
    SET status = sqlc.arg(status), output = sqlc.arg(run_output),
        finished_at = CASE WHEN sqlc.arg(status) IN ('succeeded','failed','cancelled') THEN now() ELSE run.finished_at END,
        state_revision=run.state_revision+1,
        execution_owner=CASE WHEN sqlc.arg(status)='running' THEN run.execution_owner ELSE NULL END,
        execution_lease_expires_at=CASE WHEN sqlc.arg(status)='running' THEN now()+interval '2 minutes' ELSE NULL END
    WHERE run.id = sqlc.arg(id) AND run.enterprise_id = sqlc.arg(enterprise_id)
      AND run.status=sqlc.arg(expected_status) AND run.state_revision=sqlc.arg(expected_revision)
      AND run.execution_owner IS NOT DISTINCT FROM sqlc.narg(expected_owner)
      AND (run.execution_owner IS NULL OR run.execution_lease_expires_at > now())
    RETURNING run.id, run.enterprise_id, run.status, run.state_revision
), audit AS (
    INSERT INTO workflow_run_events (run_id, node_id, status, detail)
    SELECT changed.id, sqlc.arg(node_id), sqlc.arg(event_status), sqlc.arg(event_detail)
    FROM changed
    RETURNING run_id
), lifecycle AS (
    INSERT INTO dream_workflow_lifecycle_outbox (
        enterprise_id, dream_run_id, workflow_run_id, status, lifecycle_error
    )
    SELECT changed.enterprise_id, dream.id, changed.id, changed.status, sqlc.arg(lifecycle_error)
    FROM changed
    JOIN audit ON audit.run_id = changed.id
    JOIN dream_runs AS dream
      ON dream.enterprise_id = changed.enterprise_id AND dream.workflow_run_id = changed.id
    WHERE changed.status IN ('waiting_confirmation','succeeded','failed','cancelled')
    ON CONFLICT (workflow_run_id, status) DO UPDATE
    SET lifecycle_error = EXCLUDED.lifecycle_error, updated_at = now()
    RETURNING workflow_run_id
)
SELECT changed.state_revision FROM changed JOIN audit ON audit.run_id = changed.id;

-- name: ClaimWorkflowRunLease :one
UPDATE workflow_runs
SET status='running', execution_owner=sqlc.arg(execution_owner),
    execution_lease_expires_at=now()+interval '2 minutes', state_revision=state_revision+1
WHERE id=sqlc.arg(id) AND status='pending' AND execution_owner IS NULL
RETURNING *;

-- name: RenewWorkflowRunLease :execrows
UPDATE workflow_runs SET execution_lease_expires_at=now()+interval '2 minutes'
WHERE id=sqlc.arg(id) AND status='running' AND execution_owner=sqlc.arg(execution_owner)
  AND execution_lease_expires_at > now();

-- name: RecoverExpiredWorkflowRuns :many
WITH changed AS (
    UPDATE workflow_runs
    SET status='pending', execution_owner=NULL, execution_lease_expires_at=NULL,
        state_revision=state_revision+1
    WHERE id IN (
        SELECT id FROM workflow_runs
        WHERE status='running' AND (execution_lease_expires_at IS NULL OR execution_lease_expires_at <= now())
        ORDER BY started_at LIMIT sqlc.arg(result_limit) FOR UPDATE SKIP LOCKED
    )
    RETURNING id, state_revision
), audit AS (
    INSERT INTO workflow_run_events(run_id,node_id,status,detail)
    SELECT id,'','recovered',jsonb_build_object('state_revision',state_revision) FROM changed
    RETURNING run_id
)
SELECT changed.id FROM changed JOIN audit ON audit.run_id=changed.id;

-- name: ListPendingWorkflowRuns :many
SELECT id FROM workflow_runs WHERE status='pending' ORDER BY started_at LIMIT sqlc.arg(result_limit);

-- name: GetWorkflowRun :one
SELECT * FROM workflow_runs WHERE id = $1;

-- name: InsertWorkflowRunEvent :one
INSERT INTO workflow_run_events (run_id, node_id, status, detail)
VALUES ($1, $2, $3, $4)
RETURNING *;

-- name: ListWorkflowRunEvents :many
SELECT * FROM workflow_run_events WHERE run_id = $1 ORDER BY id;
