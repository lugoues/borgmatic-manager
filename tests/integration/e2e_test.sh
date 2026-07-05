#!/usr/bin/env bash
# tests/integration/e2e_test.sh
#
# End-to-end test of the borgmatic-manager host-service model, using two
# backup groups sharing one repository:
#
#   group "files", volumes only, no databases:
#     1. a labeled container (flat labels + config label) with a data volume
#     2. later, a second container (JSON spec label) with another volume,
#        started mid-run to exercise event-driven re-discovery
#   group "db" (E2E_POSTGRES=1), database only, no volume backup:
#     a labeled postgres container dumped via a helper container
#
# Assertions:
#   - the guided "repo-create" bootstrap error appears; the repository is
#     initialized via the borgmatic passthrough subcommand
#   - generated configs are 0600 and carry the config label
#   - both volumes' files exist in a "files" archive AND their extracted
#     bytes match what was written (content, not just presence)
#   - a "db" archive contains a postgres dump, and a full restore roundtrip
#     succeeds: marker row -> backup -> drop -> restore -> row is back
#   - discover/generate one-shots work; SIGTERM shuts down cleanly
#
# Requirements:
#   - docker (socket at /var/run/docker.sock or $CONTAINER_SOCKET)
#   - borgmatic >= 2.1 and borg >= 1.4 on PATH (or $BORGMATIC_PATH)
#   - passwordless sudo, or run the whole script as root (DinD harness)
#
# Optional:
#   E2E_POSTGRES=1   also run the database group (dumps run in a helper
#                    container from the DB image, no host pg_dump)
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
FILES_GROUP="files"
DB_GROUP="db"
MANAGER_PID=""

# Borg env: unencrypted test repo, non-interactive access.
BORG_ENV=(BORG_UNKNOWN_UNENCRYPTED_REPO_ACCESS_IS_OK=yes BORG_RELOCATED_REPO_ACCESS_IS_OK=yes)

# Manager environment, reused by the daemon and every one-shot invocation.
MGR_ENV=(BORGMATIC_PATH="$BORGMATIC_PATH" CONTAINER_SOCKET="$CONTAINER_SOCKET"
  CONFIG_DIR="$CONFIG_DIR" RUNTIME_DIR="$RUNTIME_DIR" STATE_DIR="$STATE_DIR")

cleanup() {
  [ -n "$MANAGER_PID" ] && $SUDO kill -TERM "$MANAGER_PID" 2>/dev/null || true
  $CLI rm -f "$PG_NAME" "$APP_A" "$APP_B" >/dev/null 2>&1 || true
  $CLI volume rm -f "$VOL_A" "$VOL_B" >/dev/null 2>&1 || true
  $SUDO rm -rf "$WORK" 2>/dev/null || true
}
trap cleanup EXIT

borg_repo() { $SUDO env "${BORG_ENV[@]}" borg "$@"; }

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

log "starting labeled container with a data volume (group $FILES_GROUP)"
$CLI volume create "$VOL_A" >/dev/null
$CLI run -d --name "$APP_A" \
  -v "$VOL_A:/data" \
  -l borgmatic-manager.backup=true \
  -l borgmatic-manager.group=$FILES_GROUP \
  -l borgmatic-manager.config.keep_daily=14 \
  alpine sh -c 'echo e2e-data-a > /data/file-a.txt && sleep 600' >/dev/null

WITH_PG=0
if [ "${E2E_POSTGRES:-0}" = "1" ]; then
  WITH_PG=1
  log "starting labeled postgres container (database-only group $DB_GROUP)"
  $CLI run -d --name "$PG_NAME" \
    -e POSTGRES_PASSWORD=e2esecret \
    -l borgmatic-manager.group=$DB_GROUP \
    -l borgmatic-manager.db.0.type=postgresql \
    -l borgmatic-manager.db.0.name=postgres \
    -l borgmatic-manager.db.0.username=postgres \
    -l borgmatic-manager.db.0.password=e2esecret \
    "$PG_IMAGE" >/dev/null
  for _ in $(seq 1 30); do
    $CLI exec "$PG_NAME" pg_isready -U postgres >/dev/null 2>&1 && break
    sleep 1
  done

  log "writing marker row for the restore roundtrip"
  $CLI exec "$PG_NAME" psql -U postgres -q \
    -c "CREATE TABLE e2e_marker (v text); INSERT INTO e2e_marker VALUES ('roundtrip-ok');"
fi

log "starting manager"
$SUDO env "PATH=$PATH" "${BORG_ENV[@]}" "${MGR_ENV[@]}" \
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

# wait_for_archive_with GROUP PATTERN TIMEOUT_SECONDS: polls the repository
# for a group's archive whose listing contains PATTERN; prints the archive name.
wait_for_archive_with() {
  local group="$1" pattern="$2" timeout="${3:-120}"
  local deadline=$((SECONDS + timeout))
  while [ $SECONDS -lt $deadline ]; do
    for a in $(borg_repo list --short "$REPO_DIR" 2>/dev/null | grep -- "-$group-" || true); do
      if borg_repo list --format '{path}{NL}' "$REPO_DIR::$a" 2>/dev/null | grep -q "$pattern"; then
        echo "$a"
        return 0
      fi
    done
    kill -0 "$MANAGER_PID" 2>/dev/null || fail "manager exited early; log tail: $(tail -5 "$LOG")"
    sleep 3
  done
  echo "=== manager log ===" >&2; cat "$LOG" >&2
  fail "no '$group' archive containing '$pattern' within ${timeout}s"
}

