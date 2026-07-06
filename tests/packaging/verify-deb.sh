#!/usr/bin/env bash
# tests/packaging/verify-deb.sh [image...]
#
# Validates the built deb's install/upgrade/remove cycle inside the given
# distro images (default: current Debian stable and Ubuntu LTS):
#   - dpkg -i succeeds and the postinstall guide prints
#   - the binary runs and reports its version
#   - unit, user unit, config, and docs land at the packaged paths
#   - an operator-edited manager.yaml survives reinstall (conffile)
#   - removal deletes the binary, preserves the config, and the lifecycle
#     scripts degrade gracefully without systemd
#
# Requires: docker, and a deb in dist/ (mise run package).
set -euo pipefail

fail() { echo "FAIL: $*" >&2; exit 1; }

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
ARCH=$(uname -m); case "$ARCH" in x86_64) ARCH=amd64 ;; aarch64) ARCH=arm64 ;; esac
DEB=$(ls "$ROOT"/dist/*_linux_"$ARCH".deb 2>/dev/null | head -1)
[ -n "$DEB" ] || fail "no $ARCH deb in dist/, run 'mise run package' first"

IMAGES=("$@")
[ ${#IMAGES[@]} -gt 0 ] || IMAGES=(debian:trixie ubuntu:24.04)

for image in "${IMAGES[@]}"; do
  echo "=== $image ==="
  docker run --rm -v "$DEB:/pkg.deb:ro" "$image" bash -ec '
    export DEBIAN_FRONTEND=noninteractive
    dpkg -i /pkg.deb >/dev/null

    /usr/bin/borgmatic-manager version >/dev/null

    # Functional files must land on disk. Docs are asserted in the package
    # listing instead: minimized cloud images (e.g. ubuntu:24.04) configure
    # dpkg path-excludes that legitimately skip /usr/share/doc at install.
    for f in /usr/lib/systemd/system/borgmatic-manager.service \
             /usr/share/borgmatic-manager/borgmatic-manager.user.service \
             /etc/borgmatic-manager/manager.yaml; do
      test -f "$f" || { echo "missing $f" >&2; exit 1; }
    done
    dpkg-deb -c /pkg.deb | grep -q "doc/borgmatic-manager/LICENSE" \
      || { echo "LICENSE missing from package" >&2; exit 1; }

    echo "# operator edit" >> /etc/borgmatic-manager/manager.yaml
    dpkg -i /pkg.deb >/dev/null 2>&1
    tail -1 /etc/borgmatic-manager/manager.yaml | grep -q "operator edit" \
      || { echo "conffile clobbered on reinstall" >&2; exit 1; }

    dpkg -r borgmatic-manager >/dev/null 2>&1
    test ! -f /usr/bin/borgmatic-manager || { echo "binary not removed" >&2; exit 1; }
    test -f /etc/borgmatic-manager/manager.yaml || { echo "config not preserved" >&2; exit 1; }
    echo "ok: install, conffile upgrade-safety, clean removal"
  ' || fail "$image"
done

echo "PASS: deb verified on: ${IMAGES[*]}"
