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
	styleTitle  = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("15"))
)

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

// printStatus renders per-group schedule state: last run, its result, and
// when the next run is due, with the soonest next-run in the header blob.
// Refused groups (generation safety checks) are marked as such instead of
// showing as "due now" forever.
func printStatus(bs *models.BackupState, store *state.ScheduleStore, period time.Duration, refused map[string]string) {
	// Trailing blank line keeps the table off the shell prompt.
	defer fmt.Println()
	now := time.Now()
	fmt.Println()

	type row struct{ name, last, result, files, size, next string }
	rows := make([]row, 0, len(bs.Groups))
	var soonest time.Duration = -1

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
			if o.Result == "ok" {
				detail := shortDuration(time.Duration(o.DurationSeconds) * time.Second)
				if o.Warnings > 0 {
					detail += fmt.Sprintf(", %d warnings", o.Warnings)
				}
				r.result = fmt.Sprintf("ok (%s)", detail)
			} else if o.ExitCode != 0 {
				r.result = fmt.Sprintf("%s (exit %d)", o.Result, o.ExitCode)
			}
			if o.Files > 0 || o.OriginalBytes > 0 {
				r.files = fmt.Sprintf("%d", o.Files)
				r.size = humanBytes(o.OriginalBytes)
				if o.DeduplicatedBytes > 0 {
					r.size += fmt.Sprintf(" (+%s)", humanBytes(o.DeduplicatedBytes))
				}
			}
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
	if soonest == 0 {
		header = "next run: due now"
	} else if soonest > 0 {
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
				if strings.HasPrefix(r.next, "refused") {
					return base.Inherit(styleBad)
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
