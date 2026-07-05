# Hermetic end-to-end harness: a docker-in-docker "host" with borg, borgmatic,
# and the manager baked in. The inner daemon plays the production host's
# container runtime; test containers run against it, fully isolated from the
# outer daemon. Built by tests/integration/dind_test.sh from a staged context
# containing the prebuilt manager binary and the e2e script.
FROM docker:dind

# bash: the e2e script. python3/pip: borgmatic (Alpine's package lags the
# required 2.1 floor). borgbackup from apk. No database client tools on
# purpose, helper-container dumps must not need any.
RUN apk add --no-cache bash python3 py3-pip borgbackup \
    && pip3 install --break-system-packages --no-cache-dir borgmatic \
    && borg --version && borgmatic --version

COPY borgmatic-manager /usr/local/bin/borgmatic-manager
COPY e2e_test.sh /opt/e2e/e2e_test.sh

# The e2e script resolves the repo root two levels up from its own directory;
# it only uses that for 'go build', which MANAGER_BIN skips.
ENV MANAGER_BIN=/usr/local/bin/borgmatic-manager

# dockerd-entrypoint.sh (from the base image) starts the inner daemon; the
# wrapper execs the e2e script once the socket is up.
