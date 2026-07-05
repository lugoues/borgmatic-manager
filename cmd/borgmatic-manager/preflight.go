package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/lugoues/borgmatic-manager/internal/config"
)

// Version floors: below borgmatic 2.1.0 the warning detection and exit-code
// semantics don't hold; below Borg 1.4 snapshot hooks record snapshot paths
// in archives, a silent defect discovered at restore time.
var (
	minBorgmatic = [3]int{2, 1, 0}
	minBorg      = [3]int{1, 4, 0}
)

// wellKnownBorgmaticPaths are probed when borgmatic is not on PATH; uv and pipx
// install to /root/.local/bin, which is off systemd's default PATH.
var wellKnownBorgmaticPaths = []string{
	"/root/.local/bin/borgmatic",
	"/usr/local/bin/borgmatic",
}

type preflightResult struct {
	borgmaticPath    string
	borgmaticVersion string
	runTimeout       time.Duration
}

// preflight validates everything a doomed deployment would otherwise learn one
// failed cycle at a time.
func preflight(ctx context.Context, e *env) (*preflightResult, error) {
	res := &preflightResult{}

	// manager.period must parse, a typo would otherwise silently disable
	// periodic backups (the scheduler goroutine would just exit).
	if _, err := time.ParseDuration(e.cfg.Manager.Period); err != nil {
		return nil, fmt.Errorf("invalid manager.period %q: %w", e.cfg.Manager.Period, err)
	}

	timeout, timeoutErr := runTimeoutFromConfig(e.cfg)
	if timeoutErr != nil {
		return nil, timeoutErr
	}
	res.runTimeout = timeout

	if _, err := e.rt.ListVolumes(ctx); err != nil {
		return nil, fmt.Errorf("container runtime socket check failed: %w", err)
	}

	path, err := resolveBorgmatic(e.cfg)
	if err != nil {
		return nil, err
	}
	res.borgmaticPath = path

	bmVersion, err := commandOutput(ctx, path, "--version")
	if err != nil {
		return nil, fmt.Errorf("running %s --version: %w", path, err)
	}
	res.borgmaticVersion = strings.TrimSpace(bmVersion)
	if !versionAtLeast(res.borgmaticVersion, minBorgmatic) {
		return nil, fmt.Errorf("borgmatic %s is too old: need >= %d.%d.%d (distro packages often lag; install with 'uv tool install borgmatic' or pipx)",
			res.borgmaticVersion, minBorgmatic[0], minBorgmatic[1], minBorgmatic[2])
	}

	// Borg version floor: hard requirement when snapshot hooks are
	// configured (archive path recording), advisory otherwise.
	snapshotsConfigured := snapshotHooksConfigured(e.cfg, e.groupOverrides)
	if borgPath, err := exec.LookPath("borg"); err != nil {
		if snapshotsConfigured {
			return nil, fmt.Errorf("borg not found on PATH but snapshot hooks are configured")
		}
		slog.Warn("borg not found on PATH; borgmatic will fail until it is installed")
	} else if out, err := commandOutput(ctx, borgPath, "--version"); err == nil {
		// "borg 1.4.4"
		fields := strings.Fields(out)
		borgVersion := fields[len(fields)-1]
		if !versionAtLeast(borgVersion, minBorg) {
			msg := fmt.Sprintf("borg %s is older than %d.%d: snapshot-hook archives would record snapshot paths instead of original paths", borgVersion, minBorg[0], minBorg[1])
			if snapshotsConfigured {
				return nil, fmt.Errorf("%s, upgrade borg or disable the snapshot hooks", msg)
			}
			slog.Warn(msg + " (not fatal: no snapshot hooks configured)")
		}
	}

	// docker/podman CLI: borgmatic shells out to it for container-mode DB
	// connections. Generation warns per group; this is the generic heads-up.
	if _, err := exec.LookPath("docker"); err != nil {
		if _, err := exec.LookPath("podman"); err != nil {
			slog.Warn("neither docker nor podman CLI found on PATH; container-mode database backups will fail")
		}
	}

	return res, nil
}

// resolveBorgmatic finds the borgmatic binary: BORGMATIC_PATH env, then the
// manager.borgmatic_path config option, then PATH, then well-known locations.
func resolveBorgmatic(cfg *config.ManagerConfig) (string, error) {
	if p := os.Getenv("BORGMATIC_PATH"); p != "" {
		return p, nil
	}
	if p := cfg.Manager.BorgmaticPath; p != "" {
		return p, nil
	}
	if p, err := exec.LookPath("borgmatic"); err == nil {
		return p, nil
	}
	for _, p := range wellKnownBorgmaticPaths {
		if info, err := os.Stat(p); err == nil && !info.IsDir() {
			return p, nil
		}
	}
	return "", fmt.Errorf("borgmatic not found: install it (e.g. 'uv tool install borgmatic') or set manager.borgmatic_path / BORGMATIC_PATH")
}

// snapshotHooksConfigured reports whether any btrfs/zfs/lvm hook appears in
// the global borgmatic defaults or any per-group override.
func snapshotHooksConfigured(cfg *config.ManagerConfig, overrides map[string]map[string]interface{}) bool {
	hooks := []string{"btrfs", "zfs", "lvm"}
	for _, h := range hooks {
		if _, ok := cfg.Borgmatic[h]; ok {
			return true
		}
	}
	for _, override := range overrides {
		for _, h := range hooks {
			if _, ok := override[h]; ok {
				return true
			}
		}
	}
	return false
}

func commandOutput(ctx context.Context, name string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, name, args...).Output() // #nosec G204 -- version preflight of operator-configured binaries
	return string(out), err
}

// versionAtLeast parses a bare semver like "2.1.6" and compares to min.
// Unparseable versions pass (dev builds shouldn't hard-fail the preflight).
func versionAtLeast(version string, min [3]int) bool {
	parts := strings.SplitN(strings.TrimSpace(version), ".", 3)
	nums := [3]int{}
	for i := 0; i < len(parts) && i < 3; i++ {
		// Strip any suffix like "6.dev0" -> "6".
		numStr := parts[i]
		if idx := strings.IndexFunc(numStr, func(r rune) bool { return r < '0' || r > '9' }); idx >= 0 {
			numStr = numStr[:idx]
		}
		n, err := strconv.Atoi(numStr)
		if err != nil {
			return true
		}
		nums[i] = n
	}
	for i := 0; i < 3; i++ {
		if nums[i] != min[i] {
			return nums[i] > min[i]
		}
	}
	return true
}
