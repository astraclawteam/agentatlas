-- name: CreateChangeDraft :one
INSERT INTO change_drafts (
    id, enterprise_id, org_unit_id, resource_type, resource_id, action,
    requester_user_id, origin, permission_mode, revision, state,
    base_version, proposed_content
)
VALUES (
    sqlc.arg(id), sqlc.arg(enterprise_id), sqlc.arg(org_unit_id),
    sqlc.arg(resource_type), sqlc.arg(resource_id), sqlc.arg(action),
    sqlc.arg(requester_user_id), sqlc.arg(origin), sqlc.arg(permission_mode),
    sqlc.arg(revision), sqlc.arg(state), sqlc.arg(base_version),
    sqlc.arg(proposed_content)
)
RETURNING *;

-- name: UpdateChangeDraftIfRevision :one
UPDATE change_drafts
SET proposed_content = sqlc.arg(proposed_content),
    state = sqlc.arg(state),
    revision = revision + 1,
    updated_at = now()
WHERE id = sqlc.arg(id)
  AND enterprise_id = sqlc.arg(enterprise_id)
  AND revision = sqlc.arg(expected_revision)
RETURNING *;

-- name: CreateChangeVersion :one
INSERT INTO change_versions (
    id, enterprise_id, change_id, version, content, published_by
)
VALUES (
    sqlc.arg(id), sqlc.arg(enterprise_id), sqlc.arg(change_id),
    sqlc.arg(version), sqlc.arg(content), sqlc.arg(published_by)
)
RETURNING *;

-- name: CreateChangeReview :one
INSERT INTO change_reviews (
    id, enterprise_id, change_id, change_revision, reviewer_user_id,
    risk_level, risk_reasons, review_mode, state, org_path, queue,
    decision, comment
)
VALUES (
    sqlc.arg(id), sqlc.arg(enterprise_id), sqlc.arg(change_id),
    sqlc.arg(change_revision), sqlc.narg(reviewer_user_id),
    sqlc.arg(risk_level), sqlc.arg(risk_reasons), sqlc.arg(review_mode),
    sqlc.arg(state), sqlc.arg(org_path), sqlc.narg(queue),
    sqlc.arg(decision), sqlc.arg(comment)
)
RETURNING *;

-- name: GetOrCreatePublishOperation :one
INSERT INTO publish_operations (
    id, enterprise_id, change_id, change_revision, idempotency_key, status
)
VALUES (
    sqlc.arg(id), sqlc.arg(enterprise_id), sqlc.arg(change_id),
    sqlc.arg(change_revision), sqlc.arg(idempotency_key), sqlc.arg(status)
)
ON CONFLICT (enterprise_id, idempotency_key) DO UPDATE
SET idempotency_key = publish_operations.idempotency_key
WHERE publish_operations.change_id = EXCLUDED.change_id
  AND publish_operations.change_revision = EXCLUDED.change_revision
RETURNING *;
