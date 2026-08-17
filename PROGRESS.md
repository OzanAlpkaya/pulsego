# PulseGo — Progress

Last updated: 2026-08-17
Current sprint: **Sprint 3 — Scheduler and Checker** (not started)

---

## Sprint 1 — Foundation
Status: **Done** (2026-08-17, awaiting squash-merge approval)
Branch: `feature/sprint-01-foundation`

Definition of Done:
- [x] Module + layout match the repo tree in ROADMAP.md
- [x] Config validation rejects missing/invalid env vars with a clear error
- [x] Both binaries start, log structured JSON, and shut down gracefully
- [x] `docker compose up` starts postgres + api + worker
- [x] `monitors` migration applied; `sqlc generate` produces compiling code

Notes:
- Decisions taken at sprint start (D1–D7 of the approved plan):
  - **goose** over golang-migrate, with migrations embedded via `db/embed.go`.
  - The **full** `monitors` schema — all CHECK constraints and the partial
    index — landed in migration `00001`, so Sprint 2's schema line is already
    satisfied.
  - `db/queries/monitors.sql` is deliberately limited to `GetMonitor` and
    `ListMonitors`; the rest of the CRUD surface belongs to Sprint 2.
  - Migrations run as a **one-shot `cmd/migrate` job** in Compose
    (`service_completed_successfully`), never on service startup. ROADMAP §3
    updated to record the third binary.
  - ROADMAP gained a **§5a Testing Strategy** section; it was referenced by the
    `/sprint-start` skill but did not exist.
  - `/healthz` only this sprint (GET and HEAD, no DB query); `/readyz` is
    Sprint 5.
  - `sqlc`, `golangci-lint`, and `govulncheck` are pinned as go.mod `tool`
    directives, so a bare checkout needs only Go and Docker.
- Two defects found and fixed while verifying, both by the gate rather than by
  inspection: chi's `middleware.RealIP` is deprecated as spoofable (removed),
  and the Compose health check used `wget --spider`, which sends HEAD and got
  a 405 — it now performs a real GET, and `/healthz` also answers HEAD.
- Compose publishes PostgreSQL on host port **5434**, not 5432, to avoid
  colliding with a local database; override with `POSTGRES_PORT`.
- **Gate defect fixed during `/sprint-done`:** the specified build step
  `go build ./cmd/api` cannot succeed in this repository — with no `-o` it
  writes an executable named `api` into the root, which collides with the
  `api/` directory that ROADMAP §3 reserves for `openapi.yaml`. The code
  itself compiled fine. ROADMAP §5 (CI spec) and the `/sprint-done` skill now
  specify `go build -o bin/<name> ./cmd/<name>`, matching the Makefile. Left
  unfixed this would have broken CI as soon as Sprint 2 adds `openapi.yaml`.
- Evidence for DoD item 1: every package of the ROADMAP §3 tree is present and
  tracked. Three directories exist but are still empty, and ROADMAP assigns
  their contents to later sprints — `api/openapi.yaml` (Sprint 2),
  `.github/workflows/ci.yml` and `deployments/{prometheus.yml,
  otel-collector.yaml}` (Sprint 5), `web/static/` (Sprint 6).
- Verification beyond the required gate: the integration suite
  (`make test-integration`, 24s) passed against real PostgreSQL — migrations
  up and down, the partial index, and all 14 `monitors` constraints — and the
  Compose stack was brought up end to end, with `api` reporting `healthy`,
  both services running as uid 10001, and SIGTERM draining in under a second
  with exit code 0.

---

## Sprint 2 — Monitor API
Status: **Done** (2026-08-17, awaiting squash-merge approval)
Branch: `feature/sprint-02-monitor-api`

Definition of Done:
- [x] All 5 CRUD endpoints implemented and tested, including soft-delete
- [x] Invalid input returns problem+json with correct fields
- [x] API key required and constant-time compared; health endpoints exempt
- [x] OpenAPI doc matches implemented endpoints

