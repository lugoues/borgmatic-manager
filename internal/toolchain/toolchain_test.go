package toolchain

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lugoues/borgmatic-manager/internal/lockfile"
)

// uvTarball builds a release-shaped tar.gz holding one uv "binary" (a shell
// script) and returns it with its checksum.
func uvTarball(t *testing.T, script string) ([]byte, string) {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	body := []byte(script)
	require.NoError(t, tw.WriteHeader(&tar.Header{
		Name: "uv-test-unknown-linux-musl/uv", Mode: 0o755, Size: int64(len(body)), Typeflag: tar.TypeReg,
	}))
	_, err := tw.Write(body)
	require.NoError(t, err)
	require.NoError(t, tw.Close())
	require.NoError(t, gz.Close())
	sum := sha256.Sum256(buf.Bytes())
	return buf.Bytes(), hex.EncodeToString(sum[:])
}

// newTestToolchain wires a Toolchain to a fake release server and a fake uv
// runner that lays down bin/borgmatic as a script reporting the pinned
// version. The returned *[]string records every env slice runUV received.
func newTestToolchain(t *testing.T, root string, tarball []byte, sum string) (*Toolchain, *[][]string) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(tarball)
	}))
	t.Cleanup(srv.Close)

	var envs [][]string
	tc := New(root, slog.New(slog.DiscardHandler))
	tc.downloadBase = srv.URL
	tc.uvSums = map[string]string{runtime.GOARCH: sum}
	tc.runUV = func(_ context.Context, uvPath string, env []string, args ...string) ([]byte, error) {
		envs = append(envs, env)
		vdir := filepath.Dir(uvPath)
		script := "#!/bin/sh\necho borgmatic " + BorgmaticVersion + "\n"
		return nil, os.WriteFile(filepath.Join(vdir, "bin", "borgmatic"), []byte(script), 0o755)
	}
	return tc, &envs
}

func TestEnsureProvisionsAndFlipsCurrent(t *testing.T) {
	root := t.TempDir()
	tarball, sum := uvTarball(t, "#!/bin/sh\nexit 0\n")
	tc, envs := newTestToolchain(t, root, tarball, sum)

	p, err := tc.Ensure(context.Background())
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(root, "current", "bin", "borgmatic"), p)
	assert.True(t, tc.Fresh())

	// The current symlink resolves inside the version directory, and the
	// manifest records the pins.
	m, fresh, err := tc.Info()
	require.NoError(t, err)
	assert.True(t, fresh)
	assert.Equal(t, BorgmaticVersion, m.BorgmaticVersion)
	assert.Equal(t, uvVersion, m.UVVersion)

	// uv ran fully contained: managed python only, everything under the
	// version directory, nothing pointed at the host's uv or python state.
	require.Len(t, *envs, 1)
	env := strings.Join((*envs)[0], "\n")
	vdir := filepath.Join(root, "versions", versionDirName())
	assert.Contains(t, env, "UV_PYTHON_PREFERENCE=only-managed")
	assert.Contains(t, env, "UV_NO_CONFIG=1")
	assert.Contains(t, env, "UV_TOOL_DIR="+filepath.Join(vdir, "tools"))
	assert.Contains(t, env, "UV_PYTHON_INSTALL_DIR="+filepath.Join(vdir, "python"))
	assert.Contains(t, env, "HOME="+vdir)

	// Idempotent: a second Ensure sees a fresh toolchain and runs nothing.
	_, err = tc.Ensure(context.Background())
	require.NoError(t, err)
	assert.Len(t, *envs, 1, "a fresh toolchain must not be reprovisioned")
}

func TestChecksumMismatchRefusesInstall(t *testing.T) {
	root := t.TempDir()
	tarball, _ := uvTarball(t, "#!/bin/sh\nexit 0\n")
	tc, _ := newTestToolchain(t, root, tarball, strings.Repeat("0", 64))

	_, err := tc.Ensure(context.Background())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "verification")
	_, ok := tc.BorgmaticPath()
	assert.False(t, ok, "nothing may be flipped to current after a failed verification")
}

