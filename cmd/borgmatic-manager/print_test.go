package main

import (
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lugoues/borgmatic-manager/internal/models"
	"github.com/lugoues/borgmatic-manager/internal/state"
)

// captureStdout runs fn with os.Stdout redirected and returns what it wrote.
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	require.NoError(t, err)

	orig := os.Stdout
	os.Stdout = w
	fn()
	require.NoError(t, w.Close())
	os.Stdout = orig

	out, err := io.ReadAll(r)
	require.NoError(t, err)
	return string(out)
}

func padFixture(t *testing.T) (*models.BackupState, *state.ScheduleStore) {
	t.Helper()
	bs := models.NewBackupState()
	bs.AddVolume("demo", models.VolumeInfo{Name: "demo_vol", HostPath: "/mnt/demo"})
	return bs, state.LoadSchedule(t.TempDir(), nil)
}

// Both one-shot displays are bracketed by a blank line so they never butt
// against the shell prompt or a preceding log line. The two must agree:
// status looked cramped next to discover because neither padded the bottom.
func TestDisplayBlocksArePaddedTopAndBottom(t *testing.T) {
	bs, store := padFixture(t)

	for name, render := range map[string]func(){
		"discover": func() { printGroups(bs, store) },
		"status":   func() { printStatus(bs, store, time.Hour, 0, nil) },
	} {
		t.Run(name, func(t *testing.T) {
			out := captureStdout(t, render)
			require.NotEmpty(t, out)

			assert.True(t, strings.HasPrefix(out, "\n"), "block must open with a blank line")
			assert.True(t, strings.HasSuffix(out, "\n\n"), "block must close with a blank line")
			assert.False(t, strings.HasSuffix(out, "\n\n\n"), "exactly one trailing blank line")

			// The padding must be blank lines, not lines of spaces.
			lines := strings.Split(strings.TrimSuffix(out, "\n"), "\n")
			assert.Empty(t, lines[0])
			assert.Empty(t, lines[len(lines)-1])
		})
	}
}

// A failed group gets a one-line pointer to `inspect` (which carries the
// reason, log tail, and trend), naming the group so the user knows where to go.
func TestStatusFailurePointsToInspect(t *testing.T) {
	bs := models.NewBackupState()
	bs.AddVolume("demo", models.VolumeInfo{Name: "demo_vol", HostPath: "/mnt/demo"})
	store := state.LoadSchedule(t.TempDir(), nil)
	store.RecordRun("demo", state.RunOutcome{
		Finished:  time.Now(),
		Result:    state.ResultFailed,
		ExitCode:  1,
		LastError: "Repository /mnt/repo does not exist.",
	})

	out := captureStdout(t, func() { printStatus(bs, store, time.Hour, 0, nil) })

	assert.Contains(t, out, "1 group failed")
	assert.Contains(t, out, "demo", "the failing group must be named")
	assert.Contains(t, out, "inspect", "and the pointer must send the user to inspect")
}

func TestStatusShowsRunningGroup(t *testing.T) {
	bs := models.NewBackupState()
	bs.AddVolume("demo", models.VolumeInfo{Name: "demo_vol", HostPath: "/mnt/demo"})
	store := state.LoadSchedule(t.TempDir(), nil)
	// A pending record with no matching finished outcome: a run in flight.
	store.RecordPending("run-1", "demo", time.Now().Add(-3*time.Minute))

	out := captureStdout(t, func() { printStatus(bs, store, time.Hour, 0, nil) })

	assert.Contains(t, out, "running", "an in-flight group shows as running")
	assert.Contains(t, out, "1 group running", "the header reflects the running count")
	assert.NotContains(t, out, "due now", "a running group is not also shown as due")
}

func TestStatusFlagsStaleRunningPastTimeout(t *testing.T) {
	bs := models.NewBackupState()
	bs.AddVolume("demo", models.VolumeInfo{Name: "demo_vol", HostPath: "/mnt/demo"})
	store := state.LoadSchedule(t.TempDir(), nil)
	store.RecordPending("run-1", "demo", time.Now().Add(-2*time.Hour))

	// run_timeout of 30m: a 2h "run" is past it and reads as suspect.
	out := captureStdout(t, func() { printStatus(bs, store, time.Hour, 30*time.Minute, nil) })

	assert.Contains(t, out, "running?", "past run_timeout, a run is flagged as possibly stale")
}

func TestInspectRendersSectionsAndTrend(t *testing.T) {
	bs := models.NewBackupState()
	bs.AddVolume("demo", models.VolumeInfo{Name: "demo_vol", HostPath: "/mnt/demo"})
	group := bs.Groups["demo"]

	store := state.LoadSchedule(t.TempDir(), nil)
	base := time.Now().Add(-3 * time.Hour)
	for i, sz := range []int64{100, 250, 175, 400} {
		store.RecordRun("demo", state.RunOutcome{
			Finished: base.Add(time.Duration(i) * time.Hour), Result: state.ResultOK,
			DurationSeconds: 30, Files: 10, OriginalBytes: sz,
			LogTail: []string{"INFO creating archive", "INFO archive created"},
		})
	}
	rec, ok := store.Record("demo")
	require.True(t, ok)

	out := captureStdout(t, func() {
		printInspect("demo", group, rec, true, "source_directories:\n  - /mnt/demo\n", "", time.Hour)
	})

	for _, want := range []string{"Inspect demo", "Members", "Schedule", "Last run", "Size trend", "Recent runs", "Last run log", "Config"} {
		assert.Contains(t, out, want, "inspect must render the %q section", want)
	}
	assert.Contains(t, out, "source_directories", "the compiled config is shown")
	assert.Contains(t, out, "creating archive", "the last run's log tail is shown")
}

func TestInspectHandlesNoHistory(t *testing.T) {
	bs := models.NewBackupState()
	bs.AddVolume("demo", models.VolumeInfo{Name: "demo_vol", HostPath: "/mnt/demo"})
	group := bs.Groups["demo"]

	// A never-run group: no record. Must render without panicking, and note
	// the config is unavailable.
	out := captureStdout(t, func() {
		printInspect("demo", group, state.GroupRecord{}, false, "", "no config generated for this group", time.Hour)
	})

	assert.Contains(t, out, "Inspect demo")
	assert.Contains(t, out, "never", "an unrun group shows 'never' for last backup")
	assert.Contains(t, out, "no config generated")
	assert.NotContains(t, out, "Size trend", "no trend without at least two sized runs")
}