Notes:
- Decisions taken at sprint start (D1–D7 of the approved plan):
  - **Cursor pagination** for `GET /v1/monitors`, keyed on
    `(created_at DESC, id DESC)`, sharing one `cursor.go` with Sprint 3's check
    history rather than shipping two pagination styles.
  - **422** for well-formed bodies that fail field validation, **400** for
    unparseable ones (bad JSON, unknown field, bad UUID or cursor).
  - Shortening an interval sets
    `next_check_at = LEAST(next_check_at, now() + new_interval)`, so a tightened
    schedule applies promptly but an unrelated edit never delays a due check.
  - API key compared as **SHA-256 digests**, because
    `subtle.ConstantTimeCompare` returns early on a length mismatch and would
    otherwise leak the configured key's length.
  - Sprint 2 validates URL **syntax only** (scheme, host, no credentials, no
    fragment, length). Address-range and DNS checks stay in Sprint 5's
    `internal/security`; doing them in a handler would be slow and a TOCTOU trap.
  - Problem `type` values are **URNs**, not URLs to docs that do not exist.
  - **No uniqueness constraint on url** — monitoring one URL twice with
    different intervals is legitimate.
- Sprint 1 had already delivered the full constraint set and the partial index
  (decision D2 of that sprint), so this sprint's only migration is `00002`,
  adding `monitors_url_length` — the one configurable field that still had no
  bound.
- `TestOpenAPIMatchesRouter` walks the live chi router and compares it with
  `api/openapi.yaml`. It is what makes the "OpenAPI doc matches implemented
  endpoints" item enforced rather than merely asserted: adding a route without
  documenting it now fails the build.
- Integration tests moved to a **single shared PostgreSQL container** via
  `TestMain`, with truncation between tests. One container per test would have
  meant roughly five minutes for this suite; it now runs in about 30 seconds.
- Two defects caught by the gate: `goose.Down` only reverses one migration, so
  the Sprint 1 down-migration test silently stopped proving anything once a
  second migration existed (now `DownTo(0)`); and gosec flagged five integer
  conversions, four of which were fixed properly by giving `internal/domain`
  int32 constants in the units the columns actually store.
- Evidence recorded at close-out (2026-08-17), all against commit `7762e92`:
  - **CRUD + soft delete:** exercised against the running Compose stack —
    `POST` 201 with `Location`, `GET` 200, `PATCH` 200 leaving untouched fields
    alone, `DELETE` 204. After deletion `GET` and a second `DELETE` both return
    404, while PostgreSQL still holds the row: 6 rows total, 5 live.
    12 integration tests cover the same ground plus lease clearing and keyset
    pagination over rows sharing a `created_at`.
  - **problem+json:** a live invalid create returned 422 with
    `content-type: application/problem+json` and all five rejected fields in one
    `errors` array, alongside `type`, `title`, `status`, `detail`, `request_id`.
  - **API key:** 10 rejection cases pass, including a wrong key of exactly the
    same length; `/healthz` answers 200 with no credential, and a separate test
    asserts no `/v1` route reaches the service unauthenticated.
  - **OpenAPI parity:** `TestOpenAPIMatchesRouter` compares 6 documented
    operations against 6 walked from the live chi router; confirmed non-vacuous
    by printing both sets.

---

## Sprint 3 — Scheduler and Checker
Status: **Not Started**
Branch: `feature/sprint-03-scheduler-checker`

Definition of Done:
- [ ] Concurrent `SKIP LOCKED` claims never double-assign a monitor
- [ ] Expired leases are reclaimable by a different worker
- [ ] Checker respects bounded concurrency and per-request timeouts
- [ ] Manual check + cursor-paginated check history endpoints work

Notes:
-

---

## Sprint 4 — State and Incidents
Status: **Not Started**
Branch: `feature/sprint-04-state-incidents`

Definition of Done:
- [ ] State transition table covered by unit tests for every edge
- [ ] Incident open/resolve is transactional and race-safe
- [ ] Only one unresolved incident per monitor is possible (DB-enforced)
- [ ] Stats endpoint correct for all three windows (24h/7d/30d)

Notes:
-

---

## Sprint 5 — Production Hardening
Status: **Not Started**
Branch: `feature/sprint-05-hardening`

Definition of Done:
- [ ] SSRF test suite covers loopback/private/link-local/redirect-to-private
      cases and all pass
- [ ] All 10 Prometheus metrics exposed with bounded cardinality
- [ ] Traces exported via OTLP; app starts fine with `OTEL_ENABLED=false`
- [ ] `go test -race ./...` and the full CI pipeline are green
- [ ] Docker image builds, runs as non-root, passes its health check

Notes:
-

---

## Sprint 6 — Portfolio Polish
Status: **Not Started**
Branch: `feature/sprint-06-polish`

Definition of Done:
- [ ] Dashboard renders live state from the API
- [ ] README is sufficient for another developer to run and understand the
      project unaided
- [ ] Project-level Definition of Done (ROADMAP.md §9) fully satisfied

Notes:
-
