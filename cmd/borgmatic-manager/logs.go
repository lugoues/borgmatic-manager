package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

func logsCmd() *cobra.Command {
	var lines int
	var follow bool
	cmd := &cobra.Command{
		Use:   "logs <group>",
		Short: "Print a group's borgmatic output from the systemd journal",
		Long: `Reads the manager's journal (journalctl -u borgmatic-manager, or --user when
not root) and prints the log lines for one group. This is the full output;
` + "`inspect`" + ` shows a bounded tail without needing the journal.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cmd.SilenceUsage = true
			return runLogs(cmd.Context(), args[0], lines, follow)
		},
	}
	cmd.Flags().IntVarP(&lines, "lines", "n", 200, "number of matching lines to show")
	cmd.Flags().BoolVarP(&follow, "follow", "f", false, "stream new lines as they arrive")
	return cmd
}

// journalctlArgs builds the base journalctl invocation, choosing the system or
// user journal by whether we run as root (same split as the two systemd units).
func journalctlArgs() []string {
	args := []string{"-u", "borgmatic-manager", "-o", "cat", "--no-pager"}
	if os.Geteuid() != 0 {
		args = append([]string{"--user"}, args...)
	}
	return args
}

func runLogs(ctx context.Context, group string, lines int, follow bool) error {
	if _, err := exec.LookPath("journalctl"); err != nil {
		return fmt.Errorf("journalctl not found, `logs` reads the systemd journal; use `inspect %s` for the last run's tail instead", group)
	}
	if lines < 1 {
		lines = 1
	}

	args := journalctlArgs()
	if follow {
		// Seed a small backlog, then stream. journalctl filters by unit; we
		// filter by group as lines arrive.
		args = append(args, "-f", "-n", strconv.Itoa(lines))
	} else {
		// journalctl -n bounds entries pre-filter, so pull a generous window
		// and keep the last `lines` that match this group.
		window := lines * 20
		if window < 2000 {
			window = 2000
		}
		args = append(args, "-n", strconv.Itoa(window))
	}

	// #nosec G204 -- fixed journalctl flags and integer counts only; the group is filtered in-process, never passed to the command
	cmd := exec.CommandContext(ctx, "journalctl", args...)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("reading journalctl output: %w", err)
	}
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("starting journalctl: %w", err)
	}

	printed, ferr := filterJournal(stdout, group, lines, follow, os.Stdout)
	waitErr := cmd.Wait()
	if ferr != nil {
		return ferr
	}
	if waitErr != nil {
		return fmt.Errorf("journalctl exited with error: %w", waitErr)
	}
	if printed == 0 && !follow {
		fmt.Fprintf(os.Stderr, "no journal entries for group %q. The daemon logs to the journal only while running; check the unit name and that it has run.\n", group)
	}
	return nil
}

// filterJournal keeps journal records for the group and writes them formatted:
// streamed when following, else only the last `lines` matches. Returns the count.
func filterJournal(r io.Reader, group string, lines int, follow bool, w io.Writer) (int, error) {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	var buf []string
	printed := 0
	for scanner.Scan() {
		formatted, ok := formatJournalLine(scanner.Text(), group)
		if !ok {
			continue
		}
		if follow {
			_, _ = fmt.Fprintln(w, formatted)
			printed++
			continue
		}
		buf = append(buf, formatted)
		if len(buf) > lines {
			buf = buf[len(buf)-lines:]
		}
	}
	if !follow {
		for _, l := range buf {
			_, _ = fmt.Fprintln(w, l)
			printed++
		}
	}
	return printed, scanner.Err()
}

// formatJournalLine parses one slog JSON record into a "time level msg key=val"
// line; non-JSON lines and other groups return ok=false.
func formatJournalLine(line, group string) (string, bool) {
	var rec map[string]any
	if err := json.Unmarshal([]byte(line), &rec); err != nil {
		return "", false
	}
	if g, _ := rec["group"].(string); g != group {
		return "", false
	}

	ts := ""
	if t, ok := rec["time"].(string); ok {
		if parsed, err := time.Parse(time.RFC3339Nano, t); err == nil {
			ts = parsed.Local().Format("15:04:05")
		}
	}
	level, _ := rec["level"].(string)
	msg, _ := rec["msg"].(string)

	extras := make([]string, 0, len(rec))
	for k, v := range rec {
		switch k {
		case "time", "level", "msg", "group":
			continue
		}
		extras = append(extras, fmt.Sprintf("%s=%v", k, v))
	}
	sort.Strings(extras)

	out := styleDetail.Render(ts) + " " + colorLevel(level) + " " + msg
	if len(extras) > 0 {
		out += "  " + styleDetail.Render(strings.Join(extras, " "))
	}
	return out, true
}
