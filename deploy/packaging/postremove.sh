#!/bin/sh
# Package post-remove: let systemd forget the unit. Repository data,
# /etc/borgmatic-manager, and /var/lib/borgmatic-manager are deliberately
# left in place, they belong to the operator, not the package.
set -e

if command -v systemctl >/dev/null 2>&1; then
    systemctl daemon-reload || true
fi
