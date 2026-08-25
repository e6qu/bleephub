package bleephub

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"testing"
)

// Conformance regressions found by driving real, unmodified GitHub clients
// (go-github, octokit.js, PyGithub, git, gh) against a running bleephub. Each
// test here pins one of those defects closed against the vendored contract.

// simpleUserHypermedia is every member the `simple-user` schema declares
// required with format: uri. A relative value satisfies neither: PyGithub
// builds NamedUser.get_repos() by resolving the object's own `url` against the
// client base, so "/api/v3/users/admin" made it request
// <base>/api/v3/api/v3/users/admin/repos.
var simpleUserHypermedia = []string{
	"url", "html_url", "avatar_url", "followers_url", "following_url",
	"gists_url", "starred_url", "subscriptions_url", "organizations_url",
	"repos_url", "events_url", "received_events_url",
}

func assertAbsoluteUserHypermedia(t *testing.T, where string, user map[string]interface{}, base string) {
	t.Helper()
	if user == nil {
		t.Fatalf("%s: no user object", where)
	}
	for _, member := range simpleUserHypermedia {
		raw, present := user[member]
		if !present {
			t.Errorf("%s: %s is absent, but simple-user declares it required", where, member)
			continue
		}
		value, _ := raw.(string)
		if !strings.HasPrefix(value, base+"/") {
			t.Errorf("%s: %s = %q, want an absolute URL under %s", where, member, value, base)
		}
	}
}

// TestUserHypermediaIsAbsoluteEverywhere pins the `simple-user` hypermedia
// contract on every shape that renders a user: the two dedicated user
// endpoints, a repository's nested owner, and an issue's author.
func TestUserHypermediaIsAbsoluteEverywhere(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	repo := s.createTestRepo(t)
	number, _ := s.createIssueForTest(t, repo, "hypermedia")

	for _, probe := range []struct {
		name string
		path string
		pick func(map[string]interface{}) map[string]interface{}
	}{
		{"GET /users/{username}", "/api/v3/users/admin", func(m map[string]interface{}) map[string]interface{} { return m }},
		{"GET /user", "/api/v3/user", func(m map[string]interface{}) map[string]interface{} { return m }},
		{"repository owner", repo.path(), func(m map[string]interface{}) map[string]interface{} {
			owner, _ := m["owner"].(map[string]interface{})
			return owner
		}},
		{"issue user", repo.path() + "/issues/" + strconv.Itoa(number), func(m map[string]interface{}) map[string]interface{} {
			user, _ := m["user"].(map[string]interface{})
			return user
		}},
	} {
		body := decodeJSONWithStatus(t, s.authedGet(t, probe.path), http.StatusOK)
		assertAbsoluteUserHypermedia(t, probe.name, probe.pick(body), s.baseURL)
	}
}

// TestUserAvatarURLResolvesToAnImage pins the avatar half of the same defect:
// `avatar_url` is required, so an account with no stored avatar still has to
// name one, and the address it names has to serve an image.
func TestUserAvatarURLResolvesToAnImage(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)

	body := decodeJSONWithStatus(t, s.authedGet(t, "/api/v3/users/admin"), http.StatusOK)
	avatar, _ := body["avatar_url"].(string)
	if avatar == "" {
		t.Fatal("avatar_url is empty; simple-user declares it required with format: uri")
	}
	if !strings.HasPrefix(avatar, s.baseURL+"/avatars/u/") {
		t.Fatalf("avatar_url = %q, want an instance-hosted /avatars/u/ address", avatar)
	}

	resp := s.authedGet(t, strings.TrimPrefix(avatar, s.baseURL))
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s = %d, want 200", avatar, resp.StatusCode)
	}
	if got := resp.Header.Get("Content-Type"); got != "image/png" {
		t.Fatalf("avatar Content-Type = %q, want image/png", got)
	}
	png := make([]byte, 8)
	if _, err := resp.Body.Read(png); err != nil {
		t.Fatalf("reading avatar bytes: %v", err)
	}
	if !bytes.Equal(png, []byte{0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a}) {
		t.Fatalf("avatar body does not start with the PNG signature: %v", png)
	}
}

