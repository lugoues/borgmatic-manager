#!/usr/bin/env bash
# tests/e2e/run.sh, end-to-end test driver.
#
# The workload stack (labeled containers, databases, healthchecks, marker
# seeding) is entirely declarative: see compose.yaml. This script only
# drives the manager and asserts outcomes:
#
#   phase 1  stack up (healthcheck-gated) -> manager up -> guided
#            repo-create error -> repository initialized via the
#            'borgmatic-manager borgmatic' passthrough
#   phase 2  app-b appears (compose profile) -> event-driven cycle ->
#            extract both files from the archive and byte-compare them
#   phase 3  db archive has both dumps -> drop markers -> restore ->
#            markers are back
#   phase 4  discover/generate one-shots; clean SIGTERM shutdown
#
# The only polling left is for genuinely eventual outcomes (a backup cycle
# landing an archive); container readiness is gated by compose healthchecks.
#
# Requirements: run as root (the manager reads volume mountpoints), with
# docker + the compose plugin, borg >= 1.4, and borgmatic >= 2.1 (or
# $BORGMATIC_PATH). $MANAGER_BIN skips the 'go build'. $DOCKER_HOST is
# honored (e.g. a podman compat socket).
set -euo pipefail

log() { echo "--- $*"; }
fail() { echo "FAIL: $*" >&2; exit 1; }

HERE="$(cd "$(dirname "$0")" && pwd)"
ROOT="$(cd "$HERE/../.." && pwd)"
[ "$(id -u)" = "0" ] || fail "run as root (e.g. 'sudo -E env \"PATH=\$PATH\" $0'; the DinD harness already is)"

WORK="/srv/borgmatic-e2e" # matches manager.yaml's repository path
BORGMATIC_PATH="${BORGMATIC_PATH:-$(command -v borgmatic)}"
[ -n "$BORGMATIC_PATH" ] || fail "borgmatic not found; install it or set BORGMATIC_PATH"
command -v borg >/dev/null || fail "borg not found on PATH"

# The manager and every borg/borgmatic invocation share this environment.
export BORG_UNKNOWN_UNENCRYPTED_REPO_ACCESS_IS_OK=yes
export BORG_RELOCATED_REPO_ACCESS_IS_OK=yes
export BORGMATIC_PATH
export CONFIG_DIR="$WORK/etc" RUNTIME_DIR="$WORK/run" STATE_DIR="$WORK/state"
if [ -n "${DOCKER_HOST:-}" ]; then
  export CONTAINER_SOCKET="${DOCKER_HOST#unix://}"
fi

REPO="$WORK/repo"
LOG_FILE="$WORK/manager.log"
MANAGER_PID=""

compose() { docker compose -f "$HERE/compose.yaml" "$@"; }

# stack_up ARGS...: compose up gated by healthchecks; on failure, dump every
# container's state and logs so CI/DinD failures are diagnosable from output.
stack_up() {
  if ! compose "$@" up -d --wait; then
    echo "=== stack state ===" >&2
    compose "$@" ps -a >&2 || true
    echo "=== stack logs ===" >&2
    compose "$@" logs --no-color --timestamps >&2 || true
    fail "stack failed to become healthy"
  fi
}
manager() { "$WORK/borgmatic-manager" "$@"; }

cleanup() {
  [ -n "$MANAGER_PID" ] && kill -TERM "$MANAGER_PID" 2>/dev/null || true
  compose --profile late down -v --remove-orphans >/dev/null 2>&1 || true
  rm -rf "$WORK"
}
trap cleanup EXIT

# wait_for_log PATTERN TIMEOUT: the manager's own logs are the source of
# truth for lifecycle transitions that have no external artifact yet.
wait_for_log() {
  local pattern="$1" timeout="${2:-60}"
  for _ in $(seq 1 $((timeout * 2))); do
    grep -q "$pattern" "$LOG_FILE" && return 0
    kill -0 "$MANAGER_PID" 2>/dev/null || fail "manager exited early; log tail: $(tail -5 "$LOG_FILE")"
    sleep 0.5
  done
  echo "=== manager log ==="; cat "$LOG_FILE"
  fail "timed out waiting for log pattern: $pattern"
}

