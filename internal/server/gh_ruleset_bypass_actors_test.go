package bleephub

import (
	"context"
	"testing"

	"github.com/e6qu/bleephub/internal/store"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
)

// TestEveryKindOfRulesetBypassActorIsHonoured drives the real evaluator with a
// ruleset that forbids creating a branch, once per kind of bypass actor, and in
// both directions: the actor the ruleset lists creates the branch, and someone
// it does not list is refused. Only `User` was honoured before; a team, a role,
// an organization's administrators, an enterprise's owners or an app given a
// bypass was stored, returned by the API, and refused all the same.
func TestEveryKindOfRulesetBypassActorIsHonoured(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	admin := s.store.LookupUserByLogin("admin")
	org := s.store.CreateOrg(admin, "bypass-org", "Bypass", "")
	repo := s.store.CreateOrgRepo(org, admin, "guarded", "", false)
	stor := s.store.GetGitStorage(org.Login, repo.Name)
	base, err := initRepoWithFiles(stor, "main", "root", map[string]string{"README.md": "root"},
		&object.Signature{Name: "admin", Email: "admin@example.com", When: fixedRulesetTestTime})
	if err != nil {
		t.Fatalf("seed repository: %v", err)
	}

	outsider, _ := s.newUser(t, "bypass-outsider")
	listed, _ := s.newUser(t, "bypass-listed")
	teammate, _ := s.newUser(t, "bypass-teammate")
	writer, _ := s.newUser(t, "bypass-writer")
	reader, _ := s.newUser(t, "bypass-reader")
	orgAdmin, _ := s.newUser(t, "bypass-org-admin")
	for _, user := range []*store.User{listed, teammate, writer, reader} {
		s.store.SetMembership(org.Login, user.ID, store.OrgRoleMember, store.MembershipStateActive)
	}
	s.store.SetMembership(org.Login, orgAdmin.ID, store.OrgRoleAdmin, store.MembershipStateActive)
	team := s.store.CreateTeam(org.Login, "release-managers", store.TeamOptions{})
	s.store.SetTeamMembership(org.Login, team.Slug, teammate.ID, store.TeamRoleMember)
	s.store.AddRepoCollaborator(org.Login, repo.Name, writer.Login, "push")
	s.store.AddRepoCollaborator(org.Login, repo.Name, reader.Login, "pull")
	bot := store.AppBotUser(&store.App{ID: 4242, Slug: "release-bot"})
	otherBot := store.AppBotUser(&store.App{ID: 4243, Slug: "other-bot"})
	enterprise := s.store.GetEnterprise(s.enterpriseSlug())
	entOwner, _ := s.newUser(t, "bypass-ent-owner")
	s.store.SetEnterpriseMembership(enterprise.ID, entOwner.ID, store.EnterpriseRoleOwner)

	attempt := 0
	creates := func(actor *store.User) bool {
		attempt++
		ref := plumbing.NewBranchReferenceName("attempt-" + itoa(attempt))
		return s.evaluateRulesetsForRefWrite(contextWithUser(context.Background(), actor), repo, stor, ref, refCreation, base) == ""
	}

	for _, tc := range []struct {
		name           string
		bypass         store.RulesetBypassActor
		admitted       *store.User
		refused        *store.User
		refusedDespite string
	}{
		{"a user", store.RulesetBypassActor{ActorID: listed.ID, ActorType: "User", BypassMode: "always"}, listed, outsider, "not being listed"},
		{"a team", store.RulesetBypassActor{ActorID: team.ID, ActorType: "Team", BypassMode: "always"}, teammate, listed, "not being on the team"},
		{"a repository role", store.RulesetBypassActor{ActorID: 4, ActorType: "RepositoryRole", BypassMode: "always"}, writer, reader, "holding a lower role"},
		{"the organization's administrators", store.RulesetBypassActor{ActorID: 1, ActorType: "OrganizationAdmin", BypassMode: "always"}, orgAdmin, writer, "being a plain member"},
		{"the enterprise's owners", store.RulesetBypassActor{ActorID: 0, ActorType: "EnterpriseOwner", BypassMode: "exempt"}, entOwner, writer, "not owning the enterprise"},
		{"an app", store.RulesetBypassActor{ActorID: 4242, ActorType: "Integration", BypassMode: "always"}, bot, otherBot, "being another app"},
		// `pull_request` lets its holder bypass when merging a pull request, and
		// says nothing about a direct write to the reference.
		{"a pull-request-only bypass", store.RulesetBypassActor{ActorID: listed.ID, ActorType: "User", BypassMode: "pull_request"}, nil, listed, "its bypass covering only pull request merges"},
	} {
		ruleset := s.store.CreateRuleset(repo, &store.Ruleset{
			Name: tc.name, Target: "branch", Enforcement: "active",
			BypassActors: []store.RulesetBypassActor{tc.bypass},
			Rules:        []store.Rule{{Type: "creation"}},
		})
		if tc.admitted != nil && !creates(tc.admitted) {
			t.Errorf("%s: the listed actor was refused", tc.name)
		}
		if creates(tc.refused) {
			t.Errorf("%s: %s was admitted despite %s", tc.name, tc.refused.Login, tc.refusedDespite)
		}
		s.store.DeleteRuleset(ruleset.ID)
	}
}
