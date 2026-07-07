package config

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
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
	// ContainerCLI overrides the CLI used in generated database dump
	// commands ("docker" or "podman"). Empty means derive it from the
	// socket the manager is connected to, then PATH.
	ContainerCLI string `yaml:"container_cli"`
}

// LoadConfig reads the manager config, deep-merges conf.d/*.yaml drop-ins
// (lexical order, beside managerPath) over it, and loads per-group override
// fragments from groupsDir, keyed by filename sans extension. All files
// support borgmatic's !include tag. A missing groupsDir is not an error.
func LoadConfig(managerPath string, groupsDir string) (*ManagerConfig, map[string]map[string]interface{}, error) {
	managerMap, err := loadYAMLWithIncludes(managerPath)
	if err != nil {
		return nil, nil, fmt.Errorf("loading manager config: %w", err)
	}

	managerMap, err = mergeConfD(managerMap, filepath.Join(filepath.Dir(managerPath), "conf.d"))
	if err != nil {
		return nil, nil, err
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
		if !strings.HasSuffix(name, ".yaml") && !strings.HasSuffix(name, ".yml") {
			continue
		}

		groupName := strings.TrimSuffix(strings.TrimSuffix(name, ".yaml"), ".yml")
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

// mergeConfD deep-merges conf.d/*.yaml drop-ins over the base config in
// lexical filename order. A missing directory is fine.
func mergeConfD(base map[string]interface{}, confDir string) (map[string]interface{}, error) {
	entries, err := os.ReadDir(confDir)
	if err != nil {
		if os.IsNotExist(err) {
			return base, nil
		}
		return nil, fmt.Errorf("reading conf.d directory: %w", err)
	}

	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || (!strings.HasSuffix(entry.Name(), ".yaml") && !strings.HasSuffix(entry.Name(), ".yml")) {
			continue
		}
		names = append(names, entry.Name())
	}
	sort.Strings(names)

	for _, name := range names {
		fragment, err := loadYAMLWithIncludes(filepath.Join(confDir, name))
		if err != nil {
			return nil, fmt.Errorf("loading conf.d drop-in %s: %w", name, err)
		}
		base = DeepMerge(base, fragment)
	}
	return base, nil
}
