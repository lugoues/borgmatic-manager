package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
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

	root.AddCommand(runCmd(), discoverCmd(), generateCmd(), statusCmd(), inspectCmd(), logsCmd(), borgmaticCmd(), versionCmd())

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
	return &cobra.Command{
		Use:   "status",
		Short: "Show each group's last run, its result, and when the next run is due",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cmd.SilenceUsage = true
			return runStatus(cmd.Context())
		},
	}
}

func runStatus(ctx context.Context) error {
	logger := interactiveLogger()
	e, err := loadEnv()
	if err != nil {
		return err
	}
	backupState, err := discovery.Discover(ctx, e.rt, logger)
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

	printStatus(backupState, stateStore(e, logger), period, runTimeout, refused)
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
	backupState, err := discovery.Discover(ctx, e.rt, logger)
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

	printInspect(group, g, rec, haveRec, configYAML, configNote, period)
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
	groupOverrides map[string]map[string]interface{}
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

// maxPendingAge bounds how long a pending record is trusted against a live PID.
// No backup runs this long, so a record older than this whose PID is still
// "alive" is a reused-PID phantom, not a real in-flight run.
const maxPendingAge = 48 * time.Hour

// reapStalePendingRuns removes dump helpers left behind by a manager process
// that died mid-run.
//
// A pending record is an orphan only when its owning process is gone. "Nothing
// can be in flight at startup" stopped being true once ad-hoc runs began
// writing pending records to the same state file: reaping indiscriminately
// would force-remove the dump helpers of a backup still running in another
// process, failing it or archiving a truncated database dump. Records with no
// owner recorded (written before the PID was tracked) keep the old behavior and
// are treated as orphans. Manual passthrough runs never record pending IDs and
// are never touched.
func reapStalePendingRuns(ctx context.Context, store *state.ScheduleStore, reap func(context.Context, string) ([]string, error)) {
	for runID, p := range store.PendingSnapshot() {
		// A live owner means a real in-flight run, unless the record predates
		// this daemon uptime by more than any backup could plausibly take, in
		// which case the PID has been reused by an unrelated process across a
		// reboot. Reaping then only clears the phantom record (no containers
		// with that run ID still exist); leaving it would show the group
		// "running" forever.
		if processAlive(p.PID) && time.Since(p.Started) < maxPendingAge {
			slog.Info("leaving pending run alone; its process is still running",
				"group", p.Group, "run_id", runID, "pid", p.PID)
			continue
		}
		names, err := reap(ctx, runID)
		if err != nil {
			slog.Warn("cannot reap stale dump helpers; will retry next startup",
				"group", p.Group, "run_id", runID, "error", err)
			continue
		}
		if len(names) > 0 {
			slog.Warn("reaped dump helpers orphaned by a dead manager process",
				"group", p.Group, "run_id", runID, "started", p.Started.Format(time.RFC3339),
				"containers", strings.Join(names, ","))
		}
		store.ClearPending(runID)
	}
}

// processAlive reports whether pid is a live process. Signal 0 performs the
// permission and existence checks without delivering anything; EPERM means the
// process exists but belongs to another user, which still counts as alive.
//
// A pid of 0 means "no owner recorded", a legacy record, treated as dead.
// PID reuse could in principle mistake an unrelated process for the owner and
// skip a reap; the helper is then cleaned up at a later startup, which is the
// safe direction (a stray container beats killing a live backup's).
func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
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
	reapStalePendingRuns(ctx, store, e.reapRunHelpers)
	s := scheduler.NewScheduler(r, e.rt, slog.Default(), e.cfg, gen, store)
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
		// An interrupt stops the loop; everything from here on never ran.
		if ctx.Err() != nil {
			unattempted = append(unattempted, targets[i:]...)
			break
		}
		acquired, runErr := r.TryRunGroup(ctx, name, meta[name])
		// If the interrupt landed while this group was running, TryRunGroup
		// returns an error, but it is an interruption, not a backup failure.
		// Bucket this group and the rest as not-run so the summary stays
		// honest (the whole point of the interrupt tracking).
		if ctx.Err() != nil {
			unattempted = append(unattempted, targets[i:]...)
			break
		}
		switch {
		case errors.Is(runErr, runner.ErrLockedByAnotherProcess):
			// The daemon, or a concurrent ad-hoc run, holds this group's
			// repo/snapshot lock. Ad-hoc never queues: report and move on so
			// the operator can retry, rather than blocking on someone's backup.
			locked = append(locked, name)
		case runErr != nil:
			failed = append(failed, name)
		case !acquired:
			// Held in-process; can't happen in this sequential loop, but a
			// silent "success" here would be a lie.
			locked = append(locked, name)
		default:
			store.MarkSuccess(name, scheduler.GroupFingerprint(backupState.Groups[name]), now)
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

	state, err := discovery.Discover(ctx, e.rt, logger)
	if err != nil {
		return err
	}

	if len(state.Groups) == 0 {
		return fmt.Errorf("no backup groups discovered, check your labels (warnings above, if any, explain near-misses)")
	}

	printGroups(state, stateStore(e, logger))
	return nil
}

// stateStore loads the persisted schedule for one-shot display commands.
func stateStore(e *env, logger *slog.Logger) *state.ScheduleStore {
	return state.LoadSchedule(e.stateDir, logger)
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

	state, err := discovery.Discover(ctx, e.rt, logger)
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

	meta, err := e.newGenerator(configsDir, logger).Generate(state)
	if err != nil {
		return err
	}
	if _, ok := meta[group]; !ok {
		return fmt.Errorf("unknown group %q; %s", group, discoveredGroupList(state))
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
