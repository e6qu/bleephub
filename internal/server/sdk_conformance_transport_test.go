package bleephub

import (
	"encoding/base64"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"github.com/e6qu/bleephub/internal/store"
)

// The conformance harness (test/conformance) drives real, unmodified GitHub
// clients — gh, go-github, octokit.js, PyGithub and git — against a running
// Bleephub. These tests pin the server-side contracts those clients were found
// to depend on, so a regression is a unit-test failure here rather than a red
// row in the scoreboard.

// acceptGet issues a GET carrying an explicit Accept header, which the shared
// helpers do not, and returns the response for the caller to close.
func (s *isolatedServer) acceptGet(t *testing.T, path, accept string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, s.baseURL+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Accept", accept)
	req.Header.Set("Authorization", "Bearer "+defaultToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func readAllBody(t *testing.T, resp *http.Response) string {
	t.Helper()
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// --- Default labels -------------------------------------------------------

// wantDefaultLabels is GitHub's default label set, read off a live repository's
// GET /repos/{owner}/{repo}/labels (the rows reported with default:true), in
// the order the endpoint returns them.
var wantDefaultLabels = []struct{ name, color, description string }{
	{"bug", "d73a4a", "Something isn't working"},
	{"documentation", "0075ca", "Improvements or additions to documentation"},
	{"duplicate", "cfd3d7", "This issue or pull request already exists"},
	{"enhancement", "a2eeef", "New feature or request"},
	{"good first issue", "7057ff", "Good for newcomers"},
	{"help wanted", "008672", "Extra attention is needed"},
	{"invalid", "e4e669", "This doesn't seem right"},
	{"question", "d876e3", "Further information is requested"},
	{"wontfix", "ffffff", "This will not be worked on"},
}

func assertDefaultLabelSet(t *testing.T, s *isolatedServer, repoPath string) {
	t.Helper()
	resp := s.get(t, repoPath+"/labels?per_page=100", defaultToken)
	labels := decodeJSONArrayWithStatus(t, resp, http.StatusOK)
	if len(labels) != len(wantDefaultLabels) {
		names := make([]string, 0, len(labels))
		for _, l := range labels {
			name, _ := l["name"].(string)
			names = append(names, name)
		}
		t.Fatalf("%s labels = %v, want GitHub's %d default labels", repoPath, names, len(wantDefaultLabels))
	}
	for i, want := range wantDefaultLabels {
		got := labels[i]
		if got["name"] != want.name || got["color"] != want.color || got["description"] != want.description {
			t.Errorf("label %d = %v, want name=%q color=%q description=%q",
				i, got, want.name, want.color, want.description)
		}
		if got["default"] != true {
			t.Errorf("label %q default = %v, want true", want.name, got["default"])
		}
	}
}

// TestDefaultLabelsOnEveryRepositoryCreationPath pins the label set GitHub
// seeds into a new repository. `gh label list`, go-github's
// repos.listLabelsForRepo and PyGithub's Repository.get_labels all read it, and
// bleephub used to answer with an empty list.
//
// Every creation path gets the same set. A fork does too — a fork of a
// repository carrying custom labels lists exactly these nine, not the parent's
// — and so does a repository generated from a template, which copies only the
// template's files and branches.
func TestDefaultLabelsOnEveryRepositoryCreationPath(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)

	resp := s.post(t, "/api/v3/user/repos", defaultToken, map[string]interface{}{"name": "labels-user"})
	decodeJSONWithStatus(t, resp, http.StatusCreated)
	assertDefaultLabelSet(t, s, "/api/v3/repos/admin/labels-user")

	org := s.createTestOrg(t)
	resp = s.post(t, "/api/v3/orgs/"+org+"/repos", defaultToken, map[string]interface{}{"name": "labels-org"})
	decodeJSONWithStatus(t, resp, http.StatusCreated)
	assertDefaultLabelSet(t, s, "/api/v3/repos/"+org+"/labels-org")

	resp = s.post(t, "/api/v3/user/repos", defaultToken, map[string]interface{}{
		"name": "labels-template", "auto_init": true,
	})
	decodeJSONWithStatus(t, resp, http.StatusCreated)
	resp = s.patch(t, "/api/v3/repos/admin/labels-template", defaultToken,
		map[string]interface{}{"is_template": true})
	decodeJSONWithStatus(t, resp, http.StatusOK)
	resp = s.post(t, "/api/v3/repos/admin/labels-template/generate", defaultToken, map[string]interface{}{
		"owner": "admin", "name": "labels-generated",
	})
	decodeJSONWithStatus(t, resp, http.StatusCreated)
	assertDefaultLabelSet(t, s, "/api/v3/repos/admin/labels-generated")

	// The parent's extra custom label must not ride along onto the fork.
	resp = s.post(t, "/api/v3/repos/admin/labels-user/labels", defaultToken,
		map[string]interface{}{"name": "parent-only", "color": "112233"})
	decodeJSONWithStatus(t, resp, http.StatusCreated)
	forker := s.createTestUser(t, "labels-forker")
	forkToken := s.store.CreateToken(forker.ID, "repo").Value
	resp = s.post(t, "/api/v3/repos/admin/labels-user/forks", forkToken, map[string]interface{}{})
	decodeJSONWithStatus(t, resp, http.StatusAccepted)
	assertDefaultLabelSet(t, s, "/api/v3/repos/labels-forker/labels-user")
}

// TestDefaultLabelNameIsTakenOnANewRepository pins the consequence GitHub has
// and bleephub gained: the default names are already in use, so re-creating one
// is the duplicate-name validation failure, not a second "bug" label.
func TestDefaultLabelNameIsTakenOnANewRepository(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	repo := s.createTestRepo(t)
	resp := s.post(t, repo.path()+"/labels", defaultToken,
		map[string]interface{}{"name": "bug", "color": "d73a4a"})
	body := decodeJSONWithStatus(t, resp, http.StatusUnprocessableEntity)
	errors, _ := body["errors"].([]interface{})
	if len(errors) == 0 {
		t.Fatalf("duplicate default label = %v, want a validation error", body)
	}
	first, _ := errors[0].(map[string]interface{})
	if first["code"] != "already_exists" {
		t.Fatalf("error code = %v, want already_exists", first["code"])
	}
}

// --- Search pagination Link header ---------------------------------------

// linkRels parses an RFC 5988 Link header into rel → URL.
func linkRels(t *testing.T, header string) map[string]string {
	t.Helper()
	rels := map[string]string{}
	for _, part := range strings.Split(header, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		target, params, ok := strings.Cut(part, ">")
		if !ok || !strings.HasPrefix(target, "<") {
			t.Fatalf("malformed Link segment %q in %q", part, header)
		}
		for _, param := range strings.Split(params, ";") {
			param = strings.TrimSpace(param)
			if rel, found := strings.CutPrefix(param, `rel="`); found {
				rels[strings.TrimSuffix(rel, `"`)] = strings.TrimPrefix(target, "<")
			}
		}
	}
	return rels
}

// TestSearchResponsesCarryTheLinkHeader pins that search pages like every other
// collection. octokit.paginate walked one of seven matching issues and
// go-github reported no next page, because the search endpoints answered
// without a Link header — silent truncation, with no error for a client to see.
func TestSearchResponsesCarryTheLinkHeader(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	repo := s.createTestRepo(t)
	const matches = 7
	for i := 0; i < matches; i++ {
		s.createIssueForTest(t, repo, "linkheader subject "+strconv.Itoa(i))
	}

	query := url.QueryEscape("linkheader repo:" + repo.fullName())
	resp := s.get(t, "/api/v3/search/issues?per_page=1&q="+query, defaultToken)
	link := resp.Header.Get("Link")
	page1 := decodeJSONWithStatus(t, resp, http.StatusOK)
	if total, _ := page1["total_count"].(float64); int(total) != matches {
		t.Fatalf("total_count = %v, want %d", page1["total_count"], matches)
	}
	rels := linkRels(t, link)
	if rels["next"] == "" || rels["last"] == "" {
		t.Fatalf("Link = %q, want rel=next and rel=last on page 1 of %d", link, matches)
	}

	// Walk every page the header points at, exactly as octokit.paginate does,
	// and check the walk yields all the results.
	seen := map[string]bool{}
	next := rels["next"]
	items, _ := page1["items"].([]interface{})
	for _, item := range items {
		title, _ := item.(map[string]interface{})["title"].(string)
		seen[title] = true
	}
	for hop := 0; next != "" && hop < 2*matches; hop++ {
		parsed, err := url.Parse(next)
		if err != nil {
			t.Fatalf("parse Link target %q: %v", next, err)
		}
		page := decodeJSONWithStatus(t, s.get(t, parsed.RequestURI(), defaultToken), http.StatusOK)
		pageItems, _ := page["items"].([]interface{})
		for _, item := range pageItems {
			title, _ := item.(map[string]interface{})["title"].(string)
			seen[title] = true
		}
		resp := s.get(t, parsed.RequestURI(), defaultToken)
		next = linkRels(t, resp.Header.Get("Link"))["next"]
		resp.Body.Close()
	}
	if len(seen) != matches {
		t.Fatalf("walked %d distinct results, want %d", len(seen), matches)
	}

	// The last page carries prev/first and no next, which is what ends a walk.
	resp = s.get(t, "/api/v3/search/issues?per_page=1&page="+strconv.Itoa(matches)+"&q="+query, defaultToken)
	lastRels := linkRels(t, resp.Header.Get("Link"))
	resp.Body.Close()
	if lastRels["next"] != "" || lastRels["prev"] == "" || lastRels["first"] == "" {
		t.Fatalf("last page Link rels = %v, want prev+first and no next", lastRels)
	}
}

// TestEverySearchEndpointCarriesTheLinkHeader checks the contract on all seven
// search collections, not only the issues one the harness happened to drive.
func TestEverySearchEndpointCarriesTheLinkHeader(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	repo := s.createTestRepo(t)
	repoData := decodeJSONWithStatus(t, s.get(t, repo.path(), defaultToken), http.StatusOK)
	repoID := strconv.Itoa(int(repoData["id"].(float64)))

	// Two of everything, so per_page=1 always leaves a second page.
	for i := 0; i < 2; i++ {
		suffix := strconv.Itoa(i)
		s.createIssueForTest(t, repo, "sweeps subject "+suffix)
		s.post(t, repo.path()+"/labels", defaultToken,
			map[string]interface{}{"name": "sweeps-" + suffix, "color": "aabbcc"}).Body.Close()
		s.post(t, "/api/v3/user/repos", defaultToken,
			map[string]interface{}{"name": "sweeps-repo-" + suffix, "auto_init": true}).Body.Close()
		s.put(t, "/api/v3/repos/admin/sweeps-repo-"+suffix+"/contents/sweeps.txt", defaultToken,
			map[string]interface{}{
				"message": "sweeps commit " + suffix,
				"content": base64.StdEncoding.EncodeToString([]byte("sweeps\n")),
				"branch":  "main",
			}).Body.Close()
		s.put(t, "/api/v3/repos/admin/sweeps-repo-"+suffix+"/topics", defaultToken,
			map[string]interface{}{"names": []string{"sweeps-topic-" + suffix}}).Body.Close()
		s.createTestUser(t, "sweeps-user-"+suffix)
	}

	for _, endpoint := range []struct{ name, path string }{
		{"issues", "/api/v3/search/issues?per_page=1&q=" + url.QueryEscape("sweeps repo:"+repo.fullName())},
		{"repositories", "/api/v3/search/repositories?per_page=1&q=sweeps-repo"},
		{"code", "/api/v3/search/code?per_page=1&q=" + url.QueryEscape("sweeps user:admin")},
		{"commits", "/api/v3/search/commits?per_page=1&q=" + url.QueryEscape("sweeps user:admin")},
		{"users", "/api/v3/search/users?per_page=1&q=sweeps-user"},
		{"labels", "/api/v3/search/labels?per_page=1&repository_id=" + repoID + "&q=sweeps"},
		{"topics", "/api/v3/search/topics?per_page=1&q=sweeps-topic"},
	} {
		resp := s.get(t, endpoint.path, defaultToken)
		link := resp.Header.Get("Link")
		body := decodeJSONWithStatus(t, resp, http.StatusOK)
		total, _ := body["total_count"].(float64)
		if int(total) < 2 {
			t.Fatalf("search/%s total_count = %v, want at least 2 so a second page exists", endpoint.name, body["total_count"])
		}
		if rels := linkRels(t, link); rels["next"] == "" {
			t.Errorf("search/%s Link = %q, want a rel=next", endpoint.name, link)
		}
	}
}

// TestSearchLinkHeaderStopsAtGitHubsResultWindow pins the cap: GitHub serves at
// most the first 1,000 search results however large total_count is, so the last
// page a client is pointed at must be inside that window — following a link
// past it would walk into a refusal.
func TestSearchLinkHeaderStopsAtGitHubsResultWindow(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name             string
		total, perPage   int
		wantLastPage     string
		wantNextPresence bool
	}{
		{"under the window", 250, 100, "3", true},
		{"far past the window", 50000, 100, "10", true},
		{"exactly the window", 1000, 25, "40", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			recorder := newSearchLinkRecorder(t, tc.total, tc.perPage)
			rels := linkRels(t, recorder)
			if (rels["next"] != "") != tc.wantNextPresence {
				t.Fatalf("rels = %v, want next present=%v", rels, tc.wantNextPresence)
			}
			last, err := url.Parse(rels["last"])
			if err != nil {
				t.Fatalf("parse last %q: %v", rels["last"], err)
			}
			if got := last.Query().Get("page"); got != tc.wantLastPage {
				t.Fatalf("rel=last page = %q, want %q", got, tc.wantLastPage)
			}
		})
	}
}

