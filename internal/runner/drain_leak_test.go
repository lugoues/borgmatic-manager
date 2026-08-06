package runner

import (
	"context"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lugoues/borgmatic-manager/internal/config"
)

// A hook that backgrounds a process leaves it holding borgmatic's inherited
// stdout/stderr after borgmatic itself exits. Closing the pipe readers alone
// would unwedge the run but release the group and repository locks with that
// process still touching the volume or repository; the run must kill its
// process group before it is treated as finished.
func TestALeakedHookChildIsKilledBeforeTheRunFinishes(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "leaked.pid")

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	r := NewRunner(logger, t.TempDir(), "/bin/sh", nil, 0)
	r.drainTimeout = 100 * time.Millisecond
	r.killGrace = 2 * time.Second
	r.execCommand = func(_ context.Context, _ string, args ...string) *exec.Cmd {
		for _, a := range args {
			if a == "validate" {
				return exec.Command("true")
			}
		}
		// Stand in for borgmatic running a hook that backgrounds a child: the
		// child inherits stdout, so the pipes stay open after the parent
		// exits. 60s is only a ceiling; the test kills it long before.
		return exec.Command("sh", "-c", "sleep 60 & echo $! > "+pidFile)
	}

	start := time.Now()
	ran, _, err := r.TryRunGroup(context.Background(), "g", config.GroupRunMeta{})
	require.NoError(t, err)
	require.True(t, ran)
	assert.Less(t, time.Since(start), 30*time.Second,
		"the drain bound plus one kill grace, not the leaked child's lifetime")

	raw, readErr := os.ReadFile(pidFile)
	require.NoError(t, readErr, "the fake hook recorded its background child")
	pid, convErr := strconv.Atoi(strings.TrimSpace(string(raw)))
	require.NoError(t, convErr)

	// SIGTERM delivery is asynchronous; give it a moment, then require the
	// child gone. Signal 0 probes existence without sending anything.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if syscall.Kill(pid, 0) != nil {
			return // dead, as required
		}
		time.Sleep(20 * time.Millisecond)
	}
	_ = syscall.Kill(pid, syscall.SIGKILL) // do not leak it from the test either
	t.Fatal("the leaked child outlived the run: the locks were released while it could still write")
}
