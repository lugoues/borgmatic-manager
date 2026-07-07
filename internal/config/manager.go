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
// per-group override files from groupsDir. Both support borgmatic's !include
// tag (see loadYAMLWithIncludes), so shared config files work in and out of
// borgmatic-manager.
//
// A group file is a borgmatic config fragment: top-level borgmatic options
// deep-merged over the manager.yaml defaults. (A legacy wrapper form with a
// single top-level "borgmatic" key is also accepted.) The returned overrides
// map is keyed by group name (filename without .yaml). If groupsDir does not
// exist, no overrides are returned (not an error).
func LoadConfig(managerPath string, groupsDir string) (*ManagerConfig, map[string]map[string]interface{}, error) {
	managerMap, err := loadYAMLWithIncludes(managerPath)
	if err != nil {
		return nil, nil, fmt.Errorf("loading manager config: %w", err)
	}

	// Round-trip the resolved map into the typed struct.
	resolved, err := yaml.Marshal(managerMap)
	if err != nil {
		return nil, nil, fmt.Errorf("processing manager config: %w", err)
	}
	var cfg ManagerConfig
	if parseErr := yaml.Unmarshal(resolved, &cfg); parseErr != nil {
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

		raw, err := loadYAMLWithIncludes(groupPath)
		if err != nil {
			return nil, nil, fmt.Errorf("loading group override %s: %w", groupPath, err)
		}

		// Legacy wrapper form: a lone top-level "borgmatic" key.
		if len(raw) == 1 {
			if wrapped, ok := raw["borgmatic"].(map[string]interface{}); ok {
				overrides[groupName] = wrapped
				continue
			}
		}
		overrides[groupName] = raw
	}

	return &cfg, overrides, nil
}
