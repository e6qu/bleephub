package bleephub

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/e6qu/bleephub/internal/store"
)

// TestUserToServerTokenCannotEscalateAcrossInstallations pins that a ghu_
// user-to-server token's permission on a repo is bounded by THAT repo's
// installation. An app installed on the owner (issues:write) and on an org
// (metadata:read only) must not let the token write issues on the org's repos —
// permission and reach were previously decided by two independent installation
// scans, so a grant on one installation authorized another's repos.
func TestUserToServerTokenCannotEscalateAcrossInstallations(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	f := s.newCredGrantFixture(t, "escalate")
	st := s.store

	app := st.CreateApp(f.owner.ID, "Cross Install Escalation", "",
		map[string]string{"metadata": "read", "issues": "write"}, nil)
	if app == nil {
		t.Fatal("could not register the app")
	}
	// The owner (User) installation grants issues:write; the org installation only metadata:read.
	if st.CreateInstallation(app.ID, "User", f.owner.ID, f.owner.Login,
		map[string]string{"metadata": "read", "issues": "write"}, nil) == nil {
		t.Fatal("could not install on the owner")
	}
	if st.CreateInstallation(app.ID, "Organization", f.org.ID, f.org.Login,
		map[string]string{"metadata": "read"}, nil) == nil {
		t.Fatal("could not install on the org")
	}
	uts, _ := st.CreateUserToServerToken(f.owner.ID, app.ID, "", "", time.Hour, false)
	if uts == nil {
		t.Fatal("could not mint the ghu_ token")
	}
	caller := credGrantCaller{srv: s, name: "ghu_ across two installs", token: uts.Token}
	body := `{"title":"escalated"}`

	// The org repo's installation grants only metadata:read → issue create is refused
	// (404: an under-privileged integration is not told the repo exists), even though a
	// DIFFERENT installation of the same app was granted issues:write. Pre-fix: 201.
	orgRepo := st.CreateOrgRepo(f.org, f.owner, "org-target", "", true)
	if orgRepo == nil {
		t.Fatal("could not create the org repo")
	}
	if status, resp := caller.do(t, http.MethodPost, "/api/v3/repos/"+orgRepo.FullName+"/issues", body); status != http.StatusNotFound {
		t.Fatalf("issue create on org repo (its installation grants only metadata:read) = %d, want 404 (denied); body=%s", status, resp)
	}

	// Control: the owner installation covers the owner's repo AND grants issues:write.
	ownRepo := f.repo(t, "own-target")
	if status, resp := caller.do(t, http.MethodPost, "/api/v3/repos/"+ownRepo.FullName+"/issues", body); status != http.StatusCreated {
		t.Fatalf("issue create on owner repo (its installation grants issues:write) = %d, want 201; body=%s", status, resp)
	}
}

// TestUserInstallationReposIntersectUserAccess pins that
// GET /user/installations/{id}/repositories returns only repos the calling user
// can actually access within the installation, not every repo the installation
// covers — otherwise a plain org member enumerates all private org repos.
func TestUserInstallationReposIntersectUserAccess(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	st := s.store
	admin := st.LookupUserByLogin("admin")

	org := st.CreateOrg(admin, "inst-repos-org", "inst-repos-org", "")
	if org == nil {
		t.Fatal("could not create org")
	}
	// Members get no base access to private repos.
	st.UpdateOrg(org.Login, func(o *store.Org) { o.DefaultRepositoryPermission = "none" })
	pub := st.CreateOrgRepo(org, admin, "public-repo", "", false)
	secret := st.CreateOrgRepo(org, admin, "secret-repo", "", true)
	if pub == nil || secret == nil {
		t.Fatal("could not create org repos")
	}

	member, memberToken := s.userSurfaceUser(t, "inst-repos-member")
	st.SetMembership(org.Login, member.ID, store.OrgRoleMember, store.MembershipStateActive)

	app := st.CreateApp(admin.ID, "Inst Repos App", "", map[string]string{"metadata": "read"}, nil)
	inst := st.CreateInstallation(app.ID, "Organization", org.ID, org.Login, map[string]string{"metadata": "read"}, nil)
	if inst == nil {
		t.Fatal("could not install on the org")
	}

	resp := s.get(t, "/api/v3/user/installations/"+itoa(inst.ID)+"/repositories", memberToken)
	requireStatusNoClose(t, resp, 200)
	var out struct {
		TotalCount   int                      `json:"total_count"`
		Repositories []map[string]interface{} `json:"repositories"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	resp.Body.Close()

	names := map[string]bool{}
	for _, r := range out.Repositories {
		names[r["name"].(string)] = true
	}
	if !names["public-repo"] {
		t.Fatalf("public repo missing from installation repos: %v", names)
	}
	if names["secret-repo"] {
		t.Fatalf("private org repo leaked to a member with no read access: %v", names)
	}
}

// TestCombinedStatusDeterministicOnEqualTimestamps pins that when two statuses on
// the same context share a CreatedAt (frozen test clock, rapid POSTs), the LATEST
// (higher id) one wins the latest-per-context rollup — the sort had no id
// tiebreaker, so the combined state was nondeterministic.
func TestCombinedStatusDeterministicOnEqualTimestamps(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	css := s.store.CommitStatuses
	repoKey, sha := "octo/repo", "deadbeef"

	// Two statuses on the same context; Create returns the live stored pointer, so
	// force identical CreatedAt to exercise the id tiebreaker. The later (success) wins.
	a := css.Create(repoKey, sha, 1, "failure", "", "first", "ci")
	b := css.Create(repoKey, sha, 1, "success", "", "second", "ci")
	b.CreatedAt = a.CreatedAt

	for i := 0; i < 20; i++ {
		state, total, _ := css.Combined(repoKey, sha)
		if total != 1 {
			t.Fatalf("combined listed %d contexts, want 1 (latest-per-context)", total)
		}
		if state != "success" {
			t.Fatalf("combined state = %q, want success (the later same-instant status wins)", state)
		}
	}
}

// TestRulesetRequiredCheckUsesLatestRunPerName pins that a required status check
// is satisfied by the LATEST run for its name (a re-run supersedes the old one),
// not a nondeterministic first-seen run pulled from a map.
func TestRulesetRequiredCheckUsesLatestRunPerName(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	st := s.store
	admin := st.LookupUserByLogin("admin")
	repo := st.CreateRepo(admin, "ruleset-checks", "", false)
	if repo == nil {
		t.Fatal("could not create repo")
	}
	sha := "cafebabe"

	// An old failing run, then a newer passing run, for the same check name.
	old := st.CreateCheckRun(repo.FullName, sha, "ci/test", 0, 0)
	st.UpdateCheckRun(old.ID, func(cr *store.CheckRun) { cr.Status = "completed"; cr.Conclusion = "failure" })
	fresh := st.CreateCheckRun(repo.FullName, sha, "ci/test", 0, 0)
	st.UpdateCheckRun(fresh.ID, func(cr *store.CheckRun) { cr.Status = "completed"; cr.Conclusion = "success" })

	params := map[string]interface{}{"required_status_checks": []interface{}{"ci/test"}}
	// Loop: the buggy first-seen rollup pulled a random run from a map, so the old
	// failure would satisfy/miss the check about half the time.
	for i := 0; i < 50; i++ {
		if missing := s.missingRulesetStatusChecks(repo, sha, params); len(missing) != 0 {
			t.Fatalf("iteration %d: required check reported missing %v; the latest run for ci/test is success", i, missing)
		}
	}
}
