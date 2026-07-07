//
// Commands:
//
//	borgmatic-manager [run]              run the daemon (default)
//	borgmatic-manager discover           one-shot discovery, print groups and exit
//	borgmatic-manager generate [-output] one-shot config generation and exit
//	borgmatic-manager version            print the version and exit
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/coreos/go-systemd/v22/daemon"

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

func main() {
	cmd := "run"
	args := os.Args[1:]
	if len(args) > 0 {
		cmd = args[0]
		args = args[1:]
	}

	switch cmd {
	case "run":
		os.Exit(runDaemon())
	case "discover":
		os.Exit(runDiscover())
	case "generate":
		os.Exit(runGenerate(args))
	case "borgmatic":
		os.Exit(runBorgmaticPassthrough(args))
	case "version", "--version", "-v":
		fmt.Println(version)
	case "help", "--help", "-h":
		fmt.Print(usage)
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n%s", cmd, usage)
		os.Exit(2)
	}
}

const usage = `usage: borgmatic-manager [command]

commands:
  run        run the manager daemon (default)
  discover   discover labeled volumes/containers, print them, and exit
  generate   generate borgmatic configs once and exit (-output DIR)
  borgmatic  run borgmatic against a group's generated config:
               borgmatic-manager borgmatic <group> [borgmatic args...]
             e.g. repo-create --encryption repokey-blake2 | list | extract
  version    print the version and exit

environment:
  CONFIG_DIR        config directory        (default /etc/borgmatic-manager)
  RUNTIME_DIR       runtime directory       (default /run/borgmatic-manager)
  STATE_DIR         state directory         (default /var/lib/borgmatic-manager)
  CONTAINER_SOCKET  docker/podman socket    (default /var/run/docker.sock)
  BORGMATIC_PATH    borgmatic binary        (default: config, then PATH)
`

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

func runDaemon() int {
	// Structured JSON logging to stdout (journald captures it).
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)))

	// One SIGTERM or SIGINT shuts down cleanly; the runner forwards it to in-flight borgmatic.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	e, err := loadEnv()
	if err != nil {
		slog.Error("startup failed", "error", err)
		return 1
	}

	pf, err := preflight(ctx, e)
	if err != nil {
		slog.Error("preflight failed", "error", err)
		return 1
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
		return 1
	}

	slog.Info("borgmatic-manager stopped")
	return 0
}

func runDiscover() int {
	logger := textLogger()
	ctx := context.Background()

	e, err := loadEnv()
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}

	state, err := discoverState(ctx, e, logger)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}

	if len(state.Groups) == 0 {
		fmt.Println("no backup groups discovered, check your labels (warnings above, if any, explain near-misses)")
		return 1
	}

	for name, group := range state.Groups {
		fmt.Printf("group %s:\n", name)
		for _, v := range group.Volumes {
			fmt.Printf("  volume    %-20s %s\n", v.Name, v.HostPath)
		}
		for _, db := range group.Databases {
			target := "container=" + db.Container
			if db.Type == "sqlite" {
				target = db.Path
			} else if db.Hostname != "" {
				target = fmt.Sprintf("hostname=%s port=%d", db.Hostname, db.Port)
			}
			fmt.Printf("  database  %-20s %s\n", db.Type+"/"+db.Name, target)
		}
	}
	return 0
}

func runGenerate(args []string) int {
	fs := flag.NewFlagSet("generate", flag.ExitOnError)
	output := fs.String("output", "", "output directory (default: $RUNTIME_DIR/configs)")
	_ = fs.Parse(args)

	logger := textLogger()
	ctx := context.Background()

	e, err := loadEnv()
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}

	outDir := *output
	if outDir == "" {
		outDir = e.configsDir
	}

	state, err := discoverState(ctx, e, logger)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}

	gen := e.newGenerator(outDir, logger)
	meta, err := gen.Generate(state)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}

	for group := range meta {
		fmt.Println(filepath.Join(outDir, group+".yaml"))
	}
	return 0
}

// discoverState runs one discovery pass for the one-shot commands.
func discoverState(ctx context.Context, e *env, logger *slog.Logger) (*models.BackupState, error) {
	return discovery.Discover(ctx, e.rt, logger)
}

// textLogger logs human-readable warnings/errors to stderr for one-shot
// commands, keeping stdout for the command's own output.
func textLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
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

// runBorgmaticPassthrough regenerates the group's config from live labels and
// execs borgmatic with it: the supported way to touch a group's repository.
func runBorgmaticPassthrough(args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: borgmatic-manager borgmatic <group> [borgmatic args...]")
		return 2
	}
	group := args[0]
	if strings.HasPrefix(group, "-") {
		fmt.Fprintf(os.Stderr, "error: the first argument must be a group name, got flag %q\nusage: borgmatic-manager borgmatic <group> [borgmatic args...]   e.g.: borgmatic-manager borgmatic myapp create --dry-run\nrun 'borgmatic-manager discover' to list groups\n", group)
		return 2
	}

	logger := textLogger()
	ctx := context.Background()

	e, err := loadEnv()
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}

	state, err := discoverState(ctx, e, logger)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}

	gen := e.newGenerator(e.configsDir, logger)
	meta, err := gen.Generate(state)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	if _, ok := meta[group]; !ok {
		groups := make([]string, 0, len(meta))
		for name := range meta {
			groups = append(groups, name)
		}
		sort.Strings(groups)
		fmt.Fprintf(os.Stderr, "error: unknown group %q; discovered groups: %s\n", group, strings.Join(groups, ", "))
		return 1
	}

	borgmaticPath, err := resolveBorgmatic(e.cfg)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}

	configPath := filepath.Join(e.configsDir, group+".yaml")
	argv := append([]string{borgmaticPath, "--config", configPath}, args[1:]...)
	env := os.Environ()

	// Replace the process: borgmatic owns the terminal from here.
	// #nosec G702 G204 -- deliberately exec'ing the resolved borgmatic binary with the operator's own CLI arguments
	if err := syscall.Exec(borgmaticPath, argv, env); err != nil {
		fmt.Fprintln(os.Stderr, "error executing borgmatic:", err)
		return 1
	}
	return 0
}
