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

-- name: ApplyOrgSpaceEvent :one
WITH existing_space AS MATERIALIZED (
    SELECT knowledge_spaces.id, knowledge_spaces.org_version
    FROM knowledge_spaces
    WHERE knowledge_spaces.enterprise_id = sqlc.arg(event_enterprise_id)
      AND knowledge_spaces.org_scope = sqlc.arg(event_org_scope)
),
accepted_space AS (
    INSERT INTO knowledge_spaces (id, enterprise_id, kind, name, org_scope, org_version)
    VALUES (sqlc.arg(new_space_id), sqlc.arg(event_enterprise_id), sqlc.arg(event_scope_kind),
            sqlc.arg(event_space_name), sqlc.arg(event_org_scope), sqlc.arg(event_org_version))
    ON CONFLICT (enterprise_id, org_scope) DO UPDATE
    SET name = EXCLUDED.name,
        org_version = EXCLUDED.org_version,
        updated_at = now()
    WHERE knowledge_spaces.org_version < EXCLUDED.org_version
    RETURNING knowledge_spaces.id, knowledge_spaces.org_version
),
binding_write AS (
    INSERT INTO org_scope_bindings (
        enterprise_id, space_id, scope_kind, scope_id,
        parent_scope_kind, parent_scope_id
    )
    SELECT sqlc.arg(event_enterprise_id), accepted_space.id,
           sqlc.arg(event_scope_kind), sqlc.arg(event_scope_id),
           sqlc.narg(event_parent_scope_kind), sqlc.narg(event_parent_scope_id)
    FROM accepted_space
    ON CONFLICT (enterprise_id, scope_kind, scope_id)
    DO UPDATE SET space_id = EXCLUDED.space_id,
                  parent_scope_kind = EXCLUDED.parent_scope_kind,
                  parent_scope_id = EXCLUDED.parent_scope_id
    RETURNING 1
),
version_write AS (
    INSERT INTO knowledge_space_versions (space_id, org_version, snapshot)
    SELECT accepted_space.id, accepted_space.org_version,
           sqlc.arg(event_version_snapshot)::jsonb
    FROM accepted_space
    RETURNING 1
),
member_write AS (
    INSERT INTO space_membership_cache (
        space_id, user_id, display_name, org_version, updated_at
    )
    SELECT accepted_space.id, members.user_id, members.display_name,
           accepted_space.org_version, now()
    FROM accepted_space
    CROSS JOIN jsonb_to_recordset(sqlc.arg(event_member_snapshot)::jsonb)
        AS members(user_id text, display_name text)
    ON CONFLICT (space_id, user_id)
    DO UPDATE SET display_name = EXCLUDED.display_name,
                  org_version = EXCLUDED.org_version,
                  updated_at = now()
    WHERE space_membership_cache.org_version < EXCLUDED.org_version
    RETURNING 1
),
member_delete AS (
    DELETE FROM space_membership_cache AS cached
    USING accepted_space
    WHERE cached.space_id = accepted_space.id
      AND cached.org_version < accepted_space.org_version
      AND NOT EXISTS (
          SELECT 1
          FROM jsonb_to_recordset(sqlc.arg(event_member_snapshot)::jsonb)
              AS desired(user_id text, display_name text)
          WHERE desired.user_id = cached.user_id
      )
    RETURNING 1
),
effects AS (
    SELECT (SELECT count(*) FROM binding_write) +
           (SELECT count(*) FROM version_write) +
           (SELECT count(*) FROM member_write) +
           (SELECT count(*) FROM member_delete) AS write_count
)
SELECT accepted_space.id::text AS space_id,
       true::boolean AS accepted,
       (NOT EXISTS (SELECT 1 FROM existing_space))::boolean AS created
FROM accepted_space, effects
UNION ALL
SELECT existing_space.id::text AS space_id,
       false::boolean AS accepted,
       false::boolean AS created
FROM existing_space
WHERE NOT EXISTS (SELECT 1 FROM accepted_space)
LIMIT 1;

-- name: ListKnowledgeSpacesByEnterprise :many
SELECT * FROM knowledge_spaces WHERE enterprise_id = $1 ORDER BY kind, name;

-- name: UpdateKnowledgeSpaceIfNewer :execrows
UPDATE knowledge_spaces
SET name = $3, org_version = $4, updated_at = now()
WHERE id = $1 AND enterprise_id = $2 AND org_version < $4;

-- name: InsertKnowledgeSpaceVersion :exec
INSERT INTO knowledge_space_versions (space_id, org_version, snapshot)
VALUES ($1, $2, $3);

-- name: UpsertOrgScopeBinding :exec
INSERT INTO org_scope_bindings (enterprise_id, space_id, scope_kind, scope_id, parent_scope_kind, parent_scope_id)
VALUES ($1, $2, $3, $4, $5, $6)
ON CONFLICT (enterprise_id, scope_kind, scope_id)
DO UPDATE SET space_id = EXCLUDED.space_id,
              parent_scope_kind = EXCLUDED.parent_scope_kind,
              parent_scope_id = EXCLUDED.parent_scope_id;

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

-- name: ListDreamSpaceMembers :many
SELECT * FROM space_membership_cache
WHERE space_id = sqlc.arg(space_id)
ORDER BY user_id
LIMIT sqlc.arg(result_limit);

-- name: ListEnterprises :many
SELECT * FROM enterprises ORDER BY created_at;
