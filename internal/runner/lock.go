package runner

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"syscall"
)

// crossLock is an advisory file lock (flock) that coordinates borgmatic runs
// across processes, the scheduler daemon and an ad-hoc `run`, which the
// in-process semaphores cannot see. flock is released automatically when the
// file descriptor closes or the process dies, so a crash never strands a lock.
type crossLock struct {
	f *os.File
}

// tryCrossLock attempts a non-blocking exclusive flock on a file named for key
// under dir. It never waits: (lock, true) on success, (nil, false) when another
// process or descriptor already holds it, or an error for real failures. An
// empty dir disables cross-process locking (returns a nil lock that releases to
// a no-op), used by tests that don't exercise it.
func tryCrossLock(dir, key string) (*crossLock, bool, error) {
	if dir == "" {
		return nil, true, nil
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, false, err
	}
	path := filepath.Join(dir, lockFileName(key))
	// #nosec G304 -- path is derived from a hashed, in-code lock key, never user input
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, false, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, false, nil // held elsewhere
		}
		return nil, false, err
	}
	return &crossLock{f: f}, true, nil
}

// release unlocks and closes the lock file. Safe on a nil lock.
func (l *crossLock) release() {
	if l == nil || l.f == nil {
		return
	}
	_ = syscall.Flock(int(l.f.Fd()), syscall.LOCK_UN)
	_ = l.f.Close()
}

// lockFileName maps a lock key (e.g. "snapshots" or "repo:ssh://host/./path")
// to a stable, filesystem-safe file name. Repository keys can be URLs or paths,
// so the key is hashed; a short readable prefix aids debugging.
func lockFileName(key string) string {
	sum := sha256.Sum256([]byte(key))
	return "bm-" + hex.EncodeToString(sum[:8]) + ".lock"
}
