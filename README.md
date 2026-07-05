# borgmatic-manager

Label-driven backup orchestration for Docker and Podman. A host systemd
service that discovers labeled containers and volumes, generates
[borgmatic](https://torsion.org/borgmatic/) configurations, and runs periodic,
snapshot-consistent backups — no per-service config files.

## How it works

1. **Discover** — watches the Docker/Podman socket for volumes and containers
   with `borgmatic-manager.*` labels (periodically and on create/remove events)
2. **Generate** — compiles per-group borgmatic YAML from labels + your defaults
3. **Backup** — runs host-installed borgmatic per group:
   `create prune compact check`
4. **Snapshots** — on btrfs/zfs/LVM hosts, borgmatic's built-in hooks snapshot
   the filesystem for crash-consistent backups

```
                     host (systemd)
   ┌───────────────────────────────────────────────┐
   │  borgmatic-manager                            │
   │  events ──► debounce ──► orchestrator         │
   │  (socket)                │                    │
   │  scheduler ──► discover ──► generate ──► run  │
   └──────────────────────────────────────────┼────┘
                                              ▼
                                    borgmatic (host)
                                    ├─ btrfs/zfs/lvm snapshots
                                    ├─ borg create/prune/check
                                    └─ database dumps
```

The manager is deliberately thin: it is a **label-to-config compiler plus
scheduler**. Retention, consistency checks, database dumps, snapshots, and
notifications are all borgmatic features the manager configures — never
reimplements.

## Requirements

| Dependency | Minimum | Notes |
|---|---|---|
| borgmatic | **2.1.0** | distro packages usually too old — use `sudo uv tool install borgmatic` or `pipx install borgmatic` (as root) |
| borg | **1.4** | needed for original-path recording with snapshot hooks |
| Docker or Podman | — | socket access; rootless Podman supported with [limitations](#rootless-podman) |
| DB client tools | — | `pg_dump`/`psql`, `mariadb-dump`, `mysqldump`, `sqlite3` on the host, per database type you back up |

## Quick start

**1. Install** (binary + unit from a [release](https://github.com/lugoues/borgmatic-manager/releases), or from source):

```bash
sudo make install     # builds and installs binary, unit, default config
sudo uv tool install borgmatic   # if you don't have borgmatic >= 2.1
```

**2. Label your volumes** — ⚠️ volume labels are **immutable**: they only
apply at creation. For existing volumes see [migrating existing
volumes](#migrating-existing-volumes).

```yaml
# docker-compose.yaml
volumes:
  app-data:
    labels:
      borgmatic-manager.backup: "true"
      borgmatic-manager.group: "myapp"
```

**3. Configure** `/etc/borgmatic-manager/manager.yaml`:

```yaml
manager:
  period: "1h"
borgmatic:
  repositories:
    - path: /mnt/borg-repository        # or ssh://user@host/./repo
  encryption_passphrase: "change-me"    # see Secrets for better options
  keep_daily: 7
```

**4. Start, then initialize the repository** — repositories are never created
automatically. The first cycle fails with a guided error that prints the
exact command:

```bash
sudo systemctl enable --now borgmatic-manager
journalctl -u borgmatic-manager | grep repo-create
# then run the printed command, e.g.:
sudo borgmatic --config /run/borgmatic-manager/configs/myapp.yaml repo-create --encryption repokey-blake2
```

The next cycle backs up. Verify labels any time with
`sudo borgmatic-manager discover`.

## Labels reference

### Volume labels

| Label | Required | Description |
|-------|----------|-------------|
| `borgmatic-manager.backup` | Yes | `"true"` to enable backup |
| `borgmatic-manager.group` | Yes | Backup group; volumes sharing a group back up together |

Only `local`-driver volumes are supported. Volumes with mount options
(NFS/CIFS) are backed up only while mounted; other drivers are skipped with a
warning. Anything carrying a `borgmatic-manager.*` label that doesn't parse
produces a warning in the logs — typos are never silent.

### Container labels (database backups)

| Label | Required | Description |
|-------|----------|-------------|
| `borgmatic-manager.group` | Yes | Group to attach the databases to |
| `borgmatic-manager.db.{n}.type` | Yes | `postgresql`, `mysql`, `mariadb`, or `sqlite` |
| `borgmatic-manager.db.{n}.name` | Yes | Database name |
| `borgmatic-manager.db.{n}.username` | Yes* | DB user (*not for sqlite*) |
| `borgmatic-manager.db.{n}.password` | No | DB password (see [Secrets](#secrets)) |
| `borgmatic-manager.db.{n}.hostname` | No | Host-reachable address — switches to hostname mode |
| `borgmatic-manager.db.{n}.port` | No | Container-internal port (or published port in hostname mode) |
| `borgmatic-manager.db.{n}.volume` | sqlite | Volume containing the database file |
| `borgmatic-manager.db.{n}.path` | sqlite | Path of the `.db` file inside that volume |

`{n}` is a zero-based index; gaps are allowed. The v1 `db.{n}.network` label
is deprecated and ignored.

### How database connections work

Dump clients (`pg_dump`, `mariadb-dump`, …) run **on the host**. Two modes:

- **Container mode (default):** borgmatic resolves the labeled container's
  bridge IP by name (requires the docker/podman CLI). Works for containers on
  bridge networks under a root engine.
- **Hostname mode:** set `db.{n}.hostname` (e.g. `127.0.0.1` plus a published
  `port`). **Required** for host-network containers and rootless engines —
  the manager refuses to generate container mode there and logs what to do.

PostgreSQL caveat: `pg_dump` refuses servers newer than itself. Match your
host client to the container's major version, or use the officially-supported
docker-exec pattern in a group override (`groups/myapp.yaml`):

```yaml
postgresql_databases:
  - name: appdb
    hostname: 127.0.0.1
    username: postgres
    pg_dump_command: docker exec my_pg_container pg_dump
    pg_restore_command: docker exec -i my_pg_container pg_restore
    psql_command: docker exec -i my_pg_container psql
```

## Filesystem snapshots (btrfs / zfs / LVM)

Enable in `manager.yaml` (applies to all groups) or per group:

```yaml
borgmatic:
  btrfs:      # or zfs: / lvm:
```

borgmatic snapshots the subvolume/dataset **containing** each source
directory and cleans up afterward; archives record the original paths (needs
borg ≥ 1.4). Groups with snapshot hooks serialize with each other — borgmatic
snapshot cleanup is not concurrency-safe.

**Granularity matters.** Docker/Podman create volumes as plain directories,
so the snapshot unit is whatever subvolume/dataset contains
`/var/lib/docker/volumes`. On many hosts that's the root filesystem — the
manager warns when the volumes directory is not its own boundary. Dedicated
setup (greenfield):

```bash
# btrfs (before the first volume is created)
btrfs subvolume create /var/lib/docker/volumes
# zfs
zfs create -o mountpoint=/var/lib/docker/volumes pool/docker-volumes
```

**Migrating existing data:** stop the daemon, move the data aside, create the
subvolume/dataset at the path, copy back (`cp -a --reflink=auto`), start the
daemon.

Note: btrfs snapshots are created *inside* the source subvolume as
`.borgmatic-snapshot-*`; if the daemon restarts mid-backup Docker may list a
phantom volume by that name until it's removed. Harmless, but don't prune it
mid-backup.

## Secrets

- **Labels are visible** to anyone with socket access (`docker inspect`).
  For sensitive credentials use borgmatic's credential syntax as the label
  value — it passes through generation and resolves at backup time:
  `borgmatic-manager.db.0.password: "{credential container db_password}"`
- **Repository passphrase** without plaintext:
  ```bash
  systemd-creds encrypt --name=borgmatic.pw secret.txt /etc/credstore.encrypted/borgmatic.pw
  ```
  Uncomment `LoadCredentialEncrypted=borgmatic.pw` in the unit and set
  `encryption_passphrase: "{credential systemd borgmatic.pw}"`.
- Generated configs (which may contain credentials) are 0600 in a 0700 tmpfs
  directory, removed on service stop, and reconciled every cycle.

## Remote repositories (SSH)

The service runs as root; borg connects as root. One-time setup:

```bash
sudo ssh-keygen -t ed25519           # if root has no key
sudo ssh-copy-id borg@backup-host    # or install the key manually
sudo ssh-keyscan backup-host | sudo tee -a /root/.ssh/known_hosts
```

Or set `ssh_command: ssh -o StrictHostKeyChecking=accept-new` in the
`borgmatic:` map instead of pre-seeding known_hosts.

## Restoring

Restores are plain borgmatic — the manager stays out of the way:

- **Manager running:** use the generated config directly:
  `sudo borgmatic --config /run/borgmatic-manager/configs/myapp.yaml extract --archive latest`
  (databases: `... restore --archive latest`; the target container must be
  running in container mode).
- **Configs gone** (service stopped/rebooted): regenerate them from live
  labels: `sudo borgmatic-manager generate -output /tmp/restore`.
- **Host lost:** borgmatic embeds its config in every archive —
  `borgmatic config bootstrap --repository ssh://…` recovers it, then extract
  as above.

## Monitoring

Put any borgmatic monitoring hook in the `borgmatic:` map — it applies to
every group. A dead manager means missed pings, which your monitor alerts on:

```yaml
borgmatic:
  healthchecks:
    ping_url: https://hc-ping.com/your-uuid
```

Backup completion/warning counts are also in the JSON logs
(`journalctl -u borgmatic-manager`).

## Rootless Podman

Supported via the user unit
([deploy/systemd/borgmatic-manager.user.service](deploy/systemd/borgmatic-manager.user.service)):

```bash
systemctl --user enable --now podman.socket
mkdir -p ~/.config/borgmatic-manager && cp config/manager.yaml ~/.config/borgmatic-manager/
cp deploy/systemd/borgmatic-manager.user.service ~/.config/systemd/user/borgmatic-manager.service
systemctl --user daemon-reload && systemctl --user enable --now borgmatic-manager
loginctl enable-linger $USER
```

Limitations: no snapshot hooks (except btrfs's documented non-root path);
database backups need hostname mode (userspace networking has no reachable
container IPs — the manager tells you exactly this if you forget); volume
files owned by subordinate UIDs are skipped with a warning (fix ownership
with `podman unshare chown`).

## CLI

```
borgmatic-manager run                  # the daemon (default)
borgmatic-manager discover             # one-shot: print discovered groups
borgmatic-manager generate -output D   # one-shot: write configs to D
borgmatic-manager version
```

## Configuration reference

### manager.yaml

| Key | Default | Description |
|-----|---------|-------------|
| `manager.period` | `"1h"` | Backup cycle interval (Go duration) |
| `manager.borgmatic_path` | auto | borgmatic binary (PATH, then `/root/.local/bin`) |
| `manager.actions` | `[create, prune, compact, check]` | borgmatic actions per cycle, in order |
| `manager.run_timeout` | none | bound one group's run; SIGTERM → SIGKILL escalation |
| `borgmatic.*` | — | defaults merged into every group's config |

Per-group overrides live in `/etc/borgmatic-manager/groups/{group}.yaml` and
deep-merge over the defaults (lists replace). Anything borgmatic supports is
valid — including backing up **bind-mounted host paths** by adding
`source_directories` to a group override.

### Environment

| Variable | Default | Description |
|----------|---------|-------------|
| `CONFIG_DIR` | `/etc/borgmatic-manager` | manager.yaml + groups/ |
| `RUNTIME_DIR` | `/run/borgmatic-manager` | generated configs, borgmatic runtime dir |
| `STATE_DIR` | `/var/lib/borgmatic-manager` | borgmatic check-frequency state |
| `CONTAINER_SOCKET` | `/var/run/docker.sock` | Docker/Podman socket |
| `BORGMATIC_PATH` | — | borgmatic binary override |

## Concurrency model

Groups run in parallel **except**: groups sharing a repository serialize
(Borg 1.x locks repositories exclusively), and snapshot-enabled groups
serialize globally. Overlapping cycles of the same group are skipped, never
queued. Generated configs set `lock_wait: 120` so a manually-run borgmatic
doesn't instantly fail a cycle.

## Troubleshooting

- `sudo borgmatic-manager discover` — did my labels work? (near-miss labels
  warn here and in the journal)
- `journalctl -u borgmatic-manager` — JSON logs; per-group results include
  `exit_code`, `warnings`, `duration`
- "repository does not exist" — run the printed `repo-create` command (once)
- database dumps fail — is the client tool installed on the host? right
  major version for postgres? host-reachable address for host-network /
  rootless containers?

## Development

```bash
make test      # vet + unit tests
make race      # race-detector run
make e2e       # end-to-end test (needs docker, borgmatic, borg, sudo)
make build     # bin/borgmatic-manager
```

Architecture: `internal/{runtime,discovery,config,runner,scheduler,events,orchestrator}` —
see [.planning/v2-host-pivot-SPEC.md](.planning/v2-host-pivot-SPEC.md) for
the full design and its rationale.

## License

[MIT](LICENSE)
