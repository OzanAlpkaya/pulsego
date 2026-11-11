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

-- Step 2 of the check-result transaction (Sprint 4 D3): locks the monitor row
-- for the rest of the transaction, but only while workerID still holds the
-- lease. No row means the lease was lost (expired and reclaimed, or the
-- monitor was soft-deleted) and the late result must be discarded. Holding
-- this lock until commit is what serializes results for one monitor.
-- name: LockLeasedMonitor :one
SELECT * FROM monitors
WHERE id = sqlc.arg('monitor_id')
  AND lease_owner = sqlc.arg('worker_id')
  AND deleted_at IS NULL
FOR UPDATE;

-- Step 4: stores the check row. checked_at is the transaction's now(), so it
-- equals last_checked_at and any incident timestamp written alongside it.
-- name: InsertCheck :one
INSERT INTO monitor_checks (monitor_id, success, status_code, latency_ms,
                            error_kind, error_message, checked_at)
VALUES (sqlc.arg('monitor_id'), sqlc.arg('success'), sqlc.narg('status_code'),
        sqlc.arg('latency_ms'), sqlc.narg('error_kind'), sqlc.narg('error_message'),
        now())
RETURNING id;

-- Step 5: applies the transition and releases the lease. next_check_at is
-- rescheduled from completion unless a manual request moved it while the
-- check was running. updated_at is left alone: it tracks edits, not results.
-- name: ApplyCheckResult :exec
UPDATE monitors
SET state                = sqlc.arg('state'),
    consecutive_failures = sqlc.arg('consecutive_failures'),
    last_checked_at      = now(),
    last_status_code     = sqlc.narg('status_code'),
    last_latency_ms      = sqlc.arg('latency_ms')::int,
    next_check_at        = CASE
        WHEN next_check_at = sqlc.arg('claimed_next_check_at')::timestamptz
            THEN now() + make_interval(secs => interval_seconds)
        ELSE next_check_at  -- a manual request arrived mid-check; keep it
    END,
    lease_owner = NULL,
    lease_until = NULL
WHERE id = sqlc.arg('monitor_id');

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

-- Retention (Sprint 4 D8): deletes up to batch_size checks older than the
-- retention period, oldest first. SKIP LOCKED lets several workers run it at
-- once; incidents referencing a deleted check keep their row with the
-- reference set to NULL.
-- name: DeleteExpiredChecks :execrows
DELETE FROM monitor_checks
WHERE id IN (
    SELECT id FROM monitor_checks
    WHERE checked_at < now() - make_interval(days => sqlc.arg('retention_days')::int)
    ORDER BY checked_at
    LIMIT sqlc.arg('batch_size')
    FOR UPDATE SKIP LOCKED
);
