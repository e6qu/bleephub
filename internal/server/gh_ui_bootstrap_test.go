package bleephub

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"testing"
	"time"
)

// fetchTrimmedBody GETs a path and returns (status, body without the trailing
// newline writeJSON appends), so standalone endpoint bodies compare literally
// against the raw sub-payloads embedded in a bootstrap aggregate.
func fetchTrimmedBody(t *testing.T, s *isolatedServer, path, token string) (int, []byte) {
	t.Helper()
	resp := s.get(t, path, token)
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return resp.StatusCode, bytes.TrimSuffix(body, []byte("\n"))
}

// decodeBootstrap decodes an aggregate body into raw per-key sub-payloads.
func decodeBootstrap(t *testing.T, s *isolatedServer, path string) map[string]json.RawMessage {
	t.Helper()
	status, body := fetchTrimmedBody(t, s, path, defaultToken)
	if status != http.StatusOK {
		t.Fatalf("GET %s = %d, want 200; body=%s", path, status, body)
	}
	var out map[string]json.RawMessage
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("decode %s: %v", path, err)
	}
	return out
}

// assertSubEqualsStandalone asserts a bootstrap sub-payload is byte-identical
// to the standalone endpoint's body for the same viewer.
func assertSubEqualsStandalone(t *testing.T, s *isolatedServer, agg map[string]json.RawMessage, key, standalonePath string) {
	t.Helper()
	status, want := fetchTrimmedBody(t, s, standalonePath, defaultToken)
	if status != http.StatusOK {
		t.Fatalf("standalone GET %s = %d, want 200; body=%s", standalonePath, status, want)
	}
	got, ok := agg[key]
	if !ok {
		t.Fatalf("aggregate is missing key %q", key)
	}
	if !bytes.Equal(bytes.TrimSpace(got), want) {
		t.Errorf("aggregate %q != standalone %s\n got: %.300s\nwant: %.300s", key, standalonePath, got, want)
	}
}

