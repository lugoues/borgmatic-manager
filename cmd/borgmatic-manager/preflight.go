package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/lugoues/borgmatic-manager/internal/config"
	"github.com/lugoues/borgmatic-manager/internal/models"
	"github.com/lugoues/borgmatic-manager/internal/toolchain"
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

	// A typo here would otherwise silently disable periodic backups, and a
	// zero or negative period would hot-loop the cycle timer.
	if _, err := e.cfg.ParsedPeriod(); err != nil {
		return nil, err
	}

	timeout, timeoutErr := runTimeoutFromConfig(e.cfg)
	if timeoutErr != nil {
		return nil, timeoutErr
	}
	res.runTimeout = timeout

	if _, err := e.rt.ListVolumes(ctx); err != nil {
		return nil, fmt.Errorf("container runtime socket check failed (socket %s; set CONTAINER_SOCKET to override): %w", e.rt.SocketPath(), err)
	}

	// borg first: it is the engine everything drives, and it is deliberately
	// host-owned (its repository format and CLI must match what the operator
	// uses by hand against the same repositories). Checked before borgmatic so
	// a missing borg fails cleanly instead of provisioning a toolchain for a
	// deployment that cannot run anyway. Discovery runs first because labels
	// may point groups at their own borg (config.local_path), and demanding a
	// PATH borg no group will invoke would fail a working deployment.
	backupState, _, discErr := e.discoverMerged(ctx, slog.Default())
	if discErr != nil {
		return nil, fmt.Errorf("discovering groups for the borg check: %w", discErr)
	}
	if err := checkBorg(ctx, e.cfg, e.groupOverrides, backupState); err != nil {
		return nil, err
	}

	path, err := e.launchBorgmatic(ctx)
	if err != nil {
		return nil, err
	}
	res.borgmaticPath = path

	bmVersion, err := commandOutput(ctx, path, "--version")
	if err != nil {
		return nil, fmt.Errorf("running %s --version: %w", path, err)
	}
	res.borgmaticVersion = strings.TrimSpace(bmVersion)
	// Plausibility before the floor: an explicit borgmatic_path/BORGMATIC_PATH
	// shim exiting zero with garbage would otherwise ride versionAtLeast's
	// unparseable-passes rule into recording no-op runs as backups. Toolchain
	// and host candidates were vetted already; this catches the override path.
	if !toolchain.PlausibleReportedVersion(res.borgmaticVersion) {
		return nil, fmt.Errorf("borgmatic at %s reports no usable version (output %q); fix manager.borgmatic_path / BORGMATIC_PATH", path, res.borgmaticVersion)
	}
	if !versionAtLeast(borgmaticVersionOf(res.borgmaticVersion), minBorgmatic) {
		return nil, fmt.Errorf("borgmatic %s is too old: need >= %d.%d.%d (unset manager.borgmatic_path / BORGMATIC_PATH to let the manager provision its own)",
			res.borgmaticVersion, minBorgmatic[0], minBorgmatic[1], minBorgmatic[2])
	}

	// docker/podman CLI: generated helper/exec dump commands invoke it.
	// Generation warns per group; this is the generic heads-up.
	if detectContainerCLI(e.cfg, e.rt.SocketPath()) == "" {
		slog.Warn("neither docker nor podman CLI found on PATH; database dump commands will fail")
	}

	return res, nil
}

// detectContainerCLI picks the CLI for generated dump commands: explicit config
// wins; a podman socket implies podman (the docker CLI would target the wrong
// daemon); otherwise the first of docker/podman on PATH, else empty.
func detectContainerCLI(cfg *config.ManagerConfig, socketPath string) string {
	const docker, podman = "docker", "podman"
	if cli := cfg.Manager.ContainerCLI; cli != "" {
		return cli
	}
	if strings.Contains(socketPath, podman) {
		if _, err := exec.LookPath(podman); err == nil {
			return podman
		}
	}
	for _, cli := range []string{docker, podman} {
		if _, err := exec.LookPath(cli); err == nil {
			return cli
		}
	}
	return ""
}

