package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
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

	root.AddCommand(runCmd(), discoverCmd(), generateCmd(), borgmaticCmd(), versionCmd())

	if err := fang.Execute(context.Background(), root, fang.WithVersion(version)); err != nil {
		os.Exit(1)
	}
}

func runCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "run",
		Short: "Run the manager daemon",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runDaemon()
		},
	}
}

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
  borgmatic-manager borgmatic myapp extract --archive latest`,
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
		ContainerCLI: detectContainerCLI(),
	}, logger)
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
	r := runner.NewRunner(slog.Default(), e.configsDir, pf.borgmaticPath, e.cfg.Manager.Actions, pf.runTimeout)
	s := scheduler.NewScheduler(r, e.rt, slog.Default(), e.cfg, gen)
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

func runDiscover() error {
	logger := interactiveLogger()
	ctx := context.Background()

	e, err := loadEnv()
	if err != nil {
		return err
	}

	state, err := discoverState(ctx, e, logger)
	if err != nil {
		return err
	}

	if len(state.Groups) == 0 {
		return fmt.Errorf("no backup groups discovered, check your labels (warnings above, if any, explain near-misses)")
	}

	printGroups(state)
	return nil
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

	state, err := discoverState(ctx, e, logger)
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

	state, err := discoverState(ctx, e, logger)
	if err != nil {
		return err
	}

	gen := e.newGenerator(e.configsDir, logger)
	meta, err := gen.Generate(state)
	if err != nil {
		return err
	}
	if _, ok := meta[group]; !ok {
		groups := make([]string, 0, len(meta))
		for name := range meta {
			groups = append(groups, name)
		}
		sort.Strings(groups)
		return fmt.Errorf("unknown group %q; discovered groups: %s", group, strings.Join(groups, ", "))
	}

	borgmaticPath, err := resolveBorgmatic(e.cfg)
	if err != nil {
		return err
	}

	configPath := filepath.Join(e.configsDir, group+".yaml")
	argv := append([]string{borgmaticPath, "--config", configPath}, args[1:]...)

	// Replace the process: borgmatic owns the terminal from here.
	// #nosec G702 G204 -- deliberately exec'ing the resolved borgmatic binary with the operator's own CLI arguments
	if err := syscall.Exec(borgmaticPath, argv, os.Environ()); err != nil {
		return fmt.Errorf("executing borgmatic: %w", err)
	}
	return nil
}

// discoverState runs one discovery pass for the one-shot commands.
func discoverState(ctx context.Context, e *env, logger *slog.Logger) (*models.BackupState, error) {
	return discovery.Discover(ctx, e.rt, logger)
}

// interactiveLogger renders human-friendly, styled warnings/errors on stderr
// for one-shot commands, keeping stdout for the command's own output. The
// daemon uses JSON instead (journald).
func interactiveLogger() *slog.Logger {
	handler := charmlog.NewWithOptions(os.Stderr, charmlog.Options{
		ReportTimestamp: false,
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
