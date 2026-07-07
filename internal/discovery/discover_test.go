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

func volumeFixture(name string) runtime.VolumeInfo {
	return runtime.VolumeInfo{
		Name:       name,
		Driver:     "local",
		Mountpoint: "/var/lib/docker/volumes/" + name + "/_data",
	}
}

func mountFixture(name, dest string) runtime.VolumeMount {
	return runtime.VolumeMount{
		Name:        name,
		Source:      "/var/lib/docker/volumes/" + name + "/_data",
		Destination: dest,
	}
}

// backupContainer builds a backup-enabled container fixture with the given
// group and volume mounts.
func backupContainer(name, group string, mounts ...runtime.VolumeMount) runtime.ContainerInfo {
	return runtime.ContainerInfo{
		ID:    "id-" + name,
		Name:  name,
		Image: "example/" + name + ":1",
		Labels: map[string]string{
			"borgmatic-manager.enable": "true",
			"borgmatic-manager.group":  group,
		},
		Mounts: mounts,
	}
}

func mockLists(vols []runtime.VolumeInfo, ctrs []runtime.ContainerInfo) *runtime.MockRuntime {
	rt := &runtime.MockRuntime{}
	rt.On("ListVolumes", mock.Anything).Return(vols, nil)
	rt.On("ListContainers", mock.Anything).Return(ctrs, nil)
	return rt
}

func TestDiscoverContainerVolumes(t *testing.T) {
	stubProbes(t)
	rt := mockLists(
		[]runtime.VolumeInfo{volumeFixture("app-data"), volumeFixture("app-uploads"), volumeFixture("unrelated")},
		[]runtime.ContainerInfo{
			backupContainer("web", "myapp",
				mountFixture("app-data", "/data"),
				mountFixture("app-uploads", "/uploads"),
			),
		},
	)

	state, err := discovery.Discover(context.Background(), rt, discardLogger())
	require.NoError(t, err)

	require.Contains(t, state.Groups, "myapp")
	vols := state.Groups["myapp"].Volumes
	require.Len(t, vols, 2, "all named volumes of a backup-enabled container are included")
	assert.Equal(t, "app-data", vols[0].Name)
	assert.Equal(t, "/var/lib/docker/volumes/app-data/_data", vols[0].HostPath)
	assert.Equal(t, "app-uploads", vols[1].Name)

	rt.AssertExpectations(t)
}

func TestDiscoverVolumesFilterLabel(t *testing.T) {
	stubProbes(t)
	c := backupContainer("web", "myapp",
		mountFixture("app-data", "/data"),
		mountFixture("app-cache", "/cache"),
		mountFixture("app-uploads", "/uploads"),
	)
	c.Labels["borgmatic-manager.volumes"] = "app-data, /uploads"

	rt := mockLists(
		[]runtime.VolumeInfo{volumeFixture("app-data"), volumeFixture("app-cache"), volumeFixture("app-uploads")},
		[]runtime.ContainerInfo{c},
	)

	state, err := discovery.Discover(context.Background(), rt, discardLogger())
	require.NoError(t, err)

	vols := state.Groups["myapp"].Volumes
	require.Len(t, vols, 2, "filter matches by volume name or mount destination")
	assert.Equal(t, "app-data", vols[0].Name)
	assert.Equal(t, "app-uploads", vols[1].Name)
}

func TestDiscoverVolumesFilterUnmatchedEntryWarns(t *testing.T) {
	stubProbes(t)
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))

	c := backupContainer("web", "myapp", mountFixture("app-data", "/data"))
	c.Labels["borgmatic-manager.volumes"] = "app-data,typo-vol"

	rt := mockLists([]runtime.VolumeInfo{volumeFixture("app-data")}, []runtime.ContainerInfo{c})

	_, err := discovery.Discover(context.Background(), rt, logger)
	require.NoError(t, err)
	assert.Contains(t, buf.String(), "matched no attached volume")
	assert.Contains(t, buf.String(), "typo-vol")
}

