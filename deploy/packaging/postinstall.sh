#!/bin/sh
# Package post-install: make the unit visible; enabling is the operator's call.
set -e

if command -v systemctl >/dev/null 2>&1; then
    systemctl daemon-reload || true
fi

cat <<'EOF'
borgmatic-manager installed.

Next steps:
  1. Install borgmatic >= 2.1 and borg >= 1.4 (distro packages often lag):
       sudo uv tool install borgmatic   # or: pipx install borgmatic
  2. Edit /etc/borgmatic-manager/manager.yaml (repository, passphrase)
  3. Label your containers (borgmatic-manager.enable=true, .group=<name>)
  4. systemctl enable --now borgmatic-manager
  5. Initialize the repository with the command the first cycle prints:
       journalctl -u borgmatic-manager | grep repo-create
     (it will be: borgmatic-manager borgmatic <group> repo-create ...)
EOF
