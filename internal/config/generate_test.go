package config_test

import (
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

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
	gen, err := g.Generate(state)
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
	meta, err := g.Generate(state)
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
	meta, err := g.Generate(state)
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
		meta, err := g.Generate(st)
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

func TestGlobMatches(t *testing.T) {
	for _, tc := range []struct {
		pattern, name string
		want          bool
	}{
		{"app*", "apple20260102150405", true},
		{"apple*", "app20260102150405", false},
		{"*-app-*", "host-app-prod-20260102150405", true},
		{"*-app-*", "host-other-20260102150405", false},
		{"*-app", "20260102150405-apple", false},
		{"*-app", "20260102150405-app", true},
		{"*", "anything", true},
		{"exact", "exact", true},
		{"exact", "exactly", false},
		{"a*b*c", "axxbyyc", true},
		{"a*b*c", "axxc", false},
	} {
		t.Run(tc.pattern+"_vs_"+tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, config.GlobMatchesForTest(tc.pattern, tc.name))
		})
	}
}

func TestPatternsCollide(t *testing.T) {
	// The check runs in both directions because the two groups' formats need not
	// be the same: a per-group override can give one group a format whose
	// literal text is the other group's whole name. Only the second direction
	// sees that, and group order here is alphabetical, which always puts a
	// prefix before its extension, so the first direction alone would miss it.
	t.Run("a collision only the second direction sees", func(t *testing.T) {
		assert.False(t, config.GlobMatchesForTest("alpha", "alpha20260102150405"),
			"alpha's own pattern does not match beta's names")
		assert.True(t, config.PatternsCollideForTest(
			"alpha", []string{"alpha"}, // group alpha, format "{group}"
			"alpha*", []string{"alpha20260102150405"}), // group beta, format "alpha{now}"
			"but beta's pattern claims alpha's archives")
	})

	t.Run("distinguishable patterns do not collide", func(t *testing.T) {
		assert.False(t, config.PatternsCollideForTest(
			"*-app-*", []string{"20260102150405-app-20260102150405"},
			"*-other-*", []string{"20260102150405-other-20260102150405"}))
	})

	// Unreachable through generation today, because a shared-repo group whose
	// archive_name_format lacks the {group} token is refused before this runs.
	// Kept because "nothing is known about this group's archive names" must not
	// read as "its archives are distinguishable" if that ever changes.
	t.Run("an unknown pattern is treated as colliding", func(t *testing.T) {
		assert.True(t, config.PatternsCollideForTest("", nil, "*-app-*", []string{"x-app-y"}))
		assert.True(t, config.PatternsCollideForTest("*-app-*", []string{"x-app-y"}, "", nil))
	})
}

// The sample renders what borg would actually write. A placeholder whose value
// this host knows is rendered as that value; one whose value cannot be known
// falls back to a digits-only stand-in that introduces no separators.
func TestArchiveSampleName(t *testing.T) {
	defer config.SetSampleHostname("myhost")()

	assert.Equal(t, "plain", config.ArchiveSampleNameForTest("plain"))
	assert.NotContains(t, config.ArchiveSampleNameForTest("app{unknowable}"), "{",
		"a placeholder left literal would never match a real archive name")

	assert.Equal(t, "myhost-app-2026-07-27T16:04:26",
		config.ArchiveSampleNameForTest("{hostname}-app-{now}"),
		"the hostname is known, and {now} renders in borg's default form")

	// The name is what precedes the colon.
	assert.Equal(t, "myhost-x", config.ArchiveSampleNameForTest("{hostname:%s}-x"),
		"a format spec does not change which placeholder it is")
	assert.Equal(t, "myhost-x", config.ArchiveSampleNameForTest("{ HOSTNAME }-x"),
		"nor does case or padding")

	// A formatted timestamp renders through its directives, which is how a
	// two-digit month can collide with a group named after one.
	assert.Equal(t, "x-07-y", config.ArchiveSampleNameForTest("x-{now:%m}-y"))
	assert.Equal(t, "2026-07-27", config.ArchiveSampleNameForTest("{now:%Y-%m-%d}"))

	// An unsupported directive gives up rather than rendering half a value.
	assert.Equal(t, "x-20260102150405-y", config.ArchiveSampleNameForTest("x-{now:%j}-y"))
}

func TestStrftimeLayout(t *testing.T) {
	for _, tc := range []struct {
		spec, layout string
		ok           bool
	}{
		{spec: "%Y-%m-%d", layout: "2006-01-02", ok: true},
		{spec: "%H:%M:%S", layout: "15:04:05", ok: true},
		{spec: "%m", layout: "01", ok: true},
		{spec: "%b %a", layout: "Jan Mon", ok: true},
		{spec: "backup-%Y", layout: "backup-2006", ok: true},
		{spec: "100%%", layout: "100%", ok: true},
		{spec: "%j", ok: false},         // day of year has no Go layout
		{spec: "%Y-%q", ok: false},      // unknown directive
		{spec: "trailing %", ok: false}, // dangling percent
	} {
		t.Run(tc.spec, func(t *testing.T) {
			layout, ok := config.StrftimeLayoutForTest(tc.spec)
			assert.Equal(t, tc.ok, ok)
			if tc.ok {
				assert.Equal(t, tc.layout, layout)
			}
		})
	}
}

