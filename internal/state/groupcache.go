package state

import (
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/lugoues/borgmatic-manager/internal/lockfile"
	"github.com/lugoues/borgmatic-manager/internal/models"
)

// CachedGroup is a group's accumulated membership, persisted so a member
// survives its container being stopped or (with quadlets) removed. Members are
// unioned across cycles, never overwritten: a multi-container group whose app
// container stops must not lose the volumes that container contributed.
type CachedGroup struct {
	Volumes      []models.VolumeInfo      `json:"volumes,omitempty"`
	Databases    []models.DatabaseConfig  `json:"databases,omitempty"`
	LabelConfigs []map[string]interface{} `json:"label_configs,omitempty"`
	Period       time.Duration            `json:"period,omitempty"`
	// LastSeen is when discovery last found any of the group's containers alive.
	LastSeen time.Time `json:"last_seen"`
}

type groupCacheFile struct {
	Version int                    `json:"version"`
	Groups  map[string]CachedGroup `json:"groups"`
}

// dbKey identifies a database within a group. The container is part of the
// identity: two containers in one group can each expose a database with the
// same type and name, and conflating them would silently drop the stopped
// container's database from tracking instead of marking it offline.
func dbKey(db models.DatabaseConfig) string { return db.Type + "/" + db.Container + "/" + db.Name }

// Offline records which members are cached-but-not-live in the current cycle:
// their container is gone. Volumes stay backed up (data at rest); databases
// cannot be dumped without their container and are skipped.
type Offline struct {
	Volumes   map[string]map[string]bool // group -> volume name
	Databases map[string]map[string]bool // group -> "type/name"
}

func newOffline() *Offline {
	return &Offline{Volumes: map[string]map[string]bool{}, Databases: map[string]map[string]bool{}}
}

// VolumeOffline reports whether a group's volume is cached-only this cycle.
func (o *Offline) VolumeOffline(group, name string) bool { return o.Volumes[group][name] }

// DatabaseOffline reports whether a group's database is cached-only this cycle.
func (o *Offline) DatabaseOffline(group string, db models.DatabaseConfig) bool {
	return o.Databases[group][dbKey(db)]
}

// AnyOffline reports whether a group has at least one offline member.
func (o *Offline) AnyOffline(group string) bool {
	return len(o.Volumes[group]) > 0 || len(o.Databases[group]) > 0
}

// GroupOffline reports whether a group has no live container at all (every
// member is cached-only). Such a group is still backed up while its volume
// paths exist; the flag is informational.
func (o *Offline) GroupOffline(group string, g *models.VolumeGroup) bool {
	if len(g.Volumes)+len(g.Databases) == 0 {
		return false
	}
	for _, v := range g.Volumes {
		if !o.Volumes[group][v.Name] {
			return false
		}
	}
	for _, db := range g.Databases {
		if !o.Databases[group][dbKey(db)] {
			return false
		}
	}
	return true
}

// GroupCache persists discovered group membership as JSON. Like ScheduleStore,
// every mutation re-reads the shared file under an exclusive flock and merges,
// so the daemon and an ad-hoc command cannot erase each other's entries.
type GroupCache struct {
	path     string
	lockPath string
	logger   *slog.Logger
	// pathExists gates whether an offline volume is still real: a stopped
	// container's volume keeps its path, a deleted one does not. Seam for tests.
	pathExists func(string) bool

	mu     sync.Mutex
	groups map[string]CachedGroup
}

// LoadGroupCache reads the cache from stateDir, returning an empty cache when
// the file is missing or unreadable (the safe direction: a lost cache just
// hides offline members until they are seen again, never a bad backup).
func LoadGroupCache(stateDir string, logger *slog.Logger) *GroupCache {
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	c := &GroupCache{
		path:       filepath.Join(stateDir, "groups.json"),
		lockPath:   filepath.Join(stateDir, "groups.json.lock"),
		logger:     logger,
		pathExists: func(p string) bool { _, err := os.Stat(p); return err == nil },
		groups:     map[string]CachedGroup{},
	}
	c.reload()
	return c
}

// SetPathExists overrides the volume path-existence check (tests only).
func (c *GroupCache) SetPathExists(fn func(string) bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.pathExists = fn
}