// checkBorg fails the launch when a borg the generated configs will invoke is
// absent: without the engine nothing can back up or restore, and discovering
// that one failed cycle at a time serves nobody. The version floor is hard
// only for a borg that some snapshot-hook-using group actually invokes
// (archive path recording is silently wrong below it); a snapshot-free group
// may point local_path at an older borg without stopping everything else.
func checkBorg(ctx context.Context, cfg *config.ManagerConfig, overrides map[string]config.GroupOverride, state *models.BackupState) error {
	for _, bc := range borgCommands(cfg, overrides, state) {
		borgPath, err := resolveBorgCommand(bc.command)
		if err != nil {
			return fmt.Errorf("borg (%s) not found: %w; install borg from your distribution or fix the path "+
				"(borg stays host-installed; the manager only provisions borgmatic)", bc.source, err)
		}
		out, ok := cachedBorgVersion(ctx, borgPath)
		fields := strings.Fields(out)
		if !ok || len(fields) == 0 {
			// Existing but unrunnable (a broken shim, a missing loader): every
			// group invoking it would fail, which is exactly what this check
			// exists to say before the first cycle does.
			return fmt.Errorf("borg (%s): running %s --version failed (output %q)", bc.source, borgPath, strings.TrimSpace(out))
		}
		// "borg 1.4.4". A zero-exit shim printing garbage must not pass:
		// versionAtLeast deliberately waves unparseable tokens through the
		// floor, so plausibility is judged first.
		borgVersion := fields[len(fields)-1]
		if !toolchain.PlausibleReportedVersion(out) {
			return fmt.Errorf("borg (%s): %s reports no usable version (output %q)", bc.source, borgPath, strings.TrimSpace(out))
		}
		if !versionAtLeast(borgVersion, minBorg) {
			msg := fmt.Sprintf("borg %s (%s) is older than %d.%d: snapshot-hook archives would record snapshot paths instead of original paths", borgVersion, bc.source, minBorg[0], minBorg[1])
			if bc.snapshots {
				return fmt.Errorf("%s, upgrade borg or disable the snapshot hooks", msg)
			}
			slog.Warn(msg + " (not fatal: no group using this borg has snapshot hooks)")
		}
	}
	return nil
}

// borgVersionCache memoizes --version probes by path and file identity, so
// the per-cycle borg recheck stats an unchanged binary instead of execing it.
var borgVersionCache = struct {
	mu sync.Mutex
	m  map[string]borgVersionEntry
}{m: map[string]borgVersionEntry{}}

type borgVersionEntry struct {
	mtime    time.Time
	size     int64
	dev      uint64
	ino      uint64
	out      string
	probedAt time.Time
}

// borgVersionTTL bounds how long a cached verdict stands even for an
// unchanged file: a wrapper script's inode is stable while whatever it
// delegates to changes underneath it.
const borgVersionTTL = time.Hour

// fileIdentity is (device, inode): size and mtime alone can be preserved
// across an atomic replacement (a symlink repointed at a packaged binary with
// matching metadata), and a version cache blind to that would keep enforcing
// the old binary's verdict on the new one.
func fileIdentity(info os.FileInfo) (uint64, uint64) {
	if st, ok := info.Sys().(*syscall.Stat_t); ok {
		return st.Dev, st.Ino
	}
	return 0, 0
}

