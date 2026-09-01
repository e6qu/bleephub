package bleephub

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	pathpkg "path"
	"strings"
	"time"

	"github.com/e6qu/bleephub/internal/store"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/plumbing/object"
	gitStorage "github.com/go-git/go-git/v5/storage"
)

var (
	errRepoGitStorageUnavailable = errors.New("repository git storage unavailable")
	errRepoGitRepositoryEmpty    = errors.New("repository git repository empty")
	errRepoGitRefUnavailable     = errors.New("repository git ref unavailable")
	errRepoGitObjectUnavailable  = errors.New("repository git object unavailable")
)

func (s *Server) registerGHRepoObjectRoutes() {
	s.route("GET /api/v3/repos/{owner}/{repo}/commits", s.handleListCommits)
	s.route("GET /api/v3/repos/{owner}/{repo}/readme", s.handleGetReadme)
	s.route("GET /api/v3/repos/{owner}/{repo}/contents/{path...}", s.handleGetContents)
	s.route("PUT /api/v3/repos/{owner}/{repo}/contents/{path...}", s.requirePerm(store.ScopeContents, store.PermWrite, s.handlePutContents))
	s.route("DELETE /api/v3/repos/{owner}/{repo}/contents/{path...}", s.requirePerm(store.ScopeContents, store.PermWrite, s.handleDeleteContents))
}

func (s *Server) handleListCommits(w http.ResponseWriter, r *http.Request) {
	owner := r.PathValue("owner")
	repoName := r.PathValue("repo")

	repo := s.lookupReadableRepoFromPath(w, r)
	if repo == nil {
		return
	}

	options, ok := parseListCommitsOptions(w, r)
	if !ok {
		return
	}
	commits, err := s.listRepoCommits(repo, owner, repoName, options, s.baseURL(r))
	if err != nil {
		switch {
		case errors.Is(err, errRepoGitRepositoryEmpty):
			writeGHError(w, http.StatusConflict, "Git Repository is empty.")
		case errors.Is(err, errRepoGitRefUnavailable):
			writeGHError(w, http.StatusNotFound, "No commit found for SHA: "+r.URL.Query().Get("sha"))
		case errors.Is(err, errRepoGitObjectUnavailable):
			writeGHError(w, http.StatusInternalServerError, "Git object unavailable")
		default:
			writeGHError(w, http.StatusInternalServerError, "Git storage unavailable")
		}
		return
	}
	writeJSON(w, http.StatusOK, paginateAndLink(w, r, commits))
}

type listCommitsOptions struct {
	Ref    string
	Path   string
	Author string
	Since  *time.Time
	Until  *time.Time
}

func parseListCommitsOptions(w http.ResponseWriter, r *http.Request) (listCommitsOptions, bool) {
	query := r.URL.Query()
	options := listCommitsOptions{
		Ref:    query.Get("sha"),
		Path:   strings.Trim(query.Get("path"), "/"),
		Author: strings.TrimSpace(query.Get("author")),
	}
	for name, target := range map[string]**time.Time{
		"since": &options.Since,
		"until": &options.Until,
	} {
		raw := query.Get(name)
		if raw == "" {
			continue
		}
		parsed, err := time.Parse(time.RFC3339, raw)
		if err != nil {
			store.WriteGHValidationError(w, "Commit", name, "invalid")
			return listCommitsOptions{}, false
		}
		parsed = parsed.UTC()
		*target = &parsed
	}
	return options, true
}

func (s *Server) listRepoCommits(repo *store.Repo, owner, repoName string, options listCommitsOptions, baseURL string) ([]map[string]interface{}, error) {
	stor := s.store.GetGitStorage(owner, repoName)
	if stor == nil {
		return nil, errRepoGitStorageUnavailable
	}

	refName := options.Ref
	usingDefault := refName == ""
	if usingDefault {
		refName = repo.DefaultBranch
	}
	hash, err := store.ResolveGitRef(stor, refName)
	if err != nil {
		if usingDefault {
			return nil, errRepoGitRepositoryEmpty
		}
		return nil, fmt.Errorf("%w: %v", errRepoGitRefUnavailable, err)
	}

	head, err := object.GetCommit(stor, hash)
	if err != nil {
		return nil, errRepoGitObjectUnavailable
	}
	iter := object.NewCommitPreorderIter(head, nil, nil)
	defer iter.Close()

	commits := make([]map[string]interface{}, 0)
	err = iter.ForEach(func(commit *object.Commit) error {
		when := commit.Committer.When.UTC()
		if options.Since != nil && when.Before(*options.Since) {
			return nil
		}
		if options.Until != nil && when.After(*options.Until) {
			return nil
		}
		if options.Author != "" && !s.commitMatchesAuthor(commit, options.Author) {
			return nil
		}
		if options.Path != "" {
			touches, err := store.CommitTouchesPath(commit, options.Path)
			if err != nil {
				return err
			}
			if !touches {
				return nil
			}
		}
		commits = append(commits, commitToJSON(commit, repo, s.store, baseURL))
		return nil
	})
	if err != nil {
		return nil, errRepoGitObjectUnavailable
	}
	return commits, nil
}

