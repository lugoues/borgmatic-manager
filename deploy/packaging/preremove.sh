#!/bin/sh
# Package pre-remove: stop the service when the package is being removed,
# but not on upgrade (deb passes "upgrade", rpm passes "1" for upgrades).
set -e

case "${1:-}" in
    upgrade|1) exit 0 ;;
esac

if command -v systemctl >/dev/null 2>&1 && systemctl is-active --quiet borgmatic-manager 2>/dev/null; then
    echo "Stopping borgmatic-manager (in-flight backups receive SIGTERM and exit cleanly)..."
    systemctl stop borgmatic-manager || true
fi
if command -v systemctl >/dev/null 2>&1; then
    systemctl disable borgmatic-manager >/dev/null 2>&1 || true
fi
