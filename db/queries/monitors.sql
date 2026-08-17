-- name: CreateMonitor :one
INSERT INTO monitors (
    name, url, interval_seconds, timeout_ms,
    expected_status_min, expected_status_max, failure_threshold, enabled,
    next_check_at
) VALUES (
    $1, $2, $3, $4,
    $5, $6, $7, $8,
    now()
)
RETURNING *;

-- name: GetMonitor :one
SELECT *
FROM monitors
WHERE id = $1
  AND deleted_at IS NULL;

-- Keyset pagination over (created_at, id). The row-value comparison is what
-- makes the page boundary stable when several monitors share a created_at;
-- OFFSET would skip or repeat rows as the table changes underneath.
-- The two cursor parameters are always supplied together, so only the
-- timestamp needs to be nullable; cursor_id is ignored (passed as the zero
-- UUID) on the first page, where the IS NULL branch already matches.
-- name: ListMonitors :many
SELECT *
FROM monitors
WHERE deleted_at IS NULL
  AND (
      sqlc.narg('cursor_created_at')::timestamptz IS NULL
      OR (created_at, id) < (
          sqlc.narg('cursor_created_at')::timestamptz,
          sqlc.arg('cursor_id')::uuid
      )
  )
ORDER BY created_at DESC, id DESC
LIMIT sqlc.arg('page_limit');

-- Partial update: every field falls back to its current value when the caller
-- omitted it. next_check_at is pulled in only when it would otherwise sit
-- further out than the (possibly shortened) interval allows, so a tightened
-- schedule takes effect without making every edited monitor due at once.
-- name: UpdateMonitor :one
UPDATE monitors
SET name                = COALESCE(sqlc.narg('name'), name),
    url                 = COALESCE(sqlc.narg('url'), url),
    interval_seconds    = COALESCE(sqlc.narg('interval_seconds'), interval_seconds),
    timeout_ms          = COALESCE(sqlc.narg('timeout_ms'), timeout_ms),
    expected_status_min = COALESCE(sqlc.narg('expected_status_min'), expected_status_min),
    expected_status_max = COALESCE(sqlc.narg('expected_status_max'), expected_status_max),
    failure_threshold   = COALESCE(sqlc.narg('failure_threshold'), failure_threshold),
    enabled             = COALESCE(sqlc.narg('enabled'), enabled),
    next_check_at       = LEAST(
        next_check_at,
        now() + make_interval(secs => COALESCE(sqlc.narg('interval_seconds'), interval_seconds))
    ),
    updated_at          = now()
WHERE id = sqlc.arg('id')
  AND deleted_at IS NULL
RETURNING *;

-- Soft delete. The lease is cleared so a worker that is mid-cycle cannot keep
-- treating the monitor as its own.
-- name: SoftDeleteMonitor :one
UPDATE monitors
SET deleted_at  = now(),
    updated_at  = now(),
    lease_owner = NULL,
    lease_until = NULL
WHERE id = $1
  AND deleted_at IS NULL
RETURNING id;
