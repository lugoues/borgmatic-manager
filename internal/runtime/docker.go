package runtime

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"

	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/events"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/network"
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

// EventStream returns channels for container runtime events filtered to
// container/volume create/remove actions. The returned channels close when
// the context is cancelled or the underlying Docker event stream ends.
// EventStream cancels a child context on every exit path: the Docker SDK never
// closes its Events channel, so the SDK goroutine would otherwise leak per reconnect.
func (d *DockerRuntime) EventStream(ctx context.Context) (<-chan Event, <-chan error) {
	opts := events.ListOptions{
		Filters: filters.NewArgs(
			filters.Arg("type", string(events.ContainerEventType)),
			filters.Arg("type", string(events.VolumeEventType)),
			filters.Arg("event", string(events.ActionCreate)),
			filters.Arg("event", string(events.ActionRemove)),
		),
	}

	dockerMsgCh, dockerErrCh := d.client.Events(ctx, opts)

	eventCh := make(chan Event)
	errCh := make(chan error, 1)

	// Forward Docker SDK messages as runtime Events.
	go func() {
		defer close(eventCh)
		for msg := range dockerMsgCh {
			eventCh <- Event{
				Type:   string(msg.Type),
				Action: string(msg.Action),
				Actor:  msg.Actor.ID,
			}
		}
	}()

	// Forward errors from the Docker SDK.
	go func() {
		defer close(errCh)
		for err := range dockerErrCh {
			errCh <- err
			return // only forward the first error
		}
	}()

	return eventCh, errCh
}

// CreateContainer creates a new container from the given configuration.
// It translates ContainerConfig into Docker SDK types, attaching the first
// network at create time. Use ContainerNetworkConnect for additional networks.
func (d *DockerRuntime) CreateContainer(ctx context.Context, cfg ContainerConfig) (string, error) {
	containerName := fmt.Sprintf("borgmatic-%s-%d", cfg.GroupName, time.Now().Unix())

	// Build network config with first network if available.
	var networkConfig *network.NetworkingConfig
	if len(cfg.Networks) > 0 {
		networkConfig = &network.NetworkingConfig{
			EndpointsConfig: map[string]*network.EndpointSettings{
				cfg.Networks[0]: {},
			},
		}
	}

	resp, err := d.client.ContainerCreate(ctx,
		&container.Config{
			Image: cfg.Image,
			Cmd:   cfg.Cmd,
		},
		&container.HostConfig{
			Mounts: cfg.Mounts,
		},
		networkConfig,
		nil,
		containerName,
	)
	if err != nil {
		return "", fmt.Errorf("creating container: %w", err)
	}

	return resp.ID, nil
}

// ContainerNetworkConnect connects a container to a network.
func (d *DockerRuntime) ContainerNetworkConnect(ctx context.Context, networkID, containerID string) error {
	if err := d.client.NetworkConnect(ctx, networkID, containerID, nil); err != nil {
		return fmt.Errorf("connecting container %s to network %s: %w", containerID, networkID, err)
	}
	return nil
}

// StartContainer starts a previously created container.
func (d *DockerRuntime) StartContainer(ctx context.Context, id string) error {
	if err := d.client.ContainerStart(ctx, id, container.StartOptions{}); err != nil {
		return fmt.Errorf("starting container %s: %w", id, err)
	}
	return nil
}

// WaitContainer blocks until the container exits and returns its exit code.
func (d *DockerRuntime) WaitContainer(ctx context.Context, id string) (int64, error) {
	waitCh, errCh := d.client.ContainerWait(ctx, id, container.WaitConditionNotRunning)
	select {
	case result := <-waitCh:
		if result.Error != nil {
			return result.StatusCode, fmt.Errorf("container wait error: %s", result.Error.Message)
		}
		return result.StatusCode, nil
	case err := <-errCh:
		return -1, fmt.Errorf("waiting for container %s: %w", id, err)
	}
}

// RemoveContainer removes a container by ID with force.
func (d *DockerRuntime) RemoveContainer(ctx context.Context, id string) error {
	if err := d.client.ContainerRemove(ctx, id, container.RemoveOptions{Force: true}); err != nil {
		return fmt.Errorf("removing container %s: %w", id, err)
	}
	return nil
}

// ContainerLogs returns a reader for the container's stdout/stderr log stream.
// The stream follows the container output until the container exits.
func (d *DockerRuntime) ContainerLogs(ctx context.Context, id string) (io.ReadCloser, error) {
	reader, err := d.client.ContainerLogs(ctx, id, container.LogsOptions{
		ShowStdout: true,
		ShowStderr: true,
		Follow:     true,
	})
	if err != nil {
		return nil, fmt.Errorf("getting container logs for %s: %w", id, err)
	}
	return reader, nil
}

var _ ContainerRuntime = (*DockerRuntime)(nil)