func TestStaleToolchainIsReprovisionedAndOldRemoved(t *testing.T) {
	root := t.TempDir()

	// A previously provisioned, now-stale version: real directory, current
	// symlink, working binary.
	old := filepath.Join(root, "versions", "uv0.0.1-py3.12-borgmatic2.0.0")
	require.NoError(t, os.MkdirAll(filepath.Join(old, "bin"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(old, "bin", "borgmatic"), []byte("#!/bin/sh\necho borgmatic 2.0.0\n"), 0o755))
	// A manifest marks it finished: retirement only ever touches completed
	// toolchains, never a directory another process may still be building.
	require.NoError(t, os.WriteFile(filepath.Join(old, "manifest.json"), []byte("{}"), 0o644))
	require.NoError(t, os.Symlink(filepath.Join("versions", "uv0.0.1-py3.12-borgmatic2.0.0"), filepath.Join(root, "current")))

	tarball, sum := uvTarball(t, "#!/bin/sh\nexit 0\n")
	tc, _ := newTestToolchain(t, root, tarball, sum)
	require.False(t, tc.Fresh(), "the seeded toolchain must read as stale")

	p, err := tc.Ensure(context.Background())
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(root, "current", "bin", "borgmatic"), p)
	assert.True(t, tc.Fresh())

	// The superseded directory is not removed on the flip: a borgmatic
	// started from it (a passthrough holds none of the manager's locks) may
	// still be importing from its environment. It is marked, and removed only
	// once the mark has aged past the grace period.
	marker := filepath.Join(old, ".superseded")
	_, statErr := os.Stat(marker)
	require.NoError(t, statErr, "the old version is marked, not deleted")

	aged := time.Now().Add(-supersededGrace - time.Hour)
	require.NoError(t, os.Chtimes(marker, aged, aged))
	_, err = tc.Ensure(context.Background())
	require.NoError(t, err)
	_, statErr = os.Stat(old)
	assert.True(t, os.IsNotExist(statErr), "aged past the grace period, the old version is removed")
}

