package bleephub

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

	"github.com/e6qu/bleephub/internal/store"
)

// TestReviewCommentReplyAuthzTargetsInReplyToRepo pins the fix for a
// cross-repository authorization bypass in addPullRequestReviewComment. The
// resolver replies into the thread named by `inReplyTo` when that field is
// present, ignoring `pullRequestId`. The mutation-authz target must mirror that
// precedence: it has to authorize against the repository that owns the
// `inReplyTo` comment, not the (attacker-controlled, accessible) pull request in
// `pullRequestId`. Otherwise an attacker passes a pull request they own to clear
// the gate while the write lands on a private repository's review thread.
func TestReviewCommentReplyAuthzTargetsInReplyToRepo(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)

	// The victim: a private repository whose review comment the attacker will
	// try to reply into. The attacker has no access to it.
	victim := newGQLAuthzFixture(t, s.Server, "reply-victim", true)
	// The attacker: owns a separate pull request, so `pullRequestId` names a
	// subject the attacker is fully entitled to.
	attacker := newGQLAuthzFixture(t, s.Server, "reply-attacker", false)

	root := store.FindPullRequestReviewCommentByNodeID(s.store, victim.reviewCommentNodeID)
	if root == nil {
		t.Fatalf("victim fixture has no seeded review comment")
	}
	before := s.store.PRReviewComments.GetThread(root.ID)
	if before == nil || len(before.Comments) != 1 {
		t.Fatalf("victim thread should start with just its root comment: %+v", before)
	}

	env := s.gqlAuthzPost(t, attacker.ownerToken,
		`mutation($input:AddPullRequestReviewCommentInput!){addPullRequestReviewComment(input:$input){comment{body}}}`,
		map[string]interface{}{"input": map[string]interface{}{
			"pullRequestId": attacker.pr.NodeID,
			"inReplyTo":     victim.reviewCommentNodeID,
			"body":          "smuggled reply into a private repo",
		}})
	if len(gqlAuthzErrors(env)) == 0 {
		t.Fatalf("attacker with no access to the victim repo was served: %v", env)
	}

	after := s.store.PRReviewComments.GetThread(root.ID)
	if after == nil || len(after.Comments) != 1 {
		t.Fatalf("a reply was smuggled into the private thread: %+v", after)
	}
}

// TestArchivedRepoRejectsGitDataWrites pins that an archived repository is
// read-only across the Contents API and every Git Data write handler — GitHub
// returns 403 "Repository was archived so is read-only." for all of them. Prior
// rounds only guarded the issue/comment/PR write paths; the object and ref
// writers reached the store unchecked.
func TestArchivedRepoRejectsGitDataWrites(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	s.post(t, "/api/v3/user/repos", defaultToken, map[string]interface{}{
		"name": "arch-git", "auto_init": true,
	}).Body.Close()
	s.patch(t, "/api/v3/repos/admin/arch-git", defaultToken,
		map[string]interface{}{"archived": true}).Body.Close()

	dummySHA := "0123456789abcdef0123456789abcdef01234567"
	writes := []struct {
		name, method, path string
		body               map[string]interface{}
	}{
		{"put contents", http.MethodPut, "/api/v3/repos/admin/arch-git/contents/new.txt",
			map[string]interface{}{"message": "add", "content": "aGVsbG8="}},
		{"delete contents", http.MethodDelete, "/api/v3/repos/admin/arch-git/contents/README.md",
			map[string]interface{}{"message": "rm", "sha": dummySHA}},
		{"create blob", http.MethodPost, "/api/v3/repos/admin/arch-git/git/blobs",
			map[string]interface{}{"content": "aGVsbG8=", "encoding": "base64"}},
		{"create tree", http.MethodPost, "/api/v3/repos/admin/arch-git/git/trees",
			map[string]interface{}{"tree": []map[string]interface{}{}}},
		{"create commit", http.MethodPost, "/api/v3/repos/admin/arch-git/git/commits",
			map[string]interface{}{"message": "x", "tree": dummySHA}},
		{"create tag", http.MethodPost, "/api/v3/repos/admin/arch-git/git/tags",
			map[string]interface{}{"tag": "v1", "message": "x", "object": dummySHA, "type": "commit"}},
		{"create ref", http.MethodPost, "/api/v3/repos/admin/arch-git/git/refs",
			map[string]interface{}{"ref": "refs/heads/x", "sha": dummySHA}},
		{"update ref", http.MethodPatch, "/api/v3/repos/admin/arch-git/git/refs/heads/main",
			map[string]interface{}{"sha": dummySHA}},
		{"delete ref", http.MethodDelete, "/api/v3/repos/admin/arch-git/git/refs/heads/main", nil},
	}
	for _, wr := range writes {
		resp := s.do(t, wr.method, wr.path, defaultToken, wr.body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusForbidden {
			t.Errorf("%s on archived repo = %d, want 403", wr.name, resp.StatusCode)
		}
	}
}

// TestDeletedPackageVersionIsNotDownloadable pins that a soft-deleted package
// version's files stop being downloadable (404), while the version's metadata
// and the restore path still resolve it — restoring re-enables the download.
func TestDeletedPackageVersionIsNotDownloadable(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	admin := s.store.LookupUserByLogin("admin")
	_, versionID := s.seedPackageVersion(t, "user", admin.Login, "npm", "leftpad", "1.0.0")

	files := s.store.ListPackageFiles(versionID)
	if len(files) == 0 {
		t.Fatalf("seeded version has no files")
	}
	dl := "/ui-data/users/" + admin.Login + "/packages/npm/leftpad/versions/" +
		strconv.Itoa(versionID) + "/files/" + strconv.Itoa(files[0].ID)

	if resp := s.authedGet(t, dl); resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		t.Fatalf("download before delete = %d, want 200", resp.StatusCode)
	} else {
		resp.Body.Close()
	}

	s.delete(t, "/api/v3/user/packages/npm/leftpad/versions/"+strconv.Itoa(versionID), defaultToken).Body.Close()

	if resp := s.authedGet(t, dl); resp.StatusCode != http.StatusNotFound {
		resp.Body.Close()
		t.Fatalf("download of a deleted version = %d, want 404", resp.StatusCode)
	} else {
		resp.Body.Close()
	}

	s.post(t, "/api/v3/user/packages/npm/leftpad/versions/"+strconv.Itoa(versionID)+"/restore", defaultToken, nil).Body.Close()

	if resp := s.authedGet(t, dl); resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		t.Fatalf("download after restore = %d, want 200", resp.StatusCode)
	} else {
		resp.Body.Close()
	}
}