// newSearchLinkRecorder returns the Link header the search helper emits for a
// first page of the given size out of the given total.
func newSearchLinkRecorder(t *testing.T, total, perPage int) string {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet,
		"http://bleephub.test/api/v3/search/issues?q=x&per_page="+strconv.Itoa(perPage), nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Host = "bleephub.test"
	recorder := httptest.NewRecorder()
	setSearchLinkHeader(recorder, req, 1, perPage, total)
	return recorder.Header().Get("Link")
}

// --- Pull request diff and patch media types ------------------------------

// seedDiffablePullRequest opens a pull request whose head branch modifies one
// file and adds another, and returns the repository's API path.
func seedDiffablePullRequest(t *testing.T, s *isolatedServer, name string) string {
	t.Helper()
	repoPath := "/api/v3/repos/admin/" + name
	s.post(t, "/api/v3/user/repos", defaultToken, map[string]interface{}{
		"name": name, "auto_init": true,
	}).Body.Close()
	s.put(t, repoPath+"/contents/greeting.txt", defaultToken, map[string]interface{}{
		"message": "seed greeting",
		"content": base64.StdEncoding.EncodeToString([]byte("hello\n")),
		"branch":  "main",
	}).Body.Close()

	refData := decodeJSONWithStatus(t, s.get(t, repoPath+"/git/refs/heads/main", defaultToken), http.StatusOK)
	mainObj, _ := refData["object"].(map[string]interface{})
	mainSha, _ := mainObj["sha"].(string)
	if mainSha == "" {
		t.Fatalf("main ref sha missing: %v", refData)
	}
	s.post(t, repoPath+"/git/refs", defaultToken, map[string]interface{}{
		"ref": "refs/heads/feat", "sha": mainSha,
	}).Body.Close()

	blob := decodeJSONWithStatus(t, s.get(t, repoPath+"/contents/greeting.txt?ref=feat", defaultToken), http.StatusOK)
	blobSha, _ := blob["sha"].(string)
	s.put(t, repoPath+"/contents/greeting.txt", defaultToken, map[string]interface{}{
		"message": "change the greeting",
		"content": base64.StdEncoding.EncodeToString([]byte("hello world\n")),
		"branch":  "feat",
		"sha":     blobSha,
	}).Body.Close()
	s.put(t, repoPath+"/contents/added.txt", defaultToken, map[string]interface{}{
		"message": "add a file",
		"content": base64.StdEncoding.EncodeToString([]byte("brand new\n")),
		"branch":  "feat",
	}).Body.Close()
	s.post(t, repoPath+"/pulls", defaultToken, map[string]interface{}{
		"title": "Diff flow", "head": "feat", "base": "main",
	}).Body.Close()
	return repoPath
}

