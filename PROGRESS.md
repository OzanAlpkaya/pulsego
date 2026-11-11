# PulseGo — Progress

Last updated: 2026-11-11
Current sprint: **Sprint 4 — State and Incidents** (not started)

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
Branch: `feature/sprint-03-scheduler-checker`

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
