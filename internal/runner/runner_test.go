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
	"github.com/lugoues/borgmatic-manager/internal/lockfile"
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

// A borgmatic that dies on a signal has no exit status of its own: Go reports
// ExitCode() == -1. Without mapping that to the shell's 128+signal convention it
// falls through to "failed (exit -1)", including for a SIGKILL the manager
// itself sent after the run timeout.
func TestTryRunGroup_SignalDeathRecordsTerminated(t *testing.T) {
	fake := newFakeExecutor()
	fake.runScript = "kill -TERM $$; sleep 5" // die on SIGTERM, no exit code of our own
	r := newTestRunner(t, fake, nil)
	rec := &recordingStore{}
	r.SetRecorder(rec)

	_, err := r.TryRunGroup(context.Background(), "files", config.GroupRunMeta{})
	require.Error(t, err)

	rec.mu.Lock()
	defer rec.mu.Unlock()
	o := rec.outcomes["files"]
	assert.Equal(t, state.ResultTerminated, o.Result, "signal death is a termination, not a failure")
	assert.Equal(t, 143, o.ExitCode, "SIGTERM maps to 128+15, not -1")
}

// An external SIGKILL (the OOM killer, or an operator kill -9) is an abnormal
// death, not a manager-initiated termination. It must be recorded as failed so
// it reaches status's failed-groups alert, recording it as "terminated" would
// silently hide dying backups.
func TestTryRunGroup_ExternalSigkillRecordsFailed(t *testing.T) {
	fake := newFakeExecutor()
	fake.runScript = "kill -KILL $$; sleep 5" // external SIGKILL, no manager timeout
	r := newTestRunner(t, fake, nil)          // runTimeout 0: not a timeout
	rec := &recordingStore{}
	r.SetRecorder(rec)

	_, err := r.TryRunGroup(context.Background(), "files", config.GroupRunMeta{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "killed", "the error must name the abnormal kill")

	rec.mu.Lock()
	defer rec.mu.Unlock()
	o := rec.outcomes["files"]
	assert.Equal(t, state.ResultFailed, o.Result, "an external SIGKILL is a failure, not a benign termination")
	assert.Equal(t, 137, o.ExitCode, "SIGKILL maps to 128+9")
}

// A validate killed on shutdown must still fail the group, nothing may run
// against an unvalidated config, but must NOT be recorded as config-invalid:
// the config was never judged, and the mark would outlive the restart, leaving
// the group flagged broken until its next successful run.
func TestValidateConfig_ShutdownDoesNotMarkConfigInvalid(t *testing.T) {
	fake := newFakeExecutor()
	fake.validateScript = "sleep 60"
	r := newTestRunner(t, fake, nil)
	r.killGrace = 50 * time.Millisecond
	rec := &recordingStore{}
	r.SetRecorder(rec)

	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(100 * time.Millisecond); cancel() }()

	start := time.Now()
	_, err := r.TryRunGroup(ctx, "wedged", config.GroupRunMeta{})
	require.Error(t, err, "a hung validate must fail the group, not stall it")
	assert.Less(t, time.Since(start), 10*time.Second, "validate must be interruptible while holding locks")

	rec.mu.Lock()
	defer rec.mu.Unlock()
	assert.NotContains(t, rec.outcomes, "wedged",
		"a validate killed on the way down says nothing about the config")
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

// A run holds its liveness lock for the whole run body and releases and unlinks
// it on exit. The reap callback fires while the lock is still held (LIFO: reap
// before Release), so a probe there proves the lock covers the run; after
// TryRunGroup returns the lock must be acquirable and the file gone.
func TestRunHoldsAndReleasesLivenessLock(t *testing.T) {
	fake := newFakeExecutor()
	r := newTestRunner(t, fake, nil)
	lockDir := t.TempDir()
	r.SetLockDir(lockDir)

	lockPath := PendingLockPath(lockDir, "run-live")
	var heldDuringRun bool
	h := &helperReapHarness{}
	reap := func(ctx context.Context, runID string) ([]string, error) {
		// Still inside the run: the owner should hold the lock, so a probe fails.
		if _, acquired, err := lockfile.TryExclusive(lockPath); err == nil {
			heldDuringRun = !acquired
		}
		return h.reap(ctx, runID)
	}
	r.SetHelperReaper(h, reap)

	ran, err := r.TryRunGroup(context.Background(), "db", config.GroupRunMeta{RunID: "run-live"})
	require.NoError(t, err)
	require.True(t, ran)

	assert.True(t, heldDuringRun, "the liveness lock must be held while the run is in flight")

	// The file is unlinked only by the os.Remove defer, which runs after
	// lock.Release, so its absence proves the lock was both released and
	// unlinked on exit. (Probing with TryExclusive would recreate it via
	// O_CREATE, so assert absence directly instead.)
	assert.NoFileExists(t, lockPath, "the lock must be released and its file unlinked after the run")
}

func mkResult(location, label string, files int64) createResult {
	var r createResult
	r.Repository.Location = location
	r.Repository.Label = label
	r.Archive.Name = "arch"
	r.Archive.Duration = 3
	r.Archive.Stats.NFiles = files
	r.Archive.Stats.OriginalSize = files * 100
	return r
}

func TestPerRepoSuccess(t *testing.T) {
	configured := []config.RepoRef{
		{Path: "/mnt/local", Label: "local"},
		{Path: "ssh://borg@a/./r", Label: "offsite-a"},
	}

	t.Run("every repo ok, results carry stats", func(t *testing.T) {
		results := []createResult{
			mkResult("/mnt/local", "local", 10),
			mkResult("ssh://borg@a/./r", "offsite-a", 20),
		}
		out := withCreate(t).perRepoSuccess(configured, results)
		byID := map[string]state.RepoOutcome{}
		for _, o := range out {
			byID[o.ID] = o
		}
		assert.Equal(t, state.ResultOK, byID["local"].Result)
		assert.EqualValues(t, 20, byID["offsite-a"].Files)
		assert.EqualValues(t, 3, byID["offsite-a"].DurationSeconds)
	})

	t.Run("label match when the reported location differs", func(t *testing.T) {
		results := []createResult{mkResult("/resolved/elsewhere", "local", 5)}
		out := withCreate(t).perRepoSuccess([]config.RepoRef{{Path: "/mnt/local", Label: "local"}}, results)
		require.Len(t, out, 1)
		assert.Equal(t, state.ResultOK, out[0].Result, "matched by label despite the path mismatch")
		assert.EqualValues(t, 5, out[0].Files)
	})

	t.Run("prune-only cycle: ok without a create entry", func(t *testing.T) {
		out := withCreate(t).perRepoSuccess(configured, nil)
		require.Len(t, out, 2)
		for _, o := range out {
			assert.Equal(t, state.ResultOK, o.Result)
			assert.Zero(t, o.Files)
		}
	})

	t.Run("no configured repos yields nil", func(t *testing.T) {
		assert.Nil(t, withCreate(t).perRepoSuccess(nil, []createResult{mkResult("/x", "", 1)}))
	})
}

// probeFake is an execCommand seam for the confirmation probe: a `list` call
// emits the JSON mapped to its --repository (empty/missing means exit 1, an
// unreachable/errored probe).
type probeFake struct {
	mu        sync.Mutex
	listCalls []string
	lastArgs  []string
	out       map[string]string
}

func (p *probeFake) exec(_ context.Context, _ string, args ...string) *exec.Cmd {
	repo, isList := "", false
	for i, a := range args {
		switch {
		case a == "list":
			isList = true
		case a == "--repository" && i+1 < len(args):
			repo = args[i+1]
		}
	}
	if !isList {
		return exec.Command("/bin/sh", "-c", "exit 0")
	}
	p.mu.Lock()
	p.listCalls = append(p.listCalls, repo)
	p.lastArgs = append([]string(nil), args...)
	p.mu.Unlock()
	if j := p.out[repo]; j != "" {
		return exec.Command("/bin/sh", "-c", "cat <<'JSONEOF'\n"+j+"\nJSONEOF")
	}
	return exec.Command("/bin/sh", "-c", "exit 1")
}

func (p *probeFake) probed() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]string, len(p.listCalls))
	copy(out, p.listCalls)
	return out
}

