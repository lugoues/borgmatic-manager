package main

import (
	"encoding/json"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lugoues/borgmatic-manager/internal/models"
	"github.com/lugoues/borgmatic-manager/internal/scheduler"
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
		"status":   func() { printStatus(bs, store, time.Hour, 0, nil, nil) },
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

	out := captureStdout(t, func() { printStatus(bs, store, time.Hour, 0, nil, nil) })

	assert.Contains(t, out, "1 group failed")
	assert.Contains(t, out, "demo", "the failing group must be named")
	assert.Contains(t, out, "inspect", "and the pointer must send the user to inspect")
}

// Interrupting a multi-group run must not report the groups it never reached as
// backed up: "ok" is what actually ran, not everything minus the failures.
func TestAdhocSummaryDoesNotCountInterruptedGroupsAsOk(t *testing.T) {
	targets := []string{"a", "b", "c", "d", "e"}

	out := captureStdout(t, func() {
		// Only "a" ran; an interrupt stopped the rest.
		printAdhocSummary(targets, nil, nil, []string{"b", "c", "d", "e"})
	})

	assert.Contains(t, out, "1 ok", "only the group that actually ran counts as ok")
	assert.Contains(t, out, "4 not run")
	assert.Contains(t, out, "interrupted")
	assert.NotContains(t, out, "✓ backed up", "an interrupted run is not a clean success")
}

func TestStatusShowsRunningGroup(t *testing.T) {
	bs := models.NewBackupState()
	bs.AddVolume("demo", models.VolumeInfo{Name: "demo_vol", HostPath: "/mnt/demo"})
	store := state.LoadSchedule(t.TempDir(), nil)
	// A pending record with no matching finished outcome: a run in flight.
	store.RecordPending("run-1", "demo", time.Now().Add(-3*time.Minute))

	out := captureStdout(t, func() { printStatus(bs, store, time.Hour, 0, nil, nil) })

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
	out := captureStdout(t, func() { printStatus(bs, store, time.Hour, 30*time.Minute, nil, nil) })

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
			DurationSeconds: 30, Files: 10, OriginalBytes: sz, DeduplicatedBytes: sz / 10,
			LogTail: []string{"INFO creating archive", "INFO archive created"},
		})
	}
	rec, ok := store.Record("demo")
	require.True(t, ok)

	out := captureStdout(t, func() {
		printInspect("demo", group, rec, true, "source_directories:\n  - /mnt/demo\n", "", time.Hour, 0)
	})

	for _, want := range []string{"Inspect demo", "Members", "Schedule", "Last run", "Size trend", "Recent runs", "Last run log", "Config"} {
		assert.Contains(t, out, want, "inspect must render the %q section", want)
	}
	assert.Contains(t, out, "source_directories", "the compiled config is shown")
	assert.Contains(t, out, "creating archive", "the last run's log tail is shown")

	// Two trend series: total archive size, and the new data each run added.
	assert.Contains(t, out, "total", "the trend shows total archive size")
	assert.Contains(t, out, "new", "and the per-run new-data (churn) series")
	assert.Contains(t, out, "peak", "the churn line summarises its peak")
}

// A run that produced no archive added no new data (delta zero), but the
// dataset is unchanged, the total must hold its line, not drop to zero and
// draw a cliff that never happened.
func TestTrendSeriesCarriesTotalAcrossRunsWithNoArchive(t *testing.T) {
	history := []state.RunOutcome{
		{Result: state.ResultOK, OriginalBytes: 100, DeduplicatedBytes: 10},
		{Result: state.ResultFailed, ExitCode: 1}, // no archive, no stats
		{Result: state.ResultOK, OriginalBytes: 400, DeduplicatedBytes: 40},
	}

	_, totals, deltas := trendSeries(history)

	assert.Equal(t, []int64{100, 100, 400}, totals, "the failed run holds the dataset size, it does not zero it")
	assert.Equal(t, []int64{10, 0, 40}, deltas, "but it contributed no new data")
}

func TestTrendSeriesSkipsRunsBeforeTheFirstArchive(t *testing.T) {
	history := []state.RunOutcome{
		{Result: state.ResultFailed, ExitCode: 1}, // nothing known yet
		{Result: state.ResultOK, OriginalBytes: 100, DeduplicatedBytes: 10},
	}

	_, totals, deltas := trendSeries(history)

	assert.Equal(t, []int64{100}, totals, "with no total yet there is nothing to carry forward")
	assert.Equal(t, []int64{10}, deltas)
}

// With no deduplicated stats recorded, the churn line is omitted rather than
// drawn as a flat row of zeroes (which would read as "no new data").
func TestInspectOmitsChurnLineWithoutDedupStats(t *testing.T) {
	bs := models.NewBackupState()
	bs.AddVolume("demo", models.VolumeInfo{Name: "demo_vol", HostPath: "/mnt/demo"})
	group := bs.Groups["demo"]

	store := state.LoadSchedule(t.TempDir(), nil)
	for i, sz := range []int64{100, 250, 400} {
		store.RecordRun("demo", state.RunOutcome{
			Finished: time.Now().Add(time.Duration(i) * time.Hour), Result: state.ResultOK,
			OriginalBytes: sz, // no DeduplicatedBytes
		})
	}
	rec, ok := store.Record("demo")
	require.True(t, ok)

	out := captureStdout(t, func() { printInspect("demo", group, rec, true, "", "none", time.Hour, 0) })

	assert.Contains(t, out, "Size trend", "the total series still renders")
	assert.NotContains(t, out, "peak", "but the churn line is omitted without dedup stats")
}

