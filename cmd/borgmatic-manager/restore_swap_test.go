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
	"unsafe"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"

	"github.com/lugoues/borgmatic-manager/internal/runtime"
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
	assert.Empty(t, stagingDirsFor(t, data))
	assert.Empty(t, displacedDirsFor(t, data))
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
	assert.Empty(t, stagingDirsFor(t, data), "the partial extract is cleaned up")
}

// borgmatic can exit 0 having matched nothing, for instance when the archive
// predates the volume. Promoting an empty directory would silently wipe it.
func TestRestoreWithSwapRefusesAnEmptyExtract(t *testing.T) {
	data := liveVolume(t)

	err := restoreWithSwap(data, quietLogger(), false, func(string) error { return nil }, nil)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "extracted nothing")
	assert.Equal(t, []string{"original.txt"}, names(t, data), "an empty result never becomes the volume")
	assert.Empty(t, stagingDirsFor(t, data))
}

// A run killed between extract and swap leaves staging behind. The next
// restore must start clean rather than promote a mixture of two restores.
func TestRestoreWithSwapDiscardsLeftoverStaging(t *testing.T) {
	data := liveVolume(t)
	stale := data + stagingPrefix + "2001"
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
	missingStaging := data + stagingPrefix + "4001" // deliberately never created

	_, err := swapByRenamePair(missingStaging, data, quietLogger())

	require.Error(t, err)
	assert.Contains(t, err.Error(), "volume is unchanged")
	require.DirExists(t, data, "the original was put back rather than left displaced")
	assert.Equal(t, []string{"original.txt"}, names(t, data))
	assert.Empty(t, displacedDirsFor(t, data))
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

	staging, err := createStagingLike(model, quietLogger())
	require.NoError(t, err)

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

	_, err := createStagingLike(model, quietLogger())

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

	_, err := createStagingLike(model, quietLogger())
	assert.NoError(t, err)
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

	staging, err := createStagingLike(model, quietLogger())
	require.NoError(t, err)

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
	// The swap moves it, and where it lands depends on whether the filesystem
	// supports an atomic exchange and on whether the failed cleanup then moved
	// it clear of the staging path. Make it writable again wherever it ended up,
	// or TempDir's own cleanup fails.
	t.Cleanup(func() {
		paths := []string{locked}
		for _, dd := range displacedDirsFor(t, data) {
			paths = append(paths, filepath.Join(dd, "locked"))
		}
		for _, sd := range stagingDirsFor(t, data) {
			paths = append(paths, filepath.Join(sd, "locked"))
		}
		for _, kept := range keptDirsFor(t, data) {
			paths = append(paths, filepath.Join(kept, "locked"))
		}
		for _, p := range paths {
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
	displaced := data + oldPrefix + "1001"
	require.NoError(t, os.Rename(data, displaced)) // interrupted between the renames
	markAsOurs(displaced)                          // as the fallback swap does
	require.NoDirExists(t, data)

	require.NoError(t, recoverInterruptedSwap(data, quietLogger()))

	require.DirExists(t, data, "the volume has its data directory back")
	assert.Equal(t, []string{"original.txt"}, names(t, data))
	assert.NoDirExists(t, displaced)
}

func TestRecoverInterruptedSwapLeavesAHealthyVolumeAlone(t *testing.T) {
	data := liveVolume(t)
	// A displaced copy from some earlier run, with the volume perfectly fine.
	displaced := data + oldPrefix + "1001"
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
	assert.Empty(t, stagingDirsFor(t, data))
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

	staging, err := createStagingLike(model, quietLogger())
	require.NoError(t, err)

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

	if err := unix.Lsetxattr(model, "user.app.role", []byte("primary"), 0); err != nil {
		t.Skipf("this filesystem cannot hold a user xattr: %v", err)
	}

	staging, err := createStagingLike(model, quietLogger())
	require.NoError(t, err)

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
	_, err := createStagingLike(model, logger)
	require.NoError(t, err)
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
	assert.Empty(t, stagingDirsFor(t, data), "not left where a retry clears first")

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
	assert.Empty(t, stagingDirsFor(t, data), "a half-built staging directory must not be left on disk")
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
	require.NoError(t, os.Rename(target, target+oldPrefix+"1001"))
	markAsOurs(target + oldPrefix + "1001") // as the fallback swap does
	require.NoFileExists(t, target)

	resolved, err = resolveVolumeData(data)
	require.NoError(t, err, "a dangling final component is the state recovery exists to repair")
	assert.Equal(t, target, resolved)

	require.NoError(t, recoverInterruptedSwap(resolved, quietLogger()))
	assert.DirExists(t, target, "the displaced backing directory was put back")
	assert.Empty(t, displacedDirsFor(t, target))
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

	first := data + stagingPrefix + "3001"
	require.NoError(t, os.Mkdir(first, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(first, "first.txt"), []byte("first"), 0o644))
	keptFirst, err := retainOutOfTheWay(first, data)
	require.NoError(t, err)

	second := data + stagingPrefix + "3002"
	require.NoError(t, os.Mkdir(second, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(second, "second.txt"), []byte("second"), 0o644))
	keptSecond, err := retainOutOfTheWay(second, data)
	require.NoError(t, err, "a second retention in the same second must not fail")

	assert.NotEqual(t, keptFirst, keptSecond, "the two retained copies must not share a name")
	assert.Equal(t, []string{"first.txt"}, names(t, keptFirst), "the first copy was not overwritten")
	assert.Equal(t, []string{"second.txt"}, names(t, keptSecond))
	assert.Len(t, keptDirsFor(t, data), 2, "both copies are kept")
	assert.Empty(t, stagingDirsFor(t, data), "neither is left where a retry clears first")
}

// A retention that cannot complete must not leave its placeholder behind: the
// volumes directory would collect empty kept- directories that look like
// recoverable copies.
func TestRetainOutOfTheWayRemovesItsPlaceholderOnFailure(t *testing.T) {
	data := liveVolume(t)

	_, err := retainOutOfTheWay(data+stagingPrefix+"4001", data) // never created
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
	child.Env = minimalEnv("BM_EXTRACT_SIGNAL_HELPER=1")
	child.Stdout, child.Stderr = logFile, logFile
	require.NoError(t, child.Start())

	// The child builds its own paths and reports where the grandchild's is,
	// rather than taking them from the environment: a path arriving that way
	// reaches exec.Command as tainted input and trips the command-injection
	// analysis on production code that is not doing anything of the sort.
	grandchild := waitForPID(t, logPath)
	require.NoError(t, child.Process.Signal(syscall.SIGHUP))

	require.Error(t, waitBounded(t, child), "the extract must report that it did not finish")
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
	// SIGQUIT's default action is terminate *and dump core*, and a core holds
	// this process's whole address space, environment included. The mutation run
	// that checks this test can fail did exactly that and wrote credentials to
	// disk. Refuse to dump, and see minimalEnv for the other half.
	require.NoError(t, unix.Setrlimit(unix.RLIMIT_CORE, &unix.Rlimit{Cur: 0, Max: 0}))
	// MkdirTemp rather than t.TempDir: this process leaves via os.Exit, so the
	// registered cleanup would never run anyway, and saying so is clearer than
	// appearing to rely on it.
	dir, err := os.MkdirTemp("", "bm-signal-helper-")
	require.NoError(t, err)

	pidFile := filepath.Join(dir, "grandchild.pid")
	fake := filepath.Join(dir, "borgmatic")
	// The grandchild gets its own stdio, so it does not hold the parent's.
	script := "#!/bin/sh\nsleep 60 >/dev/null 2>&1 </dev/null &\necho $! > " + pidFile + "\nsleep 60\n"
	if os.Getenv("BM_FAKE_PROPAGATES_TERM") == "1" {
		// What real borgmatic does: borgmatic/signals.py handles SIGTERM with
		// os.killpg(os.getpgrp(), signal_number), so the signal reaches the borg
		// it spawned rather than stopping at borgmatic. Tests that depend on
		// that reaching borg have to model it, or they are asserting against a
		// stand-in that behaves worse than the real thing.
		script = "#!/bin/sh\ntrap 'kill -TERM -$$ 2>/dev/null; exit 143' TERM\n" +
			"sleep 60 >/dev/null 2>&1 </dev/null &\necho $! > " + pidFile + "\nwait\n"
	}
	require.NoError(t, os.WriteFile(fake, []byte(script), 0o755))
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

// A copy that cannot be moved clear of the staging path still exists, so this
// must not fail the operation. It must not be silent either: the operator is
// about to be pointed at a path the next restore deletes as its first act.
func TestRetainOrWarnReportsAFailureRatherThanHidingIt(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root writes into an unwritable directory anyway")
	}
	data := liveVolume(t)
	staging := data + stagingPrefix + "3001"
	require.NoError(t, os.Mkdir(staging, 0o755))

	// Retention creates its destination beside the target, so a parent that
	// cannot be written to is what makes it fail.
	parent := filepath.Dir(data)
	before, err := os.Stat(parent)
	require.NoError(t, err)
	require.NoError(t, os.Chmod(parent, 0o500))
	defer func() { _ = os.Chmod(parent, before.Mode().Perm()) }()

	logs, logger := capturedWarnLogger()
	got := retainOrWarn(staging, data, logger)

	assert.Equal(t, staging, got, "the caller is pointed at where the copy actually is")
	assert.Contains(t, logs.String(), "later restore may delete it", "the risk is stated")
	assert.DirExists(t, staging, "and the copy itself is still there")
}

// The extract must run with no controlling terminal. That is what stops borg's
// getpass from opening /dev/tty and hanging on SIGTTIN, and it is what lets
// Ctrl-Z stop this process (which the shell is watching) rather than a group
// the shell knows nothing about.
//
// Run under a real pty, because a test binary normally has no controlling
// terminal at all and would pass without proving anything.
func TestExtractRunsWithNoControllingTerminal(t *testing.T) {
	ptmx, err := os.OpenFile("/dev/ptmx", os.O_RDWR, 0)
	if err != nil {
		t.Skipf("no pty available: %v", err)
	}
	defer func() { _ = ptmx.Close() }()

	var n uint32
	if _, _, e := syscall.Syscall(syscall.SYS_IOCTL, ptmx.Fd(), syscall.TIOCGPTN,
		uintptr(unsafe.Pointer(&n))); e != 0 {
		t.Skipf("cannot get pty number: %v", e)
	}
	var unlock int32
	if _, _, e := syscall.Syscall(syscall.SYS_IOCTL, ptmx.Fd(), syscall.TIOCSPTLCK,
		uintptr(unsafe.Pointer(&unlock))); e != 0 {
		t.Skipf("cannot unlock pty: %v", e)
	}
	pts, err := os.OpenFile(fmt.Sprintf("/dev/pts/%d", n), os.O_RDWR, 0)
	require.NoError(t, err)
	defer func() { _ = pts.Close() }()

	// A session leader with the pty as its controlling terminal: what an
	// operator's shell gives the manager.
	// #nosec G204 -- re-execs this test binary
	child := exec.Command(os.Args[0], "-test.run=TestControllingTerminalHelper")
	child.Env = minimalEnv("BM_TTY_HELPER=1")
	child.Stdin, child.Stdout, child.Stderr = pts, pts, pts
	child.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true, Ctty: 0}
	require.NoError(t, child.Start())

	read := make(chan string, 1)
	go func() {
		buf := make([]byte, 4096)
		var seen strings.Builder
		for {
			n, readErr := ptmx.Read(buf)
			if n > 0 {
				seen.Write(buf[:n])
				if strings.Contains(seen.String(), "CHILD-DONE") {
					read <- seen.String()
					return
				}
			}
			if readErr != nil {
				read <- seen.String()
				return
			}
		}
	}()

	// Started before the read so a helper that never reports is still reaped.
	waited := make(chan error, 1)
	go func() { waited <- child.Wait() }()

	var out string
	select {
	case out = <-read:
	case <-time.After(helperGrace):
		_ = child.Process.Kill()
		t.Fatal("the helper never reported")
	}
	select {
	case waitErr := <-waited:
		require.NoError(t, waitErr, "the helper exited badly")
	case <-time.After(helperGrace):
		_ = child.Process.Kill()
		t.Fatal("the helper reported but never exited")
	}

	assert.Contains(t, out, "MANAGER-HAS-TTY=true", "the fixture must give the manager a controlling terminal")
	assert.Contains(t, out, "EXTRACT-HAS-TTY=false", "the extract must not be able to open /dev/tty")
}

// TestControllingTerminalHelper is the child half of the test above, not a test.
func TestControllingTerminalHelper(t *testing.T) {
	if os.Getenv("BM_TTY_HELPER") != "1" {
		t.Skip("child half of TestExtractRunsWithNoControllingTerminal")
	}
	fmt.Printf("MANAGER-HAS-TTY=%v\r\n", canOpenTTY())

	dir, err := os.MkdirTemp("", "bm-tty-helper-")
	require.NoError(t, err)
	fake := filepath.Join(dir, "borgmatic")
	// Reports whether it can reach a controlling terminal, the way borg's
	// getpass would.
	require.NoError(t, os.WriteFile(fake,
		[]byte("#!/bin/sh\nif (: </dev/tty) 2>/dev/null; then echo EXTRACT-HAS-TTY=true; "+
			"else echo EXTRACT-HAS-TTY=false; fi\n"), 0o755))

	_ = runBorgmaticExtract(context.Background(), fake, "config.yaml", "archive", "vol/_data",
		filepath.Join(dir, "dest"))
	fmt.Print("CHILD-DONE\r\n")
	os.Exit(0)
}

func canOpenTTY() bool {
	f, err := os.OpenFile("/dev/tty", os.O_RDONLY, 0)
	if err != nil {
		return false
	}
	_ = f.Close()
	return true
}

// A restore that succeeds but cannot delete the copy it replaced must not leave
// that copy on the staging path. The next restore clears staging as its first
// act and refuses to start if it cannot, so a success today would become the
// reason the next one fails.
func TestRestoreWithSwapDoesNotBlockTheNextRestoreWhenCleanupFails(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root deletes regardless of the directory mode")
	}
	data := liveVolume(t)
	require.NoError(t, os.Mkdir(filepath.Join(data, "sub"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(data, "sub", "pinned.txt"), []byte("x"), 0o644))
	// Unreadable and unwritable contents: RemoveAll cannot empty it.
	require.NoError(t, os.Chmod(filepath.Join(data, "sub"), 0o000))
	t.Cleanup(func() {
		for _, k := range keptDirsFor(t, data) {
			_ = os.Chmod(filepath.Join(k, "sub"), 0o755)
		}
	})

	require.NoError(t, restoreWithSwap(data, quietLogger(), false, func(dest string) error {
		return os.WriteFile(filepath.Join(dest, "restored.txt"), []byte("restored"), 0o644)
	}, nil))
	assert.Equal(t, []string{"restored.txt"}, names(t, data), "the restore itself succeeded")
	assert.Empty(t, stagingDirsFor(t, data), "the undeletable copy was moved off the staging path")
	assert.Len(t, keptDirsFor(t, data), 1, "and kept where the operator can find it")

	// The real assertion: another restore can still run.
	require.NoError(t, restoreWithSwap(data, quietLogger(), false, func(dest string) error {
		return os.WriteFile(filepath.Join(dest, "second.txt"), []byte("second"), 0o644)
	}, nil), "the next restore must not be blocked by the last one's leftovers")
	assert.Equal(t, []string{"second.txt"}, names(t, data))
}

// Ctrl-\ reaches only this process now that the extract has its own session, so
// an unforwarded SIGQUIT kills the manager and leaves borgmatic writing.
func TestExtractForwardsSIGQUITToTheWholeGroup(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "child.log")
	logFile, err := os.Create(logPath)
	require.NoError(t, err)
	defer func() { _ = logFile.Close() }()

	// #nosec G204 -- re-execs this test binary
	child := exec.Command(os.Args[0], "-test.run=TestExtractSignalHelper")
	child.Env = minimalEnv("BM_EXTRACT_SIGNAL_HELPER=1")
	child.Stdout, child.Stderr = logFile, logFile
	require.NoError(t, child.Start())

	grandchild := waitForPID(t, logPath)
	require.NoError(t, child.Process.Signal(syscall.SIGQUIT))

	require.Error(t, waitBounded(t, child), "the extract must report that it did not finish")
	logged, err := os.ReadFile(logPath)
	require.NoError(t, err)
	assert.Contains(t, string(logged), "quit", "the reason reaches the operator")

	assert.Eventually(t, func() bool {
		return syscall.Kill(grandchild, 0) != nil
	}, 10*time.Second, 50*time.Millisecond, "borg was left alive after the manager exited")
}

// minimalEnv builds the environment for a re-exec'd helper from scratch rather
// than inheriting this process's.
//
// These helpers are signalled on purpose, and a signal that dumps core writes
// the whole address space to disk. Whatever is in the parent's environment,
// tokens included, would land in that file next to the source tree. Passing
// only what the helper needs to run means there is nothing there worth writing.
func minimalEnv(extra ...string) []string {
	env := []string{"PATH=" + os.Getenv("PATH")}
	return append(env, extra...)
}

// helperGrace bounds every wait on a re-exec'd helper. It is generous, because
// the point is not speed: it is that a hang fails as a named assertion rather
// than as the suite timeout and a panic dump.
const helperGrace = 20 * time.Second

// waitBounded waits for a signalled helper to exit and fails instead of
// blocking.
//
// A regression in signal forwarding is precisely the case where the helper does
// not exit, so an unbounded Wait makes these tests useless exactly when they
// matter. One of them took two minutes to report a failure before this, because
// Wait was blocked on a pipe the helper's own child still held open.
func waitBounded(t *testing.T, child *exec.Cmd) error {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- child.Wait() }()
	select {
	case err := <-done:
		return err
	case <-time.After(helperGrace):
		_ = child.Process.Kill()
		t.Fatalf("the helper did not exit within %s", helperGrace)
		return nil
	}
}

