package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
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
	require.NoError(t, createStagingLike(staging, model, quietLogger()))

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
	err := createStagingLike(staging, model, quietLogger())

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

	assert.NoError(t, createStagingLike(filepath.Join(dir, "_data"+stagingSuffix), model, quietLogger()))
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
	require.NoError(t, createStagingLike(staging, model, quietLogger()))

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
	require.NoError(t, createStagingLike(staging, model, quietLogger()))

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
//
// Enumeration reports nothing, so these tests stay about the security metadata
// they are checking: on a real SELinux host the model would otherwise come back
// carrying a label this stub never accounted for.
func stubXattr(t *testing.T, present map[string][]byte, writeErr error) func() {
	t.Helper()
	return swapXattrSeams(t,
		func(string) ([]string, error) { return nil, nil },
		func(_, attr string) ([]byte, bool, error) {
			v, ok := present[attr]
			return v, ok, nil
		},
		func(string, string, []byte, int) error { return writeErr })
}

// swapXattrSeams replaces all three extended-attribute seams for one test.
func swapXattrSeams(t *testing.T,
	list func(string) ([]string, error),
	read func(string, string) ([]byte, bool, error),
	set func(string, string, []byte, int) error,
) func() {
	t.Helper()
	realList, realRead, realWrite := listXattrsFn, readXattrFn, setXattr
	listXattrsFn, readXattrFn, setXattr = list, read, set
	return func() { listXattrsFn, readXattrFn, setXattr = realList, realRead, realWrite }
}

// capturedWarnLogger returns a logger writing into the returned buffer, for
// asserting on a warning that is the whole point of a code path.
func capturedWarnLogger() (*bytes.Buffer, *slog.Logger) {
	buf := &bytes.Buffer{}
	return buf, slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelWarn}))
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

// The flush must not depend on being able to open what was extracted. A
// mode-000 file is readable by root but not by the unprivileged user a rootless
// Podman restore runs as, and a named pipe blocks a read-only open until a
// writer appears, which never happens once borgmatic has exited. Both used to
// be reachable from an ordinary archive.
func TestRestoreWithSwapFlushesAnUnopenableTree(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses the permission check this test is about")
	}
	data := liveVolume(t)

	err := restoreWithSwap(data, quietLogger(), false, func(dest string) error {
		if writeErr := os.WriteFile(filepath.Join(dest, "unreadable"), []byte("x"), 0o000); writeErr != nil {
			return writeErr
		}
		return unix.Mkfifo(filepath.Join(dest, "pipe"), 0o644)
	}, nil)

	require.NoError(t, err, "an archive this process cannot open must still restore")
	assert.FileExists(t, filepath.Join(data, "unreadable"))
}

// A FIFO would hang the restore rather than fail it, so the timeout is the
// assertion.
func TestRestoreWithSwapDoesNotBlockOnAFIFO(t *testing.T) {
	data := liveVolume(t)

	done := make(chan error, 1)
	go func() {
		done <- restoreWithSwap(data, quietLogger(), false, func(dest string) error {
			return unix.Mkfifo(filepath.Join(dest, "pipe"), 0o644)
		}, nil)
	}()

	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(20 * time.Second):
		t.Fatal("the restore blocked, almost certainly opening the FIFO and waiting for a writer")
	}
}

// The staged swap builds a new directory, so anything the live one carried that
// is not copied across is lost. The security metadata has its own handling; this
// covers everything else.
func TestCreateStagingLikeCopiesOtherExtendedAttributes(t *testing.T) {
	dir := t.TempDir()
	model := filepath.Join(dir, "_data")
	require.NoError(t, os.Mkdir(model, 0o755))
	staging := model + stagingSuffix

	if err := unix.Lsetxattr(model, "user.app.role", []byte("primary"), 0); err != nil {
		t.Skipf("this filesystem cannot hold a user xattr: %v", err)
	}

	require.NoError(t, createStagingLike(staging, model, quietLogger()))

	value, present, err := readXattr(staging, "user.app.role")
	require.NoError(t, err)
	require.True(t, present, "the attribute the volume carried was dropped")
	assert.Equal(t, "primary", string(value))
}