// Freshness is only a symlink name. A toolchain whose environment was deleted
// or corrupted keeps its launcher and its name, and returning it on that
// evidence would fail every launch without ever self-healing.
func TestBrokenFreshToolchainIsReprovisioned(t *testing.T) {
	root := t.TempDir()
	vdir := filepath.Join(root, "versions", versionDirName())
	require.NoError(t, os.MkdirAll(filepath.Join(vdir, "bin"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(vdir, "bin", "borgmatic"),
		[]byte("#!/bin/sh\necho 'ModuleNotFoundError: borgmatic' >&2\nexit 1\n"), 0o755))
	require.NoError(t, os.Symlink(filepath.Join("versions", versionDirName()), filepath.Join(root, "current")))

	tarball, sum := uvTarball(t, "#!/bin/sh\nexit 0\n")
	tc, envs := newTestToolchain(t, root, tarball, sum)
	require.True(t, tc.Fresh(), "the name says fresh; only the health check knows better")

	p, err := tc.Ensure(context.Background())
	require.NoError(t, err)
	require.Len(t, *envs, 1, "the broken toolchain must be rebuilt")
	out, err := os.ReadFile(p)
	require.NoError(t, err)
	assert.Contains(t, string(out), BorgmaticVersion)
}

// A repair of the composition "current" already points at must never build in
// the live directory: the running toolchain would be destroyed before its
// replacement exists. It stages into a generation sibling and flips.
func TestSameVersionRepairStagesBesideTheLiveDirectory(t *testing.T) {
	root := t.TempDir()
	name := versionDirName()
	live := filepath.Join(root, "versions", name)
	require.NoError(t, os.MkdirAll(filepath.Join(live, "bin"), 0o755))
	// Healthy exit, wrong version: the health gate demands a rebuild.
	require.NoError(t, os.WriteFile(filepath.Join(live, "bin", "borgmatic"),
		[]byte("#!/bin/sh\necho 0.0.0\n"), 0o755))
	sentinel := filepath.Join(live, "keepsake")
	require.NoError(t, os.WriteFile(sentinel, []byte("x"), 0o600))
	require.NoError(t, os.Symlink(filepath.Join("versions", name), filepath.Join(root, "current")))

	t.Run("a failed rebuild leaves the live directory untouched", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "no network", http.StatusBadGateway)
		}))
		defer srv.Close()
		tc := New(root, slog.New(slog.DiscardHandler))
		tc.downloadBase = srv.URL

		p, err := tc.Ensure(context.Background())
		require.NoError(t, err, "an executing toolchain degrades even at the wrong version; preflight's floor judges it")
		assert.Equal(t, filepath.Join(root, "current", "bin", "borgmatic"), p)
		_, statErr := os.Stat(sentinel)
		require.NoError(t, statErr, "the live directory must not be cleared for a repair that never completed")
	})

	t.Run("a successful rebuild flips to a generation sibling", func(t *testing.T) {
		tarball, sum := uvTarball(t, "#!/bin/sh\nexit 0\n")
		tc, _ := newTestToolchain(t, root, tarball, sum)
		p, err := tc.Ensure(context.Background())
		require.NoError(t, err)
		assert.True(t, tc.Fresh(), "a repair generation still counts as the pinned composition")
		assert.True(t, strings.HasPrefix(tc.currentTargetBase(), name+"-r"),
			"the rebuild lands in an unused generation sibling, never the live directory (got %s)", tc.currentTargetBase())
		out, err := os.ReadFile(p)
		require.NoError(t, err)
		assert.Contains(t, string(out), BorgmaticVersion)
		_, statErr := os.Stat(sentinel)
		require.NoError(t, statErr, "the old directory survives into the grace period")
	})
}

// A provisioning attempt that dies by its own deadline must not poison the
// degrade fallback: the stale toolchain still works and the launch must keep
// running on it.
func TestExpiredProvisionDeadlineStillDegrades(t *testing.T) {
	root := t.TempDir()
	old := filepath.Join(root, "versions", "uv0.0.1-py3.12-borgmatic2.0.0")
	require.NoError(t, os.MkdirAll(filepath.Join(old, "bin"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(old, "bin", "borgmatic"),
		[]byte("#!/bin/sh\necho 2.0.0\n"), 0o755))
	require.NoError(t, os.Symlink(filepath.Join("versions", "uv0.0.1-py3.12-borgmatic2.0.0"), filepath.Join(root, "current")))

	// The download stalls past the provisioning deadline.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(2 * time.Second)
	}))
	defer srv.Close()
	tc := New(root, slog.New(slog.DiscardHandler))
	tc.downloadBase = srv.URL
	tc.provisionTimeout = 100 * time.Millisecond

	p, err := tc.Ensure(context.Background())
	require.NoError(t, err, "the fallback health check must not run on the expired provisioning context")
	assert.Equal(t, filepath.Join(root, "current", "bin", "borgmatic"), p)
}

// Degrading to the existing toolchain is only safe when it still runs: handing
// back a broken binary would just move the failure one step later.
func TestDegradeRequiresAWorkingToolchain(t *testing.T) {
	root := t.TempDir()
	old := filepath.Join(root, "versions", "uv0.0.1-py3.12-borgmatic2.0.0")
	require.NoError(t, os.MkdirAll(filepath.Join(old, "bin"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(old, "bin", "borgmatic"),
		[]byte("#!/bin/sh\nexit 1\n"), 0o755))
	require.NoError(t, os.Symlink(filepath.Join("versions", "uv0.0.1-py3.12-borgmatic2.0.0"), filepath.Join(root, "current")))

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "no network", http.StatusBadGateway)
	}))
	defer srv.Close()
	tc := New(root, slog.New(slog.DiscardHandler))
	tc.downloadBase = srv.URL

	_, err := tc.Ensure(context.Background())
	require.Error(t, err, "a broken toolchain plus a failed provision is a real failure, not a degrade")
}

