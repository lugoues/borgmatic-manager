package main

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lugoues/borgmatic-manager/internal/config"
	bmruntime "github.com/lugoues/borgmatic-manager/internal/runtime"
)

// Fixtures captured from borgmatic 2.1.6 over borg 1.4.0. borgmatic prints a
// human banner to stdout before borg's --json-lines entries, so "stdout is
// non-empty" is not the same question as "the archive holds this path".
const (
	listStdoutPathPresent = `/srv/repo: Listing archive host-2026-07-26T06:06:35.597399
{"type": "d", "mode": "drwxr-xr-x", "user": "root", "group": "root", "uid": 0, "gid": 0, "path": "myvol/_data", "healthy": true, "source": "", "linktarget": "", "flags": 0, "mtime": "2026-07-26T06:05:36.811407", "size": 0}
{"type": "-", "mode": "-rw-r--r--", "user": "root", "group": "root", "uid": 0, "gid": 0, "path": "myvol/_data/file.txt", "healthy": true, "source": "", "linktarget": "", "flags": 0, "mtime": "2026-07-26T06:05:36.811407", "size": 6}
`
	listStdoutPathAbsent = `/srv/repo: Listing archive host-2026-07-26T06:06:35.597399
`
	listStderrBadArchive = `Archive definitely-not-there does not exist
/srv/repo: Error running actions for repository
/srv/repo: Command 'borg list --log-json --json-lines /srv/repo::definitely-not-there myvol/_data' returned non-zero exit status 31.
/srv/cfg.yaml: Error running configuration
/srv/cfg.yaml: An error occurred

summary:
An error occurred
Error running actions for repository
Archive definitely-not-there does not exist
Error running configuration

Need some help? https://torsion.org/borgmatic/#issues
`
)

// The probe that gates the destructive wipe: borgmatic's banner is on stdout
// ahead of borg's entries, so a non-empty stdout is not by itself a match.
func TestArchiveEntryPathSeparatesEntriesFromBanner(t *testing.T) {
	lines := strings.Split(strings.TrimRight(listStdoutPathPresent, "\n"), "\n")
	require.Len(t, lines, 3)

	_, ok := archiveEntryOf([]byte(lines[0]))
	assert.False(t, ok, "the banner is not an entry")

	dir, ok := archiveEntryOf([]byte(lines[1]))
	require.True(t, ok)
	assert.Equal(t, "myvol/_data", dir.Path, "the directory lists as its own entry")
	assert.Equal(t, archiveEntryDir, dir.Type, "and its type is what licenses an empty restore")

	child, ok := archiveEntryOf([]byte(lines[2]))
	require.True(t, ok)
	assert.Equal(t, "myvol/_data/file.txt", child.Path, "and its contents below it")

	_, ok = archiveEntryOf(nil)
	assert.False(t, ok, "no output is not a match")
	_, ok = archiveEntryOf([]byte("{not json"))
	assert.False(t, ok, "a malformed line is not an entry")
	got, ok := archiveEntryOf([]byte("  {\"path\": \"x\", \"type\": \"-\"}  "))
	require.True(t, ok, "surrounding whitespace is tolerated")
	assert.Equal(t, "x", got.Path)
	assert.Equal(t, "-", got.Type, "a non-directory reports its own type rather than looking like one")
}

// headWriter caps what a chatty failure can cost in memory while keeping the
// leading bytes, which is where the cause is.
func TestHeadWriterKeepsHeadAndDropsTail(t *testing.T) {
	w := &headWriter{maxBytes: 8}
	n, err := w.Write([]byte("12345"))
	require.NoError(t, err)
	assert.Equal(t, 5, n, "reports a full write: it is a capture, not a sink that can fail")
	n, err = w.Write([]byte("6789abcdef"))
	require.NoError(t, err)
	assert.Equal(t, 10, n)
	assert.Equal(t, "12345678", w.String(), "kept the head, dropped the tail")
}

// borgmatic re-wraps the real cause per repository, per config, and again under
// "summary:", so the useful message is the first line, not the last.
func TestFirstNonEmptyLinePicksTheCause(t *testing.T) {
	assert.Equal(t, "Archive definitely-not-there does not exist", firstNonEmptyLine(listStderrBadArchive))
	assert.Empty(t, firstNonEmptyLine("\n\n   \n"))
	assert.Equal(t, strings.Repeat("x", 200)+"...", firstNonEmptyLine(strings.Repeat("x", 500)), "a wall of borg output is truncated")
}

