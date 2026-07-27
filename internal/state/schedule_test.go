package state_test

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
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
	s.Retain(map[string]struct{}{"keep": {}}, nil)
	rec, ok := s.Record("gone")
	require.True(t, ok, "one absent cycle must not prune")
	assert.Equal(t, 1, rec.MissingCycles)
	s.Retain(map[string]struct{}{"keep": {}}, nil)
	_, ok = s.Record("gone")
	require.True(t, ok, "two absent cycles must not prune")

	// Third consecutive absence prunes, and it persists.
	s.Retain(map[string]struct{}{"keep": {}}, nil)
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

	s.Retain(map[string]struct{}{}, nil)          // absent once
	s.Retain(map[string]struct{}{"app": {}}, nil) // back
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
	for i := range 100 {
		s.RecordRun("files", state.RunOutcome{
			Result:        state.ResultOK,
			OriginalBytes: int64(i),
			LogTail:       []string{fmt.Sprintf("run %d line", i)},
		})
	}

	rec, ok := state.LoadSchedule(dir, discardLogger()).Record("files")
	require.True(t, ok)

	assert.LessOrEqual(t, len(rec.History), 90, "history must be bounded")
	assert.Equal(t, int64(99), rec.History[len(rec.History)-1].OriginalBytes, "newest run is last")
	assert.Equal(t, int64(10), rec.History[0].OriginalBytes, "oldest kept run is run 10 (100 recorded, cap 90)")

	// Only the last run keeps its log tail; history entries are stripped.
	require.NotNil(t, rec.LastRun)
	assert.Equal(t, []string{"run 99 line"}, rec.LastRun.LogTail)
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

