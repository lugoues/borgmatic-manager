package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// ManagerConfig is the top-level manager config file: manager settings plus a
// raw borgmatic section used as the base for per-group deep merges.
type ManagerConfig struct {
	Manager   ManagerSettings        `yaml:"manager"`
	Borgmatic map[string]interface{} `yaml:"borgmatic"`
}

// ManagerSettings holds manager-specific runtime configuration.
type ManagerSettings struct {
	// Period is the backup cycle interval (Go duration format, e.g. "1h").
	Period string `yaml:"period"`
	// BorgmaticPath is the path to the host borgmatic binary. Empty means
	// resolve via BORGMATIC_PATH env, then PATH, then well-known locations.
	BorgmaticPath string `yaml:"borgmatic_path"`
	// Actions are the borgmatic actions run per group per cycle, in order.
	// Empty means the default: create, prune, compact, check.
	Actions []string `yaml:"actions"`
	// RunTimeout bounds a single group's borgmatic run (Go duration format).
	// Empty or "0" means no timeout.
	RunTimeout string `yaml:"run_timeout"`
}

// LoadConfig reads the manager configuration from managerPath and loads any
// per-group override files from groupsDir. Each .yaml file in groupsDir
// provides a borgmatic section that will be deep-merged with the base config.
//
// The returned overrides map is keyed by group name (filename without .yaml).
// If groupsDir does not exist, no overrides are returned (not an error).
func LoadConfig(managerPath string, groupsDir string) (*ManagerConfig, map[string]map[string]interface{}, error) {
	data, err := os.ReadFile(managerPath) // #nosec G304 -- operator-provided config path
	if err != nil {
		return nil, nil, fmt.Errorf("reading manager config: %w", err)
	}

	var cfg ManagerConfig
	if parseErr := yaml.Unmarshal(data, &cfg); parseErr != nil {
		return nil, nil, fmt.Errorf("parsing manager config: %w", parseErr)
	}

	overrides := make(map[string]map[string]interface{})

	entries, err := os.ReadDir(groupsDir)
	if err != nil {
		if os.IsNotExist(err) {
			return &cfg, overrides, nil
		}
		return nil, nil, fmt.Errorf("reading groups directory: %w", err)
	}

	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		if !strings.HasSuffix(name, ".yaml") {
			continue
		}

		groupName := strings.TrimSuffix(name, ".yaml")
		groupPath := filepath.Join(groupsDir, name)

		groupData, err := os.ReadFile(groupPath) // #nosec G304 -- files under the operator's groups directory
		if err != nil {
			return nil, nil, fmt.Errorf("reading group override %s: %w", groupPath, err)
		}

		var raw map[string]interface{}
		if err := yaml.Unmarshal(groupData, &raw); err != nil {
			return nil, nil, fmt.Errorf("parsing group override %s: %w", groupPath, err)
		}

		if borgmatic, ok := raw["borgmatic"]; ok {
			if borgmaticMap, ok := borgmatic.(map[string]interface{}); ok {
				overrides[groupName] = borgmaticMap
			}
		}
	}

	return &cfg, overrides, nil
}
