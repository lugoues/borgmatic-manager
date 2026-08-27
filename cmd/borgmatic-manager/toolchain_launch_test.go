package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lugoues/borgmatic-manager/internal/config"
	"github.com/lugoues/borgmatic-manager/internal/toolchain"
)

// seedToolchain lays down a fresh, healthy-looking toolchain under stateDir
// and returns its borgmatic path. Fresh and healthy on purpose: anything less
// sends resolveBorgmatic through Ensure's provisioning path, and a unit test
// must never reach for the network.
func seedToolchain(t *testing.T, stateDir string) string {
	t.Helper()
	name := toolchain.PinnedVersionDirName()
	vdir := filepath.Join(stateDir, "toolchain", "versions", name)
	require.NoError(t, os.MkdirAll(filepath.Join(vdir, "bin"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(vdir, "bin", "borgmatic"),
		[]byte("#!/bin/sh\necho "+toolchain.BorgmaticVersion+"\n"), 0o755))
	require.NoError(t, os.Symlink(filepath.Join("versions", name),
		filepath.Join(stateDir, "toolchain", "current")))
	return filepath.Join(stateDir, "toolchain", "current", "bin", "borgmatic")
}

// hostWith puts a fake borgmatic on an otherwise-empty PATH. Under the
// manager-owns-borgmatic policy these installs exist in tests to prove they
// are IGNORED.
func hostWith(t *testing.T, script string) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "borgmatic")
	require.NoError(t, os.WriteFile(p, []byte(script), 0o755))
	t.Setenv("PATH", dir)
	return p
}

func TestResolveBorgmaticExplicitOverrideWins(t *testing.T) {
	stateDir := t.TempDir()
	seedToolchain(t, stateDir)
	pin := filepath.Join(t.TempDir(), "borgmatic")
	require.NoError(t, os.WriteFile(pin, []byte("#!/bin/sh\necho 2.1.7\n"), 0o755))
	t.Setenv("BORGMATIC_PATH", pin)

	p, err := resolveBorgmatic(context.Background(), &config.ManagerConfig{}, filepath.Join(stateDir, "toolchain"))
	require.NoError(t, err)
	assert.Equal(t, pin, p,
		"the operator's override is the single exception to the managed toolchain, even a provisioned one")
}

// A decayed explicit pin errors instead of silently no-op restoring: the
// operator pinned it, so a broken pin is an error, never a fallback.
func TestResolveBorgmaticRejectsDecayedExplicitPin(t *testing.T) {
	pin := filepath.Join(t.TempDir(), "borgmatic")
	require.NoError(t, os.WriteFile(pin, []byte("#!/bin/sh\nexit 0\n"), 0o755))
	cfg := &config.ManagerConfig{}
	cfg.Manager.BorgmaticPath = pin
	t.Setenv("BORGMATIC_PATH", "")

	_, err := resolveBorgmatic(context.Background(), cfg, filepath.Join(t.TempDir(), "toolchain"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "fix the override")
}

// The core of the ownership policy: a healthy host install without an
// explicit override means nothing. With a toolchain provisioned, the
// toolchain is selected; the host binary is never even probed.
func TestResolveBorgmaticIgnoresHealthyHostInstall(t *testing.T) {
	stateDir := t.TempDir()
	tp := seedToolchain(t, stateDir)
	hostWith(t, "#!/bin/sh\necho 2.1.7\n")
	t.Setenv("BORGMATIC_PATH", "")

	p, err := resolveBorgmatic(context.Background(), &config.ManagerConfig{}, filepath.Join(stateDir, "toolchain"))
	require.NoError(t, err)
	assert.Equal(t, tp, p)
}

// The other half of the policy: with NO toolchain, a healthy host install
// still means nothing. Provisioning is attempted, and when it cannot succeed
// the command fails; the host is not a fallback, because a host borgmatic
// healthy today is one host package upgrade away from broken.
func TestResolveBorgmaticIgnoresHostWhenProvisioningFails(t *testing.T) {
	hp := hostWith(t, "#!/bin/sh\necho 2.1.7\n")
	t.Setenv("BORGMATIC_PATH", "")

	// A regular file where the toolchain dir must go: every mkdir under it
	// fails ENOTDIR, root included, so the failure is deterministic and no
	// network is ever reached.
	tcPath := filepath.Join(t.TempDir(), "toolchain")
	require.NoError(t, os.WriteFile(tcPath, nil, 0o644))

	p, err := resolveBorgmatic(context.Background(), &config.ManagerConfig{}, tcPath)
	require.Error(t, err, "no toolchain and no way to provision one is a failure, not a host fallback")
	assert.NotEqual(t, hp, p)
	assert.Contains(t, err.Error(), "provisioning")
}

// Selecting the toolchain must shed the host's Python configuration: the
// managed launcher's shebang still honors PYTHONHOME/PYTHONPATH, so a service
// environment carrying them would re-couple every backup to the host Python.
func TestSelectingTheToolchainStripsHostPythonEnv(t *testing.T) {
	stateDir := t.TempDir()
	tp := seedToolchain(t, stateDir)
	t.Setenv("BORGMATIC_PATH", "")
	t.Setenv("PYTHONPATH", "/usr/lib/python3/dist-packages")
	t.Setenv("PYTHONHOME", "/usr")

	p, err := resolveBorgmatic(context.Background(), &config.ManagerConfig{}, filepath.Join(stateDir, "toolchain"))
	require.NoError(t, err)
	require.Equal(t, tp, p)
	assert.Empty(t, os.Getenv("PYTHONPATH"))
	assert.Empty(t, os.Getenv("PYTHONHOME"))
}

// An explicit override is the operator's own install and may rely on the very
// variables the toolchain sheds; its environment stays untouched.
func TestExplicitOverrideKeepsHostPythonEnv(t *testing.T) {
	pin := filepath.Join(t.TempDir(), "borgmatic")
	require.NoError(t, os.WriteFile(pin, []byte("#!/bin/sh\necho 2.1.7\n"), 0o755))
	cfg := &config.ManagerConfig{}
	cfg.Manager.BorgmaticPath = pin
	t.Setenv("BORGMATIC_PATH", "")
	t.Setenv("PYTHONPATH", "/opt/host/pythonpath")

	p, err := resolveBorgmatic(context.Background(), cfg, filepath.Join(t.TempDir(), "toolchain"))
	require.NoError(t, err)
	assert.Equal(t, pin, p)
	assert.Equal(t, "/opt/host/pythonpath", os.Getenv("PYTHONPATH"))
}

// The floor lives in the shared resolver, so restore and passthrough refuse
// an old explicit override exactly like the daemon does; an operator opting
// out of the toolchain does not opt out of the version requirement.
func TestResolveBorgmaticRejectsBelowFloorExplicitPin(t *testing.T) {
	pin := filepath.Join(t.TempDir(), "borgmatic")
	require.NoError(t, os.WriteFile(pin, []byte("#!/bin/sh\necho 2.0.5\n"), 0o755))
	cfg := &config.ManagerConfig{}
	cfg.Manager.BorgmaticPath = pin
	t.Setenv("BORGMATIC_PATH", "")

	_, err := resolveBorgmatic(context.Background(), cfg, filepath.Join(t.TempDir(), "toolchain"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "need >= 2.1.0")
}