func TestUIBootstrapRepoSubPayloadsMatchStandaloneEndpoints(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	repo := s.sweepRepo(t, "ui-bootstrap-repo")
	s.putRepoFile(t, repo.fullName(), "docs/guide.md", "# guide\n", "add docs")
	_, _ = s.createIssueForTest(t, repo, "open issue for count")
	_, _ = s.sweepPR(t, repo, "open pr for count")

	agg := decodeBootstrap(t, s, "/ui-data/bootstrap"+trimAPIPrefix(repo.path()))

	assertSubEqualsStandalone(t, s, agg, "repo", repo.path())
	assertSubEqualsStandalone(t, s, agg, "readme", repo.path()+"/readme")
	assertSubEqualsStandalone(t, s, agg, "root_entries", repo.path()+"/contents/")
	assertSubEqualsStandalone(t, s, agg, "languages", repo.path()+"/languages")
	assertSubEqualsStandalone(t, s, agg, "contributors", repo.path()+"/contributors")
	assertSubEqualsStandalone(t, s, agg, "latest_commit", repo.path()+"/commits/main")

	// branches/tags carry the standalone first page plus an exact total.
	var branches struct {
		FirstPage  json.RawMessage `json:"first_page"`
		TotalCount int             `json:"total_count"`
	}
	if err := json.Unmarshal(agg["branches"], &branches); err != nil {
		t.Fatalf("decode branches: %v", err)
	}
	// first_page is the standalone endpoint's per_page=100 page — the query
	// the UI's branch/tag hooks issue.
	_, wantBranches := fetchTrimmedBody(t, s, repo.path()+"/branches?per_page=100", defaultToken)
	if !bytes.Equal(bytes.TrimSpace(branches.FirstPage), wantBranches) {
		t.Errorf("branches.first_page != standalone /branches?per_page=100:\n got: %.300s\nwant: %.300s", branches.FirstPage, wantBranches)
	}
	// sweepRepo seeds main + the "feature" PR branch.
	if branches.TotalCount != 2 {
		t.Errorf("branches.total_count = %d, want 2", branches.TotalCount)
	}
	var tags struct {
		FirstPage  json.RawMessage `json:"first_page"`
		TotalCount int             `json:"total_count"`
	}
	if err := json.Unmarshal(agg["tags"], &tags); err != nil {
		t.Fatalf("decode tags: %v", err)
	}
	if tags.TotalCount != 0 || string(bytes.TrimSpace(tags.FirstPage)) != "[]" {
		t.Errorf("tags = {%s, %d}, want {[], 0}", tags.FirstPage, tags.TotalCount)
	}

	// No release exists: the nullable sub-resource is null exactly where the
	// standalone endpoint 404s.
	if status, _ := fetchTrimmedBody(t, s, repo.path()+"/releases/latest", defaultToken); status != http.StatusNotFound {
		t.Fatalf("standalone /releases/latest = %d, want 404", status)
	}
	if string(agg["latest_release"]) != "null" {
		t.Errorf("latest_release = %s, want null", agg["latest_release"])
	}

	var pullsOpen, issuesOpen int
	if err := json.Unmarshal(agg["pulls_open_count"], &pullsOpen); err != nil {
		t.Fatalf("decode pulls_open_count: %v", err)
	}
	if err := json.Unmarshal(agg["issues_open_count"], &issuesOpen); err != nil {
		t.Fatalf("decode issues_open_count: %v", err)
	}
	if pullsOpen != 1 {
		t.Errorf("pulls_open_count = %d, want 1", pullsOpen)
	}
	// issues_open_count excludes the open PR — the real issue count.
	if issuesOpen != 1 {
		t.Errorf("issues_open_count = %d, want 1 (must exclude PRs)", issuesOpen)
	}
	var discussionsEnabled bool
	if err := json.Unmarshal(agg["discussions_enabled"], &discussionsEnabled); err != nil {
		t.Fatalf("decode discussions_enabled: %v", err)
	}

	// The aggregate flows through the standard ETag layer like any writeJSON
	// endpoint: a strong validator on 200, and a 304 on If-None-Match.
	first := s.get(t, "/ui-data/bootstrap"+trimAPIPrefix(repo.path()), defaultToken)
	first.Body.Close()
	etag := first.Header.Get("ETag")
	if etag == "" {
		t.Fatal("aggregate response carries no ETag")
	}
	req, err := http.NewRequest(http.MethodGet, s.baseURL+"/ui-data/bootstrap"+trimAPIPrefix(repo.path()), nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "token "+defaultToken)
	req.Header.Set("If-None-Match", etag)
	conditional, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	conditional.Body.Close()
	if conditional.StatusCode != http.StatusNotModified {
		t.Errorf("conditional aggregate GET = %d, want 304", conditional.StatusCode)
	}

	// And through the standard /ui-data compression layer: an explicit
	// Accept-Encoding (which disables the client's transparent decoding)
	// yields a gzipped representation.
	gzReq, err := http.NewRequest(http.MethodGet, s.baseURL+"/ui-data/bootstrap"+trimAPIPrefix(repo.path()), nil)
	if err != nil {
		t.Fatal(err)
	}
	gzReq.Header.Set("Authorization", "token "+defaultToken)
	gzReq.Header.Set("Accept-Encoding", "gzip")
	gzResp, err := http.DefaultClient.Do(gzReq)
	if err != nil {
		t.Fatal(err)
	}
	gzResp.Body.Close()
	if got := gzResp.Header.Get("Content-Encoding"); got != "gzip" {
		t.Errorf("aggregate Content-Encoding = %q, want gzip", got)
	}
}

