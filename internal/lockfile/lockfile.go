package lockfile

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// Lock is a held advisory lock. Release it when done.
type Lock struct {
	f *os.File
}

// TryExclusive takes an exclusive lock without waiting; (nil, false, nil)
// means another holder, a normal outcome, not an error.
func TryExclusive(path string) (*Lock, bool, error) {
	f, err := openLockFile(path)
	if err != nil {
		return nil, false, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, false, nil // held elsewhere
		}
		return nil, false, fmt.Errorf("locking %s: %w", path, err)
	}
	return &Lock{f: f}, true, nil
}

// Exclusive takes an exclusive lock, waiting for the current holder; for
// long-running work prefer TryExclusive so callers can back off.
func Exclusive(path string) (*Lock, error) {
	f, err := openLockFile(path)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("locking %s: %w", path, err)
	}
	return &Lock{f: f}, nil
}

// Release unlocks and closes the lock file. Safe on a nil Lock.
func (l *Lock) Release() {
	if l == nil || l.f == nil {
		return
	}
	_ = syscall.Flock(int(l.f.Fd()), syscall.LOCK_UN)
	_ = l.f.Close()
}

func openLockFile(path string) (*os.File, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("creating lock directory: %w", err)
	}
	// #nosec G304 -- lock paths are built by the manager from its own state dir
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("opening lock file %s: %w", path, err)
	}
	return f, nil
}