// A corrupt state file must be healed even by a mutation that changes nothing
// (e.g. a Retain in a cycle where no group is due): otherwise the corpse is
// never rewritten and the "corrupt" warning repeats every cycle.
func TestCorruptFileHealedByNoOpMutation(t *testing.T) {
	dir := t.TempDir()
	schedPath := filepath.Join(dir, "schedule.json")
	require.NoError(t, os.WriteFile(schedPath, []byte("{not json"), 0o600))

	s := state.LoadSchedule(dir, discardLogger())
	// Retain over the (empty, corrupt-degraded) state changes nothing.
	s.Retain(map[string]struct{}{}, nil)

	// The file must now be valid JSON, not the corrupt bytes.
	data, err := os.ReadFile(schedPath)
	require.NoError(t, err)
	var f map[string]any
	assert.NoError(t, json.Unmarshal(data, &f), "the corrupt file must have been rewritten as valid JSON")
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

func TestScheduleRetainProtectedGroupNeverPruned(t *testing.T) {
	dir := t.TempDir()
	s := state.LoadSchedule(dir, discardLogger())
	s.MarkSuccess("offline", "fp", time.Now())

	protected := map[string]struct{}{"offline": {}}
	// Many absent cycles: a protected (cached) group keeps its record and its
	// last-backup, but stays marked absent so NextWake ignores it.
	for i := 0; i < 5; i++ {
		s.Retain(map[string]struct{}{}, protected)
	}
	rec, ok := s.Record("offline")
	require.True(t, ok, "a protected group is never pruned, no matter how long it is absent")
	assert.Positive(t, rec.MissingCycles, "still marked absent so it does not drive NextWake")
	assert.False(t, rec.LastSuccess.IsZero(), "last-backup survives for the offline display")

	// An unprotected group vanishing alongside it still ages out.
	s.MarkSuccess("gone", "fp", time.Now())
	for i := 0; i < 3; i++ {
		s.Retain(map[string]struct{}{}, protected)
	}
	_, ok = s.Record("gone")
	assert.False(t, ok, "unprotected groups still age out")
}

func TestRecordRunTracksPerRepositoryLastSuccess(t *testing.T) {
	dir := t.TempDir()
	s := state.LoadSchedule(dir, discardLogger())
	t0 := time.Now()

	// Run 1: both repos succeed.
	s.RecordRun("g", state.RunOutcome{
		Finished: t0, Result: state.ResultOK,
		Repositories: []state.RepoOutcome{
			{ID: "local", Result: state.ResultOK, Files: 10},
			{ID: "offsite", Result: state.ResultOK, Files: 10},
		},
	})
	// Run 2 (an hour later): offsite fails, local still succeeds.
	s.RecordRun("g", state.RunOutcome{
		Finished: t0.Add(time.Hour), Result: state.ResultFailed,
		Repositories: []state.RepoOutcome{
			{ID: "local", Result: state.ResultOK, Files: 12},
			{ID: "offsite", Result: state.ResultFailed},
		},
	})

	rec, ok := state.LoadSchedule(dir, discardLogger()).Record("g")
	require.True(t, ok)
	require.Contains(t, rec.Repositories, "local")
	require.Contains(t, rec.Repositories, "offsite")

	assert.Equal(t, t0.Add(time.Hour).Unix(), rec.Repositories["local"].LastSuccess.Unix(),
		"local's last success advances to run 2")
	assert.Equal(t, t0.Unix(), rec.Repositories["offsite"].LastSuccess.Unix(),
		"offsite's last success stays at run 1, since run 2 failed for it")
	assert.Equal(t, state.ResultFailed, rec.Repositories["offsite"].LastRun.Result)

	// Per-repo detail is not duplicated into group history entries.
	require.NotEmpty(t, rec.History)
	assert.Nil(t, rec.History[len(rec.History)-1].Repositories)
}

// A later failure replaces LastRun outright, and a probe-confirmed success
// carries no measurements at all. Reading sizes from LastRun therefore either
// stopped reporting them or reported zeros, and a dataset that appears to have
// shrunk to nothing is a worse lie than one that stops updating.
func TestRepoStatsSurviveALaterFailureAndAStatlessSuccess(t *testing.T) {
	dir := t.TempDir()
	s := state.LoadSchedule(dir, nil)

	s.RecordRun("g", state.RunOutcome{
		Finished: time.Now().Add(-2 * time.Hour), Result: state.ResultOK,
		Repositories: []state.RepoOutcome{
			{ID: "local", Result: state.ResultOK, Files: 100, OriginalBytes: 5000, DurationSeconds: 12},
		},
	})
	stats := s.Snapshot()["g"].Repositories["local"].LastStats
	require.NotNil(t, stats)
	assert.Equal(t, int64(100), stats.Files)

	// A failure afterwards: the measurements from the last real backup stand.
	s.RecordRun("g", state.RunOutcome{
		Finished: time.Now().Add(-time.Hour), Result: state.ResultFailed,
		Repositories: []state.RepoOutcome{{ID: "local", Result: state.ResultFailed}},
	})
	stats = s.Snapshot()["g"].Repositories["local"].LastStats
	require.NotNil(t, stats, "a failure must not erase the last measured backup")
	assert.Equal(t, int64(5000), stats.OriginalBytes)

	// A probe-confirmed success carries nothing to measure and must not
	// overwrite real numbers with zeros.
	s.RecordRun("g", state.RunOutcome{
		Finished: time.Now(), Result: state.ResultFailed,
		Repositories: []state.RepoOutcome{{ID: "local", Result: state.ResultOK}},
	})
	stats = s.Snapshot()["g"].Repositories["local"].LastStats
	require.NotNil(t, stats)
	assert.Equal(t, int64(5000), stats.OriginalBytes, "a stat-less success must not zero the sizes")
}

// A destination removed from the config lingered forever in status, inspect and
// the exported series, and was counted in the partial ratio, so the group
// reported partial permanently.
func TestRemovedRepositoriesAreDroppedFromState(t *testing.T) {
	dir := t.TempDir()
	s := state.LoadSchedule(dir, nil)

	s.RecordRun("g", state.RunOutcome{
		Finished: time.Now(), Result: state.ResultOK,
		ConfiguredRepositories: []string{"local", "offsite"},
		Repositories: []state.RepoOutcome{
			{ID: "local", Result: state.ResultOK},
			{ID: "offsite", Result: state.ResultOK},
		},
	})
	require.Len(t, s.Snapshot()["g"].Repositories, 2)

	// offsite is removed from the group's configuration.
	s.RecordRun("g", state.RunOutcome{
		Finished: time.Now(), Result: state.ResultOK,
		ConfiguredRepositories: []string{"local"},
		Repositories:           []state.RepoOutcome{{ID: "local", Result: state.ResultOK}},
	})
	repos := s.Snapshot()["g"].Repositories
	assert.Len(t, repos, 1, "a repository the group no longer configures must not linger")
	assert.Contains(t, repos, "local")

	// A failed run omits repositories it could not judge; that must not be read
	// as their removal.
	s.RecordRun("g", state.RunOutcome{
		Finished: time.Now(), Result: state.ResultFailed,
		ConfiguredRepositories: []string{"local"},
	})
	assert.Contains(t, s.Snapshot()["g"].Repositories, "local",
		"an unjudged repository is not a removed one")
}

// The other half of reconciliation. A destination added to an established group
// whose next run cannot judge it (a pre-backup hook fails, the probe finds no
// archive) appears in neither the outcome nor the record, so it is missing from
// the inventory series and the alert join cannot see the one destination that
// has never backed up.
func TestConfiguredRepositoriesAppearEvenWhenARunCannotJudgeThem(t *testing.T) {
	dir := t.TempDir()
	s := state.LoadSchedule(dir, nil)

	s.RecordRun("g", state.RunOutcome{
		Finished: time.Now().Add(-time.Hour), Result: state.ResultOK,
		ConfiguredRepositories: []string{"local"},
		Repositories:           []state.RepoOutcome{{ID: "local", Result: state.ResultOK, Files: 10}},
	})

	// "offsite" is added, and the next run fails before anything can be judged.
	s.RecordRun("g", state.RunOutcome{
		Finished: time.Now(), Result: state.ResultFailed,
		ConfiguredRepositories: []string{"local", "offsite"},
	})

	repos := s.Snapshot()["g"].Repositories
	require.Contains(t, repos, "offsite", "a configured destination must be in the inventory to be alertable")
	assert.Nil(t, repos["offsite"].LastRun, "with nothing known about it yet")
	assert.True(t, repos["offsite"].LastSuccess.IsZero())

	// The placeholder must not overwrite what is already known about a sibling.
	require.NotNil(t, repos["local"].LastStats)
	assert.Equal(t, int64(10), repos["local"].LastStats.Files)
}

// Snapshot and Record hand out GroupRecords whose Repositories map and History
// slice are shared with the stored records. That is only safe because a write
// replaces those structures rather than mutating them: update parses a fresh
// copy from disk and swaps it in. Mutating in place is the obvious optimization
// to make here (it would save a read and a parse per write) and it would turn
// every concurrent snapshot into a concurrent map access, which is a crash
// rather than a wrong number.
//
// Run under -race, this fails on both counts: the detector fires, and the
// assertions below catch a snapshot changing under its holder even without it.
func TestSnapshotsAreNotMutatedByLaterWrites(t *testing.T) {
	dir := t.TempDir()
	s := state.LoadSchedule(dir, nil)

	s.RecordRun("g", state.RunOutcome{
		Finished: time.Now(), Result: state.ResultOK, CreateAttempted: true,
		ConfiguredRepositories: []string{"local"},
		Repositories:           []state.RepoOutcome{{ID: "local", Result: state.ResultOK, Files: 1, Measured: true}},
	})

	snap := s.Snapshot()["g"]
	rec, ok := s.Record("g")
	require.True(t, ok)
	require.Len(t, snap.Repositories, 1)
	historyLen := len(snap.History)

	// Concurrent writers while the snapshots are held and read.
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := range 200 {
			s.RecordRun("g", state.RunOutcome{
				Finished: time.Now(), Result: state.ResultOK, CreateAttempted: true,
				ConfiguredRepositories: []string{"local", fmt.Sprintf("added-%d", i)},
				Repositories: []state.RepoOutcome{
					{ID: "local", Result: state.ResultOK, Files: int64(i), Measured: true},
				},
			})
		}
	}()
	go func() {
		defer wg.Done()
		for range 200 {
			for id := range snap.Repositories {
				_ = id
			}
			for _, h := range rec.History {
				_ = h.Result
			}
		}
	}()
	wg.Wait()

	assert.Len(t, snap.Repositories, 1, "a held snapshot must not gain entries a later write added")
	assert.Equal(t, int64(1), snap.Repositories["local"].LastRun.Files,
		"nor have its values rewritten underneath it")
	assert.Len(t, rec.History, historyLen, "and its history must not grow either")
}

