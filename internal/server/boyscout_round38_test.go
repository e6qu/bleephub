package bleephub

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/e6qu/bleephub/internal/store"
)

// TestCollaboratorMaintainAndTriagePreserved pins that maintain/triage
// collaborator permissions are stored and enforced distinctly — they were
// collapsed to "pull", silently under-granting a maintainer to read-only.
func TestCollaboratorMaintainAndTriagePreserved(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	st := s.store
	name := s.createRepoWriteRepo(t, true)
	repo := st.GetRepo("admin", name)
	if repo == nil {
		t.Fatal("repo missing")
	}
	maint, _ := s.userSurfaceUser(t, "maintainer")
	triager, _ := s.userSurfaceUser(t, "triager")

	if !st.AddRepoCollaborator("admin", name, maint.Login, "maintain") {
		t.Fatal("add maintain collaborator failed")
	}
	if !st.AddRepoCollaborator("admin", name, triager.Login, "triage") {
		t.Fatal("add triage collaborator failed")
	}

	if got := st.GetRepoCollaboratorPermission("admin", name, maint.Login); got != "maintain" {
		t.Fatalf("stored maintain permission = %q, want maintain (not collapsed)", got)
	}
	if got := st.GetRepoCollaboratorPermission("admin", name, triager.Login); got != "triage" {
		t.Fatalf("stored triage permission = %q, want triage", got)
	}

	// Access mapping: maintain confers write, triage confers read but NOT write.
	st.Mu.RLock()
	maintWrite := store.RepoCollaboratorPermissionAtLeastLocked(st, repo.FullName, maint.Login, "write")
	triageRead := store.RepoCollaboratorPermissionAtLeastLocked(st, repo.FullName, triager.Login, "read")
	triageWrite := store.RepoCollaboratorPermissionAtLeastLocked(st, repo.FullName, triager.Login, "write")
	st.Mu.RUnlock()
	if !maintWrite {
		t.Fatal("maintain collaborator should have write access")
	}
	if !triageRead {
		t.Fatal("triage collaborator should have read access")
	}
	if triageWrite {
		t.Fatal("triage collaborator must NOT have write access")
	}
}

// TestCollaboratorPermissionsObjectHasAllKeys pins that the collaborator
// permissions object carries all five booleans (admin, maintain, push, triage,
// pull), cumulative by rank — triage and maintain keys were missing.
func TestCollaboratorPermissionsObjectHasAllKeys(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	st := s.store
	name := s.createRepoWriteRepo(t, true)
	maint, _ := s.userSurfaceUser(t, "perm-maint")
	if !st.AddRepoCollaborator("admin", name, maint.Login, "maintain") {
		t.Fatal("add maintain collaborator failed")
	}

	resp := s.get(t, "/api/v3/repos/admin/"+name+"/collaborators", defaultToken)
	requireStatusNoClose(t, resp, 200)
	var list []map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&list); err != nil {
		t.Fatalf("decode: %v", err)
	}
	resp.Body.Close()

	var perms map[string]interface{}
	for _, c := range list {
		if c["login"] == maint.Login {
			perms, _ = c["permissions"].(map[string]interface{})
			if c["role_name"] != "maintain" {
				t.Fatalf("maintain collaborator role_name = %v, want maintain", c["role_name"])
			}
		}
	}
	if perms == nil {
		t.Fatalf("maintain collaborator not listed: %v", list)
	}
	for _, k := range []string{"admin", "maintain", "push", "triage", "pull"} {
		if _, ok := perms[k]; !ok {
			t.Fatalf("permissions object missing key %q: %v", k, perms)
		}
	}
	// A maintain user is push+triage+pull+maintain, but not admin.
	if perms["maintain"] != true || perms["push"] != true || perms["triage"] != true || perms["pull"] != true {
		t.Fatalf("maintain permissions not cumulative: %v", perms)
	}
	if perms["admin"] != false {
		t.Fatalf("maintain must not have admin: %v", perms)
	}
}

// TestAddCollaboratorRejectsInvalidPermission pins a 422 for an unrecognized
// permission (was silently downgraded to pull).
func TestAddCollaboratorRejectsInvalidPermission(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	name := s.createRepoWriteRepo(t, true)
	invitee, _ := s.userSurfaceUser(t, "invalid-perm-invitee")

	resp := s.put(t, "/api/v3/repos/admin/"+name+"/collaborators/"+invitee.Login, defaultToken,
		map[string]interface{}{"permission": "superadmin"})
	requireStatus(t, resp, http.StatusUnprocessableEntity)
}