// A zero-length attribute is present, not absent. user.* attributes are often
// used as bare boolean markers, and reporting them missing dropped them from
// the directory that replaces the volume.
func TestReadXattrReportsAZeroLengthAttributeAsPresent(t *testing.T) {
	dir := t.TempDir()
	if err := unix.Lsetxattr(dir, "user.marker", nil, 0); err != nil {
		t.Skipf("this filesystem cannot hold a user xattr: %v", err)
	}

	value, present, err := readXattr(dir, "user.marker")
	require.NoError(t, err)
	assert.True(t, present, "an attribute with an empty value still exists")
	assert.Empty(t, value)

	_, present, err = readXattr(dir, "user.never-set")
	require.NoError(t, err)
	assert.False(t, present, "and one that was never set is still absent")
}

func TestCreateStagingLikeCarriesAZeroLengthAttribute(t *testing.T) {
	dir := t.TempDir()
	model := filepath.Join(dir, "_data")
	require.NoError(t, os.Mkdir(model, 0o755))
	if err := unix.Lsetxattr(model, "user.marker", nil, 0); err != nil {
		t.Skipf("this filesystem cannot hold a user xattr: %v", err)
	}
	staging, err := createStagingLike(model, quietLogger())
	require.NoError(t, err)

	_, present, err := readXattr(staging, "user.marker")
	require.NoError(t, err)
	assert.True(t, present, "the marker the volume carried was dropped")
}

// The retained copy is only useful if the rename that made it survives a crash.
// Without a flush the namespace can roll back to the staging name while the
// error still points at the kept one, and the next attempt deletes it.
func TestRetainOutOfTheWayFlushesTheRename(t *testing.T) {
	data := liveVolume(t)
	staging := data + stagingPrefix + "3001"
	require.NoError(t, os.Mkdir(staging, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(staging, "kept.txt"), []byte("kept"), 0o644))

	synced := make([]string, 0, 1)
	realSync := syncDirFn
	syncDirFn = func(path string) error {
		synced = append(synced, path)
		return realSync(path)
	}
	defer func() { syncDirFn = realSync }()

	kept, err := retainOutOfTheWay(staging, data)
	require.NoError(t, err)
	assert.Equal(t, []string{filepath.Dir(data)}, synced, "the parent directory was flushed after the rename")
	assert.Equal(t, []string{"kept.txt"}, names(t, kept))
}

// A flush failure must not send the operator to a path with nothing at it: the
// data really has moved by then, it just is not durable yet.
func TestRetainOrWarnStillReportsThePathWhenTheFlushFails(t *testing.T) {
	data := liveVolume(t)
	staging := data + stagingPrefix + "3001"
	require.NoError(t, os.Mkdir(staging, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(staging, "kept.txt"), []byte("kept"), 0o644))

	realSync := syncDirFn
	syncDirFn = func(string) error { return errors.New("disk went away") }
	defer func() { syncDirFn = realSync }()

	logs, logger := capturedWarnLogger()
	kept := retainOrWarn(staging, data, logger)

	assert.NotEqual(t, staging, kept, "the copy really did move, so report where it is")
	assert.Equal(t, []string{"kept.txt"}, names(t, kept), "and it is there")
	assert.Contains(t, logs.String(), "could not be flushed", "the durability gap is reported")
}

// Two restores of one volume derive the same staging path, and the second's
// opening cleanup deletes the first's live extract. The lock is what stops the
// second from starting at all.
func TestLockVolumeRestoreExcludesASecondRestoreOfTheSameVolume(t *testing.T) {
	locks := t.TempDir()
	data := "/var/lib/docker/volumes/myvol/_data"

	first, err := lockVolumeRestore(locks, data)
	require.NoError(t, err)

	_, err = lockVolumeRestore(locks, data)
	require.Error(t, err, "a second restore of the same volume must be refused")
	assert.Contains(t, err.Error(), "another restore is already running")
	assert.Contains(t, err.Error(), "nothing was changed")

	// A different volume is unrelated and must not be blocked.
	other, err := lockVolumeRestore(locks, "/var/lib/docker/volumes/othervol/_data")
	require.NoError(t, err)
	other.Release()

	// And the lock is released for the next run.
	first.Release()
	again, err := lockVolumeRestore(locks, data)
	require.NoError(t, err, "the lock must not outlive the restore that held it")
	again.Release()
}

