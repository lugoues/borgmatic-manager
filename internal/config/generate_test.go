package config_test

import (
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"

	"github.com/lugoues/borgmatic-manager/internal/config"
	"github.com/lugoues/borgmatic-manager/internal/models"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// newTestGenerator builds a Generator with all host binaries "found" so
// dependency warnings don't fire in unrelated tests.
func newTestGenerator(t *testing.T, cfg *config.ManagerConfig, borgmaticOverrides map[string]map[string]interface{}, opts config.GeneratorOptions) (*config.Generator, string) {
	overrides := make(map[string]config.GroupOverride, len(borgmaticOverrides))
	for name, m := range borgmaticOverrides {
		overrides[name] = config.GroupOverride{Borgmatic: m}
	}
	t.Helper()
	outDir := t.TempDir()
	g := config.NewGenerator(cfg, overrides, outDir, opts, discardLogger())
	g.SetLookPath(func(string) (string, error) { return "/usr/bin/found", nil })
	return g, outDir
}

func emptyConfig() *config.ManagerConfig {
	return &config.ManagerConfig{Borgmatic: map[string]interface{}{}}
}

func readGenerated(t *testing.T, outDir, group string) map[string]interface{} {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(outDir, group+".yaml"))
	require.NoError(t, err)
	var parsed map[string]interface{}
	require.NoError(t, yaml.Unmarshal(data, &parsed))
	return parsed
}

func TestGenerateBasic(t *testing.T) {
	state := models.NewBackupState()
	state.AddVolume("web", models.VolumeInfo{Name: "web_data", HostPath: "/var/lib/docker/volumes/web_data/_data"})
	state.AddVolume("web", models.VolumeInfo{Name: "web_assets", HostPath: "/var/lib/docker/volumes/web_assets/_data"})

	g, outDir := newTestGenerator(t, emptyConfig(), nil, config.GeneratorOptions{})
	_, _, err := g.Generate(state)
	require.NoError(t, err)

	parsed := readGenerated(t, outDir, "web")

	srcDirs, ok := parsed["source_directories"].([]interface{})
	require.True(t, ok, "source_directories should be a list")
	assert.Contains(t, srcDirs, "/var/lib/docker/volumes/./web_data/_data",
		"the /./ marker makes archive paths start at the volume name")
	assert.Contains(t, srcDirs, "/var/lib/docker/volumes/./web_assets/_data")
	assert.NotContains(t, parsed, "working_directory",
		"working_directory was a container-mount concept; host paths are absolute")
}

func TestGenerateSnapshotGroupsUsePlainPaths(t *testing.T) {
	state := models.NewBackupState()
	state.AddVolume("app", models.VolumeInfo{Name: "web_data", HostPath: "/var/lib/docker/volumes/web_data/_data"})

	cfg := &config.ManagerConfig{
		Borgmatic: map[string]interface{}{"btrfs": nil},
	}

	g, outDir := newTestGenerator(t, cfg, nil, config.GeneratorOptions{})
	_, _, err := g.Generate(state)
	require.NoError(t, err)

	parsed := readGenerated(t, outDir, "app")
	srcDirs := parsed["source_directories"].([]interface{})
	assert.Contains(t, srcDirs, "/var/lib/docker/volumes/web_data/_data",
		"snapshot hooks build their own /./ rewrites; sources must stay plain")
}

func TestGenerateVolumeNameNotInPathFallsBack(t *testing.T) {
	state := models.NewBackupState()
	state.AddVolume("app", models.VolumeInfo{Name: "oddvol", HostPath: "/mnt/somewhere/else"})

	g, outDir := newTestGenerator(t, emptyConfig(), nil, config.GeneratorOptions{})
	_, _, err := g.Generate(state)
	require.NoError(t, err)

	parsed := readGenerated(t, outDir, "app")
	srcDirs := parsed["source_directories"].([]interface{})
	assert.Contains(t, srcDirs, "/mnt/somewhere/else",
		"paths without the volume name as a component are used unchanged")
}

func TestGeneratePinsRuntimeAndStateDirs(t *testing.T) {
	state := models.NewBackupState()
	state.AddVolume("app", models.VolumeInfo{Name: "v", HostPath: "/mnt/v"})

	g, outDir := newTestGenerator(t, emptyConfig(), nil, config.GeneratorOptions{
		RuntimeDir: "/run/borgmatic-manager",
		StateDir:   "/var/lib/borgmatic-manager",
	})
	_, _, err := g.Generate(state)
	require.NoError(t, err)

	parsed := readGenerated(t, outDir, "app")
	assert.Equal(t, "/run/borgmatic-manager", parsed["user_runtime_directory"])
	assert.Equal(t, "/var/lib/borgmatic-manager", parsed["user_state_directory"])
}

func TestGenerateFilePermissions(t *testing.T) {
	state := models.NewBackupState()
	state.AddVolume("app", models.VolumeInfo{Name: "v", HostPath: "/mnt/v"})

	g, outDir := newTestGenerator(t, emptyConfig(), nil, config.GeneratorOptions{})
	_, _, err := g.Generate(state)
	require.NoError(t, err)

	info, err := os.Stat(filepath.Join(outDir, "app.yaml"))
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), info.Mode().Perm(),
		"generated configs contain credentials and must be 0600")
}

func TestGenerateArchiveNameFormat(t *testing.T) {
	state := models.NewBackupState()
	state.AddVolume("myapp", models.VolumeInfo{Name: "app_vol", HostPath: "/mnt/app"})

	g, outDir := newTestGenerator(t, emptyConfig(), nil, config.GeneratorOptions{})
	_, _, err := g.Generate(state)
	require.NoError(t, err)

	parsed := readGenerated(t, outDir, "myapp")
	format, ok := parsed["archive_name_format"].(string)
	require.True(t, ok)
	assert.Equal(t, "{hostname}-myapp-{now:%Y-%m-%d_%H:%M:%S}", format)
}

func TestGenerateHelperModeDatabases(t *testing.T) {
	state := models.NewBackupState()
	state.AddDatabases("db-group", []models.DatabaseConfig{
		{Type: "postgresql", Name: "appdb", Username: "pguser", Password: "pw", Container: "pg-svc", Image: "postgres:17-alpine"},
		{Type: "mariadb", Name: "wiki", Username: "wiki", Password: "mpw", Port: 3306, Container: "maria-svc", Image: "mariadb:11"},
	})

	g, outDir := newTestGenerator(t, emptyConfig(), nil, config.GeneratorOptions{
		RuntimeDir:   "/run/borgmatic-manager",
		ContainerCLI: "docker",
	})
	meta, _, err := g.Generate(state)
	require.NoError(t, err)

	runID := meta["db-group"].RunID
	require.NotEmpty(t, runID, "generation must mint a run id for helper attribution")
	// --init: a PID-1 dump client would ignore SIGTERM and leak forever;
	// the labels let the runner reap exactly this run's orphans.
	helper := "docker run --rm --init --label borgmatic-manager.helper=db-group --label borgmatic-manager.run=" + runID

	parsed := readGenerated(t, outDir, "db-group")

	pg := parsed["postgresql_databases"].([]interface{})[0].(map[string]interface{})
	assert.Equal(t, "127.0.0.1", pg["hostname"], "helper joins the DB netns; localhost is the DB")
	assert.Equal(t, "pg-svc", pg["label"], "container name keeps archive dump paths unique")
	assert.Equal(t, helper+" --network container:pg-svc --env PGPASSWORD postgres:17-alpine pg_dump", pg["pg_dump_command"],
		"helper uses the DB container's own image so client always matches server")
	assert.Equal(t, helper+" -i --network container:pg-svc --env PGPASSWORD postgres:17-alpine pg_restore", pg["pg_restore_command"])
	assert.Equal(t, helper+" -i --network container:pg-svc --env PGPASSWORD postgres:17-alpine psql", pg["psql_command"])
	assert.NotContains(t, pg, "container", "the bridge-IP container: option is retired")

	maria := parsed["mariadb_databases"].([]interface{})[0].(map[string]interface{})
	assert.Equal(t, "environment", maria["password_transport"],
		"a defaults-file pipe cannot cross the container boundary")
	assert.Equal(t, helper+" -v /run/borgmatic-manager:/run/borgmatic-manager --network container:maria-svc --env MYSQL_PWD mariadb:11 mariadb-dump", maria["mariadb_dump_command"],
		"the runtime dir mount lets the client reach borgmatic's dump FIFO")
	assert.Equal(t, 3306, maria["port"])

	// A fresh generation mints a fresh id: reaping one run's orphans can
	// never touch another run's helpers.
	meta2, _, err := g.Generate(state)
	require.NoError(t, err)
	assert.NotEqual(t, runID, meta2["db-group"].RunID)
}

