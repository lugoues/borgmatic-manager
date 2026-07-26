package main

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Discovery warnings and errors must land in the report as labeled lines,
// with attrs appended, and flag that a specific label problem was shown.
func TestWarnCapturingLogger(t *testing.T) {
	r := &doctorReport{}
	logger := warnCapturingLogger(r)

	out := captureStdout(t, func() {
		logger.Debug("dropped")
		logger.Info("dropped too")
		logger.Warn("volume skipped", "volume", "nfs-vol", "driver", "nfs")
		logger.With("container", "web").Error("bad label")
	})

	assert.Equal(t, 1, r.warned)
	assert.Equal(t, 1, r.failed)
	assert.True(t, r.sawLabelWarning)
	assert.NotContains(t, out, "dropped")
	assert.Contains(t, out, "volume skipped volume=nfs-vol driver=nfs")
	assert.Contains(t, out, "bad label container=web")

	lines := strings.Split(strings.TrimSpace(out), "\n")
	assert.Len(t, lines, 2)
	assert.True(t, strings.HasPrefix(lines[0], "warn"), "warn line prefix")
	assert.True(t, strings.HasPrefix(lines[1], "FAIL"), "error becomes FAIL")
}

// The handler's Enabled gate keeps sub-warn records from ever reaching Handle.
func TestReportHandlerLevelGate(t *testing.T) {
	h := reportHandler{r: &doctorReport{}}
	ctx := context.Background()
	assert.False(t, h.Enabled(ctx, slog.LevelInfo))
	assert.True(t, h.Enabled(ctx, slog.LevelWarn))
	assert.True(t, h.Enabled(ctx, slog.LevelError))
}

// doctor exists to diagnose a broken setup, and "borgmatic is not installed" is
// one of the setups it must still be useful in. Label and config-shape problems
// are the manager's own to find, so generation runs regardless; only borgmatic's
// schema check is skipped, and it says so rather than going quiet.
func TestValidateGeneratedConfigsSkipsLoudlyWithoutBorgmatic(t *testing.T) {
	r := &doctorReport{}
	files := []string{"/tmp/alpha.yaml", "/tmp/beta.yaml"}

	out := captureStdout(t, func() { r.validateGeneratedConfigs(context.Background(), files, "") })

	assert.Contains(t, out, "skipped")
	assert.Contains(t, out, "borgmatic is not installed")
	assert.Contains(t, out, "still compiled", "the operator is told what did get checked")
	assert.Equal(t, 1, r.warned, "a skipped check is a warning, not a silent pass")
	assert.Equal(t, 0, r.failed, "a missing binary is not a config failure")
	assert.NotContains(t, out, "validate:alpha", "no per-group verdict can be claimed without borgmatic")
}

func TestValidateGeneratedConfigsReportsPerGroupVerdicts(t *testing.T) {
	dir := t.TempDir()
	ok := filepath.Join(dir, "yes")
	require.NoError(t, os.WriteFile(ok, []byte("#!/bin/sh\nexit 0\n"), 0o700))
	bad := filepath.Join(dir, "no")
	require.NoError(t, os.WriteFile(bad, []byte("#!/bin/sh\necho 'schema error: bad key'\nexit 1\n"), 0o700))

	r := &doctorReport{}
	out := captureStdout(t, func() {
		r.validateGeneratedConfigs(context.Background(), []string{filepath.Join(dir, "alpha.yaml")}, ok)
	})
	assert.Contains(t, out, "validate:alpha")
	assert.Equal(t, 0, r.failed)

	r2 := &doctorReport{}
	out2 := captureStdout(t, func() {
		r2.validateGeneratedConfigs(context.Background(), []string{filepath.Join(dir, "beta.yaml")}, bad)
	})
	assert.Contains(t, out2, "validate:beta")
	assert.Contains(t, out2, "schema error: bad key", "borgmatic's own message is surfaced")
	assert.Equal(t, 1, r2.failed)
}