func listJSON(archiveStart time.Time) string {
	return fmt.Sprintf(`[{"archives":[{"start":%q}]}]`, archiveStart.Format("2006-01-02T15:04:05.000000"))
}

func TestPerRepoFailure(t *testing.T) {
	runStart := time.Now()
	fresh := runStart.Add(2 * time.Second) // archive from this cycle
	stale := runStart.Add(-2 * time.Hour)  // an older archive, this cycle produced none
	local := config.RepoRef{Path: "/mnt/local", Label: "local"}
	offA := config.RepoRef{Path: "ssh://borg@a/./r", Label: "offsite-a"}
	offB := config.RepoRef{Path: "ssh://borg@b/./r", Label: "offsite-b"}

	newRunner := func(pf *probeFake) *Runner {
		logger := slog.New(slog.NewTextHandler(io.Discard, nil))
		r := NewRunner(logger, t.TempDir(), "/usr/bin/borgmatic-fake", nil, 0)
		r.execCommand = pf.exec
		return r
	}

	t.Run("repo named in an error is failed without a probe", func(t *testing.T) {
		pf := &probeFake{out: map[string]string{"/mnt/local": listJSON(fresh)}}
		r := newRunner(pf)
		run := &runState{logger: r.logger, group: "g"}
		run.recordErrorText("Repository /mnt/local does not exist.")

		out := r.perRepoFailure(context.Background(), "/cfg.yaml", []config.RepoRef{local}, nil, archiveScope{}, run, runStart)
		require.Len(t, out, 1)
		assert.Equal(t, state.ResultFailed, out[0].Result)
		assert.Empty(t, pf.probed(), "the implicated repo must not be probed")
	})

	t.Run("unimplicated repo with a fresh archive is confirmed ok", func(t *testing.T) {
		pf := &probeFake{out: map[string]string{offA.Path: listJSON(fresh)}}
		r := newRunner(pf)
		run := &runState{logger: r.logger, group: "g"}
		run.recordErrorText("Repository /mnt/local does not exist.")

		out := r.perRepoFailure(context.Background(), "/cfg.yaml", []config.RepoRef{offA}, nil, archiveScope{}, run, runStart)
		require.Len(t, out, 1)
		assert.Equal(t, state.ResultOK, out[0].Result)
		assert.Equal(t, []string{offA.Path}, pf.probed())
	})

	t.Run("unimplicated repo with only a stale archive is left untouched", func(t *testing.T) {
		pf := &probeFake{out: map[string]string{offA.Path: listJSON(stale)}}
		r := newRunner(pf)
		run := &runState{logger: r.logger, group: "g"}

		out := r.perRepoFailure(context.Background(), "/cfg.yaml", []config.RepoRef{offA}, nil, archiveScope{}, run, runStart)
		assert.Nil(t, out, "no fresh archive and not implicated: neither advanced nor failed")
	})

	t.Run("unreachable repo probe error is left untouched", func(t *testing.T) {
		pf := &probeFake{out: map[string]string{}} // probe exits 1
		r := newRunner(pf)
		run := &runState{logger: r.logger, group: "g"}

		out := r.perRepoFailure(context.Background(), "/cfg.yaml", []config.RepoRef{offA}, nil, archiveScope{}, run, runStart)
		assert.Nil(t, out)
	})

	t.Run("mixed fan-out: one failed, one confirmed ok, one omitted", func(t *testing.T) {
		pf := &probeFake{out: map[string]string{
			offA.Path: listJSON(fresh), // healthy, confirmed
			offB.Path: listJSON(stale), // no fresh archive -> omitted
		}}
		r := newRunner(pf)
		run := &runState{logger: r.logger, group: "g"}
		run.recordErrorText("Repository /mnt/local does not exist.") // local failed

		out := r.perRepoFailure(context.Background(), "/cfg.yaml", []config.RepoRef{local, offA, offB}, nil, archiveScope{}, run, runStart)
		byID := map[string]state.RepoOutcome{}
		for _, o := range out {
			byID[o.ID] = o
		}
		assert.Equal(t, state.ResultFailed, byID["local"].Result)
		assert.Equal(t, state.ResultOK, byID["offsite-a"].Result)
		_, hasB := byID["offsite-b"]
		assert.False(t, hasB, "offsite-b is neither implicated nor confirmed: omitted")
		assert.NotContains(t, pf.probed(), local.Path, "implicated repo not probed")
	})

	t.Run("shutdown in progress skips probing entirely", func(t *testing.T) {
		pf := &probeFake{out: map[string]string{offA.Path: listJSON(fresh)}}
		r := newRunner(pf)
		run := &runState{logger: r.logger, group: "g"}
		ctx, cancel := context.WithCancel(context.Background())
		cancel()

		out := r.perRepoFailure(ctx, "/cfg.yaml", []config.RepoRef{offA}, nil, archiveScope{}, run, runStart)
		assert.Nil(t, out)
		assert.Empty(t, pf.probed(), "no borgmatic work is started during shutdown")
	})

	t.Run("no configured repos yields nil", func(t *testing.T) {
		pf := &probeFake{}
		r := newRunner(pf)
		run := &runState{logger: r.logger, group: "g"}
		assert.Nil(t, r.perRepoFailure(context.Background(), "/cfg.yaml", nil, nil, archiveScope{}, run, runStart))
	})
}