func TestDiscoverAnonymousVolumesExcludedByDefault(t *testing.T) {
	stubProbes(t)
	anon := strings.Repeat("ab12", 16) // 64 hex chars
	rt := mockLists(
		[]runtime.VolumeInfo{volumeFixture("app-data"), volumeFixture(anon)},
		[]runtime.ContainerInfo{
			backupContainer("web", "myapp",
				mountFixture("app-data", "/data"),
				mountFixture(anon, "/tmp/cache"),
			),
		},
	)

	state, err := discovery.Discover(context.Background(), rt, discardLogger())
	require.NoError(t, err)

	vols := state.Groups["myapp"].Volumes
	require.Len(t, vols, 1, "anonymous volumes hold caches more often than data")
	assert.Equal(t, "app-data", vols[0].Name)
}

func TestDiscoverSharedVolumeDeduped(t *testing.T) {
	stubProbes(t)
	rt := mockLists(
		[]runtime.VolumeInfo{volumeFixture("shared")},
		[]runtime.ContainerInfo{
			backupContainer("app-a", "myapp", mountFixture("shared", "/data")),
			backupContainer("app-b", "myapp", mountFixture("shared", "/data")),
		},
	)

	state, err := discovery.Discover(context.Background(), rt, discardLogger())
	require.NoError(t, err)
	assert.Len(t, state.Groups["myapp"].Volumes, 1, "a volume shared by two group members backs up once")
}

func TestDiscoverEnableWithoutTrueDoesNothing(t *testing.T) {
	stubProbes(t)
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))

	c := backupContainer("web", "myapp", mountFixture("app-data", "/data"))
	c.Labels["borgmatic-manager.enable"] = "True" // wrong case

	rt := mockLists([]runtime.VolumeInfo{volumeFixture("app-data")}, []runtime.ContainerInfo{c})

	state, err := discovery.Discover(context.Background(), rt, logger)
	require.NoError(t, err)

	assert.Empty(t, state.Groups)
	assert.Contains(t, buf.String(), `not \"true\"`)
}

func TestDiscoverVolumeLabelsDeprecationWarning(t *testing.T) {
	stubProbes(t)
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))

	v1Volume := volumeFixture("old-style")
	v1Volume.Labels = map[string]string{
		"borgmatic-manager.enable": "true",
		"borgmatic-manager.group":  "legacy",
	}

	rt := mockLists([]runtime.VolumeInfo{v1Volume}, []runtime.ContainerInfo{})

	state, err := discovery.Discover(context.Background(), rt, logger)
	require.NoError(t, err)

	assert.Empty(t, state.Groups, "volume labels no longer create groups")
	assert.Contains(t, buf.String(), "no longer supported")
	assert.Contains(t, buf.String(), "old-style")
}

func TestDiscoverSkipsNonLocalDriver(t *testing.T) {
	stubProbes(t)
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))

	pluginVol := volumeFixture("plugin-vol")
	pluginVol.Driver = "rclone"

	rt := mockLists(
		[]runtime.VolumeInfo{pluginVol},
		[]runtime.ContainerInfo{backupContainer("web", "myapp", mountFixture("plugin-vol", "/data"))},
	)

	state, err := discovery.Discover(context.Background(), rt, logger)
	require.NoError(t, err)

	assert.Empty(t, state.Groups)
	assert.Contains(t, buf.String(), "skipping volume")
	assert.Contains(t, buf.String(), "rclone")
}

