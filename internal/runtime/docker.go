package runtime

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
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

// NewDockerRuntime connects to $CONTAINER_SOCKET, or the first existing
// well-known docker/podman socket.
func NewDockerRuntime() (*DockerRuntime, error) {
	socketPath, err := resolveSocketPath()
	if err != nil {
		return nil, err
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

// SocketPath returns the socket this client talks to (for logs and errors).
func (d *DockerRuntime) SocketPath() string {
	return d.socketPath
}

// resolveSocketPath: an explicit $CONTAINER_SOCKET always wins even if absent
// (the daemon may not be up yet); otherwise well-known paths are probed.
func resolveSocketPath() (string, error) {
	if p := os.Getenv("CONTAINER_SOCKET"); p != "" {
		// Tolerate a DOCKER_HOST-style value; unix:// is prepended later.
		return strings.TrimPrefix(p, "unix://"), nil
	}

	candidates := []string{
		"/var/run/docker.sock",
		"/run/podman/podman.sock",
	}
	if xdg := os.Getenv("XDG_RUNTIME_DIR"); xdg != "" {
		candidates = append(candidates, filepath.Join(xdg, "podman", "podman.sock"))
	}

	if found := firstSocket(candidates); found != "" {
		return found, nil
	}
	return "", fmt.Errorf("no container runtime socket found (checked %s); start docker, enable podman's API socket ('systemctl enable --now podman.socket'), or set CONTAINER_SOCKET",
		strings.Join(candidates, ", "))
}

// firstSocket returns the first candidate that exists and is a unix socket.
func firstSocket(candidates []string) string {
	for _, c := range candidates {
		if info, err := os.Stat(c); err == nil && info.Mode()&os.ModeSocket != 0 { // #nosec G703 -- probing well-known runtime socket paths

			return c
		}
	}
	return ""
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

// RemoveContainersByLabel force-removes containers (and anonymous volumes)
// carrying key=value; a leaked helper otherwise pins its image's declared VOLUME.
func (d *DockerRuntime) RemoveContainersByLabel(ctx context.Context, key, value string) ([]string, error) {
	list, err := d.client.ContainerList(ctx, container.ListOptions{
		All:     true,
		Filters: filters.NewArgs(filters.Arg("label", key+"="+value)),
	})
	if err != nil {
		return nil, fmt.Errorf("listing containers by label %s=%s: %w", key, value, err)
	}

	var removed []string
	for _, c := range list {
		if err := d.client.ContainerRemove(ctx, c.ID, container.RemoveOptions{Force: true, RemoveVolumes: true}); err != nil {
			return removed, fmt.Errorf("removing container %s: %w", c.ID[:12], err)
		}
		name := c.ID[:12]
		if len(c.Names) > 0 {
			name = strings.TrimPrefix(c.Names[0], "/")
		}
		removed = append(removed, name)
	}
	return removed, nil
}

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
