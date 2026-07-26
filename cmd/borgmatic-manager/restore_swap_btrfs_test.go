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
// btrfsPlainVolume returns a volume data dir that is an ordinary directory on
// btrfs, so a staging sibling inherits the parent's inode flags the way it does
// on any other filesystem.
func btrfsPlainVolume(t *testing.T) string {
	t.Helper()
	root := os.Getenv("BM_BTRFS_DIR")
	if root == "" {
		t.Skip("set BM_BTRFS_DIR to a writable btrfs mount to run this")
	}
	base, err := os.MkdirTemp(root, "bm-plain-")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(base) })

	data := filepath.Join(base, "volumes", "myvol", "_data")
	require.NoError(t, os.MkdirAll(data, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(data, "original.txt"), []byte("original"), 0o644))

	subvol, err := isBtrfsSubvolumeRoot(data)
	require.NoError(t, err)
	require.False(t, subvol, "the fixture must be an ordinary directory")
	return data
}

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

// A new subvolume has a new subvolume id, so it is a new qgroup: limits and
// parent assignments are keyed on the old id and stay with the directory this
// restore is about to delete. Measured rather than assumed, on a real
// filesystem, because the volume keeps working and nothing else says it has
// left its quota.
func TestRestoreWithSwapWarnsThatQgroupsDoNotFollow(t *testing.T) {
	data := btrfsVolume(t)
	if !btrfsQuotaEnabled(data) {
		t.Skip("quotas are not enabled on this btrfs mount (btrfs quota enable <mount>)")
	}

	logs, logger := capturedWarnLogger()
	require.NoError(t, restoreWithSwap(data, logger, false, func(dest string) error {
		return os.WriteFile(filepath.Join(dest, "restored.txt"), []byte("restored"), 0o644)
	}, nil))

	assert.Contains(t, logs.String(), "qgroup", "the operator is not told the quota stopped applying")
	stillSubvol, err := isBtrfsSubvolumeRoot(data)
	require.NoError(t, err)
	assert.True(t, stillSubvol, "and it is still a subvolume")
}

// Without quotas there is nothing to lose, so the warning must not fire.
func TestRestoreWithSwapIsQuietAboutQgroupsWhenQuotasAreOff(t *testing.T) {
	data := btrfsVolume(t)
	if btrfsQuotaEnabled(data) {
		t.Skip("quotas are enabled on this btrfs mount, so this case cannot be exercised here")
	}

	logs, logger := capturedWarnLogger()
	require.NoError(t, restoreWithSwap(data, logger, false, func(dest string) error {
		return os.WriteFile(filepath.Join(dest, "restored.txt"), []byte("restored"), 0o644)
	}, nil))
	assert.NotContains(t, logs.String(), "qgroup")
}

// btrfs +C (nodatacow) is inheritable inode state reached by ioctl, not an
// extended attribute, so nothing else copies it. It has to be on staging before
// borg writes: setting it afterwards does not convert files that already exist.
// Database volumes turn CoW off deliberately, and losing it silently changes
// how every restored file is stored.
func TestRestoreWithSwapKeepsNoCoWOnTheVolume(t *testing.T) {
	data := btrfsVolume(t)
	if err := setInodeFlagsExactly(data, fsNoCoWFlag); err != nil {
		t.Skipf("cannot set +C here: %v", err)
	}
	flags, ok, err := readInodeFlags(data)
	require.NoError(t, err)
	require.True(t, ok)
	require.NotZero(t, flags&fsNoCoWFlag, "precondition: the volume really is +C")

	require.NoError(t, restoreWithSwap(data, quietLogger(), false, func(dest string) error {
		staged, _, flagErr := readInodeFlags(dest)
		require.NoError(t, flagErr)
		assert.NotZero(t, staged&fsNoCoWFlag,
			"the flag must be on staging before anything is written, or the files never inherit it")
		return os.WriteFile(filepath.Join(dest, "restored.txt"), []byte("restored"), 0o644)
	}, nil))

	after, ok, err := readInodeFlags(data)
	require.NoError(t, err)
	require.True(t, ok)
	assert.NotZero(t, after&fsNoCoWFlag, "the volume silently lost nodatacow")
	assert.Equal(t, []string{"restored.txt"}, names(t, data))
}

// A flag that cannot be applied is worth saying out loud, not worth refusing a
// restore over: the data lands correctly either way.
func TestRestoreWithSwapWarnsWhenInodeFlagsCannotBeApplied(t *testing.T) {
	data := btrfsVolume(t)
	if err := setInodeFlagsExactly(data, fsNoCoWFlag); err != nil {
		t.Skipf("cannot set +C here: %v", err)
	}

	realWrite := setInodeFlagsExactly
	setInodeFlagsExactly = func(string, int) error { return errors.New("refused") }
	defer func() { setInodeFlagsExactly = realWrite }()

	logs, logger := capturedWarnLogger()
	require.NoError(t, restoreWithSwap(data, logger, false, func(dest string) error {
		return os.WriteFile(filepath.Join(dest, "restored.txt"), []byte("restored"), 0o644)
	}, nil))
	assert.Contains(t, logs.String(), "inode flags")
	assert.Equal(t, []string{"restored.txt"}, names(t, data), "and the restore still happened")
}

// The staging sibling inherits these flags from the parent directory, so one the
// target does *not* have can arrive on its own. Only replacing the masked bits
// takes it back off; adding bits leaves an inherited nodatacow in place and the
// restored files silently stop being copy-on-write when the original was.
func TestRestoreWithSwapClearsAFlagInheritedFromTheParent(t *testing.T) {
	// A plain directory rather than a subvolume: staging for a subvolume target
	// is made by "btrfs subvolume create", which does not inherit the parent's
	// flags, so the fixture would never reproduce the inheritance this is about.
	// The first version of this test used btrfsVolume and passed against the
	// broken implementation for exactly that reason.
	data := btrfsPlainVolume(t)
	parent := filepath.Dir(data)

	// The parent is +C, so anything created in it inherits nodatacow.
	if err := setInodeFlagsExactly(parent, fsNoCoWFlag); err != nil {
		t.Skipf("cannot set +C on the parent here: %v", err)
	}
	// The volume itself deliberately is not.
	require.NoError(t, setInodeFlagsExactly(data, 0))
	flags, ok, err := readInodeFlags(data)
	require.NoError(t, err)
	require.True(t, ok)
	require.Zero(t, flags&fsNoCoWFlag, "precondition: the volume is copy-on-write")

	require.NoError(t, restoreWithSwap(data, quietLogger(), false, func(dest string) error {
		staged, _, flagErr := readInodeFlags(dest)
		require.NoError(t, flagErr)
		assert.Zero(t, staged&fsNoCoWFlag,
			"staging inherited nodatacow from the parent and it was not cleared before the extract")
		return os.WriteFile(filepath.Join(dest, "restored.txt"), []byte("restored"), 0o644)
	}, nil))

	after, ok, err := readInodeFlags(data)
	require.NoError(t, err)
	require.True(t, ok)
	assert.Zero(t, after&fsNoCoWFlag, "the volume silently gained nodatacow it never had")
}
