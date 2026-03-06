package discovery_test

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"testing"

	"github.com/lugoues/borgmatic-manager/internal/discovery"
	"github.com/lugoues/borgmatic-manager/internal/runtime"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

func TestDiscover(t *testing.T) {
	rt := &runtime.MockRuntime{}
	rt.On("ListVolumes", mock.Anything).Return([]runtime.VolumeInfo{
		{Name: "app-data", Labels: map[string]string{
			"borgmatic-manager.backup": "true",
			"borgmatic-manager.group":  "myapp",
		}},
		{Name: "logs", Labels: map[string]string{
			"borgmatic-manager.backup": "true",
			"borgmatic-manager.group":  "myapp",
		}},
		{Name: "other-vol", Labels: map[string]string{
			"borgmatic-manager.backup": "true",
			"borgmatic-manager.group":  "other",
		}},
	}, nil)
	rt.On("ListContainers", mock.Anything).Return([]runtime.ContainerInfo{}, nil)

	state, err := discovery.Discover(context.Background(), rt, discardLogger())
	require.NoError(t, err)
	require.NotNil(t, state)

	assert.Len(t, state.Groups, 2)
	assert.Len(t, state.Groups["myapp"].Volumes, 2)
	assert.Len(t, state.Groups["other"].Volumes, 1)
	assert.Equal(t, "app-data", state.Groups["myapp"].Volumes[0].Name)
	assert.Equal(t, "logs", state.Groups["myapp"].Volumes[1].Name)
	assert.Equal(t, "other-vol", state.Groups["other"].Volumes[0].Name)

	rt.AssertExpectations(t)
}

func TestDiscoverMountPath(t *testing.T) {
	rt := &runtime.MockRuntime{}
	rt.On("ListVolumes", mock.Anything).Return([]runtime.VolumeInfo{
		{Name: "my-volume", Labels: map[string]string{
			"borgmatic-manager.backup": "true",
			"borgmatic-manager.group":  "app",
		}},
	}, nil)
	rt.On("ListContainers", mock.Anything).Return([]runtime.ContainerInfo{}, nil)

	state, err := discovery.Discover(context.Background(), rt, discardLogger())
	require.NoError(t, err)

	vol := state.Groups["app"].Volumes[0]
	assert.Equal(t, "/mnt/sources/my-volume", vol.MountPath)

	rt.AssertExpectations(t)
}

func TestDiscoverContainerDatabases(t *testing.T) {
	rt := &runtime.MockRuntime{}
	rt.On("ListVolumes", mock.Anything).Return([]runtime.VolumeInfo{}, nil)
	rt.On("ListContainers", mock.Anything).Return([]runtime.ContainerInfo{
		{
			ID:   "abc123",
			Name: "postgres-svc",
			Labels: map[string]string{
				"borgmatic-manager.group":         "myapp",
				"borgmatic-manager.db.0.type":     "postgresql",
				"borgmatic-manager.db.0.name":     "appdb",
				"borgmatic-manager.db.0.username": "admin",
				"borgmatic-manager.db.0.hostname": "postgres-svc",
			},
		},
	}, nil)

	state, err := discovery.Discover(context.Background(), rt, discardLogger())
	require.NoError(t, err)

	require.Contains(t, state.Groups, "myapp")
	dbs := state.Groups["myapp"].Databases
	require.Len(t, dbs, 1)
	assert.Equal(t, "postgresql", dbs[0].Type)
	assert.Equal(t, "appdb", dbs[0].Name)
	assert.Equal(t, "admin", dbs[0].Username)
	assert.Equal(t, "postgres-svc", dbs[0].Hostname)

	rt.AssertExpectations(t)
}

func TestDiscoverMixedGrouping(t *testing.T) {
	rt := &runtime.MockRuntime{}
	rt.On("ListVolumes", mock.Anything).Return([]runtime.VolumeInfo{
		{Name: "app-data", Labels: map[string]string{
			"borgmatic-manager.backup": "true",
			"borgmatic-manager.group":  "myapp",
		}},
	}, nil)
	rt.On("ListContainers", mock.Anything).Return([]runtime.ContainerInfo{
		{
			ID:   "db1",
			Name: "pg",
			Labels: map[string]string{
				"borgmatic-manager.group":         "myapp",
				"borgmatic-manager.db.0.type":     "postgresql",
				"borgmatic-manager.db.0.name":     "appdb",
				"borgmatic-manager.db.0.username": "admin",
			},
		},
	}, nil)

	state, err := discovery.Discover(context.Background(), rt, discardLogger())
	require.NoError(t, err)

	require.Contains(t, state.Groups, "myapp")
	g := state.Groups["myapp"]
	assert.Len(t, g.Volumes, 1, "should have one volume")
	assert.Len(t, g.Databases, 1, "should have one database")
	assert.Equal(t, "app-data", g.Volumes[0].Name)
	assert.Equal(t, "postgresql", g.Databases[0].Type)

	rt.AssertExpectations(t)
}

func TestDiscoverSkipsVolumeWithoutGroup(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))

	rt := &runtime.MockRuntime{}
	rt.On("ListVolumes", mock.Anything).Return([]runtime.VolumeInfo{
		{Name: "orphan-vol", Labels: map[string]string{
			"borgmatic-manager.backup": "true",
			// no group label
		}},
		{Name: "good-vol", Labels: map[string]string{
			"borgmatic-manager.backup": "true",
			"borgmatic-manager.group":  "app",
		}},
	}, nil)
	rt.On("ListContainers", mock.Anything).Return([]runtime.ContainerInfo{}, nil)

	state, err := discovery.Discover(context.Background(), rt, logger)
	require.NoError(t, err)

	// Only the good volume should be present.
	assert.Len(t, state.Groups, 1)
	assert.Len(t, state.Groups["app"].Volumes, 1)

	// Check that a warning was logged about the orphan volume.
	assert.Contains(t, buf.String(), "volume has backup=true but no group label")
	assert.Contains(t, buf.String(), "orphan-vol")

	rt.AssertExpectations(t)
}

func TestDiscoverVolumeListError(t *testing.T) {
	rt := &runtime.MockRuntime{}
	rt.On("ListVolumes", mock.Anything).Return([]runtime.VolumeInfo(nil), fmt.Errorf("socket unreachable"))

	state, err := discovery.Discover(context.Background(), rt, discardLogger())
	assert.Nil(t, state)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "listing volumes")
	assert.Contains(t, err.Error(), "socket unreachable")

	rt.AssertExpectations(t)
}

func TestDiscoverContainerListError(t *testing.T) {
	rt := &runtime.MockRuntime{}
	rt.On("ListVolumes", mock.Anything).Return([]runtime.VolumeInfo{}, nil)
	rt.On("ListContainers", mock.Anything).Return([]runtime.ContainerInfo(nil), fmt.Errorf("permission denied"))

	state, err := discovery.Discover(context.Background(), rt, discardLogger())
	assert.Nil(t, state)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "listing containers")
	assert.Contains(t, err.Error(), "permission denied")

	rt.AssertExpectations(t)
}

func TestDiscoverEmptyState(t *testing.T) {
	rt := &runtime.MockRuntime{}
	rt.On("ListVolumes", mock.Anything).Return([]runtime.VolumeInfo{}, nil)
	rt.On("ListContainers", mock.Anything).Return([]runtime.ContainerInfo{}, nil)

	state, err := discovery.Discover(context.Background(), rt, discardLogger())
	require.NoError(t, err)
	require.NotNil(t, state, "should return empty BackupState, not nil")
	assert.Empty(t, state.Groups)

	rt.AssertExpectations(t)
}
