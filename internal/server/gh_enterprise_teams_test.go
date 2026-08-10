package bleephub

import (
	"net/http"
	"testing"
)

const enterpriseAPI = "/api/v3/enterprises/bleephub"

// createEnterpriseTestOrg provisions an organization owned by the seeded
// admin through the GitHub Enterprise Server admin org-creation endpoint.
func (s *isolatedServer) createEnterpriseTestOrg(t *testing.T, login string) {
	t.Helper()
	resp := s.post(t, "/api/v3/admin/organizations", defaultToken, map[string]interface{}{
		"login": login,
		"admin": "admin",
	})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create org %s: got %d, want 201", login, resp.StatusCode)
	}
}

func TestEnterpriseTeams_CreateGetUpdateDelete(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	// Create.
	resp := s.post(t, enterpriseAPI+"/teams", defaultToken, map[string]interface{}{
		"name":        "Justice League",
		"description": "A great team.",
	})
	if resp.StatusCode != http.StatusCreated {
		resp.Body.Close()
		t.Fatalf("create: got %d, want 201", resp.StatusCode)
	}
	team := decodeJSON(t, resp)
	if team["slug"] != "justice-league" {
		t.Fatalf("slug = %v, want justice-league", team["slug"])
	}
	if team["organization_selection_type"] != "disabled" {
		t.Fatalf("organization_selection_type = %v, want disabled (create default)", team["organization_selection_type"])
	}
	if _, ok := team["group_id"]; !ok {
		t.Fatal("group_id member missing (required by the enterprise-team schema)")
	}
	if team["url"] == nil || team["html_url"] == nil || team["members_url"] == nil {
		t.Fatalf("missing url members: %v", team)
	}

	// Duplicate name → 422.
	resp = s.post(t, enterpriseAPI+"/teams", defaultToken, map[string]interface{}{"name": "Justice League"})
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("duplicate create: got %d, want 422", resp.StatusCode)
	}

	// Get by slug.
	resp = s.get(t, enterpriseAPI+"/teams/justice-league", defaultToken)
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		t.Fatalf("get: got %d, want 200", resp.StatusCode)
	}
	got := decodeJSON(t, resp)
	if got["name"] != "Justice League" {
		t.Fatalf("get name = %v", got["name"])
	}

	// List contains it.
	resp = s.get(t, enterpriseAPI+"/teams", defaultToken)
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		t.Fatalf("list: got %d, want 200", resp.StatusCode)
	}
	found := false
	for _, item := range decodeJSONArray(t, resp) {
		if item["slug"] == "justice-league" {
			found = true
		}
	}
	if !found {
		t.Fatal("list does not contain the created team")
	}

	// Update: rename re-slugs, selection type changes.
	resp = s.patch(t, enterpriseAPI+"/teams/justice-league", defaultToken, map[string]interface{}{
		"name":                        "Justice Society",
		"organization_selection_type": "selected",
	})
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		t.Fatalf("patch: got %d, want 200", resp.StatusCode)
	}
	updated := decodeJSON(t, resp)
	if updated["slug"] != "justice-society" {
		t.Fatalf("patched slug = %v, want justice-society (rename re-slugs)", updated["slug"])
	}
	if updated["organization_selection_type"] != "selected" {
		t.Fatalf("patched organization_selection_type = %v, want selected", updated["organization_selection_type"])
	}

	// Old slug is gone, new slug resolves.
	resp = s.get(t, enterpriseAPI+"/teams/justice-league", defaultToken)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("old slug after rename: got %d, want 404", resp.StatusCode)
	}

	// Delete.
	resp = s.delete(t, enterpriseAPI+"/teams/justice-society", defaultToken)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("delete: got %d, want 204", resp.StatusCode)
	}
	resp = s.get(t, enterpriseAPI+"/teams/justice-society", defaultToken)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("get after delete: got %d, want 404", resp.StatusCode)
	}
}

func TestEnterpriseTeams_AuthAndUnknownEnterprise(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	// Unknown enterprise slug → 404.
	resp := s.get(t, "/api/v3/enterprises/not-this-one/teams", defaultToken)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown enterprise: got %d, want 404", resp.StatusCode)
	}

	// Unauthenticated → 401.
	resp = s.get(t, enterpriseAPI+"/teams", "")
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthenticated list: got %d, want 401", resp.StatusCode)
	}

	// Non-owner create → 403; missing name → 422.
	memberTok := s.createEnterpriseTestUser(t, "ent-member")
	resp = s.post(t, enterpriseAPI+"/teams", memberTok, map[string]interface{}{"name": "Nope"})
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("non-owner create: got %d, want 403", resp.StatusCode)
	}
	resp = s.post(t, enterpriseAPI+"/teams", defaultToken, map[string]interface{}{"description": "no name"})
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("missing name: got %d, want 422", resp.StatusCode)
	}

	// A plain member can read the team list.
	resp = s.get(t, enterpriseAPI+"/teams", memberTok)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("member list: got %d, want 200", resp.StatusCode)
	}
}

