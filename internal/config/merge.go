package config

// DeepMerge returns a new map merging src over dst: maps merge recursively,
// everything else (including slices) is replaced by src. Neither input is mutated.
func DeepMerge(dst, src map[string]interface{}) map[string]interface{} {
	result := make(map[string]interface{})

	for k, v := range dst {
		result[k] = v
	}

	for k, srcVal := range src {
		dstVal, exists := result[k]
		if !exists {
			result[k] = srcVal
			continue
		}

		dstMap, dstOK := dstVal.(map[string]interface{})
		srcMap, srcOK := srcVal.(map[string]interface{})
		if dstOK && srcOK {
			result[k] = DeepMerge(dstMap, srcMap)
		} else {
			result[k] = srcVal
		}
	}

	return result
}
