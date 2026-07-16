package main

import (
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/lipgloss/table"
	"golang.org/x/term"

	"github.com/lugoues/borgmatic-manager/internal/models"
	"github.com/lugoues/borgmatic-manager/internal/scheduler"
	"github.com/lugoues/borgmatic-manager/internal/state"
)

// hPad keeps output off the terminal edges.
const hPad = 2

var edgePad = strings.Repeat(" ", hPad)

// lipgloss degrades to plain text on non-TTY output and honors NO_COLOR, so the
// literal tokens stay stable and greppable.
var (
	styleGroup  = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("6"))
	styleKind   = lipgloss.NewStyle().Foreground(lipgloss.Color("8"))
	styleName   = lipgloss.NewStyle().Foreground(lipgloss.Color("2"))
	styleDetail = lipgloss.NewStyle().Foreground(lipgloss.Color("8"))
	styleBad    = lipgloss.NewStyle().Foreground(lipgloss.Color("1"))
	styleWarn   = lipgloss.NewStyle().Foreground(lipgloss.Color("3"))
	styleTitle  = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("15"))
)

// colorLevel renders a slog level token, colored by severity and padded to align.
func colorLevel(level string) string {
	padded := fmt.Sprintf("%-5s", level)
	switch level {
	case "ERROR":
		return styleBad.Render(padded)
	case "WARN":
		return styleWarn.Render(padded)
	default:
		return styleDetail.Render(padded)
	}
}

// printGroups renders the discovered backup groups with their last-backup ages.
func printGroups(bs *models.BackupState, store *state.ScheduleStore) {
	// Trailing blank line keeps the block off the shell prompt.
	defer fmt.Println()
	now := time.Now()
	fmt.Println()
	fmt.Println(spreadLine(styleTitle.Render("Discover"), styleDetail.Render(summaryCounts(bs))))
	fmt.Println()

	names := make([]string, 0, len(bs.Groups))
	for name := range bs.Groups {
		names = append(names, name)
	}
	sort.Strings(names)

	// One name-column width across all groups. Pad before styling: ANSI codes
	// would throw off printf width math.
	nameWidth := 0
	for _, group := range bs.Groups {
		for _, v := range group.Volumes {
			nameWidth = max(nameWidth, len(v.Name))
		}
		for _, db := range group.Databases {
			nameWidth = max(nameWidth, len(db.Type+"/"+db.Name))
		}
	}

	for i, name := range names {
		if i > 0 {
			fmt.Println()
		}
		group := bs.Groups[name]

		lastBackup := "no backups yet"
		if rec, ok := store.Record(name); ok && !rec.LastSuccess.IsZero() {
			lastBackup = "last backup " + humanTime(rec.LastSuccess, now)
		}
		fmt.Println(spreadLine(styleGroup.Render(name), styleDetail.Render(lastBackup)))

		for _, v := range group.Volumes {
			fmt.Printf(edgePad+"  %s  %s  %s\n",
				styleKind.Render(fmt.Sprintf("%-8s", "volume")),
				styleName.Render(fmt.Sprintf("%-*s", nameWidth, v.Name)),
				styleDetail.Render(v.HostPath),
			)
		}
		for _, db := range group.Databases {
			detail := "container=" + db.Container
			switch {
			case db.Type == "sqlite":
				detail = db.Path
			case db.Hostname != "":
				detail = fmt.Sprintf("hostname=%s port=%d", db.Hostname, db.Port)
			case db.Mode == "exec":
				detail = "container=" + db.Container + " (exec)"
			}
			fmt.Printf(edgePad+"  %s  %s  %s\n",
				styleKind.Render(fmt.Sprintf("%-8s", "database")),
				styleName.Render(fmt.Sprintf("%-*s", nameWidth, db.Type+"/"+db.Name)),
				styleDetail.Render(detail),
			)
		}
	}
}

// summaryCounts renders "N groups · N volumes · N databases".
func summaryCounts(bs *models.BackupState) string {
	groups, volumes, databases := len(bs.Groups), 0, 0
	for _, g := range bs.Groups {
		volumes += len(g.Volumes)
		databases += len(g.Databases)
	}

	return fmt.Sprintf("%s · %s · %s",
		plural(groups, "group"), plural(volumes, "volume"), plural(databases, "database"))
}

