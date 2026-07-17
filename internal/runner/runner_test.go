package runner

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lugoues/borgmatic-manager/internal/config"
	"github.com/lugoues/borgmatic-manager/internal/state"
)

// fakeExecutor records every command the runner spawns and dispatches
// validate calls and action runs to configurable shell snippets.
type fakeExecutor struct {
	mu    sync.Mutex
	calls [][]string

	// validateScript and runScript are /bin/sh -c bodies. Defaults succeed.
	validateScript string
	runScript      string
}

func newFakeExecutor() *fakeExecutor {
	return &fakeExecutor{
		validateScript: "exit 0",
		runScript:      "exit 0",
	}
}

func (f *fakeExecutor) exec(_ context.Context, name string, args ...string) *exec.Cmd {
	f.mu.Lock()
	f.calls = append(f.calls, append([]string{name}, args...))
	f.mu.Unlock()

	script := f.runScript
	for _, a := range args {
		if a == "validate" {
			script = f.validateScript
			break
		}
	}
	return exec.Command("/bin/sh", "-c", script)
}

func (f *fakeExecutor) callArgs() [][]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([][]string, len(f.calls))
	copy(out, f.calls)
	return out
}

// syncBuffer is a goroutine-safe writer for capturing slog output.
type syncBuffer struct {
	mu  sync.Mutex
	buf strings.Builder
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func newTestRunner(t *testing.T, fake *fakeExecutor, logW io.Writer) *Runner {
	t.Helper()
	if logW == nil {
		logW = io.Discard
	}
	logger := slog.New(slog.NewTextHandler(logW, &slog.HandlerOptions{Level: slog.LevelDebug}))
	r := NewRunner(logger, t.TempDir(), "/usr/bin/borgmatic-fake", nil, 0)
	r.execCommand = fake.exec
	return r
}

func TestTryRunGroup_Success(t *testing.T) {
	fake := newFakeExecutor()
	r := newTestRunner(t, fake, nil)

	ran, err := r.TryRunGroup(context.Background(), "mygroup", config.GroupRunMeta{})
	require.NoError(t, err)
	assert.True(t, ran)

	calls := fake.callArgs()
	require.Len(t, calls, 2, "validate then run")

	validate := strings.Join(calls[0], " ")
	assert.Equal(t, "/usr/bin/borgmatic-fake", calls[0][0])
	assert.Contains(t, validate, "config validate")
	assert.Contains(t, validate, filepath.Join(r.configDir, "mygroup.yaml"))

	run := strings.Join(calls[1], " ")
	assert.Contains(t, run, "--config "+filepath.Join(r.configDir, "mygroup.yaml"))
	assert.Contains(t, run, "--verbosity 1")
	assert.Contains(t, run, "--log-json")
	assert.Contains(t, run, "create --json prune compact check", "default actions apply retention, not just create, and --json binds to create")
}

func TestTryRunGroup_CustomActions(t *testing.T) {
	fake := newFakeExecutor()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	r := NewRunner(logger, t.TempDir(), "/usr/bin/borgmatic-fake", []string{"create"}, 0)
	r.execCommand = fake.exec

	_, err := r.TryRunGroup(context.Background(), "g", config.GroupRunMeta{})
	require.NoError(t, err)

	run := strings.Join(fake.callArgs()[1], " ")
	assert.True(t, strings.HasSuffix(run, "--log-json create --json"), "custom actions replace defaults: %s", run)
}

func TestTryRunGroup_ValidationGate(t *testing.T) {
	fake := newFakeExecutor()
	fake.validateScript = "echo 'schema error' >&2; exit 1"
	var buf syncBuffer
	r := newTestRunner(t, fake, &buf)

	ran, err := r.TryRunGroup(context.Background(), "badgroup", config.GroupRunMeta{})
	assert.True(t, ran)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "validation failed")

	assert.Len(t, fake.callArgs(), 1, "backup must not run when validation fails")
	assert.Contains(t, buf.String(), "schema error")
}

