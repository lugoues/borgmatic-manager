package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lugoues/borgmatic-manager/internal/config"
	"github.com/lugoues/borgmatic-manager/internal/models"
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
		cmds := borgCommands(&config.ManagerConfig{}, nil, nil)
		require.Len(t, cmds, 1)
		assert.Equal(t, "borg", cmds[0].command)
	})

	t.Run("a global local_path replaces the default", func(t *testing.T) {
		cfg := &config.ManagerConfig{Borgmatic: map[string]interface{}{"local_path": "/opt/borg2/borg"}}
		cmds := borgCommands(cfg, nil, nil)
		require.Len(t, cmds, 1)
		assert.Equal(t, "/opt/borg2/borg", cmds[0].command)
	})

	t.Run("group-specific borgs are not listed; generation judges them per group", func(t *testing.T) {
		overrides := map[string]config.GroupOverride{
			"special": {Borgmatic: map[string]interface{}{"local_path": "/opt/borg2/borg"}},
			"plain":   {Borgmatic: map[string]interface{}{"keep_daily": 7}},
		}
		cmds := borgCommands(&config.ManagerConfig{}, overrides, nil)
		require.Len(t, cmds, 1, "only the default: a group's own borg failing must refuse that group, not the launch")
		assert.Equal(t, "borg", cmds[0].command)
	})
}

// A valid configuration pointing borgmatic at its own borg (local_path) must
// not be failed for lacking a separate "borg" on PATH.
func TestCheckBorgHonorsLocalPath(t *testing.T) {
	t.Run("an absolute local_path outside PATH passes", func(t *testing.T) {
		t.Setenv("PATH", t.TempDir()) // empty: no "borg" anywhere on PATH
		lp := fakeBorg(t, t.TempDir(), "1.4.5")
		cfg := &config.ManagerConfig{Borgmatic: map[string]interface{}{"local_path": lp}}
		assert.NoError(t, checkBorg(context.Background(), cfg, nil, nil))
	})

	t.Run("a missing local_path names its source", func(t *testing.T) {
		cfg := &config.ManagerConfig{Borgmatic: map[string]interface{}{"local_path": "/nonexistent/borg"}}
		err := checkBorg(context.Background(), cfg, nil, nil)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "manager.yaml local_path")
	})

	t.Run("the default is still required with no discovered state", func(t *testing.T) {
		t.Setenv("PATH", t.TempDir()) // no default borg
		lp := fakeBorg(t, t.TempDir(), "1.4.5")
		overrides := map[string]config.GroupOverride{
			"g": {Borgmatic: map[string]interface{}{"local_path": lp}},
		}
		err := checkBorg(context.Background(), &config.ManagerConfig{}, overrides, nil)
		require.Error(t, err, "no state means the default must be assumed needed")
		assert.Contains(t, err.Error(), "PATH")
	})

	t.Run("a borg that exists but cannot run fails, naming its source", func(t *testing.T) {
		dir := t.TempDir()
		require.NoError(t, os.WriteFile(filepath.Join(dir, "borg"),
			[]byte("#!/bin/sh\necho 'error while loading shared libraries' >&2\nexit 127\n"), 0o755))
		t.Setenv("PATH", dir)
		err := checkBorg(context.Background(), &config.ManagerConfig{}, nil, nil)
		require.Error(t, err, "an unrunnable borg fails every group; the daemon must not start on it")
		assert.Contains(t, err.Error(), "--version failed")
	})

	t.Run("a borg reporting nothing fails too", func(t *testing.T) {
		dir := t.TempDir()
		require.NoError(t, os.WriteFile(filepath.Join(dir, "borg"), []byte("#!/bin/sh\nexit 0\n"), 0o755))
		t.Setenv("PATH", dir)
		require.Error(t, checkBorg(context.Background(), &config.ManagerConfig{}, nil, nil))
	})

	t.Run("missing default borg fails outright", func(t *testing.T) {
		t.Setenv("PATH", t.TempDir())
		err := checkBorg(context.Background(), &config.ManagerConfig{}, nil, nil)
		require.Error(t, err)
	})

	t.Run("old borg is fatal only with snapshot hooks", func(t *testing.T) {
		dir := t.TempDir()
		fakeBorg(t, dir, "1.2.0")
		t.Setenv("PATH", dir)
		require.NoError(t, checkBorg(context.Background(), &config.ManagerConfig{}, nil, nil))
		hooked := &config.ManagerConfig{Borgmatic: map[string]interface{}{"btrfs": map[string]interface{}{}}}
		require.Error(t, checkBorg(context.Background(), hooked, nil, nil))
	})

	// A group's private borg never reaches the launch check at all: its
	// problems refuse only that group at generation time.
	t.Run("a group's own borg does not harden or fail the default", func(t *testing.T) {
		t.Setenv("PATH", t.TempDir())
		newBorg := fakeBorg(t, t.TempDir(), "1.4.5")

		cfg := &config.ManagerConfig{Borgmatic: map[string]interface{}{"local_path": newBorg}}
		overrides := map[string]config.GroupOverride{
			"snappy": {Borgmatic: map[string]interface{}{"btrfs": map[string]interface{}{}}},
			"legacy": {Borgmatic: map[string]interface{}{"local_path": "/nonexistent/old-borg", "zfs": map[string]interface{}{}}},
		}
		require.NoError(t, checkBorg(context.Background(), cfg, overrides, nil),
			"legacy's broken borg is its own refusal at generation, never a launch failure")
	})
}

