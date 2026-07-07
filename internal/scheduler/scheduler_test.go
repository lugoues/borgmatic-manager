package scheduler

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/lugoues/borgmatic-manager/internal/config"
	"github.com/lugoues/borgmatic-manager/internal/models"
	"github.com/lugoues/borgmatic-manager/internal/state"
)

// mockGroupRunner records calls to TryRunGroup for assertions.
type mockGroupRunner struct {
	mu      sync.Mutex
	calls   []string
	results map[string]tryRunResult
}

type tryRunResult struct {
	acquired bool
	err      error
}

func newMockGroupRunner() *mockGroupRunner {
	return &mockGroupRunner{
		results: make(map[string]tryRunResult),
	}
}

func (m *mockGroupRunner) TryRunGroup(ctx context.Context, groupName string, meta config.GroupRunMeta) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls = append(m.calls, groupName)
	if r, ok := m.results[groupName]; ok {
		return r.acquired, r.err
	}
	return true, nil
}

func (m *mockGroupRunner) getCalls() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	result := make([]string, len(m.calls))
	copy(result, m.calls)
	sort.Strings(result)
	return result
}

func TestRunAllGroups_Parallel(t *testing.T) {
	runner := newMockGroupRunner()
	cfg := &config.ManagerConfig{
		Manager: config.ManagerSettings{Period: "1h"},
	}
	logger := slog.Default()

	s := NewScheduler(runner, nil, logger, cfg, nil, nil)

	state := models.NewBackupState()
	state.AddVolume("group-a", models.VolumeInfo{Name: "vol1", HostPath: "/mnt/vol1"})
	state.AddVolume("group-b", models.VolumeInfo{Name: "vol2", HostPath: "/mnt/vol2"})
	state.AddVolume("group-c", models.VolumeInfo{Name: "vol3", HostPath: "/mnt/vol3"})

	s.RunAllGroups(context.Background(), state, nil)

	calls := runner.getCalls()
	expected := []string{"group-a", "group-b", "group-c"}
	if len(calls) != len(expected) {
		t.Fatalf("expected %d calls, got %d: %v", len(expected), len(calls), calls)
	}
	for i, name := range expected {
		if calls[i] != name {
			t.Errorf("expected call[%d] = %q, got %q", i, name, calls[i])
		}
	}
}

func TestRunAllGroups_SkipEmptyGroups(t *testing.T) {
	runner := newMockGroupRunner()
	cfg := &config.ManagerConfig{
		Manager: config.ManagerSettings{Period: "1h"},
	}
	logger := slog.Default()

	s := NewScheduler(runner, nil, logger, cfg, nil, nil)

	state := models.NewBackupState()
	state.AddVolume("has-volumes", models.VolumeInfo{Name: "vol1", HostPath: "/mnt/vol1"})
	// "empty-group" has no volumes (only databases or nothing)
	state.Groups["empty-group"] = &models.VolumeGroup{}

	s.RunAllGroups(context.Background(), state, nil)

	calls := runner.getCalls()
	if len(calls) != 1 {
		t.Fatalf("expected 1 call, got %d: %v", len(calls), calls)
	}
	if calls[0] != "has-volumes" {
		t.Errorf("expected call for has-volumes, got %q", calls[0])
	}
}

func TestRunAllGroups_MutexSkip(t *testing.T) {
	runner := newMockGroupRunner()
	runner.results["busy-group"] = tryRunResult{acquired: false, err: nil}

	cfg := &config.ManagerConfig{
		Manager: config.ManagerSettings{Period: "1h"},
	}
	logger := slog.Default()

	s := NewScheduler(runner, nil, logger, cfg, nil, nil)

	state := models.NewBackupState()
	state.AddVolume("busy-group", models.VolumeInfo{Name: "vol1", HostPath: "/mnt/vol1"})
	state.AddVolume("free-group", models.VolumeInfo{Name: "vol2", HostPath: "/mnt/vol2"})

	s.RunAllGroups(context.Background(), state, nil)

	calls := runner.getCalls()
	if len(calls) != 2 {
		t.Fatalf("expected 2 calls, got %d: %v", len(calls), calls)
	}
	// Both should be called - the "skip" is handled inside TryRunGroup returning false.
	// The scheduler should not error on (false, nil).
}

func TestRunAllGroups_ErrorContinues(t *testing.T) {
	runner := newMockGroupRunner()
	runner.results["error-group"] = tryRunResult{acquired: true, err: errors.New("backup failed")}

	cfg := &config.ManagerConfig{
		Manager: config.ManagerSettings{Period: "1h"},
	}
	logger := slog.Default()

	s := NewScheduler(runner, nil, logger, cfg, nil, nil)

	state := models.NewBackupState()
	state.AddVolume("error-group", models.VolumeInfo{Name: "vol1", HostPath: "/mnt/vol1"})
	state.AddVolume("ok-group", models.VolumeInfo{Name: "vol2", HostPath: "/mnt/vol2"})

	// Should not panic or abort; both groups should be attempted.
	s.RunAllGroups(context.Background(), state, nil)

	calls := runner.getCalls()
	if len(calls) != 2 {
		t.Fatalf("expected 2 calls (both groups attempted), got %d: %v", len(calls), calls)
	}
}

