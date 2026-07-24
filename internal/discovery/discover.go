package discovery

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/lugoues/borgmatic-manager/internal/models"
	"github.com/lugoues/borgmatic-manager/internal/runtime"
)

// Test seams for host filesystem probes.
var (
	isMountPoint = defaultIsMountPoint
	canReadDir   = defaultCanReadDir
	dirIsEmpty   = defaultDirIsEmpty
)

// Discover queries the runtime and builds a BackupState from container labels.
// Volume labels are no longer consulted: immutable after creation, they were a trap.
func Discover(ctx context.Context, rt runtime.ContainerRuntime, logger *slog.Logger) (*models.BackupState, error) {
	state := models.NewBackupState()

	volumes, err := rt.ListVolumes(ctx)
	if err != nil {
		return nil, err // already wrapped with context by the runtime
	}

	// Index all volumes for skip checks and sqlite path resolution.
	volumesByName := make(map[string]runtime.VolumeInfo, len(volumes))
	for _, v := range volumes {
		volumesByName[v.Name] = v
		if HasManagerLabels(v.Labels) {
			logger.Warn("volume labels are no longer supported: label the container instead (borgmatic-manager.enable + .group on the service)",
				"volume", v.Name)
		}
	}

	containers, err := rt.ListContainers(ctx)
	if err != nil {
		return nil, err // already wrapped with context by the runtime
	}

	// Deterministic processing order for stable dedupe and config merging.
	sort.Slice(containers, func(i, j int) bool { return containers[i].Name < containers[j].Name })

	// Dedupe per group: two containers sharing a volume must not back it up twice.
	seenVolumes := make(map[string]map[string]bool)

	// periodSetBy tracks which container set each group's period, for conflict errors.
	periodSetBy := make(map[string]string)

	var specErrs []error
	for _, c := range containers {
		intent, ok, err := containerIntentFor(c, logger)
		if err != nil {
			// Collect every broken spec; the cycle fails rather than shrinking the backup set.
			specErrs = append(specErrs, err)
			continue
		}
		if !ok {
			continue
		}

		if intent.enabled {
			discoverContainerVolumes(state, c, intent, volumesByName, seenVolumes, logger)
		}

		dbs, err := finalizeDatabases(intent.databases, c, volumesByName)
		if err != nil {
			specErrs = append(specErrs, err)
			continue
		}
		if len(dbs) > 0 {
			state.AddDatabases(intent.group, dbs)
		}

		if intent.config != nil {
			state.AddLabelConfig(intent.group, intent.config)
		}

		if intent.period != 0 {
			// Conflicting overrides fail the cycle: picking one silently would
			// run the group on a period somebody didn't ask for.
			if existing := state.Groups[intent.group]; existing != nil && existing.Period != 0 && existing.Period != intent.period {
				specErrs = append(specErrs, fmt.Errorf(
					"group %s: conflicting period overrides: %s from container %s vs %s from container %s",
					intent.group, existing.Period, periodSetBy[intent.group], intent.period, c.Name))
				continue
			}
			state.SetPeriod(intent.group, intent.period)
			periodSetBy[intent.group] = c.Name
		}

		if !intent.enabled && len(dbs) == 0 && intent.config == nil && intent.period == 0 {
			logger.Warn("container has a group but no enable=true, databases, or config; it contributes nothing",
				"container", c.Name, "group", intent.group)
		}
	}

	if len(specErrs) > 0 {
		return nil, fmt.Errorf("refusing to run with invalid backup labels (a broken label silently shrinks the backup set): %w", errors.Join(specErrs...))
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
	enabled          bool
	volumesFilter    []string
	hasVolumesFilter bool
	databases        []models.DatabaseConfig
	config           map[string]interface{}
	// period overrides manager.period for the group; 0 means no override.
	period time.Duration
}

// containerIntentFor builds the container's intent; a spec label shadows all flat
// labels. ok=false means nothing parseable; a structurally invalid spec is an error.
func containerIntentFor(c runtime.ContainerInfo, logger *slog.Logger) (containerIntent, bool, error) {
	// Skip manager-owned dump helpers: never backup sources, they would warn every cycle.
	if c.Labels[models.HelperRunLabel] != "" {
		logger.Debug("skipping manager-owned helper container", "container", c.Name)
		return containerIntent{}, false, nil
	}

	if spec, present, err := ParseSpecLabel(c.Labels, c.Name); present || err != nil {
		if err != nil {
			return containerIntent{}, false, err
		}
		if err := validateGroupName(spec.Group, c.Name); err != nil {
			return containerIntent{}, false, err
		}
		warnIgnoredFlatLabels(c.Labels, c.Name, logger)
		dbs, err := spec.databases(c.Name, logger)
		if err != nil {
			return containerIntent{}, false, err
		}
		intent := containerIntent{
			group:     spec.Group,
			enabled:   spec.Enable,
			databases: dbs,
			config:    spec.Config,
		}
		if spec.Period != "" {
			period, err := parsePeriodValue(spec.Period, c.Name)
			if err != nil {
				return containerIntent{}, false, err
			}
			intent.period = period
		}
		if len(spec.Volumes) > 0 {
			intent.volumesFilter = spec.Volumes
			intent.hasVolumesFilter = true
		}
		return intent, true, nil
	}

	group := GetGroup(c.Labels)
	if group == "" {
		if HasManagerLabels(c.Labels) {
			logger.Warn("container has borgmatic-manager labels but no group label", "container", c.Name)
		}
		return containerIntent{}, false, nil
	}
	if err := validateGroupName(group, c.Name); err != nil {
		return containerIntent{}, false, err
	}

	if c.Labels[labelEnableRenamed] != "" {
		logger.Warn("the borgmatic-manager.backup label was renamed: use borgmatic-manager.enable",
			"container", c.Name)
	}
	if !IsEnabled(c.Labels) && c.Labels[labelEnable] != "" {
		logger.Warn("container enable label is not \"true\"; its volumes will not be backed up",
			"container", c.Name, "enable_label", c.Labels[labelEnable])
	}

	dbs, err := ParseDatabaseLabels(c.Labels, logger)
	if err != nil {
		return containerIntent{}, false, fmt.Errorf("container %s: %w", c.Name, err)
	}
	period, err := ParsePeriodLabel(c.Labels, c.Name)
	if err != nil {
		return containerIntent{}, false, err
	}
	intent := containerIntent{
		group:     group,
		enabled:   IsEnabled(c.Labels),
		databases: dbs,
		config:    ParseConfigLabels(c.Labels, logger),
		period:    period,
	}
	intent.volumesFilter, intent.hasVolumesFilter = VolumesFilter(c.Labels)
	return intent, true, nil
}

// validateGroupName rejects names unsafe as a config filename: the label is
// attacker-influenced and flows into a root-owned file path.
func validateGroupName(group, containerName string) error {
	if !validGroupName.MatchString(group) {
		return fmt.Errorf("container %s: invalid group name %q: must match %s (it becomes a config filename)",
			containerName, group, validGroupName.String())
	}
	return nil
}

var anonymousVolumeName = regexp.MustCompile(`^[0-9a-f]{64}$`)

// validGroupName: the group becomes a root-owned config filename, so no path
// separators or leading dot may reach filepath.Join.
var validGroupName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

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
		logger.Warn("container has enable=true but no named volumes attached", "container", c.Name, "group", group)
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
	// Lazily-mounted volumes (NFS/CIFS via the local driver) have an empty
	// Lazily-mounted volumes (NFS/CIFS) are empty dirs while unused; options alone
	// don't prove lazy mounting (podman), so the empty unmounted dir is the signal.
	if len(v.Options) > 0 && !isMountPoint(v.Mountpoint) && dirIsEmpty(v.Mountpoint) {
		return true, fmt.Sprintf("volume has mount options (%v), is not currently mounted, and its data directory is empty", v.Options)
	}
	if !canReadDir(v.Mountpoint) {
		return true, "mountpoint is not readable by the manager (rootless subuid ownership? see 'podman unshare')"
	}
	return false, ""
}

