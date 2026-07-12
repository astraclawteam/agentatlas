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

-- name: ClaimDreamRun :execrows
UPDATE dream_runs SET status = 'running' WHERE id = $1 AND status = 'pending';

-- name: GetLatestDreamRunForPolicy :one
SELECT * FROM dream_runs WHERE policy_id = $1 ORDER BY window_end DESC LIMIT 1;

-- name: UpdateDreamRunStatus :execrows
UPDATE dream_runs
SET status = $2, error = $3,
    finished_at = CASE WHEN $2 IN ('succeeded','failed') THEN now() ELSE finished_at END
WHERE id = $1;

-- name: InsertDreamInput :exec
INSERT INTO dream_inputs (run_id, source_type, source_id)
VALUES ($1, $2, $3);

-- name: CreateDreamSummary :one
INSERT INTO dream_summaries (id, run_id, enterprise_id, space_id, layer, summary_text, sealed_object_key, evidence_pointer_id, risk_signals)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
RETURNING *;

-- name: GetDreamSummary :one
SELECT * FROM dream_summaries WHERE id = $1;

-- name: ListDreamSummariesBySpace :many
SELECT * FROM dream_summaries
WHERE space_id = $1 AND layer = $2
ORDER BY created_at DESC
LIMIT $3;

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
 AND parent_binding.scope_id = bindings.parent_scope_id
JOIN knowledge_spaces AS parent_space
  ON parent_space.id = parent_binding.space_id
 AND parent_space.enterprise_id = bindings.enterprise_id
WHERE spaces.enterprise_id = sqlc.arg(enterprise_id)
  AND bindings.enterprise_id = sqlc.arg(enterprise_id)
  AND (
      parent_binding.scope_id = sqlc.arg(parent_org_unit_id)::text
      OR parent_space.org_scope = sqlc.arg(parent_org_unit_id)::text
  )
ORDER BY spaces.kind, spaces.name, spaces.id;

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
 AND parent_binding.scope_id = bindings.parent_scope_id
JOIN knowledge_spaces AS parent_space
  ON parent_space.id = parent_binding.space_id
 AND parent_space.enterprise_id = bindings.enterprise_id
WHERE runs.enterprise_id = sqlc.arg(enterprise_id)
  AND (
      parent_binding.scope_id = sqlc.arg(parent_org_unit_id)::text
      OR parent_space.org_scope = sqlc.arg(parent_org_unit_id)::text
  )
  AND runs.status = 'succeeded'
  AND runs.window_start = sqlc.arg(window_start)
  AND runs.window_end = sqlc.arg(window_end)
ORDER BY runs.org_unit_id, runs.id;

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
