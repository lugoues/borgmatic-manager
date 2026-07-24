package config_test

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lugoues/borgmatic-manager/internal/config"
)

// writeFiles lays out a config tree in a temp dir and returns its root.
func writeFiles(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, content := range files {
		path := filepath.Join(dir, name)
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
		require.NoError(t, os.WriteFile(path, []byte(content), 0o644))
	}
	return dir
}

func TestIncludeMergeKeyDeepLocalWins(t *testing.T) {
	dir := writeFiles(t, map[string]string{
		"common.yaml": `
keep_daily: 7
keep_weekly: 4
healthchecks:
    ping_url: https://hc-ping.com/common
    states: [finish]
`,
		"manager.yaml": `
manager:
    period: "1h"
borgmatic:
    <<: !include common.yaml
    keep_daily: 14
    healthchecks:
        states: [finish, fail]
`,
	})

	cfg, _, err := config.LoadConfig(filepath.Join(dir, "manager.yaml"), filepath.Join(dir, "groups"))
	require.NoError(t, err)

	assert.Equal(t, 14, cfg.Borgmatic["keep_daily"], "local keys win over the include")
	assert.Equal(t, 4, cfg.Borgmatic["keep_weekly"], "include-only keys survive")
	hc, ok := cfg.Borgmatic["healthchecks"].(map[string]interface{})
	require.True(t, ok)
	assert.Equal(t, "https://hc-ping.com/common", hc["ping_url"], "merge is deep, not shallow: nested include keys survive")
	assert.Equal(t, []interface{}{"finish", "fail"}, hc["states"], "local nested lists replace included ones")
}

func TestIncludeAsValue(t *testing.T) {
	dir := writeFiles(t, map[string]string{
		"common.yaml": "keep_daily: 7\nrepositories:\n    - path: /mnt/repo\n",
		"manager.yaml": `
manager:
    period: "1h"
borgmatic: !include common.yaml
`,
	})

	cfg, _, err := config.LoadConfig(filepath.Join(dir, "manager.yaml"), filepath.Join(dir, "groups"))
	require.NoError(t, err)
	assert.Equal(t, 7, cfg.Borgmatic["keep_daily"])
}

func TestIncludeNestedAndRelativeToIncludingFile(t *testing.T) {
	dir := writeFiles(t, map[string]string{
		// base.yaml lives in shared/ and includes a sibling relative to ITSELF.
		"shared/base.yaml":      "<<: !include retention.yaml\nchecks:\n    - name: repository\n",
		"shared/retention.yaml": "keep_daily: 7\n",
		"manager.yaml": `
manager:
    period: "1h"
borgmatic:
    <<: !include shared/base.yaml
`,
	})

	cfg, _, err := config.LoadConfig(filepath.Join(dir, "manager.yaml"), filepath.Join(dir, "groups"))
	require.NoError(t, err)
	assert.Equal(t, 7, cfg.Borgmatic["keep_daily"], "relative include paths resolve against the including file")
	assert.NotNil(t, cfg.Borgmatic["checks"])
}