// Keyed on the resolved directory, not the name used to reach it: --into and a
// symlinked _data both let two invocations mean the same directory.
func TestLockVolumeRestoreKeysOnTheResolvedTarget(t *testing.T) {
	locks := t.TempDir()
	held, err := lockVolumeRestore(locks, "/srv/backing/v")
	require.NoError(t, err)
	defer held.Release()

	_, err = lockVolumeRestore(locks, "/srv/backing/v")
	require.Error(t, err, "the same resolved path is the same lock")
}

// Ctrl-Z has to stop the restore, not just the process the shell watches. With
// the extract in its own session the terminal cannot reach it, so an
// unforwarded SIGTSTP suspends the manager while borgmatic keeps writing: the
// shell reports the command stopped while a merge or forced in-place restore
// carries on modifying the live volume.
func TestExtractIsSuspendedAndResumedWithTheManager(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "child.log")
	logFile, err := os.Create(logPath)
	require.NoError(t, err)
	defer func() { _ = logFile.Close() }()

	// #nosec G204 -- re-execs this test binary
	child := exec.Command(os.Args[0], "-test.run=TestExtractSignalHelper")
	child.Env = minimalEnv("BM_EXTRACT_SIGNAL_HELPER=1")
	child.Stdout, child.Stderr = logFile, logFile
	require.NoError(t, child.Start())
	t.Cleanup(func() {
		_ = child.Process.Signal(syscall.SIGCONT)
		_ = child.Process.Kill()
		reapBounded(t, child)
	})

	grandchild := waitForPID(t, logPath)
	require.NoError(t, child.Process.Signal(syscall.SIGTSTP))

	// The manager stops, which is what makes the shell notice at all.
	requireProcState(t, child.Process.Pid, "T", "the manager did not stop")
	// And the extract stops with it, which is the part that was missing.
	requireProcState(t, grandchild, "T", "borgmatic kept running while the restore was suspended")

	require.NoError(t, child.Process.Signal(syscall.SIGCONT))
	requireProcStateNot(t, child.Process.Pid, "T", "the manager did not resume")
	requireProcStateNot(t, grandchild, "T", "borgmatic was left stopped after the restore resumed")
}

// procState reads the single-letter run state from /proc/<pid>/stat. The comm
// field can contain spaces and parentheses, so the scan starts after the last
// ')' rather than splitting the whole line.
func procState(pid int) (string, error) {
	raw, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return "", err
	}
	end := strings.LastIndexByte(string(raw), ')')
	if end < 0 || end+2 >= len(raw) {
		return "", fmt.Errorf("cannot parse /proc/%d/stat", pid)
	}
	fields := strings.Fields(string(raw[end+2:]))
	if len(fields) == 0 {
		return "", fmt.Errorf("no state in /proc/%d/stat", pid)
	}
	return fields[0], nil
}

func requireProcState(t *testing.T, pid int, want, msg string) {
	t.Helper()
	require.Eventually(t, func() bool {
		got, err := procState(pid)
		return err == nil && got == want
	}, helperGrace, 20*time.Millisecond, msg)
}

func requireProcStateNot(t *testing.T, pid int, unwanted, msg string) {
	t.Helper()
	require.Eventually(t, func() bool {
		got, err := procState(pid)
		return err == nil && got != unwanted
	}, helperGrace, 20*time.Millisecond, msg)
}

// The consequence of a false positive here is that an ordinary volume takes the
// destructive in-place path for no reason, so the check has to be certain
// before it says yes. An unencrypted directory, and a filesystem that does not
// report on encryption at all, must both come back false without an error.
func TestIsEncryptedDirIsFalseForAnOrdinaryDirectory(t *testing.T) {
	for name, path := range map[string]string{
		"temp dir":       t.TempDir(),
		"its parent":     filepath.Dir(t.TempDir()),
		"a volume _data": liveVolume(t),
	} {
		blocks, reason, err := encryptionBlocksStaging(path)
		require.NoError(t, err, "%s: reporting on encryption must not fail", name)
		assert.False(t, blocks, "%s: an unencrypted directory must never be pushed off the staged path", name)
		assert.Empty(t, reason, "%s: and there is nothing to explain", name)
	}
}

func TestIsEncryptedDirReportsAMissingPath(t *testing.T) {
	_, _, err := encryptionBlocksStaging(filepath.Join(t.TempDir(), "nope"))
	require.Error(t, err, "a path that cannot be inspected is not the same as one that is unencrypted")
}

// stagingDirsFor lists the staging directories beside a volume's data path.
// Their names are unique per run, so tests look them up rather than compute
// them.
func stagingDirsFor(t *testing.T, data string) []string {
	t.Helper()
	matches, _, err := siblingsWithPrefix(data, stagingPrefix)
	require.NoError(t, err)
	return matches
}

// Clearing leftovers is the first thing a restore does, and against a fixed
// name that was an unconditional recursive delete of whatever happened to hold
// it. Nothing marked such a directory as this tool's, and for a symlink-backed
// volume it can sit in a directory an application owns.
func TestRestoreWithSwapDoesNotDeleteAnUnrelatedSibling(t *testing.T) {
	data := liveVolume(t)
	// The name the old fixed-suffix scheme would have claimed, plus a couple of
	// near misses that share the prefix's opening.
	bystanders := []string{
		data + ".borgmatic-manager-restoring",
		data + ".backup",
		filepath.Join(filepath.Dir(data), "_data-old"),
	}
	for _, b := range bystanders {
		require.NoError(t, os.Mkdir(b, 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(b, "someone-elses.txt"), []byte("keep"), 0o644))
	}

	require.NoError(t, restoreWithSwap(data, quietLogger(), false, func(dest string) error {
		return os.WriteFile(filepath.Join(dest, "restored.txt"), []byte("restored"), 0o644)
	}, nil))

	assert.Equal(t, []string{"restored.txt"}, names(t, data), "the restore still happened")
	for _, b := range bystanders {
		assert.FileExists(t, filepath.Join(b, "someone-elses.txt"), "%s was deleted by a restore that did not create it", b)
	}
}

// Staging directories from a run that died are this tool's own, and the
// per-volume lock means no live run holds one, so they are cleared.
func TestRestoreWithSwapClearsItsOwnLeftovers(t *testing.T) {
	data := liveVolume(t)
	stale := data + stagingPrefix + "2002"
	require.NoError(t, os.Mkdir(stale, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(stale, "partial.txt"), []byte("partial"), 0o644))
	markAsOurs(stale) // it stands in for one this tool created

	require.NoError(t, restoreWithSwap(data, quietLogger(), false, func(dest string) error {
		return os.WriteFile(filepath.Join(dest, "restored.txt"), []byte("restored"), 0o644)
	}, nil))

	assert.NoDirExists(t, stale, "a leftover from a dead run is this tool's to clear")
	assert.Empty(t, stagingDirsFor(t, data), "and nothing new is left behind")
}

// Each run gets its own staging name, so two runs can never reason about the
// same directory.
func TestStagingNamesAreUniquePerRun(t *testing.T) {
	dir := t.TempDir()
	model := filepath.Join(dir, "_data")
	require.NoError(t, os.Mkdir(model, 0o755))

	first, err := createStagingDir(model, quietLogger())
	require.NoError(t, err)
	second, err := createStagingDir(model, quietLogger())
	require.NoError(t, err)

	assert.NotEqual(t, first, second)
	for _, p := range []string{first, second} {
		assert.True(t, strings.HasPrefix(filepath.Base(p), filepath.Base(model)+stagingPrefix),
			"%s must be recognisable as this tool's staging directory", p)
	}
}

// A _data that resolves to something other than a directory is a malformed
// volume, and both swap paths would replace that node with the staged
// directory: the file would be deleted by a restore reporting success.
func TestCheckVolumeDataDirRejectsANonDirectory(t *testing.T) {
	dir := t.TempDir()

	good := filepath.Join(dir, "_data")
	require.NoError(t, os.Mkdir(good, 0o755))
	require.NoError(t, checkVolumeDataDir(good, "myvol"))

	file := filepath.Join(dir, "afile")
	require.NoError(t, os.WriteFile(file, []byte("x"), 0o644))
	err := checkVolumeDataDir(file, "myvol")
	require.Error(t, err, "a regular file must be refused")
	assert.Contains(t, err.Error(), "not a directory")
	assert.Contains(t, err.Error(), "nothing was changed")

	// The realistic shape: _data is a symlink to a file, which resolution
	// follows and the old existence-only check accepted.
	link := filepath.Join(dir, "linked")
	require.NoError(t, os.Symlink(file, link))
	require.Error(t, checkVolumeDataDir(link, "myvol"), "a symlink to a file must be refused too")

	require.Error(t, checkVolumeDataDir(filepath.Join(dir, "missing"), "myvol"), "and a missing one still is")
}

// SIGKILL runs no handler, so forwarding cannot help here: the extract has to
// be tied to this process by the kernel. Without that a manager that is killed
// or OOM-killed leaves borgmatic and borg running, and the per-volume lock dies
// with the manager, so the next attempt starts while the orphan is still
// writing.
func TestExtractDiesWithAKilledManager(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "child.log")
	logFile, err := os.Create(logPath)
	require.NoError(t, err)
	defer func() { _ = logFile.Close() }()

	// #nosec G204 -- re-execs this test binary
	child := exec.Command(os.Args[0], "-test.run=TestExtractSignalHelper")
	child.Env = minimalEnv("BM_EXTRACT_SIGNAL_HELPER=1", "BM_FAKE_PROPAGATES_TERM=1")
	child.Stdout, child.Stderr = logFile, logFile
	require.NoError(t, child.Start())

	grandchild := waitForPID(t, logPath)
	leader, err := parentOf(grandchild)
	require.NoError(t, err, "the fake borgmatic must be running to be orphaned")

	// No handler runs, and nothing is forwarded.
	require.NoError(t, child.Process.Kill())
	reapBounded(t, child)

	// The kernel guarantees this one: Pdeathsig is delivered to the direct child
	// and needs no cooperation from it.
	assert.Eventually(t, func() bool {
		return syscall.Kill(leader, 0) != nil
	}, helperGrace, 20*time.Millisecond, "borgmatic outlived the manager that was killed")
	// This one runs through borgmatic, which propagates SIGTERM to its process
	// group. Pdeathsig is per-process and cannot reach borg on its own, so this
	// asserts the contract with borgmatic rather than a property of the kernel.
	assert.Eventually(t, func() bool {
		return syscall.Kill(grandchild, 0) != nil
	}, helperGrace, 20*time.Millisecond, "borg outlived the manager that was killed")
}

// parentOf reads a process's ppid from /proc, so the test can name the
// borgmatic between the manager and the borg it spawned.
func parentOf(pid int) (int, error) {
	raw, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return 0, err
	}
	end := strings.LastIndexByte(string(raw), ')')
	if end < 0 || end+2 >= len(raw) {
		return 0, fmt.Errorf("cannot parse /proc/%d/stat", pid)
	}
	fields := strings.Fields(string(raw[end+2:]))
	if len(fields) < 2 {
		return 0, fmt.Errorf("no ppid in /proc/%d/stat", pid)
	}
	return strconv.Atoi(fields[1])
}

