package main

import (
	"regexp"
	"strings"
)

// configLineRE matches a "key: value" YAML line (optionally a list item). Keys
// with no inline value (block openers) do not match and are never touched.
var configLineRE = regexp.MustCompile(`^(\s*(?:- )?)([A-Za-z0-9_]+)(:[ \t]+)(\S.*)$`)

// redactConfigSecrets masks sensitive values before a config is displayed by
// inspect; the on-disk config keeps the real values. {credential ...} and
// {env ...} references stay visible: they are not secrets.
func redactConfigSecrets(cfg string) string {
	lines := strings.Split(cfg, "\n")
	for i, line := range lines {
		m := configLineRE.FindStringSubmatch(line)
		if m == nil || !isSensitiveKey(m[2]) {
			continue
		}
		val := strings.Trim(strings.TrimSpace(m[4]), `"'`)
		if strings.HasPrefix(val, "{") {
			continue // a {credential ...} / {env ...} reference, not a literal secret
		}
		lines[i] = m[1] + m[2] + m[3] + "***redacted***"
	}
	return strings.Join(lines, "\n")
}

// isSensitiveKey reports whether a key's value must never be printed. Broad on
// purpose: over-redacting costs a look at the on-disk config, under-redacting leaks.
func isSensitiveKey(key string) bool {
	k := strings.ToLower(key)
	switch k {
	case "encryption_passphrase", "password", "passphrase", "secret", "token", "access_token", "api_key", "apikey":
		return true
	}
	for _, suffix := range []string{"_password", "_passphrase", "_token", "_secret", "_api_key"} {
		if strings.HasSuffix(k, suffix) {
			return true
		}
	}
	return false
}
