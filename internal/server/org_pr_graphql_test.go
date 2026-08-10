package bleephub

import (
	"testing"
)

func TestPRGraphQL_OrgOwnedHeadRepositoryOwner(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	admin := s.store.UsersByLogin["admin"]
	orgLogin := "sweep-org-head-owner"
	org := s.store.CreateOrg(admin, orgLogin, "Sweep Org Head Owner", "")
	if org == nil {
		t.Fatal("failed to create org")
	}
	defer func() {
		s.store.mu.Lock()
		delete(s.store.Orgs, org.ID)
		delete(s.store.OrgsByLogin, orgLogin)
		delete(s.store.Memberships, membershipKey(orgLogin, admin.ID))
		s.store.mu.Unlock()
	}()

	repo := s.store.CreateOrgRepo(org, admin, "sweep-org-repo", "", false)
	if repo == nil {
		t.Fatal("failed to create org repo")
	}
	seedPullRequestBranches(t, s.Server, repo, "feature")
	defer func() {
		if _, err := s.store.DeleteRepo(orgLogin, "sweep-org-repo"); err != nil {
			t.Fatalf("DeleteRepo: %v", err)
		}
	}()

	prNum, _ := s.sweepPR(t, repoRef{owner: orgLogin, name: "sweep-org-repo"}, "org pr")

	query := `query PR($owner: String!, $repo: String!, $num: Int!) {
		repository(owner: $owner, name: $repo) {
			pullRequest(number: $num) {
				headRepositoryOwner { login }
				headRepository { nameWithOwner }
			}
		}
	}`
	d := s.gqlData(t, query, map[string]interface{}{"owner": orgLogin, "repo": "sweep-org-repo", "num": prNum})
	repoData, _ := d["repository"].(map[string]interface{})
	prData, _ := repoData["pullRequest"].(map[string]interface{})
	hro, _ := prData["headRepositoryOwner"].(map[string]interface{})
	if hro == nil || hro["login"] != orgLogin {
		t.Errorf("headRepositoryOwner.login = %v, want %q", prData["headRepositoryOwner"], orgLogin)
	}
}