// A project id is inode state reached by ioctl, so none of the copying carries
// it and the replacement inherits the parent's instead. The warning is all this
// promises, so it has to be accurate in both directions: silent when the
// replacement lands in the same project, specific when it does not.
func TestProjectQuotaWarningComparesTheReplacementWithTheVolume(t *testing.T) {
	dir := t.TempDir()
	model := filepath.Join(dir, "_data")
	require.NoError(t, os.Mkdir(model, 0o755))
	staging := filepath.Join(dir, "staging")
	require.NoError(t, os.Mkdir(staging, 0o755))

	realID := projectQuotaID
	defer func() { projectQuotaID = realID }()

	for name, tc := range map[string]struct {
		volume, replacement uint64
		known               bool
		wantWarn            bool
	}{
		"the same project":          {volume: 4242, replacement: 4242, known: true},
		"escaping a quota":          {volume: 4242, replacement: 0, known: true, wantWarn: true},
		"inheriting someone else's": {volume: 0, replacement: 77, known: true, wantWarn: true},
		"neither is under a quota":  {volume: 0, replacement: 0, known: true},
		"the filesystem cannot say": {volume: 4242, replacement: 0, known: false},
	} {
		t.Run(name, func(t *testing.T) {
			projectQuotaID = func(path string) (uint64, bool) {
				if path == model {
					return tc.volume, tc.known
				}
				return tc.replacement, tc.known
			}
			logs, logger := capturedWarnLogger()
			warnAboutProjectQuota(model, staging, logger)
			if tc.wantWarn {
				assert.Contains(t, logs.String(), "project quota")
			} else {
				assert.Empty(t, logs.String())
			}
		})
	}
}

// The reader itself, against whatever this host provides. An ordinary directory
// must not look like it is under a quota, because a false positive here is a
// warning that sends an operator looking for a quota that never existed.
func TestProjectQuotaIDDoesNotInventOneForAnOrdinaryDirectory(t *testing.T) {
	id, known := projectQuotaID(t.TempDir())
	if !known {
		t.Skip("this filesystem does not report project ids")
	}
	assert.Zero(t, id, "an ordinary directory is not under a project quota")
}

// Recovery is reported to the operator as "the volume has been put back", and
// this restore can still fail for a dozen reasons before it reaches a swap of
// its own. An unflushed recovery can be rolled back by a power loss after that
// report, leaving _data missing again with nothing left explaining why.
func TestRecoverInterruptedSwapFlushesTheRename(t *testing.T) {
	data := liveVolume(t)
	require.NoError(t, os.Rename(data, data+oldPrefix+"1001")) // interrupted between the renames
	markAsOurs(data + oldPrefix + "1001")                      // as the fallback swap does

	synced := make([]string, 0, 1)
	realSync := syncDirFn
	syncDirFn = func(path string) error {
		synced = append(synced, path)
		return realSync(path)
	}
	defer func() { syncDirFn = realSync }()

	require.NoError(t, recoverInterruptedSwap(data, quietLogger()))
	assert.Equal(t, []string{filepath.Dir(data)}, synced, "the parent was flushed after putting the data back")
	assert.Equal(t, []string{"original.txt"}, names(t, data))
}

// A recovery that cannot be made durable must be reported, not reported as
// success: the operator would otherwise be told the volume is back when a power
// loss can still take it away again.
func TestRecoverInterruptedSwapReportsAFlushFailure(t *testing.T) {
	data := liveVolume(t)
	require.NoError(t, os.Rename(data, data+oldPrefix+"1001"))
	markAsOurs(data + oldPrefix + "1001") // as the fallback swap does

	realSync := syncDirFn
	syncDirFn = func(string) error { return errors.New("disk went away") }
	defer func() { syncDirFn = realSync }()

	err := recoverInterruptedSwap(data, quietLogger())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "may not survive a power loss")
	assert.DirExists(t, data, "the data really was put back, which the message has to reflect")
}

// displacedDirsFor lists the directories a rename-pair swap displaced beside a
// volume's data path. Unique per run, so tests look them up.
func displacedDirsFor(t *testing.T, data string) []string {
	t.Helper()
	matches, _, err := siblingsWithPrefix(data, oldPrefix)
	require.NoError(t, err)
	return matches
}

// The fallback swap used to clear a fixed displaced name before renaming onto
// it, which recursively deleted whatever was there. For a symlink-backed volume
// the resolved target sits in a directory an application owns, so a restore
// could erase unrelated data before it had touched the volume at all.
func TestSwapByRenamePairDoesNotDeleteAnUnrelatedSibling(t *testing.T) {
	data := liveVolume(t)
	bystander := data + ".borgmatic-manager-replaced" // the name the old scheme claimed
	require.NoError(t, os.Mkdir(bystander, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(bystander, "someone-elses.txt"), []byte("keep"), 0o644))

	staging, err := createStagingLike(data, quietLogger())
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(staging, "restored.txt"), []byte("restored"), 0o644))

	displaced, err := swapByRenamePair(staging, data, quietLogger())
	require.NoError(t, err)

	assert.Equal(t, []string{"restored.txt"}, names(t, data), "the swap still happened")
	assert.Equal(t, []string{"original.txt"}, names(t, displaced), "and the old data is where it says")
	assert.FileExists(t, filepath.Join(bystander, "someone-elses.txt"),
		"a directory this restore did not create was deleted")
}

// Recovery has to find a displaced copy whose name it no longer knows in
// advance, and must not guess when there is more than one.
func TestRecoverInterruptedSwapFindsAUniquelyNamedDisplacedCopy(t *testing.T) {
	data := liveVolume(t)
	displaced := data + oldPrefix + "6001"
	require.NoError(t, os.Rename(data, displaced))
	markAsOurs(displaced) // as the fallback swap does

	require.NoError(t, recoverInterruptedSwap(data, quietLogger()))
	require.DirExists(t, data)
	assert.Equal(t, []string{"original.txt"}, names(t, data))
	assert.Empty(t, displacedDirsFor(t, data))
}

func TestRecoverInterruptedSwapRefusesToGuessBetweenTwo(t *testing.T) {
	data := liveVolume(t)
	require.NoError(t, os.Rename(data, data+oldPrefix+"7001"))
	markAsOurs(data + oldPrefix + "7001") // as the fallback swap does
	// A second one that also holds data: two real candidates, which is the only
	// genuinely ambiguous case.
	second := data + oldPrefix + "7002"
	require.NoError(t, os.Mkdir(second, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(second, "also-data.txt"), []byte("data"), 0o644))
	markAsOurs(second) // both are provably this tool's, which is what makes it ambiguous

	err := recoverInterruptedSwap(data, quietLogger())
	require.Error(t, err, "picking one of two copies of a volume's data is not this program's call")
	assert.Contains(t, err.Error(), "by hand")
	assert.NoDirExists(t, data, "and nothing was moved")
}

// Staging an encrypted directory replaces it with plaintext and reports
// success. Restoring in place unnecessarily only gives up the safety net. The
// two are not symmetric, so anything undetermined must block staging.
func TestEncryptionBlocksStagingWhenItCannotBeDetermined(t *testing.T) {
	dir := t.TempDir()
	blocks, reason, err := encryptionBlocksStaging(dir)
	require.NoError(t, err)
	require.False(t, blocks, "precondition: this host can answer the question")
	require.Empty(t, reason)

	// A kernel without statx, which is where fscrypt can exist unseen: fscrypt
	// predates statx, so this is not hypothetical.
	realStatx := statxFn
	statxFn = func(int, string, int, int, *unix.Statx_t) error { return unix.ENOSYS }
	defer func() { statxFn = realStatx }()

	blocks, reason, err = encryptionBlocksStaging(dir)
	require.NoError(t, err, "an unanswerable question is not an error, it is an answer of its own")
	assert.True(t, blocks, "unknown must not be read as unencrypted")
	assert.Contains(t, reason, "cannot be determined")
}

// reapBounded reaps a helper that has just been killed, without waiting forever
// for it.
//
// SIGKILL cannot be blocked, so this normally returns at once; a process stuck
// in uninterruptible sleep is the case worth bounding. It exists for the same
// reason as waitBounded, and separately from it because these callers have
// already killed the process and want it reaped rather than judged: a wait that
// blocks here would still turn a stuck helper into the suite timeout.
func reapBounded(t *testing.T, child *exec.Cmd) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		_, _ = child.Process.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(helperGrace):
		t.Errorf("a killed helper was not reaped within %s", helperGrace)
	}
}

// The target path is not a pattern this program chose: a symlink-backed volume
// resolves to whatever the link names. A literal bracket in that path made
// every staged restore fail, and a wildcard could match a different volume's
// live staging directory and have it deleted.
func TestRestoreWithSwapHandlesGlobCharactersInTheTargetPath(t *testing.T) {
	for _, name := range []string{"weird[name", "star*name", "question?name"} {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			data := filepath.Join(root, "volumes", name, "_data")
			require.NoError(t, os.MkdirAll(data, 0o755))
			require.NoError(t, os.WriteFile(filepath.Join(data, "original.txt"), []byte("original"), 0o644))

			require.NoError(t, restoreWithSwap(data, quietLogger(), false, func(dest string) error {
				return os.WriteFile(filepath.Join(dest, "restored.txt"), []byte("restored"), 0o644)
			}, nil))
			assert.Equal(t, []string{"restored.txt"}, names(t, data))
			assert.Empty(t, stagingDirsFor(t, data))
		})
	}
}

// A wildcard in one volume's path must not reach another volume's staging
// directory, which is the dangerous half of the same defect.
func TestSiblingsWithPrefixDoesNotMatchAcrossVolumes(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.MkdirAll(root, 0o755))
	greedy := filepath.Join(root, "_data*")
	other := filepath.Join(root, "_data-other")
	require.NoError(t, os.Mkdir(other+stagingPrefix+"9002", 0o755))

	matches, _, err := siblingsWithPrefix(greedy, stagingPrefix)
	require.NoError(t, err)
	assert.Empty(t, matches, "a wildcard in one volume's name must not reach another volume's staging directory")
}

// _data -> /mnt/link -> /real/data, interrupted after /real/data was moved
// aside. Stopping at the first link leaves recovery looking beside the wrong
// directory and the volume unrecoverable.
func TestResolveVolumeDataFollowsAChainOfDanglingLinks(t *testing.T) {
	root := t.TempDir()
	backing := filepath.Join(root, "backing")
	require.NoError(t, os.Mkdir(backing, 0o755))
	real := filepath.Join(backing, "data")
	require.NoError(t, os.Mkdir(real, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(real, "original.txt"), []byte("original"), 0o644))

	mid := filepath.Join(root, "link")
	require.NoError(t, os.Symlink(real, mid))
	volume := filepath.Join(root, "volumes", "myvol")
	require.NoError(t, os.MkdirAll(volume, 0o755))
	data := filepath.Join(volume, "_data")
	require.NoError(t, os.Symlink(mid, data))

	resolved, err := resolveVolumeData(data)
	require.NoError(t, err)
	assert.Equal(t, real, resolved, "an intact chain resolves to the end of it")

	// Interrupted mid-swap: the real directory is displaced, both links remain.
	displaced := real + oldPrefix + "1001"
	require.NoError(t, os.Rename(real, displaced))
	markAsOurs(displaced) // as the fallback swap does

	resolved, err = resolveVolumeData(data)
	require.NoError(t, err, "a dangling end of the chain is the state recovery exists to repair")
	assert.Equal(t, real, resolved, "the chain must be followed past the intermediate link")

	require.NoError(t, recoverInterruptedSwap(resolved, quietLogger()))
	assert.DirExists(t, real)
	assert.Equal(t, []string{"original.txt"}, names(t, real))
}