// cachedBorgVersion is commandOutput --version through the cache. Only
// successes are cached: a transient probe failure (a timeout under load)
// cached against an unchanged file would skip every future cycle until the
// daemon restarts, turning one bad moment into a silent standstill. Failures
// re-probe next cycle, which is the retry.
func cachedBorgVersion(ctx context.Context, path string) (string, bool) {
	info, statErr := os.Stat(path)
	if statErr != nil {
		return "", false
	}
	dev, ino := fileIdentity(info)
	borgVersionCache.mu.Lock()
	e, hit := borgVersionCache.m[path]
	borgVersionCache.mu.Unlock()
	if hit && e.mtime.Equal(info.ModTime()) && e.size == info.Size() && e.dev == dev && e.ino == ino &&
		time.Since(e.probedAt) < borgVersionTTL {
		return e.out, true
	}
	out, err := commandOutput(ctx, path, "--version")
	if err != nil {
		return out, false
	}
	borgVersionCache.mu.Lock()
	borgVersionCache.m[path] = borgVersionEntry{mtime: info.ModTime(), size: info.Size(), dev: dev, ino: ino, out: out, probedAt: time.Now()}
	borgVersionCache.mu.Unlock()
	return out, true
}

// borgCommand is one borg executable the generated configs will invoke, where
// its name came from (for error messages), and whether any group invoking it
// has snapshot hooks (which hardens the version floor for it).
type borgCommand struct {
	command   string
	source    string
	snapshots bool
}

// borgCommands names the one borg every group runs on: "borg" on PATH
// unless manager.yaml sets borgmatic's local_path. The borg binary is
// global-only by design (generation strips group-level local_path with a
// warning: from a label it would be root code execution for anyone who can
// label a container, and per-group engines were a footgun), so the default is
// always required and there is exactly one command to validate. Snapshot
// hooks from any source harden its version floor: every group, label-defined
// hooks included, runs on this borg.
func borgCommands(cfg *config.ManagerConfig, overrides map[string]config.GroupOverride, state *models.BackupState) []borgCommand {
	hooks := hasSnapshotHooks(cfg.Borgmatic)
	for _, o := range overrides {
		if o.Borgmatic != nil {
			hooks = hooks || hasSnapshotHooks(o.Borgmatic)
		}
	}
	if state != nil {
		for _, grp := range state.Groups {
			for _, lc := range grp.LabelConfigs {
				hooks = hooks || hasSnapshotHooks(lc)
			}
		}
	}
	if globalLP, _ := cfg.Borgmatic["local_path"].(string); globalLP != "" {
		return []borgCommand{{command: globalLP, source: "manager.yaml local_path", snapshots: hooks}}
	}
	return []borgCommand{{command: "borg", source: "PATH", snapshots: hooks}}
}

// hasSnapshotHooks reports whether one config map enables a btrfs/zfs/lvm hook.
func hasSnapshotHooks(m map[string]interface{}) bool {
	for _, h := range config.SnapshotHookKeys {
		if _, ok := m[h]; ok {
			return true
		}
	}
	return false
}

// resolveBorgCommand resolves a configured borg the way borgmatic will: a
// bare name through PATH, anything with a separator as a direct executable.
func resolveBorgCommand(command string) (string, error) {
	if !strings.ContainsRune(command, os.PathSeparator) {
		return exec.LookPath(command)
	}
	info, err := os.Stat(command)
	if err != nil {
		return "", err
	}
	if info.IsDir() || info.Mode().Perm()&0o111 == 0 {
		return "", fmt.Errorf("%s is not an executable file", command)
	}
	return command, nil
}

