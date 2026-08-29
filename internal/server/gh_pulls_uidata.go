package bleephub

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/e6qu/bleephub/internal/store"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	gitStorage "github.com/go-git/go-git/v5/storage"
)

// Browser-only pull request operations. GitHub serves both from its web UI
// only ("Commit suggestion" has no REST operation, hiding whitespace is the
// web diff's `w=1` toggle), so they live under /ui-data. The files endpoint
// with ignore_whitespace=1 recomputes each patch whitespace-insensitively;
// apply-suggestion commits the first ```suggestion fence onto the PR head
// branch, co-authored by the comment author.
func (s *Server) registerGHPullsUIDataRoutes() {
	s.route("GET /ui-data/repos/{owner}/{repo}/pulls/{number}/files", s.handleUIPullFiles)
	s.route("POST /ui-data/repos/{owner}/{repo}/pulls/{number}/review-comments/{comment_id}/apply-suggestion", s.handleUIApplySuggestion)
}

// whitespace-ignoring PR files

func (s *Server) handleUIPullFiles(w http.ResponseWriter, r *http.Request) {
	repo := s.lookupReadableRepoFromPath(w, r)
	if repo == nil {
		return
	}
	num, err := strconv.Atoi(r.PathValue("number"))
	if err != nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	pr := s.store.GetPullRequestByNumber(repo.ID, num)
	if pr == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	files, err := pullRequestChangedFiles(s.store, repo, pr, s.baseURL(r))
	if err != nil {
		writeGHError(w, http.StatusInternalServerError, "diff derivation failed")
		return
	}
	if q := r.URL.Query().Get("ignore_whitespace"); q != "1" && q != "true" {
		writeJSON(w, http.StatusOK, paginateAndLink(w, r, files))
		return
	}

	changes := pullRequestDiffChanges(s.store, repo, pr)
	out := make([]map[string]interface{}, 0, len(files))
	for _, file := range files {
		name, _ := file["filename"].(string)
		status, _ := file["status"].(string)
		patch, _ := file["patch"].(string)
		ch := changes[name]
		// Added/removed/binary files, and changes that can't be re-derived,
		// keep the REST rendering: hiding whitespace never hides a whole-file
		// addition or removal.
		if ch == nil || patch == "" || status == "added" || status == "removed" {
			out = append(out, file)
			continue
		}
		fromLines, toLines, ok := changeFileLines(ch)
		if !ok {
			out = append(out, file)
			continue
		}
		wsPatch, adds, dels, ok := wsInsensitiveUnifiedPatch(fromLines, toLines)
		if !ok {
			// Bounded diff gave up; keep the original.
			out = append(out, file)
			continue
		}
		if adds == 0 && dels == 0 {
			continue // whitespace-only change: the file disappears
		}
		file["patch"] = wsPatch
		file["additions"] = adds
		file["deletions"] = dels
		file["changes"] = adds + dels
		out = append(out, file)
	}
	writeJSON(w, http.StatusOK, paginateAndLink(w, r, out))
}

// pullRequestDiffChanges re-derives the merge-base→head tree diff the REST
// files endpoint renders, keyed by the filename pullRequestChangedFiles
// reports for each change.
func pullRequestDiffChanges(st *store.Store, repo *store.Repo, pr *store.PullRequest) map[string]*object.Change {
	source, err := pullRequestDiffSource(st, repo, pr)
	if err != nil || source == nil {
		return nil
	}
	changes, err := object.DiffTree(source.baseTree, source.headTree)
	if err != nil {
		return nil
	}
	out := make(map[string]*object.Change, len(changes))
	for _, ch := range changes {
		name := ch.To.Name
		if name == "" {
			name = ch.From.Name
		}
		out[name] = ch
	}
	return out
}

// changeFileLines loads both sides of a change as line slices; ok=false for
// binary content or unreadable blobs.
func changeFileLines(ch *object.Change) (from, to []string, ok bool) {
	fromFile, toFile, err := ch.Files()
	if err != nil || fromFile == nil || toFile == nil {
		return nil, nil, false
	}
	if bin, err := fromFile.IsBinary(); err != nil || bin {
		return nil, nil, false
	}
	if bin, err := toFile.IsBinary(); err != nil || bin {
		return nil, nil, false
	}
	fromContent, err := fromFile.Contents()
	if err != nil {
		return nil, nil, false
	}
	toContent, err := toFile.Contents()
	if err != nil {
		return nil, nil, false
	}
	return splitContentLines(fromContent), splitContentLines(toContent), true
}

// splitContentLines splits file content on "\n", treating a trailing newline
// as a terminator, not an extra empty line.
func splitContentLines(content string) []string {
	if content == "" {
		return nil
	}
	lines := strings.Split(content, "\n")
	if lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	return lines
}