func TestContainsPathToken(t *testing.T) {
	assert.True(t, containsPathToken("Repository /mnt/local does not exist.", "/mnt/local"))
	assert.True(t, containsPathToken("lock /mnt/local/lock.exclusive failed", "/mnt/local"), "a child path (the repo's lock file) still references the repo")
	assert.True(t, containsPathToken("borg create /mnt/local::archive-1 ...", "/mnt/local"), "an archive ref (repo::name) still references the repo")
	assert.True(t, containsPathToken("borg create ssh://borg@a/./r ...", "ssh://borg@a/./r"))
	assert.False(t, containsPathToken("Repository /mnt/local2 does not exist.", "/mnt/local"), "must not match a longer sibling name")
	assert.False(t, containsPathToken("Repository /mnt/localish does not exist.", "/mnt/local"), "must not match a name-continuation")
	assert.False(t, containsPathToken("Repository /srv/mnt/local does not exist.", "/mnt/local"), "an absolute path preceded by a name byte is a different path")
	assert.False(t, containsPathToken("nothing here", "/mnt/local"))
}

func TestNewestArchiveAtOrAfter(t *testing.T) {
	runStart := time.Now()
	assert.True(t, newestArchiveAtOrAfter([]byte(listJSON(runStart.Add(time.Second))), runStart))
	// An archive from before the run is not this run's, however recently it was
	// written. The old five-second tolerance credited a failed run with the
	// previous run's archive, which a manual retry or an event-triggered cycle
	// on a small dataset reaches easily.
	assert.False(t, newestArchiveAtOrAfter([]byte(listJSON(runStart.Add(-2*time.Second))), runStart),
		"an archive written before the run started did not come from it")
	assert.False(t, newestArchiveAtOrAfter([]byte(listJSON(runStart.Add(-1500*time.Millisecond))), runStart),
		"nor one from a second and a half earlier")
	// Whole-second borg timestamps must still match a run that started mid-second.
	assert.True(t, newestArchiveAtOrAfter([]byte(listJSON(runStart.Truncate(time.Second))), runStart),
		"an archive stamped in the same second the run began still counts")
	assert.False(t, newestArchiveAtOrAfter([]byte(listJSON(runStart.Add(-time.Hour))), runStart))
	assert.False(t, newestArchiveAtOrAfter([]byte(`[{"archives":[]}]`), runStart), "no archives")
	assert.False(t, newestArchiveAtOrAfter([]byte("not json"), runStart))
	assert.True(t, newestArchiveAtOrAfter([]byte(fmt.Sprintf(`[{"archives":[{"time":%q}]}]`,
		runStart.Add(time.Second).Format("2006-01-02T15:04:05"))), runStart), "falls back to time field and the no-microseconds layout")
}

func (p *probeFake) argsOfLastProbe() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.lastArgs...)
}

