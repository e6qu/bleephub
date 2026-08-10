package bleephub

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
)

func TestOrgCopilotSpaces_CRUD(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	org, _ := s.copilotTestOrg(t, "copilot-spaces-org")
	base := "/api/v3/orgs/" + org.Login + "/copilot-spaces"

	// Name is required.
	requireStatus(t, s.post(t, base, defaultToken, map[string]interface{}{"description": "no name"}), 422)

	// Instructions are capped at 4000 characters.
	requireStatus(t, s.post(t, base, defaultToken, map[string]interface{}{
		"name": "too long", "general_instructions": strings.Repeat("x", 4001)}), 422)

	// Invalid base role.
	requireStatus(t, s.post(t, base, defaultToken, map[string]interface{}{"name": "bad role", "base_role": "owner"}), 422)

	resp := s.post(t, base, defaultToken, map[string]interface{}{
		"name":                 "Team Planning Space",
		"description":          "Organization space for team planning",
		"general_instructions": "Help the team with planning",
		"base_role":            "reader",
	})
	space := decodeJSONWithStatus(t, resp, 201)
	if space["number"] != float64(1) || space["name"] != "Team Planning Space" || space["base_role"] != "reader" {
		t.Fatalf("created space = %v", space)
	}
	if space["owner"].(map[string]interface{})["login"] != org.Login {
		t.Fatalf("space owner = %v", space["owner"])
	}
	if space["creator"].(map[string]interface{})["login"] != "admin" {
		t.Fatalf("space creator = %v", space["creator"])
	}

	// The advertised api_url resolves to the space itself.
	apiURL, _ := space["api_url"].(string)
	if !strings.HasPrefix(apiURL, s.baseURL) {
		t.Fatalf("api_url = %q, want it under %q", apiURL, s.baseURL)
	}
	requireStatus(t, s.get(t, strings.TrimPrefix(apiURL, s.baseURL), defaultToken), 200)

	// List and get.
	listed := decodeJSONWithStatus(t, s.get(t, base, defaultToken), 200)
	if spaces := listed["spaces"].([]interface{}); len(spaces) != 1 {
		t.Fatalf("listed %d spaces, want 1", len(spaces))
	}
	got := decodeJSONWithStatus(t, s.get(t, base+"/1", defaultToken), 200)
	if got["name"] != "Team Planning Space" || got["description"] != "Organization space for team planning" {
		t.Fatalf("get space = %v", got)
	}

	// Update round-trips.
	updated := decodeJSONWithStatus(t, s.put(t, base+"/1", defaultToken,
		map[string]interface{}{"name": "Updated Space", "base_role": "writer"}), 200)
	if updated["name"] != "Updated Space" || updated["base_role"] != "writer" {
		t.Fatalf("updated space = %v", updated)
	}

	// Delete, then the space is gone.
	requireStatus(t, s.delete(t, base+"/1", defaultToken), 204)
	requireStatus(t, s.get(t, base+"/1", defaultToken), 404)

	// Unknown org → 404.
	requireStatus(t, s.get(t, "/api/v3/orgs/no-such-org/copilot-spaces", defaultToken), 404)
}

func TestUserCopilotSpaces_CRUDAndVisibility(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	owner := seedTestUser(s.Server, "space-owner-user")
	other := seedTestUser(s.Server, "space-other-user")
	ownerTok := s.store.CreateToken(owner.ID, "repo").Value
	otherTok := s.store.CreateToken(other.ID, "repo").Value
	base := "/api/v3/users/" + owner.Login + "/copilot-spaces"

	// Spaces are created for the authenticated user only.
	requireStatus(t, s.post(t, base, otherTok, map[string]interface{}{"name": "Hijack"}), 403)

	// User spaces only allow reader / no_access base roles.
	requireStatus(t, s.post(t, base, ownerTok, map[string]interface{}{"name": "Bad", "base_role": "writer"}), 422)

	space := decodeJSONWithStatus(t, s.post(t, base, ownerTok, map[string]interface{}{"name": "My Development Space"}), 201)
	if space["base_role"] != "no_access" {
		t.Fatalf("default base_role = %v, want no_access", space["base_role"])
	}
	if space["owner"].(map[string]interface{})["login"] != owner.Login {
		t.Fatalf("owner = %v", space["owner"])
	}
	if space["description"] != nil {
		t.Fatalf("description = %v, want null", space["description"])
	}

	// Numbers are sequential per owner.
	second := decodeJSONWithStatus(t, s.post(t, base, ownerTok, map[string]interface{}{"name": "Second Space"}), 201)
	if second["number"] != float64(2) {
		t.Fatalf("second space number = %v, want 2", second["number"])
	}

	// A no_access space is invisible to other users.
	requireStatus(t, s.get(t, base+"/1", otherTok), 404)
	visible := decodeJSONWithStatus(t, s.get(t, base, otherTok), 200)
	if spaces := visible["spaces"].([]interface{}); len(spaces) != 0 {
		t.Fatalf("outsider sees %d spaces, want 0", len(spaces))
	}

	// base_role reader opens it up for reading, not writing.
	requireStatus(t, s.put(t, base+"/1", ownerTok, map[string]interface{}{"base_role": "reader"}), 200)
	requireStatus(t, s.get(t, base+"/1", otherTok), 200)
	requireStatus(t, s.put(t, base+"/1", otherTok, map[string]interface{}{"name": "Defaced"}), 403)
	requireStatus(t, s.delete(t, base+"/1", otherTok), 403)
}

