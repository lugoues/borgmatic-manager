package scheduler

import (
	"context"
	"log/slog"
	"sort"
	"sync"
	"time"

	"github.com/lugoues/borgmatic-manager/internal/config"
	"github.com/lugoues/borgmatic-manager/internal/discovery"
	"github.com/lugoues/borgmatic-manager/internal/models"
	"github.com/lugoues/borgmatic-manager/internal/runtime"
)

// GroupRunner abstracts runner.Runner for testability.
type GroupRunner interface {
	TryRunGroup(ctx context.Context, groupName string, group *models.VolumeGroup, cfg *config.ManagerConfig) (bool, error)
}

// Scheduler runs a periodic ticker, invoking borgmatic for all discovered
// groups in parallel on each tick. Groups with no volumes are skipped, and
// the runner's per-group mutex prevents overlapping runs.
type Scheduler struct {
	runner         GroupRunner
	rt             runtime.ContainerRuntime
	logger         *slog.Logger
	cfg            *config.ManagerConfig
	groupOverrides map[string]map[string]interface{}
	outputDir      string

	// discoverFunc and generateFunc are overridable for testing.
	discoverFunc func(ctx context.Context) (*models.BackupState, error)
	generateFunc func(state *models.BackupState) error
}

// NewScheduler creates a new Scheduler with the given dependencies.
func NewScheduler(
	runner GroupRunner,
	rt runtime.ContainerRuntime,
	logger *slog.Logger,
	cfg *config.ManagerConfig,
	groupOverrides map[string]map[string]interface{},
	outputDir string,
) *Scheduler {
	s := &Scheduler{
		runner:         runner,
		rt:             rt,
		logger:         logger,
		cfg:            cfg,
		groupOverrides: groupOverrides,
		outputDir:      outputDir,
	}

	s.discoverFunc = func(ctx context.Context) (*models.BackupState, error) {
		return discovery.Discover(ctx, s.rt, s.logger)
	}
	s.generateFunc = func(state *models.BackupState) error {
		return config.GenerateConfigs(state, s.cfg, s.groupOverrides, s.outputDir)
	}

	return s
}

// RunAllGroups iterates all groups in the given state and runs them in parallel
// goroutines. Groups with no volumes are skipped. Errors from individual groups
// are logged but do not abort the tick.
func (s *Scheduler) RunAllGroups(ctx context.Context, state *models.BackupState) {
	// Sort group names for deterministic log output.
	names := make([]string, 0, len(state.Groups))
	for name := range state.Groups {
		names = append(names, name)
	}
	sort.Strings(names)

	var wg sync.WaitGroup

	for _, name := range names {
		group := state.Groups[name]

		// Skip groups with no volumes.
		if len(group.Volumes) == 0 {
			s.logger.Debug("skipping group with no volumes", "group", name)
			continue
		}

		wg.Add(1)
		go func(groupName string, g *models.VolumeGroup) {
			defer wg.Done()

			acquired, err := s.runner.TryRunGroup(ctx, groupName, g, s.cfg)
			if err != nil {
				s.logger.Warn("group backup error", "group", groupName, "error", err)
				return
			}
			if !acquired {
				s.logger.Debug("skipping group, already running", "group", groupName)
			}
		}(name, group)
	}

	wg.Wait()
}

// RunCycle performs a full discover -> generate -> run cycle.
func (s *Scheduler) RunCycle(ctx context.Context) error {
	s.logger.Info("starting backup cycle")

	state, err := s.discoverFunc(ctx)
	if err != nil {
		return err
	}

	if err := s.generateFunc(state); err != nil {
		return err
	}

	s.RunAllGroups(ctx, state)
	return nil
}

// Start blocks, running a ticker loop until the context is cancelled.
// It runs an initial cycle immediately on startup, then fires at the
// configured period interval.
func (s *Scheduler) Start(ctx context.Context) error {
	period, err := time.ParseDuration(s.cfg.Manager.Period)
	if err != nil {
		return err
	}

	s.logger.Info("scheduler starting", "period", period)

	// Run initial cycle immediately on startup.
	if err := s.RunCycle(ctx); err != nil {
		s.logger.Error("initial cycle failed", "error", err)
	}

	ticker := time.NewTicker(period)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			if err := s.RunCycle(ctx); err != nil {
				s.logger.Error("cycle failed", "error", err)
			}
		case <-ctx.Done():
			s.logger.Info("scheduler stopping")
			return nil
		}
	}
}