func TestUIBootstrapIssueSubPayloadsMatchStandaloneEndpoints(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	repo := s.sweepRepo(t, "ui-bootstrap-issue")
	_, number := s.createIssueForTest(t, repo, "bootstrap issue")
	numStr := itoa(number)
	mustPost(t, s.post(t, repo.path()+"/issues/"+numStr+"/comments", defaultToken,
		map[string]interface{}{"body": "first comment"}))
	mustPost(t, s.post(t, repo.path()+"/labels", defaultToken,
		map[string]interface{}{"name": "regression", "color": "ff0000"}))
	mustPost(t, s.post(t, repo.path()+"/milestones", defaultToken,
		map[string]interface{}{"title": "v1"}))
	// A closed milestone makes state=all observably different from state=open,
	// pinning the aggregate's milestones source as ?state=all.
	closedMilestone := decodeJSONWithStatus(t, s.post(t, repo.path()+"/milestones", defaultToken,
		map[string]interface{}{"title": "v0-closed"}), http.StatusCreated)
	closedNum := itoa(int(closedMilestone["number"].(float64)))
	decodeJSONWithStatus(t, s.patch(t, repo.path()+"/milestones/"+closedNum, defaultToken,
		map[string]interface{}{"state": "closed"}), http.StatusOK)

	agg := decodeBootstrap(t, s, "/ui-data/bootstrap"+trimAPIPrefix(repo.path())+"/issues/"+numStr)

	assertSubEqualsStandalone(t, s, agg, "issue", repo.path()+"/issues/"+numStr)
	assertSubEqualsStandalone(t, s, agg, "comments", repo.path()+"/issues/"+numStr+"/comments")
	assertSubEqualsStandalone(t, s, agg, "timeline", repo.path()+"/issues/"+numStr+"/timeline")
	assertSubEqualsStandalone(t, s, agg, "labels", repo.path()+"/labels")
	assertSubEqualsStandalone(t, s, agg, "milestones", repo.path()+"/milestones?state=all")
	assertSubEqualsStandalone(t, s, agg, "assignees_available", repo.path()+"/assignees")
	// state=all embeds both milestones; state=open would drop the closed one.
	var milestones []json.RawMessage
	if err := json.Unmarshal(agg["milestones"], &milestones); err != nil {
		t.Fatalf("decode milestones: %v", err)
	}
	if len(milestones) != 2 {
		t.Errorf("milestones embeds %d rows, want 2 (state=all includes the closed one)", len(milestones))
	}

	// A number the issue endpoint refuses is refused identically here.
	status, body := fetchTrimmedBody(t, s, "/ui-data/bootstrap"+trimAPIPrefix(repo.path())+"/issues/999999", defaultToken)
	wantStatus, wantBody := fetchTrimmedBody(t, s, repo.path()+"/issues/999999", defaultToken)
	if status != wantStatus || status != http.StatusNotFound {
		t.Errorf("missing-issue bootstrap = %d, standalone = %d, want both 404", status, wantStatus)
	}
	if !bytes.Equal(body, wantBody) {
		t.Errorf("missing-issue bootstrap body = %s, want standalone %s", body, wantBody)
	}
}

