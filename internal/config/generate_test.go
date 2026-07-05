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
	assert.Contains(t, srcDirs, "/var/lib/docker/volumes/web_data/_data")
	assert.Contains(t, srcDirs, "/var/lib/docker/volumes/web_assets/_data")
	assert.NotContains(t, parsed, "working_directory",
		"working_directory was a container-mount concept; host paths are absolute")
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

func TestGenerateContainerModeDatabases(t *testing.T) {
	state := models.NewBackupState()
	state.AddDatabases("db-group", []models.DatabaseConfig{
		{Type: "postgresql", Name: "appdb", Username: "pguser", Container: "pg-svc", NetworkMode: "bridge"},
		{Type: "mariadb", Name: "wiki", Username: "wiki", Password: "pw", Port: 3306, Container: "maria-svc", NetworkMode: "bridge"},
	})

	g, outDir := newTestGenerator(t, emptyConfig(), nil, config.GeneratorOptions{})
	_, err := g.Generate(state)
	require.NoError(t, err)

	parsed := readGenerated(t, outDir, "db-group")

	pgDBs, ok := parsed["postgresql_databases"].([]interface{})
	require.True(t, ok, "should have postgresql_databases")
	require.Len(t, pgDBs, 1)
	pg := pgDBs[0].(map[string]interface{})
	assert.Equal(t, "pg-svc", pg["container"], "default mode connects via the labeled container")
	assert.Equal(t, "pguser", pg["username"])
	assert.NotContains(t, pg, "hostname")

	mariaDBs, ok := parsed["mariadb_databases"].([]interface{})
	require.True(t, ok)
	require.Len(t, mariaDBs, 1)
	maria := mariaDBs[0].(map[string]interface{})
	assert.Equal(t, "maria-svc", maria["container"])
	assert.Equal(t, 3306, maria["port"])
	assert.Equal(t, "pw", maria["password"])
}

func TestGenerateHostnameModeOverridesContainer(t *testing.T) {
	state := models.NewBackupState()
	state.AddDatabases("db-group", []models.DatabaseConfig{
		{Type: "postgresql", Name: "appdb", Username: "u", Hostname: "127.0.0.1", Port: 5433, Container: "pg-svc"},
	})

	g, outDir := newTestGenerator(t, emptyConfig(), nil, config.GeneratorOptions{})
	_, err := g.Generate(state)
	require.NoError(t, err)

	parsed := readGenerated(t, outDir, "db-group")
	pg := parsed["postgresql_databases"].([]interface{})[0].(map[string]interface{})
	assert.Equal(t, "127.0.0.1", pg["hostname"])
	assert.Equal(t, 5433, pg["port"])
	assert.NotContains(t, pg, "container", "hostname mode must not emit container")
}

func TestGenerateRefusesContainerModeWhenRootless(t *testing.T) {
	state := models.NewBackupState()
	state.AddDatabases("db-group", []models.DatabaseConfig{
		{Type: "postgresql", Name: "appdb", Username: "u", Container: "pg-svc"},
	})
	state.AddVolume("db-group", models.VolumeInfo{Name: "v", HostPath: "/mnt/v"})

	g, outDir := newTestGenerator(t, emptyConfig(), nil, config.GeneratorOptions{Rootless: true})
	_, err := g.Generate(state)
	require.NoError(t, err)

	parsed := readGenerated(t, outDir, "db-group")
	assert.NotContains(t, parsed, "postgresql_databases",
		"container mode cannot work rootless; the entry must be refused, not emitted broken")
}

func TestGenerateRefusesContainerModeForHostNetwork(t *testing.T) {
	state := models.NewBackupState()
	state.AddDatabases("db-group", []models.DatabaseConfig{
		{Type: "postgresql", Name: "appdb", Username: "u", Container: "pg-svc", NetworkMode: "host"},
		{Type: "postgresql", Name: "gooddb", Username: "u", Container: "pg2", NetworkMode: "bridge"},
	})

	g, outDir := newTestGenerator(t, emptyConfig(), nil, config.GeneratorOptions{})
	_, err := g.Generate(state)
	require.NoError(t, err)

	parsed := readGenerated(t, outDir, "db-group")
	pgDBs := parsed["postgresql_databases"].([]interface{})
	require.Len(t, pgDBs, 1, "host-network entry refused, bridge entry kept")
	assert.Equal(t, "gooddb", pgDBs[0].(map[string]interface{})["name"])
}

func TestGenerateHostnameModeWorksRootless(t *testing.T) {
	state := models.NewBackupState()
	state.AddDatabases("db-group", []models.DatabaseConfig{
		{Type: "mariadb", Name: "wiki", Username: "u", Hostname: "127.0.0.1", Port: 13306, Container: "maria"},
	})

	g, outDir := newTestGenerator(t, emptyConfig(), nil, config.GeneratorOptions{Rootless: true})
	_, err := g.Generate(state)
	require.NoError(t, err)

	parsed := readGenerated(t, outDir, "db-group")
	maria := parsed["mariadb_databases"].([]interface{})[0].(map[string]interface{})
	assert.Equal(t, "127.0.0.1", maria["hostname"])
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
		{Type: "postgresql", Name: "appdb", Username: "u", Container: "pg", NetworkMode: "bridge"},
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

	// Simulate a config left over from a removed group, plus a non-config file.
	require.NoError(t, os.WriteFile(filepath.Join(outDir, "removed-group.yaml"), []byte("stale"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(outDir, "notes.txt"), []byte("keep"), 0o600))

	_, err := g.Generate(state)
	require.NoError(t, err)

	_, err = os.Stat(filepath.Join(outDir, "current.yaml"))
	require.NoError(t, err)
	_, err = os.Stat(filepath.Join(outDir, "removed-group.yaml"))
	assert.True(t, os.IsNotExist(err), "stale group config must be removed")
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
