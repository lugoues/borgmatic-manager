package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/lugoues/borgmatic-manager/internal/config"
)

// fakeCLIs puts stub docker and podman executables on PATH.
func fakeCLIs(t *testing.T, names ...string) {
	t.Helper()
	dir := t.TempDir()
	for _, name := range names {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("#!/bin/sh\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", dir)
}

func TestDetectContainerCLI(t *testing.T) {
	plain := &config.ManagerConfig{}

	// The CLI must match the engine the manager watches: a docker CLI
	// pointed at the docker daemon can't reach containers on the podman
	// socket, so a podman socket prefers the podman CLI even when both
	// are installed.
	fakeCLIs(t, "docker", "podman")
	if got := detectContainerCLI(plain, "/run/podman/podman.sock"); got != "podman" {
		t.Fatalf("podman socket must prefer the podman CLI, got %q", got)
	}
	if got := detectContainerCLI(plain, "/var/run/docker.sock"); got != "docker" {
		t.Fatalf("docker socket must pick the docker CLI, got %q", got)
	}

	// Explicit config wins over everything.
	override := &config.ManagerConfig{Manager: config.ManagerSettings{ContainerCLI: "podman"}}
	if got := detectContainerCLI(override, "/var/run/docker.sock"); got != "podman" {
		t.Fatalf("manager.container_cli must win, got %q", got)
	}

	// Podman socket but only docker installed: fall back rather than
	// return a CLI that isn't there.
	fakeCLIs(t, "docker")
	if got := detectContainerCLI(plain, "/run/podman/podman.sock"); got != "docker" {
		t.Fatalf("expected docker fallback, got %q", got)
	}

	fakeCLIs(t)
	if got := detectContainerCLI(plain, "/run/podman/podman.sock"); got != "" {
		t.Fatalf("no CLIs on PATH must yield empty, got %q", got)
	}
}
