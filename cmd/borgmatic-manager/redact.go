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
	out := make([]string, 0, len(lines))

	for i := 0; i < len(lines); i++ {
		line := lines[i]
		m := configLineRE.FindStringSubmatch(line)
		if m == nil || !isSensitiveKey(m[2]) {
			out = append(out, line)
			continue
		}
		val := strings.Trim(strings.TrimSpace(m[4]), `"'`)
		if strings.HasPrefix(val, "{") {
			out = append(out, line) // a {credential ...} / {env ...} reference, not a literal secret
			continue
		}
		out = append(out, m[1]+m[2]+m[3]+redactedPlaceholder)

		// yaml.v3 emits multiline secrets as "password: |-" plus indented body lines;
		// masking only the key's line would print the secret, so drop the whole body.
		if isBlockScalar(val) {
			keyCol := len(m[1]) // the column the key starts at, past any "- "
			for i+1 < len(lines) && isBlockBody(lines[i+1], keyCol) {
				i++
			}
		}
	}
	return strings.Join(out, "\n")
}

const redactedPlaceholder = "***redacted***"

// isBlockScalar reports a YAML block scalar indicator, "|" or ">", with any
// chomping or explicit-indent modifier ("|-", "|+", ">2").
func isBlockScalar(value string) bool {
	return strings.HasPrefix(value, "|") || strings.HasPrefix(value, ">")
}

// isBlockBody reports whether line belongs to a block scalar opened at keyCol.
// Blank lines are legal inside a block, so they are consumed too.
func isBlockBody(line string, keyCol int) bool {
	if strings.TrimSpace(line) == "" {
		return true
	}
	return indentWidth(line) > keyCol
}

func indentWidth(s string) int {
	return len(s) - len(strings.TrimLeft(s, " \t"))
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
