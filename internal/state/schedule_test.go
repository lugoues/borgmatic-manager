package state_test

import (
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lugoues/borgmatic-manager/internal/state"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestScheduleRoundTrip(t *testing.T) {
	dir := t.TempDir()
	started := time.Date(2026, 7, 7, 3, 0, 0, 0, time.UTC)

	s := state.LoadSchedule(dir, discardLogger())
	s.MarkSuccess("files", "fp-1", started)

	// A fresh load (as after a daemon restart) sees the same record.
	reloaded := state.LoadSchedule(dir, discardLogger())
	rec, ok := reloaded.Record("files")
	require.True(t, ok, "record must survive reload")
	assert.True(t, rec.LastSuccess.Equal(started))
	assert.Equal(t, "fp-1", rec.Fingerprint)
}

func TestScheduleMissingFileIsEmpty(t *testing.T) {
	s := state.LoadSchedule(t.TempDir(), discardLogger())
	_, ok := s.Record("anything")
	assert.False(t, ok)
}

func TestScheduleCorruptFileDegradesToEmpty(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "schedule.json"), []byte("{not json"), 0o600))

	s := state.LoadSchedule(dir, discardLogger())
	_, ok := s.Record("files")
	assert.False(t, ok, "corrupt state must mean everything is due, not a crash")

	// And the store must still be able to persist over the corpse.
	s.MarkSuccess("files", "fp", time.Now())
	reloaded := state.LoadSchedule(dir, discardLogger())
	_, ok = reloaded.Record("files")
	assert.True(t, ok)
}

func TestScheduleRetainGraceThenPrune(t *testing.T) {
	dir := t.TempDir()
	s := state.LoadSchedule(dir, discardLogger())
	s.MarkSuccess("keep", "fp", time.Now())
	s.MarkSuccess("gone", "fp", time.Now())

	// Two absent cycles: the record survives (redeploy blips must not
	// wipe schedules and trigger a backup storm on reappearance).
	s.Retain(map[string]struct{}{"keep": {}})
	rec, ok := s.Record("gone")
	require.True(t, ok, "one absent cycle must not prune")
	assert.Equal(t, 1, rec.MissingCycles)
	s.Retain(map[string]struct{}{"keep": {}})
	_, ok = s.Record("gone")
	require.True(t, ok, "two absent cycles must not prune")

	// Third consecutive absence prunes, and it persists.
	s.Retain(map[string]struct{}{"keep": {}})
	_, ok = s.Record("gone")
	assert.False(t, ok)
	reloaded := state.LoadSchedule(dir, discardLogger())
	_, ok = reloaded.Record("gone")
	assert.False(t, ok, "pruning must persist")
	_, ok = reloaded.Record("keep")
	assert.True(t, ok)
}

func TestScheduleRetainReappearanceResetsGrace(t *testing.T) {
	dir := t.TempDir()
	s := state.LoadSchedule(dir, discardLogger())
	s.MarkSuccess("app", "fp", time.Now())

	s.Retain(map[string]struct{}{})          // absent once
	s.Retain(map[string]struct{}{"app": {}}) // back
	rec, ok := s.Record("app")
	require.True(t, ok)
	assert.Equal(t, 0, rec.MissingCycles, "reappearance must reset the absence counter")
	assert.False(t, rec.LastSuccess.IsZero(), "schedule must be intact after the blip")
}

func TestScheduleStateDirCreatedOnDemand(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "not-yet-created")
	s := state.LoadSchedule(dir, discardLogger())
	s.MarkSuccess("files", "fp", time.Now())

	_, ok := state.LoadSchedule(dir, discardLogger()).Record("files")
	assert.True(t, ok)
}

func TestRecordRunPreservedAcrossMarkSuccess(t *testing.T) {
	dir := t.TempDir()
	s := state.LoadSchedule(dir, discardLogger())

	outcome := state.RunOutcome{
		Finished: time.Date(2026, 7, 7, 3, 5, 0, 0, time.UTC),
		Result:   "ok", Warnings: 2, DurationSeconds: 34, Archive: "files-2026-07-07",
	}
	s.RecordRun("files", outcome)
	s.MarkSuccess("files", "fp-1", time.Date(2026, 7, 7, 3, 0, 0, 0, time.UTC))

	rec, ok := state.LoadSchedule(dir, discardLogger()).Record("files")
	require.True(t, ok)
	require.NotNil(t, rec.LastRun, "MarkSuccess must not clobber the run outcome")
	assert.Equal(t, "files-2026-07-07", rec.LastRun.Archive)
	assert.Equal(t, int64(2), rec.LastRun.Warnings)
	assert.Equal(t, "fp-1", rec.Fingerprint, "and RecordRun must not clobber schedule fields")

	// A later failure overwrites the outcome but not the schedule.
	s.RecordRun("files", state.RunOutcome{Result: "failed", ExitCode: 2})
	rec, _ = state.LoadSchedule(dir, discardLogger()).Record("files")
	assert.Equal(t, "failed", rec.LastRun.Result)
	assert.False(t, rec.LastSuccess.IsZero())
}

