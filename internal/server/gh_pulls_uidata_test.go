package bleephub

import (
	"encoding/base64"
	"net/http"
	"strings"
	"testing"

	"github.com/e6qu/bleephub/internal/store"
	"github.com/go-git/go-git/v5/plumbing"
)

// seedContentPR creates an auto-init repo, commits each files entry on main,
// branches "sugg" from main, commits each branchFiles entry there, and opens a
// PR sugg→main. Returns the repo, PR number, and the head SHA.
func (s *isolatedServer) seedContentPR(t *testing.T, files, branchFiles map[string]string) (repoRef, int, string) {
	t.Helper()
	repo := s.createRepoWriteRepo(t, true)
	ref := repoRef{owner: "admin", name: repo}
	storeRepo := s.store.GetRepo(ref.owner, ref.name)
	stor := s.store.GetGitStorage(ref.owner, ref.name)
	sig := repoSignature("admin", "bleephub@local")

	for path, content := range files {
		if _, err := createFileCommit(stor, "main", path, content, "seed "+path, sig); err != nil {
			t.Fatalf("commit %s on main: %v", path, err)
		}
	}
	mainHead := store.ResolveBranchSha(stor, "main")
	if err := stor.SetReference(plumbing.NewHashReference(plumbing.NewBranchReferenceName("sugg"), plumbing.NewHash(mainHead))); err != nil {
		t.Fatalf("branch sugg: %v", err)
	}
	head := mainHead
	for path, content := range branchFiles {
		h, err := createFileCommit(stor, "sugg", path, content, "change "+path, sig)
		if err != nil {
			t.Fatalf("commit %s on sugg: %v", path, err)
		}
		head = h.String()
	}
	_ = storeRepo

	pr := decodeJSONWithStatus(t, s.post(t, ref.path()+"/pulls", defaultToken, map[string]interface{}{
		"title": "content pr", "head": "sugg", "base": "main", "body": "body",
	}), http.StatusCreated)
	return ref, int(pr["number"].(float64)), head
}

func TestUIPullFilesIgnoreWhitespace(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	repo, number, _ := s.seedContentPR(t,
		map[string]string{
			"code.txt": "a\nb\nc\nd\ne\n",
			"ws.txt":   "w1\nw2\n",
		},
		map[string]string{
			"code.txt": "a\n  b\nc\nX\ne\n", // line 2 whitespace-only, line 4 real
			"ws.txt":   "w1  \n\tw2\n",      // whitespace-only
		})
	uiPath := "/ui-data/repos/" + repo.fullName() + "/pulls/" + itoa(number) + "/files"

	// Without the flag the body is byte-identical to the REST files endpoint.
	uiStatus, uiBody := fetchTrimmedBody(t, s, uiPath, defaultToken)
	restStatus, restBody := fetchTrimmedBody(t, s, repo.path()+"/pulls/"+itoa(number)+"/files", defaultToken)
	if uiStatus != http.StatusOK || restStatus != http.StatusOK {
		t.Fatalf("files = %d / %d, want 200/200", uiStatus, restStatus)
	}
	if string(uiBody) != string(restBody) {
		t.Errorf("flagless ui-data files differ from REST files:\n ui: %.400s\nrest: %.400s", uiBody, restBody)
	}

	// With the flag: the whitespace-only file disappears, the mixed file keeps
	// only its real change, and the stats are recomputed.
	filtered := decodeJSONArrayWithStatus(t, s.get(t, uiPath+"?ignore_whitespace=1", defaultToken), http.StatusOK)
	if len(filtered) != 1 {
		t.Fatalf("ignore_whitespace files = %d entries (%v), want only code.txt", len(filtered), filtered)
	}
	file := filtered[0]
	if file["filename"] != "code.txt" {
		t.Fatalf("surviving file = %v, want code.txt", file["filename"])
	}
	if int(file["additions"].(float64)) != 1 || int(file["deletions"].(float64)) != 1 || int(file["changes"].(float64)) != 2 {
		t.Errorf("recomputed stats = +%v -%v Δ%v, want +1 -1 Δ2", file["additions"], file["deletions"], file["changes"])
	}
	patch, _ := file["patch"].(string)
	if !strings.Contains(patch, "-d") || !strings.Contains(patch, "+X") {
		t.Errorf("patch lacks the real change:\n%s", patch)
	}
	if strings.Contains(patch, "-b") || strings.Contains(patch, "+  b") {
		t.Errorf("patch still treats the whitespace-only line as a change:\n%s", patch)
	}
	if !strings.HasPrefix(patch, "@@ ") {
		t.Errorf("patch does not start at a hunk header: %.80s", patch)
	}
}

