// Package runner implements the borgmatic backup runner, which creates ephemeral
// borgmatic containers per backup group, streams their logs, waits for exit,
// and removes them.
package runner

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"

	"github.com/docker/docker/api/types/mount"
	"github.com/docker/docker/pkg/stdcopy"
	"github.com/lugoues/borgmatic-manager/internal/config"
	"github.com/lugoues/borgmatic-manager/internal/models"
	"github.com/lugoues/borgmatic-manager/internal/runtime"
)

const defaultBorgmaticImage = "ghcr.io/borgmatic-collective/borgmatic:latest"

// Runner creates and manages ephemeral borgmatic containers for backup groups.
type Runner struct {
	rt         runtime.ContainerRuntime
	logger     *slog.Logger
	configDir  string
	mutexes    map[string]*sync.Mutex
	mutexesMu  sync.Mutex
}

// NewRunner creates a new Runner with the given container runtime, logger,
// and directory where generated borgmatic config files are stored.
func NewRunner(rt runtime.ContainerRuntime, logger *slog.Logger, configDir string) *Runner {
	return &Runner{
		rt:        rt,
		logger:    logger,
		configDir: configDir,
		mutexes:   make(map[string]*sync.Mutex),
	}
}

// getMutex returns the per-group mutex, creating it lazily if needed.
func (r *Runner) getMutex(groupName string) *sync.Mutex {
	r.mutexesMu.Lock()
	defer r.mutexesMu.Unlock()

	mu, ok := r.mutexes[groupName]
	if !ok {
		mu = &sync.Mutex{}
		r.mutexes[groupName] = mu
	}
	return mu
}

// TryRunGroup attempts to run a backup for the given group. If the group's
// mutex is already held (another backup is running), it returns (false, nil)
// without blocking. Otherwise it runs the backup and returns (true, err).
func (r *Runner) TryRunGroup(ctx context.Context, groupName string, group *models.VolumeGroup, cfg *config.ManagerConfig) (bool, error) {
	mu := r.getMutex(groupName)
	if !mu.TryLock() {
		r.logger.Debug("skipping group: backup already running", "group", groupName)
		return false, nil
	}
	defer mu.Unlock()

	err := r.RunGroup(ctx, groupName, group, cfg)
	return true, err
}

// RunGroup creates an ephemeral borgmatic container for the given backup group,
// streams its logs, waits for it to exit, and removes it. This is the core
// execution lifecycle for a single backup group.
func (r *Runner) RunGroup(ctx context.Context, groupName string, group *models.VolumeGroup, cfg *config.ManagerConfig) error {
	image := resolveImage(cfg)
	mounts := r.buildMounts(groupName, group)
	networks := collectNetworks(group)

	containerCfg := runtime.ContainerConfig{
		Image:      image,
		GroupName:  groupName,
		ConfigPath: filepath.Join(r.configDir, groupName+".yaml"),
		Mounts:     mounts,
		Networks:   networks,
		Cmd:        []string{"borgmatic", "--config", "/etc/borgmatic/config.yaml", "create", "--verbosity", "1"},
	}

	r.logger.Info("creating borgmatic container", "group", groupName, "image", image)

	// Use context.Background() for container operations so borgmatic containers
	// complete independently of manager shutdown.
	containerID, err := r.rt.CreateContainer(context.Background(), containerCfg)
	if err != nil {
		return fmt.Errorf("creating container for group %s: %w", groupName, err)
	}

	// Connect additional networks (beyond the first, which is set at create time).
	for i, net := range networks {
		if i == 0 {
			continue
		}
		if err := r.rt.ContainerNetworkConnect(context.Background(), net, containerID); err != nil {
			r.logger.Warn("failed to connect network", "group", groupName, "network", net, "error", err)
		}
	}

	// Start container; clean up on failure.
	if err := r.rt.StartContainer(context.Background(), containerID); err != nil {
		r.rt.RemoveContainer(context.Background(), containerID)
		return fmt.Errorf("starting container for group %s: %w", groupName, err)
	}

	// Stream logs. ContainerLogs with Follow blocks until container exits.
	r.streamLogs(groupName, containerID)

	// Wait for exit code.
	exitCode, err := r.rt.WaitContainer(context.Background(), containerID)
	if err != nil {
		r.logger.Error("error waiting for container", "group", groupName, "error", err)
	} else {
		r.logger.Info("borgmatic finished", "group", groupName, "exit_code", exitCode)
	}

	// Always remove container, even on error.
	if rmErr := r.rt.RemoveContainer(context.Background(), containerID); rmErr != nil {
		r.logger.Warn("failed to remove container", "group", groupName, "error", rmErr)
	}

	if err != nil {
		return fmt.Errorf("waiting for container for group %s: %w", groupName, err)
	}

	return nil
}