// Groups are allowed to share a repository, so the newest archive in one can
// belong to a different group. Without scoping, that group's fresh archive
// confirms this group's failed run as a success, advancing its last-success and
// silencing the alert that should have fired.
func TestProbeIsScopedToThisGroupsArchives(t *testing.T) {
	runStart := time.Now().Add(-time.Hour)
	offsite := config.RepoRef{Path: "ssh://borg@a/./r", Label: "offsite"}

	pf := &probeFake{out: map[string]string{offsite.Path: listJSON(runStart.Add(30 * time.Minute))}}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	r := NewRunner(logger, t.TempDir(), "/usr/bin/borgmatic-fake", nil, 0)
	r.execCommand = pf.exec

	run := &runState{logger: r.logger, group: "g"}
	run.recordErrorText("something else went wrong")

	out := r.perRepoFailure(context.Background(), "/cfg.yaml",
		[]config.RepoRef{offsite}, nil, archiveScope{pattern: "host-g-*"}, run, runStart)
	require.Len(t, out, 1)
	assert.Equal(t, state.ResultOK, out[0].Result)

	args := pf.argsOfLastProbe()
	require.Contains(t, args, "--match-archives", "the probe must ask only for this group's archives")
	for i, a := range args {
		if a == "--match-archives" {
			require.Less(t, i+1, len(args))
			assert.Equal(t, "host-g-*", args[i+1])
		}
	}
}

// And with no pattern it must not pass an empty filter, which borg would read
// as matching nothing.
func TestProbeOmitsTheFilterWhenThereIsNoPattern(t *testing.T) {
	runStart := time.Now().Add(-time.Hour)
	offsite := config.RepoRef{Path: "ssh://borg@a/./r", Label: "offsite"}

	pf := &probeFake{out: map[string]string{offsite.Path: listJSON(runStart.Add(30 * time.Minute))}}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	r := NewRunner(logger, t.TempDir(), "/usr/bin/borgmatic-fake", nil, 0)
	r.execCommand = pf.exec

	run := &runState{logger: r.logger, group: "g"}
	run.recordErrorText("something else went wrong")

	_ = r.perRepoFailure(context.Background(), "/cfg.yaml", []config.RepoRef{offsite}, nil, archiveScope{}, run, runStart)
	assert.NotContains(t, pf.argsOfLastProbe(), "--match-archives")
}

// Every one of this group's repository locks is still held while these probes
// run, and another group sharing one is skipped for the whole time. Serially
// that is probeTimeout per repository, so several unreachable destinations
// could hold them for minutes; concurrently the worst case is one timeout no
// matter how many there are.
func TestProbesRunConcurrentlySoLocksAreNotHeldPerRepository(t *testing.T) {
	runStart := time.Now().Add(-time.Hour)
	repos := []config.RepoRef{
		{Path: "ssh://borg@a/./r", Label: "a"},
		{Path: "ssh://borg@b/./r", Label: "b"},
		{Path: "ssh://borg@c/./r", Label: "c"},
	}

	var mu sync.Mutex
	live, peak := 0, 0
	gate := make(chan struct{})
	counting := func(_ context.Context, _ string, args ...string) *exec.Cmd {
		isList := false
		for _, a := range args {
			if a == "list" {
				isList = true
			}
		}
		if !isList {
			return exec.Command("/bin/sh", "-c", "exit 0")
		}
		mu.Lock()
		live++
		if live > peak {
			peak = live
		}
		reached := live == len(repos)
		mu.Unlock()
		if reached {
			close(gate) // every probe is in flight at once
		}
		<-gate
		mu.Lock()
		live--
		mu.Unlock()
		return exec.Command("/bin/sh", "-c", "exit 1")
	}

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	r := NewRunner(logger, t.TempDir(), "/usr/bin/borgmatic-fake", nil, 0)
	r.execCommand = counting

	run := &runState{logger: r.logger, group: "g"}
	run.recordErrorText("a hook failed before any repository ran")

	done := make(chan struct{})
	go func() {
		_ = r.perRepoFailure(context.Background(), "/cfg.yaml", repos, nil, archiveScope{}, run, runStart)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(20 * time.Second):
		t.Fatal("probes did not all run: serial probing cannot reach the gate")
	}

	mu.Lock()
	defer mu.Unlock()
	assert.Equal(t, len(repos), peak, "each repository must be probed while the others are in flight")
}

// withCreate is a runner whose action set includes create, which is what makes
// a zero exit mean "an archive was written".
func withCreate(t *testing.T) *Runner {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	return NewRunner(logger, t.TempDir(), "/usr/bin/borgmatic-fake", nil, 0)
}

