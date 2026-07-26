package main

import (
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"
)

// liveVolume builds a volume data dir holding one file, the thing a restore
// must never destroy without a replacement in hand.
func liveVolume(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	data := filepath.Join(root, "volumes", "myvol", "_data")
	require.NoError(t, os.MkdirAll(data, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(data, "original.txt"), []byte("original"), 0o644))
	return data
}

func names(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		out = append(out, e.Name())
	}
	return out
}

func TestRestoreWithSwapReplacesTheLiveData(t *testing.T) {
	data := liveVolume(t)

	err := restoreWithSwap(data, quietLogger(), false, func(dest string) error {
		return os.WriteFile(filepath.Join(dest, "restored.txt"), []byte("restored"), 0o644)
	}, nil)
	require.NoError(t, err)

	assert.Equal(t, []string{"restored.txt"}, names(t, data), "mirror semantics: the old contents are gone")
	body, err := os.ReadFile(filepath.Join(data, "restored.txt"))
	require.NoError(t, err)
	assert.Equal(t, "restored", string(body))

	// Neither scratch directory outlives the operation.
	assert.NoDirExists(t, data+stagingSuffix)
	assert.NoDirExists(t, data+oldSuffix)
}

// The whole point: a failed extract must cost the operator nothing. Before
// this, the target was emptied first, so any failure after that left an empty
// volume and no restore.
func TestRestoreWithSwapLeavesLiveDataIntactWhenExtractFails(t *testing.T) {
	data := liveVolume(t)
	boom := errors.New("archive was pruned mid-extract")

	err := restoreWithSwap(data, quietLogger(), false, func(dest string) error {
		// A partial extract, then failure: the realistic shape.
		require.NoError(t, os.WriteFile(filepath.Join(dest, "half.txt"), []byte("partial"), 0o644))
		return boom
	}, nil)

	require.Error(t, err)
	require.ErrorIs(t, err, boom, "the cause reaches the operator")
	assert.Contains(t, err.Error(), "volume is untouched")
	assert.Equal(t, []string{"original.txt"}, names(t, data), "the original data survived untouched")
	assert.NoDirExists(t, data+stagingSuffix, "the partial extract is cleaned up")
}

// borgmatic can exit 0 having matched nothing, for instance when the archive
// predates the volume. Promoting an empty directory would silently wipe it.
func TestRestoreWithSwapRefusesAnEmptyExtract(t *testing.T) {
	data := liveVolume(t)

	err := restoreWithSwap(data, quietLogger(), false, func(string) error { return nil }, nil)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "extracted nothing")
	assert.Equal(t, []string{"original.txt"}, names(t, data), "an empty result never becomes the volume")
	assert.NoDirExists(t, data+stagingSuffix)
}

// A run killed between extract and swap leaves staging behind. The next
// restore must start clean rather than promote a mixture of two restores.
func TestRestoreWithSwapDiscardsLeftoverStaging(t *testing.T) {
	data := liveVolume(t)
	stale := data + stagingSuffix
	require.NoError(t, os.MkdirAll(stale, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(stale, "from-a-previous-run.txt"), []byte("stale"), 0o644))

	err := restoreWithSwap(data, quietLogger(), false, func(dest string) error {
		assert.Empty(t, names(t, dest), "the staging directory starts empty")
		return os.WriteFile(filepath.Join(dest, "restored.txt"), []byte("restored"), 0o644)
	}, nil)
	require.NoError(t, err)
	assert.Equal(t, []string{"restored.txt"}, names(t, data))
}

// The replacement directory has to be indistinguishable from the one it
// replaces, or a container that owned the volume can no longer read it.
func TestRestoreWithSwapPreservesDirectoryMode(t *testing.T) {
	data := liveVolume(t)
	require.NoError(t, os.Chmod(data, 0o770))
	before, err := os.Stat(data)
	require.NoError(t, err)

	require.NoError(t, restoreWithSwap(data, quietLogger(), false, func(dest string) error {
		return os.WriteFile(filepath.Join(dest, "restored.txt"), []byte("x"), 0o644)
	}, nil))

	after, err := os.Stat(data)
	require.NoError(t, err)
	assert.Equal(t, before.Mode().Perm(), after.Mode().Perm(), "mode carried onto the replacement")
}

// The rename-pair fallback is not one atomic step. If the second rename fails,
// the volume must not be left without a data directory at all. Exercised
// directly, since a kernel with RENAME_EXCHANGE never reaches this path.
func TestSwapIntoPlaceRestoresTheOriginalIfTheSecondRenameFails(t *testing.T) {
	data := liveVolume(t)
	missingStaging := data + stagingSuffix // deliberately never created

	_, err := swapByRenamePair(missingStaging, data)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "volume is unchanged")
	require.DirExists(t, data, "the original was put back rather than left displaced")
	assert.Equal(t, []string{"original.txt"}, names(t, data))
	assert.NoDirExists(t, data+oldSuffix)
}