// An empty archive is a legitimate backup: every count is zero, and if it
// finishes in under a second its duration truncates to zero too. Inferring
// "was this measured" from "is anything nonzero" leaves that backup reported
// under an older, larger archive forever, which reads as a dataset that never
// changed rather than one that emptied.
func TestAMeasuredEmptyArchiveReplacesTheOlderStats(t *testing.T) {
	dir := t.TempDir()
	s := state.LoadSchedule(dir, nil)

	s.RecordRun("g", state.RunOutcome{
		Finished: time.Now().Add(-time.Hour), Result: state.ResultOK, CreateAttempted: true,
		Repositories: []state.RepoOutcome{
			{ID: "local", Result: state.ResultOK, Files: 900, OriginalBytes: 4000, Measured: true},
		},
	})

	// The source emptied: a real archive, measured, all zeros.
	s.RecordRun("g", state.RunOutcome{
		Finished: time.Now(), Result: state.ResultOK, CreateAttempted: true,
		Repositories: []state.RepoOutcome{{ID: "local", Result: state.ResultOK, Measured: true}},
	})

	stats := s.Snapshot()["g"].Repositories["local"].LastStats
	require.NotNil(t, stats)
	assert.Zero(t, stats.Files, "a measured empty archive is the current truth, not a missing measurement")
	assert.Zero(t, stats.OriginalBytes)

	// A probe-confirmed success is still not a measurement and must not zero it.
	s.RecordRun("g", state.RunOutcome{
		Finished: time.Now(), Result: state.ResultFailed,
		Repositories: []state.RepoOutcome{{ID: "local", Result: state.ResultOK}},
	})
	stats = s.Snapshot()["g"].Repositories["local"].LastStats
	require.NotNil(t, stats)
	assert.True(t, stats.Measured, "the retained stats are still the measured ones")
}

