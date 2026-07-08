package discovery

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/lugoues/borgmatic-manager/internal/models"
)

// labelSpec carries the whole configuration as one JSON blob; when present,
// all other borgmatic-manager.* labels on the container are ignored.
const labelSpec = "borgmatic-manager.spec"

// ContainerSpec is the one-label JSON configuration document.
type ContainerSpec struct {
	// Group is the backup group (required).
	Group string `json:"group"`
	// Enable opts the container's named volumes into raw file backup.
	Enable bool `json:"enable"`
	// Volumes filters backed-up volumes; absent or empty both mean all named volumes.
	Volumes []string `json:"volumes"`
	// Databases mirrors the db.{n}.* labels; the JSON field is "db".
	Databases []SpecDatabase `json:"db"`
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

// ParseSpecLabel parses the strict-JSON borgmatic-manager.spec label (bool = present).
// A present-but-invalid spec fails discovery: skipping it silently shrinks the backup set.
func ParseSpecLabel(labels map[string]string, containerName string) (*ContainerSpec, bool, error) {
	raw, ok := labels[labelSpec]
	if !ok {
		return nil, false, nil
	}

	dec := json.NewDecoder(strings.NewReader(raw))
	dec.DisallowUnknownFields()
	var spec ContainerSpec
	err := dec.Decode(&spec)
	if err == nil && dec.More() {
		err = fmt.Errorf("trailing data after the JSON document")
	}
	if err != nil {
		hint := `the value must be valid JSON, e.g. {"group": "x", "enable": true}; fields: group, enable, volumes, db, config`
		if strings.HasPrefix(strings.TrimSpace(raw), "{") && !strings.Contains(raw, `"`) {
			// Quadlet: systemd strips unquoted double quotes from Label= values.
			hint = `the label value contains no double quotes, systemd/quadlet strips them from unquoted Label= lines; wrap the whole assignment in single quotes: Label='borgmatic-manager.spec={"group": "x", "enable": true}'`
		}
		return nil, true, fmt.Errorf("container %s: invalid borgmatic-manager.spec label %q: %w (%s)", containerName, raw, err, hint)
	}

	if spec.Group == "" {
		return nil, true, fmt.Errorf("container %s: borgmatic-manager.spec is missing the required \"group\" field", containerName)
	}

	return &spec, true, nil
}

// databases validates spec entries under the same rules as the flat db.{n}.*
// labels; broken entries fail the cycle.
func (s *ContainerSpec) databases(containerName string, logger *slog.Logger) ([]models.DatabaseConfig, error) {
	var errs []error
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
		ref := fmt.Sprintf("%s spec db[%d]", containerName, i)
		if err := validateDatabase(&cfg, ref, logger); err != nil {
			errs = append(errs, err)
			continue
		}
		result = append(result, cfg)
	}
	if len(errs) > 0 {
		return nil, errors.Join(errs...)
	}
	if len(result) == 0 {
		return nil, nil
	}
	return result, nil
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