// A maintenance-only cycle exits zero without writing an archive anywhere.
// Recording that as a per-repository success advances every repository's
// last-success and resets its staleness, so a manager configured that way looks
// permanently healthy while never backing anything up.
func TestMaintenanceOnlyRunRecordsNoRepositorySuccess(t *testing.T) {
	configured := []config.RepoRef{
		{Path: "/mnt/local", Label: "local"},
		{Path: "ssh://borg@a/./r", Label: "offsite"},
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	maintenance := NewRunner(logger, t.TempDir(), "/usr/bin/borgmatic-fake",
		[]string{actionPrune, "compact", actionCheck}, 0)
	assert.Nil(t, maintenance.perRepoSuccess(configured, nil),
		"a cycle without create backed nothing up, so no repository succeeded")

	backing := NewRunner(logger, t.TempDir(), "/usr/bin/borgmatic-fake",
		[]string{actionCreate, actionPrune}, 0)
	assert.Len(t, backing.perRepoSuccess(configured, nil), len(configured),
		"a cycle that does create still records each repository, stats or not")
}

// The entry count alone does not bound memory: the scanner accepts lines up to
// a megabyte, so 500 of them is half a gigabyte held for the whole run. A
// backup that fails by drowning the manager in output is not an improvement
// over one that just fails.
func TestRetainedErrorTextIsBoundedByBytesAsWellAsCount(t *testing.T) {
	t.Run("many small messages stop at the entry cap", func(t *testing.T) {
		rs := &runState{}
		for i := range maxErrText + 50 {
			rs.recordErrorText(fmt.Sprintf("error %d", i))
		}
		assert.Len(t, rs.errText, maxErrText)
	})

	t.Run("few huge messages stop at the byte cap", func(t *testing.T) {
		rs := &runState{}
		huge := strings.Repeat("x", 64<<10)
		for range maxErrText {
			rs.recordErrorText(huge)
		}
		assert.Less(t, len(rs.errText), maxErrText,
			"the byte budget must bind first when messages are large")
		assert.LessOrEqual(t, rs.errTextBytes, maxErrTextBytes+len(huge),
			"retention overshoots by at most the message that crossed the line")
	})

	t.Run("messages recorded before the cap are still matchable", func(t *testing.T) {
		rs := &runState{}
		rs.recordErrorText("  Error while creating archive /mnt/repo1  ")
		for range 100 {
			rs.recordErrorText(strings.Repeat("y", 64<<10))
		}
		require.NotEmpty(t, rs.errText)
		assert.Equal(t, "Error while creating archive /mnt/repo1", rs.errText[0],
			"the cap must drop late noise, not the messages already kept")
		assert.True(t, mentionedInErrors("/mnt/repo1", rs.errText))
	})
}

// borgmatic reports the repository location borg resolved, which is not always
// the string the config held. When the pairing misses, the run is still recorded
// as successful but carries no stats at all, so the repository's size, file
// count and duration never appear for a backup that in fact completed.
func TestResultsMatchRepositoriesBorgReportsDifferently(t *testing.T) {
	res := func(loc, label string, files int64) createResult {
		var r createResult
		r.Repository.Location = loc
		r.Repository.Label = label
		r.Archive.Stats.NFiles = files
		return r
	}

	t.Run("a runtime-expanded path pairs with the only result", func(t *testing.T) {
		matched := matchResults(
			[]config.RepoRef{{Path: "${BORG_REPO}"}},
			[]createResult{res("/srv/borg/repo", "", 42)},
		)
		require.Contains(t, matched, 0, "an unmatched lone repository takes the lone result")
		assert.Equal(t, int64(42), matched[0].Archive.Stats.NFiles)
	})

	// Both repositories need normalizing and both results are unclaimed, so the
	// single-leftover rule cannot rescue this: the pairing has to come from
	// canonical identity, and it has to survive the results arriving in the
	// opposite order.
	t.Run("normalized paths pair by canonical identity, not by position", func(t *testing.T) {
		dir := t.TempDir()
		matched := matchResults(
			[]config.RepoRef{{Path: dir + "/repo/"}, {Path: dir + "/other/"}},
			[]createResult{res(dir+"/other", "", 7), res(dir+"/repo", "", 99)},
		)
		require.Contains(t, matched, 0)
		require.Contains(t, matched, 1)
		assert.Equal(t, int64(99), matched[0].Archive.Stats.NFiles, "a trailing slash is not an identity")
		assert.Equal(t, int64(7), matched[1].Archive.Stats.NFiles)
	})

	t.Run("an exact match is never stolen by a later repository", func(t *testing.T) {
		matched := matchResults(
			[]config.RepoRef{{Path: "/a"}, {Path: "${BORG_REPO}"}},
			[]createResult{res("/a", "", 1), res("/resolved", "", 2)},
		)
		assert.Equal(t, int64(1), matched[0].Archive.Stats.NFiles)
		assert.Equal(t, int64(2), matched[1].Archive.Stats.NFiles,
			"the leftover pairing takes what the exact match did not claim")
	})

	// Two labelled repositories whose paths are both unresolvable: the label is
	// the only usable identity, and the single-leftover rule cannot stand in for
	// it because neither side has a lone candidate.
	t.Run("labels identify repositories whose paths cannot", func(t *testing.T) {
		matched := matchResults(
			[]config.RepoRef{{Path: "${BORG_REPO}", Label: "local"}, {Path: "${BORG_REPO_2}", Label: "offsite"}},
			[]createResult{res("/resolved-b", "offsite", 5), res("/resolved-a", "local", 8)},
		)
		require.Contains(t, matched, 0)
		require.Contains(t, matched, 1)
		assert.Equal(t, int64(8), matched[0].Archive.Stats.NFiles)
		assert.Equal(t, int64(5), matched[1].Archive.Stats.NFiles)
	})

	// The exact pass is what makes a reported location authoritative. Canonical
	// identity cannot stand in for it here: the location borgmatic echoed back is
	// one it never expanded, so it canonicalizes to "unknown" and the canonical
	// pass has to skip it. Reported verbatim is still reported.
	t.Run("a location reported verbatim is matched even when it cannot be canonicalized", func(t *testing.T) {
		matched := matchResults(
			[]config.RepoRef{{Path: "${BORG_REPO}"}, {Path: "${OTHER_REPO}"}, {Path: "/plain"}},
			[]createResult{res("${BORG_REPO}", "", 7), res("/elsewhere", "", 8), res("/plain", "", 9)},
		)
		require.Contains(t, matched, 0)
		assert.Equal(t, int64(7), matched[0].Archive.Stats.NFiles, "the literal location still identifies it")
		assert.Equal(t, int64(8), matched[1].Archive.Stats.NFiles, "leaving one unambiguous leftover pair")
		assert.Equal(t, int64(9), matched[2].Archive.Stats.NFiles)
	})

	// "unknown" is what CanonicalRepoKey returns for a path it cannot resolve.
	// It says "no identity", not "this identity", so two repositories that both
	// canonicalize to it are not the same repository. If borgmatic ever reports a
	// location it did not expand, treating that key as a match cross-attributes
	// one destination's measurements to another.
	t.Run("an unresolvable path is not an identity two repositories can share", func(t *testing.T) {
		matched := matchResults(
			[]config.RepoRef{{Path: "${BORG_REPO}"}, {Path: "${BORG_REPO_2}"}},
			[]createResult{res("${SOMETHING_ELSE}", "", 1), res("${AND_ANOTHER}", "", 2)},
		)
		assert.Empty(t, matched)
	})

	t.Run("two ambiguous repositories are left unmatched", func(t *testing.T) {
		matched := matchResults(
			[]config.RepoRef{{Path: "${BORG_REPO}"}, {Path: "${BORG_REPO_2}"}},
			[]createResult{res("/one", "", 1), res("/two", "", 2)},
		)
		assert.Empty(t, matched,
			"guessing between two runtime-expanded paths would attribute one destination's "+
				"measurements to another, which is worse than reporting none")
	})

	// The asymmetric case the two-and-two shape cannot detect: one result and
	// two candidates for it. Handing it to whichever repository came first is a
	// coin flip, and half the time it credits a backup to the wrong destination.
	t.Run("one result and two candidates is still ambiguous", func(t *testing.T) {
		matched := matchResults(
			[]config.RepoRef{{Path: "${BORG_REPO}"}, {Path: "${BORG_REPO_2}"}},
			[]createResult{res("/one", "", 1)},
		)
		assert.Empty(t, matched, "a lone result must not be claimed by one of two candidates")
	})

	// A result carries one repository's measurements, so it can be spent once.
	// A config that lists the same repository twice must not have its stats
	// counted against both entries.
	t.Run("a result is claimed by at most one repository", func(t *testing.T) {
		matched := matchResults(
			[]config.RepoRef{{Path: "/a"}, {Path: "/a"}},
			[]createResult{res("/a", "", 3)},
		)
		assert.Len(t, matched, 1, "one result cannot satisfy two repositories")
		assert.Equal(t, int64(3), matched[0].Archive.Stats.NFiles)
	})
}

// The end-to-end shape of the same bug: a successful run over a repository
// borgmatic reports under its resolved path must still carry its measurements
// into state, or LastStats stays empty after every successful backup.
func TestSuccessCarriesStatsForARuntimeExpandedRepository(t *testing.T) {
	var res createResult
	res.Repository.Location = "/srv/borg/resolved"
	res.Archive.Name = "demo-2026-07-27"
	res.Archive.Stats.NFiles = 1234
	res.Archive.Stats.OriginalSize = 5000
	res.Archive.Duration = 12.5

	r := NewRunner(slog.New(slog.NewTextHandler(io.Discard, nil)), t.TempDir(),
		"/usr/bin/borgmatic-fake", []string{actionCreate}, 0)
	out := r.perRepoSuccess([]config.RepoRef{{Path: "${BORG_REPO}"}}, []createResult{res})

	require.Len(t, out, 1)
	assert.Equal(t, state.ResultOK, out[0].Result)
	assert.Equal(t, int64(1234), out[0].Files, "the stats must survive the resolved-path mismatch")
	assert.Equal(t, int64(5000), out[0].OriginalBytes)
}

// create succeeds everywhere and a later action (prune, compact, check) exits
// nonzero. borgmatic has already reported each archive and its measurements, so
// rebuilding every repository from confirmation probes throws away numbers that
// were in hand: the probe can only answer "an archive exists", which records ok
// with no stats, leaving the size, file count and duration stale or, after a
// first backup, absent entirely.
func TestALaterActionFailureKeepsTheCreateMeasurements(t *testing.T) {
	runStart := time.Now().Add(-time.Minute)
	fresh := runStart.Add(10 * time.Second)
	local := config.RepoRef{Path: "/mnt/local", Label: "local"}
	offsite := config.RepoRef{Path: "ssh://borg@a/./r", Label: "offsite"}

	result := func(loc string, files, size int64) createResult {
		var r createResult
		r.Repository.Location = loc
		r.Archive.Stats.NFiles = files
		r.Archive.Stats.OriginalSize = size
		r.Archive.Duration = 42
		return r
	}

	newRunner := func(pf *probeFake) *Runner {
		r := NewRunner(slog.New(slog.NewTextHandler(io.Discard, nil)), t.TempDir(),
			"/usr/bin/borgmatic-fake", nil, 0)
		r.execCommand = pf.exec
		return r
	}

	t.Run("measured repositories keep their stats and are not probed", func(t *testing.T) {
		pf := &probeFake{out: map[string]string{local.Path: listJSON(fresh), offsite.Path: listJSON(fresh)}}
		r := newRunner(pf)
		run := &runState{logger: r.logger, group: "g"}
		run.recordErrorText("Command error: borg prune exited with code 2")

		out := r.perRepoFailure(context.Background(), "/cfg.yaml",
			[]config.RepoRef{local, offsite},
			[]createResult{result(local.Path, 1234, 5000), result(offsite.Path, 7, 90)},
			archiveScope{}, run, runStart)

		require.Len(t, out, 2)
		assert.Equal(t, state.ResultOK, out[0].Result)
		assert.Equal(t, int64(1234), out[0].Files, "the reported measurements must survive the run failing")
		assert.Equal(t, int64(5000), out[0].OriginalBytes)
		assert.Equal(t, int64(42), out[0].DurationSeconds)
		assert.Equal(t, int64(7), out[1].Files)
		assert.Empty(t, pf.probed(), "a repository borgmatic already measured needs no probe")
	})

	// Error matching is a substring test against the message text, and a prune
	// failure names the same path the successful create just reported. Direct
	// evidence of the archive has to win over that inference, or the destination
	// that did back up is recorded as failed.
	t.Run("a create result outweighs an error naming the same path", func(t *testing.T) {
		pf := &probeFake{}
		r := newRunner(pf)
		run := &runState{logger: r.logger, group: "g"}
		run.recordErrorText("Error running prune for /mnt/local")

		out := r.perRepoFailure(context.Background(), "/cfg.yaml",
			[]config.RepoRef{local}, []createResult{result(local.Path, 5, 60)}, archiveScope{}, run, runStart)

		require.Len(t, out, 1)
		assert.Equal(t, state.ResultOK, out[0].Result)
		assert.Equal(t, int64(5), out[0].Files)
	})

	// The mixed case: one destination measured, one not. The unmeasured one must
	// still go through the existing implicate-or-probe path.
	t.Run("unmeasured repositories are still probed", func(t *testing.T) {
		pf := &probeFake{out: map[string]string{offsite.Path: listJSON(fresh)}}
		r := newRunner(pf)
		run := &runState{logger: r.logger, group: "g"}

		out := r.perRepoFailure(context.Background(), "/cfg.yaml",
			[]config.RepoRef{local, offsite},
			[]createResult{result(local.Path, 3, 40)},
			archiveScope{}, run, runStart)

		require.Len(t, out, 2)
		assert.Equal(t, int64(3), out[0].Files, "the measured one keeps its stats")
		assert.Equal(t, state.ResultOK, out[1].Result, "the unmeasured one is confirmed by probe")
		assert.Zero(t, out[1].Files, "a probe proves an archive exists, not how big it is")
		assert.Equal(t, []string{offsite.Path}, pf.probed(), "and only it is probed")
	})
}

// borg names the location it resolved, not the string the config held, so a
// failure attribution that compares the literal spelling misses. The
// consequence is the worst of the three: the destination borg said failed is
// dropped from persisted state entirely, and in a mixed fan-out is counted as
// unknown rather than failed.
func TestFailureAttributionRecognizesNormalizedRepositoryPaths(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.MkdirAll(dir+"/repo", 0o755))

	for _, tc := range []struct {
		name       string
		configured string
		message    string
		want       bool
	}{
		{name: "as configured", configured: "/mnt/repo",
			message: "Error: Repository /mnt/repo does not exist.", want: true},
		{name: "trailing separator trimmed by borg", configured: "/mnt/repo/",
			message: "Error: Repository /mnt/repo does not exist.", want: true},
		// The other direction needs no normalization: the token-boundary check
		// already treats a separator as a delimiter rather than part of the name.
		{name: "reported with the separator the config omitted", configured: "/mnt/repo",
			message: "Error: Repository /mnt/repo/ does not exist.", want: true},
		{name: "relative path resolved", configured: dir + "/./repo",
			message: "Error: Repository " + dir + "/repo is locked.", want: true},
		{name: "a different repository is not implicated", configured: "/mnt/repo",
			message: "Error: Repository /mnt/other does not exist.", want: false},
		{name: "a longer path is not a prefix match", configured: "/mnt/repo",
			message: "Error: Repository /mnt/repository does not exist.", want: false},
		{name: "an unresolvable path matches only its literal form", configured: "${BORG_REPO}",
			message: "Error: Repository ${BORG_REPO} does not exist.", want: true},
		{name: "and does not match some other unresolvable path", configured: "${BORG_REPO}",
			message: "Error: Repository ${OTHER_REPO} does not exist.", want: false},
		// CanonicalRepoKey returns the literal string "unknown" for a path it
		// cannot resolve. Letting that into the spellings would make any message
		// containing "unknown" as a path token implicate every unresolvable
		// repository in the group.
		{name: "the unresolvable-path key is not a spelling to match on", configured: "${BORG_REPO}",
			message: "Error: Repository /mnt/unknown does not exist.", want: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, mentionedInErrors(tc.configured, []string{tc.message}))
		})
	}
}