func TestRecordRunBuildsBoundedHistory(t *testing.T) {
	dir := t.TempDir()
	s := state.LoadSchedule(dir, discardLogger())

	// Record more runs than the history cap; each carries a log tail.
	for i := range 40 {
		s.RecordRun("files", state.RunOutcome{
			Result:        state.ResultOK,
			OriginalBytes: int64(i),
			LogTail:       []string{fmt.Sprintf("run %d line", i)},
		})
	}

	rec, ok := state.LoadSchedule(dir, discardLogger()).Record("files")
	require.True(t, ok)

	assert.LessOrEqual(t, len(rec.History), 30, "history must be bounded")
	assert.Equal(t, int64(39), rec.History[len(rec.History)-1].OriginalBytes, "newest run is last")
	assert.Equal(t, int64(10), rec.History[0].OriginalBytes, "oldest kept run is run 10 (40 recorded, cap 30)")

	// Only the last run keeps its log tail; history entries are stripped.
	require.NotNil(t, rec.LastRun)
	assert.Equal(t, []string{"run 39 line"}, rec.LastRun.LogTail)
	for _, h := range rec.History {
		assert.Nil(t, h.LogTail, "history entries must not carry log tails")
	}
}

// The daemon and an ad-hoc run each hold their own store over the same file.
// Writes must merge: dumping an in-memory map would let whichever saved last
// erase the other's success marks, history, and pending records.
func TestConcurrentStoresDoNotEraseEachOther(t *testing.T) {
	dir := t.TempDir()
	daemon := state.LoadSchedule(dir, discardLogger())
	adhoc := state.LoadSchedule(dir, discardLogger())

	// Both loaded the same (empty) file, so each has a stale view of the other.
	daemon.MarkSuccess("alpha", "fp-alpha", time.Date(2026, 7, 7, 3, 0, 0, 0, time.UTC))
	adhoc.MarkSuccess("beta", "fp-beta", time.Date(2026, 7, 7, 4, 0, 0, 0, time.UTC))
	daemon.RecordRun("alpha", state.RunOutcome{Result: state.ResultOK, OriginalBytes: 10})

	// A third reader sees the union.
	fresh := state.LoadSchedule(dir, discardLogger())
	alpha, ok := fresh.Record("alpha")
	require.True(t, ok, "the daemon's group survived")
	beta, ok := fresh.Record("beta")
	require.True(t, ok, "the ad-hoc run's group was not erased by the daemon's later write")
	assert.Equal(t, "fp-alpha", alpha.Fingerprint)
	assert.Equal(t, "fp-beta", beta.Fingerprint)
	assert.False(t, beta.LastSuccess.IsZero(), "the ad-hoc run's success mark survived")
	require.NotNil(t, alpha.LastRun)
}

// A pending record carries its owning PID so startup reconciliation can tell a
// dead process's orphan from a live process's in-flight run.
func TestRecordPendingStampsOwningProcess(t *testing.T) {
	dir := t.TempDir()
	s := state.LoadSchedule(dir, discardLogger())

	s.RecordPending("run-1", "files", time.Now())

	p, ok := state.LoadSchedule(dir, discardLogger()).PendingSnapshot()["run-1"]
	require.True(t, ok)
	assert.Equal(t, os.Getpid(), p.PID, "the writer's PID identifies the owner")
}

// A transient read failure during a mutation must NOT overwrite good state with
// an empty read: the old whole-map save() couldn't do this, the read-modify-
// write layer can, and it would fire after every backup.
func TestUpdateAbortsOnReadErrorInsteadOfWiping(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("unreadable-file permissions do not apply to root")
	}
	dir := t.TempDir()
	schedPath := filepath.Join(dir, "schedule.json")

	s := state.LoadSchedule(dir, discardLogger())
	s.MarkSuccess("keeper", "fp", time.Date(2026, 7, 7, 3, 0, 0, 0, time.UTC))

	// Make the existing state unreadable, then mutate: the update must abort.
	require.NoError(t, os.Chmod(schedPath, 0))
	t.Cleanup(func() { _ = os.Chmod(schedPath, 0o600) })
	s.MarkSuccess("newcomer", "fp2", time.Now())

	// Restore readability and read fresh from disk.
	require.NoError(t, os.Chmod(schedPath, 0o600))
	reloaded := state.LoadSchedule(dir, discardLogger())
	_, hasKeeper := reloaded.Record("keeper")
	_, hasNewcomer := reloaded.Record("newcomer")
	assert.True(t, hasKeeper, "the existing group must survive a read error during an update")
	assert.False(t, hasNewcomer, "the update that could not read must not have persisted (it would have wiped keeper)")
}

func TestPendingRunsRoundTrip(t *testing.T) {
	dir := t.TempDir()
	s := state.LoadSchedule(dir, discardLogger())
	started := time.Date(2026, 7, 8, 9, 0, 0, 0, time.UTC)

	s.RecordPending("run-abc", "db", started)

	// A fresh load (daemon crashed and restarted) still sees it.
	reloaded := state.LoadSchedule(dir, discardLogger())
	pending := reloaded.PendingSnapshot()
	require.Len(t, pending, 1)
	assert.Equal(t, "db", pending["run-abc"].Group)
	assert.True(t, pending["run-abc"].Started.Equal(started))

	reloaded.ClearPending("run-abc")
	assert.Empty(t, reloaded.PendingSnapshot())
	assert.Empty(t, state.LoadSchedule(dir, discardLogger()).PendingSnapshot(), "clear must persist")
}