func TestWSInsensitiveUnifiedPatch(t *testing.T) {
	t.Parallel()
	// Whitespace-only edits produce no hunks at all.
	if patch, adds, dels, ok := wsInsensitiveUnifiedPatch(
		[]string{"a", "b", "c"}, []string{" a", "b\t", "  c"}); !ok || patch != "" || adds != 0 || dels != 0 {
		t.Errorf("ws-only diff = (%q, +%d, -%d, %v), want empty", patch, adds, dels, ok)
	}
	// Identical inputs.
	if patch, adds, dels, ok := wsInsensitiveUnifiedPatch([]string{"a"}, []string{"a"}); !ok || patch != "" || adds+dels != 0 {
		t.Errorf("equal diff = (%q, +%d, -%d, %v), want empty", patch, adds, dels, ok)
	}
	// Pure insert at the top of an empty file.
	patch, adds, dels, ok := wsInsensitiveUnifiedPatch(nil, []string{"x", "y"})
	if !ok || adds != 2 || dels != 0 {
		t.Fatalf("insert diff = +%d -%d ok=%v, want +2 -0", adds, dels, ok)
	}
	if patch != "@@ -0,0 +1,2 @@\n+x\n+y" {
		t.Errorf("insert patch = %q", patch)
	}
	// Distant changes split into separate hunks with 3 context lines.
	a := []string{"1", "2", "3", "4", "5", "6", "7", "8", "9", "10", "11", "12", "13", "14", "15"}
	b := append([]string(nil), a...)
	b[0] = "one"
	b[14] = "fifteen"
	patch, adds, dels, ok = wsInsensitiveUnifiedPatch(a, b)
	if !ok || adds != 2 || dels != 2 {
		t.Fatalf("two-change diff = +%d -%d ok=%v, want +2 -2", adds, dels, ok)
	}
	if strings.Count(patch, "@@ -") != 2 {
		t.Errorf("two distant changes rendered %d hunks, want 2:\n%s", strings.Count(patch, "@@ -"), patch)
	}
	if !strings.Contains(patch, "@@ -1,4 +1,4 @@\n-1\n+one\n 2\n 3\n 4") {
		t.Errorf("first hunk malformed:\n%s", patch)
	}
	if !strings.Contains(patch, "@@ -12,4 +12,4 @@\n 12\n 13\n 14\n-15\n+fifteen") {
		t.Errorf("second hunk malformed:\n%s", patch)
	}
	// Nearby changes merge into one hunk; context shows the NEW side text for
	// whitespace-shifted lines.
	patch, adds, dels, ok = wsInsensitiveUnifiedPatch(
		[]string{"ctx1", "old", "mid", "gone", "ctx2"},
		[]string{"ctx1", "new", "  mid", "ctx2"})
	if !ok || adds != 1 || dels != 2 {
		t.Fatalf("merge diff = +%d -%d ok=%v, want +1 -2", adds, dels, ok)
	}
	if strings.Count(patch, "@@ -") != 1 {
		t.Errorf("nearby changes should share one hunk:\n%s", patch)
	}
	if !strings.Contains(patch, "   mid") { // ' ' prefix + "  mid" (new side)
		t.Errorf("context should carry the new-side whitespace:\n%s", patch)
	}
}

func TestUIApplySuggestion(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	repo, number, headSHA := s.seedContentPR(t,
		map[string]string{"code.txt": "a\nb\nc\nd\ne\n"},
		map[string]string{"code.txt": "a\nb\nc\nX\ne\n"})

	comment := decodeJSONWithStatus(t, s.post(t, repo.path()+"/pulls/"+itoa(number)+"/comments", defaultToken,
		map[string]interface{}{
			"body":      "use Y instead\n```suggestion\nY\n```\ntrailing",
			"commit_id": headSHA,
			"path":      "code.txt",
			"line":      4,
			"side":      "RIGHT",
		}), http.StatusCreated)
	commentID := int(comment["id"].(float64))

	applyPath := "/ui-data/repos/" + repo.fullName() + "/pulls/" + itoa(number) +
		"/review-comments/" + itoa(commentID) + "/apply-suggestion"
	applied := decodeJSONWithStatus(t, s.post(t, applyPath, defaultToken, map[string]interface{}{}), http.StatusCreated)
	sha, _ := applied["sha"].(string)
	if len(sha) != 40 {
		t.Fatalf("apply-suggestion sha = %q, want a 40-char commit sha", sha)
	}

	// The head branch now carries the suggestion.
	contents := decodeJSONWithStatus(t, s.get(t, repo.path()+"/contents/code.txt?ref=sugg", defaultToken), http.StatusOK)
	decoded, err := base64.StdEncoding.DecodeString(strings.ReplaceAll(contents["content"].(string), "\n", ""))
	if err != nil {
		t.Fatalf("decode contents: %v", err)
	}
	if string(decoded) != "a\nb\nc\nY\ne\n" {
		t.Errorf("head content = %q, want the suggestion applied", decoded)
	}

	// The commit is authored by the caller and co-authored by the comment
	// author, under GitHub's suggestion commit message.
	commit := decodeJSONWithStatus(t, s.get(t, repo.path()+"/commits/"+sha, defaultToken), http.StatusOK)
	commitObj, _ := commit["commit"].(map[string]interface{})
	message, _ := commitObj["message"].(string)
	if !strings.HasPrefix(message, "Apply suggestions from code review") || !strings.Contains(message, "Co-authored-by: admin <") {
		t.Errorf("commit message = %q, want the suggestion message with a Co-authored-by trailer", message)
	}
}