// TestPullRequestDiffAndPatchMediaTypes pins the media types `gh pr diff` needs.
// gh asks with the versioned spelling application/vnd.github.v3.diff; the
// endpoint used to ignore it and answer the pull request object, which gh then
// printed as if it were the diff.
func TestPullRequestDiffAndPatchMediaTypes(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	repoPath := seedDiffablePullRequest(t, s, "pr-diff-media")

	// Both spellings of the diff media type, because real clients send both.
	for _, accept := range []string{"application/vnd.github.v3.diff", "application/vnd.github.diff"} {
		resp := s.acceptGet(t, repoPath+"/pulls/1", accept)
		contentType := resp.Header.Get("Content-Type")
		mediaType := resp.Header.Get("X-GitHub-Media-Type")
		diff := readAllBody(t, resp)
		if !strings.HasPrefix(diff, "diff --git") {
			t.Fatalf("Accept %s body = %.120q, want a unified diff", accept, diff)
		}
		if !strings.Contains(diff, "greeting.txt") || !strings.Contains(diff, "added.txt") {
			t.Errorf("Accept %s diff does not cover both changed files: %s", accept, diff)
		}
		if !strings.Contains(diff, "+hello world") || !strings.Contains(diff, "-hello") {
			t.Errorf("Accept %s diff has no hunk for the modified file: %s", accept, diff)
		}
		if contentType != "application/vnd.github.diff; charset=utf-8" {
			t.Errorf("Accept %s Content-Type = %q", accept, contentType)
		}
		if !strings.Contains(mediaType, "param=diff") {
			t.Errorf("Accept %s X-GitHub-Media-Type = %q, want param=diff", accept, mediaType)
		}
	}

	// .patch is a git-format-patch series, one per commit the PR adds.
	resp := s.acceptGet(t, repoPath+"/pulls/1", "application/vnd.github.v3.patch")
	contentType := resp.Header.Get("Content-Type")
	mediaType := resp.Header.Get("X-GitHub-Media-Type")
	patch := readAllBody(t, resp)
	if !strings.HasPrefix(patch, "From ") || !strings.Contains(patch, "Subject: [PATCH]") {
		t.Fatalf("patch body = %.160q, want a git-format-patch series", patch)
	}
	if strings.Count(patch, "\nFrom ")+1 != 2 {
		t.Errorf("patch series covers %d commits, want the PR's 2", strings.Count(patch, "\nFrom ")+1)
	}
	if contentType != "application/vnd.github.patch; charset=utf-8" {
		t.Errorf("patch Content-Type = %q", contentType)
	}
	if !strings.Contains(mediaType, "param=patch") {
		t.Errorf("patch X-GitHub-Media-Type = %q, want param=patch", mediaType)
	}

	// Without a custom media type the endpoint is still the JSON pull request.
	body := decodeJSONWithStatus(t, s.get(t, repoPath+"/pulls/1", defaultToken), http.StatusOK)
	if body["number"] == nil {
		t.Fatalf("default representation = %v, want the pull request object", body)
	}
}