func TestGenerateHelperModeUsesPodmanCLI(t *testing.T) {
	state := models.NewBackupState()
	state.AddDatabases("g", []models.DatabaseConfig{
		{Type: "mysql", Name: "db", Username: "u", Container: "mysql-svc", Image: "mysql:8"},
	})

	g, outDir := newTestGenerator(t, emptyConfig(), nil, config.GeneratorOptions{
		RuntimeDir:   "/run/borgmatic-manager",
		ContainerCLI: "podman",
	})
	_, _, err := g.Generate(state)
	require.NoError(t, err)

	parsed := readGenerated(t, outDir, "g")
	my := parsed["mysql_databases"].([]interface{})[0].(map[string]interface{})
	assert.Contains(t, my["mysql_dump_command"], "podman run --rm")
	assert.Contains(t, my["mysql_dump_command"], "mysqldump")
}

func TestGenerateExecModePostgres(t *testing.T) {
	state := models.NewBackupState()
	state.AddDatabases("g", []models.DatabaseConfig{
		{Type: "postgresql", Name: "appdb", Username: "u", Container: "pg-svc", Image: "postgres:17", Mode: "exec"},
	})

	g, outDir := newTestGenerator(t, emptyConfig(), nil, config.GeneratorOptions{ContainerCLI: "docker"})
	_, _, err := g.Generate(state)
	require.NoError(t, err)

	parsed := readGenerated(t, outDir, "g")
	pg := parsed["postgresql_databases"].([]interface{})[0].(map[string]interface{})
	assert.Equal(t, "docker exec --env PGPASSWORD pg-svc pg_dump", pg["pg_dump_command"])
	assert.Equal(t, "docker exec -i --env PGPASSWORD pg-svc pg_restore", pg["pg_restore_command"])
}

func TestGenerateHostnameModeSkipsHelper(t *testing.T) {
	state := models.NewBackupState()
	state.AddDatabases("db-group", []models.DatabaseConfig{
		{Type: "postgresql", Name: "appdb", Username: "u", Hostname: "127.0.0.1", Port: 5433, Container: "pg-svc", Image: "postgres:17"},
	})

	g, outDir := newTestGenerator(t, emptyConfig(), nil, config.GeneratorOptions{ContainerCLI: "docker"})
	_, _, err := g.Generate(state)
	require.NoError(t, err)

	parsed := readGenerated(t, outDir, "db-group")
	pg := parsed["postgresql_databases"].([]interface{})[0].(map[string]interface{})
	assert.Equal(t, "127.0.0.1", pg["hostname"])
	assert.Equal(t, 5433, pg["port"])
	assert.NotContains(t, pg, "pg_dump_command", "hostname mode uses the host client, no helper container")
}

func TestGenerateLabelConfigMergesOverGroupFiles(t *testing.T) {
	state := models.NewBackupState()
	state.AddVolume("app", models.VolumeInfo{Name: "v", HostPath: "/mnt/v"})
	state.AddLabelConfig("app", map[string]interface{}{
		"keep_daily": 30,
		"healthchecks": map[string]interface{}{
			"ping_url": "https://hc-ping.com/label",
		},
	})

	cfg := &config.ManagerConfig{
		Borgmatic: map[string]interface{}{"keep_daily": 7, "keep_weekly": 4},
	}
	overrides := map[string]map[string]interface{}{
		"app": {"keep_daily": 14},
	}

	g, outDir := newTestGenerator(t, cfg, overrides, config.GeneratorOptions{})
	_, _, err := g.Generate(state)
	require.NoError(t, err)

	parsed := readGenerated(t, outDir, "app")
	assert.Equal(t, 30, parsed["keep_daily"], "label config wins over groups/*.yaml which wins over defaults")
	assert.Equal(t, 4, parsed["keep_weekly"], "untouched defaults survive")
	hc := parsed["healthchecks"].(map[string]interface{})
	assert.Equal(t, "https://hc-ping.com/label", hc["ping_url"])
}

func TestGenerateSQLiteDatabases(t *testing.T) {
	state := models.NewBackupState()
	state.AddDatabases("app", []models.DatabaseConfig{
		{Type: "sqlite", Name: "app", Volume: "app-data", Path: "/var/lib/docker/volumes/app-data/_data/app.sqlite3"},
	})

	g, outDir := newTestGenerator(t, emptyConfig(), nil, config.GeneratorOptions{})
	_, _, err := g.Generate(state)
	require.NoError(t, err)

	parsed := readGenerated(t, outDir, "app")
	dbs := parsed["sqlite_databases"].([]interface{})
	require.Len(t, dbs, 1)
	db := dbs[0].(map[string]interface{})
	assert.Equal(t, "/var/lib/docker/volumes/app-data/_data/app.sqlite3", db["path"])
	assert.NotContains(t, db, "container")
	assert.NotContains(t, db, "hostname")
}

func TestGenerateWarnsOnMissingHostClient(t *testing.T) {
	state := models.NewBackupState()
	state.AddDatabases("db-group", []models.DatabaseConfig{
		{Type: "postgresql", Name: "appdb", Username: "u", Hostname: "127.0.0.1"},
	})

	var buf strings.Builder
	logger := slog.New(slog.NewTextHandler(&buf, nil))
	outDir := t.TempDir()
	g := config.NewGenerator(emptyConfig(), nil, outDir, config.GeneratorOptions{}, logger)
	g.SetLookPath(func(bin string) (string, error) {
		return "", os.ErrNotExist
	})

	_, _, err := g.Generate(state)
	require.NoError(t, err)
	assert.Contains(t, buf.String(), "pg_dump")
	assert.Contains(t, buf.String(), "not found on PATH")
}

func TestGenerateRunMeta(t *testing.T) {
	state := models.NewBackupState()
	state.AddVolume("alpha", models.VolumeInfo{Name: "a", HostPath: "/mnt/a"})
	state.AddVolume("beta", models.VolumeInfo{Name: "b", HostPath: "/mnt/b"})

	cfg := &config.ManagerConfig{
		Borgmatic: map[string]interface{}{
			"repositories": []interface{}{
				map[string]interface{}{"path": "/mnt/repo/"},
			},
			"btrfs": nil,
		},
	}
	overrides := map[string]map[string]interface{}{
		"beta": {
			"repositories": []interface{}{
				map[string]interface{}{"path": "ssh://borg@host/./repo/"},
			},
		},
	}

	g, _ := newTestGenerator(t, cfg, overrides, config.GeneratorOptions{})
	meta, _, err := g.Generate(state)
	require.NoError(t, err)

	require.Contains(t, meta, "alpha")
	require.Contains(t, meta, "beta")
	assert.Equal(t, []string{"/mnt/repo"}, meta["alpha"].Repos, "local paths are cleaned")
	assert.Equal(t, []string{"ssh://borg@host/./repo"}, meta["beta"].Repos, "URLs keep form, trailing slash trimmed")
	assert.True(t, meta["alpha"].SnapshotHooks, "bare btrfs: key must count as snapshot hooks enabled")
	assert.True(t, meta["beta"].SnapshotHooks)
}

func TestGenerateRunMetaEnvPlaceholderSerializes(t *testing.T) {
	state := models.NewBackupState()
	state.AddVolume("app", models.VolumeInfo{Name: "v", HostPath: "/mnt/v"})

	cfg := &config.ManagerConfig{
		Borgmatic: map[string]interface{}{
			"repositories": []interface{}{
				map[string]interface{}{"path": "${BORG_REPO}"},
			},
		},
	}

	g, _ := newTestGenerator(t, cfg, nil, config.GeneratorOptions{})
	meta, _, err := g.Generate(state)
	require.NoError(t, err)
	assert.Equal(t, []string{"unknown"}, meta["app"].Repos,
		"unresolvable repo paths must collapse to one conservative lock key")
}