func TestResolveVolumeDataRefusesASymlinkLoop(t *testing.T) {
	root := t.TempDir()
	a, b := filepath.Join(root, "a"), filepath.Join(root, "b")
	require.NoError(t, os.Symlink(b, a))
	require.NoError(t, os.Symlink(a, b))

	_, err := resolveVolumeData(a)
	require.Error(t, err, "a loop must terminate rather than spin")
	assert.Contains(t, err.Error(), "loop")
}

// Staging is a sibling, so it needs to create an entry in the parent. The old
// in-place restore only wrote inside _data, so a volume with an unwritable
// parent was restorable before and must not stop being so.
func TestCanCreateSiblingReportsAnUnwritableParent(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root writes into an unwritable directory anyway")
	}
	data := liveVolume(t)
	ok, err := canCreateSibling(data)
	require.NoError(t, err)
	assert.True(t, ok, "an ordinary volume can be staged beside")

	parent := filepath.Dir(data)
	before, err := os.Stat(parent)
	require.NoError(t, err)
	require.NoError(t, os.Chmod(parent, 0o555))
	defer func() { _ = os.Chmod(parent, before.Mode().Perm()) }()

	ok, err = canCreateSibling(data)
	require.NoError(t, err, "an unwritable parent is an answer, not a failure")
	assert.False(t, ok)
}

func TestCanCreateSiblingLeavesNothingBehind(t *testing.T) {
	data := liveVolume(t)
	ok, err := canCreateSibling(data)
	require.NoError(t, err)
	require.True(t, ok)
	assert.Empty(t, stagingDirsFor(t, data), "the probe must not survive itself")
}

// The fallback reserves a name and then renames onto it. A process that dies in
// between leaves an empty reservation, and the volume is untouched. Left in
// place they accumulate, and a later genuine interruption then produces two
// matches and a refusal to choose, turning a recoverable volume into a manual
// job.
func TestRecoverInterruptedSwapReapsAbandonedReservations(t *testing.T) {
	data := liveVolume(t)
	stale := data + oldPrefix + "8001"
	require.NoError(t, os.Mkdir(stale, 0o755))
	markAsOurs(stale) // it stands in for a reservation this tool made

	require.NoError(t, recoverInterruptedSwap(data, quietLogger()))
	assert.NoDirExists(t, stale, "an empty reservation beside an intact volume is rubbish")
	assert.Equal(t, []string{"original.txt"}, names(t, data), "and the volume is untouched")

	// The scenario the reaping exists to prevent: a later interruption must
	// still be recoverable rather than ambiguous.
	require.NoError(t, os.Rename(data, data+oldPrefix+"8002"))
	markAsOurs(data + oldPrefix + "8002") // as the fallback swap does
	require.NoError(t, recoverInterruptedSwap(data, quietLogger()))
	assert.Equal(t, []string{"original.txt"}, names(t, data), "the volume came back")
	assert.Empty(t, displacedDirsFor(t, data))
}

// A directory beside an intact volume that holds data is not this function's to
// delete, and not the operator's to discover months later.
func TestRecoverInterruptedSwapKeepsAndReportsDisplacedDataBesideALiveVolume(t *testing.T) {
	data := liveVolume(t)
	leftover := data + oldPrefix + "9001"
	require.NoError(t, os.Mkdir(leftover, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(leftover, "something.txt"), []byte("x"), 0o644))
	if !markAsOurs(leftover) {
		t.Skip("this filesystem cannot hold the marker")
	}

	logs, logger := capturedWarnLogger()
	require.NoError(t, recoverInterruptedSwap(data, logger))
	assert.DirExists(t, leftover, "data is never removed by recovery")
	assert.Contains(t, logs.String(), "holds data")
}

// A volume is allowed to be empty, so an empty displaced copy is the volume
// when the target is gone. Reaping it because it looks like a reservation would
// throw away the very directory recovery exists to put back.
func TestRecoverInterruptedSwapRestoresAnEmptyVolume(t *testing.T) {
	root := t.TempDir()
	data := filepath.Join(root, "volumes", "myvol", "_data")
	require.NoError(t, os.MkdirAll(data, 0o755))
	require.NoError(t, os.Rename(data, data+oldPrefix+"1001"))
	markAsOurs(data + oldPrefix + "1001") // as the fallback swap does

	require.NoError(t, recoverInterruptedSwap(data, quietLogger()))
	require.DirExists(t, data, "an empty volume is still a volume")
	assert.Empty(t, names(t, data))
	assert.Empty(t, displacedDirsFor(t, data))
}

// One real copy plus an older abandoned reservation is not ambiguous: the
// reservation is provably not the volume.
func TestRecoverInterruptedSwapIgnoresAReservationAlongsideRealData(t *testing.T) {
	data := liveVolume(t)
	abandoned := data + oldPrefix + "8001"
	require.NoError(t, os.Mkdir(abandoned, 0o755))
	markAsOurs(abandoned)
	require.NoError(t, os.Rename(data, data+oldPrefix+"8002"))
	markAsOurs(data + oldPrefix + "8002") // as the fallback swap does

	require.NoError(t, recoverInterruptedSwap(data, quietLogger()))
	assert.Equal(t, []string{"original.txt"}, names(t, data))
	assert.Empty(t, displacedDirsFor(t, data), "and the reservation went with it")
}

// The prefix is public and predictable, so matching on it alone means cleanup
// deletes anything an application happens to name that way. Only the exact
// shape MkdirTemp produces is provably this tool's.
func TestOnlyMkdirTempShapedNamesCountAsOurs(t *testing.T) {
	const prefix = "_data.borgmatic-manager-restoring-"
	for name, want := range map[string]bool{
		"_data.borgmatic-manager-restoring-123456": true,
		"_data.borgmatic-manager-restoring-0":      true,
		"_data.borgmatic-manager-restoring-cache":  false,
		"_data.borgmatic-manager-restoring-":       false,
		"_data.borgmatic-manager-restoring-12a":    false,
		"_data.borgmatic-manager-restoring-1-2":    false,
		"_data.borgmatic-manager-restoring":        false,
		"something-else":                           false,
	} {
		assert.Equal(t, want, madeByMkdirTemp(name, prefix), "%s", name)
	}
}

func TestRestoreWithSwapDoesNotDeleteALookalikeSibling(t *testing.T) {
	data := liveVolume(t)
	lookalike := data + stagingPrefix + "cache" // shares the prefix, not the shape
	require.NoError(t, os.Mkdir(lookalike, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(lookalike, "someone-elses.txt"), []byte("keep"), 0o644))

	require.NoError(t, restoreWithSwap(data, quietLogger(), false, func(dest string) error {
		return os.WriteFile(filepath.Join(dest, "restored.txt"), []byte("restored"), 0o644)
	}, nil))

	assert.Equal(t, []string{"restored.txt"}, names(t, data), "the restore still happened")
	assert.FileExists(t, filepath.Join(lookalike, "someone-elses.txt"),
		"a directory sharing the prefix but not the shape was deleted")
}

// Write and search without read is a real mode, and the in-place restore needed
// no permission to enumerate the parent at all. Not being able to look must not
// stop a restore that does not depend on looking.
func TestSiblingsWithPrefixReportsAnUnlistableParentRatherThanFailing(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads regardless of the directory mode")
	}
	data := liveVolume(t)
	parent := filepath.Dir(data)
	before, err := os.Stat(parent)
	require.NoError(t, err)
	require.NoError(t, os.Chmod(parent, 0o300)) // write + search, no read
	defer func() { _ = os.Chmod(parent, before.Mode().Perm()) }()

	matches, enumerable, err := siblingsWithPrefix(data, stagingPrefix)
	require.NoError(t, err, "an unlistable parent is an answer, not a failure")
	assert.False(t, enumerable)
	assert.Empty(t, matches)

	// And the callers carry on rather than aborting the run.
	require.NoError(t, recoverInterruptedSwap(data, quietLogger()))
	require.NoError(t, clearStagingLeftovers(data, quietLogger()))
}

// A default ACL on the parent lands on staging by inheritance. Copying only
// present values would promote a root granting access the volume never granted,
// and passing it to everything created under it afterwards.
func TestCreateStagingLikeRemovesAnAclTheVolumeDoesNotHave(t *testing.T) {
	dir := t.TempDir()
	model := filepath.Join(dir, "_data")
	require.NoError(t, os.Mkdir(model, 0o755))

	removed := map[string]bool{}
	restore := swapXattrSeams(t,
		func(string) ([]string, error) { return nil, nil },
		func(string, string) ([]byte, bool, error) { return nil, false, nil }, // the volume has no ACLs
		func(string, string, []byte, int) error { return nil })
	defer restore()
	realRemove := removeXattr
	removeXattr = func(_, attr string) error { removed[attr] = true; return nil }
	defer func() { removeXattr = realRemove }()

	_, err := createStagingLike(model, quietLogger())
	require.NoError(t, err)
	assert.True(t, removed["system.posix_acl_access"], "an inherited access ACL must be cleared")
	assert.True(t, removed["system.posix_acl_default"], "and an inherited default ACL with it")
}

// A name is not a signature. A directory an application happens to call
// <volume>.borgmatic-manager-restoring-1234 has the exact shape MkdirTemp
// produces, so shape alone sent it to a recursive delete.
func TestRestoreWithSwapLeavesAnUnmarkedStagingLookalikeAlone(t *testing.T) {
	data := liveVolume(t)
	impostor := data + stagingPrefix + "1234" // right shape, never created by us
	require.NoError(t, os.Mkdir(impostor, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(impostor, "someone-elses.txt"), []byte("keep"), 0o644))

	logs, logger := capturedWarnLogger()
	require.NoError(t, restoreWithSwap(data, logger, false, func(dest string) error {
		return os.WriteFile(filepath.Join(dest, "restored.txt"), []byte("restored"), 0o644)
	}, nil))

	assert.Equal(t, []string{"restored.txt"}, names(t, data), "the restore still happened")
	assert.FileExists(t, filepath.Join(impostor, "someone-elses.txt"),
		"a directory with the right name but no mark was deleted")
	assert.Contains(t, logs.String(), "carries no mark")
}