// Reading a label must succeed on any host and report accurately. Whether one
// is there is not the test's to assume: on an SELinux system every file carries
// a context, and on every other system none do. Asserting absence here passed
// for the wrong reason until it was run on a real enforcing host, where the
// temp directory came back labelled container_file_t.
func TestReadSELinuxContextReportsWhatTheHostHas(t *testing.T) {
	dir := t.TempDir()
	label, present, err := readSELinuxContext(dir)

	require.NoError(t, err, "reading a context is never an error, labelled or not")
	if present {
		assert.NotEmpty(t, label, "a host that labels files gives a non-empty context")
	} else {
		assert.Empty(t, label, "a host without SELinux has nothing to report")
	}
}

// The claim this PR could not check without a real SELinux host: a labelled
// volume's label reaches the directory that replaces it. Skips where there is
// no label to carry, which is every non-SELinux host.
func TestCreateStagingLikeCarriesARealSELinuxLabel(t *testing.T) {
	dir := t.TempDir()
	model := filepath.Join(dir, "_data")
	require.NoError(t, os.Mkdir(model, 0o755))

	want, present, err := readSELinuxContext(model)
	require.NoError(t, err)
	if !present {
		t.Skip("no SELinux label to carry on this host")
	}

	staging := filepath.Join(dir, "_data"+stagingSuffix)
	require.NoError(t, createStagingLike(staging, model))

	got, present, err := readSELinuxContext(staging)
	require.NoError(t, err)
	require.True(t, present, "the replacement must be labelled, or its container cannot read it")
	assert.Equal(t, want, got, "and with the same label the original had")
}

// A label that exists but cannot be applied must stop the restore before the
// swap. Promoting a mislabelled directory hands back a volume its own container
// cannot read, and the original is gone by the time anyone finds out. Driven
// through the xattr seam, because this kernel has no SELinux to refuse for real
// (Ubuntu ships AppArmor, and an LSM is not something a container can supply).
func TestCreateStagingLikeRefusesWhenTheLabelCannotBeApplied(t *testing.T) {
	dir := t.TempDir()
	model := filepath.Join(dir, "_data")
	require.NoError(t, os.Mkdir(model, 0o755))

	const label = "system_u:object_r:container_file_t:s0"
	// Pretend the volume is labelled, and that applying it is refused, which is
	// what an unprivileged restore on an enforcing host actually sees.
	restore := stubXattr(t, map[string][]byte{seLinuxAttr: []byte(label)}, unix.EPERM)
	defer restore()

	staging := filepath.Join(dir, "_data"+stagingSuffix)
	err := createStagingLike(staging, model)

	require.Error(t, err, "an unappliable label must stop the restore, not be ignored")
	require.ErrorIs(t, err, unix.EPERM)
	assert.Contains(t, err.Error(), label, "the operator is told which label")
	assert.Contains(t, err.Error(), "nothing was changed")
	assert.Contains(t, err.Error(), "--merge", "and how to proceed anyway")
}

// A volume with no label at all must not be blocked: that is every non-SELinux
// host, and the overwhelmingly common case.
func TestCreateStagingLikeProceedsWithoutALabel(t *testing.T) {
	dir := t.TempDir()
	model := filepath.Join(dir, "_data")
	require.NoError(t, os.Mkdir(model, 0o755))

	// Refuses every write, but there is nothing to write.
	restore := stubXattr(t, nil, unix.EPERM)
	defer restore()

	assert.NoError(t, createStagingLike(filepath.Join(dir, "_data"+stagingSuffix), model))
}

// The special bits carry semantics a volume can depend on: setgid decides
// group inheritance for everything created after the restore.
func TestCreateStagingLikePreservesSpecialModeBits(t *testing.T) {
	dir := t.TempDir()
	model := filepath.Join(dir, "_data")
	require.NoError(t, os.Mkdir(model, 0o755))
	require.NoError(t, os.Chmod(model, 0o775|os.ModeSetgid)) // FileMode, not the 0o2000 octal
	before, err := os.Stat(model)
	require.NoError(t, err)
	require.NotZero(t, before.Mode()&os.ModeSetgid, "precondition: setgid is set")

	staging := filepath.Join(dir, "_data"+stagingSuffix)
	require.NoError(t, createStagingLike(staging, model))

	after, err := os.Stat(staging)
	require.NoError(t, err)
	assert.NotZero(t, after.Mode()&os.ModeSetgid, "setgid carried onto the replacement")
	assert.Equal(t, before.Mode().Perm(), after.Mode().Perm())
}

