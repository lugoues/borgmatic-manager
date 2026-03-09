package scheduler

import (
	"context"
	"errors"
	"log/slog"
	"sort"
	"sync"
	"testing"

	"github.com/lugoues/borgmatic-manager/internal/config"
	"github.com/lugoues/borgmatic-manager/internal/models"
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

func (m *mockGroupRunner) TryRunGroup(ctx context.Context, groupName string, group *models.VolumeGroup, cfg *config.ManagerConfig) (bool, error) {
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

// mockDiscoverFunc and mockGenerateFunc for RunCycle tests.
type mockDeps struct {
	mu             sync.Mutex
	discoverCalled bool
	generateCalled bool
	runAllCalled   bool
	discoverState  *models.BackupState
	discoverErr    error
	generateErr    error
}

func TestRunAllGroups_Parallel(t *testing.T) {
	runner := newMockGroupRunner()
	cfg := &config.ManagerConfig{
		Manager: config.ManagerSettings{Period: "1h"},
	}
	logger := slog.Default()

	s := NewScheduler(runner, nil, logger, cfg, nil, "/tmp/test")

	state := models.NewBackupState()
	state.AddVolume("group-a", models.VolumeInfo{Name: "vol1", MountPath: "/mnt/vol1"})
	state.AddVolume("group-b", models.VolumeInfo{Name: "vol2", MountPath: "/mnt/vol2"})
	state.AddVolume("group-c", models.VolumeInfo{Name: "vol3", MountPath: "/mnt/vol3"})

	s.RunAllGroups(context.Background(), state)

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

	s := NewScheduler(runner, nil, logger, cfg, nil, "/tmp/test")

	state := models.NewBackupState()
	state.AddVolume("has-volumes", models.VolumeInfo{Name: "vol1", MountPath: "/mnt/vol1"})
	// "empty-group" has no volumes (only databases or nothing)
	state.Groups["empty-group"] = &models.VolumeGroup{}

	s.RunAllGroups(context.Background(), state)

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

	s := NewScheduler(runner, nil, logger, cfg, nil, "/tmp/test")

	state := models.NewBackupState()
	state.AddVolume("busy-group", models.VolumeInfo{Name: "vol1", MountPath: "/mnt/vol1"})
	state.AddVolume("free-group", models.VolumeInfo{Name: "vol2", MountPath: "/mnt/vol2"})

	s.RunAllGroups(context.Background(), state)

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

	s := NewScheduler(runner, nil, logger, cfg, nil, "/tmp/test")

	state := models.NewBackupState()
	state.AddVolume("error-group", models.VolumeInfo{Name: "vol1", MountPath: "/mnt/vol1"})
	state.AddVolume("ok-group", models.VolumeInfo{Name: "vol2", MountPath: "/mnt/vol2"})

	// Should not panic or abort; both groups should be attempted.
	s.RunAllGroups(context.Background(), state)

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
	state.AddVolume("test-group", models.VolumeInfo{Name: "vol1", MountPath: "/mnt/vol1"})

	discoverCalled := false
	generateCalled := false

	s := NewScheduler(runner, nil, logger, cfg, nil, "/tmp/test")

	// Override discover and generate funcs for testing.
	s.discoverFunc = func(ctx context.Context) (*models.BackupState, error) {
		discoverCalled = true
		return state, nil
	}
	s.generateFunc = func(st *models.BackupState) error {
		generateCalled = true
		return nil
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

	s := NewScheduler(runner, nil, logger, cfg, nil, "/tmp/test")

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
	state.AddVolume("test-group", models.VolumeInfo{Name: "vol1", MountPath: "/mnt/vol1"})

	s := NewScheduler(runner, nil, logger, cfg, nil, "/tmp/test")

	// Override discover and generate to return quickly.
	s.discoverFunc = func(ctx context.Context) (*models.BackupState, error) {
		return state, nil
	}
	s.generateFunc = func(st *models.BackupState) error {
		return nil
	}

	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan error, 1)
	go func() {
		done <- s.Start(ctx)
	}()

	// Cancel immediately after initial run.
	cancel()

	err := <-done
	if err != nil {
		t.Fatalf("expected nil error on context cancellation, got: %v", err)
	}

	// Initial run should have called the runner.
	calls := runner.getCalls()
	if len(calls) != 1 || calls[0] != "test-group" {
		t.Errorf("expected initial run to call test-group, got %v", calls)
	}
}
