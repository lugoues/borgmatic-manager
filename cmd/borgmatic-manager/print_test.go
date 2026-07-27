package main

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lugoues/borgmatic-manager/internal/lockfile"
	"github.com/lugoues/borgmatic-manager/internal/models"
	"github.com/lugoues/borgmatic-manager/internal/runner"
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
		"discover": func() { printGroups(bs, store, nil) },
		"status":   func() { printStatus(bs, store, "", time.Hour, 0, nil, nil, nil) },
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

	out := captureStdout(t, func() { printStatus(bs, store, "", time.Hour, 0, nil, nil, nil) })

	assert.Contains(t, out, "1 group failed")
	assert.Contains(t, out, "demo", "the failing group must be named")
	assert.Contains(t, out, "inspect", "and the pointer must send the user to inspect")
}

func TestStatusShowsPartialFanOut(t *testing.T) {
	bs := models.NewBackupState()
	bs.AddVolume("demo", models.VolumeInfo{Name: "demo_vol", HostPath: "/mnt/demo"})
	store := state.LoadSchedule(t.TempDir(), nil)
	// Group failed, but one of two destinations still backed up.
	store.RecordRun("demo", state.RunOutcome{
		Finished:  time.Now(),
		Result:    state.ResultFailed,
		ExitCode:  1,
		LastError: "Repository /mnt/offsite does not exist.",
		Repositories: []state.RepoOutcome{
			{ID: "local", Result: state.ResultOK},
			{ID: "offsite", Result: state.ResultFailed},
		},
	})

	out := captureStdout(t, func() { printStatus(bs, store, "", time.Hour, 0, nil, nil, nil) })

	assert.Contains(t, out, "partial (1/2 ok)", "a fan-out with one live destination reads as partial")
	assert.Contains(t, out, "1 group partial", "and is called out separately from a flat failure")
	assert.NotContains(t, out, "1 group failed", "a partial group is not counted as a full failure")
}

func TestInspectShowsPerRepositoryBreakdown(t *testing.T) {
	bs := models.NewBackupState()
	bs.AddVolume("demo", models.VolumeInfo{Name: "demo_vol", HostPath: "/mnt/demo"})
	group := bs.Groups["demo"]
	store := state.LoadSchedule(t.TempDir(), nil)
	store.RecordRun("demo", state.RunOutcome{
		Finished: time.Now(),
		Result:   state.ResultFailed,
		ExitCode: 1,
		Repositories: []state.RepoOutcome{
			{ID: "local", Result: state.ResultOK, Files: 1234, OriginalBytes: 5 << 30},
			{ID: "offsite", Result: state.ResultFailed},
		},
	})
	rec, _ := store.Record("demo")

	out := captureStdout(t, func() {
		printInspect("demo", group, rec, true, "", "none", nil, time.Hour, 0, nil)
	})

	assert.Contains(t, out, "Repositories", "the per-destination section renders for a fan-out")
	assert.Contains(t, out, "local")
	assert.Contains(t, out, "offsite")
	assert.Contains(t, out, "1234 files", "a healthy destination shows its size")
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

	out := captureStdout(t, func() { printStatus(bs, store, "", time.Hour, 0, nil, nil, nil) })

	assert.Contains(t, out, "running", "an in-flight group shows as running")
	assert.Contains(t, out, "1 group running", "the header reflects the running count")
	assert.NotContains(t, out, "due now", "a running group is not also shown as due")
}

// deadPID returns a PID that cannot name a live process: one past the kernel's
// pid_max, for which kill(2) is guaranteed ESRCH. Reusing a just-exited child's
// PID would race the kernel's recycling.
func deadPID(t *testing.T) int {
	t.Helper()
	raw, err := os.ReadFile("/proc/sys/kernel/pid_max")
	require.NoError(t, err)
	max, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	require.NoError(t, err)
	pid := max + 1
	require.False(t, processAlive(pid), "pid %d must not be live", pid)
	return pid
}

// recordPendingWithPID writes a pending record stamped with pid, bypassing
// RecordPending, which always stamps the live test process.
func recordPendingWithPID(t *testing.T, stateDir, runID, group string, started time.Time, pid int) {
	t.Helper()
	rec := map[string]any{"group": group, "started": started}
	if pid != 0 {
		rec["pid"] = pid
	}
	doc := map[string]any{
		"version":      1,
		"groups":       map[string]any{},
		"pending_runs": map[string]any{runID: rec},
	}
	data, err := json.Marshal(doc)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(stateDir, "schedule.json"), data, 0o600))
}