func TestDiscoverSkipsUnmountedLazyVolume(t *testing.T) {
	restore := discovery.StubAllFSProbes(
		func(string) bool { return false }, // nothing is mounted
		func(string) bool { return true },
		func(string) bool { return true }, // and the data dir is empty
	)
	t.Cleanup(restore)

	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))

	nfsVol := volumeFixture("nfs-vol")
	nfsVol.Options = map[string]string{"type": "nfs", "device": ":/export"}

	rt := mockLists(
		[]runtime.VolumeInfo{nfsVol, volumeFixture("plain-vol")},
		[]runtime.ContainerInfo{
			backupContainer("web", "myapp",
				mountFixture("nfs-vol", "/nfs"),
				mountFixture("plain-vol", "/data"),
			),
		},
	)

	state, err := discovery.Discover(context.Background(), rt, logger)
	require.NoError(t, err)

	require.Contains(t, state.Groups, "myapp")
	require.Len(t, state.Groups["myapp"].Volumes, 1)
	assert.Equal(t, "plain-vol", state.Groups["myapp"].Volumes[0].Name)
	assert.Contains(t, buf.String(), "not currently mounted")
}

func TestDiscoverBacksUpOptionedVolumeWithData(t *testing.T) {
	// Podman attaches options to ordinary local volumes, so options alone
	// must not trigger the lazy-mount skip when data is visibly present.
	restore := discovery.StubAllFSProbes(
		func(string) bool { return false }, // not a mountpoint
		func(string) bool { return true },
		func(string) bool { return false }, // data directory has entries
	)
	t.Cleanup(restore)

	vol := volumeFixture("pg-data")
	vol.Options = map[string]string{"o": "noquota"}

	rt := mockLists(
		[]runtime.VolumeInfo{vol},
		[]runtime.ContainerInfo{backupContainer("pg", "app", mountFixture("pg-data", "/var/lib/postgresql/data"))},
	)

	state, err := discovery.Discover(context.Background(), rt, discardLogger())
	require.NoError(t, err)

	require.Contains(t, state.Groups, "app")
	assert.Len(t, state.Groups["app"].Volumes, 1, "visible data must always be backed up")
}

func TestDiscoverSkipsUnreadableVolume(t *testing.T) {
	restore := discovery.StubFSProbes(
		func(string) bool { return true },
		func(path string) bool { return !strings.Contains(path, "secret") },
	)
	t.Cleanup(restore)

	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))

	rt := mockLists(
		[]runtime.VolumeInfo{volumeFixture("secret-vol"), volumeFixture("open-vol")},
		[]runtime.ContainerInfo{
			backupContainer("web", "myapp",
				mountFixture("secret-vol", "/secret"),
				mountFixture("open-vol", "/data"),
			),
		},
	)

	state, err := discovery.Discover(context.Background(), rt, logger)
	require.NoError(t, err)

	require.Len(t, state.Groups["myapp"].Volumes, 1)
	assert.Equal(t, "open-vol", state.Groups["myapp"].Volumes[0].Name)
	assert.Contains(t, buf.String(), "not readable")
}

func TestDiscoverContainerDatabases(t *testing.T) {
	stubProbes(t)
	rt := mockLists([]runtime.VolumeInfo{}, []runtime.ContainerInfo{
		{
			ID:    "abc123",
			Name:  "postgres-svc",
			Image: "postgres:17-alpine",
			Labels: map[string]string{
				"borgmatic-manager.group":         "myapp",
				"borgmatic-manager.db.0.type":     "postgresql",
				"borgmatic-manager.db.0.name":     "appdb",
				"borgmatic-manager.db.0.username": "admin",
			},
		},
	})

	state, err := discovery.Discover(context.Background(), rt, discardLogger())
	require.NoError(t, err)

	require.Contains(t, state.Groups, "myapp")
	dbs := state.Groups["myapp"].Databases
	require.Len(t, dbs, 1)
	assert.Equal(t, "postgresql", dbs[0].Type)
	assert.Equal(t, "postgres-svc", dbs[0].Container)
	assert.Equal(t, "postgres:17-alpine", dbs[0].Image,
		"discovery must record the image so helper dumps match the server version")
}

