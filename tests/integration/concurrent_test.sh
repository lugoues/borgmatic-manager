#!/usr/bin/env bash
# tests/integration/concurrent_test.sh
# Validates TST-04: Two borgmatic containers can concurrently create archives
# in the same borg repository without corruption or lock failure.
# Supports both borg 1.x and 2.x (auto-detects version).
#
# Requirements:
#   - docker (or podman with docker CLI compatibility)
#   - Network access to pull borgmatic image (on first run)
#
# Usage:
#   bash tests/integration/concurrent_test.sh
#   BORGMATIC_IMAGE=my-borgmatic:dev bash tests/integration/concurrent_test.sh
set -euo pipefail

BORGMATIC_IMAGE="${BORGMATIC_IMAGE:-ghcr.io/borgmatic-collective/borgmatic:latest}"

echo "=== Concurrent Borg Integration Test ==="
echo "Image: $BORGMATIC_IMAGE"

# Create temp directories for repo and two source directories
REPO_DIR="$(mktemp -d)"
SOURCE_A="$(mktemp -d)"
SOURCE_B="$(mktemp -d)"
# Use docker to clean repo dir since borg writes as root inside the container
cleanup() {
  docker run --rm -v "$REPO_DIR:/d" --entrypoint rm "$BORGMATIC_IMAGE" -rf /d 2>/dev/null || true
  rm -rf "$REPO_DIR" "$SOURCE_A" "$SOURCE_B" 2>/dev/null || true
}
trap cleanup EXIT

# Create test data in each source directory
echo "data-from-source-a" > "$SOURCE_A/file-a.txt"
dd if=/dev/urandom of="$SOURCE_A/random-a.bin" bs=1024 count=4 2>/dev/null
echo "data-from-source-b" > "$SOURCE_B/file-b.txt"
dd if=/dev/urandom of="$SOURCE_B/random-b.bin" bs=1024 count=4 2>/dev/null

echo "--- Initializing borg repository ---"

# Detect borg version and use appropriate commands
# Use --entrypoint to bypass borgmatic's custom init entrypoint
BORG_VERSION=$(docker run --rm --entrypoint borg "$BORGMATIC_IMAGE" --version 2>&1 | grep -oP '\d+' | head -1)
if [ "$BORG_VERSION" -ge 2 ]; then
  BORG_INIT="repo-create"
  BORG_LIST="repo-list"
else
  BORG_INIT="init"
  BORG_LIST="list"
fi
echo "Borg major version: $BORG_VERSION (init=$BORG_INIT, list=$BORG_LIST)"

docker run --rm \
  --entrypoint borg \
  -v "$REPO_DIR:/repo" \
  -e BORG_UNKNOWN_UNENCRYPTED_REPO_ACCESS_IS_OK=yes \
  "$BORGMATIC_IMAGE" \
  $BORG_INIT --encryption none /repo

echo "--- Running two concurrent borg create operations ---"

# Run two concurrent borg create operations in background
docker run --rm \
  --entrypoint borg \
  -v "$REPO_DIR:/repo" \
  -v "$SOURCE_A:/source:ro" \
  -e BORG_UNKNOWN_UNENCRYPTED_REPO_ACCESS_IS_OK=yes \
  "$BORGMATIC_IMAGE" \
  create "/repo::archive-a-{now}" /source &
PID_A=$!

docker run --rm \
  --entrypoint borg \
  -v "$REPO_DIR:/repo" \
  -v "$SOURCE_B:/source:ro" \
  -e BORG_UNKNOWN_UNENCRYPTED_REPO_ACCESS_IS_OK=yes \
  "$BORGMATIC_IMAGE" \
  create "/repo::archive-b-{now}" /source &
PID_B=$!

# Wait for both and capture exit codes
# Disable errexit around wait calls since wait returns the child's exit code
set +e
wait $PID_A
EXIT_A=$?
wait $PID_B
EXIT_B=$?
set -e

echo "--- Checking results ---"
echo "Exit code A: $EXIT_A"
echo "Exit code B: $EXIT_B"

if [ "$EXIT_A" -ne 0 ] || [ "$EXIT_B" -ne 0 ]; then
  echo "FAIL: concurrent borg create failed (exit A=$EXIT_A, B=$EXIT_B)"
  exit 1
fi

# Verify at least 2 archives exist
ARCHIVE_COUNT=$(docker run --rm \
  --entrypoint borg \
  -v "$REPO_DIR:/repo" \
  -e BORG_UNKNOWN_UNENCRYPTED_REPO_ACCESS_IS_OK=yes \
  "$BORGMATIC_IMAGE" \
  $BORG_LIST /repo --format="{name}{NL}" | wc -l)

# Trim whitespace from wc output
ARCHIVE_COUNT=$(echo "$ARCHIVE_COUNT" | tr -d '[:space:]')

if [ "$ARCHIVE_COUNT" -lt 2 ]; then
  echo "FAIL: expected at least 2 archives, got $ARCHIVE_COUNT"
  exit 1
fi

echo "PASS: concurrent borg archive creation succeeded ($ARCHIVE_COUNT archives)"
exit 0
