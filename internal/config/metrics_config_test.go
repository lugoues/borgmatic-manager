package config_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lugoues/borgmatic-manager/internal/config"
)

func TestMetricsSettingsValidate(t *testing.T) {
	cases := []struct {
		name    string
		m       config.MetricsSettings
		wantErr string // substring; empty means no error
	}{
		{"disabled skips validation", config.MetricsSettings{Enabled: false, Protocol: "nonsense"}, ""},
		{"default protocol ok", config.MetricsSettings{Enabled: true}, ""},
		{"http ok", config.MetricsSettings{Enabled: true, Protocol: "http", Endpoint: "http://localhost:4318"}, ""},
		{"grpc ok", config.MetricsSettings{Enabled: true, Protocol: "grpc", Endpoint: "https://otel.example/v1/metrics"}, ""},
		{"case-insensitive protocol", config.MetricsSettings{Enabled: true, Protocol: "GRPC"}, ""},
		{"swapped fields: url in protocol", config.MetricsSettings{Enabled: true, Protocol: "https://otel.example/v1/metrics", Endpoint: "http"}, "metrics.protocol"},
		{"bad protocol", config.MetricsSettings{Enabled: true, Protocol: "carrier-pigeon"}, "metrics.protocol"},
		{"endpoint without scheme", config.MetricsSettings{Enabled: true, Protocol: "http", Endpoint: "otel.example:4318"}, "not a valid URL"},
		{"endpoint bad scheme", config.MetricsSettings{Enabled: true, Protocol: "http", Endpoint: "ftp://otel.example"}, "must use http or https"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.m.Validate()
			if tc.wantErr == "" {
				assert.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.wantErr)
		})
	}
}

func TestLoadConfigRejectsUnknownManagerKeys(t *testing.T) {
	write := func(t *testing.T, body string) error {
		t.Helper()
		dir := t.TempDir()
		require.NoError(t, os.WriteFile(filepath.Join(dir, "manager.yaml"), []byte(body), 0o600))
		_, _, err := config.LoadConfig(filepath.Join(dir, "manager.yaml"), filepath.Join(dir, "groups"))
		return err
	}

	t.Run("misspelled manager key", func(t *testing.T) {
		err := write(t, "manager:\n  period: \"1h\"\n  mtrics:\n    enabled: true\n")
		require.Error(t, err, "a dropped setting must fail, not silently disable")
		assert.Contains(t, err.Error(), "misspelled or misplaced")
	})

	t.Run("misspelled key nested under metrics", func(t *testing.T) {
		err := write(t, "manager:\n  period: \"1h\"\n  metrics:\n    xenabled: true\n")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "misspelled or misplaced")
	})

	t.Run("valid config with metrics loads", func(t *testing.T) {
		err := write(t, "manager:\n  period: \"1h\"\n  metrics:\n    enabled: true\n    endpoint: \"https://otel.example/v1/metrics\"\n    protocol: \"http\"\n")
		assert.NoError(t, err)
	})

	t.Run("arbitrary borgmatic keys still allowed", func(t *testing.T) {
		err := write(t, "manager:\n  period: \"1h\"\nborgmatic:\n  some_future_borgmatic_option: 42\n")
		assert.NoError(t, err, "the borgmatic section is free-form; borgmatic validates it")
	})
}

func TestLoadConfigRejectsBadMetricsProtocol(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "manager.yaml"), []byte(
		"manager:\n  period: \"1h\"\n  metrics:\n    enabled: true\n    endpoint: \"http\"\n    protocol: \"https://otel.example/v1/metrics\"\n"), 0o600))

	_, _, err := config.LoadConfig(filepath.Join(dir, "manager.yaml"), filepath.Join(dir, "groups"))
	require.Error(t, err, "a swapped endpoint/protocol must fail startup, not silently disable metrics")
	assert.Contains(t, err.Error(), "metrics.protocol")
}
