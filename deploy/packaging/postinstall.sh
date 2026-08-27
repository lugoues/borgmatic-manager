#!/bin/sh
# Package post-install: seed the config once (never on upgrade, operator
# files in /etc belong to the operator) and make the unit visible; enabling
# is the operator's call. On upgrade, restart a running service so it never
# keeps executing the replaced binary (Debian dh_installsystemd convention).
set -e

# deb passes "configure <old-version>" on upgrade ($2 empty on fresh
# install); rpm passes "2" on upgrade, "1" on fresh install.
upgrade=""
case "${1:-}" in
    configure) [ -n "${2:-}" ] && upgrade=1 ;;
    2) upgrade=1 ;;
esac

mkdir -p /etc/borgmatic-manager/conf.d /etc/borgmatic-manager/groups
if [ ! -e /etc/borgmatic-manager/manager.yaml ]; then
    # 0600: operators put encryption_passphrase in this file.
    cp /usr/share/borgmatic-manager/manager.yaml /etc/borgmatic-manager/manager.yaml
    chmod 0600 /etc/borgmatic-manager/manager.yaml
fi

if command -v systemctl >/dev/null 2>&1; then
    systemctl daemon-reload || true
fi

# The stamp is read before the upgrade branch, because $1/$2 cannot tell a
# reinstall from an upgrade. `dpkg -r` leaves the package in config-files state,
# so a later `dpkg -i` calls "configure <old-version>" exactly as an upgrade
# does. Testing the arguments alone sent reinstalls down the try-restart path,
# which does nothing to a service preremove had just stopped, and the package
# came back enabled and dead: the silent stop this whole split exists to avoid.
#
# The stamp is the discriminator the arguments cannot provide. A true upgrade
# never has one, because preremove exits before stopping anything.
#
# Two independent conditions then have to agree, and the service is started only
# in their intersection:
#
#   the stamp   preremove stopped a service that was really running, so a fresh
#               install has none, and neither does one the operator had already
#               stopped on purpose before removing the package.
#   is-enabled  the operator's standing intent is "run this at boot". A running
#               but disabled service was a transient state, and a remove and
#               reinstall is closer to a reboot than to a no-op, so that state
#               is not something to recreate.
STOPPED_STAMP=/var/lib/borgmatic-manager/.stopped-by-package
stamped=""
if [ -e "$STOPPED_STAMP" ]; then
    stamped=1
    # Consume it whether or not it leads to a start: it describes the removal
    # this install just undid, so leaving it behind would let a later reinstall
    # act on a stale record.
    rm -f "$STOPPED_STAMP"
fi

if [ -n "$stamped" ] \
   && command -v systemctl >/dev/null 2>&1 \
   && systemctl is-enabled --quiet borgmatic-manager.service 2>/dev/null \
   && ! systemctl is-active --quiet borgmatic-manager 2>/dev/null; then
    # deb-systemd-invoke honors policy-rc.d, so chroot and container builds
    # still get to refuse.
    if command -v deb-systemd-invoke >/dev/null 2>&1; then
        deb-systemd-invoke start borgmatic-manager.service || true
    else
        systemctl start borgmatic-manager.service || true
    fi
    if systemctl is-active --quiet borgmatic-manager 2>/dev/null; then
        echo "borgmatic-manager reinstalled; the removal had stopped a running service, so it was started again."
        exit 0
    fi
fi

# A reinstall that did not start anything (disabled, or policy-rc.d refused)
# still must not print first-install instructions: this host is already set up.
if [ -n "$upgrade" ] || [ -n "$stamped" ]; then
    # try-restart: restarts only if currently active, never starts a
    # service the operator left stopped or disabled. An in-flight backup
    # receives SIGTERM and exits cleanly (borg checkpoints; the next cycle
    # resumes). deb-systemd-invoke honors policy-rc.d in chroots/containers.
    if command -v deb-systemd-invoke >/dev/null 2>&1; then
        deb-systemd-invoke try-restart borgmatic-manager.service || true
    elif command -v systemctl >/dev/null 2>&1; then
        systemctl try-restart borgmatic-manager.service || true
    fi
    if [ -n "$upgrade" ] && command -v systemctl >/dev/null 2>&1 && systemctl is-active --quiet borgmatic-manager 2>/dev/null; then
        echo "borgmatic-manager upgraded; service restarted on the new binary."
    fi
    exit 0
fi

cat <<'EOF'
borgmatic-manager installed.

Next steps:
  1. Install borg >= 1.4 (the only hard dependency; distro packages often
     lag). borgmatic is NOT needed: the manager provisions and uses its own
     isolated toolchain on first use. A host-installed borgmatic is ignored
     unless manager.borgmatic_path or BORGMATIC_PATH points at it.
  2. Edit /etc/borgmatic-manager/manager.yaml (repository, passphrase);
     local tweaks belong in /etc/borgmatic-manager/conf.d/*.yaml, which
     package upgrades never touch (the shipped default lives at
     /usr/share/borgmatic-manager/manager.yaml for reference)
  3. Label your containers (borgmatic-manager.enable=true, .group=<name>)
  4. systemctl enable --now borgmatic-manager
  5. Initialize the repository with the command the first cycle prints:
       journalctl -u borgmatic-manager | grep repo-create
     (it will be: borgmatic-manager borgmatic <group> repo-create ...)
EOF
