-- Aggregates one monitor's checks over (now() - window, now()]: the start is
-- exclusive, the end inclusive. Latency figures cover successful checks only,
-- because a failed check's latency is its timeout, not a response time, so
-- they are NULL when no check in the window succeeded. percentile_disc returns
-- an observed value, so p95 is whole milliseconds. Backed by
-- monitor_checks_history_idx.
--
-- The aggregate is computed once, in the CTE (referenced twice, so PostgreSQL
-- materializes it). Reading the latency columns through a LEFT JOIN is only
-- for sqlc: it infers every aggregate as NOT NULL, and the outer join is what
-- makes it generate nullable fields for the two that can be NULL.
-- name: GetMonitorStats :one
WITH window_stats AS (
    SELECT count(*)                                           AS total_checks,
           count(*) FILTER (WHERE success)                    AS successful_checks,
           (avg(latency_ms) FILTER (WHERE success))::float8   AS avg_latency_ms,
           (percentile_disc(0.95) WITHIN GROUP (ORDER BY latency_ms)
               FILTER (WHERE success))::int                   AS p95_latency_ms
    FROM monitor_checks
    WHERE monitor_id = sqlc.arg('monitor_id')
      AND checked_at > now() - make_interval(secs => sqlc.arg('window_seconds')::float8)
)
SELECT counts.total_checks,
       counts.successful_checks,
       latency.avg_latency_ms,
       latency.p95_latency_ms,
       now()::timestamptz AS window_end
FROM window_stats AS counts
LEFT JOIN window_stats AS latency ON true;
