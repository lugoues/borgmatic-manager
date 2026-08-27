package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"sync"
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
		// "borg 1.4.4"
		borgVersion := fields[len(fields)-1]
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
	mtime time.Time
	size  int64
	out   string
	ok    bool
}

// cachedBorgVersion is commandOutput --version through the cache. Probe
// failures are cached too (keyed to the same file identity): a broken binary
// stays broken until the file changes.
func cachedBorgVersion(ctx context.Context, path string) (string, bool) {
	info, statErr := os.Stat(path)
	if statErr != nil {
		return "", false
	}
	borgVersionCache.mu.Lock()
	e, hit := borgVersionCache.m[path]
	borgVersionCache.mu.Unlock()
	if hit && e.mtime.Equal(info.ModTime()) && e.size == info.Size() {
		return e.out, e.ok
	}
	out, err := commandOutput(ctx, path, "--version")
	entry := borgVersionEntry{mtime: info.ModTime(), size: info.Size(), out: out, ok: err == nil}
	borgVersionCache.mu.Lock()
	borgVersionCache.m[path] = entry
	borgVersionCache.mu.Unlock()
	return out, err == nil
}

// borgCommand is one borg executable the generated configs will invoke, where
// its name came from (for error messages), and whether any group invoking it
// has snapshot hooks (which hardens the version floor for it).
type borgCommand struct {
	command   string
	source    string
	snapshots bool
}

// borgCommands lists every borg the configuration names, by walking each
// group's effective config the way generation merges it (global defaults,
// then the group's file override, then its label fragments, last wins). The
// default "borg" on PATH is demanded only when some group actually falls
// through to it: a deployment where every group labels its own local_path
// worked before this check existed and must keep working. With no discovered
// state (or none available, doctor with a dead socket) the default is
// conservatively required.
func borgCommands(cfg *config.ManagerConfig, overrides map[string]config.GroupOverride, state *models.BackupState) []borgCommand {
	globalHooks := hasSnapshotHooks(cfg.Borgmatic)
	globalLP, _ := cfg.Borgmatic["local_path"].(string)

	var cmds []borgCommand
	index := map[string]int{}
	add := func(command, source string, snapshots bool) {
		if command == "" {
			return
		}
		if i, ok := index[command]; ok {
			// Two configs naming one borg: the floor is hard if either side
			// uses snapshot hooks.
			cmds[i].snapshots = cmds[i].snapshots || snapshots
			return
		}
		index[command] = len(cmds)
		cmds = append(cmds, borgCommand{command: command, source: source, snapshots: snapshots})
	}

	// Each discovered group's effective borg, merged in generation's order.
	// Groups that fall through to the default make it required and lend it
	// their snapshot scope.
	type layer struct {
		m      map[string]interface{}
		source string
	}
	groupNames := []string{}
	if state != nil {
		for name := range state.Groups {
			groupNames = append(groupNames, name)
		}
		sort.Strings(groupNames)
	}
	defaultNeeded := state == nil || len(groupNames) == 0
	defaultHooks := globalHooks
	for _, name := range groupNames {
		chain := []layer{{cfg.Borgmatic, "manager.yaml local_path"}}
		if o, ok := overrides[name]; ok && o.Borgmatic != nil {
			chain = append(chain, layer{o.Borgmatic, "group " + name + " local_path"})
		}
		for _, lc := range state.Groups[name].LabelConfigs {
			chain = append(chain, layer{lc, "group " + name + " label local_path"})
		}

		lp, lpSource := "", ""
		hooks := false
		for _, l := range chain {
			if v, _ := l.m["local_path"].(string); v != "" {
				lp, lpSource = v, l.source
			}
			hooks = hooks || hasSnapshotHooks(l.m)
		}
		if lp == "" {
			defaultNeeded = true
			defaultHooks = defaultHooks || hooks
			continue
		}
		add(lp, lpSource, hooks)
	}

	// Overrides for groups not currently discovered still describe borgs that
	// will run when their group returns, without a rerun of this check. One
	// with its own local_path is checked directly (a discovered group's
	// override is NOT re-added here: labels merge after it and may have
	// superseded it, and demanding an executable generation never invokes
	// would refuse a working deployment). One without falls through to the
	// default, dragging its snapshot hooks onto the default's floor.
	names := make([]string, 0, len(overrides))
	for name := range overrides {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if state != nil {
			if _, discovered := state.Groups[name]; discovered {
				continue
			}
		}
		o := overrides[name]
		lp := ""
		if o.Borgmatic != nil {
			lp, _ = o.Borgmatic["local_path"].(string)
		}
		if lp != "" {
			add(lp, "group "+name+" local_path", globalHooks || hasSnapshotHooks(o.Borgmatic))
			continue
		}
		// Even a manager-only override (a period alone) names a group that
		// will exist and inherit the default borg when it wakes, without a
		// preflight rerun.
		defaultNeeded = true
		if o.Borgmatic != nil {
			defaultHooks = defaultHooks || hasSnapshotHooks(o.Borgmatic)
		}
	}

	if defaultNeeded {
		if globalLP != "" {
			add(globalLP, "manager.yaml local_path", defaultHooks)
		} else {
			add("borg", "PATH", defaultHooks)
		}
	}
	return cmds
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

// healthyHostBorgmatic reports a host-installed borgmatic that actually runs
// and meets the version floor. A shim that exists but cannot exec (the broken
// pipx case) or an install below the floor is not healthy: the caller
// provisions the toolchain instead of failing on it.
func healthyHostBorgmatic(ctx context.Context) (string, bool) {
	p, ok := hostBorgmaticPath()
	if !ok {
		return "", false
	}
	out, err := commandOutput(ctx, p, "--version")
	if err != nil {
		slog.Warn("host borgmatic is broken; provisioning the manager's own toolchain instead", "path", p, "error", err)
		return "", false
	}
	v := borgmaticVersionOf(out)
	if v == "" || !versionAtLeast(v, minBorgmatic) {
		slog.Warn("host borgmatic is too old or reports no version; provisioning the manager's own toolchain instead", "path", p, "version", strings.TrimSpace(out))
		return "", false
	}
	return p, true
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

// hostBorgmaticPath finds borgmatic on PATH or in the well-known locations.
func hostBorgmaticPath() (string, bool) {
	if p, err := exec.LookPath("borgmatic"); err == nil {
		return p, true
	}
	for _, p := range wellKnownBorgmaticPaths {
		if info, err := os.Stat(p); err == nil && !info.IsDir() {
			return p, true
		}
	}
	return "", false
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
	if p, ok := hostBorgmaticPath(); ok {
		return p, nil
	}
	return "", fmt.Errorf("borgmatic not found: start the daemon or a run once to let the manager provision its own, install it (e.g. 'uv tool install borgmatic'), or set manager.borgmatic_path / BORGMATIC_PATH")
}

func commandOutput(ctx context.Context, name string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...) // #nosec G204 -- version preflight of operator-configured binaries
	// WaitDelay releases the output-pipe wait after the kill: a probed
	// binary whose descendant inherits stdout and wedges must not turn this
	// bounded probe into an unbounded one, or a broken toolchain launcher
	// would block restore and passthrough from ever reaching the host
	// fallback.
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
