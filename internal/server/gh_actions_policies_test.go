package bleephub

import (
	"net/http"
	"sort"
	"strconv"
	"testing"

	"github.com/e6qu/bleephub/internal/store"
)

func actionsPolicyBody(name, enforcement string, extra map[string]interface{}) map[string]interface{} {
	body := map[string]interface{}{"name": name, "enforcement": enforcement}
	for key, value := range extra {
		body[key] = value
	}
	return body
}

func restrictEventsRule(events ...string) map[string]interface{} {
	return map[string]interface{}{"type": "restrict_action_events", "parameters": map[string]interface{}{"allowed_events": events}}
}

func restrictActorsRule(actors ...map[string]interface{}) map[string]interface{} {
	return map[string]interface{}{"type": "restrict_actions_actors", "parameters": map[string]interface{}{"allowed_actors": actors}}
}

// TestRepoActionsPolicyLifecycle walks a repository policy through its whole
// surface as the repository's administrator: create, list, read, update, delete.
// Every 2xx body is checked against GitHub's schema by the response observer.
func TestRepoActionsPolicyLifecycle(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	admin := s.store.LookupUserByLogin("admin")
	repo := s.store.CreateRepo(admin, "policy-lifecycle", "", true)
	base := "/api/v3/repos/" + repo.FullName + "/actions/policies"

	created := decodeBody(t, s.post(t, base, defaultToken, actionsPolicyBody("release gate", "active", map[string]interface{}{
		"conditions": map[string]interface{}{"workflow_path": map[string]interface{}{
			"include": []string{".github/workflows/release-*.yml"}, "exclude": []string{},
		}},
		"rules": []interface{}{
			restrictEventsRule("workflow_dispatch", "push"),
			restrictActorsRule(map[string]interface{}{"id": admin.ID, "type": "User"}, map[string]interface{}{"id": 5, "type": "RepositoryRole"}),
		},
	})), http.StatusCreated)
	if created["name"] != "release gate" || created["target"] != "actions" ||
		created["source_type"] != "Repository" || created["source"] != repo.FullName || created["enforcement"] != "active" {
		t.Fatalf("created policy = %v", created)
	}
	id := int(created["id"].(float64))
	one := base + "/" + strconv.Itoa(id)
	if href := innerObject(t, innerObject(t, created, "_links"), "self")["href"]; href != s.baseURL+one {
		t.Errorf("self link = %v, want %s", href, s.baseURL+one)
	}
	if rules := created["rules"].([]interface{}); len(rules) != 2 {
		t.Fatalf("created rules = %v", rules)
	}

	// The list is an envelope, leaves the rules out, and paginates.
	listed := decodeBody(t, s.get(t, base+"?per_page=1", defaultToken), http.StatusOK)
	if listed["total_count"] != float64(1) {
		t.Fatalf("list = %v", listed)
	}
	if row := listed["policies"].([]interface{})[0].(map[string]interface{}); row["id"] != float64(id) || row["rules"] != nil {
		t.Errorf("list row = %v, want the policy without its rules", row)
	}

	got := decodeBody(t, s.get(t, one, defaultToken), http.StatusOK)
	path := innerObject(t, innerObject(t, got, "conditions"), "workflow_path")
	if include := path["include"].([]interface{}); len(include) != 1 || include[0] != ".github/workflows/release-*.yml" {
		t.Errorf("stored workflow_path = %v", path)
	}

	// An update changes what it names and keeps the rest — the workflow
	// targeting included, which an omitted workflow_path preserves.
	updated := decodeBody(t, s.put(t, one, defaultToken, map[string]interface{}{
		"enforcement": "evaluate", "conditions": map[string]interface{}{},
	}), http.StatusOK)
	if updated["enforcement"] != "evaluate" || updated["name"] != "release gate" {
		t.Errorf("updated policy = %v", updated)
	}
	keptPath := innerObject(t, innerObject(t, updated, "conditions"), "workflow_path")
	if include := keptPath["include"].([]interface{}); len(include) != 1 || include[0] != ".github/workflows/release-*.yml" {
		t.Errorf("an update that omitted workflow_path changed it to %v", keptPath)
	}
	if rules := updated["rules"].([]interface{}); len(rules) != 2 {
		t.Errorf("an update that omitted rules changed them to %v", rules)
	}

	expectStatus(t, s.delete(t, one, defaultToken), http.StatusNoContent, "delete the policy")
	expectStatus(t, s.get(t, one, defaultToken), http.StatusNotFound, "read the deleted policy")
	expectStatus(t, s.delete(t, one, defaultToken), http.StatusNotFound, "delete it twice")
}

