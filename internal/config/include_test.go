package config_test

import (
	"os"
	"path/filepath"
	"testing"

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

func TestGroupFilesAreTopLevelFragmentsWithIncludes(t *testing.T) {
	dir := writeFiles(t, map[string]string{
		"common.yaml":       "keep_daily: 7\n",
		"manager.yaml":      "manager:\n    period: \"1h\"\nborgmatic:\n    keep_weekly: 4\n",
		"groups/myapp.yaml": "<<: !include ../common.yaml\nkeep_monthly: 6\n",
		// Legacy wrapper form still accepted.
		"groups/legacy.yaml": "borgmatic:\n    keep_daily: 30\n",
	})

	_, overrides, err := config.LoadConfig(filepath.Join(dir, "manager.yaml"), filepath.Join(dir, "groups"))
	require.NoError(t, err)

	require.Contains(t, overrides, "myapp")
	assert.Equal(t, 7, overrides["myapp"]["keep_daily"], "group files support includes")
	assert.Equal(t, 6, overrides["myapp"]["keep_monthly"], "top-level fragment form works")

	require.Contains(t, overrides, "legacy")
	assert.Equal(t, 30, overrides["legacy"]["keep_daily"], "legacy borgmatic: wrapper still accepted")
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