// A transient probe failure must not be remembered against an unchanged file:
// cached, it would skip every future cycle until the daemon restarts.
func TestTransientBorgProbeFailureIsRetried(t *testing.T) {
	dir := t.TempDir()
	// Fails exactly once (marker file), then reports a healthy version, with
	// identical size and mtime irrelevant: only successes may be cached.
	// ": >" instead of touch: the test's PATH holds only the fake borg.
	script := "#!/bin/sh\nif [ ! -f \"$0.mark\" ]; then : > \"$0.mark\"; exit 1; fi\necho borg 1.4.5\n"
	require.NoError(t, os.WriteFile(filepath.Join(dir, "borg"), []byte(script), 0o755))
	t.Setenv("PATH", dir)

	require.Error(t, checkBorg(context.Background(), &config.ManagerConfig{}, nil, nil),
		"the first probe fails")
	require.NoError(t, checkBorg(context.Background(), &config.ManagerConfig{}, nil, nil),
		"the second probe must run and succeed; a cached failure would stall every cycle")
}

// An atomic replacement can preserve size and mtime (a repointed symlink, a
// same-sized package); the cache must still see the new binary.
func TestBorgVersionCacheSeesAtomicReplacement(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "borg")
	// Two same-length scripts reporting different versions.
	oldScript := "#!/bin/sh\necho borg 1.4.5 #pad\n"
	newScript := "#!/bin/sh\necho borg 1.2.0 #pad\n"
	require.Len(t, newScript, len(oldScript))
	require.NoError(t, os.WriteFile(target, []byte(oldScript), 0o755))
	info, err := os.Stat(target)
	require.NoError(t, err)
	t.Setenv("PATH", dir)

	require.NoError(t, checkBorg(context.Background(), &config.ManagerConfig{}, nil, nil),
		"1.4.5 passes and is cached")

	// Atomic replacement with identical size and mtime, new inode.
	repl := filepath.Join(dir, "borg.new")
	require.NoError(t, os.WriteFile(repl, []byte(newScript), 0o755))
	require.NoError(t, os.Chtimes(repl, info.ModTime(), info.ModTime()))
	require.NoError(t, os.Rename(repl, target))
	require.NoError(t, os.Chtimes(target, info.ModTime(), info.ModTime()))

	hooked := &config.ManagerConfig{Borgmatic: map[string]interface{}{"btrfs": map[string]interface{}{}}}
	require.Error(t, checkBorg(context.Background(), hooked, nil, nil),
		"the downgraded borg must be re-probed and fail the snapshot floor, not served from cache")
}

// A zero-exit borg shim printing garbage is not borg: versionAtLeast waves
// unparseable tokens through the floor, so plausibility must gate first.
func TestCheckBorgRejectsGarbageVersionOutput(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "borg"), []byte("#!/bin/sh\necho ok\n"), 0o755))
	t.Setenv("PATH", dir)
	err := checkBorg(context.Background(), &config.ManagerConfig{}, nil, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no usable version")
}

// The borg binary is global-only, so there is exactly one command and it is
// always required; snapshot hooks from ANY source harden its floor, since
// every group runs on it.
func TestSingleGlobalBorgGate(t *testing.T) {
	t.Run("label local_path does not suppress the default", func(t *testing.T) {
		t.Setenv("PATH", t.TempDir()) // no borg anywhere
		st := models.NewBackupState()
		st.AddVolume("app", models.VolumeInfo{Name: "v1", HostPath: "/mnt/v1"})
		st.Groups["app"].LabelConfigs = append(st.Groups["app"].LabelConfigs,
			map[string]interface{}{"local_path": "/anything/borg"})
		err := checkBorg(context.Background(), &config.ManagerConfig{}, nil, st)
		require.Error(t, err, "generation strips group-level local_path, so the default borg always runs")
		assert.Contains(t, err.Error(), "PATH")
	})

	t.Run("label snapshot hooks harden the global floor", func(t *testing.T) {
		dir := t.TempDir()
		fakeBorg(t, dir, "1.2.0")
		t.Setenv("PATH", dir)
		st := models.NewBackupState()
		st.AddVolume("snappy", models.VolumeInfo{Name: "v1", HostPath: "/mnt/v1"})
		require.NoError(t, checkBorg(context.Background(), &config.ManagerConfig{}, nil, st),
			"old borg without hooks anywhere is advisory")
		st.Groups["snappy"].LabelConfigs = append(st.Groups["snappy"].LabelConfigs,
			map[string]interface{}{"btrfs": map[string]interface{}{}})
		require.Error(t, checkBorg(context.Background(), &config.ManagerConfig{}, nil, st),
			"a label-defined snapshot hook runs on the global borg and hardens its floor")
	})

	t.Run("dormant override hooks harden the floor too", func(t *testing.T) {
		dir := t.TempDir()
		fakeBorg(t, dir, "1.2.0")
		t.Setenv("PATH", dir)
		overrides := map[string]config.GroupOverride{
			"dormant": {Borgmatic: map[string]interface{}{"zfs": map[string]interface{}{}}},
		}
		require.Error(t, checkBorg(context.Background(), &config.ManagerConfig{}, overrides, nil))
	})
}

// A YAML typo turning the global local_path into a number or list must fail
// loudly, not silently fall back to PATH with an engine nobody chose.
func TestMalformedGlobalLocalPathFailsLaunch(t *testing.T) {
	for _, bad := range []interface{}{42, []interface{}{"/opt/borg"}, ""} {
		cfg := &config.ManagerConfig{Borgmatic: map[string]interface{}{"local_path": bad}}
		err := checkBorg(context.Background(), cfg, nil, nil)
		require.Error(t, err, "local_path %#v must be rejected", bad)
		assert.Contains(t, err.Error(), "must be a non-empty string")
	}
}