// spreadLine joins a left and right fragment on one line, pushing the right one
// to the terminal's right edge (two spaces apart when the width is unknown).
func spreadLine(left, right string) string {
	gap := 2
	if width, _, err := term.GetSize(int(os.Stdout.Fd())); err == nil && width > 0 {
		if g := width - 2*hPad - lipgloss.Width(left) - lipgloss.Width(right); g > gap {
			gap = g
		}
	}
	return edgePad + left + strings.Repeat(" ", gap) + right
}

// humanTime renders a short age for the recent past, an absolute date beyond a day.
func humanTime(t, now time.Time) string {
	if d := now.Sub(t); d >= 0 && d < 24*time.Hour {
		if d < time.Minute {
			return "just now"
		}
		return shortDuration(d) + " ago"
	}
	return t.Local().Format("Jan 2 2006 @ 15:04")
}

func plural(n int, noun string) string {
	if n == 1 {
		return fmt.Sprintf("1 %s", noun)
	}
	return fmt.Sprintf("%d %ss", n, noun)
}

// printStatus renders per-group schedule state. Refused groups are marked as
// such instead of showing "due now" forever; in-flight runs show "running" with
// elapsed time, flagged when past runTimeout.
func printStatus(bs *models.BackupState, store *state.ScheduleStore, period, runTimeout time.Duration, refused map[string]string) {
	// Trailing blank line keeps the table off the shell prompt.
	defer fmt.Println()
	now := time.Now()
	fmt.Println()

	// A group in the pending set is mid-run. Keep the earliest start per group so
	// a stale-plus-fresh pair surfaces the longer-running one.
	running := map[string]time.Time{}
	for _, p := range store.PendingSnapshot() {
		if started, ok := running[p.Group]; !ok || p.Started.Before(started) {
			running[p.Group] = p.Started
		}
	}

	type row struct {
		name, last, result, files, size, next string
		reason                                string // captured cause, failed runs only
		failed                                bool
	}
	rows := make([]row, 0, len(bs.Groups))
	var soonest time.Duration = -1
	runningCount := 0

	names := make([]string, 0, len(bs.Groups))
	for name := range bs.Groups {
		names = append(names, name)
	}
	sort.Strings(names)

	for _, name := range names {
		group := bs.Groups[name]
		if len(group.Volumes) == 0 && len(group.Databases) == 0 {
			continue
		}

		r := row{name: name, last: "never", result: "-", files: "-", size: "-", next: "due now"}
		var wait time.Duration // 0 = due now

		rec, ok := store.Record(name)
		if ok && rec.LastRun != nil {
			o := rec.LastRun
			r.last = shortDuration(now.Sub(o.Finished)) + " ago"
			r.result = o.Result
			if o.Result == state.ResultOK {
				detail := shortDuration(time.Duration(o.DurationSeconds) * time.Second)
				if o.Warnings > 0 {
					detail += fmt.Sprintf(", %d warnings", o.Warnings)
				}
				r.result = fmt.Sprintf("ok (%s)", detail)
			} else if o.ExitCode != 0 {
				r.result = fmt.Sprintf("%s (exit %d)", o.Result, o.ExitCode)
			}
			if o.Result == state.ResultFailed {
				r.failed = true
				r.reason = o.LastError
			}
			if o.Files > 0 || o.OriginalBytes > 0 {
				r.files = fmt.Sprintf("%d", o.Files)
				r.size = humanBytes(o.OriginalBytes)
				if o.DeduplicatedBytes > 0 {
					r.size += fmt.Sprintf(" (+%s)", humanBytes(o.DeduplicatedBytes))
				}
			}
		}
		// In-flight runs override next-run and stay out of soonest (happening, not scheduled).
		if started, isRunning := running[name]; isRunning {
			elapsed := now.Sub(started)
			if runTimeout > 0 && elapsed > runTimeout {
				r.next = "running? (" + shortDuration(elapsed) + ")" // past run_timeout, possibly stale
			} else {
				r.next = "running (" + shortDuration(elapsed) + ")"
			}
			runningCount++
			rows = append(rows, r)
			continue
		}
		switch {
		case refused[name] != "":
			r.next = "refused: " + refused[name]
			rows = append(rows, r)
			continue // a refused group never runs; keep it out of soonest
		case !ok:
			// r.next stays "due now"
		case rec.Fingerprint != scheduler.GroupFingerprint(group):
			r.next = "due now (membership changed)"
		case rec.LastSuccess.Add(period).After(now):
			wait = time.Until(rec.LastSuccess.Add(period))
			r.next = "in " + shortDuration(wait)
		default:
			r.next = "due now"
		}
		if soonest < 0 || wait < soonest {
			soonest = wait
		}
		rows = append(rows, r)
	}

	header := "no groups"
	switch {
	case runningCount > 0:
		header = plural(runningCount, "group") + " running"
	case soonest == 0:
		header = "next run: due now"
	case soonest > 0:
		header = "next run in " + shortDuration(soonest)
	}
	fmt.Println(spreadLine(styleTitle.Render("Status"), styleDetail.Render(header)))
	fmt.Println()

	tbl := table.New().
		BorderTop(false).BorderBottom(false).BorderLeft(false).BorderRight(false).
		BorderColumn(false).BorderHeader(false).BorderRow(false).
		StyleFunc(func(row, col int) lipgloss.Style {
			base := lipgloss.NewStyle().PaddingRight(2)
			switch {
			case row == table.HeaderRow:
				return base.Inherit(styleKind)
			case col == 0:
				return base.Inherit(styleName)
			case col == 2:
				r := rows[row]
				if strings.HasPrefix(r.result, "ok") {
					return base.Inherit(styleName)
				}
				if r.result != "-" {
					return base.Inherit(styleBad)
				}
			case col == 5:
				r := rows[row]
				switch {
				case strings.HasPrefix(r.next, "refused"):
					return base.Inherit(styleBad)
				case strings.HasPrefix(r.next, "running?"):
					return base.Inherit(styleWarn) // past run_timeout, possibly stale
				case strings.HasPrefix(r.next, "running"):
					return base.Inherit(styleGroup) // active, bold cyan
				}
				return base.Inherit(styleDetail)
			}
			return base
		}).
		Headers("group", "last run", "result", "files", "size", "next run")
	for _, r := range rows {
		tbl.Row(r.name, r.last, r.result, r.files, r.size, r.next)
	}
	fmt.Println(lipgloss.NewStyle().MarginLeft(hPad).Render(tbl.Render()))

	// Failed groups get a pointer to inspect; status stays a dashboard.
	var failed []string
	for _, r := range rows {
		if r.failed {
			failed = append(failed, r.name)
		}
	}
	if len(failed) > 0 {
		fmt.Println()
		fmt.Println(edgePad + styleBad.Render(plural(len(failed), "group")+" failed") +
			styleDetail.Render(": "+strings.Join(failed, ", ")))
		fmt.Println(edgePad + styleDetail.Render("Run ") +
			styleName.Render("borgmatic-manager inspect <group>") +
			styleDetail.Render(" to see why, or ") +
			styleName.Render("logs <group>") +
			styleDetail.Render(" for the full output."))
	}
}