// launchBorgmatic picks the borgmatic a launch runs with, provisioning the
// manager's own toolchain when nothing usable exists:
//
//  1. An explicit manager.borgmatic_path / BORGMATIC_PATH always wins and
//     disables provisioning entirely: the operator chose.
//  2. An existing toolchain is used; if its pins are stale it is refreshed
//     (degrading to the stale one when the refresh cannot download).
//  3. With no toolchain, a healthy host install is respected: no downloads
//     behind the back of a host that manages borgmatic itself.
//  4. Only a host whose borgmatic is missing, broken, or too old provisions
//     the toolchain. A pipx upgrade that broke its environments lands here
//     and heals instead of failing every cycle.
func (e *env) launchBorgmatic(ctx context.Context) (string, error) {
	if p := explicitBorgmaticPath(e.cfg); p != "" {
		return p, nil
	}
	tc := toolchain.New(e.toolchainDir(), slog.Default())
	if tc.Exists() {
		// Any provisioned toolchain, however damaged (a vanished launcher
		// included), is repaired rather than abandoned: Ensure hands a fresh
		// healthy one straight back, refreshes a stale one, and reprovisions
		// a broken one. Falling back to the host here would silently
		// re-couple the daemon to the host's Python packaging.
		//
		// Stripped BEFORE the probes inside Ensure, not after: a poisoned
		// PYTHONHOME failing the health checks would refuse the very
		// toolchain that exists to escape it.
		stripHostPythonEnv()
		return tc.Ensure(ctx)
	}
	if p, ok := healthyHostBorgmatic(ctx); ok {
		return p, nil
	}
	stripHostPythonEnv()
	return tc.Ensure(ctx)
}

// healthyHostBorgmatic reports the first host-installed borgmatic that
// actually runs, reports a plausible version (a corrupted shim exiting zero
// with garbage must not be selected: versionAtLeast deliberately passes
// unparseable tokens), and meets the version floor. Candidates are probed in
// priority order and an unhealthy one does not hide a healthy one behind it:
// a broken PATH shim with a good /usr/local install must not force
// provisioning.
func healthyHostBorgmatic(ctx context.Context) (string, bool) {
	for _, p := range hostBorgmaticCandidates() {
		out, err := commandOutput(ctx, p, "--version")
		if err != nil {
			slog.Warn("host borgmatic is broken; trying the next candidate", "path", p, "error", err)
			continue
		}
		if !toolchain.PlausibleReportedVersion(out) {
			slog.Warn("host borgmatic reports no usable version; trying the next candidate", "path", p, "reported", strings.TrimSpace(out))
			continue
		}
		if v := borgmaticVersionOf(out); !versionAtLeast(v, minBorgmatic) {
			slog.Warn("host borgmatic is too old; trying the next candidate", "path", p, "version", v)
			continue
		}
		return p, true
	}
	return "", false
}

// borgmaticVersionOf extracts the version token from `borgmatic --version`
// output: bare ("2.1.7") in current releases, prefixed ("borgmatic 2.1.7") in
// some packagings. versionAtLeast deliberately passes unparseable strings
// (dev builds), so handing it the prefixed form whole would wave an old
// install through the floor.
func borgmaticVersionOf(out string) string {
	fields := strings.Fields(out)
	if len(fields) == 0 {
		return ""
	}
	return fields[len(fields)-1]
}

// hostBorgmaticCandidates lists host borgmatic locations in priority order:
// PATH, then the well-known install dirs systemd's PATH misses.
func hostBorgmaticCandidates() []string {
	var out []string
	seen := map[string]bool{}
	if p, err := exec.LookPath("borgmatic"); err == nil {
		out = append(out, p)
		seen[p] = true
	}
	for _, p := range wellKnownBorgmaticPaths {
		if seen[p] {
			continue
		}
		if info, err := os.Stat(p); err == nil && !info.IsDir() {
			out = append(out, p)
		}
	}
	return out
}

// sanitizedProbe is commandOutput --version with the host's Python
// configuration removed from the probe's own environment only.
func sanitizedProbe(ctx context.Context, bin string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, "--version") // #nosec G204 -- probing the toolchain launcher
	cmd.Env = toolchain.SanitizedEnviron()
	cmd.WaitDelay = 5 * time.Second
	out, err := cmd.Output()
	return string(out), err
}

