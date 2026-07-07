package main

import (
	"fmt"
	"sort"

	"github.com/charmbracelet/lipgloss"

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

// printGroups renders the discovered backup groups.
func printGroups(state *models.BackupState) {
	names := make([]string, 0, len(state.Groups))
	for name := range state.Groups {
		names = append(names, name)
	}
	sort.Strings(names)

	for i, name := range names {
		if i > 0 {
			fmt.Println()
		}
		group := state.Groups[name]
		fmt.Println(styleGroup.Render("group " + name))

		for _, v := range group.Volumes {
			fmt.Printf("  %s %s  %s\n",
				styleKind.Render("volume  "),
				styleName.Render(fmt.Sprintf("%-28s", v.Name)),
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
			fmt.Printf("  %s %s  %s\n",
				styleKind.Render("database"),
				styleName.Render(fmt.Sprintf("%-28s", db.Type+"/"+db.Name)),
				styleDetail.Render(detail),
			)
		}
	}
}
