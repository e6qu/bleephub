package bleephub

import (
	"testing"

	"github.com/e6qu/bleephub/internal/store"
)

// TestUpdateFieldPreservesKeptOptionIDs is the STORE-043 regression: updating a
// single-select field's options must keep the ID of any option retained by
// name, so items whose stored OptionID points at it don't dangle. Only genuinely
// new options get fresh IDs.
func TestUpdateFieldPreservesKeptOptionIDs(t *testing.T) {
	st := store.NewStore()
	p := st.ProjectsV2.CreateProject(1, "User", "Board", 1)
	f := st.ProjectsV2.CreateField(p.ID, "Status", store.ProjectV2FieldSingleSelect,
		[]*store.ProjectV2SingleSelectOption{{Name: "Todo"}, {Name: "Done"}}, nil)

	var todoID string
	for _, o := range f.Options {
		if o.Name == "Todo" {
			todoID = o.ID
		}
	}
	if todoID == "" {
		t.Fatal("Todo option was not created")
	}

	// Keep Todo, drop Done, add In Progress.
	st.ProjectsV2.UpdateField(f.ID, nil, []*store.ProjectV2SingleSelectOption{{Name: "Todo"}, {Name: "In Progress"}})

	updated := st.ProjectsV2.GetField(f.ID)
	var gotTodoID, inProgID string
	for _, o := range updated.Options {
		switch o.Name {
		case "Todo":
			gotTodoID = o.ID
		case "In Progress":
			inProgID = o.ID
		}
	}
	if gotTodoID != todoID {
		t.Fatalf("Todo option ID changed on update (dangles item values): was %q, now %q", todoID, gotTodoID)
	}
	if inProgID == "" || inProgID == todoID {
		t.Fatalf("new option did not get a fresh ID: %q", inProgID)
	}
	if len(updated.Options) != 2 {
		t.Fatalf("expected 2 options after update, got %d", len(updated.Options))
	}
}