func (s *Server) commitMatchesAuthor(commit *object.Commit, author string) bool {
	if strings.EqualFold(commit.Author.Name, author) || strings.EqualFold(commit.Author.Email, author) {
		return true
	}
	user := s.store.ResolveUserBySignature(commit.Author.Name, commit.Author.Email)
	return user != nil && strings.EqualFold(user.Login, author)
}

func (s *Server) handleGetTree(w http.ResponseWriter, r *http.Request) {
	owner := r.PathValue("owner")
	repoName := r.PathValue("repo")
	sha := r.PathValue("sha")

	repo := s.lookupReadableRepoFromPath(w, r)
	if repo == nil {
		return
	}

	stor := s.store.GetGitStorage(owner, repoName)
	if stor == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}

	resolvedSHA, tree, err := store.ResolveGitTreeish(stor, sha)
	if errors.Is(err, store.ErrGitTreeishInvalidObject) {
		writeGHError(w, http.StatusUnprocessableEntity, "Invalid object requested. SHA must identify a commit or a tree.")
		return
	}
	if err != nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}

	entries := make([]map[string]interface{}, 0, len(tree.Entries))
	truncated := false
	appendEntry := func(name string, e object.TreeEntry) {
		// GitHub caps the tree response at 100,000 entries and sets truncated:true.
		if len(entries) >= gitTreeEntryLimit {
			truncated = true
			return
		}
		entries = append(entries, gitTreeEntryJSON(stor, s.baseURL(r), repo.FullName, name, e))
	}
	if _, recursive := r.URL.Query()["recursive"]; recursive {
		walker := object.NewTreeWalker(tree, true, nil)
		defer walker.Close()
		for {
			name, entry, err := walker.Next()
			if err == io.EOF {
				break
			}
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			appendEntry(name, entry)
			if truncated {
				break
			}
		}
	} else {
		for _, entry := range tree.Entries {
			appendEntry(entry.Name, entry)
			if truncated {
				break
			}
		}
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"sha":       resolvedSHA.String(),
		"url":       s.baseURL(r) + "/api/v3/repos/" + repo.FullName + "/git/trees/" + resolvedSHA.String(),
		"tree":      entries,
		"truncated": truncated,
	})
}

// gitTreeEntryLimit is GitHub's 100,000-entry ceiling on a tree response.
const gitTreeEntryLimit = 100000

const (
	// contentsAPIInlineFileBytes: above 1 MB the contents API stops inlining
	// (content:"", encoding:"none").
	contentsAPIInlineFileBytes = 1 << 20
	// contentsAPIMaxFileBytes: above 100 MB the contents API is unsupported.
	contentsAPIMaxFileBytes = 100 << 20
	// gitBlobMaxFileBytes is the Git Data blob endpoint's 100 MB ceiling.
	gitBlobMaxFileBytes = 100 << 20
	// contentsAPIDirectoryEntryLimit: GitHub truncates a directory listing at
	// 1,000 entries and still answers 200.
	contentsAPIDirectoryEntryLimit = 1000
)

// writeGHBlobTooLargeError writes GitHub's 403 Blob/data/too_large refusal.
func writeGHBlobTooLargeError(w http.ResponseWriter, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusForbidden)
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"message":           message,
		"documentation_url": "https://docs.github.com/rest",
		"errors": []map[string]string{
			{"resource": "Blob", "field": "data", "code": "too_large"},
		},
	})
}