func TestIncludeCycleErrors(t *testing.T) {
	dir := writeFiles(t, map[string]string{
		"a.yaml": "<<: !include b.yaml\n",
		"b.yaml": "<<: !include a.yaml\n",
		"manager.yaml": `
manager:
    period: "1h"
borgmatic:
    <<: !include a.yaml
`,
	})

	_, _, err := config.LoadConfig(filepath.Join(dir, "manager.yaml"), filepath.Join(dir, "groups"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "include cycle")
}

func TestIncludeMissingFileErrors(t *testing.T) {
	dir := writeFiles(t, map[string]string{
		"manager.yaml": `
manager:
    period: "1h"
borgmatic:
    <<: !include nope.yaml
`,
	})

	_, _, err := config.LoadConfig(filepath.Join(dir, "manager.yaml"), filepath.Join(dir, "groups"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "nope.yaml")
}

func TestRetainOmitTagsRejectedClearly(t *testing.T) {
	dir := writeFiles(t, map[string]string{
		"common.yaml": "keep_daily: 7\n",
		"manager.yaml": `
manager:
    period: "1h"
borgmatic:
    <<: !include common.yaml
    checks: !retain
        - name: repository
`,
	})

	_, _, err := config.LoadConfig(filepath.Join(dir, "manager.yaml"), filepath.Join(dir, "groups"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "!retain")
	assert.Contains(t, err.Error(), "not supported")
}

func TestGroupFilesAreManagerShapedOverlays(t *testing.T) {
	dir := writeFiles(t, map[string]string{
		"common.yaml":       "keep_daily: 7\n",
		"manager.yaml":      "manager:\n    period: \"1h\"\nborgmatic:\n    keep_weekly: 4\n",
		"groups/myapp.yaml": "manager:\n    period: \"30m\"\nborgmatic:\n    <<: !include ../common.yaml\n    keep_monthly: 6\n",
		"groups/plain.yaml": "borgmatic:\n    keep_daily: 30\n",
	})

	cfg, overrides, err := config.LoadConfig(filepath.Join(dir, "manager.yaml"), filepath.Join(dir, "groups"))
	require.NoError(t, err)

	require.Contains(t, overrides, "myapp")
	assert.Equal(t, 7, overrides["myapp"].Borgmatic["keep_daily"], "group borgmatic sections support includes")
	assert.Equal(t, 6, overrides["myapp"].Borgmatic["keep_monthly"], "borgmatic section merges include with local keys")
	assert.Equal(t, 30*time.Minute, overrides["myapp"].Period, "group manager.period parses")
	assert.Equal(t, 30*time.Minute, cfg.GroupPeriods["myapp"], "group period lands in cfg.GroupPeriods")

	require.Contains(t, overrides, "plain")
	assert.Equal(t, 30, overrides["plain"].Borgmatic["keep_daily"], "borgmatic-only overlay works")
	assert.Zero(t, overrides["plain"].Period)
}

func TestGroupFileBareKeysRejected(t *testing.T) {
	dir := writeFiles(t, map[string]string{
		"manager.yaml":      "manager:\n    period: \"1h\"\nborgmatic: {}\n",
		"groups/myapp.yaml": "keep_daily: 14\n",
	})

	_, _, err := config.LoadConfig(filepath.Join(dir, "manager.yaml"), filepath.Join(dir, "groups"))
	require.ErrorContains(t, err, "unknown top-level key")
	require.ErrorContains(t, err, "nest borgmatic options")
}

func TestGroupFileManagerSectionValidation(t *testing.T) {
	cases := []struct {
		name, body, wantErr string
	}{
		{"unknown manager key", "manager:\n    run_timeout: \"5m\"\n", "unknown manager option"},
		{"bad period", "manager:\n    period: \"often\"\n", "invalid manager.period"},
		{"non-positive period", "manager:\n    period: \"0s\"\n", "must be positive"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := writeFiles(t, map[string]string{
				"manager.yaml":      "manager:\n    period: \"1h\"\nborgmatic: {}\n",
				"groups/myapp.yaml": tc.body,
			})
			_, _, err := config.LoadConfig(filepath.Join(dir, "manager.yaml"), filepath.Join(dir, "groups"))
			require.ErrorContains(t, err, tc.wantErr)
		})
	}
}

func TestNativeAnchorsAndMergeStillWork(t *testing.T) {
	dir := writeFiles(t, map[string]string{
		"manager.yaml": `
manager:
    period: "1h"
defaults: &defaults
    keep_daily: 7
borgmatic:
    <<: *defaults
    keep_weekly: 4
`,
	})

	cfg, _, err := config.LoadConfig(filepath.Join(dir, "manager.yaml"), filepath.Join(dir, "groups"))
	require.NoError(t, err)
	assert.Equal(t, 7, cfg.Borgmatic["keep_daily"], "plain YAML anchor merges keep working")
	assert.Equal(t, 4, cfg.Borgmatic["keep_weekly"])
}

func TestConfDDropInsMergeInLexicalOrder(t *testing.T) {
	dir := writeFiles(t, map[string]string{
		"manager.yaml": "manager:\n    period: \"1h\"\nborgmatic:\n    keep_daily: 7\n    keep_weekly: 4\n",
		// Lexical order: 10- applies before 50-, so 50- wins on conflicts.
		"conf.d/10-retention.yaml": "borgmatic:\n    keep_daily: 14\n    keep_hourly: 24\n",
		"conf.d/50-period.yaml":    "manager:\n    period: \"30m\"\nborgmatic:\n    keep_daily: 30\n",
		"conf.d/ignored.txt":       "not yaml\n",
	})

	cfg, _, err := config.LoadConfig(filepath.Join(dir, "manager.yaml"), filepath.Join(dir, "groups"))
	require.NoError(t, err)

	assert.Equal(t, "30m", cfg.Manager.Period, "drop-ins can override the manager section")
	assert.Equal(t, 30, cfg.Borgmatic["keep_daily"], "later drop-ins win over earlier ones")
	assert.Equal(t, 24, cfg.Borgmatic["keep_hourly"], "non-conflicting drop-in keys accumulate")
	assert.Equal(t, 4, cfg.Borgmatic["keep_weekly"], "manager.yaml keys survive when untouched")
}

func TestConfDSupportsIncludes(t *testing.T) {
	dir := writeFiles(t, map[string]string{
		"manager.yaml":         "manager:\n    period: \"1h\"\nborgmatic:\n    keep_daily: 7\n",
		"shared.yaml":          "keep_monthly: 12\n",
		"conf.d/10-local.yaml": "borgmatic:\n    <<: !include ../shared.yaml\n",
	})

	cfg, _, err := config.LoadConfig(filepath.Join(dir, "manager.yaml"), filepath.Join(dir, "groups"))
	require.NoError(t, err)
	assert.Equal(t, 12, cfg.Borgmatic["keep_monthly"], "drop-ins support !include")
}

func TestConfDMissingIsFine(t *testing.T) {
	dir := writeFiles(t, map[string]string{
		"manager.yaml": "manager:\n    period: \"1h\"\nborgmatic:\n    keep_daily: 7\n",
	})

	cfg, _, err := config.LoadConfig(filepath.Join(dir, "manager.yaml"), filepath.Join(dir, "groups"))
	require.NoError(t, err)
	assert.Equal(t, "1h", cfg.Manager.Period)
}

func TestYmlExtensionAccepted(t *testing.T) {
	dir := writeFiles(t, map[string]string{
		"manager.yaml":    "manager:\n    period: \"1h\"\nborgmatic:\n    keep_daily: 7\n",
		"conf.d/10-x.yml": "borgmatic:\n    keep_daily: 14\n",
		"groups/app.yml":  "borgmatic:\n    keep_monthly: 6\n",
	})

	cfg, overrides, err := config.LoadConfig(filepath.Join(dir, "manager.yaml"), filepath.Join(dir, "groups"))
	require.NoError(t, err)
	assert.Equal(t, 14, cfg.Borgmatic["keep_daily"], ".yml drop-ins must load")
	require.Contains(t, overrides, "app", ".yml group files must load")
	assert.Equal(t, 6, overrides["app"].Borgmatic["keep_monthly"])
}
