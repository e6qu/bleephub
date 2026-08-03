package bleephub

import "testing"

// TestUpdateFieldPreservesKeptOptionIDs is the STORE-043 regression: updating a
// single-select field's options must keep the ID of any option retained by
// name, so items whose stored OptionID points at it don't dangle. Only genuinely
// new options get fresh IDs.
func TestUpdateFieldPreservesKeptOptionIDs(t *testing.T) {
	st := NewStore()
	p := st.ProjectsV2.CreateProject(1, "User", "Board", 1)
	f := st.ProjectsV2.CreateField(p.ID, "Status", ProjectV2FieldSingleSelect,
		[]*ProjectV2SingleSelectOption{{Name: "Todo"}, {Name: "Done"}}, nil)

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
	st.ProjectsV2.UpdateField(f.ID, nil, []*ProjectV2SingleSelectOption{{Name: "Todo"}, {Name: "In Progress"}})

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

// TestExternalURLPrefix is the GQL-042 regression: GraphQL `url` fields go
// through externalURL, which prefixes BLEEPHUB_EXTERNAL_URL when set and stays
// relative otherwise (resourcePath fields never call it).
func TestExternalURLPrefix(t *testing.T) {
	t.Setenv("BLEEPHUB_EXTERNAL_URL", "https://gh.example.com/")
	if got := externalURL("/octo/repo/issues/1"); got != "https://gh.example.com/octo/repo/issues/1" {
		t.Fatalf("externalURL with endpoint = %q", got)
	}

	t.Setenv("BLEEPHUB_EXTERNAL_URL", "")
	if got := externalURL("/octo/repo/issues/1"); got != "/octo/repo/issues/1" {
		t.Fatalf("externalURL without endpoint = %q, want the relative path", got)
	}
}
