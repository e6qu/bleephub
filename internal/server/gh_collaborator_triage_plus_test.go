package bleephub

import (
	"net/http"
	"sort"
	"strings"
	"testing"

	"github.com/e6qu/bleephub/internal/store"
)

// TestTriagePlusIsARungOfItsOwn covers the permission GitHub added between
// triage and push. It must be stored as itself rather than collapsed to pull,
// confer read and not write, and take its own place in the `permission` filter:
// above triage, below push.
func TestTriagePlusIsARungOfItsOwn(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	st := s.store
	name := s.createRepoWriteRepo(t, true)
	repo := st.GetRepo("admin", name)
	for _, rung := range []string{"triage", "triage_plus", "push"} {
		user, _ := s.userSurfaceUser(t, "rung-"+rung)
		// The API accepts the rung (it answers with an invitation)…
		expectStatus(t, s.put(t, "/api/v3/repos/admin/"+name+"/collaborators/"+user.Login, defaultToken,
			map[string]string{"permission": rung}), http.StatusCreated, "invite a "+rung+" collaborator")
		// …and the collaborator it would become holds it.
		if !st.AddRepoCollaborator("admin", name, user.Login, rung) {
			t.Fatalf("add %s collaborator", rung)
		}
	}

	if got := st.GetRepoCollaboratorPermission("admin", name, "rung-triage_plus"); got != "triage_plus" {
		t.Fatalf("stored permission = %q, want triage_plus kept as itself", got)
	}
	st.Mu.RLock()
	reads := store.RepoCollaboratorPermissionAtLeastLocked(st, repo.FullName, "rung-triage_plus", "read")
	writes := store.RepoCollaboratorPermissionAtLeastLocked(st, repo.FullName, "rung-triage_plus", "write")
	st.Mu.RUnlock()
	if !reads || writes {
		t.Fatalf("triage_plus reads=%v writes=%v, want read without write", reads, writes)
	}

	permission := decodeBody(t, s.get(t, "/api/v3/repos/admin/"+name+"/collaborators/rung-triage_plus/permission", defaultToken), http.StatusOK)
	if permission["role_name"] != "triage_plus" {
		t.Errorf("role_name = %v, want triage_plus", permission["role_name"])
	}
	for _, row := range decodeJSONArray(t, s.get(t, "/api/v3/repos/admin/"+name+"/collaborators?affiliation=direct", defaultToken)) {
		if row["login"] != "rung-triage_plus" {
			continue
		}
		flags := innerObject(t, row, "permissions")
		if flags["triage"] != true || flags["pull"] != true || flags["push"] != false {
			t.Errorf("permissions = %v, want triage and pull without push", flags)
		}
	}

	listed := func(filter string) []string {
		t.Helper()
		var logins []string
		for _, row := range decodeJSONArray(t, s.get(t, "/api/v3/repos/admin/"+name+"/collaborators?affiliation=direct&permission="+filter, defaultToken)) {
			logins = append(logins, row["login"].(string))
		}
		sort.Strings(logins)
		return logins
	}
	// The owner holds admin, so is above every rung asked for.
	if got := strings.Join(listed("triage_plus"), ","); got != "admin,rung-push,rung-triage_plus" {
		t.Errorf("permission=triage_plus lists %s, want everyone at triage_plus or above and not triage", got)
	}
	if got := strings.Join(listed("triage"), ","); got != "admin,rung-push,rung-triage,rung-triage_plus" {
		t.Errorf("permission=triage lists %s, want every rung from triage up", got)
	}
	if got := strings.Join(listed("push"), ","); got != "admin,rung-push" {
		t.Errorf("permission=push lists %s, want triage_plus left below push", got)
	}
}
