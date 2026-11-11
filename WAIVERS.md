# PulseGo — Waivers

Each waiver records a deviation from the letter of a Definition of Done item
or a gate step: what, why, and how to undo it.

---

## W1 — Sprint 5: "All 10 metrics exposed" means across the deployment

Recorded 2026-11-11, decided by the tech lead during the Sprint 5 close-out
(PROGRESS.md, Sprint 5 Notes, D14).

**What.** The Sprint 5 DoD item "All 10 Prometheus metrics exposed with
bounded cardinality" is read as: all 10 across the deployment, each emitted
once it has been observed. It is not read as "all 10 names on every
`/metrics` endpoint from the first scrape". In practice:

- The api's `/metrics` shows all 10 after its first real `/v1` request.
- The worker's `/metrics` shows 8: metrics 3–10, after its first claim
  cycle. It never shows `pulsego_http_requests_total` or
  `pulsego_http_request_duration_seconds`.
- On a freshly started api, `pulsego_db_operation_duration_seconds` is
  absent until the first query. The `/readyz` ping is not a query.

**Why.**

- Both binaries register all 10 metrics (`telemetry.NewMetrics`). But
  Prometheus writes out a label vector only once it has at least one series,
  and a series exists only after its first observation. That is standard
  client behaviour.
- The HTTP metrics come from the `Observe` middleware. It runs on the api's
  public router only (D6), and the worker has none.
- Pre-creating series for label values that never occur would be misleading.
  For example, HTTP routes on the worker would show traffic patterns that
  cannot exist.

Proof:

- `TestMetricsAllTenRegistered` shows all 10 are exposed once observed.
- The gate's manual exposition lists in PROGRESS.md show what each binary
  emits in practice.

**How to undo.** In `telemetry.NewMetrics` (`preinitialise`), pre-create
label children for the HTTP metrics and the DB operation metric. Then every
binary exposes all 10 names from the first scrape. For example:

- every registered route × normalised method × common code;
- every sqlc query name × `ok`/`error`.

Then tighten the gate step back to "all 10 names on both binaries' `/metrics`"
and remove this waiver.