// TestCommitAndCompareAcceptTheVersionedDiffMediaType pins the other two
// endpoints GitHub serves diff and patch from. They honoured only the
// unversioned spelling, so a client sending application/vnd.github.v3.diff —
// which is what gh sends — got JSON from them too.
func TestCommitAndCompareAcceptTheVersionedDiffMediaType(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	repoPath := seedDiffablePullRequest(t, s, "commit-diff-media")

	head := decodeJSONWithStatus(t, s.get(t, repoPath+"/git/refs/heads/feat", defaultToken), http.StatusOK)
	headObj, _ := head["object"].(map[string]interface{})
	headSHA, _ := headObj["sha"].(string)

	for _, path := range []string{repoPath + "/commits/" + headSHA, repoPath + "/compare/main...feat"} {
		resp := s.acceptGet(t, path, "application/vnd.github.v3.diff")
		mediaType := resp.Header.Get("X-GitHub-Media-Type")
		diff := readAllBody(t, resp)
		if !strings.HasPrefix(diff, "diff --git") {
			t.Errorf("GET %s with the versioned diff media type = %.120q, want a unified diff", path, diff)
		}
		if !strings.Contains(mediaType, "param=diff") {
			t.Errorf("GET %s X-GitHub-Media-Type = %q, want param=diff", path, mediaType)
		}

		resp = s.acceptGet(t, path, "application/vnd.github.v3.patch")
		patch := readAllBody(t, resp)
		if !strings.HasPrefix(patch, "From ") {
			t.Errorf("GET %s with the versioned patch media type = %.120q, want a format-patch", path, patch)
		}
	}
}

