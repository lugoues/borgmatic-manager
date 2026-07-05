package discovery_test

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/lugoues/borgmatic-manager/internal/discovery"
	"github.com/lugoues/borgmatic-manager/internal/runtime"
)

// stubProbes makes every mountpoint look mounted and readable so tests can
// use fixture paths that don't exist on the test host.
func stubProbes(t *testing.T) {
	t.Helper()
	restore := discovery.StubFSProbes(
		func(string) bool { return true },
		func(string) bool { return true },
	)
	t.Cleanup(restore)
}

// localVolume builds a backup-labeled local-driver volume fixture.
func localVolume(name, group string) runtime.VolumeInfo {
	return runtime.VolumeInfo{
		Name:       name,
		Driver:     "local",
		Mountpoint: "/var/lib/docker/volumes/" + name + "/_data",
		Labels: map[string]string{
			"borgmatic-manager.backup": "true",
			"borgmatic-manager.group":  group,
		},
	}
}

func TestDiscover(t *testing.T) {
	stubProbes(t)
	rt := &runtime.MockRuntime{}
	rt.On("ListVolumes", mock.Anything).Return([]runtime.VolumeInfo{
		localVolume("app-data", "myapp"),
		localVolume("logs", "myapp"),
		localVolume("other-vol", "other"),
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

func TestDiscoverHostPathFromMountpoint(t *testing.T) {
	stubProbes(t)
	rt := &runtime.MockRuntime{}
	rt.On("ListVolumes", mock.Anything).Return([]runtime.VolumeInfo{
		localVolume("my-volume", "app"),
	}, nil)
	rt.On("ListContainers", mock.Anything).Return([]runtime.ContainerInfo{}, nil)

	state, err := discovery.Discover(context.Background(), rt, discardLogger())
	require.NoError(t, err)

	vol := state.Groups["app"].Volumes[0]
	assert.Equal(t, "/var/lib/docker/volumes/my-volume/_data", vol.HostPath,
		"HostPath must be the runtime-reported mountpoint, not a fabricated path")

	rt.AssertExpectations(t)
}

func TestDiscoverUnlabeledVolumesIgnoredSilently(t *testing.T) {
	stubProbes(t)
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))

	rt := &runtime.MockRuntime{}
	rt.On("ListVolumes", mock.Anything).Return([]runtime.VolumeInfo{
		{Name: "plain", Driver: "local", Mountpoint: "/var/lib/docker/volumes/plain/_data"},
		localVolume("labeled", "app"),
	}, nil)
	rt.On("ListContainers", mock.Anything).Return([]runtime.ContainerInfo{}, nil)

	state, err := discovery.Discover(context.Background(), rt, logger)
	require.NoError(t, err)

	assert.Len(t, state.Groups, 1)
	assert.NotContains(t, buf.String(), "plain", "unlabeled volumes must not produce warnings")

	rt.AssertExpectations(t)
}

func TestDiscoverNearMissVolumeWarns(t *testing.T) {
	stubProbes(t)
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))

	rt := &runtime.MockRuntime{}
	rt.On("ListVolumes", mock.Anything).Return([]runtime.VolumeInfo{
		{
			Name:       "typo-vol",
			Driver:     "local",
			Mountpoint: "/var/lib/docker/volumes/typo-vol/_data",
			Labels: map[string]string{
				"borgmatic-manager.backup": "True", // wrong case: not "true"
				"borgmatic-manager.group":  "app",
			},
		},
	}, nil)
	rt.On("ListContainers", mock.Anything).Return([]runtime.ContainerInfo{}, nil)

	state, err := discovery.Discover(context.Background(), rt, logger)
	require.NoError(t, err)

	assert.Empty(t, state.Groups)
	assert.Contains(t, buf.String(), "not enabled")
	assert.Contains(t, buf.String(), "typo-vol")

	rt.AssertExpectations(t)
}

func TestDiscoverSkipsNonLocalDriver(t *testing.T) {
	stubProbes(t)
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))

	vol := localVolume("plugin-vol", "app")
	vol.Driver = "rclone"

	rt := &runtime.MockRuntime{}
	rt.On("ListVolumes", mock.Anything).Return([]runtime.VolumeInfo{vol}, nil)
	rt.On("ListContainers", mock.Anything).Return([]runtime.ContainerInfo{}, nil)

	state, err := discovery.Discover(context.Background(), rt, logger)
	require.NoError(t, err)

	assert.Empty(t, state.Groups)
	assert.Contains(t, buf.String(), "skipping volume")
	assert.Contains(t, buf.String(), "rclone")

	rt.AssertExpectations(t)
}

