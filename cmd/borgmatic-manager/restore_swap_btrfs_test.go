//go:build btrfs

// Tests that need a real btrfs filesystem. They are behind a build tag rather
// than a runtime skip so the fixture's environment-derived path never reaches
// production code in the default build, where the taint analysis reads it as
// untrusted input arriving at os.Open and os.Chmod.
//
// Run them with a writable btrfs mount:
//
//	BM_BTRFS_DIR=/mnt/btrfs mise run test-btrfs

package main

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lugoues/borgmatic-manager/internal/config"
)

// btrfsVolume returns a volume data dir that is its own btrfs subvolume, or
// skips. The exchange happily trades a plain directory for a subvolume root, so
// nothing fails and nothing warns: the only way to catch the loss is to look at
// what the volume is afterwards.
func btrfsVolume(t *testing.T) string {
	t.Helper()
	root := os.Getenv("BM_BTRFS_DIR")
	if root == "" {
		t.Skip("set BM_BTRFS_DIR to a writable btrfs mount to run this")
	}
	base, err := os.MkdirTemp(root, "bm-subvol-")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(base) })

	volume := filepath.Join(base, "volumes", "myvol")
	require.NoError(t, os.MkdirAll(volume, 0o755))
	data := filepath.Join(volume, "_data")
	require.NoError(t, createBtrfsSubvolume(data))
	require.NoError(t, os.WriteFile(filepath.Join(data, "original.txt"), []byte("original"), 0o644))

	subvol, err := isBtrfsSubvolumeRoot(data)
	require.NoError(t, err)
	require.True(t, subvol, "the fixture must actually be a subvolume")
	return data
}

func TestRestoreWithSwapKeepsTheVolumeABtrfsSubvolume(t *testing.T) {
	data := btrfsVolume(t)

	require.NoError(t, restoreWithSwap(data, quietLogger(), false, func(dest string) error {
		return os.WriteFile(filepath.Join(dest, "restored.txt"), []byte("restored"), 0o644)
	}, nil))

	assert.Equal(t, []string{"restored.txt"}, names(t, data), "the restore landed")
	stillSubvol, err := isBtrfsSubvolumeRoot(data)
	require.NoError(t, err)
	assert.True(t, stillSubvol, "the volume was silently demoted to an ordinary directory")
}

// Without btrfs-progs the restore must still happen. Losing the subvolume is
// worth a warning, not a refusal: the operator still needs their data back.
func TestRestoreWithSwapRestoresEvenIfNoSubvolumeCanBeCreated(t *testing.T) {
	data := btrfsVolume(t)

	real := createBtrfsSubvolume
	createBtrfsSubvolume = func(string) error { return errors.New("btrfs: command not found") }
	defer func() { createBtrfsSubvolume = real }()

	logs, logger := capturedWarnLogger()
	require.NoError(t, restoreWithSwap(data, logger, false, func(dest string) error {
		return os.WriteFile(filepath.Join(dest, "restored.txt"), []byte("restored"), 0o644)
	}, nil))

	assert.Equal(t, []string{"restored.txt"}, names(t, data), "the data still came back")
	assert.Contains(t, logs.String(), "ordinary one", "and the demotion was reported")
}

// Inode 256 is a btrfs subvolume root, but on any other filesystem it is just an
// inode number. Mistaking one for a subvolume would send an ext4 volume down a
// path that shells out to btrfs for no reason.
func TestIsBtrfsSubvolumeRootIgnoresInode256Elsewhere(t *testing.T) {
	dir := t.TempDir()
	onBtrfs, err := config.IsBtrfs(dir)
	require.NoError(t, err)
	if onBtrfs {
		t.Skip("the temp dir is on btrfs, so it cannot stand in for another filesystem")
	}
	got, err := isBtrfsSubvolumeRoot(dir)
	require.NoError(t, err)
	assert.False(t, got, "a non-btrfs path is never a subvolume root")
}
