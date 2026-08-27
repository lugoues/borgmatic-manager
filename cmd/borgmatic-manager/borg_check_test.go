package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lugoues/borgmatic-manager/internal/config"
)

// fakeBorg writes an executable that reports the given version.
func fakeBorg(t *testing.T, dir, version string) string {
	t.Helper()
	p := filepath.Join(dir, "borg")
	require.NoError(t, os.WriteFile(p, []byte("#!/bin/sh\necho borg "+version+"\n"), 0o755))
	return p
}

func TestBorgCommandsListsEveryConfiguredBorg(t *testing.T) {
	t.Run("no configuration means the PATH default", func(t *testing.T) {
		cmds := borgCommands(&config.ManagerConfig{}, nil)
		require.Len(t, cmds, 1)
		assert.Equal(t, "borg", cmds[0].command)
	})

	t.Run("a global local_path replaces the default", func(t *testing.T) {
		cfg := &config.ManagerConfig{Borgmatic: map[string]interface{}{"local_path": "/opt/borg2/borg"}}
		cmds := borgCommands(cfg, nil)
		require.Len(t, cmds, 1)
		assert.Equal(t, "/opt/borg2/borg", cmds[0].command)
	})

	t.Run("a group override adds its borg beside the default", func(t *testing.T) {
		overrides := map[string]config.GroupOverride{
			"special": {Borgmatic: map[string]interface{}{"local_path": "/opt/borg2/borg"}},
			"plain":   {Borgmatic: map[string]interface{}{"keep_daily": 7}},
		}
		cmds := borgCommands(&config.ManagerConfig{}, overrides)
		require.Len(t, cmds, 2, "groups without an override still need the default borg")
		assert.Equal(t, "borg", cmds[0].command)
		assert.Equal(t, "/opt/borg2/borg", cmds[1].command)
	})
}

// A valid configuration pointing borgmatic at its own borg (local_path) must
// not be failed for lacking a separate "borg" on PATH.
func TestCheckBorgHonorsLocalPath(t *testing.T) {
	t.Run("an absolute local_path outside PATH passes", func(t *testing.T) {
		t.Setenv("PATH", t.TempDir()) // empty: no "borg" anywhere on PATH
		lp := fakeBorg(t, t.TempDir(), "1.4.5")
		cfg := &config.ManagerConfig{Borgmatic: map[string]interface{}{"local_path": lp}}
		assert.NoError(t, checkBorg(context.Background(), cfg, nil, false))
	})

	t.Run("a missing local_path names its source", func(t *testing.T) {
		cfg := &config.ManagerConfig{Borgmatic: map[string]interface{}{"local_path": "/nonexistent/borg"}}
		err := checkBorg(context.Background(), cfg, nil, false)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "manager.yaml local_path")
	})

	t.Run("a group override is checked and the default still required", func(t *testing.T) {
		t.Setenv("PATH", t.TempDir()) // no default borg
		lp := fakeBorg(t, t.TempDir(), "1.4.5")
		overrides := map[string]config.GroupOverride{
			"g": {Borgmatic: map[string]interface{}{"local_path": lp}},
		}
		err := checkBorg(context.Background(), &config.ManagerConfig{}, overrides, false)
		require.Error(t, err, "groups without the override still invoke the default borg")
		assert.Contains(t, err.Error(), "PATH")
	})

	t.Run("missing default borg fails outright", func(t *testing.T) {
		t.Setenv("PATH", t.TempDir())
		err := checkBorg(context.Background(), &config.ManagerConfig{}, nil, false)
		require.Error(t, err)
	})

	t.Run("old borg is fatal only with snapshot hooks", func(t *testing.T) {
		dir := t.TempDir()
		fakeBorg(t, dir, "1.2.0")
		t.Setenv("PATH", dir)
		require.NoError(t, checkBorg(context.Background(), &config.ManagerConfig{}, nil, false))
		require.Error(t, checkBorg(context.Background(), &config.ManagerConfig{}, nil, true))
	})
}