func TestEnterpriseTeamMemberships_Flow(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	resp := s.post(t, enterpriseAPI+"/teams", defaultToken, map[string]interface{}{"name": "Membership Crew"})
	if resp.StatusCode != http.StatusCreated {
		resp.Body.Close()
		t.Fatalf("create team: got %d", resp.StatusCode)
	}
	resp.Body.Close()

	tokA := s.createEnterpriseTestUser(t, "ent-mem-a")
	_ = s.createEnterpriseTestUser(t, "ent-mem-b")

	base := enterpriseAPI + "/teams/membership-crew/memberships"

	// PUT single membership → 201 simple-user.
	resp = s.put(t, base+"/ent-mem-a", defaultToken, nil)
	if resp.StatusCode != http.StatusCreated {
		resp.Body.Close()
		t.Fatalf("put membership: got %d, want 201", resp.StatusCode)
	}
	added := decodeJSON(t, resp)
	if added["login"] != "ent-mem-a" {
		t.Fatalf("put membership login = %v", added["login"])
	}

	// Unknown user → 404.
	resp = s.put(t, base+"/no-such-user", defaultToken, nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("put unknown user: got %d, want 404", resp.StatusCode)
	}

	// Non-owner cannot add → 403.
	resp = s.put(t, base+"/ent-mem-b", tokA, nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("non-owner put: got %d, want 403", resp.StatusCode)
	}

	// Bulk add → 200 array of the added users.
	resp = s.post(t, base+"/add", defaultToken, map[string]interface{}{
		"usernames": []string{"ent-mem-b"},
	})
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		t.Fatalf("bulk add: got %d, want 200", resp.StatusCode)
	}
	bulk := decodeJSONArray(t, resp)
	if len(bulk) != 1 || bulk[0]["login"] != "ent-mem-b" {
		t.Fatalf("bulk add response = %v", bulk)
	}

	// GET single membership.
	resp = s.get(t, base+"/ent-mem-a", defaultToken)
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		t.Fatalf("get membership: got %d, want 200", resp.StatusCode)
	}
	resp.Body.Close()

	// List has both, sorted by user ID.
	resp = s.get(t, base, defaultToken)
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		t.Fatalf("list memberships: got %d, want 200", resp.StatusCode)
	}
	members := decodeJSONArray(t, resp)
	if len(members) != 2 {
		t.Fatalf("membership count = %d, want 2", len(members))
	}

	// Bulk remove → 200 array of removed users.
	resp = s.post(t, base+"/remove", defaultToken, map[string]interface{}{
		"usernames": []string{"ent-mem-b"},
	})
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		t.Fatalf("bulk remove: got %d, want 200", resp.StatusCode)
	}
	removed := decodeJSONArray(t, resp)
	if len(removed) != 1 || removed[0]["login"] != "ent-mem-b" {
		t.Fatalf("bulk remove response = %v", removed)
	}

	// DELETE single membership → 204, then GET → 404.
	resp = s.delete(t, base+"/ent-mem-a", defaultToken)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("delete membership: got %d, want 204", resp.StatusCode)
	}
	resp = s.get(t, base+"/ent-mem-a", defaultToken)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("get after delete: got %d, want 404", resp.StatusCode)
	}
}

