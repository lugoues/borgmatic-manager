package runtime_test

import (
	"context"
	"io"
	"strings"
	"testing"

	"github.com/docker/docker/api/types/mount"
	"github.com/lugoues/borgmatic-manager/internal/runtime"
	"github.com/stretchr/testify/assert"
)

// TestContainerConfigFields verifies that ContainerConfig has all required fields.
func TestContainerConfigFields(t *testing.T) {
	cfg := runtime.ContainerConfig{
		Image:      "ghcr.io/borgmatic-collective/borgmatic:latest",
		GroupName:  "nextcloud",
		ConfigPath: "/etc/borgmatic/generated/nextcloud.yaml",
		Mounts: []mount.Mount{
			{Type: mount.TypeBind, Source: "/etc/borgmatic/generated/nextcloud.yaml", Target: "/etc/borgmatic/config.yaml", ReadOnly: true},
		},
		Networks: []string{"db_net"},
		Cmd:      []string{"borgmatic", "create"},
	}

	assert.Equal(t, "ghcr.io/borgmatic-collective/borgmatic:latest", cfg.Image)
	assert.Equal(t, "nextcloud", cfg.GroupName)
	assert.Equal(t, "/etc/borgmatic/generated/nextcloud.yaml", cfg.ConfigPath)
	assert.Len(t, cfg.Mounts, 1)
	assert.Equal(t, []string{"db_net"}, cfg.Networks)
	assert.Equal(t, []string{"borgmatic", "create"}, cfg.Cmd)
}

// TestMockRuntimePhase3Methods verifies MockRuntime implements all Phase 3 methods via mock.Called.
func TestMockRuntimePhase3Methods(t *testing.T) {
	m := &runtime.MockRuntime{}
	ctx := context.Background()

	cfg := runtime.ContainerConfig{
		Image:     "borgmatic:latest",
		GroupName: "test",
		Cmd:       []string{"borgmatic", "create"},
	}

	// CreateContainer
	m.On("CreateContainer", ctx, cfg).Return("container-123", nil)
	id, err := m.CreateContainer(ctx, cfg)
	assert.NoError(t, err)
	assert.Equal(t, "container-123", id)

	// StartContainer
	m.On("StartContainer", ctx, "container-123").Return(nil)
	err = m.StartContainer(ctx, "container-123")
	assert.NoError(t, err)

	// ContainerNetworkConnect
	m.On("ContainerNetworkConnect", ctx, "net-1", "container-123").Return(nil)
	err = m.ContainerNetworkConnect(ctx, "net-1", "container-123")
	assert.NoError(t, err)

	// ContainerLogs
	logReader := io.NopCloser(strings.NewReader("log output"))
	m.On("ContainerLogs", ctx, "container-123").Return(logReader, nil)
	reader, err := m.ContainerLogs(ctx, "container-123")
	assert.NoError(t, err)
	assert.NotNil(t, reader)

	// WaitContainer
	m.On("WaitContainer", ctx, "container-123").Return(int64(0), nil)
	code, err := m.WaitContainer(ctx, "container-123")
	assert.NoError(t, err)
	assert.Equal(t, int64(0), code)

	// RemoveContainer
	m.On("RemoveContainer", ctx, "container-123").Return(nil)
	err = m.RemoveContainer(ctx, "container-123")
	assert.NoError(t, err)

	m.AssertExpectations(t)
}
