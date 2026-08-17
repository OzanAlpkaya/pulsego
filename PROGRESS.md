# PulseGo — Progress

Last updated: 2026-08-17
Current sprint: **Sprint 2 — Monitor API** (not started)

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
Status: **Not Started**
Branch: `feature/sprint-02-monitor-api`

Definition of Done:
- [ ] All 5 CRUD endpoints implemented and tested, including soft-delete
- [ ] Invalid input returns problem+json with correct fields
- [ ] API key required and constant-time compared; health endpoints exempt
- [ ] OpenAPI doc matches implemented endpoints

Notes:
-

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
