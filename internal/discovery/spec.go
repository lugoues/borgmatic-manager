package discovery

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"

	"github.com/lugoues/borgmatic-manager/internal/models"
)

// labelSpec carries the whole configuration as one JSON blob; when present,
// all other borgmatic-manager.* labels on the container are ignored.
const labelSpec = "borgmatic-manager.spec"

// ContainerSpec is the one-label JSON configuration document.
//
//	{
//	  "group": "myapp",
//	  "enable": true,
//	  "volumes": ["app-data", "/uploads"],
//	  "databases": [{"type": "postgresql", "name": "appdb", "username": "u"}],
//	  "config": {"keep_daily": 14}
//	}
type ContainerSpec struct {
	// Group is the backup group (required).
	Group string `json:"group"`
	// Enable opts the container's named volumes into raw file backup.
	Enable bool `json:"enable"`
	// Volumes filters which volumes back up (names or in-container mount
	// paths). Absent means all named volumes.
	Volumes *[]string `json:"volumes"`
	// Databases lists database dumps, mirroring the db.{n}.* labels.
	Databases []SpecDatabase `json:"databases"`
	// Config is a borgmatic config fragment for the group, mirroring config.* labels.
	Config map[string]interface{} `json:"config"`
}

// SpecDatabase mirrors the db.{n}.* label fields.
type SpecDatabase struct {
	Type     string `json:"type"`
	Name     string `json:"name"`
	Username string `json:"username"`
	Password string `json:"password"`
	Hostname string `json:"hostname"`
	Port     int    `json:"port"`
	Mode     string `json:"mode"`
	Volume   string `json:"volume"`
	Path     string `json:"path"`
	Options  string `json:"options"`
}

// ParseSpecLabel parses the borgmatic-manager.spec label. The second return
// reports whether the label is present at all. A present-but-invalid spec
// returns (nil, true) after warning, the container is skipped entirely
// rather than half-applied. The value is strict JSON: unknown field names
// are errors (typos surface instead of silently dropping settings), and
// non-JSON dialects are rejected rather than half-supported.
func ParseSpecLabel(labels map[string]string, containerName string, logger *slog.Logger) (*ContainerSpec, bool) {
	raw, ok := labels[labelSpec]
	if !ok {
		return nil, false
	}

	dec := json.NewDecoder(strings.NewReader(raw))
	dec.DisallowUnknownFields()
	var spec ContainerSpec
	err := dec.Decode(&spec)
	if err == nil && dec.More() {
		err = fmt.Errorf("trailing data after the JSON document")
	}
	if err != nil {
		logger.Warn("invalid borgmatic-manager.spec label; container skipped",
			"container", containerName, "error", err,
			"hint", `the value must be valid JSON, e.g. {"group": "x", "enable": true}; in quadlet files wrap the assignment in single quotes: Label='borgmatic-manager.spec={"group": "x"}'; fields: group, enable, volumes, databases, config`)
		return nil, true
	}

	if spec.Group == "" {
		logger.Warn("borgmatic-manager.spec is missing the required \"group\" field; container skipped",
			"container", containerName)
		return nil, true
	}

	return &spec, true
}

// databases converts and validates the spec's database entries using the
// same per-type rules as the flat db.{n}.* labels.
func (s *ContainerSpec) databases(containerName string, logger *slog.Logger) []models.DatabaseConfig {
	var result []models.DatabaseConfig
	for i, sdb := range s.Databases {
		cfg := models.DatabaseConfig{
			Type:     sdb.Type,
			Name:     sdb.Name,
			Username: sdb.Username,
			Password: sdb.Password,
			Hostname: sdb.Hostname,
			Port:     sdb.Port,
			Mode:     sdb.Mode,
			Volume:   sdb.Volume,
			Path:     sdb.Path,
			Options:  sdb.Options,
		}
		ref := fmt.Sprintf("%s spec databases[%d]", containerName, i)
		if !validateDatabase(&cfg, ref, logger) {
			continue
		}
		result = append(result, cfg)
	}
	if len(result) == 0 {
		return nil
	}
	return result
}

// warnIgnoredFlatLabels logs flat borgmatic-manager.* labels shadowed by a spec label.
func warnIgnoredFlatLabels(labels map[string]string, containerName string, logger *slog.Logger) {
	var ignored []string
	for key := range labels {
		if strings.HasPrefix(key, labelPrefix) && key != labelSpec {
			ignored = append(ignored, key)
		}
	}
	if len(ignored) > 0 {
		logger.Warn("borgmatic-manager.spec is set: other borgmatic-manager.* labels on this container are ignored",
			"container", containerName, "ignored", strings.Join(ignored, ","))
	}
}
