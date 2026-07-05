package discovery

import (
	"fmt"
	"log/slog"
	"sort"
	"strconv"
	"strings"

	"github.com/lugoues/borgmatic-manager/internal/models"
)

const (
	labelPrefix = "borgmatic-manager."
	labelBackup = "borgmatic-manager.backup"
	labelGroup  = "borgmatic-manager.group"
	dbPrefix    = "borgmatic-manager.db."
)

// dbTypeSQLite gets special validation: no credentials, volume+path required.
const dbTypeSQLite = "sqlite"

var validDBTypes = map[string]bool{
	"postgresql": true,
	"mysql":      true,
	"mariadb":    true,
	dbTypeSQLite: true,
}

// IsBackupEnabled checks whether the volume has opted into backup
// via the borgmatic-manager.backup=true label.
func IsBackupEnabled(labels map[string]string) bool {
	return labels[labelBackup] == "true"
}

// GetGroup returns the borgmatic-manager.group label value, or empty string if absent.
func GetGroup(labels map[string]string) string {
	return labels[labelGroup]
}

// HasManagerLabels reports whether any borgmatic-manager.* label is present;
// used to warn on near-miss configurations instead of staying silent.
func HasManagerLabels(labels map[string]string) bool {
	for key := range labels {
		if strings.HasPrefix(key, labelPrefix) {
			return true
		}
	}
	return false
}

// ParseDatabaseLabels extracts indexed borgmatic-manager.db.{n}.* labels
// into a sorted slice of DatabaseConfig structs. Entries missing required
// fields or having an unknown type are warned about and skipped. Required
// fields are per-type: postgresql/mysql/mariadb need type, name, and
// username; sqlite needs type, name, volume, and path (and takes no
// credentials or address, sqlite has no authentication).
func ParseDatabaseLabels(labels map[string]string, logger *slog.Logger) []models.DatabaseConfig {
	if len(labels) == 0 {
		return nil
	}

	configs := make(map[int]*models.DatabaseConfig)

	for key, value := range labels {
		if !strings.HasPrefix(key, dbPrefix) {
			continue
		}

		// key is "borgmatic-manager.db.{index}.{field}"
		suffix := strings.TrimPrefix(key, dbPrefix)
		parts := strings.SplitN(suffix, ".", 2)
		if len(parts) != 2 {
			logger.Warn("malformed db label: missing field name", "label", key)
			continue
		}

		index, err := strconv.Atoi(parts[0])
		if err != nil {
			logger.Warn(fmt.Sprintf("non-integer db index %q, skipping label", parts[0]), "label", key)
			continue
		}

		field := parts[1]

		if configs[index] == nil {
			configs[index] = &models.DatabaseConfig{}
		}

		setDBField(configs[index], field, value, logger)
	}

	if len(configs) == 0 {
		return nil
	}

	// Sort indices for deterministic output.
	indices := make([]int, 0, len(configs))
	for idx := range configs {
		indices = append(indices, idx)
	}
	sort.Ints(indices)

	var result []models.DatabaseConfig
	for _, idx := range indices {
		cfg := configs[idx]

		if cfg.Type == "" {
			logger.Warn(fmt.Sprintf("db.%d missing required field 'type', skipping", idx))
			continue
		}
		if !validDBTypes[cfg.Type] {
			logger.Warn(fmt.Sprintf("db.%d has unknown type %q, skipping", idx, cfg.Type))
			continue
		}
		if cfg.Name == "" {
			logger.Warn(fmt.Sprintf("db.%d missing required field 'name', skipping", idx))
			continue
		}

		if cfg.Type == dbTypeSQLite {
			if !validateSQLite(cfg, idx, logger) {
				continue
			}
		} else if cfg.Username == "" {
			logger.Warn(fmt.Sprintf("db.%d missing required field 'username', skipping", idx))
			continue
		}

		result = append(result, *cfg)
	}

	if len(result) == 0 {
		return nil
	}

	return result
}

// validateSQLite requires volume and path; credentials and addresses are
// meaningless for sqlite and cleared with a warning.
func validateSQLite(cfg *models.DatabaseConfig, idx int, logger *slog.Logger) bool {
	if cfg.Volume == "" {
		logger.Warn(fmt.Sprintf("db.%d (sqlite) missing required field 'volume', skipping", idx))
		return false
	}
	if cfg.Path == "" {
		logger.Warn(fmt.Sprintf("db.%d (sqlite) missing required field 'path', skipping", idx))
		return false
	}
	if cfg.Username != "" || cfg.Password != "" || cfg.Hostname != "" || cfg.Port != 0 {
		logger.Warn(fmt.Sprintf("db.%d (sqlite) ignoring username/password/hostname/port: sqlite has no authentication", idx))
		cfg.Username = ""
		cfg.Password = ""
		cfg.Hostname = ""
		cfg.Port = 0
	}
	return true
}

// setDBField maps a field name to the corresponding DatabaseConfig struct field.
// Unknown fields are silently ignored for forward compatibility.
func setDBField(cfg *models.DatabaseConfig, field, value string, logger *slog.Logger) {
	switch field {
	case "type":
		cfg.Type = value
	case "name":
		cfg.Name = value
	case "username":
		cfg.Username = value
	case "password":
		cfg.Password = value
	case "hostname":
		cfg.Hostname = value
	case "port":
		port, err := strconv.Atoi(value)
		if err != nil {
			logger.Warn(fmt.Sprintf("invalid port value %q, using 0", value))
			return
		}
		cfg.Port = port
	case "volume":
		cfg.Volume = value
	case "path":
		cfg.Path = value
	case "options":
		cfg.Options = value
	case "network":
		// v1 label; host-run borgmatic connects via container IP or 'hostname' instead.
		logger.Warn("the 'network' db label is deprecated and ignored: host-run borgmatic connects via the container's IP (default) or the 'hostname' label")
	default:
		// Unknown fields ignored silently (forward-compatible).
	}
}