// The end-to-end shape: a destination borg names under its normalized spelling
// must be recorded as failed, not silently omitted.
func TestANormalizedFailureIsRecordedAgainstTheRightRepository(t *testing.T) {
	runStart := time.Now().Add(-time.Minute)
	offsite := config.RepoRef{Path: "/mnt/offsite/", Label: "offsite"}
	pf := &probeFake{}
	r := NewRunner(slog.New(slog.NewTextHandler(io.Discard, nil)), t.TempDir(),
		"/usr/bin/borgmatic-fake", nil, 0)
	r.execCommand = pf.exec
	run := &runState{logger: r.logger, group: "g"}
	run.recordErrorText("Error: Repository /mnt/offsite does not exist.")

	out := r.perRepoFailure(context.Background(), "/cfg.yaml",
		[]config.RepoRef{offsite}, nil, archiveScope{}, run, runStart)

	require.Len(t, out, 1, "the implicated destination must be recorded, not dropped")
	assert.Equal(t, state.ResultFailed, out[0].Result)
	assert.Empty(t, pf.probed(), "and it must not be probed")
}

// CreateAttempted is what stops a maintenance-only cycle being counted as a
// backup per destination, and it can only be answered by the runner: state and
// metrics see the outcome, not the actions. Measured is the same shape of
// question one level down, distinguishing "borgmatic reported this archive"
// from "a probe found one", which an empty archive cannot answer by its numbers.
func TestRecordedOutcomesCarryTheCreateAndMeasurementSignals(t *testing.T) {
	t.Run("a backup run is marked attempted and its stats measured", func(t *testing.T) {
		fake := newFakeExecutor()
		fake.runScript = `echo '[{"repository":{"location":"/repo","label":"local"},"archive":{"name":"a","stats":{"nfiles":0,"original_size":0}}}]'; exit 0`
		r := NewRunner(slog.New(slog.NewTextHandler(io.Discard, nil)), t.TempDir(),
			"/usr/bin/borgmatic-fake", []string{actionCreate}, 0)
		r.execCommand = fake.exec
		rec := &recordingStore{}
		r.SetRecorder(rec)

		_, err := r.TryRunGroup(context.Background(), "files", config.GroupRunMeta{
			Repositories: []config.RepoRef{{Path: "/repo", Label: "local"}},
		})
		require.NoError(t, err)

		rec.mu.Lock()
		defer rec.mu.Unlock()
		o := rec.outcomes["files"]
		assert.True(t, o.CreateAttempted, "the run wrote an archive")
		require.Len(t, o.Repositories, 1)
		assert.True(t, o.Repositories[0].Measured,
			"an all-zero archive borgmatic reported is measured, and only this flag says so")
	})

	t.Run("a maintenance-only cycle is marked as not attempting a backup", func(t *testing.T) {
		fake := newFakeExecutor()
		r := NewRunner(slog.New(slog.NewTextHandler(io.Discard, nil)), t.TempDir(),
			"/usr/bin/borgmatic-fake", []string{actionPrune, actionCheck}, 0)
		r.execCommand = fake.exec
		rec := &recordingStore{}
		r.SetRecorder(rec)

		_, err := r.TryRunGroup(context.Background(), "files", config.GroupRunMeta{
			Repositories: []config.RepoRef{{Path: "/repo", Label: "local"}},
		})
		require.NoError(t, err)

		rec.mu.Lock()
		defer rec.mu.Unlock()
		o := rec.outcomes["files"]
		assert.False(t, o.CreateAttempted, "prune and check back nothing up")
		assert.Empty(t, o.Repositories)
	})

}

