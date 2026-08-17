# PulseGo — Progress

Last updated: _(set by `/sprint-done`)_
Current sprint: **Sprint 1 — Foundation** (not started)

---

## Sprint 1 — Foundation
Status: **Not Started**
Branch: `feature/sprint-01-foundation`

Definition of Done:
- [ ] Module + layout match the repo tree in ROADMAP.md
- [ ] Config validation rejects missing/invalid env vars with a clear error
- [ ] Both binaries start, log structured JSON, and shut down gracefully
- [ ] `docker compose up` starts postgres + api + worker
- [ ] `monitors` migration applied; `sqlc generate` produces compiling code

Notes:
-

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