func section(title string) {
	fmt.Println()
	fmt.Println(edgePad + styleGroup.Render(title))
}

// kv prints one "label   value" line under a section, label column aligned.
func kv(label, value string) {
	fmt.Printf(edgePad+"  %s  %s\n", styleDetail.Render(fmt.Sprintf("%-13s", label)), value)
}

var blockRunes = []rune("▁▂▃▄▅▆▇█")

// sparkline renders values as a single line of block characters, scaled
// between the series min and max. Fewer than two values renders empty.
func sparkline(values []int64) string {
	if len(values) < 2 {
		return ""
	}
	lo, hi := values[0], values[0]
	for _, v := range values {
		lo = min(lo, v)
		hi = max(hi, v)
	}
	var b strings.Builder
	for _, v := range values {
		idx := 0
		if span := hi - lo; span > 0 {
			idx = int((v - lo) * int64(len(blockRunes)-1) / span)
		}
		b.WriteRune(blockRunes[idx])
	}
	return b.String()
}

// printInspect renders a detailed view of one group. configYAML is the compiled
// config, or configNote explains why it is unavailable.
func printInspect(name string, group *models.VolumeGroup, rec state.GroupRecord, haveRec bool, configYAML, configNote string, period time.Duration) {
	defer fmt.Println()
	now := time.Now()
	fmt.Println()

	memberCount := fmt.Sprintf("%s · %s",
		plural(len(group.Volumes), "volume"), plural(len(group.Databases), "database"))
	fmt.Println(spreadLine(styleTitle.Render("Inspect "+name), styleDetail.Render(memberCount)))

	section("Members")
	nameWidth := 0
	for _, v := range group.Volumes {
		nameWidth = max(nameWidth, len(v.Name))
	}
	for _, db := range group.Databases {
		nameWidth = max(nameWidth, len(db.Type+"/"+db.Name))
	}
	for _, v := range group.Volumes {
		fmt.Printf(edgePad+"  %s  %s  %s\n",
			styleKind.Render(fmt.Sprintf("%-8s", "volume")),
			styleName.Render(fmt.Sprintf("%-*s", nameWidth, v.Name)),
			styleDetail.Render(v.HostPath))
	}
	for _, db := range group.Databases {
		fmt.Printf(edgePad+"  %s  %s  %s\n",
			styleKind.Render(fmt.Sprintf("%-8s", "database")),
			styleName.Render(fmt.Sprintf("%-*s", nameWidth, db.Type+"/"+db.Name)),
			styleDetail.Render("container="+db.Container))
	}

	section("Schedule")
	if haveRec && !rec.LastSuccess.IsZero() {
		kv("last backup", humanTime(rec.LastSuccess, now))
	} else {
		kv("last backup", styleDetail.Render("never"))
	}
	switch {
	case !haveRec:
		kv("next run", "due now")
	case rec.Fingerprint != scheduler.GroupFingerprint(group):
		kv("next run", "due now "+styleDetail.Render("(membership changed)"))
	case rec.LastSuccess.Add(period).After(now):
		kv("next run", "in "+shortDuration(time.Until(rec.LastSuccess.Add(period))))
	default:
		kv("next run", "due now")
	}

	if rec.LastRun != nil {
		o := rec.LastRun
		section("Last run")
		result := o.Result
		if o.ExitCode != 0 {
			result = fmt.Sprintf("%s (exit %d)", o.Result, o.ExitCode)
		}
		if o.Result == state.ResultOK {
			kv("result", styleName.Render(result))
		} else {
			kv("result", styleBad.Render(result))
		}
		if !o.Finished.IsZero() {
			kv("finished", humanTime(o.Finished, now))
		}
		kv("duration", shortDuration(time.Duration(o.DurationSeconds)*time.Second))
		if o.Warnings > 0 {
			kv("warnings", fmt.Sprintf("%d", o.Warnings))
		}
		if o.Archive != "" {
			kv("archive", o.Archive)
		}
		if o.Files > 0 || o.OriginalBytes > 0 {
			size := humanBytes(o.OriginalBytes)
			if o.DeduplicatedBytes > 0 {
				size += fmt.Sprintf(" (+%s dedup)", humanBytes(o.DeduplicatedBytes))
			}
			kv("size", fmt.Sprintf("%d files, %s", o.Files, size))
		}
		if o.LastError != "" {
			kv("error", styleBad.Render(o.LastError))
		}
	}

	// Size trend across successful runs.
	sizes := make([]int64, 0, len(rec.History))
	for _, h := range rec.History {
		if h.OriginalBytes > 0 {
			sizes = append(sizes, h.OriginalBytes)
		}
	}
	if spark := sparkline(sizes); spark != "" {
		section("Size trend")
		fmt.Printf(edgePad+"  %s   %s → %s  (%d runs)\n",
			styleName.Render(spark),
			styleDetail.Render(humanBytes(sizes[0])),
			styleDetail.Render(humanBytes(sizes[len(sizes)-1])),
			len(sizes))
	}

	if len(rec.History) > 0 {
		section("Recent runs")
		fmt.Println()
		printRecentRuns(rec.History, now)
	}

	if rec.LastRun != nil && len(rec.LastRun.LogTail) > 0 {
		section("Last run log")
		fmt.Println()
		tail := rec.LastRun.LogTail
		const showLines = 20
		if len(tail) > showLines {
			fmt.Println(edgePad + styleDetail.Render(fmt.Sprintf("  … %d earlier lines omitted", len(tail)-showLines)))
			tail = tail[len(tail)-showLines:]
		}
		for _, line := range tail {
			fmt.Println(edgePad + "  " + colorLogLine(line))
		}
		fmt.Println()
		fmt.Println(edgePad + styleDetail.Render("Full log: ") +
			styleName.Render("borgmatic-manager logs "+name))
	}

	section("Config")
	if configYAML == "" {
		fmt.Println(edgePad + "  " + styleDetail.Render(configNote))
		return
	}
	fmt.Println(edgePad + "  " + styleDetail.Render("(passwords and passphrases redacted; real values are in the 0600 on-disk config)"))
	lines := strings.Split(strings.TrimRight(configYAML, "\n"), "\n")
	const maxConfigLines = 60
	if len(lines) > maxConfigLines {
		shown := lines[:maxConfigLines]
		for _, line := range shown {
			fmt.Println(edgePad + "  " + styleDetail.Render(line))
		}
		fmt.Println(edgePad + "  " + styleDetail.Render(fmt.Sprintf("… %d more lines. See: borgmatic-manager generate", len(lines)-maxConfigLines)))
		return
	}
	for _, line := range lines {
		fmt.Println(edgePad + "  " + styleDetail.Render(line))
	}
}

