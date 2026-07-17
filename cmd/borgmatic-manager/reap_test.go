package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lugoues/borgmatic-manager/internal/lockfile"
	"github.com/lugoues/borgmatic-manager/internal/runner"
	"github.com/lugoues/borgmatic-manager/internal/state"
)

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// nopReap records which run IDs the reaper decided to clean up.
func recordingReap(reaped *[]string, names ...string) func(context.Context, string) ([]string, error) {
	return func(_ context.Context, runID string) ([]string, error) {
		*reaped = append(*reaped, runID)
		return names, nil
	}
}

func TestProcessAlive(t *testing.T) {
	assert.True(t, processAlive(os.Getpid()), "our own process is alive")
	assert.False(t, processAlive(0), "no owner recorded reads as dead, so legacy records stay reapable")

	// A child that has run and been waited for is gone.
	cmd := exec.Command("/bin/true")
	require.NoError(t, cmd.Run())
	assert.False(t, processAlive(cmd.Process.Pid), "a finished, reaped process is not alive")
}

// Startup reconciliation must not reap a pending run whose owner still holds its
// liveness lock: ad-hoc runs record pending entries in the same state file, so a
// daemon restart mid-ad-hoc-run would otherwise force-remove that run's dump
// helpers, failing the backup or archiving a truncated database dump.
func TestReapStalePendingRuns_LeavesLiveOwnerAlone(t *testing.T) {
	lockDir := t.TempDir()
	store := state.LoadSchedule(t.TempDir(), quietLogger())

	// Simulate a live owner: hold the run's liveness lock for the whole test.
	lock, err := lockfile.Exclusive(runner.PendingLockPath(lockDir, "live-run"))
	require.NoError(t, err)
	defer lock.Release()

	store.RecordPending("live-run", "app", time.Now())

	var reaped []string
	reapStalePendingRuns(context.Background(), store, lockDir, recordingReap(&reaped))

	assert.Empty(t, reaped, "a live process's helpers must not be reaped")
	assert.Contains(t, store.PendingSnapshot(), "live-run", "and its pending record must survive for that run to clear itself")
}

// A held lock is trusted regardless of run age: a multi-day initial seed backup
// is a legitimate long run, and reaping it by a wall-clock age cap would corrupt
// the backup. There is no age cap.
func TestReapStalePendingRuns_LeavesLongRunningLiveOwnerAlone(t *testing.T) {
	lockDir := t.TempDir()
	store := state.LoadSchedule(t.TempDir(), quietLogger())

	lock, err := lockfile.Exclusive(runner.PendingLockPath(lockDir, "seed"))
	require.NoError(t, err)
	defer lock.Release()

	store.RecordPending("seed", "app", time.Now().Add(-72*time.Hour))

	var reaped []string
	reapStalePendingRuns(context.Background(), store, lockDir, recordingReap(&reaped))

	assert.Empty(t, reaped, "a long-running live backup must never be reaped by age")
	assert.Contains(t, store.PendingSnapshot(), "seed")
}

// A lock file that exists but is not held means the owner exited without
// clearing its record (e.g. crashed between reap and ClearPending). The reaper
// acquires the lock, reaps, clears the record, and unlinks the file.
func TestReapStalePendingRuns_ReapsPresentUnheldLock(t *testing.T) {
	lockDir := t.TempDir()
	store := state.LoadSchedule(t.TempDir(), quietLogger())

	lockPath := runner.PendingLockPath(lockDir, "abandoned")
	require.NoError(t, os.WriteFile(lockPath, nil, 0o600)) // present, unheld

	store.RecordPending("abandoned", "db", time.Now())

	var reaped []string
	reapStalePendingRuns(context.Background(), store, lockDir, recordingReap(&reaped, "helper-1"))

	assert.Equal(t, []string{"abandoned"}, reaped, "an unheld lock means a dead owner and must be reaped")
	assert.NotContains(t, store.PendingSnapshot(), "abandoned", "and its record is cleared")
	assert.NoFileExists(t, lockPath, "and its lock file is removed")
}

// The upgrade seam: a record written by the previous binary (no lock file yet)
// whose owner is a still-live, separate process, an ad-hoc run that outlived a
// daemon restart. The reaper must leave it alone, keying on the stamped PID, and
// must NOT create a lock file that a later cycle would misread as a dead owner.
func TestReapStalePendingRuns_LeavesLockAbsentButPIDLive(t *testing.T) {
	lockDir := t.TempDir()
	store := state.LoadSchedule(t.TempDir(), quietLogger())
	store.RecordPending("pre-lock", "app", time.Now()) // stamped with our live PID

	var reaped []string
	reapStalePendingRuns(context.Background(), store, lockDir, recordingReap(&reaped))

	assert.Empty(t, reaped, "a live pre-lock run must survive the upgrade")
	assert.Contains(t, store.PendingSnapshot(), "pre-lock")
	assert.NoFileExists(t, runner.PendingLockPath(lockDir, "pre-lock"),
		"the reaper must not create a lock file on the absent-file path")
}