func TestTryRunGroup_MutexSkip(t *testing.T) {
	fake := newFakeExecutor()
	fake.runScript = "sleep 2"
	r := newTestRunner(t, fake, nil)

	go func() {
		_, _ = r.TryRunGroup(context.Background(), "mygroup", config.GroupRunMeta{})
	}()
	// Wait until the first run holds the group lock (validate + run started).
	require.Eventually(t, func() bool {
		return len(fake.callArgs()) >= 2
	}, 2*time.Second, 10*time.Millisecond)

	ran, err := r.TryRunGroup(context.Background(), "mygroup", config.GroupRunMeta{})
	require.NoError(t, err)
	assert.False(t, ran, "overlapping run of the same group must be skipped, not queued")
}

func TestTryRunGroup_ExitCodeError(t *testing.T) {
	fake := newFakeExecutor()
	fake.runScript = "echo boom >&2; exit 1"
	r := newTestRunner(t, fake, nil)

	ran, err := r.TryRunGroup(context.Background(), "g", config.GroupRunMeta{})
	assert.True(t, ran)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "exit 1")
}

func TestRunGroup_LogJSONParsing(t *testing.T) {
	fake := newFakeExecutor()
	fake.runScript = `
echo '{"type":"log_message","levelname":"INFO","message":"creating archive","name":"borgmatic"}'
echo '{"type":"log_message","levelname":"WARNING","message":"file changed while we backed it up","name":"borg"}'
echo 'plain stderr noise' >&2
exit 0`
	var buf syncBuffer
	r := newTestRunner(t, fake, &buf)

	_, err := r.TryRunGroup(context.Background(), "g", config.GroupRunMeta{})
	require.NoError(t, err)

	out := buf.String()
	assert.Contains(t, out, "creating archive")
	assert.Contains(t, out, "level=WARN")
	assert.Contains(t, out, "file changed while we backed it up")
	assert.Contains(t, out, "plain stderr noise")
	// Two warnings total: the JSON WARNING record + the raw stderr line.
	assert.Contains(t, out, "warnings=2")
}

func TestRunGroup_RepoMissingHintOnce(t *testing.T) {
	fake := newFakeExecutor()
	fake.runScript = `echo '{"type":"log_message","levelname":"CRITICAL","message":"Repository /mnt/repo does not exist.","name":"borg"}' >&2; exit 1`
	var buf syncBuffer
	r := newTestRunner(t, fake, &buf)

	_, err := r.TryRunGroup(context.Background(), "g", config.GroupRunMeta{})
	require.Error(t, err)
	_, err = r.TryRunGroup(context.Background(), "g", config.GroupRunMeta{})
	require.Error(t, err)

	out := buf.String()
	assert.Contains(t, out, "repo-create", "missing repo must produce the guided bootstrap hint")
	assert.Equal(t, 1, strings.Count(out, "repo-create --encryption"),
		"the bootstrap hint logs once per group, not every cycle")
}

// lockProbeScript writes a start marker, flags overlap if another instance's
// marker exists, then removes its marker.
func lockProbeScript(dir, id string) string {
	return fmt.Sprintf(`
for other in %[1]s/running-*; do
  [ -e "$other" ] && echo OVERLAP > %[1]s/overlap
done
touch %[1]s/running-%[2]s
sleep 0.3
rm %[1]s/running-%[2]s
exit 0`, dir, id)
}

// lockProbeExecutor returns an execCommand seam whose run commands detect
// concurrent execution via marker files in dir.
func lockProbeExecutor(dir string) func(context.Context, string, ...string) *exec.Cmd {
	var mu sync.Mutex
	counter := 0
	return func(_ context.Context, name string, args ...string) *exec.Cmd {
		for _, a := range args {
			if a == "validate" {
				return exec.Command("true")
			}
		}
		mu.Lock()
		counter++
		id := fmt.Sprintf("%d", counter)
		mu.Unlock()
		return exec.Command("/bin/sh", "-c", lockProbeScript(dir, id))
	}
}

func TestSharedRepoGroupsSerialize(t *testing.T) {
	dir := t.TempDir()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	r := NewRunner(logger, t.TempDir(), "/usr/bin/borgmatic-fake", nil, 0)
	r.execCommand = lockProbeExecutor(dir)

	meta := config.GroupRunMeta{Repos: []string{"/mnt/shared-repo"}}
	var wg sync.WaitGroup
	for _, group := range []string{"alpha", "beta", "gamma"} {
		wg.Add(1)
		go func(g string) {
			defer wg.Done()
			ran, err := r.TryRunGroup(context.Background(), g, meta)
			assert.True(t, ran, "shared-repo groups must queue (blocking), not skip, skipping starves them")
			assert.NoError(t, err)
		}(group)
	}
	wg.Wait()

	_, err := os.Stat(filepath.Join(dir, "overlap"))
	assert.True(t, os.IsNotExist(err), "groups sharing a repository must never run concurrently")
}

