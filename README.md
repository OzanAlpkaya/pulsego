# PulseGo

Uptime monitoring platform in Go. Register HTTP/HTTPS endpoints; background
workers check them on a schedule, record latency and availability, and open and
resolve incidents when failures persist.

Two binaries share one PostgreSQL-backed domain — `cmd/api` serves the REST API,
`cmd/worker` runs the checks. PostgreSQL is both the store and the scheduler:
workers claim due monitors with `FOR UPDATE SKIP LOCKED` and hold time-bounded
leases, so several workers can run without double-processing and a crashed
worker's monitors become reclaimable on their own.

> **Status: Sprint 1 of 6 complete.** The foundation — configuration, logging,
> both binaries, the `monitors` schema, and the Compose stack — is in place. The
> monitor API (Sprint 2), scheduler and checker (Sprint 3), incidents (Sprint 4),
> SSRF protection and observability (Sprint 5), and the dashboard plus the full
> README (Sprint 6) are still to come. See `ROADMAP.md` and `PROGRESS.md`.

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
make test-integration  # Testcontainers; needs a Docker daemon
make lint              # golangci-lint
make vuln              # govulncheck
make sqlc              # regenerate query code after editing db/queries
make build             # binaries into bin/
```

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
