#!/usr/bin/env bash
# tests/integration/concurrent_test.sh
# Validates TST-04: Two borgmatic containers can concurrently create archives
# in the same borg 2.x repository without corruption or lock failure.
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

echo "=== Concurrent Borg 2.x Integration Test ==="
echo "Image: $BORGMATIC_IMAGE"

# Create temp directories for repo and two source directories
REPO_DIR="$(mktemp -d)"
SOURCE_A="$(mktemp -d)"
SOURCE_B="$(mktemp -d)"
trap 'rm -rf "$REPO_DIR" "$SOURCE_A" "$SOURCE_B"' EXIT

# Create test data in each source directory
echo "data-from-source-a" > "$SOURCE_A/file-a.txt"
dd if=/dev/urandom of="$SOURCE_A/random-a.bin" bs=1024 count=4 2>/dev/null
echo "data-from-source-b" > "$SOURCE_B/file-b.txt"
dd if=/dev/urandom of="$SOURCE_B/random-b.bin" bs=1024 count=4 2>/dev/null

echo "--- Initializing borg 2.x repository ---"

# Initialize a borg 2.x repository (repo-create, NOT init)
docker run --rm \
  -v "$REPO_DIR:/repo" \
  -e BORG_UNKNOWN_UNENCRYPTED_REPO_ACCESS_IS_OK=yes \
  "$BORGMATIC_IMAGE" \
  borg repo-create --encryption none /repo

echo "--- Running two concurrent borg create operations ---"

# Run two concurrent borg create operations in background
docker run --rm \
  -v "$REPO_DIR:/repo" \
  -v "$SOURCE_A:/source:ro" \
  -e BORG_UNKNOWN_UNENCRYPTED_REPO_ACCESS_IS_OK=yes \
  "$BORGMATIC_IMAGE" \
  borg create "/repo::archive-a-{now}" /source &
PID_A=$!

docker run --rm \
  -v "$REPO_DIR:/repo" \
  -v "$SOURCE_B:/source:ro" \
  -e BORG_UNKNOWN_UNENCRYPTED_REPO_ACCESS_IS_OK=yes \
  "$BORGMATIC_IMAGE" \
  borg create "/repo::archive-b-{now}" /source &
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

# Verify at least 2 archives exist using borg 2.x repo-list (NOT list)
ARCHIVE_COUNT=$(docker run --rm \
  -v "$REPO_DIR:/repo" \
  -e BORG_UNKNOWN_UNENCRYPTED_REPO_ACCESS_IS_OK=yes \
  "$BORGMATIC_IMAGE" \
  borg repo-list /repo --format="{name}{NL}" | wc -l)

# Trim whitespace from wc output
ARCHIVE_COUNT=$(echo "$ARCHIVE_COUNT" | tr -d '[:space:]')

if [ "$ARCHIVE_COUNT" -lt 2 ]; then
  echo "FAIL: expected at least 2 archives, got $ARCHIVE_COUNT"
  exit 1
fi

echo "PASS: concurrent borg 2.x archive creation succeeded ($ARCHIVE_COUNT archives)"
exit 0
