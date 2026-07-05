#!/usr/bin/env bash
# tests/integration/dind_test.sh
#
# Runs the full end-to-end test inside a docker-in-docker container: the
# manager, borg, and borgmatic live inside a DinD "host" with its own daemon
# and test containers, the closest harness to the production topology, fully
# isolated from the outer daemon, and proof of the zero-host-DB-clients story
# (the image deliberately contains no pg_dump).
#
# Requirements: docker able to run --privileged containers; go toolchain.
set -euo pipefail

log() { echo "=== $*"; }
fail() { echo "FAIL: $*" >&2; exit 1; }

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
IMAGE="borgmatic-manager-e2e-dind"
NAME="bm-e2e-dind-$$"

cleanup() {
  docker rm -f "$NAME" >/dev/null 2>&1 || true
}
trap cleanup EXIT

log "building manager binary"
CTX="$(mktemp -d)"
trap 'rm -rf "$CTX"; cleanup' EXIT
(cd "$ROOT" && CGO_ENABLED=0 go build -o "$CTX/borgmatic-manager" ./cmd/borgmatic-manager)
cp "$ROOT/tests/integration/e2e_test.sh" "$CTX/"
cp "$ROOT/tests/integration/dind/Dockerfile" "$CTX/"

log "building DinD test image"
docker build -q -t "$IMAGE" "$CTX" >/dev/null

log "starting DinD host (privileged)"
docker run -d --privileged --name "$NAME" \
  -e DOCKER_TLS_CERTDIR= \
  "$IMAGE" >/dev/null

log "waiting for the inner docker daemon"
docker exec "$NAME" sh -c '
  n=0
  until docker info >/dev/null 2>&1; do
    n=$((n+1)); [ $n -gt 60 ] && exit 1
    sleep 1
  done' || { docker logs "$NAME" | tail -20; fail "inner dockerd did not come up"; }

log "running the e2e suite inside the DinD host"
docker exec \
  -e E2E_POSTGRES="${E2E_POSTGRES:-1}" \
  -e E2E_MARIADB="${E2E_MARIADB:-1}" \
  -e E2E_PG_IMAGE="${E2E_PG_IMAGE:-postgres:17-alpine}" \
  -e E2E_MARIA_IMAGE="${E2E_MARIA_IMAGE:-mariadb:11}" \
  "$NAME" bash /opt/e2e/e2e_test.sh

log "PASS: DinD end-to-end succeeded"