// --- Contents API media types ---------------------------------------------

// TestContentsRawMediaTypeIsText pins the Content-Type of the contents API's
// raw representation. octokit decides from it whether to hand the caller a
// string or a binary Buffer, so application/octet-stream turned a text file
// into a Buffer; GitHub answers text/plain; charset=utf-8.
func TestContentsRawMediaTypeIsText(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	s.post(t, "/api/v3/user/repos", defaultToken, map[string]interface{}{
		"name": "contents-media", "auto_init": true,
	}).Body.Close()
	repoPath := "/api/v3/repos/admin/contents-media"
	const body = "# contents media\n\nraw body\n"
	seeded := decodeJSONWithStatus(t, s.get(t, repoPath+"/contents/README.md", defaultToken), http.StatusOK)
	s.put(t, repoPath+"/contents/README.md", defaultToken, map[string]interface{}{
		"message": "seed readme",
		"content": base64.StdEncoding.EncodeToString([]byte(body)),
		"branch":  "main",
		"sha":     seeded["sha"],
	}).Body.Close()

	for _, path := range []string{repoPath + "/contents/README.md", repoPath + "/readme"} {
		for _, accept := range []string{"application/vnd.github.raw", "application/vnd.github.v3.raw"} {
			resp := s.acceptGet(t, path, accept)
			contentType := resp.Header.Get("Content-Type")
			mediaType := resp.Header.Get("X-GitHub-Media-Type")
			nosniff := resp.Header.Get("X-Content-Type-Options")
			raw := readAllBody(t, resp)
			if contentType != "text/plain; charset=utf-8" {
				t.Errorf("GET %s (%s) Content-Type = %q, want text/plain; charset=utf-8", path, accept, contentType)
			}
			if mediaType != "github.v3; param=raw" {
				t.Errorf("GET %s (%s) X-GitHub-Media-Type = %q, want github.v3; param=raw", path, accept, mediaType)
			}
			if nosniff != "nosniff" {
				t.Errorf("GET %s (%s) X-Content-Type-Options = %q, want nosniff", path, accept, nosniff)
			}
			if raw != body {
				t.Errorf("GET %s (%s) body = %q, want the file's bytes", path, accept, raw)
			}
		}
	}

	// The +json spelling names a format as well as a parameter, and the header
	// reports both.
	resp := s.acceptGet(t, repoPath+"/contents/README.md", "application/vnd.github.raw+json")
	mediaType := resp.Header.Get("X-GitHub-Media-Type")
	resp.Body.Close()
	if mediaType != "github.v3; param=raw; format=json" {
		t.Errorf("raw+json X-GitHub-Media-Type = %q", mediaType)
	}

	// html renders the file, object answers the JSON object representation.
	resp = s.acceptGet(t, repoPath+"/contents/README.md", "application/vnd.github.html")
	mediaType = resp.Header.Get("X-GitHub-Media-Type")
	contentType := resp.Header.Get("Content-Type")
	rendered := readAllBody(t, resp)
	if contentType != "text/html; charset=utf-8" {
		t.Errorf("html Content-Type = %q", contentType)
	}
	if mediaType != "github.v3; param=html" {
		t.Errorf("html X-GitHub-Media-Type = %q", mediaType)
	}
	if !strings.Contains(rendered, "<h1") {
		t.Errorf("html body = %q, want rendered markdown", rendered)
	}

	resp = s.acceptGet(t, repoPath+"/contents", "application/vnd.github.object")
	mediaType = resp.Header.Get("X-GitHub-Media-Type")
	listing := decodeJSONWithStatus(t, resp, http.StatusOK)
	if mediaType != "github.v3; param=object" {
		t.Errorf("object X-GitHub-Media-Type = %q", mediaType)
	}
	if listing["type"] != "dir" || listing["entries"] == nil {
		t.Errorf("object representation = %v, want {type: dir, entries: [...]}", listing)
	}

	// The plain JSON representation still reports itself as JSON.
	resp = s.get(t, repoPath+"/contents/README.md", defaultToken)
	mediaType = resp.Header.Get("X-GitHub-Media-Type")
	resp.Body.Close()
	if mediaType != "github.v3; format=json" {
		t.Errorf("default X-GitHub-Media-Type = %q, want github.v3; format=json", mediaType)
	}
}