// TestRenamedRepositoryRedirects pins the documented moved_permanently
// response: `GET /repos/{owner}/{repo}` declares a 301 for a repository that
// moved, and bleephub answered 404, which breaks every stored URL on a rename.
// The redirect covers the whole sub-resource tree, because the repository is
// what moved.
func TestRenamedRepositoryRedirects(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	repo := s.createTestRepo(t)
	oldPath := repo.path()

	resp := s.patch(t, oldPath, defaultToken, map[string]interface{}{"name": repo.name + "-renamed"})
	decodeJSONWithStatus(t, resp, http.StatusOK)
	newFull := repo.owner + "/" + repo.name + "-renamed"

	for _, sub := range []string{"", "/issues", "/branches"} {
		req, err := http.NewRequest(http.MethodGet, s.baseURL+oldPath+sub, nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+defaultToken)
		// The redirect itself is the assertion, so do not let the client follow it.
		client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		}}
		moved, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		location := moved.Header.Get("Location")
		_ = moved.Body.Close()
		if moved.StatusCode != http.StatusMovedPermanently {
			t.Fatalf("GET %s = %d, want 301", oldPath+sub, moved.StatusCode)
		}
		if want := s.baseURL + "/api/v3/repos/" + newFull + sub; location != want {
			t.Fatalf("Location = %q, want %q", location, want)
		}
	}

	// A client that follows the redirect lands on the renamed repository.
	body := decodeJSONWithStatus(t, s.authedGet(t, oldPath), http.StatusOK)
	if body["full_name"] != newFull {
		t.Fatalf("followed redirect resolved to %v, want %s", body["full_name"], newFull)
	}
}

// TestRenamedRepositoryRedirectsGitTransport pins the git half: a clone made
// before a rename fetches through its stale remote, which is the one thing a
// rename must not break.
func TestRenamedRepositoryRedirectsGitTransport(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	repo := s.createTestRepo(t)

	resp := s.patch(t, repo.path(), defaultToken, map[string]interface{}{"name": repo.name + "-moved"})
	decodeJSONWithStatus(t, resp, http.StatusOK)

	for _, service := range []string{"git-upload-pack", "git-receive-pack"} {
		stale := s.baseURL + "/" + repo.fullName() + ".git/info/refs?service=" + service
		req, err := http.NewRequest(http.MethodGet, stale, nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+defaultToken)
		client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		}}
		moved, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		location := moved.Header.Get("Location")
		_ = moved.Body.Close()
		if moved.StatusCode != http.StatusMovedPermanently {
			t.Fatalf("%s through the old remote = %d, want 301", service, moved.StatusCode)
		}
		// Same-origin, so the Location is a path: it names the repository's new
		// full name, the same smart-HTTP endpoint, and the same service, with
		// nothing taken from the request's host.
		want := "/" + repo.owner + "/" + repo.name + "-moved.git/info/refs?service=" + service
		if location != want {
			t.Fatalf("Location = %q, want %q", location, want)
		}
		// Following it reaches the moved repository, which is the only thing
		// the redirect is for.
		followed, err := client.Get(s.baseURL + location)
		if err != nil {
			t.Fatal(err)
		}
		followedStatus := followed.StatusCode
		_ = followed.Body.Close()
		// git's ref advertisement needs a credential; unauthenticated it is
		// refused, but it must not be a 404 — the redirect target has to exist.
		if followedStatus == http.StatusNotFound {
			t.Fatalf("following Location %q = 404, so the redirect points at nothing", location)
		}
	}
}

// TestTransferredRepositoryRedirects pins the transfer half of the same
// record: a repository that changed owner leaves the same redirect behind.
func TestTransferredRepositoryRedirects(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	org := s.createTestOrg(t)
	repo := s.createTestRepo(t)
	oldPath := repo.path()

	resp := s.post(t, oldPath+"/transfer", defaultToken, map[string]interface{}{"new_owner": org})
	if resp.StatusCode != http.StatusAccepted && resp.StatusCode != http.StatusOK {
		t.Fatalf("transfer = %d", resp.StatusCode)
	}
	_ = resp.Body.Close()

	body := decodeJSONWithStatus(t, s.authedGet(t, oldPath), http.StatusOK)
	if body["full_name"] != org+"/"+repo.name {
		t.Fatalf("old owner path resolved to %v, want %s/%s", body["full_name"], org, repo.name)
	}
}

// TestRepositoryRedirectRetiresWhenTheNameIsTakenAgain pins the other half of
// the rule: a former name that something live occupies again is not a name
// that redirects.
func TestRepositoryRedirectRetiresWhenTheNameIsTakenAgain(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	repo := s.createTestRepo(t)
	oldName := repo.name

	resp := s.patch(t, repo.path(), defaultToken, map[string]interface{}{"name": oldName + "-away"})
	decodeJSONWithStatus(t, resp, http.StatusOK)

	created := s.post(t, "/api/v3/user/repos", defaultToken, map[string]interface{}{"name": oldName})
	decodeJSONWithStatus(t, created, http.StatusCreated)

	body := decodeJSONWithStatus(t, s.authedGet(t, repo.path()), http.StatusOK)
	if body["full_name"] != repo.fullName() {
		t.Fatalf("reused name resolved to %v, want the new repository %s", body["full_name"], repo.fullName())
	}
}