// A config that fails schema validation still names its destinations: the
// manager wrote it. Recording the failure without them counts the run once under
// an empty repository, dropping it out of every repository-filtered dashboard,
// and leaves the inventory unable to reconcile a destination added or removed in
// the same edit that broke the config.
func TestAValidationFailureIsAttributedToTheConfiguredRepositories(t *testing.T) {
	fake := newFakeExecutor()
	fake.validateScript = "echo 'schema error' >&2; exit 1"
	r := newTestRunner(t, fake, nil)
	rec := &recordingStore{}
	r.SetRecorder(rec)

	_, err := r.TryRunGroup(context.Background(), "badgroup", config.GroupRunMeta{
		Repositories: []config.RepoRef{
			{Path: "/mnt/local", Label: "local"},
			{Path: "ssh://borg@a/./r"},
		},
	})
	require.Error(t, err)

	rec.mu.Lock()
	defer rec.mu.Unlock()
	o := rec.outcomes["badgroup"]
	assert.Equal(t, "config-invalid", o.Result)
	assert.Equal(t, []string{"local", "ssh://borg@a/./r"}, o.ConfiguredRepositories,
		"the destinations are known even when the config is not valid")
	assert.False(t, o.CreateAttempted, "nothing ran, so nothing was backed up")
}

