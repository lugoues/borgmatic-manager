package discovery

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// The lexical containment check cannot see symlinks, so this one exists to. A
// dangling link is the subtle case: EvalSymlinks reports it exactly like a
// missing file, but a missing file is inert while a dangling link is a
// redirection that starts working the moment its target is created, which by
// backup time it can be.
func TestCheckNoSymlinkEscape(t *testing.T) {
	outside := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(outside, "shadow"), []byte("x"), 0o600))

	newVolume := func(t *testing.T) string {
		t.Helper()
		return t.TempDir()
	}

	t.Run("a regular file inside the volume is accepted", func(t *testing.T) {
		vol := newVolume(t)
		p := filepath.Join(vol, "db.sqlite")
		require.NoError(t, os.WriteFile(p, []byte("x"), 0o600))
		require.NoError(t, checkNoSymlinkEscape(vol, p))
	})

	t.Run("a database that does not exist yet is accepted", func(t *testing.T) {
		vol := newVolume(t)
		require.NoError(t, checkNoSymlinkEscape(vol, filepath.Join(vol, "data", "db.sqlite")))
	})

	t.Run("a symlink to an existing file outside the volume is rejected", func(t *testing.T) {
		vol := newVolume(t)
		link := filepath.Join(vol, "link")
		require.NoError(t, os.Symlink(filepath.Join(outside, "shadow"), link))
		require.Error(t, checkNoSymlinkEscape(vol, link))
	})

	t.Run("a symlinked directory leading outside is rejected", func(t *testing.T) {
		vol := newVolume(t)
		link := filepath.Join(vol, "dir")
		require.NoError(t, os.Symlink(outside, link))
		require.Error(t, checkNoSymlinkEscape(vol, filepath.Join(link, "shadow")))
	})

	t.Run("a dangling symlink is rejected, not read as a missing file", func(t *testing.T) {
		vol := newVolume(t)
		link := filepath.Join(vol, "db.sqlite")
		require.NoError(t, os.Symlink(filepath.Join(outside, "not-yet-created"), link))
		err := checkNoSymlinkEscape(vol, link)
		require.Error(t, err, "the target can be created after discovery, and the root backup would follow it")
		require.Contains(t, err.Error(), "target does not exist")
	})

	t.Run("a dangling intermediate symlink is rejected too", func(t *testing.T) {
		vol := newVolume(t)
		link := filepath.Join(vol, "dir")
		require.NoError(t, os.Symlink(filepath.Join(outside, "no-such-dir"), link))
		require.Error(t, checkNoSymlinkEscape(vol, filepath.Join(link, "db.sqlite")))
	})

	t.Run("a symlink staying inside the volume is accepted", func(t *testing.T) {
		vol := newVolume(t)
		real := filepath.Join(vol, "real.sqlite")
		require.NoError(t, os.WriteFile(real, []byte("x"), 0o600))
		link := filepath.Join(vol, "alias.sqlite")
		require.NoError(t, os.Symlink(real, link))
		require.NoError(t, checkNoSymlinkEscape(vol, link))
	})
}