func gitTreeEntryJSON(stor gitStorage.Storer, baseURL, fullName, name string, entry object.TreeEntry) map[string]interface{} {
	entryType := "tree"
	switch {
	case entry.Mode.IsFile():
		entryType = "blob"
	case entry.Mode == filemode.Submodule:
		entryType = "commit"
	}
	out := map[string]interface{}{
		"path": name,
		// GitHub emits the 6-char octal mode (100644); go-git's Mode.String() is
		// 7-char (0100644), breaking round-tripping and comparisons.
		"mode": fmt.Sprintf("%06o", uint32(entry.Mode)),
		"type": entryType,
		"sha":  entry.Hash.String(),
	}
	if entryType == "commit" {
		// A gitlink names a commit in the submodule's repo, absent here, so there
		// is no object URL. github.com omits url and size for commit entries.
		return out
	}
	out["url"] = baseURL + "/api/v3/repos/" + fullName + "/git/" + entryType + "s/" + entry.Hash.String()
	if entryType == "blob" {
		if blob, err := object.GetBlob(stor, entry.Hash); err == nil {
			out["size"] = blob.Size
		}
	}
	return out
}

func (s *Server) handleGetBlob(w http.ResponseWriter, r *http.Request) {
	owner := r.PathValue("owner")
	repoName := r.PathValue("repo")
	sha := r.PathValue("sha")

	repo := s.lookupReadableRepoFromPath(w, r)
	if repo == nil {
		return
	}

	stor := s.store.GetGitStorage(owner, repoName)
	if stor == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}

	blob, err := object.GetBlob(stor, plumbing.NewHash(sha))
	if err != nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}

	// Refuse past 100 MB, decided from blob.Size so an oversized blob is never
	// read into memory.
	if blob.Size > gitBlobMaxFileBytes {
		writeGHBlobTooLargeError(w, "This API returns blobs up to 100 MB in size. The requested blob is too large to fetch via the API.")
		return
	}

	reader, err := blob.Reader()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer reader.Close()

	content, err := io.ReadAll(reader)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	// The blob endpoint addresses an object by hash, with no filename to type it,
	// so the raw media type answers octet-stream.
	if acceptsGitHubMediaType(r.Header.Get("Accept"), "raw") {
		setGitHubMediaType(w, r, "raw")
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(content)
		return
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"sha":      sha,
		"node_id":  encodeNodeID("Blob", 0, sha),
		"url":      s.baseURL(r) + "/api/v3/repos/" + repo.FullName + "/git/blobs/" + sha,
		"size":     blob.Size,
		"encoding": "base64",
		"content":  base64.StdEncoding.EncodeToString(content),
	})
}

func (s *Server) handleGetReadme(w http.ResponseWriter, r *http.Request) {
	repo := s.lookupReadableRepoFromPath(w, r)
	if repo == nil {
		return
	}

	refName := r.URL.Query().Get("ref")
	tree, _, err := s.repoTreeAtRef(repo, refName)
	if err != nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	if refName == "" {
		refName = repo.DefaultBranch
	}
	stor := s.gitStorageForRepo(repo)

	for _, name := range []string{"README.md", "README", "README.txt", "readme.md"} {
		entry, err := tree.FindEntry(name)
		if err != nil {
			continue
		}

		blob, err := object.GetBlob(stor, entry.Hash)
		if err != nil {
			continue
		}

		reader, err := blob.Reader()
		if err != nil {
			continue
		}
		content, err := io.ReadAll(reader)
		_ = reader.Close()
		if err != nil {
			continue
		}

		accept := r.Header.Get("Accept")
		if acceptsGitHubMediaType(accept, "raw") {
			writeContentsRaw(w, r, content)
			return
		}
		if acceptsGitHubMediaType(accept, "html") {
			rendered, err := s.renderMarkdown(string(content), "gfm", repo.FullName, s.baseURL(r))
			if err != nil {
				writeGHError(w, http.StatusInternalServerError, "Markdown rendering failed")
				return
			}
			setGitHubMediaType(w, r, "html")
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, rendered)
			return
		}

		out := contentFileJSON(s.baseURL(r), repo, refName, name, entry.Hash.String(), blob.Size)
		out["encoding"] = "base64"
		out["content"] = base64.StdEncoding.EncodeToString(content)
		writeJSON(w, http.StatusOK, out)
		return
	}

	writeGHError(w, http.StatusNotFound, "Not Found")
}

