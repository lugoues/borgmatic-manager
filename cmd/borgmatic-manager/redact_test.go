package main

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestRedactConfigSecrets(t *testing.T) {
	in := strings.Join([]string{
		"source_directories:",
		"  - /mnt/demo",
		"encryption_passphrase: hunter2",
		"keep_daily: 14",
		"postgresql_databases:",
		"  - name: appdb",
		"    password: s3cr3t",
		`    pg_dump_command: docker run --env PGPASSWORD img pg_dump`,
	}, "\n")

	out := redactConfigSecrets(in)

	assert.NotContains(t, out, "hunter2", "the borg passphrase must be masked")
	assert.NotContains(t, out, "s3cr3t", "the database password must be masked")
	assert.Contains(t, out, "encryption_passphrase: ***redacted***")
	assert.Contains(t, out, "password: ***redacted***")

	// Non-secrets and command strings (which carry only --env NAME, not the
	// value) stay intact.
	assert.Contains(t, out, "keep_daily: 14")
	assert.Contains(t, out, "--env PGPASSWORD", "the command name is not the secret and stays visible")
	assert.Contains(t, out, "source_directories:")
}

func TestRedactLeavesCredentialReferences(t *testing.T) {
	in := `encryption_passphrase: "{credential systemd BORG_PASSPHRASE}"`

	out := redactConfigSecrets(in)

	assert.Equal(t, in, out, "a credential reference is not a secret and must stay visible")
}

func TestIsSensitiveKey(t *testing.T) {
	for _, k := range []string{"password", "encryption_passphrase", "PASSWORD", "access_token", "db_password", "some_secret"} {
		assert.True(t, isSensitiveKey(k), "%q should be sensitive", k)
	}
	for _, k := range []string{"keep_daily", "source_directories", "pg_dump_command", "hostname", "label"} {
		assert.False(t, isSensitiveKey(k), "%q should not be sensitive", k)
	}
}