func TestDiscoverSkipsUnmountedLazyVolume(t *testing.T) {
	restore := discovery.StubFSProbes(
		func(string) bool { return false }, // nothing is mounted
		func(string) bool { return true },
	)
	t.Cleanup(restore)

	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))

	nfsVol := localVolume("nfs-vol", "app")
	nfsVol.Options = map[string]string{"type": "nfs", "device": ":/export", "o": "addr=10.0.0.1"}

	rt := &runtime.MockRuntime{}
	rt.On("ListVolumes", mock.Anything).Return([]runtime.VolumeInfo{
		nfsVol,
		localVolume("plain-vol", "app"), // no options: mount state is irrelevant
	}, nil)
	rt.On("ListContainers", mock.Anything).Return([]runtime.ContainerInfo{}, nil)

	state, err := discovery.Discover(context.Background(), rt, logger)
	require.NoError(t, err)

	require.Contains(t, state.Groups, "app")
	assert.Len(t, state.Groups["app"].Volumes, 1)
	assert.Equal(t, "plain-vol", state.Groups["app"].Volumes[0].Name)
	assert.Contains(t, buf.String(), "not currently mounted")

	rt.AssertExpectations(t)
}

func TestDiscoverBacksUpMountedLazyVolume(t *testing.T) {
	restore := discovery.StubFSProbes(
		func(string) bool { return true }, // the NFS volume is currently mounted
		func(string) bool { return true },
	)
	t.Cleanup(restore)

	nfsVol := localVolume("nfs-vol", "app")
	nfsVol.Options = map[string]string{"type": "nfs"}

	rt := &runtime.MockRuntime{}
	rt.On("ListVolumes", mock.Anything).Return([]runtime.VolumeInfo{nfsVol}, nil)
	rt.On("ListContainers", mock.Anything).Return([]runtime.ContainerInfo{}, nil)

	state, err := discovery.Discover(context.Background(), rt, discardLogger())
	require.NoError(t, err)

	require.Contains(t, state.Groups, "app")
	assert.Len(t, state.Groups["app"].Volumes, 1)

	rt.AssertExpectations(t)
}

func TestDiscoverSkipsUnreadableVolume(t *testing.T) {
	restore := discovery.StubFSProbes(
		func(string) bool { return true },
		func(path string) bool { return !strings.Contains(path, "secret") },
	)
	t.Cleanup(restore)

	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))

	rt := &runtime.MockRuntime{}
	rt.On("ListVolumes", mock.Anything).Return([]runtime.VolumeInfo{
		localVolume("secret-vol", "app"),
		localVolume("open-vol", "app"),
	}, nil)
	rt.On("ListContainers", mock.Anything).Return([]runtime.ContainerInfo{}, nil)

	state, err := discovery.Discover(context.Background(), rt, logger)
	require.NoError(t, err)

	require.Contains(t, state.Groups, "app")
	assert.Len(t, state.Groups["app"].Volumes, 1)
	assert.Equal(t, "open-vol", state.Groups["app"].Volumes[0].Name)
	assert.Contains(t, buf.String(), "not readable")

	rt.AssertExpectations(t)
}

func TestDiscoverContainerDatabases(t *testing.T) {
	stubProbes(t)
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
	assert.Equal(t, "postgres-svc", dbs[0].Container,
		"discovery must attach the source container name for container-mode connections")

	rt.AssertExpectations(t)
}

func TestDiscoverNearMissContainerWarns(t *testing.T) {
	stubProbes(t)
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))

	rt := &runtime.MockRuntime{}
	rt.On("ListVolumes", mock.Anything).Return([]runtime.VolumeInfo{}, nil)
	rt.On("ListContainers", mock.Anything).Return([]runtime.ContainerInfo{
		{
			ID:   "c1",
			Name: "pg-no-group",
			Labels: map[string]string{
				// db labels but no group label
				"borgmatic-manager.db.0.type":     "postgresql",
				"borgmatic-manager.db.0.name":     "appdb",
				"borgmatic-manager.db.0.username": "admin",
			},
		},
		{
			ID:     "c2",
			Name:   "unrelated",
			Labels: map[string]string{"com.example": "x"},
		},
	}, nil)

	state, err := discovery.Discover(context.Background(), rt, logger)
	require.NoError(t, err)

	assert.Empty(t, state.Groups)
	assert.Contains(t, buf.String(), "no group label")
	assert.Contains(t, buf.String(), "pg-no-group")
	assert.NotContains(t, buf.String(), "unrelated", "containers without manager labels stay silent")

	rt.AssertExpectations(t)
}

