# PulseGo — Project Roadmap

> Portfolio-ready uptime monitoring platform in Go. Timeboxed to ~5–7 focused
> development days across **6 sprints**, each sprint mapping 1:1 to a
> milestone from the original spec. This document is the single source of
> truth for scope and order — Claude Code should never need to reference an
> external doc mid-sprint.

## 1. Project Purpose

Users register HTTP/HTTPS endpoints. Background workers periodically check
them, record response times, calculate availability, and open/resolve
incidents when repeated failures occur. Two independently runnable binaries
(`cmd/api`, `cmd/worker`) share one PostgreSQL-backed domain — no
microservices, no Redis/Kafka/Kubernetes in v1.

## 2. Tech Stack

Current stable Go · `net/http` · `go-chi/chi/v5` · PostgreSQL ·
`jackc/pgx/v5/pgxpool` · `sqlc` (no ORM — SQL stays visible) · Goose or
golang-migrate · `log/slog` (JSON) · Prometheus Go client · OpenTelemetry
(OTLP export) · Testcontainers-Go · Docker / Docker Compose · GitHub Actions ·
golangci-lint · OpenAPI 3.1.

## 3. Architecture

```
Client ────────────►  Go API Service (cmd/api)
                              │
                              ▼
                        PostgreSQL
                              ▲
                              │
                        Go Worker Pool (cmd/worker)
                              │
                              ▼
                     Monitored Websites
```

API manages monitors and exposes history. Workers claim scheduled monitors
from PostgreSQL (`FOR UPDATE SKIP LOCKED`) and execute checks concurrently.

Repository layout:

```
pulsego/
├── cmd/{api,worker}/main.go
├── internal/{config,domain,httpapi,monitor,checker,scheduler,repository,security,telemetry}/
├── db/{migrations,queries}/  sqlc.yaml
├── api/openapi.yaml
├── deployments/{Dockerfile,compose.yaml,prometheus.yml,otel-collector.yaml}
├── web/static/
├── .github/workflows/ci.yml
├── .env.example  Makefile  README.md
```

Interfaces live in the package that consumes them, not in a global
`interfaces` package. Domain logic stays separate from transport/DB code.

## 4. Non-Goals (v1)

Kubernetes · Kafka/NATS · Redis · microservices · SMS/email delivery ·
complex multi-tenant authorization · billing · mobile app · browser/synthetic
journey monitoring · regional worker clusters. (Webhook notifications may be
a later stretch feature — not in scope now.)

---

## 5. Sprint Plan

### Sprint 1 — Foundation
**Branch:** `feature/sprint-01-foundation`

Scope:
- Initialize Go module and the full directory layout above.
- `internal/config`: load + validate all env vars at startup (see §7);
  fail fast on invalid values.
- `log/slog` JSON structured logging wired into both binaries.
- `cmd/api` and `cmd/worker` entry points with graceful shutdown
  (`SHUTDOWN_TIMEOUT`, context cancellation on SIGTERM/SIGINT).
- Docker Compose: `postgres`, `api`, `worker` services; `docker compose up`
  must bring the stack up with no undocumented manual steps.
- Initial migration for the `monitors` table (full schema below) + `sqlc.yaml`
  configured and generating code.

**Definition of Done:**
- [ ] Module + layout match the repo tree above.
- [ ] Config validation rejects missing/invalid env vars with a clear error.
- [ ] Both binaries start, log structured JSON, and shut down gracefully.
- [ ] `docker compose up` starts postgres + api + worker.
- [ ] `monitors` migration applied; `sqlc generate` produces compiling code.

### Sprint 2 — Monitor API
**Branch:** `feature/sprint-02-monitor-api`

Scope:
- Full `monitors` table constraints + the `(enabled, next_check_at) WHERE
  deleted_at IS NULL` partial index.
- CRUD: `POST/GET/GET{id}/PATCH/DELETE /v1/monitors`. `DELETE` is a
  soft-delete (`deleted_at`).
- Bearer API-key middleware on `/v1/*` (constant-time comparison against
  `API_KEY`); `/healthz`, `/readyz` stay unauthenticated.
- RFC 7807 `application/problem+json` error responses (`type`, `title`,
  `status`, `detail`, `request_id`) for all validation/error paths.
- Request ID middleware.
- Reasonable min/max limits enforced on every configurable monitor field
  (interval, timeout, thresholds, status range).
- `api/openapi.yaml` covering the monitor CRUD surface.
- Handler + repository tests.

**Definition of Done:**
- [ ] All 5 CRUD endpoints implemented and tested, including soft-delete.
- [ ] Invalid input returns problem+json with correct fields.
- [ ] API key required and constant-time compared; health endpoints exempt.
- [ ] OpenAPI doc matches implemented endpoints.

### Sprint 3 — Scheduler and Checker
**Branch:** `feature/sprint-03-scheduler-checker`

