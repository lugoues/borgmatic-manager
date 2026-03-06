// Package runtime provides the container runtime abstraction for borgmatic-manager.
// It defines the ContainerRuntime interface that works with both Docker and Podman
// through the Docker-compatible API, enabling volume discovery, container inspection,
// and (in future phases) backup container orchestration.
package runtime

import "context"

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
	// TODO(phase-3): implement runner methods
	CreateContainer(ctx context.Context, config ContainerConfig) (string, error)

	// StartContainer starts a previously created container.
	// TODO(phase-3): implement runner methods
	StartContainer(ctx context.Context, id string) error

	// WaitContainer blocks until a container exits and returns its exit code.
	// TODO(phase-3): implement runner methods
	WaitContainer(ctx context.Context, id string) (int64, error)

	// RemoveContainer removes a container by ID.
	// TODO(phase-3): implement runner methods
	RemoveContainer(ctx context.Context, id string) error
}

// VolumeInfo describes a container volume discovered by the runtime.
type VolumeInfo struct {
	Name   string
	Labels map[string]string
}

// ContainerInfo describes a container discovered by the runtime.
type ContainerInfo struct {
	ID     string
	Name   string
	Labels map[string]string
}

// Event represents a container runtime event (e.g., container start/stop).
// TODO(phase-3): expand with full event metadata
type Event struct {
	Type   string
	Action string
	Actor  string
}

// ContainerConfig holds configuration for creating a backup container.
// Populated in Phase 3
type ContainerConfig struct{}
