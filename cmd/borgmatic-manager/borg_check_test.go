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

	t.Run("a group override adds its borg beside the default", func(t *testing.T) {
		overrides := map[string]config.GroupOverride{
			"special": {Borgmatic: map[string]interface{}{"local_path": "/opt/borg2/borg"}},
			"plain":   {Borgmatic: map[string]interface{}{"keep_daily": 7}},
		}
		cmds := borgCommands(&config.ManagerConfig{}, overrides, nil)
		require.Len(t, cmds, 2, "groups without an override still need the default borg")
		got := []string{cmds[0].command, cmds[1].command}
		assert.ElementsMatch(t, []string{"borg", "/opt/borg2/borg"}, got)
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

	t.Run("a group override is checked and the default still required", func(t *testing.T) {
		t.Setenv("PATH", t.TempDir()) // no default borg
		lp := fakeBorg(t, t.TempDir(), "1.4.5")
		overrides := map[string]config.GroupOverride{
			"g": {Borgmatic: map[string]interface{}{"local_path": lp}},
		}
		err := checkBorg(context.Background(), &config.ManagerConfig{}, overrides, nil)
		require.Error(t, err, "groups without the override still invoke the default borg")
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

	// One group's snapshot hooks must not harden the floor for a different
	// group's private borg: below-1.4 is supported without snapshot hooks, and
	// a mixed configuration is valid.
	t.Run("the floor is scoped to the groups using each borg", func(t *testing.T) {
		t.Setenv("PATH", t.TempDir())
		newBorg := fakeBorg(t, t.TempDir(), "1.4.5")
		oldDir := t.TempDir()
		require.NoError(t, os.WriteFile(filepath.Join(oldDir, "borg-old"), []byte("#!/bin/sh\necho borg 1.2.0\n"), 0o755))
		oldBorg := filepath.Join(oldDir, "borg-old")

		cfg := &config.ManagerConfig{Borgmatic: map[string]interface{}{"local_path": newBorg}}
		overrides := map[string]config.GroupOverride{
			"snappy": {Borgmatic: map[string]interface{}{"btrfs": map[string]interface{}{}}},
			"legacy": {Borgmatic: map[string]interface{}{"local_path": oldBorg}},
		}
		require.NoError(t, checkBorg(context.Background(), cfg, overrides, nil),
			"the old borg belongs to a snapshot-free group; only the default borg carries the snapshot floor")

		overrides["legacy"] = config.GroupOverride{Borgmatic: map[string]interface{}{
			"local_path": oldBorg, "zfs": map[string]interface{}{},
		}}
		require.Error(t, checkBorg(context.Background(), cfg, overrides, nil),
			"the same old borg with hooks in its own group is fatal")
	})
}

// Labels may point every group at its own borg; a deployment doing so worked
// before the outright borg requirement and must keep working without a PATH
// borg. A group falling through to the default still demands it.
func TestCheckBorgHonorsLabelLocalPath(t *testing.T) {
	t.Setenv("PATH", t.TempDir()) // no default borg anywhere

	lp := fakeBorg(t, t.TempDir(), "1.4.5")
	st := models.NewBackupState()
	st.AddVolume("app", models.VolumeInfo{Name: "v1", HostPath: "/mnt/v1"})
	st.Groups["app"].LabelConfigs = append(st.Groups["app"].LabelConfigs,
		map[string]interface{}{"local_path": lp})

	require.NoError(t, checkBorg(context.Background(), &config.ManagerConfig{}, nil, st),
		"every discovered group labels its own borg; no PATH borg is invoked")

	t.Run("a group without a label still needs the default", func(t *testing.T) {
		st.AddVolume("plain", models.VolumeInfo{Name: "v2", HostPath: "/mnt/v2"})
		err := checkBorg(context.Background(), &config.ManagerConfig{}, nil, st)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "PATH")
	})

	t.Run("a broken label borg names its source", func(t *testing.T) {
		st2 := models.NewBackupState()
		st2.AddVolume("app", models.VolumeInfo{Name: "v1", HostPath: "/mnt/v1"})
		st2.Groups["app"].LabelConfigs = append(st2.Groups["app"].LabelConfigs,
			map[string]interface{}{"local_path": "/nonexistent/borg"})
		err := checkBorg(context.Background(), &config.ManagerConfig{}, nil, st2)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "label local_path")
	})

	t.Run("label snapshot hooks harden that group's borg floor", func(t *testing.T) {
		old := fakeBorg(t, t.TempDir(), "1.2.0")
		st3 := models.NewBackupState()
		st3.AddVolume("snappy", models.VolumeInfo{Name: "v1", HostPath: "/mnt/v1"})
		st3.Groups["snappy"].LabelConfigs = append(st3.Groups["snappy"].LabelConfigs,
			map[string]interface{}{"local_path": old, "btrfs": map[string]interface{}{}})
		err := checkBorg(context.Background(), &config.ManagerConfig{}, nil, st3)
		require.Error(t, err, "an old borg under a snapshot-hook group is fatal, label-sourced or not")
	})
}

// Generation merges labels after the group's file override, so a label
// local_path supersedes the override's. The superseded executable is never
// invoked and must not be able to fail preflight.
func TestSupersededOverrideBorgIsNotDemanded(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	labelBorg := fakeBorg(t, t.TempDir(), "1.4.5")

	st := models.NewBackupState()
	st.AddVolume("app", models.VolumeInfo{Name: "v1", HostPath: "/mnt/v1"})
	st.Groups["app"].LabelConfigs = append(st.Groups["app"].LabelConfigs,
		map[string]interface{}{"local_path": labelBorg})
	overrides := map[string]config.GroupOverride{
		"app": {Borgmatic: map[string]interface{}{"local_path": "/nonexistent/old-borg"}},
	}

	require.NoError(t, checkBorg(context.Background(), &config.ManagerConfig{}, overrides, st),
		"the label superseded the override; generation never invokes /nonexistent/old-borg")
}

// A dormant group (override present, containers currently gone) that inherits
// the default borg still runs on it when it returns, without preflight
// rerunning. Its snapshot hooks must reach the default's version floor, and
// its fallthrough must keep the default required even when every discovered
// group brings its own borg.
func TestDormantSnapshotGroupHardensTheInheritedDefault(t *testing.T) {
	dir := t.TempDir()
	fakeBorg(t, dir, "1.2.0") // old default borg
	t.Setenv("PATH", dir)

	labelBorg := fakeBorg(t, t.TempDir(), "1.4.5")
	st := models.NewBackupState()
	st.AddVolume("app", models.VolumeInfo{Name: "v1", HostPath: "/mnt/v1"})
	st.Groups["app"].LabelConfigs = append(st.Groups["app"].LabelConfigs,
		map[string]interface{}{"local_path": labelBorg})

	overrides := map[string]config.GroupOverride{
		"dormant": {Borgmatic: map[string]interface{}{"btrfs": map[string]interface{}{}}},
	}
	err := checkBorg(context.Background(), &config.ManagerConfig{}, overrides, st)
	require.Error(t, err,
		"the dormant snapshot group inherits the old default borg; waving it through would record snapshot paths when it wakes")

	t.Run("and keeps the default required at all", func(t *testing.T) {
		t.Setenv("PATH", t.TempDir()) // no default borg anywhere
		err := checkBorg(context.Background(), &config.ManagerConfig{}, overrides, st)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "PATH")
	})
}

// A dormant group configured only through manager settings (a period alone)
// still inherits the default borg when it wakes, without a preflight rerun.
func TestManagerOnlyDormantOverrideKeepsDefaultRequired(t *testing.T) {
	t.Setenv("PATH", t.TempDir()) // no default borg
	labelBorg := fakeBorg(t, t.TempDir(), "1.4.5")

	st := models.NewBackupState()
	st.AddVolume("app", models.VolumeInfo{Name: "v1", HostPath: "/mnt/v1"})
	st.Groups["app"].LabelConfigs = append(st.Groups["app"].LabelConfigs,
		map[string]interface{}{"local_path": labelBorg})

	overrides := map[string]config.GroupOverride{"dormant": {}}
	err := checkBorg(context.Background(), &config.ManagerConfig{}, overrides, st)
	require.Error(t, err, "the dormant group will inherit the default borg; its absence must fail now, not when the group wakes")
	assert.Contains(t, err.Error(), "PATH")
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
