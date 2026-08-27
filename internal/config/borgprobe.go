package config

import (
	"context"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"time"
)

// borgProbeCache memoizes borg --version probes by path and file identity
// (device, inode, size, mtime: an atomic swap can preserve the latter two).
// Only successes are cached: a transient failure remembered against an
// unchanged file would refuse a group on every future generation until the
// daemon restarts. Package-level, because a generator is rebuilt each cycle
// and a per-generator cache would exec every configured borg every cycle.
var borgProbeCache = struct {
	mu sync.Mutex
	m  map[string]borgProbeEntry
}{m: map[string]borgProbeEntry{}}

type borgProbeEntry struct {
	mtime time.Time
	size  int64
	dev   uint64
	ino   uint64
	out   string
}

// cachedBorgVersionProbe runs `path --version` through the cache.
func cachedBorgVersionProbe(path string) (string, error) {
	info, err := os.Stat(path)
	if err != nil {
		return "", err
	}
	var dev, ino uint64
	if st, ok := info.Sys().(*syscall.Stat_t); ok {
		dev, ino = st.Dev, st.Ino
	}
	borgProbeCache.mu.Lock()
	e, hit := borgProbeCache.m[path]
	borgProbeCache.mu.Unlock()
	if hit && e.mtime.Equal(info.ModTime()) && e.size == info.Size() && e.dev == dev && e.ino == ino {
		return e.out, nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, path, "--version") // #nosec G204 -- probing the operator-configured borg
	// A wedged probe's descendant holding stdout must not turn the bound
	// into an unbounded wait.
	cmd.WaitDelay = 5 * time.Second
	out, err := cmd.Output()
	if err != nil {
		return string(out), err
	}
	borgProbeCache.mu.Lock()
	borgProbeCache.m[path] = borgProbeEntry{mtime: info.ModTime(), size: info.Size(), dev: dev, ino: ino, out: string(out)}
	borgProbeCache.mu.Unlock()
	return string(out), nil
}

// plausibleBorgVersion reports whether v looks like a release version: a
// real borg always prints one, and anything that does not is not borg.
func plausibleBorgVersion(v string) bool {
	if v == "" || v[0] < '0' || v[0] > '9' {
		return false
	}
	for _, r := range v {
		if r == '.' {
			return true
		}
	}
	return false
}

// borgVersionAtLeast parses a bare version like "1.4.4" against min.
// Unparseable versions pass: dev builds must not refuse groups.
func borgVersionAtLeast(version string, min [3]int) bool {
	var parts [3]int
	n := 0
	num := 0
	inNum := false
	for _, r := range version {
		switch {
		case r >= '0' && r <= '9':
			num = num*10 + int(r-'0')
			inNum = true
		case r == '.' && inNum && n < 2:
			parts[n] = num
			n++
			num = 0
			inNum = false
		default:
			if !inNum && n == 0 {
				return true // unparseable
			}
			r = 0 // stop at the first non-numeric suffix
		}
		if r == 0 {
			break
		}
	}
	if inNum && n < 3 {
		parts[n] = num
		n++
	}
	if n == 0 {
		return true
	}
	for i := range 3 {
		if parts[i] != min[i] {
			return parts[i] > min[i]
		}
	}
	return true
}
