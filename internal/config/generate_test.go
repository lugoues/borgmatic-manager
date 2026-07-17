package config_test

import (
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
func newTestGenerator(t *testing.T, cfg *config.ManagerConfig, overrides map[string]map[string]interface{}, opts config.GeneratorOptions) (*config.Generator, string) {
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
	_, err := g.Generate(state)
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
	_, err := g.Generate(state)
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
	_, err := g.Generate(state)
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
	_, err := g.Generate(state)
	require.NoError(t, err)

	parsed := readGenerated(t, outDir, "app")
	assert.Equal(t, "/run/borgmatic-manager", parsed["user_runtime_directory"])
	assert.Equal(t, "/var/lib/borgmatic-manager", parsed["user_state_directory"])
}

func TestGenerateFilePermissions(t *testing.T) {
	state := models.NewBackupState()
	state.AddVolume("app", models.VolumeInfo{Name: "v", HostPath: "/mnt/v"})

	g, outDir := newTestGenerator(t, emptyConfig(), nil, config.GeneratorOptions{})
	_, err := g.Generate(state)
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
	_, err := g.Generate(state)
	require.NoError(t, err)

	parsed := readGenerated(t, outDir, "myapp")
	format, ok := parsed["archive_name_format"].(string)
	require.True(t, ok)
	assert.Equal(t, "{hostname}-myapp-{now:%Y-%m-%d_%H:%M}", format)
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
	meta, err := g.Generate(state)
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
	meta2, err := g.Generate(state)
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
	_, err := g.Generate(state)
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
	_, err := g.Generate(state)
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
	_, err := g.Generate(state)
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
	_, err := g.Generate(state)
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
	_, err := g.Generate(state)
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

	_, err := g.Generate(state)
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
	meta, err := g.Generate(state)
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
	meta, err := g.Generate(state)
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

	_, err := g.Generate(state)
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
	_, err := g.Generate(state)
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
	_, err := g.Generate(state)
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
	_, err := g.Generate(state)
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
	_, err := g.Generate(state)
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
	_, err := g.Generate(state)
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
	meta, err := g.Generate(state)
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

	meta, err := g.Generate(state)
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

	meta, err := g.Generate(state)
	require.NoError(t, err)

	assert.Empty(t, meta, "a format merely containing the group names as substrings must be refused, not accepted")
	for _, group := range []string{"app", "data"} {
		_, statErr := os.Stat(filepath.Join(outDir, group+".yaml"))
		assert.True(t, os.IsNotExist(statErr), "no config may be written for refused group %s", group)
	}
	assert.Contains(t, buf.String(), "must contain the literal {group} token")
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
	meta, err := g.Generate(state)
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

	meta, err := g.Generate(state)
	require.NoError(t, err)

	assert.Len(t, meta, 2, "prefix collisions warn, they do not refuse")
	assert.Contains(t, buf.String(), "retention can cross group boundaries")
}