// TestAPolicyWithNoStoredWorkflowConditionReportsAllWorkflows pins the schema's
// note: an omitted condition is reported as the one it means.
func TestAPolicyWithNoStoredWorkflowConditionReportsAllWorkflows(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	repo := s.store.CreateRepo(s.store.LookupUserByLogin("admin"), "policy-default-path", "", false)
	created := decodeBody(t, s.post(t, "/api/v3/repos/"+repo.FullName+"/actions/policies", defaultToken,
		actionsPolicyBody("everything", "disabled", nil)), http.StatusCreated)
	path := innerObject(t, innerObject(t, created, "conditions"), "workflow_path")
	include, exclude := path["include"].([]interface{}), path["exclude"].([]interface{})
	if len(include) != 1 || include[0] != "~ALL" || len(exclude) != 0 {
		t.Fatalf("reported workflow_path = %v, want include [~ALL] and exclude []", path)
	}
	if stored := s.store.GetActionsPolicy(int(created["id"].(float64))); stored.Conditions != nil {
		t.Errorf("stored conditions = %+v, want none: the default is not a stored condition", stored.Conditions)
	}
}

func TestActionsPolicyValidation(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	admin := s.store.LookupUserByLogin("admin")
	repo := s.store.CreateRepo(admin, "policy-validation", "", false)
	org := s.store.CreateOrg(admin, "policy-validation-org", "Policy Validation", "")
	orgRepo := s.store.CreateOrgRepo(org, admin, "inside", "", false)
	repoBase := "/api/v3/repos/" + repo.FullName + "/actions/policies"
	orgBase := "/api/v3/orgs/" + org.Login + "/actions/policies"

	path := func(include, exclude []string) map[string]interface{} {
		return map[string]interface{}{"workflow_path": map[string]interface{}{"include": include, "exclude": exclude}}
	}
	cases := []struct {
		name string
		base string
		body map[string]interface{}
	}{
		{"no name", repoBase, map[string]interface{}{"enforcement": "active"}},
		{"no enforcement", repoBase, map[string]interface{}{"name": "p"}},
		{"unknown enforcement", repoBase, actionsPolicyBody("p", "sometimes", nil)},
		{"unknown rule type", repoBase, actionsPolicyBody("p", "active", map[string]interface{}{
			"rules": []interface{}{map[string]interface{}{"type": "restrict_everything"}}})},
		{"events rule without its parameter", repoBase, actionsPolicyBody("p", "active", map[string]interface{}{
			"rules": []interface{}{map[string]interface{}{"type": "restrict_action_events"}}})},
		{"unknown event", repoBase, actionsPolicyBody("p", "active", map[string]interface{}{
			"rules": []interface{}{restrictEventsRule("full_moon")}})},
		{"unknown actor type", repoBase, actionsPolicyBody("p", "active", map[string]interface{}{
			"rules": []interface{}{restrictActorsRule(map[string]interface{}{"id": 1, "type": "Wizard"})}})},
		{"workflow_path with no pattern", repoBase, actionsPolicyBody("p", "active", map[string]interface{}{
			"conditions": path([]string{}, []string{})})},
		{"~ALL beside another pattern", repoBase, actionsPolicyBody("p", "active", map[string]interface{}{
			"conditions": path([]string{"~ALL", "ci.yml"}, []string{})})},
		{"~ALL excluded", repoBase, actionsPolicyBody("p", "active", map[string]interface{}{
			"conditions": path([]string{"ci.yml"}, []string{"~ALL"})})},
		{"repository selector on a repository policy", repoBase, actionsPolicyBody("p", "active", map[string]interface{}{
			"conditions": map[string]interface{}{"repository_name": map[string]interface{}{"include": []string{"~ALL"}, "exclude": []string{}}}})},
		{"organization policy that selects no repositories", orgBase, actionsPolicyBody("p", "active", nil)},
		{"organization policy with two selectors", orgBase, actionsPolicyBody("p", "active", map[string]interface{}{
			"conditions": map[string]interface{}{
				"repository_name": map[string]interface{}{"include": []string{"~ALL"}, "exclude": []string{}},
				"repository_id":   map[string]interface{}{"repository_ids": []int{orgRepo.ID}},
			}})},
		{"organization policy naming a repository it does not own", orgBase, actionsPolicyBody("p", "active", map[string]interface{}{
			"conditions": map[string]interface{}{"repository_id": map[string]interface{}{"repository_ids": []int{repo.ID}}}})},
	}
	for _, tc := range cases {
		expectStatus(t, s.post(t, tc.base, defaultToken, tc.body), http.StatusUnprocessableEntity, tc.name)
	}
	if total := len(s.store.ListRepoActionsPolicies(repo, true)) + len(s.store.ListOrgActionsPolicies(org.ID)); total != 0 {
		t.Fatalf("%d refused policies were stored", total)
	}

	expectStatus(t, s.get(t, repoBase+"?has_parents=maybe", defaultToken), http.StatusUnprocessableEntity, "unknown has_parents")
	expectStatus(t, s.get(t, repoBase+"?page=0", defaultToken), http.StatusUnprocessableEntity, "page zero")
}

