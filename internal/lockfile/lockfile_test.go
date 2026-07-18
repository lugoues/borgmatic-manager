package lockfile_test

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lugoues/borgmatic-manager/internal/lockfile"
)

// TestTryExclusive_HeldBlocksSecondAttempt is the invariant the whole
// cross-process design rests on: while one descriptor holds the lock, another
// attempt on the same path must report "held elsewhere" (nil, false, nil), not
// block and not error. flock is per open file description, so two Opens of the
// same path contend exactly as two separate processes would.
func TestTryExclusive_HeldBlocksSecondAttempt(t *testing.T) {
	path := filepath.Join(t.TempDir(), "x.lock")

	first, ok, err := lockfile.TryExclusive(path)
	require.NoError(t, err)
	require.True(t, ok, "first acquisition succeeds")
	require.NotNil(t, first)

	second, ok, err := lockfile.TryExclusive(path)
	require.NoError(t, err, "a held lock is a normal outcome, not an error")
	assert.False(t, ok, "the lock is held; a concurrent attempt must fail, not block")
	assert.Nil(t, second, "no Lock is returned when acquisition fails")
}

// TestTryExclusive_ReacquireAfterRelease confirms Release frees the lock for
// the next taker: the daemon and an ad-hoc run hand a repo/snapshot lock back
// and forth this way every cycle.
func TestTryExclusive_ReacquireAfterRelease(t *testing.T) {
	path := filepath.Join(t.TempDir(), "x.lock")

	first, ok, err := lockfile.TryExclusive(path)
	require.NoError(t, err)
	require.True(t, ok)

	first.Release()

	again, ok, err := lockfile.TryExclusive(path)
	require.NoError(t, err)
	require.True(t, ok, "after release the lock is free again")
	again.Release()
}

// TestTryExclusive_DistinctPathsIndependent confirms each key is its own lock:
// two different repositories (or a repo lock and the snapshot lock) never
// contend with each other.
func TestTryExclusive_DistinctPathsIndependent(t *testing.T) {
	dir := t.TempDir()

	a, ok, err := lockfile.TryExclusive(filepath.Join(dir, "a.lock"))
	require.NoError(t, err)
	require.True(t, ok)
	defer a.Release()

	b, ok, err := lockfile.TryExclusive(filepath.Join(dir, "b.lock"))
	require.NoError(t, err)
	require.True(t, ok, "a different path is a separate lock")
	b.Release()
}

// TestTryExclusive_CreatesParentDirectory confirms openLockFile makes the lock
// directory: callers pass paths under a state dir that may not exist yet on a
// first run.
func TestTryExclusive_CreatesParentDirectory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "deeper", "x.lock")

	lock, ok, err := lockfile.TryExclusive(path)
	require.NoError(t, err, "a missing parent directory is created, not an error")
	require.True(t, ok)
	lock.Release()

	assert.FileExists(t, path, "the lock file is left on disk for the next taker")
}

// TestExclusive_WaitsForHolderThenAcquires exercises the blocking path: a taker
// waits for the current holder and acquires the moment it releases. Used for the
// short read-modify-write critical sections around schedule state.
func TestExclusive_WaitsForHolderThenAcquires(t *testing.T) {
	path := filepath.Join(t.TempDir(), "x.lock")

	first, err := lockfile.Exclusive(path)
	require.NoError(t, err)

	type result struct {
		lock *lockfile.Lock
		err  error
	}
	acquired := make(chan result, 1)
	go func() {
		l, err := lockfile.Exclusive(path)
		acquired <- result{l, err}
	}()

	// While the lock is held, the waiter must not proceed.
	select {
	case <-acquired:
		t.Fatal("Exclusive acquired while another descriptor held the lock")
	case <-time.After(100 * time.Millisecond):
	}

	first.Release()

	select {
	case r := <-acquired:
		require.NoError(t, r.err)
		require.NotNil(t, r.lock, "the waiter acquires once the holder releases")
		r.lock.Release()
	case <-time.After(2 * time.Second):
		t.Fatal("Exclusive did not acquire within 2s of the holder releasing")
	}
}

// TestExclusive_ThenTryExclusiveFails confirms the blocking and non-blocking
// takers share one lock: a TryExclusive against a blocking-held lock reports
// "held", which is how an ad-hoc run backs off instead of waiting.
func TestExclusive_ThenTryExclusiveFails(t *testing.T) {
	path := filepath.Join(t.TempDir(), "x.lock")

	held, err := lockfile.Exclusive(path)
	require.NoError(t, err)
	defer held.Release()

	lock, ok, err := lockfile.TryExclusive(path)
	require.NoError(t, err)
	assert.False(t, ok, "a blocking-held lock is seen as held by a non-blocking attempt")
	assert.Nil(t, lock)
}

// readOnlyDir returns a directory the current user cannot write to, skipping the
// test when that cannot be enforced (root ignores the permission bits, so the
// error paths under test would never fire). Perms are restored on cleanup so
// t.TempDir's own teardown can remove the tree.
func readOnlyDir(t *testing.T) string {
	t.Helper()
	if os.Geteuid() == 0 {
		t.Skip("running as root: file-mode permission bits are not enforced, so the failure path cannot be exercised")
	}
	dir := filepath.Join(t.TempDir(), "ro")
	require.NoError(t, os.Mkdir(dir, 0o500)) // r-x: no write, so creating a file inside fails
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
	return dir
}

// TestTryExclusive_OpenErrorPropagates confirms a real I/O failure (a lock file
// that cannot be created) surfaces as an error, distinct from the (nil, false,
// nil) "held elsewhere" outcome, the reaper and schedule store branch on that
// difference to decide whether to back off or to abort a mutation.
func TestTryExclusive_OpenErrorPropagates(t *testing.T) {
	path := filepath.Join(readOnlyDir(t), "x.lock")

	lock, ok, err := lockfile.TryExclusive(path)
	require.Error(t, err, "an uncreatable lock file is a real failure, not 'held elsewhere'")
	assert.False(t, ok)
	assert.Nil(t, lock)
}

// TestTryExclusive_MkdirErrorPropagates confirms the same when the lock
// directory itself cannot be created (its parent is unwritable).
func TestTryExclusive_MkdirErrorPropagates(t *testing.T) {
	path := filepath.Join(readOnlyDir(t), "subdir", "x.lock")

	lock, ok, err := lockfile.TryExclusive(path)
	require.Error(t, err, "a lock directory that cannot be created is a real failure")
	assert.False(t, ok)
	assert.Nil(t, lock)
}

// TestExclusive_OpenErrorPropagates confirms the blocking taker reports the same
// I/O failure rather than blocking forever on a lock it can never open.
func TestExclusive_OpenErrorPropagates(t *testing.T) {
	path := filepath.Join(readOnlyDir(t), "x.lock")

	lock, err := lockfile.Exclusive(path)
	require.Error(t, err)
	assert.Nil(t, lock)
}

// TestRelease_NilAndDoubleReleaseAreSafe: Release is documented safe on a nil
// Lock (an empty lock dir returns one), and a defensive double Release must not
// panic.
func TestRelease_NilAndDoubleReleaseAreSafe(t *testing.T) {
	var nilLock *lockfile.Lock
	assert.NotPanics(t, nilLock.Release, "Release on a nil Lock is a no-op")

	path := filepath.Join(t.TempDir(), "x.lock")
	lock, ok, err := lockfile.TryExclusive(path)
	require.NoError(t, err)
	require.True(t, ok)

	assert.NotPanics(t, lock.Release)
	assert.NotPanics(t, lock.Release, "a second Release must not panic")
}
