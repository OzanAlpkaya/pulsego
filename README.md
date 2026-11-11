# PulseGo

Uptime monitoring platform in Go. Register HTTP/HTTPS endpoints; background
workers check them on a schedule, record latency and availability, and open and
resolve incidents when failures persist.

Two binaries share one PostgreSQL-backed domain — `cmd/api` serves the REST API,
`cmd/worker` runs the checks. PostgreSQL is both the store and the scheduler:
workers claim due monitors with `FOR UPDATE SKIP LOCKED` and hold time-bounded
leases, so several workers can run without double-processing and a crashed
worker's monitors become reclaimable on their own.

> **Status: Sprint 4 of 6 complete.** Monitors can be registered, workers
> check them on a schedule, and persistent failures open and resolve
> incidents. SSRF protection and observability (Sprint 5) and the dashboard
> plus the full README (Sprint 6) are still to come — until Sprint 5, do not
> point the worker at untrusted monitor input. See `ROADMAP.md` and
> `PROGRESS.md`.

## Quick start

Requires Go 1.26+ and Docker.

```bash
make bootstrap   # download modules, verify tooling, create .env
make up          # postgres + migrations + api + worker
curl localhost:8080/healthz
make down
```

Compose publishes the API on `8080`, the internal endpoints on `9090`, and
PostgreSQL on `5434` (not 5432, so it does not collide with a database already
running locally). Override with `API_PORT`, `API_INTERNAL_PORT`, and
`POSTGRES_PORT`.

## Running locally without Compose

```bash
cp .env.example .env      # then set DATABASE_URL and API_KEY
make migrate-up
make run-api              # in one shell
make run-worker           # in another
```

## API

Every `/v1` endpoint needs the API key; `/healthz` deliberately does not. The
full contract is `api/openapi.yaml`, and a test walks the router against that
document so the two cannot drift apart.

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

## Configuration

Every variable is documented in `.env.example` and validated at startup. An
invalid configuration stops the process with every problem listed at once:

```
api: config: API_KEY must be at least 16 characters
config: DATABASE_URL is required
config: WORKER_CONCURRENCY must be between 1 and 256, got 999
```

Two rules are worth calling out:

- `WORKER_LEASE_DURATION` must exceed `DEFAULT_CHECK_TIMEOUT`, otherwise a
  second worker could reclaim a monitor while the first is still waiting on the
  network.
- `ALLOW_PRIVATE_TARGETS=true` is refused unless `ENVIRONMENT=development`. It
  lets monitors point at loopback and private addresses, which turns the checker
  into an SSRF gadget against your own network. The protection it bypasses
  arrives in Sprint 5.

## Development

```bash
make test              # unit tests, race detector
make test-integration  # Testcontainers (Docker), or PULSEGO_TEST_DATABASE_URL
make lint              # golangci-lint
make vuln              # govulncheck
make sqlc              # regenerate query code after editing db/queries
make build             # binaries into bin/
```

Integration tests start PostgreSQL containers through Testcontainers by
default. To run them without Docker, point `PULSEGO_TEST_DATABASE_URL` at an
existing PostgreSQL server whose role has the `CREATEDB` privilege:

```bash
PULSEGO_TEST_DATABASE_URL='postgres://user:pass@localhost:5432/postgres?sslmode=disable' \
  make test-integration
```

The suite never writes to the database in that URL. It creates its own
`pulsego_test_<random>` databases on the server, one per pristine-schema test
plus one shared database, and drops each with `DROP DATABASE … WITH (FORCE)`
when it is done. The application itself never reads this variable.

Developer tools (`sqlc`, `golangci-lint`, `govulncheck`) are pinned as go.mod
`tool` directives, so a bare checkout needs nothing installed beyond Go and
Docker.

## Layout

```
cmd/{api,worker,migrate}   service entry points; migrate is a one-shot job
internal/config            env loading and validation
internal/domain            core types and limits
internal/httpapi           routing, middleware, handlers
internal/repository        pgxpool setup + sqlc-generated queries
internal/{monitor,checker,scheduler,security,telemetry}
db/{migrations,queries}    goose migrations, sqlc query sources
deployments/               Dockerfile, Compose stack
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
