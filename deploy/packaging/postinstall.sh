#!/bin/sh
# Package post-install: seed the config once (never on upgrade, operator
# files in /etc belong to the operator) and make the unit visible; enabling
# is the operator's call.
set -e

mkdir -p /etc/borgmatic-manager/conf.d /etc/borgmatic-manager/groups
if [ ! -e /etc/borgmatic-manager/manager.yaml ]; then
    cp /usr/share/borgmatic-manager/manager.yaml /etc/borgmatic-manager/manager.yaml
fi

if command -v systemctl >/dev/null 2>&1; then
    systemctl daemon-reload || true
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