// finalizeDatabases attaches the source container and resolves sqlite paths.
// Unknown volumes and escaping paths are errors: attacker-influenced input read by root.
func finalizeDatabases(dbs []models.DatabaseConfig, c runtime.ContainerInfo, volumesByName map[string]runtime.VolumeInfo) ([]models.DatabaseConfig, error) {
	result := dbs[:0]
	for _, db := range dbs {
		db.Container = c.Name
		db.Image = c.Image

		if db.Type == dbTypeSQLite {
			vol, ok := volumesByName[db.Volume]
			if !ok {
				return nil, fmt.Errorf("container %s: sqlite database %q references unknown volume %q", c.Name, db.Name, db.Volume)
			}
			joined := filepath.Join(vol.Mountpoint, db.Path)
			rel, err := filepath.Rel(vol.Mountpoint, joined)
			if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
				return nil, fmt.Errorf("container %s: sqlite database %q path %q escapes volume %q", c.Name, db.Name, db.Path, db.Volume)
			}
			db.Path = joined
		}

		result = append(result, db)
	}
	if len(result) == 0 {
		return nil, nil
	}
	return result, nil
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

// defaultDirIsEmpty: unreadable or missing counts as empty (no data either way).
func defaultDirIsEmpty(path string) bool {
	entries, err := os.ReadDir(path)
	return err != nil || len(entries) == 0
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
