package bleephub

import (
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/http"
	pathpkg "path"
	"strings"
	"time"

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
	errGitTreeishNotFound        = errors.New("git treeish not found")
	errGitTreeishInvalidObject   = errors.New("git treeish must identify a commit or tree")
)

func (s *Server) registerGHRepoObjectRoutes() {
	s.route("GET /api/v3/repos/{owner}/{repo}/commits", s.handleListCommits)
	s.route("GET /api/v3/repos/{owner}/{repo}/readme", s.handleGetReadme)
	s.route("GET /api/v3/repos/{owner}/{repo}/contents/{path...}", s.handleGetContents)
	s.route("PUT /api/v3/repos/{owner}/{repo}/contents/{path...}", s.requirePerm(scopeContents, permWrite, s.handlePutContents))
	s.route("DELETE /api/v3/repos/{owner}/{repo}/contents/{path...}", s.requirePerm(scopeContents, permWrite, s.handleDeleteContents))
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
			writeGHValidationError(w, "Commit", name, "invalid")
			return listCommitsOptions{}, false
		}
		parsed = parsed.UTC()
		*target = &parsed
	}
	return options, true
}

func (s *Server) listRepoCommits(repo *Repo, owner, repoName string, options listCommitsOptions, baseURL string) ([]map[string]interface{}, error) {
	stor := s.store.GetGitStorage(owner, repoName)
	if stor == nil {
		return nil, errRepoGitStorageUnavailable
	}

	refName := options.Ref
	usingDefault := refName == ""
	if usingDefault {
		refName = repo.DefaultBranch
	}
	hash, err := resolveGitRef(stor, refName)
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
			touches, err := commitTouchesPath(commit, options.Path)
			if err != nil {
				return err
			}
			if !touches {
				return nil
			}
		}
		commits = append(commits, commitToJSON(commit, repo, baseURL))
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

func commitTouchesPath(commit *object.Commit, requested string) (bool, error) {
	matches := func(candidate string) bool {
		return candidate == requested || strings.HasPrefix(candidate, requested+"/")
	}
	tree, err := commit.Tree()
	if err != nil {
		return false, err
	}
	if commit.NumParents() == 0 {
		found := false
		walker := object.NewTreeWalker(tree, true, nil)
		defer walker.Close()
		for {
			name, _, err := walker.Next()
			if errors.Is(err, io.EOF) {
				return found, nil
			}
			if err != nil {
				return false, err
			}
			if matches(name) {
				found = true
			}
		}
	}
	parent, err := commit.Parent(0)
	if err != nil {
		return false, err
	}
	parentTree, err := parent.Tree()
	if err != nil {
		return false, err
	}
	changes, err := object.DiffTree(parentTree, tree)
	if err != nil {
		return false, err
	}
	for _, change := range changes {
		if matches(change.From.Name) || matches(change.To.Name) {
			return true, nil
		}
	}
	return false, nil
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

	resolvedSHA, tree, err := resolveGitTreeish(stor, sha)
	if errors.Is(err, errGitTreeishInvalidObject) {
		writeGHError(w, http.StatusUnprocessableEntity, "Invalid object requested. SHA must identify a commit or a tree.")
		return
	}
	if err != nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}

	entries := make([]map[string]interface{}, 0, len(tree.Entries))
	appendEntry := func(name string, e object.TreeEntry) {
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
		}
	} else {
		for _, entry := range tree.Entries {
			appendEntry(entry.Name, entry)
		}
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"sha":       resolvedSHA.String(),
		"url":       s.baseURL(r) + "/api/v3/repos/" + repo.FullName + "/git/trees/" + resolvedSHA.String(),
		"tree":      entries,
		"truncated": false,
	})
}

// resolveGitTreeish implements GitHub's deliberately broader tree_sha
// contract. A caller may supply a tree object SHA, a commit SHA, or a branch
// or tag name. References are dereferenced (including annotated tags), while a
// raw tag-object SHA is rejected just like github.com.
func resolveGitTreeish(stor gitStorage.Storer, value string) (plumbing.Hash, *object.Tree, error) {
	value = strings.Trim(value, "/")
	if value == "" {
		return plumbing.ZeroHash, nil, errGitTreeishNotFound
	}

	hash, found, err := resolveGitObjectReference(stor, value)
	if err != nil {
		return plumbing.ZeroHash, nil, err
	}
	if found {
		hash, err = peelGitTagObjects(stor, hash)
		if err != nil {
			return plumbing.ZeroHash, nil, errGitTreeishNotFound
		}
	} else {
		if !validGitObjectID(value) {
			return plumbing.ZeroHash, nil, errGitTreeishNotFound
		}
		hash = plumbing.NewHash(value)
	}

	encoded, err := stor.EncodedObject(plumbing.AnyObject, hash)
	if err != nil {
		return plumbing.ZeroHash, nil, errGitTreeishNotFound
	}
	switch encoded.Type() {
	case plumbing.TreeObject:
		tree, err := object.GetTree(stor, hash)
		if err != nil {
			return plumbing.ZeroHash, nil, errGitTreeishNotFound
		}
		return hash, tree, nil
	case plumbing.CommitObject:
		commit, err := object.GetCommit(stor, hash)
		if err != nil {
			return plumbing.ZeroHash, nil, errGitTreeishNotFound
		}
		tree, err := commit.Tree()
		if err != nil {
			return plumbing.ZeroHash, nil, errGitTreeishNotFound
		}
		// GitHub identifies the response by the resolved commit SHA even
		// though the entries come from that commit's root tree.
		return hash, tree, nil
	default:
		return plumbing.ZeroHash, nil, errGitTreeishInvalidObject
	}
}