// TestEmptyObjectResponsesCarryAnEmptyObject pins the operations whose
// documented response is `application/json` with schema `empty-object`. A
// zero-length body is not an empty object: `gh run cancel` and `gh run rerun`
// decode the body and abort with "unexpected end of JSON input".
//
// The three run-control operations here are the ones the audit found writing a
// bare status line; the codespaces secret PUTs are covered by
// TestSecretUpsertReportsCreatedWithAnEmptyObject.
func TestEmptyObjectResponsesCarryAnEmptyObject(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)

	for _, op := range []struct {
		name   string
		suffix string
		want   int
	}{
		{"cancel", "/cancel", http.StatusAccepted},
		{"rerun", "/rerun", http.StatusCreated},
		{"rerun-failed-jobs", "/rerun-failed-jobs", http.StatusCreated},
	} {
		repoKey := "octo/empty-object-" + op.name
		wf := s.seedRerunRepo(t, repoKey, twoJobYAML)
		path := "/api/v3/repos/" + repoKey + "/actions/runs/" + strconv.Itoa(wf.RunID) + op.suffix
		resp := s.post(t, path, defaultToken, nil)
		body := decodeJSONWithStatus(t, resp, op.want)
		if len(body) != 0 {
			t.Errorf("%s body = %v, want the documented empty object", op.name, body)
		}
	}
}

// TestSecretUpsertReportsCreatedWithAnEmptyObject pins the other half of the
// empty-object audit: every ".../secrets/{secret_name}" PUT declares 201 with
// schema empty-object for a new secret and 204 for a replaced one. The
// codespaces secret endpoints answered 204 for both, so a client could not tell
// a create from an update and never saw the documented 201 body.
func TestSecretUpsertReportsCreatedWithAnEmptyObject(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	repo := s.createTestRepo(t)
	org := s.createTestOrg(t)

	for _, path := range []string{
		"/api/v3/user/codespaces/secrets/CONFORMANCE",
		repo.path() + "/codespaces/secrets/CONFORMANCE",
		"/api/v3/orgs/" + org + "/codespaces/secrets/CONFORMANCE",
	} {
		created := s.putSealedSecret(t, path, "first")
		body := decodeJSONWithStatus(t, created, http.StatusCreated)
		if len(body) != 0 {
			t.Errorf("PUT %s (new) body = %v, want the documented empty object", path, body)
		}
		replaced := s.putSealedSecret(t, path, "second")
		statusCode := replaced.StatusCode
		_ = replaced.Body.Close()
		if statusCode != http.StatusNoContent {
			t.Errorf("PUT %s (replace) = %d, want 204", path, statusCode)
		}
	}
}

// TestStatusCheckPolicyKeepsContextsAndChecksInStep pins the status-check-policy
// contract: the schema requires BOTH `contexts` and `checks`, and they are two
// views of one set. bleephub stored only `checks`, so `contexts` and the
// /contexts sub-resource read back empty for a policy written through `checks`.
func TestStatusCheckPolicyKeepsContextsAndChecksInStep(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	repo := s.createRepoWriteRepo(t, true)
	base := "/api/v3/repos/admin/" + repo + "/branches/main/protection"

	// Written through `checks`: `contexts` names the same set.
	resp := s.put(t, base, defaultToken, map[string]interface{}{
		"required_status_checks": map[string]interface{}{
			"strict": true,
			"checks": []map[string]interface{}{{"context": "ci/one"}, {"context": "ci/two"}},
		},
	})
	decodeJSONWithStatus(t, resp, http.StatusOK)

	policy := decodeJSONWithStatus(t, s.get(t, base+"/required_status_checks", defaultToken), http.StatusOK)
	assertStringSet(t, "contexts after a checks write", policy["contexts"], "ci/one", "ci/two")
	assertContextsSubresource(t, s, base, "ci/one", "ci/two")

	// Written through `contexts`: `checks` names the same set.
	resp = s.patch(t, base+"/required_status_checks", defaultToken, map[string]interface{}{
		"contexts": []string{"ci/three"},
	})
	patched := decodeJSONWithStatus(t, resp, http.StatusOK)
	assertStringSet(t, "contexts after a contexts write", patched["contexts"], "ci/three")
	checks, _ := patched["checks"].([]interface{})
	if len(checks) != 1 {
		t.Fatalf("checks after a contexts write = %v, want one entry", patched["checks"])
	}
	if got := checks[0].(map[string]interface{})["context"]; got != "ci/three" {
		t.Fatalf("checks[0].context = %v, want ci/three", got)
	}

	// POST to the sub-resource returns the whole set, not just what was added.
	added := decodeJSONArrayValues(t, s.post(t, base+"/required_status_checks/contexts", defaultToken,
		map[string]interface{}{"contexts": []string{"ci/four"}}))
	assertStringSet(t, "POST /contexts response", added, "ci/three", "ci/four")
	policy = decodeJSONWithStatus(t, s.get(t, base+"/required_status_checks", defaultToken), http.StatusOK)
	assertStringSet(t, "checks after a /contexts POST", checkContexts(policy), "ci/three", "ci/four")
}

