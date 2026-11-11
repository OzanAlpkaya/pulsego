# PulseGo — Progress

Last updated: 2026-11-11
Current sprint: all 6 sprints done, Sprint 6 awaiting review/merge

---

## Sprint 1 — Foundation
Status: **Done** (2026-08-17)
Branch: `feature/sprint-01-foundation` — squash-merged into `main` as `61266ae`

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
Status: **Done** (2026-08-17)
Branch: `feature/sprint-02-monitor-api` — squash-merged into `main` as `799a622`

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
Status: **Done** (2026-11-11)
Branch: `feature/sprint-03-scheduler-checker` — squash-merged into `main` as `d897497`

Definition of Done:
- [x] Concurrent `SKIP LOCKED` claims never double-assign a monitor
      (`TestClaimParallelClaimersNeverDoubleAssign`,
      `TestClaimSkipsRowsLockedByOpenTransaction`)
- [x] Expired leases are reclaimable by a different worker
      (`TestExpiredLeaseReclaimedByDifferentWorker`)
- [x] Checker respects bounded concurrency and per-request timeouts
      (`TestConcurrencyBound`, `TestCheckTimeout`, `TestSchedulerEndToEnd`)
- [x] Manual check + cursor-paginated check history endpoints work
      (`TestRequestCheckEndpoint`, `TestListChecksEndpoint`,
      `TestListChecksKeysetPagination`, `TestCursorKindMismatchIsRejected`,
      `TestSchedulerEndToEnd`)

Notes:
- Plan approved 2026-11-11. Implementation ran in agent-office, following
  the D9 commit order below. The decisions below are binding and self-contained — an
  implementer should not need the planning conversation. Anything not settled
  here is a question for the user, not a guess (CLAUDE.md, "Ask, don't guess").
- Decisions taken at sprint start (D1–D11 of the approved plan):
  - **D1 — Integration tests can run without Docker.** When
    `PULSEGO_TEST_DATABASE_URL` is set, the integration suite uses that server
    and never calls Testcontainers. When it is unset, behaviour is exactly as
    today: one shared container in `TestMain`, plus one container per
    pristine-schema test (`startPostgres(t)`).
    - With the variable set, *every* database the suite uses is created and
      dropped by the suite. This includes the shared pool's database (Q2):
      it runs `TRUNCATE`, so it must never be the database the URL points at.
      Each test gets `CREATE DATABASE pulsego_test_<16 hex chars from
      crypto/rand>` on that server, and its cleanup (`t.Cleanup`, or a
      `defer` in `runTests`) runs `DROP DATABASE … WITH (FORCE)`.
    - The connection URL for a created database is the given URL with its
      path swapped for the new name.
    - The database name is the only SQL in the repository that is not
      parameterised, because DDL identifiers cannot be bind parameters. It is
      generated by the code, never taken from input, and quoted with
      `pgx.Identifier{name}.Sanitize()`.
    - If the role lacks `CREATEDB`, the suite fails with a message naming
      that privilege. It does not skip.
    - Helpers stay in `internal/repository/*_integration_test.go`
      (`package repository_test`). There is no new package, so the ROADMAP §3
      layout is untouched.
    - `TestNewPoolRejectsUnreachableDatabase` is unchanged.
    - Document the variable in the README's test section and as a
      commented-out line in `.env.example`. The app's `config.Load` does not
      read it.
    - The server is started with **`pg-test-up`**. It comes from
      agent-office's stack pack and exists only inside agent-office's
      sandboxed workers. It is **not** a Makefile target, and none is to be
      added for it.
  - **D2 — `monitor_checks` schema**, as migration
    `db/migrations/00003_create_monitor_checks.sql`:
    ```sql
    -- +goose Up
    -- +goose StatementBegin
    CREATE TABLE monitor_checks (
        id            BIGINT      GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
        monitor_id    UUID        NOT NULL REFERENCES monitors (id) ON DELETE CASCADE,
        success       BOOLEAN     NOT NULL,
        status_code   INT,
        latency_ms    INT         NOT NULL,
        error_kind    VARCHAR(32),
        error_message TEXT,
        checked_at    TIMESTAMPTZ NOT NULL DEFAULT now(),

        -- Bounds mirrored by internal/domain/check.go.
        CONSTRAINT monitor_checks_status_code_range
            CHECK (status_code IS NULL OR status_code BETWEEN 100 AND 599),
        CONSTRAINT monitor_checks_latency_non_negative
            CHECK (latency_ms >= 0),
        CONSTRAINT monitor_checks_error_kind_valid
            CHECK (error_kind IS NULL OR error_kind IN
                ('timeout', 'dns', 'connection', 'tls', 'redirect',
                 'unexpected_status', 'unknown')),
        CONSTRAINT monitor_checks_error_message_length
            CHECK (error_message IS NULL OR length(error_message) <= 512),
        -- A check either succeeded with no error, or failed with a kind.
        CONSTRAINT monitor_checks_outcome_consistent
            CHECK (success = (error_kind IS NULL)),
        CONSTRAINT monitor_checks_message_only_on_failure
            CHECK (error_message IS NULL OR NOT success),
        CONSTRAINT monitor_checks_success_has_status
            CHECK (NOT success OR status_code IS NOT NULL),
        CONSTRAINT monitor_checks_unexpected_status_has_status
            CHECK (error_kind IS DISTINCT FROM 'unexpected_status'
                   OR status_code IS NOT NULL)
    );
    -- +goose StatementEnd

    -- History endpoint (keyset over checked_at DESC, id DESC).
    -- +goose StatementBegin
    CREATE INDEX monitor_checks_history_idx
        ON monitor_checks (monitor_id, checked_at DESC, id DESC);
    -- +goose StatementEnd

    -- Retention cleanup (Sprint 4).
    -- +goose StatementBegin
    CREATE INDEX monitor_checks_checked_at_idx ON monitor_checks (checked_at);
    -- +goose StatementEnd

    -- +goose Down
    -- +goose StatementBegin
    DROP TABLE monitor_checks;
    -- +goose StatementEnd
    ```
    - New file `internal/domain/check.go` holds:
      - `type ErrorKind string`, with one constant per allowed value and
        `Valid()`.
      - `MaxErrorMessageLength = 512`.
      - `CheckOutcome` (`Success`, `StatusCode *int32`, `LatencyMS int32`,
        `ErrorKind *ErrorKind`, `ErrorMessage *string`): what the checker
        produces.
      - `Check` (an outcome plus `ID int64`, `MonitorID`, `CheckedAt`): the
        stored row.
      - `ErrMonitorDisabled`, next to `ErrNotFound` in `errors.go`.
    - **Required test-helper change:** `TRUNCATE monitors` fails once a
      foreign key references the table. `truncateMonitors` becomes
      `TRUNCATE monitor_checks, monitors`.
  - **D3 — The claim is a single statement and commits before any network
    call.** In `db/queries/checks.sql`:
    ```sql
    -- name: ClaimDueMonitors :many
    WITH due AS (
        SELECT id
        FROM monitors
        WHERE enabled
          AND deleted_at IS NULL
          AND next_check_at <= now()
          AND (lease_until IS NULL OR lease_until <= now())
        ORDER BY next_check_at
        LIMIT sqlc.arg('batch_size')
        FOR UPDATE SKIP LOCKED
    )
    UPDATE monitors m
    SET lease_owner = sqlc.arg('worker_id'),
        lease_until = now() + GREATEST(
            make_interval(secs => sqlc.arg('lease_seconds')::float8),
            make_interval(secs => m.timeout_ms / 1000.0 + 5)
        )
    FROM due
    WHERE m.id = due.id
    RETURNING m.*;
    ```
    - **Lease formula (Q4):** each row's lease is
      `GREATEST(WORKER_LEASE_DURATION, monitor timeout + 5s)`. The
      `WORKER_LEASE_DURATION > DEFAULT_CHECK_TIMEOUT` config rule stays as it
      is. The per-row formula is what covers monitors that set their own
      timeout of up to 60s, which would otherwise equal the default lease.
    - All lease and schedule times come from PostgreSQL's `now()`, never the
      worker's clock, so clock skew between workers cannot matter.
    - Neither the claim nor the record touches `updated_at`, which tracks
      edits to the monitor definition, not scheduling.
    - The returned `next_check_at` is kept by the scheduler as the "claimed
      next_check_at" for D4.
  - **D4 — Recording a result is lease-guarded and atomic.** It is one
    statement: if the lease update matches no row, nothing is inserted.
    ```sql
    -- name: RecordCheckResult :one
    WITH released AS (
        UPDATE monitors
        SET last_checked_at  = now(),
            last_status_code = sqlc.narg('status_code'),
            last_latency_ms  = sqlc.arg('latency_ms'),
            next_check_at    = CASE
                WHEN next_check_at = sqlc.arg('claimed_next_check_at')::timestamptz
                    THEN now() + make_interval(secs => interval_seconds)
                ELSE next_check_at  -- a manual request arrived mid-check; keep it
            END,
            lease_owner = NULL,
            lease_until = NULL
        WHERE id = sqlc.arg('monitor_id')
          AND lease_owner = sqlc.arg('worker_id')
          AND deleted_at IS NULL
        RETURNING id
    )
    INSERT INTO monitor_checks (monitor_id, success, status_code, latency_ms,
                                error_kind, error_message, checked_at)
    -- Explicit casts: sqlc cannot infer parameter types in a SELECT list.
    SELECT id, sqlc.arg('success')::boolean, sqlc.narg('status_code')::int,
           sqlc.arg('latency_ms')::int, sqlc.narg('error_kind')::varchar,
           sqlc.narg('error_message')::text, now()
    FROM released
    RETURNING id;

    -- name: ReleaseLease :execrows
    UPDATE monitors
    SET lease_owner = NULL, lease_until = NULL
    WHERE id = sqlc.arg('monitor_id')
      AND lease_owner = sqlc.arg('worker_id');
    ```
    - **No row returned (`pgx.ErrNoRows`) means the lease was lost.** It
      expired and another worker reclaimed it, or the monitor was
      soft-deleted. This maps to `scheduler.ErrLeaseLost`, which is logged at
      warn level and the result is discarded. This is what keeps a late
      worker from writing a duplicate result.
    - The `claimed_next_check_at` comparison works because pgx round-trips
      PostgreSQL's microsecond timestamps exactly. A `PATCH` mid-check cannot
      change the value either: its `LEAST(...)` keeps a value that is already
      in the past.
    - The next run is scheduled from completion (`now() + interval`), not
      from the old slot, so there are no catch-up bursts after downtime.
    - `ReleaseLease` leaves `next_check_at` alone, so a released monitor is
      still due and is reclaimed on the next poll.
    - State, `consecutive_failures` and incidents are **not** touched. That
      is Sprint 4, which widens this into its single check-result
      transaction.
    - The implementation is `repository.SchedulerStore` in
      `internal/repository/scheduler.go`, with methods `Claim`, `Record` and
      `Release`. It translates errors the same way `MonitorStore` does.
  - **D5 — Checker (`internal/checker`).**
    - **Transport:** one shared `*http.Transport`, with values as package
      constants rather than environment variables:
      - dialer `Timeout` 10s and `KeepAlive` 30s
      - `TLSHandshakeTimeout` 10s
      - `MaxIdleConns` 100 and `MaxIdleConnsPerHost` 4
      - `IdleConnTimeout` 90s
      - `ForceAttemptHTTP2` true
      - `Proxy: nil`, so the environment's proxy settings are ignored
      The dialer is built behind a hook so that Sprint 5's validating SSRF
      dialer drops in without restructuring.
    - **Client:** `http.Client{Timeout: 0}`. Each check runs under
      `context.WithTimeout(parent, monitor.Timeout())`, and that per-request
      deadline is the only time limit.
    - **Redirects:** `CheckRedirect` allows at most 5 redirects, then returns
      a sentinel error that is classified as `redirect`. The final response
      is the one evaluated.
    - **Request:** a `GET` with `User-Agent: PulseGo/1.0`.
    - **Response body:** read through `io.LimitReader(body, 64 KiB)` into
      `io.Discard`, then closed. It is never stored or logged (Rule 10).
    - **Latency:** measured from request start until response headers
      arrive. On failure, it is the time elapsed until the failure.
    - **Signature:** `Check(ctx, domain.Monitor) (domain.CheckOutcome,
      error)`. The error is non-nil only when the *caller's* context was
      cancelled (shutdown). That case is not a check result and is never
      recorded.
    - **Classification, in this order:**
      1. Parent context cancelled → the error above.
      2. Deadline exceeded, or `net.Error.Timeout()` → `timeout`. A DNS
         lookup cut off by the deadline counts as a timeout.
      3. `*net.DNSError` → `dns`.
      4. The redirect sentinel → `redirect`.
      5. `*tls.CertificateVerificationError`, `x509.*` errors,
         `tls.RecordHeaderError` or `tls.AlertError` → `tls`.
      6. `*net.OpError`, `ECONNREFUSED`, `ECONNRESET`, `EHOSTUNREACH`,
         `ENETUNREACH`, `io.EOF` or `io.ErrUnexpectedEOF` → `connection`.
      7. Anything else → `unknown`.
      A status outside `[expected_status_min, expected_status_max]` is
      `unexpected_status`, with a message like
      `status 503 outside expected range 200-399`.
    - **`error_message`:**
      - Unwrap the `*url.Error` first, so the URL is never stored: its query
        string may carry a secret.
      - Truncate to 512 bytes on a UTF-8 rune boundary.
      - It is `nil` on success.
  - **D6 — Scheduler (`internal/scheduler`) and `cmd/worker`.**
    - **Interfaces** are declared in the scheduler package (ROADMAP §3):
      - `Store`: `Claim`, `Record` and `Release`, as in D3 and D4.
      - `Checker`: `Check`, as in D5.
      `ErrLeaseLost` lives here too.
    - **Concurrency bound:**
      - A token channel with capacity `WORKER_CONCURRENCY`. The poll loop is
        its only acquirer, so `cap - len` is an exact count of free slots.
      - Each tick claims `min(WORKER_BATCH_SIZE, free slots)`, and skips the
        claim entirely when no slot is free.
      - Each claimed monitor takes a token, runs check → record (or release)
        in one goroutine, then returns the token. So at most N check
        goroutines exist at once.
      - Because nothing claimed ever waits in a queue, a lease cannot run out
        before its check starts.
    - **Loop:** one claim at startup, then one every `WORKER_POLL_INTERVAL`.
      A failed claim is logged and retried on the next tick. There is no
      immediate re-poll when a batch comes back full.
    - **Shutdown:**
      - Checks run under a context of their own, not the signal context, so
        a signal does not abort them.
      - On the signal, claiming stops and in-flight checks get
        `SHUTDOWN_TIMEOUT` to finish.
      - After that their context is cancelled, each cancelled check calls
        `Release`, and `Run` returns once every goroutine has exited.
      - Database writes after the signal use
        `context.WithTimeout(context.WithoutCancel(ctx), 5s)`.
    - **Wiring:** `cmd/worker/main.go` replaces the ping loop with
      `scheduler.New(...).Run(ctx)`, using the existing `newWorkerID()`.
    - Workers are told apart in logs by their `worker_id`; no other
      per-worker identity is added.
  - **D7 — Manual check and check history (`internal/monitor`,
    `internal/httpapi`).**
    - **`POST /v1/monitors/{monitorID}/checks`:**
      - The service first calls `Get`, which returns 404 for a missing or
        soft-deleted monitor.
      - A disabled monitor returns `domain.ErrMonitorDisabled`, which becomes
        **409 problem+json** (Q3) with type
        `urn:pulsego:problem:monitor-disabled`.
      - Otherwise it runs `RequestCheck`:
        ```sql
        -- name: RequestCheck :one
        UPDATE monitors SET next_check_at = now()
        WHERE id = $1 AND deleted_at IS NULL
        RETURNING *;
        ```
      - The response is **202 Accepted** with the monitor representation.
      - A monitor disabled between the `Get` and the update is harmless,
        because the claim filters on `enabled`.
    - **`GET /v1/monitors/{monitorID}/checks?limit=&cursor=`:**
      - 404 for a missing or deleted monitor. An existing monitor with no
        checks gets 200 with an empty `data`.
      - Same envelope `{data, next_cursor}` and the same
        `DefaultPageSize`/`MaxPageSize` as the monitor list. The limit+1 trick
        detects whether there is a next page.
      - Query:
        ```sql
        -- name: ListChecks :many
        SELECT * FROM monitor_checks
        WHERE monitor_id = sqlc.arg('monitor_id')
          AND (sqlc.narg('cursor_checked_at')::timestamptz IS NULL
               OR (checked_at, id) < (sqlc.narg('cursor_checked_at')::timestamptz,
                                      sqlc.arg('cursor_id')::bigint))
        ORDER BY checked_at DESC, id DESC
        LIMIT sqlc.arg('page_limit');
        ```
    - **Cursors:**
      - `cursor.go` gains a check cursor (`checked_at` plus an `int64` id),
        using the same base64url-JSON encoding as the monitor cursor.
      - Each payload carries a kind field, so a monitor cursor sent to
        `/checks` (or the other way round) is a 400, not a silently wrong
        page.
      - The existing monitor cursor stays decodable.
    - **Responses:** a check is returned as `id`, `success`, `status_code`,
      `latency_ms`, `error_kind`, `error_message` and `checked_at`.
    - Both operations, plus the `MonitorDisabled` (409) response, go into
      `api/openapi.yaml`. `TestOpenAPIMatchesRouter` must then see
      8 operations.
  - **D8 — No new environment variables.** The 5s lease margin, the checker's
    transport constants, the 64 KiB drain cap and the 5-redirect limit are
    all code constants.
  - **D9 — Commit order.** One concern per commit, all on this branch:
    1. `docs:` PROGRESS.md status update plus this plan (this commit).
    2. `test:` support `PULSEGO_TEST_DATABASE_URL` for integration tests (D1).
    3. `feat:` add the `monitor_checks` migration, `checks.sql` queries,
       generated code and `domain/check.go` (D2, plus the D3/D4/D7 SQL), with
       the truncate-helper change.
    4. `feat:` add claim, record and release to the repository (D3, D4), with
       their integration tests, including parallel claimers.
    5. `feat:` add the HTTP checker with error classification (D5).
    6. `feat:` add the scheduler and wire it into `cmd/worker` (D6).
    7. `feat:` add the manual check and check history endpoints and OpenAPI
       (D7).
    8. `docs:` close out Sprint 3 in PROGRESS.md, after the gate (D11).
  - **D10 — Test list** (ROADMAP §5a categories). The DoD item each test
    proves is in brackets.
    - **Unit tests** (no tag):
      - **Checker**, against httptest servers:
        - status in range → success
        - status out of range → `unexpected_status`
        - a handler sleeping 2s against a 200ms timeout → `timeout`,
          returning in well under 2s [DoD 3]
        - a closed listener → `connection`
        - an untrusted `httptest.NewTLSServer` → `tls`
        - a redirect loop → `redirect`
        - `User-Agent` sent
        - body not retained
        - parent cancellation → an error, not an outcome
      - **Classification:** table tests with synthetic wrapped errors,
        including `*net.DNSError` → `dns` and a DNS timeout → `timeout`.
      - **Error messages:** rune-safe truncation at 512, and the URL
        stripped.
      - **Concurrency bound:** a blocking handler, N=3, batch size 10. Track
        the maximum in-flight with an atomic and assert it is exactly 3,
        never more [DoD 3].
      - **Scheduler**, with a fake store and checker:
        - never claims more than the free slots
        - skips the claim when the slots are full
        - shutdown waits for in-flight checks
        - checks that are still running when the timeout ends are cancelled
          and released, not recorded
        - `ErrLeaseLost` is logged and swallowed
      - **Handlers:** 202, 404 and 409 for the manual check; pagination,
        the 400 on a wrong-kind cursor and 404 for history.
      - **Cursors:** round trip for the check cursor, and kind mismatch
        rejected.
    - **Integration tests** (`//go:build integration`, in
      `internal/repository`):
      - `TestClaimParallelClaimersNeverDoubleAssign` [DoD 1]:
        - 300 due monitors and 8 claimers, each with its own pool
          connection, started together by a barrier.
        - Batch size 7; each claimer loops until it claims nothing.
        - 5 rounds, re-arming the monitors between rounds.
        - Asserts every monitor was claimed exactly once per round, the
          union is all 300, and the `lease_owner` in the database equals the
          claimer.
      - `TestClaimSkipsRowsLockedByOpenTransaction` [DoD 1]: transaction A
        claims and stays open, then B claims under `statement_timeout=2s`. B
        gets a disjoint, non-empty set immediately. This proves it is
        `SKIP LOCKED` specifically: plain `FOR UPDATE` would block and time
        out.
      - `TestExpiredLeaseReclaimedByDifferentWorker` [DoD 2]: worker A's
        lease is pushed into the past with direct SQL, worker B reclaims it,
        and A's late `Record` returns `ErrLeaseLost` with no
        `monitor_checks` row written.
      - `TestClaimIgnoresActiveLease`: an active lease is skipped.
      - `TestClaimSkipsDisabledDeletedAndNotDue`: disabled, deleted and
        not-yet-due monitors are skipped.
      - `TestClaimRespectsBatchSize`: a claim never returns more than the
        batch size.
      - `TestClaimLeaseCoversMonitorTimeout`: a monitor with a 60s timeout
        and a 10s lease gets `lease_until ≥ now + 65s` (Q4).
      - `TestRecordCheckResult`: the row is inserted, the `last_*` fields
        are updated, the lease is cleared, and `next_check_at ≈ now +
        interval`.
      - `TestRecordPreservesManualRequestDuringCheck`.
      - `TestReleaseLease`: only the owner can release; the monitor stays
        due.
      - `TestRequestCheck`: sets `next_check_at` to now; a deleted monitor
        gives `ErrNotFound`.
      - `TestListChecksKeysetPagination`: includes rows that share a
        `checked_at` [DoD 4].
      - `TestMonitorChecksConstraints`: one case per D2 constraint, in the
        style of `TestMonitorsConstraints`.
      - **Migrations:** `TestMigrationsUpDown` also asserts that
        `monitor_checks` appears and disappears.
      - **End to end:** the real `scheduler` + `checker` +
        `SchedulerStore`, with an httptest target. Create a monitor, and a
        check row appears within the poll interval plus the timeout
        [DoD 3, DoD 4].
  - **D11 — Gate before reporting done.**
    - Run the full `/sprint-done` gate (`gofmt -l`, `go vet`,
      `go tool golangci-lint run`, `go test -race ./...`,
      `go tool govulncheck ./...`, and the three `-o bin/` builds) **plus**
      `make test-integration`.
    - Run the integration suite with `PULSEGO_TEST_DATABASE_URL` set (after
      `pg-test-up`), and without it wherever a Docker host is available.
    - If the Testcontainers mode cannot be run in the sandbox, report it as
      **not run**. Do not claim it passed.
    - Record the real output under these Notes.
    - **Do not merge or push.** Squash-merge waits for explicit user
      approval (Rule 2).
