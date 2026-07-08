package state_test

import (
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
