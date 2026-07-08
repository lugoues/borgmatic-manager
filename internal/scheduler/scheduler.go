package scheduler

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/lugoues/borgmatic-manager/internal/config"
	"github.com/lugoues/borgmatic-manager/internal/discovery"
	"github.com/lugoues/borgmatic-manager/internal/models"
	"github.com/lugoues/borgmatic-manager/internal/runtime"
	"github.com/lugoues/borgmatic-manager/internal/state"
)

// minWake floors the dynamic timer so an overdue-but-unrunnable group
// (e.g. one that keeps failing) can't spin the cycle loop hot.
const minWake = 30 * time.Second

// discoverTimeout bounds discovery + generation per cycle: a wedged daemon
// socket would otherwise block RunCycle forever and stop all scheduling.
const discoverTimeout = 2 * time.Minute

// GroupRunner abstracts runner.Runner for testability.
type GroupRunner interface {
	TryRunGroup(ctx context.Context, groupName string, meta config.GroupRunMeta) (bool, error)
}

// Scheduler drives discover -> generate -> run cycles, running each group only
// when due. Persisted last-success times let a restart resume the schedule.
type Scheduler struct {
	runner    GroupRunner
	rt        runtime.ContainerRuntime
	logger    *slog.Logger
	cfg       *config.ManagerConfig
	generator *config.Generator

	// store persists last-success times; nil disables dueness gating entirely.
	store  *state.ScheduleStore
	period time.Duration

	// cycleMu makes cycles mutually exclusive: reconcile in one cycle could
	// otherwise delete what a concurrent cycle just wrote.
	cycleMu sync.Mutex

	// lastAttempt (in-memory only) marks run starts so next-wake retries failures
	// after a period instead of hot-looping; a restart makes them due again.
	mu          sync.Mutex
	lastAttempt map[string]time.Time

	// now is overridable for testing.
	now func() time.Time

	// discoverFunc and generateFunc are overridable for testing.
	discoverFunc func(ctx context.Context) (*models.BackupState, error)
	generateFunc func(state *models.BackupState) (map[string]config.GroupRunMeta, error)
}

// NewScheduler creates a Scheduler; a nil store disables dueness gating.
func NewScheduler(
	runner GroupRunner,
	rt runtime.ContainerRuntime,
	logger *slog.Logger,
	cfg *config.ManagerConfig,
	generator *config.Generator,
	store *state.ScheduleStore,
) *Scheduler {
	s := &Scheduler{
		runner:      runner,
		rt:          rt,
		logger:      logger,
		cfg:         cfg,
		generator:   generator,
		store:       store,
		lastAttempt: map[string]time.Time{},
		now:         time.Now,
	}

	// An unparseable period surfaces from Start; until then 0 means always due.
	if period, err := time.ParseDuration(cfg.Manager.Period); err == nil {
		s.period = period
	}

	s.discoverFunc = func(ctx context.Context) (*models.BackupState, error) {
		return discovery.Discover(ctx, s.rt, s.logger)
	}
	s.generateFunc = func(state *models.BackupState) (map[string]config.GroupRunMeta, error) {
		return s.generator.Generate(state)
	}

	return s
}

// GroupFingerprint identifies a group's backup content set: volume/database
// membership changes alter it, config-label changes do not.
func GroupFingerprint(group *models.VolumeGroup) string {
	lines := make([]string, 0, len(group.Volumes)+len(group.Databases))
	for _, v := range group.Volumes {
		lines = append(lines, "volume\x00"+v.Name)
	}
	for _, db := range group.Databases {
		lines = append(lines, strings.Join([]string{"db", db.Type, db.Name, db.Container, db.Hostname, db.Path}, "\x00"))
	}
	sort.Strings(lines)
	sum := sha256.Sum256([]byte(strings.Join(lines, "\n")))
	return hex.EncodeToString(sum[:])
}

// groupDue reports whether a group should run now, and when it is next due
// otherwise. Unknown groups, changed membership, and clock rollbacks all
// resolve to "due": the safe failure direction is an extra backup.
func (s *Scheduler) groupDue(name, fingerprint string, now time.Time) (bool, time.Time) {
	if s.store == nil || s.period <= 0 {
		return true, now
	}
	rec, ok := s.store.Record(name)
	if !ok || rec.Fingerprint != fingerprint || rec.LastSuccess.After(now) {
		return true, now
	}
	next := rec.LastSuccess.Add(s.period)
	return !now.Before(next), next
}