func TestSnapshotGroupsSerialize(t *testing.T) {
	dir := t.TempDir()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	r := NewRunner(logger, t.TempDir(), "/usr/bin/borgmatic-fake", nil, 0)
	r.execCommand = lockProbeExecutor(dir)

	// Disjoint repos, only the snapshot lock forces serialization
	// (borgmatic's prefix-matched snapshot cleanup is mutually destructive).
	var wg sync.WaitGroup
	for i, repo := range []string{"/mnt/repo-a", "/mnt/repo-b"} {
		wg.Add(1)
		go func(g, repo string) {
			defer wg.Done()
			meta := config.GroupRunMeta{Repos: []string{repo}, SnapshotHooks: true}
			_, err := r.TryRunGroup(context.Background(), g, meta)
			assert.NoError(t, err)
		}(fmt.Sprintf("group-%d", i), repo)
	}
	wg.Wait()

	_, err := os.Stat(filepath.Join(dir, "overlap"))
	assert.True(t, os.IsNotExist(err), "snapshot-enabled groups must serialize globally")
}

func TestDisjointReposRunConcurrently(t *testing.T) {
	dir := t.TempDir()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	r := NewRunner(logger, t.TempDir(), "/usr/bin/borgmatic-fake", nil, 0)

	// Each script announces itself and waits (bounded) for the other; both
	// finish in time only if they run concurrently.
	script := func(self, other string) string {
		return fmt.Sprintf(`
touch %[1]s/%[2]s
n=0
while [ ! -e %[1]s/%[3]s ]; do
  n=$((n+1)); [ $n -gt 100 ] && exit 7
  sleep 0.05
done
exit 0`, dir, self, other)
	}

	scripts := map[string]string{
		"alpha": script("start-alpha", "start-beta"),
		"beta":  script("start-beta", "start-alpha"),
	}
	r.execCommand = func(_ context.Context, name string, args ...string) *exec.Cmd {
		for _, a := range args {
			if a == "validate" {
				return exec.Command("true")
			}
		}
		for group, s := range scripts {
			for _, a := range args {
				if strings.Contains(a, group+".yaml") {
					return exec.Command("/bin/sh", "-c", s)
				}
			}
		}
		return exec.Command("false")
	}

	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i, g := range []string{"alpha", "beta"} {
		wg.Add(1)
		go func(i int, g, repo string) {
			defer wg.Done()
			_, errs[i] = r.TryRunGroup(context.Background(), g, config.GroupRunMeta{Repos: []string{repo}})
		}(i, g, "/mnt/repo-"+g)
	}
	wg.Wait()

	assert.NoError(t, errs[0], "groups with disjoint repositories must run in parallel")
	assert.NoError(t, errs[1], "groups with disjoint repositories must run in parallel")
}

func TestShutdownSignalsProcessGroup(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, "got-term")
	fake := newFakeExecutor()
	fake.runScript = fmt.Sprintf(`
trap 'touch %s; exit 143' TERM
touch %s/started
n=0
while [ $n -lt 200 ]; do n=$((n+1)); sleep 0.05; done
exit 0`, marker, dir)
	r := newTestRunner(t, fake, nil)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := r.TryRunGroup(ctx, "g", config.GroupRunMeta{})
		done <- err
	}()

	require.Eventually(t, func() bool {
		_, err := os.Stat(filepath.Join(dir, "started"))
		return err == nil
	}, 3*time.Second, 10*time.Millisecond, "borgmatic child should have started")

	cancel()

	select {
	case err := <-done:
		require.Error(t, err)
		assert.Contains(t, err.Error(), "terminated")
	case <-time.After(5 * time.Second):
		t.Fatal("run did not stop after context cancellation")
	}

	_, err := os.Stat(marker)
	assert.NoError(t, err, "child must receive SIGTERM (delivered to its process group)")
}