// TestRepoRawRouteStillServesTextPlainWithNosniff guards the separate web raw
// route, which serves repository bytes as inert text on purpose. It is not the
// contents API and must keep its own headers.
func TestRepoRawRouteStillServesTextPlainWithNosniff(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	s.post(t, "/api/v3/user/repos", defaultToken, map[string]interface{}{
		"name": "raw-route", "auto_init": true,
	}).Body.Close()
	s.put(t, "/api/v3/repos/admin/raw-route/contents/page.html", defaultToken, map[string]interface{}{
		"message": "seed html",
		"content": base64.StdEncoding.EncodeToString([]byte("<script>alert(1)</script>\n")),
		"branch":  "main",
	}).Body.Close()

	resp := s.get(t, "/admin/raw-route/raw/main/page.html", defaultToken)
	contentType := resp.Header.Get("Content-Type")
	nosniff := resp.Header.Get("X-Content-Type-Options")
	resp.Body.Close()
	if !strings.HasPrefix(contentType, "text/plain") || nosniff != "nosniff" {
		t.Fatalf("raw route Content-Type = %q, X-Content-Type-Options = %q", contentType, nosniff)
	}
}

// --- Gist URLs -------------------------------------------------------------

// TestGistURLsAreKeyedByGistID pins every URL field on a gist. bleephub emitted
// an owner-scoped /{owner}/{id} html_url and an owner-scoped clone URL; both
// GitHub and a single-host GitHub Enterprise Server key a gist by its id alone.
func TestGistURLsAreKeyedByGistID(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	resp := s.post(t, "/api/v3/gists", defaultToken, map[string]interface{}{
		"description": "url shapes",
		"public":      true,
		"files":       map[string]interface{}{"a.txt": map[string]interface{}{"content": "hi\n"}},
	})
	gist := decodeJSONWithStatus(t, resp, http.StatusCreated)
	id, _ := gist["id"].(string)
	if id == "" {
		t.Fatalf("created gist has no id: %v", gist)
	}
	base := s.baseURL
	for field, want := range map[string]string{
		"url":          base + "/api/v3/gists/" + id,
		"forks_url":    base + "/api/v3/gists/" + id + "/forks",
		"commits_url":  base + "/api/v3/gists/" + id + "/commits",
		"comments_url": base + "/api/v3/gists/" + id + "/comments",
		"html_url":     base + "/gist/" + id,
		"git_pull_url": base + "/gist/" + id + ".git",
		"git_push_url": base + "/gist/" + id + ".git",
	} {
		if got, _ := gist[field].(string); got != want {
			t.Errorf("gist %s = %q, want %q", field, got, want)
		}
	}

	// The same shapes on the read paths, not only on the create response.
	fetched := decodeJSONWithStatus(t, s.get(t, "/api/v3/gists/"+id, defaultToken), http.StatusOK)
	if fetched["html_url"] != gist["html_url"] || fetched["git_pull_url"] != gist["git_pull_url"] {
		t.Errorf("GET gist URLs = %v/%v, want the create response's", fetched["html_url"], fetched["git_pull_url"])
	}
	listed := decodeJSONArrayWithStatus(t, s.get(t, "/api/v3/gists", defaultToken), http.StatusOK)
	if len(listed) == 0 {
		t.Fatal("gist listing is empty")
	}
	for _, item := range listed {
		if item["id"] != id {
			continue
		}
		if item["html_url"] != gist["html_url"] {
			t.Errorf("listed gist html_url = %v, want %v", item["html_url"], gist["html_url"])
		}
	}
}

