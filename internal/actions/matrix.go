package actions

import (
	"fmt"
	"reflect"
	"sort"
	"strings"

	"github.com/e6qu/bleephub/internal/store"
)

// ExpandMatrix produces all combinations from a MatrixDef.
// It computes the Cartesian product of Values, applies includes, then excludes.
func ExpandMatrix(m *store.MatrixDef) []map[string]interface{} {
	if m == nil {
		return nil
	}
	if len(m.Values) == 0 {
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
	original := make(map[string]bool, len(m.Values))
	for key := range m.Values {
		original[key] = true
	}
	combos = applyIncludes(combos, m.Include, original)
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

// applyIncludes adds include entries to the combination list, following
// GitHub's documented rule: an include entry is merged into every combination
// of the *original* matrix whose original values it does not overwrite, and is
// otherwise appended as a standalone combination.
//
// Two consequences of "original" are load-bearing and easy to get wrong:
//
//   - Only the Cartesian combinations (post-exclude) are candidates for
//     merging. Combinations a previous include appended are not, so
//     `- fruit: banana` followed by `- {fruit: banana, animal: cat}` yields two
//     combinations, not one merged combination.
//   - Keys a previous include contributed are not original values, so a later
//     include overwrites them. `- color: green` followed by
//     `- {color: pink, animal: cat}` leaves the cat rows pink, not green.
func applyIncludes(combos, includes []map[string]interface{}, original map[string]bool) []map[string]interface{} {
	// Candidate count is frozen before any include appends: indices below it
	// keep addressing the same maps even when append reallocates the slice.
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

// matchesOriginalKeys reports whether an include entry can be merged into a
// combination: every key the entry shares with the combination's *original*
// matrix dimensions must carry the same value. Keys an earlier include added
// are not original values, so they neither block the merge nor survive it.
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
	return reflect.DeepEqual(store.NormalizeYAMLValue(left), store.NormalizeYAMLValue(right))
}