func resolveGitObjectReference(stor gitStorage.Storer, value string) (plumbing.Hash, bool, error) {
	names := make([]plumbing.ReferenceName, 0, 4)
	add := func(name plumbing.ReferenceName) {
		for _, existing := range names {
			if existing == name {
				return
			}
		}
		names = append(names, name)
	}
	if strings.HasPrefix(value, "refs/") {
		add(plumbing.ReferenceName(value))
	} else {
		if strings.HasPrefix(value, "heads/") || strings.HasPrefix(value, "tags/") {
			add(plumbing.ReferenceName("refs/" + value))
		}
		add(plumbing.NewBranchReferenceName(value))
		add(plumbing.NewTagReferenceName(value))
	}
	for _, name := range names {
		ref, err := stor.Reference(name)
		if err != nil {
			continue
		}
		hash, err := resolvedReferenceHash(stor, ref, map[plumbing.ReferenceName]bool{})
		return hash, true, err
	}
	return plumbing.ZeroHash, false, nil
}

func resolvedReferenceHash(stor gitStorage.Storer, ref *plumbing.Reference, seen map[plumbing.ReferenceName]bool) (plumbing.Hash, error) {
	if ref == nil || seen[ref.Name()] {
		return plumbing.ZeroHash, errGitTreeishNotFound
	}
	seen[ref.Name()] = true
	if ref.Type() == plumbing.HashReference {
		return ref.Hash(), nil
	}
	if ref.Type() != plumbing.SymbolicReference {
		return plumbing.ZeroHash, errGitTreeishNotFound
	}
	target, err := stor.Reference(ref.Target())
	if err != nil {
		return plumbing.ZeroHash, err
	}
	return resolvedReferenceHash(stor, target, seen)
}

func peelGitTagObjects(stor gitStorage.Storer, hash plumbing.Hash) (plumbing.Hash, error) {
	seen := map[plumbing.Hash]bool{}
	for {
		if hash.IsZero() || seen[hash] {
			return plumbing.ZeroHash, errGitTreeishNotFound
		}
		seen[hash] = true
		encoded, err := stor.EncodedObject(plumbing.AnyObject, hash)
		if err != nil {
			return plumbing.ZeroHash, err
		}
		if encoded.Type() != plumbing.TagObject {
			return hash, nil
		}
		tag, err := object.GetTag(stor, hash)
		if err != nil {
			return plumbing.ZeroHash, err
		}
		hash = tag.Target
	}
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
		"mode": entry.Mode.String(),
		"type": entryType,
		"sha":  entry.Hash.String(),
		"url":  baseURL + "/api/v3/repos/" + fullName + "/git/" + entryType + "s/" + entry.Hash.String(),
	}
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

	// Search for README variants
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

		out := contentFileJSON(s.baseURL(r), repo, refName, name, entry.Hash.String(), blob.Size)
		out["encoding"] = "base64"
		out["content"] = base64.StdEncoding.EncodeToString(content)
		writeJSON(w, http.StatusOK, out)
		return
	}

	writeGHError(w, http.StatusNotFound, "Not Found")
}