// TestGetCollaboratorPermissionNoneForNonCollaborator pins that a real user with
// no access returns 200 permission:"none", not 404 (only a missing user 404s).
func TestGetCollaboratorPermissionNoneForNonCollaborator(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	name := s.createRepoWriteRepo(t, true)
	stranger, _ := s.userSurfaceUser(t, "no-access-user")

	resp := s.get(t, "/api/v3/repos/admin/"+name+"/collaborators/"+stranger.Login+"/permission", defaultToken)
	requireStatusNoClose(t, resp, 200)
	body := decodeJSON(t, resp)
	if body["permission"] != "none" {
		t.Fatalf("permission for a non-collaborator = %v, want none", body["permission"])
	}

	// A user that does not exist still 404s.
	resp = s.get(t, "/api/v3/repos/admin/"+name+"/collaborators/nosuchuser12345/permission", defaultToken)
	requireStatus(t, resp, http.StatusNotFound)
}

// TestDeleteRepoSubscriptionIdempotent pins that un-watching a repo you were not
// watching returns 204, not 404.
func TestDeleteRepoSubscriptionIdempotent(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	name := s.createRepoWriteRepo(t, false)

	resp := s.delete(t, "/api/v3/repos/admin/"+name+"/subscription", defaultToken)
	requireStatus(t, resp, http.StatusNoContent)
}

// TestCollaboratorListRequiresPushForUsers pins that a user acting in their own
// capacity must have push access to list collaborators / read a user's
// permission — a read-only collaborator is 403, a pusher is 200.
func TestCollaboratorListRequiresPushForUsers(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	st := s.store
	name := s.createRepoWriteRepo(t, true)
	reader, readerTok := s.userSurfaceUser(t, "collab-reader")
	pusher, pusherTok := s.userSurfaceUser(t, "collab-pusher")
	if !st.AddRepoCollaborator("admin", name, reader.Login, "pull") {
		t.Fatal("add pull collaborator failed")
	}
	if !st.AddRepoCollaborator("admin", name, pusher.Login, "push") {
		t.Fatal("add push collaborator failed")
	}

	// Read-only collaborator can read the repo (so not 404) but lacks push → 403.
	requireStatus(t, s.get(t, "/api/v3/repos/admin/"+name+"/collaborators", readerTok), http.StatusForbidden)
	requireStatus(t, s.get(t, "/api/v3/repos/admin/"+name+"/collaborators/"+pusher.Login+"/permission", readerTok), http.StatusForbidden)

	// A collaborator with push access can list.
	requireStatus(t, s.get(t, "/api/v3/repos/admin/"+name+"/collaborators", pusherTok), http.StatusOK)
}

// TestCollaboratorListAffiliationAndPermissionFilters pins the affiliation
// (outside) and permission query filters on the collaborator list.
func TestCollaboratorListAffiliationAndPermissionFilters(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	st := s.store
	admin := st.LookupUserByLogin("admin")
	org := st.CreateOrg(admin, "collab-filter-org", "", "")
	repo := st.CreateOrgRepo(org, admin, "filtered", "", true)
	if repo == nil {
		t.Fatal("org repo missing")
	}
	member, _ := s.userSurfaceUser(t, "collab-org-member")
	outsider, _ := s.userSurfaceUser(t, "collab-outsider")
	triager, _ := s.userSurfaceUser(t, "collab-triager")
	st.SetMembership(org.Login, member.ID, store.OrgRoleMember, store.MembershipStateActive)
	for _, u := range []*store.User{member, outsider} {
		if !st.AddRepoCollaborator(org.Login, repo.Name, u.Login, "push") {
			t.Fatalf("add collaborator %s failed", u.Login)
		}
	}
	if !st.AddRepoCollaborator(org.Login, repo.Name, triager.Login, "triage") {
		t.Fatal("add triage collaborator failed")
	}

	logins := func(query string) map[string]bool {
		resp := s.get(t, "/api/v3/repos/"+repo.FullName+"/collaborators"+query, defaultToken)
		requireStatusNoClose(t, resp, 200)
		var list []map[string]interface{}
		if err := json.NewDecoder(resp.Body).Decode(&list); err != nil {
			t.Fatalf("decode: %v", err)
		}
		resp.Body.Close()
		out := map[string]bool{}
		for _, c := range list {
			out[c["login"].(string)] = true
		}
		return out
	}

	// affiliation=outside: only the collaborator who is not an org member (and not the owner).
	outside := logins("?affiliation=outside")
	if !outside[outsider.Login] {
		t.Fatalf("affiliation=outside missing the outside collaborator: %v", outside)
	}
	if outside[member.Login] {
		t.Fatalf("affiliation=outside leaked an org member: %v", outside)
	}
	if outside[org.Login] {
		t.Fatalf("affiliation=outside included the owning org: %v", outside)
	}

	// permission=push: excludes the triage collaborator; keeps push collaborators.
	push := logins("?permission=push")
	if !push[outsider.Login] || !push[member.Login] {
		t.Fatalf("permission=push dropped a push collaborator: %v", push)
	}
	if push[triager.Login] {
		t.Fatalf("permission=push kept a triage collaborator: %v", push)
	}
}