func TestUIBootstrapPullSubPayloadsMatchStandaloneEndpoints(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	repo := s.sweepRepo(t, "ui-bootstrap-pull")
	number, _ := s.sweepPR(t, repo, "bootstrap pr")
	numStr := itoa(number)
	mustPost(t, s.post(t, repo.path()+"/issues/"+numStr+"/comments", defaultToken,
		map[string]interface{}{"body": "conversation comment"}))
	reviewResp := s.post(t, repo.path()+"/pulls/"+numStr+"/reviews", defaultToken,
		map[string]interface{}{"body": "looks good", "event": "COMMENT"})
	decodeJSONWithStatus(t, reviewResp, http.StatusOK)

	agg := decodeBootstrap(t, s, "/ui-data/bootstrap"+trimAPIPrefix(repo.path())+"/pulls/"+numStr)

	assertSubEqualsStandalone(t, s, agg, "pull", repo.path()+"/pulls/"+numStr)
	assertSubEqualsStandalone(t, s, agg, "timeline", repo.path()+"/issues/"+numStr+"/timeline")
	assertSubEqualsStandalone(t, s, agg, "comments", repo.path()+"/issues/"+numStr+"/comments")
	assertSubEqualsStandalone(t, s, agg, "reviews", repo.path()+"/pulls/"+numStr+"/reviews")
	assertSubEqualsStandalone(t, s, agg, "review_comments", repo.path()+"/pulls/"+numStr+"/comments")
	assertSubEqualsStandalone(t, s, agg, "requested_reviewers", repo.path()+"/pulls/"+numStr+"/requested_reviewers")
	// The PR aggregate embeds the same sidebar sources as the issue aggregate.
	assertSubEqualsStandalone(t, s, agg, "labels", repo.path()+"/labels")
	assertSubEqualsStandalone(t, s, agg, "milestones", repo.path()+"/milestones?state=all")
	assertSubEqualsStandalone(t, s, agg, "assignees_available", repo.path()+"/assignees")

	var pull struct {
		Head struct {
			SHA string `json:"sha"`
		} `json:"head"`
		ChangedFiles int `json:"changed_files"`
		Additions    int `json:"additions"`
		Deletions    int `json:"deletions"`
	}
	if err := json.Unmarshal(agg["pull"], &pull); err != nil {
		t.Fatalf("decode pull: %v", err)
	}
	assertSubEqualsStandalone(t, s, agg, "check_runs", repo.path()+"/commits/"+pull.Head.SHA+"/check-runs")
	assertSubEqualsStandalone(t, s, agg, "combined_status", repo.path()+"/commits/"+pull.Head.SHA+"/status")

	var files struct {
		ChangedFiles int `json:"changed_files"`
		Additions    int `json:"additions"`
		Deletions    int `json:"deletions"`
	}
	if err := json.Unmarshal(agg["files_summary"], &files); err != nil {
		t.Fatalf("decode files_summary: %v", err)
	}
	if files.ChangedFiles != pull.ChangedFiles || files.Additions != pull.Additions || files.Deletions != pull.Deletions {
		t.Errorf("files_summary = %+v, want the pull's own diff stats %d/%d/%d",
			files, pull.ChangedFiles, pull.Additions, pull.Deletions)
	}

	// An issue number is refused by the pulls bootstrap exactly like the
	// standalone pull endpoint refuses it.
	_, issueNumber := s.createIssueForTest(t, repo, "not a pr")
	status, body := fetchTrimmedBody(t, s, "/ui-data/bootstrap"+trimAPIPrefix(repo.path())+"/pulls/"+itoa(issueNumber), defaultToken)
	wantStatus, wantBody := fetchTrimmedBody(t, s, repo.path()+"/pulls/"+itoa(issueNumber), defaultToken)
	if status != wantStatus || status != http.StatusNotFound {
		t.Errorf("issue-number pull bootstrap = %d, standalone = %d, want both 404", status, wantStatus)
	}
	if !bytes.Equal(body, wantBody) {
		t.Errorf("issue-number pull bootstrap body = %s, want standalone %s", body, wantBody)
	}
}

// putTreeMetaUpdate updates an existing file (contents PUT requires the blob
// sha for updates) and returns the new commit sha.
func putTreeMetaUpdate(t *testing.T, s *isolatedServer, repo repoRef, path, content, message string) string {
	t.Helper()
	current := decodeJSON(t, s.get(t, repo.path()+"/contents/"+path, defaultToken))
	blobSHA, _ := current["sha"].(string)
	if blobSHA == "" {
		t.Fatalf("no blob sha for %s", path)
	}
	resp := s.put(t, repo.path()+"/contents/"+path, defaultToken, map[string]interface{}{
		"message": message,
		"content": base64.StdEncoding.EncodeToString([]byte(content)),
		"sha":     blobSHA,
	})
	out := decodeJSONWithStatus(t, resp, http.StatusOK)
	commit := out["commit"].(map[string]interface{})
	return commit["sha"].(string)
}

