#!/usr/bin/env bash
# tests/e2e/dind.sh
#
# Runs the end-to-end suite inside a docker-in-docker container: the
# manager, borg, borgmatic, and the compose stack live inside a DinD "host"
# with its own daemon, the closest harness to the production topology,
# fully isolated from the outer daemon, and proof of the
# zero-host-DB-clients story (the image deliberately contains no pg_dump
# or mariadb client).
#
# Requirements: docker able to run --privileged containers; go toolchain.
set -euo pipefail

log() { echo "=== $*"; }
fail() { echo "FAIL: $*" >&2; exit 1; }

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
IMAGE="borgmatic-manager-e2e-dind"
NAME="bm-e2e-dind-$$"
CTX="$(mktemp -d)"

cleanup() {
  docker rm -f "$NAME" >/dev/null 2>&1 || true
  rm -rf "$CTX"
}
trap cleanup EXIT

log "building manager binary"
(cd "$ROOT" && CGO_ENABLED=0 go build -o "$CTX/borgmatic-manager" ./cmd/borgmatic-manager)
cp -r "$ROOT/tests/e2e" "$CTX/e2e"
cp "$ROOT/tests/e2e/dind/Dockerfile" "$CTX/"

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
  -e E2E_PG_IMAGE="${E2E_PG_IMAGE:-postgres:17-alpine}" \
  -e E2E_MARIA_IMAGE="${E2E_MARIA_IMAGE:-mariadb:11}" \
  "$NAME" bash /opt/e2e/run.sh

log "PASS: DinD end-to-end succeeded"