// TestActionsPoliciesAreAdministratorsOnly covers both directions at both
// levels: the administrator is served, and everyone else is turned away without
// learning more than they already could.
func TestActionsPoliciesAreAdministratorsOnly(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	admin := s.store.LookupUserByLogin("admin")
	repo := s.store.CreateRepo(admin, "policy-private", "", true)
	org := s.store.CreateOrg(admin, "policy-authz-org", "Policy Authz", "")
	member, memberToken := s.newUser(t, "policy-member")
	s.store.SetMembership(org.Login, member.ID, store.OrgRoleMember, store.MembershipStateActive)
	_, strangerToken := s.newUser(t, "policy-stranger")

	repoBase := "/api/v3/repos/" + repo.FullName + "/actions/policies"
	orgBase := "/api/v3/orgs/" + org.Login + "/actions/policies"
	body := actionsPolicyBody("gate", "active", nil)
	orgBody := actionsPolicyBody("gate", "active", map[string]interface{}{
		"conditions": map[string]interface{}{"repository_name": map[string]interface{}{"include": []string{"~ALL"}, "exclude": []string{}}}})

	repoPolicy := decodeBody(t, s.post(t, repoBase, defaultToken, body), http.StatusCreated)
	orgPolicy := decodeBody(t, s.post(t, orgBase, defaultToken, orgBody), http.StatusCreated)
	repoOne := repoBase + "/" + strconv.Itoa(int(repoPolicy["id"].(float64)))
	orgOne := orgBase + "/" + strconv.Itoa(int(orgPolicy["id"].(float64)))

	// A stranger cannot tell the private repository exists.
	expectStatus(t, s.get(t, repoBase, strangerToken), http.StatusNotFound, "stranger lists a private repository's policies")
	expectStatus(t, s.post(t, repoBase, strangerToken, body), http.StatusNotFound, "stranger creates a policy")
	expectStatus(t, s.delete(t, repoOne, strangerToken), http.StatusNotFound, "stranger deletes a policy")

	// A plain member knows the organization exists, and is refused.
	expectStatus(t, s.get(t, orgBase, memberToken), http.StatusForbidden, "member lists organization policies")
	expectStatus(t, s.post(t, orgBase, memberToken, orgBody), http.StatusForbidden, "member creates an organization policy")
	expectStatus(t, s.put(t, orgOne, memberToken, map[string]interface{}{"enforcement": "disabled"}), http.StatusForbidden, "member updates an organization policy")
	expectStatus(t, s.delete(t, orgOne, memberToken), http.StatusForbidden, "member deletes an organization policy")
	if stored := s.store.GetActionsPolicy(int(orgPolicy["id"].(float64))); stored == nil || stored.Enforcement != "active" {
		t.Fatalf("a refused write changed the organization policy: %+v", stored)
	}

	// A policy is addressed through its owner: another owner's id is not found.
	expectStatus(t, s.get(t, repoBase+"/"+strconv.Itoa(int(orgPolicy["id"].(float64))), defaultToken),
		http.StatusNotFound, "read an organization policy through a repository")
	expectStatus(t, s.get(t, orgBase+"/"+strconv.Itoa(int(repoPolicy["id"].(float64))), defaultToken),
		http.StatusNotFound, "read a repository policy through an organization")
}