// A single sample answers only for the date it used. Codex's case: group "07"
// has the pattern "*-07-*", and a sibling whose format includes {now:%m} writes
// "x-07-prod-..." every July. Whether they collide depends on the month, so the
// check renders across the year.
func TestACollisionThatOnlyExistsInSomeMonthsIsStillFound(t *testing.T) {
	defer config.SetSampleHostname("myhost")()

	names := config.ArchiveSampleNamesForTest("x-{now:%m}-prod-{now}")

	// Every month is rendered, so the check does not depend on when it runs.
	for m := 1; m <= 12; m++ {
		prefix := fmt.Sprintf("x-%02d-prod-", m)
		found := false
		for _, name := range names {
			if strings.HasPrefix(name, prefix) {
				found = true
				break
			}
		}
		assert.True(t, found, "month %02d must be represented", m)
	}

	assert.True(t, config.PatternsCollideForTest(
		"x-07-*", config.ArchiveSampleNamesForTest("x-{group}-{now}"),
		"x-*-prod-*", names),
		`the group named "07" claims the July archives of its sibling`)

	// The same for a day-of-month format, which twelve monthly samples missed:
	// they only ever rendered days 2 to 13.
	assert.True(t, config.PatternsCollideForTest(
		"x-31-*", config.ArchiveSampleNamesForTest("x-{group}-{now}"),
		"x-*-prod-*", config.ArchiveSampleNamesForTest("x-{now:%d}-prod-{now}")),
		`the group named "31" claims what its sibling writes on the 31st`)

	// And for hours, minutes and seconds, whose domains are larger still.
	for _, tc := range []struct{ group, directive string }{
		{"23", "%H"}, {"59", "%M"}, {"07", "%S"},
	} {
		assert.True(t, config.PatternsCollideForTest(
			"x-"+tc.group+"-*", config.ArchiveSampleNamesForTest("x-{group}-{now}"),
			"x-*-prod-*", config.ArchiveSampleNamesForTest("x-{now:"+tc.directive+"}-prod-{now}")),
			"a group named %q collides with a sibling formatting %s", tc.group, tc.directive)
	}
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
	meta, err := g.Generate(st)
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
	meta, err := g.Generate(st)
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
	meta, err := g.Generate(st)
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
	meta, err := g.Generate(st)
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
	meta, err := g.Generate(st)
	require.NoError(t, err)

	assert.NotEmpty(t, meta["07"].AmbiguousRepos,
		`"x-07-*" claims the archives prod writes every July`)
	assert.NotEmpty(t, meta["prod"].AmbiguousRepos)
}

// A year is as plausible a group name as a month, and the check runs for years
// on end, so both the current year and the next one are covered.
func TestSampleInstantsCoverEveryDirectiveValue(t *testing.T) {
	seen := func(format string) map[string]bool {
		out := map[string]bool{}
		for _, name := range config.ArchiveSampleNamesForTest(format) {
			out[name] = true
		}
		return out
	}

	months := seen("{now:%m}")
	for m := 1; m <= 12; m++ {
		assert.True(t, months[fmt.Sprintf("%02d", m)], "month %02d", m)
	}
	days := seen("{now:%d}")
	for d := 1; d <= 31; d++ {
		assert.True(t, days[fmt.Sprintf("%02d", d)], "day %02d", d)
	}
	hours := seen("{now:%H}")
	for h := 0; h < 24; h++ {
		assert.True(t, hours[fmt.Sprintf("%02d", h)], "hour %02d", h)
	}
	minutes := seen("{now:%M}")
	for m := 0; m < 60; m++ {
		assert.True(t, minutes[fmt.Sprintf("%02d", m)], "minute %02d", m)
	}
	seconds := seen("{now:%S}")
	for s := 0; s < 60; s++ {
		assert.True(t, seconds[fmt.Sprintf("%02d", s)], "second %02d", s)
	}

	years := seen("{now:%Y}")
	now := time.Now()
	assert.True(t, years[fmt.Sprintf("%d", now.Year())], "the current year")
	assert.True(t, years[fmt.Sprintf("%d", now.Year()+1)], "and the next one")

	// A format with no time placeholder renders once, not hundreds of times: the
	// collision check is quadratic in the sample count.
	assert.Len(t, config.ArchiveSampleNamesForTest("x-{hostname}-y"), 1)
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