// contentBlobSHA reports the blob sha at path on branch, and whether a file exists there.
func contentBlobSHA(stor gitStorage.Storer, branch, path string) (string, bool) {
	ref, err := stor.Reference(plumbing.NewBranchReferenceName(branch))
	if err != nil || ref == nil {
		return "", false
	}
	commit, err := object.GetCommit(stor, ref.Hash())
	if err != nil {
		return "", false
	}
	tree, err := commit.Tree()
	if err != nil {
		return "", false
	}
	entry, err := tree.FindEntry(path)
	if err != nil {
		return "", false
	}
	return entry.Hash.String(), true
}

// contentSHAPreconditionMet enforces the contents API's optimistic-concurrency
// check, writing the refusal itself. `sha` names the blob a write replaces;
// without it concurrent editors silently lose an edit. Creation carries none.
func contentSHAPreconditionMet(w http.ResponseWriter, stor gitStorage.Storer, branch, path, given string) bool {
	current, exists := contentBlobSHA(stor, branch, path)
	switch {
	case exists && given == "":
		store.WriteGHValidationError(w, "Commit", "sha", "missing_field")
	case exists && given != current:
		writeGHError(w, http.StatusConflict, path+" does not match "+given)
	case !exists && given != "":
		store.WriteGHValidationError(w, "Commit", "sha", "invalid")
	default:
		return true
	}
	return false
}

type contentRefWriteRefusal struct {
	message string
}

func (e *contentRefWriteRefusal) Error() string {
	return e.message
}

func (s *Server) contentRefWriteGuard(r *http.Request, repo *store.Repo, stor gitStorage.Storer, ref plumbing.ReferenceName, kind refWriteKind) func(plumbing.Hash) error {
	return func(target plumbing.Hash) error {
		if refusal := s.protectedRefWriteRefusal(r.Context(), repo, stor, ref, kind, target); refusal != "" {
			return &contentRefWriteRefusal{message: refusal}
		}
		return nil
	}
}

func writeContentCommitError(w http.ResponseWriter, err error) {
	var refusal *contentRefWriteRefusal
	switch {
	case errors.As(err, &refusal):
		writeGHError(w, http.StatusForbidden, refusal.message)
	case errors.Is(err, gitStorage.ErrReferenceHasChanged):
		writeGHError(w, http.StatusConflict, "The branch changed while the file was being updated")
	default:
		writeGHError(w, http.StatusUnprocessableEntity, err.Error())
	}
}

