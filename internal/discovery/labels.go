package discovery

import (
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strconv"
	"strings"
	"time"

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
	labelPeriod        = "borgmatic-manager.period"
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

// ParsePeriodLabel parses the optional borgmatic-manager.period label, which
// overrides manager.period for the container's group. Invalid values are
// errors: silently falling back to the global period would hide a typo.
func ParsePeriodLabel(labels map[string]string, containerName string) (time.Duration, error) {
	raw, ok := labels[labelPeriod]
	if !ok {
		return 0, nil
	}
	return parsePeriodValue(raw, containerName)
}

// parsePeriodValue validates a period from a label or spec: a positive Go duration.
func parsePeriodValue(raw, containerName string) (time.Duration, error) {
	d, err := time.ParseDuration(strings.TrimSpace(raw))
	if err != nil {
		return 0, fmt.Errorf("container %s: invalid period %q: %w", containerName, raw, err)
	}
	if d <= 0 {
		return 0, fmt.Errorf("container %s: invalid period %q: must be positive", containerName, raw)
	}
	return d, nil
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

// ParseDatabaseLabels parses indexed borgmatic-manager.db.{n}.* labels. Malformed
// or invalid entries are errors: skipping one silently shrinks the backup set.
func ParseDatabaseLabels(labels map[string]string, logger *slog.Logger) ([]models.DatabaseConfig, error) {
	if len(labels) == 0 {
		return nil, nil
	}

	configs := make(map[int]*models.DatabaseConfig)

	var errs []error
	for key, value := range labels {
		if !strings.HasPrefix(key, dbPrefix) {
			continue
		}

		// key is "borgmatic-manager.db.{index}.{field}"
		suffix := strings.TrimPrefix(key, dbPrefix)
		parts := strings.SplitN(suffix, ".", 2)
		if len(parts) != 2 {
			errs = append(errs, fmt.Errorf("malformed db label %q: missing field name", key))
			continue
		}

		index, err := strconv.Atoi(parts[0])
		if err != nil {
			errs = append(errs, fmt.Errorf("db label %q has a non-integer index %q", key, parts[0]))
			continue
		}

		field := parts[1]

		if configs[index] == nil {
			configs[index] = &models.DatabaseConfig{}
		}

		if err := setDBField(configs[index], field, value, logger); err != nil {
			errs = append(errs, fmt.Errorf("db label %q: %w", key, err))
		}
	}
	if len(errs) > 0 {
		return nil, errors.Join(errs...)
	}

	if len(configs) == 0 {
		return nil, nil
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
		if err := validateDatabase(cfg, fmt.Sprintf("db.%d", idx), logger); err != nil {
			errs = append(errs, err)
			continue
		}
		result = append(result, *cfg)
	}
	if len(errs) > 0 {
		return nil, errors.Join(errs...)
	}

	if len(result) == 0 {
		return nil, nil
	}

	return result, nil
}

// validateDatabase enforces per-type field rules (ref names the source). Broken
// entries are errors (silent shrink); lossless fix-ups stay warnings.
func validateDatabase(cfg *models.DatabaseConfig, ref string, logger *slog.Logger) error {
	if cfg.Type == "" {
		return fmt.Errorf("%s is missing the required field 'type'", ref)
	}
	if !validDBTypes[cfg.Type] {
		return fmt.Errorf("%s has unknown type %q", ref, cfg.Type)
	}
	if cfg.Name == "" {
		return fmt.Errorf("%s is missing the required field 'name'", ref)
	}

	if cfg.Type == dbTypeSQLite {
		if err := validateSQLite(cfg, ref, logger); err != nil {
			return err
		}
	} else if cfg.Username == "" {
		return fmt.Errorf("%s is missing the required field 'username'", ref)
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

	return nil
}

// validateSQLite requires volume and path; credentials and addresses are
// meaningless for sqlite and cleared with a warning.
func validateSQLite(cfg *models.DatabaseConfig, ref string, logger *slog.Logger) error {
	if cfg.Volume == "" {
		return fmt.Errorf("%s (sqlite) is missing the required field 'volume'", ref)
	}
	if cfg.Path == "" {
		return fmt.Errorf("%s (sqlite) is missing the required field 'path'", ref)
	}
	if cfg.Username != "" || cfg.Password != "" || cfg.Hostname != "" || cfg.Port != 0 {
		logger.Warn(fmt.Sprintf("%s (sqlite) ignoring username/password/hostname/port: sqlite has no authentication", ref))
		cfg.Username = ""
		cfg.Password = ""
		cfg.Hostname = ""
		cfg.Port = 0
	}
	// sqlite entries have no dump command; stray options would fail borgmatic's
	// schema validation every cycle, so clear them.
	if cfg.Options != "" {
		logger.Warn(fmt.Sprintf("%s (sqlite) ignoring 'options': sqlite entries take no dump options", ref))
		cfg.Options = ""
	}
	return nil
}

// setDBField maps a label field onto DatabaseConfig. Unknown fields and bad
// values are errors: a typo would silently change what gets backed up.
func setDBField(cfg *models.DatabaseConfig, field, value string, logger *slog.Logger) error {
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
			return fmt.Errorf("invalid port value %q", value)
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
		return fmt.Errorf("unknown field %q (valid: type, name, username, password, hostname, port, volume, path, options, mode)", field)
	}
	return nil
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