# wait_for_archive GROUP PATTERN TIMEOUT: backups are eventual (they land on
# the next cycle), so this polls the repository, with borg itself, not the
# manager's logs, for a group archive whose listing contains PATTERN.
wait_for_archive() {
  local group="$1" pattern="$2" timeout="${3:-120}"
  local deadline=$((SECONDS + timeout))
  while [ $SECONDS -lt $deadline ]; do
    for a in $(borg list --short "$REPO" 2>/dev/null | grep -- "-$group-" || true); do
      if borg list --format '{path}{NL}' "$REPO::$a" 2>/dev/null | grep -q "$pattern"; then
        echo "$a"
        return 0
      fi
    done
    kill -0 "$MANAGER_PID" 2>/dev/null || fail "manager exited early; log tail: $(tail -5 "$LOG_FILE")"
    sleep 3
  done
  echo "=== manager log ===" >&2; cat "$LOG_FILE" >&2
  fail "no '$group' archive containing '$pattern' within ${timeout}s"
}

# --- phase 0: build & stack up ---------------------------------------------

mkdir -p "$CONFIG_DIR" "$RUNTIME_DIR" "$STATE_DIR"
cp "$HERE/manager.yaml" "$HERE/common.yaml" "$CONFIG_DIR/"
mkdir -p "$CONFIG_DIR/conf.d"
cp "$HERE"/conf.d/*.yaml "$CONFIG_DIR/conf.d/"

if [ -n "${MANAGER_BIN:-}" ]; then
  log "using prebuilt manager binary: $MANAGER_BIN"
  cp "$MANAGER_BIN" "$WORK/borgmatic-manager"
else
  log "building manager"
  (cd "$ROOT" && go build -o "$WORK/borgmatic-manager" ./cmd/borgmatic-manager)
fi

log "stack up (healthcheck-gated)"
stack_up

# --- phase 1: manager up, guided bootstrap ---------------------------------

log "starting manager"
# Invoked directly (not via the manager() helper): a backgrounded function
# runs in a subshell, so $! would be the subshell's PID and SIGTERM would
# never reach the daemon.
"$WORK/borgmatic-manager" run > "$LOG_FILE" 2>&1 &
MANAGER_PID=$!

log "expecting the guided repo-create bootstrap error"
wait_for_log "repo-create" 60

FILES_CONFIG="$RUNTIME_DIR/configs/files.yaml"
[ -f "$FILES_CONFIG" ] || fail "generated config missing at $FILES_CONFIG"
[ "$(stat -c %a "$FILES_CONFIG")" = "600" ] || fail "generated config must be 0600"
grep -q "keep_daily: 14" "$FILES_CONFIG" || fail "config label (keep_daily) not applied"
grep -q "lock_wait: 30" "$FILES_CONFIG" || fail "conf.d drop-in (lock_wait) not merged"

log "initializing the repository via the borgmatic passthrough"
manager borgmatic files repo-create --encryption none

# --- phase 2: event-driven cycle, content validation ------------------------

log "starting app-b (spec label; its create event must trigger a cycle)"
# Pre-seed app-b's marker file so the event-triggered cycle (create event
# + 5s debounce) can never observe a half-initialized volume: with the
# long period there is no second run to paper over a premature snapshot.
docker volume create e2e-data-b >/dev/null
docker run --rm -v e2e-data-b:/data alpine sh -c 'echo e2e-data-b > /data/file-b.txt'
stack_up --profile late

# The manager period is 30m, far above this phase's timeout: only the
# create *event* can land app-b's archive in time, so a broken
# EventStream fails here instead of being masked by the periodic tick.
log "waiting for a 'files' archive containing both volumes (event-driven)"
FILES_ARCHIVE=$(wait_for_archive files "file-b.txt" 120)

log "extracting and byte-comparing both files (volume-named archive paths)"
LISTING=$(borg list --format '{path}{NL}' "$REPO::$FILES_ARCHIVE")
echo "$LISTING" | grep -q "^e2e-data-a/_data/file-a.txt$" \
  || fail "archive paths must start at the volume name, got: $(echo "$LISTING" | grep file-a | head -1)"
for f in a b; do
  path=$(echo "$LISTING" | grep "file-$f.txt" | head -1)
  [ -n "$path" ] || fail "archive $FILES_ARCHIVE is missing file-$f.txt"
  content=$(borg extract --stdout "$REPO::$FILES_ARCHIVE" "$path")
  [ "$content" = "e2e-data-$f" ] || fail "file-$f.txt content mismatch: got '$content'"
done
echo "$LISTING" | grep -q "_databases" && fail "the volumes-only group must not contain database dumps"

# --- phase 3: database dumps and the restore roundtrip ----------------------

log "waiting for a 'db' archive with both database dumps"
DB_ARCHIVE=$(wait_for_archive db "mariadb_databases" 180)
borg list --format '{path}{NL}' "$REPO::$DB_ARCHIVE" | grep -q "postgresql_databases" \
  || fail "db archive is missing the postgres dump"

log "dropping the marker tables"
compose exec -T postgres psql -U postgres -q -c "DROP TABLE e2e_marker;"
compose exec -T mariadb mariadb -uroot -pe2esecret e2edb -e "DROP TABLE e2e_marker;"

log "restoring both databases through the manager"
manager borgmatic db restore --archive "$DB_ARCHIVE"

log "asserting the marker rows are back"
PG_MARKER=$(compose exec -T postgres psql -U postgres -tA -c "SELECT v FROM e2e_marker;")
[ "$PG_MARKER" = "roundtrip-ok" ] || fail "postgres marker after restore: got '$PG_MARKER'"
MARIA_MARKER=$(compose exec -T mariadb mariadb -uroot -pe2esecret e2edb -N -B -e "SELECT v FROM e2e_marker;")
[ "$MARIA_MARKER" = "roundtrip-ok" ] || fail "mariadb marker after restore: got '$MARIA_MARKER'"

# --- phase 4: one-shots and clean shutdown ----------------------------------

log "discover and generate one-shots"
DISCOVER_OUT=$(manager discover)
for expected in "^  files" "^  db" "e2e-data-a"; do
  echo "$DISCOVER_OUT" | grep -q "$expected" || fail "discover output missing '$expected': $DISCOVER_OUT"
done
manager generate --output "$WORK/generate-out" >/dev/null
[ -f "$WORK/generate-out/files.yaml" ] || fail "generate --output did not write files.yaml"

log "status one-shot reflects recorded runs"
STATUS_OUT=$(manager status)
echo "$STATUS_OUT" | grep -q "files" || fail "status missing files group: $STATUS_OUT"
echo "$STATUS_OUT" | grep -Eq "ok \([0-9]" || fail "status missing an ok result with duration: $STATUS_OUT"
echo "$STATUS_OUT" | grep -q "in " || fail "status missing a next-run estimate: $STATUS_OUT"

log "clean shutdown on SIGTERM"
kill -TERM "$MANAGER_PID"
for _ in $(seq 1 60); do
  kill -0 "$MANAGER_PID" 2>/dev/null || break
  sleep 0.5
done
kill -0 "$MANAGER_PID" 2>/dev/null && fail "manager did not exit after SIGTERM"
MANAGER_PID=""
grep -q '"msg":"borgmatic-manager stopped"' "$LOG_FILE" || fail "missing clean shutdown log line"

echo "PASS: end-to-end backup flow succeeded"