// wsStripLine removes all whitespace from a line, so two lines compare equal
// exactly when they differ only in whitespace (git diff -w).
func wsStripLine(line string) string {
	var b strings.Builder
	for _, r := range line {
		switch r {
		case ' ', '\t', '\r', '\v', '\f':
			continue
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// wsEdit is one step of a line-level edit script: '=' pairs a[ai] with b[bi],
// '-' deletes a[ai], '+' inserts b[bi].
type wsEdit struct {
	op     byte
	ai, bi int
}

// wsDiffCaps bound the Myers diff: beyond these the recompute gives up and the
// caller keeps the exact REST patch (correct, just not whitespace-filtered).
const (
	wsDiffMaxLines = 20000 // total lines across both sides
	wsDiffMaxD     = 2000  // maximum edit distance explored
)

// wsDiffEdits computes a shortest edit script between the key slices with the
// classic Myers O(ND) algorithm, returning ok=false when a cap is exceeded.
func wsDiffEdits(ka, kb []string) ([]wsEdit, bool) {
	n, m := len(ka), len(kb)
	if n+m > wsDiffMaxLines {
		return nil, false
	}
	maxD := n + m
	if maxD > wsDiffMaxD {
		maxD = wsDiffMaxD
	}
	off := n + m
	v := make([]int, 2*(n+m)+1)
	// trace[d] snapshots v for k ∈ [-(d-1), d-1] before iteration d runs — the
	// diagonals the backtrack step reads.
	trace := make([][]int, 0, maxD+1)
	dFound := -1
	for d := 0; d <= maxD && dFound < 0; d++ {
		width := 2*d - 1
		if width < 1 {
			width = 1
		}
		snapshot := make([]int, width)
		for i := range snapshot {
			k := i - (d - 1)
			if d == 0 {
				k = 0
			}
			snapshot[i] = v[off+k]
		}
		trace = append(trace, snapshot)
		for k := -d; k <= d; k += 2 {
			var x int
			if k == -d || (k != d && v[off+k-1] < v[off+k+1]) {
				x = v[off+k+1]
			} else {
				x = v[off+k-1] + 1
			}
			y := x - k
			for x < n && y < m && ka[x] == kb[y] {
				x++
				y++
			}
			v[off+k] = x
			if x >= n && y >= m {
				dFound = d
				break
			}
		}
	}
	if dFound < 0 {
		return nil, false
	}

	// snapAt reads trace[d]'s value for diagonal k (0 outside the window).
	snapAt := func(d, k int) int {
		snapshot := trace[d]
		base := d - 1
		if d == 0 {
			base = 0
		}
		i := k + base
		if i < 0 || i >= len(snapshot) {
			return 0
		}
		return snapshot[i]
	}

	edits := make([]wsEdit, 0, n+m)
	x, y := n, m
	for d := dFound; d > 0; d-- {
		k := x - y
		var prevK int
		if k == -d || (k != d && snapAt(d, k-1) < snapAt(d, k+1)) {
			prevK = k + 1
		} else {
			prevK = k - 1
		}
		prevX := snapAt(d, prevK)
		prevY := prevX - prevK
		for x > prevX && y > prevY {
			x--
			y--
			edits = append(edits, wsEdit{op: '=', ai: x, bi: y})
		}
		if x == prevX {
			y--
			edits = append(edits, wsEdit{op: '+', ai: -1, bi: y})
		} else {
			x--
			edits = append(edits, wsEdit{op: '-', ai: x, bi: -1})
		}
	}
	for x > 0 && y > 0 {
		x--
		y--
		edits = append(edits, wsEdit{op: '=', ai: x, bi: y})
	}
	for i, j := 0, len(edits)-1; i < j; i, j = i+1, j-1 {
		edits[i], edits[j] = edits[j], edits[i]
	}
	return edits, true
}

// wsInsensitiveUnifiedPatch diffs the line slices on whitespace-stripped
// lines and renders GitHub's `patch` form (hunks only, 3 context lines, no
// preamble). Context lines show the new (b) side text. ok=false means the
// bounded diff gave up.
func wsInsensitiveUnifiedPatch(a, b []string) (patch string, adds, dels int, ok bool) {
	ka := make([]string, len(a))
	for i, line := range a {
		ka[i] = wsStripLine(line)
	}
	kb := make([]string, len(b))
	for i, line := range b {
		kb[i] = wsStripLine(line)
	}
	edits, ok := wsDiffEdits(ka, kb)
	if !ok {
		return "", 0, 0, false
	}
	for _, e := range edits {
		switch e.op {
		case '+':
			adds++
		case '-':
			dels++
		}
	}
	if adds == 0 && dels == 0 {
		return "", 0, 0, true
	}

	// preA[i]/preB[i]: lines of a/b consumed before edit i, for hunk headers.
	preA := make([]int, len(edits)+1)
	preB := make([]int, len(edits)+1)
	for i, e := range edits {
		preA[i+1], preB[i+1] = preA[i], preB[i]
		if e.op == '=' || e.op == '-' {
			preA[i+1]++
		}
		if e.op == '=' || e.op == '+' {
			preB[i+1]++
		}
	}

	const ctx = 3
	var buf strings.Builder
	i := 0
	for i < len(edits) {
		if edits[i].op == '=' {
			i++
			continue
		}
		// Grow the hunk: changes separated by ≤ 2*ctx context lines merge in,
		// as git does.
		last := i
		j := i + 1
		gap := 0
		for j < len(edits) {
			if edits[j].op == '=' {
				gap++
				if gap > 2*ctx {
					break
				}
			} else {
				gap = 0
				last = j
			}
			j++
		}
		lo := i - ctx
		if lo < 0 {
			lo = 0
		}
		hi := last + ctx
		if hi > len(edits)-1 {
			hi = len(edits) - 1
		}

		aLen := preA[hi+1] - preA[lo]
		bLen := preB[hi+1] - preB[lo]
		aStart := preA[lo]
		if aLen > 0 {
			aStart++
		}
		bStart := preB[lo]
		if bLen > 0 {
			bStart++
		}
		buf.WriteString("@@ -")
		buf.WriteString(hunkRange(aStart, aLen))
		buf.WriteString(" +")
		buf.WriteString(hunkRange(bStart, bLen))
		buf.WriteString(" @@")
		for _, e := range edits[lo : hi+1] {
			buf.WriteByte('\n')
			switch e.op {
			case '=':
				buf.WriteByte(' ')
				buf.WriteString(b[e.bi])
			case '-':
				buf.WriteByte('-')
				buf.WriteString(a[e.ai])
			case '+':
				buf.WriteByte('+')
				buf.WriteString(b[e.bi])
			}
		}
		i = hi + 1
	}
	return buf.String(), adds, dels, true
}

// hunkRange renders one side of a @@ header, omitting ",1" as git does.
func hunkRange(start, length int) string {
	if length == 1 {
		return strconv.Itoa(start)
	}
	return strconv.Itoa(start) + "," + strconv.Itoa(length)
}

// apply suggestion

// firstSuggestionBlock extracts the body of the first ```suggestion fence;
// ok=false when there is no terminated fence.
func firstSuggestionBlock(body string) ([]string, bool) {
	lines := strings.Split(body, "\n")
	for i, line := range lines {
		t := strings.TrimSpace(line)
		if t != "```suggestion" && !strings.HasPrefix(t, "```suggestion ") {
			continue
		}
		for j := i + 1; j < len(lines); j++ {
			if strings.TrimSpace(lines[j]) == "```" {
				return lines[i+1 : j], true
			}
		}
		return nil, false
	}
	return nil, false
}

// gitFileContent reads path's blob content at the given commit; found=false
// when the commit or path does not resolve.
func gitFileContent(stor gitStorage.Storer, hash plumbing.Hash, path string) (content string, found bool) {
	commit, err := object.GetCommit(stor, hash)
	if err != nil {
		return "", false
	}
	tree, err := commit.Tree()
	if err != nil {
		return "", false
	}
	file, err := tree.File(path)
	if err != nil {
		return "", false
	}
	content, err = file.Contents()
	if err != nil {
		return "", false
	}
	return content, true
}

func (s *Server) handleUIApplySuggestion(w http.ResponseWriter, r *http.Request) {
	user := ghUserFromContext(r.Context())
	if user == nil {
		writeGHError(w, http.StatusUnauthorized, "Bad credentials")
		return
	}
	repo := s.lookupReadableRepoFromPath(w, r)
	if repo == nil {
		return
	}
	num, err := strconv.Atoi(r.PathValue("number"))
	if err != nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	pr := s.store.GetPullRequestByNumber(repo.ID, num)
	if pr == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	commentID, err := strconv.Atoi(r.PathValue("comment_id"))
	if err != nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	comment := s.store.PRReviewComments.Get(commentID)
	if comment == nil || comment.PullRequestID != pr.ID {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}

	// The commit lands on the PR head branch in the head repo (a fork for
	// cross-repo PRs), so push access is judged there.
	headRepo := store.PullRequestHeadRepo(s.store, pr)
	if headRepo == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	if !s.viewerCanPushRepo(r.Context(), headRepo) {
		writeGHError(w, http.StatusForbidden, "Must have push access to Repository.")
		return
	}
	if pr.State != "OPEN" {
		writeGHError(w, http.StatusUnprocessableEntity, "Suggestions can only be applied to open pull requests")
		return
	}
	if comment.Side == "LEFT" {
		writeGHError(w, http.StatusUnprocessableEntity, "Suggestions on the LEFT (deleted) side cannot be applied")
		return
	}
	if comment.Line == nil || *comment.Line <= 0 {
		writeGHError(w, http.StatusUnprocessableEntity, "Comment is not anchored to a line range")
		return
	}
	suggestion, ok := firstSuggestionBlock(comment.Body)
	if !ok {
		writeGHError(w, http.StatusUnprocessableEntity, "Comment contains no suggestion")
		return
	}

	stor, _ := store.PullRequestGitStorage(s.store, repo, pr)
	if stor == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	headHash, err := store.ResolveGitRef(stor, pr.HeadRefName)
	if err != nil {
		writeGHError(w, http.StatusUnprocessableEntity, "Head branch is gone")
		return
	}

	endLine := *comment.Line
	startLine := endLine
	if comment.StartLine != nil && *comment.StartLine > 0 {
		startLine = *comment.StartLine
	}

	content, found := gitFileContent(stor, headHash, comment.Path)
	if !found {
		writeGHError(w, http.StatusConflict, "The suggestion is outdated: the file no longer exists on the head branch")
		return
	}
	hadTrailingNewline := content == "" || strings.HasSuffix(content, "\n")
	curLines := splitContentLines(content)
	if startLine < 1 || endLine > len(curLines) {
		writeGHError(w, http.StatusConflict, "The suggestion is outdated: the commented lines no longer exist")
		return
	}

	// Outdated detection: when the head moved past the commented commit, the
	// target lines must still read as they did at comment time, else 409.
	if comment.CommitID != "" && comment.CommitID != headHash.String() {
		commented, foundAtComment := gitFileContent(stor, plumbing.NewHash(comment.CommitID), comment.Path)
		if !foundAtComment {
			writeGHError(w, http.StatusConflict, "The suggestion is outdated: its commit is no longer reachable")
			return
		}
		commentedLines := splitContentLines(commented)
		if endLine > len(commentedLines) {
			writeGHError(w, http.StatusConflict, "The suggestion is outdated: the commented lines no longer exist")
			return
		}
		for i := startLine - 1; i < endLine; i++ {
			if curLines[i] != commentedLines[i] {
				writeGHError(w, http.StatusConflict, "The suggestion is outdated: the file changed since the comment was made")
				return
			}
		}
	}

	newLines := make([]string, 0, len(curLines)-(endLine-startLine+1)+len(suggestion))
	newLines = append(newLines, curLines[:startLine-1]...)
	newLines = append(newLines, suggestion...)
	newLines = append(newLines, curLines[endLine:]...)
	newContent := strings.Join(newLines, "\n")
	if hadTrailingNewline && newContent != "" {
		newContent += "\n"
	}
	if newContent == content {
		writeGHError(w, http.StatusUnprocessableEntity, "The suggestion matches the current content")
		return
	}

	if ph := s.createSecretScanningPushProtectionPlaceholder(headRepo, secretScanningContentMatches(newContent)); ph != nil {
		writeSecretScanningPushProtectionBlocked(w, ph)
		return
	}

	author := s.store.GetUserByID(comment.AuthorID)
	coAuthor := "unknown"
	if author != nil {
		email := author.Email
		if email == "" {
			email = author.Login + "@users.noreply.bleephub.local"
		}
		coAuthor = author.Login + " <" + email + ">"
	}
	message := "Apply suggestions from code review\n\nCo-authored-by: " + coAuthor

	branchRef := plumbing.NewBranchReferenceName(pr.HeadRefName)
	sig := repoSignature(user.Login, "bleephub@local")
	commitHash, err := createFileCommitExpectedGuarded(
		stor, pr.HeadRefName, comment.Path, newContent, message, sig, headHash,
		s.contentRefWriteGuard(r, headRepo, stor, branchRef, refFastForward),
	)
	if err != nil {
		writeContentCommitError(w, err)
		return
	}

	base := s.baseURL(r)
	if err := s.scanCommitForSecretScanning(headRepo, stor, commitHash, base); err != nil {
		writeGHError(w, http.StatusInternalServerError, "secret scanning failed")
		return
	}
	s.afterCommittedRefUpdate(headRepo, user, branchRef.String(), headHash.String(), commitHash.String(), base)

	writeJSON(w, http.StatusCreated, map[string]interface{}{
		"sha": commitHash.String(),
		"message": fmt.Sprintf("Apply suggestions from code review (co-authored by %s)",
			strings.SplitN(coAuthor, " <", 2)[0]),
	})
}