// recordDeadPending writes a pending record whose owner is gone, the state a
// SIGKILLed run leaves behind: the deferred ClearPending never runs.
func recordDeadPending(t *testing.T, stateDir, runID, group string, started time.Time) {
	t.Helper()
	recordPendingWithPID(t, stateDir, runID, group, started, deadPID(t))
}

// A run killed outright (SIGKILL, OOM, power loss) never clears its pending
// record, and only the daemon reaps those at startup. Until this filter, such a
// record pinned its group at "running" forever and hid the real due state.
func TestStatusIgnoresPendingRunWhoseProcessIsGone(t *testing.T) {
	bs := models.NewBackupState()
	bs.AddVolume("demo", models.VolumeInfo{Name: "demo_vol", HostPath: "/mnt/demo"})
	dir := t.TempDir()
	recordDeadPending(t, dir, "run-1", "demo", time.Now().Add(-3*time.Minute))
	store := state.LoadSchedule(dir, nil)

	out := captureStdout(t, func() { printStatus(bs, store, "", time.Hour, 0, nil, nil, nil) })

	assert.NotContains(t, out, "running", "a dead owner's record is not an in-flight run")
	assert.Contains(t, out, "due now", "and the group's real due state is visible again")

	doc := buildStatusDoc(bs, store, "", time.Hour, 0, nil, nil, nil, nil, time.Now())
	require.Len(t, doc.Groups, 1)
	assert.Nil(t, doc.Groups[0].Running, "status --json agrees with the table")
	require.NotNil(t, doc.Groups[0].Due)
	assert.True(t, *doc.Groups[0].Due)
}

// A record with no stamped PID (written by a pre-PID binary) cannot be proven
// dead, so it stays visible: this is a display filter, not a reaper.
func TestStatusKeepsPendingRunWithNoStampedPID(t *testing.T) {
	bs := models.NewBackupState()
	bs.AddVolume("demo", models.VolumeInfo{Name: "demo_vol", HostPath: "/mnt/demo"})
	dir := t.TempDir()
	doc := `{"version":1,"groups":{},"pending_runs":{"run-1":{"group":"demo","started":"` +
		time.Now().Add(-3*time.Minute).Format(time.RFC3339) + `"}}}`
	require.NoError(t, os.WriteFile(filepath.Join(dir, "schedule.json"), []byte(doc), 0o600))

	out := captureStdout(t, func() { printStatus(bs, state.LoadSchedule(dir, nil), "", time.Hour, 0, nil, nil, nil) })

	assert.Contains(t, out, "running", "unprovable liveness keeps the record visible")
}

// The PID alone cannot survive PID reuse: after a reboot the kernel hands low
// PIDs straight back out, so a crashed run's stamped PID can name an unrelated
// live process and pin the group at "running" again. The per-run advisory lock
// is the authority, exactly as reapStalePendingRuns treats it: the kernel drops
// it when the owner dies, so a lock we can take proves the owner is gone no
// matter who holds its PID now.
func TestStatusUsesLivenessLockNotPIDWhenPIDIsRecycled(t *testing.T) {
	bs := models.NewBackupState()
	bs.AddVolume("demo", models.VolumeInfo{Name: "demo_vol", HostPath: "/mnt/demo"})
	dir := t.TempDir()
	lockDir := filepath.Join(dir, "locks")
	require.NoError(t, os.MkdirAll(lockDir, 0o700))

	// The crashed run's PID now names this very test process: alive, but not the
	// owner. An unheld lock file is what the kernel leaves after the owner dies.
	recordPendingWithPID(t, dir, "run-1", "demo", time.Now().Add(-3*time.Minute), os.Getpid())
	require.NoError(t, os.WriteFile(runner.PendingLockPath(lockDir, "run-1"), nil, 0o600))
	store := state.LoadSchedule(dir, nil)

	out := captureStdout(t, func() { printStatus(bs, store, lockDir, time.Hour, 0, nil, nil, nil) })
	assert.NotContains(t, out, "running", "an unheld liveness lock outranks a recycled PID")
	assert.Contains(t, out, "due now")

	doc := buildStatusDoc(bs, store, lockDir, time.Hour, 0, nil, nil, nil, nil, time.Now())
	require.Len(t, doc.Groups, 1)
	assert.Nil(t, doc.Groups[0].Running, "status --json agrees with the table")
}

