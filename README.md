# PulseGo

Uptime monitoring platform in Go. Register HTTP/HTTPS endpoints; background
workers check them on a schedule, record latency and availability, and open and
resolve incidents when failures persist.

Two binaries share one PostgreSQL-backed domain — `cmd/api` serves the REST API,
`cmd/worker` runs the checks. PostgreSQL is both the store and the scheduler:
workers claim due monitors with `FOR UPDATE SKIP LOCKED` and hold time-bounded
leases, so several workers can run without double-processing and a crashed
worker's monitors become reclaimable on their own.

> **Status: Sprint 5 of 6 complete.** Monitors can be registered, workers
> check them on a schedule behind SSRF protection, persistent failures open and
> resolve incidents, and both binaries export Prometheus metrics and OTLP
> traces. The dashboard and the full README (Sprint 6) are still to come. See
> `ROADMAP.md` and `PROGRESS.md`.

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
  into an SSRF gadget against your own network. See
  [SSRF protection](#ssrf-protection).

## SSRF protection

A monitor makes the worker send requests to a URL chosen by an API client, so
every outbound check is guarded by `internal/security` against server-side
request forgery:

- **What is blocked.** Unspecified (`0.0.0.0/8`, `::`), loopback
  (`127.0.0.0/8`, `::1`), private (`10/8`, `172.16/12`, `192.168/16`),
  CGNAT (`100.64/10`), link-local (`169.254/16` — including the
  `169.254.169.254` cloud metadata endpoint — and `fe80::/10`), multicast,
  broadcast and reserved ranges, documentation and benchmarking ranges, IPv6
  ULA and site-local, NAT64, 6to4 and Teredo. IPv4-mapped IPv6 addresses
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

`ALLOW_PRIVATE_TARGETS=true` turns the address, `localhost` and port checks
off (resolve-then-dial-the-IP still applies). It exists so you can monitor a
service on your own machine during development, and the config refuses it
outside `ENVIRONMENT=development`: with it on, anyone holding the API key can
make the worker reach internal services, admin ports and cloud metadata
credentials from inside your network, and read back status codes and timings.

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

## CI

`.github/workflows/ci.yml` runs on every push and pull request, in three jobs:

- **`checks`:** `gofmt`, `go mod tidy -diff`, `sqlc diff`, `go vet` and
  `golangci-lint` (each also with the `integration` tag), `go test -race`,
  `govulncheck`, and the three `go build -o bin/...` builds.
- **`integration`:** the integration suite in Testcontainers mode.
- **`docker`:** builds the api, worker and migrate images. It runs them
  against PostgreSQL and waits for both health checks to pass. It then checks
  that the containers run as non-root and that `/readyz` returns 200.

## Layout

```
cmd/{api,worker,migrate}   service entry points; migrate is a one-shot job
internal/config            env loading and validation
internal/domain            core types and limits
internal/httpapi           routing, middleware, handlers
internal/repository        pgxpool setup + sqlc-generated queries
internal/{monitor,checker,scheduler,security,telemetry}
db/{migrations,queries}    goose migrations, sqlc query sources
deployments/               Dockerfile, Compose stack, Prometheus/OTel/Grafana config
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