log "waiting for guided bootstrap error (repository does not exist)"
wait_for_log "repo-create" 60

GEN_CONFIG="$RUNTIME_DIR/configs/$FILES_GROUP.yaml"
$SUDO test -f "$GEN_CONFIG" || fail "generated config missing at $GEN_CONFIG"
PERM=$($SUDO stat -c %a "$GEN_CONFIG")
[ "$PERM" = "600" ] || fail "generated config must be 0600, got $PERM"

log "asserting the config label was applied"
$SUDO grep -q "keep_daily: 14" "$GEN_CONFIG" || fail "borgmatic-manager.config.keep_daily label not applied to generated config"

log "initializing repository via the borgmatic passthrough subcommand"
$SUDO env "PATH=$PATH" "${BORG_ENV[@]}" "${MGR_ENV[@]}" \
  "$WORK/borgmatic-manager" borgmatic "$FILES_GROUP" repo-create --encryption none

log "starting second labeled container via JSON spec label (event-driven re-discovery)"
$CLI volume create "$VOL_B" >/dev/null
$CLI run -d --name "$APP_B" \
  -v "$VOL_B:/data" \
  -l borgmatic-manager.spec="{\"group\": \"$FILES_GROUP\", \"backup\": true}" \
  alpine sh -c 'echo e2e-data-b > /data/file-b.txt && sleep 600' >/dev/null

log "waiting for a '$FILES_GROUP' archive containing both volumes"
FILES_ARCHIVE=$(wait_for_archive_with "$FILES_GROUP" "file-b.txt" 120)

log "validating backed-up file CONTENT (extract and compare)"
FILES_LISTING=$(borg_repo list --format '{path}{NL}' "$REPO_DIR::$FILES_ARCHIVE")
for f in a b; do
  ARCHIVE_PATH=$(echo "$FILES_LISTING" | grep "file-$f.txt" | head -1)
  [ -n "$ARCHIVE_PATH" ] || fail "archive $FILES_ARCHIVE does not contain file-$f.txt"
  CONTENT=$(borg_repo extract --stdout "$REPO_DIR::$FILES_ARCHIVE" "$ARCHIVE_PATH")
  [ "$CONTENT" = "e2e-data-$f" ] || fail "file-$f.txt content mismatch: got '$CONTENT', want 'e2e-data-$f'"
done

log "asserting the volumes-only archive contains no database dumps"
if echo "$FILES_LISTING" | grep -q "postgresql_databases"; then
  fail "'$FILES_GROUP' group must not contain database dumps"
fi

if [ "$WITH_PG" = "1" ]; then
  log "waiting for a '$DB_GROUP' archive containing a postgres dump"
  DB_ARCHIVE=$(wait_for_archive_with "$DB_GROUP" "postgresql_databases" 180)

  log "restore roundtrip: drop the marker table, restore, assert the row is back"
  $CLI exec "$PG_NAME" psql -U postgres -q -c "DROP TABLE e2e_marker;"
  $SUDO env "PATH=$PATH" "${BORG_ENV[@]}" "${MGR_ENV[@]}" \
    "$WORK/borgmatic-manager" borgmatic "$DB_GROUP" restore --archive "$DB_ARCHIVE" \
    || fail "borgmatic restore failed"
  MARKER=$($CLI exec "$PG_NAME" psql -U postgres -tA -c "SELECT v FROM e2e_marker;")
  [ "$MARKER" = "roundtrip-ok" ] || fail "restored marker mismatch: got '$MARKER'"
fi

log "testing discover one-shot"
DISCOVER_OUT=$($SUDO env "PATH=$PATH" "${MGR_ENV[@]}" "$WORK/borgmatic-manager" discover)
echo "$DISCOVER_OUT" | grep -q "group $FILES_GROUP" || fail "discover did not list group $FILES_GROUP: $DISCOVER_OUT"
echo "$DISCOVER_OUT" | grep -q "$VOL_A" || fail "discover did not list $VOL_A"
if [ "$WITH_PG" = "1" ]; then
  echo "$DISCOVER_OUT" | grep -q "group $DB_GROUP" || fail "discover did not list group $DB_GROUP"
fi

log "testing generate one-shot"
GEN_OUT_DIR="$WORK/generate-out"
$SUDO env "PATH=$PATH" "${MGR_ENV[@]}" \
  "$WORK/borgmatic-manager" generate -output "$GEN_OUT_DIR" >/dev/null
$SUDO test -f "$GEN_OUT_DIR/$FILES_GROUP.yaml" || fail "generate -output did not write $FILES_GROUP.yaml"

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