func TestProvisionFailureDegradesToExistingToolchain(t *testing.T) {
	root := t.TempDir()
	old := filepath.Join(root, "versions", "uv0.0.1-py3.12-borgmatic2.0.0")
	require.NoError(t, os.MkdirAll(filepath.Join(old, "bin"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(old, "bin", "borgmatic"), []byte("#!/bin/sh\necho borgmatic 2.0.0\n"), 0o755))
	require.NoError(t, os.Symlink(filepath.Join("versions", "uv0.0.1-py3.12-borgmatic2.0.0"), filepath.Join(root, "current")))

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "no network", http.StatusBadGateway)
	}))
	defer srv.Close()
	tc := New(root, slog.New(slog.DiscardHandler))
	tc.downloadBase = srv.URL

	p, err := tc.Ensure(context.Background())
	require.NoError(t, err, "a stale toolchain that works beats no toolchain at all")
	assert.Equal(t, filepath.Join(root, "current", "bin", "borgmatic"), p)
	assert.False(t, tc.Fresh(), "still stale; the next launch retries")
}

func TestProvisionFailureWithNothingProvisionedFails(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "no network", http.StatusBadGateway)
	}))
	defer srv.Close()
	tc := New(t.TempDir(), slog.New(slog.DiscardHandler))
	tc.downloadBase = srv.URL

	_, err := tc.Ensure(context.Background())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "provisioning borgmatic toolchain")
}

func TestSmokeTestFailureDoesNotFlipCurrent(t *testing.T) {
	root := t.TempDir()
	tarball, sum := uvTarball(t, "#!/bin/sh\nexit 0\n")
	tc, _ := newTestToolchain(t, root, tarball, sum)
	// The runner installs a borgmatic that reports the wrong version.
	tc.runUV = func(_ context.Context, uvPath string, _ []string, _ ...string) ([]byte, error) {
		return nil, os.WriteFile(filepath.Join(filepath.Dir(uvPath), "bin", "borgmatic"),
			[]byte("#!/bin/sh\necho borgmatic 0.0.0\n"), 0o755)
	}

	_, err := tc.Ensure(context.Background())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "smoke test")
	_, ok := tc.BorgmaticPath()
	assert.False(t, ok)
}

func TestCurrentBorgmaticWithoutProvisioning(t *testing.T) {
	root := t.TempDir()
	_, ok := CurrentBorgmatic(root)
	assert.False(t, ok, "no toolchain, no path; and nothing may be downloaded to make one")

	vdir := filepath.Join(root, "versions", versionDirName())
	require.NoError(t, os.MkdirAll(filepath.Join(vdir, "bin"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(vdir, "bin", "borgmatic"), []byte("#!/bin/sh\n"), 0o755))
	require.NoError(t, os.Symlink(filepath.Join("versions", versionDirName()), filepath.Join(root, "current")))

	p, ok := CurrentBorgmatic(root)
	require.True(t, ok)
	assert.Equal(t, filepath.Join(root, "current", "bin", "borgmatic"), p)
}

func TestDownloadedUVIsInvokedFromItsVersionDir(t *testing.T) {
	// The real runUV execs the downloaded file; prove the plumbing holds by
	// letting the fake uv script do the install itself.
	root := t.TempDir()
	script := fmt.Sprintf("#!/bin/sh\nmkdir -p \"$(dirname \"$0\")/bin\"\nprintf '#!/bin/sh\necho borgmatic %s\n' > \"$(dirname \"$0\")/bin/borgmatic\"\nchmod 0755 \"$(dirname \"$0\")/bin/borgmatic\"\n", BorgmaticVersion)
	tarball, sum := uvTarball(t, script)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(tarball)
	}))
	defer srv.Close()
	tc := New(root, slog.New(slog.DiscardHandler))
	tc.downloadBase = srv.URL
	tc.uvSums = map[string]string{runtime.GOARCH: sum}

	p, err := tc.Ensure(context.Background())
	require.NoError(t, err)
	out, err := os.ReadFile(p)
	require.NoError(t, err)
	assert.Contains(t, string(out), BorgmaticVersion)
}