func TestDiscoverSQLitePathResolution(t *testing.T) {
	stubProbes(t)
	rt := &runtime.MockRuntime{}
	rt.On("ListVolumes", mock.Anything).Return([]runtime.VolumeInfo{
		// Not backup-labeled: sqlite references must resolve against ALL volumes.
		{Name: "app-data", Driver: "local", Mountpoint: "/var/lib/docker/volumes/app-data/_data"},
	}, nil)
	rt.On("ListContainers", mock.Anything).Return([]runtime.ContainerInfo{
		{
			ID:   "c1",
			Name: "app",
			Labels: map[string]string{
				"borgmatic-manager.group":       "myapp",
				"borgmatic-manager.db.0.type":   "sqlite",
				"borgmatic-manager.db.0.name":   "app",
				"borgmatic-manager.db.0.volume": "app-data",
				"borgmatic-manager.db.0.path":   "db/app.sqlite3",
			},
		},
	}, nil)

	state, err := discovery.Discover(context.Background(), rt, discardLogger())
	require.NoError(t, err)

	require.Contains(t, state.Groups, "myapp")
	dbs := state.Groups["myapp"].Databases
	require.Len(t, dbs, 1)
	assert.Equal(t, "/var/lib/docker/volumes/app-data/_data/db/app.sqlite3", dbs[0].Path)

	rt.AssertExpectations(t)
}

func TestDiscoverSQLiteUnknownVolumeSkipped(t *testing.T) {
	stubProbes(t)
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))

	rt := &runtime.MockRuntime{}
	rt.On("ListVolumes", mock.Anything).Return([]runtime.VolumeInfo{}, nil)
	rt.On("ListContainers", mock.Anything).Return([]runtime.ContainerInfo{
		{
			ID:   "c1",
			Name: "app",
			Labels: map[string]string{
				"borgmatic-manager.group":       "myapp",
				"borgmatic-manager.db.0.type":   "sqlite",
				"borgmatic-manager.db.0.name":   "app",
				"borgmatic-manager.db.0.volume": "no-such-volume",
				"borgmatic-manager.db.0.path":   "app.sqlite3",
			},
		},
	}, nil)

	state, err := discovery.Discover(context.Background(), rt, logger)
	require.NoError(t, err)

	assert.Empty(t, state.Groups)
	assert.Contains(t, buf.String(), "unknown volume")

	rt.AssertExpectations(t)
}

func TestDiscoverMixedGrouping(t *testing.T) {
	stubProbes(t)
	rt := &runtime.MockRuntime{}
	rt.On("ListVolumes", mock.Anything).Return([]runtime.VolumeInfo{
		localVolume("app-data", "myapp"),
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
	stubProbes(t)
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))

	orphan := runtime.VolumeInfo{
		Name:       "orphan-vol",
		Driver:     "local",
		Mountpoint: "/var/lib/docker/volumes/orphan-vol/_data",
		Labels: map[string]string{
			"borgmatic-manager.backup": "true",
			// no group label
		},
	}

	rt := &runtime.MockRuntime{}
	rt.On("ListVolumes", mock.Anything).Return([]runtime.VolumeInfo{
		orphan,
		localVolume("good-vol", "app"),
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
	require.Error(t, err)
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
	require.Error(t, err)
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

func TestMountPointsParsing(t *testing.T) {
	mountinfo := `21 26 0:19 / /proc rw,nosuid,nodev,noexec,relatime shared:12 - proc proc rw
26 1 8:1 / / rw,relatime shared:1 - ext4 /dev/sda1 rw
101 26 0:44 / /var/lib/docker/volumes/nfs-vol/_data rw,relatime shared:60 - nfs4 10.0.0.1:/export rw
102 26 0:45 / /mnt/with\040space rw - ext4 /dev/sdb1 rw
malformed line
`
	points := discovery.MountPointsFromReader(strings.NewReader(mountinfo))
	assert.True(t, points["/proc"])
	assert.True(t, points["/var/lib/docker/volumes/nfs-vol/_data"])
	assert.True(t, points["/mnt/with space"], "octal escapes must be decoded")
	assert.False(t, points["/nonexistent"])
}
