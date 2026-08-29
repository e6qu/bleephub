package bleephub

import (
	"encoding/base64"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/e6qu/bleephub/internal/store"
)

// TestCodeownersPatternSemantics pins the path-matching rules GitHub documents
// for CODEOWNERS, pattern by pattern, including the two places its syntax departs
// from gitignore's: a wildcard-terminated pattern does not own the subtree below
// the files it matches, and `!` is not a negation.
func TestCodeownersPatternSemantics(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		pattern  string
		matches  []string
		misses   []string
		nilRegex bool
	}{
		{
			name:    "bare star owns every file at every depth",
			pattern: "*",
			matches: []string{"README.md", "src/main.go", "a/b/c/d.txt"},
		},
		{
			name:    "extension pattern is not anchored",
			pattern: "*.js",
			matches: []string{"index.js", "src/app.js", "a/b/c.js"},
			misses:  []string{"index.jsx", "js", "src/js/README.md"},
		},
		{
			name:    "root-anchored directory owns its whole subtree",
			pattern: "/build/logs/",
			matches: []string{"build/logs/out.txt", "build/logs/deep/er.txt"},
			misses:  []string{"build/logs", "src/build/logs/out.txt", "build/other.txt"},
		},
		{
			name:    "trailing wildcard stops at the directory's own children",
			pattern: "docs/*",
			matches: []string{"docs/getting-started.md"},
			misses:  []string{"docs/build-app/troubleshooting.md", "sub/docs/getting-started.md", "docs"},
		},
		{
			name:    "directory without a leading slash matches at any depth",
			pattern: "apps/",
			matches: []string{"apps/main.go", "deeply/nested/apps/main.go", "apps/a/b.go"},
			misses:  []string{"apps", "applications/main.go"},
		},
		{
			name:    "leading slash anchors the directory at the root",
			pattern: "/docs/",
			matches: []string{"docs/a.md", "docs/nested/b.md"},
			misses:  []string{"sub/docs/a.md"},
		},
		{
			name:    "double star prefix matches a directory anywhere, subtree included",
			pattern: "**/logs",
			matches: []string{"logs/out.txt", "build/logs/out.txt", "deeply/nested/logs/a/b.txt"},
			misses:  []string{"logstash/out.txt", "build/logging/out.txt"},
		},
		{
			name:    "double star in the middle spans zero or more directories",
			pattern: "/src/**/test",
			matches: []string{"src/test/a.go", "src/a/test/b.go", "src/a/b/test/c.go"},
			misses:  []string{"test/a.go", "other/src/test/a.go"},
		},
		{
			name:    "a literal path owns the file and anything under it",
			pattern: "/apps/github",
			matches: []string{"apps/github", "apps/github/index.js"},
			misses:  []string{"apps/githubbed", "apps/other/github.js"},
		},
		{
			name:    "question mark matches exactly one character inside a segment",
			pattern: "/src/v?.go",
			matches: []string{"src/v1.go"},
			misses:  []string{"src/v10.go", "src/v.go", "src/a/v1.go"},
		},
		{name: "a bare slash owns nothing", pattern: "/", nilRegex: true},
		{name: "three asterisks are rejected", pattern: "a/***/b", nilRegex: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			re := codeownersPatternRegexp(tc.pattern)
			if tc.nilRegex {
				require.Nil(t, re, "pattern %q must compile to nothing", tc.pattern)
				return
			}
			require.NotNil(t, re, "pattern %q must compile", tc.pattern)
			for _, path := range tc.matches {
				require.True(t, re.MatchString(path), "%q must own %q (regex %s)", tc.pattern, path, re)
			}
			for _, path := range tc.misses {
				require.False(t, re.MatchString(path), "%q must not own %q (regex %s)", tc.pattern, path, re)
			}
		})
	}
}

