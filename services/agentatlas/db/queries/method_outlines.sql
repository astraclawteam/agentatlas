-- name: CreateMethodOutline :one
INSERT INTO method_outlines (id, enterprise_id, title, outline, org_scope)
VALUES ($1, $2, $3, $4, $5)
RETURNING *;

-- name: GetMethodOutline :one
SELECT * FROM method_outlines WHERE id = $1;

-- name: ListMethodOutlines :many
SELECT * FROM method_outlines
WHERE enterprise_id = $1
ORDER BY created_at DESC
LIMIT 200;
