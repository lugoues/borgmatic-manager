---
created: 2026-08-28T00:21:22.665Z
title: Collapse the toolchain generation machinery to single-dir provisioning
area: toolchain
files:
  - internal/toolchain/toolchain.go
  - internal/toolchain/toolchain_test.go
  - cmd/borgmatic-manager/preflight.go
---

## Problem

The toolchain provisioning that merged in PRs #27/#29 carries defensive
machinery sized for scenarios a single-daemon deployment rarely or never
produces: generation-history dirs with `-rN` repair suffixes, `.superseded`
markers with a 24h grace period, a `/proc/*/maps` in-use scan, and
manifest-driven `cleanOldVersions`. The cost is concrete, not theoretical:
roughly 20 of the ~70 review findings across those PRs were defects in the
defensive layers themselves; every future edit must preserve an invariant
map the code cannot enforce (never rename a versioned dir because of
absolute shebangs, manifest only after the smoke test, strip Python env
only after a probe passes); and one fix (the WaitDelay orphan sweep) had to
land at four probe sites. Defensive paths are the least-exercised code in
the repo, so their bugs surface as incidents, not cycles. The owner wants
this addressed sooner rather than later (decided 2026-08-28), not parked
behind a "wait for a confusing bug" trigger.

## Solution

Replace the generation-history design with the simple shape: build the
pinned version in a temp dir, atomic rename into place, flip the `current`
symlink, delete the old dir. Expect roughly 150-250 lines to go, plus their
pinned tests, which encode the current design's specifics (marker aging,
grace windows, generation naming, in-use detection) and must be deleted
alongside, as happened with borgprobe.go in PR #27.

Keep: the cross-process provision lock (first-launch races are real under
systemd restart policy), the checksum-verified uv download, the
exact-version smoke test, probe hardening (Setpgid group-kill, WaitDelay
group sweep), and Ensure's degrade-to-existing-healthy-toolchain path.

Design questions to probe with the owner BEFORE implementing (per the
consult-on-design rule):

1. Deleting the old generation can break a long-running backup mid-run
   because Python imports lazily. Pick one: keep a minimal in-use guard,
   defer the delete to the next launch, or accept the narrow window of an
   ad-hoc run concurrent with a daemon reprovision.
2. Does repair-after-corruption stay automatic (reprovision in place), or
   become the documented delete-dir-and-restart operator action?