// An absent lock file whose stamped PID is provably gone is a real orphan.
func TestReapStalePendingRuns_ReapsWhenLockAbsentAndPIDDead(t *testing.T) {
	lockDir := t.TempDir()
	dir := t.TempDir()

	// A finished, reaped child is a dead PID.
	cmd := exec.Command("/bin/true")
	require.NoError(t, cmd.Run())
	deadPID := cmd.Process.Pid

	require.NoError(t, os.WriteFile(filepath.Join(dir, "schedule.json"),
		[]byte(fmt.Sprintf(`{"version":1,"groups":{},"pending_runs":{"orphan":{"group":"db","started":"2026-01-01T00:00:00Z","pid":%d}}}`, deadPID)), 0o600))
	store := state.LoadSchedule(dir, quietLogger())

	var reaped []string
	reapStalePendingRuns(context.Background(), store, lockDir, recordingReap(&reaped, "helper-1"))

	assert.Equal(t, []string{"orphan"}, reaped, "a dead PID with no lock file is an orphan and must be reaped")
	assert.NotContains(t, store.PendingSnapshot(), "orphan", "and its record is cleared")
	assert.NoFileExists(t, runner.PendingLockPath(lockDir, "orphan"),
		"the reaper must not have created a lock file to decide this")
}

// A legacy record predating the PID field (pid absent, i.e. 0) has no owner to
// consult and no lock file, so it is treated as an orphan and reaped.
func TestReapStalePendingRuns_ReapsLegacyNoPIDRecord(t *testing.T) {
	lockDir := t.TempDir()
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "schedule.json"),
		[]byte(`{"version":1,"groups":{},"pending_runs":{"legacy":{"group":"db","started":"2026-01-01T00:00:00Z"}}}`), 0o600))
	store := state.LoadSchedule(dir, quietLogger())

	var reaped []string
	reapStalePendingRuns(context.Background(), store, lockDir, recordingReap(&reaped, "helper-1"))

	assert.Equal(t, []string{"legacy"}, reaped, "an ownerless legacy record is an orphan and must be reaped")
	assert.NotContains(t, store.PendingSnapshot(), "legacy")
}

// With no lock dir configured, liveness cannot be proven, so every record is
// left in place rather than risk reaping a live run.
func TestReapStalePendingRuns_NoLockDirLeavesEverything(t *testing.T) {
	store := state.LoadSchedule(t.TempDir(), quietLogger())
	store.RecordPending("any", "app", time.Now())

	var reaped []string
	reapStalePendingRuns(context.Background(), store, "", recordingReap(&reaped))

	assert.Empty(t, reaped)
	assert.Contains(t, store.PendingSnapshot(), "any")
}

func TestSweepDeadPIDDirs(t *testing.T) {
	base := t.TempDir()
	live := filepath.Join(base, strconv.Itoa(os.Getpid()))
	dead := filepath.Join(base, "999999") // not a live PID
	notPID := filepath.Join(base, "keepme")
	for _, d := range []string{live, dead, notPID} {
		require.NoError(t, os.Mkdir(d, 0o700))
	}

	sweepDeadPIDDirs(base)

	assert.DirExists(t, live, "the current process's dir stays")
	assert.NoDirExists(t, dead, "a dead PID's dir is removed")
	assert.DirExists(t, notPID, "a non-PID-named dir is left alone")
}

// The sweep removes only pending-*.lock files that no current record references
// and that no process holds; referenced and non-pending files are left alone.
func TestSweepOrphanedPendingLocks(t *testing.T) {
	lockDir := t.TempDir()
	store := state.LoadSchedule(t.TempDir(), quietLogger())
	store.RecordPending("keep", "app", time.Now())

	referenced := runner.PendingLockPath(lockDir, "keep")
	orphan := runner.PendingLockPath(lockDir, "vanished-owner")
	nonPending := filepath.Join(lockDir, "bm-deadbeef.lock")
	require.NoError(t, os.WriteFile(referenced, nil, 0o600))
	require.NoError(t, os.WriteFile(orphan, nil, 0o600))
	require.NoError(t, os.WriteFile(nonPending, nil, 0o600))

	sweepOrphanedPendingLocks(lockDir, store)

	assert.FileExists(t, referenced, "a lock file a live record references stays")
	assert.NoFileExists(t, orphan, "an unreferenced, unheld pending lock is swept")
	assert.FileExists(t, nonPending, "a non-pending lock file is not this sweep's concern")
}

// A held, unreferenced pending lock is left alone: the sweep must not race a run
// that is mid-startup (record not yet visible to this snapshot).
func TestSweepOrphanedPendingLocks_LeavesHeldLock(t *testing.T) {
	lockDir := t.TempDir()
	store := state.LoadSchedule(t.TempDir(), quietLogger())

	held := runner.PendingLockPath(lockDir, "starting-up")
	lock, err := lockfile.Exclusive(held)
	require.NoError(t, err)
	defer lock.Release()

	sweepOrphanedPendingLocks(lockDir, store)

	assert.FileExists(t, held, "a held lock file must not be swept")
}
