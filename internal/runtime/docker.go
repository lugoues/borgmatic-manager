package runtime

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/events"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/mount"
	"github.com/docker/docker/api/types/volume"
	dockerclient "github.com/docker/docker/client"
)

// DockerRuntime implements ContainerRuntime using the Docker SDK.
// It works with both Docker and Podman via the Docker-compatible API socket.
type DockerRuntime struct {
	client     *dockerclient.Client
	socketPath string
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

	return &DockerRuntime{client: cli, socketPath: socketPath}, nil
}

// Rootless reports whether the engine runs rootless. It checks the engine's
// reported security options, falling back to a socket-path heuristic when the
// info call fails. Rootless engines use userspace networking, which breaks
// container-IP database connections.
func (d *DockerRuntime) Rootless(ctx context.Context) bool {
	info, err := d.client.Info(ctx)
	if err == nil {
		for _, opt := range info.SecurityOptions {
			if strings.Contains(opt, "rootless") {
				return true
			}
		}
		return false
	}
	return strings.Contains(d.socketPath, "/run/user/")
}

// ListVolumes returns all volumes. Filtering is client-side so near-miss labels
// warn and unlabeled volumes stay referenceable (sqlite).
func (d *DockerRuntime) ListVolumes(ctx context.Context) ([]VolumeInfo, error) {
	resp, err := d.client.VolumeList(ctx, volume.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("listing volumes: %w", err)
	}

	if len(resp.Volumes) == 0 {
		return nil, nil
	}

	vols := make([]VolumeInfo, 0, len(resp.Volumes))
	for _, v := range resp.Volumes {
		vols = append(vols, VolumeInfo{
			Name:       v.Name,
			Mountpoint: v.Mountpoint,
			Driver:     v.Driver,
			Options:    v.Options,
			Labels:     v.Labels,
		})
	}
	return vols, nil
}

// ListContainers returns all containers. Label filtering happens client-side
// in discovery so near-miss labels can be warned about.
func (d *DockerRuntime) ListContainers(ctx context.Context) ([]ContainerInfo, error) {
	containers, err := d.client.ContainerList(ctx, container.ListOptions{
		All: true,
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
		var mounts []VolumeMount
		for _, m := range c.Mounts {
			if m.Type != mount.TypeVolume {
				continue
			}
			mounts = append(mounts, VolumeMount{
				Name:        m.Name,
				Source:      m.Source,
				Destination: m.Destination,
			})
		}
		infos = append(infos, ContainerInfo{
			ID:     c.ID,
			Name:   name,
			Image:  c.Image,
			Labels: c.Labels,
			Mounts: mounts,
		})
	}
	return infos, nil
}

// relevantActions trigger re-discovery. Docker emits "destroy", podman "remove";
// matched client-side because server-side action filters are not portable.
var relevantActions = map[string]bool{
	"create":  true,
	"destroy": true,
	"remove":  true,
}

// EventStream returns channels for container runtime events, filtered to
// container/volume create and removal actions. The event channel closes when
// the context is cancelled or the underlying stream ends; the error channel
// receives at most one error.
//
// The Docker SDK never closes its message channel, so the forwarding goroutine
// holds a per-connection child context and cancels it on every exit path,
// releasing the SDK's producer goroutine and preventing a leak per reconnect.
// EventStream cancels a child context on every exit path: the Docker SDK never
// closes its Events channel, so the SDK goroutine would otherwise leak per reconnect.
func (d *DockerRuntime) EventStream(ctx context.Context) (<-chan Event, <-chan error) {
	opts := events.ListOptions{
		Filters: filters.NewArgs(
			filters.Arg("type", string(events.ContainerEventType)),
			filters.Arg("type", string(events.VolumeEventType)),
		),
	}

	streamCtx, cancel := context.WithCancel(ctx)
	dockerMsgCh, dockerErrCh := d.client.Events(streamCtx, opts)

	eventCh := make(chan Event)
	errCh := make(chan error, 1)

	go func() {
		defer cancel()
		defer close(eventCh)
		for {
			select {
			case <-streamCtx.Done():
				return

			case err, ok := <-dockerErrCh:
				if ok {
					errCh <- err // buffered(1); at most one error arrives
				}
				return

			case msg := <-dockerMsgCh:
				if !relevantActions[string(msg.Action)] {
					continue
				}
				evt := Event{
					Type:   string(msg.Type),
					Action: string(msg.Action),
					Actor:  msg.Actor.ID,
				}
				select {
				case eventCh <- evt:
				case <-streamCtx.Done():
					return
				}
			}
		}
	}()

	return eventCh, errCh
}

var _ ContainerRuntime = (*DockerRuntime)(nil)