// Once the swap lands, the restore has succeeded. Failing to delete the copy it
// replaced is wasted disk, not a failed restore, and must not turn a completed
// restore into a nonzero exit that scripts read as "it did not work".
func TestRestoreWithSwapSucceedsEvenIfTheReplacedCopyCannotBeRemoved(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores the directory permissions this test relies on")
	}
	data := liveVolume(t)
	// A subdirectory whose contents cannot be unlinked. The renames still work,
	// because the volume directory itself stays writable, but removing the
	// displaced copy afterwards does not.
	locked := filepath.Join(data, "locked")
	require.NoError(t, os.Mkdir(locked, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(locked, "pinned.txt"), []byte("pinned"), 0o644))
	require.NoError(t, os.Chmod(locked, 0o500))
	// The swap moves it to the displaced path, and it has to be writable again
	// for TempDir's own cleanup to succeed; chmod wherever it ended up.
	// The swap moves it, and where depends on whether the filesystem supports an
	// atomic exchange, so make it writable again wherever it ended up or
	// TempDir's own cleanup fails.
	t.Cleanup(func() {
		for _, p := range []string{locked, filepath.Join(data+oldSuffix, "locked"), filepath.Join(data+stagingSuffix, "locked")} {
			_ = os.Chmod(p, 0o755)
		}
	})

	err := restoreWithSwap(data, quietLogger(), false, func(dest string) error {
		return os.WriteFile(filepath.Join(dest, "restored.txt"), []byte("restored"), 0o644)
	}, nil)

	require.NoError(t, err, "a cleanup failure must not fail a restore that landed")
	assert.Equal(t, []string{"restored.txt"}, names(t, data), "the restored data is live")
}

// The state a crash between the fallback path's two renames leaves behind: no
// data directory, and the only copy under a suffixed name. Without recovery the
// next restore stops at "data directory is not present" and the operator has to
// know to go looking for it.
func TestRecoverInterruptedSwapPutsTheDataBack(t *testing.T) {
	data := liveVolume(t)
	displaced := data + oldSuffix
	require.NoError(t, os.Rename(data, displaced)) // interrupted between the renames
	require.NoDirExists(t, data)

	require.NoError(t, recoverInterruptedSwap(data, quietLogger()))

	require.DirExists(t, data, "the volume has its data directory back")
	assert.Equal(t, []string{"original.txt"}, names(t, data))
	assert.NoDirExists(t, displaced)
}

func TestRecoverInterruptedSwapLeavesAHealthyVolumeAlone(t *testing.T) {
	data := liveVolume(t)
	// A displaced copy from some earlier run, with the volume perfectly fine.
	displaced := data + oldSuffix
	require.NoError(t, os.Mkdir(displaced, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(displaced, "stale.txt"), []byte("stale"), 0o644))

	require.NoError(t, recoverInterruptedSwap(data, quietLogger()))

	assert.Equal(t, []string{"original.txt"}, names(t, data), "live data is never overwritten by a leftover")
	assert.DirExists(t, displaced, "and the leftover is left for the operator to judge")
}

// A volume that was empty when it was backed up is in the archive as a bare
// directory. Restoring it back to empty is the correct outcome, so an empty
// extract must be accepted when the archive says that is what it holds.
func TestRestoreWithSwapAcceptsAnArchivedEmptyDirectory(t *testing.T) {
	data := liveVolume(t)

	err := restoreWithSwap(data, quietLogger(), true, func(string) error { return nil }, nil)

	require.NoError(t, err, "the archive holds this directory with no children")
	assert.Empty(t, names(t, data), "the volume is restored to the empty state it was archived in")
	assert.NoDirExists(t, data+stagingSuffix)
}

// posixACL builds a minimal POSIX access ACL granting a named user rwx, in the
// on-disk xattr layout: a 4-byte version followed by 8-byte entries of
// {tag, perm, id}. Built by hand so the test needs no acl tooling installed.
func posixACL(uid uint32) []byte {
	const (
		tagUserObj  = 0x01
		tagUser     = 0x02
		tagGroupObj = 0x04
		tagMask     = 0x10
		tagOther    = 0x20
		undefinedID = 0xFFFFFFFF
	)
	acl := binary.LittleEndian.AppendUint32(nil, 2) // version
	for _, e := range []struct {
		tag, perm uint16
		id        uint32
	}{
		{tagUserObj, 7, undefinedID},
		{tagUser, 5, uid},
		{tagGroupObj, 5, undefinedID},
		{tagMask, 7, undefinedID},
		{tagOther, 5, undefinedID},
	} {
		acl = binary.LittleEndian.AppendUint16(acl, e.tag)
		acl = binary.LittleEndian.AppendUint16(acl, e.perm)
		acl = binary.LittleEndian.AppendUint32(acl, e.id)
	}
	return acl
}

