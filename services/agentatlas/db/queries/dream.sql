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
INSERT INTO dream_runs (id, policy_id, version, enterprise_id, status, window_start, window_end)
VALUES ($1, $2, $3, $4, $5, $6, $7)
RETURNING *;

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

-- name: ListDreamSummariesBySpace :many
SELECT * FROM dream_summaries
WHERE space_id = $1 AND layer = $2
ORDER BY created_at DESC
LIMIT $3;

-- name: InsertDreamEvidencePointer :exec
INSERT INTO dream_evidence_pointers (dream_summary_id, evidence_pointer_id)
VALUES ($1, $2)
ON CONFLICT DO NOTHING;
