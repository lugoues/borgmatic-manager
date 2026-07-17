package main

import (
	"io"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lugoues/borgmatic-manager/internal/config"
	"github.com/lugoues/borgmatic-manager/internal/models"
)

func adhocFixture() (*models.BackupState, map[string]config.GroupRunMeta) {
	bs := models.NewBackupState()
	bs.AddVolume("alpha", models.VolumeInfo{Name: "alpha_vol", HostPath: "/mnt/a"})
	bs.AddVolume("beta", models.VolumeInfo{Name: "beta_vol", HostPath: "/mnt/b"})
	// "beta" was refused by generation: present in discovery, absent from meta.
	meta := map[string]config.GroupRunMeta{"alpha": {}}
	return bs, meta
}

// Bare `run` started the daemon through v1.5. It must now refuse rather than
// silently back everything up and exit, a stale systemd unit doing that would
// leave the unit dead and scheduled backups quietly stopped.
func TestRunCmd_BareRunRefusesWithMigrationHint(t *testing.T) {
	cmd := runCmd()
	cmd.SetArgs(nil)
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)

	err := cmd.Execute()

	require.Error(t, err, "bare run must not be treated as a target")
	assert.Contains(t, err.Error(), "--scheduler", "the error points at the daemon form")
	assert.Contains(t, err.Error(), "--all", "and at the back-up-everything form")
	assert.Contains(t, err.Error(), "v1.5", "and explains why the meaning changed")
}

func TestRunCmd_SchedulerRejectsGroupArgs(t *testing.T) {
	cmd := runCmd()
	cmd.SetArgs([]string{"--scheduler", "myapp"})
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)

	err := cmd.Execute()

	require.Error(t, err)
	assert.Contains(t, err.Error(), "takes no group arguments")
}

func TestRunCmd_AllRejectsGroupArgs(t *testing.T) {
	cmd := runCmd()
	cmd.SetArgs([]string{"--all", "myapp"})
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)

	err := cmd.Execute()

	require.Error(t, err)
	assert.Contains(t, err.Error(), "already backs up every group")
}

func TestResolveAdhocTargets_AllWhenNoneNamed(t *testing.T) {
	bs, meta := adhocFixture()
	meta["gamma"] = config.GroupRunMeta{}

	targets, err := resolveAdhocTargets(bs, meta, nil)
	require.NoError(t, err)
	assert.Equal(t, []string{"alpha", "gamma"}, targets, "all generated groups, sorted; a refused group is excluded")
}

func TestResolveAdhocTargets_NamedSubset(t *testing.T) {
	bs, meta := adhocFixture()

	targets, err := resolveAdhocTargets(bs, meta, []string{"alpha"})
	require.NoError(t, err)
	assert.Equal(t, []string{"alpha"}, targets)
}

func TestResolveAdhocTargets_UnknownGroup(t *testing.T) {
	bs, meta := adhocFixture()

	_, err := resolveAdhocTargets(bs, meta, []string{"nope"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unknown group")
}

func TestResolveAdhocTargets_RefusedGroupCannotRun(t *testing.T) {
	bs, meta := adhocFixture()

	_, err := resolveAdhocTargets(bs, meta, []string{"beta"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "refused", "a discovered-but-refused group must fail loudly, not silently skip")
}