// TestEnterpriseRulesetSkipsUserOwnedRepositories pins that an enterprise-scoped
// ruleset applies to organization-owned repositories but not to personal
// (user-owned) repositories, which are not part of an organization.
func TestEnterpriseRulesetSkipsUserOwnedRepositories(t *testing.T) {
	t.Parallel()
	st := store.NewStore()
	st.SeedDefaultUser()
	admin := st.LookupUserByLogin("admin")

	rs := st.CreateEnterpriseRuleset("bleephub", &store.Ruleset{
		Name:        "enterprise deletion policy",
		Target:      "branch",
		Enforcement: "active",
		Rules:       []store.Rule{{Type: "deletion"}},
	})
	if rs == nil {
		t.Fatalf("could not create enterprise ruleset")
	}

	userRepo := st.CreateRepo(admin, "personal", "", false)
	org := st.CreateOrg(admin, "acme", "Acme", "")
	orgRepo := st.CreateOrgRepo(org, admin, "widget", "", false)
	if userRepo == nil || org == nil || orgRepo == nil {
		t.Fatalf("could not seed repos")
	}

	if got := st.ListRulesetsForRepository(userRepo, true); len(got) != 0 {
		t.Errorf("enterprise ruleset applied to a personal repo: %+v", got)
	}
	if got := st.ListRulesetsForRepository(orgRepo, true); len(got) != 1 {
		t.Errorf("enterprise ruleset did not apply to an org repo: %+v", got)
	}
}

// TestEnterpriseServerStatisticsRejectsDelegatedSiteAdmin pins that the
// instance-wide server-statistics endpoint hides itself from a site admin's
// delegated credentials (a fine-grained PAT), matching the sibling
// /enterprise/stats/* gate. A delegated credential must not read enterprise-wide
// aggregates that the sibling endpoint refuses it.
func TestEnterpriseServerStatisticsRejectsDelegatedSiteAdmin(t *testing.T) {
	s := newTestServer()
	s.registerGHEnterpriseLicensingRoutes()
	admin := s.store.LookupUserByLogin("admin") // SiteAdmin

	fg := s.store.CreateToken(admin.ID, "")
	s.store.Mu.Lock()
	fg.FineGrained = true
	s.store.Mu.Unlock()

	req := httptest.NewRequest(http.MethodGet,
		"/api/v3/enterprise-installation/bleephub/server-statistics", nil)
	req.Header.Set("Authorization", "token "+fg.Value)
	rec := httptest.NewRecorder()
	s.requestHandler().ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("fine-grained PAT reached server-statistics = %d, want 404", rec.Code)
	}
}

// TestEnterpriseServerStatisticsCountsOrgTeamsOnly pins that the ghe_stats
// total_teams metric counts organization teams only, matching the sibling
// admin-stats endpoint. Enterprise teams are a distinct concept and must not
// inflate it.
func TestEnterpriseServerStatisticsCountsOrgTeamsOnly(t *testing.T) {
	s := newTestServer()
	s.registerGHEnterpriseLicensingRoutes()

	if s.store.CreateEnterpriseTeam("platform", "", "manual", nil, "") == nil {
		t.Fatalf("could not seed an enterprise team")
	}
	s.store.Mu.RLock()
	wantOrgTeams := len(s.store.Teams)
	enterpriseTeams := len(s.store.EnterpriseTeams)
	s.store.Mu.RUnlock()
	if enterpriseTeams == 0 {
		t.Fatalf("test needs at least one enterprise team to be meaningful")
	}

	rec := enterpriseActionsRequest(t, s, http.MethodGet,
		"/api/v3/enterprise-installation/bleephub/server-statistics", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("server-statistics = %d, want 200", rec.Code)
	}
	var stats []map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &stats); err != nil || len(stats) != 1 {
		t.Fatalf("decode server-statistics %q: %v", rec.Body.String(), err)
	}
	orgs := stats[0]["ghe_stats"].(map[string]interface{})["orgs"].(map[string]interface{})
	if got := int(orgs["total_teams"].(float64)); got != wantOrgTeams {
		t.Fatalf("total_teams = %d, want %d (org teams only, excluding %d enterprise teams)",
			got, wantOrgTeams, enterpriseTeams)
	}
}
