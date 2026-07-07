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
    cp /usr/share/borgmatic-manager/manager.yaml /etc/borgmatic-manager/manager.yaml
fi

if command -v systemctl >/dev/null 2>&1; then
    systemctl daemon-reload || true
fi

if [ -n "$upgrade" ]; then
    # try-restart: restarts only if currently active, never starts a
    # service the operator left stopped or disabled. An in-flight backup
    # receives SIGTERM and exits cleanly (borg checkpoints; the next cycle
    # resumes). deb-systemd-invoke honors policy-rc.d in chroots/containers.
    if command -v deb-systemd-invoke >/dev/null 2>&1; then
        deb-systemd-invoke try-restart borgmatic-manager.service || true
    elif command -v systemctl >/dev/null 2>&1; then
        systemctl try-restart borgmatic-manager.service || true
    fi
    if command -v systemctl >/dev/null 2>&1 && systemctl is-active --quiet borgmatic-manager 2>/dev/null; then
        echo "borgmatic-manager upgraded; service restarted on the new binary."
    fi
    exit 0
fi

cat <<'EOF'
borgmatic-manager installed.

Next steps:
  1. Install borgmatic >= 2.1 and borg >= 1.4 (distro packages often lag):
       sudo uv tool install borgmatic   # or: pipx install borgmatic
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