func TestUIApplySuggestionMultiLineRange(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	repo, number, headSHA := s.seedContentPR(t,
		map[string]string{"code.txt": "a\nb\nc\n"},
		map[string]string{"code.txt": "one\ntwo\nc\n"})

	comment := decodeJSONWithStatus(t, s.post(t, repo.path()+"/pulls/"+itoa(number)+"/comments", defaultToken,
		map[string]interface{}{
			"body":       "```suggestion\nmerged\n```",
			"commit_id":  headSHA,
			"path":       "code.txt",
			"start_line": 1,
			"line":       2,
			"side":       "RIGHT",
		}), http.StatusCreated)
	applyPath := "/ui-data/repos/" + repo.fullName() + "/pulls/" + itoa(number) +
		"/review-comments/" + itoa(int(comment["id"].(float64))) + "/apply-suggestion"
	decodeJSONWithStatus(t, s.post(t, applyPath, defaultToken, map[string]interface{}{}), http.StatusCreated)

	contents := decodeJSONWithStatus(t, s.get(t, repo.path()+"/contents/code.txt?ref=sugg", defaultToken), http.StatusOK)
	decoded, err := base64.StdEncoding.DecodeString(strings.ReplaceAll(contents["content"].(string), "\n", ""))
	if err != nil {
		t.Fatalf("decode contents: %v", err)
	}
	if string(decoded) != "merged\nc\n" {
		t.Errorf("head content = %q, want the two-line range replaced by one line", decoded)
	}
}

func TestUIApplySuggestionRefusals(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	repo, number, headSHA := s.seedContentPR(t,
		map[string]string{"code.txt": "a\nb\nc\nd\ne\n"},
		map[string]string{"code.txt": "a\nb\nc\nX\ne\n"})
	postComment := func(body map[string]interface{}) int {
		t.Helper()
		defaults := map[string]interface{}{
			"commit_id": headSHA, "path": "code.txt", "line": 4, "side": "RIGHT",
		}
		for k, v := range body {
			defaults[k] = v
		}
		c := decodeJSONWithStatus(t, s.post(t, repo.path()+"/pulls/"+itoa(number)+"/comments", defaultToken, defaults), http.StatusCreated)
		return int(c["id"].(float64))
	}
	applyPath := func(id int) string {
		return "/ui-data/repos/" + repo.fullName() + "/pulls/" + itoa(number) +
			"/review-comments/" + itoa(id) + "/apply-suggestion"
	}

	// No suggestion fence (and an unterminated fence) → 422.
	noFence := postComment(map[string]interface{}{"body": "just words"})
	requireStatus(t, s.post(t, applyPath(noFence), defaultToken, nil), http.StatusUnprocessableEntity)
	unterminated := postComment(map[string]interface{}{"body": "```suggestion\nY"})
	requireStatus(t, s.post(t, applyPath(unterminated), defaultToken, nil), http.StatusUnprocessableEntity)

	// LEFT-side comments cannot be applied.
	left := postComment(map[string]interface{}{"body": "```suggestion\nY\n```", "side": "LEFT"})
	requireStatus(t, s.post(t, applyPath(left), defaultToken, nil), http.StatusUnprocessableEntity)

	// A viewer without push access to the head repo is refused.
	good := postComment(map[string]interface{}{"body": "```suggestion\nY\n```"})
	_, readerTok := s.newUser(t, "suggestion-reader")
	requireStatus(t, s.post(t, applyPath(good), readerTok, nil), http.StatusForbidden)

	// An unknown comment id 404s.
	requireStatus(t, s.post(t, applyPath(999999), defaultToken, nil), http.StatusNotFound)

	// Outdated: the commented line changed on the head branch since → 409.
	stor := s.store.GetGitStorage(repo.owner, repo.name)
	if _, err := createFileCommit(stor, "sugg", "code.txt", "a\nb\nc\nDRIFTED\ne\n", "drift", repoSignature("admin", "bleephub@local")); err != nil {
		t.Fatalf("drift commit: %v", err)
	}
	requireStatus(t, s.post(t, applyPath(good), defaultToken, nil), http.StatusConflict)

	// Closed PRs refuse suggestions.
	fresh := postComment(map[string]interface{}{
		"body":      "```suggestion\nZ\n```",
		"commit_id": store.ResolveBranchSha(stor, "sugg"),
	})
	decodeJSONWithStatus(t, s.patch(t, repo.path()+"/pulls/"+itoa(number), defaultToken,
		map[string]interface{}{"state": "closed"}), http.StatusOK)
	requireStatus(t, s.post(t, applyPath(fresh), defaultToken, nil), http.StatusUnprocessableEntity)
}
