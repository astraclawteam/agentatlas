-- name: CreateEvidencePointer :one
INSERT INTO evidence_pointers (id, enterprise_id, resource_type, resource_ref, source_system, content_hash, summary_hash, agentnexus_resource_uri, required_scopes)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
RETURNING *;

-- name: GetEvidencePointer :one
SELECT * FROM evidence_pointers WHERE id = $1;

-- name: ListEvidencePointersByIDs :many
SELECT * FROM evidence_pointers WHERE enterprise_id = $1 AND id = ANY($2::text[]);
