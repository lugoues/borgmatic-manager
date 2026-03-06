package runtime

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/volume"
	dockerclient "github.com/docker/docker/client"
)

// DockerRuntime implements ContainerRuntime using the Docker SDK.
// It works with both Docker and Podman via the Docker-compatible API socket.
type DockerRuntime struct {
	client *dockerclient.Client
}

// NewDockerRuntime creates a new DockerRuntime connected to the container socket.
// The socket path is read from the CONTAINER_SOCKET environment variable,
// defaulting to /var/run/docker.sock if unset. This works identically for
// Docker and Podman sockets.
func NewDockerRuntime() (*DockerRuntime, error) {
	socketPath := os.Getenv("CONTAINER_SOCKET")
	if socketPath == "" {
		socketPath = "/var/run/docker.sock"
	}

	cli, err := dockerclient.NewClientWithOpts(
		dockerclient.WithHost("unix://"+socketPath),
		dockerclient.WithAPIVersionNegotiation(),
	)
	if err != nil {
		return nil, fmt.Errorf("creating docker client: %w", err)
	}

	return &DockerRuntime{client: cli}, nil
}

// ListVolumes returns volumes labeled with borgmatic-manager.backup=true.
func (d *DockerRuntime) ListVolumes(ctx context.Context) ([]VolumeInfo, error) {
	resp, err := d.client.VolumeList(ctx, volume.ListOptions{
		Filters: filters.NewArgs(
			filters.Arg("label", "borgmatic-manager.backup=true"),
		),
	})
	if err != nil {
		return nil, fmt.Errorf("listing volumes: %w", err)
	}

	if len(resp.Volumes) == 0 {
		return nil, nil
	}

	vols := make([]VolumeInfo, 0, len(resp.Volumes))
	for _, v := range resp.Volumes {
		vols = append(vols, VolumeInfo{
			Name:   v.Name,
			Labels: v.Labels,
		})
	}
	return vols, nil
}

// ListContainers returns containers labeled with borgmatic-manager.group.
func (d *DockerRuntime) ListContainers(ctx context.Context) ([]ContainerInfo, error) {
	containers, err := d.client.ContainerList(ctx, container.ListOptions{
		All: true,
		Filters: filters.NewArgs(
			filters.Arg("label", "borgmatic-manager.group"),
		),
	})
	if err != nil {
		return nil, fmt.Errorf("listing containers: %w", err)
	}

	if len(containers) == 0 {
		return nil, nil
	}

	infos := make([]ContainerInfo, 0, len(containers))
	for _, c := range containers {
		name := ""
		if len(c.Names) > 0 {
			name = strings.TrimPrefix(c.Names[0], "/")
		}
		infos = append(infos, ContainerInfo{
			ID:     c.ID,
			Name:   name,
			Labels: c.Labels,
		})
	}
	return infos, nil
}

// EventStream returns channels for container runtime events.
// TODO(phase-3): implement event-driven discovery
func (d *DockerRuntime) EventStream(_ context.Context) (<-chan Event, <-chan error) {
	errCh := make(chan error, 1)
	errCh <- fmt.Errorf("not implemented: available in Phase 3")
	close(errCh)
	return nil, errCh
}

// CreateContainer creates a new container from the given configuration.
// TODO(phase-3): implement runner methods
func (d *DockerRuntime) CreateContainer(_ context.Context, _ ContainerConfig) (string, error) {
	return "", fmt.Errorf("not implemented: available in Phase 3")
}

// StartContainer starts a previously created container.
// TODO(phase-3): implement runner methods
func (d *DockerRuntime) StartContainer(_ context.Context, _ string) error {
	return fmt.Errorf("not implemented: available in Phase 3")
}

// WaitContainer blocks until the container exits and returns its exit code.
// TODO(phase-3): implement runner methods
func (d *DockerRuntime) WaitContainer(_ context.Context, _ string) (int64, error) {
	return 0, fmt.Errorf("not implemented: available in Phase 3")
}

// RemoveContainer removes a container by ID.
// TODO(phase-3): implement runner methods
func (d *DockerRuntime) RemoveContainer(_ context.Context, _ string) error {
	return fmt.Errorf("not implemented: available in Phase 3")
}

var _ ContainerRuntime = (*DockerRuntime)(nil)