Scope:
- `monitor_checks` table migration (see §6) + indexes.
- Claim transaction: select enabled/non-deleted/due monitors ignoring active
  leases, `FOR UPDATE SKIP LOCKED`, configurable batch size, assign
  `lease_owner`/`lease_until`, commit **before** any network call. Each
  worker instance gets a unique ID.
- Bounded-concurrency checker: configurable worker count, context
  cancellation, per-request timeouts, connection reuse via a configured
  `http.Transport`, graceful shutdown. No unbounded goroutine per monitor.
- Timeout handling + error classification (timeout vs. DNS vs. connection vs.
  unexpected status, etc.).
- `POST /v1/monitors/{id}/checks` (manual check → sets `next_check_at = now`,
  returns `202 Accepted`) and `GET /v1/monitors/{id}/checks` with
  cursor-based pagination.
- Lease duration must exceed the HTTP timeout so crashed workers' monitors
  become reclaimable.

**Definition of Done:**
- [ ] Concurrent `SKIP LOCKED` claims never double-assign a monitor
      (covered by an integration test with parallel claimers).
- [ ] Expired leases are reclaimable by a different worker.
- [ ] Checker respects bounded concurrency and per-request timeouts.
- [ ] Manual check + cursor-paginated check history endpoints work.

### Sprint 4 — State and Incidents
**Branch:** `feature/sprint-04-state-incidents`

Scope:
- `incidents` table migration + partial unique index (one unresolved
  incident per monitor).
- State machine: `unknown→up/degraded`, `up→degraded`, `degraded→up/down`,
  `down→up`, exactly as specified. Track `consecutive_failures`.
- On entering `down`: open an incident. On returning to `up`: resolve the
  open incident. All of {insert check row, update monitor state/failure
  count/last-result fields, open/close incident, set `next_check_at`, clear
  lease} happen in **one transaction**.
- `GET /v1/monitors/{id}/incidents`.
- `GET /v1/monitors/{id}/stats?window=24h|7d|30d` → uptime %, avg latency,
  p95 latency, successful checks, total checks.
- Retention cleanup job for old `monitor_checks` rows past
  `CHECK_RETENTION_DAYS`.

**Definition of Done:**
- [ ] State transition table covered by unit tests for every edge.
- [ ] Incident open/resolve is transactional and race-safe.
- [ ] Only one unresolved incident per monitor is possible (DB-enforced).
- [ ] Stats endpoint correct for all three windows.

### Sprint 5 — Production Hardening
**Branch:** `feature/sprint-05-hardening`

Scope — **SSRF protection** (mandatory, applies to every outbound check):
- Allow only `http`/`https`; reject embedded credentials, malformed
  hosts/URLs.
- Block loopback, private, link-local, multicast, unspecified, and IPv6 ULA
  addresses.
- Resolve DNS before connecting; use a custom dialer that connects to the
  *validated* IP (not a re-resolved one — avoid TOCTOU).
- Revalidate every redirect target; limit redirect count.
- Restrict ports to 80/443 by default; `ALLOW_PRIVATE_TARGETS=true` is
  dev-only — document the risk in README.
- Bounded, discarded response body read; never persist bodies or sensitive
  headers. User-Agent: `PulseGo/1.0`.

Scope — **Observability:**
- Prometheus metrics: `pulsego_http_requests_total`,
  `pulsego_http_request_duration_seconds`, `pulsego_monitor_checks_total`,
  `pulsego_monitor_check_duration_seconds`, `pulsego_monitor_state_total`,
  `pulsego_worker_claimed_monitors_total`, `pulsego_worker_active_checks`,
  `pulsego_worker_claim_errors_total`, `pulsego_scheduler_lag_seconds`,
  `pulsego_db_operation_duration_seconds`. No high-cardinality labels — never
  full URLs, monitor IDs, error messages, or request IDs as labels.
  `/metrics` ideally on `INTERNAL_HTTP_ADDR`.
- OpenTelemetry traces (OTLP export) on: incoming HTTP requests, PostgreSQL
  operations, worker claim cycles, outbound checks, incident transitions.
  Must be togglable via `OTEL_ENABLED` and never block startup when off.
- `/healthz` (no DB query) vs `/readyz` (must verify PostgreSQL reachable).

Scope — **Tests, Docker, CI:**
- Full integration suite (Testcontainers-Go): migrations, CRUD, worker
  claims, concurrent `SKIP LOCKED`, lease expiration/reclaim, check-result
  transaction, incident uniqueness, cursor pagination, soft delete, stats.
- Multi-stage Dockerfile: `CGO_ENABLED=0`, non-root user, CA certs included,
  minimal final image, Docker health checks. No auto-migration on every
  replica startup.
- CI (`.github/workflows/ci.yml`): `gofmt` check, `go vet ./...`,
  `golangci-lint run`, `go test -race ./...`, `govulncheck ./...`,
  `go build ./cmd/api`, `go build ./cmd/worker`, `docker build`.

**Definition of Done:**
- [ ] SSRF test suite covers loopback/private/link-local/redirect-to-private
      cases and all pass.
