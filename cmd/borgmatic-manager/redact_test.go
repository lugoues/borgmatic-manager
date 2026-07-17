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

// A secret containing a newline is marshaled by yaml.v3 as a block scalar
// ("password: |-" plus indented lines). Masking only the key's line would print
// the secret body verbatim under a banner claiming secrets were redacted.
func TestRedactMultilineBlockScalarSecrets(t *testing.T) {
	in := strings.Join([]string{
		"encryption_passphrase: |-",
		"    PASSPHRASE_LINE_ONE",
		"    PASSPHRASE_LINE_TWO",
		"keep_daily: 14",
		"postgresql_databases:",
		"    - name: appdb",
		"      password: |-",
		"        DBSECRET_LINE_ONE",
		"        DBSECRET_LINE_TWO",
		"      hostname: 127.0.0.1",
	}, "\n")

	out := redactConfigSecrets(in)

	for _, secret := range []string{
		"PASSPHRASE_LINE_ONE", "PASSPHRASE_LINE_TWO",
		"DBSECRET_LINE_ONE", "DBSECRET_LINE_TWO",
	} {
		assert.NotContains(t, out, secret, "the block scalar body must not survive redaction")
	}
	assert.Contains(t, out, "encryption_passphrase: ***redacted***")
	assert.Contains(t, out, "password: ***redacted***")

	// Structure around the blocks stays intact: consuming the body must stop at
	// the next key, not swallow the rest of the document.
	assert.Contains(t, out, "keep_daily: 14")
	assert.Contains(t, out, "name: appdb")
	assert.Contains(t, out, "hostname: 127.0.0.1")
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
