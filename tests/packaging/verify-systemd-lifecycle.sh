#!/usr/bin/env bash
# tests/packaging/verify-systemd-lifecycle.sh [image]
#
# Validates the maintainer scripts against a real systemd (default:
# debian:12 booted with systemd as PID 1). verify-deb.sh covers file
# layout and conffile safety but runs without an init, so everything the
# scripts do with systemctl is a no-op there. This covers the part that
# only a real init can answer:
#
#   - a fresh install neither enables nor starts anything
#   - `dpkg -r` stops the service but KEEPS the enablement, and records
#     that it stopped a running service
#   - reinstalling after that removal brings the service back UP. Debian
#     reports it as `configure <old-version>`, identical to an upgrade,
#     so only that record distinguishes them
#   - `dpkg --purge` drops the enablement, including a --runtime one
#   - a service the operator stopped on purpose is not restarted for them
#   - policy-rc.d is honored, so chroot and container builds can refuse
#
# Requires: docker with --privileged, and a deb in dist/ (mise run package).
set -euo pipefail

fail() { echo "FAIL: $*" >&2; exit 1; }

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
ARCH=$(uname -m); case "$ARCH" in x86_64) ARCH=amd64 ;; aarch64) ARCH=arm64 ;; esac
DEB=$(ls "$ROOT"/dist/*_linux_"$ARCH".deb 2>/dev/null | head -1)
[ -n "$DEB" ] || fail "no $ARCH deb in dist/, run 'mise run package' first"

IMAGE="${1:-debian:12}"
CONTAINER="borgmatic-manager-pkglifecycle-$$"

cleanup() { docker rm -f "$CONTAINER" >/dev/null 2>&1 || true; }
trap cleanup EXIT

echo "=== booting $IMAGE with systemd as PID 1"
docker run -d --name "$CONTAINER" --privileged \
  --tmpfs /run --tmpfs /run/lock \
  -v /sys/fs/cgroup:/sys/fs/cgroup:rw --cgroupns=host \
  "$IMAGE" /bin/bash -c \
  'apt-get update -qq && apt-get install -y -qq systemd systemd-sysv >/dev/null 2>&1 && exec /lib/systemd/systemd' >/dev/null

for _ in $(seq 1 90); do
  case "$(docker exec "$CONTAINER" systemctl is-system-running 2>/dev/null || true)" in
    running|degraded) break ;;
  esac
  sleep 2
done
docker exec "$CONTAINER" systemctl is-system-running >/dev/null 2>&1 \
  || [ "$(docker exec "$CONTAINER" systemctl is-system-running 2>/dev/null || true)" = degraded ] \
  || fail "systemd did not come up in $IMAGE"

# /root, not /tmp: systemd mounts a tmpfs over /tmp and would hide the file.
docker cp "$DEB" "$CONTAINER:/root/bm.deb"

# -i is load-bearing: without it the heredoc never reaches `bash -s`, which
# then exits 0 having asserted nothing at all.
docker exec -i "$CONTAINER" bash -s <<'ASSERT'
set -uo pipefail
PASS=0; FAIL=0
WANTS=/etc/systemd/system/multi-user.target.wants/borgmatic-manager.service
RUNTIME_WANTS=/run/systemd/system/multi-user.target.wants/borgmatic-manager.service
STAMP=/var/lib/borgmatic-manager/.stopped-by-package
POLICY=/usr/sbin/policy-rc.d

ck() {
  if [ "$2" = "$3" ]; then echo "  ok    $1 ($3)"; PASS=$((PASS+1))
  else echo "  FAIL  $1: expected '$2' got '$3'"; FAIL=$((FAIL+1)); fi
}
# Every dpkg invocation is itself an assertion. A maintainer script that does
# the right thing and then exits nonzero leaves state the checks below would
# happily accept, so discarding these statuses would let that ship. There is no
# `set -e` here on purpose: one failure should not abort the remaining
# scenarios. Removing or purging a package that is not installed exits 0, so
# teardown can be checked on the same footing as the scenario steps.
dpkg_run() {
  local label="$1"; shift
  local out rc
  out=$("$@" 2>&1); rc=$?
  if [ "$rc" != 0 ]; then
    echo "  FAIL  $label exited $rc"
    printf '%s\n' "$out" | tail -3 | sed 's/^/          /'
    FAIL=$((FAIL+1))
  fi
}
active()  { systemctl is-active borgmatic-manager 2>/dev/null || true; }
# Wait for a settled state rather than sleeping a guessed interval: on a loaded
# CI runner a fixed sleep is the difference between a green suite and a flake.
# Returns as soon as the state matches, and gives up after ~15s so a genuine
# failure still reports the wrong state instead of hanging.
settle() {
  local want="$1" i
  for i in $(seq 1 150); do
    [ "$(systemctl is-active borgmatic-manager 2>/dev/null || true)" = "$want" ] && return 0
    sleep 0.1
  done
  return 0
}
enabled() { systemctl is-enabled borgmatic-manager 2>/dev/null || true; }
# The unit's PID. An upgrade must replace the running process, and "still
# active" cannot show that: a try-restart that quietly became a no-op leaves the
# old process up and looks identical.
mainpid() { systemctl show borgmatic-manager -p MainPID --value 2>/dev/null || true; }
exists()  { [ -e "$1" ] && echo yes || echo no; }
# -e follows the link, and the wants symlink dangles once dpkg removes the
# unit file, which is exactly the state under test. -L asks the real question.
linked()  { [ -L "$1" ] && echo yes || echo no; }

# The packaged ExecStart needs borgmatic and a container socket, neither of
# which exist here, so it could never reach "active". The maintainer scripts
# only ever consult is-enabled/is-active, so a drop-in that just sleeps
# exercises them faithfully. Re-applied after each install: dpkg restores the
# unit file, and a reload is needed for the override to take effect.
dropin() {
  mkdir -p /etc/systemd/system/borgmatic-manager.service.d
  printf '[Service]\nExecStart=\nExecStart=/bin/sleep infinity\nRestart=no\nType=simple\n' \
    > /etc/systemd/system/borgmatic-manager.service.d/lifecycle-test.conf
  systemctl daemon-reload
}
reset() {
  dpkg_run "reset: dpkg --purge" dpkg --purge borgmatic-manager
  rm -rf /var/lib/borgmatic-manager /etc/borgmatic-manager
  systemctl daemon-reload
}
install_pkg() { dpkg_run "dpkg -i" dpkg -i /root/bm.deb; dropin; }

# A real Debian host has no policy-rc.d. This image ships one that denies
# everything, which is its own test case at the end.
[ -e "$POLICY" ] && mv "$POLICY" "$POLICY.off"

echo "--- fresh install enables nothing"
reset; install_pkg
ck "not enabled" disabled "$(enabled)"
ck "not active" inactive "$(active)"
ck "no stopped-service record" no "$(exists $STAMP)"

echo "--- operator enables and starts it"
systemctl enable --quiet borgmatic-manager; systemctl start borgmatic-manager; settle active
ck "enabled" enabled "$(enabled)"
ck "active" active "$(active)"

echo "--- dpkg -r stops it but keeps the operator's choices"
dpkg_run "dpkg -r" dpkg -r borgmatic-manager; settle inactive
ck "stopped" inactive "$(active)"
ck "enablement preserved" yes "$(linked $WANTS)"
ck "recorded that it stopped a running service" yes "$(exists $STAMP)"

echo "--- reinstall brings the service back up"
install_pkg; settle active
ck "still enabled" enabled "$(enabled)"
ck "running again" active "$(active)"
ck "record consumed" no "$(exists $STAMP)"

echo "--- purge forgets the enablement"
dpkg_run "dpkg --purge" dpkg --purge borgmatic-manager; systemctl daemon-reload
ck "enablement dropped" no "$(linked $WANTS)"
ck "record dropped" no "$(exists $STAMP)"

echo "--- upgrading a running service restarts it onto the new binary"
reset; install_pkg
systemctl enable --quiet borgmatic-manager; systemctl start borgmatic-manager; settle active
before_pid=$(mainpid)
install_pkg; settle active
after_pid=$(mainpid)
ck "still active" active "$(active)"
# The invariant an upgrade owes the operator: never keep executing the binary
# the package just replaced.
if [ -n "$before_pid" ] && [ "$before_pid" != 0 ] && [ -n "$after_pid" ] && [ "$after_pid" != 0 ] \
   && [ "$before_pid" != "$after_pid" ]; then replaced=yes; else replaced="no ($before_pid -> $after_pid)"; fi
ck "process replaced, not left on the old binary" yes "$replaced"
ck "no record left behind" no "$(exists $STAMP)"

echo "--- a deliberately stopped service is left stopped"
reset; install_pkg
systemctl enable --quiet borgmatic-manager; systemctl start borgmatic-manager; settle active
systemctl stop borgmatic-manager; settle inactive
dpkg_run "dpkg -r" dpkg -r borgmatic-manager
ck "nothing was running, so nothing recorded" no "$(exists $STAMP)"
install_pkg; sleep 2  # a negative needs a real pause: nothing to settle toward
ck "operator's choice respected" inactive "$(active)"

echo "--- purge clears a --runtime enablement too"
reset; install_pkg
systemctl enable --runtime --quiet borgmatic-manager
ck "runtime enablement present" yes "$(linked $RUNTIME_WANTS)"
dpkg_run "dpkg --purge" dpkg --purge borgmatic-manager
ck "runtime enablement dropped" no "$(linked $RUNTIME_WANTS)"

echo "--- policy-rc.d can still refuse the restart"
[ -e "$POLICY.off" ] && mv "$POLICY.off" "$POLICY"
reset; install_pkg
systemctl enable --quiet borgmatic-manager; systemctl start borgmatic-manager; settle active
dpkg_run "dpkg -r" dpkg -r borgmatic-manager
ck "stop was recorded" yes "$(exists $STAMP)"
install_pkg; sleep 2  # a negative needs a real pause: nothing to settle toward
ck "policy refused the start" inactive "$(active)"
ck "record still consumed, not left stale" no "$(exists $STAMP)"

echo
echo "$PASS assertions passed, $FAIL failed"
[ "$FAIL" = 0 ]
ASSERT

echo "PASS: systemd package lifecycle verified on $IMAGE"
