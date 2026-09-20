package bleephub

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/e6qu/bleephub/internal/store"
)

const policyWorkflowYAML = `name: gated
on:
  push:
  workflow_dispatch:
  schedule:
    - cron: "0 0 * * *"
jobs:
  build:
    runs-on: ubuntu-latest
    steps:
      - run: echo hi
`

// policyRepo seeds a repository whose one workflow every event below triggers,
// and returns a counter of the runs it has.
func policyRepo(t *testing.T, s *isolatedServer, repoKey string) (*store.Repo, func() int) {
	t.Helper()
	s.cancelRepoRunsCleanup(t, repoKey)
	commitWorkflowYAMLToStorage(t, s.Server, repoKey, ".github/workflows/gated.yml", policyWorkflowYAML)
	repo := s.store.GetRepoByFullName(repoKey)
	if repo == nil {
		t.Fatalf("repository %s was not created", repoKey)
	}
	return repo, func() int {
		s.store.Mu.RLock()
		defer s.store.Mu.RUnlock()
		count := 0
		for _, run := range s.store.Workflows {
			if run.RepoFullName == repoKey {
				count++
			}
		}
		return count
	}
}

func (s *isolatedServer) pushAs(repoKey string, sender *store.User) {
	payload := map[string]interface{}{}
	if sender != nil {
		payload["sender"] = senderPayload(sender, s.baseURL)
	}
	s.triggerWorkflowsForEvent(repoKey, "push", "", "refs/heads/main", payload)
}

// TestAnActorsPolicyAdmitsOnlyTheActorsItLists is the both-directions case for
// restrict_actions_actors, across the kinds of actor a rule can list: the same
// push starts a run for someone the policy lists and none for someone it does
// not, and a push nobody is behind is not an actor the policy could list.
func TestAnActorsPolicyAdmitsOnlyTheActorsItLists(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	repo, runs := policyRepo(t, s, "actorsowner/actors-repo")
	owner := s.store.LookupUserByLogin("actorsowner")
	listed, _ := s.newUser(t, "actors-listed")
	writer, _ := s.newUser(t, "actors-writer")
	reader, _ := s.newUser(t, "actors-reader")
	s.store.AddRepoCollaborator("actorsowner", "actors-repo", writer.Login, "push")
	s.store.AddRepoCollaborator("actorsowner", "actors-repo", reader.Login, "pull")

	s.store.CreateActionsPolicy(&store.ActionsPolicy{RepoID: repo.ID, Name: "listed people", Enforcement: "active",
		Rules: []store.ActionsPolicyRule{{Type: store.ActionsPolicyRuleRestrictActors, AllowedActors: []store.ActionsPolicyActor{
			{ID: listed.ID, Type: "User"},
			{ID: 4, Type: "RepositoryRole"}, // write, or higher
		}}}})

	for _, tc := range []struct {
		name     string
		sender   *store.User
		admitted bool
	}{
		{"a listed user", listed, true},
		{"a holder of the listed role", writer, true},
		{"a holder of a higher role", owner, true},
		{"a holder of a lower role", reader, false},
		{"nobody", nil, false},
	} {
		before := runs()
		s.pushAs(repo.FullName, tc.sender)
		if started := runs() - before; (started == 1) != tc.admitted {
			t.Errorf("a push by %s started %d runs, admitted=%v", tc.name, started, tc.admitted)
		}
	}
}

// TestATeamActorAdmitsItsMembers covers the team actor, which needs an
// organization to live in.
func TestATeamActorAdmitsItsMembers(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	admin := s.store.LookupUserByLogin("admin")
	org := s.store.CreateOrg(admin, "policy-team-org", "Policy Team", "")
	team := s.store.CreateTeam(org.Login, "releasers", store.TeamOptions{})
	member, _ := s.newUser(t, "team-member")
	outsider, _ := s.newUser(t, "team-outsider")
	for _, user := range []*store.User{member, outsider} {
		s.store.SetMembership(org.Login, user.ID, store.OrgRoleMember, store.MembershipStateActive)
	}
	s.store.SetTeamMembership(org.Login, team.Slug, member.ID, store.TeamRoleMember)

	allowed := store.ActionsPolicyActor{ID: team.ID, Type: "Team"}
	if !s.actionsPolicyActorAdmits(allowed, nil, member) {
		t.Error("a member of the listed team was not admitted")
	}
	if s.actionsPolicyActorAdmits(allowed, nil, outsider) {
		t.Error("an organization member outside the listed team was admitted")
	}
}