func TestDiscoverConfigLabels(t *testing.T) {
	stubProbes(t)
	c := backupContainer("web", "myapp", mountFixture("app-data", "/data"))
	c.Labels["borgmatic-manager.config.keep_daily"] = "14"
	c.Labels["borgmatic-manager.config.healthchecks.ping_url"] = "https://hc-ping.com/x"

	rt := mockLists([]runtime.VolumeInfo{volumeFixture("app-data")}, []runtime.ContainerInfo{c})

	state, err := discovery.Discover(context.Background(), rt, discardLogger())
	require.NoError(t, err)

	require.Contains(t, state.Groups, "myapp")
	cfgs := state.Groups["myapp"].LabelConfigs
	require.Len(t, cfgs, 1)
	assert.Equal(t, 14, cfgs[0]["keep_daily"], "label values parse as typed YAML")
	hc, ok := cfgs[0]["healthchecks"].(map[string]interface{})
	require.True(t, ok, "dotted paths build nested maps")
	assert.Equal(t, "https://hc-ping.com/x", hc["ping_url"])
}

func TestDiscoverSQLitePathResolution(t *testing.T) {
	stubProbes(t)
	rt := mockLists(
		// Volume not attached to the labeled container and not backed up raw:
		// sqlite references resolve against the full volume list.
		[]runtime.VolumeInfo{volumeFixture("app-data")},
		[]runtime.ContainerInfo{
			{
				ID:    "c1",
				Name:  "app",
				Image: "example/app:1",
				Labels: map[string]string{
					"borgmatic-manager.group":       "myapp",
					"borgmatic-manager.db.0.type":   "sqlite",
					"borgmatic-manager.db.0.name":   "app",
					"borgmatic-manager.db.0.volume": "app-data",
					"borgmatic-manager.db.0.path":   "db/app.sqlite3",
				},
			},
		},
	)

	state, err := discovery.Discover(context.Background(), rt, discardLogger())
	require.NoError(t, err)

	dbs := state.Groups["myapp"].Databases
	require.Len(t, dbs, 1)
	assert.Equal(t, "/var/lib/docker/volumes/app-data/_data/db/app.sqlite3", dbs[0].Path)
}

func TestDiscoverSQLiteUnknownVolumeSkipped(t *testing.T) {
	stubProbes(t)
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))

	rt := mockLists([]runtime.VolumeInfo{}, []runtime.ContainerInfo{
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
	})

	state, err := discovery.Discover(context.Background(), rt, logger)
	require.NoError(t, err)

	assert.Empty(t, state.Groups)
	assert.Contains(t, buf.String(), "unknown volume")
}

func TestDiscoverNearMissContainerWarns(t *testing.T) {
	stubProbes(t)
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))

	rt := mockLists([]runtime.VolumeInfo{}, []runtime.ContainerInfo{
		{
			ID:   "c1",
			Name: "pg-no-group",
			Labels: map[string]string{
				"borgmatic-manager.db.0.type":     "postgresql",
				"borgmatic-manager.db.0.name":     "appdb",
				"borgmatic-manager.db.0.username": "admin",
			},
		},
		{
			ID:     "c2",
			Name:   "grouped-but-empty",
			Labels: map[string]string{"borgmatic-manager.group": "myapp"},
		},
		{
			ID:     "c3",
			Name:   "unrelated",
			Labels: map[string]string{"com.example": "x"},
		},
	})

	state, err := discovery.Discover(context.Background(), rt, logger)
	require.NoError(t, err)

	assert.NotContains(t, state.Groups, "pg-no-group")
	assert.Contains(t, buf.String(), "no group label")
	assert.Contains(t, buf.String(), "pg-no-group")
	assert.Contains(t, buf.String(), "contributes nothing")
	assert.Contains(t, buf.String(), "grouped-but-empty")
	assert.NotContains(t, buf.String(), "unrelated", "containers without manager labels stay silent")
}

