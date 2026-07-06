-- name: CreateArtifact :one
INSERT INTO artifacts (id, enterprise_id, filename, content_type, object_key, source_hash, evidence_pointer_id, size_bytes)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
RETURNING *;

-- name: GetArtifact :one
SELECT * FROM artifacts WHERE id = $1;

-- name: CreateArtifactJob :one
INSERT INTO artifact_processing_jobs (id, enterprise_id, artifact_id, status, parser_hint)
VALUES ($1, $2, $3, $4, $5)
RETURNING *;

-- name: UpdateArtifactJobStatus :execrows
UPDATE artifact_processing_jobs
SET status = $2, error = $3,
    started_at = CASE WHEN $2 = 'running' AND started_at IS NULL THEN now() ELSE started_at END,
    finished_at = CASE WHEN $2 IN ('succeeded','failed') THEN now() ELSE finished_at END
WHERE id = $1;

-- name: GetArtifactJob :one
SELECT * FROM artifact_processing_jobs WHERE id = $1;

-- name: InsertArtifactStep :exec
INSERT INTO artifact_processing_steps (job_id, step, status, detail)
VALUES ($1, $2, $3, $4);

-- name: CreateAtlasDocument :one
INSERT INTO atlas_documents (id, enterprise_id, artifact_id, source_hash, content_type, provider, object_key, confidence)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
RETURNING *;

-- name: GetAtlasDocument :one
SELECT * FROM atlas_documents WHERE id = $1;

-- name: InsertDocumentBlock :exec
INSERT INTO document_blocks (atlas_document_id, block_id, block_type, page, block_order, text_hash, sanitized_excerpt)
VALUES ($1, $2, $3, $4, $5, $6, $7);

-- name: InsertDocumentSummary :one
INSERT INTO document_summaries (atlas_document_id, level, ref, summary_text)
VALUES ($1, $2, $3, $4)
RETURNING *;

-- name: ListDocumentSummaries :many
SELECT * FROM document_summaries WHERE atlas_document_id = $1 ORDER BY id;

-- name: InsertParserProviderRun :exec
INSERT INTO parser_provider_runs (job_id, provider_id, status, latency_ms, confidence, error)
VALUES ($1, $2, $3, $4, $5, $6);