func TestEnterpriseTeamOrganizations_Assignments(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	s.createEnterpriseTestOrg(t, "ent-team-org-1")
	s.createEnterpriseTestOrg(t, "ent-team-org-2")

	// Selection type "disabled" (default): assignments cannot be edited.
	resp := s.post(t, enterpriseAPI+"/teams", defaultToken, map[string]interface{}{"name": "Org Squad"})
	if resp.StatusCode != http.StatusCreated {
		resp.Body.Close()
		t.Fatalf("create team: got %d", resp.StatusCode)
	}
	resp.Body.Close()
	base := enterpriseAPI + "/teams/org-squad/organizations"

	resp = s.put(t, base+"/ent-team-org-1", defaultToken, nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("assign while disabled: got %d, want 422", resp.StatusCode)
	}

	// Switch to "selected" and assign.
	resp = s.patch(t, enterpriseAPI+"/teams/org-squad", defaultToken, map[string]interface{}{
		"organization_selection_type": "selected",
	})
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("patch selection type: got %d, want 200", resp.StatusCode)
	}

	resp = s.put(t, base+"/ent-team-org-1", defaultToken, nil)
	if resp.StatusCode != http.StatusCreated {
		resp.Body.Close()
		t.Fatalf("assign org: got %d, want 201", resp.StatusCode)
	}
	assigned := decodeJSON(t, resp)
	if assigned["login"] != "ent-team-org-1" {
		t.Fatalf("assigned org login = %v", assigned["login"])
	}

	// Bulk add the second org.
	resp = s.post(t, base+"/add", defaultToken, map[string]interface{}{
		"organization_slugs": []string{"ent-team-org-2"},
	})
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		t.Fatalf("bulk add orgs: got %d, want 200", resp.StatusCode)
	}
	bulk := decodeJSONArray(t, resp)
	if len(bulk) != 1 || bulk[0]["login"] != "ent-team-org-2" {
		t.Fatalf("bulk add response = %v", bulk)
	}

	// List returns both assignments.
	resp = s.get(t, base, defaultToken)
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		t.Fatalf("list assignments: got %d, want 200", resp.StatusCode)
	}
	if got := len(decodeJSONArray(t, resp)); got != 2 {
		t.Fatalf("assignment count = %d, want 2", got)
	}

	// Single-assignment read.
	resp = s.get(t, base+"/ent-team-org-2", defaultToken)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("get assignment: got %d, want 200", resp.StatusCode)
	}

	// Bulk remove → 204.
	resp = s.post(t, base+"/remove", defaultToken, map[string]interface{}{
		"organization_slugs": []string{"ent-team-org-2"},
	})
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("bulk remove orgs: got %d, want 204", resp.StatusCode)
	}
	resp = s.get(t, base+"/ent-team-org-2", defaultToken)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("get removed assignment: got %d, want 404", resp.StatusCode)
	}

	// DELETE single assignment.
	resp = s.delete(t, base+"/ent-team-org-1", defaultToken)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("delete assignment: got %d, want 204", resp.StatusCode)
	}

	// Selection type "all" derives every organization on the instance.
	resp = s.patch(t, enterpriseAPI+"/teams/org-squad", defaultToken, map[string]interface{}{
		"organization_selection_type": "all",
	})
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("patch selection all: got %d, want 200", resp.StatusCode)
	}
	resp = s.get(t, base+"/ent-team-org-2", defaultToken)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("assignment under all: got %d, want 200 (every org assigned)", resp.StatusCode)
	}

	// Cleanup so team lists elsewhere stay predictable.
	resp = s.delete(t, enterpriseAPI+"/teams/org-squad", defaultToken)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("cleanup delete: got %d", resp.StatusCode)
	}
}

// TestEnterpriseTeamBulk_AtomicOnInvalid covers REST-062: a bulk add/remove
// that names one valid and one invalid entry must reject the whole request
// with a 422 and commit nothing — the earlier valid entries must not already
// be applied by the time the invalid one is discovered.
func TestEnterpriseTeamBulk_AtomicOnInvalid(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	resp := s.post(t, enterpriseAPI+"/teams", defaultToken, map[string]interface{}{"name": "Atomic Crew"})
	if resp.StatusCode != http.StatusCreated {
		resp.Body.Close()
		t.Fatalf("create team: got %d", resp.StatusCode)
	}
	resp.Body.Close()
	_ = s.createEnterpriseTestUser(t, "ent-atomic-a")
	memberships := enterpriseAPI + "/teams/atomic-crew/memberships"

	// Valid user first, invalid user second → 422, and the valid user must not
	// have been added.
	resp = s.post(t, memberships+"/add", defaultToken, map[string]interface{}{
		"usernames": []string{"ent-atomic-a", "no-such-user"},
	})
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("bulk add with invalid entry: got %d, want 422", resp.StatusCode)
	}
	resp = s.get(t, memberships+"/ent-atomic-a", defaultToken)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("valid member added despite 422: get membership got %d, want 404", resp.StatusCode)
	}

	// Now add the valid user cleanly, then a bulk remove naming it plus an
	// invalid entry must 422 and leave the valid member in place.
	resp = s.put(t, memberships+"/ent-atomic-a", defaultToken, nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("seed membership: got %d, want 201", resp.StatusCode)
	}
	resp = s.post(t, memberships+"/remove", defaultToken, map[string]interface{}{
		"usernames": []string{"ent-atomic-a", "no-such-user"},
	})
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("bulk remove with invalid entry: got %d, want 422", resp.StatusCode)
	}
	resp = s.get(t, memberships+"/ent-atomic-a", defaultToken)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("valid member removed despite 422: get membership got %d, want 200", resp.StatusCode)
	}

	// Organization assignments have the same contract in "selected" mode.
	s.createEnterpriseTestOrg(t, "ent-atomic-org")
	resp = s.patch(t, enterpriseAPI+"/teams/atomic-crew", defaultToken, map[string]interface{}{
		"organization_selection_type": "selected",
	})
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("patch selection type: got %d, want 200", resp.StatusCode)
	}
	orgs := enterpriseAPI + "/teams/atomic-crew/organizations"
	resp = s.post(t, orgs+"/add", defaultToken, map[string]interface{}{
		"organization_slugs": []string{"ent-atomic-org", "no-such-org"},
	})
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("bulk add orgs with invalid entry: got %d, want 422", resp.StatusCode)
	}
	resp = s.get(t, orgs+"/ent-atomic-org", defaultToken)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("valid org assigned despite 422: get assignment got %d, want 404", resp.StatusCode)
	}

	resp = s.delete(t, enterpriseAPI+"/teams/atomic-crew", defaultToken)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("cleanup delete: got %d", resp.StatusCode)
	}
}
