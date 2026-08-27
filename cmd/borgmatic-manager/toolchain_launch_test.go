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
// sends launchBorgmatic through Ensure's provisioning path, and a unit test
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
	t.Run("a prefixed version report is parsed, not waved through", func(t *testing.T) {
		// versionAtLeast passes unparseable strings by design, so "borgmatic
		// 2.0.0" fed whole would read as healthy and block provisioning.
		hostWith(t, "#!/bin/sh\necho borgmatic 2.0.0\n")
		_, ok := healthyHostBorgmatic(context.Background())
		assert.False(t, ok)

		hp := hostWith(t, "#!/bin/sh\necho borgmatic 2.1.7\n")
		p, ok := healthyHostBorgmatic(context.Background())
		require.True(t, ok, "a prefixed but current version is healthy")
		assert.Equal(t, hp, p)
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

	p, err := resolveBorgmatic(context.Background(), &config.ManagerConfig{}, filepath.Join(stateDir, "toolchain"))
	require.NoError(t, err)
	assert.Equal(t, tp, p)

	t.Run("explicit config path still wins", func(t *testing.T) {
		cfg := &config.ManagerConfig{}
		cfg.Manager.BorgmaticPath = "/opt/pin/borgmatic"
		p, err := resolveBorgmatic(context.Background(), cfg, filepath.Join(stateDir, "toolchain"))
		require.NoError(t, err)
		assert.Equal(t, "/opt/pin/borgmatic", p)
	})

	t.Run("no toolchain falls through to PATH", func(t *testing.T) {
		hp := hostWith(t, "#!/bin/sh\necho 2.1.7\n")
		p, err := resolveBorgmatic(context.Background(), &config.ManagerConfig{}, filepath.Join(t.TempDir(), "toolchain"))
		require.NoError(t, err)
		assert.Equal(t, hp, p)
	})
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

	e := &env{cfg: &config.ManagerConfig{}, stateDir: stateDir}
	p, err := e.launchBorgmatic(context.Background())
	require.NoError(t, err)
	require.Equal(t, tp, p)
	assert.Empty(t, os.Getenv("PYTHONPATH"))
	assert.Empty(t, os.Getenv("PYTHONHOME"))
}

// A broken toolchain must hand restore/passthrough an unaltered host
// fallback: the host install may itself rely on the Python variables the
// toolchain sheds, and stripping them before the probe's verdict would break
// the very fallback being reached for.
func TestBrokenToolchainFallbackKeepsHostPythonEnv(t *testing.T) {
	stateDir := t.TempDir()
	name := toolchain.PinnedVersionDirName()
	vdir := filepath.Join(stateDir, "toolchain", "versions", name)
	require.NoError(t, os.MkdirAll(filepath.Join(vdir, "bin"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(vdir, "bin", "borgmatic"),
		[]byte("#!/bin/sh\nexit 1\n"), 0o755))
	require.NoError(t, os.Symlink(filepath.Join("versions", name),
		filepath.Join(stateDir, "toolchain", "current")))

	hp := hostWith(t, "#!/bin/sh\necho 2.1.7\n")
	t.Setenv("BORGMATIC_PATH", "")
	t.Setenv("PYTHONPATH", "/opt/host/pythonpath")

	p, err := resolveBorgmatic(context.Background(), &config.ManagerConfig{}, filepath.Join(stateDir, "toolchain"))
	require.NoError(t, err)
	assert.Equal(t, hp, p, "the broken toolchain falls through to the host")
	assert.Equal(t, "/opt/host/pythonpath", os.Getenv("PYTHONPATH"),
		"the host fallback runs in the environment the host install expects")
}

// A zero-exit no-op launcher must not be selected for passthrough/restore:
// exit status alone would let those commands silently do nothing.
func TestResolveBorgmaticRejectsNoOpToolchainLauncher(t *testing.T) {
	stateDir := t.TempDir()
	name := toolchain.PinnedVersionDirName()
	vdir := filepath.Join(stateDir, "toolchain", "versions", name)
	require.NoError(t, os.MkdirAll(filepath.Join(vdir, "bin"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(vdir, "bin", "borgmatic"),
		[]byte("#!/bin/sh\nexit 0\n"), 0o755))
	require.NoError(t, os.Symlink(filepath.Join("versions", name),
		filepath.Join(stateDir, "toolchain", "current")))

	hp := hostWith(t, "#!/bin/sh\necho 2.1.7\n")
	t.Setenv("BORGMATIC_PATH", "")

	p, err := resolveBorgmatic(context.Background(), &config.ManagerConfig{}, filepath.Join(stateDir, "toolchain"))
	require.NoError(t, err)
	assert.Equal(t, hp, p, "the silent launcher is rejected; the host fallback is real borgmatic")
}

// A host shim exiting zero with garbage output is not borgmatic: selecting it
// would skip provisioning and record no-op runs as backups.
func TestHealthyHostRejectsGarbageVersion(t *testing.T) {
	hostWith(t, "#!/bin/sh\necho ok\n")
	_, ok := healthyHostBorgmatic(context.Background())
	assert.False(t, ok)
}

// A broken PATH shim must not hide a healthy install at a well-known
// location: forcing provisioning (or failing offline) despite a usable host
// install serves nobody.
func TestUnhealthyHostCandidateDoesNotHideALaterOne(t *testing.T) {
	hostWith(t, "#!/bin/sh\nexit 1\n") // broken shim on PATH

	goodDir := t.TempDir()
	good := filepath.Join(goodDir, "borgmatic")
	require.NoError(t, os.WriteFile(good, []byte("#!/bin/sh\necho 2.1.7\n"), 0o755))
	orig := wellKnownBorgmaticPaths
	wellKnownBorgmaticPaths = []string{good}
	t.Cleanup(func() { wellKnownBorgmaticPaths = orig })

	p, ok := healthyHostBorgmatic(context.Background())
	require.True(t, ok)
	assert.Equal(t, good, p)

	t.Run("resolveBorgmatic walks the same candidates", func(t *testing.T) {
		p, err := resolveBorgmatic(context.Background(), &config.ManagerConfig{}, filepath.Join(t.TempDir(), "toolchain"))
		require.NoError(t, err)
		assert.Equal(t, good, p)
	})
}
