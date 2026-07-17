package main

import (
	"context"
	"io"
	"log/slog"
	"os"
	"os/exec"
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
