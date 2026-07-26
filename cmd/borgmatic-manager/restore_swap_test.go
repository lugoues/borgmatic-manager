package main

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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

	err := restoreWithSwap(data, quietLogger(), func(dest string) error {
		return os.WriteFile(filepath.Join(dest, "restored.txt"), []byte("restored"), 0o644)
	})
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

	err := restoreWithSwap(data, quietLogger(), func(dest string) error {
		// A partial extract, then failure: the realistic shape.
		require.NoError(t, os.WriteFile(filepath.Join(dest, "half.txt"), []byte("partial"), 0o644))
		return boom
	})

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

	err := restoreWithSwap(data, quietLogger(), func(string) error { return nil })

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

	err := restoreWithSwap(data, quietLogger(), func(dest string) error {
		assert.Empty(t, names(t, dest), "the staging directory starts empty")
		return os.WriteFile(filepath.Join(dest, "restored.txt"), []byte("restored"), 0o644)
	})
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

	require.NoError(t, restoreWithSwap(data, quietLogger(), func(dest string) error {
		return os.WriteFile(filepath.Join(dest, "restored.txt"), []byte("x"), 0o644)
	}))

	after, err := os.Stat(data)
	require.NoError(t, err)
	assert.Equal(t, before.Mode().Perm(), after.Mode().Perm(), "mode carried onto the replacement")
}

// The two renames are not one atomic step. If the second fails, the volume
// must not be left without a data directory at all.
func TestSwapIntoPlaceRestoresTheOriginalIfTheSecondRenameFails(t *testing.T) {
	data := liveVolume(t)
	missingStaging := data + stagingSuffix // deliberately never created

	err := swapIntoPlace(missingStaging, data)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "volume is unchanged")
	require.DirExists(t, data, "the original was put back rather than left displaced")
	assert.Equal(t, []string{"original.txt"}, names(t, data))
	assert.NoDirExists(t, data+oldSuffix)
}

// Not an SELinux host: reading a context that is not there is normal, not an
// error, and must not fail the restore.
func TestCopySELinuxContextIgnoresAbsentLabels(t *testing.T) {
	dir := t.TempDir()
	from := filepath.Join(dir, "from")
	to := filepath.Join(dir, "to")
	require.NoError(t, os.Mkdir(from, 0o755))
	require.NoError(t, os.Mkdir(to, 0o755))

	assert.NoError(t, copySELinuxContext(from, to))
}
