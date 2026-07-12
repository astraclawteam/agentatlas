-- name: CreateDreamPolicy :one
INSERT INTO dream_policies (id, enterprise_id, org_scope, status, draft)
VALUES ($1, $2, $3, $4, $5)
RETURNING *;

-- name: GetDreamPolicy :one
SELECT * FROM dream_policies WHERE id = $1;

-- name: UpdateDreamPolicyStatus :execrows
UPDATE dream_policies SET status = $2, updated_at = now() WHERE id = $1;

-- name: PublishDreamPolicyVersion :one
INSERT INTO dream_policy_versions (policy_id, version, definition)
VALUES ($1, $2, $3)
RETURNING *;

-- name: GetLatestDreamPolicyVersion :one
SELECT * FROM dream_policy_versions WHERE policy_id = $1 ORDER BY version DESC LIMIT 1;

-- name: GetDreamPolicyVersion :one
SELECT * FROM dream_policy_versions WHERE policy_id = sqlc.arg(policy_id) AND version = sqlc.arg(version);

-- name: ListPublishedDreamPolicies :many
SELECT * FROM dream_policies WHERE enterprise_id = $1 AND status = 'published' ORDER BY id;

-- name: CreateDreamRun :one
INSERT INTO dream_runs (
    id, policy_id, version, enterprise_id, status, window_start, window_end,
    org_unit_id, policy_version, workflow_id, workflow_version, timezone,
    input_snapshot, visibility_snapshot, model_route, model_version, attempt,
    rerun_of_run_id, coverage, missing_inputs, idempotency_key
)
VALUES (
    sqlc.arg(id), sqlc.arg(policy_id), sqlc.arg(version), sqlc.arg(enterprise_id),
    sqlc.arg(status), sqlc.arg(window_start), sqlc.arg(window_end),
    sqlc.arg(org_unit_id), sqlc.arg(policy_version), sqlc.arg(workflow_id)::text,
    sqlc.arg(workflow_version)::integer, sqlc.arg(timezone),
    sqlc.arg(input_snapshot), sqlc.arg(visibility_snapshot), sqlc.arg(model_route),
    sqlc.arg(model_version), sqlc.arg(attempt), sqlc.narg(rerun_of_run_id),
    sqlc.arg(coverage), sqlc.arg(missing_inputs), sqlc.arg(idempotency_key)
)
RETURNING *;

-- name: GetDreamRun :one
SELECT * FROM dream_runs WHERE id = $1;

-- name: BindDreamWorkflowRun :one
UPDATE dream_runs SET workflow_run_id = sqlc.arg(workflow_run_id)
WHERE enterprise_id = sqlc.arg(enterprise_id) AND id = sqlc.arg(run_id)
  AND (workflow_run_id IS NULL OR workflow_run_id = sqlc.arg(workflow_run_id))
RETURNING *;

-- name: PublishDreamWorkflowWait :one
UPDATE dream_runs SET workflow_run_id=sqlc.arg(workflow_run_id), status='waiting_confirmation', error='',
    execution_owner=NULL, execution_lease_expires_at=NULL
WHERE enterprise_id=sqlc.arg(enterprise_id) AND id=sqlc.arg(run_id)
  AND status IN ('running','waiting_confirmation') AND (workflow_run_id IS NULL OR workflow_run_id=sqlc.arg(workflow_run_id))
RETURNING *;

-- name: GetDreamRunByWorkflowRun :one
SELECT * FROM dream_runs WHERE enterprise_id = sqlc.arg(enterprise_id) AND workflow_run_id = sqlc.arg(workflow_run_id);

-- name: RequeueDreamRunAfterWorkflow :one
UPDATE dream_runs SET status = 'pending', error = '', execution_owner=NULL, execution_lease_expires_at=NULL
WHERE enterprise_id = sqlc.arg(enterprise_id) AND workflow_run_id = sqlc.arg(workflow_run_id)
  AND status IN ('waiting_confirmation','pending')
RETURNING *;

-- name: FailDreamRunAfterWorkflow :one
UPDATE dream_runs SET status='failed', error=sqlc.arg(error), finished_at=COALESCE(finished_at, now()),
    execution_owner=NULL, execution_lease_expires_at=NULL
WHERE enterprise_id=sqlc.arg(enterprise_id) AND workflow_run_id=sqlc.arg(workflow_run_id)
  AND status IN ('running','waiting_confirmation','pending','failed')
RETURNING *;

-- name: ListPendingDreamWorkflowLifecycle :many
SELECT * FROM dream_workflow_lifecycle_outbox
WHERE processed_at IS NULL
ORDER BY id
LIMIT sqlc.arg(result_limit);

-- name: RecordDreamWorkflowLifecycleFailure :exec
UPDATE dream_workflow_lifecycle_outbox
SET attempts = attempts + 1, last_error = sqlc.arg(last_error), updated_at = now()
WHERE id = sqlc.arg(id) AND processed_at IS NULL;

