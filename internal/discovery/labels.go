package discovery

import (
	"fmt"
	"log/slog"
	"sort"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/lugoues/borgmatic-manager/internal/models"
)

const (
	labelPrefix = "borgmatic-manager."
	labelEnable = "borgmatic-manager.enable"
	// labelEnableRenamed is the pre-rename spelling; seen -> warn, ignored.
	labelEnableRenamed = "borgmatic-manager.backup"
	labelGroup         = "borgmatic-manager.group"
	labelVolumes       = "borgmatic-manager.volumes"
	dbPrefix           = "borgmatic-manager.db."
	configPrefix       = "borgmatic-manager.config."
)

// dbTypeSQLite gets special validation: no credentials, volume+path required.
const dbTypeSQLite = "sqlite"

var validDBTypes = map[string]bool{
	"postgresql": true,
	"mysql":      true,
	"mariadb":    true,
	dbTypeSQLite: true,
}

// IsEnabled reports whether borgmatic-manager.enable=true opts the container's volumes in.
func IsEnabled(labels map[string]string) bool {
	return labels[labelEnable] == "true"
}

// VolumesFilter parses the comma-separated volumes label. Absent or empty both
// mean all named volumes: an empty filter matching nothing would shrink the set.
func VolumesFilter(labels map[string]string) ([]string, bool) {
	raw, ok := labels[labelVolumes]
	if !ok {
		return nil, false
	}
	var out []string
	for _, part := range strings.Split(raw, ",") {
		if trimmed := strings.TrimSpace(part); trimmed != "" {
			out = append(out, trimmed)
		}
	}
	return out, len(out) > 0
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
		if !validateDatabase(cfg, fmt.Sprintf("db.%d", idx), logger) {
			continue
		}
		result = append(result, *cfg)
	}

	if len(result) == 0 {
		return nil
	}

	return result
}

// validateDatabase enforces per-type field rules for a database definition,
// regardless of whether it came from flat db.{n}.* labels or a spec blob.
// ref names the source in warnings (e.g. "db.0", "web spec databases[1]").
func validateDatabase(cfg *models.DatabaseConfig, ref string, logger *slog.Logger) bool {
	if cfg.Type == "" {
		logger.Warn(fmt.Sprintf("%s missing required field 'type', skipping", ref))
		return false
	}
	if !validDBTypes[cfg.Type] {
		logger.Warn(fmt.Sprintf("%s has unknown type %q, skipping", ref, cfg.Type))
		return false
	}
	if cfg.Name == "" {
		logger.Warn(fmt.Sprintf("%s missing required field 'name', skipping", ref))
		return false
	}

	if cfg.Type == dbTypeSQLite {
		if !validateSQLite(cfg, ref, logger) {
			return false
		}
	} else if cfg.Username == "" {
		logger.Warn(fmt.Sprintf("%s missing required field 'username', skipping", ref))
		return false
	}

	switch cfg.Mode {
	case "", "helper":
		cfg.Mode = ""
	case "exec":
		if cfg.Type != "postgresql" {
			logger.Warn(fmt.Sprintf("%s: exec mode is only supported for postgresql (mysql/mariadb dumps write through a FIFO the exec'd client cannot reach); falling back to the helper container", ref))
			cfg.Mode = ""
		}
	default:
		logger.Warn(fmt.Sprintf("%s: unknown mode %q, using the default helper container", ref, cfg.Mode))
		cfg.Mode = ""
	}

	return true
}

// validateSQLite requires volume and path; credentials and addresses are
// meaningless for sqlite and cleared with a warning.
func validateSQLite(cfg *models.DatabaseConfig, ref string, logger *slog.Logger) bool {
	if cfg.Volume == "" {
		logger.Warn(fmt.Sprintf("%s (sqlite) missing required field 'volume', skipping", ref))
		return false
	}
	if cfg.Path == "" {
		logger.Warn(fmt.Sprintf("%s (sqlite) missing required field 'path', skipping", ref))
		return false
	}
	if cfg.Username != "" || cfg.Password != "" || cfg.Hostname != "" || cfg.Port != 0 {
		logger.Warn(fmt.Sprintf("%s (sqlite) ignoring username/password/hostname/port: sqlite has no authentication", ref))
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
	case "mode":
		cfg.Mode = value
	case "network":
		// v1 label; host-run borgmatic connects via container IP or 'hostname' instead.
		logger.Warn("the 'network' db label is deprecated and ignored: host-run borgmatic connects via the container's IP (default) or the 'hostname' label")
	default:
		// Unknown fields ignored silently (forward-compatible).
	}
}

// ParseConfigLabels builds a nested config fragment from config.* labels. Values
// parse as YAML; option names are checked later by 'borgmatic config validate'.
func ParseConfigLabels(labels map[string]string, logger *slog.Logger) map[string]interface{} {
	var result map[string]interface{}

	// Sorted iteration: with conflicting paths (config.a and config.a.b)
	// the outcome must not depend on map order, sorting makes the
	// deeper path win consistently, and the conflict is warned below.
	keys := make([]string, 0, len(labels))
	for key := range labels {
		if strings.HasPrefix(key, configPrefix) {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)

	for _, key := range keys {
		value := labels[key]
		path := strings.TrimPrefix(key, configPrefix)
		if path == "" {
			logger.Warn("malformed config label: empty option path", "label", key)
			continue
		}

		var parsed interface{}
		if err := yaml.Unmarshal([]byte(value), &parsed); err != nil {
			logger.Warn("config label value is not valid YAML; using the raw string", "label", key, "error", err)
			parsed = value
		}

		if result == nil {
			result = make(map[string]interface{})
		}
		node := result
		parts := strings.Split(path, ".")
		for i, part := range parts {
			if i == len(parts)-1 {
				node[part] = parsed
				break
			}
			child, ok := node[part].(map[string]interface{})
			if !ok {
				if _, conflict := node[part]; conflict {
					logger.Warn("conflicting config labels: a nested option overrides a value set by a shorter label path",
						"label", key, "overridden_path", strings.Join(parts[:i+1], "."))
				}
				child = make(map[string]interface{})
				node[part] = child
			}
			node = child
		}
	}

	return result
}
