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

// seedToolchain lays down a provisioned-looking toolchain under stateDir and
// returns its borgmatic path.
func seedToolchain(t *testing.T, stateDir string) string {
	t.Helper()
	// The version directory name only matters for freshness, which these
	// tests don't depend on; any provisioned layout resolves.
	vdir := filepath.Join(stateDir, "toolchain", "versions", "seeded")
	require.NoError(t, os.MkdirAll(filepath.Join(vdir, "bin"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(vdir, "bin", "borgmatic"),
		[]byte("#!/bin/sh\necho 2.1.7\n"), 0o755))
	require.NoError(t, os.Symlink(filepath.Join("versions", "seeded"),
		filepath.Join(stateDir, "toolchain", "current")))
	return filepath.Join(stateDir, "toolchain", "current", "bin", "borgmatic")
}

// hostWith puts a fake borgmatic on an otherwise-empty PATH.
func hostWith(t *testing.T, script string) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "borgmatic")
	require.NoError(t, os.WriteFile(p, []byte(script), 0o755))
	t.Setenv("PATH", dir)
	return p
}

func TestLaunchBorgmaticExplicitOverrideWins(t *testing.T) {
	stateDir := t.TempDir()
	seedToolchain(t, stateDir)
	t.Setenv("BORGMATIC_PATH", "/opt/custom/borgmatic")

	e := &env{cfg: &config.ManagerConfig{}, stateDir: stateDir}
	p, err := e.launchBorgmatic(context.Background())
	require.NoError(t, err)
	assert.Equal(t, "/opt/custom/borgmatic", p,
		"the operator's override disables the toolchain entirely, even a provisioned one")
}

func TestLaunchBorgmaticPrefersProvisionedToolchainOverHost(t *testing.T) {
	stateDir := t.TempDir()
	tp := seedToolchain(t, stateDir)
	hostWith(t, "#!/bin/sh\necho 2.1.7\n")
	t.Setenv("BORGMATIC_PATH", "")

	e := &env{cfg: &config.ManagerConfig{}, stateDir: stateDir}
	p, err := e.launchBorgmatic(context.Background())
	require.NoError(t, err)
	assert.Equal(t, tp, p,
		"once provisioned, the toolchain is the stable choice; flapping back to the host would change borgmatic mid-history")
}

func TestLaunchBorgmaticRespectsHealthyHostWhenNoToolchain(t *testing.T) {
	stateDir := t.TempDir()
	hp := hostWith(t, "#!/bin/sh\necho 2.1.7\n")
	t.Setenv("BORGMATIC_PATH", "")

	e := &env{cfg: &config.ManagerConfig{}, stateDir: stateDir}
	p, err := e.launchBorgmatic(context.Background())
	require.NoError(t, err)
	assert.Equal(t, hp, p,
		"a host that manages its own healthy borgmatic gets no surprise downloads")
	_, statErr := os.Stat(filepath.Join(stateDir, "toolchain"))
	assert.True(t, os.IsNotExist(statErr), "nothing may be provisioned")
}

func TestHealthyHostBorgmaticRejectsBrokenAndOldInstalls(t *testing.T) {
	t.Run("a shim that cannot exec is not healthy", func(t *testing.T) {
		// The broken-pipx shape: the file exists, running it fails.
		hostWith(t, "#!/bin/sh\necho 'No module named borgmatic' >&2\nexit 1\n")
		_, ok := healthyHostBorgmatic(context.Background())
		assert.False(t, ok)
	})
	t.Run("an install below the version floor is not healthy", func(t *testing.T) {
		hostWith(t, "#!/bin/sh\necho 1.8.0\n")
		_, ok := healthyHostBorgmatic(context.Background())
		assert.False(t, ok)
	})
	t.Run("a working install at the floor is healthy", func(t *testing.T) {
		hp := hostWith(t, "#!/bin/sh\necho 2.1.0\n")
		p, ok := healthyHostBorgmatic(context.Background())
		require.True(t, ok)
		assert.Equal(t, hp, p)
	})
}

func TestResolveBorgmaticPrefersToolchainOverPath(t *testing.T) {
	stateDir := t.TempDir()
	tp := seedToolchain(t, stateDir)
	hostWith(t, "#!/bin/sh\necho 2.1.7\n")
	t.Setenv("BORGMATIC_PATH", "")

	p, err := resolveBorgmatic(&config.ManagerConfig{}, filepath.Join(stateDir, "toolchain"))
	require.NoError(t, err)
	assert.Equal(t, tp, p)

	t.Run("explicit config path still wins", func(t *testing.T) {
		cfg := &config.ManagerConfig{}
		cfg.Manager.BorgmaticPath = "/opt/pin/borgmatic"
		p, err := resolveBorgmatic(cfg, filepath.Join(stateDir, "toolchain"))
		require.NoError(t, err)
		assert.Equal(t, "/opt/pin/borgmatic", p)
	})

	t.Run("no toolchain falls through to PATH", func(t *testing.T) {
		hp := hostWith(t, "#!/bin/sh\necho 2.1.7\n")
		p, err := resolveBorgmatic(&config.ManagerConfig{}, filepath.Join(t.TempDir(), "toolchain"))
		require.NoError(t, err)
		assert.Equal(t, hp, p)
	})
}
