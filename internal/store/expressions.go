package store

import (
	"fmt"
	"math"
	"strconv"
)

// ExprToString renders a value as GitHub interpolates it into strings:
// null→"", bools→true/false, numbers in shortest form, arrays/objects as the
// literal words "Array"/"Object".
func ExprToString(v interface{}) string {
	switch t := v.(type) {
	case nil:
		return ""
	case bool:
		if t {
			return "true"
		}
		return "false"
	case float64:
		if math.IsNaN(t) {
			return "NaN"
		}
		if math.IsInf(t, 1) {
			return "Infinity"
		}
		if math.IsInf(t, -1) {
			return "-Infinity"
		}
		return strconv.FormatFloat(t, 'f', -1, 64)
	case string:
		return t
	case []interface{}:
		return "Array"
	case map[string]interface{}:
		return "Object"
	default:
		return fmt.Sprintf("%v", t)
	}
}
