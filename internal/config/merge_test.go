package config_test

import (
	"testing"

	"github.com/lugoues/borgmatic-manager/internal/config"
	"github.com/stretchr/testify/assert"
)

func TestDeepMerge(t *testing.T) {
	tests := []struct {
		name     string
		dst      map[string]interface{}
		src      map[string]interface{}
		expected map[string]interface{}
	}{
		{
			name:     "nil dst and nil src returns empty map",
			dst:      nil,
			src:      nil,
			expected: map[string]interface{}{},
		},
		{
			name: "nil dst returns src",
			dst:  nil,
			src:  map[string]interface{}{"key": "value"},
			expected: map[string]interface{}{
				"key": "value",
			},
		},
		{
			name: "nil src returns dst",
			dst:  map[string]interface{}{"key": "value"},
			src:  nil,
			expected: map[string]interface{}{
				"key": "value",
			},
		},
		{
			name: "flat keys: src overwrites dst for same keys",
			dst:  map[string]interface{}{"a": "old", "b": "keep"},
			src:  map[string]interface{}{"a": "new", "c": "added"},
			expected: map[string]interface{}{
				"a": "new",
				"b": "keep",
				"c": "added",
			},
		},
		{
			name: "nested maps: recursively merges",
			dst: map[string]interface{}{
				"nested": map[string]interface{}{
					"a": 1,
					"b": 2,
				},
			},
			src: map[string]interface{}{
				"nested": map[string]interface{}{
					"b": 3,
					"c": 4,
				},
			},
			expected: map[string]interface{}{
				"nested": map[string]interface{}{
					"a": 1,
					"b": 3,
					"c": 4,
				},
			},
		},
		{
			name: "lists: src list replaces dst list",
			dst: map[string]interface{}{
				"items": []interface{}{"a", "b"},
			},
			src: map[string]interface{}{
				"items": []interface{}{"x"},
			},
			expected: map[string]interface{}{
				"items": []interface{}{"x"},
			},
		},
		{
			name: "mixed types: src scalar replaces dst map",
			dst: map[string]interface{}{
				"key": map[string]interface{}{"nested": "value"},
			},
			src: map[string]interface{}{
				"key": "scalar",
			},
			expected: map[string]interface{}{
				"key": "scalar",
			},
		},
		{
			name: "deeply nested maps (3+ levels) merge correctly",
			dst: map[string]interface{}{
				"l1": map[string]interface{}{
					"l2": map[string]interface{}{
						"l3": map[string]interface{}{
							"a": "dst",
							"b": "keep",
						},
					},
				},
			},
			src: map[string]interface{}{
				"l1": map[string]interface{}{
					"l2": map[string]interface{}{
						"l3": map[string]interface{}{
							"a": "src",
							"c": "new",
						},
					},
				},
			},
			expected: map[string]interface{}{
				"l1": map[string]interface{}{
					"l2": map[string]interface{}{
						"l3": map[string]interface{}{
							"a": "src",
							"b": "keep",
							"c": "new",
						},
					},
				},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := config.DeepMerge(tt.dst, tt.src)
			assert.Equal(t, tt.expected, result)
		})
	}
}

func TestDeepMerge_DoesNotMutateDstOrSrc(t *testing.T) {
	dst := map[string]interface{}{
		"a": "original",
		"nested": map[string]interface{}{
			"x": 1,
		},
	}
	src := map[string]interface{}{
		"a": "changed",
		"nested": map[string]interface{}{
			"y": 2,
		},
	}

	// Take copies of original values for comparison
	dstOriginalA := dst["a"]
	srcOriginalA := src["a"]

	result := config.DeepMerge(dst, src)

	// Result should be correct
	assert.Equal(t, "changed", result["a"])

	// dst should not be mutated
	assert.Equal(t, dstOriginalA, dst["a"])
	assert.Equal(t, map[string]interface{}{"x": 1}, dst["nested"])

	// src should not be mutated
	assert.Equal(t, srcOriginalA, src["a"])
	assert.Equal(t, map[string]interface{}{"y": 2}, src["nested"])
}