func TestDiscoverVolumeListError(t *testing.T) {
	rt := &runtime.MockRuntime{}
	rt.On("ListVolumes", mock.Anything).Return([]runtime.VolumeInfo(nil), fmt.Errorf("socket unreachable"))

	state, err := discovery.Discover(context.Background(), rt, discardLogger())
	assert.Nil(t, state)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "socket unreachable",
		"the runtime's error passes through (it already carries context)")

	rt.AssertExpectations(t)
}

func TestDiscoverContainerListError(t *testing.T) {
	rt := &runtime.MockRuntime{}
	rt.On("ListVolumes", mock.Anything).Return([]runtime.VolumeInfo{}, nil)
	rt.On("ListContainers", mock.Anything).Return([]runtime.ContainerInfo(nil), fmt.Errorf("permission denied"))

	state, err := discovery.Discover(context.Background(), rt, discardLogger())
	assert.Nil(t, state)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "permission denied",
		"the runtime's error passes through (it already carries context)")

	rt.AssertExpectations(t)
}

func TestDiscoverEmptyState(t *testing.T) {
	rt := mockLists([]runtime.VolumeInfo{}, []runtime.ContainerInfo{})

	state, err := discovery.Discover(context.Background(), rt, discardLogger())
	require.NoError(t, err)
	require.NotNil(t, state, "should return empty BackupState, not nil")
	assert.Empty(t, state.Groups)
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

func TestDiscoverSpecLabel(t *testing.T) {
	stubProbes(t)
	spec := `{
	  "group": "myapp",
	  "enable": true,
	  "volumes": ["app-data"],
	  "db": [
	    {"type": "postgresql", "name": "appdb", "username": "postgres", "password": "pw"},
	    {"type": "sqlite", "name": "cache", "volume": "app-data", "path": "cache.db"}
	  ],
	  "config": {"keep_daily": 14, "healthchecks": {"ping_url": "https://hc-ping.com/x"}}
	}`

	c := runtime.ContainerInfo{
		ID:    "id-web",
		Name:  "web",
		Image: "example/web:1",
		Labels: map[string]string{
			"borgmatic-manager.spec": spec,
		},
		Mounts: []runtime.VolumeMount{
			mountFixture("app-data", "/data"),
			mountFixture("app-cache", "/cache"),
		},
	}

	rt := mockLists(
		[]runtime.VolumeInfo{volumeFixture("app-data"), volumeFixture("app-cache")},
		[]runtime.ContainerInfo{c},
	)

	state, err := discovery.Discover(context.Background(), rt, discardLogger())
	require.NoError(t, err)

	require.Contains(t, state.Groups, "myapp")
	g := state.Groups["myapp"]

	require.Len(t, g.Volumes, 1, "spec volumes filter applies")
	assert.Equal(t, "app-data", g.Volumes[0].Name)

	require.Len(t, g.Databases, 2)
	assert.Equal(t, "postgresql", g.Databases[0].Type)
	assert.Equal(t, "web", g.Databases[0].Container, "spec databases get the container attached like flat labels")
	assert.Equal(t, "/var/lib/docker/volumes/app-data/_data/cache.db", g.Databases[1].Path,
		"spec sqlite paths resolve like flat labels")

	require.Len(t, g.LabelConfigs, 1)
	assert.InDelta(t, 14, g.LabelConfigs[0]["keep_daily"], 0, "spec config values come through as JSON numbers")
}

func TestDiscoverSpecShadowsFlatLabels(t *testing.T) {
	stubProbes(t)
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))

	c := runtime.ContainerInfo{
		ID:   "id-web",
		Name: "web",
		Labels: map[string]string{
			"borgmatic-manager.spec":   `{"group": "from-spec", "enable": true}`,
			"borgmatic-manager.group":  "from-flat-labels",
			"borgmatic-manager.enable": "true",
		},
		Mounts: []runtime.VolumeMount{mountFixture("app-data", "/data")},
	}

	rt := mockLists([]runtime.VolumeInfo{volumeFixture("app-data")}, []runtime.ContainerInfo{c})

	state, err := discovery.Discover(context.Background(), rt, logger)
	require.NoError(t, err)

	assert.Contains(t, state.Groups, "from-spec", "the spec is authoritative")
	assert.NotContains(t, state.Groups, "from-flat-labels")
	assert.Contains(t, buf.String(), "are ignored")
	assert.Contains(t, buf.String(), "borgmatic-manager.group")
}

