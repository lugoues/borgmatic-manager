# Borgmatic Manager

## Overview

A container-runtime-agnostic service that dynamically discovers volumes and containers labeled for backup, generates borgmatic configurations grouped by service, and spawns ephemeral borgmatic containers to execute backups. The manager itself runs as a long-lived container (e.g., via Podman Quadlet, Docker Compose, etc.) and communicates with the container runtime via the Docker-compatible API socket.

## Architecture

```
┌───────────────────────────────────────────────────────────┐
│  Container Host                                           │
│                                                           │
│  ┌────────────────────────┐                               │
│  │  borgmatic-manager     │──── listens ──► Container     │
│  │  (long-lived container)│                 Runtime       │
│  │                        │                 Socket        │
│  │  - discovers volumes   │                 /events       │
│  │  - discovers containers│                               │
│  │  - generates configs   │                               │
│  │  - periodically spawns │                               │
│  │    borgmatic containers │                               │
│  └──────┬─────────────────┘                               │
│         │ container runtime API: create + start            │
│         ▼                                                 │
│  ┌──────────────────────┐                                 │
│  │ borgmatic container  │  (ephemeral, per-group-run)     │
│  │  --volume cfg:/cfg   │                                 │
│  │  --volume src:/mnt/… │                                 │
│  │  --network db-net    │                                 │
│  └──────────────────────┘                                 │
└───────────────────────────────────────────────────────────┘
```

## 1. Labeling Convention

The manager discovers backup intent from labels on two types of objects:

- **Volumes**: declare which volumes to back up and which group they belong to
- **Containers**: declare database hooks (dump before backup) and associate with a group

### Volume Labels

Applied to container volumes (Quadlet `.volume` files, Docker volume labels, etc.).

| Label | Required | Description |
|---|---|---|
| `borgmatic-manager.backup=true` | Yes | Opts this volume into backup |
| `borgmatic-manager.group={name}` | Yes | Groups this volume with others. All volumes sharing a group are backed up together in one borgmatic run. |

### Container Labels (Database Hooks)

Applied to database containers. Uses indexed keys to support multiple databases per container, modeled after Traefik's label convention.

