package config

import (
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

// maxIncludeDepth bounds nested includes; hitting it means a cycle.
const maxIncludeDepth = 10

// loadYAMLWithIncludes reads a YAML file supporting borgmatic's !include tag.
// Semantics match borgmatic's loader: relative paths resolve against the
// including file, "<<" deep-merges with local keys winning. borgmatic's
// !retain/!omit tags are not supported and produce a clear error.
func loadYAMLWithIncludes(path string) (map[string]interface{}, error) {
	value, err := loadIncludedValue(path, 0)
	if err != nil {
		return nil, err
	}
	if value == nil {
		return map[string]interface{}{}, nil
	}
	m, ok := value.(map[string]interface{})
	if !ok {
		return nil, fmt.Errorf("%s: top level must be a mapping", path)
	}
	return m, nil
}

func loadIncludedValue(path string, depth int) (interface{}, error) {
	if depth > maxIncludeDepth {
		return nil, fmt.Errorf("%s: includes nested more than %d levels deep (include cycle?)", path, maxIncludeDepth)
	}

	data, err := os.ReadFile(path) // #nosec G304 -- operator-referenced include path
	if err != nil {
		return nil, fmt.Errorf("reading include %s: %w", path, err)
	}

	var root yaml.Node
	if err := yaml.Unmarshal(data, &root); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", path, err)
	}
	if root.Kind == 0 || len(root.Content) == 0 {
		return nil, nil // empty file
	}

	return resolveNode(root.Content[0], filepath.Dir(path), depth)
}

// resolveNode converts a YAML node into a Go value, resolving !include tags
// and applying "<<" merge keys with deep-merge, local-wins semantics.
func resolveNode(node *yaml.Node, baseDir string, depth int) (interface{}, error) {
	switch node.Tag {
	case "!include":
		if node.Kind != yaml.ScalarNode || node.Value == "" {
			return nil, fmt.Errorf("line %d: !include takes a file path", node.Line)
		}
		includePath := node.Value
		if !filepath.IsAbs(includePath) {
			includePath = filepath.Join(baseDir, includePath)
		}
		return loadIncludedValue(includePath, depth+1)

	case "!retain", "!omit":
		return nil, fmt.Errorf("line %d: borgmatic's %s merge tag is not supported by borgmatic-manager; use groups/*.yaml or config labels for overrides", node.Line, node.Tag)
	}

	switch node.Kind {
	case yaml.AliasNode:
		return resolveNode(node.Alias, baseDir, depth)

	case yaml.SequenceNode:
		out := make([]interface{}, 0, len(node.Content))
		for _, child := range node.Content {
			v, err := resolveNode(child, baseDir, depth)
			if err != nil {
				return nil, err
			}
			out = append(out, v)
		}
		return out, nil

	case yaml.MappingNode:
		local := make(map[string]interface{})
		merged := make(map[string]interface{})
		for i := 0; i+1 < len(node.Content); i += 2 {
			keyNode, valueNode := node.Content[i], node.Content[i+1]

			if keyNode.Value == "<<" && keyNode.Tag != "!!str" {
				sources, err := mergeSources(valueNode, baseDir, depth)
				if err != nil {
					return nil, err
				}
				for _, src := range sources {
					merged = DeepMerge(merged, src)
				}
				continue
			}

			v, err := resolveNode(valueNode, baseDir, depth)
			if err != nil {
				return nil, err
			}
			local[keyNode.Value] = v
		}
		if len(merged) == 0 {
			return local, nil
		}
		// Including mapping's own keys win (borgmatic's include semantics).
		return DeepMerge(merged, local), nil

	default: // scalars
		var v interface{}
		if err := node.Decode(&v); err != nil {
			return nil, err
		}
		return v, nil
	}
}

// mergeSources resolves a "<<" merge value into one or more mappings
// (a single include/mapping/alias, or a sequence of them).
func mergeSources(node *yaml.Node, baseDir string, depth int) ([]map[string]interface{}, error) {
	values := []*yaml.Node{node}
	if node.Kind == yaml.SequenceNode && node.Tag != "!include" {
		values = node.Content
	}

	var out []map[string]interface{}
	for _, v := range values {
		resolved, err := resolveNode(v, baseDir, depth)
		if err != nil {
			return nil, err
		}
		if resolved == nil {
			continue // empty include merges nothing
		}
		m, ok := resolved.(map[string]interface{})
		if !ok {
			return nil, fmt.Errorf("line %d: '<<' merge value must be a mapping", v.Line)
		}
		out = append(out, m)
	}
	return out, nil
}