// And the mark must not travel into the volume: what gets promoted is no longer
// scratch, and a stray marker would make the live volume look like one.
func TestRestoreWithSwapDoesNotPromoteTheScratchMark(t *testing.T) {
	data := liveVolume(t)
	require.NoError(t, restoreWithSwap(data, quietLogger(), false, func(dest string) error {
		ours, _, err := provablyOurs(dest)
		require.NoError(t, err)
		if !ours {
			t.Skip("this filesystem cannot hold the marker, so there is nothing to clear")
		}
		return os.WriteFile(filepath.Join(dest, "restored.txt"), []byte("restored"), 0o644)
	}, nil))

	marked, _, err := provablyOurs(data)
	require.NoError(t, err)
	assert.False(t, marked, "the promoted volume still carries the scratch mark")
}

// MkdirTemp replaces the last star in its pattern rather than appending, so a
// target basename containing one had the random suffix substituted into the
// middle of the name and the result no longer began with the prefix recovery
// searches for.
func TestScratchNamesStayFindableWhenTheTargetContainsAStar(t *testing.T) {
	// The star has to be in the *final* component, because that is what reaches
	// MkdirTemp as pattern syntax. A symlink-backed volume resolving to
	// /srv/app/weird*data is how this arises. Putting it in a parent directory
	// instead proves nothing, which is how the first version of this test passed
	// against the broken implementation.
	root := t.TempDir()
	data := filepath.Join(root, "volumes", "myvol", "weird*data")
	require.NoError(t, os.MkdirAll(data, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(data, "original.txt"), []byte("original"), 0o644))

	// The displaced name a rename-pair swap reserves has to be discoverable by
	// the recovery that looks for it afterwards.
	staging, err := createStagingLike(data, quietLogger())
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(staging, "restored.txt"), []byte("restored"), 0o644))
	displaced, err := swapByRenamePair(staging, data, quietLogger())
	require.NoError(t, err)
	assert.Equal(t, []string{displaced}, displacedDirsFor(t, data),
		"the displaced copy must be findable by the prefix recovery searches for")

	// And that is exactly what makes an interrupted swap recoverable.
	require.NoError(t, os.RemoveAll(data))
	require.NoError(t, recoverInterruptedSwap(data, quietLogger()))
	assert.Equal(t, []string{"original.txt"}, names(t, data), "the volume came back")
}

// A name an application happens to use is not evidence. Recovering from an
// unverified directory would rename someone else's data into the volume's path
// and present it as the volume.
func TestRecoverInterruptedSwapRefusesAnUnmarkedCandidate(t *testing.T) {
	data := liveVolume(t)
	impostor := data + oldPrefix + "1234" // right shape, never displaced by us
	require.NoError(t, os.Mkdir(impostor, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(impostor, "someone-elses.txt"), []byte("keep"), 0o644))
	require.NoError(t, os.RemoveAll(data)) // the volume is missing for some other reason

	logs, logger := capturedWarnLogger()
	require.NoError(t, recoverInterruptedSwap(data, logger))

	assert.NoDirExists(t, data, "unrelated data must not be presented as the volume")
	assert.FileExists(t, filepath.Join(impostor, "someone-elses.txt"), "and it must be left where it is")
	assert.Contains(t, logs.String(), "carries no mark")
}

// The proof has to outlive every step that can still strand staging. Removing
// it before the swap left a window in which a kill produced a complete,
// volume-sized tree with no mark, which the next restore would refuse to delete
// and leave on disk forever.
func TestStagingStaysProvablyOursUntilItIsPromoted(t *testing.T) {
	data := liveVolume(t)
	var stagingPath string

	require.NoError(t, restoreWithSwap(data, quietLogger(), false, func(dest string) error {
		stagingPath = dest
		return os.WriteFile(filepath.Join(dest, "restored.txt"), []byte("restored"), 0o644)
	}, func() error {
		// The last thing that runs before the swap: the tree is complete here,
		// and this is the moment a kill would strand it.
		ours, _, err := provablyOurs(stagingPath)
		require.NoError(t, err)
		if ours {
			return nil
		}
		marked, _, markErr := provablyOurs(stagingPath)
		require.NoError(t, markErr)
		if !marked {
			t.Skip("this filesystem cannot hold the marker, so there is nothing to prove")
		}
		return nil
	}))
	assert.Equal(t, []string{"restored.txt"}, names(t, data))
}

// The scratch name is the target's own name plus a prefix and a random suffix,
// so a target with a long but perfectly valid basename can leave no room. That
// is a reason to restore in place, not a reason to refuse a volume the previous
// implementation could restore.
func TestCanCreateSiblingTreatsANameTooLongAsCannotStage(t *testing.T) {
	root := t.TempDir()
	// NAME_MAX is 255 on every filesystem this runs on; leave just too little
	// room for the prefix and MkdirTemp's digits.
	longName := strings.Repeat("n", 250)
	data := filepath.Join(root, "volumes", "myvol", longName)
	require.NoError(t, os.MkdirAll(data, 0o755))

	ok, err := canCreateSibling(data)
	require.NoError(t, err, "a name with no room for a sibling is an answer, not a failure")
	assert.False(t, ok, "and it means the restore writes in place")
}

func TestCanCreateSiblingAcceptsAnOrdinaryName(t *testing.T) {
	ok, err := canCreateSibling(liveVolume(t))
	require.NoError(t, err)
	assert.True(t, ok)
}

// The decision that determines whether a volume's data is destroyed before its
// replacement exists. --force must reach it, or the flag keeps its promise only
// until a container actually starts, which is the case it exists for.
func TestInPlaceReasonCoversEveryCannotStageCondition(t *testing.T) {
	const why = "this volume's data directory is encrypted"
	for name, tc := range map[string]struct {
		force, mounted, encrypted, unstageable bool
		wantInPlace                            bool
		wantContains                           string
	}{
		"nothing in the way":       {wantInPlace: false},
		"forced":                   {force: true, wantInPlace: true, wantContains: "--force was given"},
		"forced with nothing else": {force: true, wantInPlace: true, wantContains: "in place"},
		"a mount point":            {mounted: true, wantInPlace: true, wantContains: "its own mount point"},
		"encrypted":                {encrypted: true, wantInPlace: true, wantContains: why},
		"nowhere to stage":         {unstageable: true, wantInPlace: true, wantContains: "cannot be created"},
		"forced and also a mount":  {force: true, mounted: true, wantInPlace: true, wantContains: "--force was given"},
	} {
		t.Run(name, func(t *testing.T) {
			got := inPlaceReason(tc.force, tc.mounted, tc.encrypted, tc.unstageable, why)
			if !tc.wantInPlace {
				assert.Empty(t, got, "a volume with nothing in the way must be staged")
				return
			}
			require.NotEmpty(t, got, "this condition must send the restore in place")
			assert.Contains(t, got, tc.wantContains)
		})
	}
}

// The proof of ownership has to outlive every step that can still leave a
// complete staging tree on disk. Removing it earlier left a window in which a
// kill produced a volume-sized tree with no mark, which the next restore would
// refuse to delete and leave forever.
func TestStagingIsStillProvablyOursAtTheMomentItIsPromoted(t *testing.T) {
	data := liveVolume(t)

	marked := false
	realSwap := swapIntoPlaceFn
	swapIntoPlaceFn = func(staging, target string, logger *slog.Logger) (string, error) {
		var err error
		marked, _, err = provablyOurs(staging)
		require.NoError(t, err)
		return realSwap(staging, target, logger)
	}
	defer func() { swapIntoPlaceFn = realSwap }()

	require.NoError(t, restoreWithSwap(data, quietLogger(), false, func(dest string) error {
		if ours, _, err := provablyOurs(dest); err == nil && !ours {
			t.Skip("this filesystem cannot hold the marker, so there is nothing to prove")
		}
		return os.WriteFile(filepath.Join(dest, "restored.txt"), []byte("restored"), 0o644)
	}, nil))

	assert.True(t, marked, "staging lost its ownership mark before the swap committed")
	assert.Equal(t, []string{"restored.txt"}, names(t, data))
}

// The mark has to be on the inode before it moves. A kill between the rename
// and a later mark left the volume missing beside a directory recovery could
// not verify, which is worse than the problem the mark was added to solve.
func TestDisplacedDataCarriesProofFromTheInstantItExists(t *testing.T) {
	data := liveVolume(t)
	if ours, _, err := provablyOurs(data); err == nil && ours {
		t.Fatal("precondition: a live volume is not marked")
	}

	staging, err := createStagingLike(data, quietLogger())
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(staging, "restored.txt"), []byte("restored"), 0o644))

	displaced, err := swapByRenamePair(staging, data, quietLogger())
	require.NoError(t, err)

	ours, _, err := provablyOurs(displaced)
	require.NoError(t, err)
	if !ours {
		t.Skip("this filesystem cannot hold the marker")
	}
	// Which is exactly what makes the interrupted state recoverable.
	require.NoError(t, os.RemoveAll(data))
	require.NoError(t, recoverInterruptedSwap(data, quietLogger()))
	assert.Equal(t, []string{"original.txt"}, names(t, data))
}

// A failed rename leaves the volume where it was, so the claim on it is
// meaningless and must not stay behind.
func TestAFailedRenamePairLeavesNoClaimOnTheVolume(t *testing.T) {
	data := liveVolume(t)
	_, err := swapByRenamePair(data+stagingPrefix+"9999", data, quietLogger()) // staging never created
	require.Error(t, err)

	ours, _, err := provablyOurs(data)
	require.NoError(t, err)
	assert.False(t, ours, "the volume kept a scratch mark after a swap that never happened")
}

// Recovery cleans up after itself, but only its own leavings.
func TestRecoverInterruptedSwapLeavesUnverifiedSiblingsAlone(t *testing.T) {
	data := liveVolume(t)
	bystander := data + oldPrefix + "4242" // right shape, empty, not ours
	require.NoError(t, os.Mkdir(bystander, 0o755))
	real := data + oldPrefix + "1001"
	require.NoError(t, os.Rename(data, real))
	markAsOurs(real)
	if ours, _, err := provablyOurs(real); err != nil || !ours {
		t.Skip("this filesystem cannot hold the marker")
	}

	require.NoError(t, recoverInterruptedSwap(data, quietLogger()))
	assert.Equal(t, []string{"original.txt"}, names(t, data), "the volume came back")
	assert.DirExists(t, bystander, "an unverified sibling is not this function's to delete")
}

// Putting a directory back needs permission on the parent, not on the directory
// itself. Enumerating a sole verified candidate asked for permission the
// recovery does not need and made an owner-mode-0300 volume unrecoverable.
func TestRecoverInterruptedSwapReportsACandidateItCannotVerify(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads regardless of the directory mode")
	}
	data := liveVolume(t)
	displaced := data + oldPrefix + "1001"
	require.NoError(t, os.Rename(data, displaced))
	markAsOurs(displaced)
	if ours, _, err := provablyOurs(displaced); err != nil || !ours {
		t.Skip("this filesystem cannot hold the marker")
	}
	require.NoError(t, os.Chmod(displaced, 0o300)) // search and write, no read
	t.Cleanup(func() { _ = os.Chmod(displaced, 0o755) })

	// Reading the mark needs the same permission listing does, so ownership
	// cannot be established here at all. That is a real limit of marking rather
	// than a bug to work around: recovering an unverifiable directory would put
	// back the risk the mark exists to remove. What it must not do is fail with
	// a bare permission error that says nothing about what to do next.
	logs, logger := capturedWarnLogger()
	require.NoError(t, recoverInterruptedSwap(data, logger),
		"an unreadable candidate is reported, not a hard failure")
	assert.NoDirExists(t, data, "and nothing unverifiable is renamed into the volume's path")
	assert.Contains(t, logs.String(), "cannot be read")
	assert.Contains(t, logs.String(), "rename it into place yourself")
}