| Label | Required | Description |
|---|---|---|
| `borgmatic-manager.group={name}` | Yes | Associates this container's DB hooks with a backup group |
| `borgmatic-manager.db.{n}.type` | Yes | Database type: `postgresql`, `mysql`, `mariadb`, `sqlite` |
| `borgmatic-manager.db.{n}.name` | Yes | Database name to dump |
| `borgmatic-manager.db.{n}.username` | Yes | Database user |
| `borgmatic-manager.db.{n}.password` | No | Database password (prefer secrets/env injection — see Secrets section) |
| `borgmatic-manager.db.{n}.hostname` | No | Hostname to connect to (defaults to the container's name on the shared network) |
| `borgmatic-manager.db.{n}.network` | No | Network the borgmatic container must join to reach this DB |
| `borgmatic-manager.db.{n}.port` | No | Port if non-default |
| `borgmatic-manager.db.{n}.options` | No | Additional dump options (e.g., `--no-owner`) |

Where `{n}` is a zero-indexed integer: `borgmatic-manager.db.0.type`, `borgmatic-manager.db.1.type`, etc.

### Example: Quadlet Volume + Container

```ini
# ~/.config/containers/systemd/myapp-data.volume
[Volume]
Label=borgmatic-manager.backup=true
Label=borgmatic-manager.group=myapp
```

```ini
# ~/.config/containers/systemd/myapp-uploads.volume
[Volume]
Label=borgmatic-manager.backup=true
Label=borgmatic-manager.group=myapp
```

```ini
# ~/.config/containers/systemd/myapp-db.container
[Container]
Image=docker.io/library/postgres:16
Label=borgmatic-manager.group=myapp
Label=borgmatic-manager.db.0.type=postgresql
Label=borgmatic-manager.db.0.name=myapp_production
Label=borgmatic-manager.db.0.username=postgres
Label=borgmatic-manager.db.0.hostname=myapp-db
Label=borgmatic-manager.db.0.network=myapp-network
# ...
```

Both volumes and the DB container are in the `myapp` group. When borgmatic runs for this group, it mounts the two volumes and connects to `myapp-db` on `myapp-network` to dump `myapp_production` before the borg backup.

## 2. Borgmatic Config Generation

### Approach: Structured Config

The manager constructs borgmatic configuration as Go structs, applies per-group overrides via deep merge of YAML files, and marshals the result to YAML. This is type-safe and testable.

Go templates for user-customizable config generation may be considered in a future version.

### Config Layout

```
/config/
  manager.yaml                # Manager's own configuration (global defaults, runtime settings)
  groups/
    myapp.yaml                # Per-group overrides (optional)
    nextcloud.yaml
  generated/                  # Manager writes rendered borgmatic configs here
    myapp.yaml
    nextcloud.yaml
```

### Manager Configuration (`manager.yaml`)

Global settings for the manager and default borgmatic values that apply to all groups unless overridden.

```yaml
# manager.yaml
period: 15m  # How often to invoke borgmatic. Borgmatic's own schedule config
             # determines whether it actually runs a backup on each invocation.

defaults:
  repository_path: "ssh://borg@backup-server/./{hostname}"
  encryption_passcommand: "cat /etc/borg/passphrase"
  schedule: "0 2 * * *"  # borgmatic's built-in schedule (per-group overridable)
  retention:
    keep_hourly: 0
    keep_daily: 7
    keep_weekly: 4
    keep_monthly: 6
  consistency:
    checks:
      - name: repository
        frequency: 2 weeks
      - name: archives
        frequency: 4 weeks
```

The borgmatic image is configured via the `BORGMATIC_IMAGE` environment variable on the manager container (default: `ghcr.io/borgmatic-collective/borgmatic:latest`).

### Per-Group Overrides (`groups/{group}.yaml`)

Optional YAML files that override or extend the defaults for a specific group.

```yaml
# groups/myapp.yaml
schedule: "0 */6 * * *"  # Override: every 6 hours instead of default 2am

retention:
  keep_daily: 14
  keep_weekly: 8
```

### Generated Borgmatic Config

The manager deep-merges: `defaults` → `groups/{group}.yaml` → discovered volumes/databases, then writes valid borgmatic YAML.

```yaml
# generated/myapp.yaml (output — not user-edited)
source_directories:
  - /mnt/sources

working_directory: /mnt/sources

repositories:
  - path: ssh://borg@backup-server/./myhostname
    label: myhostname

archive_name_format: "{hostname}-myapp-{now:%Y-%m-%d_%H:%M}"

encryption_passcommand: "cat /etc/borg/passphrase"

schedule: "0 */6 * * *"

retention:
  keep_hourly: 0
  keep_daily: 14
  keep_weekly: 8
  keep_monthly: 6

checks:
  - name: repository
    frequency: 2 weeks
  - name: archives
    frequency: 4 weeks

postgresql_databases:
  - name: myapp_production
    hostname: myapp-db
    port: 5432
    username: postgres
```

### Single Repository Per Machine

All groups on a machine share a single borg repository. Archive names organize the groups:

```
Repository: ssh://borg@backup-server/./myhostname
  Archive: myhostname-myapp-2026-03-05_02:00
    myapp-data/
      ... (volume contents)
    myapp-uploads/
      ... (volume contents)
    .borgmatic/
      postgresql_databases/
        myapp_production

  Archive: myhostname-nextcloud-2026-03-05_02:00
    nextcloud-data/
      ... (volume contents)
    nextcloud-db-dumps/
      ...
```

Volumes are mounted under `/mnt/sources/{volume_name}` in the borgmatic container. `working_directory: /mnt/sources` ensures archive paths are clean (no `/mnt/sources/` prefix).

### Borg 2.x Concurrency

Borg 2.x supports concurrent operations on the same repository:
- `create`, `prune`, `delete` — can run in parallel
- `compact`, `check` — require exclusive lock

Since all groups share one repository, multiple borgmatic containers can create archives simultaneously. The manager should schedule `compact` and `check` operations to avoid conflicting with concurrent creates (borgmatic's `frequency` setting on checks helps here, as does running checks at off-peak times).

> **TODO**: Validate with a test — run two borgmatic containers concurrently creating archives in the same borg 2.x repository.

## 3. Manager Service

### Language: Go

- Single static binary, minimal container image (scratch/distroless)
- Native concurrency (goroutines for event listener, ticker, runner)
- Docker client library works with any Docker-compatible API
- Precedent in the container tooling space (Traefik, docker-socket-proxy, etc.)

### Container Runtime Abstraction

The manager uses the Docker-compatible API, which both Docker and Podman expose. The socket path is configurable via `CONTAINER_SOCKET` environment variable. No Podman-specific or Docker-specific assumptions.

```go
// The manager talks to any Docker-compatible API
type ContainerRuntime interface {
    ListVolumes(ctx context.Context, filter VolumeFilter) ([]Volume, error)
    ListContainers(ctx context.Context, filter ContainerFilter) ([]Container, error)
    EventStream(ctx context.Context, filter EventFilter) (<-chan Event, error)
    CreateContainer(ctx context.Context, config ContainerConfig) (string, error)
    StartContainer(ctx context.Context, id string) error
    WaitContainer(ctx context.Context, id string) (int, error)
    RemoveContainer(ctx context.Context, id string) error
}
```

### Core Architecture

```
┌─────────────────────────────────────────────────────┐
│                 main()                              │
│                                                     │
│  ┌─────────────┐  ┌────────────┐  ┌──────────────┐ │
│  │  Discovery   │  │  Ticker    │  │  Runner      │ │
│  │  (volumes +  │  │  (period)  │  │  (spawns     │ │
│  │  containers) │  │            │  │  borgmatic   │ │
│  └──────┬───────┘  └─────┬──────┘  │  containers) │ │
│         │                │         └──────┬───────┘ │
│         │         ┌──────┴──────┐         │         │
│         │         │ Event       │         │         │
│         └────────►│ Listener    ├─────────┘         │
│                   │ (re-discover│                    │
│                   │  on change) │                    │
│                   └─────────────┘                    │
└─────────────────────────────────────────────────────┘
```

### Discovery

Two-phase discovery:

1. **Volume discovery**: Query volumes with label `borgmatic-manager.backup=true`, group by `borgmatic-manager.group`
2. **Container discovery**: Query containers with label `borgmatic-manager.group=*`, parse `borgmatic-manager.db.*` labels, associate with groups

If a labeled volume or container is unavailable (e.g., DB container not running), the error is logged and the backup proceeds with what's available — borgmatic will report the database hook failure through its own error/notification system.

```go
func Discover(ctx context.Context, runtime ContainerRuntime) (*BackupState, error) {
    // Phase 1: volumes
    volumes, _ := runtime.ListVolumes(ctx, VolumeFilter{
        Labels: map[string]string{"borgmatic-manager.backup": "true"},
    })

    state := NewBackupState()
    for _, vol := range volumes {
        group := vol.Labels["borgmatic-manager.group"]
        state.AddVolume(group, vol)
    }

    // Phase 2: containers with database labels
    containers, _ := runtime.ListContainers(ctx, ContainerFilter{
        LabelExists: "borgmatic-manager.group",
    })

    for _, ctr := range containers {
        group := ctr.Labels["borgmatic-manager.group"]
        databases := ParseDatabaseLabels(ctr.Labels)
        state.AddDatabases(group, databases)
    }

    return state, nil
}
```

### Scheduling

The manager uses a simple **periodic ticker** configured by the `period` setting in `manager.yaml` (e.g., `15m`, `1h`). Each tick, the manager spawns borgmatic for every discovered group.

**Borgmatic handles its own scheduling internally** via its `schedule` configuration. When invoked, borgmatic checks if a backup is actually due based on its schedule. If not due, it exits immediately without doing work. This delegates scheduling logic to borgmatic where it belongs.

```go
func (m *Manager) run(ctx context.Context) {
    ticker := time.NewTicker(m.config.Period)
    defer ticker.Stop()

    for {
        select {
        case <-ticker.C:
            m.runAllGroups(ctx)
        case <-ctx.Done():
            return
        }
    }
}

func (m *Manager) runAllGroups(ctx context.Context) {
    for groupName, group := range m.state.Groups {
        go m.runGroup(ctx, groupName, group)
    }
}
```

### Event Listening

Connect to the container runtime's event stream:

```
GET /events?filters={"type":["volume","container"]}
```

On volume or container create/remove/label-change events, trigger re-discovery and regenerate configs for affected groups.

### Spawning Borgmatic

The manager creates an ephemeral container via the runtime API:

- **Image**: Configurable via `BORGMATIC_IMAGE` env var (default: `ghcr.io/borgmatic-collective/borgmatic:latest`)
- **Volumes mounted**:
  - Generated borgmatic config → `/etc/borgmatic.d/{group}.yaml` (read-only)
  - Each group's backup volumes → `/mnt/sources/{volume_name}` (read-only)
  - Borg repo cache volume → `/root/.cache/borg` (read-write, persistent across runs)
  - SSH keys volume → `/root/.ssh` (read-only, for remote repo access)
  - Borg passphrase secret/file
- **Networks**: Joined to any networks required for database access (from `borgmatic-manager.db.{n}.network` labels)
- **Behavior**: Remove after exit (`--rm`), manager logs exit code and captures output

### Concurrency

Each group runs borgmatic in its own ephemeral container with an isolated filesystem, avoiding borgmatic's limitation around shared runtime files.

Since all groups share a single borg repository and Borg 2.x allows concurrent `create`/`prune`/`delete` operations, **groups can run in parallel**.

The manager holds a per-group mutex to prevent the same group from overlapping with itself (e.g., if a previous run is still in progress when the next tick fires):

```go
type GroupRunner struct {
    mu sync.Mutex
}

func (r *GroupRunner) Run(ctx context.Context) error {
    if !r.mu.TryLock() {
        log.Warn("group already running, skipping")
        return nil
    }
    defer r.mu.Unlock()
    // ... spawn borgmatic container
}
```

### Graceful Shutdown

On SIGTERM, the manager exits immediately. Running borgmatic containers are ephemeral (`--rm`) and will finish on their own — the container runtime manages their lifecycle independently.

## 4. Secrets Management

Secrets (borg passphrase, database passwords) are provided through standard container mechanisms. The manager does not implement its own secrets management.

- **Environment variables**: Passed through to the borgmatic container
- **File mounts**: Bind-mount files containing secrets (e.g., `/run/secrets/borg-passphrase`)
- **Container runtime secrets**: Podman secrets, Docker secrets, systemd `LoadCredential=`, etc.

Database passwords from container labels (`borgmatic-manager.db.{n}.password`) are discouraged for production. Instead:
- Use borgmatic's environment variable interpolation in the generated config
- Or mount a secrets file and reference it via borgmatic's config

## 5. Error Handling & Notifications

The manager is **not responsible for alerting**. It:
- Logs borgmatic container exit codes and stdout/stderr to its own stdout (visible via `journalctl`, `docker logs`, etc.)
- Logs discovery errors (missing volumes, unavailable containers) as warnings

**Alerting is borgmatic's responsibility** via its built-in notification hooks (`on_error`, `ntfy`, `healthchecks`, etc.), configured in the base defaults or per-group overrides.

## 6. Container Runtime Socket

The manager just needs a socket. How the socket is provided is an orchestration concern:
- Rootless Podman: `/run/user/{uid}/podman/podman.sock`
- Root Podman: `/run/podman/podman.sock`
- Docker: `/var/run/docker.sock`
- Socket proxy: A volume binding a socket-proxy for restricted access

The socket path is configured via `CONTAINER_SOCKET` environment variable.

## 7. File Structure

```
borgmatic-manager/
├── Dockerfile                      # Multi-stage Go build
├── go.mod
├── go.sum
├── cmd/
│   └── manager/
│       └── main.go                 # Entry point, wiring
├── internal/
│   ├── config/
│   │   ├── manager.go              # Manager config loading (manager.yaml)
│   │   ├── borgmatic.go            # Borgmatic config struct + YAML marshal
│   │   └── merge.go                # Deep merge for group overrides
│   ├── discovery/
│   │   ├── discovery.go            # Volume + container discovery
│   │   └── labels.go               # Parse borgmatic-manager.db.{n}.* labels
│   ├── events/
│   │   └── listener.go             # Container runtime event stream
│   ├── runner/
│   │   └── runner.go               # Spawn ephemeral borgmatic containers
│   ├── runtime/
│   │   ├── runtime.go              # ContainerRuntime interface
│   │   └── docker.go               # Docker-compatible API implementation
│   └── models/
│       └── models.go               # BackupState, VolumeGroup, DatabaseConfig
├── config/
│   ├── manager.yaml                # Default manager configuration
│   └── groups/                     # Per-group override YAMLs (user-managed)
│       └── .gitkeep
├── deploy/
│   ├── quadlet/                    # Podman Quadlet examples
│   │   ├── borgmatic-manager.container
│   │   ├── borgmatic-manager-config.volume
│   │   ├── borgmatic-generated-config.volume
│   │   ├── borg-repo-cache.volume
│   │   └── borg-ssh.volume
│   └── compose/                    # Docker Compose example
│       └── docker-compose.yaml
└── test/
    ├── labels_test.go
    ├── config_test.go
    ├── discovery_test.go
    └── integration/
        └── concurrent_borg_test.go # Validate concurrent borgmatic on same repo
```

## 8. Quadlet Example (Podman)

```ini
# borgmatic-manager.container
[Unit]
Description=Borgmatic Manager - discovers volumes/containers and runs borgmatic
After=podman.socket

[Container]
Image=localhost/borgmatic-manager:latest
Volume=borgmatic-manager-config.volume:/config:Z
Volume=borgmatic-generated-config.volume:/config/generated:Z
Volume=borg-repo-cache.volume:/root/.cache/borg:Z
Volume=borg-ssh.volume:/root/.ssh:ro,Z
Volume=%t/podman/podman.sock:/run/podman/podman.sock:ro
Environment=CONTAINER_SOCKET=unix:///run/podman/podman.sock
Environment=BORGMATIC_IMAGE=ghcr.io/borgmatic-collective/borgmatic:latest

[Service]
Restart=always

[Install]
WantedBy=default.target
```

## 9. Implementation Order

1. **`models.go`** — Go types: `BackupState`, `VolumeGroup`, `VolumeInfo`, `DatabaseConfig`
2. **`labels.go`** — Parse `borgmatic-manager.db.{n}.*` indexed labels from container label maps
3. **`runtime.go` + `docker.go`** — Container runtime interface and Docker-compatible API implementation
4. **`discovery.go`** — Two-phase discovery: volumes (by label) + containers (by label)
5. **`borgmatic.go` + `merge.go`** — Borgmatic config struct, YAML marshal, deep merge with group overrides
6. **`runner.go`** — Build container config (volumes, networks, image, command), create/start/wait/remove via runtime API
7. **`listener.go`** — Event stream connection, callback on volume/container events, re-discovery trigger
8. **`main.go`** — Wire it all: load config → discover → generate configs → start ticker → start event listener → block on signal
9. **`Dockerfile`** — Multi-stage build: Go build stage → minimal runtime image
10. **`manager.yaml`** — Default manager configuration
11. **Deploy examples** — Quadlet files, docker-compose.yaml
12. **Tests** — Unit tests for label parsing, config generation, merge logic; integration test for concurrent borgmatic on same borg 2.x repo

## 10. Future Work (Out of Scope for v1)

- **CLI tool**: Inspect manager state, trigger ad-hoc backups, assist with restores
- **Go template support**: Allow users to customize borgmatic config generation with Go templates
- **HTTP/socket API**: Status endpoint, health checks, programmatic backup triggers
- **Borg 1.x compatibility**: If needed, serialize group runs when sharing a repository