-- name: CompleteDreamWorkflowLifecycle :exec
UPDATE dream_workflow_lifecycle_outbox
SET processed_at = now(), last_error = '', updated_at = now()
WHERE id = sqlc.arg(id) AND processed_at IS NULL;

-- name: ReserveDreamOutputHash :one
UPDATE dream_runs SET output_hash=sqlc.arg(output_hash)
WHERE enterprise_id=sqlc.arg(enterprise_id) AND id=sqlc.arg(run_id)
  AND (output_hash IS NULL OR output_hash=sqlc.arg(output_hash))
RETURNING *;

-- name: ReserveDreamOutputHashOwned :one
UPDATE dream_runs SET output_hash=sqlc.arg(output_hash)
WHERE enterprise_id=sqlc.arg(enterprise_id) AND id=sqlc.arg(run_id)
  AND execution_owner=sqlc.arg(execution_owner) AND execution_lease_expires_at > now()
  AND status='running' AND (output_hash IS NULL OR output_hash=sqlc.arg(output_hash))
RETURNING *;

-- name: FenceDreamExecutionOwner :one
SELECT id FROM dream_runs
WHERE id=sqlc.arg(id) AND execution_owner=sqlc.arg(execution_owner)
  AND execution_lease_expires_at > now() AND status='running'
FOR UPDATE;

-- name: ClaimDreamRun :execrows
UPDATE dream_runs SET status = 'running' WHERE id = $1 AND status = 'pending';

-- name: ClaimDreamRunLease :execrows
UPDATE dream_runs
SET status='running', execution_owner=sqlc.arg(execution_owner),
    execution_lease_expires_at=now()+interval '2 minutes'
WHERE id=sqlc.arg(id) AND status='pending';

-- name: RenewDreamRunLease :execrows
UPDATE dream_runs
SET execution_lease_expires_at=now()+interval '2 minutes'
WHERE id=sqlc.arg(id) AND status='running' AND execution_owner=sqlc.arg(execution_owner);

-- name: RecoverExpiredDreamRunAfterWorkflow :one
UPDATE dream_runs
SET status='pending', error='', execution_owner=NULL, execution_lease_expires_at=NULL
WHERE enterprise_id=sqlc.arg(enterprise_id) AND workflow_run_id=sqlc.arg(workflow_run_id)
  AND status='running' AND (execution_lease_expires_at IS NULL OR execution_lease_expires_at <= now())
RETURNING *;

-- name: RecoverExpiredUnboundDreamRuns :many
UPDATE dream_runs
SET status='pending', error='', execution_owner=NULL, execution_lease_expires_at=NULL
WHERE id IN (
    SELECT id FROM dream_runs
    WHERE status='running' AND workflow_run_id IS NULL
      AND (execution_lease_expires_at IS NULL OR execution_lease_expires_at <= now())
    ORDER BY created_at
    LIMIT sqlc.arg(result_limit)
    FOR UPDATE SKIP LOCKED
)
RETURNING id;

-- name: ListPendingDreamRuns :many
SELECT id FROM dream_runs WHERE status='pending' ORDER BY created_at LIMIT sqlc.arg(result_limit);

-- name: GetLatestDreamRunForPolicy :one
SELECT * FROM dream_runs WHERE policy_id = $1 ORDER BY window_end DESC LIMIT 1;

-- name: UpdateDreamRunStatus :execrows
UPDATE dream_runs
SET status = $2, error = $3,
    finished_at = CASE WHEN $2 IN ('succeeded','failed') THEN now() ELSE finished_at END,
    execution_owner = CASE WHEN $2 IN ('succeeded','failed') THEN NULL ELSE execution_owner END,
    execution_lease_expires_at = CASE WHEN $2 IN ('succeeded','failed') THEN NULL ELSE execution_lease_expires_at END
WHERE id = $1;

-- name: CompleteDreamRunOwned :execrows
UPDATE dream_runs
SET status=sqlc.arg(status), error=sqlc.arg(error), finished_at=now(),
    execution_owner=NULL, execution_lease_expires_at=NULL
WHERE id=sqlc.arg(id) AND status='running'
  AND execution_owner=sqlc.arg(execution_owner) AND execution_lease_expires_at > now()
  AND sqlc.arg(status) IN ('succeeded','failed');

-- name: InsertDreamInput :exec
INSERT INTO dream_inputs (run_id, source_type, source_id)
VALUES ($1, $2, $3)
ON CONFLICT (run_id, source_type, source_id) DO NOTHING;

-- name: ListDreamInputsForRun :many
SELECT * FROM dream_inputs WHERE run_id = sqlc.arg(run_id) ORDER BY source_type, source_id;