func TestExtractUVTakesOnlyTheUVMember(t *testing.T) {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, m := range []struct{ name, body string }{
		{"uv-test/README.md", "docs"},
		{"uv-test/uv", "#!/bin/sh\n"},
		{"uv-test/uvx", "#!/bin/sh\n"},
	} {
		require.NoError(t, tw.WriteHeader(&tar.Header{Name: m.name, Mode: 0o755, Size: int64(len(m.body)), Typeflag: tar.TypeReg}))
		_, err := io.WriteString(tw, m.body)
		require.NoError(t, err)
	}
	require.NoError(t, tw.Close())
	require.NoError(t, gz.Close())

	dest := filepath.Join(t.TempDir(), "uv")
	require.NoError(t, extractUV(bytes.NewReader(buf.Bytes()), dest))
	info, err := os.Stat(dest)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o755), info.Mode().Perm())
}

// A generation is never reused: a retired directory inside its grace period
// may still back a running borgmatic, and rebuilding into it would delete
// that process's files. The allocator skips anything on disk.
func TestBuildDirNameNeverReusesExistingDirectories(t *testing.T) {
	root := t.TempDir()
	tc := New(root, slog.New(slog.DiscardHandler))
	name := versionDirName()

	first, err := tc.buildDirName()
	require.NoError(t, err)
	assert.Equal(t, name, first, "an empty root builds the pinned name")

	// Current on -r3, with the base and -r2 retired but still on disk (grace).
	for _, d := range []string{name, name + "-r2", name + "-r3"} {
		require.NoError(t, os.MkdirAll(filepath.Join(root, "versions", d), 0o755))
	}
	require.NoError(t, os.Symlink(filepath.Join("versions", name+"-r3"), filepath.Join(root, "current")))

	next, err := tc.buildDirName()
	require.NoError(t, err)
	assert.Equal(t, name+"-r4", next,
		"cycling back onto a retired directory would delete files a running borgmatic may still need")
}

// Waiting for another process's provisioning lock must observe cancellation:
// SIGTERM during someone else's stalled download stops the waiter now.
func TestProvisionLockWaitObservesCancellation(t *testing.T) {
	root := t.TempDir()
	tc := New(root, slog.New(slog.DiscardHandler))
	require.NoError(t, os.MkdirAll(root, 0o700))

	held, acquired, err := lockfile.TryExclusive(filepath.Join(root, "provision.lock"))
	require.NoError(t, err)
	require.True(t, acquired)
	defer held.Release()

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err = tc.Ensure(ctx)
	require.Error(t, err)
	assert.Less(t, time.Since(start), 5*time.Second,
		"the waiter must give up with the context, not wait out the holder")
}