// --- Git LFS without object storage ---------------------------------------

// TestLFSWorksWithoutConfiguredObjectStorage pins that Git LFS is usable on a
// deployment that never set BLEEPHUB_OBJECT_S3_BUCKET. `git lfs push` used to
// abort against "Git LFS object storage is not configured on this server",
// while the repository's own lfs_enabled default said LFS was on. Real GitHub
// always has LFS.
func TestLFSWorksWithoutConfiguredObjectStorage(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	if s.store.ObjectByteStore != nil {
		t.Fatal("this test must run with no configured object storage")
	}
	resp := s.post(t, "/api/v3/user/repos", defaultToken, map[string]interface{}{"name": "lfs-default"})
	decodeJSONWithStatus(t, resp, http.StatusCreated)
	repo := repoRef{owner: "admin", name: "lfs-default"}
	content := []byte("large file storage payload\n")
	oid := lfsOIDOf(content)

	status, batch, raw := s.lfsBatch(t, repo, defaultToken, lfsBatchBody("upload", content))
	if status != http.StatusOK {
		t.Fatalf("batch(upload) = %d: %s", status, raw)
	}
	if len(batch.Objects) != 1 {
		t.Fatalf("batch objects = %v", batch.Objects)
	}
	upload, ok := batch.Objects[0].Actions["upload"]
	if !ok {
		t.Fatalf("batch(upload) handed out no upload action: %s", raw)
	}
	uploadResp := s.lfsUpload(t, upload.Href, defaultToken, content)
	uploadResp.Body.Close()
	if uploadResp.StatusCode != http.StatusOK {
		t.Fatalf("upload transfer = %d", uploadResp.StatusCode)
	}

	status, batch, raw = s.lfsBatch(t, repo, defaultToken, lfsBatchBody("download", content))
	if status != http.StatusOK {
		t.Fatalf("batch(download) = %d: %s", status, raw)
	}
	download, ok := batch.Objects[0].Actions["download"]
	if !ok {
		t.Fatalf("batch(download) handed out no download action: %s", raw)
	}
	got := s.acceptGet(t, strings.TrimPrefix(download.Href, s.baseURL), "*/*")
	if body := readAllBody(t, got); body != string(content) {
		t.Fatalf("downloaded %q, want %q", body, content)
	}
	if _, held := s.store.LFSObjectSize(repo.fullName(), oid); !held {
		t.Fatalf("the repository does not hold %s after the upload", oid)
	}
}

