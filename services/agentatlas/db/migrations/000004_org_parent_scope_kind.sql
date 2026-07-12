-- +goose Up

ALTER TABLE org_scope_bindings
    ADD COLUMN parent_scope_kind text
    CHECK (parent_scope_kind IN ('employee','project_group','department','business_unit','company'));

WITH unique_parent AS (
    SELECT child.id AS child_binding_id, min(parent.scope_kind) AS parent_scope_kind
    FROM org_scope_bindings AS child
    JOIN org_scope_bindings AS parent
      ON parent.enterprise_id = child.enterprise_id
     AND parent.scope_id = child.parent_scope_id
    WHERE child.parent_scope_id IS NOT NULL
    GROUP BY child.id
    HAVING count(*) = 1
)
UPDATE org_scope_bindings AS child
SET parent_scope_kind = unique_parent.parent_scope_kind
FROM unique_parent
WHERE child.id = unique_parent.child_binding_id;

-- Ambiguous legacy parent IDs cannot be assigned safely. Drop only the
-- unverifiable edge so hierarchy reads fail closed instead of choosing a kind.
UPDATE org_scope_bindings
SET parent_scope_id = NULL
WHERE parent_scope_id IS NOT NULL AND parent_scope_kind IS NULL;

ALTER TABLE org_scope_bindings
    ADD CONSTRAINT org_scope_bindings_parent_identity_pair_check
    CHECK ((parent_scope_kind IS NULL) = (parent_scope_id IS NULL)),
    ADD CONSTRAINT org_scope_bindings_parent_identity_fk
    FOREIGN KEY (enterprise_id, parent_scope_kind, parent_scope_id)
    REFERENCES org_scope_bindings (enterprise_id, scope_kind, scope_id);

-- +goose Down

-- The old schema cannot represent a referenced parent when the same bare ID
-- exists under multiple kinds. Fail closed instead of restoring ambiguous
-- hierarchy joins; operators must remove or rename the collision first.
-- +goose StatementBegin
DO $$
BEGIN
    IF EXISTS (
        SELECT 1
        FROM org_scope_bindings AS child
        JOIN org_scope_bindings AS candidate
          ON candidate.enterprise_id = child.enterprise_id
         AND candidate.scope_id = child.parent_scope_id
        WHERE child.parent_scope_id IS NOT NULL
        GROUP BY child.id
        HAVING count(*) > 1
    ) THEN
        RAISE EXCEPTION 'cannot roll back migration 000004 while referenced cross-kind parent ID collisions exist';
    END IF;
END $$;
-- +goose StatementEnd

ALTER TABLE org_scope_bindings
    DROP CONSTRAINT org_scope_bindings_parent_identity_fk,
    DROP CONSTRAINT org_scope_bindings_parent_identity_pair_check,
    DROP COLUMN parent_scope_kind;
