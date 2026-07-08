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

	// RemoveContainersByLabel force-removes containers (and their anonymous
	// volumes) carrying key=value; used to reap orphaned dump helpers.
	RemoveContainersByLabel(ctx context.Context, key, value string) ([]string, error)
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
	// Image is the container's image reference. Helper-container database
	// dumps run this image so client and server versions always match.
	Image  string
	Labels map[string]string
	// Mounts lists named volume mounts (bind/tmpfs excluded).
	Mounts []VolumeMount
}

// VolumeMount is a named volume attached to a container.
type VolumeMount struct {
	Name string
	// Source is the volume's data path on the host.
	Source string
	// Destination is the mount path inside the container.
	Destination string
}

// Event is a runtime event (container/volume create or removal).
type Event struct {
	Type   string
	Action string
	Actor  string
}
