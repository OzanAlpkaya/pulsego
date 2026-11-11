# PulseGo

Uptime monitoring platform in Go. Register HTTP/HTTPS endpoints; background
workers check them on a schedule, record latency and availability, and open and
resolve incidents when failures persist.

Two binaries share one PostgreSQL-backed domain — `cmd/api` serves the REST API,
`cmd/worker` runs the checks. PostgreSQL is both the store and the scheduler:
workers claim due monitors with `FOR UPDATE SKIP LOCKED` and hold time-bounded
leases, so several workers can run without double-processing and a crashed
worker's monitors become reclaimable on their own.

> **Status: Sprint 2 of 6 complete.** The foundation and the monitor CRUD API
> are in place. The scheduler and checker (Sprint 3), incidents (Sprint 4),
> SSRF protection and observability (Sprint 5), and the dashboard plus the full
> README (Sprint 6) are still to come — so monitors can be registered, but
> nothing checks them yet. See `ROADMAP.md` and `PROGRESS.md`.

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