func TestUITreeMetaAttributesEntriesInOneLogWalk(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	repo := s.createTestRepo(t)
	c1 := s.putRepoFile(t, repo.fullName(), "a.txt", "one\n", "c1: add a.txt")
	c2 := s.putRepoFile(t, repo.fullName(), "docs/guide.md", "# guide\n", "c2: add docs/guide.md")
	c3 := s.putRepoFile(t, repo.fullName(), "b.txt", "two\n", "c3: add b.txt")
	c4 := s.putRepoFile(t, repo.fullName(), "docs/deeper/x.md", "x\n", "c4: add docs/deeper/x.md")
	c5 := putTreeMetaUpdate(t, s, repo, "a.txt", "one edited\n", "c5: edit a.txt")

	type latest struct {
		SHA             string `json:"sha"`
		MessageHeadline string `json:"message_headline"`
		AuthorLogin     string `json:"author_login"`
		AuthorDate      string `json:"author_date"`
	}
	type entry struct {
		Name   string  `json:"name"`
		Path   string  `json:"path"`
		Type   string  `json:"type"`
		Size   int64   `json:"size"`
		Latest *latest `json:"latest"`
	}
	var root struct {
		Ref          string          `json:"ref"`
		Path         string          `json:"path"`
		LatestCommit json.RawMessage `json:"latest_commit"`
		Entries      []entry         `json:"entries"`
	}
	status, body := fetchTrimmedBody(t, s, "/ui-data"+trimAPIPrefix(repo.path())+"/tree-meta", defaultToken)
	if status != http.StatusOK {
		t.Fatalf("tree-meta root = %d; body=%s", status, body)
	}
	if err := json.Unmarshal(body, &root); err != nil {
		t.Fatal(err)
	}

	want := map[string]struct {
		typ string
		sha string
	}{
		"a.txt": {"file", c5}, // edited later: newest touching commit wins
		"b.txt": {"file", c3}, // sibling untouched since creation
		"docs":  {"dir", c4},  // prefix match: a change under docs/ attributes docs
	}
	if len(root.Entries) != len(want) {
		t.Fatalf("root entries = %d, want %d: %+v", len(root.Entries), len(want), root.Entries)
	}
	for _, e := range root.Entries {
		w, ok := want[e.Name]
		if !ok {
			t.Errorf("unexpected entry %q", e.Name)
			continue
		}
		if e.Type != w.typ {
			t.Errorf("%s type = %q, want %q", e.Name, e.Type, w.typ)
		}
		if e.Latest == nil || e.Latest.SHA != w.sha {
			t.Errorf("%s latest = %+v, want sha %s", e.Name, e.Latest, w.sha)
		}
		if e.Latest != nil && (e.Latest.MessageHeadline == "" || e.Latest.AuthorLogin == "" || e.Latest.AuthorDate == "") {
			t.Errorf("%s latest missing fields: %+v", e.Name, e.Latest)
		}
		if e.Type == "file" && e.Size == 0 {
			t.Errorf("%s size = 0, want blob size", e.Name)
		}
	}
	// Root latest_commit is the tip, in the commits-list JSON shape.
	var rootLatest struct {
		SHA string `json:"sha"`
	}
	if err := json.Unmarshal(root.LatestCommit, &rootLatest); err != nil || rootLatest.SHA != c5 {
		t.Errorf("root latest_commit sha = %q (err=%v), want %s", rootLatest.SHA, err, c5)
	}

	// Scoped to docs/: entries are attributed within the subtree, and
	// latest_commit is the newest commit touching docs/ (c4), not the tip.
	var docs struct {
		LatestCommit json.RawMessage `json:"latest_commit"`
		Entries      []entry         `json:"entries"`
	}
	status, body = fetchTrimmedBody(t, s, "/ui-data"+trimAPIPrefix(repo.path())+"/tree-meta?path=docs", defaultToken)
	if status != http.StatusOK {
		t.Fatalf("tree-meta docs = %d; body=%s", status, body)
	}
	if err := json.Unmarshal(body, &docs); err != nil {
		t.Fatal(err)
	}
	wantDocs := map[string]string{"guide.md": c2, "deeper": c4}
	if len(docs.Entries) != len(wantDocs) {
		t.Fatalf("docs entries = %d, want %d: %+v", len(docs.Entries), len(wantDocs), docs.Entries)
	}
	for _, e := range docs.Entries {
		if sha, ok := wantDocs[e.Name]; !ok || e.Latest == nil || e.Latest.SHA != sha {
			t.Errorf("docs/%s latest = %+v, want sha %s", e.Name, e.Latest, wantDocs[e.Name])
		}
	}
	var docsLatest struct {
		SHA string `json:"sha"`
	}
	if err := json.Unmarshal(docs.LatestCommit, &docsLatest); err != nil || docsLatest.SHA != c4 {
		t.Errorf("docs latest_commit sha = %q (err=%v), want %s", docsLatest.SHA, err, c4)
	}
	_ = c1 // creation commit of a.txt is superseded by the c5 edit

	// Unknown ref and non-directory path 404.
	if status, _ := fetchTrimmedBody(t, s, "/ui-data"+trimAPIPrefix(repo.path())+"/tree-meta?ref=no-such-branch", defaultToken); status != http.StatusNotFound {
		t.Errorf("tree-meta bad ref = %d, want 404", status)
	}
	if status, _ := fetchTrimmedBody(t, s, "/ui-data"+trimAPIPrefix(repo.path())+"/tree-meta?path=missing-dir", defaultToken); status != http.StatusNotFound {
		t.Errorf("tree-meta missing dir = %d, want 404", status)
	}
}

