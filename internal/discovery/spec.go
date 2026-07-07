package discovery

import (
	"fmt"
	"log/slog"
	"strings"

	"gopkg.in/yaml.v3"

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
	Group string `yaml:"group" json:"group"`
	// Enable opts the container's named volumes into raw file backup.
	Enable bool `yaml:"enable" json:"enable"`
	// Volumes filters which volumes back up (names or in-container mount
	// paths). Absent means all named volumes.
	Volumes *[]string `yaml:"volumes" json:"volumes"`
	// Databases lists database dumps, mirroring the db.{n}.* labels.
	Databases []SpecDatabase `yaml:"databases" json:"databases"`
	// Config is a borgmatic config fragment for the group, mirroring config.* labels.
	Config map[string]interface{} `yaml:"config" json:"config"`
}

// SpecDatabase mirrors the db.{n}.* label fields.
type SpecDatabase struct {
	Type     string `yaml:"type" json:"type"`
	Name     string `yaml:"name" json:"name"`
	Username string `yaml:"username" json:"username"`
	Password string `yaml:"password" json:"password"`
	Hostname string `yaml:"hostname" json:"hostname"`
	Port     int    `yaml:"port" json:"port"`
	Mode     string `yaml:"mode" json:"mode"`
	Volume   string `yaml:"volume" json:"volume"`
	Path     string `yaml:"path" json:"path"`
	Options  string `yaml:"options" json:"options"`
}

// ParseSpecLabel parses the borgmatic-manager.spec label. The second return
// reports whether the label is present at all. A present-but-invalid spec
// returns (nil, true) after warning, the container is skipped entirely
// rather than half-applied. Decoding is strict: unknown field names are
// errors, so typos surface instead of silently dropping settings.
func ParseSpecLabel(labels map[string]string, containerName string, logger *slog.Logger) (*ContainerSpec, bool) {
	raw, ok := labels[labelSpec]
	if !ok {
		return nil, false
	}

	dec := yaml.NewDecoder(strings.NewReader(raw))
	dec.KnownFields(true)
	var spec ContainerSpec
	if err := dec.Decode(&spec); err != nil {
		logger.Warn("invalid borgmatic-manager.spec label; container skipped",
			"container", containerName, "error", err,
			"hint", `write JSON ({"group": "x", "enable": true}) or YAML flow ({group: x, enable: true}, a space after each colon is required); fields: group, enable, volumes, databases, config`)
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
