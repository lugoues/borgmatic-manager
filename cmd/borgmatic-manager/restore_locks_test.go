package main

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/lugoues/borgmatic-manager/internal/config"
	"github.com/lugoues/borgmatic-manager/internal/models"
)

// The lock set must cover the group being restored FROM and every group that
// backs up the volume being restored INTO: --into can point at a volume the
// source group does not own, and it is the owner's scheduled backup that must
// not archive the volume mid-wipe.
func TestRestoreLockKeys(t *testing.T) {
	bs := models.NewBackupState()
	bs.AddVolume("source", models.VolumeInfo{Name: "src-vol", HostPath: "/mnt/src"})
	bs.AddVolume("owner", models.VolumeInfo{Name: "target-vol", HostPath: "/mnt/tgt"})
	bs.AddVolume("bystander", models.VolumeInfo{Name: "other-vol", HostPath: "/mnt/other"})

	meta := map[string]config.GroupRunMeta{
		"source":    {Repos: []string{"/repo/a"}},
		"owner":     {Repos: []string{"/repo/b"}},
		"bystander": {Repos: []string{"/repo/c"}},
	}

	t.Run("restoring into another group's volume locks both groups", func(t *testing.T) {
		keys := restoreLockKeys(bs, meta, "source", "target-vol")
		assert.Equal(t, []string{
			"group:owner", "group:source",
			"repo:/repo/a", "repo:/repo/b",
		}, keys, "the bystander group is not involved and must not be locked")
	})

	t.Run("restoring a group's own volume locks it once", func(t *testing.T) {
		keys := restoreLockKeys(bs, meta, "source", "src-vol")
		assert.Equal(t, []string{"group:source", "repo:/repo/a"}, keys)
	})

	t.Run("shared repositories are deduplicated", func(t *testing.T) {
		shared := map[string]config.GroupRunMeta{
			"source": {Repos: []string{"/repo/shared"}},
			"owner":  {Repos: []string{"/repo/shared"}},
		}
		keys := restoreLockKeys(bs, shared, "source", "target-vol")
		assert.Equal(t, []string{"group:owner", "group:source", "repo:/repo/shared"}, keys)
	})

	t.Run("a refused target group still contributes its group key", func(t *testing.T) {
		// "owner" absent from meta: refused by THIS generation. A daemon can
		// still be finishing a backup generated before the configuration
		// change, and the group flock is the identity both generations share;
		// only the repository keys are unknowable here.
		partial := map[string]config.GroupRunMeta{"source": {Repos: []string{"/repo/a"}}}
		keys := restoreLockKeys(bs, partial, "source", "target-vol")
		assert.Equal(t, []string{"group:owner", "group:source", "repo:/repo/a"}, keys)
	})

	t.Run("the source group key is taken even with no resolvable repos", func(t *testing.T) {
		empty := map[string]config.GroupRunMeta{"source": {}}
		keys := restoreLockKeys(bs, empty, "source", "src-vol")
		assert.Equal(t, []string{"group:source"}, keys,
			"the group flock alone still serializes against the runner, which flocks the same key")
	})
}