// The in-place restore kept the inode and its ACLs for free. Promoting a new
// directory loses anything not carried across, and a dropped access ACL
// silently revokes access while a dropped default ACL changes what everything
// created after the restore inherits.
func TestCreateStagingLikeCopiesPOSIXACLs(t *testing.T) {
	dir := t.TempDir()
	model := filepath.Join(dir, "_data")
	require.NoError(t, os.Mkdir(model, 0o755))

	access, dflt := posixACL(1001), posixACL(1002)
	if err := unix.Lsetxattr(model, "system.posix_acl_access", access, 0); err != nil {
		t.Skipf("POSIX ACLs unsupported here: %v", err)
	}
	require.NoError(t, unix.Lsetxattr(model, "system.posix_acl_default", dflt, 0))

	staging := filepath.Join(dir, "_data"+stagingSuffix)
	require.NoError(t, createStagingLike(staging, model))

	for attr, want := range map[string][]byte{
		"system.posix_acl_access":  access,
		"system.posix_acl_default": dflt,
	} {
		got, present, err := readXattr(staging, attr)
		require.NoError(t, err)
		require.True(t, present, "%s was not carried to the replacement", attr)
		assert.Equal(t, want, got, "%s differs from the original", attr)
	}
}

// stubXattr makes the model directory appear to carry present, and makes every
// attempt to write an attribute fail with writeErr. Returns a restore func.
func stubXattr(t *testing.T, present map[string][]byte, writeErr error) func() {
	t.Helper()
	realRead, realWrite := readXattrFn, setXattr
	readXattrFn = func(path, attr string) ([]byte, bool, error) {
		v, ok := present[attr]
		return v, ok, nil
	}
	setXattr = func(string, string, []byte, int) error { return writeErr }
	return func() { readXattrFn, setXattr = realRead, realWrite }
}

// The extract can run for a long time, and a container started meanwhile has
// mounted the very inode the swap is about to move. The last-moment recheck
// must stop the swap, and the restored copy must be kept rather than discarded.
func TestRestoreWithSwapAbortsIfTheVolumeBecameLiveDuringTheExtract(t *testing.T) {
	data := liveVolume(t)
	appeared := errors.New("a container started using the volume")

	err := restoreWithSwap(data, quietLogger(), false, func(dest string) error {
		return os.WriteFile(filepath.Join(dest, "restored.txt"), []byte("restored"), 0o644)
	}, func() error { return appeared })

	require.Error(t, err)
	require.ErrorIs(t, err, appeared)
	assert.Contains(t, err.Error(), "volume is untouched")
	assert.Equal(t, []string{"original.txt"}, names(t, data), "the live data was not swapped")
}

// A named pipe in the archive would block a read-only open forever, and
// borgmatic has already exited by then, so the restore would hang just before
// the swap. Only regular files and directories are opened.
func TestSyncTreeSkipsFIFOs(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "regular.txt"), []byte("x"), 0o644))
	require.NoError(t, os.Mkdir(filepath.Join(dir, "sub"), 0o755))
	if err := unix.Mkfifo(filepath.Join(dir, "pipe"), 0o644); err != nil {
		t.Skipf("cannot create a FIFO here: %v", err)
	}

	done := make(chan error, 1)
	go func() { done <- syncTree(dir) }()

	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(20 * time.Second):
		t.Fatal("syncTree blocked, almost certainly opening the FIFO and waiting for a writer")
	}
}

// A completed extract can represent hours of work. When the safety recheck
// aborts the swap, that copy is kept and named in the error, and it has to be
// kept somewhere the operator's very next attempt will not delete: a retry
// clears the staging path as its first act.
func TestRestoreWithSwapKeepsTheExtractWhenTheRecheckAborts(t *testing.T) {
	data := liveVolume(t)
	appeared := errors.New("a container started using the volume")

	err := restoreWithSwap(data, quietLogger(), false, func(dest string) error {
		return os.WriteFile(filepath.Join(dest, "restored.txt"), []byte("restored"), 0o644)
	}, func() error { return appeared })

	require.Error(t, err)
	assert.NoDirExists(t, data+stagingSuffix, "not left on the path a retry clears first")

	kept := keptDirsFor(t, data)
	require.Len(t, kept, 1, "the completed extract is kept under a name later runs do not reuse")
	assert.Contains(t, err.Error(), kept[0], "and the error points at where it actually is")
	assert.Equal(t, []string{"restored.txt"}, names(t, kept[0]))
	assert.Equal(t, []string{"original.txt"}, names(t, data), "the volume is untouched")
}

