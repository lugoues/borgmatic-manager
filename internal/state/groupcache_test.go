package state_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lugoues/borgmatic-manager/internal/models"
	"github.com/lugoues/borgmatic-manager/internal/state"
)

// twoVolumeApp is a multi-container group: an app container (books/data) plus a
// still-running postgres, all under group "app".
func twoVolumeApp() *models.BackupState {
	bs := models.NewBackupState()
	bs.AddVolume("app", models.VolumeInfo{Name: "books", HostPath: "/vol/books"})
	bs.AddVolume("app", models.VolumeInfo{Name: "data", HostPath: "/vol/data"})
	bs.AddVolume("app", models.VolumeInfo{Name: "postgres", HostPath: "/vol/pg"})
	return bs
}

// alwaysExists makes every cached volume path look present (nothing deleted).
func loadCache(t *testing.T, dir string, exists func(string) bool) *state.GroupCache {
	t.Helper()
	c := state.LoadGroupCache(dir, nil)
	if exists != nil {
		c.SetPathExists(exists)
	} else {
		c.SetPathExists(func(string) bool { return true })
	}
	return c
}

func TestGroupCacheUnionsMembersWhenOneContainerStops(t *testing.T) {
	dir := t.TempDir()

	// Cycle 1: all three volumes live.
	c := loadCache(t, dir, nil)
	c.Reconcile(twoVolumeApp(), time.Now())

	// Cycle 2 (fresh process): only postgres is live now; the app container
	// that carried books/data was stopped. Their volume paths still exist.
	c2 := loadCache(t, dir, nil)
	partial := models.NewBackupState()
	partial.AddVolume("app", models.VolumeInfo{Name: "postgres", HostPath: "/vol/pg"})
	merged, off := c2.Reconcile(partial, time.Now())

	require.Contains(t, merged.Groups, "app")
	names := map[string]bool{}
	for _, v := range merged.Groups["app"].Volumes {
		names[v.Name] = true
	}
	assert.True(t, names["books"] && names["data"] && names["postgres"],
		"the stopped container's volumes must survive, not vanish: got %v", names)
	assert.True(t, off.VolumeOffline("app", "books"), "books is offline")
	assert.True(t, off.VolumeOffline("app", "data"), "data is offline")
	assert.False(t, off.VolumeOffline("app", "postgres"), "postgres is live")
	assert.False(t, off.GroupOffline("app", merged.Groups["app"]), "the group still has a live container")
}

// claimedApp is twoVolumeApp with each volume's claiming container recorded, as
// discovery reports it: "web" carries books/data, "db" carries postgres.
func claimedApp() *models.BackupState {
	bs := twoVolumeApp()
	bs.Containers = map[string]bool{"web": true, "db": true}
	bs.AddClaim("app", "books", "web")
	bs.AddClaim("app", "data", "web")
	bs.AddClaim("app", "postgres", "db")
	return bs
}

func volumeNames(g *models.VolumeGroup) map[string]bool {
	names := map[string]bool{}
	for _, v := range g.Volumes {
		names[v.Name] = true
	}
	return names
}

// A volumes filter (or a dropped enable label) leaves the container present but
// no longer claiming the volume. That is a removal, not an offline container.
func TestGroupCacheDropsVolumesTheirContainerNoLongerClaims(t *testing.T) {
	dir := t.TempDir()
	loadCache(t, dir, nil).Reconcile(claimedApp(), time.Now())

	// "web" is still there but now claims only books.
	live := models.NewBackupState()
	live.Containers = map[string]bool{"web": true, "db": true}
	live.AddVolume("app", models.VolumeInfo{Name: "books", HostPath: "/vol/books"})
	live.AddClaim("app", "books", "web")
	live.AddVolume("app", models.VolumeInfo{Name: "postgres", HostPath: "/vol/pg"})
	live.AddClaim("app", "postgres", "db")
	merged, off := loadCache(t, dir, nil).Reconcile(live, time.Now())

	assert.Equal(t, map[string]bool{"books": true, "postgres": true}, volumeNames(merged.Groups["app"]))
	assert.False(t, off.AnyOffline("app"))

	// And it stays gone on the next cycle.
	merged, _ = loadCache(t, dir, nil).Reconcile(live, time.Now())
	assert.False(t, volumeNames(merged.Groups["app"])["data"])
}

// A shared volume whose other claimer was removed must not be dropped when the
// remaining claimer lets go: the removed one's claim still stands.
func TestGroupCacheKeepsSharedVolumeWhileACoClaimerIsAbsent(t *testing.T) {
	dir := t.TempDir()
	both := models.NewBackupState()
	both.Containers = map[string]bool{"a": true, "b": true}
	both.AddVolume("app", models.VolumeInfo{Name: "shared", HostPath: "/vol/shared"})
	both.AddClaim("app", "shared", "a")
	both.AddClaim("app", "shared", "b")
	loadCache(t, dir, nil).Reconcile(both, time.Now())

	// b removed; a still claims it.
	onlyA := models.NewBackupState()
	onlyA.Containers = map[string]bool{"a": true}
	onlyA.AddVolume("app", models.VolumeInfo{Name: "shared", HostPath: "/vol/shared"})
	onlyA.AddClaim("app", "shared", "a")
	loadCache(t, dir, nil).Reconcile(onlyA, time.Now())

	// a lets go while b is still absent.
	none := models.NewBackupState()
	none.Containers = map[string]bool{"a": true}
	none.AddVolume("app", models.VolumeInfo{Name: "other", HostPath: "/vol/other"})
	none.AddClaim("app", "other", "a")
	merged, off := loadCache(t, dir, nil).Reconcile(none, time.Now())

	assert.True(t, volumeNames(merged.Groups["app"])["shared"], "b's claim keeps it")
	assert.True(t, off.VolumeOffline("app", "shared"))
}