func TestRunTimeoutEscalatesToKill(t *testing.T) {
	fake := newFakeExecutor()
	// Ignores SIGTERM entirely; only SIGKILL can end it.
	fake.runScript = `trap '' TERM
n=0
while [ $n -lt 400 ]; do n=$((n+1)); sleep 0.05; done`
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	r := NewRunner(logger, t.TempDir(), "/usr/bin/borgmatic-fake", nil, 300*time.Millisecond)
	r.killGrace = 500 * time.Millisecond
	r.execCommand = fake.exec

	start := time.Now()
	ran, err := r.TryRunGroup(context.Background(), "g", config.GroupRunMeta{})
	elapsed := time.Since(start)

	assert.True(t, ran)
	require.Error(t, err, "a killed run must report an error")
	assert.Less(t, elapsed, 10*time.Second,
		"SIGKILL escalation must end a SIGTERM-ignoring child; wedged children may not hold locks forever")
}

func TestGroupLockReleasedAfterRun(t *testing.T) {
	fake := newFakeExecutor()
	r := newTestRunner(t, fake, nil)

	for i := 0; i < 3; i++ {
		ran, err := r.TryRunGroup(context.Background(), "g", config.GroupRunMeta{Repos: []string{"/r"}, SnapshotHooks: true})
		require.NoError(t, err, "iteration %d", i)
		require.True(t, ran, "all locks must be released between sequential runs (iteration %d)", i)
	}
}

// recordingStore captures RecordRun calls.
type recordingStore struct {
	mu       sync.Mutex
	outcomes map[string]state.RunOutcome
}

func (r *recordingStore) RecordRun(group string, outcome state.RunOutcome) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.outcomes == nil {
		r.outcomes = map[string]state.RunOutcome{}
	}
	r.outcomes[group] = outcome
}

func TestTryRunGroup_RecordsOutcome(t *testing.T) {
	fake := newFakeExecutor()
	// The create --json result arrives on stdout concatenated with a log
	// record on the same line (observed borgmatic behavior), parsing
	// must not depend on line boundaries.
	fake.runScript = `echo '{"levelname":"INFO","message":"Creating archive at \"/repo::files-old-name\"","name":"borg"}[{"archive":{"name":"files-2026-07-07","stats":{"nfiles":42,"original_size":1218,"compressed_size":1203,"deduplicated_size":97}}}]'; echo '{"levelname":"WARNING","message":"w","name":"borg"}' >&2; exit 0`
	r := newTestRunner(t, fake, nil)
	rec := &recordingStore{}
	r.SetRecorder(rec)

	ran, err := r.TryRunGroup(context.Background(), "files", config.GroupRunMeta{})
	require.NoError(t, err)
	require.True(t, ran)

	rec.mu.Lock()
	defer rec.mu.Unlock()
	o, ok := rec.outcomes["files"]
	require.True(t, ok, "a completed run must record an outcome")
	assert.Equal(t, "ok", o.Result)
	assert.Equal(t, "files-2026-07-07", o.Archive, "result archive name wins over the log-line capture")
	assert.Equal(t, int64(1), o.Warnings)
	assert.Equal(t, int64(42), o.Files)
	assert.Equal(t, int64(1218), o.OriginalBytes)
	assert.Equal(t, int64(97), o.DeduplicatedBytes)
	assert.False(t, o.Finished.IsZero())
}

func TestTryRunGroup_JSONBoundToCreateOnly(t *testing.T) {
	fake := newFakeExecutor()
	r := newTestRunner(t, fake, nil)

	_, err := r.TryRunGroup(context.Background(), "files", config.GroupRunMeta{})
	require.NoError(t, err)

	run := strings.Join(fake.callArgs()[1], " ")
	assert.Contains(t, run, "create --json prune", "--json must bind to the create action")
	assert.Equal(t, 1, strings.Count(run, "--json"), "other actions must not get --json")
}

func TestTryRunGroup_RecordsFailureOutcome(t *testing.T) {
	fake := newFakeExecutor()
	fake.runScript = "exit 2"
	r := newTestRunner(t, fake, nil)
	rec := &recordingStore{}
	r.SetRecorder(rec)

	_, err := r.TryRunGroup(context.Background(), "files", config.GroupRunMeta{})
	require.Error(t, err)

	rec.mu.Lock()
	defer rec.mu.Unlock()
	o, ok := rec.outcomes["files"]
	require.True(t, ok, "failures must be recorded too, status wants the truth")
	assert.Equal(t, "failed", o.Result)
	assert.Equal(t, 2, o.ExitCode)
}