// contentBlobSHA reports the blob sha at path on branch, and whether the path
// holds a file there at all.
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
// contract and writes the refusal itself, reporting whether the caller may
// proceed. `sha` names the blob the write replaces: without it every write is
// an unconditional overwrite and two concurrent editors silently lose one
// edit. Creation carries no sha, since there is no blob to name.
func contentSHAPreconditionMet(w http.ResponseWriter, stor gitStorage.Storer, branch, path, given string) bool {
	current, exists := contentBlobSHA(stor, branch, path)
	switch {
	case exists && given == "":
		writeGHValidationError(w, "Commit", "sha", "missing_field")
	case exists && given != current:
		writeGHError(w, http.StatusConflict, path+" does not match "+given)
	case !exists && given != "":
		writeGHValidationError(w, "Commit", "sha", "invalid")
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

func (s *Server) contentRefWriteGuard(r *http.Request, repo *Repo, stor gitStorage.Storer, ref plumbing.ReferenceName, kind refWriteKind) func(plumbing.Hash) error {
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
		writeGHValidationError(w, "Commit", "message", "missing_field")
		return
	}
	if req.Content == "" {
		writeGHValidationError(w, "Commit", "content", "missing_field")
		return
	}

	decoded, err := base64.StdEncoding.DecodeString(req.Content)
	if err != nil {
		writeGHValidationError(w, "Commit", "content", "invalid")
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

	// Determine whether this commit initializes an empty repository. A
	// missing branch on a repository that already has branches is a 404 on
	// real GitHub — PUT contents never creates branches (and committing via
	// an unrelated worktree would silently advance the current branch).
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
			writeSecretScanningPushProtectionBlocked(w, ph)
			return
		}
		commitHash, err = commitRootBranchWithFiles(
			stor, branch, req.Message, files, sig, true,
			s.contentRefWriteGuard(r, repo, stor, branchRef, refCreation),
		)
	} else {
		if ph := s.createSecretScanningPushProtectionPlaceholder(repo, secretScanningContentMatches(string(decoded))); ph != nil {
			writeSecretScanningPushProtectionBlocked(w, ph)
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

	s.store.UpdateRepo(owner, repoName, func(r *Repo) {
		r.PushedAt = time.Now().UTC()
	})

	base := s.baseURL(r)
	if err := s.scanCommitForSecretScanning(repo, stor, commitHash, base); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.afterCommittedRefUpdate(repo, user, branchRef.String(), beforeHash, commitHash.String(), base)
	contentOut := contentFileJSON(base, repo, branch, path, entry.Hash.String(), blob.Size)

	// 201 created, 200 updated — as on GitHub, and now answerable because the
	// precondition above already established which one this was.
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
		writeGHValidationError(w, "Commit", "message", "missing_field")
		return
	}
	if req.SHA == "" {
		writeGHValidationError(w, "Commit", "sha", "missing_field")
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
	// The same precondition a write carries: a delete naming the wrong blob is
	// a delete of something the caller has not seen.
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

	s.store.UpdateRepo(owner, repoName, func(r *Repo) {
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

// contentFileJSON builds the common members of the GitHub content-file
// shape (name/path/sha/size plus the hypermedia URLs and _links the
// schema requires) for a blob at the given path on the given ref.
func contentFileJSON(baseURL string, repo *Repo, ref, path, sha string, size int64) map[string]interface{} {
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

func contentDirectoryEntryJSON(baseURL string, repo *Repo, ref, path string, entry object.TreeEntry, size int64) map[string]interface{} {
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

	// GitHub accepts a branch, tag, full ref, or commit SHA here. Keep the
	// original ref text in hypermedia URLs while resolving it through the
	// same treeish helper used by the rest of the repository read surface.
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

	// Empty path means list the root tree.
	if path == "" {
		s.writeTreeListing(w, r, stor, repo, refName, tree, tree, "")
		return
	}

	// Try as file first
	entry, err := tree.FindEntry(path)
	if err != nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}

	if entry.Mode == filemode.Symlink {
		target, err := readBlob(stor, entry.Hash)
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

	// It's a directory (tree entry)
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
	repo *Repo,
	refName string,
	requestedPath string,
	hash plumbing.Hash,
) {
	blob, err := object.GetBlob(stor, hash)
	if err != nil {
		writeGHError(w, http.StatusInternalServerError, "Git object unavailable")
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

	accept := r.Header.Get("Accept")
	if strings.Contains(accept, "application/vnd.github.raw") {
		w.Header().Set("Content-Type", "application/octet-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(content)
		return
	}
	if strings.Contains(accept, "application/vnd.github.html") {
		rendered, err := s.renderMarkdown(string(content), "gfm", repo.FullName, s.baseURL(r))
		if err != nil {
			writeGHError(w, http.StatusInternalServerError, "Markdown rendering failed")
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, rendered)
		return
	}
	out := contentFileJSON(s.baseURL(r), repo, refName, requestedPath, hash.String(), blob.Size)
	out["encoding"] = "base64"
	out["content"] = base64.StdEncoding.EncodeToString(content)
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) writeTreeListing(
	w http.ResponseWriter,
	r *http.Request,
	stor gitStorage.Storer,
	repo *Repo,
	ref string,
	root *object.Tree,
	tree *object.Tree,
	prefix string,
) {
	items := make([]map[string]interface{}, 0, len(tree.Entries))
	for _, e := range tree.Entries {
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
			target, err := readBlob(stor, e.Hash)
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
	if strings.Contains(r.Header.Get("Accept"), "application/vnd.github.object") {
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
	raw, err := readBlob(stor, entry.Hash)
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

// repoHasAnyBranch reports whether the repository has at least one branch.
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