// TestRepositoryPolicyListIncludesTheOrganizationPoliciesThatSelectIt covers
// has_parents and each way an organization policy selects repositories.
func TestRepositoryPolicyListIncludesTheOrganizationPoliciesThatSelectIt(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	admin := s.store.LookupUserByLogin("admin")
	org := s.store.CreateOrg(admin, "policy-parents-org", "Policy Parents", "")
	api := s.store.CreateOrgRepo(org, admin, "api-server", "", false)
	web := s.store.CreateOrgRepo(org, admin, "web-client", "", false)
	orgBase := "/api/v3/orgs/" + org.Login + "/actions/policies"

	create := func(name string, conditions map[string]interface{}) {
		t.Helper()
		decodeBody(t, s.post(t, orgBase, defaultToken, actionsPolicyBody(name, "active", map[string]interface{}{"conditions": conditions})), http.StatusCreated)
	}
	create("by-name", map[string]interface{}{"repository_name": map[string]interface{}{"include": []string{"api-*"}, "exclude": []string{}}})
	create("by-id", map[string]interface{}{"repository_id": map[string]interface{}{"repository_ids": []int{web.ID}}})
	create("all-but-web", map[string]interface{}{"repository_name": map[string]interface{}{"include": []string{"~ALL"}, "exclude": []string{"web-*"}}})
	decodeBody(t, s.post(t, "/api/v3/repos/"+api.FullName+"/actions/policies", defaultToken, actionsPolicyBody("own", "active", nil)), http.StatusCreated)

	names := func(repo *store.Repo, query string) []string {
		t.Helper()
		listed := decodeBody(t, s.get(t, "/api/v3/repos/"+repo.FullName+"/actions/policies"+query, defaultToken), http.StatusOK)
		var out []string
		for _, row := range listed["policies"].([]interface{}) {
			out = append(out, row.(map[string]interface{})["name"].(string))
		}
		sort.Strings(out)
		return out
	}
	if got := names(api, ""); len(got) != 3 || got[0] != "all-but-web" || got[1] != "by-name" || got[2] != "own" {
		t.Errorf("api-server policies = %v, want its own and the two that select it", got)
	}
	if got := names(web, ""); len(got) != 1 || got[0] != "by-id" {
		t.Errorf("web-client policies = %v, want only by-id", got)
	}
	if got := names(api, "?has_parents=false"); len(got) != 1 || got[0] != "own" {
		t.Errorf("api-server policies without parents = %v, want only its own", got)
	}
}

// TestActionsPoliciesSurviveARestartAndDieWithTheirOwner covers persistence and
// both cascades. Ids are never reissued, so a policy that outlived its owner
// would be a row nothing could reach.
func TestActionsPoliciesSurviveARestartAndDieWithTheirOwner(t *testing.T) {
	t.Setenv("BLEEPHUB_PERSIST", "true")
	t.Setenv("BLEEPHUB_DATA_DIR", t.TempDir())
	// reopen attaches a fresh store to the data directory, as a restart does.
	reopen := func() (*store.Store, func()) {
		t.Helper()
		persistence, err := store.NewPersistence()
		if err != nil {
			t.Fatalf("open persistence: %v", err)
		}
		st := store.NewStore()
		if err := st.SetPersistence(persistence); err != nil {
			t.Fatalf("attach persistence: %v", err)
		}
		return st, func() {
			if err := persistence.Close(); err != nil {
				t.Fatalf("close persistence: %v", err)
			}
		}
	}

	first, closeFirst := reopen()
	first.SeedDefaultUser()
	admin := first.LookupUserByLogin("admin")
	repo := first.CreateRepo(admin, "policy-durable", "", false)
	org := first.CreateOrg(admin, "policy-durable-org", "Durable", "")
	repoPolicy := first.CreateActionsPolicy(&store.ActionsPolicy{RepoID: repo.ID, Name: "kept", Enforcement: "active",
		Rules: []store.ActionsPolicyRule{{Type: store.ActionsPolicyRuleRestrictEvents, AllowedEvents: []string{"push"}}}})
	orgPolicy := first.CreateActionsPolicy(&store.ActionsPolicy{OrgID: org.ID, Name: "org-kept", Enforcement: "evaluate"})
	closeFirst()

	second, closeSecond := reopen()
	reloaded := second.GetActionsPolicy(repoPolicy.ID)
	if reloaded == nil || reloaded.Name != "kept" || len(reloaded.Rules) != 1 || reloaded.Rules[0].AllowedEvents[0] != "push" {
		t.Fatalf("reloaded repository policy = %+v", reloaded)
	}
	if next := second.CreateActionsPolicy(&store.ActionsPolicy{RepoID: repo.ID, Name: "next", Enforcement: "active"}); next.ID <= orgPolicy.ID {
		t.Fatalf("a policy created after the restart reused id %d", next.ID)
	}
	if deleted, err := second.DeleteRepo("admin", "policy-durable"); err != nil || !deleted {
		t.Fatalf("delete repository: %v %v", deleted, err)
	}
	if !second.DeleteOrg(org.Login) {
		t.Fatal("delete organization")
	}
	if second.GetActionsPolicy(repoPolicy.ID) != nil || second.GetActionsPolicy(orgPolicy.ID) != nil {
		t.Fatal("a policy outlived the repository or organization that owned it")
	}
	closeSecond()

	third, closeThird := reopen()
	defer closeThird()
	if third.GetActionsPolicy(repoPolicy.ID) != nil || third.GetActionsPolicy(orgPolicy.ID) != nil {
		t.Fatal("a deleted owner's policy came back after a restart")
	}
}