func TestUserCopilotSpaces_CursorPagination(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	owner := seedTestUser(s.Server, "space-pagination-user")
	ownerTok := s.store.CreateToken(owner.ID, "repo").Value
	base := "/api/v3/users/" + owner.Login + "/copilot-spaces"
	for i := 1; i <= 3; i++ {
		requireStatus(t, s.post(t, base, ownerTok, map[string]interface{}{"name": fmt.Sprintf("Space %d", i)}), 201)
	}

	resp := s.get(t, base+"?per_page=2", ownerTok)
	link := resp.Header.Get("Link")
	page := decodeJSONWithStatus(t, resp, 200)["spaces"].([]interface{})
	if len(page) != 2 {
		t.Fatalf("first page has %d spaces, want 2", len(page))
	}
	if !strings.Contains(link, `rel="next"`) || !strings.Contains(link, "after=2") {
		t.Fatalf("Link = %q, want a next cursor after space 2", link)
	}

	page = decodeJSONWithStatus(t, s.get(t, base+"?per_page=2&after=2", ownerTok), 200)["spaces"].([]interface{})
	if len(page) != 1 || page[0].(map[string]interface{})["number"] != float64(3) {
		t.Fatalf("after=2 page = %v, want just space 3", page)
	}

	page = decodeJSONWithStatus(t, s.get(t, base+"?per_page=2&before=3", ownerTok), 200)["spaces"].([]interface{})
	if len(page) != 2 || page[1].(map[string]interface{})["number"] != float64(2) {
		t.Fatalf("before=3 page = %v, want spaces 1 and 2", page)
	}
}