// A launcher that hangs instead of exiting must read as unhealthy, or a
// wedged managed Python would hold the daemon's startup forever instead of
// being reprovisioned.
func TestHungProbeIsUnhealthyAndReprovisions(t *testing.T) {
	root := t.TempDir()
	vdir := filepath.Join(root, "versions", versionDirName())
	require.NoError(t, os.MkdirAll(filepath.Join(vdir, "bin"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(vdir, "bin", "borgmatic"),
		[]byte("#!/bin/sh\nsleep 60\n"), 0o755))
	require.NoError(t, os.Symlink(filepath.Join("versions", versionDirName()), filepath.Join(root, "current")))

	tarball, sum := uvTarball(t, "#!/bin/sh\nexit 0\n")
	tc, envs := newTestToolchain(t, root, tarball, sum)
	tc.probeTimeout = 200 * time.Millisecond

	start := time.Now()
	p, err := tc.Ensure(context.Background())
	require.NoError(t, err)
	assert.Less(t, time.Since(start), 30*time.Second)
	require.Len(t, *envs, 1, "the hung toolchain must be rebuilt")
	out, err := os.ReadFile(p)
	require.NoError(t, err)
	assert.Contains(t, string(out), BorgmaticVersion)
}

// Under Restart=on-failure a transient outage retries provisioning every few
// seconds. Each failed attempt must clean up after itself, and crash leftovers
// must be reaped, or the attempts pile up partial Python environments until
// the disk or the generation namespace runs out.
func TestFailedProvisioningLeavesNoDebris(t *testing.T) {
	root := t.TempDir()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "no network", http.StatusBadGateway)
	}))
	defer srv.Close()
	tc := New(root, slog.New(slog.DiscardHandler))
	tc.downloadBase = srv.URL

	for range 3 {
		_, err := tc.Ensure(context.Background())
		require.Error(t, err)
	}
	entries, err := os.ReadDir(filepath.Join(root, "versions"))
	require.NoError(t, err)
	assert.Empty(t, entries, "every failed attempt removes its own directory")

	// A crash leftover (directory without a manifest, from a provisioner that
	// died before its own cleanup) is reaped by the next attempt, which then
	// builds under the ordinary pinned name.
	crashed := filepath.Join(root, "versions", versionDirName())
	require.NoError(t, os.MkdirAll(filepath.Join(crashed, "bin"), 0o755))

	tarball, sum := uvTarball(t, "#!/bin/sh\nexit 0\n")
	tc2, _ := newTestToolchain(t, root, tarball, sum)
	_, err = tc2.Ensure(context.Background())
	require.NoError(t, err)
	assert.Equal(t, versionDirName(), tc2.currentTargetBase(),
		"the reaped leftover frees the pinned name instead of forcing a generation")
}

// A toolchain whose launcher or whole target directory vanished is repair
// evidence, not absence. Exists() reports it, and Ensure rebuilds it.
func TestVanishedLauncherIsReprovisioned(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.MkdirAll(root, 0o700))
	require.NoError(t, os.Symlink(filepath.Join("versions", "gone"), filepath.Join(root, "current")))

	tc0 := New(root, slog.New(slog.DiscardHandler))
	require.True(t, tc0.Exists(), "a dangling current symlink is provisioned evidence")
	_, ok := tc0.BorgmaticPath()
	require.False(t, ok)

	tarball, sum := uvTarball(t, "#!/bin/sh\nexit 0\n")
	tc, _ := newTestToolchain(t, root, tarball, sum)
	p, err := tc.Ensure(context.Background())
	require.NoError(t, err)
	out, err := os.ReadFile(p)
	require.NoError(t, err)
	assert.Contains(t, string(out), BorgmaticVersion)
}