func (c *GroupCache) reload() {
	c.mu.Lock()
	defer c.mu.Unlock()
	f, _ := c.readFile()
	c.groups = f.Groups
}

// readFile loads the cache. A missing file or corrupt content is an empty
// cache (nothing worth preserving); a transient read failure returns an error
// so Reconcile does not overwrite intact on-disk state with an empty view.
func (c *GroupCache) readFile() (groupCacheFile, error) {
	f := groupCacheFile{Version: 1, Groups: map[string]CachedGroup{}}
	data, err := os.ReadFile(c.path)
	if err != nil {
		if !os.IsNotExist(err) {
			c.logger.Warn("cannot read group cache; offline members hidden until rediscovered", "path", c.path, "error", err)
			return f, err
		}
		return f, nil
	}
	var parsed groupCacheFile
	if err := json.Unmarshal(data, &parsed); err != nil {
		c.logger.Warn("group cache is corrupt; rebuilding from discovery", "path", c.path, "error", err)
		return f, nil
	}
	if parsed.Groups != nil {
		f.Groups = parsed.Groups
	}
	return f, nil
}

// Reconcile unions live membership into the cache per member (never
// overwriting a whole group), drops cached volumes whose path is gone (deleted,
// not merely stopped), and returns the merged view plus the offline members.
// Live member data wins on a name collision. The cache is persisted under an
// exclusive flock.
func (c *GroupCache) Reconcile(live *models.BackupState, now time.Time) (*models.BackupState, *Offline) {
	c.mu.Lock()
	defer c.mu.Unlock()

	lock, err := lockfile.Exclusive(c.lockPath)
	if err != nil {
		c.logger.Warn("cannot lock group cache; using live discovery only", "error", err)
		return live, newOffline()
	}
	defer lock.Release()

	f, err := c.readFile()
	if err != nil {
		// The disk cache may be intact even though this read failed; writing a
		// live-only view now would permanently drop every offline member.
		c.logger.Warn("cannot read group cache; using live discovery only and leaving the cache untouched", "error", err)
		return live, newOffline()
	}
	off := newOffline()

	names := map[string]struct{}{}
	for name := range f.Groups {
		names[name] = struct{}{}
	}
	for name := range live.Groups {
		names[name] = struct{}{}
	}

	merged := models.NewBackupState()
	for name := range names {
		liveGroup := live.Groups[name]
		cached := f.Groups[name]

		vols, offVols := mergeVolumes(liveVolumes(liveGroup), cached.Volumes, c.pathExists)
		dbs, offDBs := mergeDatabases(liveDatabases(liveGroup), cached.Databases)

		if len(vols) == 0 && len(dbs) == 0 {
			delete(f.Groups, name) // every volume path gone and no databases: truly removed
			continue
		}

		// Config (label fragments, period) is deliberately a live snapshot, not
		// a union like membership: labels describe intent, and intent must
		// follow the containers that are actually present. Unioning would keep
		// a removed container's repository or hook config alive forever (the
		// cache has no expiry). The trade-off: while a config-contributing
		// container is stopped and a sibling stays live, the group runs on the
		// sibling's config alone until the container returns. A fully-offline
		// group keeps its complete cached config.
		lastSeen, labelConfigs, period := cached.LastSeen, cached.LabelConfigs, cached.Period
		if liveGroup != nil {
			lastSeen, labelConfigs, period = now, liveGroup.LabelConfigs, liveGroup.Period
		}

		f.Groups[name] = CachedGroup{Volumes: vols, Databases: dbs, LabelConfigs: labelConfigs, Period: period, LastSeen: lastSeen}
		merged.Groups[name] = &models.VolumeGroup{Volumes: vols, Databases: dbs, LabelConfigs: labelConfigs, Period: period}
		if len(offVols) > 0 {
			off.Volumes[name] = offVols
		}
		if len(offDBs) > 0 {
			off.Databases[name] = offDBs
		}
	}

	c.write(f)
	c.groups = f.Groups
	return merged, off
}

func liveVolumes(g *models.VolumeGroup) []models.VolumeInfo {
	if g == nil {
		return nil
	}
	return g.Volumes
}