// TestCodeownersLastMatchWinsAndClearing covers the file-level rules: the last
// matching pattern owns the path, a pattern naming no owners clears ownership for
// what it matches, comments and blank lines are not rules, and `!` is not a
// negation because GitHub's syntax has none.
func TestCodeownersLastMatchWinsAndClearing(t *testing.T) {
	t.Parallel()
	content := "" +
		"# every file, unless something below claims it\n" +
		"*       @global-owner\n" +
		"\n" +
		"*.js    @js-owner\n" +
		"/apps/  @octocat\n" +
		"/apps/github\n" +
		"!/apps/secret  @nobody\n"

	rules := parseCodeownersRules(content)
	require.Len(t, rules, 4, "comment, blank and ! lines are not rules: %v", rules)

	owners := func(path string) []string {
		index, ok := codeownersRuleForPath(rules, path)
		if !ok {
			return nil
		}
		return rules[index].owners
	}

	require.Equal(t, []string{"@global-owner"}, owners("README.md"))
	// A later pattern wins over the catch-all above it.
	require.Equal(t, []string{"@js-owner"}, owners("src/index.js"))
	require.Equal(t, []string{"@octocat"}, owners("apps/main.go"))
	// ...and a still-later one wins over that: an owner-less pattern clears
	// ownership rather than falling back to the broader rule.
	require.Empty(t, owners("apps/github/index.rb"), "an owner-less pattern clears ownership")
	// The `!` line is not a rule at all, so the /apps/ rule still owns the path.
	require.Equal(t, []string{"@octocat"}, owners("apps/secret/key.txt"))
	// A .js file under the cleared directory: /apps/github is last, so cleared.
	require.Empty(t, owners("apps/github/app.js"))
}

// codeownersRepo creates a private admin repository with the given CODEOWNERS
// content on its default branch and a "feature" branch cut from it, returning the
// repository.
func codeownersRepo(t *testing.T, s *isolatedServer, name, codeowners string) *store.Repo {
	t.Helper()
	resp := s.post(t, "/api/v3/user/repos", defaultToken, map[string]interface{}{
		"name": name, "auto_init": true, "private": true,
	})
	require.Equal(t, http.StatusCreated, resp.StatusCode)
	resp.Body.Close()
	if codeowners != "" {
		s.putReadsFile(t, name, ".github/CODEOWNERS", codeowners, "add CODEOWNERS", "")
	}
	repo := s.store.GetRepo("admin", name)
	require.NotNil(t, repo)
	seedPullRequestBranches(t, s.Server, repo, "feature")
	return repo
}

// codeownersUser seeds a user with a token, optionally as a collaborator on
// repo — a user left off the collaborator list cannot read a private repo, which
// is what makes them ineligible as a reviewer.
func codeownersUser(t *testing.T, s *isolatedServer, login string, repo *store.Repo, permission string) (*store.User, string) {
	t.Helper()
	user := s.createTestUser(t, login)
	require.NotNil(t, user)
	if permission != "" {
		require.True(t, s.store.AddRepoCollaborator("admin", repo.Name, login, permission))
	}
	return user, s.store.CreateToken(user.ID, "repo").Value
}

// requestedReviewerLogins reads the pull request's requested reviewers through
// the endpoint GitHub serves them from.
func requestedReviewerLogins(t *testing.T, s *isolatedServer, repo *store.Repo, number int) ([]string, []string) {
	t.Helper()
	resp := s.get(t, "/api/v3/repos/"+repo.FullName+"/pulls/"+itoa(number)+"/requested_reviewers", defaultToken)
	out := decodeJSONWithStatus(t, resp, http.StatusOK)
	var users, teams []string
	for _, entry := range out["users"].([]interface{}) {
		users = append(users, entry.(map[string]interface{})["login"].(string))
	}
	for _, entry := range out["teams"].([]interface{}) {
		teams = append(teams, entry.(map[string]interface{})["slug"].(string))
	}
	return users, teams
}