// The probe must treat a failed borgmatic as "cannot tell", never as "nothing
// there": returning false with no error would let the caller wipe the target.
func TestListArchivedVolumesErrorsWhenBorgmaticFails(t *testing.T) {
	vols, err := listArchivedVolumes(context.Background(), "/bin/false", "cfg.yaml", "latest")
	found, _, _ := archivedState(vols, "myvol")
	require.Error(t, err, "a non-zero exit is an error, not an empty result")
	assert.False(t, found)
}

func TestListArchivedVolumesReadsEntriesFromStdout(t *testing.T) {
	dir := t.TempDir()
	stub := filepath.Join(dir, "borgmatic")
	require.NoError(t, os.WriteFile(stub, []byte("#!/bin/sh\ncat <<'EOF'\n"+listStdoutPathPresent+"EOF\n"), 0o700))

	vols, err := listArchivedVolumes(context.Background(), stub, "cfg.yaml", "latest")
	found, _, _ := archivedState(vols, "myvol")
	require.NoError(t, err)
	assert.True(t, found, "entries under the path mean the extract has something to write")
}

func TestListArchivedVolumesReportsBannerOnlyAsEmpty(t *testing.T) {
	dir := t.TempDir()
	stub := filepath.Join(dir, "borgmatic")
	require.NoError(t, os.WriteFile(stub, []byte("#!/bin/sh\ncat <<'EOF'\n"+listStdoutPathAbsent+"EOF\n"), 0o700))

	vols, err := listArchivedVolumes(context.Background(), stub, "cfg.yaml", "latest")
	found, _, _ := archivedState(vols, "myvol")
	require.NoError(t, err, "borgmatic exited 0: this is a real answer, not a probe failure")
	assert.False(t, found, "an archive predating the volume must not be mirrored over live data")
}

// borg emits one JSON line per file, so an archive holding millions of them
// would be gigabytes if buffered. Streaming keeps memory flat: this listing is
// far larger than any buffer the probe is allowed to hold.
func TestListArchivedVolumesStreamsALargeListing(t *testing.T) {
	dir := t.TempDir()
	stub := filepath.Join(dir, "borgmatic")
	// ~200k entries, several tens of MB, emitted without ever being retained.
	script := "#!/bin/sh\n" +
		"echo '/srv/repo: Listing archive host-1'\n" +
		"i=0\n" +
		"while [ $i -lt 200000 ]; do\n" +
		"  echo '{\"type\": \"-\", \"path\": \"myvol/_data/some-reasonably-long-file-name\"}'\n" +
		"  i=$((i+1))\n" +
		"done\n"
	require.NoError(t, os.WriteFile(stub, []byte(script), 0o700))

	// Retained heap, not cumulative allocation: the property is that the listing
	// is never held, and a per-line parse legitimately churns memory the
	// collector reclaims. TotalAlloc would measure the churn and miss the point.
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	vols, err := listArchivedVolumes(context.Background(), stub, "cfg.yaml", "latest")
	found, _, _ := archivedState(vols, "myvol")
	runtime.GC()
	runtime.ReadMemStats(&after)

	require.NoError(t, err)
	assert.True(t, found)
	retained := int64(after.HeapAlloc) - int64(before.HeapAlloc)
	assert.Less(t, retained, int64(8<<20),
		"the listing must be streamed, not accumulated (retained %d bytes)", retained)
}

// An entry proves the path is there, but a listing that dies partway through
// proves nothing about the extract that follows. Draining to the end is what
// makes the exit status meaningful, so a late failure still refuses the wipe.
func TestListArchivedVolumesFailsWhenListingDiesAfterAnEntry(t *testing.T) {
	dir := t.TempDir()
	stub := filepath.Join(dir, "borgmatic")
	script := "#!/bin/sh\n" +
		"echo '/srv/repo: Listing archive host-1'\n" +
		"echo '{\"type\": \"-\", \"path\": \"myvol/_data/file.txt\"}'\n" +
		"echo 'Data integrity error: chunk id mismatch' >&2\n" +
		"exit 2\n"
	require.NoError(t, os.WriteFile(stub, []byte(script), 0o700))

	vols, err := listArchivedVolumes(context.Background(), stub, "cfg.yaml", "latest")
	found, _, _ := archivedState(vols, "myvol")
	require.Error(t, err, "an entry seen before a failure is not a confirmation")
	assert.False(t, found)
	assert.Contains(t, err.Error(), "chunk id mismatch", "the cause reaches the operator")
}