func TestGenerateReconcilesStaleConfigs(t *testing.T) {
	state := models.NewBackupState()
	state.AddVolume("current", models.VolumeInfo{Name: "v", HostPath: "/mnt/v"})

	g, outDir := newTestGenerator(t, emptyConfig(), nil, config.GeneratorOptions{})

	// Simulate a config left over from a removed group (it carries the
	// generated header), plus operator files that must never be touched:
	// generate --output can point at any directory.
	stale := "# Auto-generated by borgmatic-manager. Do not edit.\nrepositories: []\n"
	require.NoError(t, os.WriteFile(filepath.Join(outDir, "removed-group.yaml"), []byte(stale), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(outDir, "operator.yaml"), []byte("keep: me\n"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(outDir, "notes.txt"), []byte("keep"), 0o600))

	_, _, err := g.Generate(state)
	require.NoError(t, err)

	_, err = os.Stat(filepath.Join(outDir, "current.yaml"))
	require.NoError(t, err)
	_, err = os.Stat(filepath.Join(outDir, "removed-group.yaml"))
	assert.True(t, os.IsNotExist(err), "stale group config must be removed")
	_, err = os.Stat(filepath.Join(outDir, "operator.yaml"))
	require.NoError(t, err, "yaml without the generated header must be left alone")
	_, err = os.Stat(filepath.Join(outDir, "notes.txt"))
	require.NoError(t, err, "non-yaml files are left alone")
}

func TestGenerateDeepMerge(t *testing.T) {
	state := models.NewBackupState()
	state.AddVolume("app", models.VolumeInfo{Name: "data", HostPath: "/mnt/data"})

	cfg := &config.ManagerConfig{
		Borgmatic: map[string]interface{}{
			"keep_daily":   7,
			"keep_weekly":  4,
			"keep_monthly": 6,
		},
	}

	// Group override changes keep_daily
	overrides := map[string]map[string]interface{}{
		"app": {
			"keep_daily": 14,
		},
	}

	g, outDir := newTestGenerator(t, cfg, overrides, config.GeneratorOptions{})
	_, _, err := g.Generate(state)
	require.NoError(t, err)

	parsed := readGenerated(t, outDir, "app")

	// Override should win
	assert.Equal(t, 14, parsed["keep_daily"])
	// Default should remain
	assert.Equal(t, 4, parsed["keep_weekly"])
	assert.Equal(t, 6, parsed["keep_monthly"])
}

func TestGenerateHeader(t *testing.T) {
	state := models.NewBackupState()
	state.AddVolume("test", models.VolumeInfo{Name: "vol", HostPath: "/mnt/vol"})

	g, outDir := newTestGenerator(t, emptyConfig(), nil, config.GeneratorOptions{})
	_, _, err := g.Generate(state)
	require.NoError(t, err)

	data, err := os.ReadFile(filepath.Join(outDir, "test.yaml"))
	require.NoError(t, err)

	content := string(data)
	assert.True(t, strings.HasPrefix(content, "# Auto-generated by borgmatic-manager. Do not edit.\n"),
		"file should start with auto-generated header comment, got: %s", content[:min(len(content), 80)])
}

func TestGenerateMultipleGroups(t *testing.T) {
	state := models.NewBackupState()
	state.AddVolume("alpha", models.VolumeInfo{Name: "a_vol", HostPath: "/mnt/a"})
	state.AddVolume("beta", models.VolumeInfo{Name: "b_vol", HostPath: "/mnt/b"})

	g, outDir := newTestGenerator(t, emptyConfig(), nil, config.GeneratorOptions{})
	_, _, err := g.Generate(state)
	require.NoError(t, err)

	_, err = os.Stat(filepath.Join(outDir, "alpha.yaml"))
	require.NoError(t, err, "alpha.yaml should exist")

	_, err = os.Stat(filepath.Join(outDir, "beta.yaml"))
	assert.NoError(t, err, "beta.yaml should exist")
}

func TestGenerateOmitempty(t *testing.T) {
	state := models.NewBackupState()
	state.AddVolume("minimal", models.VolumeInfo{Name: "vol", HostPath: "/mnt/vol"})

	g, outDir := newTestGenerator(t, emptyConfig(), nil, config.GeneratorOptions{})
	_, _, err := g.Generate(state)
	require.NoError(t, err)

	data, err := os.ReadFile(filepath.Join(outDir, "minimal.yaml"))
	require.NoError(t, err)

	content := string(data)
	// No databases were added, so these keys should not appear
	assert.NotContains(t, content, "postgresql_databases")
	assert.NotContains(t, content, "mysql_databases")
	assert.NotContains(t, content, "mariadb_databases")
	assert.NotContains(t, content, "sqlite_databases")
}

func TestGenerateCustomArchiveFormatWithGroupToken(t *testing.T) {
	state := models.NewBackupState()
	state.AddVolume("home-assistant", models.VolumeInfo{Name: "v", HostPath: "/mnt/v"})

	// Repo-per-host setups drop the redundant {hostname}.
	cfg := &config.ManagerConfig{
		Borgmatic: map[string]interface{}{
			"archive_name_format": "{group}-{now:%Y-%m-%d}",
		},
	}

	g, outDir := newTestGenerator(t, cfg, nil, config.GeneratorOptions{})
	_, _, err := g.Generate(state)
	require.NoError(t, err)

	parsed := readGenerated(t, outDir, "home-assistant")
	assert.Equal(t, "home-assistant-{now:%Y-%m-%d}", parsed["archive_name_format"],
		"{group} is substituted by the manager; borg placeholders pass through")
}

func TestGenerateCustomFormatWithoutGroupAllowedOnExclusiveRepo(t *testing.T) {
	state := models.NewBackupState()
	state.AddVolume("solo", models.VolumeInfo{Name: "v", HostPath: "/mnt/v"})

	cfg := &config.ManagerConfig{
		Borgmatic: map[string]interface{}{
			"repositories":        []interface{}{map[string]interface{}{"path": "/mnt/solo-repo"}},
			"archive_name_format": "backup-{now}",
		},
	}

	g, outDir := newTestGenerator(t, cfg, nil, config.GeneratorOptions{})
	meta, _, err := g.Generate(state)
	require.NoError(t, err)

	require.Contains(t, meta, "solo", "an exclusive repository permits any format")
	parsed := readGenerated(t, outDir, "solo")
	assert.Equal(t, "backup-{now}", parsed["archive_name_format"])
}

func TestGenerateCustomFormatWithoutGroupRefusedOnSharedRepo(t *testing.T) {
	state := models.NewBackupState()
	state.AddVolume("alpha", models.VolumeInfo{Name: "a", HostPath: "/mnt/a"})
	state.AddVolume("beta", models.VolumeInfo{Name: "b", HostPath: "/mnt/b"})

	cfg := &config.ManagerConfig{
		Borgmatic: map[string]interface{}{
			"repositories":        []interface{}{map[string]interface{}{"path": "/mnt/shared"}},
			"archive_name_format": "backup-{now}", // no {group}: groups indistinguishable
		},
	}

	var buf strings.Builder
	logger := slog.New(slog.NewTextHandler(&buf, nil))
	outDir := t.TempDir()
	g := config.NewGenerator(cfg, nil, outDir, config.GeneratorOptions{}, logger)
	g.SetLookPath(func(string) (string, error) { return "/usr/bin/found", nil })

	meta, _, err := g.Generate(state)
	require.NoError(t, err)

	assert.Empty(t, meta, "groups sharing a repo with an indistinguishable format must be refused")
	_, statErr := os.Stat(filepath.Join(outDir, "alpha.yaml"))
	assert.True(t, os.IsNotExist(statErr), "no config may be written for a refused group")
	assert.Contains(t, buf.String(), "must contain the literal {group} token")
}

// The guard must require the {group} token, not merely that the resolved format
// happens to contain the group's name: "{hostname}-appdata-{now}" contains both
// "app" and "data" as substrings, so a Contains-based check passed both groups
// while leaving their archives identically named, letting one group's prune
// permanently delete the other's archives.
func TestGenerateSubstringGroupNameRefusedOnSharedRepo(t *testing.T) {
	state := models.NewBackupState()
	state.AddVolume("app", models.VolumeInfo{Name: "a", HostPath: "/mnt/a"})
	state.AddVolume("data", models.VolumeInfo{Name: "d", HostPath: "/mnt/d"})

	cfg := &config.ManagerConfig{
		Borgmatic: map[string]interface{}{
			"repositories":        []interface{}{map[string]interface{}{"path": "/mnt/shared"}},
			"archive_name_format": "{hostname}-appdata-{now}", // contains "app" and "data"; no {group}
		},
	}

	var buf strings.Builder
	logger := slog.New(slog.NewTextHandler(&buf, nil))
	outDir := t.TempDir()
	g := config.NewGenerator(cfg, nil, outDir, config.GeneratorOptions{}, logger)
	g.SetLookPath(func(string) (string, error) { return "/usr/bin/found", nil })

	meta, _, err := g.Generate(state)
	require.NoError(t, err)

	assert.Empty(t, meta, "a format merely containing the group names as substrings must be refused, not accepted")
	for _, group := range []string{"app", "data"} {
		_, statErr := os.Stat(filepath.Join(outDir, group+".yaml"))
		assert.True(t, os.IsNotExist(statErr), "no config may be written for refused group %s", group)
	}
	assert.Contains(t, buf.String(), "must contain the literal {group} token")
}

// RenderGroup returns one group's compiled config without writing any files,
// and runs the full plan so the shared-repo guard still applies.
func TestRenderGroup(t *testing.T) {
	state := models.NewBackupState()
	state.AddVolume("app", models.VolumeInfo{Name: "a", HostPath: "/mnt/a"})
	state.AddVolume("data", models.VolumeInfo{Name: "d", HostPath: "/mnt/d"})

	cfg := &config.ManagerConfig{
		Borgmatic: map[string]interface{}{
			"repositories":        []interface{}{map[string]interface{}{"path": "/mnt/shared"}},
			"archive_name_format": "{hostname}-{group}-{now}",
		},
	}
	g, outDir := newTestGenerator(t, cfg, nil, config.GeneratorOptions{})

	yaml, refusal, err := g.RenderGroup(state, "app")
	require.NoError(t, err)
	assert.Empty(t, refusal)
	assert.Contains(t, yaml, "{hostname}-app-{now}", "the group's own compiled config is returned")
	assert.NotContains(t, yaml, "{hostname}-data-{now}", "and only that group's")

	// Nothing was written.
	entries, _ := os.ReadDir(outDir)
	assert.Empty(t, entries, "RenderGroup must not write any config files")

	// An unknown group is empty, not an error.
	yaml, refusal, err = g.RenderGroup(state, "nope")
	require.NoError(t, err)
	assert.Empty(t, yaml)
	assert.Empty(t, refusal)
}

// RenderGroup reports a shared-repo refusal rather than returning a config that
// would be unsafe to run.
func TestRenderGroupReportsRefusal(t *testing.T) {
	state := models.NewBackupState()
	state.AddVolume("app", models.VolumeInfo{Name: "a", HostPath: "/mnt/a"})
	state.AddVolume("data", models.VolumeInfo{Name: "d", HostPath: "/mnt/d"})

	cfg := &config.ManagerConfig{
		Borgmatic: map[string]interface{}{
			"repositories":        []interface{}{map[string]interface{}{"path": "/mnt/shared"}},
			"archive_name_format": "backup-{now}", // no {group}, shared repo
		},
	}
	g, _ := newTestGenerator(t, cfg, nil, config.GeneratorOptions{})

	yaml, refusal, err := g.RenderGroup(state, "app")
	require.NoError(t, err)
	assert.Empty(t, yaml)
	assert.Contains(t, refusal, "{group} token", "the guard's reason is surfaced, seen only because the full plan ran")
}

// The token still satisfies the guard for groups sharing a repository.
func TestGenerateGroupTokenAllowedOnSharedRepo(t *testing.T) {
	state := models.NewBackupState()
	state.AddVolume("app", models.VolumeInfo{Name: "a", HostPath: "/mnt/a"})
	state.AddVolume("data", models.VolumeInfo{Name: "d", HostPath: "/mnt/d"})

	cfg := &config.ManagerConfig{
		Borgmatic: map[string]interface{}{
			"repositories":        []interface{}{map[string]interface{}{"path": "/mnt/shared"}},
			"archive_name_format": "{hostname}-{group}-{now}",
		},
	}

	g, outDir := newTestGenerator(t, cfg, nil, config.GeneratorOptions{})
	meta, _, err := g.Generate(state)
	require.NoError(t, err)

	require.Contains(t, meta, "app")
	require.Contains(t, meta, "data")
	assert.Equal(t, "{hostname}-app-{now}", readGenerated(t, outDir, "app")["archive_name_format"])
	assert.Equal(t, "{hostname}-data-{now}", readGenerated(t, outDir, "data")["archive_name_format"],
		"each group's archives carry its own name, so prune stays scoped")
}

func TestGeneratePrefixGroupNamesWarnOnSharedRepo(t *testing.T) {
	state := models.NewBackupState()
	state.AddVolume("app", models.VolumeInfo{Name: "a", HostPath: "/mnt/a"})
	state.AddVolume("app-prod", models.VolumeInfo{Name: "b", HostPath: "/mnt/b"})

	cfg := &config.ManagerConfig{
		Borgmatic: map[string]interface{}{
			"repositories": []interface{}{map[string]interface{}{"path": "/mnt/shared"}},
		},
	}

	var buf strings.Builder
	logger := slog.New(slog.NewTextHandler(&buf, nil))
	g := config.NewGenerator(cfg, nil, t.TempDir(), config.GeneratorOptions{}, logger)
	g.SetLookPath(func(string) (string, error) { return "/usr/bin/found", nil })

	meta, _, err := g.Generate(state)
	require.NoError(t, err)

	assert.Len(t, meta, 2, "prefix collisions warn, they do not refuse")
	assert.Contains(t, buf.String(), "retention can cross group boundaries")
}

// A run id scopes dump-helper reaping to one actual run. Plan and RenderGroup
// start no run, so a freshly minted id there is an identifier no helper will
// ever carry: it reads as the current run's while never matching it. Both use a
// visible placeholder instead.
func TestPlanAndRenderGroupDoNotMintRealRunIDs(t *testing.T) {
	state := models.NewBackupState()
	state.AddDatabases("db-group", []models.DatabaseConfig{
		{Type: "postgresql", Name: "postgres", Container: "pg-svc", Image: "postgres:17-alpine",
			Username: "postgres", Password: "s3cret", Mode: "helper"},
	})
	g, _ := newTestGenerator(t, &config.ManagerConfig{Borgmatic: map[string]interface{}{
		"repositories": []interface{}{map[string]interface{}{"path": "/srv/repo"}},
	}}, nil, config.GeneratorOptions{RuntimeDir: "/run/borgmatic-manager", ContainerCLI: "docker"})

	meta, _, err := g.Plan(state)
	require.NoError(t, err)
	assert.Equal(t, config.PlaceholderRunID, meta["db-group"].RunID, "Plan starts no run, so it mints no run id")

	// Stable across calls: two inspects of an unchanged group must not disagree.
	meta2, _, err := g.Plan(state)
	require.NoError(t, err)
	assert.Equal(t, meta["db-group"].RunID, meta2["db-group"].RunID)

	rendered, refusal, err := g.RenderGroup(state, "db-group")
	require.NoError(t, err)
	require.Empty(t, refusal)
	assert.Contains(t, rendered, "borgmatic-manager.run="+config.PlaceholderRunID,
		"the displayed config labels helpers with the placeholder")
	assert.NotRegexp(t, `borgmatic-manager\.run=[0-9a-f]{16}`, rendered,
		"and never with a real-looking hex id that would not match the one on disk")

	rendered2, _, err := g.RenderGroup(state, "db-group")
	require.NoError(t, err)
	assert.Equal(t, rendered, rendered2, "rendering twice gives the same config")

	// Generate still mints a real one: reaping depends on it.
	gen, _, err := g.Generate(state)
	require.NoError(t, err)
	assert.NotEqual(t, config.PlaceholderRunID, gen["db-group"].RunID)
	assert.Regexp(t, `^[0-9a-f]{16}$`, gen["db-group"].RunID)
}

func TestGenerateExtractsRepositoryRefsWithLabels(t *testing.T) {
	state := models.NewBackupState()
	state.AddVolume("app", models.VolumeInfo{Name: "v", HostPath: "/mnt/v"})

	cfg := &config.ManagerConfig{Borgmatic: map[string]interface{}{
		"repositories": []interface{}{
			map[string]interface{}{"path": "/mnt/local", "label": "local"},
			map[string]interface{}{"path": "ssh://borg@host/./r", "label": "offsite"},
			"/bare/path", // no label: ref carries just the path
		},
	}}
	g, _ := newTestGenerator(t, cfg, nil, config.GeneratorOptions{})
	meta, _, err := g.Generate(state)
	require.NoError(t, err)

	refs := meta["app"].Repositories
	require.Len(t, refs, 3)
	assert.Equal(t, config.RepoRef{Path: "/mnt/local", Label: "local"}, refs[0])
	assert.Equal(t, config.RepoRef{Path: "ssh://borg@host/./r", Label: "offsite"}, refs[1])
	assert.Equal(t, config.RepoRef{Path: "/bare/path"}, refs[2])
}

// Groups may share a repository, so "the newest archive here" is not the same
// question as "the newest archive this group wrote". The pattern is what makes
// the difference askable.
func TestArchiveMatchPatternReplacesBorgPlaceholders(t *testing.T) {
	for name, tc := range map[string]struct{ format, want string }{
		"the default shape":   {"{hostname}-myapp-{now:%Y-%m-%d_%H:%M:%S}", "*-myapp-*"},
		"group already named": {"myapp-{now}", "myapp-*"},
		"no placeholders":     {"fixed-name", "fixed-name"},
		"leading placeholder": {"{fqdn}-db", "*-db"},
		"adjacent":            {"{a}{b}-x", "**-x"},
	} {
		t.Run(name, func(t *testing.T) {
			assert.Equal(t, tc.want, config.ArchiveMatchPattern(tc.format))
		})
	}
}

// Generation warns about groups whose names prefix one another sharing a
// repository and keeps both, so the overlap has to be reported to the runner:
// "app"'s pattern matches "app-prod"'s archives, and a probe using it cannot
// tell whose backup it found.
func TestPrefixCollidingGroupsAreMarkedAmbiguous(t *testing.T) {
	// The default format contains {hostname}, so an unpinned hostname makes the
	// assertions depend on the machine: a host named after one of these groups
	// creates a collision that has nothing to do with what is being tested.
	defer config.SetSampleHostname("backup01")()

	state := models.NewBackupState()
	state.AddVolume("app", models.VolumeInfo{Name: "v1", HostPath: "/mnt/v1"})
	state.AddVolume("app-prod", models.VolumeInfo{Name: "v2", HostPath: "/mnt/v2"})
	state.AddVolume("other", models.VolumeInfo{Name: "v3", HostPath: "/mnt/v3"})

	cfg := &config.ManagerConfig{
		Borgmatic: map[string]interface{}{
			"repositories":        []interface{}{map[string]interface{}{"path": "/mnt/repo"}},
			"archive_name_format": "{hostname}-{group}-{now}",
		},
	}

	g, _ := newTestGenerator(t, cfg, nil, config.GeneratorOptions{})
	meta, _, err := g.Generate(state)
	require.NoError(t, err)

	assert.NotEmpty(t, meta["app"].AmbiguousRepos,
		"the prefix matches the longer group's archives")
	assert.NotEmpty(t, meta["app-prod"].AmbiguousRepos,
		"and it shares a repository with a group whose retention crosses into its own")
	assert.Empty(t, meta["other"].AmbiguousRepos,
		"a group no other name prefixes keeps a usable pattern")
	assert.NotEmpty(t, meta["app"].ArchivePattern,
		"the pattern is still produced; it is retention that still needs it")
}

// Overlap is a property of the names a format generates, not of the group names
// and not of the patterns as sets of strings. A hyphen-shaped name check misses
// a format that puts no separator before {now}; comparing patterns as sets calls
// every pair of groups a collision, because "*-app-*" and "*-other-*" both match
// "x-app--other-y".
func TestArchivePatternOverlapIsDetectedFromTheGeneratedNames(t *testing.T) {
	generate := func(t *testing.T, format string, groups ...string) map[string]config.GroupRunMeta {
		t.Helper()
		// Pinned for the {hostname} cases: the machine's own name must not
		// decide whether these patterns collide.
		t.Cleanup(config.SetSampleHostname("backup01"))
		st := models.NewBackupState()
		for i, name := range groups {
			st.AddVolume(name, models.VolumeInfo{Name: fmt.Sprintf("v%d", i), HostPath: fmt.Sprintf("/mnt/v%d", i)})
		}
		cfg := &config.ManagerConfig{Borgmatic: map[string]interface{}{
			"repositories":        []interface{}{map[string]interface{}{"path": "/mnt/shared"}},
			"archive_name_format": format,
		}}
		g, _ := newTestGenerator(t, cfg, nil, config.GeneratorOptions{})
		meta, _, err := g.Generate(st)
		require.NoError(t, err)
		return meta
	}

	t.Run("no separator before the timestamp still collides", func(t *testing.T) {
		meta := generate(t, "{group}{now}", "app", "apple")
		assert.NotEmpty(t, meta["app"].AmbiguousRepos,
			`"app*" matches the archives "apple" writes`)
		assert.NotEmpty(t, meta["apple"].AmbiguousRepos)
	})

	t.Run("a trailing group name cannot collide", func(t *testing.T) {
		meta := generate(t, "{now}-{group}", "app", "apple")
		assert.Empty(t, meta["app"].AmbiguousRepos,
			`no name ends in both "-app" and "-apple"`)
		assert.Empty(t, meta["apple"].AmbiguousRepos)
	})

	t.Run("unrelated groups sharing a repository are not collisions", func(t *testing.T) {
		meta := generate(t, "{hostname}-{group}-{now}", "app", "other", "third")
		for _, name := range []string{"app", "other", "third"} {
			assert.Empty(t, meta[name].AmbiguousRepos,
				"%s shares a repository but its archives are distinguishable", name)
		}
	})

	t.Run("the prefix shape is still caught", func(t *testing.T) {
		meta := generate(t, "{hostname}-{group}-{now}", "app", "app-prod")
		assert.NotEmpty(t, meta["app"].AmbiguousRepos)
		assert.NotEmpty(t, meta["app-prod"].AmbiguousRepos)
	})
}

func TestPatternsCollide(t *testing.T) {
	defer config.SetSampleHostname("myhost")()

	// Both directions, because the two groups' formats need not be the same: a
	// per-group override can give one group a format whose literal text is the
	// other group's whole name. Only the second direction sees that, and group
	// order is alphabetical, which always puts a prefix before its extension.
	t.Run("a collision only the second direction sees", func(t *testing.T) {
		assert.False(t, config.PatternMatchesFormatForTest("alpha", "alpha{now}"),
			"alpha's own pattern does not match beta's names")
		assert.True(t, config.PatternsCollideForTest(
			"alpha", "{group}", // group alpha, whose name is the whole format
			"alpha*", "alpha{now}"), // group beta, whose literal text is alpha's name
			"but beta's pattern claims alpha's archives")
	})

	t.Run("distinguishable patterns do not collide", func(t *testing.T) {
		assert.False(t, config.PatternsCollideForTest(
			"*-app-*", "{hostname}-app-{now}",
			"*-other-*", "{hostname}-other-{now}"))
	})

	// Unreachable through generation today, because a shared-repo group whose
	// archive_name_format lacks the {group} token is refused before this runs.
	// Kept because "nothing is known about this group's archive names" must not
	// read as "its archives are distinguishable" if that ever changes.
	t.Run("an unknown pattern is treated as colliding", func(t *testing.T) {
		assert.True(t, config.PatternsCollideForTest("", "", "*-app-*", "x-app-{now}"))
		assert.True(t, config.PatternsCollideForTest("*-app-*", "x-app-{now}", "", ""))
	})
}

// Sampling instants could never answer this. Each directive is small on its own,
// but a format with two of them generates their product: group "02-59" collides
// with a sibling formatting "{now:%m}-{now:%S}" only in February, on second 59,
// and no feasible number of sampled instants contains every such pair. Matching
// against the format's structure explores the combinations without enumerating
// them.
func TestDirectiveCombinationsAreMatched(t *testing.T) {
	defer config.SetSampleHostname("myhost")()

	assert.True(t, config.PatternMatchesFormatForTest("x-02-59-*", "x-{now:%m}-{now:%S}-prod-{now}"),
		"February, second 59")
	assert.False(t, config.PatternMatchesFormatForTest("x-13-59-*", "x-{now:%m}-{now:%S}-prod-{now}"),
		"there is no thirteenth month")
	assert.False(t, config.PatternMatchesFormatForTest("x-02-60-*", "x-{now:%m}-{now:%S}-prod-{now}"),
		"nor a sixtieth second")

	// Through the collision decision, in both directions.
	assert.True(t, config.PatternsCollideForTest(
		"x-02-59-*", "x-{group}-{now}",
		"x-*-*-prod-*", "x-{now:%m}-{now:%S}-prod-{now}"))

	// Single directives still work, including the earlier month and day cases.
	assert.True(t, config.PatternMatchesFormatForTest("x-07-*", "x-{now:%m}-prod-{now}"))
	assert.True(t, config.PatternMatchesFormatForTest("x-31-*", "x-{now:%d}-prod-{now}"))
	assert.False(t, config.PatternMatchesFormatForTest("x-32-*", "x-{now:%d}-prod-{now}"))
	assert.True(t, config.PatternMatchesFormatForTest("x-23-*", "x-{now:%H}-prod-{now}"))
	assert.False(t, config.PatternMatchesFormatForTest("x-24-*", "x-{now:%H}-prod-{now}"))
}

// The domains are what make the match exact, so they are asserted directly.
func TestFormatDirectiveDomains(t *testing.T) {
	defer config.SetSampleHostname("myhost")()

	domainOf := func(format string) []string {
		alts := config.FormatAlternativesForTest(format)
		require.Len(t, alts, 1, "one directive, one segment")
		return alts[0]
	}

	assert.Len(t, domainOf("{now:%m}"), 12)
	assert.Len(t, domainOf("{now:%d}"), 31)
	assert.Len(t, domainOf("{now:%H}"), 24)
	assert.Len(t, domainOf("{now:%M}"), 60)
	assert.Len(t, domainOf("{now:%S}"), 60)
	assert.Contains(t, domainOf("{now:%b}"), "Jul")
	assert.Contains(t, domainOf("{now:%A}"), "Wednesday")
	// Years are open-ended, so they are a digit run rather than a value set: any
	// enumeration would call two groups disjoint the moment a retained archive
	// predates the window.
	assert.Equal(t, [][]string{{"<4 digits>"}}, config.FormatAlternativesForTest("{now:%Y}"))
	assert.Equal(t, [][]string{{"<2 digits>"}}, config.FormatAlternativesForTest("{now:%y}"))

	// A known host value is a literal, not a domain.
	assert.Equal(t, [][]string{{"myhost"}}, config.FormatAlternativesForTest("{hostname}"))

	// {now} with no spec is borg's default rendering, so it decomposes the same
	// way: literals and directives, not one opaque blob.
	alts := config.FormatAlternativesForTest("{now}")
	assert.Greater(t, len(alts), 1, "the default timestamp is structured too")

	// A placeholder this code cannot characterize matches anything, which
	// reports a collision rather than missing one.
	assert.Equal(t, [][]string{nil}, config.FormatAlternativesForTest("{something-new}"))
	assert.True(t, config.PatternMatchesFormatForTest("literally-anything-*", "{something-new}"))

	// The same for a directive inside a spec that this code does not know: the
	// segment must widen, not narrow. Narrowing it to a fixed value would make
	// two formats look disjoint because a value nobody can predict happened not
	// to match. %Z is a timezone abbreviation, which has no shape worth
	// characterizing.
	assert.Equal(t, [][]string{{"x-"}, nil, {"-y"}}, config.FormatAlternativesForTest("x-{now:%Z}-y"))
	assert.True(t, config.PatternMatchesFormatForTest("x-anything-at-all-y", "x-{now:%Z}-y"))
	assert.True(t, config.PatternMatchesFormatForTest("x-*-y", "x-{now:%Z}-y"))

	// A directive whose length is known but whose values are not is a digit run:
	// bounded, so it does not mark unrelated groups ambiguous, and complete, so
	// it does not miss one.
	assert.Equal(t, [][]string{{"x-"}, {"<3 digits>"}, {"-y"}}, config.FormatAlternativesForTest("x-{now:%j}-y"))
	assert.True(t, config.PatternMatchesFormatForTest("x-366-y", "x-{now:%j}-y"))
	assert.False(t, config.PatternMatchesFormatForTest("x-abc-y", "x-{now:%j}-y"))
}

// The collision the digits-only stand-in hid: with this hostname, group "prod"
// writes "db-app-node-prod-..." and group "app" claims it with "*-app-*".
func TestAHostnameCanCreateACollision(t *testing.T) {
	defer config.SetSampleHostname("db-app-node")()

	st := models.NewBackupState()
	st.AddVolume("app", models.VolumeInfo{Name: "v1", HostPath: "/mnt/v1"})
	st.AddVolume("prod", models.VolumeInfo{Name: "v2", HostPath: "/mnt/v2"})
	cfg := &config.ManagerConfig{Borgmatic: map[string]interface{}{
		"repositories": []interface{}{map[string]interface{}{"path": "/mnt/shared"}},
	}}

	g, _ := newTestGenerator(t, cfg, nil, config.GeneratorOptions{})
	meta, _, err := g.Generate(st)
	require.NoError(t, err)

	assert.NotEmpty(t, meta["app"].AmbiguousRepos,
		`"*-app-*" matches "db-app-node-prod-..." through the hostname`)
	assert.NotEmpty(t, meta["prod"].AmbiguousRepos)
}

// The control: an ordinary hostname leaves those same two groups distinguishable.
func TestAnOrdinaryHostnameLeavesGroupsDistinguishable(t *testing.T) {
	defer config.SetSampleHostname("backup01")()

	st := models.NewBackupState()
	st.AddVolume("app", models.VolumeInfo{Name: "v1", HostPath: "/mnt/v1"})
	st.AddVolume("prod", models.VolumeInfo{Name: "v2", HostPath: "/mnt/v2"})
	cfg := &config.ManagerConfig{Borgmatic: map[string]interface{}{
		"repositories": []interface{}{map[string]interface{}{"path": "/mnt/shared"}},
	}}

	g, _ := newTestGenerator(t, cfg, nil, config.GeneratorOptions{})
	meta, _, err := g.Generate(st)
	require.NoError(t, err)

	assert.Empty(t, meta["app"].AmbiguousRepos)
	assert.Empty(t, meta["prod"].AmbiguousRepos)
}

// Ambiguity belongs to a repository, not to a group. A group sharing one
// destination with a colliding sibling and holding another to itself keeps
// confirmation on the second: nothing else writes there.
func TestAmbiguityNamesOnlyTheSharedRepository(t *testing.T) {
	defer config.SetSampleHostname("backup01")()

	st := models.NewBackupState()
	st.AddVolume("app", models.VolumeInfo{Name: "v1", HostPath: "/mnt/v1"})
	st.AddVolume("app-prod", models.VolumeInfo{Name: "v2", HostPath: "/mnt/v2"})

	cfg := &config.ManagerConfig{Borgmatic: map[string]interface{}{
		"repositories": []interface{}{map[string]interface{}{"path": "/mnt/shared"}},
	}}
	// app also backs up to a repository of its own.
	overrides := map[string]map[string]interface{}{
		"app": {"repositories": []interface{}{
			map[string]interface{}{"path": "/mnt/shared"},
			map[string]interface{}{"path": "/mnt/app-only"},
		}},
	}

	g, _ := newTestGenerator(t, cfg, overrides, config.GeneratorOptions{})
	meta, _, err := g.Generate(st)
	require.NoError(t, err)

	assert.Equal(t, []string{config.CanonicalRepoKey("/mnt/shared")}, meta["app"].AmbiguousRepos,
		"only the destination it shares with the colliding sibling")
	assert.NotContains(t, meta["app"].AmbiguousRepos, config.CanonicalRepoKey("/mnt/app-only"),
		"the repository it has to itself can still confirm")
	assert.Equal(t, []string{config.CanonicalRepoKey("/mnt/shared")}, meta["app-prod"].AmbiguousRepos)
}

// A group refused for lacking the {group} token never runs, so it writes nothing
// that could contaminate a survivor's repository. Judging collisions against the
// pre-refusal set instead punished the survivor twice: the refused group has no
// pattern, which reads as "cannot be distinguished", so the survivor lost
// success confirmation in a repository it now has entirely to itself, and warned
// about a conflict with a group that is not running.
func TestARefusedGroupDoesNotMakeASurvivorAmbiguous(t *testing.T) {
	defer config.SetSampleHostname("backup01")()

	st := models.NewBackupState()
	st.AddVolume("good", models.VolumeInfo{Name: "v1", HostPath: "/mnt/v1"})
	st.AddVolume("bad", models.VolumeInfo{Name: "v2", HostPath: "/mnt/v2"})

	cfg := &config.ManagerConfig{Borgmatic: map[string]interface{}{
		"repositories": []interface{}{map[string]interface{}{"path": "/mnt/shared"}},
	}}
	// "bad" shares the repository but names its archives without the {group}
	// token, so generation refuses it.
	overrides := map[string]map[string]interface{}{
		"bad": {"archive_name_format": "{hostname}-fixed-{now}"},
	}

	g, _ := newTestGenerator(t, cfg, overrides, config.GeneratorOptions{})
	meta, _, err := g.Generate(st)
	require.NoError(t, err)

	require.NotContains(t, meta, "bad", "the group without the token is refused")
	require.Contains(t, meta, "good")
	assert.Empty(t, meta["good"].AmbiguousRepos,
		"the survivor has the repository to itself and can still confirm its backups")
}

// The end-to-end shape of the same collision, through generation: a group named
// after a month number and a sibling that puts the month in its archive names.
func TestAGroupNamedLikeADateCollidesWithAFormattedTimestamp(t *testing.T) {
	defer config.SetSampleHostname("backup01")()

	st := models.NewBackupState()
	st.AddVolume("07", models.VolumeInfo{Name: "v1", HostPath: "/mnt/v1"})
	st.AddVolume("prod", models.VolumeInfo{Name: "v2", HostPath: "/mnt/v2"})

	cfg := &config.ManagerConfig{Borgmatic: map[string]interface{}{
		"repositories":        []interface{}{map[string]interface{}{"path": "/mnt/shared"}},
		"archive_name_format": "x-{group}-{now}",
	}}
	overrides := map[string]map[string]interface{}{
		"prod": {"archive_name_format": "x-{now:%m}-{group}-{now}"},
	}

	g, _ := newTestGenerator(t, cfg, overrides, config.GeneratorOptions{})
	meta, _, err := g.Generate(st)
	require.NoError(t, err)

	assert.NotEmpty(t, meta["07"].AmbiguousRepos,
		`"x-07-*" claims the archives prod writes every July`)
	assert.NotEmpty(t, meta["prod"].AmbiguousRepos)
}

// An at-sign in a local path is not a remote repository. Treating it as one
// skipped the cleaning and symlink resolution that make two spellings compare
// equal, so two groups writing to the same repository got different lock keys:
// they could run against one Borg repository concurrently, and generation's
// shared-repository archive checks never fired.
func TestLocalPathsWithAtSignsAreNotRemote(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.MkdirAll(dir+"/backups@local/repo", 0o755))

	assert.Equal(t,
		config.CanonicalRepoKey(dir+"/backups@local/repo"),
		config.CanonicalRepoKey(dir+"/backups@local/./repo"),
		"two spellings of one local repository must share a key, at-sign or not")

	// The genuinely remote forms are unaffected.
	assert.Equal(t, "ssh://borg@host/./repo", config.CanonicalRepoKey("ssh://borg@host/./repo/"))
	assert.Equal(t, "borg@host:/srv/repo", config.CanonicalRepoKey("borg@host:/srv/repo"))
	assert.Equal(t, "host:/srv/repo", config.CanonicalRepoKey("host:/srv/repo"),
		"borg accepts host:path with no user")

	// An absolute path is never remote, whatever it contains, so it is still
	// cleaned rather than passed through as a host spec.
	assert.Equal(t, "/srv/a@b:c/repo", config.CanonicalRepoKey("/srv/a@b:c/./repo"))

	// A relative path with no host separator is local too, and cleaning it is
	// what tells the two spellings apart from a remote spec passed through whole.
	assert.Equal(t, "repo@dir/x", config.CanonicalRepoKey("repo@dir/./x"))
	assert.Equal(t, "repo@dir/x", config.CanonicalRepoKey("repo@dir/y/../x"))

	// A colon with nothing usable in front of it is not a host spec either, so
	// it is still cleaned like the local path it is.
	assert.Equal(t, "8080/repo", config.CanonicalRepoKey(":8080/../8080/repo"))
}