func (s *Server) handlePutContents(w http.ResponseWriter, r *http.Request) {
	owner := r.PathValue("owner")
	repoName := r.PathValue("repo")
	path := r.PathValue("path")

	repo := s.lookupReadableRepoFromPath(w, r)
	if repo == nil {
		return
	}

	user := ghUserFromContext(r.Context())
	if repo.Private && !s.viewerCanReadRepo(r.Context(), repo) {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	if !s.viewerCanPushRepo(r.Context(), repo) {
		writeGHError(w, http.StatusForbidden, "Must have push access to Repository.")
		return
	}
	if s.rejectIfArchived(w, repo) {
		return
	}

	var req struct {
		Message string `json:"message"`
		Content string `json:"content"`
		Branch  string `json:"branch"`
		SHA     string `json:"sha"`
		Author  *struct {
			Name  string `json:"name"`
			Email string `json:"email"`
		} `json:"author"`
		Committer *struct {
			Name  string `json:"name"`
			Email string `json:"email"`
		} `json:"committer"`
	}
	if !decodeJSONBody(w, r, &req) {
		return
	}
	if req.Message == "" {
		store.WriteGHValidationError(w, "Commit", "message", "missing_field")
		return
	}
	if req.Content == "" {
		store.WriteGHValidationError(w, "Commit", "content", "missing_field")
		return
	}

	decoded, err := base64.StdEncoding.DecodeString(req.Content)
	if err != nil {
		store.WriteGHValidationError(w, "Commit", "content", "invalid")
		return
	}

	branch := req.Branch
	if branch == "" {
		branch = repo.DefaultBranch
	}

	stor := s.store.GetGitStorage(owner, repoName)
	if stor == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}

	sig := repoSignature(user.Login, "bleephub@local")
	if req.Committer != nil {
		sig = repoSignature(req.Committer.Name, req.Committer.Email)
	} else if req.Author != nil {
		sig = repoSignature(req.Author.Name, req.Author.Email)
	}

	// PUT contents never creates branches: a missing branch on a repo that
	// already has branches is 404; only an empty repo initializes here.
	branchRef := plumbing.NewBranchReferenceName(branch)
	ref, refErr := stor.Reference(branchRef)
	var commitHash plumbing.Hash
	var isInitial bool
	beforeHash := plumbing.ZeroHash.String()
	if refErr != nil || ref == nil {
		if repoHasAnyBranch(stor) {
			writeGHError(w, http.StatusNotFound, "Branch not found")
			return
		}
		isInitial = true
	} else {
		beforeHash = ref.Hash().String()
	}

	_, replacing := contentBlobSHA(stor, branch, path)
	if !contentSHAPreconditionMet(w, stor, branch, path, req.SHA) {
		return
	}

	if isInitial {
		files := map[string]string{path: string(decoded)}
		if ph := s.createSecretScanningPushProtectionPlaceholder(repo, secretScanningContentMatches(string(decoded))); ph != nil {
			writeSecretScanningRuleViolation(w, http.StatusConflict, ph)
			return
		}
		commitHash, err = commitRootBranchWithFiles(
			stor, branch, req.Message, files, sig, true,
			s.contentRefWriteGuard(r, repo, stor, branchRef, refCreation),
		)
	} else {
		if ph := s.createSecretScanningPushProtectionPlaceholder(repo, secretScanningContentMatches(string(decoded))); ph != nil {
			writeSecretScanningRuleViolation(w, http.StatusConflict, ph)
			return
		}
		commitHash, err = createFileCommitExpectedGuarded(
			stor, branch, path, string(decoded), req.Message, sig, plumbing.NewHash(beforeHash),
			s.contentRefWriteGuard(r, repo, stor, branchRef, refFastForward),
		)
	}
	if err != nil {
		writeContentCommitError(w, err)
		return
	}

	commit, err := object.GetCommit(stor, commitHash)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	tree, err := commit.Tree()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	entry, err := tree.FindEntry(path)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	blob, err := object.GetBlob(stor, entry.Hash)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	s.store.UpdateRepo(owner, repoName, func(r *store.Repo) {
		r.PushedAt = time.Now().UTC()
	})

	base := s.baseURL(r)
	if err := s.scanCommitForSecretScanning(repo, stor, commitHash, base); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.afterCommittedRefUpdate(repo, user, branchRef.String(), beforeHash, commitHash.String(), base)
	contentOut := contentFileJSON(base, repo, branch, path, entry.Hash.String(), blob.Size)

	// 201 created, 200 updated.
	status := http.StatusCreated
	if replacing {
		status = http.StatusOK
	}
	writeJSON(w, status, map[string]interface{}{
		"content": contentOut,
		"commit": map[string]interface{}{
			"sha":     commitHash.String(),
			"message": strings.TrimSpace(commit.Message),
			"author": map[string]interface{}{
				"name":  commit.Author.Name,
				"email": commit.Author.Email,
				"date":  commit.Author.When.Format(time.RFC3339),
			},
			"committer": map[string]interface{}{
				"name":  commit.Committer.Name,
				"email": commit.Committer.Email,
				"date":  commit.Committer.When.Format(time.RFC3339),
			},
			"tree": map[string]interface{}{
				"sha": commit.TreeHash.String(),
			},
		},
	})
}

