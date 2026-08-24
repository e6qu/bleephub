package actions

import (
	"reflect"
	"testing"

	"github.com/e6qu/bleephub/internal/store"
)

// TestExpandMatrixDocumentedIncludeExample reproduces the exact example from
// GitHub's "Using a matrix for your jobs" documentation, which is the
// authoritative statement of include semantics.
func TestExpandMatrixDocumentedIncludeExample(t *testing.T) {
	m := &store.MatrixDef{
		Order:  []string{"fruit", "animal"},
		Values: map[string][]interface{}{"fruit": {"apple", "pear"}, "animal": {"cat", "dog"}},
		Include: []map[string]interface{}{
			{"color": "green"},
			{"color": "pink", "animal": "cat"},
			{"fruit": "apple", "shape": "circle"},
			{"fruit": "banana"},
			{"fruit": "banana", "animal": "cat"},
		},
	}
	got := ExpandMatrix(m)
	want := []map[string]interface{}{
		{"fruit": "apple", "animal": "cat", "color": "pink", "shape": "circle"},
		{"fruit": "apple", "animal": "dog", "color": "green", "shape": "circle"},
		{"fruit": "pear", "animal": "cat", "color": "pink"},
		{"fruit": "pear", "animal": "dog", "color": "green"},
		{"fruit": "banana"},
		{"fruit": "banana", "animal": "cat"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("matrix expansion mismatch\n got: %#v\nwant: %#v", got, want)
	}
}

func TestExpandMatrixNilDefinition(t *testing.T) {
	if combos := ExpandMatrix(nil); combos != nil {
		t.Fatalf("ExpandMatrix(nil) = %#v, want nil", combos)
	}
}