func TestRunCycle_DiscoverAndGenerate(t *testing.T) {
	runner := newMockGroupRunner()
	cfg := &config.ManagerConfig{
		Manager: config.ManagerSettings{Period: "1h"},
	}
	logger := slog.Default()

	state := models.NewBackupState()
	state.AddVolume("test-group", models.VolumeInfo{Name: "vol1", HostPath: "/mnt/vol1"})

	discoverCalled := false
	generateCalled := false

	s := NewScheduler(runner, nil, logger, cfg, nil, nil)

	// Override discover and generate funcs for testing.
	s.discoverFunc = func(ctx context.Context) (*models.BackupState, error) {
		discoverCalled = true
		return state, nil
	}
	s.generateFunc = func(st *models.BackupState) (map[string]config.GroupRunMeta, error) {
		generateCalled = true
		return nil, nil
	}

	err := s.RunCycle(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !discoverCalled {
		t.Error("expected Discover to be called")
	}
	if !generateCalled {
		t.Error("expected GenerateConfigs to be called")
	}

	calls := runner.getCalls()
	if len(calls) != 1 || calls[0] != "test-group" {
		t.Errorf("expected RunAllGroups to be called for test-group, got %v", calls)
	}
}

func TestStart_InvalidPeriod(t *testing.T) {
	runner := newMockGroupRunner()
	cfg := &config.ManagerConfig{
		Manager: config.ManagerSettings{Period: "invalid-duration"},
	}
	logger := slog.Default()

	s := NewScheduler(runner, nil, logger, cfg, nil, nil)

	err := s.Start(context.Background())
	if err == nil {
		t.Fatal("expected error for invalid period, got nil")
	}
}

func TestStart_ContextCancellation(t *testing.T) {
	runner := newMockGroupRunner()
	cfg := &config.ManagerConfig{
		Manager: config.ManagerSettings{Period: "1h"},
	}
	logger := slog.Default()

	state := models.NewBackupState()
	state.AddVolume("test-group", models.VolumeInfo{Name: "vol1", HostPath: "/mnt/vol1"})

	s := NewScheduler(runner, nil, logger, cfg, nil, nil)

	// Override discover and generate to return quickly.
	s.discoverFunc = func(ctx context.Context) (*models.BackupState, error) {
		return state, nil
	}
	s.generateFunc = func(st *models.BackupState) (map[string]config.GroupRunMeta, error) {
		return nil, nil
	}

	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan error, 1)
	go func() {
		done <- s.Start(ctx)
	}()

	cancel()

	err := <-done
	if err != nil {
		t.Fatalf("expected nil error on context cancellation, got: %v", err)
	}

	// Start must NOT run an initial cycle, the orchestrator owns startup;
	// v1 ran the first backup twice because both did.
	calls := runner.getCalls()
	if len(calls) != 0 {
		t.Errorf("expected no runs before the first tick, got %v", calls)
	}
}

func testStore(t *testing.T) *state.ScheduleStore {
	t.Helper()
	return state.LoadSchedule(t.TempDir(), slog.New(slog.NewTextHandler(io.Discard, nil)))
}

func dueTestScheduler(runner *mockGroupRunner, store *state.ScheduleStore, at time.Time) *Scheduler {
	cfg := &config.ManagerConfig{Manager: config.ManagerSettings{Period: "1h"}}
	s := NewScheduler(runner, nil, slog.New(slog.NewTextHandler(io.Discard, nil)), cfg, nil, store)
	s.now = func() time.Time { return at }
	return s
}

func singleGroupState() *models.BackupState {
	bs := models.NewBackupState()
	bs.AddVolume("app", models.VolumeInfo{Name: "vol1", HostPath: "/mnt/vol1"})
	return bs
}

func TestDueGating_RecentSuccessSkipsUntilPeriodElapses(t *testing.T) {
	runner := newMockGroupRunner()
	store := testStore(t)
	t0 := time.Date(2026, 7, 7, 3, 0, 0, 0, time.UTC)

	s := dueTestScheduler(runner, store, t0)
	s.RunAllGroups(context.Background(), singleGroupState(), nil)
	if len(runner.getCalls()) != 1 {
		t.Fatalf("first cycle must run the group, got %v", runner.getCalls())
	}

	// 10 minutes later (restart, event, early tick): not due, no run.
	s.now = func() time.Time { return t0.Add(10 * time.Minute) }
	s.RunAllGroups(context.Background(), singleGroupState(), nil)
	if len(runner.getCalls()) != 1 {
		t.Fatalf("group ran again before its period elapsed: %v", runner.getCalls())
	}

	// Past the period: due again.
	s.now = func() time.Time { return t0.Add(61 * time.Minute) }
	s.RunAllGroups(context.Background(), singleGroupState(), nil)
	if len(runner.getCalls()) != 2 {
		t.Fatalf("group must be due after its period, got %v", runner.getCalls())
	}
}

