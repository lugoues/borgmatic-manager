package config

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// ManagerConfig is the top-level manager config file: manager settings plus a
// raw borgmatic section used as the base for per-group deep merges.
type ManagerConfig struct {
	Manager   ManagerSettings        `yaml:"manager"`
	Borgmatic map[string]interface{} `yaml:"borgmatic"`

	// GroupPeriods holds per-group manager.period overrides from the groups/
	// overlays, resolved by LoadConfig. Label overrides still beat these.
	GroupPeriods map[string]time.Duration `yaml:"-"`
}

// ParsedPeriod parses and validates manager.period in one place. Non-positive
// periods are rejected: zero would hot-loop the cycle timer.
func (c *ManagerConfig) ParsedPeriod() (time.Duration, error) {
	d, err := time.ParseDuration(c.Manager.Period)
	if err != nil {
		return 0, fmt.Errorf("invalid manager.period %q: %w", c.Manager.Period, err)
	}
	if d <= 0 {
		return 0, fmt.Errorf("invalid manager.period %q: must be positive", c.Manager.Period)
	}
	return d, nil
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

// GroupOverride is one groups/{group}.yaml overlay: the same manager+borgmatic
// shape as manager.yaml, scoped to a single group.
type GroupOverride struct {
	// Period overrides manager.period for the group; 0 means no override.
	Period time.Duration
	// Borgmatic is the group's borgmatic config fragment, deep-merged over
	// the manager.yaml borgmatic defaults.
	Borgmatic map[string]interface{}
}

// parseGroupOverride validates a group overlay. Top-level keys are restricted
// to manager/borgmatic, and manager keys to period: a typo'd or un-nested key
// silently changing nothing would be worse than the error.
func parseGroupOverride(raw map[string]interface{}, path string) (GroupOverride, error) {
	var o GroupOverride
	for key, val := range raw {
		switch key {
		case "borgmatic":
			m, ok := val.(map[string]interface{})
			if !ok {
				return o, fmt.Errorf("group override %s: borgmatic section must be a mapping", path)
			}
			o.Borgmatic = m
		case "manager":
			m, ok := val.(map[string]interface{})
			if !ok {
				return o, fmt.Errorf("group override %s: manager section must be a mapping", path)
			}
			for mk, mv := range m {
				switch mk {
				case "period":
					s, ok := mv.(string)
					if !ok {
						return o, fmt.Errorf("group override %s: manager.period must be a duration string", path)
					}
					d, err := time.ParseDuration(strings.TrimSpace(s))
					if err != nil {
						return o, fmt.Errorf("group override %s: invalid manager.period %q: %w", path, s, err)
					}
					if d <= 0 {
						return o, fmt.Errorf("group override %s: invalid manager.period %q: must be positive", path, s)
					}
					o.Period = d
				default:
					return o, fmt.Errorf("group override %s: unknown manager option %q (supported: period)", path, mk)
				}
			}
		default:
			return o, fmt.Errorf("group override %s: unknown top-level key %q: group files are manager.yaml-shaped overlays; nest borgmatic options under a borgmatic: section", path, key)
		}
	}
	return o, nil
}

// LoadConfig reads the manager config, deep-merges conf.d/*.yaml drop-ins
// (lexical order, beside managerPath) over it, and loads per-group overlays
// from groupsDir, keyed by filename sans extension. All files support
// borgmatic's !include tag. A missing groupsDir is not an error.
func LoadConfig(managerPath string, groupsDir string) (*ManagerConfig, map[string]GroupOverride, error) {
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

	overrides := make(map[string]GroupOverride)

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

		override, err := parseGroupOverride(raw, groupPath)
		if err != nil {
			return nil, nil, err
		}
		overrides[groupName] = override
		if override.Period > 0 {
			if cfg.GroupPeriods == nil {
				cfg.GroupPeriods = make(map[string]time.Duration)
			}
			cfg.GroupPeriods[groupName] = override.Period
		}
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