// The mark has to be on the inode before the rename, not applied to the name
// afterwards. What matters is the instant between: a kill there used to leave a
// missing volume beside a directory recovery could not verify.
func TestTheDisplacedCopyIsMarkedAtTheInstantOfTheRename(t *testing.T) {
	data := liveVolume(t)

	markedAtRename := false
	realMove := moveAsideFn
	moveAsideFn = func(target, displaced string) error {
		if err := realMove(target, displaced); err != nil {
			return err
		}
		var err error
		markedAtRename, _, err = provablyOurs(displaced)
		require.NoError(t, err)
		return nil
	}
	defer func() { moveAsideFn = realMove }()

	staging, err := createStagingLike(data, quietLogger())
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(staging, "restored.txt"), []byte("restored"), 0o644))
	if ours, _, oErr := provablyOurs(staging); oErr == nil && !ours {
		t.Skip("this filesystem cannot hold the marker")
	}

	_, err = swapByRenamePair(staging, data, quietLogger())
	require.NoError(t, err)
	assert.True(t, markedAtRename,
		"the displaced copy was unverifiable in the window between the rename and a later mark")
}

// The probe creates a directory in the parent, and a restore that will never
// stage does not need one. Running it anyway let a full or read-only parent
// abort a merge or forced restore that would not have touched it.
func TestTheStagingProbeIsSkippedWhenNothingWillBeStaged(t *testing.T) {
	const why = "this volume's data directory is encrypted"
	for name, tc := range map[string]struct {
		merge, force, mounted, encrypted bool
		wantProbe                        bool
	}{
		"an ordinary mirror restore": {wantProbe: true},
		"a merge":                    {merge: true},
		"forced":                     {force: true},
		"a mount point":              {mounted: true},
		"encrypted":                  {encrypted: true},
	} {
		t.Run(name, func(t *testing.T) {
			probed := false
			real := canCreateSiblingFn
			canCreateSiblingFn = func(string) (bool, error) { probed = true; return true, nil }
			defer func() { canCreateSiblingFn = real }()

			_, err := resolveInPlaceReason(tc.merge, tc.force, tc.mounted, tc.encrypted, why, "/var/lib/docker/volumes/v/_data")
			require.NoError(t, err)
			assert.Equal(t, tc.wantProbe, probed, "whether the parent was touched")
		})
	}
}

func TestAnUnstageableParentStillSendsAMirrorRestoreInPlace(t *testing.T) {
	real := canCreateSiblingFn
	canCreateSiblingFn = func(string) (bool, error) { return false, nil }
	defer func() { canCreateSiblingFn = real }()

	reason, err := resolveInPlaceReason(false, false, false, false, "", "/var/lib/docker/volumes/v/_data")
	require.NoError(t, err)
	assert.Contains(t, reason, "cannot be created")
}

// The mask is the whole contract of copyInodeFlags, and a constant declared
// beside it but left out of the expression compiles silently: unused constants
// are legal in Go. FS_DAX_FL was added to the block and missing from the mask
// for two rounds because of exactly that.
func TestEveryDeclaredInodeFlagIsInTheInheritableMask(t *testing.T) {
	for name, flag := range map[string]int{
		"FS_NOCOW_FL":        fsNoCoWFlag,
		"FS_COMPR_FL":        fsCompressFlag,
		"FS_NOCOMP_FL":       fsNoCompressFlag,
		"FS_NOATIME_FL":      fsNoAtimeFlag,
		"FS_NODUMP_FL":       fsNoDumpFlag,
		"FS_SYNC_FL":         fsSyncFlag,
		"FS_DIRSYNC_FL":      fsDirSyncFlag,
		"FS_DAX_FL":          fsDaxFlag,
		"FS_JOURNAL_DATA_FL": fsJournalDataFlag,
		"FS_PROJINHERIT_FL":  fsProjInheritFlag,
		"FS_CASEFOLD_FL":     fsCasefoldFlag,
	} {
		assert.NotZero(t, inheritableMask&flag, "%s is declared but not carried", name)
	}
}

// A staging directory made for a btrfs subvolume target is itself a subvolume,
// and emptying one is not removing it. Left behind it is not merely litter: the
// next restore repeats the same removal and stops on it, so the volume becomes
// unrestorable until someone deletes it by hand.
func TestRemoveScratchDirFallsBackToSubvolumeDeletion(t *testing.T) {
	dir := t.TempDir()
	stubborn := filepath.Join(dir, "staging")
	require.NoError(t, os.Mkdir(stubborn, 0o755))

	// A filesystem that empties the directory but refuses to remove its root.
	realRemove, realDelete := removeAllFn, deleteBtrfsSubvolume
	removeAllFn = func(string) error { return errors.New("rmdir: operation not permitted") }
	deleted := ""
	deleteBtrfsSubvolume = func(path string) error { deleted = path; return os.Remove(path) }
	isSubvolume = func(string) (bool, error) { return true, nil }
	defer func() {
		removeAllFn, deleteBtrfsSubvolume = realRemove, realDelete
		isSubvolume = isBtrfsSubvolumeRoot
	}()

	require.NoError(t, removeScratchDir(stubborn))
	assert.Equal(t, stubborn, deleted, "the subvolume path was not handed to btrfs")
	assert.NoDirExists(t, stubborn)
}

// And an ordinary directory must not send anyone looking for btrfs.
func TestRemoveScratchDirDoesNotReachForBtrfsOnAnOrdinaryDirectory(t *testing.T) {
	dir := t.TempDir()
	plain := filepath.Join(dir, "staging")
	require.NoError(t, os.Mkdir(plain, 0o755))

	realDelete := deleteBtrfsSubvolume
	called := false
	deleteBtrfsSubvolume = func(string) error { called = true; return nil }
	defer func() { deleteBtrfsSubvolume = realDelete }()

	require.NoError(t, removeScratchDir(plain))
	assert.False(t, called, "an ordinary directory removes ordinarily")
	assert.NoDirExists(t, plain)
}

// Without a claim on the inode an interruption in the two-rename window leaves
// a volume recovery cannot put back automatically. That has to be said before
// the window opens, not discovered afterwards.
func TestSwapByRenamePairWarnsWhenItCannotClaimTheVolume(t *testing.T) {
	data := liveVolume(t)
	staging, err := createStagingLike(data, quietLogger())
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(staging, "restored.txt"), []byte("restored"), 0o644))

	realSet := setXattr
	setXattr = func(_, attr string, _ []byte, _ int) error {
		if attr == scratchMarkerAttr {
			return unix.EOPNOTSUPP
		}
		return realSet("", attr, nil, 0)
	}
	defer func() { setXattr = realSet }()

	logs, logger := capturedWarnLogger()
	_, err = swapByRenamePair(staging, data, logger)
	require.NoError(t, err, "the restore is still worth doing; the recovery is manual, not impossible")
	assert.Contains(t, logs.String(), "put back by hand")
	assert.Equal(t, []string{"restored.txt"}, names(t, data))
}

// Asking whether a directory is empty needs permission to read it. An
// application-owned one this user cannot read would otherwise abort every
// restore before the ownership check that would have dismissed it ever ran.
func TestRecoverInterruptedSwapSkipsAnUnreadableStrangerBesideALiveVolume(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads regardless of the directory mode")
	}
	data := liveVolume(t)
	stranger := data + oldPrefix + "1234"
	require.NoError(t, os.Mkdir(stranger, 0o755))
	require.NoError(t, os.Chmod(stranger, 0o000))
	t.Cleanup(func() { _ = os.Chmod(stranger, 0o755) })

	logs, logger := capturedWarnLogger()
	require.NoError(t, recoverInterruptedSwap(data, logger),
		"an unreadable stranger must not abort a restore of a healthy volume")
	assert.Contains(t, logs.String(), "carries no mark")
	assert.Equal(t, []string{"original.txt"}, names(t, data))
}

// The exchange sends the live inode to the staging name, and that inode was
// never this tool's. Without a claim it arrives there unprovable, and a kill
// before cleanup leaves a volume-sized directory no later run will remove.
func TestTheDisplacedInodeIsProvableAfterAnExchange(t *testing.T) {
	data := liveVolume(t)
	staging, err := createStagingLike(data, quietLogger())
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(staging, "restored.txt"), []byte("restored"), 0o644))
	if ours, _, oErr := provablyOurs(staging); oErr == nil && !ours {
		t.Skip("this filesystem cannot hold the marker")
	}

	displaced, err := swapIntoPlace(staging, data, quietLogger())
	require.NoError(t, err)
	if displaced != staging {
		t.Skip("this filesystem used the rename-pair fallback, which is covered separately")
	}

	ours, known, err := provablyOurs(displaced)
	require.NoError(t, err)
	require.True(t, known)
	assert.True(t, ours, "the displaced original cannot be cleaned up by a later run")
	assert.Equal(t, []string{"restored.txt"}, names(t, data))
}

// On a labelled filesystem a fresh directory is given a context by policy. The
// label copying only ever applies one the model already carries, so an
// unlabelled volume could be replaced by a labelled root and quietly gain or
// lose access.
func TestCreateStagingLikeClearsALabelTheVolumeDoesNotHave(t *testing.T) {
	dir := t.TempDir()
	model := filepath.Join(dir, "_data")
	require.NoError(t, os.Mkdir(model, 0o755))

	removed := map[string]bool{}
	restore := swapXattrSeams(t,
		func(string) ([]string, error) { return nil, nil },
		func(path, attr string) ([]byte, bool, error) {
			// The volume carries no label; the fresh staging directory does.
			if attr == seLinuxAttr && path != model {
				return []byte("system_u:object_r:container_file_t:s0"), true, nil
			}
			return nil, false, nil
		},
		func(string, string, []byte, int) error { return nil })
	defer restore()
	realRemove := removeXattr
	removeXattr = func(_, attr string) error { removed[attr] = true; return nil }
	defer func() { removeXattr = realRemove }()

	_, err := createStagingLike(model, quietLogger())
	require.NoError(t, err)
	assert.True(t, removed[seLinuxAttr], "the inherited label was left on the replacement")
}