// A retry after that abort must not destroy the kept copy.
func TestRestoreWithSwapRetryDoesNotDeleteAKeptExtract(t *testing.T) {
	data := liveVolume(t)
	appeared := errors.New("a container started using the volume")

	require.Error(t, restoreWithSwap(data, quietLogger(), false, func(dest string) error {
		return os.WriteFile(filepath.Join(dest, "restored.txt"), []byte("restored"), 0o644)
	}, func() error { return appeared }))
	kept := keptDirsFor(t, data)
	require.Len(t, kept, 1)

	// The operator stops the container and runs it again.
	require.NoError(t, restoreWithSwap(data, quietLogger(), false, func(dest string) error {
		return os.WriteFile(filepath.Join(dest, "second.txt"), []byte("second"), 0o644)
	}, func() error { return nil }))

	assert.DirExists(t, kept[0], "the retry must not delete what the previous run deliberately kept")
	assert.Equal(t, []string{"second.txt"}, names(t, data))
}

// keptDirsFor lists the retained copies beside a volume's data directory.
func keptDirsFor(t *testing.T, data string) []string {
	t.Helper()
	entries, err := os.ReadDir(filepath.Dir(data))
	require.NoError(t, err)
	var out []string
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), filepath.Base(data)+".borgmatic-manager-kept-") {
			out = append(out, filepath.Join(filepath.Dir(data), e.Name()))
		}
	}
	return out
}

// A bind mount whose source shares the filesystem has the same device number as
// its parent, so a st_dev comparison misses it. That is exactly how a
// local-driver bind volume looks, and renaming it would fail with EBUSY after a
// full extract had already run.
func TestIsOwnMountPointDetectsASameFilesystemBind(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("bind mounting needs root")
	}
	root := t.TempDir()
	src, dst := filepath.Join(root, "src"), filepath.Join(root, "dst")
	require.NoError(t, os.Mkdir(src, 0o755))
	require.NoError(t, os.Mkdir(dst, 0o755))
	if err := unix.Mount(src, dst, "", unix.MS_BIND, ""); err != nil {
		t.Skipf("cannot bind mount here: %v", err)
	}
	t.Cleanup(func() { _ = unix.Unmount(dst, unix.MNT_DETACH) })

	var self, parent unix.Stat_t
	require.NoError(t, unix.Lstat(dst, &self))
	require.NoError(t, unix.Lstat(root, &parent))
	require.Equal(t, parent.Dev, self.Dev, "precondition: the device number is identical, which is why st_dev is not enough")

	mounted, err := isOwnMountPoint(dst)
	require.NoError(t, err)
	assert.True(t, mounted, "mountinfo sees the bind that st_dev cannot")

	plain, err := isOwnMountPoint(src)
	require.NoError(t, err)
	assert.False(t, plain, "an ordinary directory is not a mount point")
}

func TestUnescapeMountField(t *testing.T) {
	assert.Equal(t, "/var/lib/docker/volumes/v/_data", unescapeMountField("/var/lib/docker/volumes/v/_data"))
	assert.Equal(t, "/mnt/with space", unescapeMountField(`/mnt/with\040space`), "the kernel octal-escapes spaces")
	assert.Equal(t, "/mnt/tab\tsep", unescapeMountField(`/mnt/tab\011sep`))
}

// createStagingLike builds the directory and then configures it. A failure in
// that second half must not leave the half-built directory behind: the cleanup
// has to cover it, which means being registered before it exists.
func TestRestoreWithSwapLeavesNoStagingWhenSetupFailsPartway(t *testing.T) {
	data := liveVolume(t)

	// A label the host claims to have but refuses to apply: createStagingLike
	// fails after os.Mkdir has already run.
	restore := stubXattr(t, map[string][]byte{seLinuxAttr: []byte("system_u:object_r:container_file_t:s0")}, unix.EPERM)
	defer restore()

	err := restoreWithSwap(data, quietLogger(), false, func(string) error {
		t.Fatal("extract must not run when the staging directory could not be set up")
		return nil
	}, nil)

	require.Error(t, err)
	assert.NoDirExists(t, data+stagingSuffix, "a half-built staging directory must not be left on disk")
	assert.Equal(t, []string{"original.txt"}, names(t, data), "and the volume is untouched")
}
