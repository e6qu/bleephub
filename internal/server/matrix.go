package bleephub

import (
	"fmt"
	"reflect"
	"sort"
	"strings"
)

// ExpandMatrix produces all combinations from a MatrixDef.
// It computes the Cartesian product of Values, applies includes, then excludes.
func ExpandMatrix(m *MatrixDef) []map[string]interface{} {
	if m == nil || len(m.Values) == 0 {
		// No matrix values — just apply includes if any
		if len(m.Include) > 0 {
			return m.Include
		}
		return nil
	}

	combos := expandCartesian(m.Values, m.Order)
	// Exclude first, then include. GitHub documents this order precisely so
	// that an include entry can add back a combination exclude removed;
	// running them the other way round lets the exclude delete what the
	// include just restored, which is the opposite of what the workflow asked
	// for and silently produces a smaller matrix.
	combos = applyExcludes(combos, m.Exclude)
	combos = applyIncludes(combos, m.Include)
	return combos
}

// expandCartesian computes the Cartesian product of matrix values.
// Keys are sorted for deterministic ordering.
func expandCartesian(values map[string][]interface{}, declaredOrder []string) []map[string]interface{} {
	keys := append([]string(nil), declaredOrder...)
	seen := make(map[string]bool, len(keys))
	for _, key := range keys {
		seen[key] = true
	}
	var remaining []string
	for key := range values {
		if !seen[key] {
			remaining = append(remaining, key)
		}
	}
	sort.Strings(remaining)
	keys = append(keys, remaining...)

	// Start with a single empty combination
	result := []map[string]interface{}{make(map[string]interface{})}

	for _, key := range keys {
		vals := values[key]
		var expanded []map[string]interface{}
		for _, combo := range result {
			for _, val := range vals {
				newCombo := make(map[string]interface{}, len(combo)+1)
				for k, v := range combo {
					newCombo[k] = v
				}
				newCombo[key] = val
				expanded = append(expanded, newCombo)
			}
		}
		result = expanded
	}

	return result
}

// applyIncludes adds include entries to the combination list.
// If an include entry matches an existing combo on shared keys, it extends that combo.
// Otherwise, it's added as a new combination.
func applyIncludes(combos []map[string]interface{}, includes []map[string]interface{}) []map[string]interface{} {
	for _, inc := range includes {
		matched := false
		for _, combo := range combos {
			if matchesSharedKeys(combo, inc) {
				// Extend the combo with extra keys from include
				for k, v := range inc {
					if _, exists := combo[k]; !exists {
						combo[k] = v
					}
				}
				matched = true
			}
		}
		if !matched {
			// Add as a new combination
			newCombo := make(map[string]interface{}, len(inc))
			for k, v := range inc {
				newCombo[k] = v
			}
			combos = append(combos, newCombo)
		}
	}
	return combos
}

// applyExcludes removes combinations that match any exclude entry.
func applyExcludes(combos []map[string]interface{}, excludes []map[string]interface{}) []map[string]interface{} {
	if len(excludes) == 0 {
		return combos
	}

	var result []map[string]interface{}
	for _, combo := range combos {
		excluded := false
		for _, exc := range excludes {
			if matchesAllKeys(combo, exc) {
				excluded = true
				break
			}
		}
		if !excluded {
			result = append(result, combo)
		}
	}
	return result
}

// matchesSharedKeys returns true if all keys present in both maps have equal values.
func matchesSharedKeys(combo, entry map[string]interface{}) bool {
	for k, v := range entry {
		if cv, ok := combo[k]; ok {
			if !matrixValuesEqual(cv, v) {
				return false
			}
		}
	}
	return true
}

// matchesAllKeys returns true if combo contains all key-value pairs from entry.
func matchesAllKeys(combo, entry map[string]interface{}) bool {
	for k, v := range entry {
		cv, ok := combo[k]
		if !ok {
			return false
		}
		if !matrixValuesEqual(cv, v) {
			return false
		}
	}
	return true
}

// MatrixJobName generates a display name like "test (ubuntu, 3.9)".
func MatrixJobName(baseKey string, values map[string]interface{}, declarationOrder ...[]string) string {
	if len(values) == 0 {
		return baseKey
	}

	var keys []string
	if len(declarationOrder) > 0 {
		for _, key := range declarationOrder[0] {
			if _, ok := values[key]; ok {
				keys = append(keys, key)
			}
		}
	}
	seen := make(map[string]bool, len(keys))
	for _, key := range keys {
		seen[key] = true
	}
	var remaining []string
	for key := range values {
		if !seen[key] {
			remaining = append(remaining, key)
		}
	}
	sort.Strings(remaining)
	keys = append(keys, remaining...)

	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%v", values[k]))
	}
	return fmt.Sprintf("%s (%s)", baseKey, strings.Join(parts, ", "))
}

func matrixValuesEqual(left, right interface{}) bool {
	return reflect.DeepEqual(normalizeYAMLValue(left), normalizeYAMLValue(right))
}
