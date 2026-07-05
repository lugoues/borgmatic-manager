// Package runtime provides the container runtime abstraction for borgmatic-manager.
// It defines the ContainerRuntime interface that works with both Docker and Podman
// through the Docker-compatible API, enabling volume discovery, container inspection,
// and (in future phases) backup container orchestration.
package runtime

import (
	"context"
	"io"

	"github.com/docker/docker/api/types/mount"
)

// ContainerRuntime defines the interface for interacting with a container runtime
// (Docker or Podman). Discovery operations (ListVolumes, ListContainers) are used
// in Phase 2; runner operations are placeholders for Phase 3.
type ContainerRuntime interface {
	// ListVolumes returns volumes matching borgmatic-manager backup labels.
	ListVolumes(ctx context.Context) ([]VolumeInfo, error)

	// ListContainers returns containers matching borgmatic-manager group labels.
	ListContainers(ctx context.Context) ([]ContainerInfo, error)

	// TODO(phase-3): implement event-driven discovery
	EventStream(ctx context.Context) (<-chan Event, <-chan error)

	// CreateContainer creates a new container and returns its ID.
	CreateContainer(ctx context.Context, config ContainerConfig) (string, error)

	// StartContainer starts a previously created container.
	StartContainer(ctx context.Context, id string) error

	// WaitContainer blocks until a container exits and returns its exit code.
	WaitContainer(ctx context.Context, id string) (int64, error)

	// RemoveContainer removes a container by ID.
	RemoveContainer(ctx context.Context, id string) error

	// ContainerNetworkConnect connects a container to a network.
	ContainerNetworkConnect(ctx context.Context, networkID, containerID string) error

	// ContainerLogs returns a reader for the container's stdout/stderr log stream.
	ContainerLogs(ctx context.Context, id string) (io.ReadCloser, error)
}

// VolumeInfo describes a container volume discovered by the runtime.
type VolumeInfo struct {
	Name string
	// Mountpoint is the host data path (e.g. /var/lib/docker/volumes/<name>/_data).
	Mountpoint string
	// Driver is the volume driver ("local" for plain directories).
	Driver string
	// Options are driver creation options; non-empty on local implies lazy mounting (NFS/CIFS).
	Options map[string]string
	Labels  map[string]string
}

// ContainerInfo describes a container discovered by the runtime.
type ContainerInfo struct {
	ID   string
	Name string
	// NetworkMode is the container's network mode (e.g. "bridge", "host").
	NetworkMode string
	Labels      map[string]string
}

// Event represents a container runtime event (e.g., container start/stop).
// TODO(phase-3): expand with full event metadata
type Event struct {
	Type   string
	Action string
	Actor  string
}

// ContainerConfig holds configuration for creating a backup container.
type ContainerConfig struct {
	// Image is the borgmatic container image (e.g., "ghcr.io/borgmatic-collective/borgmatic:latest").
	Image string
	// GroupName is the backup group name, used for container naming and log prefixes.
	GroupName string
	// ConfigPath is the host path to the generated borgmatic config YAML.
	ConfigPath string
	// Mounts defines all volume mounts for the container.
	Mounts []mount.Mount
	// Networks lists network names to attach (from DB labels).
	Networks []string
	// Cmd is the borgmatic command and arguments.
	Cmd []string
}
