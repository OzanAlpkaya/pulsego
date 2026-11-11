# PulseGo

[![CI](https://github.com/OzanAlpkaya/pulsego/actions/workflows/ci.yml/badge.svg)](https://github.com/OzanAlpkaya/pulsego/actions/workflows/ci.yml)

Uptime monitoring platform in Go. Register HTTP/HTTPS endpoints; background
workers check them on a schedule, record latency and availability, and open and
resolve incidents when failures persist.

Two binaries share one PostgreSQL-backed domain — `cmd/api` serves the REST API
and a read-only dashboard, `cmd/worker` runs the checks. PostgreSQL is both the
store and the scheduler: workers claim due monitors with
`FOR UPDATE SKIP LOCKED` and hold time-bounded leases, so several workers can
run without double-processing and a crashed worker's monitors become
reclaimable on their own.

> **Status: all six sprints of `ROADMAP.md` are complete.** PulseGo is a
> finished v1. `PROGRESS.md` records what each sprint delivered and why.

![Demo: a monitor goes down, an incident opens, the monitor recovers and the incident resolves](docs/demo.gif)

## Contents

- [Features](#features)
- [Architecture](#architecture)
- [Tech choices](#tech-choices)
- [Quick start with Docker Compose](#quick-start-with-docker-compose)
- [Running locally without Compose](#running-locally-without-compose)
- [Configuration](#configuration)
- [API](#api)
- [Monitor state and incidents](#monitor-state-and-incidents)
- [Dashboard](#dashboard)
- [Demo data](#demo-data)
- [Workers and leases](#workers-and-leases)
- [SSRF protection](#ssrf-protection)
- [Observability](#observability)
- [Testing](#testing)
- [CI](#ci)
- [Deployment notes](#deployment-notes)
- [Layout](#layout)
- [Database and migrations](#database-and-migrations)
- [Key Engineering Decisions](#key-engineering-decisions)
- [Known limitations](#known-limitations)
- [Roadmap](#roadmap)
- [Screenshots](#screenshots)
- [CV-ready description](#cv-ready-description)

## Features

- **Monitor CRUD** over a REST API (`/v1/monitors`) with cursor pagination,
  partial updates, soft delete and an OpenAPI 3.1 contract
  (`api/openapi.yaml`) that a test keeps in sync with the router.
- **Scheduled HTTP(S) checks** with per-monitor interval (10 s – 24 h),
  timeout (100 ms – 60 s), expected status range and failure threshold.
- **Check history** with status code, latency and a classified error kind
  (`timeout`, `dns`, `connection`, `tls`, `redirect`, `unexpected_status`,
  `blocked`, `unknown`). Response bodies are never stored.
- **State machine and incidents:** `unknown → up / degraded → down`. An
  incident opens when a monitor goes down and resolves when it recovers.
- **Stats:** uptime percentage, average and p95 latency over 24 h, 7 d or
  30 d.
- **Horizontally scalable workers** that claim work with
  `FOR UPDATE SKIP LOCKED` and recover abandoned work through leases.
- **SSRF protection** for user-supplied URLs: at the API, at dial time, and on
  every redirect.
- **Retention:** check history older than `CHECK_RETENTION_DAYS` is deleted
  in batches.
- **Read-only dashboard** served by the api at `/`.
- **Observability:** JSON logs (`log/slog`), Prometheus metrics with bounded
  label cardinality, optional OpenTelemetry traces, and liveness and
  readiness probes.
- **Operations:** config validated at startup, graceful shutdown, a one-shot
  migration job, non-root container images, and CI on every push.

## Architecture

```mermaid
flowchart LR
    browser["Browser dashboard"]
    client["API client, e.g. curl"]

    subgraph apisvc["api - cmd/api"]
        apipublic["Public listener :8080<br/>dashboard at / and REST API at /v1"]
        apiinternal["Internal listener :9090<br/>/healthz, /readyz, /metrics"]
    end

    migrate["migrate - one-shot job<br/>goose migrations"]
    pg[("PostgreSQL<br/>storage and scheduler")]

    subgraph workersvc["worker - cmd/worker, one or more replicas"]
        pool["Worker pool<br/>claim with FOR UPDATE SKIP LOCKED<br/>time-bounded lease"]
        dialer["SSRF-guarded dialer"]
        workerinternal["Internal listener :9090<br/>/healthz, /readyz, /metrics"]
    end

    targets["Monitored targets<br/>public HTTP and HTTPS endpoints"]

    subgraph obs["Optional profile: observability"]
        collector["otel-collector"]
        jaeger["Jaeger"]
        prom["Prometheus"]
        grafana["Grafana"]
    end

    browser -->|"HTTP, same origin"| apipublic
    client -->|"Bearer API key"| apipublic
    apipublic -->|"SQL via sqlc and pgx"| pg
    migrate -->|"applies schema, then exits"| pg
    pool -->|"claim, record result, release lease"| pg
    pool --> dialer
    dialer -->|"GET, validated IPs only"| targets
    prom -.->|"scrape"| apiinternal
    prom -.->|"scrape"| workerinternal
    grafana -.-> prom
    apipublic -.->|"OTLP traces"| collector
    pool -.->|"OTLP traces"| collector
    collector -.-> jaeger
```

- The **api** is stateless. Its public listener (`HTTP_ADDR`, default `:8080`)
  serves the dashboard and `/v1`; its internal listener (`INTERNAL_HTTP_ADDR`,
  default `:9090`) serves the probes and metrics and should never be exposed
  publicly.
- The **worker** has no public listener, only the internal one.
- **migrate** applies the embedded migrations and exits. Compose starts the
  api and worker only after it succeeds.
- Dotted lines are optional: traces flow only with `OTEL_ENABLED=true`, and
  the four observability services start only with
  `--profile observability`.

## Tech choices

| Area | Choice | Why |
|---|---|---|
| Language | Go (module requires 1.26.5+; CI builds with the latest 1.27.x) | One static binary per service, cheap concurrency for checks. |
| HTTP | `net/http` + `go-chi/chi/v5` | Standard handlers; chi adds route patterns, which also serve as bounded metric labels. |
| Database | PostgreSQL 17 + `jackc/pgx/v5` (`pgxpool`) | One dependency for storage *and* scheduling. |
| SQL | `sqlc` | SQL stays visible in `db/queries`; no ORM. |
| Migrations | `goose`, embedded in `cmd/migrate` | Plain SQL files, run as a one-shot job. |
| Logs | `log/slog` JSON | Standard library, structured. |
| Metrics | Prometheus Go client | Each binary has its own registry. |
| Traces | OpenTelemetry, OTLP/HTTP export | Vendor-neutral and off by default. |
| Tests | `testing`, `httptest`, Testcontainers-Go | Integration tests use a real PostgreSQL, never a mock. |
| Packaging | Docker multi-stage build (Alpine runtime), Docker Compose | One Dockerfile builds all three images. |
| CI | GitHub Actions, golangci-lint, govulncheck | Runs on every push and pull request. |
| API contract | OpenAPI 3.1 (`api/openapi.yaml`) | A test checks it against the live router. |

Developer tools (`sqlc`, `golangci-lint`, `govulncheck`) are pinned as go.mod
`tool` directives. A bare checkout needs only Go and Docker.

## Quick start with Docker Compose

Requires Go 1.26.5+ (for `make bootstrap`) and Docker with Compose v2.

```bash
make bootstrap   # download modules, verify tooling, create .env
make up          # postgres + migrations + api + worker
curl localhost:8080/healthz
# dashboard: http://localhost:8080/  (API key: local-development-api-key)
make down        # stop the stack and delete its volume
```

`make up` runs `docker compose -f deployments/compose.yaml up --build -d`.
Compose itself does not read the repository's `.env`: it fills each app
setting in `deployments/compose.yaml` from the shell environment, or from its
own default (`${VAR:-default}`). The Makefile supplies that environment. It
reads `.env` (created by `make bootstrap` from `.env.example`) and exports
every variable in it, so `API_KEY`, `ENVIRONMENT`, the worker settings and
the rest come from `.env`.

A value you set yourself wins over its `.env` line, whether it is already in
the environment or given as a make argument:

```bash
LOG_LEVEL=debug make up    # environment beats .env
make up LOG_LEVEL=debug    # so does a make argument
```

This holds for every variable and every target, not only `make up`.
`DATABASE_URL`, `HTTP_ADDR` and `INTERNAL_HTTP_ADDR` are fixed in
`deployments/compose.yaml`, so neither `.env` nor the environment changes
them under Compose. The default API key is `local-development-api-key`; for
anything beyond a laptop, set `API_KEY` in `.env` (`openssl rand -hex 32`).

Compose publishes these host ports:

| Port | What | Override |
|---|---|---|
| `8080` | api public listener: dashboard and `/v1` | `API_PORT` |
| `9090` | api internal listener: `/healthz`, `/readyz`, `/metrics` | `API_INTERNAL_PORT` |
| `5434` | PostgreSQL (not 5432, so it does not collide with a local database) | `POSTGRES_PORT` |

The worker's internal port is not published; Prometheus reaches it inside the
Compose network. `make logs` follows the api and worker logs.

To run more than one worker, scale the service:

```bash
docker compose -f deployments/compose.yaml up -d --scale worker=3
```

Plain `docker compose` commands like this one do not go through the
Makefile, so they do not get the variables it exports from `.env`. Export
them yourself (`set -a; . ./.env; set +a`) if you changed any.

## Running locally without Compose

You need Go and a PostgreSQL server (the project is developed and tested on
PostgreSQL 17). The Makefile reads `.env` and exports
every variable in it to `make run-api`, `make run-worker` and the
`migrate-*` targets. As with `make up`, a variable already in the environment
or passed as a make argument wins over `.env`.

1. **Get a database.** Any PostgreSQL works. To borrow only the database from
   Compose: `docker compose -f deployments/compose.yaml up -d postgres`, which
   listens on `localhost:5434` with user, password and database `pulsego`.

2. **Create `.env`** and point `DATABASE_URL` at that database:

   ```bash
   cp .env.example .env
   # edit .env, for example:
   # DATABASE_URL=postgres://pulsego:pulsego@localhost:5434/pulsego?sslmode=disable
   ```

   `.env.example` defaults to `localhost:5432`, a database you run yourself.
   Its `API_KEY` (`local-development-api-key`) is fine for local use only.

3. **Migrate, then start both services**, each in its own shell:

   ```bash
   make migrate-up
   make run-api      # public :8080, internal :9090
   make run-worker   # internal :9091
   ```

   `make run-worker` moves the worker's internal listener to `:9091`, so it
   does not collide with the api's `:9090`. If `8080` or `9090` are taken on
   your machine, change `HTTP_ADDR` and `INTERNAL_HTTP_ADDR` in `.env`, for
   example `HTTP_ADDR=127.0.0.1:18080` and
   `INTERNAL_HTTP_ADDR=127.0.0.1:19090`.

4. **Check that it works:**

   ```bash
   curl localhost:8080/healthz    # {"status":"ok"}
   curl localhost:9090/readyz     # {"status":"ready"}  (api, pings PostgreSQL)
   curl localhost:9091/readyz     # {"status":"ready"}  (worker)
   ```

   Then create a monitor (see [API](#api)) or open http://localhost:8080/.

`make migrate-status` shows the applied migrations. Stop the services with
Ctrl-C; both drain in-flight work within `SHUTDOWN_TIMEOUT`.

## Configuration

Every variable is listed in `.env.example` and validated at startup by
`internal/config`. An invalid configuration stops the process with every
problem listed at once:

```
api: config: API_KEY must be at least 16 characters
config: DATABASE_URL is required
config: WORKER_CONCURRENCY must be between 1 and 256, got 999
```

| Variable | Default | Allowed values | Purpose |
|---|---|---|---|
| `SERVICE_NAME` | `pulsego` | any | `service` field in logs; trace service names become `<name>-api` and `<name>-worker`. |
| `ENVIRONMENT` | `development` | `development`, `staging`, `production` | Gates `ALLOW_PRIVATE_TARGETS`. |
| `LOG_LEVEL` | `info` | `debug`, `info`, `warn`, `error` | Minimum log level. |
| `HTTP_ADDR` | `:8080` | listen address, must differ from `INTERNAL_HTTP_ADDR` | api public listener (dashboard and `/v1`). The worker ignores it. |
| `INTERNAL_HTTP_ADDR` | `:9090` | listen address | `/healthz`, `/readyz`, `/metrics` for api and worker. |
| `DATABASE_URL` | — (required) | `postgres://` or `postgresql://` URL | PostgreSQL connection. |
| `API_KEY` | — (required) | at least 16 characters | Bearer token for `/v1`. Generate one with `openssl rand -hex 32`. |
| `WORKER_CONCURRENCY` | `10` | 1–256 | Checks running at once per worker process. |
| `WORKER_BATCH_SIZE` | `20` | 1–500 | Most monitors claimed per poll. |
| `WORKER_POLL_INTERVAL` | `5s` | 1s–5m | Time between claims. |
| `WORKER_LEASE_DURATION` | `60s` | 5s–10m, and greater than `DEFAULT_CHECK_TIMEOUT` | How long a claim is held. |
| `DEFAULT_CHECK_INTERVAL` | `60s` | 10s–24h | Interval for new monitors that do not set one. |
| `DEFAULT_CHECK_TIMEOUT` | `5s` | 100ms–60s | Timeout for new monitors that do not set one. |
| `CHECK_RETENTION_DAYS` | `30` | 1–365 | Days of check history to keep. |
| `ALLOW_PRIVATE_TARGETS` | `false` | `true` only with `ENVIRONMENT=development` | **Development only.** Turns off the SSRF address checks. |
| `OTEL_ENABLED` | `false` | `true`, `false` | Export traces over OTLP/HTTP. |
| `OTEL_EXPORTER_OTLP_ENDPOINT` | empty | full `http(s)://` URL; required when `OTEL_ENABLED=true` | Collector base URL, for example `http://localhost:4318`. |
| `SHUTDOWN_TIMEOUT` | `15s` | 1s–2m | Budget for draining in-flight work after SIGTERM/SIGINT. |

`PULSEGO_TEST_DATABASE_URL` is read only by the integration tests, never by
the application (see [Testing](#testing)).

Two rules are worth calling out:

- `WORKER_LEASE_DURATION` must exceed `DEFAULT_CHECK_TIMEOUT`, otherwise a
  second worker could reclaim a monitor while the first is still waiting on the
  network.
- `ALLOW_PRIVATE_TARGETS=true` is refused unless `ENVIRONMENT=development`. It
  lets monitors point at loopback and private addresses, which turns the checker
  into an SSRF gadget against your own network. See
  [SSRF protection](#ssrf-protection).

## API

Every `/v1` endpoint needs the API key as `Authorization: Bearer <key>`;
`/healthz`, `/readyz` and the dashboard files deliberately do not. The full
contract is `api/openapi.yaml`, and a test walks the router against that
document so the two cannot drift apart.

A monitor has these writable fields. Only `name` and `url` are required:

| Field | Default | Range |
|---|---|---|
| `name` | — | 1–120 characters |
| `url` | — | `http`/`https`, at most 2048 bytes, must pass the [SSRF policy](#ssrf-protection) |
| `interval_seconds` | `DEFAULT_CHECK_INTERVAL` (60) | 10–86400 |
| `timeout_ms` | `DEFAULT_CHECK_TIMEOUT` (5000) | 100–60000 |
| `expected_status_min` / `expected_status_max` | 200 / 399 | 100–599 |
| `failure_threshold` | 3 | 1–10 |
| `enabled` | `true` | — |

`timeout_ms` must be shorter than the interval, and `expected_status_min`
must not exceed `expected_status_max`.

List endpoints take `limit` (default 20, at most 100) and return
`next_cursor` while there are more pages.

```bash
KEY=local-development-api-key

# Create — only name and url are required
curl -X POST localhost:8080/v1/monitors -H "Authorization: Bearer $KEY" \
  -d '{"name":"Example","url":"https://example.com"}'

# List, newest first; page with the returned cursor
curl "localhost:8080/v1/monitors?limit=20" -H "Authorization: Bearer $KEY"
curl "localhost:8080/v1/monitors?limit=20&cursor=<next_cursor>" -H "Authorization: Bearer $KEY"

curl localhost:8080/v1/monitors/<id> -H "Authorization: Bearer $KEY"

# Partial update — omitted fields are left alone
curl -X PATCH localhost:8080/v1/monitors/<id> -H "Authorization: Bearer $KEY" \
  -d '{"enabled":false}'

# Request a check now (202; 409 if the monitor is disabled)
curl -X POST localhost:8080/v1/monitors/<id>/checks -H "Authorization: Bearer $KEY"

# Check history, newest first; paged like the monitor list
curl "localhost:8080/v1/monitors/<id>/checks?limit=20" -H "Authorization: Bearer $KEY"

# Incident history, newest first; paged like the monitor list
curl "localhost:8080/v1/monitors/<id>/incidents?limit=20" -H "Authorization: Bearer $KEY"

# Uptime and latency over a window: 24h (the default), 7d or 30d
curl "localhost:8080/v1/monitors/<id>/stats?window=7d" -H "Authorization: Bearer $KEY"

# Soft delete
curl -X DELETE localhost:8080/v1/monitors/<id> -H "Authorization: Bearer $KEY"
```

Errors are RFC 7807 documents. Validation failures list every rejected field at
once, so fixing a monitor definition takes one round trip:

```json
{
  "type": "urn:pulsego:problem:validation-error",
  "title": "Validation Failed",
  "status": 422,
  "request_id": "0da153fed4bb/zeGoxyFs1O-000006",
  "errors": [
    { "field": "name", "message": "is required" },
    { "field": "url", "message": "must use the http or https scheme" }
  ]
}
```

`400` means the request could not be parsed (bad JSON, unknown field, bad UUID
or cursor); `422` means it parsed but a field is invalid. The `request_id`
matches the server log record for the same failure.

Two behaviours worth knowing:

- **Delete is soft.** The row is kept with a `deleted_at` timestamp so historical
  check results stay meaningful, but the monitor disappears from every read
  endpoint and is no longer scheduled. Deleting twice returns `404`.
- **Shortening an interval pulls the next check forward** — but only when the
  scheduled time would otherwise fall outside the new interval, so editing a
  monitor never delays a check that is already due, and a bulk edit does not
  make everything due at once.

## Monitor state and incidents

Each check result moves the monitor through a small state machine. These are
the only edges (`internal/domain/state.go`):

| From       | Success | Failure                                          |
|------------|---------|--------------------------------------------------|
| `unknown`  | `up`    | `degraded`                                       |
| `up`       | `up`    | `degraded`                                       |
| `degraded` | `up`    | `down` once failures ≥ threshold, else `degraded` |
| `down`     | `up`    | `down`                                           |

A success resets `consecutive_failures` to 0; a failure adds one. There is no
`up → down` edge, so a monitor needs **`max(failure_threshold, 2)`**
consecutive failures to reach `down`. With `failure_threshold: 1`, the first
failure gives `degraded` and the second gives `down`. Lowering the threshold
during an outage takes effect on the next failure.

Incidents follow the state:

- **Open** on `degraded → down`. `started_at` is the failing check's
  `checked_at`, `opening_check_id` points at that check, and `cause` is
  `"<error_kind>: <error_message>"` (or just the kind), at most 512 bytes. It is
  never a response body.
- **Resolve** on `down → up`. `resolved_at` and `closing_check_id` come from
  the first successful check.
- `down → down` opens nothing new. A monitor has at most one unresolved
  incident; a partial unique index enforces that in the database.

Recording a result is one transaction: lock the monitor row, compute the
transition from the locked row, insert the check, update the monitor, open or
resolve the incident, commit. If any step fails, nothing is kept and the lease
expires and is reclaimed as usual.

An incident page looks like this; `resolved_at` and `closing_check_id` are
`null` while it is open:

```json
{
  "data": [
    {
      "id": "6f1c0c2e-7c55-4b0e-9a57-2d4f3b8f7a10",
      "monitor_id": "1b4e28ba-2fa1-4d3b-a3f5-ef19b5a7633b",
      "started_at": "2026-11-11T09:14:02.511Z",
      "resolved_at": null,
      "opening_check_id": 4812,
      "closing_check_id": null,
      "cause": "unexpected_status: status 503 outside expected range 200-299",
      "created_at": "2026-11-11T09:14:02.511Z"
    }
  ]
}
```

### Stats

`GET /v1/monitors/{id}/stats?window=24h|7d|30d` summarises the checks in the
window `(now − window, now]`, measured by the database clock:

```json
{
  "window": "24h",
  "from": "2026-11-10T09:20:00Z",
  "to": "2026-11-11T09:20:00Z",
  "total_checks": 1440,
  "successful_checks": 1437,
  "uptime_percent": 99.79,
  "avg_latency_ms": 182.4,
  "p95_latency_ms": 311
}
```

- Latency figures cover **successful checks only**, since a timeout's latency
  is just the timeout. `p95_latency_ms` is an observed value
  (`percentile_disc`), in whole milliseconds.
- `uptime_percent` and `avg_latency_ms` are rounded to two decimals.
- With no checks in the window, `uptime_percent` is `null`; with no successful
  checks, both latency fields are `null`.
- Any other `window` value is a `400` naming the field `window`.

### Retention

The worker deletes checks older than `CHECK_RETENTION_DAYS` (default 30,
range 1–365). It runs once at startup and then every hour, deleting in
batches of 5000 until a batch comes back short. Several workers can run it at
once safely. Incidents are kept; their `opening_check_id` and
`closing_check_id` become `null` once those checks are deleted.

**Caveat:** stats only see retained checks. With `CHECK_RETENTION_DAYS` below
30, the `30d` window covers only the retained days (and below 7, so does the
`7d` window).

## Dashboard

The api serves a small read-only dashboard on its public listener. It is
plain HTML, CSS and JavaScript embedded into the binary (`web/static`), with
no build step and no third-party assets.

1. Open http://localhost:8080/ (or your `HTTP_ADDR`).
2. Enter the API key (`local-development-api-key` for the Compose defaults).

It shows:

- **Summary tiles:** how many monitors are up, down, degraded or unknown, and
  the total.
- **Monitors table:** name and target host, state, uptime and p95 latency over the
  last 24 h, last check time, last status code and latency.
- **Recent incidents:** open incidents first, then the most recently
  resolved, with cause and duration.

It refreshes every 10 seconds. Everything comes from the same `/v1`
endpoints documented above, called from the browser. There is no separate
dashboard backend and no way to change anything from it.

The API key is kept in the tab's **`sessionStorage` only**. It is sent only
in the `Authorization` header and never put into a URL, a cookie or
`localStorage`. Closing the tab or clicking **Forget API key** drops it.
The dashboard responses carry a strict same-origin Content Security Policy,
`X-Content-Type-Options: nosniff` and `Referrer-Policy: no-referrer`.

See [Screenshots](#screenshots) for what it looks like.

## Demo data

**Seed a running api** with four example monitors:

```bash
API_KEY=local-development-api-key make demo-seed   # runs scripts/seed-demo.sh
```

- `API_URL` defaults to `http://localhost:8080`.
- Without `TARGET_BASE`, it seeds public endpoints (`example.com`,
  `www.iana.org`, `httpbin.org/delay/1`, and `httpbin.org/status/503`). The
  last one always fails, so within a minute it is `down` with an open
  incident. These URLs pass the default SSRF policy, so this works against
  the Compose stack as it is.
- Re-running is safe: monitors whose name already exists are skipped.
- If `.env` exists, the Makefile exports its `API_KEY`, so plain
  `make demo-seed` works too. An `API_KEY` set on the command line wins.

**Regenerate the screenshots and the GIF** with `make demo`
(`scripts/demo.sh`).

> [!WARNING]
> **Local demo only, never production.** The demo targets listen on
> `127.0.0.1`, so the api and worker need `ENVIRONMENT=development` and
> `ALLOW_PRIVATE_TARGETS=true`, which turns SSRF protection off. No committed
> file sets them — not the script, the Makefile, `.env.example`, Compose or
> CI. You pass both on the command line every time.

```bash
ENVIRONMENT=development ALLOW_PRIVATE_TARGETS=true \
  DATABASE_URL='postgres://pulsego:pulsego@localhost:5434/pulsego_demo?sslmode=disable' \
  make demo
```

Make arguments work as well:
`make demo ENVIRONMENT=development ALLOW_PRIVATE_TARGETS=true`. Either way
they win over `.env`, and so does a `DATABASE_URL` given the same way;
otherwise `DATABASE_URL` comes from `.env`.

**Prerequisites:** Go, curl, ffmpeg, Chromium or Chrome on `PATH`, and an
empty, throwaway PostgreSQL database in `DATABASE_URL` (create it first,
for example `pulsego_demo` above). The script migrates it. A re-run reuses
the seeded monitors, so earlier incidents stay on the dashboard; use a fresh
database for clean screenshots.

**What it does:**

- Builds and starts its own api, worker and local demo targets
  (`demo/cmd/targets`) on `127.0.0.1:18080`, `:18082`, `:19090` and `:19091`.
  The api and worker inherit `ENVIRONMENT` and `ALLOW_PRIVATE_TARGETS` from
  your command unchanged.
- Seeds four monitors against those targets with a random API key (never
  printed), through `scripts/seed-demo.sh`.
- Collects 60 s of check history (`DEMO_WARMUP`), then drives the
  "Payments webhook" target down until an incident opens, and back up until
  it resolves.
- Takes a few minutes in total: the build, the 60 s warm-up and the incident
  cycle. The capture step gives up after 6 minutes.
- Writes `docs/screenshots/dashboard.png`, `incident-open.png`,
  `incident-resolved.png` and `docs/demo.gif`, then stops everything it
  started.

**Refusal.** Without both variables, the script exits with status 2 before
it builds or starts anything (under `make`, make then reports `Error 2`).
The same happens if `.env` sets `ALLOW_PRIVATE_TARGETS=false`, as
`.env.example` does, and you pass nothing on the command line:

```
demo: refusing to start: the demo needs ENVIRONMENT=development and
ALLOW_PRIVATE_TARGETS=true in the environment, got:
  ENVIRONMENT=<unset>
  ALLOW_PRIVATE_TARGETS=<unset>
Together they turn SSRF protection off so the api and worker can check the
demo targets on 127.0.0.1.

Run it as (local demo only, never in production):

  ENVIRONMENT=development ALLOW_PRIVATE_TARGETS=true make demo
```

Never copy `ALLOW_PRIVATE_TARGETS=true` into `.env`, Compose or a
deployment. The other `DEMO_*` settings (addresses, GIF size and frame
rate) are listed at the top of `scripts/demo.sh`.

The capture tool lives in a nested Go module (`demo/`), so chromedp never
enters the main module, its tests or its vulnerability scan.

## Workers and leases

Each worker process runs one poll loop and a pool of at most
`WORKER_CONCURRENCY` checks:

1. **Claim.** Every `WORKER_POLL_INTERVAL`, the worker runs one SQL statement
   (`ClaimDueMonitors` in `db/queries/checks.sql`). It selects enabled,
   non-deleted monitors with `next_check_at <= now()` and no live lease,
   with `FOR UPDATE SKIP LOCKED`. It then sets `lease_owner` to the worker ID
   and `lease_until` to `now() + GREATEST(WORKER_LEASE_DURATION, monitor
   timeout + 5s)`. It never asks for more monitors than it has free slots
   (and at most `WORKER_BATCH_SIZE`). The lease commits before any network
   call, and every timestamp is PostgreSQL's `now()`, so clock skew between
   workers does not matter.
2. **Check.** Each claimed monitor gets a GET through the SSRF-guarded
   client, bounded by the monitor's timeout. The body is drained (at most
   64 KiB) and discarded, never stored.
3. **Record.** One transaction locks the monitor row *only if this worker
   still holds the lease*. It inserts the check, applies the state
   transition, opens or resolves an incident, reschedules `next_check_at`
   and clears the lease. If the lease was lost (it expired and another
   worker took it, or the monitor was deleted), the late result is discarded.
4. **Shutdown.** On SIGTERM/SIGINT the worker stops claiming. In-flight
   checks get `SHUTDOWN_TIMEOUT` to finish; the ones still running are
   cancelled and their leases released without a result, so the monitors
   stay due and are picked up again at once.

The worker ID is `<hostname>-<uuid>`, so a lease in the database can be
traced back to one process, and two restarts of one container stay distinct.

If a worker crashes mid-check, nothing releases its leases. They simply
expire at `lease_until`, and the next claim by any worker picks those
monitors up. A manual `POST /v1/monitors/{id}/checks` sets `next_check_at` to
now, and a result recorded afterwards keeps that time instead of
rescheduling over it.

## SSRF protection

A monitor makes the worker send requests to a URL chosen by an API client, so
every outbound check is guarded by `internal/security` against server-side
request forgery:

- **What is blocked.** Unspecified (`0.0.0.0/8`, `::`), loopback
  (`127.0.0.0/8`, `::1`), private (`10/8`, `172.16/12`, `192.168/16`),
  CGNAT (`100.64/10`), link-local (`169.254/16` — including the
  `169.254.169.254` cloud metadata endpoint — and `fe80::/10`), multicast,
  broadcast and reserved ranges, documentation and benchmarking ranges, IPv6
  ULA and site-local, NAT64, 6to4 and Teredo. Three IPv6 special-use ranges
  are blocked as well: `::/96` (IPv4-compatible, deprecated), `100::/64`
  (discard-only) and `2001:10::/28` (ORCHID, deprecated); `::` and `::1`
  still report "unspecified" and "loopback". IPv4-mapped IPv6 addresses
  (`::ffff:127.0.0.1`) are unmapped first. `localhost`, `*.localhost`,
  ambiguous numeric hosts (`127.1`, `2130706433`, `0x7f.0.0.1`), IPv6 zone
  identifiers and URLs with credentials are rejected too.
- **Ports.** Only 80 and 443 (or none, meaning the scheme default).
- **At the API.** Create and update validate `url` statically — no DNS — and
  answer 422 on field `url` for a blocked target. An update that does not set
  `url` skips this policy check (the syntax checks still run). That way a
  monitor stored before the policy existed can still be renamed or disabled.
  The dialer still blocks it at check time.
- **At check time.** The dialer resolves the host **once**, rejects it if
  *any* answer is blocked, and then dials only the validated IP literals, so a
  DNS-rebinding second lookup can never happen. The socket address is checked
  again right before `connect`.
- **Redirects.** At most 5, and every hop is validated again, statically and
  by the dialer; a redirect to a private address is never dialled.
- A refused check is stored with `error_kind` `blocked`. Stored error messages
  never contain the URL or its query string.

The full list, with the reason reported for each range, is
`blockedRanges` in `internal/security/policy.go`.

### `ALLOW_PRIVATE_TARGETS` is for local development only

`ALLOW_PRIVATE_TARGETS=true` turns the address, `localhost` and port checks
off (resolve-then-dial-the-IP still applies). It exists so you can monitor a
service on your own machine during development, and the config refuses it
outside `ENVIRONMENT=development`.

The risk is real even on a "development" box that is reachable from a
network. With it on, anyone holding the API key can make the worker reach
internal services, admin ports and cloud metadata credentials
(`169.254.169.254`) from inside your network. They can then read back status
codes, timings and error kinds through the check history. Keep it `false` in
`.env`, in Compose and in every deployed environment. `scripts/demo.sh` never
sets it: you pass it on the command line for a local demo run, and the script
refuses to start without it (see [Demo data](#demo-data)).

## Observability

### Health and readiness

- **`/healthz`** never touches the database. It answers 200 as long as the
  process serves HTTP, so it is the liveness probe: the Docker `HEALTHCHECK`
  calls it.
- **`/readyz`** pings PostgreSQL with a 2-second timeout. It answers 200
  `{"status":"ready"}`, or 503 problem+json of type
  `urn:pulsego:problem:not-ready`. The body says only that a dependency is
  unavailable; the actual error goes to the log at warn. Use it to route traffic, not to restart a process.

Both answer GET and HEAD without the API key. The api serves them on
`HTTP_ADDR` and on `INTERNAL_HTTP_ADDR`; the worker has no public listener and
serves them on `INTERNAL_HTTP_ADDR` only.

### Metrics

`/metrics` (GET) is served on **`INTERNAL_HTTP_ADDR`** only (default `:9090`),
by both the api and the worker — never on the public port. Each binary uses
its own registry with all ten metrics, plus the Go runtime and process
collectors:

| Metric | Type | Labels |
|---|---|---|
| `pulsego_http_requests_total` | counter | `method`, `route`, `code` |
| `pulsego_http_request_duration_seconds` | histogram | `method`, `route` |
| `pulsego_monitor_checks_total` | counter | `result` (`success`/`failure`), `error_kind` (`none` or the kind) |
| `pulsego_monitor_check_duration_seconds` | histogram | `result` |
| `pulsego_monitor_state_total` | counter | `from`, `to` (only real changes) |
| `pulsego_worker_claimed_monitors_total` | counter | — |
| `pulsego_worker_active_checks` | gauge | — |
| `pulsego_worker_claim_errors_total` | counter | — |
| `pulsego_scheduler_lag_seconds` | histogram | — (claim time − `next_check_at`, ≥ 0) |
| `pulsego_db_operation_duration_seconds` | histogram | `operation` (sqlc query name, else `other`), `outcome` (`ok`/`error`) |

Every label value comes from a fixed set, so cardinality stays bounded:

- `method` is one of the standard methods, else `OTHER`.
- `route` is the chi route pattern (`/v1/monitors/{id}`), else `unmatched`.
  A `/v1` request rejected by the API key check never reaches its route, so
  it is counted as `/v1/*`.
- `code` is the 3-digit status.
- `error_kind` and the states are checked against the domain enums, else
  `unknown`.

URLs, monitor IDs, error messages and request IDs are never labels.

The HTTP metrics count the api's public routes only; the internal listener is
not instrumented, so scrapes and probes are not counted. A metric with labels
shows up in `/metrics` only after its first observation:

- The HTTP metrics appear after the first public request. The worker has no
  public router, so its `/metrics` never shows them; it shows the other 8.
- `pulsego_db_operation_duration_seconds` appears after the first query. The
  `/readyz` ping is not a query, so it does not count.
- The check and state counters are pre-created at zero for every known label
  value, so they are there from the start.

```bash
curl -s localhost:9090/metrics | grep '^pulsego_'
```

### Tracing

Tracing is off by default (`OTEL_ENABLED=false`). Then no exporter is
created and nothing goes over the network. With `OTEL_ENABLED=true`, spans go
over OTLP/HTTP (protobuf) to `OTEL_EXPORTER_OTLP_ENDPOINT`. It must be a full
http(s) URL, such as `http://otel-collector:4318`; the config refuses to start
without one. Setup never dials, so an unreachable collector cannot block
startup or requests: export errors are logged at warn, and the final flush is
bounded by `SHUTDOWN_TIMEOUT`. The trace service names are
`<SERVICE_NAME>-api` and `<SERVICE_NAME>-worker` (by default `pulsego-api`
and `pulsego-worker`).

Spans:

- **Incoming requests:** `"<METHOD> <route>"`.
- **PostgreSQL:** `db <operation>`, with no statement text or arguments.
- **Worker claim cycles:** `scheduler.claim`.
- **Each check:** a `scheduler.check` trace with the children
  `checker.check` (the outbound request) and `monitor.record`.
  `monitor.record` carries the `incident.opened` and `incident.resolved`
  events.

No span carries headers, query strings, bodies or the API key.

### Local stack

```bash
docker compose -f deployments/compose.yaml --profile observability up --build
```

This adds an OpenTelemetry Collector, Jaeger, Prometheus and Grafana to the
usual stack (`deployments/compose.yaml`):

| UI | URL | Override |
|---|---|---|
| Jaeger | http://localhost:16686 | `JAEGER_UI_PORT` |
| Prometheus | http://localhost:9092 | `PROMETHEUS_PORT` |
| Grafana | http://localhost:3000 (anonymous viewer, Prometheus datasource provisioned) | `GRAFANA_PORT` |

Prometheus scrapes `api:9090` and `worker:9090` inside the Compose network.
Traces only arrive in Jaeger if you also turn tracing on:

```bash
OTEL_ENABLED=true OTEL_EXPORTER_OTLP_ENDPOINT=http://otel-collector:4318 \
  docker compose -f deployments/compose.yaml --profile observability up --build
```

## Testing

```bash
make test              # unit tests with the race detector: go test -race ./...
make test-integration  # integration tests (build tag "integration"), race detector on
make lint              # golangci-lint
make vet               # go vet
make vuln              # govulncheck
make sqlc              # regenerate query code after editing db/queries
make build             # api, worker and migrate binaries into bin/
```

**Unit tests** need no database and no Docker. They cover config validation,
the state machine, error classification, the SSRF policy and dialer, stats
maths, cursors, the dashboard handlers and every HTTP handler via `httptest`.
A test also checks the router against `api/openapi.yaml`.

**Integration tests** (`//go:build integration`) run against a real
PostgreSQL. They cover migrations up and down, constraints, concurrent
`SKIP LOCKED` claims, lease expiry and reclaim, the check-result
transaction, incidents, pagination, stats and retention. They find a database
in one of two ways:

- **Testcontainers (default).** With `PULSEGO_TEST_DATABASE_URL` unset, the
  suite starts PostgreSQL containers through Docker. This is what CI does.
- **An existing server, no Docker.** Point `PULSEGO_TEST_DATABASE_URL` at a
  PostgreSQL server whose role has the `CREATEDB` privilege:

  ```bash
  PULSEGO_TEST_DATABASE_URL='postgres://user:pass@localhost:5432/postgres?sslmode=disable' \
    make test-integration
  ```

  The suite never writes to the database in that URL. It creates its own
  `pulsego_test_<random>` databases on the server, one per pristine-schema
  test plus one shared database, and drops each with
  `DROP DATABASE … WITH (FORCE)` when it is done. The application itself
  never reads this variable.

The demo tooling is a separate Go module; test it with
`cd demo && go test ./...`.

## CI

`.github/workflows/ci.yml` runs on every push and pull request, in three jobs:

- **`checks`:** `gofmt`, `go mod tidy -diff`, `sqlc diff`, `go vet` and
  `golangci-lint` (each also with the `integration` tag), `go test -race`,
  `govulncheck`, and the three `go build -o bin/...` builds.
- **`integration`:** the integration suite in Testcontainers mode.
- **`docker`:** builds the api, worker and migrate images. It runs them
  against PostgreSQL and waits for both health checks to pass. It then checks
  that the containers run as non-root and that `/readyz` returns 200.

The badge at the top of this file shows the latest result.

## Deployment notes

PulseGo has not been run in production; these notes follow from how it is
built.

- **Images.** `make docker-build` builds `pulsego-api:local`,
  `pulsego-worker:local` and `pulsego-migrate:local` from one
  `deployments/Dockerfile` (`--build-arg BINARY=…`). The runtime is Alpine
  with CA certificates, running as uid `10001`. The image `HEALTHCHECK` calls
  `http://127.0.0.1:9090/healthz`, so keep `INTERNAL_HTTP_ADDR` on port
  `9090` inside containers, or override the health check.
- **Order.** Run `migrate up` to completion before rolling out a new api or
  worker version. The services never migrate themselves.
- **Configuration.** Set `ENVIRONMENT=production` (or `staging`), a random
  `API_KEY` of at least 16 characters (`openssl rand -hex 32`), and leave
  `ALLOW_PRIVATE_TARGETS=false`; the config refuses `true` outside
  development anyway. Never commit `.env`.
- **Network.** The api speaks plain HTTP; put TLS in front of it with a
  reverse proxy or load balancer. Expose only the public listener
  (`HTTP_ADDR`). Keep `INTERNAL_HTTP_ADDR` (probes and metrics) reachable
  only by your orchestrator and Prometheus.
- **Probes.** Use `/healthz` for liveness and `/readyz` for readiness.
- **Scaling.** The api is stateless; run as many replicas as you like behind
  a load balancer. Workers scale horizontally too: add replicas and they
  split due monitors through `SKIP LOCKED`. Each process opens up to
  `max(10, WORKER_CONCURRENCY + 2)` PostgreSQL connections; size
  `max_connections` for the sum of all replicas.
- **Shutdown.** Both binaries drain on SIGTERM within `SHUTDOWN_TIMEOUT`
  (default 15 s). Give your orchestrator a longer grace period than that.
- **Egress.** Workers need outbound HTTP(S) to the monitored targets and
  DNS. They need no inbound traffic except the internal listener.

## Layout

```
cmd/{api,worker,migrate}   service entry points; migrate is a one-shot job
internal/config            env loading and validation
internal/domain            core types, limits and the state machine
internal/httpapi           routing, middleware, handlers, dashboard routes
internal/monitor           monitor service and validation
internal/checker           the HTTP check and error classification
internal/scheduler         claim loop, leases, retention
internal/security          SSRF policy, guarded dialer, redirect policy
internal/repository        pgxpool setup + sqlc-generated queries
internal/telemetry         logging, Prometheus metrics, tracing
db/{migrations,queries}    goose migrations, sqlc query sources
api/openapi.yaml           OpenAPI 3.1 contract
web/static                 dashboard HTML, CSS and JavaScript (embedded)
deployments/               Dockerfile, Compose stack, Prometheus/OTel/Grafana config
scripts/                   seed-demo.sh, demo.sh
demo/                      separate Go module: demo targets and screenshot capture
docs/                      screenshots and demo GIF
```

No ORM: the SQL lives in `db/queries` and `sqlc` compiles it into type-safe Go.

## Database and migrations

Migrations are goose files embedded into the `migrate` binary. The API and the
worker never migrate on startup — Compose runs `migrate` once to completion and
only then starts the services, so scaling to several replicas cannot race on the
schema.

```bash
make migrate-up
make migrate-status
make migrate-down     # rolls back one migration
```

Three tables hold everything:

- **`monitors`**: the definition plus scheduling state (`next_check_at`,
  `lease_owner`, `lease_until`) and current state.
- **`monitor_checks`**: one row per check.
- **`incidents`**: at most one unresolved incident per monitor, enforced by a
  partial unique index.

## Key Engineering Decisions

### Why PostgreSQL is both storage and scheduler

A check is due when `next_check_at <= now()`. That fact already lives in the
`monitors` row, so the database *is* the queue. Claiming, recording a result
and rescheduling happen in the same place as the data they change. A crash
can therefore never leave a queue and a table disagreeing. One dependency
also means one thing to back up, monitor and reason about. The partial
index on `(enabled, next_check_at) WHERE deleted_at IS NULL` keeps the claim
query cheap.

### Why `SKIP LOCKED` enables multiple workers

The claim runs `SELECT … FOR UPDATE SKIP LOCKED` inside an `UPDATE` that
writes the lease. When two workers claim at the same moment, each one skips
the rows the other has already locked instead of waiting on them. So they
get **disjoint** batches in one round trip, with no blocking and no retry
loop. Adding a worker adds throughput; nothing has to coordinate them. An
integration test runs concurrent claims and asserts that no monitor is
claimed twice.

### Why Redis was intentionally omitted

Redis (or Kafka, NATS…) would add a second source of truth for "what is
due" that has to be kept consistent with PostgreSQL. It would also add a
service to run and secure, and a failure mode where the two disagree. At
this scale, PostgreSQL row locks give the same exclusive-claim guarantee,
transactionally with the data. The project's non-goals rule out extra
infrastructure until a measured bottleneck justifies it.

### How leases recover abandoned jobs

A claim sets `lease_owner` and `lease_until`, and commits *before* any network
call. A monitor with a live lease is invisible to other claims. A healthy
worker clears its lease when it records the result. A worker that crashes
never does, so its lease simply runs out, and the next claim by any worker
picks the monitor up. No heartbeat or janitor process is needed.

Three details keep this safe:

- The lease is at least the monitor's timeout plus 5 s, and the config
  requires `WORKER_LEASE_DURATION > DEFAULT_CHECK_TIMEOUT`. A slow check is
  therefore not reclaimed while it is still running.
- Recording a result requires that this worker still holds the lease. A late
  result from a worker whose lease was reclaimed is discarded, not applied
  twice.
- All lease times come from PostgreSQL's `now()`, so worker clocks do not
  matter.

### How SSRF protection works

Monitors make the server fetch URLs chosen by API clients. The protection
works in layers:

1. The API checks the URL statically (scheme, host, port 80/443, blocked
   ranges, no credentials) without DNS.
2. At check time the dialer resolves the host once, rejects it if *any*
   address is blocked, and dials only the validated IP. A DNS-rebinding
   second lookup cannot happen.
3. The socket address is checked again just before `connect`.
4. Every redirect hop goes through the same checks, and there are at most 5.

Blocked checks are stored as `error_kind: blocked`, without the URL. See
[SSRF protection](#ssrf-protection) for the full list.

### How metric cardinality is controlled

Every label value comes from a closed set:

- HTTP method (else `OTHER`);
- chi route **pattern**, not path (else `unmatched`);
- 3-digit status code;
- domain enums for result, error kind and state (else `unknown`);
- sqlc query names for database operations (else `other`).

URLs, monitor IDs, worker IDs, error messages and request IDs are never
labels. The `telemetry` package normalises every value itself, so a caller
cannot add an unbounded label by mistake. The series count therefore does
not grow with the number of monitors or requests.

## Known limitations

- **One shared API key.** There are no users, roles or tenants; everyone with
  the key can read and change every monitor. The dashboard asks for that same
  key.
- **HTTP(S) GET only.** There are no other methods, no request bodies or
  headers, no response-body assertions, and only ports 80 and 443. TCP, ICMP
  and browser checks are not supported.
- **No notifications.** Incidents are visible through the API and the
  dashboard only; nothing sends email, chat or webhooks.
- **Single region.** All workers check from wherever they run, so a network
  problem near the workers looks like an outage.
- **No TLS in the binary.** Terminate TLS in front of the api.
- **Retention limits stats.** Stats see retained checks only. With
  `CHECK_RETENTION_DAYS` below 30, the `30d` window covers only the retained
  days (below 7, so does `7d`). Incidents are kept forever.
- **Dashboard scale.** The dashboard loads at most 1,000 monitors (10 pages of
  100), and requests stats and incidents per monitor every 10 s. It is meant
  for small installations.
- **Private targets.** Internal services cannot be monitored outside
  development, by design.

## Roadmap

All six sprints in `ROADMAP.md` are done:

| Sprint | Scope | Status |
|---|---|---|
| 1 — Foundation | Layout, config, logging, graceful shutdown, Compose, first migration | Done |
| 2 — Monitor API | CRUD, validation, problem+json, API key, OpenAPI | Done |
| 3 — Scheduler and Checker | `SKIP LOCKED` claims, leases, HTTP checks, check history | Done |
| 4 — State and Incidents | State machine, incidents, stats, retention | Done |
| 5 — Production Hardening | SSRF protection, metrics, tracing, readiness, CI | Done |
| 6 — Portfolio Polish | Dashboard, demo tooling, screenshots, this README | Done |

Possible future work, all outside v1 scope:

- **Webhook notifications** on incident open and resolve (the only
  stretch feature `ROADMAP.md` names).
- Per-monitor request headers and response-body assertions.
- Scoped or multiple API keys.

The v1 non-goals still apply: no Kubernetes, Kafka/NATS, Redis,
microservices, SMS/email delivery, multi-tenant authorization, billing,
mobile app, browser/synthetic journeys or regional worker clusters.

## Screenshots

Generated by `make demo` against local demo targets (see
[Demo data](#demo-data)).

**Dashboard** — summary, monitors with 24 h uptime and p95, recent incidents:

![PulseGo dashboard with several monitors up and their uptime and latency](docs/screenshots/dashboard.png)

**Incident open** — a monitor is down and its incident is open:

![Dashboard showing a monitor down with an open incident](docs/screenshots/incident-open.png)

**Incident resolved** — the monitor recovered and the incident closed:

![Dashboard showing the monitor back up and the incident resolved](docs/screenshots/incident-resolved.png)

**Demo** — the full cycle as a GIF: [`docs/demo.gif`](docs/demo.gif).

## CV-ready description

> Built a production-oriented uptime monitoring platform in Go using
> PostgreSQL, type-safe SQL, horizontally scalable background workers,
> lease-based job scheduling, OpenTelemetry, Prometheus, integration tests,
> Docker, and CI/CD. Implemented incident lifecycle management, graceful
> shutdown, and SSRF protection for user-supplied monitoring targets.