-- name: CreateDreamSummary :one
INSERT INTO dream_summaries (id, run_id, enterprise_id, space_id, layer, summary_text, sealed_object_key, evidence_pointer_id, risk_signals, facts, themes, trends, todos)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8,
        COALESCE(sqlc.narg(risk_signals)::jsonb, '[]'::jsonb), COALESCE(sqlc.narg(facts)::jsonb, '[]'::jsonb),
        COALESCE(sqlc.narg(themes)::jsonb, '[]'::jsonb), COALESCE(sqlc.narg(trends)::jsonb, '[]'::jsonb), COALESCE(sqlc.narg(todos)::jsonb, '[]'::jsonb))
RETURNING *;

-- name: GetDreamSummary :one
SELECT * FROM dream_summaries WHERE id = $1;

-- name: ListDreamSummariesBySpace :many
SELECT * FROM dream_summaries
WHERE space_id = $1 AND layer = $2
ORDER BY created_at DESC
LIMIT $3;

-- name: GetDreamSummaryForRunLayer :one
SELECT * FROM dream_summaries
WHERE enterprise_id = sqlc.arg(enterprise_id)
  AND run_id = sqlc.arg(run_id)
  AND space_id = sqlc.arg(space_id)
  AND layer = sqlc.arg(layer)
ORDER BY created_at DESC, id DESC
LIMIT 1;

-- name: InsertDreamEvidencePointer :exec
INSERT INTO dream_evidence_pointers (dream_summary_id, evidence_pointer_id)
VALUES ($1, $2)
ON CONFLICT DO NOTHING;

-- name: ListChildSpaces :many
SELECT DISTINCT spaces.*
FROM knowledge_spaces AS spaces
JOIN org_scope_bindings AS bindings ON bindings.space_id = spaces.id
JOIN org_scope_bindings AS parent_binding
  ON parent_binding.enterprise_id = bindings.enterprise_id
 AND parent_binding.scope_kind = bindings.parent_scope_kind
 AND parent_binding.scope_id = bindings.parent_scope_id
JOIN knowledge_spaces AS parent_space
  ON parent_space.id = parent_binding.space_id
 AND parent_space.enterprise_id = bindings.enterprise_id
WHERE spaces.enterprise_id = sqlc.arg(enterprise_id)
  AND bindings.enterprise_id = sqlc.arg(enterprise_id)
  AND parent_binding.scope_kind = sqlc.arg(parent_scope_kind)::text
  AND parent_binding.scope_id = sqlc.arg(parent_scope_id)::text
ORDER BY spaces.kind, spaces.name, spaces.id;

-- name: ListDreamImmediateChildren :many
SELECT DISTINCT spaces.*,
       parent_space.id::text AS parent_space_id,
       parent_binding.scope_kind::text AS parent_scope_kind,
       parent_binding.scope_id::text AS parent_scope_id,
       parent_space.org_scope::text AS parent_org_scope
FROM knowledge_spaces AS spaces
JOIN org_scope_bindings AS bindings
  ON bindings.enterprise_id = spaces.enterprise_id
 AND bindings.space_id = spaces.id
JOIN org_scope_bindings AS parent_binding
  ON parent_binding.enterprise_id = bindings.enterprise_id
 AND parent_binding.scope_kind = bindings.parent_scope_kind
 AND parent_binding.scope_id = bindings.parent_scope_id
JOIN knowledge_spaces AS parent_space
  ON parent_space.enterprise_id = parent_binding.enterprise_id
 AND parent_space.id = parent_binding.space_id
WHERE spaces.enterprise_id = sqlc.arg(enterprise_id)
  AND parent_binding.scope_kind = sqlc.arg(parent_scope_kind)::text
  AND parent_binding.scope_id = sqlc.arg(parent_scope_id)::text
ORDER BY spaces.kind, spaces.name, spaces.id, parent_space.id,
         parent_binding.scope_kind, parent_binding.scope_id, parent_space.org_scope
LIMIT sqlc.arg(result_limit);

-- name: ListCompletedChildDreamRuns :many
SELECT runs.*
FROM dream_runs AS runs
JOIN org_scope_bindings AS bindings
  ON bindings.enterprise_id = runs.enterprise_id
JOIN knowledge_spaces AS child_space
  ON child_space.id = bindings.space_id
 AND child_space.enterprise_id = runs.enterprise_id
 AND (bindings.scope_id = runs.org_unit_id OR child_space.org_scope = runs.org_unit_id)
JOIN org_scope_bindings AS parent_binding
  ON parent_binding.enterprise_id = bindings.enterprise_id
 AND parent_binding.scope_kind = bindings.parent_scope_kind
 AND parent_binding.scope_id = bindings.parent_scope_id
JOIN knowledge_spaces AS parent_space
  ON parent_space.id = parent_binding.space_id
 AND parent_space.enterprise_id = bindings.enterprise_id