// printRecentRuns renders a compact table of past run outcomes, newest first.
func printRecentRuns(history []state.RunOutcome, now time.Time) {
	const maxRows = 10
	rows := history
	if len(rows) > maxRows {
		rows = rows[len(rows)-maxRows:]
	}

	type rr struct{ when, result, size, duration string }
	display := make([]rr, 0, len(rows))
	// Newest first.
	for i := len(rows) - 1; i >= 0; i-- {
		o := rows[i]
		when := "-"
		if !o.Finished.IsZero() {
			when = shortDuration(now.Sub(o.Finished)) + " ago"
		}
		result := o.Result
		if o.ExitCode != 0 {
			result = fmt.Sprintf("%s (exit %d)", o.Result, o.ExitCode)
		}
		size := "-"
		if o.OriginalBytes > 0 {
			size = humanBytes(o.OriginalBytes)
		}
		display = append(display, rr{when, result, size, shortDuration(time.Duration(o.DurationSeconds) * time.Second)})
	}

	tbl := table.New().
		BorderTop(false).BorderBottom(false).BorderLeft(false).BorderRight(false).
		BorderColumn(false).BorderHeader(false).BorderRow(false).
		StyleFunc(func(row, col int) lipgloss.Style {
			base := lipgloss.NewStyle().PaddingRight(2)
			switch {
			case row == table.HeaderRow:
				return base.Inherit(styleKind)
			case col == 1:
				if strings.HasPrefix(display[row].result, state.ResultOK) {
					return base.Inherit(styleName)
				}
				return base.Inherit(styleBad)
			}
			return base
		}).
		Headers("when", "result", "size", "duration")
	for _, r := range display {
		tbl.Row(r.when, r.result, r.size, r.duration)
	}
	fmt.Println(lipgloss.NewStyle().MarginLeft(hPad).Render(tbl.Render()))
}