func TestTryRunGroup_RecordsFailureReason(t *testing.T) {
	fake := newFakeExecutor()
	// Two CRITICAL lines then a non-zero exit. Only the first is the cause;
	// the second is fallout and must not overwrite it.
	fake.runScript = `echo '{"type":"log_message","levelname":"CRITICAL","message":"Repository /mnt/repo does not exist.","name":"borg"}' >&2
echo '{"type":"log_message","levelname":"CRITICAL","message":"terminating with error status.","name":"borgmatic"}' >&2
exit 1`
	r := newTestRunner(t, fake, nil)
	rec := &recordingStore{}
	r.SetRecorder(rec)

	_, err := r.TryRunGroup(context.Background(), "files", config.GroupRunMeta{})
	require.Error(t, err)

	rec.mu.Lock()
	defer rec.mu.Unlock()
	o := rec.outcomes["files"]
	assert.Equal(t, "failed", o.Result)
	assert.Equal(t, "Repository /mnt/repo does not exist.", o.LastError,
		"the first CRITICAL message is the cause; later ones must not overwrite it")
}

func TestTryRunGroup_SuccessHasNoReason(t *testing.T) {
	fake := newFakeExecutor()
	// A WARNING on a clean run must not be mistaken for a failure reason.
	fake.runScript = `echo '{"type":"log_message","levelname":"WARNING","message":"file changed while we backed it up","name":"borg"}' >&2; exit 0`
	r := newTestRunner(t, fake, nil)
	rec := &recordingStore{}
	r.SetRecorder(rec)

	_, err := r.TryRunGroup(context.Background(), "files", config.GroupRunMeta{})
	require.NoError(t, err)

	rec.mu.Lock()
	defer rec.mu.Unlock()
	o := rec.outcomes["files"]
	assert.Equal(t, "ok", o.Result)
	assert.Empty(t, o.LastError, "a successful run carries no failure reason")
}

func TestTryRunGroup_RecordsLogTail(t *testing.T) {
	fake := newFakeExecutor()
	fake.runScript = `echo '{"type":"log_message","levelname":"INFO","message":"creating archive","name":"borgmatic"}'
echo '{"type":"log_message","levelname":"WARNING","message":"file changed while we backed it up","name":"borg"}' >&2
echo '{"type":"log_message","levelname":"DEBUG","message":"noisy internal detail","name":"borg"}' >&2
exit 0`
	r := newTestRunner(t, fake, nil)
	rec := &recordingStore{}
	r.SetRecorder(rec)

	_, err := r.TryRunGroup(context.Background(), "files", config.GroupRunMeta{})
	require.NoError(t, err)

	rec.mu.Lock()
	defer rec.mu.Unlock()
	o := rec.outcomes["files"]
	joined := strings.Join(o.LogTail, "\n")
	assert.Contains(t, joined, "creating archive", "the inspect tail keeps INFO lines")
	assert.Contains(t, joined, "file changed while we backed it up", "and WARNING lines")
	assert.NotContains(t, joined, "noisy internal detail", "but drops DEBUG noise")
}

func TestTryRunGroup_SkipsWhenRepoLockedByAnotherProcess(t *testing.T) {
	fake := newFakeExecutor()
	r := newTestRunner(t, fake, nil)
	lockDir := t.TempDir()
	r.SetLockDir(lockDir)

	// Stand in for another process (the daemon) holding this group's repo lock.
	held, ok, err := tryCrossLock(lockDir, "repo:/repo/shared")
	require.NoError(t, err)
	require.True(t, ok)
	defer held.Release()

	ran, err := r.TryRunGroup(context.Background(), "files", config.GroupRunMeta{Repos: []string{"/repo/shared"}})
	require.ErrorIs(t, err, ErrLockedByAnotherProcess,
		"a held cross-process lock must be reported distinctly: the scheduler backs off on it, rather than re-attempting every minWake")
	assert.False(t, ran, "the group must be skipped while another process holds its repo lock")
	assert.Empty(t, fake.callArgs(), "borgmatic must not run when the lock is held")
}

