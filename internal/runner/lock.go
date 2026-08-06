package runner

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"path/filepath"

	"github.com/lugoues/borgmatic-manager/internal/lockfile"
)

// ErrLockedByAnotherProcess reports another process holds this group's repo or
// snapshot flock: a skip, not a failure. Callers must check it with errors.Is.
var ErrLockedByAnotherProcess = errors.New("group is locked by another process")

// tryCrossLock non-blockingly flocks the file for key under dir: (lock, true),
// (nil, false) when held elsewhere, error otherwise. Empty dir disables locking.
func tryCrossLock(dir, key string) (*lockfile.Lock, bool, error) {
	if dir == "" {
		return nil, true, nil
	}
	return lockfile.TryExclusive(filepath.Join(dir, lockFileName(key)))
}

// lockFileName hashes a lock key (may be a URL or path) to a filesystem-safe name.
func lockFileName(key string) string {
	sum := sha256.Sum256([]byte(key))
	return "bm-" + hex.EncodeToString(sum[:8]) + ".lock"
}

// GroupLockKey is the cross-process lock key serializing runs of one group.
func GroupLockKey(group string) string { return "group:" + group }

// RepoLockKey is the cross-process lock key serializing runs against one
// canonicalized repository.
func RepoLockKey(repo string) string { return "repo:" + repo }

// TryCrossLock non-blockingly takes the named cross-process lock under
// lockDir: (lock, true) on success, (nil, false) when another process holds
// it. Exported so restore-volume can exclude the daemon's backups while it
// rewrites a volume.
func TryCrossLock(lockDir, key string) (*lockfile.Lock, bool, error) {
	return tryCrossLock(lockDir, key)
}

// PendingLockPath is the path of a run's liveness lock, held for the run's
// lifetime so startup reconciliation can tell a live run from a crashed one.
func PendingLockPath(lockDir, runID string) string {
	sum := sha256.Sum256([]byte(runID))
	return filepath.Join(lockDir, "pending-"+hex.EncodeToString(sum[:8])+".lock")
}