WHERE runs.enterprise_id = sqlc.arg(enterprise_id)
  AND parent_binding.scope_kind = sqlc.arg(parent_scope_kind)::text
  AND parent_binding.scope_id = sqlc.arg(parent_scope_id)::text
  AND runs.status = 'succeeded'
  AND runs.window_start = sqlc.arg(window_start)
  AND runs.window_end = sqlc.arg(window_end)
ORDER BY runs.org_unit_id, runs.id;

-- name: ListDreamCompletedChildRuns :many
SELECT DISTINCT runs.*,
       child_space.id::text AS child_space_id,
       child_space.org_scope::text AS child_org_scope,
       parent_space.id::text AS parent_space_id,
       parent_binding.scope_kind::text AS parent_scope_kind,
       parent_binding.scope_id::text AS parent_scope_id,
       parent_space.org_scope::text AS parent_org_scope
FROM dream_runs AS runs
JOIN org_scope_bindings AS bindings
  ON bindings.enterprise_id = runs.enterprise_id
JOIN knowledge_spaces AS child_space
  ON child_space.id = bindings.space_id
 AND child_space.enterprise_id = runs.enterprise_id
 AND (bindings.scope_id = runs.org_unit_id OR child_space.org_scope = runs.org_unit_id)
JOIN org_scope_bindings AS parent_binding
  ON parent_binding.enterprise_id = bindings.enterprise_id
 AND parent_binding.scope_kind = bindings.parent_scope_kind
 AND parent_binding.scope_id = bindings.parent_scope_id
JOIN knowledge_spaces AS parent_space
  ON parent_space.id = parent_binding.space_id
 AND parent_space.enterprise_id = bindings.enterprise_id
WHERE runs.enterprise_id = sqlc.arg(enterprise_id)
  AND parent_binding.scope_kind = sqlc.arg(parent_scope_kind)::text
  AND parent_binding.scope_id = sqlc.arg(parent_scope_id)::text
  AND runs.status = 'succeeded'
  AND runs.window_start = sqlc.arg(window_start)
  AND runs.window_end = sqlc.arg(window_end)
ORDER BY runs.org_unit_id, runs.id, child_space.id, child_space.org_scope,
         parent_space.id, parent_binding.scope_kind, parent_binding.scope_id, parent_space.org_scope
LIMIT sqlc.arg(result_limit);

-- name: ListDreamRunsByOrg :many
SELECT *
FROM dream_runs
WHERE enterprise_id = sqlc.arg(enterprise_id)
  AND org_unit_id = sqlc.arg(org_unit_id)
ORDER BY window_end DESC, id DESC
LIMIT sqlc.arg(result_limit);

-- name: GetDreamRunView :one
SELECT runs.*,
       ARRAY(
           SELECT lineage.parent_run_id
           FROM dream_run_lineage AS lineage
           WHERE lineage.run_id = runs.id AND lineage.relation = 'child_summary'
           ORDER BY lineage.parent_run_id
       )::text[] AS parent_run_ids,
       (SELECT count(*) FROM dream_inputs AS inputs WHERE inputs.run_id = runs.id) AS input_count,
       COALESCE(summary.summary_text, '') AS display_summary,
       COALESCE(summary.facts, '[]'::jsonb) AS facts,
       COALESCE(summary.themes, '[]'::jsonb) AS themes,
       COALESCE(summary.trends, '[]'::jsonb) AS trends,
       COALESCE(summary.risk_signals, '[]'::jsonb) AS risks,
       COALESCE(summary.todos, '[]'::jsonb) AS todos,
       summary.evidence_pointer_id
FROM dream_runs AS runs
LEFT JOIN LATERAL (
    SELECT dream_summaries.*
    FROM dream_summaries
    WHERE dream_summaries.run_id = runs.id
      AND dream_summaries.enterprise_id = runs.enterprise_id
      AND dream_summaries.layer = 'display'
    ORDER BY dream_summaries.created_at DESC, dream_summaries.id DESC
    LIMIT 1
) AS summary ON true
WHERE runs.enterprise_id = sqlc.arg(enterprise_id)
  AND runs.id = sqlc.arg(run_id);

-- name: CreateDreamRunLineage :one
INSERT INTO dream_run_lineage (run_id, parent_run_id, relation)
VALUES (sqlc.arg(run_id), sqlc.arg(parent_run_id), sqlc.arg(relation))
RETURNING *;

-- name: CreateDreamAnnotation :one
INSERT INTO dream_run_annotations (
    id, enterprise_id, run_id, annotation_type, body, created_by
)
VALUES (
    sqlc.arg(id), sqlc.arg(enterprise_id), sqlc.arg(run_id),
    sqlc.arg(annotation_type), sqlc.arg(body), sqlc.arg(created_by)
)
RETURNING *;