func TestDiscoverSpecInvalidJSONFailsDiscovery(t *testing.T) {
	stubProbes(t)
	rt := mockLists([]runtime.VolumeInfo{}, []runtime.ContainerInfo{
		{
			ID:     "c1",
			Name:   "web",
			Labels: map[string]string{"borgmatic-manager.spec": `{"group": "myapp", "enable": tru`},
		},
	})

	state, err := discovery.Discover(context.Background(), rt, discardLogger())
	require.Error(t, err, "a broken spec silently shrinks the backup set; the cycle must fail, not warn")
	assert.Nil(t, state)
	assert.Contains(t, err.Error(), "invalid borgmatic-manager.spec")
	assert.Contains(t, err.Error(), "must be valid JSON", "the error must teach the accepted syntax")
}

func TestDiscoverSpecUnknownFieldFailsDiscovery(t *testing.T) {
	stubProbes(t)
	rt := mockLists([]runtime.VolumeInfo{}, []runtime.ContainerInfo{
		{
			ID:     "c1",
			Name:   "web",
			Labels: map[string]string{"borgmatic-manager.spec": `{"group": "myapp", "databses": []}`},
		},
	})

	_, err := discovery.Discover(context.Background(), rt, discardLogger())
	require.Error(t, err, "typo'd field names must be rejected, not dropped")
	assert.Contains(t, err.Error(), "databses")
}

func TestDiscoverSpecMissingGroupFailsDiscovery(t *testing.T) {
	stubProbes(t)
	rt := mockLists([]runtime.VolumeInfo{}, []runtime.ContainerInfo{
		{
			ID:     "c1",
			Name:   "web",
			Labels: map[string]string{"borgmatic-manager.spec": `{"enable": true}`},
		},
	})

	_, err := discovery.Discover(context.Background(), rt, discardLogger())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "missing the required")
}

func TestDiscoverSpecDatabaseValidationShared(t *testing.T) {
	stubProbes(t)
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))

	// mariadb with mode=exec must fall back to helper, same as flat labels.
	spec := `{"group": "g", "db": [{"type": "mariadb", "name": "db", "username": "u", "mode": "exec"}]}`
	rt := mockLists([]runtime.VolumeInfo{}, []runtime.ContainerInfo{
		{ID: "c1", Name: "maria", Image: "mariadb:11", Labels: map[string]string{"borgmatic-manager.spec": spec}},
	})

	state, err := discovery.Discover(context.Background(), rt, logger)
	require.NoError(t, err)

	dbs := state.Groups["g"].Databases
	require.Len(t, dbs, 1)
	assert.Empty(t, dbs[0].Mode, "exec falls back to helper for mariadb")
	assert.Contains(t, buf.String(), "exec mode is only supported for postgresql")
}

func TestDiscoverSpecRejectsNonJSON(t *testing.T) {
	stubProbes(t)
	// The spec is defined as JSON; YAML-flow dialects must be rejected with
	// guidance, not half-supported as a parser accident.
	c := runtime.ContainerInfo{
		ID:   "id-ha-pg",
		Name: "systemd-home-assistant-postgresql",
		Labels: map[string]string{
			"borgmatic-manager.spec": "{group: home-assistant, enable: true, volumes: [systemd-home-assistant-postgresql]}",
		},
		Mounts: []runtime.VolumeMount{mountFixture("systemd-home-assistant-postgresql", "/var/lib/postgresql/data")},
	}

	rt := mockLists([]runtime.VolumeInfo{volumeFixture("systemd-home-assistant-postgresql")}, []runtime.ContainerInfo{c})

	_, err := discovery.Discover(context.Background(), rt, discardLogger())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "strips them", "quote-less JSON-shaped values must get the quadlet-specific hint")
	assert.Contains(t, err.Error(), "single quotes")
}

