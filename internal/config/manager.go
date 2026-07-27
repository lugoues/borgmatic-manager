package config

import (
	"bytes"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
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
	// Metrics configures OpenTelemetry metric export (daemon only).
	Metrics MetricsSettings `yaml:"metrics"`
}

// MetricsSettings configures native OpenTelemetry metric export over OTLP. The
// exporter also honors the standard OTEL_EXPORTER_OTLP_* and
// OTEL_RESOURCE_ATTRIBUTES environment variables; fields here take precedence.
type MetricsSettings struct {
	// Enabled turns on metric export. Off by default: no exporter, no metrics.
	Enabled bool `yaml:"enabled"`
	// Endpoint is the OTLP collector URL (e.g. "http://localhost:4318"). Empty
	// falls back to OTEL_EXPORTER_OTLP_ENDPOINT, then the OTLP default.
	Endpoint string `yaml:"endpoint"`
	// Protocol selects the OTLP transport: "http" (default) or "grpc".
	Protocol string `yaml:"protocol"`
}

// Validate rejects a misconfigured metrics block at load time, so a typo fails
// fast with a clear message instead of silently disabling metrics when the
// daemon starts. It only checks when metrics are enabled; runtime conditions
// (an unreachable collector) stay best-effort and never block startup.
func (m MetricsSettings) Validate() error {
	if !m.Enabled {
		return nil
	}
	switch strings.ToLower(strings.TrimSpace(m.Protocol)) {
	case "", "http", "http/protobuf", "grpc":
	default:
		return fmt.Errorf("metrics.protocol %q is invalid (want \"http\" or \"grpc\"); the OTLP URL belongs in metrics.endpoint", m.Protocol)
	}
	if m.Endpoint != "" {
		// Reported redacted: a rejected endpoint is as likely to carry a
		// credential as an accepted one, and this error is logged as "startup
		// failed" to the same journal the successful startup line goes to. A
		// scheme typo in an authenticated URL is precisely how a password ends
		// up there.
		shown := RedactEndpoint(m.Endpoint)
		u, err := url.Parse(m.Endpoint)
		switch {
		case err != nil || u.Host == "":
			return fmt.Errorf("metrics.endpoint %q is not a valid URL (want e.g. \"https://collector.example/v1/metrics\")", shown)
		case u.Scheme != "http" && u.Scheme != "https":
			return fmt.Errorf("metrics.endpoint %q must use http or https", shown)
		}
	}
	return nil
}

// RedactEndpoint strips userinfo and query values from an endpoint, leaving the
// parts that identify where metrics are going.
//
// It lives here, next to the validation, because this is the first place the
// configured value is handled and both the rejection and the startup log need
// the same treatment. A value that does not parse is reported as unprintable
// rather than guessed at: a malformed endpoint can still hold a secret, and a
// typo in the scheme is one of the ways it gets malformed.
func RedactEndpoint(endpoint string) string {
	if endpoint == "" {
		return endpoint
	}
	if !strings.Contains(endpoint, "//") {
		// Malformed before the authority, "https:/user:pw@host" being the shape
		// a scheme typo produces, so url.Parse finds no host and every
		// credential in it stays in the string. A value carrying userinfo or a
		// query is unprintable when it cannot be parsed; a plain "host:port" has
		// nothing to hide and stays readable.
		if strings.ContainsAny(endpoint, "@?#") {
			return "(unprintable endpoint)"
		}
		return endpoint
	}
	u, err := url.Parse(endpoint)
	if err != nil {
		return "(unparsable endpoint)"
	}
	if u.User != nil {
		u.User = url.User("redacted")
	}
	if u.RawQuery != "" {
		u.RawQuery = "redacted"
	}
	u.Fragment = ""
	return u.String()
}

// rejectMisplacedManagerOptions fails a config that puts a manager option at the
// document root instead of under "manager".
//
// The root is decoded leniently on purpose, because a config may carry
// anchor-holder keys that mean nothing to this program. That leniency is what
// makes the mistake silent: "metrics:" written beside "manager:" instead of
// inside it is dropped without a word, and the service starts with metrics off
// while the operator has every reason to think they enabled them. The strict
// check on the manager section cannot see it, because the key is not in there.
//
// The names come from the struct, so a new manager option is covered without
// anyone remembering to add it here.
func rejectMisplacedManagerOptions(root map[string]interface{}) error {
	for key := range root {
		if key == "manager" || !managerOptionNames[key] {
			continue
		}
		return fmt.Errorf("manager option %q is at the top level of the manager config; "+
			"it belongs under \"manager:\" and is otherwise ignored", key)
	}
	return nil
}

// managerOptionNames is every yaml key ManagerSettings accepts, read from the
// struct so the two cannot drift apart.
var managerOptionNames = func() map[string]bool {
	out := map[string]bool{}
	t := reflect.TypeOf(ManagerSettings{})
	for i := range t.NumField() {
		tag, _, _ := strings.Cut(t.Field(i).Tag.Get("yaml"), ",")
		if tag != "" && tag != "-" {
			out[tag] = true
		}
	}
	return out
}()

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

// strictCheckManagerSection strict-decodes the merged manager section into a
// throwaway ManagerSettings so an unknown key (a typo, or a manager option
// nested under the wrong place) fails loudly. A missing section is fine: the
// period validation catches an absent or empty manager block elsewhere.
func strictCheckManagerSection(section interface{}) error {
	if section == nil {
		return nil
	}
	raw, err := yaml.Marshal(section)
	if err != nil {
		return fmt.Errorf("processing manager section: %w", err)
	}
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	dec.KnownFields(true)
	var check ManagerSettings
	if err := dec.Decode(&check); err != nil {
		return fmt.Errorf("invalid manager section (check for a misspelled or misplaced key): %w", err)
	}
	return nil
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

	// Round-trip the resolved map into the typed struct. This decode is lenient
	// so the free-form borgmatic section and top-level YAML anchor holders (a
	// `defaults: &defaults` merged into borgmatic) pass through untouched.
	resolved, err := yaml.Marshal(managerMap)
	if err != nil {
		return nil, nil, fmt.Errorf("processing manager config: %w", err)
	}
	var cfg ManagerConfig
	if parseErr := yaml.Unmarshal(resolved, &cfg); parseErr != nil {
		return nil, nil, fmt.Errorf("parsing manager config: %w", parseErr)
	}

	// The manager section is our own typed config, so strict-decode it: a
	// misspelled or misplaced key (mtrics, xenabled, a manager option nested
	// under the wrong section) is a hard error rather than a silently dropped
	// setting. borgmatic options are validated by borgmatic's own config check.
	if strictErr := strictCheckManagerSection(managerMap["manager"]); strictErr != nil {
		return nil, nil, strictErr
	}
	if misplacedErr := rejectMisplacedManagerOptions(managerMap); misplacedErr != nil {
		return nil, nil, misplacedErr
	}
	if validateErr := cfg.Manager.Metrics.Validate(); validateErr != nil {
		return nil, nil, fmt.Errorf("invalid manager config: %w", validateErr)
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
