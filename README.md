# borgmatic-manager

> **⚠️ v2 pivot in progress.** This branch is mid-rewrite from container-based
> execution to a host systemd service (see
> [.planning/v2-host-pivot-SPEC.md](.planning/v2-host-pivot-SPEC.md)).
> The deployment instructions below describe the old v1 model and will not
> work until the pivot completes; this README is rewritten in the final phase.

Automated backup orchestration for Docker and Podman. Discovers labeled containers and volumes, generates [borgmatic](https://torsion.org/borgmatic/) configurations, and runs periodic backups — all without manual config files.

## How It Works

1. **Discover** — scans Docker/Podman for volumes and containers with `borgmatic-manager.*` labels
2. **Generate** — builds per-group borgmatic YAML configs from labels + defaults
3. **Backup** — spins up ephemeral borgmatic containers to run `borg create`
4. **Repeat** — runs on a configurable schedule and reacts to container/volume events

```
              ┌─────────────────────────┐
              │   borgmatic-manager     │
              │                         │
              │  Scheduler ──► Discover │
              │       │        │        │
              │  Listener      Generate │
              │  (events)      │        │
              │                Run      │
              └────────────────┼────────┘
                               │
                  ┌────────────┼────────────┐
                  │            │            │
             borgmatic    borgmatic    borgmatic
             (group-a)   (group-b)   (group-c)
                  │            │            │
                  └────────────┼────────────┘
                               │
                          Borg Repository
```

## Quick Start

### 1. Label your volumes

```yaml
# docker-compose.yaml
services:
  myapp:
    image: myapp:latest
    volumes:
      - app-data:/data

volumes:
  app-data:
    labels:
      borgmatic-manager.backup: "true"
      borgmatic-manager.group: "myapp"
```

### 2. Configure borgmatic-manager

Create a `config/manager.yaml`:

```yaml
manager:
  period: "1h"

borgmatic:
  repositories:
    - path: /mnt/borg-repository
  keep_daily: 7
  keep_weekly: 4
  keep_monthly: 6
  checks:
    - name: repository
      frequency: "1 week"
    - name: archives
      frequency: "1 month"
```

### 3. Run borgmatic-manager

```bash
docker run -d \
  --name borgmatic-manager \
  -v /var/run/docker.sock:/var/run/docker.sock \
  -v ./config:/etc/borgmatic-manager:ro \
  -v borgmatic-cache:/root/.cache/borg \
  borgmatic-manager
```

That's it. borgmatic-manager discovers `app-data`, generates a borgmatic config for the `myapp` group, and runs backups every hour.

## Labels Reference

### Volume Labels

| Label | Required | Description |
|-------|----------|-------------|
| `borgmatic-manager.backup` | Yes | Set to `"true"` to enable backup |
| `borgmatic-manager.group` | Yes | Backup group name (volumes sharing a group are backed up together) |

### Container Labels (for database backups)

| Label | Required | Description |
|-------|----------|-------------|
| `borgmatic-manager.group` | Yes | Must match a volume group |
| `borgmatic-manager.db.{n}.type` | Yes | `postgresql`, `mysql`, `mariadb`, or `sqlite` |
| `borgmatic-manager.db.{n}.name` | Yes | Database name |
| `borgmatic-manager.db.{n}.username` | Yes | Database user |
| `borgmatic-manager.db.{n}.password` | No | Database password |
| `borgmatic-manager.db.{n}.hostname` | No | Database hostname |
| `borgmatic-manager.db.{n}.port` | No | Database port |
| `borgmatic-manager.db.{n}.network` | No | Docker network to connect borgmatic container to |
| `borgmatic-manager.db.{n}.options` | No | Extra dump options (e.g., `--no-owner`) |

`{n}` is a zero-based index. Multiple databases use incremented indices (0, 1, 2...). Gaps are allowed.

## Examples

### Back up multiple volumes in one group

```yaml
volumes:
  app-data:
    labels:
      borgmatic-manager.backup: "true"
      borgmatic-manager.group: "myapp"
  app-uploads:
    labels:
      borgmatic-manager.backup: "true"
      borgmatic-manager.group: "myapp"
```

Both volumes are mounted and backed up together in a single borgmatic run.

### Back up a PostgreSQL database

```yaml
services:
  postgres:
    image: postgres:16
    labels:
      borgmatic-manager.group: "myapp"
      borgmatic-manager.db.0.type: "postgresql"
      borgmatic-manager.db.0.name: "appdb"
      borgmatic-manager.db.0.username: "postgres"
      borgmatic-manager.db.0.password: "secret"
      borgmatic-manager.db.0.network: "backend"
```

### Multiple databases on one container

```yaml
services:
  postgres:
    image: postgres:16
    labels:
      borgmatic-manager.group: "myapp"
      borgmatic-manager.db.0.type: "postgresql"
      borgmatic-manager.db.0.name: "appdb"
      borgmatic-manager.db.0.username: "postgres"
      borgmatic-manager.db.1.type: "postgresql"
      borgmatic-manager.db.1.name: "analytics"
      borgmatic-manager.db.1.username: "postgres"
```

### Per-group config overrides

Create `config/groups/myapp.yaml` to override defaults for a specific group:

```yaml
repositories:
  - path: /mnt/remote-repo
keep_daily: 14
keep_weekly: 8
```

This deep-merges with the base `borgmatic:` config from `manager.yaml`.

## Configuration

### manager.yaml

| Key | Default | Description |
|-----|---------|-------------|
| `manager.period` | `"1h"` | Backup cycle interval (Go duration: `30m`, `1h`, `24h`) |
| `manager.borgmatic_image` | `ghcr.io/borgmatic-collective/borgmatic:latest` | Borgmatic container image |
| `borgmatic.*` | — | Default borgmatic settings applied to all groups |

### Environment Variables

| Variable | Default | Description |
|----------|---------|-------------|
| `CONFIG_DIR` | `/etc/borgmatic-manager` | Config directory path |
| `CONTAINER_SOCKET` | `/var/run/docker.sock` | Docker/Podman socket path |
| `BORGMATIC_IMAGE` | (from config) | Override borgmatic image (takes precedence over config) |

## Deployment

### Docker Compose

```bash
docker compose -f deploy/compose/docker-compose.yaml up -d
```

See [deploy/compose/docker-compose.yaml](deploy/compose/docker-compose.yaml) for the full example.

### Podman Quadlet (systemd)

```bash
# Rootless
cp deploy/quadlet/* ~/.config/containers/systemd/
systemctl --user daemon-reload
systemctl --user start borgmatic-manager

# Root
cp deploy/quadlet/* /etc/containers/systemd/
systemctl daemon-reload
systemctl start borgmatic-manager
```

See [deploy/quadlet/](deploy/quadlet/) for the unit files.

### Build from Source

```bash
docker build -t borgmatic-manager .
```

The multi-stage Dockerfile compiles a static Go binary and packages it in a minimal scratch image (~10MB).

## Architecture

```
internal/
├── models/        # Core data types (BackupState, VolumeGroup, DatabaseConfig)
├── discovery/     # Label parsing and Docker volume/container queries
├── config/        # YAML loading, deep merge, borgmatic config generation
├── runtime/       # Container runtime abstraction (Docker/Podman)
├── runner/        # Ephemeral borgmatic container lifecycle
├── scheduler/     # Periodic backup ticker
├── orchestrator/  # Main event loop (scheduler + event listener)
└── events/        # Container event stream with debouncing
```

Key design decisions:
- **Label-driven** — no manual per-service config files needed
- **Ephemeral containers** — borgmatic runs in disposable containers, not a long-lived sidecar
- **Per-group parallelism** — groups back up concurrently with per-group mutex to prevent overlaps
- **Event-reactive** — re-discovers when containers/volumes are created or removed (5s debounce)
- **Atomic config writes** — temp file + rename to prevent partial reads

## License

See [LICENSE](LICENSE) for details.
