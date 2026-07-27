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

// A rejected endpoint is as likely to carry a credential as an accepted one, and
// this error is logged as "startup failed" to the same journal the successful
// startup line goes to. A scheme typo in an authenticated URL is exactly how a
// password gets there.
func TestValidationErrorsCarryNoCredential(t *testing.T) {
	for _, tc := range []struct{ name, endpoint string }{
		{name: "a bad scheme", endpoint: "ftp://user:password@collector.example/v1"},
		{name: "a bad scheme with a query token", endpoint: "ftp://collector.example/v1?token=s3cret"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := config.MetricsSettings{Enabled: true, Endpoint: tc.endpoint}.Validate()
			require.Error(t, err)
			assert.NotContains(t, err.Error(), "password")
			assert.NotContains(t, err.Error(), "s3cret")
			assert.Contains(t, err.Error(), "collector.example",
				"the operator still needs to recognize which endpoint was rejected")
		})
	}

	t.Run("an unparsable endpoint is not echoed", func(t *testing.T) {
		err := config.MetricsSettings{Enabled: true, Endpoint: "ht tp://u:pw@ho st/x"}.Validate()
		require.Error(t, err)
		assert.NotContains(t, err.Error(), "pw")
	})

	t.Run("a valid endpoint is still accepted", func(t *testing.T) {
		require.NoError(t, config.MetricsSettings{Enabled: true,
			Endpoint: "https://user:password@collector.example/v1/metrics"}.Validate())
	})
}

// The document root is decoded leniently so a config may carry anchor-holder
// keys, and that leniency is what makes the mistake silent: "metrics:" written
// beside "manager:" instead of inside it is dropped without a word, and the
// service starts with metrics off while the operator has every reason to think
// they enabled them.
func TestManagerOptionsAtTheTopLevelAreRejected(t *testing.T) {
	dir := t.TempDir()
	write := func(t *testing.T, body string) error {
		t.Helper()
		require.NoError(t, os.WriteFile(filepath.Join(dir, "manager.yaml"), []byte(body), 0o600))
		_, _, err := config.LoadConfig(filepath.Join(dir, "manager.yaml"), filepath.Join(dir, "groups"))
		return err
	}

	t.Run("a misplaced metrics block fails fast", func(t *testing.T) {
		err := write(t, "manager:\n  period: 1h\nmetrics:\n  enabled: true\n")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "metrics")
		assert.Contains(t, err.Error(), "manager:")
	})

	t.Run("other manager options too", func(t *testing.T) {
		require.Error(t, write(t, "manager:\n  period: 1h\nrun_timeout: 2h\n"))
	})

	t.Run("the correct placement is accepted", func(t *testing.T) {
		require.NoError(t, write(t, "manager:\n  period: 1h\n  metrics:\n    enabled: false\n"))
	})

	t.Run("an unrecognized top-level key is still allowed", func(t *testing.T) {
		require.NoError(t, write(t, "x-anchors:\n  a: &a 1\nmanager:\n  period: 1h\n"))
	})
}

// A scheme typo produces exactly this shape: malformed before the authority, so
// url.Parse finds no host and every credential in it stays in the string. That
// value then reaches the journal through the validation error, which is the path
// the earlier redaction was supposed to close.
func TestMalformedEndpointsWithoutDoubleSlashesAreNotEchoed(t *testing.T) {
	for _, tc := range []struct{ name, endpoint, secret string }{
		{name: "userinfo", endpoint: "https:/user:password@collector.example", secret: "password"},
		{name: "query token", endpoint: "https:/collector.example/v1?token=s3cret", secret: "s3cret"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert.NotContains(t, config.RedactEndpoint(tc.endpoint), tc.secret)
			err := config.MetricsSettings{Enabled: true, Endpoint: tc.endpoint}.Validate()
			require.Error(t, err)
			assert.NotContains(t, err.Error(), tc.secret,
				"the value reaches the journal through this error")
		})
	}

	t.Run("a plain host and port stays readable", func(t *testing.T) {
		assert.Equal(t, "collector:4317", config.RedactEndpoint("collector:4317"))
	})
	t.Run("an empty endpoint is unchanged", func(t *testing.T) {
		assert.Empty(t, config.RedactEndpoint(""))
	})
}
