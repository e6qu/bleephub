package bleephub

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
)

// TestMilestonePatchAppliesDueOn pins that PATCH .../milestones/{number} applies
// due_on (and a null clears it) — the handler previously read only title,
// description and state, so a due date could never be set or changed after
// creation.
func TestMilestonePatchAppliesDueOn(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	s.seedRepo(t, "ms-repo", false)

	created := decodeJSON(t, s.post(t, "/api/v3/repos/admin/ms-repo/milestones", defaultToken, map[string]interface{}{"title": "v1"}))
	num := int(created["number"].(float64))
	if created["due_on"] != nil {
		t.Fatalf("new milestone due_on = %v, want null", created["due_on"])
	}
	path := fmt.Sprintf("/api/v3/repos/admin/ms-repo/milestones/%d", num)

	got := decodeJSON(t, s.patch(t, path, defaultToken, map[string]interface{}{"due_on": "2027-01-01T00:00:00Z"}))
	due, _ := got["due_on"].(string)
	if !strings.HasPrefix(due, "2027-01-01") {
		t.Fatalf("due_on after PATCH = %v, want 2027-01-01…", got["due_on"])
	}

	// A null due_on clears it.
	got = decodeJSON(t, s.patch(t, path, defaultToken, map[string]interface{}{"due_on": nil}))
	if got["due_on"] != nil {
		t.Fatalf("due_on after null PATCH = %v, want null", got["due_on"])
	}
}

// TestLabelRenameRejectsCollision pins that renaming a label onto a name another
// label already holds is refused (422), the same invariant the create path
// enforces — it previously produced two identically-named labels.
func TestLabelRenameRejectsCollision(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	s.seedRepo(t, "lbl-repo", false)
	labels := "/api/v3/repos/admin/lbl-repo/labels"
	for _, name := range []string{"bug", "wip"} {
		resp := s.post(t, labels, defaultToken, map[string]interface{}{"name": name, "color": "ededed"})
		resp.Body.Close()
	}

	resp := s.patch(t, labels+"/wip", defaultToken, map[string]interface{}{"new_name": "bug"})
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("rename wip->bug = %d, want 422 (duplicate label)", resp.StatusCode)
	}

	// Renaming to a free name still works.
	resp = s.patch(t, labels+"/wip", defaultToken, map[string]interface{}{"new_name": "in-progress"})
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("rename wip->in-progress = %d, want 200", resp.StatusCode)
	}
}

// TestSubscriptionSubscribedAndIgnoredExclusive pins that setting both subscribed
// and ignored yields ignored winning (subscribed cleared), as GitHub makes them
// mutually exclusive.
func TestSubscriptionSubscribedAndIgnoredExclusive(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	s.seedRepo(t, "sub-repo", false)

	got := decodeJSON(t, s.put(t, "/api/v3/repos/admin/sub-repo/subscription", defaultToken, map[string]interface{}{
		"subscribed": true, "ignored": true,
	}))
	if got["ignored"] != true {
		t.Fatalf("ignored = %v, want true", got["ignored"])
	}
	if got["subscribed"] != false {
		t.Fatalf("subscribed = %v, want false (ignored wins)", got["subscribed"])
	}
}
