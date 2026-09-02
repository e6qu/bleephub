package bleephub

import (
	"net/http"
	"testing"
	"time"
)

// setUserCreatedAt back-dates (or forward-dates) a user's account creation so the
// existing_users interaction limit can be exercised deterministically.
func (s *isolatedServer) setUserCreatedAt(id int, at time.Time) {
	s.store.Mu.Lock()
	defer s.store.Mu.Unlock()
	if u := s.store.Users[id]; u != nil {
		u.CreatedAt = at
	}
}

// TestInteractionLimitCollaboratorsOnly pins that a collaborators_only limit bars
// a non-collaborator from opening an issue while leaving the owner unaffected,
// and that clearing the limit restores access. Enforcement was previously absent
// — the limit could be read back but never blocked anyone.
func TestInteractionLimitCollaboratorsOnly(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	repo := s.seedRepo(t, "limited", false)
	_, outsiderTok := s.newUser(t, "outsider")

	future := s.currentTime().Add(24 * time.Hour)
	if !s.store.SetRepoInteractionLimit(repo.ID, "collaborators_only", &future) {
		t.Fatal("SetRepoInteractionLimit failed")
	}

	// A non-collaborator is refused.
	resp := s.post(t, "/api/v3/repos/admin/limited/issues", outsiderTok, map[string]interface{}{"title": "hi"})
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("outsider opening an issue under collaborators_only = %d, want 403", resp.StatusCode)
	}

	// The owner is exempt.
	resp = s.post(t, "/api/v3/repos/admin/limited/issues", defaultToken, map[string]interface{}{"title": "owner ok"})
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("owner opening an issue under collaborators_only = %d, want 201", resp.StatusCode)
	}

	// Clearing the limit restores the outsider's access.
	if !s.store.SetRepoInteractionLimit(repo.ID, "", nil) {
		t.Fatal("clearing SetRepoInteractionLimit failed")
	}
	resp = s.post(t, "/api/v3/repos/admin/limited/issues", outsiderTok, map[string]interface{}{"title": "now allowed"})
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("outsider opening an issue after the limit is cleared = %d, want 201", resp.StatusCode)
	}
}

// TestInteractionLimitExistingUsers pins that an existing_users limit bars a
// brand-new account but admits an established one.
func TestInteractionLimitExistingUsers(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	repo := s.seedRepo(t, "newbie-gate", false)
	fresh, freshTok := s.newUser(t, "fresh")

	future := s.currentTime().Add(24 * time.Hour)
	s.store.SetRepoInteractionLimit(repo.ID, "existing_users", &future)

	// A just-created account is barred.
	s.setUserCreatedAt(fresh.ID, s.currentTime())
	resp := s.post(t, "/api/v3/repos/admin/newbie-gate/issues", freshTok, map[string]interface{}{"title": "too soon"})
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("fresh account under existing_users = %d, want 403", resp.StatusCode)
	}

	// The same account, once older than the window, is admitted.
	s.setUserCreatedAt(fresh.ID, s.currentTime().Add(-48*time.Hour))
	resp = s.post(t, "/api/v3/repos/admin/newbie-gate/issues", freshTok, map[string]interface{}{"title": "established now"})
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("established account under existing_users = %d, want 201", resp.StatusCode)
	}
}

// TestInteractionLimitEnforcedOnGraphQL pins that the same collaborators_only
// limit is enforced on the GraphQL createIssue mutation, not just the REST path
// — otherwise a client could bypass the limit through GraphQL. The owner remains
// able to create, proving the gate is limit-specific rather than a blanket deny.
func TestInteractionLimitEnforcedOnGraphQL(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	repo := s.seedRepo(t, "gql-limited", false)
	_, outsiderTok := s.newUser(t, "gql-outsider")

	future := s.currentTime().Add(24 * time.Hour)
	s.store.SetRepoInteractionLimit(repo.ID, "collaborators_only", &future)

	mutation := map[string]interface{}{
		"query": `mutation($input: CreateIssueInput!){ createIssue(input:$input){ issue { number } } }`,
		"variables": map[string]interface{}{
			"input": map[string]interface{}{"repositoryId": repo.NodeID, "title": "via graphql"},
		},
	}

	body := decodeJSON(t, s.post(t, "/api/graphql", outsiderTok, mutation))
	if errs, _ := body["errors"].([]interface{}); len(errs) == 0 {
		t.Fatalf("outsider createIssue under collaborators_only: expected a GraphQL error, got %v", body)
	}

	body = decodeJSON(t, s.post(t, "/api/graphql", defaultToken, mutation))
	if errs, _ := body["errors"].([]interface{}); len(errs) != 0 {
		t.Fatalf("owner createIssue under collaborators_only: unexpected errors %v", body["errors"])
	}
}

// TestOwnerBlockBarsContentCreation pins that a user the repository owner has
// blocked cannot open an issue or comment there, while an unblocked user can.
// Blocks were enforced on follow/invitation but never on content creation.
func TestOwnerBlockBarsContentCreation(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	s.seedRepo(t, "no-trolls", false)
	admin := s.store.LookupUserByLogin("admin")

	// Seed an issue (as the owner) for the comment path to target.
	resp := s.post(t, "/api/v3/repos/admin/no-trolls/issues", defaultToken, map[string]interface{}{"title": "topic"})
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("seeding issue = %d, want 201", resp.StatusCode)
	}

	troll, trollTok := s.newUser(t, "troll")
	s.store.BlockUser(admin.ID, troll.ID) // the owner blocks the troll

	resp = s.post(t, "/api/v3/repos/admin/no-trolls/issues", trollTok, map[string]interface{}{"title": "spam"})
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("blocked user opening an issue = %d, want 403", resp.StatusCode)
	}

	resp = s.post(t, "/api/v3/repos/admin/no-trolls/issues/1/comments", trollTok, map[string]interface{}{"body": "spam"})
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("blocked user commenting = %d, want 403", resp.StatusCode)
	}

	// An unblocked user is unaffected.
	_, guestTok := s.newUser(t, "guest")
	resp = s.post(t, "/api/v3/repos/admin/no-trolls/issues/1/comments", guestTok, map[string]interface{}{"body": "hello"})
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("unblocked user commenting = %d, want 201", resp.StatusCode)
	}
}