func (s *Server) handleDeleteContents(w http.ResponseWriter, r *http.Request) {
	owner := r.PathValue("owner")
	repoName := r.PathValue("repo")
	path := r.PathValue("path")

	repo := s.lookupReadableRepoFromPath(w, r)
	if repo == nil {
		return
	}

	user := ghUserFromContext(r.Context())
	if repo.Private && !s.viewerCanReadRepo(r.Context(), repo) {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	if !s.viewerCanPushRepo(r.Context(), repo) {
		writeGHError(w, http.StatusForbidden, "Must have push access to Repository.")
		return
	}
	if s.rejectIfArchived(w, repo) {
		return
	}

	var req struct {
		Message string `json:"message"`
		SHA     string `json:"sha"`
		Branch  string `json:"branch"`
		Author  *struct {
			Name  string `json:"name"`
			Email string `json:"email"`
		} `json:"author"`
		Committer *struct {
			Name  string `json:"name"`
			Email string `json:"email"`
		} `json:"committer"`
	}
	if !decodeJSONBody(w, r, &req) {
		return
	}
	if req.Message == "" {
		store.WriteGHValidationError(w, "Commit", "message", "missing_field")
		return
	}
	if req.SHA == "" {
		store.WriteGHValidationError(w, "Commit", "sha", "missing_field")
		return
	}

	branch := req.Branch
	if branch == "" {
		branch = repo.DefaultBranch
	}

	stor := s.store.GetGitStorage(owner, repoName)
	if stor == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}

	branchRef := plumbing.NewBranchReferenceName(branch)
	ref, err := stor.Reference(branchRef)
	if err != nil {
		writeGHError(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	if _, exists := contentBlobSHA(stor, branch, path); !exists {
		writeGHError(w, http.StatusUnprocessableEntity, fmt.Sprintf("path %s does not exist", path))
		return
	}
	// Same precondition as a write: a delete naming the wrong blob deletes
	// something the caller has not seen.
	if !contentSHAPreconditionMet(w, stor, branch, path, req.SHA) {
		return
	}

	var commit *object.Commit

	sig := repoSignature(user.Login, "bleephub@local")
	if req.Committer != nil {
		sig = repoSignature(req.Committer.Name, req.Committer.Email)
	} else if req.Author != nil {
		sig = repoSignature(req.Author.Name, req.Author.Email)
	}

	commitHash, err := deleteFileCommit(
		stor, branch, path, req.Message, sig, ref.Hash(),
		s.contentRefWriteGuard(r, repo, stor, branchRef, refFastForward),
	)
	if err != nil {
		writeContentCommitError(w, err)
		return
	}

	commit, err = object.GetCommit(stor, commitHash)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	s.store.UpdateRepo(owner, repoName, func(r *store.Repo) {
		r.PushedAt = time.Now().UTC()
	})
	s.afterCommittedRefUpdate(repo, user, branchRef.String(), ref.Hash().String(), commitHash.String(), s.baseURL(r))

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"content": nil,
		"commit": map[string]interface{}{
			"sha":     commitHash.String(),
			"message": strings.TrimSpace(commit.Message),
			"author": map[string]interface{}{
				"name":  commit.Author.Name,
				"email": commit.Author.Email,
				"date":  commit.Author.When.Format(time.RFC3339),
			},
			"committer": map[string]interface{}{
				"name":  commit.Committer.Name,
				"email": commit.Committer.Email,
				"date":  commit.Committer.When.Format(time.RFC3339),
			},
			"tree": map[string]interface{}{
				"sha": commit.TreeHash.String(),
			},
		},
	})
}

// contentFileJSON builds the content-file shape for a blob at path on ref.
func contentFileJSON(baseURL string, repo *store.Repo, ref, path, sha string, size int64) map[string]interface{} {
	selfURL := baseURL + "/api/v3/repos/" + repo.FullName + "/contents/" + path + "?ref=" + ref
	gitURL := baseURL + "/api/v3/repos/" + repo.FullName + "/git/blobs/" + sha
	htmlURL := baseURL + "/" + repo.FullName + "/blob/" + ref + "/" + path
	downloadURL := baseURL + "/" + repo.FullName + "/raw/" + ref + "/" + path
	return map[string]interface{}{
		"name":         path[strings.LastIndex(path, "/")+1:],
		"path":         path,
		"sha":          sha,
		"size":         size,
		"type":         "file",
		"url":          selfURL,
		"git_url":      gitURL,
		"html_url":     htmlURL,
		"download_url": downloadURL,
		"_links": map[string]interface{}{
			"self": selfURL,
			"git":  gitURL,
			"html": htmlURL,
		},
	}
}

func contentDirectoryEntryJSON(baseURL string, repo *store.Repo, ref, path string, entry object.TreeEntry, size int64) map[string]interface{} {
	selfURL := baseURL + "/api/v3/repos/" + repo.FullName + "/contents/" + path + "?ref=" + ref
	htmlKind := "blob"
	gitKind := "blobs"
	entryType := "file"
	var downloadURL interface{} = baseURL + "/" + repo.FullName + "/raw/" + ref + "/" + path
	var gitURL interface{} = baseURL + "/api/v3/repos/" + repo.FullName + "/git/" + gitKind + "/" + entry.Hash.String()
	switch entry.Mode {
	case filemode.Dir:
		entryType = "dir"
		htmlKind = "tree"
		gitKind = "trees"
		gitURL = baseURL + "/api/v3/repos/" + repo.FullName + "/git/" + gitKind + "/" + entry.Hash.String()
		downloadURL = nil
	case filemode.Symlink:
		entryType = "symlink"
	case filemode.Submodule:
		entryType = "submodule"
		gitURL = nil
		downloadURL = nil
	}
	htmlURL := baseURL + "/" + repo.FullName + "/" + htmlKind + "/" + ref + "/" + path
	return map[string]interface{}{
		"name":         path[strings.LastIndex(path, "/")+1:],
		"path":         path,
		"sha":          entry.Hash.String(),
		"size":         size,
		"type":         entryType,
		"url":          selfURL,
		"git_url":      gitURL,
		"html_url":     htmlURL,
		"download_url": downloadURL,
		"_links": map[string]interface{}{
			"self": selfURL,
			"git":  gitURL,
			"html": htmlURL,
		},
	}
}