// A repository outlives any window of years that could be enumerated, and its
// retained archives still carry the year they were written in. A group named
// after a past year therefore claims those archives, and its retention can prune
// them, however far back the year is.
func TestYearsOutsideAnyWindowStillCollide(t *testing.T) {
	defer config.SetSampleHostname("myhost")()

	for _, year := range []string{"2020", "1999", "2031", "2026"} {
		assert.True(t, config.PatternMatchesFormatForTest("x-"+year+"-*", "x-{now:%Y}-prod-{now}"),
			"a group named %q claims the archives written in that year", year)
	}

	// Still a year-shaped run, not anything at all: three digits or letters are
	// not a four-digit year, and reporting them as one would mark unrelated
	// groups ambiguous.
	assert.False(t, config.PatternMatchesFormatForTest("x-202-*", "x-{now:%Y}-prod-{now}"))
	assert.False(t, config.PatternMatchesFormatForTest("x-20200-*", "x-{now:%Y}-prod-{now}"))
	assert.False(t, config.PatternMatchesFormatForTest("x-abcd-*", "x-{now:%Y}-prod-{now}"))

	// Two-digit years the same way.
	assert.True(t, config.PatternMatchesFormatForTest("x-99-*", "x-{now:%y}-prod-{now}"))
	assert.False(t, config.PatternMatchesFormatForTest("x-999-*", "x-{now:%y}-prod-{now}"))

	// And through the collision decision, which is where it matters.
	assert.True(t, config.PatternsCollideForTest(
		"x-2020-*", "x-{group}-{now}",
		"x-*-prod-*", "x-{now:%Y}-prod-{now}"))
}

