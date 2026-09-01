package bleephub

import (
	"context"
	"net/http"
	"net/url"
	"testing"

	"github.com/e6qu/bleephub/internal/store"
)

// TestSearchIssuesNegatedAuthorExcludes pins that a negated identity qualifier
// (`-author:x`) actually excludes matching issues; negated qualifiers were
// silently skipped, so `-author:x` returned issues authored by x.
func TestSearchIssuesNegatedAuthorExcludes(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	s.seedRepo(t, "search-neg", false)
	s.post(t, "/api/v3/repos/admin/search-neg/issues", defaultToken,
		map[string]interface{}{"title": "by admin"}).Body.Close()

	other := s.createTestUser(t, "search-neg-other")
	s.store.AddRepoCollaborator("admin", "search-neg", other.Login, "push")
	otherTok := s.store.CreateToken(other.ID, "repo")
	s.post(t, "/api/v3/repos/admin/search-neg/issues", otherTok.Value,
		map[string]interface{}{"title": "by other"}).Body.Close()

	q := url.QueryEscape("repo:admin/search-neg -author:search-neg-other")
	got := decodeJSON(t, s.get(t, "/api/v3/search/issues?q="+q, defaultToken))
	items, _ := got["items"].([]interface{})
	titles := map[string]bool{}
	for _, it := range items {
		if m, ok := it.(map[string]interface{}); ok {
			if title, _ := m["title"].(string); title != "" {
				titles[title] = true
			}
		}
	}
	if !titles["by admin"] {
		t.Fatalf("-author search dropped the admin-authored issue: %v", titles)
	}
	if titles["by other"] {
		t.Fatal("-author:search-neg-other still returned an issue authored by that user")
	}
}

// TestRepoVisibilityPatchMakesPrivate pins that PATCH with the modern
// `visibility` field actually changes visibility; it was silently ignored, so a
// PATCH {"visibility":"private"} returned 200 with the repo still public.
func TestRepoVisibilityPatchMakesPrivate(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	s.seedRepo(t, "vis-repo", false) // public

	resp := s.patch(t, "/api/v3/repos/admin/vis-repo", defaultToken, map[string]interface{}{"visibility": "private"})
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		t.Fatalf("PATCH visibility = %d, want 200", resp.StatusCode)
	}
	resp.Body.Close()

	got := decodeJSON(t, s.get(t, "/api/v3/repos/admin/vis-repo", defaultToken))
	if got["private"] != true || got["visibility"] != "private" {
		t.Fatalf("after PATCH visibility=private: private=%v visibility=%v, want true/private",
			got["private"], got["visibility"])
	}
}

// TestPendingTeamMemberHasNoRepoAccess pins that a user added to a team while
// their org membership is only pending (invited, not accepted) gets no access
// to the team's repos until they accept.
func TestPendingTeamMemberHasNoRepoAccess(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	org := s.seedTestOrg(t, "pending-team-org")
	repo := s.seedOrgRepo(t, org, "team-repo", true) // private
	member := s.createTestUser(t, "pending-team-member")
	team := s.store.CreateTeam(org.Login, "eng", store.TeamOptions{Permission: store.TeamPermission("push")})
	if !s.store.AddTeamRepo(org.Login, team.Slug, repo.FullName) {
		t.Fatal("AddTeamRepo failed")
	}
	s.store.SetTeamMembership(org.Login, team.Slug, member.ID, store.TeamRoleMember)

	// Pending org membership: no access yet.
	s.store.SetMembership(org.Login, member.ID, store.OrgRoleMember, store.MembershipStatePending)
	if store.CanReadRepoAsUser(s.store, member, repo) {
		t.Fatal("a pending team invitee could read the team's private repo")
	}

	// Accepting (active) grants the team access.
	s.store.SetMembership(org.Login, member.ID, store.OrgRoleMember, store.MembershipStateActive)
	if !store.CanReadRepoAsUser(s.store, member, repo) {
		t.Fatal("an active team member was denied read access")
	}
}

// TestEnvironmentReviewerAuthorization pins that only a designated reviewer may
// approve/reject a protected environment; the Actions:write route gate alone let
// any write collaborator release a deployment past the required-reviewers gate.
func TestEnvironmentReviewerAuthorization(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	reviewer := s.createTestUser(t, "env-reviewer")
	outsider := s.createTestUser(t, "env-outsider")
	ctx := context.Background()

	env := &store.Environment{Name: "production", Reviewers: []map[string]interface{}{
		{"type": "User", "id": reviewer.ID},
	}}
	// repo=nil skips the admin branch, isolating the reviewer-list check.
	if !s.canReviewEnvironment(ctx, nil, env, reviewer) {
		t.Fatal("a designated reviewer was refused")
	}
	if s.canReviewEnvironment(ctx, nil, env, outsider) {
		t.Fatal("a non-reviewer was allowed to approve a protected environment")
	}
	// An environment with no required reviewers is not gated here.
	if !s.canReviewEnvironment(ctx, nil, &store.Environment{Name: "staging"}, outsider) {
		t.Fatal("an unprotected environment wrongly refused review")
	}
}

// TestForkParentNotLeakedWhenPrivate pins that a fork whose parent has since
// gone private does not leak the private parent's metadata to a viewer without
// access to the parent.
func TestForkParentNotLeakedWhenPrivate(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	parent := s.seedRepo(t, "parent-repo", false) // public
	s.seedRepo(t, "fork-repo", false)             // public fork
	s.store.UpdateRepo("admin", "fork-repo", func(r *store.Repo) {
		r.Fork = true
		r.ParentID = parent.ID
		r.SourceID = parent.ID
	})
	// The parent goes private.
	s.store.UpdateRepo("admin", "parent-repo", func(r *store.Repo) {
		r.Private = true
		r.Visibility = "private"
	})

	// An outsider who can read the public fork but NOT the private parent.
	outsider := s.createTestUser(t, "fork-outsider")
	tok := s.store.CreateToken(outsider.ID, "repo")
	got := decodeJSON(t, s.get(t, "/api/v3/repos/admin/fork-repo", tok.Value))
	if _, ok := got["parent"]; ok {
		t.Fatal("the now-private parent's metadata leaked to an unauthorized viewer")
	}
	if _, ok := got["source"]; ok {
		t.Fatal("the now-private source's metadata leaked to an unauthorized viewer")
	}

	// The owner still sees the parent.
	ownerGot := decodeJSON(t, s.get(t, "/api/v3/repos/admin/fork-repo", defaultToken))
	if _, ok := ownerGot["parent"]; !ok {
		t.Fatal("the owner should still see the fork's parent")
	}
}