func (s *Server) handleGetContents(w http.ResponseWriter, r *http.Request) {
	path := r.PathValue("path")

	repo := s.lookupReadableRepoFromPath(w, r)
	if repo == nil {
		return
	}

	// ref accepts a branch, tag, full ref, or commit SHA; keep the original text
	// in hypermedia URLs while resolving it through the treeish helper.
	refName := r.URL.Query().Get("ref")
	tree, _, err := s.repoTreeAtRef(repo, refName)
	if err != nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	if refName == "" {
		refName = repo.DefaultBranch
	}
	stor := s.gitStorageForRepo(repo)

	if path == "" {
		s.writeTreeListing(w, r, stor, repo, refName, tree, tree, "")
		return
	}

	entry, err := tree.FindEntry(path)
	if err != nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}

	if entry.Mode == filemode.Symlink {
		target, err := store.ReadGitBlob(stor, entry.Hash)
		if err != nil {
			writeGHError(w, http.StatusInternalServerError, "Git object unavailable")
			return
		}
		targetPath := pathpkg.Clean(pathpkg.Join(pathpkg.Dir(path), string(target)))
		if targetPath != "." && targetPath != ".." && !strings.HasPrefix(targetPath, "../") {
			if targetEntry, targetErr := tree.FindEntry(targetPath); targetErr == nil && targetEntry.Mode.IsFile() {
				s.writeContentsFile(w, r, stor, repo, refName, path, targetEntry.Hash)
				return
			}
		}
		out := contentDirectoryEntryJSON(s.baseURL(r), repo, refName, path, *entry, int64(len(target)))
		out["target"] = string(target)
		writeJSON(w, http.StatusOK, out)
		return
	}
	if entry.Mode == filemode.Submodule {
		out := contentDirectoryEntryJSON(s.baseURL(r), repo, refName, path, *entry, 0)
		out["submodule_git_url"] = submoduleGitURL(stor, tree, path)
		writeJSON(w, http.StatusOK, out)
		return
	}
	if entry.Mode.IsFile() {
		s.writeContentsFile(w, r, stor, repo, refName, path, entry.Hash)
		return
	}

	subTree, err := object.GetTree(stor, entry.Hash)
	if err != nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}

	s.writeTreeListing(w, r, stor, repo, refName, tree, subTree, path)
}

func (s *Server) writeContentsFile(
	w http.ResponseWriter,
	r *http.Request,
	stor gitStorage.Storer,
	repo *store.Repo,
	refName string,
	requestedPath string,
	hash plumbing.Hash,
) {
	blob, err := object.GetBlob(stor, hash)
	if err != nil {
		writeGHError(w, http.StatusInternalServerError, "Git object unavailable")
		return
	}
	accept := r.Header.Get("Accept")
	// Blob-size contract, decided from blob.Size so an oversized file is never
	// read into memory: <=1 MB base64-inlined; 1-100 MB JSON carries content:""
	// encoding:"none" (only raw/object media types work); >100 MB unsupported.
	if blob.Size > contentsAPIMaxFileBytes {
		writeGHBlobTooLargeError(w, "This API does not support blobs larger than 100 MB in size.")
		return
	}
	if blob.Size > contentsAPIInlineFileBytes && !acceptsGitHubMediaType(accept, "raw") {
		out := contentFileJSON(s.baseURL(r), repo, refName, requestedPath, hash.String(), blob.Size)
		out["encoding"] = "none"
		out["content"] = ""
		if acceptsGitHubMediaType(accept, "object") {
			setGitHubMediaType(w, r, "object")
		}
		writeJSON(w, http.StatusOK, out)
		return
	}

	reader, err := blob.Reader()
	if err != nil {
		writeGHError(w, http.StatusInternalServerError, "Git object unavailable")
		return
	}
	defer reader.Close()
	content, err := io.ReadAll(reader)
	if err != nil {
		writeGHError(w, http.StatusInternalServerError, "Git object unavailable")
		return
	}

	if acceptsGitHubMediaType(accept, "raw") {
		writeContentsRaw(w, r, content)
		return
	}
	if acceptsGitHubMediaType(accept, "html") {
		rendered, err := s.renderMarkdown(string(content), "gfm", repo.FullName, s.baseURL(r))
		if err != nil {
			writeGHError(w, http.StatusInternalServerError, "Markdown rendering failed")
			return
		}
		setGitHubMediaType(w, r, "html")
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, rendered)
		return
	}
	out := contentFileJSON(s.baseURL(r), repo, refName, requestedPath, hash.String(), blob.Size)
	out["encoding"] = "base64"
	out["content"] = base64.StdEncoding.EncodeToString(content)
	if acceptsGitHubMediaType(accept, "object") {
		setGitHubMediaType(w, r, "object")
	}
	writeJSON(w, http.StatusOK, out)
}

