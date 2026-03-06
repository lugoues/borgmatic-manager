// Package discovery implements Docker container label parsing for
// borgmatic-manager's label-driven backup configuration.
package discovery

import (
	"log/slog"

	"github.com/lugoues/borgmatic-manager/internal/models"
)

// IsBackupEnabled checks whether the container has opted into backup
// via the borgmatic-manager.backup=true label.
func IsBackupEnabled(_ map[string]string) bool {
	return false
}

// GetGroup returns the borgmatic-manager.group label value, or empty string if absent.
func GetGroup(_ map[string]string) string {
	return ""
}

// ParseDatabaseLabels extracts indexed borgmatic-manager.db.{n}.* labels
// into a sorted slice of DatabaseConfig structs.
func ParseDatabaseLabels(_ map[string]string, _ *slog.Logger) []models.DatabaseConfig {
	return nil
}