// Two groups pointing at different environment-backed destinations do not share
// a repository. Collapsing both to the unknown key serialized unrelated backups
// against each other and, when their formats omitted {group}, refused groups
// that never shared anything.
func TestDefinedEnvironmentRepositoriesResolveToTheirDestinations(t *testing.T) {
	t.Setenv("LOCAL_REPO", "/mnt/local")
	t.Setenv("OFFSITE_REPO", "/mnt/offsite")

	local := config.CanonicalRepoKey("${LOCAL_REPO}")
	offsite := config.CanonicalRepoKey("${OFFSITE_REPO}")
	assert.NotEqual(t, local, offsite, "different destinations are different repositories")
	assert.NotEqual(t, config.UnknownRepoKey, local)
	assert.Equal(t, config.CanonicalRepoKey("/mnt/local"), local,
		"and the resolved key is the one the literal path would have")

	t.Run("an undefined variable stays conservative", func(t *testing.T) {
		assert.Equal(t, config.UnknownRepoKey, config.CanonicalRepoKey("${NOT_DEFINED_ANYWHERE}"))
	})

	t.Run("two undefined variables still collide", func(t *testing.T) {
		assert.Equal(t, config.CanonicalRepoKey("${NOT_A}"), config.CanonicalRepoKey("${NOT_B}"),
			"nothing here can tell whether they are the same repository")
	})
}

