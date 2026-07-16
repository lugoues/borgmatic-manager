package main

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestFilterJournalByGroup(t *testing.T) {
	input := strings.Join([]string{
		`{"time":"2026-07-16T03:00:01Z","level":"INFO","msg":"starting borgmatic","group":"home-assistant"}`,
		`{"time":"2026-07-16T03:00:02Z","level":"INFO","msg":"starting borgmatic","group":"paperless"}`,
		`{"time":"2026-07-16T03:00:03Z","level":"ERROR","msg":"repository does not exist","group":"home-assistant"}`,
		`not-json-should-be-skipped`,
		`{"time":"2026-07-16T03:00:04Z","level":"INFO","msg":"done","group":"paperless"}`,
	}, "\n")

	var out strings.Builder
	printed, err := filterJournal(strings.NewReader(input), "home-assistant", 100, false, &out)
	require.NoError(t, err)

	assert.Equal(t, 2, printed, "only the two home-assistant records match")
	got := out.String()
	assert.Contains(t, got, "repository does not exist")
	assert.NotContains(t, got, "paperless", "other groups are filtered out")
	assert.NotContains(t, got, "not-json", "non-JSON lines are skipped")
}

func TestFilterJournalKeepsLastN(t *testing.T) {
	var lines []string
	for i := range 10 {
		lines = append(lines, `{"level":"INFO","msg":"line`+string(rune('0'+i))+`","group":"g"}`)
	}

	var out strings.Builder
	printed, err := filterJournal(strings.NewReader(strings.Join(lines, "\n")), "g", 3, false, &out)
	require.NoError(t, err)

	assert.Equal(t, 3, printed, "only the last N matching lines are kept")
	got := out.String()
	assert.Contains(t, got, "line9", "the newest line is kept")
	assert.NotContains(t, got, "line0", "the oldest is dropped")
}

func TestFormatJournalLineIncludesLevelAndExtras(t *testing.T) {
	line := `{"time":"2026-07-16T03:00:03Z","level":"ERROR","msg":"boom","group":"g","source":"borg","exit_code":2}`

	formatted, ok := formatJournalLine(line, "g")
	require.True(t, ok)
	assert.Contains(t, formatted, "boom")
	assert.Contains(t, formatted, "ERROR")
	assert.Contains(t, formatted, "source=borg", "structured attributes are appended")
	assert.Contains(t, formatted, "exit_code=2")

	_, ok = formatJournalLine(line, "other")
	assert.False(t, ok, "a different group does not match")
}
