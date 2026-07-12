-- name: CreateWorkBrief :one
INSERT INTO work_briefs (id, enterprise_id, employee_user_id, brief_date, summary, topics, project_refs, source_hash, evidence_pointer_id)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
RETURNING *;

-- name: GetWorkBrief :one
SELECT * FROM work_briefs WHERE id = $1;

-- name: GetWorkBriefBySourceHash :one
SELECT * FROM work_briefs WHERE enterprise_id = $1 AND source_hash = $2;

-- name: ListWorkBriefsByEmployee :many
SELECT * FROM work_briefs
WHERE enterprise_id = $1 AND employee_user_id = $2 AND brief_date >= $3 AND brief_date <= $4
ORDER BY brief_date DESC;

-- name: ListWorkBriefsForWindow :many
SELECT * FROM work_briefs
WHERE enterprise_id = $1 AND employee_user_id = ANY($2::text[]) AND brief_date >= $3 AND brief_date <= $4
ORDER BY employee_user_id, brief_date;

-- name: ListDreamWorkBriefsForWindow :many
SELECT * FROM work_briefs
WHERE enterprise_id = sqlc.arg(enterprise_id)
  AND employee_user_id = ANY(sqlc.arg(employee_user_ids)::text[])
  AND brief_date >= sqlc.arg(window_start)
  AND brief_date < sqlc.arg(window_end)
ORDER BY employee_user_id, brief_date, id
LIMIT sqlc.arg(result_limit);