// borg matches archive names as shell patterns, so a format carrying a literal
// "?" or "[" produces a pattern that is not literal at all. Treating those as
// ordinary characters declared such a pair disjoint while borg's own retention
// would have crossed the boundary.
func TestGlobMetacharactersInAPatternAreMatchedAsBorgMatchesThem(t *testing.T) {
	defer config.SetSampleHostname("myhost")()

	t.Run("a question mark matches any single character", func(t *testing.T) {
		assert.True(t, config.PatternMatchesFormatForTest("snap?app-*", "snapXapp-{now}"))
		assert.False(t, config.PatternMatchesFormatForTest("snap?app-*", "snapXYapp-{now}"),
			"one character, not any run of them")
	})

	t.Run("a character class matches its members", func(t *testing.T) {
		assert.True(t, config.PatternMatchesFormatForTest("snap[abc]app-*", "snapbapp-{now}"))
		assert.False(t, config.PatternMatchesFormatForTest("snap[abc]app-*", "snapzapp-{now}"))
	})

	t.Run("ranges and negation", func(t *testing.T) {
		assert.True(t, config.PatternMatchesFormatForTest("x-[0-9]-*", "x-7-{now}"))
		assert.False(t, config.PatternMatchesFormatForTest("x-[0-9]-*", "x-a-{now}"))
		assert.True(t, config.PatternMatchesFormatForTest("x-[!0-9]-*", "x-a-{now}"))
		assert.False(t, config.PatternMatchesFormatForTest("x-[!0-9]-*", "x-7-{now}"))
	})

	t.Run("an unterminated bracket is a literal bracket", func(t *testing.T) {
		assert.True(t, config.PatternMatchesFormatForTest("x-[abc-*", "x-[abc-{now}"))
	})

	t.Run("a metacharacter still constrains a digit run", func(t *testing.T) {
		assert.True(t, config.PatternMatchesFormatForTest("x-????-*", "x-{now:%Y}-{now}"),
			"four of anything covers a four-digit year")
		assert.False(t, config.PatternMatchesFormatForTest("x-[a-z][a-z][a-z][a-z]-*", "x-{now:%Y}-{now}"),
			"letters cannot be a year")
	})

	// Through the collision decision, which is the point.
	assert.True(t, config.PatternsCollideForTest(
		"snap?app-*", "snap?{group}-{now}",
		"snapXapp-*", "snapXapp-{now}"))
}

