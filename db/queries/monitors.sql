-- Sprint 1 keeps this file to the two read paths needed to prove the sqlc
-- pipeline end to end. The full CRUD surface arrives in Sprint 2.

-- name: GetMonitor :one
SELECT *
FROM monitors
WHERE id = $1
  AND deleted_at IS NULL;

-- name: ListMonitors :many
SELECT *
FROM monitors
WHERE deleted_at IS NULL
ORDER BY created_at DESC, id DESC
LIMIT $1 OFFSET $2;
