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

	// Config paths from environment or defaults.
	configDir := getEnv("CONFIG_DIR", "/etc/borgmatic-manager")
	managerPath := filepath.Join(configDir, "manager.yaml")
	groupsDir := filepath.Join(configDir, "groups")
	outputDir := filepath.Join(configDir, "generated")

	// Load configuration.
	cfg, groupOverrides, err := config.LoadConfig(managerPath, groupsDir)
	if err != nil {
		slog.Error("failed to load config", "error", err)
		os.Exit(1)
	}

	// Create container runtime.
	rt, err := runtime.NewDockerRuntime()
	if err != nil {
		slog.Error("failed to create container runtime", "error", err)
		os.Exit(1)
	}

	r := runner.NewRunner(rt, slog.Default(), outputDir)
	s := scheduler.NewScheduler(r, rt, slog.Default(), cfg, groupOverrides, outputDir)
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
