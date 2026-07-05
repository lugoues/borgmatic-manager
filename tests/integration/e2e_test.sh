#!/usr/bin/env bash
# tests/integration/e2e_test.sh
#
# End-to-end test of the borgmatic-manager host-service model:
#   1. builds the manager binary
#   2. starts a labeled container (backup=true + group + config label) with a
#      named volume holding data
#   3. starts the manager (as root: it must read /var/lib/docker/volumes)
#   4. asserts the guided "repo-create" bootstrap error appears
#   5. initializes the repository via the borgmatic passthrough subcommand
#   6. starts a second labeled container -> event-driven re-discovery cycle
#   7. asserts a borg archive exists and contains the volume data, and that
#      the config label was applied
#   8. asserts the discover/generate one-shots work
#   9. SIGTERMs the manager and asserts clean shutdown
#
# Requirements:
#   - docker (socket at /var/run/docker.sock or $CONTAINER_SOCKET)
#   - borgmatic >= 2.1 and borg >= 1.4 on PATH (or $BORGMATIC_PATH)
#   - passwordless sudo, or run the whole script as root (DinD harness)
#
# Optional:
#   E2E_POSTGRES=1   also test a labeled postgres container (dumps run in a
#                    helper container from the DB image, no host pg_dump)
#   MANAGER_BIN=...  use a prebuilt manager binary instead of 'go build'
#   E2E_CLI=...      container CLI (default docker)
#   E2E_PG_IMAGE=... postgres image (default postgres:17-alpine)
set -euo pipefail

log() { echo "--- $*"; }
fail() { echo "FAIL: $*" >&2; exit 1; }

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
# Root (e.g. inside the DinD harness) needs no sudo.
SUDO="sudo"
[ "$(id -u)" = "0" ] && SUDO=""
CLI="${E2E_CLI:-docker}"
PG_IMAGE="${E2E_PG_IMAGE:-postgres:17-alpine}"
CONTAINER_SOCKET="${CONTAINER_SOCKET:-/var/run/docker.sock}"
BORGMATIC_PATH="${BORGMATIC_PATH:-$(command -v borgmatic)}"
[ -n "$BORGMATIC_PATH" ] || fail "borgmatic not found; install it or set BORGMATIC_PATH"
command -v borg >/dev/null || fail "borg not found on PATH"

WORK="$(mktemp -d)"
CONFIG_DIR="$WORK/etc"
RUNTIME_DIR="$WORK/run"
STATE_DIR="$WORK/state"
REPO_DIR="$WORK/repo"
LOG="$WORK/manager.log"
mkdir -p "$CONFIG_DIR" "$RUNTIME_DIR" "$STATE_DIR"

VOL_A="e2e-vol-a-$$"
VOL_B="e2e-vol-b-$$"
APP_A="e2e-app-a-$$"
APP_B="e2e-app-b-$$"
PG_NAME="e2e-pg-$$"
GROUP="e2e"
MANAGER_PID=""

# Borg env: unencrypted test repo, non-interactive access.
BORG_ENV=(BORG_UNKNOWN_UNENCRYPTED_REPO_ACCESS_IS_OK=yes BORG_RELOCATED_REPO_ACCESS_IS_OK=yes)

cleanup() {
  [ -n "$MANAGER_PID" ] && $SUDO kill -TERM "$MANAGER_PID" 2>/dev/null || true
  $CLI rm -f "$PG_NAME" "$APP_A" "$APP_B" >/dev/null 2>&1 || true
  $CLI volume rm -f "$VOL_A" "$VOL_B" >/dev/null 2>&1 || true
  $SUDO rm -rf "$WORK" 2>/dev/null || true
}
trap cleanup EXIT

if [ -n "${MANAGER_BIN:-}" ]; then
  log "using prebuilt manager binary: $MANAGER_BIN"
  cp "$MANAGER_BIN" "$WORK/borgmatic-manager"
else
  log "building manager"
  (cd "$ROOT" && go build -o "$WORK/borgmatic-manager" ./cmd/borgmatic-manager)
fi

log "writing manager.yaml"
cat > "$CONFIG_DIR/manager.yaml" <<EOF
manager:
  period: "15s"
borgmatic:
  repositories:
    - path: $REPO_DIR
  keep_daily: 7
  lock_wait: 30
EOF

log "starting labeled container with a data volume"
$CLI volume create "$VOL_A" >/dev/null
$CLI run -d --name "$APP_A" \
  -v "$VOL_A:/data" \
  -l borgmatic-manager.backup=true \
  -l borgmatic-manager.group=$GROUP \
  -l borgmatic-manager.config.keep_daily=14 \
  alpine sh -c 'echo e2e-data-a > /data/file-a.txt && sleep 600' >/dev/null

WITH_PG=0
if [ "${E2E_POSTGRES:-0}" = "1" ]; then
  WITH_PG=1
  log "starting labeled postgres container"
  $CLI run -d --name "$PG_NAME" \
    -e POSTGRES_PASSWORD=e2esecret \
    -l borgmatic-manager.group=$GROUP \
    -l borgmatic-manager.db.0.type=postgresql \
    -l borgmatic-manager.db.0.name=postgres \
    -l borgmatic-manager.db.0.username=postgres \
    -l borgmatic-manager.db.0.password=e2esecret \
    "$PG_IMAGE" >/dev/null
  for _ in $(seq 1 30); do
    $CLI exec "$PG_NAME" pg_isready -U postgres >/dev/null 2>&1 && break
    sleep 1
  done
fi

