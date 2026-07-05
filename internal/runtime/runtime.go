package runtime

import (
	"context"
)

// ContainerRuntime is the discovery-facing interface to Docker or Podman.
type ContainerRuntime interface {
	// ListVolumes returns all volumes with their host mountpoints.
	ListVolumes(ctx context.Context) ([]VolumeInfo, error)

	ListContainers(ctx context.Context) ([]ContainerInfo, error)

	EventStream(ctx context.Context) (<-chan Event, <-chan error)
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

// Event is a runtime event (container/volume create or removal).
type Event struct {
	Type   string
	Action string
	Actor  string
}
