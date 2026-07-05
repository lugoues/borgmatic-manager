# Hermetic end-to-end harness: a docker-in-docker "host" with borg,
# borgmatic, the compose plugin, and the manager baked in. The inner daemon
# plays the production host's container runtime; the compose stack runs
# against it, fully isolated from the outer daemon. Built by
# tests/e2e/dind.sh from a staged context.
FROM docker:dind

# bash + compose: the test driver. python3/pip: borgmatic (Alpine's package
# lags the required 2.1 floor). borgbackup from apk. No database client
# tools on purpose, helper-container dumps must not need any.
RUN apk add --no-cache bash python3 py3-pip borgbackup docker-cli-compose \
    && pip3 install --break-system-packages --no-cache-dir borgmatic \
    && borg --version && borgmatic --version

COPY borgmatic-manager /usr/local/bin/borgmatic-manager
COPY e2e /opt/e2e

ENV MANAGER_BIN=/usr/local/bin/borgmatic-manager

# dockerd-entrypoint.sh (from the base image) starts the inner daemon; the
# wrapper execs /opt/e2e/run.sh once the socket is up.