// "2.1.70" contains "2.1.7"; a substring version check would keep an
// unexpectedly upgraded launcher forever. The comparison is exact.
func TestWrongVersionByPrefixIsReprovisioned(t *testing.T) {
	root := t.TempDir()
	vdir := filepath.Join(root, "versions", versionDirName())
	require.NoError(t, os.MkdirAll(filepath.Join(vdir, "bin"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(vdir, "bin", "borgmatic"),
		[]byte("#!/bin/sh\necho "+BorgmaticVersion+"0\n"), 0o755))
	require.NoError(t, os.Symlink(filepath.Join("versions", versionDirName()), filepath.Join(root, "current")))

	tarball, sum := uvTarball(t, "#!/bin/sh\nexit 0\n")
	tc, envs := newTestToolchain(t, root, tarball, sum)
	_, err := tc.Ensure(context.Background())
	require.NoError(t, err)
	require.Len(t, *envs, 1, "the prefix-matching version must be rebuilt")
}

// The probes themselves must not read the host's Python configuration: a
// poisoned PYTHONHOME failing the health check would refuse the very
// toolchain that exists to escape it.
func TestProbesIgnoreHostPythonEnv(t *testing.T) {
	t.Setenv("PYTHONHOME", "/definitely/broken")
	root := t.TempDir()
	vdir := filepath.Join(root, "versions", versionDirName())
	require.NoError(t, os.MkdirAll(filepath.Join(vdir, "bin"), 0o755))
	// Fails when PYTHONHOME leaks through, healthy otherwise.
	require.NoError(t, os.WriteFile(filepath.Join(vdir, "bin", "borgmatic"),
		[]byte("#!/bin/sh\n[ -n \"$PYTHONHOME\" ] && exit 1\necho "+BorgmaticVersion+"\n"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(vdir, "manifest.json"), []byte("{}"), 0o644))
	require.NoError(t, os.Symlink(filepath.Join("versions", versionDirName()), filepath.Join(root, "current")))

	tc := New(root, slog.New(slog.DiscardHandler))
	p, ok := tc.freshAndHealthy(context.Background())
	require.True(t, ok, "the probe runs on a sanitized environment")
	assert.Equal(t, filepath.Join(root, "current", "bin", "borgmatic"), p)
}

// While a provisioner holds the lock, the fast path must not retire anything:
// a staged generation past its manifest write would be marked superseded and
// carry that clock into its life as current.
func TestFastPathCleanupSkipsWhileProvisionLockHeld(t *testing.T) {
	root := t.TempDir()
	vdir := filepath.Join(root, "versions", versionDirName())
	require.NoError(t, os.MkdirAll(filepath.Join(vdir, "bin"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(vdir, "bin", "borgmatic"),
		[]byte("#!/bin/sh\necho "+BorgmaticVersion+"\n"), 0o755))
	require.NoError(t, os.Symlink(filepath.Join("versions", versionDirName()), filepath.Join(root, "current")))

	// A finished-looking old version that WOULD be retired.
	old := filepath.Join(root, "versions", "uv0.0.1-py3.12-borgmatic2.0.0")
	require.NoError(t, os.MkdirAll(old, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(old, "manifest.json"), []byte("{}"), 0o644))

	held, acquired, err := lockfile.TryExclusive(filepath.Join(root, "provision.lock"))
	require.NoError(t, err)
	require.True(t, acquired)
	defer held.Release()

	tc := New(root, slog.New(slog.DiscardHandler))
	_, err = tc.Ensure(context.Background())
	require.NoError(t, err, "the fresh fast path does not wait on the lock")
	_, statErr := os.Stat(filepath.Join(old, ".superseded"))
	assert.True(t, os.IsNotExist(statErr), "no retirement while another process provisions")
}

// A launcher truncated into a zero-exit no-op must not degrade-pass: it would
// sail through preflight's floor (unparseable versions pass there by design)
// and record no-op invocations as successful backups.
func TestDegradeRejectsANoOpLauncher(t *testing.T) {
	root := t.TempDir()
	old := filepath.Join(root, "versions", "uv0.0.1-py3.12-borgmatic2.0.0")
	require.NoError(t, os.MkdirAll(filepath.Join(old, "bin"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(old, "bin", "borgmatic"),
		[]byte("#!/bin/sh\nexit 0\n"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(old, "manifest.json"), []byte("{}"), 0o644))
	require.NoError(t, os.Symlink(filepath.Join("versions", "uv0.0.1-py3.12-borgmatic2.0.0"), filepath.Join(root, "current")))

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "no network", http.StatusBadGateway)
	}))
	defer srv.Close()
	tc := New(root, slog.New(slog.DiscardHandler))
	tc.downloadBase = srv.URL

	_, err := tc.Ensure(context.Background())
	require.Error(t, err, "exit 0 with no version is not borgmatic; degrading to it would fake successful backups")
}
