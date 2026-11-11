-- Opens an incident at the opening check (Sprint 4 D3). The casts make sqlc
-- generate non-pointer parameters: an incident is always opened with a check
-- and a cause. started_at is the
-- transaction's now(), identical to that check's checked_at. ON CONFLICT on
-- the partial unique index makes a second open a no-op (0 rows) rather than
-- an error, which would wedge the monitor: an already-open incident is only
-- possible after a hand edit of state or incidents.
-- name: OpenIncident :execrows
INSERT INTO incidents (monitor_id, started_at, opening_check_id, cause)
VALUES (sqlc.arg('monitor_id'), now(), sqlc.arg('opening_check_id')::bigint,
        sqlc.arg('cause')::text)
ON CONFLICT (monitor_id) WHERE resolved_at IS NULL DO NOTHING;

-- Resolves the monitor's open incident at the closing check. 0 rows (nothing
-- open) is not an error.
-- name: ResolveIncident :execrows
UPDATE incidents
SET resolved_at = now(), closing_check_id = sqlc.arg('closing_check_id')::bigint
WHERE monitor_id = sqlc.arg('monitor_id')
  AND resolved_at IS NULL;

-- Keyset pagination over (started_at, id), newest first, backed by
-- incidents_history_idx. As with ListChecks, cursor_id is ignored on the
-- first page, where cursor_started_at is NULL.
-- name: ListIncidents :many
SELECT * FROM incidents
WHERE monitor_id = sqlc.arg('monitor_id')
  AND (sqlc.narg('cursor_started_at')::timestamptz IS NULL
       OR (started_at, id) < (sqlc.narg('cursor_started_at')::timestamptz,
                              sqlc.arg('cursor_id')::uuid))
ORDER BY started_at DESC, id DESC
LIMIT sqlc.arg('page_limit');