// And a label that cannot be removed is reported rather than failing a restore
// that has otherwise succeeded.
func TestCreateStagingLikeWarnsWhenAnInheritedLabelCannotBeCleared(t *testing.T) {
	dir := t.TempDir()
	model := filepath.Join(dir, "_data")
	require.NoError(t, os.Mkdir(model, 0o755))

	restore := swapXattrSeams(t,
		func(string) ([]string, error) { return nil, nil },
		func(path, attr string) ([]byte, bool, error) {
			if attr == seLinuxAttr && path != model {
				return []byte("system_u:object_r:container_file_t:s0"), true, nil
			}
			return nil, false, nil
		},
		func(string, string, []byte, int) error { return nil })
	defer restore()
	realRemove := removeXattr
	removeXattr = func(_, attr string) error {
		if attr == seLinuxAttr {
			return unix.EPERM
		}
		return nil
	}
	defer func() { removeXattr = realRemove }()

	logs, logger := capturedWarnLogger()
	_, err := createStagingLike(model, logger)
	require.NoError(t, err, "a label that cannot be cleared must not fail the restore")
	assert.Contains(t, logs.String(), "carry a label the original did not")
}

// A filesystem without an atomic exchange is one thing; a filesystem that has
// one and failed is another. Reading every failure as the first sends a genuine
// I/O error into the non-atomic two-rename window instead of returning with the
// volume untouched.
func TestSwapIntoPlaceFallsBackOnlyForAnUnsupportedExchange(t *testing.T) {
	for name, tc := range map[string]struct {
		exchangeErr error
		wantFalls   bool
	}{
		"not implemented":     {unix.ENOSYS, true},
		"not supported here":  {unix.EOPNOTSUPP, true},
		"rejected as invalid": {unix.EINVAL, true},
		"an I/O error":        {unix.EIO, false},
		"permission denied":   {unix.EACCES, false},
		"busy":                {unix.EBUSY, false},
	} {
		t.Run(name, func(t *testing.T) {
			data := liveVolume(t)
			staging, err := createStagingLike(data, quietLogger())
			require.NoError(t, err)
			require.NoError(t, os.WriteFile(filepath.Join(staging, "restored.txt"), []byte("restored"), 0o644))

			fellBack := false
			realExchange, realMove := exchangeFn, moveAsideFn
			exchangeFn = func(string, string) error { return tc.exchangeErr }
			moveAsideFn = func(target, displaced string) error {
				fellBack = true
				return realMove(target, displaced)
			}
			defer func() { exchangeFn, moveAsideFn = realExchange, realMove }()

			_, err = swapIntoPlace(staging, data, quietLogger())
			assert.Equal(t, tc.wantFalls, fellBack, "whether the two-rename window was entered")
			if tc.wantFalls {
				require.NoError(t, err)
				return
			}
			require.Error(t, err, "a real failure must return with the volume untouched")
			assert.Equal(t, []string{"original.txt"}, names(t, data))
		})
	}
}

// A failed exchange must not leave a claim on a volume it never moved.
func TestSwapIntoPlaceClearsItsClaimWhenTheExchangeFails(t *testing.T) {
	data := liveVolume(t)
	staging, err := createStagingLike(data, quietLogger())
	require.NoError(t, err)
	if ours, _, oErr := provablyOurs(staging); oErr == nil && !ours {
		t.Skip("this filesystem cannot hold the marker")
	}

	realExchange := exchangeFn
	exchangeFn = func(string, string) error { return unix.EIO }
	defer func() { exchangeFn = realExchange }()

	_, err = swapIntoPlace(staging, data, quietLogger())
	require.Error(t, err)
	ours, _, err := provablyOurs(data)
	require.NoError(t, err)
	assert.False(t, ours, "the volume kept a claim from an exchange that never happened")
}

// A volume name is not the identity that matters: the swap moves an inode, and
// two volumes can point at one backing directory through a symlinked _data. A
// container holding it under the other name was reported as absent and left
// writing into an orphan the cleanup then deleted.
func TestVolumeHasRunningContainerMatchesAnAliasByResolvedPath(t *testing.T) {
	root := t.TempDir()
	backing := filepath.Join(root, "backing", "shared")
	require.NoError(t, os.MkdirAll(backing, 0o755))

	// Two volumes, one backing directory.
	aData := filepath.Join(root, "volumes", "vol-a", "_data")
	bData := filepath.Join(root, "volumes", "vol-b", "_data")
	require.NoError(t, os.MkdirAll(filepath.Dir(aData), 0o755))
	require.NoError(t, os.MkdirAll(filepath.Dir(bData), 0o755))
	require.NoError(t, os.Symlink(backing, aData))
	require.NoError(t, os.Symlink(backing, bData))

	rt := &fakeRuntime{containers: []runtime.ContainerInfo{{
		Name:    "user-of-b",
		Running: true,
		Mounts:  []runtime.VolumeMount{{Name: "vol-b", Source: bData, Destination: "/data"}},
	}}}

	// Restoring vol-a, whose resolved target is the directory vol-b is on.
	live, err := volumeHasRunningContainer(context.Background(), rt, "vol-a", backing)
	require.NoError(t, err)
	assert.True(t, live, "a container holding the same inode under another name was missed")
}

func TestVolumeHasRunningContainerIgnoresAnUnrelatedVolume(t *testing.T) {
	root := t.TempDir()
	mine := filepath.Join(root, "volumes", "mine", "_data")
	theirs := filepath.Join(root, "volumes", "theirs", "_data")
	require.NoError(t, os.MkdirAll(mine, 0o755))
	require.NoError(t, os.MkdirAll(theirs, 0o755))

	rt := &fakeRuntime{containers: []runtime.ContainerInfo{
		{Name: "stopped", Running: false, Mounts: []runtime.VolumeMount{{Name: "mine", Source: mine}}},
		{Name: "other", Running: true, Mounts: []runtime.VolumeMount{{Name: "theirs", Source: theirs}}},
	}}

	live, err := volumeHasRunningContainer(context.Background(), rt, "mine", mine)
	require.NoError(t, err)
	assert.False(t, live, "a stopped user and an unrelated volume are both irrelevant")
}

// fakeRuntime is the smallest ContainerRuntime that can answer a liveness check.
type fakeRuntime struct {
	runtime.ContainerRuntime
	containers []runtime.ContainerInfo
}

func (f *fakeRuntime) ListContainers(context.Context) ([]runtime.ContainerInfo, error) {
	return f.containers, nil
}

// A parent that grants write and search but not read is a layout the previous
// in-place restore handled without ever reading it. Staging can be created and
// filled there, so the flush must not be the one step that needs more.
func TestRestoreWithSwapWorksUnderAnUnreadableParent(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads regardless of the directory mode")
	}
	data := liveVolume(t)
	parent := filepath.Dir(data)
	before, err := os.Stat(parent)
	require.NoError(t, err)
	require.NoError(t, os.Chmod(parent, 0o300)) // write + search, no read
	defer func() { _ = os.Chmod(parent, before.Mode().Perm()) }()

	require.NoError(t, restoreWithSwap(data, quietLogger(), false, func(dest string) error {
		return os.WriteFile(filepath.Join(dest, "restored.txt"), []byte("restored"), 0o644)
	}, nil), "a restore that needs no read on the parent must not fail on the flush")
	assert.Equal(t, []string{"restored.txt"}, names(t, data))
}

// The flush only needs some directory on the right filesystem; which of the
// candidates is readable depends on modes this code does not choose.
func TestSyncStagedDataFallsThroughToAReadableCandidate(t *testing.T) {
	dir := t.TempDir()
	require.Error(t, syncStagedData(filepath.Join(dir, "nope")),
		"a candidate that cannot be opened is not a flush")
	require.NoError(t, syncStagedData(filepath.Join(dir, "nope"), dir),
		"but a later one that can be opened is")
	require.Error(t, syncStagedData(), "and no candidates at all is a failure, not a silent success")
}

// signal.Notify sends without blocking and drops what it cannot deliver, so a
// one-slot channel lets a SIGCONT displace a SIGTERM that was already waiting.
func TestForwardedSignalsAreNotDroppedWhenTwoArriveTogether(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "child.log")
	logFile, err := os.Create(logPath)
	require.NoError(t, err)
	defer func() { _ = logFile.Close() }()

	// #nosec G204 -- re-execs this test binary
	child := exec.Command(os.Args[0], "-test.run=TestExtractSignalHelper")
	child.Env = minimalEnv("BM_EXTRACT_SIGNAL_HELPER=1")
	child.Stdout, child.Stderr = logFile, logFile
	require.NoError(t, child.Start())
	t.Cleanup(func() {
		_ = child.Process.Signal(syscall.SIGCONT)
		_ = child.Process.Kill()
		reapBounded(t, child)
	})

	grandchild := waitForPID(t, logPath)

	// Suspend, then deliver a termination and a resume back to back: the
	// terminate must survive the continue rather than be displaced by it.
	require.NoError(t, child.Process.Signal(syscall.SIGTSTP))
	requireProcState(t, child.Process.Pid, "T", "the manager did not stop")
	require.NoError(t, child.Process.Signal(syscall.SIGTERM))
	require.NoError(t, child.Process.Signal(syscall.SIGCONT))

	assert.Eventually(t, func() bool {
		return syscall.Kill(grandchild, 0) != nil
	}, helperGrace, 20*time.Millisecond, "the termination was dropped and borg kept running")
}

// The capacity is the invariant that prevents a dropped signal. Asserted
// structurally rather than by racing two signals into the channel, because that
// race is won or lost on scheduling and a test of it passes either way.
func TestTheSignalChannelCanHoldEverySignalItRegisters(t *testing.T) {
	ch, forwarded := forwardedSignalChannel()
	require.NotEmpty(t, forwarded)
	assert.GreaterOrEqual(t, cap(ch), len(forwarded),
		"signal.Notify drops what it cannot deliver, so a smaller channel can lose a terminate to a continue")

	for _, want := range []os.Signal{
		syscall.SIGINT, syscall.SIGQUIT, syscall.SIGTERM,
		syscall.SIGHUP, syscall.SIGTSTP, syscall.SIGCONT,
	} {
		assert.Contains(t, forwarded, want, "the extract's own session makes this the only path for %s", want)
	}
}

// A pre-swap failure has to clean up staging even when staging is a subvolume
// that ordinary removal cannot reap. Left behind, the next restore fails trying
// the same cleanup.
func TestRestoreWithSwapReapsAStubbornStagingSubvolumeOnFailure(t *testing.T) {
	data := liveVolume(t)
	boom := errors.New("archive was pruned mid-extract")

	realRemove, realDelete, realIs := removeAllFn, deleteBtrfsSubvolume, isSubvolume
	removeAllFn = func(path string) error {
		if strings.Contains(path, stagingPrefix) {
			return errors.New("rmdir: operation not permitted")
		}
		return realRemove(path)
	}
	isSubvolume = func(path string) (bool, error) { return strings.Contains(path, stagingPrefix), nil }
	deleted := ""
	deleteBtrfsSubvolume = func(path string) error { deleted = path; return realRemove(path) }
	defer func() { removeAllFn, deleteBtrfsSubvolume, isSubvolume = realRemove, realDelete, realIs }()

	err := restoreWithSwap(data, quietLogger(), false, func(string) error { return boom }, nil)
	require.ErrorIs(t, err, boom)
	assert.NotEmpty(t, deleted, "the staging subvolume was not handed to btrfs")
	assert.Empty(t, stagingDirsFor(t, data), "and nothing was left behind")
	assert.Equal(t, []string{"original.txt"}, names(t, data), "the volume is untouched")
}
