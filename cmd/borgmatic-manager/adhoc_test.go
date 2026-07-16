package main

import (
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