func TestDiscoverRenamedBackupLabelWarns(t *testing.T) {
	stubProbes(t)
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))

	c := runtime.ContainerInfo{
		ID:   "c1",
		Name: "old-style",
		Labels: map[string]string{
			"borgmatic-manager.group":  "app",
			"borgmatic-manager.backup": "true", // pre-rename spelling
		},
		Mounts: []runtime.VolumeMount{mountFixture("app-data", "/data")},
	}

	rt := mockLists([]runtime.VolumeInfo{volumeFixture("app-data")}, []runtime.ContainerInfo{c})

	state, err := discovery.Discover(context.Background(), rt, logger)
	require.NoError(t, err)

	assert.Empty(t, state.Groups, "the old label must not silently work")
	assert.Contains(t, buf.String(), "renamed")
	assert.Contains(t, buf.String(), "borgmatic-manager.enable")
}

func TestDiscoverCollectsAllSpecErrors(t *testing.T) {
	stubProbes(t)
	rt := mockLists([]runtime.VolumeInfo{}, []runtime.ContainerInfo{
		{ID: "c1", Name: "broken-one", Labels: map[string]string{"borgmatic-manager.spec": `{bad`}},
		{ID: "c2", Name: "broken-two", Labels: map[string]string{"borgmatic-manager.spec": `{"enable": true}`}},
	})

	_, err := discovery.Discover(context.Background(), rt, discardLogger())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "broken-one", "every broken spec is reported so one pass fixes them all")
	assert.Contains(t, err.Error(), "broken-two")
}

func TestDiscoverEmptyVolumesFilterMeansAll(t *testing.T) {
	stubProbes(t)
	// Template-generated specs emit every field; "volumes": [] must behave
	// like an absent filter, not one that matches nothing.
	spec := `{"group": "myapp", "enable": true, "volumes": [], "db": [], "config": {}}`
	c := runtime.ContainerInfo{
		ID:     "id-web",
		Name:   "web",
		Labels: map[string]string{"borgmatic-manager.spec": spec},
		Mounts: []runtime.VolumeMount{mountFixture("app-data", "/data")},
	}

	rt := mockLists([]runtime.VolumeInfo{volumeFixture("app-data")}, []runtime.ContainerInfo{c})

	state, err := discovery.Discover(context.Background(), rt, discardLogger())
	require.NoError(t, err)

	require.Contains(t, state.Groups, "myapp")
	assert.Len(t, state.Groups["myapp"].Volumes, 1, "empty volumes filter must mean all named volumes")
}

func TestDiscoverEmptyVolumesLabelMeansAll(t *testing.T) {
	stubProbes(t)
	c := backupContainer("web", "myapp", mountFixture("app-data", "/data"))
	c.Labels["borgmatic-manager.volumes"] = "  " // set but empty

	rt := mockLists([]runtime.VolumeInfo{volumeFixture("app-data")}, []runtime.ContainerInfo{c})

	state, err := discovery.Discover(context.Background(), rt, discardLogger())
	require.NoError(t, err)
	assert.Len(t, state.Groups["myapp"].Volumes, 1, "an empty flat volumes label must mean all named volumes")
}

func TestParseConfigLabelsConflictIsDeterministic(t *testing.T) {
	labels := map[string]string{
		"borgmatic-manager.config.a":   "1",
		"borgmatic-manager.config.a.b": "2",
	}
	// Conflicting paths must resolve the same way every cycle (map
	// iteration order used to decide), with the deeper path winning.
	for i := 0; i < 20; i++ {
		got := discovery.ParseConfigLabels(labels, discardLogger())
		a, ok := got["a"].(map[string]interface{})
		require.True(t, ok, "nested option must win deterministically, got %#v", got["a"])
		assert.Equal(t, 2, a["b"])
	}
}