// TestCodeownersAutoRequestOnOpenAndHeadAdvance covers the automatic reviewer
// requests: the owners of the files a pull request changes are requested when it
// opens and again when a push brings newly owned files into the diff, while the
// author, an owner who cannot read the repo, an owner-less path and an owner who
// has already reviewed are all left alone.
func TestCodeownersAutoRequestOnOpenAndHeadAdvance(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	repo := codeownersRepo(t, s, "codeowners-auto", ""+
		"docs/           @docs-owner @locked-out\n"+
		"/src/api/       @api-owner\n"+
		"/src/api/legacy\n"+
		"/admin-only/    @admin\n")

	docsOwner, _ := codeownersUser(t, s, "docs-owner", repo, "push")
	require.NotNil(t, docsOwner)
	codeownersUser(t, s, "api-owner", repo, "push")
	// A CODEOWNERS entry for somebody with no access to a private repo names a
	// person who could never review it, so they are never requested.
	codeownersUser(t, s, "locked-out", repo, "")

	s.putReadsFile(t, repo.Name, "docs/guide.md", "# guide\n", "document", "feature")
	resp := s.post(t, "/api/v3/repos/"+repo.FullName+"/pulls", defaultToken, map[string]interface{}{
		"title": "docs change", "head": "feature", "base": "main",
	})
	created := decodeJSONWithStatus(t, resp, http.StatusCreated)
	number := int(created["number"].(float64))

	users, teams := requestedReviewerLogins(t, s, repo, number)
	require.Equal(t, []string{"docs-owner"}, users, "only the readable, non-author owner of the changed path is requested")
	require.Empty(t, teams)

	// A push that adds an owned file requests that file's owner too.
	s.putReadsFile(t, repo.Name, "src/api/handler.go", "package api\n", "add handler", "feature")
	users, _ = requestedReviewerLogins(t, s, repo, number)
	require.ElementsMatch(t, []string{"docs-owner", "api-owner"}, users, "a head advance requests the new paths' owners")

	// A push into a path whose last matching pattern names nobody adds no
	// reviewer, and neither does one the author owns.
	s.putReadsFile(t, repo.Name, "src/api/legacy/old.go", "package legacy\n", "legacy", "feature")
	s.putReadsFile(t, repo.Name, "admin-only/notes.md", "notes\n", "author-owned", "feature")
	users, _ = requestedReviewerLogins(t, s, repo, number)
	require.ElementsMatch(t, []string{"docs-owner", "api-owner"}, users,
		"a cleared path and the author's own ownership request nobody")

	// An owner who has reviewed is not asked again by a later push, even once
	// their pending request has been withdrawn.
	resp = s.do(t, http.MethodDelete, "/api/v3/repos/"+repo.FullName+"/pulls/"+itoa(number)+"/requested_reviewers",
		defaultToken, map[string]interface{}{"reviewers": []string{"docs-owner"}})
	require.Equal(t, http.StatusOK, resp.StatusCode)
	resp.Body.Close()
	docsToken := s.store.CreateToken(docsOwner.ID, "repo").Value
	resp = s.post(t, "/api/v3/repos/"+repo.FullName+"/pulls/"+itoa(number)+"/reviews", docsToken, map[string]interface{}{
		"body": "looks fine", "event": "COMMENT",
	})
	require.Equal(t, http.StatusOK, resp.StatusCode)
	resp.Body.Close()

	s.putReadsFile(t, repo.Name, "docs/more.md", "# more\n", "more docs", "feature")
	users, _ = requestedReviewerLogins(t, s, repo, number)
	require.Equal(t, []string{"api-owner"}, users, "an owner who already reviewed is not re-requested")
}

// TestCodeownersRepoWithoutCodeownersFileIsUnaffected pins that a repo with no
// CODEOWNERS file gets no reviewer requests and that require_code_owner_reviews
// cannot block a merge there — there is nobody it could be waiting for.
func TestCodeownersRepoWithoutCodeownersFileIsUnaffected(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	repo := codeownersRepo(t, s, "codeowners-absent", "")

	resp := s.put(t, "/api/v3/repos/"+repo.FullName+"/branches/main/protection", defaultToken, map[string]interface{}{
		"required_pull_request_reviews": map[string]interface{}{"require_code_owner_reviews": true},
		"enforce_admins":                true,
	})
	require.Equal(t, http.StatusOK, resp.StatusCode)
	resp.Body.Close()

	s.putReadsFile(t, repo.Name, "docs/guide.md", "# guide\n", "document", "feature")
	resp = s.post(t, "/api/v3/repos/"+repo.FullName+"/pulls", defaultToken, map[string]interface{}{
		"title": "no owners here", "head": "feature", "base": "main",
	})
	created := decodeJSONWithStatus(t, resp, http.StatusCreated)
	number := int(created["number"].(float64))

	users, teams := requestedReviewerLogins(t, s, repo, number)
	require.Empty(t, users)
	require.Empty(t, teams)

	detail := decodeJSONWithStatus(t, s.get(t, "/api/v3/repos/"+repo.FullName+"/pulls/"+itoa(number), defaultToken), http.StatusOK)
	require.NotEqual(t, "blocked", detail["mergeable_state"], "no CODEOWNERS file blocks nothing: %v", detail["mergeable_state"])

	resp = s.put(t, "/api/v3/repos/"+repo.FullName+"/pulls/"+itoa(number)+"/merge", defaultToken, map[string]interface{}{})
	require.Equal(t, http.StatusOK, resp.StatusCode)
	resp.Body.Close()
}

