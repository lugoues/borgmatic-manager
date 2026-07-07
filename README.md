# borgmatic-manager

Label-driven backup orchestration for Docker and Podman. A host systemd
service that discovers labeled containers, generates
[borgmatic](https://torsion.org/borgmatic/) configurations, and runs periodic,
snapshot-consistent backups — no per-service config files.

## How it works

1. **Discover** — watches the Docker/Podman socket for containers with
   `borgmatic-manager.*` labels (periodically and on create/remove events);
   a labeled container's named volumes and databases join its backup group
2. **Generate** — compiles per-group borgmatic YAML from labels + your defaults
3. **Backup** — runs host-installed borgmatic per group:
   `create prune compact check`; database dumps run in short-lived helper
   containers joined to the database's network namespace
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
| `sqlite3` | — | on the host, only if you back up sqlite databases (postgres/mysql/mariadb dumps run inside helper containers — no host clients needed) |

## Quick start

**1. Install** (binary + unit from a [release](https://github.com/lugoues/borgmatic-manager/releases), or from source):

```bash
# From a release: grab the .deb/.rpm and install it
sudo apt install ./borgmatic-manager_*_linux_amd64.deb

# Or from source
mise run install      # builds and installs binary, unit, default config (sudo inside)

sudo uv tool install borgmatic   # if you don't have borgmatic >= 2.1
```

**2. Label your containers** — labels live on the *service*, not the volume,
so a normal `docker compose up` after editing applies them:

```yaml
# docker-compose.yaml
services:
  myapp:
    image: myapp:latest
    volumes:
      - app-data:/data
    labels:
      borgmatic-manager.enable: "true"   # back up this service's named volumes
      borgmatic-manager.group: "myapp"

volumes:
  app-data:
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
# then run the printed command:
sudo borgmatic-manager borgmatic myapp repo-create --encryption repokey-blake2
```

The next cycle backs up. Verify labels any time with
`sudo borgmatic-manager discover`.

## Labels reference

All labels go on **containers** (volume labels are not supported — they are
immutable after creation, which made them a trap).

| Label | Description |
|-------|-------------|
| `borgmatic-manager.group` | Backup group. Required for a container to participate at all; containers sharing a group back up together. |
| `borgmatic-manager.enable` | `"true"` to back up this container's named volumes. |
| `borgmatic-manager.volumes` | Optional comma-separated filter: volume names or in-container mount paths (e.g. `app-data,/uploads`). Omitted or empty: all named volumes (anonymous volumes excluded). |
| `borgmatic-manager.db.{n}.*` | Database dump definitions (below). |
| `borgmatic-manager.config.<option>` | Any borgmatic option for this group (below). |
| `borgmatic-manager.spec` | The whole configuration as one JSON blob (below) — alternative to all of the above. |

Only `local`-driver volumes are supported; volumes with mount options
(NFS/CIFS) are backed up only while mounted; other drivers are skipped with a
warning. Anything carrying a `borgmatic-manager.*` label that doesn't parse
produces a warning — typos are never silent (`borgmatic-manager discover`
shows the result).

### Database labels

| Label | Required | Description |
|-------|----------|-------------|
| `borgmatic-manager.db.{n}.type` | Yes | `postgresql`, `mysql`, `mariadb`, or `sqlite` |
| `borgmatic-manager.db.{n}.name` | Yes | Database name |
| `borgmatic-manager.db.{n}.username` | Yes* | DB user (*not for sqlite*) |
| `borgmatic-manager.db.{n}.password` | No | DB password (see [Secrets](#secrets)) |
| `borgmatic-manager.db.{n}.hostname` | No | Host-reachable address — switches to hostname mode (host client tools required) |
| `borgmatic-manager.db.{n}.port` | No | Database port (container-internal in the default mode) |
| `borgmatic-manager.db.{n}.mode` | No | `exec` to exec into the DB container instead of a helper (postgresql only) |
| `borgmatic-manager.db.{n}.volume` | sqlite | Volume containing the database file |
| `borgmatic-manager.db.{n}.path` | sqlite | Path of the `.db` file inside that volume |

`{n}` is a zero-based index; gaps are allowed. The v1 `db.{n}.network` label
is deprecated and ignored.

Each group backs up into one archive per cycle, containing every volume at
a volume-named path (`myvol/_data/...` — the storage location under
`/var/lib/docker` is stripped) plus any database dumps. Exception: groups
with snapshot hooks keep full host paths (the hooks own the path rewriting).

Archives are named `{hostname}-{group}-{now:%Y-%m-%d_%H:%M}` by default.
Override `archive_name_format` at any config layer; `{group}` is substituted
by the manager, everything else is a borg placeholder. A repo-per-host setup
can drop the redundant hostname:

```yaml
borgmatic:
  archive_name_format: "{group}-{now:%Y-%m-%d_%H:%M}"
```

Safety rule, enforced at generation: when groups share a repository, the
format must contain the group name (use `{group}`) — retention is scoped by
archive name, so indistinguishable formats would let one group's prune eat
another group's history. Groups with exclusive repositories may use any
format.

### How database dumps run

By default the manager generates borgmatic commands that spawn a
**short-lived helper container from the database container's own image**,
joined to its network namespace (`--network container:<name>`):

- works with **any** network setup — bridge, custom, internal, host, none,
  pods, rootless — no published ports needed
- the dump client is always the **same version as the server** (it ships in
  the same image)
- zero database client tools on the host

You never write these commands; `borgmatic-manager generate` shows what is
produced. Setting `db.{n}.hostname` switches that database to a plain
host-side connection instead (requires `pg_dump`/`mariadb-dump`/`mysqldump`
on the host), and `db.{n}.mode: exec` runs the client inside the DB container
itself (postgresql only — mysql/mariadb dumps stream through a FIFO that an
exec'd client cannot reach).

### Config labels (traefik-style)

Any borgmatic option can be set per group straight from a label — dotted
paths become nested YAML, values are parsed as YAML (numbers, booleans,
`[flow, lists]`):

```yaml
labels:
  borgmatic-manager.config.keep_daily: "14"
  borgmatic-manager.config.healthchecks.ping_url: "https://hc-ping.com/uuid"
  borgmatic-manager.config.repositories: "[{path: ssh://borg@host/./myapp}]"
```

Precedence: `manager.yaml` defaults → `groups/<group>.yaml` → **config
labels** → discovered data (source dirs, database hooks). Typo'd option
names fail the per-run `borgmatic config validate` gate with a precise error.

### One-label JSON spec

Prefer a single document over many labels? `borgmatic-manager.spec` carries
everything at once:

```yaml
labels:
  borgmatic-manager.spec: >-
    {
      "group": "myapp",
      "enable": true,
      "volumes": ["app-data"],
      "db": [
        {"type": "postgresql", "name": "appdb", "username": "postgres", "password": "secret"}
      ],
      "config": {"keep_daily": 14}
    }
```

Fields mirror the flat labels exactly: `group` (required), `enable`,
`volumes` (filter; omit or leave empty for all named volumes), `db` (a list with the same
fields as `db.{n}.*`), `config` (same as `config.*`, arbitrarily nested).

The value is strict JSON. In quadlet `Label=` lines, wrap the whole
assignment in systemd single quotes so the inner JSON quotes need no
escaping (systemd word-splits unquoted values on spaces):

```
Label='borgmatic-manager.spec={"group": "myapp", "enable": true, "volumes": ["app-data"]}'
```

Parsing is strict — an unknown or misspelled field rejects the whole spec with a
warning rather than silently dropping it, and database entries get the same
per-type validation as flat labels. If `spec` is present, any other
`borgmatic-manager.*` labels on that container are ignored (with a warning
listing them): pick one style per container.

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

Everything goes through the passthrough subcommand — it regenerates the
group's config from live labels and hands you borgmatic, so it works even
after a reboot cleared the runtime directory:

```bash
sudo borgmatic-manager borgmatic myapp list
sudo borgmatic-manager borgmatic myapp extract --archive latest
sudo borgmatic-manager borgmatic myapp restore --archive latest   # databases
```

Database restores run through the same generated helper containers as dumps
(the target container must be running). Configs change safely while backups
run: files are replaced atomically and borgmatic reads its config once at
start, so an in-flight run never sees a partial or changed config.

**Host lost entirely:** borgmatic embeds its config in every archive —
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
volume files owned by subordinate UIDs are skipped with a warning (fix
ownership with `podman unshare chown`). Database dumps work normally — the
helper container joins the DB container's network namespace, which needs no
routable container IP.

## CLI

```
borgmatic-manager run                     # the daemon
borgmatic-manager discover                # one-shot: print discovered groups
borgmatic-manager generate --output D     # one-shot: write configs to D
borgmatic-manager borgmatic <group> ...   # run borgmatic against a group
borgmatic-manager version
```

## Configuration reference

### manager.yaml

| Key | Default | Description |
|-----|---------|-------------|
| `manager.period` | `"1h"` | Backup cycle interval (Go duration). Creation cadence and retention are independent: without `keep_hourly`, hourly archives collapse to one per day at prune time |
| `manager.borgmatic_path` | auto | borgmatic binary (PATH, then `/root/.local/bin`) |
| `manager.actions` | `[create, prune, compact, check]` | borgmatic actions per cycle, in order |
| `manager.container_cli` | derived from socket | CLI for generated dump commands (`docker`/`podman`); default follows the connected socket |
| `manager.run_timeout` | none | bound one group's run; SIGTERM → SIGKILL escalation |
| `borgmatic.*` | — | defaults merged into every group's config |

Local tweaks belong in `/etc/borgmatic-manager/conf.d/*.yaml` (`.yml` works too) — full config
fragments (`manager:` and/or `borgmatic:` sections) deep-merged over
`manager.yaml` in lexical filename order. Package upgrades never touch
`/etc`; the shipped default lives at
`/usr/share/borgmatic-manager/manager.yaml` and is copied in only on first
install, so improvements to it reach you via that reference copy without
upgrade prompts.

Per-group overrides live in `/etc/borgmatic-manager/groups/{group}.yaml` —
each file is a borgmatic config fragment (top-level options) deep-merged over
the defaults (lists replace). Anything borgmatic supports is valid — including
backing up **bind-mounted host paths** by adding `source_directories` to a
group override.

**Shared config in and out of BM:** borgmatic's `!include` tag works in
`manager.yaml` and `groups/*.yaml`, so the same shared file can serve
standalone borgmatic configs and the manager:

```yaml
borgmatic:
  <<: !include /etc/borgmatic/common.yaml   # deep merge; local keys win
  keep_daily: 14
```

Relative paths resolve against the including file; includes nest. borgmatic's
`!retain`/`!omit` merge tags are not supported (use group files or config
labels for overrides instead).

### Environment

| Variable | Default | Description |
|----------|---------|-------------|
| `CONFIG_DIR` | `/etc/borgmatic-manager` | manager.yaml + conf.d/ + groups/ |
| `RUNTIME_DIR` | `/run/borgmatic-manager` | generated configs, borgmatic runtime dir |
| `STATE_DIR` | `/var/lib/borgmatic-manager` | schedule state (`schedule.json`) + borgmatic check-frequency state |
| `CONTAINER_SOCKET` | autodetected | Docker/Podman socket; probes `/var/run/docker.sock`, `/run/podman/podman.sock`, `$XDG_RUNTIME_DIR/podman/podman.sock` |
| `BORGMATIC_PATH` | — | borgmatic binary override |

## Scheduling

The schedule is persistent (like a systemd timer with `Persistent=true`):
each group's last successful run is recorded in `$STATE_DIR/schedule.json`,
and a group only runs when its period has elapsed since then **or its
membership changed** (a volume or database joined/left — so a newly labeled
container is backed up within seconds, without re-running everything else).
Consequences:

- Restarts and package upgrades resume the schedule; they don't trigger
  backups. A backup interrupted by the restart is still due and re-runs
  immediately.
- Container create/remove events regenerate configs every time, but only
  run groups whose membership actually changed.
- Failed or interrupted groups stay due and retry; only borgmatic exit 0
  marks success.
- Missing or corrupt schedule state degrades to "everything is due" — the
  failure direction is an extra backup, never a skipped one.

`borgmatic-manager status` shows the resulting schedule: each group's last
run, its outcome (duration, warnings, archive name, exit code, file count
and sizes from borgmatic's create result — captured during the run, so no
repository access is needed), and when the next run is due. For repository-level detail, use
`borgmatic-manager borgmatic <group> info`.

To force an immediate full run: `rm /var/lib/borgmatic-manager/schedule.json`
and restart the service. Manual `borgmatic-manager borgmatic <group> create`
runs bypass the schedule and don't update it.

## Concurrency model

Groups run in parallel **except**: groups sharing a repository serialize
(Borg 1.x locks repositories exclusively), and snapshot-enabled groups
serialize globally. Overlapping cycles of the same group are skipped, never
queued. Generated configs set `lock_wait: 120` so a manually-run borgmatic
doesn't instantly fail a cycle.

## Troubleshooting

- `sudo borgmatic-manager status` — per-group last run, result (duration,
  warnings, exit code), file count and archive size, and the next due time
- `sudo borgmatic-manager discover` — did my labels work? (near-miss labels
  warn here and in the journal)
- `journalctl -u borgmatic-manager` — JSON logs; per-group results include
  `exit_code`, `warnings`, `duration`
- "repository does not exist" — run the printed `repo-create` command (once)
- database dumps fail — is the DB container running? (helper containers join
  its network namespace, so it must be up); in hostname mode, is the client
  tool on the host and the address reachable?

## Development

Tools and tasks are managed with [mise](https://mise.jdx.dev) (`mise install` once):

```bash
mise run test      # vet + unit tests
mise run race      # race-detector run
mise run e2e       # end-to-end test (docker+compose, borgmatic, borg, sudo)
mise run e2e-dind  # hermetic e2e inside a docker-in-docker host (needs only docker)
mise run build     # bin/borgmatic-manager
```

Architecture: `internal/{runtime,discovery,config,runner,scheduler,events,orchestrator}` —
see [.planning/v2-host-pivot-SPEC.md](.planning/v2-host-pivot-SPEC.md) for
the full design and its rationale.

## License

[MIT](LICENSE)
