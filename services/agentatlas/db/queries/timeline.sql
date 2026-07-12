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

-- name: ListBrowserKnowledgeItems :many
SELECT sops.id, sops.title AS summary_text, 'sop'::text AS source_type, sops.updated_at AS node_time, spaces.name AS scope_name
FROM sops
JOIN knowledge_spaces AS spaces
  ON spaces.enterprise_id = sops.enterprise_id
 AND spaces.id = sqlc.arg(space_id)
 AND spaces.org_scope = sops.org_scope
WHERE sops.enterprise_id = sqlc.arg(enterprise_id)
  AND sops.org_scope = sqlc.arg(org_scope)
  AND (sqlc.arg(search_query)::text = '' OR sops.title ILIKE '%' || sqlc.arg(search_query)::text || '%' ESCAPE '\')
UNION ALL
SELECT outlines.id, outlines.title AS summary_text, 'method_outline'::text AS source_type, outlines.updated_at AS node_time, spaces.name AS scope_name
FROM method_outlines AS outlines
JOIN knowledge_spaces AS spaces
  ON spaces.enterprise_id = outlines.enterprise_id
 AND spaces.id = sqlc.arg(space_id)
 AND spaces.org_scope = outlines.org_scope
WHERE outlines.enterprise_id = sqlc.arg(enterprise_id)
  AND outlines.org_scope = sqlc.arg(org_scope)
  AND (sqlc.arg(search_query)::text = '' OR outlines.title ILIKE '%' || sqlc.arg(search_query)::text || '%' ESCAPE '\')
ORDER BY node_time DESC, id
LIMIT sqlc.arg(result_limit);
