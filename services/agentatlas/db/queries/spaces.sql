-- name: UpsertEnterprise :one
INSERT INTO enterprises (id, name)
VALUES ($1, $2)
ON CONFLICT (id) DO UPDATE SET name = EXCLUDED.name
RETURNING *;

-- name: EnsureEnterprise :exec
INSERT INTO enterprises (id, name)
VALUES ($1, $2)
ON CONFLICT (id) DO NOTHING;

-- name: GetEnterprise :one
SELECT * FROM enterprises WHERE id = $1;

-- name: InsertKnowledgeSpace :one
INSERT INTO knowledge_spaces (id, enterprise_id, kind, name, org_scope, org_version)
VALUES ($1, $2, $3, $4, $5, $6)
RETURNING *;

-- name: GetKnowledgeSpace :one
SELECT * FROM knowledge_spaces WHERE id = $1;

-- name: GetKnowledgeSpaceByScope :one
SELECT * FROM knowledge_spaces WHERE enterprise_id = $1 AND org_scope = $2;

-- name: ListKnowledgeSpacesByEnterprise :many
SELECT * FROM knowledge_spaces WHERE enterprise_id = $1 ORDER BY kind, name;

-- name: UpdateKnowledgeSpaceIfNewer :execrows
UPDATE knowledge_spaces
SET name = $3, org_version = $4, updated_at = now()
WHERE id = $1 AND enterprise_id = $2 AND org_version <= $4;

-- name: InsertKnowledgeSpaceVersion :exec
INSERT INTO knowledge_space_versions (space_id, org_version, snapshot)
VALUES ($1, $2, $3);

-- name: UpsertOrgScopeBinding :exec
INSERT INTO org_scope_bindings (enterprise_id, space_id, scope_kind, scope_id, parent_scope_id)
VALUES ($1, $2, $3, $4, $5)
ON CONFLICT (enterprise_id, scope_kind, scope_id)
DO UPDATE SET space_id = EXCLUDED.space_id, parent_scope_id = EXCLUDED.parent_scope_id;

-- name: UpsertOrgSnapshot :exec
INSERT INTO org_snapshots (enterprise_id, org_version, snapshot)
VALUES ($1, $2, $3)
ON CONFLICT (enterprise_id, org_version) DO NOTHING;

-- name: UpsertSpaceMember :exec
INSERT INTO space_membership_cache (space_id, user_id, display_name, org_version, updated_at)
VALUES ($1, $2, $3, $4, now())
ON CONFLICT (space_id, user_id)
DO UPDATE SET display_name = EXCLUDED.display_name, org_version = EXCLUDED.org_version, updated_at = now()
WHERE space_membership_cache.org_version <= EXCLUDED.org_version;

-- name: DeleteSpaceMembersStale :execrows
DELETE FROM space_membership_cache WHERE space_id = $1 AND org_version < $2;

-- name: ListSpaceMembers :many
SELECT * FROM space_membership_cache WHERE space_id = $1 ORDER BY user_id;

-- name: ListEnterprises :many
SELECT * FROM enterprises ORDER BY created_at;