// The fully qualified and reverse names come from resolver configuration and a
// PTR lookup, so a value derived here could differ from what borg writes. A
// wrong literal is worse than none: it declares two groups disjoint on the
// strength of a name neither uses.
func TestUnreproducibleHostPlaceholdersAreConservative(t *testing.T) {
	defer config.SetSampleHostname("db")()

	assert.Equal(t, [][]string{nil}, config.FormatAlternativesForTest("{fqdn}"))
	assert.Equal(t, [][]string{nil}, config.FormatAlternativesForTest("{reverse-hostname}"))
	assert.True(t, config.PatternMatchesFormatForTest("*-app-*", "{fqdn}-prod-{now}"),
		"an fqdn that could contain anything must not be ruled out")

	// {hostname} offers both spellings borg may render, so a collision through
	// either is found and neither is asserted to be the one.
	defer config.SetSampleHostname("db.example.com")()
	assert.Equal(t, [][]string{{"db.example.com", "db"}}, config.FormatAlternativesForTest("{hostname}"))
	assert.True(t, config.PatternMatchesFormatForTest("db-*", "{hostname}-{now}"))
	assert.True(t, config.PatternMatchesFormatForTest("db.example.com-*", "{hostname}-{now}"))
}

// borg matches shell patterns over characters, so "?" covers a whole multi-byte
// rune. Walking bytes had it swallow only the first third of an "é" and declare
// two formats disjoint that borg would not.
func TestGlobWildcardsMatchWholeCharacters(t *testing.T) {
	defer config.SetSampleHostname("myhost")()

	assert.True(t, config.PatternMatchesFormatForTest("snap?app-*", "snapéapp-{now}"),
		"one character, however many bytes it takes")
	assert.False(t, config.PatternMatchesFormatForTest("snap??app-*", "snapéapp-{now}"),
		"and only one")
	assert.True(t, config.PatternMatchesFormatForTest("x-?-*", "x-日-{now}"))

	t.Run("a literal multi-byte character still matches itself", func(t *testing.T) {
		assert.True(t, config.PatternMatchesFormatForTest("snapéapp-*", "snapéapp-{now}"))
		assert.False(t, config.PatternMatchesFormatForTest("snapéapp-*", "snapèapp-{now}"))
	})

	t.Run("an ascii class does not accept a multi-byte character", func(t *testing.T) {
		assert.False(t, config.PatternMatchesFormatForTest("snap[a-z]app-*", "snapéapp-{now}"))
		assert.True(t, config.PatternMatchesFormatForTest("snap[!a-z]app-*", "snapéapp-{now}"),
			"but a negated one does")
	})

	// Through the collision decision, where it decides whether retention can
	// cross the boundary.
	assert.True(t, config.PatternsCollideForTest(
		"snap?app-*", "snap?{group}-{now}",
		"snapéapp-*", "snapéapp-{now}"))
}