- **Gate results (2026-11-11)**, D11, run on merge commit `c864afd`
  (Go 1.27.3 in the agent-office sandbox, `GOTOOLCHAIN=local`):
  - **golangci-lint bump:** the pinned golangci-lint v2.12.2 could not run on
    Go 1.27.3. Its `golang.org/x/tools` v0.49.0 cannot decode Go 1.27
    export data (`export data version 5 is greater than maximum supported
    version 4`), so it stopped on a typecheck error importing the standard
    library and linted nothing. The `go.mod` tool directive was bumped to
    **v2.15.0** (commit `8c1ca74`, lint style fixes in `04f3ab7`) and merged
    into this branch. The gate below used golangci-lint v2.15.0.
  - `go mod tidy` → no diff.
  - `gofmt -l .` → empty.
  - `go vet ./...` → rc=0 (also rc=0 with `-tags integration`).
  - `go tool golangci-lint run` → `0 issues.` (also `0 issues.` with
    `--build-tags integration`).
  - `go test -race -count=1 ./...` → `ok` for checker, config, domain,
    httpapi, monitor, scheduler and telemetry; rc=0.
  - `go tool govulncheck ./...` → rc=3 in the sandbox: one standard-library
    finding, `GO-2026-6859` ("HTTP/1 server crash due to MaxBytesReader
    header race in net/http"), in `net/http@go1.27.3` and fixed in
    `net/http@go1.27.4`. It came from the sandbox toolchain, not from Sprint 3
    code, and there were no findings in imported packages or required modules.
    The temporary waiver recorded here was **removed** after the host
    re-run on go1.27.4 below came back clean.
  - `go build -o bin/api ./cmd/api`, `go build -o bin/worker ./cmd/worker`,
    `go build -o bin/migrate ./cmd/migrate` → all rc=0.
  - **Integration suite, `PULSEGO_TEST_DATABASE_URL` mode** (local
    PostgreSQL 17 via `pg-test-up`). Ran
    `go test -race -tags integration -count=1 -timeout 15m ./...`, the
    `make test-integration` recipe, run directly because `make` is not
    installed in the sandbox. Two consecutive runs were green: all 8
    packages `ok`, rc=0 (repository 1.310s / 1.320s, scheduler
    1.790s / 1.791s). Every D10 integration test ran and passed. Leftover
    `pulsego_test_*` databases afterwards: **0**.
  - **Testcontainers mode** (variable unset): not run in the sandbox
    (no Docker). It was run on the host; see below.
- **Host gate re-run (2026-11-11)**, on the developer host against commit
  `b9acf29`, with **`GOTOOLCHAIN=go1.27.4`** set for every Go command.
  `go version` reported `go1.27.4 linux/amd64`. The distro Go is still
  go1.27.3, and `go.mod` was left unchanged: still `go 1.26.5`, with no
  `toolchain` line.
  - `gofmt -l .` (go1.27.4's gofmt) → empty.
  - `go vet ./...` → rc=0.
  - `go tool golangci-lint run` (v2.15.0, built with go1.27.4) →
    `0 issues.`
  - `go test -race ./...` → `ok` for checker, config, domain, httpapi,
    monitor, scheduler and telemetry; rc=0.
  - `go tool govulncheck ./...` (govulncheck v1.7.0, Go go1.27.4, DB updated
    2026-11-11) → **`No vulnerabilities found.`**, rc=0. GO-2026-6859
    no longer applies.
  - The three `go build -o bin/{api,worker,migrate} ./cmd/...` builds → all
    rc=0.
  - **Integration suite, Testcontainers mode:** ran
    `make test-integration` (GNU Make 4.4.1, Docker 29.9.3) with
    `PULSEGO_TEST_DATABASE_URL` unset. All 8 packages `ok`, rc=0, in 50s
    (repository 42.5s). Docker events for that window show 6
    `postgres:17-alpine` containers and the `testcontainers/ryuk:0.14.0`
    reaper starting.
  - A targeted `-v` rerun of the 11 tests cited in the DoD above, in the
    same mode, gave `--- PASS` for every one, with none skipped. The three D1
    tests in `testdb_integration_test.go` `SKIP` in this mode by design,
    because they apply only when the variable is set. They passed in the
    sandbox URL-mode runs above.
  - **Flake that did not reproduce:** the first integration run, on the
    pre-merge base `197ebb2`, failed to link the scheduler and telemetry
    test binaries (`runtime/race/internal/amd64v1(.text): relocation
    target __popcountdi2 not defined`, `[build failed]`). Every later run
    passed: a targeted rerun of those two packages with
    `-race -count=1`, two full reruns on `197ebb2` and three full reruns
    on `c864afd`, so 5/5 full reruns were green. Recorded as a one-off
    race-runtime linker flake, not a code defect.
- Known risk accepted for this sprint: there is **no SSRF protection until
  Sprint 5**. From the D6 commit on, the worker sends requests to any URL,
  including loopback and cloud metadata addresses. `ALLOW_PRIVATE_TARGETS` is
  validated by config but enforced by nothing yet. Do not run the worker
  against untrusted monitor input in a shared environment before Sprint 5.
  D5's dialer hook is the seam Sprint 5's validating dialer plugs into.

---

## Sprint 4 — State and Incidents
Status: **Done** (2026-11-11)
Branch: `feature/sprint-04-state-incidents` — squash-merged into `main` as `e4586e4`

Definition of Done:
- [x] State transition table covered by unit tests for every edge
      (`TestTransition`, `TestTransitionCoversEveryAllowedEdge`,
      `TestTransitionNeverLeavesAllowedEdges`, `TestTransitionErrors`)
- [x] Incident open/resolve is transactional and race-safe
      (`TestRecordLifecycleOpensAndResolvesIncident`,
      `TestRecordConcurrentResultsForOneMonitor`,
      `TestRecordReadsStateUnderRowLock`,
      `TestRecordRollsBackOnIncidentFailure`)
- [x] Only one unresolved incident per monitor is possible (DB-enforced)
      (`TestIncidentsOneUnresolvedPerMonitor`,
      `TestIncidentsConcurrentUnresolvedInsert`)
- [x] Stats endpoint correct for all three windows (24h/7d/30d)
      (`TestMonitorStatsWindows`, `TestMonitorStatsEndpoint` in
      `internal/repository` and `internal/httpapi`)

Notes:
- Plan approved 2026-11-11 (approval given in advance by the user for this
  sprint; the user reviews these decisions before the squash-merge).
  Implementation runs in agent-office, following the D10 commit order below.
  The decisions below are binding and self-contained — an implementer should
  not need the planning conversation. Anything not settled here is a question
  for the user, not a guess (CLAUDE.md, "Ask, don't guess").
- Decisions taken at sprint start (D1–D12 of the approved plan):
  - **D1 — The state machine is a pure domain function**, in
    `internal/domain/state.go`.
    - **Signature:**
      `func Transition(cur State, consecutiveFailures int32, threshold int16, success bool) (StateChange, error)`.
      - `StateChange{From, To State; ConsecutiveFailures int32; Incident IncidentAction}`.
      - `IncidentAction` is one of `IncidentNone`, `IncidentOpen` and
        `IncidentResolve`.
      - An invalid `cur`, or a threshold below 1, returns an error.
    - **Allowed edges** are exactly ROADMAP's, plus self-loops:
      unknown→up, unknown→degraded, up→up, up→degraded, degraded→up,
      degraded→degraded, degraded→down, down→down and down→up. Nothing else
      is allowed: no up→down, unknown→down or down→degraded.
    - **Success:** failures reset to 0, and `To` is up, from any state.
    - **Failure:** failures become the previous count + 1, saturating at
      `MaxInt32`.
      - From unknown or up → degraded.
      - From degraded → down if the new failure count is ≥ the threshold,
        otherwise degraded.
      - From down → down.
    - **Incidents:** `IncidentOpen` only on degraded→down; `IncidentResolve`
      only on down→up.
    - **Consequence, recorded explicitly:** because up→down is not an edge, a
      monitor reaches down after `max(failure_threshold, 2)` consecutive
      failures. With threshold 1, the first failure gives degraded and the
      second gives down. Update the `StateDegraded`/`StateDown` doc comments
      in `internal/domain/monitor.go` to say so. The threshold limits do not
      change.
    - **Lowering a threshold mid-outage** takes effect on the next failure: a
      degraded monitor whose failures are ≥ the new threshold goes down on
      its next failure.
  - **D2 — `incidents` schema**, as migration
    `db/migrations/00004_create_incidents.sql` (goose, with
    `StatementBegin`/`StatementEnd` around each statement, as in `00003`):
    ```sql
    -- +goose Up
    -- +goose StatementBegin
    CREATE TABLE incidents (
        id               UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
        monitor_id       UUID        NOT NULL REFERENCES monitors (id) ON DELETE CASCADE,
        started_at       TIMESTAMPTZ NOT NULL,
        resolved_at      TIMESTAMPTZ,
        opening_check_id BIGINT      REFERENCES monitor_checks (id) ON DELETE SET NULL,
        closing_check_id BIGINT      REFERENCES monitor_checks (id) ON DELETE SET NULL,
        cause            TEXT,
        created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
        CONSTRAINT incidents_resolved_after_start
            CHECK (resolved_at IS NULL OR resolved_at >= started_at),
        CONSTRAINT incidents_closing_only_when_resolved
            CHECK (closing_check_id IS NULL OR resolved_at IS NOT NULL),
        CONSTRAINT incidents_cause_length
            CHECK (cause IS NULL OR length(cause) <= 512)
    );
    -- +goose StatementEnd

    -- DoD 3: the database, not the application, guarantees one open incident.
    -- +goose StatementBegin
    CREATE UNIQUE INDEX incidents_one_unresolved_per_monitor
        ON incidents (monitor_id) WHERE resolved_at IS NULL;
    -- +goose StatementEnd

    -- +goose StatementBegin
    CREATE INDEX incidents_history_idx
        ON incidents (monitor_id, started_at DESC, id DESC);
    -- +goose StatementEnd

    -- Keep retention's ON DELETE SET NULL from scanning incidents per deleted check.
    -- +goose StatementBegin
    CREATE INDEX incidents_opening_check_idx
        ON incidents (opening_check_id) WHERE opening_check_id IS NOT NULL;
    -- +goose StatementEnd

    -- +goose StatementBegin
    CREATE INDEX incidents_closing_check_idx
        ON incidents (closing_check_id) WHERE closing_check_id IS NOT NULL;
    -- +goose StatementEnd

    -- +goose Down
    -- +goose StatementBegin
    DROP TABLE incidents;
    -- +goose StatementEnd
    ```
    - The check foreign keys are nullable and `ON DELETE SET NULL`, so
      retention (D8) can delete old checks that an incident points at.
    - **`cause`** is `"<error_kind>: <error_message>"` of the opening check,
      or just `<error_kind>` when there is no message. It is truncated to
      512 bytes on a rune boundary in Go; reuse the checker's truncation
      helper, extracting it into `domain` if needed. It is never a response
      body (Rule 10).
    - **`started_at`** is the opening check's `checked_at`, and
      **`resolved_at`** is the closing check's `checked_at`. Both are `now()`
      in the same transaction, so they are identical to the check row's
      value.
    - `internal/domain/incident.go` holds an `Incident` struct that mirrors
      the row.
    - **Required test-helper change:** `truncateMonitors` becomes
      `TRUNCATE incidents, monitor_checks, monitors`.
  - **D3 — Recording a result is one transaction.** It replaces Sprint 3's
    single-statement `RecordCheckResult`, which is deleted.
    `SchedulerStore.Record` keeps its parameters and becomes:
    1. `BEGIN` (READ COMMITTED).
    2. `LockLeasedMonitor :one`:
       ```sql
       SELECT * FROM monitors
       WHERE id = $monitor_id AND lease_owner = $worker_id AND deleted_at IS NULL
       FOR UPDATE;
       ```
       No row → roll back and return `scheduler.ErrLeaseLost` (semantics
       unchanged).
    3. Compute `domain.Transition` from the **locked row's** `state`,
       `consecutive_failures` and `failure_threshold`, never from the
       claim-time snapshot.
    4. `InsertCheck :one` inserts the `monitor_checks` row with
       `checked_at = now()`, `RETURNING id`.
    5. `ApplyCheckResult :exec` updates `monitors`: `state`,
       `consecutive_failures`, `last_checked_at = now()`,
       `last_status_code`, `last_latency_ms`, and `next_check_at` with
       Sprint 3's
       `CASE WHEN next_check_at = claimed_next_check_at THEN now() + interval ELSE next_check_at END`,
       and sets `lease_owner`/`lease_until` to NULL. `updated_at` is left
       untouched.
    6. Depending on the incident action:
       - `IncidentOpen` → `OpenIncident :execrows`:
         ```sql
         INSERT … VALUES (monitor_id, now(), check_id, cause)
         ON CONFLICT (monitor_id) WHERE resolved_at IS NULL DO NOTHING;
         ```
         0 rows means an incident is already open, which is only possible if
         state and incidents were edited by hand. It is not an error: the
         store's caller logs it at warn via the returned change. A
         constraint error here would wedge the monitor forever.
       - `IncidentResolve` → `ResolveIncident :execrows`:
         ```sql
         UPDATE incidents SET resolved_at = now(), closing_check_id = $check_id
         WHERE monitor_id = $1 AND resolved_at IS NULL;
         ```
         0 rows is not an error.
    7. `COMMIT`. Any error → roll back. Nothing of the result persists, and
       the lease stays held until it expires; it is then reclaimed, as in
       Sprint 3.
    - **Race-safety argument** (write it down in the code):
      - Every writer of a result holds the monitor row lock from step 2
        until commit, so results for one monitor serialize.
      - The lease guard means a second, concurrent recorder finds no row.
      - The partial unique index is the final guarantee.
    - **Interfaces:**
      - `NewSchedulerStore` takes
        `interface{ sqlcgen.DBTX; Begin(context.Context) (pgx.Tx, error) }`,
        which `*pgxpool.Pool` satisfies.
      - `Record` returns `(domain.StateChange, error)`, and the scheduler's
        `Store` interface changes to match.
      - When `From != To`, the scheduler logs `monitor state changed` at
        info, with `monitor_id`, `from`, `to` and the incident action. No
        other scheduler behaviour changes.
    - The claim (`ClaimDueMonitors`) is unchanged. Its `SKIP LOCKED` simply
      skips a row that is being recorded.
  - **D4 — `GET /v1/monitors/{monitorID}/incidents?limit=&cursor=`.**
    - 404 for a missing or soft-deleted monitor; the service calls `Get`
      first, as check history does. An existing monitor with no incidents
      gets 200 with an empty `data`.
    - Same `{data, next_cursor}` envelope, the same
      `DefaultPageSize`/`MaxPageSize`, and the limit+1 trick.
    - Order is `started_at DESC, id DESC`, with the keyset
      `(started_at, id) < ($cursor_started_at, $cursor_id::uuid)`. The query
      is `ListIncidents :many` in `db/queries/incidents.sql`.
    - **Cursors:** a new cursor kind `incident` (`started_at` plus a UUID) in
      `internal/httpapi/cursor.go`. A monitor or check cursor sent here, or
      an incident cursor sent elsewhere, is a 400.
    - **Responses:** an incident is returned as `id`, `monitor_id`,
      `started_at`, `resolved_at`, `opening_check_id`, `closing_check_id`,
      `cause` and `created_at`. Nullable fields are JSON `null`.
  - **D5 — `GET /v1/monitors/{monitorID}/stats?window=24h|7d|30d`.**
    - `window` omitted → `24h`. Any other value → 400 validation
      problem+json naming the field `window`. 404 for a missing or deleted
      monitor.
    - **Query:** `GetMonitorStats :one` in `db/queries/stats.sql`. It is one
      statement over the monitor's `monitor_checks` rows with
      `checked_at > now() - make_interval(secs => $window_seconds)`, so the
      start is exclusive and the end, `now()`, is inclusive. It selects:
      - `count(*)`
      - `count(*) FILTER (WHERE success)`
      - `avg(latency_ms) FILTER (WHERE success)`
      - `percentile_disc(0.95) WITHIN GROUP (ORDER BY latency_ms) FILTER (WHERE success)`
      - `now()` as the window end
      The existing `monitor_checks_history_idx` serves it.
    - **Latency stats cover successful checks only**, because a timeout's
      latency is the timeout, not a response time. p95 uses
      `percentile_disc`, so it is an observed value, in whole ms.
    - **Response:** `{window, from, to, total_checks, successful_checks,
      uptime_percent, avg_latency_ms, p95_latency_ms}`.
      - `uptime_percent = successful / total * 100` and `avg_latency_ms` are
        rounded to 2 decimals in Go, in `internal/domain/stats.go` (pure and
        unit-tested).
      - With 0 checks, `uptime_percent` is null. With 0 successful checks,
        `avg_latency_ms` and `p95_latency_ms` are null.
    - Windows are relative to the database's `now()`. If
      `CHECK_RETENTION_DAYS` is below 30, the 30d window only covers
      retained data; this is documented in the OpenAPI spec and the README.
  - **D6 — OpenAPI and router.** Both operations go into `api/openapi.yaml`,
    with the `Incident`, `IncidentPage` and `MonitorStats` schemas and the
    400 for a bad window or cursor. `TestOpenAPIMatchesRouter` must then see
    10 operations.
  - **D7 — All Sprint 4 SQL lands in one commit.** The D2 migration and the
    D3, D4, D5 and D8 queries go in together with the `sqlc generate`
    output. Later sub-tasks do not edit `db/` or `sqlcgen` without the
    lead's say-so.
  - **D8 — Retention cleanup runs in `cmd/worker`.**
    - Query:
      ```sql
      -- name: DeleteExpiredChecks :execrows
      DELETE FROM monitor_checks
      WHERE id IN (
          SELECT id FROM monitor_checks
          WHERE checked_at < now() - make_interval(days => sqlc.arg('retention_days')::int)
          ORDER BY checked_at
          LIMIT sqlc.arg('batch_size')
          FOR UPDATE SKIP LOCKED
      );
      ```
    - `internal/scheduler/retention.go` holds a `Retention` runner with its
      own consumer interface:
      `RetentionStore{ DeleteExpiredChecks(ctx, retentionDays, batchSize int32) (int64, error) }`.
    - **Loop:**
      - It runs once at startup, then every hour.
      - Each run deletes in batches of 5000 until a batch returns fewer
        rows, then logs the total deleted at info.
      - An error is logged and retried on the next tick.
      - It stops when the signal context is done.
      - The interval and batch size are constants, not environment
        variables.
    - **Wiring:** `cmd/worker` starts it next to the scheduler, with
      `CHECK_RETENTION_DAYS`.
    - Several workers running it at once is safe: `SKIP LOCKED`, and
      deletes are idempotent. Incidents keep their rows; their check
      references become NULL.
  - **D9 — No new environment variables.** The retention interval and batch
    size, the cause length and the stats rounding are all code constants.
  - **D10 — Commit order.** One concern per commit. Each agent-office
    sub-task commits on its own branch, and they are merged at QA:
    1. `docs:` this plan plus the status update (this commit).
    2. `feat:` the state transition function, with table tests (D1).
    3. `feat:` the incidents migration, all Sprint 4 queries, generated code,
       the domain incident type and the truncate-helper change, with
       migration and constraint tests (D2, D7).
    4. `feat:` transactional check-result recording with incidents (D3),
       with integration tests.
    5. `feat:` the incident history endpoint (D4, D6).
    6. `feat:` the stats endpoint (D5, D6).
    7. `feat:` retention cleanup in the worker (D8).
    8. `docs:` README, and close out Sprint 4 after the gate (D12).
  - **D11 — Test list** (ROADMAP §5a categories). The DoD item each test
    proves is in brackets.
    - **Unit tests** (no tag):
      - `TestTransition`: a table with one named case per edge, covering all
        9 edges, including degraded→degraded below the threshold,
        degraded→down at the threshold, threshold 1 behaviour, and down→down
        incrementing the count. Each case asserts `To`, the failure count and
        the incident action [DoD 1].
      - `TestTransitionNeverLeavesAllowedEdges`: exhaustive over states ×
        success × failures 0..12 × thresholds 1..10 [DoD 1].
      - **Transition errors and saturation:** an invalid state or threshold
        returns an error; the failure count saturates.
      - **Stats maths:** rounding, zero totals, and all checks failed.
      - **Cause:** formatting and truncation.
      - **Handlers:** incidents 200, 404, pagination and the 400 on a
        wrong-kind cursor; stats 200 for each window, the default window,
        the 400 on a bad window, and 404.
      - **Cursors:** round trip for the incident kind.
      - **Retention runner**, with a fake store: batches until a short
        batch, retries after an error, stops on cancel.
      - **Scheduler:** logs the state change.
    - **Integration tests** (`//go:build integration`, in
      `internal/repository`):
      - `TestIncidentsOneUnresolvedPerMonitor` [DoD 3]: a second unresolved
        insert for one monitor fails with SQLSTATE 23505 on
        `incidents_one_unresolved_per_monitor`. One unresolved plus one
        resolved, and two resolved, are fine. Other monitors are
        unaffected.
      - `TestIncidentsConcurrentUnresolvedInsert` [DoD 3]: two transactions
        insert an unresolved incident for one monitor concurrently; exactly
        one commits.
      - `TestIncidentsConstraints`: one case per D2 CHECK constraint.
      - **Migrations:** `TestMigrationsUpDown` also asserts that
        `incidents` appears and disappears.
      - `TestRecordLifecycleOpensAndResolvesIncident` [DoD 2]: unknown → up
        → degraded → degraded → down (one open incident, with
        `opening_check_id` = that check, `started_at` = its `checked_at`,
        and `cause` set) → down (no second incident) → up (resolved, with
        `closing_check_id` set) → degraded → down (a second incident).
      - `TestRecordConcurrentResultsForOneMonitor` [DoD 2]:
        - A monitor is degraded at threshold − 1 and leased. 16 goroutines
          behind a barrier, each with its own connection, Record a failure.
          Exactly 1 returns nil and 15 return `ErrLeaseLost`; there is 1
          check row, the state is down, and there is exactly 1 unresolved
          incident.
        - Then 30 re-armed rounds (lease set via SQL) of 8 concurrent
          recorders with random success or failure. After every round,
          exactly one recorder succeeded, the check row count equals the
          round count, and (state = down) ⇔ exactly one unresolved
          incident; never more than one.
      - `TestRecordReadsStateUnderRowLock` [DoD 2]: an open transaction
        holds `SELECT … FOR UPDATE` on the monitor, and Record blocks (not
        done after 200ms). The transaction sets `consecutive_failures` to
        threshold − 1 and the state to degraded, then commits. Record then
        transitions to down and opens an incident, which proves it reads
        the locked row.
      - `TestRecordRollsBackOnIncidentFailure` [DoD 2]: a test-only trigger
        raises on `INSERT INTO incidents`. Record at the threshold returns
        an error (not `ErrLeaseLost`). There is no check row, the monitor's
        state, failures and `last_*` fields are unchanged, and the lease is
        still held.
      - The existing `TestRecordCheckResult`,
        `TestRecordPreservesManualRequestDuringCheck`,
        `TestExpiredLeaseReclaimedByDifferentWorker` and the scheduler
        end-to-end test keep passing on the new Record.
      - `TestListIncidentsKeysetPagination`: includes rows that share a
        `started_at`.
      - `TestMonitorStatsWindows` [DoD 4]:
        - Checks are inserted with explicit `checked_at` values about 1h,
          2d, 10d and 40d ago, with mixed success and failure, known
          latencies, and none near a window boundary.
        - It asserts the exact total, successful count, avg and p95 for
          24h, 7d and 30d.
        - Another monitor's checks are excluded.
        - An empty monitor gives zeros and nulls.
      - An HTTP-level test hits the stats endpoint for all three windows
        [DoD 4].
      - `TestDeleteExpiredChecks`: only rows older than the retention period
        are deleted, the batch size is respected, incident check references
        become NULL, and incident rows remain.
  - **D12 — Gate before reporting done.**
    - Run the full `/sprint-done` gate: `gofmt -l`, `go vet` (also with
      `-tags integration`), `go tool golangci-lint run` (also with
      `--build-tags integration`), `go test -race ./...`,
      `go tool govulncheck ./...`, and the three `-o bin/` builds.
    - **Plus** the integration suite,
      `go test -race -tags integration -count=1 -timeout 15m ./...`, with
      `PULSEGO_TEST_DATABASE_URL` set after `pg-test-up`.
    - Testcontainers mode is reported as **not run** (no Docker in the
      sandbox).
    - Record the real output under these Notes.
    - **Do not merge or push** (Rule 2).
- **Gate results (2026-11-11)**, D12, run on the D12 branch, whose base
  (`bcdffe5`) holds the work of every Sprint 4 sub-task (Go 1.27.4 in the
  agent-office sandbox, `GOTOOLCHAIN=local`):
  - `go mod tidy` → no diff. `go tool sqlc generate` → no diff.
  - `gofmt -l .` → empty.
  - `go vet ./...` → rc=0 (also rc=0 with `-tags integration`).
  - `go tool golangci-lint run` (v2.15.0) → `0 issues.` (also `0 issues.`
    with `--build-tags integration`).
  - `go test -race -count=1 ./...` → `ok` for checker, config, domain,
    httpapi, monitor, scheduler and telemetry; rc=0.
  - `go tool govulncheck ./...` (govulncheck v1.7.0, Go go1.27.4, DB updated
    2026-11-11) → `No vulnerabilities found.`, rc=0. No waiver needed.
  - `go build -o bin/api ./cmd/api`, `go build -o bin/worker ./cmd/worker`,
    `go build -o bin/migrate ./cmd/migrate` → all rc=0.
  - **Integration suite, `PULSEGO_TEST_DATABASE_URL` mode** (local
    PostgreSQL 17 via `pg-test-up`), with
    `go test -race -tags integration -count=1 -timeout 15m ./...`:
    - Run 1 failed to **link** the scheduler test binary
      (`runtime/race/internal/amd64v1(.text): relocation target
      __popcountdi2 not defined`, `FAIL … internal/scheduler [build
      failed]`). Every other package was `ok`. This is the same
      race-runtime linker flake recorded under Sprint 3; no test ran and
      failed.
    - Runs 2 and 3, consecutive: all 8 packages `ok`, rc=0 (repository
      2.058s / 2.050s, scheduler 1.806s / 1.808s).
    - Targeted `-v` run of every DoD test above, plus
      `TestIncidentsConstraints`, `TestListIncidentsKeysetPagination`,
      `TestMigrationsUpDown` and `TestDeleteExpiredChecks`: `--- PASS` for
      all 17 (`TestMonitorStatsEndpoint` exists in two packages), 0 skipped,
      rc=0.
    - Leftover `pulsego_test_*` databases afterwards: **0**.
  - **Testcontainers mode** (variable unset): **not run**. The sandbox has
    no Docker. (Since run on the host; see the host gate below.)
  - README updated: incident and stats endpoints, the state machine and the
    `max(failure_threshold, 2)` rule, the incident lifecycle, retention and
    the 30d/retention caveat, and the status line.
- **Host gate (2026-11-11)**, `/sprint-done` on the developer host, on
  `feature/sprint-04-state-incidents` at `aab9e7a` (the QA merge), with
  `GOTOOLCHAIN=go1.27.4` set for every Go command (`go version` →
  `go1.27.4 linux/amd64`):
  - `go mod tidy` → no diff. `go tool sqlc generate` → no diff.
  - `gofmt -l .` → empty.
  - `go vet ./...` → rc=0 (also rc=0 with `-tags integration`).
  - `go tool golangci-lint run` → `0 issues.` (also `0 issues.` with
    `--build-tags integration`).
  - `go test -race -count=1 ./...` → `ok` for checker, config, domain,
    httpapi, monitor, scheduler and telemetry; rc=0. No race-runtime linker
    flake on the host.
  - `go tool govulncheck ./...` → `No vulnerabilities found.`, rc=0.
  - `go build -o bin/api ./cmd/api`, `go build -o bin/worker ./cmd/worker`,
    `go build -o bin/migrate ./cmd/migrate` → all rc=0.
  - **Integration suite, Testcontainers mode** (`PULSEGO_TEST_DATABASE_URL`
    unset, Docker 29.9.3, Testcontainers-Go v0.44.0, `postgres:17-alpine`):
    - `make test-integration` → all 8 test packages `ok`, rc=0, 43s wall
      clock (repository 42.3s, scheduler 1.8s).
    - `-v -count=1` run of `internal/repository` and `internal/scheduler`:
      64 top-level `--- PASS`, 0 `--- FAIL`, 3 `--- SKIP`. The three skips
      are the URL-mode-only `testdb` helper tests
      (`TestSharedPoolNeverUsesTheURLDatabase`,
      `TestCreateTestDatabaseIsNamedAndDroppedWithForce`,
      `TestCreateTestDatabaseWithoutCreateDBNamesThePrivilege`), which skip
      by design when the variable is unset.
    - Every DoD test above, plus `TestIncidentsConstraints`,
      `TestListIncidentsKeysetPagination`, `TestMigrationsUpDown` and
      `TestDeleteExpiredChecks`: `--- PASS`.
    - Containers left behind afterwards: **0**.
  - Both integration modes have now passed for Sprint 4.
- Review notes (2026-11-11, before the merge):
  - Monitor-list cursors issued before cursors carried a kind (Sprint 2
    format `{"t","i"}`) are still accepted by `GET /v1/monitors`:
    `decodeCursor` lets an empty kind through, and its logic is unchanged
    from `main`. Covered by `TestLegacyMonitorCursorStillDecodes`. Incident
    and check cursors still require their kind, so a kind-less cursor is
    never read as one of them (`TestIncidentCursorKindMismatchIsRejected`).
  - **Decision (user, 2026-11-11): `failure_threshold: 1` is documented, not
    rejected.** It stays a valid value and behaves like 2 (D1): reaching
    down takes `max(failure_threshold, 2)` consecutive failures. Rejecting
    it would have been a breaking API change plus a migration rewriting
    existing rows. The limits stay 1–10. The README and the domain doc
    comments already said so. The OpenAPI `state` description, which said
    degraded means "`failure_threshold` has not been reached" and so was
    wrong for threshold 1, now states the rule.
- Known risk still open: no SSRF protection until Sprint 5 (see Sprint 3
  Notes).

---

## Sprint 5 — Production Hardening
Status: **Done** (2026-11-11)
Branch: `feature/sprint-05-hardening` — squash-merged into `main` as `44b0461`

Definition of Done:
- [x] SSRF test suite covers loopback/private/link-local/redirect-to-private
      cases and all pass
      (`TestValidateURL`, `TestBlockedReason`,
      `TestErrorsNeverLeakQueryOrUserinfo`, `TestDialerRejectsPrivateAnswer`,
      `TestDialerRejectsBlockedLiteral`, `TestDialerRejectsMixedAnswers`,
      `TestDialerDialsValidatedIP`, `TestControlCheck`,
      `TestRedirectToPrivateIsBlocked`, `TestRedirectLimit`,
      `TestCheckerBlocksLoopbackTarget`, `TestCheckRedirectToPrivateIsBlocked`,
      `TestClassifyBlockedFirst`, `TestCreateMonitorRejectsPrivateTarget`,
      `TestValidateURLAppliesSSRFPolicy`, `TestUpdateChecksPolicyOnlyForNewURL`,
      `TestMigration00005BlockedErrorKind`)
- [x] All 10 Prometheus metrics exposed with bounded cardinality
      (`TestMetricsAllTenRegistered`, `TestMetricsCardinalityBounded`,
      `TestMetricsLabelValues`, `TestNormalizeMethod`, `TestNormalizeRoute`,
      `TestObserveRecordsRouteTemplates`, `TestInternalRouterServesMetrics`,
      `TestPublicRouterDoesNotServeMetrics`,
      `TestQueryTracerRecordsOperationNames`, plus the manual exposition
      lists in the gate results below). Read as "all 10 across the
      deployment, each emitted once observed": see D14 and `WAIVERS.md`.
- [x] Traces exported via OTLP; app starts fine with `OTEL_ENABLED=false`
      (`TestTracingExportsToOTLPReceiver`,
      `TestTracingDisabledMakesNoRequests`,
      `TestTracingUnreachableEndpointDoesNotBlock`,
      `TestObserveStartsServerSpanWithPropagatedParent`,
      `TestQueryTracerRecordsMetricAndSpan`, `TestSpansForOneCheck`,
      `TestCheckSpanOnSuccess`, plus both binaries started with
      `OTEL_ENABLED=false` in the gate below)
- [x] `go test -race ./...` and the full CI pipeline are green
      (`go test -race` green in the gate below; the CI pipeline part is
      verified by the CI jobs `checks` and `integration` — **waiting for CI
      after push**, see Notes)
- [x] Docker image builds, runs as non-root, passes its health check
      (verified by the CI job `docker` — **waiting for CI after push**, see
      Notes; there is no Docker in the sandbox)

Notes:
- Plan approved 2026-11-11 (approval given in advance by the user for this
  sprint; the user reviews these decisions before the squash-merge).
  Implementation runs in agent-office, following the D11 commit order below.
  The decisions below are binding and self-contained — an implementer should
  not need the planning conversation. Anything not settled here is a question
  for the user, not a guess (CLAUDE.md, "Ask, don't guess").
- Decisions taken at sprint start (D1–D13 of the approved plan):
  - **D1 — The SSRF policy lives in `internal/security`.** Stdlib only, and
    no I/O except through the injected resolver.
    - **Policy:** `type Policy struct{ AllowPrivate bool }`, built from
      `ALLOW_PRIVATE_TARGETS`.
    - **`Policy.ValidateURL(raw string) error`** is static: no DNS. Rules:
      - The scheme is `http` or `https`, the URL parses, and the host is
        not empty.
      - No userinfo.
      - The port is empty, 80 or 443 (any port 1–65535 when
        `AllowPrivate`).
      - IPv6 zone identifiers are rejected.
      - When not `AllowPrivate`:
        - A literal IP host must pass the address check.
        - `localhost` and `*.localhost` are rejected.
        - Non-canonical numeric hosts (`127.1`, `2130706433`,
          `0x7f.0.0.1`, octal forms) are rejected as ambiguous.
    - **`BlockedReason(netip.Addr) (reason string, blocked bool)`.** An
      IPv4-mapped IPv6 address is unmapped first. Blocked ranges:
      - Unspecified: `0.0.0.0`, `::`, and `0.0.0.0/8`.
      - Loopback: `127.0.0.0/8`, `::1`.
      - Private: `10.0.0.0/8`, `172.16.0.0/12`, `192.168.0.0/16`.
      - Shared/CGNAT: `100.64.0.0/10`.
      - Link-local: `169.254.0.0/16` (including the `169.254.169.254`
        metadata address), `fe80::/10`.
      - Multicast: `224.0.0.0/4`, `ff00::/8`.
      - Broadcast `255.255.255.255` and reserved `240.0.0.0/4`.
      - IETF protocol assignments: `192.0.0.0/24`.
      - Benchmarking: `198.18.0.0/15`.
      - Documentation: `192.0.2.0/24`, `198.51.100.0/24`,
        `203.0.113.0/24`, `2001:db8::/32`.
      - IPv6 ULA `fc00::/7` and site-local `fec0::/10`.
      - NAT64: `64:ff9b::/96` and `64:ff9b:1::/48`.
      - 6to4 `2002::/16` and Teredo `2001::/32`.
    - **`Dialer`** (`NewDialer(policy, opts...)`,
      `DialContext(ctx, network, addr)`):
      1. Splits `host:port` and checks the port.
      2. Resolves the host **once** through an injectable resolver
         (`LookupNetIP(ctx, network, host) ([]netip.Addr, error)`; the
         default is `net.DefaultResolver`). Zero answers is a DNS error.
      3. If **any** answer is blocked, the whole host is rejected (mixed
         answers, DNS rebinding).
      4. Dials the validated IP literals in order
         (`net.JoinHostPort(ip.String(), port)`) — never the host name
         again, so there is no TOCTOU re-resolution.
      - Defense in depth: the inner `net.Dialer.Control` re-checks the
        actual socket address.
      - With `AllowPrivate`, the range check is skipped, but
        resolve-then-dial-the-IP still applies.
      - Options `WithResolver` and `WithDialFunc` let tests inject answers
        and route connections to an httptest server.
    - **`RedirectPolicy(policy, max int)`** returns an
      `http.Client.CheckRedirect` func: at most `max` (5) redirects, and
      every hop's URL passes `ValidateURL`. The dialer then checks the
      resolved IPs of the hop.
    - **Errors:** every rejection wraps the sentinel
      `security.ErrBlockedTarget` (`errors.Is`). Messages carry the reason
      and the host only — never the full URL, the query or userinfo.
  - **D2 — SSRF wiring.**
    - **New error kind `blocked`** (`domain.ErrorKindBlocked`):
      - Migration `00005_error_kind_blocked.sql` recreates
        `monitor_checks_error_kind_valid` with `'blocked'`. Its Down maps
        `blocked` rows to `unknown`, then restores the old constraint.
      - The `error_kind` enum in `api/openapi.yaml` gains `blocked`.
      - `classify` maps `errors.Is(err, security.ErrBlockedTarget)` to
        `blocked`, before any other rule.
    - **Checker:** `checker.New(policy security.Policy, opts...)`. The
      policy is a required argument, so an unprotected checker cannot be
      built by accident.
      - It installs the security dialer and
        `RedirectPolicy(policy, MaxRedirects)`.
      - It validates `m.URL` before the request: blocked, without dialing.
      - Existing checker tests that hit httptest on `127.0.0.1` use
        `Policy{AllowPrivate: true}`.
    - **The persisted `error_message` never contains the URL.** Use the
      inner error of `*url.Error`, so query strings cannot leak (Rule 10).
    - **API:** monitor create and update run `Policy.ValidateURL` (static
      only, no DNS: DNS in a handler is slow and a TOCTOU trap; the dialer
      is the real guarantee). A rejection returns the existing 422
      validation problem on field `url`.
    - **Wiring:** `cmd/api` passes `ALLOW_PRIVATE_TARGETS` into the monitor
      service; `cmd/worker` passes it into the checker.
    - **README** gets an "SSRF protection" section, including the dev-only
      risk of `ALLOW_PRIVATE_TARGETS=true`.
  - **D3 — Prometheus metrics**, in `internal/telemetry/metrics.go`, on an
    own `*prometheus.Registry` (plus the Go and process collectors) — never
    the global one. Exactly these 10:
    1. `pulsego_http_requests_total` — counter {method, route, code}.
    2. `pulsego_http_request_duration_seconds` — histogram {method, route},
       default buckets.
    3. `pulsego_monitor_checks_total` — counter
       {result=success|failure, error_kind=none|<kind>}.
    4. `pulsego_monitor_check_duration_seconds` — histogram {result},
       buckets 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30.
    5. `pulsego_monitor_state_total` — counter {from, to}, incremented when
       a recorded result changes the state (from ≠ to).
    6. `pulsego_worker_claimed_monitors_total` — counter, no labels.
    7. `pulsego_worker_active_checks` — gauge, no labels.
    8. `pulsego_worker_claim_errors_total` — counter, no labels.
    9. `pulsego_scheduler_lag_seconds` — histogram of claim time − the
       claimed `next_check_at`, per monitor, clamped to ≥ 0; buckets 0.1,
       0.5, 1, 2, 5, 10, 30, 60, 120, 300.
    10. `pulsego_db_operation_duration_seconds` — histogram
        {operation, outcome=ok|error}. `operation` is the sqlc query name,
        parsed from the `-- name: X` prefix of the SQL; anything else is
        `other`.
    - **Cardinality rules:**
      - `method` is normalised to GET, HEAD, POST, PUT, PATCH, DELETE or
        OPTIONS, else `OTHER`.
      - `route` is the chi route pattern, or `unmatched`.
      - `code` is the 3-digit status.
      - `error_kind` and the states are checked against the domain enums
        (else `unknown`).
      - Never URLs, monitor IDs, error messages or request IDs.
    - Both binaries register all 10. `/metrics` is served only on
      `INTERNAL_HTTP_ADDR`.
  - **D4 — Tracing**, in `internal/telemetry/tracing.go`:
    `SetupTracing(ctx, TracingConfig{Enabled, Endpoint, ServiceName, Environment}) (shutdown func(context.Context) error, err error)`.
    - **Disabled:** noop provider, no-op shutdown, no network at all.
    - **Enabled:**
      - `otlptracehttp` (OTLP/HTTP protobuf, not gRPC — fewer
        dependencies), with the endpoint as a full URL (e.g.
        `http://otel-collector:4318`).
      - Batch span processor; `ParentBased(AlwaysSample)`.
      - Resource attributes `service.name` and `deployment.environment`.
      - Global W3C TraceContext + Baggage propagator.
      - Creating the exporter never dials. Export errors go to slog at warn
        via `otel.SetErrorHandler`.
      - Shutdown is bounded by `SHUTDOWN_TIMEOUT`; its error is logged,
        never fatal.
      - An unreachable collector must never block startup or requests.
    - **Config:** when `OTEL_ENABLED=true`, `OTEL_EXPORTER_OTLP_ENDPOINT`
      must be an http(s) URL with a host; otherwise it is a config error.
    - **Spans.** No headers, no query strings, no bodies, no API key, no SQL
      arguments.
      - Incoming HTTP: a `"<METHOD> <route>"` server span
        (`http.request.method`, `http.route`, `http.response.status_code`).
      - DB: a `"db <operation>"` client span from the pgx tracer
        (`db.system.name=postgresql`, `db.operation.name`; no statement
        text or arguments).
      - Worker claim cycle: `scheduler.claim` (the claimed count).
      - Per check: a `scheduler.check` root span with the children:
        - `checker.check`, a client span (`server.address` = host only,
          `http.response.status_code`, `pulsego.error_kind`);
        - `monitor.record`, with the events `incident.opened` /
          `incident.resolved` and the `state.from` / `state.to`
          attributes.
  - **D5 — One pgx `QueryTracer`** in `internal/telemetry` produces both the
    DB metric and the DB span. `repository.NewPool` takes it as an optional
    argument (nil allowed).
  - **D6 — HTTP surface.**
    - **One `Observe` middleware** on the public router records metrics 1–2
      and the server span. The route is read from chi after the handler
      ran. The internal router is not instrumented (scrapes would be
      noise).
    - **`/readyz`** (GET and HEAD) on both the public and the internal
      router:
      - Pings PostgreSQL through a `Pinger` interface with a 2s timeout.
      - 200 `{"status":"ready"}`; otherwise 503 problem+json with type
        `urn:pulsego:problem:not-ready` and no error detail in the body
        (the error is logged at warn).
      - `/healthz` stays DB-free.
      - The public `/readyz` goes into `api/openapi.yaml`.
    - **`/metrics`** (GET) on the internal router only.
      `NewInternalRouter` takes the pinger and the metrics handler.
  - **D7 — Worker observability.**
    - The worker gets an internal HTTP server on `INTERNAL_HTTP_ADDR`
      (`/healthz`, `/readyz`, `/metrics`), with graceful shutdown.
    - The scheduler takes a consumer-side `Metrics` interface and records
      metrics 3–9 and the D4 worker spans.
    - `make run-worker` sets `INTERNAL_HTTP_ADDR=:9091`, so it does not
      collide with a local api.
  - **D8 — Docker and Compose.**
    - **Dockerfile:** multi-stage, runtime kept on `alpine:3.22`.
      - The shell and busybox `wget` are kept for the health check and for
        debugging. Distroless was considered and rejected, because it needs
        a probe binary.
      - `CGO_ENABLED=0`, `-trimpath`, ca-certificates. The GNU `wget`
        package is dropped (busybox `wget` is enough).
      - Numeric non-root `USER 10001:10001`.
      - `HEALTHCHECK` does a GET on `http://127.0.0.1:9090/healthz`.
      - `GO_VERSION=1.27` (`golang:1.27-alpine`): the latest Go 1.27
        patch, independent of `go.mod`'s `go` directive (see D15).
      - No migration at startup: the `migrate` image stays a one-shot job,
        and its health check is disabled in Compose.
    - **Compose:**
      - The worker gets a health check.
      - Profile `observability` adds otel-collector (OTLP/HTTP 4318 in,
        Jaeger out), jaeger, prometheus (scrapes `api:9090` and
        `worker:9090`) and grafana with a provisioned Prometheus
        datasource.
      - Files: `deployments/prometheus.yml`,
        `deployments/otel-collector.yaml`,
        `deployments/grafana-datasources.yaml`.
      - Tracing stays off unless `OTEL_ENABLED=true`.
    - `.dockerignore` also excludes `.env.*` (keeping `!.env.example`),
      `*.test`, the root-level `/worker` and `/migrate` binaries (anchored,
      so `cmd/worker` and `cmd/migrate` stay in the context), and
      `.idea`/`.vscode`, so env files and local artefacts never reach the
      build context (commit 027a7e0).
  - **D9 — CI**, in `.github/workflows/ci.yml`. It runs on every `push` and
    `pull_request`, with `permissions: contents: read`, and concurrency
    cancels superseded runs.
    - **Job `checks`:** `gofmt -l .` must be empty; `go mod tidy -diff`;
      `go tool sqlc diff`; `go vet ./...` (plus `-tags integration`);
      `go tool golangci-lint run` (plus `--build-tags integration`);
      `go test -race ./...`; `go tool govulncheck ./...`; and the three
      `go build -o bin/...` builds.
    - **Job `integration`:** Testcontainers mode —
      `go test -race -tags integration -count=1 -timeout 15m ./...` with
      `PULSEGO_TEST_DATABASE_URL` unset.
    - **Job `docker`:**
      - `docker build` of the api, worker and migrate images.
      - Then it runs postgres, the migrate image, and the api and worker
        images on one network.
      - It waits (bounded) until `docker inspect` reports both `healthy`,
        asserts the container user is non-root (`id -u` ≠ 0), and checks
        that `/readyz` returns 200.
      - Logs are dumped on failure.
    - **Actions:** only `actions/checkout` and `actions/setup-go`
      (`go-version: '1.27.x'`, `check-latest: true`, with
      `GOTOOLCHAIN: local` at workflow level; see D15), each pinned to a
      full commit SHA with a `# vX.Y.Z` comment. golangci-lint and govulncheck run via `go tool`,
      so there is no other third-party action.
    - The Docker build, the container health check and Testcontainers mode
      are verified only by GitHub Actions, after the user pushes (no Docker
      in the sandbox).
  - **D10 — No new environment variables.** New modules only:
    - `github.com/prometheus/client_golang`;
    - `go.opentelemetry.io/otel` (plus `sdk`, `trace` and
      `exporters/otlp/otlptrace/otlptracehttp`);
    - `go.opentelemetry.io/proto/otlp`, for the test receiver.
    - No otelhttp/otelpgx contrib: our own middleware and tracer keep the
      labels and attributes under our control.
    - Only the telemetry sub-task adds modules.
  - **D11 — Commit order.** One concern per commit. Each agent-office
    sub-task commits on its own branch, and they are merged at QA:
    1. `docs:` this plan plus the status update (this commit).
    2. `feat:` `internal/security`, with the SSRF unit suite (D1).
    3. `feat:` SSRF wiring: the blocked error kind, migration 00005, the
       checker, API validation and the README section (D2).
    4. `feat:` telemetry foundation: metrics, tracing, the pgx tracer and
       the config endpoint check (D3–D5, D10).
    5. `feat:` API observability: middleware, `/readyz`, `/metrics`, the
       pool tracer and the api wiring (D6).
    6. `feat:` worker observability: the internal server, scheduler metrics
       and spans (D7).
    7. `build:` Dockerfile, the Compose observability profile, the CI
       workflow and the Makefile (D8, D9).
    8. `docs:` README observability section, and close out Sprint 5 after
       the gate (D13).
  - **D12 — Test list.** The DoD item each test proves is in brackets.
    - **Unit tests** (no tag):
      - `TestValidateURL`: schemes, credentials, malformed URLs, ports,
        localhost, ambiguous numeric hosts, and literal blocked IPs
        including `::ffff:127.0.0.1` [SSRF].
      - `TestBlockedReason`: one case per range, plus public addresses
        allowed [SSRF].
      - `TestDialerRejectsPrivateAnswer`, `TestDialerRejectsMixedAnswers`,
        `TestDialerDialsValidatedIP` (with a rebinding resolver, the inner
        dial receives exactly the validated IP) [SSRF].
      - `TestRedirectToPrivateIsBlocked` and `TestRedirectLimit` (httptest
        via the injected dial): a redirect to `127.0.0.1`, to
        `169.254.169.254`, or to a name resolving to `10.x` gives
        error_kind `blocked`, and the blocked hop is never dialled [SSRF].
      - `TestCheckerBlocksLoopbackTarget` [SSRF].
      - API: `TestCreateMonitorRejectsPrivateTarget` [SSRF].
      - `TestMetricsAllTenRegistered` and `TestMetricsCardinalityBounded`
        (thousands of distinct paths, IDs, methods and errors stay within a
        fixed series count, and no label holds a UUID or URL) [metrics].
      - `TestTracingExportsToOTLPReceiver` (an httptest OTLP/HTTP receiver
        decodes the protobuf and sees spans with `service.name`),
        `TestTracingDisabledMakesNoRequests` and
        `TestTracingUnreachableEndpointDoesNotBlock` [traces].
      - `TestReadyz` (200/503 with a fake pinger) and
        `TestInternalRouterServesMetrics` [metrics].
      - Scheduler metric tests with a fake `Metrics` [metrics].
      - Config tests for the endpoint check.
    - **Integration tests** (`-tags integration`):
      - Migration 00005 up/down.
      - `TestReadyzWithDatabase`.
      - The pgx tracer records operation names against real PostgreSQL.
      - The existing suite already covers the ROADMAP list (migrations,
        CRUD, claims, SKIP LOCKED, lease reclaim, the record transaction,
        incident uniqueness, pagination, soft delete, stats). The close-out
        sub-task audits it and fills gaps.
  - **D13 — Gate before reporting done.**
    - Run the full `/sprint-done` gate: `gofmt -l`, `go vet` (also with
      `-tags integration`), `go tool golangci-lint run` (also with
      `--build-tags integration`), `go test -race ./...`,
      `go tool govulncheck ./...`, and the three `-o bin/` builds.
    - **Plus** the integration suite, with `PULSEGO_TEST_DATABASE_URL` set
      after `pg-test-up`.
    - **Plus** starting both binaries with `OTEL_ENABLED=false` against the
      sandbox PostgreSQL, and probing `/healthz`, `/readyz` and `/metrics`.
    - The Docker build, the container health check and Testcontainers mode
      are reported as **waiting for CI**.
    - Record the real output under these Notes.
    - **Do not merge or push** (Rule 2).
- **D14 — Metric exposition** (decided by the tech lead on 2026-11-11, during
  the D13 gate).
  - The D13 step "the 10 metric names appear on `/metrics` of both
    binaries" was too strict, and it failed as written:
    - a fresh api showed 9/10;
    - the worker always shows 8/10.
  - Why:
    - **HTTP metrics 1–2 are api-only.** Only the public router is
      instrumented (D6), and the worker has no public router.
    - **Label-vector series appear on first observation.** That is
      standard Prometheus behaviour; an empty vector is not written out.
    - **The `/readyz` ping is not a traced query.** So
      `pulsego_db_operation_duration_seconds` appears only after the first
      real query.
  - Fake pre-initialised series (e.g. HTTP routes on the worker) were
    rejected as misleading.
  - The revised step:
    - api `/metrics` shows all 10 names after one real `/v1` request;
    - worker `/metrics` shows its 8 (metrics 3–10) after one claim cycle.
  - Waiver recorded in `WAIVERS.md`.
- **D15 — Build toolchain vs. go directive.**
  - `go.mod`'s `go 1.26.5` stays as the minimum language/version
    requirement. No `toolchain` line.
  - CI (`actions/setup-go` with `go-version: '1.27.x'`, `check-latest: true`,
    and `GOTOOLCHAIN: local` so the go command never switches toolchains)
    and the Docker build (`golang:1.27-alpine`, `ARG GO_VERSION=1.27`) use
    the latest Go 1.27 patch, so the standard library carries every
    security fix.
  - Reason: CI `govulncheck` failed with ~20 standard-library
    vulnerabilities in go1.26.5, which setup-go had taken from `go.mod`.
    The sandbox (go1.27.4) never showed them.
  - `golang.org/x/net` bumped from v0.61.0 to v0.62.0 (indirect) for
    GO-2026-6866 and GO-2026-6860. `go mod tidy` changed nothing else.
  - Gate result (2026-11-11, Go go1.27.4, `GOTOOLCHAIN=local`): `gofmt -l`
    empty; `go vet` (± `-tags integration`) rc=0; `golangci-lint` (±
    `--build-tags integration`) `0 issues.`; `go test -race ./...` rc=0;
    `go tool govulncheck ./...` (v1.7.0, DB updated 2026-11-11)
    `No vulnerabilities found.` with no uncalled module findings; the three
    `go build -o bin/...` rc=0; `go mod tidy -diff` and `go tool sqlc diff`
    no diff; integration suite (`PULSEGO_TEST_DATABASE_URL` mode) every
    package `ok`, 0 leftover test databases; `ci.yml` parses with
    yaml.v3. The CI run on go1.27.x itself is **waiting for CI after
    push**.
- Resolved during implementation:
  - **PATCH skips the SSRF policy check when the request does not set
    `url`** (`a74a34f`, `TestUpdateChecksPolicyOnlyForNewURL`). A monitor
    stored before Sprint 5 with a now-disallowed URL can still be renamed
    or disabled. The syntax checks still run, and the dialer still blocks
    the target at check time (`error_kind` `blocked`).
  - **Unauthenticated `/v1` requests are labelled `route="/v1/*"`.** The
    API key middleware rejects them before chi matches the inner route
    (`TestObserveRecordsRouteTemplates`).
  - **Trace service names are `<SERVICE_NAME>-api` and
    `<SERVICE_NAME>-worker`**, by default `pulsego-api` and
    `pulsego-worker`, the same as the log `service` attribute.
  - **Prometheus is on host port 9092** in the `observability` profile
    (`PROMETHEUS_PORT`). Host 9090 is the api's internal port and 9091 is
    `make run-worker`.
- **Resolved: the Sprint 3/4 risk "no SSRF protection until Sprint 5".**
  - Every outbound check goes through `internal/security`: the policy, the
    validating dialer and the redirect policy (D1, D2).
  - Monitor create and update validate `url` against the policy.
  - `ALLOW_PRIVATE_TARGETS=true` is refused outside
    `ENVIRONMENT=development`.
- **Integration-suite audit** (D12, against the ROADMAP Sprint 5 list). No
  gap was found, so no test was added. The tests are in
  `internal/repository/*_integration_test.go`:
  - Migrations: `TestMigrationsUpDown`,
    `TestMigration00005BlockedErrorKind`, `TestMonitorsPartialIndex`,
    `TestMonitorsConstraints`, `TestMonitorChecksConstraints`,
    `TestIncidentsConstraints`, `TestIncidentsForeignKeys`.
  - CRUD: `TestCreateAndGet`, `TestGetMissingReturnsNotFound`,
    `TestConstraintViolationBecomesValidationError`, `TestUpdatePartial`,
    `TestUpdateShorteningIntervalPullsNextCheckForward`,
    `TestUpdateDoesNotPushNextCheckOut`, `TestUpdateMissingReturnsNotFound`,
    `TestListOrdersNewestFirst`, `TestListEmptyReturnsEmptySlice`.
  - Worker claims: `TestClaimIgnoresActiveLease`,
    `TestClaimSkipsDisabledDeletedAndNotDue`, `TestClaimRespectsBatchSize`,
    `TestClaimLeaseCoversMonitorTimeout`, `TestReleaseLease`,
    `TestRequestCheck`, `TestSchedulerEndToEnd`.
  - Concurrent `SKIP LOCKED`: `TestClaimParallelClaimersNeverDoubleAssign`,
    `TestClaimSkipsRowsLockedByOpenTransaction`.
  - Lease expiration/reclaim: `TestExpiredLeaseReclaimedByDifferentWorker`,
    `TestRecordOnDeletedMonitorLosesLease`.
  - Check-result transaction: `TestRecordCheckResult`,
    `TestRecordPreservesManualRequestDuringCheck`,
    `TestRecordLifecycleOpensAndResolvesIncident`,
    `TestRecordReadsStateUnderRowLock`,
    `TestRecordRollsBackOnIncidentFailure`,
    `TestRecordWithIncidentAlreadyOpen`,
    `TestRecordConcurrentResultsForOneMonitor`.
  - Incident uniqueness: `TestIncidentsOneUnresolvedPerMonitor`,
    `TestIncidentsConcurrentUnresolvedInsert`.
  - Cursor pagination: `TestListKeysetPagination`,
    `TestListChecksKeysetPagination`, `TestListIncidentsKeysetPagination`
    (plus the `…EmptyReturnsEmptySlice` variants).
  - Soft delete: `TestSoftDelete`, `TestSoftDeleteClearsLease`
    (claims and records skip deleted monitors, see above).
  - Stats: `TestMonitorStatsWindows`, `TestMonitorStatsEndpoint`.
  - Also covered (Sprint 5 additions and retention):
    `TestReadyzWithDatabase`, `TestNewPoolInstallsQueryTracer`,
    `TestQueryTracerRecordsOperationNames`, `TestDeleteExpiredChecks`,
    `TestQAConcurrentRetentionCleanersDeleteEachRowOnce`.
- **Base check before the gate.** The close-out base lacked `a74a34f` (the
  monitor PATCH fix). Per the tech lead, it was merged with
  `git merge --no-ff a74a34f` (merge commit `c01fc7b`; it touches
  `internal/monitor` only). Every other Sprint 5 commit was already in the
  base: `87d1604`, `1c9d131`, `be80a6b`, `ba11427`, `6042b61`, `a2bce16` and
  `027a7e0`. `make run-worker` still sets `INTERNAL_HTTP_ADDR=:9091`. The
  full gate was re-run after the merge; the results below are from that run.
- **Gate results (2026-11-11)**, D13, run on `c01fc7b` plus the README
  changes (Go 1.27.4 in the agent-office sandbox, `GOTOOLCHAIN=local`):
  - `gofmt -l .` → empty.
  - `go vet ./...` → rc=0 (also rc=0 with `-tags integration`).
  - `go tool golangci-lint run` (v2.15.0) → `0 issues.` (also `0 issues.`
    with `--build-tags integration`).
  - `go test -race -count=1 ./...` → `ok` for checker, config, domain,
    httpapi, monitor, scheduler, security and telemetry; rc=0.
  - `go tool govulncheck ./...` (v1.7.0, DB updated 2026-11-11) →
    `No vulnerabilities found.`, rc=0. At the time it also reported 2
    vulnerabilities in `golang.org/x/net` v0.61.0 that the code does not
    call; they are gone since the D15 bump to v0.62.0. No waiver needed.
  - `go build -o bin/api ./cmd/api`, `go build -o bin/worker ./cmd/worker`,
    `go build -o bin/migrate ./cmd/migrate` → all rc=0.
  - `go mod tidy` → no diff in `go.mod`/`go.sum`. `go tool sqlc diff` →
    rc=0, no diff.
  - **Integration suite, `PULSEGO_TEST_DATABASE_URL` mode** (local
    PostgreSQL 17 via `pg-test-up`),
    `go test -race -tags integration -count=1 -timeout 15m ./...`:
    - every package `ok`, rc=0 (repository 2.165s, scheduler 1.866s);
    - no race-runtime linker flake, so `-p 1` was not needed;
    - leftover `pulsego_test_*` databases: **0**.
  - `make help` → rc=0, lists all 22 targets; `demo` reads "caller passes
    ENVIRONMENT=development ALLOW_PRIVATE_TARGETS=true (local only)".
  - D10 refusal:
    - `scripts/demo.sh` and `make demo` with both variables unset → `demo:
      refusing to start …`, exit 2;
    - `ENVIRONMENT=production ALLOW_PRIVATE_TARGETS=true scripts/demo.sh`
      → exit 2;
    - no api, worker or targets process was started.
  - D9 precedence, checked with a temporary `.env` copied from
    `.env.example` (removed afterwards) and a probe target that prints
    the variables:
    - with no variables set, `.env` gives `ALLOW_PRIVATE_TARGETS=false`;
    - `ALLOW_PRIVATE_TARGETS=true ENVIRONMENT=development make …` →
      `true`;
    - `make … LOG_LEVEL=debug ALLOW_PRIVATE_TARGETS=true` → both win;
    - `make demo` with that `.env` and no variables → still exit 2.
  - **Binary smoke test** (run with the `ab54a63` binaries; the merge
    changed no Go code). Setup:
    - a scratch database `pulsego_smoke` on the sandbox server;
    - `bin/migrate up` → `successfully migrated database to version: 5`;
    - `bin/api` on `127.0.0.1:18080` / internal `127.0.0.1:19090`;
    - `bin/worker` on internal `127.0.0.1:19091`;
    - both with `OTEL_ENABLED=false`.
  - Smoke-test results:
    - `/healthz` → 200 `{"status":"ok"}` and `/readyz` → 200
      `{"status":"ready"}` on the api public port, the api internal port
      and the worker internal port.
    - `/metrics` → 200 on both internal ports, 404 on the api public port.
    - api `/metrics`, after one `GET /v1/monitors` (200): all 10 —
      `pulsego_db_operation_duration_seconds`,
      `pulsego_http_request_duration_seconds`,
      `pulsego_http_requests_total`,
      `pulsego_monitor_check_duration_seconds`,
      `pulsego_monitor_checks_total`, `pulsego_monitor_state_total`,
      `pulsego_scheduler_lag_seconds`, `pulsego_worker_active_checks`,
      `pulsego_worker_claim_errors_total`,
      `pulsego_worker_claimed_monitors_total`.
    - worker `/metrics`, after at least one claim cycle (7s at a 5s poll
      interval): the 8 expected (metrics 3–10) —
      `pulsego_db_operation_duration_seconds`,
      `pulsego_monitor_check_duration_seconds`,
      `pulsego_monitor_checks_total`, `pulsego_monitor_state_total`,
      `pulsego_scheduler_lag_seconds`, `pulsego_worker_active_checks`,
      `pulsego_worker_claim_errors_total`,
      `pulsego_worker_claimed_monitors_total`. The HTTP metrics 1–2 are
      absent, as D14 expects.
    - SIGTERM to both → api `shutting down` … `server stopped` (public,
      internal) … `stopped`, exit 0; worker `scheduler stopping`
      (`in_flight` 0) … `scheduler stopped` … `server stopped` (internal)
      … `stopped`, exit 0.
  - **Waiting for CI after push** (no Docker in the sandbox), via
    `.github/workflows/ci.yml`:
    - job `checks`: the full CI pipeline part of DoD item 4;
    - job `integration`: the integration suite in Testcontainers mode;
    - job `docker`: the image builds, the non-root user, the health
      checks and `/readyz` (DoD item 5).
  - README updated:
    - a new Observability section: health vs readiness, the 10 metrics
      and their labels, cardinality rules, exposition behaviour, tracing,
      and the Compose `observability` profile with its URLs;
    - a new CI section;
    - the SSRF section now notes the PATCH rule;
    - the status line says Sprint 5 is complete, and the "no SSRF
      protection" warning is gone.

---

## Sprint 6 — Portfolio Polish
Status: **Done** (2026-11-11)
Branch: `feature/sprint-06-polish` (agent-office implements the sprint on
per-task branches merged into one integration branch; that branch is
squash-merged into `main` as `feature/sprint-06-polish` after user approval)

Definition of Done:
- [x] Dashboard renders live state from the API
      (`TestDashboardIndexNeedsNoAPIKey`, `TestDashboardServesStaticFiles`,
      `TestDashboardMissingFilesAre404`, `TestDashboardDoesNotOpenTheAPI`,
      `TestDashboardIndexIsCSPCompatible`, `TestOpenAPIMatchesRouter`,
      `TestNormalizeRoute`; `docs/screenshots/dashboard.png`,
      `incident-open.png`, `incident-resolved.png` and `docs/demo.gif`
      captured from a live api + worker by `scripts/demo.sh`; plus the
      dashboard routes in the smoke test in the Notes)
- [x] README is sufficient for another developer to run and understand the
      project unaided (every ROADMAP Sprint 6 README topic has its own
      section, including "Key Engineering Decisions" with all six required
      subsections, the Mermaid diagram, the CI badge and the screenshots;
      the smoke test in the Notes followed "Running locally without
      Compose". The from-scratch walkthrough is the D8 QA step; the Compose
      quick start needs Docker — **waiting for CI after push**, see Notes)
- [x] Project-level Definition of Done (ROADMAP.md §9) fully satisfied
      (item by item in the Notes; the Docker and GitHub Actions items are
      **waiting for CI after push**)

Notes:
- Plan approved 2026-11-11 (approval given in advance by the user for this
  sprint; the user reviews these decisions before the squash-merge).
  Implementation runs in agent-office, following the D8 work order below.
  The decisions below are binding and self-contained — an implementer should
  not need the planning conversation. Anything not settled here is a question
  for the user, not a guess.
- Decisions taken at sprint start (D1–D8 of the approved plan):
  - **D1 — Dashboard: static files, embedded, data from `/v1` only.**
    - **Files:** static HTML + CSS + vanilla JS in `web/static/`. No build
      step, no Node, no framework, no CDN.
    - **Embedding:** a `web` package (`web/embed.go`) embeds `web/static/`
      with `//go:embed`.
    - **Routes:** the api's public router serves `GET /` (`index.html`) and
      `GET /static/*`. These routes carry no data and need no API key.
    - **Data:** only the existing `/v1` endpoints — the monitor list,
      per-monitor `stats?window=24h` and per-monitor incidents. No new API
      endpoints, no server-side data rendering.
    - **API key:**
      - The user enters it in a form.
      - It is kept in `sessionStorage` only and sent in the existing auth
        header.
      - It is never placed in a URL and never logged (Rule 10).
    - **Per monitor it shows:**
      - name;
      - target host — the host only, never the query or userinfo;
      - state badge;
      - uptime % (24h) and p95 latency (24h);
      - last check: time, latency and status.
    - **Recent incidents list:** open/resolved, monitor, started, duration,
      cause.
    - **Behaviour:** auto-refresh every 10 s. Empty, loading and error
      states; a 401 re-prompts for the key.
    - **XSS:** every string from the API is inserted with `textContent`.
      No `innerHTML` with data.
    - **Response headers** on the dashboard routes:
      - `Content-Security-Policy: default-src 'self'` (so no inline script
        or style);
      - `X-Content-Type-Options: nosniff`;
      - `Referrer-Policy: no-referrer`.
    - **Metrics:** the new routes keep the `route` label bounded — the chi
      templates `/` and `/static/*`.
  - **D2 — IPv6 special-use ranges** left open by Sprint 5 D1 are now
    blocked in `security.BlockedReason`:
    - `::/96` — IPv4-compatible (deprecated);
    - `100::/64` — discard-only;
    - `2001:10::/28` — ORCHID (deprecated).
    - `::` keeps its "unspecified" reason and `::1` its "loopback" reason:
      the specific ranges are checked first.
    - Tests: table tests in `internal/security`, plus a dialer test.
  - **D3 — Demo tooling in a nested Go module `demo/`** (its own `go.mod`).
    chromedp therefore never enters the main module's dependency graph,
    `go test ./...`, the govulncheck scope or the binaries.
    - `demo/cmd/targets` serves local demo targets: one steady, one slow,
      and one that goes down and comes back on a schedule.
    - `demo/cmd/capture` drives headless Chromium with chromedp.
    - `scripts/demo.sh` orchestrates the whole run:
      1. migrate;
      2. start api + worker with `ALLOW_PRIVATE_TARGETS=true` (local demo
         only, Rule 8);
      3. start the targets;
      4. seed;
      5. capture;
      6. build the GIF with ffmpeg;
      7. clean up.
    - `scripts/seed-demo.sh` seeds monitors with curl against any running
      API (`API_URL`, `API_KEY`, optional target base URL).
    - Outputs:
      - `docs/screenshots/dashboard.png`;
      - `docs/screenshots/incident-open.png`;
      - `docs/screenshots/incident-resolved.png`;
      - `docs/demo.gif` (< 5 MB).
    - Makefile targets `demo-seed` and `demo`.
  - **D4 — README rewrite.** It covers every item of the ROADMAP Sprint 6
    scope, including:
    - a "Key Engineering Decisions" section;
    - an architecture diagram in Mermaid;
    - a CI status badge
      (`https://github.com/OzanAlpkaya/pulsego/actions/workflows/ci.yml/badge.svg`);
    - the dashboard, with the screenshots and the GIF.
  - **D5 — Scope limits.** No new services, no ORM, no new API endpoints,
    no schema change. `api/openapi.yaml` changes only if needed to document
    the dashboard routes as non-API; otherwise the route-coverage test
    exempts them with a stated reason.
  - **D6 — Project-level DoD (ROADMAP §9).** Each item gets an evidence
    line in these Notes: test names, gate output, or a CI job. Items that
    need Docker or GitHub Actions are marked **waiting for CI after push**,
    as in Sprint 5.
  - **D7 — Close-out gate.** The full `/sprint-done` gate, plus the
    integration suite run with `PULSEGO_TEST_DATABASE_URL` (local
    PostgreSQL 17, no Docker). The results are recorded in these Notes.
  - **D8 — Work order:**
    1. this plan, the IPv6 ranges (D2) and the dashboard (D1), in
       parallel;
    2. demo tooling and captures (D3);
    3. README (D4);
    4. close-out gate (D7);
    5. QA — which also follows the README local setup from scratch.
- Decisions taken during the sprint (follow-ups `301db2c` and `8de25be`):
  - **D9 — Values already in the environment win over `.env`.** The
    Makefile's `.env` include now lets values already in the environment
    (`VAR=value make …`) and make arguments (`make … VAR=value`) win over
    `.env`, for every variable. This is dotenv behaviour. So a `.env` copied
    from `.env.example` cannot silently override a command-line opt-in. The
    environment values are saved before the include and restored,
    unexpanded, after it.
  - **D10 — The caller opts in to `ALLOW_PRIVATE_TARGETS`; `demo.sh` never
    sets it.** This supersedes D3 step 2. The caller must run
    `ENVIRONMENT=development ALLOW_PRIVATE_TARGETS=true make demo` (local
    demo only). Otherwise `scripts/demo.sh` exits 2 before it starts
    anything (Rule 8). No committed file sets `ALLOW_PRIVATE_TARGETS=true`;
    `git grep` finds it only in comments, docs, usage text and config tests.
    The README demo section and the `.env` precedence note were aligned in
    `8de25be`.
- **Gate results (2026-11-11)**, D7, run on `8487b85`. That is the
  close-out branch with `8de25be` merged in, so it holds all Sprint 6 work:
  the plan, the IPv6 ranges, the dashboard, the demo, the README and the
  D9/D10 follow-ups. Run in the agent-office sandbox (Go 1.27.4,
  `GOTOOLCHAIN=local`). A first run on `ab54a63` gave the same results;
  the merge changes only `Makefile`, `README.md`, `scripts/demo.sh` and
  `scripts/seed-demo.sh`, with no Go code. Nothing needed fixing, so this
  close-out changes only `PROGRESS.md`.
  - `gofmt -l .` → empty.
  - `go vet ./...` → rc=0 (also rc=0 with `-tags integration`).
  - `go tool golangci-lint run` (v2.15.0) → `0 issues.` in 2.0s (also
    `0 issues.` with `--build-tags integration`).
  - `go test -race -count=1 ./...` → `ok` for checker, config, domain,
    httpapi, monitor, scheduler, security and telemetry; 172 top-level tests
    passed, 0 skipped, 0 failed; rc=0, about 12s wall-clock.
  - `go tool govulncheck ./...` (v1.7.0, DB `https://vuln.go.dev`) →
    `No vulnerabilities found.`, rc=0. No waiver needed.
  - `go build -o bin/api ./cmd/api`, `go build -o bin/worker ./cmd/worker`,
    `go build -o bin/migrate ./cmd/migrate` → all rc=0.
  - Nested demo module: `cd demo && go vet ./... && go build ./...` → both
    rc=0.
  - `go mod tidy` → no diff in `go.mod`/`go.sum`. `go tool sqlc diff` →
    rc=0, no diff.
  - **Integration suite, `PULSEGO_TEST_DATABASE_URL` mode** (local
    PostgreSQL 17 via `pg-test-up`, no Docker),
    `go test -race -tags integration -count=1 -timeout 15m ./...`:
    - every package `ok`, rc=0, about 6s wall-clock (repository 2.150s,
      scheduler 1.864s);
    - 229 top-level tests passed, 0 skipped, 0 failed (`-v` re-run), so
      no integration test fell back to Testcontainers or skipped;
    - no race-runtime linker flake, so `-p 1` was not needed;
    - leftover `pulsego_test_*` databases: **0**.
  - **Binary smoke test.** Setup:
    - a scratch database `pulsego_smoke` on the sandbox server, dropped
      afterwards;
    - `bin/migrate up` → `successfully migrated database to version: 5`;
    - `bin/api` on `127.0.0.1:18080` / internal `127.0.0.1:19090`;
    - `bin/worker` on internal `127.0.0.1:19091`, `WORKER_POLL_INTERVAL=2s`;
    - both with `OTEL_ENABLED=false` and the default
      `ALLOW_PRIVATE_TARGETS=false`.
  - Smoke-test results:
    - `/healthz` → 200 `{"status":"ok"}` and `/readyz` → 200
      `{"status":"ready"}` on the api public port, the api internal port
      and the worker internal port.
    - `/metrics` → 200 on both internal ports, 404 on the api public port.
      After the run the api exposed 10 `pulsego_*` metric names and the
      worker 8 (metrics 3–10), as W1 expects.
    - Dashboard: `GET /` → 200 `text/html`, `<title>PulseGo — Dashboard</title>`,
      with `Content-Security-Policy: default-src 'self'; base-uri 'none';
      form-action 'none'; frame-ancestors 'none'`,
      `X-Content-Type-Options: nosniff` and `Referrer-Policy: no-referrer`.
      `/static/style.css`, `/static/app.js` and `/static/favicon.svg` → 200
      with the right content types. `web/static/app.js` contains no
      `innerHTML`.
    - `POST /v1/monitors` for `http://127.0.0.1:18080/healthz` → 422
      `validation-error`, field `url` (private target rejected by default).
    - `POST /v1/monitors` for `https://example.com` → 201, `state`
      `unknown`. Within 0.1s, with no manual trigger, the worker claimed and
      checked it: `GET /v1/monitors/{id}/checks` returned one stored result,
      and the monitor showed `last_checked_at` set, `consecutive_failures` 1,
      `state` `degraded` and `next_check_at` 30s later. The result is
      `error_kind` `dns`, because the sandbox has no outbound network; the
      worker's `pulsego_monitor_checks_total{error_kind="dns",result="failure"}`
      was 1.
    - The API key does not appear in either log (`grep -c` → 0 for both).
    - SIGTERM to both → api `server stopped` (public, internal) …
      `stopped`; worker `scheduler stopping` … `scheduler stopped` …
      `server stopped` … `stopped`.
  - **No secrets committed** — scan of the working tree and all 13 commits
    of the history (`git grep` over `git rev-list --all`):
    - private-key headers, AWS `AKIA…`, GitHub `ghp_`/`github_pat_`, Slack
      `xox?-`, `sk-…` and Google `AIza…` patterns → no hits;
    - no `.env`, `*.pem`, `*.key`, `id_rsa` or credentials file was ever
      committed; the only env file is `.env.example`. `.env` and `.env.*`
      are git-ignored (except `.env.example`), and `.env` is also in
      `.dockerignore`;
    - `password`/`secret`/`token`/`api_key` assignments → only documented
      local or throwaway values: `POSTGRES_PASSWORD: pulsego` for the local
      Compose and CI database, `API_KEY=local-development-api-key`
      (`.env.example`, the Compose default, README), and
      `API_KEY=ci-only-throwaway-api-key` (CI). `scripts/demo.sh` generates
      a random key per run.
- **Project-level Definition of Done (ROADMAP §9)**, item by item. The CI
  jobs are in `.github/workflows/ci.yml`: `checks` (gofmt, vet, lint,
  `go test -race`, govulncheck, the three builds), `integration` (the
  suite in Testcontainers mode) and `docker` (builds the images, runs
  postgres + migrate + api + worker with the Compose settings, waits for
  `healthy`, checks the non-root UID and `/readyz`).
  - `docker compose up` starts postgres/api/worker — **waiting for CI
    after push**: no Docker in the sandbox. The CI job `docker` runs the
    same images and settings as `deployments/compose.yaml` with
    `docker run`; `docker compose up` itself needs a Docker host (it was
    verified in Sprint 1, and the compose file has not changed since
    Sprint 5).
  - A monitor can be created via API — `TestCreateMonitor`,
    `TestCreateAndGet`, `TestCreateAppliesDefaults`,
    `TestCreateHonoursExplicitValues`, `TestCreateMonitorRejectsBadBodies`;
    smoke test (201).
  - A worker claims and checks it automatically —
    `TestSchedulerEndToEnd`, `TestClaimSkipsDisabledDeletedAndNotDue`,
    `TestClaimRespectsBatchSize`, `TestNeverClaimsMoreThanFreeSlots`; smoke
    test (checked with no manual trigger).
  - Results/latency history persisted — `TestRecordCheckResult`,
    `TestListChecksKeysetPagination`, `TestListChecksEndpoint`,
    `TestMonitorStatsWindows`, `TestMonitorStatsEndpoint`,
    `TestMonitorChecksConstraints`, `TestDeleteExpiredChecks`; smoke test
    (`/checks` returned the stored result).
  - Repeated failures open an incident —
    `TestRecordLifecycleOpensAndResolvesIncident`, `TestTransition`,
    `TestTransitionCoversEveryAllowedEdge`,
    `TestIncidentsOneUnresolvedPerMonitor`,
    `TestIncidentsConcurrentUnresolvedInsert`,
    `TestRecordWithIncidentAlreadyOpen`,
    `TestQAIncidentHistoryEndpointAfterRecord`.
  - Recovery resolves it — `TestRecordLifecycleOpensAndResolvesIncident`,
    `TestRecordSpanIncidentResolved`, `TestTransition`.
  - Multiple workers never double-process a leased monitor —
    `TestClaimParallelClaimersNeverDoubleAssign`,
    `TestClaimSkipsRowsLockedByOpenTransaction`,
    `TestClaimIgnoresActiveLease`, `TestRecordConcurrentResultsForOneMonitor`,
    `TestRecordReadsStateUnderRowLock`, `TestLeaseLostIsLoggedAndSwallowed`.
  - Expired leases are reclaimable —
    `TestExpiredLeaseReclaimedByDifferentWorker`,
    `TestClaimLeaseCoversMonitorTimeout`,
    `TestChecksRunningAtShutdownTimeoutAreReleased`, `TestReleaseLease`.
  - Private/unsafe targets rejected by default — the Sprint 5 SSRF suite
    (`TestValidateURL`, `TestBlockedReason`, `TestDialerRejectsPrivateAnswer`,
    `TestDialerRejectsMixedAnswers`, `TestRedirectToPrivateIsBlocked`,
    `TestCheckerBlocksLoopbackTarget`, `TestCreateMonitorRejectsPrivateTarget`,
    `TestUpdateChecksPolicyOnlyForNewURL`,
    `TestQALegacyUnsafeMonitorBlockedEndToEnd`) with the D2 IPv6 ranges
    in `TestBlockedReason`; smoke test (422 for a loopback target).
  - Health/readiness/metrics endpoints work — `TestHealthz`,
    `TestReadyz`, `TestReadyzBoundsThePing`, `TestReadyzWithDatabase`,
    `TestHealthEndpointsAreUnauthenticated`,
    `TestInternalRouterServesMetrics`, `TestPublicRouterDoesNotServeMetrics`,
    `TestMetricsAllTenRegistered`; smoke test (all three binaries' ports).
    Image health checks: CI job `docker` — **waiting for CI after push**.
  - API documented via OpenAPI — `api/openapi.yaml` (3.1);
    `TestOpenAPIIsWellFormed`, `TestOpenAPIMatchesRouter`,
    `TestHealthIsDocumentedAsUnauthenticated`.
  - Unit + integration tests pass — gate above: 172 unit tests and 229
    tests with `-tags integration` (`PULSEGO_TEST_DATABASE_URL` mode), all
    green. Testcontainers mode: CI job `integration` — **waiting for CI
    after push**.
  - Race detector passes — both runs above use `-race`; CI job `checks`
    runs it too.
  - CI builds both binaries + the Docker image — **waiting for CI after
    push**: CI jobs `checks` (the three `go build` steps) and `docker`
    (`docker build -f deployments/Dockerfile` per binary). The same three
    builds pass locally (gate above).
  - No secrets committed — scan above: no hits.
  - README is sufficient standalone — Sprint 6 DoD item 2 above. The
    from-scratch walkthrough is the D8 QA step; the Compose quick start is
    **waiting for CI after push** / a Docker host.
- **Waiting for CI after push** (no Docker or GitHub Actions in the
  sandbox): `docker compose up`, the Docker image build, non-root user and
  health checks (job `docker`), the CI pipeline itself (job `checks`) and
  the Testcontainers integration run (job `integration`). Everything else
  passed here. No new waiver: only W1 (Sprint 5) applies.
