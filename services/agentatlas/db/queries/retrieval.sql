-- name: CreateRetrievalPlan :one
INSERT INTO retrieval_plans (id, enterprise_id, query_hash, sanitized_query, space_ids, org_scopes, filters)
VALUES ($1, $2, $3, $4, $5, $6, $7)
RETURNING *;

-- name: GetRetrievalPlan :one
SELECT * FROM retrieval_plans WHERE id = $1;

-- name: InsertRetrievalPlanStep :exec
INSERT INTO retrieval_plan_steps (plan_id, step_no, kind, params)
VALUES ($1, $2, $3, $4);

-- name: ListRetrievalPlanSteps :many
SELECT * FROM retrieval_plan_steps WHERE plan_id = $1 ORDER BY step_no;

-- name: InsertRetrievalResult :exec
INSERT INTO retrieval_results (plan_id, evidence_pointer_id, doc_ref, score, snippet)
VALUES ($1, $2, $3, $4, $5);

-- name: CreateIndexJob :one
INSERT INTO index_jobs (id, enterprise_id, source_type, source_id, status)
VALUES ($1, $2, $3, $4, $5)
RETURNING *;

-- name: UpdateIndexJobStatus :execrows
UPDATE index_jobs
SET status = $2, error = $3,
    finished_at = CASE WHEN $2 IN ('succeeded','failed') THEN now() ELSE finished_at END
WHERE id = $1;

-- name: ListPendingIndexJobs :many
SELECT * FROM index_jobs WHERE status = 'pending' ORDER BY created_at LIMIT $1;

-- name: UpsertIndexDocument :exec
INSERT INTO index_documents (id, enterprise_id, index_name, source_type, source_id, evidence_pointer_id, org_version)
VALUES ($1, $2, $3, $4, $5, $6, $7)
ON CONFLICT (index_name, source_type, source_id)
DO UPDATE SET evidence_pointer_id = EXCLUDED.evidence_pointer_id,
              org_version = EXCLUDED.org_version,
              indexed_at = now();
