package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/charmbracelet/fang"
	charmlog "github.com/charmbracelet/log"
	"github.com/coreos/go-systemd/v22/daemon"
	"github.com/spf13/cobra"

	"github.com/lugoues/borgmatic-manager/internal/config"
	"github.com/lugoues/borgmatic-manager/internal/discovery"
	"github.com/lugoues/borgmatic-manager/internal/events"
	"github.com/lugoues/borgmatic-manager/internal/lockfile"
	"github.com/lugoues/borgmatic-manager/internal/models"
	"github.com/lugoues/borgmatic-manager/internal/orchestrator"
	"github.com/lugoues/borgmatic-manager/internal/runner"
	"github.com/lugoues/borgmatic-manager/internal/runtime"
	"github.com/lugoues/borgmatic-manager/internal/scheduler"
	"github.com/lugoues/borgmatic-manager/internal/state"
)

// version is set at build time via -ldflags "-X main.version=...".
var version = "dev"

func main() {
	root := &cobra.Command{
		Use:     "borgmatic-manager",
		Short:   "Label-driven borgmatic backup orchestration for Docker and Podman",
		Version: version,
		Long: `Discovers containers labeled borgmatic-manager.*, generates per-group
borgmatic configurations, and runs periodic, snapshot-consistent backups.`,
	}

	root.AddCommand(runCmd(), discoverCmd(), generateCmd(), statusCmd(), inspectCmd(), logsCmd(), doctorCmd(), restoreVolumeCmd(), borgmaticCmd(), versionCmd())

	if err := fang.Execute(context.Background(), root, fang.WithVersion(version)); err != nil {
		os.Exit(1)
	}
}

