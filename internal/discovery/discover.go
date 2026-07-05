package discovery

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/lugoues/borgmatic-manager/internal/models"
	"github.com/lugoues/borgmatic-manager/internal/runtime"
)

// Test seams for host filesystem probes.
var (
	isMountPoint = defaultIsMountPoint
	canReadDir   = defaultCanReadDir
)

// Discover queries the runtime and builds a BackupState from container labels.
// Volume labels are no longer consulted: immutable after creation, they were a trap.
func Discover(ctx context.Context, rt runtime.ContainerRuntime, logger *slog.Logger) (*models.BackupState, error) {
	state := models.NewBackupState()

	volumes, err := rt.ListVolumes(ctx)
	if err != nil {
		return nil, fmt.Errorf("listing volumes: %w", err)
	}

	// Index all volumes for skip checks and sqlite path resolution.
	volumesByName := make(map[string]runtime.VolumeInfo, len(volumes))
	for _, v := range volumes {
		volumesByName[v.Name] = v
		if HasManagerLabels(v.Labels) {
			logger.Warn("volume labels are no longer supported: label the container instead (borgmatic-manager.backup + .group on the service)",
				"volume", v.Name)
		}
	}

	containers, err := rt.ListContainers(ctx)
	if err != nil {
		return nil, fmt.Errorf("listing containers: %w", err)
	}

	// Deterministic processing order for stable dedupe and config merging.
	sort.Slice(containers, func(i, j int) bool { return containers[i].Name < containers[j].Name })

	// Dedupe per group: two containers sharing a volume must not back it up twice.
	seenVolumes := make(map[string]map[string]bool)

	for _, c := range containers {
		intent, ok := containerIntentFor(c, logger)
		if !ok {
			continue
		}

		if intent.backup {
			discoverContainerVolumes(state, c, intent, volumesByName, seenVolumes, logger)
		}

		dbs := finalizeDatabases(intent.databases, c, volumesByName, logger)
		if len(dbs) > 0 {
			state.AddDatabases(intent.group, dbs)
		}

		if intent.config != nil {
			state.AddLabelConfig(intent.group, intent.config)
		}

		if !intent.backup && len(dbs) == 0 && intent.config == nil {
			logger.Warn("container has a group but no volume backup, databases, or config; it contributes nothing",
				"container", c.Name, "group", intent.group)
		}
	}

	totalVols := countVolumes(state)
	totalDBs := countDatabases(state)

	if len(state.Groups) == 0 {
		logger.Warn("no backup groups discovered; check container labels")
	}

	logger.Info("discovery complete",
		"groups", len(state.Groups),
		"volumes", totalVols,
		"databases", totalDBs,
	)

	return state, nil
}

// containerIntent is a container's normalized contribution, from flat labels or a spec blob.
type containerIntent struct {
	group            string
	backup           bool
	volumesFilter    []string
	hasVolumesFilter bool
	databases        []models.DatabaseConfig
	config           map[string]interface{}
}

// containerIntentFor builds the container's intent. A spec label, when
// present, is authoritative and shadows all flat labels; otherwise the flat
// labels are parsed. ok is false when the container contributes nothing
// parseable (warnings already emitted).
func containerIntentFor(c runtime.ContainerInfo, logger *slog.Logger) (containerIntent, bool) {
	if spec, present := ParseSpecLabel(c.Labels, c.Name, logger); present {
		if spec == nil {
			return containerIntent{}, false
		}
		warnIgnoredFlatLabels(c.Labels, c.Name, logger)
		intent := containerIntent{
			group:     spec.Group,
			backup:    spec.Backup,
			databases: spec.databases(c.Name, logger),
			config:    spec.Config,
		}
		if spec.Volumes != nil {
			intent.volumesFilter = *spec.Volumes
			intent.hasVolumesFilter = true
		}
		return intent, true
	}

	group := GetGroup(c.Labels)
	if group == "" {
		if HasManagerLabels(c.Labels) {
			logger.Warn("container has borgmatic-manager labels but no group label", "container", c.Name)
		}
		return containerIntent{}, false
	}

	if !IsBackupEnabled(c.Labels) && c.Labels[labelBackup] != "" {
		logger.Warn("container backup label is not \"true\"; its volumes will not be backed up",
			"container", c.Name, "backup_label", c.Labels[labelBackup])
	}

	intent := containerIntent{
		group:     group,
		backup:    IsBackupEnabled(c.Labels),
		databases: ParseDatabaseLabels(c.Labels, logger),
		config:    ParseConfigLabels(c.Labels, logger),
	}
	intent.volumesFilter, intent.hasVolumesFilter = VolumesFilter(c.Labels)
	return intent, true
}

var anonymousVolumeName = regexp.MustCompile(`^[0-9a-f]{64}$`)

// discoverContainerVolumes adds the container's named volumes to its group.
// Anonymous volumes are excluded unless the filter names them (usually caches, not data).
func discoverContainerVolumes(state *models.BackupState, c runtime.ContainerInfo, intent containerIntent, volumesByName map[string]runtime.VolumeInfo, seenVolumes map[string]map[string]bool, logger *slog.Logger) {
	group := intent.group
	filter, hasFilter := intent.volumesFilter, intent.hasVolumesFilter
	matched := make(map[string]bool, len(filter))

	if seenVolumes[group] == nil {
		seenVolumes[group] = make(map[string]bool)
	}

	for _, m := range c.Mounts {
		included := true
		if hasFilter {
			included = false
			for _, f := range filter {
				if f == m.Name || f == m.Destination {
					included = true
					matched[f] = true
					break
				}
			}
		} else if anonymousVolumeName.MatchString(m.Name) {
			continue
		}
		if !included || seenVolumes[group][m.Name] {
			continue
		}

		vol, ok := volumesByName[m.Name]
		if !ok {
			logger.Warn("container mount references a volume the runtime did not list; skipping",
				"container", c.Name, "volume", m.Name)
			continue
		}
		if skip, reason := shouldSkipVolume(vol); skip {
			logger.Warn("skipping volume: "+reason, "volume", m.Name, "group", group, "container", c.Name, "driver", vol.Driver)
			continue
		}

		hostPath := vol.Mountpoint
		if hostPath == "" {
			hostPath = m.Source
		}

		seenVolumes[group][m.Name] = true
		state.AddVolume(group, models.VolumeInfo{
			Name:     m.Name,
			HostPath: hostPath,
		})
	}

	if !hasFilter && len(c.Mounts) == 0 {
		logger.Warn("container has backup=true but no named volumes attached", "container", c.Name, "group", group)
	}
	for _, f := range filter {
		if !matched[f] {
			logger.Warn("volumes filter entry matched no attached volume", "container", c.Name, "entry", f)
		}
	}
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
		db.Image = c.Image

		if db.Type == dbTypeSQLite {
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
	defer func() { _ = f.Close() }()
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
	f, err := os.Open(path) // #nosec G304 -- readability probe of runtime-reported mountpoints
	if err != nil {
		return false
	}
	_ = f.Close()
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