func TestTryRunGroup_RunsWhenLockFree(t *testing.T) {
	fake := newFakeExecutor()
	r := newTestRunner(t, fake, nil)
	r.SetLockDir(t.TempDir())

	ran, err := r.TryRunGroup(context.Background(), "files", config.GroupRunMeta{Repos: []string{"/repo/free"}})
	require.NoError(t, err)
	assert.True(t, ran, "with the lock free, the group runs")
	assert.NotEmpty(t, fake.callArgs(), "borgmatic is invoked")
}

func TestValidateConfig_TimeoutKillsAndFails(t *testing.T) {
	fake := newFakeExecutor()
	fake.validateScript = "sleep 60"
	r := newTestRunner(t, fake, nil)
	r.killGrace = 50 * time.Millisecond
	rec := &recordingStore{}
	r.SetRecorder(rec)

	// Shrink the validate timeout via a cancelled context (the shutdown
	// path exercises the same terminate-and-wait machinery).
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(100 * time.Millisecond); cancel() }()

	start := time.Now()
	_, err := r.TryRunGroup(ctx, "wedged", config.GroupRunMeta{})
	require.Error(t, err, "a hung validate must fail the group, not stall it")
	assert.Less(t, time.Since(start), 10*time.Second, "validate must be interruptible while holding locks")

	rec.mu.Lock()
	defer rec.mu.Unlock()
	o, ok := rec.outcomes["wedged"]
	require.True(t, ok, "validation failures must reach status")
	assert.Equal(t, "config-invalid", o.Result)
}

func TestValidateConfig_FailureRecordedForStatus(t *testing.T) {
	fake := newFakeExecutor()
	fake.validateScript = "echo 'schema error' >&2; exit 1"
	r := newTestRunner(t, fake, nil)
	rec := &recordingStore{}
	r.SetRecorder(rec)

	_, err := r.TryRunGroup(context.Background(), "bad", config.GroupRunMeta{})
	require.Error(t, err)

	rec.mu.Lock()
	defer rec.mu.Unlock()
	assert.Equal(t, "config-invalid", rec.outcomes["bad"].Result)
}

// helperReapHarness fakes the pending tracker and reap func, recording the
// order of lifecycle events.
type helperReapHarness struct {
	mu     sync.Mutex
	events []string
}

func (h *helperReapHarness) RecordPending(runID, group string, started time.Time) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.events = append(h.events, "pending:"+group+":"+runID)
}

func (h *helperReapHarness) ClearPending(runID string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.events = append(h.events, "clear:"+runID)
}

func (h *helperReapHarness) reap(_ context.Context, runID string) ([]string, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.events = append(h.events, "reap:"+runID)
	return []string{"stray-helper"}, nil
}

func TestHelperReapLifecycle(t *testing.T) {
	fake := newFakeExecutor()
	r := newTestRunner(t, fake, nil)
	h := &helperReapHarness{}
	r.SetHelperReaper(h, h.reap)

	ran, err := r.TryRunGroup(context.Background(), "db", config.GroupRunMeta{RunID: "run-1"})
	require.NoError(t, err)
	require.True(t, ran)

	h.mu.Lock()
	defer h.mu.Unlock()
	require.Equal(t, []string{"pending:db:run-1", "reap:run-1", "clear:run-1"}, h.events,
		"pending before spawn, reap after exit, clear after reap")
}

func TestHelperReapRunsOnFailureToo(t *testing.T) {
	fake := newFakeExecutor()
	fake.runScript = "exit 2" // the repo-missing first run is the classic leak path
	r := newTestRunner(t, fake, nil)
	h := &helperReapHarness{}
	r.SetHelperReaper(h, h.reap)

	_, err := r.TryRunGroup(context.Background(), "db", config.GroupRunMeta{RunID: "run-2"})
	require.Error(t, err)

	h.mu.Lock()
	defer h.mu.Unlock()
	assert.Contains(t, h.events, "reap:run-2", "failed runs are exactly when helpers orphan")
	assert.Contains(t, h.events, "clear:run-2")
}

func TestHelperReapSkippedWithoutRunID(t *testing.T) {
	fake := newFakeExecutor()
	r := newTestRunner(t, fake, nil)
	h := &helperReapHarness{}
	r.SetHelperReaper(h, h.reap)

	_, err := r.TryRunGroup(context.Background(), "db", config.GroupRunMeta{})
	require.NoError(t, err)

	h.mu.Lock()
	defer h.mu.Unlock()
	assert.Empty(t, h.events, "no run id (legacy meta) means no reap bookkeeping")
}