func runCmd() *cobra.Command {
	var scheduler, all bool
	cmd := &cobra.Command{
		Use:   "run [group...]",
		Short: "Back up now: named groups or --all; --scheduler runs the daemon",
		Long: `run performs an immediate on-demand backup: discover, generate configs, and run
borgmatic once for the groups you name (or every group with --all), then exit.
It records results just like a scheduled run, so status and inspect see it.

With --scheduler, run is instead the long-lived daemon the systemd unit starts:
it backs up on manager.period and reacts to container events. It takes no group
arguments.

A target is required. Bare "run" started the daemon in v1.5 and earlier, so it
errors rather than silently doing something different to a stale caller.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			cmd.SilenceUsage = true
			switch {
			// Messages avoid leading with a flag: the renderer capitalizes the first letter.
			case scheduler:
				if len(args) > 0 {
					return errors.New("the --scheduler flag runs the daemon and takes no group arguments")
				}
				if all {
					return errors.New("cannot combine --scheduler with --all: one runs the daemon, the other backs up once and exits")
				}
				return runDaemon()
			case all:
				if len(args) > 0 {
					return errors.New("the --all flag already backs up every group; do not also name groups")
				}
				return runAdhoc(cmd.Context(), nil)
			case len(args) > 0:
				return runAdhoc(cmd.Context(), args)
			default:
				return errBareRun
			}
		},
	}
	cmd.Flags().BoolVar(&scheduler, "scheduler", false, "run as the scheduling daemon (used by the systemd unit)")
	cmd.Flags().BoolVar(&all, "all", false, "back up every discovered group now, then exit")
	return cmd
}

// errBareRun refuses a target-less run: bare "run" started the daemon through
// v1.5, so a stale systemd unit would otherwise silently back up once and exit.
var errBareRun = errors.New(`backup target required: pass --all to back up every group, name the groups to back up, or --scheduler to run the daemon. Bare "run" started the daemon in v1.5 and earlier, if this came from a systemd unit, update its ExecStart to "run --scheduler"`)

func discoverCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "discover",
		Short: "Discover labeled containers, print the backup groups, and exit",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cmd.SilenceUsage = true
			return runDiscover()
		},
	}
}

func statusCmd() *cobra.Command {
	var jsonOut bool
	cmd := &cobra.Command{
		Use:   "status",
		Short: "Show each group's last run, its result, and when the next run is due",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cmd.SilenceUsage = true
			return runStatus(cmd.Context(), jsonOut)
		},
	}
	cmd.Flags().BoolVar(&jsonOut, "json", false, "machine-readable output for scripts and exporters")
	return cmd
}

func runStatus(ctx context.Context, jsonOut bool) error {
	logger := interactiveLogger()
	e, err := loadEnv()
	if err != nil {
		return err
	}
	backupState, offline, err := e.discoverMerged(ctx, logger)
	if err != nil {
		return err
	}
	period, err := e.cfg.ParsedPeriod()
	if err != nil {
		return err
	}
	// Plan (no writes) surfaces groups generation refuses, so status can
	// say "refused" instead of a forever-"due now" that never runs.
	_, refusals, err := e.newGenerator(e.configsDir, logger).Plan(backupState)
	if err != nil {
		return err
	}
	refused := make(map[string]string, len(refusals))
	for _, r := range refusals {
		refused[r.Group] = r.Reason
	}

	runTimeout, err := runTimeoutFromConfig(e.cfg)
	if err != nil {
		return err
	}

	if jsonOut {
		return printStatusJSON(backupState, stateStore(e, logger), period, runTimeout, e.cfg.GroupPeriods, refused, offline)
	}
	printStatus(backupState, stateStore(e, logger), period, runTimeout, e.cfg.GroupPeriods, refused, offline)
	return nil
}

func inspectCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "inspect <group>",
		Short: "Show a group's members, schedule, recent runs, size trend, last log, and config",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cmd.SilenceUsage = true
			return runInspect(cmd.Context(), args[0])
		},
	}
}

func runInspect(ctx context.Context, group string) error {
	logger := interactiveLogger()
	e, err := loadEnv()
	if err != nil {
		return err
	}
	backupState, offline, err := e.discoverMerged(ctx, logger)
	if err != nil {
		return err
	}
	g, ok := backupState.Groups[group]
	if !ok {
		return fmt.Errorf("unknown group %q; %s", group, discoveredGroupList(backupState))
	}
	period, err := e.cfg.ParsedPeriod()
	if err != nil {
		return err
	}

	rec, haveRec := stateStore(e, logger).Record(group)
	configYAML, configNote := renderGroupConfig(backupState, e, logger, group)

	printInspect(group, g, rec, haveRec, configYAML, configNote, period, e.cfg.GroupPeriods[group], offline)
	return nil
}

// renderGroupConfig compiles one group's borgmatic config for display, or a note
// explaining why it is unavailable. Never fails the command; output is redacted.
func renderGroupConfig(backupState *models.BackupState, e *env, logger *slog.Logger, group string) (configYAML, note string) {
	cfg, refusal, err := e.newGenerator("", logger).RenderGroup(backupState, group)
	switch {
	case err != nil:
		return "", "config generation failed: " + err.Error()
	case refusal != "":
		return "", "config refused: " + refusal
	case cfg == "":
		return "", "no config generated for this group"
	default:
		return redactConfigSecrets(cfg), ""
	}
}

func discoveredGroupList(backupState *models.BackupState) string {
	if len(backupState.Groups) == 0 {
		return "no groups discovered, check your labels"
	}
	names := make([]string, 0, len(backupState.Groups))
	for name := range backupState.Groups {
		names = append(names, name)
	}
	sort.Strings(names)
	return "discovered groups: " + strings.Join(names, ", ")
}

func generateCmd() *cobra.Command {
	var output string
	cmd := &cobra.Command{
		Use:   "generate",
		Short: "Generate borgmatic configs once and exit",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cmd.SilenceUsage = true
			return runGenerate(output)
		},
	}
	cmd.Flags().StringVarP(&output, "output", "o", "", "output directory (default: $RUNTIME_DIR/configs)")
	return cmd
}

func borgmaticCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "borgmatic <group> [borgmatic args...]",
		Short: "Run borgmatic against a group's generated config",
		Long: `Regenerates the group's config from live labels and execs borgmatic with
it, the supported way to interact with a group's repository:

  borgmatic-manager borgmatic myapp repo-create --encryption repokey-blake2
  borgmatic-manager borgmatic myapp list
  borgmatic-manager borgmatic myapp extract --archive latest

Advanced/escape hatch: this runs borgmatic directly and BYPASSES the manager's
cross-run locks. A passthrough that touches the repository or takes snapshots
(e.g. create) can collide with a scheduled or ad-hoc run on the same repo. Use
it for read/restore/bootstrap, and avoid mutating actions while backups run.`,
		// Everything after the group belongs to borgmatic untouched.
		DisableFlagParsing: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) > 0 && (args[0] == "-h" || args[0] == "--help") {
				return cmd.Help()
			}
			cmd.SilenceUsage = true
			return runBorgmaticPassthrough(args)
		},
	}
}

func versionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print the version and exit",
		Args:  cobra.NoArgs,
		Run: func(cmd *cobra.Command, _ []string) {
			fmt.Println(version)
		},
	}
}

// env holds the resolved directory layout and loaded configuration.
type env struct {
	configDir  string
	runtimeDir string
	stateDir   string
	configsDir string

	cfg            *config.ManagerConfig
	groupOverrides map[string]config.GroupOverride
	rt             *runtime.DockerRuntime
}

func loadEnv() (*env, error) {
	e := &env{
		configDir:  getEnv("CONFIG_DIR", "/etc/borgmatic-manager"),
		runtimeDir: getEnv("RUNTIME_DIR", "/run/borgmatic-manager"),
		stateDir:   getEnv("STATE_DIR", "/var/lib/borgmatic-manager"),
	}
	e.configsDir = filepath.Join(e.runtimeDir, "configs")

	cfg, groupOverrides, err := config.LoadConfig(filepath.Join(e.configDir, "manager.yaml"), filepath.Join(e.configDir, "groups"))
	if err != nil {
		return nil, fmt.Errorf("loading config: %w", err)
	}
	e.cfg = cfg
	e.groupOverrides = groupOverrides

	rt, err := runtime.NewDockerRuntime()
	if err != nil {
		return nil, fmt.Errorf("creating container runtime client: %w", err)
	}
	e.rt = rt
	return e, nil
}

func (e *env) newGenerator(outputDir string, logger *slog.Logger) *config.Generator {
	return config.NewGenerator(e.cfg, e.groupOverrides, outputDir, config.GeneratorOptions{
		RuntimeDir:   e.runtimeDir,
		StateDir:     e.stateDir,
		ContainerCLI: detectContainerCLI(e.cfg, e.rt.SocketPath()),
	}, logger)
}

// reapRunHelpers force-removes a run's dump helper containers by run-ID label.
func (e *env) reapRunHelpers(ctx context.Context, runID string) ([]string, error) {
	return e.rt.RemoveContainersByLabel(ctx, models.HelperRunLabel, runID)
}

// privateConfigDir returns a per-PID dir under the runtime tmpfs (configs carry
// credentials, never disk-backed /tmp), sweeping dead-PID leftovers first.
func (e *env) privateConfigDir(kind string) (string, error) {
	base := filepath.Join(e.runtimeDir, kind)
	if err := os.MkdirAll(base, 0o700); err != nil {
		return "", fmt.Errorf("creating %s config directory: %w", kind, err)
	}
	sweepDeadPIDDirs(base)

	dir := filepath.Join(base, strconv.Itoa(os.Getpid()))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("creating %s config directory: %w", kind, err)
	}
	return dir, nil
}

// sweepDeadPIDDirs removes subdirectories of base whose name is a PID no longer
// alive. Best-effort: cleanup, not correctness.
func sweepDeadPIDDirs(base string) {
	entries, err := os.ReadDir(base)
	if err != nil {
		return
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		pid, err := strconv.Atoi(entry.Name())
		if err != nil || processAlive(pid) {
			continue
		}
		_ = os.RemoveAll(filepath.Join(base, entry.Name()))
	}
}

// newRunner wires a runner for one process; only logger and configDir differ
// between the daemon and an ad-hoc run, so they are parameters.
func (e *env) newRunner(logger *slog.Logger, configDir, borgmaticPath string, runTimeout time.Duration, store *state.ScheduleStore) *runner.Runner {
	r := runner.NewRunner(logger, configDir, borgmaticPath, e.cfg.Manager.Actions, runTimeout)
	r.SetLockDir(filepath.Join(e.stateDir, "locks"))
	r.SetRecorder(store)
	r.SetHelperReaper(store, e.reapRunHelpers)
	return r
}

// reapStalePendingRuns reaps dump helpers left by a manager process that died
// mid-run. Liveness comes from the per-run advisory lock (kernel-dropped on
// crash); no-lock-file records fall back to the stamped PID, biased to keep.
func reapStalePendingRuns(ctx context.Context, store *state.ScheduleStore, lockDir string, reap func(context.Context, string) ([]string, error)) {
	if lockDir == "" {
		return // no lock dir, no way to prove liveness: leave every record
	}
	for runID, p := range store.PendingSnapshot() {
		lockPath := runner.PendingLockPath(lockDir, runID)

		if _, statErr := os.Stat(lockPath); statErr != nil {
			if !errors.Is(statErr, os.ErrNotExist) {
				slog.Warn("cannot stat pending-run liveness lock; leaving the record",
					"group", p.Group, "run_id", runID, "error", statErr)
				continue
			}
			// No lock file (pre-lock binary or failed acquisition): the owner may be
			// live, so reap only when the stamped PID is provably gone. No TryExclusive
			// here: it would create a file the next cycle reads as present-unheld.
			if p.PID != 0 && processAlive(p.PID) {
				slog.Info("leaving pending run alone; no liveness lock yet but its PID is live",
					"group", p.Group, "run_id", runID, "pid", p.PID)
				continue
			}
			reapAndClear(ctx, store, reap, runID, p) // owner gone (or legacy no-PID)
			continue
		}

		// A lock file exists: the authoritative path.
		lock, acquired, err := lockfile.TryExclusive(lockPath)
		if err != nil {
			slog.Warn("cannot probe pending-run liveness lock; leaving the record",
				"group", p.Group, "run_id", runID, "error", err)
			continue
		}
		if !acquired {
			slog.Info("leaving pending run alone; a live process holds its liveness lock",
				"group", p.Group, "run_id", runID)
			continue
		}
		// We took the lock: the owner is gone.
		if reapAndClear(ctx, store, reap, runID, p) {
			_ = os.Remove(lockPath)
		}
		lock.Release()
	}
}

// reapAndClear reaps a dead run's helpers and clears its record. Returns true
// when cleared (safe to remove the lock file), false to retry next startup.
func reapAndClear(ctx context.Context, store *state.ScheduleStore, reap func(context.Context, string) ([]string, error), runID string, p state.PendingRun) bool {
	names, err := reap(ctx, runID)
	if err != nil {
		slog.Warn("cannot reap stale dump helpers; will retry next startup",
			"group", p.Group, "run_id", runID, "error", err)
		return false
	}
	if len(names) > 0 {
		slog.Warn("reaped dump helpers orphaned by a dead manager process",
			"group", p.Group, "run_id", runID, "started", p.Started.Format(time.RFC3339),
			"containers", strings.Join(names, ","))
	}
	store.ClearPending(runID)
	return true
}

// sweepOrphanedPendingLocks removes pending-*.lock files no record references
// and no process holds; a crash between ClearPending and lock removal strands them.
func sweepOrphanedPendingLocks(lockDir string, store *state.ScheduleStore) {
	if lockDir == "" {
		return
	}
	referenced := map[string]bool{}
	for runID := range store.PendingSnapshot() {
		referenced[filepath.Base(runner.PendingLockPath(lockDir, runID))] = true
	}
	entries, err := os.ReadDir(lockDir)
	if err != nil {
		return
	}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasPrefix(name, "pending-") || !strings.HasSuffix(name, ".lock") {
			continue
		}
		if referenced[name] {
			continue
		}
		// Unreferenced: remove only if not held, to avoid racing a run mid-startup.
		path := filepath.Join(lockDir, name)
		lock, acquired, err := lockfile.TryExclusive(path)
		if err != nil || !acquired {
			continue
		}
		lock.Release()
		_ = os.Remove(path)
	}
}

// processAlive reports whether pid is live via signal 0, biased to "alive when
// unsure" (EPERM counts): a false "dead" would let callers reap a live run.
func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

// runningGroups maps group name to the earliest start time of a live pending
// run, so a stale-plus-fresh pair surfaces the longer-running one.
//
// A record whose owner was SIGKILLed (or lost to OOM or power loss) never gets
// its deferred ClearPending, and only the daemon reaps those at startup: an
// ad-hoc run deliberately does not, since a daemon may be legitimately mid-run.
// Without this filter such a record pins its group at "running" forever, hiding
// the real due state. Records with no stamped PID (pre-PID binaries) stay
// visible: they cannot be proven dead, and this is a display filter, not a reap.
func runningGroups(store *state.ScheduleStore) map[string]time.Time {
	running := map[string]time.Time{}
	for _, p := range store.PendingSnapshot() {
		if p.PID != 0 && !processAlive(p.PID) {
			continue
		}
		if started, ok := running[p.Group]; !ok || p.Started.Before(started) {
			running[p.Group] = p.Started
		}
	}
	return running
}

func runDaemon() error {
	// Structured JSON logging to stdout (journald captures it).
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)))

	// One SIGTERM or SIGINT shuts down cleanly; the runner forwards it to in-flight borgmatic.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	e, err := loadEnv()
	if err != nil {
		slog.Error("startup failed", "error", err)
		return err
	}

	pf, err := preflight(ctx, e)
	if err != nil {
		slog.Error("preflight failed", "error", err)
		return err
	}

	gen := e.newGenerator(e.configsDir, slog.Default())
	store := state.LoadSchedule(e.stateDir, slog.Default())
	r := e.newRunner(slog.Default(), e.configsDir, pf.borgmaticPath, pf.runTimeout, store)
	locksDir := filepath.Join(e.stateDir, "locks")
	reapStalePendingRuns(ctx, store, locksDir, e.reapRunHelpers)
	sweepOrphanedPendingLocks(locksDir, store)
	s := scheduler.NewScheduler(r, e.rt, slog.Default(), e.cfg, gen, store)
	s.SetGroupCache(state.LoadGroupCache(e.stateDir, slog.Default()))
	l := events.NewListener(e.rt, slog.Default())
	o := orchestrator.NewOrchestrator(s, l, slog.Default())

	slog.Info("borgmatic-manager starting",
		"version", version,
		"period", e.cfg.Manager.Period,
		"config_dir", e.configDir,
		"socket", e.rt.SocketPath(),
		"borgmatic", pf.borgmaticPath,
		"borgmatic_version", pf.borgmaticVersion,
	)

	// Readiness for Type=notify units; a no-op outside systemd.
	_, _ = daemon.SdNotify(false, daemon.SdNotifyReady)

	if err := o.Run(ctx); err != nil {
		slog.Error("fatal error", "error", err)
		return err
	}

	slog.Info("borgmatic-manager stopped")
	return nil
}

// runAdhoc backs up the target groups once and exits, recording outcomes to the
// same schedule state as the daemon. It deliberately does NOT reap stale pending
// helpers: a scheduler daemon may be legitimately mid-run.
func runAdhoc(ctx context.Context, groups []string) error {
	// Ctrl-C / SIGTERM cancels; the runner forwards it to the borgmatic process group.
	ctx, stop := signal.NotifyContext(ctx, syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	logger := interactiveLogger() // quiet: warnings from discovery/generation
	e, err := loadEnv()
	if err != nil {
		return err
	}

	pf, err := preflight(ctx, e)
	if err != nil {
		return err
	}

	backupState, err := discovery.Discover(ctx, e.rt, logger)
	if err != nil {
		return err
	}

	// Generate into a private tmpfs directory, never the daemon's live configs
	// dir: sharing it races the daemon (deleted configs, mismatched RunIDs that
	// leak dump helpers), and the configs carry credentials so never /tmp.
	configsDir, err := e.privateConfigDir("run")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(configsDir) }()

	meta, err := e.newGenerator(configsDir, logger).Generate(backupState)
	if err != nil {
		return err
	}

	targets, err := resolveAdhocTargets(backupState, meta, groups)
	if err != nil {
		return err
	}
	if len(targets) == 0 {
		return fmt.Errorf("no runnable groups, none discovered, or all were refused by generation (see warnings above)")
	}

	// A verbose logger so the user watches borgmatic progress live; outcomes
	// still land in the shared schedule state.
	store := state.LoadSchedule(e.stateDir, logger)
	r := e.newRunner(progressLogger(), configsDir, pf.borgmaticPath, pf.runTimeout, store)

	now := time.Now()
	var failed, locked, unattempted []string
	for i, name := range targets {
		// An interrupt between groups stops the loop; nothing from here ran.
		if ctx.Err() != nil {
			unattempted = append(unattempted, targets[i:]...)
			break
		}
		acquired, runErr := r.TryRunGroup(ctx, name, meta[name])

		switch classifyAdhocOutcome(acquired, runErr, ctx.Err() != nil) {
		case adhocSuccess:
			store.MarkSuccess(name, scheduler.GroupFingerprint(backupState.Groups[name]), now)
		case adhocLocked:
			locked = append(locked, name)
		case adhocNotRun:
			unattempted = append(unattempted, name)
		case adhocFailed:
			failed = append(failed, name)
		}

		// Interrupted: the current group is already classified; the rest never run.
		if ctx.Err() != nil {
			unattempted = append(unattempted, targets[i+1:]...)
			break
		}
	}

	printAdhocSummary(targets, failed, locked, unattempted)
	switch {
	case len(failed) > 0:
		return fmt.Errorf("%d of %d group(s) failed", len(failed), len(targets))
	case len(unattempted) > 0:
		return fmt.Errorf("interrupted: %d group(s) were not backed up", len(unattempted))
	case len(locked) > 0:
		return fmt.Errorf("%d group(s) are locked by a run already in progress, try again later", len(locked))
	}
	return nil
}

// adhocOutcome buckets one group's ad-hoc run result for the summary.
type adhocOutcome int

const (
	adhocSuccess adhocOutcome = iota
	adhocLocked
	adhocNotRun // interrupted mid-run, or not reached
	adhocFailed
)

// classifyAdhocOutcome maps a TryRunGroup result to its summary bucket. Success
// is checked before interrupt: a group that finished just before the interrupt
// must record its success, not be dropped as "not run".
func classifyAdhocOutcome(acquired bool, runErr error, interrupted bool) adhocOutcome {
	switch {
	case runErr == nil && acquired:
		return adhocSuccess
	case errors.Is(runErr, runner.ErrLockedByAnotherProcess):
		// Another process holds the repo/snapshot lock; ad-hoc never queues.
		return adhocLocked
	case interrupted:
		// The error is the interruption, not a backup failure.
		return adhocNotRun
	case runErr != nil:
		return adhocFailed
	default:
		// !acquired with no error: held in-process. Can't happen in the
		// sequential ad-hoc loop, but a silent "success" would be a lie.
		return adhocLocked
	}
}

// resolveAdhocTargets returns the groups to back up: all that generated a config
// when none are named, otherwise the named ones, validated against refusals.
func resolveAdhocTargets(backupState *models.BackupState, meta map[string]config.GroupRunMeta, requested []string) ([]string, error) {
	if len(requested) == 0 {
		names := make([]string, 0, len(meta))
		for name := range meta {
			names = append(names, name)
		}
		sort.Strings(names)
		return names, nil
	}

	targets := make([]string, 0, len(requested))
	for _, name := range requested {
		if _, ok := backupState.Groups[name]; !ok {
			return nil, fmt.Errorf("unknown group %q; %s", name, discoveredGroupList(backupState))
		}
		if _, ok := meta[name]; !ok {
			return nil, fmt.Errorf("group %q was refused by generation (see warnings above) and cannot be run", name)
		}
		targets = append(targets, name)
	}
	return targets, nil
}

func runDiscover() error {
	logger := interactiveLogger()
	ctx := context.Background()

	e, err := loadEnv()
	if err != nil {
		return err
	}

	backupState, offline, err := e.discoverMerged(ctx, logger)
	if err != nil {
		return err
	}

	if len(backupState.Groups) == 0 {
		return fmt.Errorf("no backup groups discovered, check your labels (warnings above, if any, explain near-misses)")
	}

	printGroups(backupState, stateStore(e, logger), offline)
	return nil
}

// stateStore loads the persisted schedule for one-shot display commands.
func stateStore(e *env, logger *slog.Logger) *state.ScheduleStore {
	return state.LoadSchedule(e.stateDir, logger)
}

// discoverMerged runs live discovery and overlays the durable group cache so a
// stopped or quadlet-removed container's group still appears. It refreshes the
// cache with the live set as a side effect. Returns the merged state and the
// set of offline (cached-only) group names.
func (e *env) discoverMerged(ctx context.Context, logger *slog.Logger) (*models.BackupState, *state.Offline, error) {
	live, err := discovery.Discover(ctx, e.rt, logger)
	if err != nil {
		return nil, nil, err
	}
	merged, offline := state.LoadGroupCache(e.stateDir, logger).Reconcile(live, time.Now())
	return merged, offline, nil
}

func runGenerate(output string) error {
	logger := interactiveLogger()
	ctx := context.Background()

	e, err := loadEnv()
	if err != nil {
		return err
	}

	outDir := output
	if outDir == "" {
		outDir = e.configsDir
	}

	state, err := discovery.Discover(ctx, e.rt, logger)
	if err != nil {
		return err
	}

	gen := e.newGenerator(outDir, logger)
	meta, err := gen.Generate(state)
	if err != nil {
		return err
	}

	groups := make([]string, 0, len(meta))
	for group := range meta {
		groups = append(groups, group)
	}
	sort.Strings(groups)
	for _, group := range groups {
		fmt.Println(filepath.Join(outDir, group+".yaml"))
	}
	return nil
}

// runBorgmaticPassthrough regenerates the group's config from live labels and
// execs borgmatic with it: the supported way to touch a group's repository.
func runBorgmaticPassthrough(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: borgmatic-manager borgmatic <group> [borgmatic args...]")
	}
	group := args[0]
	if strings.HasPrefix(group, "-") {
		return fmt.Errorf("the first argument must be a group name, got flag %q (e.g.: borgmatic-manager borgmatic myapp create --dry-run); run 'borgmatic-manager discover' to list groups", group)
	}

	logger := interactiveLogger()
	ctx := context.Background()

	e, err := loadEnv()
	if err != nil {
		return err
	}

	// Merge the durable cache so restore/list still work after the container is
	// stopped or (quadlets) removed: the group's config is regenerated from its
	// last-known membership.
	backupState, offline, err := e.discoverMerged(ctx, logger)
	if err != nil {
		return err
	}

	// Private tmpfs dir, never the daemon's live configs dir: rewriting it races
	// in-flight runs (mismatched RunIDs leak dump helpers). exec means no cleanup
	// on exit; privateConfigDir sweeps dead-PID leftovers on the next run.
	configsDir, err := e.privateConfigDir("passthrough")
	if err != nil {
		return err
	}

	meta, err := e.newGenerator(configsDir, logger).Generate(backupState)
	if err != nil {
		return err
	}
	if _, ok := meta[group]; !ok {
		return fmt.Errorf("unknown group %q; %s", group, discoveredGroupList(backupState))
	}
	if offline.GroupOffline(group, backupState.Groups[group]) {
		lastSeen := ""
		if ts, ok := state.LoadGroupCache(e.stateDir, logger).LastSeen(group); ok && !ts.IsZero() {
			lastSeen = " (last seen " + ts.Local().Format("2006-01-02 15:04") + ")"
		}
		logger.Warn("group is offline: its container is not running, using the last cached config"+lastSeen+"; membership may be stale", "group", group)
	}

	borgmaticPath, err := resolveBorgmatic(e.cfg)
	if err != nil {
		return err
	}

	configPath := filepath.Join(configsDir, group+".yaml")
	argv := append([]string{borgmaticPath, "--config", configPath}, args[1:]...)

	// exec cannot hold the manager's cross-run locks, so passthrough bypasses them; warn once.
	fmt.Fprintln(os.Stderr, "note: passthrough bypasses borgmatic-manager's cross-run locks, avoid mutating actions (e.g. create) while a scheduled or ad-hoc backup may touch this repository")

	// Replace the process: borgmatic owns the terminal from here.
	// #nosec G702 G204 -- deliberately exec'ing the resolved borgmatic binary with the operator's own CLI arguments
	if err := syscall.Exec(borgmaticPath, argv, os.Environ()); err != nil {
		return fmt.Errorf("executing borgmatic: %w", err)
	}
	return nil
}

func restoreVolumeCmd() *cobra.Command {
	var archive, into string
	var force, merge, snapshot bool
	cmd := &cobra.Command{
		Use:   "restore-volume <group> <volume>",
		Short: "Extract one volume from an archive back into place",
		Long: `Restores a named volume. The manager resolves the volume's host path, so
there is no --destination to get wrong: it extracts straight into the volume's
own data directory.

By default it mirrors: the target is emptied first for an exact
point-in-time restore. --merge keeps files added since the backup (archived
files still overwrite).

Two ways to keep the current data as a safety net before overwriting it:
--into <volume> extracts into a different existing volume, leaving the source
untouched; --snapshot (btrfs only) makes a copy-on-write copy of the volume
first, then restores in place, so you can roll back.

Extracting into a volume a running container is using risks corruption, so it
refuses unless the container is stopped or --force.`,
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			cmd.SilenceUsage = true
			return runRestoreVolume(cmd.Context(), args[0], args[1], archive, into, force, merge, snapshot)
		},
	}
	cmd.Flags().StringVar(&archive, "archive", "latest", "archive to restore from")
	cmd.Flags().StringVar(&into, "into", "", "restore into this volume instead of the source (must already exist)")
	cmd.Flags().BoolVar(&force, "force", false, "extract even if a running container is using the target volume")
	cmd.Flags().BoolVar(&merge, "merge", false, "keep files added since the backup instead of emptying the target first")
	cmd.Flags().BoolVar(&snapshot, "snapshot", false, "btrfs: copy-on-write snapshot the volume before overwriting it, as a rollback point")
	// --into and --snapshot are two ways to preserve the current data; pick one.
	cmd.MarkFlagsMutuallyExclusive("into", "snapshot")
	return cmd
}

// volumeRestorePlan is the resolved borg geometry for a restore: what to pull
// from the archive and where to land it.
type volumeRestorePlan struct {
	volumesRoot  string // e.g. /var/lib/docker/volumes
	archivePath  string // path prefix in the archive: <sourceVolume>/_data
	targetVolume string // volume being written to (source, or --into)
	targetData   string // <volumesRoot>/<targetVolume>/_data
}

// planVolumeRestore derives the archive path and target from the source
// volume's host path. into names an alternate target volume in the same
// volumes root; empty means restore into the source volume.
func planVolumeRestore(sourceHostPath, into string) (volumeRestorePlan, error) {
	volumesRoot := config.VolumesRoot(sourceHostPath)
	archivePath := strings.TrimPrefix(sourceHostPath, volumesRoot+string(filepath.Separator))
	if archivePath == "" || archivePath == sourceHostPath {
		return volumeRestorePlan{}, fmt.Errorf("cannot derive the archive path for %s; restore manually with the borgmatic passthrough", sourceHostPath)
	}
	targetVolume := filepath.Base(filepath.Dir(sourceHostPath))
	targetData := sourceHostPath
	if into != "" {
		// A bare volume name only. Anything with a separator or a ".." escapes
		// the volumes root once joined, and emptyVolumeData's guard is a
		// substring test that ".../mnt/volumes/x" would sail straight through.
		if into != filepath.Clean(into) || strings.ContainsRune(into, filepath.Separator) || into == ".." || into == "." {
			return volumeRestorePlan{}, fmt.Errorf("--into %q must be a bare volume name, not a path", into)
		}
		targetVolume = into
		targetData = filepath.Join(volumesRoot, into, "_data")
	}
	return volumeRestorePlan{volumesRoot: volumesRoot, archivePath: archivePath, targetVolume: targetVolume, targetData: targetData}, nil
}

// runRestoreVolume extracts a single volume back into its data directory (or an
// --into target), computing borg's --path/--destination from the volume's known
// location so the operator never has to.
func runRestoreVolume(ctx context.Context, group, volume, archive, into string, force, merge, snapshot bool) error {
	logger := interactiveLogger()

	e, err := loadEnv()
	if err != nil {
		return err
	}

	backupState, _, err := e.discoverMerged(ctx, logger)
	if err != nil {
		return err
	}
	g, ok := backupState.Groups[group]
	if !ok {
		return fmt.Errorf("unknown group %q; %s", group, discoveredGroupList(backupState))
	}

	var hostPath string
	names := make([]string, 0, len(g.Volumes))
	for _, v := range g.Volumes {
		names = append(names, v.Name)
		if v.Name == volume {
			hostPath = v.HostPath
		}
	}
	if hostPath == "" {
		return fmt.Errorf("group %q has no volume %q; volumes: %s", group, volume, strings.Join(names, ", "))
	}

	plan, err := planVolumeRestore(hostPath, into)
	if err != nil {
		return err
	}
	if _, statErr := os.Stat(plan.targetData); statErr != nil {
		return fmt.Errorf("target volume data directory %s is not present; create the volume first (docker/podman volume create %s): %w", plan.targetData, plan.targetVolume, statErr)
	}

	// Extracting into a volume a running container writes races those writes.
	// A stopped or removed container is safe.
	running, err := volumeHasRunningContainer(ctx, e.rt, plan.targetVolume)
	if err != nil {
		return err
	}
	if running && !force {
		return fmt.Errorf("a running container is using volume %q; stop it first, or pass --force to extract into a live volume", plan.targetVolume)
	}

	configsDir, err := e.privateConfigDir("restore")
	if err != nil {
		return err
	}
	if _, genErr := e.newGenerator(configsDir, logger).Generate(backupState); genErr != nil {
		return genErr
	}

	// Preserve the current data before overwriting it: a btrfs CoW copy the
	// operator can roll back from, then removed once the restore is verified.
	if snapshot {
		snap, snapErr := snapshotVolume(ctx, plan.targetData)
		if snapErr != nil {
			return snapErr
		}
		logger.Warn("snapshotted the volume before restore; remove it once you have verified the restore", "snapshot", snap)
	}

	mode := "mirror"
	if merge {
		mode = "merge"
	} else {
		if wipeErr := emptyVolumeData(plan.targetData); wipeErr != nil {
			return fmt.Errorf("emptying the target before a mirror restore: %w", wipeErr)
		}
		logger.Warn("emptied the target volume for a mirror restore", "path", plan.targetData)
	}

	borgmaticPath, err := resolveBorgmatic(e.cfg)
	if err != nil {
		return err
	}
	configPath := filepath.Join(configsDir, group+".yaml")
	// Strip "<sourceVolume>/_data" so files land directly in the target's data
	// dir, which lets --into retarget a differently-named volume.
	argv := []string{borgmaticPath, "--config", configPath, "extract", "--archive", archive,
		"--path", plan.archivePath, "--strip-components", "2", "--destination", plan.targetData}

	fmt.Fprintf(os.Stderr, "restoring %s/%s from archive %s into %s (%s)\n", group, volume, archive, plan.targetData, mode)

	// #nosec G204 -- deliberately exec'ing the resolved borgmatic binary with computed extract arguments
	if err := syscall.Exec(borgmaticPath, argv, os.Environ()); err != nil {
		return fmt.Errorf("executing borgmatic: %w", err)
	}
	return nil
}

// volumeHasRunningContainer reports whether any currently-running container
// mounts the named volume. A stopped or removed container does not count: its
// data is at rest and safe to extract into.
func volumeHasRunningContainer(ctx context.Context, rt runtime.ContainerRuntime, volume string) (bool, error) {
	containers, err := rt.ListContainers(ctx)
	if err != nil {
		return false, err
	}
	for _, c := range containers {
		if !c.Running {
			continue
		}
		for _, m := range c.Mounts {
			if m.Name == volume {
				return true, nil
			}
		}
	}
	return false, nil
}

// snapshotVolume makes a copy-on-write copy of the volume's data as a rollback
// point, requiring btrfs so the copy is near-instant and space-shared. It
// returns the snapshot path. cp --reflink=always fails loudly if the copy would
// not be a real reflink, so a silent full-size duplicate never happens.
func snapshotVolume(ctx context.Context, targetData string) (string, error) {
	onBtrfs, err := config.IsBtrfs(targetData)
	if err != nil {
		return "", fmt.Errorf("checking the filesystem of %s: %w", targetData, err)
	}
	if !onBtrfs {
		return "", fmt.Errorf("--snapshot needs the volume on btrfs, but %s is not; use --into <volume> instead", targetData)
	}
	snap := targetData + ".pre-restore-" + time.Now().Format("20060102-150405")
	// #nosec G204 G702 -- no shell: fixed cp argv over host-derived paths (volume mountpoint + timestamp)
	out, err := exec.CommandContext(ctx, "cp", "--reflink=always", "-a", targetData, snap).CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("btrfs snapshot copy failed: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return snap, nil
}

// emptyVolumeData removes the contents of a volume's _data directory (keeping
// the directory itself). It refuses paths that do not look like a container
// volume, so a misconfigured host path cannot wipe something unrelated.
func emptyVolumeData(hostPath string) error {
	if !strings.Contains(hostPath, string(filepath.Separator)+"volumes"+string(filepath.Separator)) {
		return fmt.Errorf("refusing to empty %q: not a recognizable container volume path", hostPath)
	}
	entries, err := os.ReadDir(hostPath)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if err := os.RemoveAll(filepath.Join(hostPath, entry.Name())); err != nil {
			return err
		}
	}
	return nil
}

// interactiveLogger renders warnings and errors styled on stderr for one-shot
// commands, keeping stdout for the command's own output.
func interactiveLogger() *slog.Logger {
	handler := charmlog.NewWithOptions(os.Stderr, charmlog.Options{
		ReportTimestamp: false,
		Level:           charmlog.WarnLevel,
	})
	return slog.New(handler)
}

// progressLogger renders INFO-and-up on stderr so the operator watches the
// on-demand run live; stdout stays clean for the summary.
func progressLogger() *slog.Logger {
	handler := charmlog.NewWithOptions(os.Stderr, charmlog.Options{
		ReportTimestamp: true,
		TimeFormat:      "15:04:05",
		Level:           charmlog.InfoLevel,
	})
	return slog.New(handler)
}

func getEnv(key, defaultValue string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return defaultValue
}

// runTimeoutFromConfig parses manager.run_timeout; empty means none.
func runTimeoutFromConfig(cfg *config.ManagerConfig) (time.Duration, error) {
	if cfg.Manager.RunTimeout == "" || cfg.Manager.RunTimeout == "0" {
		return 0, nil
	}
	d, err := time.ParseDuration(cfg.Manager.RunTimeout)
	if err != nil {
		return 0, fmt.Errorf("invalid manager.run_timeout %q: %w", cfg.Manager.RunTimeout, err)
	}
	return d, nil
}