// A stream that cannot be read through is "cannot tell", never "nothing there":
// reporting absent would let the caller empty the volume. It must also not hang:
// once the scanner stops reading, a still-writing borgmatic would block on a
// full pipe and take Wait down with it.
func TestListArchivedVolumesErrorsOnUnreadableStreamWithoutHanging(t *testing.T) {
	dir := t.TempDir()
	stub := filepath.Join(dir, "borgmatic")
	// An over-long line the scanner must reject, followed by far more output
	// than a pipe buffer holds, so a missing cancel deadlocks instead of failing.
	script := "#!/bin/sh\nprintf '{'\nhead -c " + strconv.Itoa(maxListLineBytes+1024) +
		" /dev/zero | tr '\\000' 'a'\nprintf '\\n'\n" +
		"head -c " + strconv.Itoa(8<<20) + " /dev/zero | tr '\\000' 'b'\n"
	require.NoError(t, os.WriteFile(stub, []byte(script), 0o700))

	type result struct {
		found bool
		err   error
	}
	done := make(chan result, 1)
	go func() {
		vols, err := listArchivedVolumes(context.Background(), stub, "cfg.yaml", "latest")
		found, _, _ := archivedState(vols, "myvol")
		done <- result{found, err}
	}()

	select {
	case got := <-done:
		require.Error(t, got.err, "a truncated listing must not read as an empty one")
		assert.False(t, got.found)
	case <-time.After(30 * time.Second):
		t.Fatal("probe hung: it stopped reading stdout but waited on a process still writing to it")
	}
}

// The listing is filtered by borg, with the same expression the entries are
// classified by.
func TestListArchivedVolumesPassesExtractArguments(t *testing.T) {
	dir := t.TempDir()
	stub := filepath.Join(dir, "borgmatic")
	argsFile := filepath.Join(dir, "args")
	require.NoError(t, os.WriteFile(stub, []byte("#!/bin/sh\necho \"$@\" > "+argsFile+"\n"), 0o700))

	_, err := listArchivedVolumes(context.Background(), stub, "/tmp/cfg.yaml", "weekly-1")
	require.NoError(t, err)
	recorded, err := os.ReadFile(argsFile)
	require.NoError(t, err)
	assert.Equal(t, "--config /tmp/cfg.yaml list --archive weekly-1 --path re:"+archivedVolumeExpr+" --json", strings.TrimSpace(string(recorded)))
}

func TestEmptyVolumeDataRefusesNonVolumePaths(t *testing.T) {
	dir := t.TempDir() // no "/volumes/" component
	require.NoError(t, os.WriteFile(filepath.Join(dir, "keep"), []byte("x"), 0o600))

	err := emptyVolumeData(dir, dir)
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

	require.NoError(t, emptyVolumeData(data, data))

	entries, err := os.ReadDir(data)
	require.NoError(t, err)
	assert.Empty(t, entries, "contents removed")
	_, err = os.Stat(data)
	assert.NoError(t, err, "but the _data directory itself is kept")
}

// archivedState reports what a listing said about volume name.
func archivedState(vols []archivedVolume, name string) (found, hasChildren, rootIsDir bool) {
	for _, v := range vols {
		if v.Name == name {
			return true, v.HasChildren, v.RootIsDir
		}
	}
	return false, false, false
}

func TestPlanVolumeRestoreTargetsTheRuntimeMountpoint(t *testing.T) {
	vols := []bmruntime.VolumeInfo{
		{Name: "myvol", Mountpoint: "/var/lib/docker/volumes/myvol/_data"},
		{Name: "spare", Mountpoint: "/srv/elsewhere/spare/_data"},
	}

	p, err := planVolumeRestore(vols, "myvol", "")
	require.NoError(t, err)
	assert.Equal(t, "myvol", p.targetVolume)
	assert.Equal(t, "/var/lib/docker/volumes/myvol/_data", p.targetData)

	p, err = planVolumeRestore(vols, "myvol", "spare")
	require.NoError(t, err)
	assert.Equal(t, "spare", p.targetVolume)
	assert.Equal(t, "/srv/elsewhere/spare/_data", p.targetData)

	// The source need not exist on this host, only the target.
	_, err = planVolumeRestore(vols, "gone", "spare")
	require.NoError(t, err)
	_, err = planVolumeRestore(vols, "gone", "")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "create the volume first")
}