// The mirror image: a held lock keeps the run visible even though the record
// carries no usable PID. Over-hiding would claim a live backup is not running.
func TestStatusKeepsPendingRunWhoseLivenessLockIsHeld(t *testing.T) {
	bs := models.NewBackupState()
	bs.AddVolume("demo", models.VolumeInfo{Name: "demo_vol", HostPath: "/mnt/demo"})
	dir := t.TempDir()
	lockDir := filepath.Join(dir, "locks")
	require.NoError(t, os.MkdirAll(lockDir, 0o700))

	recordPendingWithPID(t, dir, "run-1", "demo", time.Now().Add(-3*time.Minute), deadPID(t))
	lock, acquired, err := lockfile.TryExclusive(runner.PendingLockPath(lockDir, "run-1"))
	require.NoError(t, err)
	require.True(t, acquired)
	defer lock.Release()

	out := captureStdout(t, func() { printStatus(bs, state.LoadSchedule(dir, nil), lockDir, time.Hour, 0, nil, nil, nil) })

	assert.Contains(t, out, "running", "a held lock proves the owner is alive, whatever the PID says")
}

// A record whose lock file the owner never got to create falls back to the PID,
// matching reapStalePendingRuns' handling of the same no-lock-file case.
func TestStatusFallsBackToPIDWhenNoLivenessLockFile(t *testing.T) {
	bs := models.NewBackupState()
	bs.AddVolume("demo", models.VolumeInfo{Name: "demo_vol", HostPath: "/mnt/demo"})
	dir := t.TempDir()
	lockDir := filepath.Join(dir, "locks")
	require.NoError(t, os.MkdirAll(lockDir, 0o700))
	recordDeadPending(t, dir, "run-1", "demo", time.Now().Add(-3*time.Minute))

	out := captureStdout(t, func() { printStatus(bs, state.LoadSchedule(dir, nil), lockDir, time.Hour, 0, nil, nil, nil) })

	assert.NotContains(t, out, "running", "no lock file and a dead PID: the owner is gone")
	// Probing must not mint the lock file: the daemon's sweep reads a
	// present-unheld lock as a crashed run and would reap against it.
	_, statErr := os.Stat(runner.PendingLockPath(lockDir, "run-1"))
	assert.True(t, os.IsNotExist(statErr), "a status read never creates a liveness lock")
}

