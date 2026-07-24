package main

import (
	"log/slog"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
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
	assert.False(t, h.Enabled(nil, slog.LevelInfo))
	assert.True(t, h.Enabled(nil, slog.LevelWarn))
	assert.True(t, h.Enabled(nil, slog.LevelError))
}