func liveDatabases(g *models.VolumeGroup) []models.DatabaseConfig {
	if g == nil {
		return nil
	}
	return g.Databases
}

// mergeVolumes unions live and cached volumes by name (live wins). A cached-only
// volume is kept and marked offline while its path exists; a path-gone volume is
// dropped as truly deleted.
func mergeVolumes(liveVols, cachedVols []models.VolumeInfo, pathExists func(string) bool) ([]models.VolumeInfo, map[string]bool) {
	out := make([]models.VolumeInfo, 0, len(liveVols)+len(cachedVols))
	offline := map[string]bool{}
	liveByName := map[string]bool{}

	for _, v := range liveVols {
		out = append(out, v)
		liveByName[v.Name] = true
	}
	for _, v := range cachedVols {
		if liveByName[v.Name] || !pathExists(v.HostPath) {
			continue
		}
		out = append(out, v)
		offline[v.Name] = true
	}
	return out, offline
}

// mergeDatabases unions live and cached databases by type+name. A cached-only
// database is offline: its container is gone, so it cannot be dumped this cycle,
// but it stays tracked for restore awareness.
func mergeDatabases(liveDBs, cachedDBs []models.DatabaseConfig) ([]models.DatabaseConfig, map[string]bool) {
	out := make([]models.DatabaseConfig, 0, len(liveDBs)+len(cachedDBs))
	offline := map[string]bool{}
	liveByKey := map[string]bool{}

	for _, db := range liveDBs {
		out = append(out, db)
		liveByKey[dbKey(db)] = true
	}
	for _, db := range cachedDBs {
		if liveByKey[dbKey(db)] {
			continue
		}
		out = append(out, db)
		offline[dbKey(db)] = true
	}
	return out, offline
}

// StripUndumpableDatabases removes offline (container-gone) databases from the
// backup set in place: a dump helper cannot join a namespace that is gone.
// Volumes stay, so a partly-stopped group still backs up its files. warn, if
// set, is called for each skipped database.
func (o *Offline) StripUndumpableDatabases(merged *models.BackupState, warn func(group string, db models.DatabaseConfig)) {
	for name, g := range merged.Groups {
		offset := o.Databases[name]
		if len(offset) == 0 {
			continue
		}
		kept := g.Databases[:0]
		for _, db := range g.Databases {
			if offset[dbKey(db)] {
				if warn != nil {
					warn(name, db)
				}
				continue
			}
			kept = append(kept, db)
		}
		g.Databases = kept
	}
}

// Names returns every cached group name; the scheduler passes it to
// ScheduleStore.Retain so an offline group's last-backup record is not pruned.
func (c *GroupCache) Names() map[string]struct{} {
	c.mu.Lock()
	defer c.mu.Unlock()
	names := make(map[string]struct{}, len(c.groups))
	for name := range c.groups {
		names[name] = struct{}{}
	}
	return names
}

// LastSeen returns when a group last had a live container; ok is false for
// unknown groups.
func (c *GroupCache) LastSeen(name string) (time.Time, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	g, ok := c.groups[name]
	return g.LastSeen, ok
}

func (c *GroupCache) write(f groupCacheFile) {
	if err := os.MkdirAll(filepath.Dir(c.path), 0o700); err != nil {
		c.logger.Warn("cannot create state directory; group cache will not persist", "path", c.path, "error", err)
		return
	}
	// The cache can carry plaintext DB passwords from labels, so 0600.
	tmp, err := os.CreateTemp(filepath.Dir(c.path), "groups-*.json.tmp")
	if err != nil {
		c.logger.Warn("cannot write group cache", "error", err)
		return
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()

	data, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		_ = tmp.Close()
		c.logger.Warn("cannot encode group cache", "error", err)
		return
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		c.logger.Warn("cannot write group cache", "error", err)
		return
	}
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		c.logger.Warn("cannot chmod group cache", "error", err)
		return
	}
	if err := tmp.Close(); err != nil {
		c.logger.Warn("cannot close group cache temp", "error", err)
		return
	}
	if err := os.Rename(tmpName, c.path); err != nil {
		c.logger.Warn("cannot install group cache", "error", err)
	}
}