// Records written before Measured existed carry stats and no flag. Reading the
// flag alone would drop them on the first run after an upgrade.
func TestStatsWrittenBeforeTheMeasuredFlagAreStillKept(t *testing.T) {
	dir := t.TempDir()
	s := state.LoadSchedule(dir, nil)

	s.RecordRun("g", state.RunOutcome{
		Finished: time.Now(), Result: state.ResultOK, CreateAttempted: true,
		Repositories: []state.RepoOutcome{
			{ID: "local", Result: state.ResultOK, Files: 12, OriginalBytes: 34}, // no Measured
		},
	})

	stats := s.Snapshot()["g"].Repositories["local"].LastStats
	require.NotNil(t, stats, "an outcome carrying numbers is still a measurement")
	assert.Equal(t, int64(12), stats.Files)
}

// A create that succeeds and a check that then hangs until a 24-hour timeout
// must not stamp the repository as fresh at the moment of the timeout: staleness
// would understate the archive's age by exactly the hang, delaying the alert by
// as long as the problem lasted.
func TestRepositoryFreshnessComesFromTheArchiveNotTheRunEnd(t *testing.T) {
	dir := t.TempDir()
	s := state.LoadSchedule(dir, nil)

	wrote := time.Now().Add(-24 * time.Hour)
	s.RecordRun("g", state.RunOutcome{
		Finished: time.Now(), Result: state.ResultTerminated, CreateAttempted: true,
		Repositories: []state.RepoOutcome{
			{ID: "local", Result: state.ResultOK, Measured: true, Files: 5, CompletedAt: wrote},
		},
	})

	rec := s.Snapshot()["g"].Repositories["local"]
	assert.WithinDuration(t, wrote, rec.LastSuccess, time.Second,
		"the archive is a day old, whatever the run did afterwards")

	t.Run("without a completion time the run end still stands in", func(t *testing.T) {
		finished := time.Now()
		s.RecordRun("h", state.RunOutcome{
			Finished: finished, Result: state.ResultOK, CreateAttempted: true,
			Repositories: []state.RepoOutcome{{ID: "local", Result: state.ResultOK}},
		})
		rec := s.Snapshot()["h"].Repositories["local"]
		assert.WithinDuration(t, finished, rec.LastSuccess, time.Second)
	})
}

