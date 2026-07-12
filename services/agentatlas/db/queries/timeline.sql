-- name: InsertTimelineNode :one
INSERT INTO timeline_nodes (id, enterprise_id, space_id, org_scope, node_time, source_type, summary_text, tags, evidence_pointer_id)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
RETURNING *;

-- name: GetTimelineNode :one
SELECT * FROM timeline_nodes WHERE id = $1;

-- name: ListTimelineNodes :many
SELECT * FROM timeline_nodes
WHERE space_id = $1
  AND ($2::timestamptz IS NULL OR node_time >= $2)
  AND ($3::timestamptz IS NULL OR node_time <= $3)
ORDER BY node_time DESC
LIMIT $4;

-- name: ListDreamTimelineNodes :many
SELECT * FROM timeline_nodes
WHERE enterprise_id = sqlc.arg(enterprise_id)
  AND space_id = sqlc.arg(space_id)
  AND org_scope = sqlc.arg(org_scope)
  AND source_type = sqlc.arg(source_type)
  AND node_time >= sqlc.arg(window_start)
  AND node_time < sqlc.arg(window_end)
ORDER BY node_time, id
LIMIT sqlc.arg(result_limit);