// Both path layouts list by volume name, and nothing that is not a volume
// root (dumps, deeper directories) is mistaken for one.
func TestListArchivedVolumesFindsBothLayouts(t *testing.T) {
	dir := t.TempDir()
	stub := filepath.Join(dir, "borgmatic")
	out := `/srv/repo: Listing archive host-1
{"type": "d", "path": "app/_data"}
{"type": "-", "path": "app/_data/config.yml"}
{"type": "d", "path": "var/lib/containers/storage/volumes/systemd-pg/_data"}
{"type": "-", "path": "var/lib/containers/storage/volumes/systemd-pg/_data/PG_VERSION"}
{"type": "d", "path": "var/lib/containers/storage/volumes/blank/_data"}
{"type": "-", "path": "borgmatic/postgresql_databases/db/dump"}
`
	require.NoError(t, os.WriteFile(stub, []byte("#!/bin/sh\ncat <<'EOF'\n"+out+"EOF\n"), 0o700))

	vols, err := listArchivedVolumes(context.Background(), stub, "cfg.yaml", "host-1")
	require.NoError(t, err)
	assert.Equal(t, []archivedVolume{
		{Name: "app", Path: "app/_data", RootIsDir: true, HasChildren: true},
		{Name: "blank", Path: "var/lib/containers/storage/volumes/blank/_data", RootIsDir: true},
		{Name: "systemd-pg", Path: "var/lib/containers/storage/volumes/systemd-pg/_data", RootIsDir: true, HasChildren: true},
	}, vols)
}

func TestPickArchivedVolumeRefusesMissingAndAmbiguous(t *testing.T) {
	vols := []archivedVolume{
		{Name: "app", Path: "app/_data"},
		{Name: "pg", Path: "pg/_data"},
		{Name: "pg", Path: "var/lib/docker/volumes/pg/_data"},
	}

	got, err := pickArchivedVolume(vols, "app", "host-1", "grp")
	require.NoError(t, err)
	assert.Equal(t, "app/_data", got.Path)

	_, err = pickArchivedVolume(vols, "nope", "host-1", "grp")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "volumes in it: app, pg, pg", "the operator sees what is restorable")

	_, err = pickArchivedVolume(vols, "pg", "host-1", "grp")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "more than one path")
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

// The probe answers two questions: is the path in the archive at all, and does
// it have children. A volume archived while empty is present as a bare
// directory, and only the second question tells that apart from a path that
// matched nothing.
func TestListArchivedVolumesDistinguishesAnEmptyArchivedDirectory(t *testing.T) {
	dir := t.TempDir()
	stub := filepath.Join(dir, "borgmatic")
	onlyTheDirectory := `/srv/repo: Listing archive host-1
{"type": "d", "mode": "drwxr-xr-x", "path": "myvol/_data", "size": 0}
`
	require.NoError(t, os.WriteFile(stub, []byte("#!/bin/sh\ncat <<'EOF'\n"+onlyTheDirectory+"EOF\n"), 0o700))

	vols, err := listArchivedVolumes(context.Background(), stub, "cfg.yaml", "latest")
	found, hasChildren, rootIsDir := archivedState(vols, "myvol")
	require.NoError(t, err)
	assert.True(t, found, "the path is in the archive")
	assert.False(t, hasChildren, "but it held nothing")
	assert.True(t, rootIsDir, "and it really is a directory, which is what licenses restoring to empty")
}

func TestListArchivedVolumesReportsChildren(t *testing.T) {
	dir := t.TempDir()
	stub := filepath.Join(dir, "borgmatic")
	require.NoError(t, os.WriteFile(stub, []byte("#!/bin/sh\ncat <<'EOF'\n"+listStdoutPathPresent+"EOF\n"), 0o700))

	vols, err := listArchivedVolumes(context.Background(), stub, "cfg.yaml", "latest")
	found, hasChildren, rootIsDir := archivedState(vols, "myvol")
	require.NoError(t, err)
	assert.True(t, found)
	assert.True(t, hasChildren, "the archive holds files under the path")
	assert.True(t, rootIsDir)
}

// An archive holding a symlink or a regular file where the volume directory
// should be lists exactly like a directory with nothing in it. Only the type
// separates them, and mistaking one for the other licenses swapping an empty
// directory over a volume that has data in it.
func TestListArchivedVolumesDoesNotMistakeANonDirectoryForAnEmptyOne(t *testing.T) {
	for name, line := range map[string]string{
		"a symlink":      `{"type": "l", "mode": "lrwxrwxrwx", "path": "myvol/_data", "linktarget": "/elsewhere", "size": 0}`,
		"a regular file": `{"type": "-", "mode": "-rw-r--r--", "path": "myvol/_data", "size": 12}`,
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			stub := filepath.Join(dir, "borgmatic")
			out := "/srv/repo: Listing archive host-1\n" + line + "\n"
			require.NoError(t, os.WriteFile(stub, []byte("#!/bin/sh\ncat <<'EOF'\n"+out+"EOF\n"), 0o700))

			vols, err := listArchivedVolumes(context.Background(), stub, "cfg.yaml", "host-1")
			found, hasChildren, rootIsDir := archivedState(vols, "myvol")
			require.NoError(t, err)
			assert.True(t, found, "it is in the archive")
			assert.False(t, hasChildren)
			assert.False(t, rootIsDir, "but it is not a directory, so an empty restore must not be licensed")
		})
	}
}