func TestDueGating_SurvivesRestart(t *testing.T) {
	runner := newMockGroupRunner()
	dir := t.TempDir()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	t0 := time.Date(2026, 7, 7, 3, 0, 0, 0, time.UTC)

	s1 := dueTestScheduler(runner, state.LoadSchedule(dir, logger), t0)
	s1.RunAllGroups(context.Background(), singleGroupState(), nil)

	// New store + new scheduler = daemon restart. Same membership, 10
	// minutes later: the startup cycle must not re-run the group.
	s2 := dueTestScheduler(runner, state.LoadSchedule(dir, logger), t0.Add(10*time.Minute))
	s2.RunAllGroups(context.Background(), singleGroupState(), nil)

	if calls := runner.getCalls(); len(calls) != 1 {
		t.Fatalf("restart must resume the schedule, not re-run backups: %v", calls)
	}
}

func TestDueGating_MembershipChangeRunsImmediately(t *testing.T) {
	runner := newMockGroupRunner()
	store := testStore(t)
	t0 := time.Date(2026, 7, 7, 3, 0, 0, 0, time.UTC)

	s := dueTestScheduler(runner, store, t0)
	s.RunAllGroups(context.Background(), singleGroupState(), nil)

	// A new volume joins the group 5 minutes later (new container started):
	// the group must be due immediately, not after the period.
	grown := singleGroupState()
	grown.AddVolume("app", models.VolumeInfo{Name: "vol2", HostPath: "/mnt/vol2"})
	s.now = func() time.Time { return t0.Add(5 * time.Minute) }
	s.RunAllGroups(context.Background(), grown, nil)

	if calls := runner.getCalls(); len(calls) != 2 {
		t.Fatalf("membership change must trigger an immediate run: %v", calls)
	}
}

func TestDueGating_FailureIsNotMarkedSuccess(t *testing.T) {
	runner := newMockGroupRunner()
	runner.results["app"] = tryRunResult{acquired: true, err: errors.New("borgmatic failed")}
	store := testStore(t)
	t0 := time.Date(2026, 7, 7, 3, 0, 0, 0, time.UTC)

	s := dueTestScheduler(runner, store, t0)
	s.RunAllGroups(context.Background(), singleGroupState(), nil)
	s.now = func() time.Time { return t0.Add(1 * time.Minute) }
	s.RunAllGroups(context.Background(), singleGroupState(), nil)

	if calls := runner.getCalls(); len(calls) != 2 {
		t.Fatalf("a failed group must stay due: %v", calls)
	}
}

func TestNextWake(t *testing.T) {
	runner := newMockGroupRunner()
	store := testStore(t)
	t0 := time.Date(2026, 7, 7, 3, 0, 0, 0, time.UTC)

	s := dueTestScheduler(runner, store, t0)

	// No history: one full period.
	if got := s.NextWake(); got != time.Hour {
		t.Fatalf("empty history must wake after a full period, got %v", got)
	}

	// After a success at t0, at t0+50m the next wake is the 10m remainder.
	s.RunAllGroups(context.Background(), singleGroupState(), nil)
	s.now = func() time.Time { return t0.Add(50 * time.Minute) }
	if got := s.NextWake(); got != 10*time.Minute {
		t.Fatalf("expected 10m remainder wake, got %v", got)
	}

	// Overdue (e.g. long downtime): clamped to the floor, not negative.
	s.now = func() time.Time { return t0.Add(3 * time.Hour) }
	if got := s.NextWake(); got != minWake {
		t.Fatalf("overdue wake must clamp to minWake, got %v", got)
	}
}

func TestDueGating_VanishedGroupIsForgotten(t *testing.T) {
	runner := newMockGroupRunner()
	store := testStore(t)
	t0 := time.Date(2026, 7, 7, 3, 0, 0, 0, time.UTC)

	s := dueTestScheduler(runner, store, t0)
	s.RunAllGroups(context.Background(), singleGroupState(), nil)

	// Group disappears (container unlabeled): its record must not linger
	// and pin NextWake to its stale due time.
	s.now = func() time.Time { return t0.Add(2 * time.Hour) }
	s.RunAllGroups(context.Background(), models.NewBackupState(), nil)
	if got := s.NextWake(); got != time.Hour {
		t.Fatalf("vanished group must not distort next wake, got %v", got)
	}
	if _, ok := store.Record("app"); ok {
		t.Fatal("vanished group record must be pruned")
	}
}
