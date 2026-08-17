# CLAUDE.md — PulseGo

## Read first

- `ROADMAP.md` — full sprint plan, DB model, config, and Definition of Done.
  Source of truth for scope and order.
- `PROGRESS.md` — current status per sprint. Update it at the end of every
  sprint; never let it drift from reality.

## Working rules

1. **One branch per sprint:** `feature/sprint-NN-short-name`, cut from
   `main`.
2. **Never push to `main` directly.** Squash-merge only, and only after
   explicit user approval — do not merge automatically even if every check
   is green.
3. **Plan before code.** For every sprint, post an implementation plan and
   wait for approval before writing code.
4. **No skipping ahead.** A new sprint may not start while the previous
   sprint's Definition of Done in `PROGRESS.md` has unchecked items.
5. **All files in English** — code, comments, docs, commit messages.
6. **No ORM.** SQL stays visible and compiles through `sqlc`. Don't
   introduce an ORM under any framing (including "just for this one
   query").
7. **No Redis, Kafka, Kubernetes, or extra services** beyond `postgres`,
   `api`, `worker` (+ the optional observability profile: otel-collector,
   prometheus, grafana, jaeger). See Non-Goals in `ROADMAP.md`.
8. **SSRF protection is scoped to Sprint 5** per the plan, but once it
   exists, never weaken it or set `ALLOW_PRIVATE_TARGETS=true` outside
   local development.
9. **Observability must degrade gracefully.** `OTEL_ENABLED=false` and an
   unreachable Prometheus scrape must never block app startup or request
   handling.
10. **Never persist or log** response bodies, full authorization headers, or
    the API key — not in the database, not in logs, not as metric labels.
11. No LLM/AI component in this project — no `LLM_ENABLED`-style pattern
    applies here.

## Skills

This repo has two project skills — invoke them instead of re-deriving the
steps manually each time:

- **`/sprint-start`** — starts the next sprint: reads `PROGRESS.md`,
  enforces Rule 4, creates the branch, drafts a plan for approval.
- **`/sprint-done`** — runs the full Go quality gate (`gofmt`, `go vet`,
  `golangci-lint`, `go test -race`, `govulncheck`, both binary builds),
  checks the sprint's Definition of Done against `ROADMAP.md`, updates
  `PROGRESS.md`. Stops before merging — always waits for your go-ahead.

## Tech stack quick reference

Go (current stable) · chi v5 · pgx/v5 pgxpool · sqlc · goose/golang-migrate ·
log/slog (JSON) · Prometheus client · OpenTelemetry (OTLP) ·
Testcontainers-Go · Docker Compose · GitHub Actions · golangci-lint ·
OpenAPI 3.1.
