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
