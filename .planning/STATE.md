---
gsd_state_version: 1.0
milestone: v2.1
milestone_name: host-pivot-review
status: completed
stopped_at: Completed v2.1 owner-review round
last_updated: "2026-07-05T08:00:00.000Z"
last_activity: 2026-07-05 — v2.1: container-only labels, helper-container dumps, config labels, borgmatic passthrough, cosign
progress:
  total_phases: 5
  completed_phases: 5
  total_plans: 5
  completed_plans: 5
  percent: 100
---

# Project State

## Project Reference

See: .planning/PROJECT.md (updated 2026-07-05)

**Core value:** Fully automated, label-driven borgmatic backup orchestration that requires zero manual config updates when services are added, removed, or reconfigured.
**Current focus:** v2.0 complete — awaiting owner review / first tagged release

## Current Position

Milestone: v2.1 (host pivot + owner review round) — COMPLETE
Spec: .planning/v2-host-pivot-SPEC.md (adversarially reviewed before execution)
Execution note: v2.0 was executed in a single autonomous session (2026-07-05)
directly from the spec rather than through per-phase GSD plan documents;
commits map 1:1 to spec phases (cff9bf0, 3bd9ccc, 8f9c8fd + 6fee29c, 7b625a9,
phase-5 commit).

Progress: [██████████] 100%

## Accumulated Context

### Decisions

Logged in PROJECT.md Key Decisions. Highlights for future sessions:

- Borg 2 is still beta (July 2026) — per-repo serialization is required; if
  Borg 2 stabilizes, SER-01 can be revisited (v1's TST-04 assumption).
- Version floors (borgmatic 2.1.0 / borg 1.4) are enforced in
  cmd/borgmatic-manager/preflight.go; raising them is a one-line change.
- Podman compat emits 'remove' where Docker emits 'destroy' — event actions
  must stay client-side matched (internal/runtime/docker.go relevantActions).
- v2.1: labels are container-only (volume labels warn + are ignored); DB dumps
  default to manager-generated helper containers (--network container:<db>,
  DB's own image; mysql/mariadb additionally bind-mount the runtime dir for
  the dump FIFO and use password_transport: environment).
- 'borgmatic-manager borgmatic <group> [args]' is the supported repo
  interaction path; it regenerates config from live labels before exec.
- borgmatic snapshot cleanup is prefix-matched — the global snapshot lock in
  the runner is load-bearing; do not parallelize snapshot groups.
- Generated configs live on tmpfs by design; restore paths that depend on
  them must go through `generate -output` or borgmatic config bootstrap.

### Pending Todos

- Owner review of the MIT LICENSE copyright line ("borgmatic-manager contributors")
- First tagged release (goreleaser) once CI is green on GitHub
- v3 candidates listed in ROADMAP.md
- .planning/todos/pending/2026-07-27-tie-the-extract-session-to-the-manager-lifetime.md
  — restore: an extract orphaned by a killed manager is detected but not
  prevented; needs a supervisor process, deferred from PR #17 review
- .planning/todos/pending/2026-08-28-collapse-the-toolchain-generation-machinery-to-single-dir-provisioning.md
  (toolchain): delete the generation-history defenses (repair suffixes,
  grace markers, /proc scan) for temp-dir + atomic-rename provisioning;
  ~20 of the PR #27/#29 review findings were defects in these layers.
  Two design questions for the owner are listed in the todo; probe them
  before implementing.

### Blockers/Concerns

- CI has not yet run on GitHub (no remote configured in the dev environment);
  the docker E2E passed locally, the podman job is CI-only.

## Session Continuity

Last session: 2026-07-05 — full v2.0 execution (autonomous)
Stopped at: v2.0 complete
Resume file: None
