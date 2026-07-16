package runner

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestTryCrossLock_ExclusiveThenReleased(t *testing.T) {
	dir := t.TempDir()

	first, ok, err := tryCrossLock(dir, "repo:/repo/a")
	require.NoError(t, err)
	require.True(t, ok, "first acquisition succeeds")

	// A second attempt on the same key (a different fd, as another process
	// would) must not acquire while the first is held.
	second, ok, err := tryCrossLock(dir, "repo:/repo/a")
	require.NoError(t, err)
	assert.False(t, ok, "the lock is held; a concurrent attempt must fail, not block")
	assert.Nil(t, second)

	// A different key is independent.
	other, ok, err := tryCrossLock(dir, "repo:/repo/b")
	require.NoError(t, err)
	require.True(t, ok, "a different repo key is a separate lock")
	other.release()

	// Once released, the key is acquirable again.
	first.release()
	again, ok, err := tryCrossLock(dir, "repo:/repo/a")
	require.NoError(t, err)
	require.True(t, ok, "after release the lock is free")
	again.release()
}

func TestTryCrossLock_EmptyDirDisables(t *testing.T) {
	lock, ok, err := tryCrossLock("", "repo:/repo/a")
	require.NoError(t, err)
	assert.True(t, ok, "an empty lock dir disables cross-process locking")
	lock.release() // must be a safe no-op on a nil lock
}
