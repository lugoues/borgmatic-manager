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
	assert.Contains(t, run, "create prune compact check", "default actions apply retention, not just create")
}

func TestTryRunGroup_CustomActions(t *testing.T) {
	fake := newFakeExecutor()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	r := NewRunner(logger, t.TempDir(), "/usr/bin/borgmatic-fake", []string{"create"}, 0)
	r.execCommand = fake.exec

	_, err := r.TryRunGroup(context.Background(), "g", config.GroupRunMeta{})
	require.NoError(t, err)

	run := strings.Join(fake.callArgs()[1], " ")
	assert.True(t, strings.HasSuffix(run, "--log-json create"), "custom actions replace defaults: %s", run)
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
