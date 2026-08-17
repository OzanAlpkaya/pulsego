---
name: sprint-done
description: Closes out the current PulseGo sprint. Runs the full Go quality gate (gofmt, go vet, golangci-lint, race tests, govulncheck, both binary builds), checks the sprint's Definition of Done from ROADMAP.md against PROGRESS.md, updates PROGRESS.md, and stops before merging for explicit approval.
disable-model-invocation: true
---

# /sprint-done

1. **Run the quality gate, in order, and capture the result of each:**
   ```
   gofmt -l .
   go vet ./...
   golangci-lint run
   go test -race ./...
   govulncheck ./...
   go build ./cmd/api
   go build ./cmd/worker
   ```
   If this is Sprint 5 or later, also run `docker build` against
   `deployments/Dockerfile`, and run the integration suite (Testcontainers-Go,
   requires Docker) with `make test-integration`.

2. **If anything fails**, stop here. Report exactly which command failed and
   why. Do not touch `PROGRESS.md` or proceed to step 3.

3. **Check the current sprint's Definition of Done** in `ROADMAP.md` §5,
   item by item, against what was actually implemented — not against what
   was planned. Only check off an item if it is genuinely satisfied.

4. **Update `PROGRESS.md`:**
   - Check off every satisfied DoD item for this sprint.
   - Set status to `Done` only if *all* items are checked; otherwise leave
     it `In Progress` and list precisely what's missing under Notes.
   - Update the "Last updated" and "Current sprint" lines at the top.

5. **If any Definition of Done item is not satisfied**, say so explicitly
   and do not mark the sprint complete — list exactly what's missing
   instead.

6. **Do not merge or push.** Report the sprint's status and, if everything
   is green, ask the user to confirm before running the squash-merge to
   `main`. This mirrors CLAUDE.md Rule 2 — merging always needs explicit
   go-ahead.