func TestUIBootstrapInsightsCountsExactly(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	repo := s.sweepRepo(t, "ui-bootstrap-insights")
	// Fixture commits are stamped by the git layer, not the injectable server
	// clock, so anchor "now" to the tip commit's own timestamp: every fixture
	// then falls inside the 1w window without the test reading the wall clock.
	var tip []struct {
		Commit struct {
			Committer struct {
				Date string `json:"date"`
			} `json:"committer"`
		} `json:"commit"`
	}
	_, tipBody := fetchTrimmedBody(t, s, repo.path()+"/commits?per_page=1", defaultToken)
	if err := json.Unmarshal(tipBody, &tip); err != nil || len(tip) == 0 {
		t.Fatalf("tip commit: err=%v body=%s", err, tipBody)
	}
	tipDate, err := time.Parse(time.RFC3339, tip[0].Commit.Committer.Date)
	if err != nil {
		t.Fatal(err)
	}
	anchored := tipDate.Add(time.Hour)
	s.replaceClockNow(func() time.Time { return anchored })

	_, closedIssue := s.createIssueForTest(t, repo, "will close")
	_, _ = s.createIssueForTest(t, repo, "stays open")
	patch := s.patch(t, repo.path()+"/issues/"+itoa(closedIssue), defaultToken,
		map[string]interface{}{"state": "closed"})
	decodeJSONWithStatus(t, patch, http.StatusOK)

	mergedPR, _ := s.sweepPR(t, repo, "will merge")
	merge := s.put(t, repo.path()+"/pulls/"+itoa(mergedPR)+"/merge", defaultToken,
		map[string]interface{}{})
	decodeJSONWithStatus(t, merge, http.StatusOK)
	seedPullRequestBranches(t, s.Server, s.store.GetRepo(repo.owner, repo.name), "feature-two")
	open := s.post(t, repo.path()+"/pulls", defaultToken, map[string]interface{}{
		"title": "stays open", "head": "feature-two", "base": "main",
	})
	decodeJSONWithStatus(t, open, http.StatusCreated)

	status, body := fetchTrimmedBody(t, s, "/ui-data/bootstrap"+trimAPIPrefix(repo.path())+"/insights?period=1w", defaultToken)
	if status != http.StatusOK {
		t.Fatalf("insights = %d; body=%s", status, body)
	}
	var out struct {
		Period             string `json:"period"`
		MergedPRs          int    `json:"merged_prs_count"`
		OpenedPRs          int    `json:"opened_prs_count"`
		ClosedIssues       int    `json:"closed_issues_count"`
		NewIssues          int    `json:"new_issues_count"`
		ActiveContributors int    `json:"active_contributors"`
		TopContributors    []struct {
			Login   string `json:"login"`
			Commits int    `json:"commits"`
		} `json:"top_contributors"`
		CommitActivity json.RawMessage `json:"commit_activity"`
		Languages      json.RawMessage `json:"languages"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatal(err)
	}
	if out.Period != "1w" {
		t.Errorf("period = %q, want 1w", out.Period)
	}
	if out.MergedPRs != 1 {
		t.Errorf("merged_prs_count = %d, want 1", out.MergedPRs)
	}
	if out.OpenedPRs != 2 {
		t.Errorf("opened_prs_count = %d, want 2 (merged + open)", out.OpenedPRs)
	}
	if out.ClosedIssues != 1 {
		t.Errorf("closed_issues_count = %d, want 1", out.ClosedIssues)
	}
	if out.NewIssues != 2 {
		t.Errorf("new_issues_count = %d, want 2", out.NewIssues)
	}
	if out.ActiveContributors != 1 {
		t.Errorf("active_contributors = %d, want 1 (admin only)", out.ActiveContributors)
	}
	if len(out.TopContributors) != 1 || out.TopContributors[0].Login != "admin" || out.TopContributors[0].Commits < 1 {
		t.Errorf("top_contributors = %+v, want [{admin, >=1}]", out.TopContributors)
	}

	var agg map[string]json.RawMessage
	if err := json.Unmarshal(body, &agg); err != nil {
		t.Fatal(err)
	}
	assertSubEqualsStandalone(t, s, agg, "commit_activity", repo.path()+"/stats/commit_activity")
	assertSubEqualsStandalone(t, s, agg, "languages", repo.path()+"/languages")

	if status, _ := fetchTrimmedBody(t, s, "/ui-data/bootstrap"+trimAPIPrefix(repo.path())+"/insights?period=2y", defaultToken); status != http.StatusUnprocessableEntity {
		t.Errorf("insights invalid period = %d, want 422", status)
	}
}

func TestUIBootstrapEndpointsHidePrivateReposLikeTheRepoEndpoint(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	repo := s.seedRepo(t, "ui-bootstrap-private", true)
	_, outsiderToken := s.newUser(t, "ui-bootstrap-outsider")

	base := "/repos/" + repo.FullName
	paths := []string{
		"/ui-data/bootstrap" + base,
		"/ui-data" + base + "/tree-meta",
		"/ui-data/bootstrap" + base + "/issues/1",
		"/ui-data/bootstrap" + base + "/pulls/1",
		"/ui-data/bootstrap" + base + "/insights",
	}
	wantStatus, wantBody := fetchTrimmedBody(t, s, "/api/v3"+base, outsiderToken)
	if wantStatus != http.StatusNotFound {
		t.Fatalf("standalone private repo for outsider = %d, want 404", wantStatus)
	}
	for _, path := range paths {
		status, body := fetchTrimmedBody(t, s, path, outsiderToken)
		if status != http.StatusNotFound {
			t.Errorf("GET %s as outsider = %d, want 404", path, status)
		}
		if !bytes.Equal(body, wantBody) {
			t.Errorf("GET %s 404 body = %s, want the repo endpoint's %s", path, body, wantBody)
		}
		anonStatus, _ := fetchTrimmedBody(t, s, path, "")
		if anonStatus != http.StatusNotFound {
			t.Errorf("GET %s anonymously = %d, want 404", path, anonStatus)
		}
	}

	// The owner still gets the aggregate for the private repo.
	if status, _ := fetchTrimmedBody(t, s, "/ui-data/bootstrap"+base, defaultToken); status != http.StatusOK {
		t.Errorf("owner GET private bootstrap = %d, want 200", status)
	}
}

// trimAPIPrefix converts a repoRef.path() ("/api/v3/repos/o/r") into the
// "/repos/o/r" suffix the /ui-data tree-meta route uses.
func trimAPIPrefix(apiPath string) string {
	return apiPath[len("/api/v3"):]
}