// borg reads a leading "re:", "sh:" or "pp:" as a syntax selector, so a format
// beginning with one produces a pattern borg interprets as a regular expression
// while the collision check models it as a shell glob. The two then disagree
// about which archives belong to this group.
func TestAFormatStartingWithAPatternSelectorStillModelsAsAGlob(t *testing.T) {
	defer config.SetSampleHostname("myhost")()

	// The check treats "re:" as literal text, which is what borg will do once
	// the probe pins the syntax with "sh:".
	assert.True(t, config.PatternMatchesFormatForTest("re:.*app-*", "re:.*app-{now}"))
	assert.False(t, config.PatternMatchesFormatForTest("re:.*app-*", "re:XYapp-{now}"),
		`the "." is a literal dot here, not a regex wildcard`)
}

// A refused group's repositories travel with the refusal, because the group
// never runs and nothing else will ever report what it configures.
func TestARefusalCarriesTheGroupsRepositories(t *testing.T) {
	defer config.SetSampleHostname("backup01")()

	st := models.NewBackupState()
	st.AddVolume("good", models.VolumeInfo{Name: "v1", HostPath: "/mnt/v1"})
	st.AddVolume("bad", models.VolumeInfo{Name: "v2", HostPath: "/mnt/v2"})

	cfg := &config.ManagerConfig{Borgmatic: map[string]interface{}{
		"repositories": []interface{}{map[string]interface{}{"path": "/mnt/shared", "label": "shared"}},
	}}
	overrides := map[string]map[string]interface{}{
		"bad": {"archive_name_format": "{hostname}-fixed-{now}"},
	}

	g, _ := newTestGenerator(t, cfg, overrides, config.GeneratorOptions{})
	_, refusals, err := g.Generate(st)
	require.NoError(t, err)

	require.Len(t, refusals, 1)
	assert.Equal(t, "bad", refusals[0].Group)
	require.Len(t, refusals[0].Repositories, 1)
	assert.Equal(t, "shared", refusals[0].Repositories[0].Label)
	assert.Equal(t, "/mnt/shared", refusals[0].Repositories[0].Path)
}

// A directory component ending in ":" makes an absolute path look like a URL.
// Returning it uncleaned gave it a different lock key from the spelling POSIX
// says is the same path, so two groups ran concurrently against one repository
// and skipped the shared-repository archive checks.
func TestAnAbsolutePathIsLocalWhateverPunctuationItHolds(t *testing.T) {
	assert.Equal(t,
		config.CanonicalRepoKey("/srv/a:/repo"),
		config.CanonicalRepoKey("/srv/a://repo"),
		"a repeated separator is collapsed; both name the same path")

	assert.Equal(t, "/srv/a:/repo", config.CanonicalRepoKey("/srv/a://repo"))
	assert.Equal(t, "/srv/x/repo", config.CanonicalRepoKey("/srv/x/./repo"))

	t.Run("genuine remote forms are untouched", func(t *testing.T) {
		assert.Equal(t, "ssh://borg@host/./repo", config.CanonicalRepoKey("ssh://borg@host/./repo/"))
		assert.Equal(t, "sftp://host/srv/repo", config.CanonicalRepoKey("sftp://host/srv/repo"))
		assert.Equal(t, "borg@host:/srv/repo", config.CanonicalRepoKey("borg@host:/srv/repo"))
	})
}

// A malformed repositories value yields no refs for the same reason an empty
// list does. Confusing the two makes the reconciliation delete every persisted
// record for a group whose operator was mid-edit.
func TestExtractRepoRefsDistinguishesEmptyFromUnreadable(t *testing.T) {
	for _, tc := range []struct {
		name       string
		value      interface{}
		present    bool
		wantRefs   int
		understood bool
	}{
		{name: "absent", present: false, understood: true},
		{name: "empty list", value: []interface{}{}, present: true, understood: true},
		{name: "paths", value: []interface{}{"/mnt/a", "/mnt/b"}, present: true, wantRefs: 2, understood: true},
		{name: "mappings", present: true, wantRefs: 1, understood: true,
			value: []interface{}{map[string]interface{}{"path": "/mnt/a", "label": "local"}}},
		{name: "a scalar where a list belongs", value: "/mnt/a", present: true, understood: false},
		{name: "a mapping with no path", present: true, understood: false,
			value: []interface{}{map[string]interface{}{"label": "local"}}},
		{name: "a path of the wrong type", present: true, understood: false,
			value: []interface{}{map[string]interface{}{"path": 42}}},
		{name: "an empty string entry", value: []interface{}{""}, present: true, understood: false},
		{name: "an entry of an unknown shape", value: []interface{}{42}, present: true, understood: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			final := map[string]interface{}{}
			if tc.present {
				final["repositories"] = tc.value
			}
			refs, understood := config.ExtractRepoRefsForTest(final)
			assert.Equal(t, tc.understood, understood)
			assert.Len(t, refs, tc.wantRefs)
		})
	}
}
