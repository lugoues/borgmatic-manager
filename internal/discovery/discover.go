package discovery

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/lugoues/borgmatic-manager/internal/models"
	"github.com/lugoues/borgmatic-manager/internal/runtime"
)

// Test seams for host filesystem probes.
var (
	isMountPoint = defaultIsMountPoint
	canReadDir   = defaultCanReadDir
)

// Discover queries the container runtime for volumes and containers, then
// populates a BackupState using the label parser. Listings are unfiltered and
// label matching happens here, so misconfigured ("near-miss") labels produce
// warnings instead of silence, and unlabeled volumes can be referenced by
// sqlite database labels.
func Discover(ctx context.Context, rt runtime.ContainerRuntime, logger *slog.Logger) (*models.BackupState, error) {
	state := models.NewBackupState()

	volumes, err := rt.ListVolumes(ctx)
	if err != nil {
		return nil, fmt.Errorf("listing volumes: %w", err)
	}

	// Index all volumes by name for sqlite path resolution, including ones
	// not labeled for backup.
	volumesByName := make(map[string]runtime.VolumeInfo, len(volumes))
	for _, v := range volumes {
		volumesByName[v.Name] = v
	}

	for _, v := range volumes {
		if !IsBackupEnabled(v.Labels) {
			if HasManagerLabels(v.Labels) {
				logger.Warn("volume has borgmatic-manager labels but is not enabled: set borgmatic-manager.backup=\"true\"",
					"volume", v.Name, "backup_label", v.Labels[labelBackup])
			}
			continue
		}

		group := GetGroup(v.Labels)
		if group == "" {
			logger.Warn("volume has backup=true but no group label", "volume", v.Name)
			continue
		}

		if skip, reason := shouldSkipVolume(v); skip {
			logger.Warn("skipping volume: "+reason, "volume", v.Name, "group", group, "driver", v.Driver)
			continue
		}

		state.AddVolume(group, models.VolumeInfo{
			Name:     v.Name,
			HostPath: v.Mountpoint,
		})
	}

	// Discover containers with database labels.
	containers, err := rt.ListContainers(ctx)
	if err != nil {
		return nil, fmt.Errorf("listing containers: %w", err)
	}

	for _, c := range containers {
		group := GetGroup(c.Labels)
		if group == "" {
			if HasManagerLabels(c.Labels) {
				logger.Warn("container has borgmatic-manager labels but no group label", "container", c.Name)
			}
			continue
		}
		dbs := ParseDatabaseLabels(c.Labels, logger)
		dbs = finalizeDatabases(dbs, c, volumesByName, logger)
		if len(dbs) > 0 {
			state.AddDatabases(group, dbs)
		}
	}

	totalVols := countVolumes(state)
	totalDBs := countDatabases(state)

	if len(state.Groups) == 0 {
		logger.Warn("no backup groups discovered; check volume/container labels")
	}

	logger.Info("discovery complete",
		"groups", len(state.Groups),
		"volumes", totalVols,
		"databases", totalDBs,
	)

	return state, nil
}

// shouldSkipVolume reports whether a volume cannot be backed up from the host:
// non-local driver, lazily-mounted and empty, or unreadable mountpoint.
func shouldSkipVolume(v runtime.VolumeInfo) (bool, string) {
	if v.Driver != "local" {
		return true, fmt.Sprintf("driver %q has no readable host path; only the local driver is supported", v.Driver)
	}
	if len(v.Options) > 0 && !isMountPoint(v.Mountpoint) {
		return true, "volume has mount options but is not currently mounted; its data directory is empty until a container uses it"
	}
	if !canReadDir(v.Mountpoint) {
		return true, "mountpoint is not readable by the manager (rootless subuid ownership? see 'podman unshare')"
	}
	return false, ""
}

// finalizeDatabases attaches the source container to each parsed database
// config and resolves sqlite volume-relative paths to absolute host paths.
// Entries referencing unknown volumes are warned about and dropped.
func finalizeDatabases(dbs []models.DatabaseConfig, c runtime.ContainerInfo, volumesByName map[string]runtime.VolumeInfo, logger *slog.Logger) []models.DatabaseConfig {
	result := dbs[:0]
	for _, db := range dbs {
		db.Container = c.Name
		db.NetworkMode = c.NetworkMode

		if db.Type == "sqlite" {
			vol, ok := volumesByName[db.Volume]
			if !ok {
				logger.Warn("sqlite database references unknown volume, skipping",
					"container", c.Name, "database", db.Name, "volume", db.Volume)
				continue
			}
			db.Path = filepath.Join(vol.Mountpoint, db.Path)
		}

		result = append(result, db)
	}
	if len(result) == 0 {
		return nil
	}
	return result
}

// defaultIsMountPoint checks /proc/self/mountinfo.
func defaultIsMountPoint(path string) bool {
	f, err := os.Open("/proc/self/mountinfo")
	if err != nil {
		return false
	}
	defer f.Close()
	return mountPoints(f)[filepath.Clean(path)]
}

// mountPoints parses mountinfo; the mount point is field 5, octal escapes decoded.
func mountPoints(r io.Reader) map[string]bool {
	points := make(map[string]bool)
	scanner := bufio.NewScanner(r)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) < 5 {
			continue
		}
		points[unescapeMountPath(fields[4])] = true
	}
	return points
}

// unescapeMountPath decodes mountinfo's octal escapes (\040 space, \134 backslash).
func unescapeMountPath(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+3 < len(s) {
			if n, err := parseOctal(s[i+1 : i+4]); err == nil {
				b.WriteByte(n)
				i += 3
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

func parseOctal(s string) (byte, error) {
	var n int
	for _, c := range s {
		if c < '0' || c > '7' {
			return 0, fmt.Errorf("not octal: %q", s)
		}
		n = n*8 + int(c-'0')
	}
	return byte(n), nil
}

func defaultCanReadDir(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	f.Close()
	return true
}

func countVolumes(state *models.BackupState) int {
	total := 0
	for _, g := range state.Groups {
		total += len(g.Volumes)
	}
	return total
}

func countDatabases(state *models.BackupState) int {
	total := 0
	for _, g := range state.Groups {
		total += len(g.Databases)
	}
	return total
}
