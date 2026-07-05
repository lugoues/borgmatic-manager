// It wires all dependencies, sets up signal handling, and runs the orchestrator.
package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/lugoues/borgmatic-manager/internal/config"
	"github.com/lugoues/borgmatic-manager/internal/events"
	"github.com/lugoues/borgmatic-manager/internal/orchestrator"
	"github.com/lugoues/borgmatic-manager/internal/runner"
	"github.com/lugoues/borgmatic-manager/internal/runtime"
	"github.com/lugoues/borgmatic-manager/internal/scheduler"
)

func main() {
	// Structured JSON logging to stdout.
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)))

	// Signal handling: single SIGTERM or SIGINT causes clean shutdown.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	// Directory layout from environment or systemd-conventional defaults.
	configDir := getEnv("CONFIG_DIR", "/etc/borgmatic-manager")
	runtimeDir := getEnv("RUNTIME_DIR", "/run/borgmatic-manager")
	stateDir := getEnv("STATE_DIR", "/var/lib/borgmatic-manager")
	managerPath := filepath.Join(configDir, "manager.yaml")
	groupsDir := filepath.Join(configDir, "groups")
	configsDir := filepath.Join(runtimeDir, "configs")

	// Load configuration.
	cfg, groupOverrides, err := config.LoadConfig(managerPath, groupsDir)
	if err != nil {
		slog.Error("failed to load config", "error", err)
		os.Exit(1)
	}

	// Create the container runtime client (discovery + events).
	rt, err := runtime.NewDockerRuntime()
	if err != nil {
		slog.Error("failed to create container runtime client", "error", err)
		os.Exit(1)
	}

	gen := config.NewGenerator(cfg, groupOverrides, configsDir, config.GeneratorOptions{
		RuntimeDir: runtimeDir,
		StateDir:   stateDir,
		Rootless:   rt.Rootless(ctx),
	}, slog.Default())
	r := runner.NewRunner(rt, slog.Default(), configsDir)
	s := scheduler.NewScheduler(r, rt, slog.Default(), cfg, gen)
	l := events.NewListener(rt, slog.Default())
	o := orchestrator.NewOrchestrator(s, l, slog.Default())

	slog.Info("borgmatic-manager starting", "period", cfg.Manager.Period, "config_dir", configDir)

	if err := o.Run(ctx); err != nil {
		slog.Error("fatal error", "error", err)
		os.Exit(1)
	}

	slog.Info("borgmatic-manager stopped")
}

func getEnv(key, defaultValue string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return defaultValue
}