// RunAllGroups runs every due group in parallel goroutines; per-group errors
// are logged but do not abort the tick.
func (s *Scheduler) RunAllGroups(ctx context.Context, backupState *models.BackupState, meta map[string]config.GroupRunMeta) {
	// Sort group names for deterministic log output.
	names := make([]string, 0, len(backupState.Groups))
	for name := range backupState.Groups {
		names = append(names, name)
	}
	sort.Strings(names)

	now := s.now()
	live := make(map[string]struct{}, len(names))
	waiting := 0

	var wg sync.WaitGroup

	for _, name := range names {
		group := backupState.Groups[name]

		if len(group.Volumes) == 0 && len(group.Databases) == 0 {
			s.logger.Debug("skipping group with no volumes or databases", "group", name)
			continue
		}

		// Refused by generation (no config, no lock keys): skip it and prune its schedule record.
		m, ok := meta[name]
		if !ok {
			s.logger.Debug("skipping group without a generated config", "group", name)
			continue
		}
		live[name] = struct{}{}

		fingerprint := GroupFingerprint(group)
		if due, next := s.groupDue(name, fingerprint, now); !due {
			s.logger.Debug("group not due", "group", name, "next_run", next.Format(time.RFC3339))
			waiting++
			continue
		}

		// Optimistic attempt mark keeps NextWake from hot-looping; restored below
		// if the run lock was held.
		s.mu.Lock()
		prevAttempt, hadAttempt := s.lastAttempt[name]
		s.lastAttempt[name] = now
		s.mu.Unlock()

		wg.Add(1)
		go func(groupName, fingerprint string, m config.GroupRunMeta, prevAttempt time.Time, hadAttempt bool) {
			defer wg.Done()

			acquired, err := s.runner.TryRunGroup(ctx, groupName, m)
			if !acquired && err == nil {
				s.logger.Debug("skipping group, already running", "group", groupName)
				s.mu.Lock()
				if hadAttempt {
					s.lastAttempt[groupName] = prevAttempt
				} else {
					delete(s.lastAttempt, groupName)
				}
				s.mu.Unlock()
				return
			}
			if err != nil {
				s.logger.Warn("group backup error", "group", groupName, "error", err)
				return
			}
			if s.store != nil {
				s.store.MarkSuccess(groupName, fingerprint, now)
			}
		}(name, fingerprint, m, prevAttempt, hadAttempt)
	}

	if s.store != nil {
		// Drop schedule state for vanished groups so it can't distort
		// next-wake computation or grow without bound.
		s.store.Retain(live)
		s.mu.Lock()
		for name := range s.lastAttempt {
			if _, ok := live[name]; !ok {
				delete(s.lastAttempt, name)
			}
		}
		s.mu.Unlock()

		if waiting > 0 {
			s.logger.Info("cycle plan", "due", len(live)-waiting, "waiting", waiting)
		}
	}

	wg.Wait()
}

// RunCycle performs a full discover -> generate -> run cycle. Cycles are
// mutually exclusive: an event trigger arriving mid-periodic-cycle waits.
func (s *Scheduler) RunCycle(ctx context.Context) error {
	s.cycleMu.Lock()
	defer s.cycleMu.Unlock()

	s.logger.Info("starting backup cycle")

	dctx, cancel := context.WithTimeout(ctx, discoverTimeout)
	defer cancel()

	backupState, err := s.discoverFunc(dctx)
	if err != nil {
		return err
	}

	meta, err := s.generateFunc(backupState)
	if err != nil {
		return err
	}

	s.RunAllGroups(ctx, backupState, meta)
	return nil
}

// NextWake returns the sleep until the earliest group comes due, clamped to
// [minWake, period]; with no history it is one full period.
func (s *Scheduler) NextWake() time.Duration {
	if s.store == nil || s.period <= 0 {
		return s.period
	}

	now := s.now()
	wake := s.period

	records := s.store.Snapshot()
	s.mu.Lock()
	for name, attempt := range s.lastAttempt {
		rec := records[name]
		if attempt.After(rec.LastSuccess) {
			rec.LastSuccess = attempt
			records[name] = rec
		}
	}
	s.mu.Unlock()

	for _, rec := range records {
		if rec.MissingCycles > 0 {
			continue // absent groups don't drive the wake timer
		}
		if d := rec.LastSuccess.Add(s.period).Sub(now); d < wake {
			wake = d
		}
	}

	if wake < minWake {
		wake = minWake
	}
	return wake
}

// Start blocks, waking at each next-due time until the context is
// cancelled. The orchestrator owns the startup cycle, so the first wake is
// never immediate.
func (s *Scheduler) Start(ctx context.Context) error {
	// period is immutable after construction (groupDue reads it concurrently);
	// this guard covers non-daemon constructions that skip preflight.
	if s.period <= 0 {
		return fmt.Errorf("invalid manager.period %q: must be a positive duration", s.cfg.Manager.Period)
	}

	s.logger.Info("scheduler starting", "period", s.period)

	timer := time.NewTimer(s.NextWake())
	defer timer.Stop()

	for {
		select {
		case <-timer.C:
			if err := s.RunCycle(ctx); err != nil {
				s.logger.Error("cycle failed", "error", err)
			}
			wake := s.NextWake()
			s.logger.Debug("scheduler sleeping", "until_next_due", wake.Round(time.Second).String())
			timer.Reset(wake)
		case <-ctx.Done():
			s.logger.Info("scheduler stopping")
			return nil
		}
	}
}