// Discovery can skip a volume its container still selects (unlisted,
// unreadable, lazily unmounted). The claim stands, so it is not a removal.
func TestGroupCacheKeepsClaimedVolumeDiscoverySkipped(t *testing.T) {
	dir := t.TempDir()
	loadCache(t, dir, nil).Reconcile(claimedApp(), time.Now())

	live := claimedApp()
	live.Groups["app"].Volumes = live.Groups["app"].Volumes[1:] // books skipped, still claimed by web
	merged, off := loadCache(t, dir, nil).Reconcile(live, time.Now())

	assert.True(t, volumeNames(merged.Groups["app"])["books"])
	assert.True(t, off.VolumeOffline("app", "books"))
}

// A removed container (a quadlet with --rm) is absent, so its volumes stay as
// offline members. So does a volume whose claimers were never recorded.
func TestGroupCacheKeepsVolumesOfRemovedContainers(t *testing.T) {
	dir := t.TempDir()
	loadCache(t, dir, nil).Reconcile(claimedApp(), time.Now())

	live := models.NewBackupState()
	live.Containers = map[string]bool{"db": true}
	live.AddVolume("app", models.VolumeInfo{Name: "postgres", HostPath: "/vol/pg"})
	live.AddClaim("app", "postgres", "db")
	merged, off := loadCache(t, dir, nil).Reconcile(live, time.Now())

	assert.Equal(t, map[string]bool{"books": true, "data": true, "postgres": true}, volumeNames(merged.Groups["app"]))
	assert.True(t, off.VolumeOffline("app", "books"))

	legacy := t.TempDir()
	loadCache(t, legacy, nil).Reconcile(twoVolumeApp(), time.Now()) // no claims recorded
	merged, _ = loadCache(t, legacy, nil).Reconcile(live, time.Now())
	assert.True(t, volumeNames(merged.Groups["app"])["books"], "unknown claimers keep the volume")
}

func TestGroupCacheDropsDeletedVolumes(t *testing.T) {
	dir := t.TempDir()
	c := loadCache(t, dir, nil)
	c.Reconcile(twoVolumeApp(), time.Now())

	// The app container is gone AND its volumes were actually deleted (paths
	// gone). Only postgres survives; books/data drop rather than linger.
	c2 := loadCache(t, dir, func(p string) bool { return p == "/vol/pg" })
	partial := models.NewBackupState()
	partial.AddVolume("app", models.VolumeInfo{Name: "postgres", HostPath: "/vol/pg"})
	merged, off := c2.Reconcile(partial, time.Now())

	require.Len(t, merged.Groups["app"].Volumes, 1, "deleted volumes are dropped")
	assert.Equal(t, "postgres", merged.Groups["app"].Volumes[0].Name)
	assert.Empty(t, off.Volumes["app"], "nothing offline: the missing volumes were removed")
}

func TestGroupCacheFullyOfflineGroupSurvivesAndIsBacked(t *testing.T) {
	dir := t.TempDir()
	c := loadCache(t, dir, nil)
	c.Reconcile(liveOne("solo", "solo_vol", "/vol/solo"), time.Now())

	// Whole group's container gone, volume path still exists: kept, backed up,
	// and flagged fully offline.
	c2 := loadCache(t, dir, nil)
	merged, off := c2.Reconcile(models.NewBackupState(), time.Now())
	require.Contains(t, merged.Groups, "solo")
	assert.True(t, off.GroupOffline("solo", merged.Groups["solo"]))
}

func TestGroupCacheStripUndumpableDatabases(t *testing.T) {
	dir := t.TempDir()
	live := models.NewBackupState()
	live.AddVolume("app", models.VolumeInfo{Name: "app_vol", HostPath: "/vol/app"})
	live.AddDatabases("app", []models.DatabaseConfig{{Type: "postgresql", Name: "appdb", Container: "pg"}})

	c := loadCache(t, dir, nil)
	c.Reconcile(live, time.Now())

	// Postgres container stops; only the volume is live now.
	c2 := loadCache(t, dir, nil)
	volOnly := models.NewBackupState()
	volOnly.AddVolume("app", models.VolumeInfo{Name: "app_vol", HostPath: "/vol/app"})
	merged, off := c2.Reconcile(volOnly, time.Now())

	require.Len(t, merged.Groups["app"].Databases, 1, "the db is tracked while offline")
	skipped := []string{}
	off.StripUndumpableDatabases(merged, func(group string, db models.DatabaseConfig) {
		skipped = append(skipped, group+":"+db.Type+"/"+db.Name)
	})
	assert.Equal(t, []string{"app:postgresql/appdb"}, skipped, "the offline db is skipped from the backup")
	assert.Empty(t, merged.Groups["app"].Databases, "and removed from the backup set")
	assert.Len(t, merged.Groups["app"].Volumes, 1, "but the volume is still backed up")
}

func liveOne(group, vol, path string) *models.BackupState {
	bs := models.NewBackupState()
	bs.AddVolume(group, models.VolumeInfo{Name: vol, HostPath: path})
	return bs
}
