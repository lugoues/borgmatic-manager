package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lugoues/borgmatic-manager/internal/config"
)

func TestEmptyVolumeDataRefusesNonVolumePaths(t *testing.T) {
	dir := t.TempDir() // no "/volumes/" component
	require.NoError(t, os.WriteFile(filepath.Join(dir, "keep"), []byte("x"), 0o600))

	err := emptyVolumeData(dir)
	require.Error(t, err, "a path that is not a container volume must be refused")
	assert.Contains(t, err.Error(), "not a recognizable container volume")
	_, statErr := os.Stat(filepath.Join(dir, "keep"))
	assert.NoError(t, statErr, "nothing was deleted")
}

func TestEmptyVolumeDataClearsContentsKeepsDir(t *testing.T) {
	root := t.TempDir()
	data := filepath.Join(root, "volumes", "myvol", "_data")
	require.NoError(t, os.MkdirAll(filepath.Join(data, "sub"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(data, "a.txt"), []byte("a"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(data, "sub", "b.txt"), []byte("b"), 0o600))

	require.NoError(t, emptyVolumeData(data))

	entries, err := os.ReadDir(data)
	require.NoError(t, err)
	assert.Empty(t, entries, "contents removed")
	_, err = os.Stat(data)
	assert.NoError(t, err, "but the _data directory itself is kept")
}

func TestPlanVolumeRestoreSourceAndInto(t *testing.T) {
	// docker source volume, restore into itself
	p, err := planVolumeRestore("/var/lib/docker/volumes/myvol/_data", "")
	require.NoError(t, err)
	assert.Equal(t, "/var/lib/docker/volumes", p.volumesRoot)
	assert.Equal(t, "myvol/_data", p.archivePath)
	assert.Equal(t, "myvol", p.targetVolume)
	assert.Equal(t, "/var/lib/docker/volumes/myvol/_data", p.targetData)

	// --into a spare volume: same root, retargeted data dir, archive path unchanged
	p, err = planVolumeRestore("/var/lib/docker/volumes/myvol/_data", "myvol-restore")
	require.NoError(t, err)
	assert.Equal(t, "myvol/_data", p.archivePath, "still pulls the source volume's path from the archive")
	assert.Equal(t, "myvol-restore", p.targetVolume)
	assert.Equal(t, "/var/lib/docker/volumes/myvol-restore/_data", p.targetData)

	// rootless podman path
	p, err = planVolumeRestore("/home/u/.local/share/containers/storage/volumes/systemd-app/_data", "")
	require.NoError(t, err)
	assert.Equal(t, "/home/u/.local/share/containers/storage/volumes", p.volumesRoot)
	assert.Equal(t, "systemd-app/_data", p.archivePath)
}

func TestPlanVolumeRestoreRefusesIntoPaths(t *testing.T) {
	// --merge skips emptyVolumeData entirely, and that guard is only a substring
	// test for "/volumes/" anyway, so the escape has to be refused here.
	for _, into := range []string{
		"../../../srv/x",
		"../sibling",
		"sub/vol",
		"/absolute/vol",
		"..",
		".",
	} {
		_, err := planVolumeRestore("/var/lib/docker/volumes/myvol/_data", into)
		require.Error(t, err, "--into %q escapes the volumes root", into)
		assert.Contains(t, err.Error(), "bare volume name")
	}

	// Names that merely look path-adjacent are still valid volume names.
	for _, into := range []string{"myvol-restore", "my.vol", "..vol", "vol.."} {
		_, err := planVolumeRestore("/var/lib/docker/volumes/myvol/_data", into)
		assert.NoError(t, err, "--into %q is a legal volume name", into)
	}
}

func TestSnapshotVolumeRefusesNonBtrfs(t *testing.T) {
	dir := t.TempDir() // devcontainer temp is not btrfs
	_, err := snapshotVolume(context.Background(), dir)
	require.Error(t, err, "--snapshot must refuse a non-btrfs volume")
	assert.Contains(t, err.Error(), "btrfs")
	assert.Contains(t, err.Error(), "--into", "and point the user at the portable alternative")
}

func TestRestoreVolumeIntoAndSnapshotAreMutuallyExclusive(t *testing.T) {
	cmd := restoreVolumeCmd()
	cmd.SetArgs([]string{"grp", "vol", "--into", "spare", "--snapshot"})
	cmd.SilenceErrors = true
	cmd.SilenceUsage = true
	err := cmd.Execute()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "into")
	assert.Contains(t, err.Error(), "snapshot")
	assert.Contains(t, err.Error(), "none of the others")
}

// Exercises the real reflink path; run with BM_BTRFS_DIR pointing at a btrfs mount.
func TestSnapshotVolumeOnBtrfs(t *testing.T) {
	base := os.Getenv("BM_BTRFS_DIR")
	if base == "" {
		t.Skip("set BM_BTRFS_DIR to a btrfs mount to run")
	}
	data := filepath.Join(base, "vol", "_data")
	require.NoError(t, os.MkdirAll(filepath.Join(data, "sub"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(data, "a.txt"), []byte("hello"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(data, "sub", "b.txt"), []byte("world"), 0o644))
	t.Cleanup(func() { _ = os.RemoveAll(filepath.Join(base, "vol")) })

	ok, err := config.IsBtrfs(data)
	require.NoError(t, err)
	assert.True(t, ok, "the btrfs mount must be detected")

	snap, err := snapshotVolume(context.Background(), data)
	require.NoError(t, err)
	assert.Contains(t, snap, "_data.pre-restore-")

	got, err := os.ReadFile(filepath.Join(snap, "a.txt"))
	require.NoError(t, err)
	assert.Equal(t, "hello", string(got))
	got, err = os.ReadFile(filepath.Join(snap, "sub", "b.txt"))
	require.NoError(t, err)
	assert.Equal(t, "world", string(got))
}
