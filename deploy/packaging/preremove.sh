#!/bin/sh
# Package pre-remove: stop the service when the package is being removed,
# but not on upgrade (deb passes "upgrade", rpm passes "1" for upgrades).
#
# Stopping only. Disabling belongs in postremove, and only on purge: on deb,
# "remove" is meant to leave the operator's choices intact so a reinstall comes
# back enabled, and disabling here silently lost that.
set -e

STOPPED_STAMP=/var/lib/borgmatic-manager/.stopped-by-package

case "${1:-}" in
    upgrade|1) exit 0 ;;
esac

if command -v systemctl >/dev/null 2>&1 && systemctl is-active --quiet borgmatic-manager 2>/dev/null; then
    echo "Stopping borgmatic-manager (in-flight backups receive SIGTERM and exit cleanly)..."
    systemctl stop borgmatic-manager || true

    # Record that the package stopped a service that was really running, so a
    # reinstall can restore exactly that and nothing more. "Enabled but
    # inactive" is not a good enough signal on its own: an operator who
    # deliberately stopped an enabled service looks identical, and starting it
    # on reinstall would override their decision.
    #
    # Only after confirming it actually stopped. The stop is best-effort, and a
    # stamp written after a failed stop would later start a service this hook
    # never managed to stop.
    if ! systemctl is-active --quiet borgmatic-manager 2>/dev/null; then
        mkdir -p "$(dirname "$STOPPED_STAMP")" 2>/dev/null || true
        : > "$STOPPED_STAMP" 2>/dev/null || true
    fi
fi
