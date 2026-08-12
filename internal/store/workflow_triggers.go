package store

// NormalizeYAMLValue maps YAML-decoded scalars into the expression value
// space (ints become float64 like every other expression number).
func NormalizeYAMLValue(v interface{}) interface{} {
	switch t := v.(type) {
	case int:
		return float64(t)
	case int64:
		return float64(t)
	case uint64:
		return float64(t)
	default:
		return v
	}
}

// WorkflowInputDef is a declared workflow_dispatch / workflow_call input.
type WorkflowInputDef struct {
	Description string        `yaml:"description"`
	Required    bool          `yaml:"required"`
	Default     interface{}   `yaml:"default"`
	Type        string        `yaml:"type"` // string | choice | boolean | number | environment
	Options     []interface{} `yaml:"options"`
}