func TestOrgCopilotSpaceCollaborators(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	org, members := s.copilotTestOrg(t, "copilot-collab-org", "collab-bob", "collab-carol")
	bob, carol := members[0], members[1]
	bobTok := s.store.CreateToken(bob.ID, "repo").Value
	team := s.store.CreateTeam(org.Login, "collab team", TeamOptions{})
	s.store.SetTeamMembership(org.Login, team.Slug, carol.ID, TeamRoleMember)

	base := "/api/v3/orgs/" + org.Login + "/copilot-spaces"
	requireStatus(t, s.post(t, base, defaultToken, map[string]interface{}{"name": "Collab Space"}), 201)
	spaceBase := base + "/1"

	// The default no_access space is hidden from plain members.
	requireStatus(t, s.get(t, spaceBase, bobTok), 404)

	// Validation: bad actor type, bad role, unknown user.
	requireStatus(t, s.post(t, spaceBase+"/collaborators", defaultToken,
		map[string]interface{}{"actor_type": "Robot", "actor_identifier": bob.Login, "role": "reader"}), 422)
	requireStatus(t, s.post(t, spaceBase+"/collaborators", defaultToken,
		map[string]interface{}{"actor_type": "User", "actor_identifier": bob.Login, "role": "owner"}), 422)
	requireStatus(t, s.post(t, spaceBase+"/collaborators", defaultToken,
		map[string]interface{}{"actor_type": "User", "actor_identifier": "no-such-user", "role": "reader"}), 422)

	// Add bob as a writer.
	collab := decodeJSONWithStatus(t, s.post(t, spaceBase+"/collaborators", defaultToken,
		map[string]interface{}{"actor_type": "User", "actor_identifier": bob.Login, "role": "writer"}), 201)
	if collab["actor_type"] != "User" || collab["role"] != "writer" || collab["login"] != bob.Login {
		t.Fatalf("user collaborator = %v", collab)
	}

	// Writer access: bob sees the space but cannot change settings.
	requireStatus(t, s.get(t, spaceBase, bobTok), 200)
	requireStatus(t, s.put(t, spaceBase, bobTok, map[string]interface{}{"name": "Renamed"}), 403)

	// Add the team as reader; carol gains read access through it.
	teamCollab := decodeJSONWithStatus(t, s.post(t, spaceBase+"/collaborators", defaultToken,
		map[string]interface{}{"actor_type": "Team", "actor_identifier": team.Slug, "role": "reader"}), 201)
	if teamCollab["actor_type"] != "Team" || teamCollab["slug"] != team.Slug || teamCollab["type"] != "Team" {
		t.Fatalf("team collaborator = %v", teamCollab)
	}
	carolTok := s.store.CreateToken(carol.ID, "repo").Value
	requireStatus(t, s.get(t, spaceBase, carolTok), 200)

	listed := decodeJSONWithStatus(t, s.get(t, spaceBase+"/collaborators", defaultToken), 200)
	if collabs := listed["collaborators"].([]interface{}); len(collabs) != 2 {
		t.Fatalf("listed %d collaborators, want 2", len(collabs))
	}

	// Update bob's role via the path-addressed PUT.
	updated := decodeJSONWithStatus(t, s.put(t, spaceBase+"/collaborators/User/"+bob.Login, defaultToken,
		map[string]interface{}{"role": "admin"}), 200)
	if updated["role"] != "admin" {
		t.Fatalf("updated collaborator = %v", updated)
	}

	// role=no_access removes the grant (204).
	requireStatus(t, s.put(t, spaceBase+"/collaborators/User/"+bob.Login, defaultToken,
		map[string]interface{}{"role": "no_access"}), 204)
	requireStatus(t, s.get(t, spaceBase, bobTok), 404)

	// Remove the team; removing it again is a 404.
	requireStatus(t, s.delete(t, spaceBase+"/collaborators/Team/"+team.Slug, defaultToken), 204)
	requireStatus(t, s.delete(t, spaceBase+"/collaborators/Team/"+team.Slug, defaultToken), 404)

	listed = decodeJSONWithStatus(t, s.get(t, spaceBase+"/collaborators", defaultToken), 200)
	if collabs := listed["collaborators"].([]interface{}); len(collabs) != 0 {
		t.Fatalf("listed %d collaborators after removals, want 0", len(collabs))
	}
}

func TestUserCopilotSpaceCollaborators_TeamRejected(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	owner := seedTestUser(s.Server, "space-team-reject-user")
	ownerTok := s.store.CreateToken(owner.ID, "repo").Value
	base := "/api/v3/users/" + owner.Login + "/copilot-spaces"
	requireStatus(t, s.post(t, base, ownerTok, map[string]interface{}{"name": "Solo Space"}), 201)

	requireStatus(t, s.post(t, base+"/1/collaborators", ownerTok,
		map[string]interface{}{"actor_type": "Team", "actor_identifier": "any-team", "role": "reader"}), 422)
}

