package main

import (
	"fmt"
	"os"
	"sort"

	"github.com/charmbracelet/lipgloss"
	"golang.org/x/term"

	"github.com/lugoues/borgmatic-manager/internal/models"
)

// lipgloss degrades to plain text on non-TTY output and honors NO_COLOR, so the
// literal tokens stay stable and greppable.
var (
	styleGroup  = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("6"))
	styleKind   = lipgloss.NewStyle().Foreground(lipgloss.Color("8"))
	styleName   = lipgloss.NewStyle().Foreground(lipgloss.Color("2"))
	styleDetail = lipgloss.NewStyle().Foreground(lipgloss.Color("8"))
)

// printGroups renders the discovered backup groups, headed by a dim summary
// aligned to the terminal's right edge (plain and left-aligned when piped).
func printGroups(state *models.BackupState) {
	fmt.Println(summaryLine(state))
	fmt.Println()

	names := make([]string, 0, len(state.Groups))
	for name := range state.Groups {
		names = append(names, name)
	}
	sort.Strings(names)

	// One name-column width across all groups. Pad before styling: ANSI codes
	// would throw off printf width math.
	nameWidth := 0
	for _, group := range state.Groups {
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
		group := state.Groups[name]
		fmt.Println(styleGroup.Render("group " + name))

		for _, v := range group.Volumes {
			fmt.Printf("  %s  %s  %s\n",
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
			fmt.Printf("  %s  %s  %s\n",
				styleKind.Render(fmt.Sprintf("%-8s", "database")),
				styleName.Render(fmt.Sprintf("%-*s", nameWidth, db.Type+"/"+db.Name)),
				styleDetail.Render(detail),
			)
		}
	}
}

// summaryLine renders "N groups · N volumes · N databases", right-aligned
// on a TTY.
func summaryLine(state *models.BackupState) string {
	groups, volumes, databases := len(state.Groups), 0, 0
	for _, g := range state.Groups {
		volumes += len(g.Volumes)
		databases += len(g.Databases)
	}

	s := styleDetail.Render(fmt.Sprintf("%s · %s · %s",
		plural(groups, "group"), plural(volumes, "volume"), plural(databases, "database")))

	if width, _, err := term.GetSize(int(os.Stdout.Fd())); err == nil && width > 0 {
		return lipgloss.PlaceHorizontal(width, lipgloss.Right, s)
	}
	return s
}

func plural(n int, noun string) string {
	if n == 1 {
		return fmt.Sprintf("1 %s", noun)
	}
	return fmt.Sprintf("%d %ss", n, noun)
}