// Groups named "app" and "app-prod" sharing a repository have overlapping
// archive patterns: "*-app-*" matches "*-app-prod-*" too. Generation warns and
// keeps both, so the probe must not treat "an archive matching my pattern
// exists" as "my backup reached this repository": a sibling's archive written in
// the same whole second would confirm a success this group never had.
func TestAnOverlappingArchivePatternCannotConfirmSuccess(t *testing.T) {
	runStart := time.Now().Add(-time.Minute)
	fresh := runStart.Add(10 * time.Second)
	offsite := config.RepoRef{Path: "/mnt/offsite", Label: "offsite"}

	newRunner := func(pf *probeFake) *Runner {
		r := NewRunner(slog.New(slog.NewTextHandler(io.Discard, nil)), t.TempDir(),
			"/usr/bin/borgmatic-fake", nil, 0)
		r.execCommand = pf.exec
		return r
	}

	t.Run("an ambiguous pattern leaves the destination unjudged", func(t *testing.T) {
		pf := &probeFake{out: map[string]string{offsite.Path: listJSON(fresh)}}
		r := newRunner(pf)
		run := &runState{logger: r.logger, group: "app"}

		out := r.perRepoFailure(context.Background(), "/cfg.yaml", []config.RepoRef{offsite}, nil,
			archiveScope{pattern: "*-app-*", ambiguous: true}, run, runStart)

		assert.Nil(t, out, "a probe that cannot tell the groups apart confirms nothing")
		assert.Empty(t, pf.probed(), "and is not worth running")
	})

	t.Run("the same pattern confirms when no sibling shares the repository", func(t *testing.T) {
		pf := &probeFake{out: map[string]string{offsite.Path: listJSON(fresh)}}
		r := newRunner(pf)
		run := &runState{logger: r.logger, group: "app"}

		out := r.perRepoFailure(context.Background(), "/cfg.yaml", []config.RepoRef{offsite}, nil,
			archiveScope{pattern: "*-app-*"}, run, runStart)

		require.Len(t, out, 1)
		assert.Equal(t, state.ResultOK, out[0].Result)
	})

	// A destination borgmatic measured needs no probe, so ambiguity cannot cost
	// it its stats.
	t.Run("a measured destination is unaffected", func(t *testing.T) {
		var res createResult
		res.Repository.Location = offsite.Path
		res.Archive.Stats.NFiles = 11
		pf := &probeFake{}
		r := newRunner(pf)
		run := &runState{logger: r.logger, group: "app"}

		out := r.perRepoFailure(context.Background(), "/cfg.yaml", []config.RepoRef{offsite},
			[]createResult{res}, archiveScope{pattern: "*-app-*", ambiguous: true}, run, runStart)

		require.Len(t, out, 1)
		assert.Equal(t, state.ResultOK, out[0].Result)
		assert.Equal(t, int64(11), out[0].Files)
	})
}
