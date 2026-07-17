package main

import (
	"context"
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

	"github.com/lugoues/borgmatic-manager/internal/state"
)

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestProcessAlive(t *testing.T) {
	assert.True(t, processAlive(os.Getpid()), "our own process is alive")
	assert.False(t, processAlive(0), "no owner recorded reads as dead, so legacy records stay reapable")

	// A child that has run and been waited for is gone.
	cmd := exec.Command("/bin/true")
	require.NoError(t, cmd.Run())
	assert.False(t, processAlive(cmd.Process.Pid), "a finished, reaped process is not alive")
}

// Startup reconciliation must not reap a pending run whose process is still
// alive: ad-hoc runs record pending entries in the same state file, so a daemon
// restart mid-ad-hoc-run would otherwise force-remove that run's dump helpers,
// failing the backup or archiving a truncated database dump.
func TestReapStalePendingRuns_LeavesLiveOwnerAlone(t *testing.T) {
	store := state.LoadSchedule(t.TempDir(), quietLogger())
	store.RecordPending("live-run", "app", time.Now()) // stamped with our (live) PID

	var reaped []string
	reapStalePendingRuns(context.Background(), store, func(_ context.Context, runID string) ([]string, error) {
		reaped = append(reaped, runID)
		return nil, nil
	})

	assert.Empty(t, reaped, "a live process's helpers must not be reaped")
	assert.Contains(t, store.PendingSnapshot(), "live-run", "and its pending record must survive for that run to clear itself")
}

// A live owner is trusted regardless of how long it has been running: a
// multi-day initial seed backup is a legitimate long run, and reaping it by a
// wall-clock age cap would corrupt the backup. There is no age cap.
func TestReapStalePendingRuns_LeavesLongRunningLiveOwnerAlone(t *testing.T) {
	store := state.LoadSchedule(t.TempDir(), quietLogger())
	// Our own (live) PID, started three days ago, a legitimate long backup.
	store.RecordPending("seed", "app", time.Now().Add(-72*time.Hour))

	var reaped []string
	reapStalePendingRuns(context.Background(), store, func(_ context.Context, runID string) ([]string, error) {
		reaped = append(reaped, runID)
		return nil, nil
	})

	assert.Empty(t, reaped, "a long-running live backup must never be reaped by age")
	assert.Contains(t, store.PendingSnapshot(), "seed")
}

// A PID reused by an unrelated (non-manager) process after a reboot is a
// phantom: identity, not liveness, distinguishes it. PID 1 is always alive but
// is not a borgmatic-manager, so its record is reaped.
func TestReapStalePendingRuns_ReapsReusedNonManagerPID(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "schedule.json"),
		[]byte(`{"version":1,"groups":{},"pending_runs":{"phantom":{"group":"db","started":"2026-01-01T00:00:00Z","pid":1}}}`), 0o600))
	store := state.LoadSchedule(dir, quietLogger())

	var reaped []string
	reapStalePendingRuns(context.Background(), store, func(_ context.Context, runID string) ([]string, error) {
		reaped = append(reaped, runID)
		return nil, nil
	})

	assert.Equal(t, []string{"phantom"}, reaped, "a PID reused by a non-manager process is a phantom and must be reaped")
	assert.NotContains(t, store.PendingSnapshot(), "phantom")
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

// A record whose owner is gone is a real orphan and must still be cleaned up.
func TestReapStalePendingRuns_ReapsDeadOwner(t *testing.T) {
	dir := t.TempDir()
	// A pending record with no owner recorded stands in for one written by a
	// process that has since died (and for legacy records).
	require.NoError(t, os.WriteFile(dir+"/schedule.json",
		[]byte(`{"version":1,"groups":{},"pending_runs":{"orphan":{"group":"db","started":"2026-01-01T00:00:00Z"}}}`), 0o600))
	store := state.LoadSchedule(dir, quietLogger())

	var reaped []string
	reapStalePendingRuns(context.Background(), store, func(_ context.Context, runID string) ([]string, error) {
		reaped = append(reaped, runID)
		return []string{"helper-1"}, nil
	})

	assert.Equal(t, []string{"orphan"}, reaped, "an ownerless record is an orphan and must be reaped")
	assert.NotContains(t, store.PendingSnapshot(), "orphan", "and its pending record is cleared")
}