// discover and inspect must describe a group's members identically. inspect
// carried its own copy of the rendering and had lost the sqlite and hostname
// cases, labelling both "container=...", which is wrong for a sqlite database
// (it has no container connection) and for hostname mode.
func TestDiscoverAndInspectAgreeOnMemberDetail(t *testing.T) {
	bs := models.NewBackupState()
	bs.AddVolume("demo", models.VolumeInfo{Name: "demo_vol", HostPath: "/mnt/demo"})
	bs.AddDatabases("demo", []models.DatabaseConfig{
		{Type: "sqlite", Name: "app", Path: "/data/app.db"},
		{Type: "postgresql", Name: "remote", Hostname: "db.internal", Port: 5432},
		{Type: "postgresql", Name: "local", Container: "pg"},
	})
	group := bs.Groups["demo"]
	store := state.LoadSchedule(t.TempDir(), nil)

	discover := captureStdout(t, func() { printGroups(bs, store) })
	inspect := captureStdout(t, func() {
		printInspect("demo", group, state.GroupRecord{}, false, "", "none", time.Hour, 0)
	})

	for _, want := range []string{"/data/app.db", "hostname=db.internal port=5432", "container=pg"} {
		assert.Contains(t, discover, want)
		assert.Contains(t, inspect, want, "inspect must describe members exactly as discover does")
	}
}

func TestInspectHandlesNoHistory(t *testing.T) {
	bs := models.NewBackupState()
	bs.AddVolume("demo", models.VolumeInfo{Name: "demo_vol", HostPath: "/mnt/demo"})
	group := bs.Groups["demo"]

	// A never-run group: no record. Must render without panicking, and note
	// the config is unavailable.
	out := captureStdout(t, func() {
		printInspect("demo", group, state.GroupRecord{}, false, "", "no config generated for this group", time.Hour, 0)
	})

	assert.Contains(t, out, "Inspect demo")
	assert.Contains(t, out, "never", "an unrun group shows 'never' for last backup")
	assert.Contains(t, out, "no config generated")
	assert.NotContains(t, out, "Size trend", "no trend without at least two sized runs")
}

func TestBuildStatusDoc(t *testing.T) {
	now := time.Date(2026, 7, 24, 12, 0, 0, 0, time.UTC)

	bs := models.NewBackupState()
	bs.AddVolume("done", models.VolumeInfo{Name: "vol-a", HostPath: "/mnt/a"})
	bs.AddVolume("busy", models.VolumeInfo{Name: "vol-b", HostPath: "/mnt/b"})
	bs.AddVolume("blocked", models.VolumeInfo{Name: "vol-c", HostPath: "/mnt/c"})
	bs.AddVolume("empty-skip", models.VolumeInfo{}) // zero-member groups are filtered upstream of this
	bs.Groups["empty-skip"].Volumes = nil

	store := state.LoadSchedule(t.TempDir(), nil)
	// "done" succeeded 10m ago with stats and a log tail that must not leak.
	store.MarkSuccess("done", scheduler.GroupFingerprint(bs.Groups["done"]), now.Add(-10*time.Minute))
	store.RecordRun("done", state.RunOutcome{
		Finished:        now.Add(-10 * time.Minute),
		Result:          state.ResultOK,
		DurationSeconds: 42,
		Files:           100,
		OriginalBytes:   1 << 20,
		LogTail:         []string{"should not appear"},
	})
	// "busy" is mid-run, started 5m ago, with a 1m run_timeout: stale.
	store.RecordPending("run-1", "busy", now.Add(-5*time.Minute))

	doc := buildStatusDoc(bs, store, time.Hour, time.Minute,
		map[string]time.Duration{"done": 30 * time.Minute},
		map[string]string{"blocked": "shared repo"}, now)

	require.Len(t, doc.Groups, 3, "empty groups are excluded")
	byName := map[string]statusGroupJSON{}
	for _, g := range doc.Groups {
		byName[g.Name] = g
	}

	done := byName["done"]
	assert.EqualValues(t, 1800, done.PeriodSeconds, "file period override wins over global")
	require.NotNil(t, done.LastRun)
	assert.Equal(t, state.ResultOK, done.LastRun.Result)
	assert.Nil(t, done.LastRun.LogTail, "log tail stays out of status output")
	require.NotNil(t, done.Due)
	assert.False(t, *done.Due, "succeeded 10m ago on a 30m period")
	require.NotNil(t, done.NextRun)
	assert.Equal(t, now.Add(20*time.Minute), *done.NextRun)

	busy := byName["busy"]
	require.NotNil(t, busy.Running)
	assert.EqualValues(t, 300, busy.Running.ElapsedSeconds)
	assert.True(t, busy.Running.Stale, "past run_timeout")
	assert.Nil(t, busy.Due, "running groups carry no dueness")

	blocked := byName["blocked"]
	assert.Equal(t, "shared repo", blocked.Refused)
	assert.Nil(t, blocked.Due)

	// The whole document must round-trip as JSON.
	raw, err := json.Marshal(doc)
	require.NoError(t, err)
	assert.Contains(t, string(raw), `"generated_at"`)
	assert.NotContains(t, string(raw), "should not appear")
}