func TestCopilotSpaceResources_CRUD(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	org, _ := s.copilotTestOrg(t, "copilot-resources-org")
	admin := s.store.LookupUserByLogin("admin")
	repo := s.store.CreateOrgRepo(org, admin, "resources-repo", "", false)
	base := "/api/v3/orgs/" + org.Login + "/copilot-spaces"
	requireStatus(t, s.post(t, base, defaultToken, map[string]interface{}{"name": "Resource Space"}), 201)
	resBase := base + "/1/resources"

	// Validation: type outside the create enum, missing metadata,
	// dangling repository_id.
	requireStatus(t, s.post(t, resBase, defaultToken, map[string]interface{}{
		"resource_type": "unknown", "metadata": map[string]interface{}{}}), 422)
	requireStatus(t, s.post(t, resBase, defaultToken, map[string]interface{}{"resource_type": "repository"}), 422)
	requireStatus(t, s.post(t, resBase, defaultToken, map[string]interface{}{
		"resource_type": "repository", "metadata": map[string]interface{}{"repository_id": 999999}}), 422)

	// Attach a repository resource.
	repoMeta := map[string]interface{}{"repository_id": repo.ID}
	repoRes := decodeJSONWithStatus(t, s.post(t, resBase, defaultToken, map[string]interface{}{
		"resource_type": "repository", "metadata": repoMeta}), 201)
	if repoRes["resource_type"] != "repository" || repoRes["created_at"] == nil {
		t.Fatalf("repository resource = %v", repoRes)
	}
	if meta := repoRes["metadata"].(map[string]interface{}); meta["repository_id"] != float64(repo.ID) {
		t.Fatalf("resource metadata = %v", meta)
	}

	// Attaching the identical resource again returns the existing one.
	dup := decodeJSONWithStatus(t, s.post(t, resBase, defaultToken, map[string]interface{}{
		"resource_type": "repository", "metadata": repoMeta}), 200)
	if dup["id"] != repoRes["id"] {
		t.Fatalf("duplicate resource id = %v, want %v", dup["id"], repoRes["id"])
	}

	// A free-text resource requires its text.
	requireStatus(t, s.post(t, resBase, defaultToken, map[string]interface{}{
		"resource_type": "free_text", "metadata": map[string]interface{}{"name": "notes"}}), 422)
	textRes := decodeJSONWithStatus(t, s.post(t, resBase, defaultToken, map[string]interface{}{
		"resource_type": "free_text", "metadata": map[string]interface{}{"name": "notes", "text": "Remember the milk"}}), 201)

	// List, get, update, delete.
	listed := decodeJSONWithStatus(t, s.get(t, resBase, defaultToken), 200)
	if resources := listed["resources"].([]interface{}); len(resources) != 2 {
		t.Fatalf("listed %d resources, want 2", len(resources))
	}
	textID := fmt.Sprint(int(textRes["id"].(float64)))
	requireStatus(t, s.get(t, resBase+"/"+textID, defaultToken), 200)
	updated := decodeJSONWithStatus(t, s.put(t, resBase+"/"+textID, defaultToken, map[string]interface{}{
		"metadata": map[string]interface{}{"name": "notes", "text": "Remember the bread"}}), 200)
	if updated["metadata"].(map[string]interface{})["text"] != "Remember the bread" {
		t.Fatalf("updated resource = %v", updated)
	}
	requireStatus(t, s.delete(t, resBase+"/"+textID, defaultToken), 204)
	requireStatus(t, s.get(t, resBase+"/"+textID, defaultToken), 404)

	// The space embeds its remaining resources.
	space := decodeJSONWithStatus(t, s.get(t, base+"/1", defaultToken), 200)
	attrs := space["resources_attributes"].([]interface{})
	if len(attrs) != 1 || attrs[0].(map[string]interface{})["resource_type"] != "repository" {
		t.Fatalf("resources_attributes = %v, want the repository resource", attrs)
	}
}

func TestCopilotSpaceNestedResourceAttributesAreAtomic(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	org, _ := s.copilotTestOrg(t, "copilot-nested-resources-org")
	admin := s.store.LookupUserByLogin("admin")
	repo := s.store.CreateOrgRepo(org, admin, "nested-resources-repo", "", false)
	base := "/api/v3/orgs/" + org.Login + "/copilot-spaces"

	created := decodeJSONWithStatus(t, s.post(t, base, defaultToken, map[string]interface{}{
		"name": "Nested resources",
		"resources_attributes": []map[string]interface{}{
			{"resource_type": "repository", "metadata": map[string]interface{}{"repository_id": repo.ID}},
			{"resource_type": "free_text", "metadata": map[string]interface{}{"name": "notes", "text": "first"}},
		},
	}), http.StatusCreated)
	resources := created["resources_attributes"].([]interface{})
	if len(resources) != 2 {
		t.Fatalf("created resources = %v", resources)
	}
	repositoryID := int(resources[0].(map[string]interface{})["id"].(float64))
	textID := int(resources[1].(map[string]interface{})["id"].(float64))

	updated := decodeJSONWithStatus(t, s.put(t, base+"/1", defaultToken, map[string]interface{}{
		"resources_attributes": []map[string]interface{}{
			{"id": repositoryID, "_destroy": true},
			{"id": textID, "metadata": map[string]interface{}{"name": "notes", "text": "updated"}},
			{"resource_type": "media_content", "metadata": map[string]interface{}{"name": "diagram"}},
		},
	}), http.StatusOK)
	resources = updated["resources_attributes"].([]interface{})
	if len(resources) != 2 ||
		resources[0].(map[string]interface{})["metadata"].(map[string]interface{})["text"] != "updated" ||
		resources[1].(map[string]interface{})["resource_type"] != "media_content" {
		t.Fatalf("updated resources = %v", resources)
	}

	// A later invalid member rejects the entire nested update.
	requireStatus(t, s.put(t, base+"/1", defaultToken, map[string]interface{}{
		"resources_attributes": []map[string]interface{}{
			{"id": textID, "_destroy": true},
			{"resource_type": "repository", "metadata": map[string]interface{}{"repository_id": 999999}},
		},
	}), http.StatusUnprocessableEntity)
	after := decodeJSONWithStatus(t, s.get(t, base+"/1", defaultToken), http.StatusOK)
	if got := after["resources_attributes"].([]interface{}); len(got) != 2 {
		t.Fatalf("invalid nested update partially committed: %v", got)
	}
}
