package runtime

import (
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// listenUnix creates a real unix socket at path and cleans it up with the test.
func listenUnix(t *testing.T, path string) {
	t.Helper()
	require := func(err error) {
		if err != nil {
			t.Fatal(err)
		}
	}
	require(os.MkdirAll(filepath.Dir(path), 0o755))
	l, err := net.Listen("unix", path)
	require(err)
	t.Cleanup(func() { _ = l.Close() })
}

func TestFirstSocketPicksFirstExisting(t *testing.T) {
	dir := t.TempDir()
	docker := filepath.Join(dir, "docker.sock")
	podman := filepath.Join(dir, "podman.sock")
	listenUnix(t, podman)

	if got := firstSocket([]string{docker, podman}); got != podman {
		t.Fatalf("expected %s (only existing socket), got %q", podman, got)
	}

	listenUnix(t, docker)
	if got := firstSocket([]string{docker, podman}); got != docker {
		t.Fatalf("candidate order must win when both exist, got %q", got)
	}
}

func TestFirstSocketIgnoresPlainFiles(t *testing.T) {
	dir := t.TempDir()
	notASocket := filepath.Join(dir, "docker.sock")
	if err := os.WriteFile(notASocket, nil, 0o644); err != nil {
		t.Fatal(err)
	}

	if got := firstSocket([]string{notASocket}); got != "" {
		t.Fatalf("plain files must not count as sockets, got %q", got)
	}
}

func TestResolveSocketPathEnvOverrideWins(t *testing.T) {
	t.Setenv("CONTAINER_SOCKET", "/nonexistent/custom.sock")

	got, err := resolveSocketPath()
	if err != nil {
		t.Fatal(err)
	}
	// An explicit override is honored even if absent, the daemon may simply
	// not be up yet; the preflight reports reachability separately.
	if got != "/nonexistent/custom.sock" {
		t.Fatalf("explicit CONTAINER_SOCKET must win, got %q", got)
	}
}

func TestResolveSocketPathErrorIsActionable(t *testing.T) {
	t.Setenv("CONTAINER_SOCKET", "")
	// Point the podman-user candidate somewhere empty; the well-known system
	// paths may exist on the test host, in which case resolution succeeds and
	// the error path can't be forced, only assert when it fails.
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())

	if _, err := resolveSocketPath(); err != nil {
		for _, want := range []string{"podman.socket", "CONTAINER_SOCKET", "/var/run/docker.sock"} {
			if !strings.Contains(err.Error(), want) {
				t.Fatalf("error should mention %q: %v", want, err)
			}
		}
	}
}
