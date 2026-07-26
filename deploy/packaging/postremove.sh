#!/bin/sh
# Package post-remove: let systemd forget the unit, and on a final teardown drop
# the enablement too. Repository data, /etc/borgmatic-manager, and
# /var/lib/borgmatic-manager are deliberately left in place, they belong to the
# operator, not the package.
set -e

# deb distinguishes "remove" (keep the operator's choices, so a reinstall comes
# back as they left it) from "purge" (forget everything). rpm has no such split:
# its argument is the number of copies left, so 0 means the package is going
# away for good and 1 means an upgrade is in progress.
disable=no
case "${1:-}" in
    purge|0) disable=yes ;;
esac

if command -v systemctl >/dev/null 2>&1; then
    if [ "$disable" = yes ]; then
        # The unit file is already gone by now. systemctl disable still clears
        # the enablement because it removes every symlink matching the unit
        # name, not only the ones the [Install] section would have created.
        systemctl disable borgmatic-manager >/dev/null 2>&1 || true
    fi
    systemctl daemon-reload || true
fi
