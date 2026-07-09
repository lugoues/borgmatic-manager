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
		"status":   func() { printStatus(bs, store, time.Hour, nil) },
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
