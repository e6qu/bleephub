package bleephub

import (
	"net/http"
	"strings"
	"testing"
)

func TestBranchProtectionStateAcrossBranchAPIs(t *testing.T) {
	const repoName = "branch-protection-state-fidelity"
	create := ghPost(t, "/api/v3/user/repos", defaultToken, map[string]interface{}{
		"name": repoName, "auto_init": true,
	})
	requireStatus(t, create, http.StatusCreated)
	create.Body.Close()

	repo := testServer.store.GetRepo("admin", repoName)
	if repo == nil {
		t.Fatal("created repository missing from store")
	}
	shas := seedPullRequestBranches(t, testServer, repo, "policy", "plain")
	testServer.store.CreateRuleset(repo, &Ruleset{
		Name:        "protect-policy",
		Target:      "branch",
		Enforcement: "active",
		Conditions: RulesetConditions{RefName: RefNameCondition{
			Include: []string{"refs/heads/policy"},
		}},
		Rules: []Rule{{Type: "required_linear_history"}},
	})

	base := "/api/v3/repos/admin/" + repoName
	protection := ghPut(t, base+"/branches/main/protection", defaultToken, map[string]interface{}{
		"required_status_checks": map[string]interface{}{
			"strict": true, "contexts": []string{"ci"},
		},
		"required_pull_request_reviews": nil,
		"enforce_admins":                false,
		"restrictions":                  nil,
	})
	requireStatus(t, protection, http.StatusOK)
	protection.Body.Close()

	assertBranch := func(name string, wantProtected bool) map[string]interface{} {
		t.Helper()
		branch := decodeJSONWithStatus(t, ghGet(t, base+"/branches/"+name, defaultToken), http.StatusOK)
		if branch["protected"] != wantProtected {
			t.Errorf("%s protected = %v, want %v", name, branch["protected"], wantProtected)
		}
		for _, field := range []string{"_links", "protection", "protection_url"} {
			if branch[field] == nil {
				t.Errorf("%s omitted required field %q: %#v", name, field, branch)
			}
		}
		if raw, _ := branch["protection_url"].(string); !strings.Contains(raw, "/api/v3/repos/admin/"+repoName+"/branches/"+name+"/protection") {
			t.Errorf("%s protection_url = %q", name, raw)
		}
		return branch
	}

	main := assertBranch("main", true)
	mainProtection, _ := main["protection"].(map[string]interface{})
	if mainProtection["enabled"] != true {
		t.Errorf("main protection.enabled = %v, want true", mainProtection["enabled"])
	}
	assertBranch("policy", true)
	plain := assertBranch("plain", false)
	plainProtection, _ := plain["protection"].(map[string]interface{})
	if plainProtection["enabled"] != false {
		t.Errorf("plain protection.enabled = %v, want false", plainProtection["enabled"])
	}

	protectedBranches := decodeJSONArray(t, ghGet(t, base+"/branches?protected=true", defaultToken))
	assertBranchNames(t, protectedBranches, "main", "policy")
	unprotectedBranches := decodeJSONArray(t, ghGet(t, base+"/branches?protected=false", defaultToken))
	assertBranchNames(t, unprotectedBranches, "plain")
	requireStatus(t, ghGet(t, base+"/branches?protected=yes", defaultToken), http.StatusUnprocessableEntity)

	whereHead := decodeJSONArray(t, ghGet(t, base+"/commits/"+shas["main"]+"/branches-where-head", defaultToken))
	assertBranchNames(t, whereHead, "main")
	if whereHead[0]["protected"] != true {
		t.Errorf("branches-where-head protected = %v, want true", whereHead[0]["protected"])
	}

	renamed := decodeJSONWithStatus(t, ghPost(t, base+"/branches/main/rename", defaultToken, map[string]interface{}{
		"new_name": "trunk",
	}), http.StatusCreated)
	if renamed["protected"] != true {
		t.Errorf("renamed branch protected = %v, want true", renamed["protected"])
	}
	requireStatus(t, ghGet(t, base+"/branches/main/protection", defaultToken), http.StatusNotFound)
	requireStatus(t, ghGet(t, base+"/branches/trunk/protection", defaultToken), http.StatusOK)
	assertBranch("trunk", true)

	requireStatus(t, ghDelete(t, base+"/branches/trunk/protection", defaultToken), http.StatusNoContent)
	assertBranch("trunk", false)
}

func assertBranchNames(t *testing.T, branches []map[string]interface{}, want ...string) {
	t.Helper()
	got := make([]string, 0, len(branches))
	for _, branch := range branches {
		name, _ := branch["name"].(string)
		got = append(got, name)
	}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("branch names = %v, want %v", got, want)
	}
}

func TestBranchProtectedByRepositoryAndOrganizationRulesets(t *testing.T) {
	s := newTestServer()
	admin := s.store.UsersByLogin["admin"]
	userRepo := s.store.CreateRepo(admin, "ruleset-protection-user", "", false)

	s.store.CreateRuleset(userRepo, &Ruleset{
		Name:        "evaluate-only",
		Target:      "branch",
		Enforcement: "evaluate",
		Conditions: RulesetConditions{RefName: RefNameCondition{
			Include: []string{"refs/heads/main"},
		}},
		Rules: []Rule{{Type: "required_linear_history"}},
	})
	if s.store.BranchProtectedByRuleset(userRepo, "main") {
		t.Fatal("an evaluate-only ruleset must not mark a branch protected")
	}

	org := s.store.CreateOrg(admin, "branch-rules-org", "Branch Rules", "")
	orgRepo := s.store.CreateOrgRepo(org, admin, "service", "", false)
	s.store.CreateOrgRuleset(
		org.ID,
		"protect-release",
		"branch",
		"active",
		RulesetConditions{RefName: RefNameCondition{Include: []string{"release/*"}}},
		[]Rule{{Type: "required_status_checks"}},
	)
	if !s.store.BranchProtectedByRuleset(orgRepo, "release/v1") {
		t.Fatal("active organization ruleset did not protect a matching repository branch")
	}
	if s.store.BranchProtectedByRuleset(orgRepo, "main") {
		t.Fatal("organization ruleset protected a non-matching branch")
	}
}
