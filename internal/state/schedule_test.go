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

func TestScheduleRetainPrunesVanishedGroups(t *testing.T) {
	dir := t.TempDir()
	s := state.LoadSchedule(dir, discardLogger())
	s.MarkSuccess("keep", "fp", time.Now())
	s.MarkSuccess("gone", "fp", time.Now())

	s.Retain(map[string]struct{}{"keep": {}})

	_, ok := s.Record("gone")
	assert.False(t, ok)
	reloaded := state.LoadSchedule(dir, discardLogger())
	_, ok = reloaded.Record("gone")
	assert.False(t, ok, "pruning must persist")
	_, ok = reloaded.Record("keep")
	assert.True(t, ok)
}

func TestScheduleStateDirCreatedOnDemand(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "not-yet-created")
	s := state.LoadSchedule(dir, discardLogger())
	s.MarkSuccess("files", "fp", time.Now())

	_, ok := state.LoadSchedule(dir, discardLogger()).Record("files")
	assert.True(t, ok)
}