func TestStatusFlagsStaleRunningPastTimeout(t *testing.T) {
	bs := models.NewBackupState()
	bs.AddVolume("demo", models.VolumeInfo{Name: "demo_vol", HostPath: "/mnt/demo"})
	store := state.LoadSchedule(t.TempDir(), nil)
	store.RecordPending("run-1", "demo", time.Now().Add(-2*time.Hour))

	// run_timeout of 30m: a 2h "run" is past it and reads as suspect.
	out := captureStdout(t, func() { printStatus(bs, store, "", time.Hour, 30*time.Minute, nil, nil, nil) })

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
		printInspect("demo", group, rec, true, "source_directories:\n  - /mnt/demo\n", "", nil, time.Hour, 0, nil)
	})

	for _, want := range []string{"Inspect demo", "Members", "Schedule", "Last run", "Size trend", "Recent runs", "Last run log", "Config"} {
		assert.Contains(t, out, want, "inspect must render the %q section", want)
	}
	assert.Contains(t, out, "source_directories", "the compiled config is shown")
	assert.Contains(t, out, "creating archive", "the last run's log tail is shown")

	// Two trend series: total archive size, and the new data each run added.
	assert.Contains(t, out, "total", "the trend shows total archive size")
	assert.Contains(t, out, "delta", "and the per-run new-data (churn) series")
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

	out := captureStdout(t, func() { printInspect("demo", group, rec, true, "", "none", nil, time.Hour, 0, nil) })

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

	discover := captureStdout(t, func() { printGroups(bs, store, nil) })
	inspect := captureStdout(t, func() {
		printInspect("demo", group, state.GroupRecord{}, false, "", "none", nil, time.Hour, 0, nil)
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
		printInspect("demo", group, state.GroupRecord{}, false, "", "no config generated for this group", nil, time.Hour, 0, nil)
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

	doc := buildStatusDoc(bs, store, "", time.Hour, time.Minute,
		map[string]time.Duration{"done": 30 * time.Minute},
		map[string]string{"blocked": "shared repo"}, nil, nil, now)

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

func TestDiscoverAndStatusShowOfflineMembers(t *testing.T) {
	// A partly-stopped multi-container group: books offline (still backed up),
	// postgres live. The group is NOT fully offline.
	bs := models.NewBackupState()
	bs.AddVolume("app", models.VolumeInfo{Name: "books", HostPath: "/data/books"})
	bs.AddVolume("app", models.VolumeInfo{Name: "postgres", HostPath: "/data/pg"})
	store := state.LoadSchedule(t.TempDir(), nil)
	store.MarkSuccess("app", "fp", time.Now().Add(-2*time.Minute))
	off := &state.Offline{Volumes: map[string]map[string]bool{"app": {"books": true}}}

	discover := captureStdout(t, func() { printGroups(bs, store, off) })
	assert.Contains(t, discover, "books", "the offline volume is still listed")
	assert.Regexp(t, `\(offline\)[^\n]*books`, discover, "tagged offline in front, before the name")
	assert.NotRegexp(t, `\(offline\)[^\n]*postgres`, discover, "the live volume is not tagged")
	assert.NotRegexp(t, `app\b[^\n]*\(offline\)`, discover, "a partly-live group is not fully offline")

	status := captureStdout(t, func() { printStatus(bs, store, "", time.Hour, 0, nil, nil, off) })
	assert.Regexp(t, `app \(partial\)[^\n]*due now`, status, "a partly-offline group is tagged partial and keeps its schedule")
}

func TestBuildStatusDocMarksOfflineMembersButKeepsSchedule(t *testing.T) {
	bs := models.NewBackupState()
	bs.AddVolume("gone", models.VolumeInfo{Name: "gone_vol", HostPath: "/data/gone"})
	store := state.LoadSchedule(t.TempDir(), nil)
	off := &state.Offline{Volumes: map[string]map[string]bool{"gone": {"gone_vol": true}}}

	doc := buildStatusDoc(bs, store, "", time.Hour, 0, nil, nil, nil, off, time.Now())
	require.Len(t, doc.Groups, 1)
	g := doc.Groups[0]
	assert.True(t, g.Offline, "no live container: the group is offline")
	assert.Equal(t, []string{"gone_vol"}, g.OfflineVolumes)
	assert.NotNil(t, g.Due, "but it is still scheduled and backed up")
	assert.True(t, *g.Due, "and due now with no prior success")
}

// create succeeded everywhere and a later action failed, so the probes confirm
// every destination. Reporting "partial (2/2 ok)" then contradicts itself and
// buries the failure that actually happened: partial has to mean at least one
// destination is known bad.
func TestStatusDoesNotCallAnAllSuccessRunPartial(t *testing.T) {
	bs := models.NewBackupState()
	bs.AddVolume("demo", models.VolumeInfo{Name: "demo_vol", HostPath: "/mnt/demo"})
	store := state.LoadSchedule(t.TempDir(), nil)
	store.RecordRun("demo", state.RunOutcome{
		Finished:  time.Now(),
		Result:    state.ResultFailed,
		ExitCode:  2,
		LastError: "Command error: borg prune",
		Repositories: []state.RepoOutcome{
			{ID: "local", Result: state.ResultOK},
			{ID: "offsite", Result: state.ResultOK},
		},
	})

	out := captureStdout(t, func() { printStatus(bs, store, "", time.Hour, 0, nil, nil, nil) })

	assert.NotContains(t, out, "partial (2/2 ok)", "N-of-N ok is not a partial fan-out")
	assert.NotContains(t, out, "group partial")
	assert.Contains(t, out, "1 group failed", "the group-level action failure must still surface")
	assert.Contains(t, out, "failed (exit 2)", "with the exit code, and a pointer to the error")
}

// LastStats is retained precisely so a later failure does not blank out the size
// of the backup that did complete; the inspect table described the column as the
// repository's last size but read it from the failed LastRun.
func TestInspectShowsTheLastMeasuredSizeAfterAFailure(t *testing.T) {
	bs := models.NewBackupState()
	bs.AddVolume("demo", models.VolumeInfo{Name: "demo_vol", HostPath: "/mnt/demo"})
	group := bs.Groups["demo"]
	store := state.LoadSchedule(t.TempDir(), nil)
	store.RecordRun("demo", state.RunOutcome{
		Finished: time.Now().Add(-time.Hour), Result: state.ResultOK,
		Repositories: []state.RepoOutcome{
			{ID: "local", Result: state.ResultOK, Files: 1234, OriginalBytes: 5 << 30},
			{ID: "offsite", Result: state.ResultOK, Files: 99, OriginalBytes: 1 << 30},
		},
	})
	// A later run fails for one destination; its measured size still stands.
	store.RecordRun("demo", state.RunOutcome{
		Finished: time.Now(), Result: state.ResultFailed, ExitCode: 1,
		Repositories: []state.RepoOutcome{
			{ID: "local", Result: state.ResultFailed},
			{ID: "offsite", Result: state.ResultOK, Files: 99, OriginalBytes: 1 << 30},
		},
	})
	rec, _ := store.Record("demo")

	out := captureStdout(t, func() {
		printInspect("demo", group, rec, true, "", "none", nil, time.Hour, 0, nil)
	})

	assert.Contains(t, out, "1234 files",
		"the last measured size survives a later failure in the table too")
	assert.Contains(t, out, state.ResultFailed,
		"while the result column still reports the failure")
}

// perRepoFailure omits a destination it could neither implicate nor confirm (a
// probe that timed out proves nothing), while runRepoHealth counts every
// configured destination in the total. Reading "not ok" as "failed" then reports
// "some destinations failed" about one that nothing is known about.
func TestStatusDoesNotCallAnIndeterminateDestinationAFailure(t *testing.T) {
	bs := models.NewBackupState()
	bs.AddVolume("demo", models.VolumeInfo{Name: "demo_vol", HostPath: "/mnt/demo"})
	store := state.LoadSchedule(t.TempDir(), nil)
	store.RecordRun("demo", state.RunOutcome{
		Finished:               time.Now(),
		Result:                 state.ResultFailed,
		ExitCode:               1,
		LastError:              "Command error: borg check",
		ConfiguredRepositories: []string{"local", "offsite"},
		// offsite is absent: its probe timed out, so nothing is known about it.
		Repositories: []state.RepoOutcome{{ID: "local", Result: state.ResultOK}},
	})

	out := captureStdout(t, func() { printStatus(bs, store, "", time.Hour, 0, nil, nil, nil) })

	assert.NotContains(t, out, "partial", "an unjudged destination is not a failed one")
	assert.Contains(t, out, "1 group failed", "the run still failed and must say so")
}

// The control: a destination explicitly reported failed still reads as partial.
func TestStatusStillReportsAConfirmedPartialFanOut(t *testing.T) {
	bs := models.NewBackupState()
	bs.AddVolume("demo", models.VolumeInfo{Name: "demo_vol", HostPath: "/mnt/demo"})
	store := state.LoadSchedule(t.TempDir(), nil)
	store.RecordRun("demo", state.RunOutcome{
		Finished:               time.Now(),
		Result:                 state.ResultFailed,
		ExitCode:               1,
		ConfiguredRepositories: []string{"local", "offsite", "archive"},
		Repositories: []state.RepoOutcome{
			{ID: "local", Result: state.ResultOK},
			{ID: "offsite", Result: state.ResultFailed},
			// archive is indeterminate, and must not change the verdict.
		},
	})

	out := captureStdout(t, func() { printStatus(bs, store, "", time.Hour, 0, nil, nil, nil) })

	assert.Contains(t, out, "partial (1/3 ok)", "one confirmed ok and one confirmed failed is partial")
	assert.Contains(t, out, "1 group partial")
}

// A successful create of an empty source measures 0 files and 0 bytes. That is a
// backup that happened, and rendering it as "-" claims there is no measurement
// when there is one that says zero. An empty backup and an unknown one are
// different problems, and only one of them is the operator's to chase.
func TestInspectRendersAMeasuredEmptyArchive(t *testing.T) {
	bs := models.NewBackupState()
	bs.AddVolume("demo", models.VolumeInfo{Name: "demo_vol", HostPath: "/mnt/demo"})
	group := bs.Groups["demo"]
	store := state.LoadSchedule(t.TempDir(), nil)
	store.RecordRun("demo", state.RunOutcome{
		Finished: time.Now(), Result: state.ResultOK, CreateAttempted: true,
		Repositories: []state.RepoOutcome{
			{ID: "local", Result: state.ResultOK, Measured: true}, // empty archive
			{ID: "offsite", Result: state.ResultOK, Files: 5, OriginalBytes: 100, Measured: true},
		},
	})
	rec, _ := store.Record("demo")

	out := captureStdout(t, func() {
		printInspect("demo", group, rec, true, "", "none", nil, time.Hour, 0, nil)
	})

	assert.Contains(t, out, "0 files", "an archive measured as empty reports zero, not nothing")
	assert.Contains(t, out, "5 files")
}

// Repository settings do not enter the scheduler fingerprint, so removing a
// repository from a recently successful group leaves it not due, no run
// reconciles the record, and status would report the deleted destination for a
// whole period.
func TestStatusJSONReportsTheCurrentRepositoriesNotThePersistedOnes(t *testing.T) {
	bs := models.NewBackupState()
	bs.AddVolume("demo", models.VolumeInfo{Name: "demo_vol", HostPath: "/mnt/demo"})
	store := state.LoadSchedule(t.TempDir(), nil)
	store.RecordRun("demo", state.RunOutcome{
		Finished: time.Now(), Result: state.ResultOK, CreateAttempted: true,
		RepositoriesKnown:      true,
		ConfiguredRepositories: []string{"local", "offsite"},
		ConfiguredRepositoryPaths: map[string]string{
			"local": "/mnt/local", "offsite": "/mnt/offsite",
		},
		Repositories: []state.RepoOutcome{
			{ID: "local", Path: "/mnt/local", Result: state.ResultOK, Measured: true},
			{ID: "offsite", Path: "/mnt/offsite", Result: state.ResultOK, Measured: true},
		},
	})

	// offsite has since been removed from the configuration, and the group is
	// not due, so nothing has reconciled the record.
	configured := map[string]map[string]string{"demo": {"local": "/mnt/local"}}
	doc := buildStatusDoc(bs, store, "", time.Hour, 0, nil, nil, configured, nil, time.Now())

	require.Len(t, doc.Groups, 1)
	assert.Contains(t, doc.Groups[0].Repositories, "local")
	assert.NotContains(t, doc.Groups[0].Repositories, "offsite",
		"a destination the group no longer configures is history, not inventory")

	t.Run("a repointed label is dropped too", func(t *testing.T) {
		configured := map[string]map[string]string{"demo": {"local": "/mnt/local", "offsite": "/mnt/elsewhere"}}
		doc := buildStatusDoc(bs, store, "", time.Hour, 0, nil, nil, configured, nil, time.Now())
		assert.NotContains(t, doc.Groups[0].Repositories, "offsite",
			"the id survived the repoint; this history belongs to the old destination")
	})

	t.Run("a group nobody reported on is passed through", func(t *testing.T) {
		doc := buildStatusDoc(bs, store, "", time.Hour, 0, nil, nil, nil, nil, time.Now())
		assert.Len(t, doc.Groups[0].Repositories, 2,
			"silence is not a report that the group configures nothing")
	})
}

// inspect reads the same record status does, so it needs the same filtering:
// repository settings do not enter the scheduler fingerprint, and a group that
// is not due keeps a removed destination in its record until it next runs.
func TestInspectShowsOnlyTheCurrentRepositories(t *testing.T) {
	bs := models.NewBackupState()
	bs.AddVolume("demo", models.VolumeInfo{Name: "demo_vol", HostPath: "/mnt/demo"})
	group := bs.Groups["demo"]
	store := state.LoadSchedule(t.TempDir(), nil)
	store.RecordRun("demo", state.RunOutcome{
		Finished: time.Now(), Result: state.ResultOK, CreateAttempted: true,
		RepositoriesKnown:      true,
		ConfiguredRepositories: []string{"local", "offsite", "archive"},
		ConfiguredRepositoryPaths: map[string]string{
			"local": "/mnt/local", "offsite": "/mnt/offsite", "archive": "/mnt/archive",
		},
		Repositories: []state.RepoOutcome{
			{ID: "local", Path: "/mnt/local", Result: state.ResultOK, Files: 11, Measured: true},
			{ID: "offsite", Path: "/mnt/offsite", Result: state.ResultOK, Files: 22, Measured: true},
			{ID: "archive", Path: "/mnt/archive", Result: state.ResultOK, Files: 33, Measured: true},
		},
	})
	rec, _ := store.Record("demo")

	configured := map[string]string{"local": "/mnt/local", "offsite": "/mnt/offsite"}
	out := captureStdout(t, func() {
		printInspect("demo", group, rec, true, "", "none", configured, time.Hour, 0, nil)
	})

	assert.Contains(t, out, "local")
	assert.Contains(t, out, "offsite")
	assert.NotContains(t, out, "archive",
		"a destination the group no longer configures is history, not inventory")

	t.Run("an unknown inventory shows the record as it stands", func(t *testing.T) {
		out := captureStdout(t, func() {
			printInspect("demo", group, rec, true, "", "none", nil, time.Hour, 0, nil)
		})
		assert.Contains(t, out, "archive", "silence is not a report that it was removed")
	})
}