// streamLogs reads the container's multiplexed log stream and emits each line
// via slog with group and stream attributes.
func (r *Runner) streamLogs(groupName, containerID string) {
	logReader, err := r.rt.ContainerLogs(context.Background(), containerID)
	if err != nil {
		r.logger.Warn("failed to attach logs", "group", groupName, "error", err)
		return
	}
	defer logReader.Close()

	stdoutW := &slogWriter{logger: r.logger, group: groupName, stream: "stdout"}
	stderrW := &slogWriter{logger: r.logger, group: groupName, stream: "stderr"}

	// stdcopy.StdCopy demuxes the multiplexed Docker log stream into
	// separate stdout/stderr writers. It blocks until EOF (container exit).
	stdcopy.StdCopy(stdoutW, stderrW, logReader)

	// Flush any remaining buffered content without a trailing newline.
	stdoutW.flush()
	stderrW.flush()
}

// resolveImage determines the borgmatic container image to use.
// Priority: BORGMATIC_IMAGE env var > config file > default.
func resolveImage(cfg *config.ManagerConfig) string {
	if img := os.Getenv("BORGMATIC_IMAGE"); img != "" {
		return img
	}
	if cfg != nil && cfg.Manager.BorgmaticImage != "" {
		return cfg.Manager.BorgmaticImage
	}
	return defaultBorgmaticImage
}

// buildMounts constructs the Docker mount slice for a borgmatic container.
func (r *Runner) buildMounts(groupName string, group *models.VolumeGroup) []mount.Mount {
	mounts := []mount.Mount{
		// Borgmatic config file (generated, read-only bind mount)
		{
			Type:     mount.TypeBind,
			Source:   filepath.Join(r.configDir, groupName+".yaml"),
			Target:   "/etc/borgmatic/config.yaml",
			ReadOnly: true,
		},
		// Borg cache volume (read-write for cache data)
		{
			Type:     mount.TypeVolume,
			Source:   "borgmatic-cache",
			Target:   "/root/.cache/borg",
			ReadOnly: false,
		},
		// SSH keys (read-only bind mount for borg remote access)
		{
			Type:     mount.TypeBind,
			Source:   "/root/.ssh",
			Target:   "/root/.ssh",
			ReadOnly: true,
		},
	}

	// Source volumes to back up (read-only)
	for _, vol := range group.Volumes {
		mounts = append(mounts, mount.Mount{
			Type:     mount.TypeVolume,
			Source:   vol.Name,
			Target:   vol.MountPath,
			ReadOnly: true,
		})
	}

	return mounts
}

// collectNetworks returns unique network names from the group's databases.
func collectNetworks(group *models.VolumeGroup) []string {
	seen := make(map[string]struct{})
	var networks []string

	for _, db := range group.Databases {
		if db.Network == "" {
			continue
		}
		if _, exists := seen[db.Network]; !exists {
			seen[db.Network] = struct{}{}
			networks = append(networks, db.Network)
		}
	}

	return networks
}

// slogWriter implements io.Writer, emitting each complete line as a structured
// slog entry with "group" and "stream" attributes.
type slogWriter struct {
	logger *slog.Logger
	group  string
	stream string
	buf    []byte
}

func (w *slogWriter) Write(p []byte) (n int, err error) {
	w.buf = append(w.buf, p...)
	for {
		idx := bytes.IndexByte(w.buf, '\n')
		if idx < 0 {
			break
		}
		line := string(w.buf[:idx])
		w.buf = w.buf[idx+1:]
		if line != "" {
			w.logger.Info(line, "group", w.group, "stream", w.stream)
		}
	}
	return len(p), nil
}

// flush emits any remaining buffered content as a final log line.
func (w *slogWriter) flush() {
	if len(w.buf) > 0 {
		line := string(w.buf)
		w.buf = nil
		if line != "" {
			w.logger.Info(line, "group", w.group, "stream", w.stream)
		}
	}
}
