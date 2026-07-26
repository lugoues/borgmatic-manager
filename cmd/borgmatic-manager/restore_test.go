package main

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lugoues/borgmatic-manager/internal/config"
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
func TestIsArchiveEntryLineSeparatesEntriesFromBanner(t *testing.T) {
	lines := strings.Split(strings.TrimRight(listStdoutPathPresent, "\n"), "\n")
	require.Len(t, lines, 3)
	assert.False(t, isArchiveEntryLine([]byte(lines[0])), "the banner is not an entry")
	assert.True(t, isArchiveEntryLine([]byte(lines[1])))
	assert.True(t, isArchiveEntryLine([]byte(lines[2])))

	assert.False(t, isArchiveEntryLine(nil), "no output is not a match")
	assert.False(t, isArchiveEntryLine([]byte("{not json")), "a malformed line is not an entry")
	assert.True(t, isArchiveEntryLine([]byte("  {\"path\": \"x\"}  ")), "surrounding whitespace is tolerated")
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
func TestArchivePathPopulatedErrorsWhenBorgmaticFails(t *testing.T) {
	found, err := archivePathPopulated(context.Background(), "/bin/false", "cfg.yaml", "latest", "myvol/_data")
	require.Error(t, err, "a non-zero exit is an error, not an empty result")
	assert.False(t, found)
}

func TestArchivePathPopulatedReadsEntriesFromStdout(t *testing.T) {
	dir := t.TempDir()
	stub := filepath.Join(dir, "borgmatic")
	require.NoError(t, os.WriteFile(stub, []byte("#!/bin/sh\ncat <<'EOF'\n"+listStdoutPathPresent+"EOF\n"), 0o700))

	found, err := archivePathPopulated(context.Background(), stub, "cfg.yaml", "latest", "myvol/_data")
	require.NoError(t, err)
	assert.True(t, found, "entries under the path mean the extract has something to write")
}

func TestArchivePathPopulatedReportsBannerOnlyAsEmpty(t *testing.T) {
	dir := t.TempDir()
	stub := filepath.Join(dir, "borgmatic")
	require.NoError(t, os.WriteFile(stub, []byte("#!/bin/sh\ncat <<'EOF'\n"+listStdoutPathAbsent+"EOF\n"), 0o700))

	found, err := archivePathPopulated(context.Background(), stub, "cfg.yaml", "latest", "myvol/_data")
	require.NoError(t, err, "borgmatic exited 0: this is a real answer, not a probe failure")
	assert.False(t, found, "an archive predating the volume must not be mirrored over live data")
}

// borg emits one JSON line per file, so an archive holding millions of them
// would be gigabytes if buffered. One entry settles the question, so the probe
// stops there instead of reading the rest.
func TestArchivePathPopulatedStopsAtFirstEntry(t *testing.T) {
	dir := t.TempDir()
	stub := filepath.Join(dir, "borgmatic")
	// An emitter that never ends. Buffering it would hang forever and grow
	// without bound, so simply returning is the proof that it streams and stops.
	script := "#!/bin/sh\n" +
		"echo '/srv/repo: Listing archive host-1'\n" +
		"while :; do echo '{\"type\": \"-\", \"path\": \"myvol/_data/f\"}' || exit 0; done\n"
	require.NoError(t, os.WriteFile(stub, []byte(script), 0o700))

	type result struct {
		found bool
		err   error
	}
	done := make(chan result, 1)
	go func() {
		found, err := archivePathPopulated(context.Background(), stub, "cfg.yaml", "latest", "myvol/_data")
		done <- result{found, err}
	}()

	select {
	case got := <-done:
		require.NoError(t, got.err)
		assert.True(t, got.found, "the first entry settles it")
	case <-time.After(30 * time.Second):
		t.Fatal("probe did not stop at the first entry; it drained an unbounded listing")
	}
}

// A stream that cannot be read through is "cannot tell", never "nothing there":
// reporting absent would let the caller empty the volume.
func TestArchivePathPopulatedErrorsOnUnreadableStream(t *testing.T) {
	dir := t.TempDir()
	stub := filepath.Join(dir, "borgmatic")
	// One entry-looking line past the scanner's cap, and no valid entry at all.
	script := "#!/bin/sh\nprintf '{'\nhead -c " + strconv.Itoa(maxListLineBytes+1024) +
		" /dev/zero | tr '\\000' 'a'\nprintf '\\n'\n"
	require.NoError(t, os.WriteFile(stub, []byte(script), 0o700))

	found, err := archivePathPopulated(context.Background(), stub, "cfg.yaml", "latest", "myvol/_data")
	require.Error(t, err, "a truncated listing must not read as an empty one")
	assert.False(t, found)
}

// The probe passes the extract's own --archive/--path pair, so a true answer
// means that exact extract has something to write.
func TestArchivePathPopulatedPassesExtractArguments(t *testing.T) {
	dir := t.TempDir()
	stub := filepath.Join(dir, "borgmatic")
	argsFile := filepath.Join(dir, "args")
	require.NoError(t, os.WriteFile(stub, []byte("#!/bin/sh\necho \"$@\" > "+argsFile+"\n"), 0o700))

	_, err := archivePathPopulated(context.Background(), stub, "/tmp/cfg.yaml", "weekly-1", "myvol/_data")
	require.NoError(t, err)
	recorded, err := os.ReadFile(argsFile)
	require.NoError(t, err)
	assert.Equal(t, "--config /tmp/cfg.yaml list --archive weekly-1 --path myvol/_data --json", strings.TrimSpace(string(recorded)))
}

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