// checkContexts projects the `checks` view of a status-check-policy body onto
// the names it requires, so it can be compared with the `contexts` view.
func checkContexts(policy map[string]interface{}) interface{} {
	raw, _ := policy["checks"].([]interface{})
	out := make([]interface{}, 0, len(raw))
	for _, entry := range raw {
		object, _ := entry.(map[string]interface{})
		out = append(out, object["context"])
	}
	return out
}

func assertStringSet(t *testing.T, where string, got interface{}, want ...string) {
	t.Helper()
	values, _ := got.([]interface{})
	if len(values) != len(want) {
		t.Fatalf("%s = %v, want %v", where, got, want)
	}
	for i, value := range values {
		if value != want[i] {
			t.Fatalf("%s = %v, want %v", where, got, want)
		}
	}
}

func assertContextsSubresource(t *testing.T, s *isolatedServer, base string, want ...string) {
	t.Helper()
	assertStringSet(t, "GET /required_status_checks/contexts",
		decodeJSONArrayValues(t, s.get(t, base+"/required_status_checks/contexts", defaultToken)), want...)
}

func decodeJSONArrayValues(t *testing.T, resp *http.Response) interface{} {
	t.Helper()
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var out []interface{}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decoding array body: %v", err)
	}
	return out
}

// TestDisablingOneProtectionRuleKeepsTheBranchProtected pins the security half
// of the branch-protection contract: DELETE .../protection/enforce_admins is
// documented only to disable admin enforcement. It was dropping the entire
// protection record, so an admin turning off one toggle silently removed every
// other rule — including the review requirement and the push restrictions.
//
// The sibling sub-resource DELETEs are covered by the same assertion, because
// they shared the defect: protection is established by PUT .../protection and
// removed by DELETE .../protection, never as a side effect of turning one rule
// off.
func TestDisablingOneProtectionRuleKeepsTheBranchProtected(t *testing.T) {
	t.Parallel()

	for _, rule := range []string{
		"enforce_admins",
		"required_status_checks",
		"required_pull_request_reviews",
		"restrictions",
	} {
		s := newIsolatedServer(t)
		repo := s.createRepoWriteRepo(t, true)
		base := "/api/v3/repos/admin/" + repo + "/branches/main/protection"
		resp := s.put(t, base, defaultToken, map[string]interface{}{
			"required_status_checks":        map[string]interface{}{"strict": true, "contexts": []string{"ci"}},
			"enforce_admins":                true,
			"required_pull_request_reviews": map[string]interface{}{"required_approving_review_count": 2},
			"restrictions": map[string]interface{}{
				"users": []map[string]interface{}{{"login": "admin", "id": 1, "type": "User"}},
			},
		})
		decodeJSONWithStatus(t, resp, http.StatusOK)

		removed := s.delete(t, base+"/"+rule, defaultToken)
		if removed.StatusCode != http.StatusNoContent {
			t.Fatalf("DELETE /%s = %d, want 204", rule, removed.StatusCode)
		}
		_ = removed.Body.Close()

		protection := decodeJSONWithStatus(t, s.get(t, base, defaultToken), http.StatusOK)
		if rule != "required_pull_request_reviews" && protection["required_pull_request_reviews"] == nil {
			t.Errorf("DELETE /%s also removed the review requirement", rule)
		}
		if rule != "restrictions" && protection["restrictions"] == nil {
			t.Errorf("DELETE /%s also removed the push restrictions", rule)
		}
		if rule != "required_status_checks" && protection["required_status_checks"] == nil {
			t.Errorf("DELETE /%s also removed the required status checks", rule)
		}

		// Admin enforcement is a toggle, not a rule that can be absent: after
		// DELETE the resource still answers, reporting that it is off.
		enforcement := decodeJSONWithStatus(t, s.get(t, base+"/enforce_admins", defaultToken), http.StatusOK)
		wantEnabled := rule != "enforce_admins"
		if enforcement["enabled"] != wantEnabled {
			t.Errorf("after DELETE /%s, enforce_admins.enabled = %v, want %v", rule, enforcement["enabled"], wantEnabled)
		}
	}
}
