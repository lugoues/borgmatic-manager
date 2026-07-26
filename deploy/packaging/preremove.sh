#!/bin/sh
# Package pre-remove: stop the service when the package is being removed,
# but not on upgrade (deb passes "upgrade", rpm passes "1" for upgrades).
#
# Stopping only. Disabling belongs in postremove, and only on purge: on deb,
# "remove" is meant to leave the operator's choices intact so a reinstall comes
# back enabled, and disabling here silently lost that.
set -e

case "${1:-}" in
    upgrade|1) exit 0 ;;
esac

if command -v systemctl >/dev/null 2>&1 && systemctl is-active --quiet borgmatic-manager 2>/dev/null; then
    echo "Stopping borgmatic-manager (in-flight backups receive SIGTERM and exit cleanly)..."
    systemctl stop borgmatic-manager || true
fi