// An attribute this process is not allowed to write is worth a warning, not a
// refusal to restore: the operator may not know it is there, and failing leaves
// them with no way to restore the volume at all.
func TestCreateStagingLikeWarnsRatherThanFailsOnAnUnwritableAttribute(t *testing.T) {
	dir := t.TempDir()
	model := filepath.Join(dir, "_data")
	require.NoError(t, os.Mkdir(model, 0o755))
	staging := model + stagingSuffix

	restore := swapXattrSeams(t,
		func(string) ([]string, error) { return []string{"trusted.pinned"}, nil },
		func(_, attr string) ([]byte, bool, error) { return []byte("v"), attr == "trusted.pinned", nil },
		func(_, attr string, _ []byte, _ int) error {
			if attr == "trusted.pinned" {
				return unix.EPERM
			}
			return nil
		})
	defer restore()

	logs, logger := capturedWarnLogger()
	require.NoError(t, createStagingLike(staging, model, logger))
	assert.Contains(t, logs.String(), "trusted.pinned")
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

// A volume whose _data is a symlink is resolved so the swap replaces the real
// directory rather than the link. When the backing directory is renamed and the
// process dies before the second rename, the link is left dangling, and
// EvalSymlinks cannot resolve it at all: recovery has to find the displaced
// copy beside the directory the link names, not beside the link.
func TestResolveVolumeDataResolvesThroughADanglingLink(t *testing.T) {
	root := t.TempDir()
	backing := filepath.Join(root, "backing")
	require.NoError(t, os.Mkdir(backing, 0o755))
	volume := filepath.Join(root, "volumes", "myvol")
	require.NoError(t, os.MkdirAll(volume, 0o755))

	data := filepath.Join(volume, "_data")
	target := filepath.Join(backing, "v")
	require.NoError(t, os.Mkdir(target, 0o755))
	require.NoError(t, os.Symlink(target, data))

	resolved, err := resolveVolumeData(data)
	require.NoError(t, err)
	assert.Equal(t, target, resolved, "an intact link resolves to what it names")

	// The state an interrupted rename-pair swap leaves behind.
	require.NoError(t, os.Rename(target, target+oldSuffix))
	require.NoFileExists(t, target)

	resolved, err = resolveVolumeData(data)
	require.NoError(t, err, "a dangling final component is the state recovery exists to repair")
	assert.Equal(t, target, resolved)

	require.NoError(t, recoverInterruptedSwap(resolved, quietLogger()))
	assert.DirExists(t, target, "the displaced backing directory was put back")
	assert.NoDirExists(t, target+oldSuffix)
}

// The wipe guard asks whether this is a container volume. Resolving the
// volume's own symlink must not answer no: the backing directory is legitimately
// outside the volumes root, and refusing it makes such a volume impossible to
// mirror-restore.
func TestEmptyVolumeDataJudgesTheVolumeNotItsBackingDirectory(t *testing.T) {
	root := t.TempDir()
	backing := filepath.Join(root, "mnt", "v") // no "/volumes/" component
	require.NoError(t, os.MkdirAll(backing, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(backing, "a.txt"), []byte("a"), 0o600))
	identity := filepath.Join(root, "volumes", "myvol", "_data")

	require.NoError(t, emptyVolumeData(backing, identity))

	entries, err := os.ReadDir(backing)
	require.NoError(t, err)
	assert.Empty(t, entries, "the backing directory was emptied")

	err = emptyVolumeData(backing, filepath.Join(root, "elsewhere", "_data"))
	require.Error(t, err, "a target that is not a container volume is still refused")
	assert.Contains(t, err.Error(), "not a recognizable container volume")
}

// Two retentions of the same volume inside one second used to pick the same
// name. Renaming onto a non-empty retained copy fails, which put the second one
// back on the staging path a retry deletes; renaming onto an empty one silently
// replaced it. Both copies must survive, distinctly.
func TestRetainOutOfTheWayDoesNotCollideWithinASecond(t *testing.T) {
	data := liveVolume(t)

	first := data + stagingSuffix
	require.NoError(t, os.Mkdir(first, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(first, "first.txt"), []byte("first"), 0o644))
	keptFirst, err := retainOutOfTheWay(first, data)
	require.NoError(t, err)

	second := data + stagingSuffix
	require.NoError(t, os.Mkdir(second, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(second, "second.txt"), []byte("second"), 0o644))
	keptSecond, err := retainOutOfTheWay(second, data)
	require.NoError(t, err, "a second retention in the same second must not fail")

	assert.NotEqual(t, keptFirst, keptSecond, "the two retained copies must not share a name")
	assert.Equal(t, []string{"first.txt"}, names(t, keptFirst), "the first copy was not overwritten")
	assert.Equal(t, []string{"second.txt"}, names(t, keptSecond))
	assert.Len(t, keptDirsFor(t, data), 2, "both copies are kept")
	assert.NoDirExists(t, data+stagingSuffix, "neither is left where a retry clears first")
}

// A retention that cannot complete must not leave its placeholder behind: the
// volumes directory would collect empty kept- directories that look like
// recoverable copies.
func TestRetainOutOfTheWayRemovesItsPlaceholderOnFailure(t *testing.T) {
	data := liveVolume(t)

	_, err := retainOutOfTheWay(data+stagingSuffix, data) // never created
	require.Error(t, err)
	assert.Empty(t, keptDirsFor(t, data), "no empty placeholder was left behind")
}

// SIGHUP terminates by default and arrives on its own when a controlling
// terminal goes away, so leaving it unhandled killed the manager while
// borgmatic and borg kept writing into the live volume.
//
// Run in a re-exec'd child: signalling this test binary would take the whole
// run down, which is the very failure being guarded against. The child runs a
// fake borgmatic that backgrounds a grandchild, so the assertion covers the
// group sweep and not merely the leader.
func TestExtractForwardsSIGHUPToTheWholeGroup(t *testing.T) {
	// A real file rather than a buffer: a buffer makes exec create a pipe, and
	// the fake borgmatic inherits its write end, so cmd.Wait would block until
	// that process exited on its own. A regression would then read as a hang
	// instead of a failure, which is the shape of bug this test exists to catch.
	logPath := filepath.Join(t.TempDir(), "child.log")
	logFile, err := os.Create(logPath)
	require.NoError(t, err)
	defer func() { _ = logFile.Close() }()

	// #nosec G204 -- re-execs this test binary
	child := exec.Command(os.Args[0], "-test.run=TestExtractSignalHelper")
	child.Env = append(os.Environ(), "BM_EXTRACT_SIGNAL_HELPER=1")
	child.Stdout, child.Stderr = logFile, logFile
	require.NoError(t, child.Start())

	// The child builds its own paths and reports where the grandchild's is,
	// rather than taking them from the environment: a path arriving that way
	// reaches exec.Command as tainted input and trips the command-injection
	// analysis on production code that is not doing anything of the sort.
	grandchild := waitForPID(t, logPath)
	require.NoError(t, child.Process.Signal(syscall.SIGHUP))

	waitErr := child.Wait()
	require.Error(t, waitErr, "the extract must report that it did not finish")
	logged, err := os.ReadFile(logPath)
	require.NoError(t, err)
	assert.Contains(t, string(logged), "hangup", "the reason reaches the operator")

	// The grandchild is what an unhandled SIGHUP used to leave writing into the
	// volume with nothing supervising it.
	assert.Eventually(t, func() bool {
		return syscall.Kill(grandchild, 0) != nil
	}, 10*time.Second, 50*time.Millisecond, "borg was left alive after the manager exited")
}

// TestExtractSignalHelper is the child half of the test above, not a test.
func TestExtractSignalHelper(t *testing.T) {
	if os.Getenv("BM_EXTRACT_SIGNAL_HELPER") != "1" {
		t.Skip("child half of TestExtractForwardsSIGHUPToTheWholeGroup")
	}
	// MkdirTemp rather than t.TempDir: this process leaves via os.Exit, so the
	// registered cleanup would never run anyway, and saying so is clearer than
	// appearing to rely on it.
	dir, err := os.MkdirTemp("", "bm-signal-helper-")
	require.NoError(t, err)

	pidFile := filepath.Join(dir, "grandchild.pid")
	fake := filepath.Join(dir, "borgmatic")
	// The grandchild gets its own stdio, so it does not hold the parent's.
	require.NoError(t, os.WriteFile(fake,
		[]byte("#!/bin/sh\nsleep 60 >/dev/null 2>&1 </dev/null &\necho $! > "+pidFile+"\nsleep 60\n"), 0o755))
	fmt.Println(pidFileMarker + pidFile)

	err = runBorgmaticExtract(context.Background(), fake,
		"config.yaml", "archive", "vol/_data", filepath.Join(dir, "dest"))
	if err != nil {
		fmt.Println("extract returned:", err)
		os.Exit(3)
	}
	os.Exit(0)
}

// pidFileMarker prefixes the line the child uses to tell the parent where it
// put the grandchild's pid.
const pidFileMarker = "GRANDCHILD-PIDFILE: "

// waitForPID reads the child's log for the pid file it announced, then that
// file for the grandchild's pid.
func waitForPID(t *testing.T, logPath string) int {
	t.Helper()
	var pidFile string
	require.Eventually(t, func() bool {
		logged, err := os.ReadFile(logPath)
		if err != nil {
			return false
		}
		for _, line := range strings.Split(string(logged), "\n") {
			if after, found := strings.CutPrefix(line, pidFileMarker); found {
				pidFile = strings.TrimSpace(after)
				return pidFile != ""
			}
		}
		return false
	}, 20*time.Second, 50*time.Millisecond, "the child never announced its pid file")

	var pid int
	require.Eventually(t, func() bool {
		raw, err := os.ReadFile(pidFile)
		if err != nil {
			return false
		}
		pid, err = strconv.Atoi(strings.TrimSpace(string(raw)))
		return err == nil && pid > 0
	}, 20*time.Second, 50*time.Millisecond, "the fake borgmatic never recorded its grandchild")
	return pid
}
