---
name: sprint-start
description: Starts the next PulseGo sprint. Reads PROGRESS.md, refuses to advance while the current sprint has unchecked Definition of Done items, creates the sprint branch, and drafts an implementation plan for approval before any code is written.
disable-model-invocation: true
---

# /sprint-start

1. **Read `PROGRESS.md`.** Find the current sprint (the first one whose
   status is not `Done`).

2. **Enforce the gate (CLAUDE.md Rule 4).** If the current sprint's
   Definition of Done has any unchecked item, do not start a new sprint.
   Instead, report exactly which items remain and resume work on that
   sprint's branch.

3. **If the current sprint is fully checked off**, move to the next sprint
   in `ROADMAP.md` §5:
   - Create branch `feature/sprint-NN-short-name` from `main` (use the
     branch name given in that sprint's ROADMAP.md section).
   - Set the sprint's status in `PROGRESS.md` to `In Progress`.

4. **Draft an implementation plan** for the sprint's scope, referencing the
   relevant ROADMAP.md section (tables, endpoints, config, DoD). Cover:
   - Files/packages to add or change.
   - Any new migration(s) and the exact columns/constraints they add.
   - Test strategy for this sprint (unit vs. integration, per
     ROADMAP.md §"Testing Strategy" categories).
   - Anything ambiguous or missing from ROADMAP.md that needs a decision.

5. **Post the plan and stop.** Do not write code until the user approves it.
   This mirrors CLAUDE.md Rule 3 — plan before code, always.

6. **If `PROGRESS.md` shows all 6 sprints Done**, report that the roadmap is
   complete and point to the project-level Definition of Done in
   `ROADMAP.md` §9 for final verification instead of starting a sprint.