// TestAnAppActorAdmitsTheAppsBot covers the App actor: an app acts as its bot.
func TestAnAppActorAdmitsTheAppsBot(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	bot := store.AppBotUser(&store.App{ID: 77, Slug: "deployer"})
	if !s.actionsPolicyActorAdmits(store.ActionsPolicyActor{ID: 77, Type: "App"}, nil, bot) {
		t.Error("the listed app's bot was not admitted")
	}
	if s.actionsPolicyActorAdmits(store.ActionsPolicyActor{ID: 78, Type: "App"}, nil, bot) {
		t.Error("another app's bot was admitted")
	}
	// A person whose id happens to equal an app's is not that app.
	if s.actionsPolicyActorAdmits(store.ActionsPolicyActor{ID: 77, Type: "App"}, nil, &store.User{ID: 77, Type: "User"}) {
		t.Error("a user was admitted as an app")
	}
	// The sender of an installation's event round-trips to that bot.
	sender := s.eventSender(map[string]interface{}{"sender": senderPayload(bot, s.baseURL)})
	if sender == nil || sender.ID != bot.ID || sender.Type != "Bot" {
		t.Errorf("an app's event sender resolved to %+v, want the bot", sender)
	}
}

// TestAnEventsPolicyAdmitsOnlyTheEventsItLists covers restrict_action_events on
// every path that creates a run: an event, a dispatch, and the scheduler.
func TestAnEventsPolicyAdmitsOnlyTheEventsItLists(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	repo, runs := policyRepo(t, s, "eventsowner/events-repo")
	owner := s.store.LookupUserByLogin("eventsowner")
	tok := &store.Token{Value: "ghp_eventsowner000000000000000000000000000", UserID: owner.ID, Scopes: "repo,workflow"}
	s.store.Mu.Lock()
	s.store.Tokens[tok.Value] = tok
	s.store.Mu.Unlock()
	dispatch := "/api/v3/repos/" + repo.FullName + "/actions/workflows/gated.yml/dispatches"
	ref := map[string]string{"ref": "main"}

	// Permitted: with no policy, all three start a run.
	before := runs()
	s.pushAs(repo.FullName, owner)
	expectStatus(t, s.post(t, dispatch, tok.Value, ref), http.StatusNoContent, "dispatch with no policy")
	s.actions.FireDueSchedules(time.Date(2026, 6, 12, 0, 0, 0, 0, time.UTC))
	if started := runs() - before; started != 3 {
		t.Fatalf("with no policy %d of the 3 triggers started a run", started)
	}

	policy := s.store.CreateActionsPolicy(&store.ActionsPolicy{RepoID: repo.ID, Name: "manual only", Enforcement: "active",
		Rules: []store.ActionsPolicyRule{{Type: store.ActionsPolicyRuleRestrictEvents, AllowedEvents: []string{"workflow_dispatch"}}}})

	// Refused: a push and the schedule. Permitted: the listed event.
	before = runs()
	s.pushAs(repo.FullName, owner)
	s.actions.FireDueSchedules(time.Date(2026, 6, 13, 0, 0, 0, 0, time.UTC))
	if started := runs() - before; started != 0 {
		t.Fatalf("an events policy listing only workflow_dispatch let %d other runs start", started)
	}
	expectStatus(t, s.post(t, dispatch, tok.Value, ref), http.StatusNoContent, "dispatch, the listed event")
	if started := runs() - before; started != 1 {
		t.Fatalf("the listed event started %d runs, want 1", started)
	}

	// Turned round, the dispatch is refused with a reason, and nothing starts.
	s.store.UpdateActionsPolicy(policy.ID, func(p *store.ActionsPolicy) { p.Rules[0].AllowedEvents = []string{"push"} })
	before = runs()
	refused := s.post(t, dispatch, tok.Value, ref)
	message := decodeBody(t, refused, http.StatusForbidden)["message"]
	if text, _ := message.(string); text == "" || !strings.Contains(text, "manual only") {
		t.Errorf("refusal message = %q, want it to name the policy", message)
	}
	if started := runs() - before; started != 0 {
		t.Fatalf("a refused dispatch started %d runs", started)
	}

	// `evaluate` and `disabled` policies never refuse.
	for _, enforcement := range []string{"evaluate", "disabled"} {
		s.store.UpdateActionsPolicy(policy.ID, func(p *store.ActionsPolicy) { p.Enforcement = enforcement })
		expectStatus(t, s.post(t, dispatch, tok.Value, ref), http.StatusNoContent, "dispatch under an "+enforcement+" policy")
	}

	// A policy scoped to other workflows leaves this one alone.
	s.store.UpdateActionsPolicy(policy.ID, func(p *store.ActionsPolicy) {
		p.Enforcement = "active"
		p.Conditions = &store.ActionsPolicyConditions{WorkflowPath: &store.ActionsPolicyPathCondition{
			Include: []string{".github/workflows/release-*.yml"}, Exclude: []string{}}}
	})
	expectStatus(t, s.post(t, dispatch, tok.Value, ref), http.StatusNoContent, "dispatch of a workflow the policy does not target")
}