// stripHostPythonEnv removes the host's Python environment configuration
// once a toolchain borgmatic is selected. The managed launcher's shebang
// still honors PYTHONHOME and PYTHONPATH, so a service environment carrying
// them could redirect the managed interpreter's standard library or shadow
// borgmatic's dependencies with host packages, re-coupling every backup to
// the host Python this toolchain exists to escape. Host installs selected
// explicitly keep their environment untouched.
func stripHostPythonEnv() {
	for _, k := range []string{"PYTHONHOME", "PYTHONPATH", "PYTHONSTARTUP"} {
		if v := os.Getenv(k); v != "" {
			slog.Warn("unsetting host Python variable for the toolchain borgmatic", "variable", k)
			_ = os.Unsetenv(k)
		}
	}
}

// explicitBorgmaticPath is the operator's override: env, then config.
func explicitBorgmaticPath(cfg *config.ManagerConfig) string {
	if p := os.Getenv("BORGMATIC_PATH"); p != "" {
		return p
	}
	return cfg.Manager.BorgmaticPath
}

// resolveBorgmatic finds the borgmatic binary for commands that must not
// provision (restore, config rendering, passthrough): the explicit override,
// then an already-provisioned toolchain, then the host. The toolchain
// candidate is probed before it is preferred: an urgent restore must fall
// through to a working host install rather than fail on a toolchain that
// merely exists, and only the next daemon launch repairs it. The daemon and
// one-shot runs go through launchBorgmatic instead, which can provision.
func resolveBorgmatic(ctx context.Context, cfg *config.ManagerConfig, toolchainDir string) (string, error) {
	if p := explicitBorgmaticPath(cfg); p != "" {
		// Probed like every other candidate: a pinned launcher that decayed
		// into a zero-exit no-op would otherwise let a merge restore report
		// "restore complete" having extracted nothing. The operator pinned
		// this path, so a broken pin is an error, never a silent fallback.
		if out, err := commandOutput(ctx, p, "--version"); err != nil || !toolchain.PlausibleReportedVersion(out) {
			return "", fmt.Errorf("borgmatic at %s (manager.borgmatic_path / BORGMATIC_PATH) is not usable: %v (output %q); fix the override", p, err, strings.TrimSpace(out))
		}
		return p, nil
	}
	if p, ok := toolchain.CurrentBorgmatic(toolchainDir); ok {
		// Probed on a sanitized environment WITHOUT touching the process env:
		// the strip commits only once the probe passes. A broken toolchain
		// must hand restore and passthrough an unaltered host fallback, which
		// may itself rely on the very variables the toolchain sheds.
		if out, err := sanitizedProbe(ctx, p); err == nil && toolchain.PlausibleReportedVersion(out) {
			stripHostPythonEnv()
			return p, nil
		}
		slog.Warn("toolchain borgmatic is broken or reports no usable version; falling back to a host install for this command (the next daemon launch repairs the toolchain)", "path", p)
	}
	// Host candidates are probed lightly (runnable, plausible version) rather
	// than floor-checked: for a restore, an old borgmatic that works beats a
	// refusal. A broken first candidate must not hide a working later one.
	for _, p := range hostBorgmaticCandidates() {
		if out, err := commandOutput(ctx, p, "--version"); err == nil && toolchain.PlausibleReportedVersion(out) {
			return p, nil
		}
		slog.Warn("host borgmatic candidate is not usable; trying the next", "path", p)
	}
	return "", fmt.Errorf("borgmatic not found: start the daemon or a run once to let the manager provision its own, install it (e.g. 'uv tool install borgmatic'), or set manager.borgmatic_path / BORGMATIC_PATH")
}

func commandOutput(ctx context.Context, name string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...) // #nosec G204 -- version preflight of operator-configured binaries
	// Its own process group, killed whole on timeout: the per-cycle borg gate
	// probes through here with failures deliberately uncached, and a hanging
	// launcher's surviving descendant per cycle would slowly exhaust the
	// host. WaitDelay additionally releases the output-pipe wait, so a
	// descendant holding stdout cannot turn the bound into an unbounded one.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	cmd.WaitDelay = 5 * time.Second
	out, err := cmd.Output()
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