// TestRequireCodeOwnerReviewsGatesMerge covers the enforcement half: with
// require_code_owner_reviews set the merge is refused until a code owner of the
// changed files approves, an approval from somebody who owns nothing does not
// count, a dismissed approval stops counting, and the read path reports the same
// blocked state it reports for any other protection refusal.
func TestRequireCodeOwnerReviewsGatesMerge(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	repo := codeownersRepo(t, s, "codeowners-merge", "docs/ @docs-owner\n")
	docsOwner, docsToken := codeownersUser(t, s, "docs-owner", repo, "push")
	require.NotNil(t, docsOwner)
	_, strangerToken := codeownersUser(t, s, "not-an-owner", repo, "push")

	resp := s.put(t, "/api/v3/repos/"+repo.FullName+"/branches/main/protection", defaultToken, map[string]interface{}{
		"required_pull_request_reviews": map[string]interface{}{"require_code_owner_reviews": true},
		"enforce_admins":                true,
	})
	protection := decodeJSONWithStatus(t, resp, http.StatusOK)
	reviews, _ := protection["required_pull_request_reviews"].(map[string]interface{})
	require.Equal(t, true, reviews["require_code_owner_reviews"])

	s.putReadsFile(t, repo.Name, "docs/guide.md", "# guide\n", "document", "feature")
	resp = s.post(t, "/api/v3/repos/"+repo.FullName+"/pulls", defaultToken, map[string]interface{}{
		"title": "docs change", "head": "feature", "base": "main",
	})
	created := decodeJSONWithStatus(t, resp, http.StatusCreated)
	number := itoa(int(created["number"].(float64)))
	prPath := "/api/v3/repos/" + repo.FullName + "/pulls/" + number

	// Unapproved: the merge is refused and the detail payload says blocked.
	resp = s.put(t, prPath+"/merge", defaultToken, map[string]interface{}{})
	body := decodeJSONWithStatus(t, resp, http.StatusMethodNotAllowed)
	require.Contains(t, body["message"], "code owner")
	detail := decodeJSONWithStatus(t, s.get(t, prPath, defaultToken), http.StatusOK)
	require.Equal(t, "blocked", detail["mergeable_state"])

	// An approval from someone who owns none of the changed files does not
	// satisfy the rule.
	resp = s.post(t, prPath+"/reviews", strangerToken, map[string]interface{}{"body": "LGTM", "event": "APPROVE"})
	require.Equal(t, http.StatusOK, resp.StatusCode)
	resp.Body.Close()
	resp = s.put(t, prPath+"/merge", defaultToken, map[string]interface{}{})
	body = decodeJSONWithStatus(t, resp, http.StatusMethodNotAllowed)
	require.Contains(t, body["message"], "code owner")

	// The code owner approves: the gate opens.
	resp = s.post(t, prPath+"/reviews", docsToken, map[string]interface{}{"body": "owner LGTM", "event": "APPROVE"})
	review := decodeJSONWithStatus(t, resp, http.StatusOK)
	reviewID := itoa(int(review["id"].(float64)))
	detail = decodeJSONWithStatus(t, s.get(t, prPath, defaultToken), http.StatusOK)
	require.NotEqual(t, "blocked", detail["mergeable_state"], "an approved pull request is no longer blocked")

	// Dismissing that approval closes it again — a stale approval carries no
	// weight, as it carries none for the other review rules.
	resp = s.put(t, prPath+"/reviews/"+reviewID+"/dismissals", defaultToken, map[string]interface{}{
		"message": "the head moved on",
	})
	require.Equal(t, http.StatusOK, resp.StatusCode)
	resp.Body.Close()
	resp = s.put(t, prPath+"/merge", defaultToken, map[string]interface{}{})
	body = decodeJSONWithStatus(t, resp, http.StatusMethodNotAllowed)
	require.Contains(t, body["message"], "code owner")

	// A fresh approval from the code owner, and the merge goes through.
	resp = s.post(t, prPath+"/reviews", docsToken, map[string]interface{}{"body": "still good", "event": "APPROVE"})
	require.Equal(t, http.StatusOK, resp.StatusCode)
	resp.Body.Close()
	resp = s.put(t, prPath+"/merge", defaultToken, map[string]interface{}{})
	require.Equal(t, http.StatusOK, resp.StatusCode)
	resp.Body.Close()
}

