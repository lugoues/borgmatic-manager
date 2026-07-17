package scheduler

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/lugoues/borgmatic-manager/internal/config"
	"github.com/lugoues/borgmatic-manager/internal/discovery"
	"github.com/lugoues/borgmatic-manager/internal/models"
	"github.com/lugoues/borgmatic-manager/internal/runner"
	"github.com/lugoues/borgmatic-manager/internal/runtime"
	"github.com/lugoues/borgmatic-manager/internal/state"
)

// minWake floors the dynamic timer so an overdue-but-unrunnable group
// (e.g. one that keeps failing) can't spin the cycle loop hot.
const minWake = 30 * time.Second

// lockRetryInterval retries a lock-blocked group sooner than a full period (a
// failed holder records no success) but slower than minWake (no hot cycling).
const lockRetryInterval = 5 * time.Minute

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

	// lockedRetry holds per-group retry times for lock-blocked runs, so NextWake
	// wakes in lockRetryInterval rather than a full period.
	lockedRetry map[string]time.Time

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
		lockedRetry: map[string]time.Time{},
		now:         time.Now,
	}

	// An unparseable period surfaces from Start; until then 0 means always due.
	if period, err := cfg.ParsedPeriod(); err == nil {
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

// Dueness describes whether a group should run now, and why. It is the single
// authority on the rules, so scheduler, status, and inspect cannot disagree.
type Dueness struct {
	Due bool
	// Next is when the group next comes due; meaningful only when !Due.
	Next time.Time
	// MembershipChanged reports the content set changed since last success,
	// which makes the group due regardless of the period.
	MembershipChanged bool
}

// Due applies the dueness rules; period <= 0 means always due. Unknown groups,
// changed membership, and clock rollbacks resolve to due: an extra backup is
// the safe failure direction.
func Due(rec state.GroupRecord, haveRec bool, fingerprint string, period time.Duration, now time.Time) Dueness {
	if period <= 0 || !haveRec {
		return Dueness{Due: true, Next: now}
	}
	if rec.Fingerprint != fingerprint {
		return Dueness{Due: true, Next: now, MembershipChanged: true}
	}
	if rec.LastSuccess.After(now) {
		return Dueness{Due: true, Next: now} // clock rolled back
	}
	next := rec.LastSuccess.Add(period)
	return Dueness{Due: !now.Before(next), Next: next}
}

func (s *Scheduler) groupDue(name, fingerprint string, now time.Time) (bool, time.Time) {
	if s.store == nil {
		return true, now
	}
	rec, ok := s.store.Record(name)
	d := Due(rec, ok, fingerprint, s.period, now)
	return d.Due, d.Next
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
			// Succeeded within the period (maybe via the other process): clear any pending lock-retry.
			s.logger.Debug("group not due", "group", name, "next_run", next.Format(time.RFC3339))
			s.mu.Lock()
			delete(s.lockedRetry, name)
			s.mu.Unlock()
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
			switch {
			case errors.Is(err, runner.ErrLockedByAnotherProcess):
				// Keep the attempt mark (else NextWake spins at minWake) but schedule a
				// bounded retry in case the other process's run fails.
				s.logger.Debug("skipping group, locked by another process", "group", groupName)
				s.mu.Lock()
				s.lockedRetry[groupName] = now.Add(lockRetryInterval)
				s.mu.Unlock()

			case !acquired && err == nil:
				// An in-flight run in this process owns the attempt mark; restore ours
				// so the skip doesn't push the next wake out a period.
				s.logger.Debug("skipping group, already running", "group", groupName)
				s.mu.Lock()
				if hadAttempt {
					s.lastAttempt[groupName] = prevAttempt
				} else {
					delete(s.lastAttempt, groupName)
				}
				delete(s.lockedRetry, groupName)
				s.mu.Unlock()

			case err != nil:
				s.logger.Warn("group backup error", "group", groupName, "error", err)
				s.mu.Lock()
				delete(s.lockedRetry, groupName)
				s.mu.Unlock()

			default:
				if s.store != nil {
					s.store.MarkSuccess(groupName, fingerprint, now)
				}
				s.mu.Lock()
				delete(s.lockedRetry, groupName)
				s.mu.Unlock()
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
		for name := range s.lockedRetry {
			if _, ok := live[name]; !ok {
				delete(s.lockedRetry, name)
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

	// Pick up other processes' writes before deciding dueness: an ad-hoc run may
	// have just recorded a success.
	if s.store != nil {
		s.store.Reload()
	}

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
	// A group awaiting a lock retry wakes us at its retry time, not a full
	// period out (the optimistic attempt mark above would otherwise hide it).
	for _, retry := range s.lockedRetry {
		if d := retry.Sub(now); d < wake {
			wake = d
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
