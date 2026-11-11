-- Claims up to batch_size due monitors for one worker in a single statement,
-- so the lease commits before any network call. SKIP LOCKED lets concurrent
-- workers claim disjoint sets without waiting on each other. Each lease lasts
-- GREATEST(WORKER_LEASE_DURATION, monitor timeout + 5s), and every time comes
-- from PostgreSQL's now(), so clock skew between workers cannot matter.
-- updated_at is left alone: it tracks edits, not scheduling.
-- name: ClaimDueMonitors :many
WITH due AS (
    SELECT id
    FROM monitors
    WHERE enabled
      AND deleted_at IS NULL
      AND next_check_at <= now()
      AND (lease_until IS NULL OR lease_until <= now())
    ORDER BY next_check_at
    LIMIT sqlc.arg('batch_size')
    FOR UPDATE SKIP LOCKED
)
UPDATE monitors m
SET lease_owner = sqlc.arg('worker_id'),
    lease_until = now() + GREATEST(
        make_interval(secs => sqlc.arg('lease_seconds')::float8),
        make_interval(secs => m.timeout_ms / 1000.0 + 5)
    )
FROM due
WHERE m.id = due.id
RETURNING m.*;

-- Records a check result and releases the lease atomically. The insert only
-- happens if the lease update matched, so no row returned means the lease was
-- lost (expired and reclaimed, or the monitor was soft-deleted) and the late
-- result must be discarded. next_check_at is rescheduled from completion
-- unless a manual request moved it while the check was running.
-- name: RecordCheckResult :one
WITH released AS (
    UPDATE monitors
    SET last_checked_at  = now(),
        last_status_code = sqlc.narg('status_code'),
        last_latency_ms  = sqlc.arg('latency_ms'),
        next_check_at    = CASE
            WHEN next_check_at = sqlc.arg('claimed_next_check_at')::timestamptz
                THEN now() + make_interval(secs => interval_seconds)
            ELSE next_check_at  -- a manual request arrived mid-check; keep it
        END,
        lease_owner = NULL,
        lease_until = NULL
    -- Qualified: sqlc (not PostgreSQL) reports a bare "id" here as ambiguous
    -- with monitor_checks.id from the outer INSERT.
    WHERE monitors.id = sqlc.arg('monitor_id')
      AND lease_owner = sqlc.arg('worker_id')
      AND deleted_at IS NULL
    RETURNING id
)
INSERT INTO monitor_checks (monitor_id, success, status_code, latency_ms,
                            error_kind, error_message, checked_at)
-- Explicit casts: sqlc cannot infer parameter types in a SELECT list.
SELECT id, sqlc.arg('success')::boolean, sqlc.narg('status_code')::int,
       sqlc.arg('latency_ms')::int, sqlc.narg('error_kind')::varchar,
       sqlc.narg('error_message')::text, now()
FROM released
RETURNING id;

-- Gives a lease back without recording a result (e.g. a check cancelled at
-- shutdown). next_check_at is untouched, so the monitor stays due and is
-- reclaimed on the next poll.
-- name: ReleaseLease :execrows
UPDATE monitors
SET lease_owner = NULL, lease_until = NULL
WHERE id = sqlc.arg('monitor_id')
  AND lease_owner = sqlc.arg('worker_id');

-- Keyset pagination over (checked_at, id), newest first, backed by
-- monitor_checks_history_idx. As with ListMonitors, cursor_id is ignored on
-- the first page, where cursor_checked_at is NULL.
-- name: ListChecks :many
SELECT * FROM monitor_checks
WHERE monitor_id = sqlc.arg('monitor_id')
  AND (sqlc.narg('cursor_checked_at')::timestamptz IS NULL
       OR (checked_at, id) < (sqlc.narg('cursor_checked_at')::timestamptz,
                              sqlc.arg('cursor_id')::bigint))
ORDER BY checked_at DESC, id DESC
LIMIT sqlc.arg('page_limit');