// A labelled repository keeps its key when it is repointed, so without noticing
// the path change the new destination inherits the old one's last success and
// reports as recently backed up having never produced an archive.
func TestRepointingALabelledRepositoryResetsItsHistory(t *testing.T) {
	dir := t.TempDir()
	s := state.LoadSchedule(dir, nil)

	s.RecordRun("g", state.RunOutcome{
		Finished: time.Now().Add(-time.Hour), Result: state.ResultOK, CreateAttempted: true,
		Repositories: []state.RepoOutcome{
			{ID: "offsite", Path: "/mnt/old", Result: state.ResultOK, Files: 9, Measured: true},
		},
	})
	require.False(t, s.Snapshot()["g"].Repositories["offsite"].LastSuccess.IsZero())

	// Repointed to a new destination, and the first attempt fails.
	s.RecordRun("g", state.RunOutcome{
		Finished: time.Now(), Result: state.ResultFailed, CreateAttempted: true,
		Repositories: []state.RepoOutcome{
			{ID: "offsite", Path: "/mnt/new", Result: state.ResultFailed},
		},
	})

	rec := s.Snapshot()["g"].Repositories["offsite"]
	assert.True(t, rec.LastSuccess.IsZero(),
		"a destination that has never succeeded must not inherit the old one's freshness")
	assert.Nil(t, rec.LastStats, "nor its measurements")
	assert.Equal(t, "/mnt/new", rec.Path)

	t.Run("an unchanged path keeps its history", func(t *testing.T) {
		s.RecordRun("h", state.RunOutcome{
			Finished: time.Now().Add(-time.Hour), Result: state.ResultOK, CreateAttempted: true,
			Repositories: []state.RepoOutcome{{ID: "local", Path: "/mnt/a", Result: state.ResultOK, Files: 4, Measured: true}},
		})
		s.RecordRun("h", state.RunOutcome{
			Finished: time.Now(), Result: state.ResultFailed, CreateAttempted: true,
			Repositories: []state.RepoOutcome{{ID: "local", Path: "/mnt/a", Result: state.ResultFailed}},
		})
		rec := s.Snapshot()["h"].Repositories["local"]
		assert.False(t, rec.LastSuccess.IsZero())
		require.NotNil(t, rec.LastStats)
		assert.Equal(t, int64(4), rec.LastStats.Files)
	})

	t.Run("a record written before paths were stored is adopted, not reset", func(t *testing.T) {
		s.RecordRun("i", state.RunOutcome{
			Finished: time.Now().Add(-time.Hour), Result: state.ResultOK, CreateAttempted: true,
			Repositories: []state.RepoOutcome{{ID: "local", Result: state.ResultOK, Files: 7, Measured: true}},
		})
		s.RecordRun("i", state.RunOutcome{
			Finished: time.Now(), Result: state.ResultFailed, CreateAttempted: true,
			Repositories: []state.RepoOutcome{{ID: "local", Path: "/mnt/a", Result: state.ResultFailed}},
		})
		rec := s.Snapshot()["i"].Repositories["local"]
		assert.False(t, rec.LastSuccess.IsZero(), "an upgrade must not wipe every repository's freshness")
		assert.Equal(t, "/mnt/a", rec.Path)
	})
}

// The repoint the outcome never mentions: the first run after the change fails
// before the destination can be judged, so only the reconciliation carries its
// path. That is the run most likely to happen, which is what makes it matter.
func TestARepointNoticedOnlyByReconciliationStillResetsHistory(t *testing.T) {
	dir := t.TempDir()
	s := state.LoadSchedule(dir, nil)

	s.RecordRun("g", state.RunOutcome{
		Finished: time.Now().Add(-time.Hour), Result: state.ResultOK, CreateAttempted: true,
		ConfiguredRepositories:    []string{"offsite"},
		ConfiguredRepositoryPaths: map[string]string{"offsite": "/mnt/old"},
		Repositories: []state.RepoOutcome{
			{ID: "offsite", Path: "/mnt/old", Result: state.ResultOK, Files: 9, Measured: true},
		},
	})
	require.False(t, s.Snapshot()["g"].Repositories["offsite"].LastSuccess.IsZero())

	// Repointed, and the run fails before judging anything.
	s.RecordRun("g", state.RunOutcome{
		Finished: time.Now(), Result: state.ResultFailed, CreateAttempted: true,
		ConfiguredRepositories:    []string{"offsite"},
		ConfiguredRepositoryPaths: map[string]string{"offsite": "/mnt/new"},
	})

	rec := s.Snapshot()["g"].Repositories["offsite"]
	assert.True(t, rec.LastSuccess.IsZero(),
		"the new destination has never succeeded and must not inherit a last success")
	assert.Nil(t, rec.LastStats)
	assert.Equal(t, "/mnt/new", rec.Path)

	t.Run("an unchanged path keeps its history through reconciliation", func(t *testing.T) {
		s.RecordRun("h", state.RunOutcome{
			Finished: time.Now().Add(-time.Hour), Result: state.ResultOK, CreateAttempted: true,
			ConfiguredRepositories:    []string{"local"},
			ConfiguredRepositoryPaths: map[string]string{"local": "/mnt/a"},
			Repositories:              []state.RepoOutcome{{ID: "local", Path: "/mnt/a", Result: state.ResultOK, Files: 3, Measured: true}},
		})
		s.RecordRun("h", state.RunOutcome{
			Finished: time.Now(), Result: state.ResultFailed, CreateAttempted: true,
			ConfiguredRepositories:    []string{"local"},
			ConfiguredRepositoryPaths: map[string]string{"local": "/mnt/a"},
		})
		rec := s.Snapshot()["h"].Repositories["local"]
		assert.False(t, rec.LastSuccess.IsZero())
		require.NotNil(t, rec.LastStats)
	})
}