// TestCodeownersTeamOwnerRequestedAndApproves covers team ownership end to end:
// an `@org/team` owner is requested as a team reviewer, not as its individual
// members, and any member's approval satisfies the merge gate.
func TestCodeownersTeamOwnerRequestedAndApproves(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	org := "codeowners-org"
	s.createOrgViaAdminAPI(t, org)
	resp := s.post(t, "/api/v3/orgs/"+org+"/repos", defaultToken, map[string]interface{}{"name": "team-owned"})
	require.Equal(t, http.StatusCreated, resp.StatusCode)
	resp.Body.Close()
	repo := s.store.GetRepo(org, "team-owned")
	require.NotNil(t, repo)

	member := s.createTestUser(t, "team-reviewer")
	require.NotNil(t, member)
	memberToken := s.store.CreateToken(member.ID, "repo").Value
	team := s.store.CreateTeam(org, "Docs Crew", store.TeamOptions{Permission: store.TeamPermissionPush})
	require.NotNil(t, team)
	require.True(t, s.store.SetTeamMembership(org, team.Slug, member.ID, store.TeamRoleMember))
	require.True(t, s.store.AddTeamRepo(org, team.Slug, repo.FullName))

	s.put(t, "/api/v3/repos/"+repo.FullName+"/contents/.github/CODEOWNERS", defaultToken, map[string]interface{}{
		"message": "add CODEOWNERS",
		"content": base64.StdEncoding.EncodeToString([]byte("docs/ @" + org + "/" + team.Slug + "\n")),
	}).Body.Close()
	seedPullRequestBranches(t, s.Server, repo, "feature")

	resp = s.put(t, "/api/v3/repos/"+repo.FullName+"/branches/main/protection", defaultToken, map[string]interface{}{
		"required_pull_request_reviews": map[string]interface{}{"require_code_owner_reviews": true},
		"enforce_admins":                true,
	})
	require.Equal(t, http.StatusOK, resp.StatusCode)
	resp.Body.Close()

	s.put(t, "/api/v3/repos/"+repo.FullName+"/contents/docs/guide.md", defaultToken, map[string]interface{}{
		"message": "document", "content": base64.StdEncoding.EncodeToString([]byte("# guide\n")), "branch": "feature",
	}).Body.Close()
	resp = s.post(t, "/api/v3/repos/"+repo.FullName+"/pulls", defaultToken, map[string]interface{}{
		"title": "docs change", "head": "feature", "base": "main",
	})
	created := decodeJSONWithStatus(t, resp, http.StatusCreated)
	number := itoa(int(created["number"].(float64)))
	prPath := "/api/v3/repos/" + repo.FullName + "/pulls/" + number

	users, teams := requestedReviewerLogins(t, s, repo, int(created["number"].(float64)))
	require.Equal(t, []string{team.Slug}, teams, "a team owner is requested as a team")
	require.Empty(t, users, "a team owner does not request its members individually")

	resp = s.put(t, prPath+"/merge", defaultToken, map[string]interface{}{})
	body := decodeJSONWithStatus(t, resp, http.StatusMethodNotAllowed)
	require.Contains(t, body["message"], "code owner")

	// Any member of the owning team can supply the approval.
	resp = s.post(t, prPath+"/reviews", memberToken, map[string]interface{}{"body": "LGTM", "event": "APPROVE"})
	require.Equal(t, http.StatusOK, resp.StatusCode)
	resp.Body.Close()
	resp = s.put(t, prPath+"/merge", defaultToken, map[string]interface{}{})
	require.Equal(t, http.StatusOK, resp.StatusCode)
	resp.Body.Close()
}