log "starting manager"
$SUDO env "PATH=$PATH" "${BORG_ENV[@]}" \
  BORGMATIC_PATH="$BORGMATIC_PATH" CONTAINER_SOCKET="$CONTAINER_SOCKET" \
  CONFIG_DIR="$CONFIG_DIR" RUNTIME_DIR="$RUNTIME_DIR" STATE_DIR="$STATE_DIR" \
  "$WORK/borgmatic-manager" run > "$LOG" 2>&1 &
MANAGER_PID=$!

wait_for_log() {
  local pattern="$1" timeout="${2:-30}"
  for _ in $(seq 1 $((timeout * 2))); do
    grep -q "$pattern" "$LOG" && return 0
    kill -0 "$MANAGER_PID" 2>/dev/null || fail "manager exited early; log tail: $(tail -5 "$LOG")"
    sleep 0.5
  done
  echo "=== manager log ==="; cat "$LOG"
  fail "timed out waiting for log pattern: $pattern"
}

log "waiting for guided bootstrap error (repository does not exist)"
wait_for_log "repo-create" 60

GEN_CONFIG="$RUNTIME_DIR/configs/$GROUP.yaml"
$SUDO test -f "$GEN_CONFIG" || fail "generated config missing at $GEN_CONFIG"
PERM=$($SUDO stat -c %a "$GEN_CONFIG")
[ "$PERM" = "600" ] || fail "generated config must be 0600, got $PERM"

log "asserting the config label was applied"
$SUDO grep -q "keep_daily: 14" "$GEN_CONFIG" || fail "borgmatic-manager.config.keep_daily label not applied to generated config"

log "initializing repository via the borgmatic passthrough subcommand"
$SUDO env "PATH=$PATH" "${BORG_ENV[@]}" \
  BORGMATIC_PATH="$BORGMATIC_PATH" CONTAINER_SOCKET="$CONTAINER_SOCKET" \
  CONFIG_DIR="$CONFIG_DIR" RUNTIME_DIR="$RUNTIME_DIR" STATE_DIR="$STATE_DIR" \
  "$WORK/borgmatic-manager" borgmatic "$GROUP" repo-create --encryption none

log "starting second labeled container via JSON spec label (event-driven re-discovery)"
$CLI volume create "$VOL_B" >/dev/null
$CLI run -d --name "$APP_B" \
  -v "$VOL_B:/data" \
  -l borgmatic-manager.spec="{\"group\": \"$GROUP\", \"backup\": true}" \
  alpine sh -c 'echo e2e-data-b > /data/file-b.txt && sleep 600' >/dev/null

log "waiting for a successful backup"
wait_for_log '"msg":"borgmatic finished"' 120

log "asserting archive exists and contains volume data"
ARCHIVES=$($SUDO env "${BORG_ENV[@]}" borg list --short "$REPO_DIR")
[ -n "$ARCHIVES" ] || fail "no archives in repository"
LATEST=$(echo "$ARCHIVES" | tail -1)
LISTING=$($SUDO env "${BORG_ENV[@]}" borg list "$REPO_DIR::$LATEST")
echo "$LISTING" | grep -q "file-a.txt" || fail "archive $LATEST does not contain volume data"

if [ "$WITH_PG" = "1" ]; then
  log "asserting the archive contains a postgres dump"
  # The dump may land in a later cycle than the first archive; poll.
  FOUND=0
  for _ in $(seq 1 12); do
    for a in $($SUDO env "${BORG_ENV[@]}" borg list --short "$REPO_DIR"); do
      if $SUDO env "${BORG_ENV[@]}" borg list "$REPO_DIR::$a" | grep -q "postgresql_databases"; then
        FOUND=1; break 2
      fi
    done
    sleep 10
  done
  [ "$FOUND" = "1" ] || fail "no archive contains a postgresql dump"
fi

log "testing discover one-shot"
DISCOVER_OUT=$($SUDO env "PATH=$PATH" CONTAINER_SOCKET="$CONTAINER_SOCKET" CONFIG_DIR="$CONFIG_DIR" RUNTIME_DIR="$RUNTIME_DIR" STATE_DIR="$STATE_DIR" \
  "$WORK/borgmatic-manager" discover)
echo "$DISCOVER_OUT" | grep -q "group $GROUP" || fail "discover did not list group $GROUP: $DISCOVER_OUT"
echo "$DISCOVER_OUT" | grep -q "$VOL_A" || fail "discover did not list $VOL_A"

log "testing generate one-shot"
GEN_OUT_DIR="$WORK/generate-out"
$SUDO env "PATH=$PATH" CONTAINER_SOCKET="$CONTAINER_SOCKET" CONFIG_DIR="$CONFIG_DIR" RUNTIME_DIR="$RUNTIME_DIR" STATE_DIR="$STATE_DIR" \
  "$WORK/borgmatic-manager" generate -output "$GEN_OUT_DIR" >/dev/null
$SUDO test -f "$GEN_OUT_DIR/$GROUP.yaml" || fail "generate -output did not write $GROUP.yaml"

log "testing clean shutdown"
$SUDO kill -TERM "$MANAGER_PID"
for _ in $(seq 1 60); do
  kill -0 "$MANAGER_PID" 2>/dev/null || break
  sleep 0.5
done
kill -0 "$MANAGER_PID" 2>/dev/null && fail "manager did not exit after SIGTERM"
MANAGER_PID=""
grep -q '"msg":"borgmatic-manager stopped"' "$LOG" || fail "missing clean shutdown log line"

echo "PASS: end-to-end backup flow succeeded"