// colorLogLine dims a log line, reddening CRITICAL/ERROR.
func colorLogLine(line string) string {
	if strings.HasPrefix(line, "CRITICAL") || strings.HasPrefix(line, "ERROR") {
		return styleBad.Render(line)
	}
	return styleDetail.Render(line)
}

// printAdhocSummary reports the outcome of an on-demand `run` to stdout.
// locked lists groups a concurrent run already holds a lock on (ad-hoc never
// waits, the user retries).
func printAdhocSummary(targets, failed, locked []string) {
	fmt.Println()
	if len(failed) == 0 && len(locked) == 0 {
		fmt.Println(edgePad + styleName.Render(fmt.Sprintf("✓ backed up %s", plural(len(targets), "group"))))
		fmt.Println()
		return
	}

	okCount := len(targets) - len(failed) - len(locked)
	fmt.Println(edgePad + styleTitle.Render("Run complete") +
		styleDetail.Render(fmt.Sprintf("  %d ok · %d failed · %d locked", okCount, len(failed), len(locked))))
	if len(failed) > 0 {
		fmt.Println()
		fmt.Println(edgePad + styleBad.Render("failed") + styleDetail.Render(": "+strings.Join(failed, ", ")))
		fmt.Println(edgePad + styleDetail.Render("Run ") +
			styleName.Render("borgmatic-manager inspect <group>") +
			styleDetail.Render(" to see why."))
	}
	if len(locked) > 0 {
		fmt.Println()
		fmt.Println(edgePad + styleWarn.Render("locked") +
			styleDetail.Render(" (a run is already in progress): "+strings.Join(locked, ", ")))
		fmt.Println(edgePad + styleDetail.Render("Not queued. Try again once it finishes."))
	}
	fmt.Println()
}

// humanBytes renders a byte count in decimal units, like borg's output.
func humanBytes(n int64) string {
	const unit, units = 1000, "kMGTPE"
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit && exp < len(units)-1; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), units[exp])
}

// shortDuration renders a duration compactly: 45s, 26m, 3h12m, 2d4h.
func shortDuration(d time.Duration) string {
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		if m := int(d.Minutes()) % 60; m > 0 {
			return fmt.Sprintf("%dh%dm", int(d.Hours()), m)
		}
		return fmt.Sprintf("%dh", int(d.Hours()))
	default:
		days, hours := int(d.Hours())/24, int(d.Hours())%24
		if hours > 0 {
			return fmt.Sprintf("%dd%dh", days, hours)
		}
		return fmt.Sprintf("%dd", days)
	}
}
