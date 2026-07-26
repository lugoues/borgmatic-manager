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
purge=no
case "${1:-}" in
    purge|0) purge=yes ;;
esac

if [ "$purge" = yes ]; then
    # Not "systemctl disable": this hook runs after the unit file has already
    # been deleted, and disable needs to read the unit to find what to undo. It
    # fails with "unit borgmatic-manager.service does not exist" and leaves the
    # symlink in place, so a later reinstall would come back unexpectedly
    # enabled. Remove the enablement symlink directly instead; it is the one
    # [Install]'s "WantedBy=multi-user.target" creates.
    rm -f /etc/systemd/system/multi-user.target.wants/borgmatic-manager.service
    # A masked unit is a symlink to /dev/null the operator put here; purge is
    # the one time it is right to forget that too.
    if [ -L /etc/systemd/system/borgmatic-manager.service ] \
       && [ "$(readlink /etc/systemd/system/borgmatic-manager.service)" = /dev/null ]; then
        rm -f /etc/systemd/system/borgmatic-manager.service
    fi
fi

if command -v systemctl >/dev/null 2>&1; then
    systemctl daemon-reload || true
fi