// TestLocalByteStoreRoundTripsThroughTheFilesystem pins the fallback store
// itself: with a data directory configured it is filesystem-backed, following
// the precedent PackageDataDir set for package files.
func TestLocalByteStoreRoundTripsThroughTheFilesystem(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	byteStore := store.NewLocalByteStore(dir)
	fsStore, ok := byteStore.(*store.FilesystemByteStore)
	if !ok {
		t.Fatalf("NewLocalByteStore(%q) = %T, want a filesystem-backed store", dir, byteStore)
	}
	key := store.LFSObjectDataKey(strings.Repeat("ab", 32))
	payload := []byte("bytes on disk")
	if err := fsStore.Put(t.Context(), key, payload); err != nil {
		t.Fatalf("put: %v", err)
	}
	got, err := fsStore.Get(t.Context(), key)
	if err != nil || string(got) != string(payload) {
		t.Fatalf("get = %q, %v", got, err)
	}
	stream, err := fsStore.GetStream(t.Context(), key)
	if err != nil {
		t.Fatalf("get stream: %v", err)
	}
	streamed, _ := io.ReadAll(stream)
	stream.Close()
	if string(streamed) != string(payload) {
		t.Fatalf("stream = %q, want %q", streamed, payload)
	}
	if err := fsStore.Delete(t.Context(), key); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := fsStore.Get(t.Context(), key); err == nil {
		t.Fatal("get after delete succeeded")
	}
	// A key that could climb out of the root is refused rather than resolved.
	if err := fsStore.Put(t.Context(), "../escape", []byte("nope")); err == nil {
		t.Fatal("a traversing key was accepted")
	}

	// With no data directory the fallback keeps the bytes in the process, like
	// everything else about a server that configured no storage at all.
	if _, ok := store.NewLocalByteStore("").(*store.MemoryByteStore); !ok {
		t.Fatalf("NewLocalByteStore(\"\") = %T, want the in-process store", store.NewLocalByteStore(""))
	}
}