- [ ] All 10 metrics exposed with bounded cardinality.
- [ ] Traces exported via OTLP; app starts fine with `OTEL_ENABLED=false`.
- [ ] `go test -race ./...` and the full CI pipeline are green.
- [ ] Docker image builds, runs as non-root, passes its health check.

### Sprint 6 — Portfolio Polish
**Branch:** `feature/sprint-06-polish`

Scope:
- Small read-only dashboard (Go templates or static HTML + minimal JS):
  current states, uptime %, p95 latency, recent incidents.
- Architecture diagram, screenshots or a short demo GIF.
- README covering: project explanation, features, architecture diagram,
  tech choices, local setup, env config, API examples, worker/lease
  explanation, SSRF explanation, observability instructions, test commands,
  deployment notes, known limitations, roadmap, screenshots, CV-ready
  description, and a **"Key Engineering Decisions"** section explaining: why
  Postgres is used as both storage and scheduler, why `SKIP LOCKED` enables
  multiple workers, why Redis was intentionally omitted, how leases recover
  abandoned jobs, how SSRF protection works, how metric cardinality is
  controlled.
- Example curl commands + seeded demo data.
- Do **not** build a separate Node frontend unless the backend is fully done
  and time remains.

**Definition of Done:**
- [ ] Dashboard renders live state from the API.
- [ ] README is sufficient for another developer to run and understand the
      project unaided.
- [ ] Project-level Definition of Done (§9) fully satisfied.

---

## 6. Database Model

**monitors** — `id UUID PK`, `name VARCHAR(120)`, `url TEXT`,
`interval_seconds INT`, `timeout_ms INT`, `expected_status_min/max SMALLINT`,
`failure_threshold SMALLINT`, `enabled BOOLEAN`, `state VARCHAR`,
`consecutive_failures INT`, `last_checked_at/last_status_code/last_latency_ms`
nullable, `next_check_at TIMESTAMPTZ`, `lease_owner TEXT` nullable,
`lease_until TIMESTAMPTZ` nullable, `created_at/updated_at`, `deleted_at`
nullable. Constraints on status ranges, intervals, timeouts, thresholds,
valid states. Index: `(enabled, next_check_at) WHERE deleted_at IS NULL`.

**monitor_checks** — `id BIGINT identity PK`, `monitor_id UUID FK`,
`success BOOLEAN`, `status_code INT` nullable, `latency_ms INT`,
`error_kind VARCHAR` nullable, `error_message TEXT` nullable (length-capped,
never a response body), `checked_at TIMESTAMPTZ`. Indexes:
`(monitor_id, checked_at DESC, id DESC)`, `(checked_at)`.

**incidents** — `id UUID PK`, `monitor_id UUID FK`, `started_at`,
`resolved_at` nullable, `opening_check_id`/`closing_check_id` nullable,
`cause TEXT` nullable, `created_at`. Partial unique index: one unresolved
incident per monitor.

Defaults: interval 60s · timeout 5s · expected status 200–399 · failure
threshold 3. Apply sane min/max limits to every configurable value.

## 7. Configuration (env vars)

`SERVICE_NAME`, `ENVIRONMENT`, `HTTP_ADDR`, `INTERNAL_HTTP_ADDR`,
`DATABASE_URL`, `API_KEY`, `LOG_LEVEL`, `WORKER_CONCURRENCY`,
`WORKER_BATCH_SIZE`, `WORKER_POLL_INTERVAL`, `WORKER_LEASE_DURATION`,
`DEFAULT_CHECK_INTERVAL`, `DEFAULT_CHECK_TIMEOUT`, `ALLOW_PRIVATE_TARGETS`,
`CHECK_RETENTION_DAYS`, `OTEL_ENABLED`, `OTEL_EXPORTER_OTLP_ENDPOINT`,
`SHUTDOWN_TIMEOUT`. Validate all at startup. Provide `.env.example`; never
commit real secrets.

## 8. Makefile targets

`bootstrap`, `up`, `down`, `migrate-up`, `migrate-down`, `sqlc`, `run-api`,
`run-worker`, `test`, `test-integration`, `lint`, `build`, `docker-build`.

## 9. Definition of Done (project-level)

`docker compose up` starts postgres/api/worker · a monitor can be created via
API · a worker claims and checks it automatically · results/latency history
persisted · repeated failures open an incident · recovery resolves it ·
multiple workers never double-process a leased monitor · expired leases are
reclaimable · private/unsafe targets rejected by default · health/readiness/
metrics endpoints work · API documented via OpenAPI · unit + integration
tests pass · race detector passes · CI builds both binaries + the Docker
image · no secrets committed · README is sufficient standalone.

## 10. CV Description

Built a production-oriented uptime monitoring platform in Go using
PostgreSQL, type-safe SQL, horizontally scalable background workers,
lease-based job scheduling, OpenTelemetry, Prometheus, integration tests,
Docker, and CI/CD. Implemented incident lifecycle management, graceful
shutdown, and SSRF protection for user-supplied monitoring targets.