// writeContentsRaw serves a file's bytes for the raw media type as text/plain;
// charset=utf-8 — octokit returns text/* as a string and anything else as a
// Buffer. nosniff stops a raw HTML file from rendering in a browser.
func writeContentsRaw(w http.ResponseWriter, r *http.Request, content []byte) {
	setGitHubMediaType(w, r, "raw")
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(content)
}

func (s *Server) writeTreeListing(
	w http.ResponseWriter,
	r *http.Request,
	stor gitStorage.Storer,
	repo *store.Repo,
	ref string,
	root *object.Tree,
	tree *object.Tree,
	prefix string,
) {
	items := make([]map[string]interface{}, 0, len(tree.Entries))
	for _, e := range tree.Entries {
		// Cap the listing at 1,000 entries (200 with a truncated list, as GitHub
		// does). Entries keep git's own tree order, not a plain name sort — a
		// directory sorts as "name/" with the trailing slash git stores.
		if len(items) >= contentsAPIDirectoryEntryLimit {
			break
		}
		itemPath := e.Name
		if prefix != "" {
			itemPath = prefix + "/" + e.Name
		}
		var size int64
		if e.Mode.IsFile() || e.Mode == filemode.Symlink {
			blob, err := object.GetBlob(stor, e.Hash)
			if err != nil {
				writeGHError(w, http.StatusInternalServerError, "Git object unavailable")
				return
			}
			size = blob.Size
		}
		item := contentDirectoryEntryJSON(s.baseURL(r), repo, ref, itemPath, e, size)
		switch e.Mode {
		case filemode.Symlink:
			target, err := store.ReadGitBlob(stor, e.Hash)
			if err != nil {
				writeGHError(w, http.StatusInternalServerError, "Git object unavailable")
				return
			}
			item["target"] = string(target)
		case filemode.Submodule:
			item["submodule_git_url"] = submoduleGitURL(stor, root, itemPath)
		}
		items = append(items, item)
	}
	if acceptsGitHubMediaType(r.Header.Get("Accept"), "object") {
		setGitHubMediaType(w, r, "object")
		writeJSON(w, http.StatusOK, map[string]interface{}{"type": "dir", "entries": items})
		return
	}
	writeJSON(w, http.StatusOK, items)
}

func submoduleGitURL(stor gitStorage.Storer, root *object.Tree, requestedPath string) string {
	entry, err := root.FindEntry(".gitmodules")
	if err != nil || !entry.Mode.IsFile() {
		return ""
	}
	raw, err := store.ReadGitBlob(stor, entry.Hash)
	if err != nil {
		return ""
	}
	var currentPath string
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "[submodule ") {
			currentPath = ""
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		switch strings.TrimSpace(key) {
		case "path":
			currentPath = strings.TrimSpace(value)
		case "url":
			if currentPath == requestedPath {
				return strings.TrimSpace(value)
			}
		}
	}
	return ""
}

func repoHasAnyBranch(stor gitStorage.Storer) bool {
	refs, err := stor.IterReferences()
	if err != nil {
		return false
	}
	defer refs.Close()
	found := false
	_ = refs.ForEach(func(ref *plumbing.Reference) error {
		if ref.Name().IsBranch() && !ref.Hash().IsZero() {
			found = true
		}
		return nil
	})
	return found
}
