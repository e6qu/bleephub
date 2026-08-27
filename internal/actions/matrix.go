package actions

import (
	"fmt"
	"reflect"
	"sort"
	"strings"

	"github.com/e6qu/bleephub/internal/store"
)

// ExpandMatrix computes the Cartesian product of Values, then applies excludes
// and includes.
func ExpandMatrix(m *store.MatrixDef) []map[string]interface{} {
	if m == nil {
		return nil
	}
	if len(m.Values) == 0 {
		if len(m.Include) > 0 {
			return m.Include
		}
		return nil
	}

	combos := expandCartesian(m.Values, m.Order)
	// Exclude before include: GitHub's documented order lets an include add
	// back a combination an exclude removed.
	combos = applyExcludes(combos, m.Exclude)
	original := make(map[string]bool, len(m.Values))
	for key := range m.Values {
		original[key] = true
	}
	combos = applyIncludes(combos, m.Include, original)
	return combos
}

// expandCartesian computes the Cartesian product of matrix values, with
// undeclared keys sorted for deterministic ordering.
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

// applyIncludes merges each include entry into every original-matrix
// combination whose original values it does not overwrite, else appends it
// standalone. "Original" is load-bearing: only post-exclude Cartesian
// combinations are merge candidates, and keys a prior include added count as
// non-original, so a later include can overwrite them.
func applyIncludes(combos, includes []map[string]interface{}, original map[string]bool) []map[string]interface{} {
	// Freeze the candidate count before any append so indices keep addressing
	// the original maps after a reallocation.
	candidates := len(combos)
	for _, inc := range includes {
		matched := false
		for i := 0; i < candidates; i++ {
			combo := combos[i]
			if !matchesOriginalKeys(combo, inc, original) {
				continue
			}
			for k, v := range inc {
				combo[k] = v
			}
			matched = true
		}
		if !matched {
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

// matchesOriginalKeys reports whether an include entry can merge into a
// combination: every key it shares with the combination's original matrix
// dimensions must carry the same value. Keys an earlier include added don't
// count as original.
func matchesOriginalKeys(combo, entry map[string]interface{}, original map[string]bool) bool {
	for k, v := range entry {
		if !original[k] {
			continue
		}
		if cv, ok := combo[k]; ok {
			if !matrixValuesEqual(cv, v) {
				return false
			}
		}
	}
	return true
}

// matchesAllKeys reports whether combo contains all key-value pairs from entry.
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

// MatrixJobName builds a display name like "test (ubuntu, 3.9)".
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
	return reflect.DeepEqual(store.NormalizeYAMLValue(left), store.NormalizeYAMLValue(right))
}
