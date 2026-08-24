package bleephub

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path"
	"strings"
	"time"

	"github.com/e6qu/bleephub/internal/gitstore"
	"github.com/e6qu/bleephub/internal/store"
	"github.com/go-git/go-billy/v5"
	"github.com/go-git/go-billy/v5/memfs"
	git "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/plumbing/storer"
	gitStorage "github.com/go-git/go-git/v5/storage"
	"github.com/go-git/go-git/v5/storage/memory"
)

// repoSignature returns the default author/committer signature for
// bleephub-generated commits. Matches the GitHub web UI's behavior for
// auto_init and web-based file creation.
func repoSignature(name, email string) *object.Signature {
	return &object.Signature{
		Name:  name,
		Email: email,
		When:  time.Now().UTC(),
	}
}

// syncRepoHeadToDefaultBranch points the repository's stored git HEAD at the
// default branch its row now records. Every handler that moves default_branch
// calls it, so the ref advertisement — and therefore the branch a clone checks
// out — follows the change.
func (s *Server) syncRepoHeadToDefaultBranch(owner, name string) {
	repo := s.store.GetRepo(owner, name)
	if repo == nil {
		return
	}
	if err := store.SetGitHeadBranch(s.store.GetGitStorage(owner, name), repo.DefaultBranch); err != nil {
		s.logger.Error().Err(err).Str("repo", repo.FullName).Str("branch", repo.DefaultBranch).
			Msg("could not point git HEAD at the default branch")
	}
}

// worktreeHeadStorer keeps go-git's worktree machinery out of the
// repository's HEAD.
//
// Worktree.Checkout(&CheckoutOptions{Hash: …}) replaces HEAD with a DETACHED
// hash reference naming the commit it checked out, and Worktree.Commit then
// advances whatever HEAD names. On a bare server-side repository that is pure
// corruption: HEAD is the symbolic reference the clone protocol advertises the
// default branch from, and a detached HEAD makes go-git advertise no
// symref=HEAD:… capability at all — which is what left clients guessing the
// checkout branch by matching HEAD's object id against the ref list. The
// commit helpers set the branch reference themselves, so the worktree's HEAD
// bookkeeping is scratch state; hold it in memory and let every other
// reference through to the real storage.
type worktreeHeadStorer struct {
	gitStorage.Storer
	head *plumbing.Reference
}

func newWorktreeHeadStorer(stor gitStorage.Storer) *worktreeHeadStorer {
	return &worktreeHeadStorer{
		Storer: stor,
		// git.Open rejects a storer with no HEAD; a detached zero hash is the
		// placeholder the first Checkout immediately replaces.
		head: plumbing.NewHashReference(plumbing.HEAD, plumbing.ZeroHash),
	}
}

func (s *worktreeHeadStorer) Reference(name plumbing.ReferenceName) (*plumbing.Reference, error) {
	if name == plumbing.HEAD {
		return s.head, nil
	}
	return s.Storer.Reference(name)
}

func (s *worktreeHeadStorer) SetReference(ref *plumbing.Reference) error {
	if ref.Name() == plumbing.HEAD {
		s.head = ref
		return nil
	}
	return s.Storer.SetReference(ref)
}

func (s *worktreeHeadStorer) CheckAndSetReference(next, old *plumbing.Reference) error {
	if next.Name() == plumbing.HEAD {
		s.head = next
		return nil
	}
	return s.Storer.CheckAndSetReference(next, old)
}

// initRepoWithFiles creates the first commit on a freshly created repo,
// populating it with the supplied files and pointing the given branch at
// the resulting commit. It is used for auto_init and for the contents
// PUT endpoint when the caller creates the first file in an empty repo.
// initEmptyRepoWithFiles is the API-facing first-commit operation. Unlike the
// lower-level root-branch builder, it rejects initialization once any branch
// exists, including when a concurrent request won the race on a different
// branch.
func initEmptyRepoWithFiles(stor gitStorage.Storer, branch, message string, files map[string]string, sig *object.Signature) (plumbing.Hash, error) {
	return commitRootBranchWithFiles(stor, branch, message, files, sig, true, nil)
}

func commitRootBranchWithFiles(stor gitStorage.Storer, branch, message string, files map[string]string, sig *object.Signature, requireEmpty bool, guard func(plumbing.Hash) error) (plumbing.Hash, error) {
	fs := memfs.New()
	// Build the unborn-branch commit in an isolated storer. go-git's
	// Worktree.Commit advances refs/heads/master as a side effect, which
	// would expose a provisional ref in the destination and let concurrent
	// first-commit requests overwrite each other before our atomic
	// initialization boundary.
	source := memory.NewStorage()
	repo, err := git.Init(source, fs)
	if err != nil {
		return plumbing.ZeroHash, fmt.Errorf("git init: %w", err)
	}
	wt, err := repo.Worktree()
	if err != nil {
		return plumbing.ZeroHash, fmt.Errorf("worktree: %w", err)
	}
	if err := writeFilesToWorktree(fs, wt, files); err != nil {
		return plumbing.ZeroHash, err
	}
	commitHash, err := wt.Commit(message, &git.CommitOptions{Author: sig, Committer: sig})
	if err != nil {
		return plumbing.ZeroHash, fmt.Errorf("commit: %w", err)
	}
	for _, objectType := range []plumbing.ObjectType{plumbing.BlobObject, plumbing.TreeObject, plumbing.CommitObject} {
		objects, err := source.IterEncodedObjects(objectType)
		if err != nil {
			return plumbing.ZeroHash, fmt.Errorf("iterate initial %s objects: %w", objectType, err)
		}
		copyErr := objects.ForEach(func(encoded plumbing.EncodedObject) error {
			return copyEncodedObject(stor, encoded)
		})
		objects.Close()
		if copyErr != nil {
			return plumbing.ZeroHash, fmt.Errorf("store initial %s objects: %w", objectType, copyErr)
		}
	}
	branchRef := plumbing.NewHashReference(plumbing.NewBranchReferenceName(branch), commitHash)
	if guard != nil {
		if err := guard(commitHash); err != nil {
			return plumbing.ZeroHash, err
		}
	}
	if err := gitstore.InitializeRepositoryReferences(stor, branchRef, requireEmpty); err != nil {
		return plumbing.ZeroHash, fmt.Errorf("initialize refs: %w", err)
	}
	return commitHash, nil
}

// createFileCommit adds or updates a single file on the given branch and
// returns the new commit hash. It preserves the existing tree, sets the
// commit parent to the current branch HEAD, and updates the branch ref.
func createFileCommit(stor gitStorage.Storer, branch, path, content, message string, sig *object.Signature) (plumbing.Hash, error) {
	return createFileCommitExpected(stor, branch, path, content, message, sig, plumbing.ZeroHash)
}

func createFileCommitExpected(stor gitStorage.Storer, branch, path, content, message string, sig *object.Signature, expectedParent plumbing.Hash) (plumbing.Hash, error) {
	return createFileCommitExpectedGuarded(stor, branch, path, content, message, sig, expectedParent, nil)
}

func createFileCommitExpectedGuarded(stor gitStorage.Storer, branch, path, content, message string, sig *object.Signature, expectedParent plumbing.Hash, guard func(plumbing.Hash) error) (plumbing.Hash, error) {
	fs := memfs.New()
	repo, err := git.Open(newWorktreeHeadStorer(stor), fs)
	if err != nil {
		return plumbing.ZeroHash, fmt.Errorf("git open: %w", err)
	}
	wt, err := repo.Worktree()
	if err != nil {
		return plumbing.ZeroHash, fmt.Errorf("worktree: %w", err)
	}

	branchRef := plumbing.NewBranchReferenceName(branch)
	ref, err := repo.Storer.Reference(branchRef)
	if err != nil {
		return plumbing.ZeroHash, fmt.Errorf("resolve branch %s: %w", branch, err)
	}
	parentHash := ref.Hash()
	if !expectedParent.IsZero() && parentHash != expectedParent {
		return plumbing.ZeroHash, gitStorage.ErrReferenceHasChanged
	}

	if err := wt.Checkout(&git.CheckoutOptions{Hash: parentHash, Force: true}); err != nil {
		return plumbing.ZeroHash, fmt.Errorf("checkout: %w", err)
	}

	if err := writeFileToWorktree(fs, wt, path, content); err != nil {
		return plumbing.ZeroHash, err
	}

	commitHash, err := wt.Commit(message, &git.CommitOptions{
		Author:    sig,
		Committer: sig,
		Parents:   []plumbing.Hash{parentHash},
	})
	if err != nil {
		return plumbing.ZeroHash, fmt.Errorf("commit: %w", err)
	}
	if guard != nil {
		if err := guard(commitHash); err != nil {
			return plumbing.ZeroHash, err
		}
	}
	if err := repo.Storer.CheckAndSetReference(plumbing.NewHashReference(branchRef, commitHash), ref); err != nil {
		return plumbing.ZeroHash, fmt.Errorf("set ref: %w", err)
	}
	return commitHash, nil
}

// deleteFileCommit removes a single file on the given branch and returns the
// new commit hash. It returns an error if the file does not exist.
func deleteFileCommit(stor gitStorage.Storer, branch, path, message string, sig *object.Signature, expectedParent plumbing.Hash, guard func(plumbing.Hash) error) (plumbing.Hash, error) {
	fs := memfs.New()
	repo, err := git.Open(newWorktreeHeadStorer(stor), fs)
	if err != nil {
		return plumbing.ZeroHash, fmt.Errorf("git open: %w", err)
	}
	wt, err := repo.Worktree()
	if err != nil {
		return plumbing.ZeroHash, fmt.Errorf("worktree: %w", err)
	}

	branchRef := plumbing.NewBranchReferenceName(branch)
	ref, err := repo.Storer.Reference(branchRef)
	if err != nil {
		return plumbing.ZeroHash, fmt.Errorf("resolve branch %s: %w", branch, err)
	}
	parentHash := ref.Hash()
	if !expectedParent.IsZero() && parentHash != expectedParent {
		return plumbing.ZeroHash, gitStorage.ErrReferenceHasChanged
	}

	if err := wt.Checkout(&git.CheckoutOptions{Hash: parentHash, Force: true}); err != nil {
		return plumbing.ZeroHash, fmt.Errorf("checkout: %w", err)
	}

	if _, err := fs.Stat(path); err != nil {
		return plumbing.ZeroHash, fmt.Errorf("path does not exist: %s", path)
	}

	if _, err := wt.Remove(path); err != nil {
		return plumbing.ZeroHash, fmt.Errorf("git remove %s: %w", path, err)
	}

	commitHash, err := wt.Commit(message, &git.CommitOptions{
		Author:    sig,
		Committer: sig,
		Parents:   []plumbing.Hash{parentHash},
	})
	if err != nil {
		return plumbing.ZeroHash, fmt.Errorf("commit: %w", err)
	}
	if guard != nil {
		if err := guard(commitHash); err != nil {
			return plumbing.ZeroHash, err
		}
	}
	if err := repo.Storer.CheckAndSetReference(plumbing.NewHashReference(branchRef, commitHash), ref); err != nil {
		return plumbing.ZeroHash, fmt.Errorf("set ref: %w", err)
	}
	return commitHash, nil
}

func writeFilesToWorktree(fs billy.Filesystem, wt *git.Worktree, files map[string]string) error {
	for path, body := range files {
		if err := writeFileToWorktree(fs, wt, path, body); err != nil {
			return err
		}
	}
	return nil
}

func writeFileToWorktree(fs billy.Filesystem, wt *git.Worktree, path, body string) error {
	if idx := strings.LastIndex(path, "/"); idx >= 0 {
		if err := fs.MkdirAll(path[:idx], 0o755); err != nil {
			return fmt.Errorf("mkdir %s: %w", path[:idx], err)
		}
	}
	f, err := fs.Create(path)
	if err != nil {
		return fmt.Errorf("create %s: %w", path, err)
	}
	if _, err := f.Write([]byte(body)); err != nil {
		_ = f.Close()
		return fmt.Errorf("write %s: %w", path, err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("close %s: %w", path, err)
	}
	if _, err := wt.Add(path); err != nil {
		return fmt.Errorf("git add %s: %w", path, err)
	}
	return nil
}

// ensureRepoInitialized creates git storage for a repo that does not yet
// have any. It is used by org repo creation, which historically registered
// the Repo row before allocating storage.
func (s *Server) initRepoFiles(ctx context.Context, repo *store.Repo, branch, description, gitignoreTemplate, licenseTemplate string, includeReadme bool) error {
	owner, name, ok := store.SplitRepoFullName(repo.FullName)
	if !ok {
		return fmt.Errorf("invalid full name %q", repo.FullName)
	}
	stor := s.store.GetGitStorage(owner, name)
	if stor == nil {
		return fmt.Errorf("no git storage for %s", repo.FullName)
	}

	files := make(map[string]string)
	if includeReadme {
		files["README.md"] = renderReadme(repo.Name, description)
	}
	if gitignoreTemplate != "" {
		if name, ok := normalizeGitignoreName(gitignoreTemplate); ok {
			files[".gitignore"] = gitignoreTemplates[name]
		}
	}
	if licenseTemplate != "" {
		if key, ok := normalizeLicenseKey(licenseTemplate); ok {
			files["LICENSE"] = licenseBody(key, owner, repo.Name, time.Now().Year())
		}
	}
	if len(files) == 0 {
		return nil
	}

	sig := repoSignature(userDisplayName(repo), "bleephub@local")
	_, err := initEmptyRepoWithFiles(stor, branch, "Initial commit", files, sig)
	if err != nil {
		return err
	}
	s.store.UpdateRepo(owner, name, func(r *store.Repo) {
		r.PushedAt = time.Now().UTC()
		if licenseTemplate != "" {
			if key, ok := normalizeLicenseKey(licenseTemplate); ok {
				tmpl := store.LicenseTemplates[key]
				r.LicenseKey = key
				r.LicenseName = tmpl.Name
				r.LicenseSPDX = tmpl.SpdxID
			}
		}
	})
	return nil
}

func userDisplayName(repo *store.Repo) string {
	if repo.Owner != nil && repo.Owner.Name != "" {
		return repo.Owner.Name
	}
	if repo.Owner != nil {
		return repo.Owner.Login
	}
	parts := strings.SplitN(repo.FullName, "/", 2)
	if len(parts) == 2 {
		return parts[0]
	}
	return "bleephub"
}

func renderReadme(repoName, description string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# %s\n\n", repoName)
	if description != "" {
		fmt.Fprintln(&b, description)
	}
	return b.String()
}

func (s *Server) registerGHGitDataRoutes() {
	s.route("GET /api/v3/repos/{owner}/{repo}/git/blobs/{sha}", s.requirePerm(store.ScopeContents, store.PermRead, s.handleGetBlob))
	s.route("POST /api/v3/repos/{owner}/{repo}/git/blobs", s.requirePerm(store.ScopeContents, store.PermWrite, s.requireRepoPush(s.handleCreateBlob)))
	s.route("GET /api/v3/repos/{owner}/{repo}/git/trees/{sha...}", s.requirePerm(store.ScopeContents, store.PermRead, s.handleGetTree))
	s.route("POST /api/v3/repos/{owner}/{repo}/git/trees", s.requirePerm(store.ScopeContents, store.PermWrite, s.requireRepoPush(s.handleCreateTree)))
	s.route("GET /api/v3/repos/{owner}/{repo}/git/commits/{sha}", s.requirePerm(store.ScopeContents, store.PermRead, s.handleGetCommit))
	s.route("POST /api/v3/repos/{owner}/{repo}/git/commits", s.requirePerm(store.ScopeContents, store.PermWrite, s.requireRepoPush(s.handleCreateCommit)))
	s.route("GET /api/v3/repos/{owner}/{repo}/git/tags/{sha}", s.requirePerm(store.ScopeContents, store.PermRead, s.handleGetTag))
	s.route("POST /api/v3/repos/{owner}/{repo}/git/tags", s.requirePerm(store.ScopeContents, store.PermWrite, s.requireRepoPush(s.handleCreateTag)))
	s.route("GET /api/v3/repos/{owner}/{repo}/git/refs/{ref...}", s.requirePerm(store.ScopeContents, store.PermRead, s.handleGetRefs))
	s.route("POST /api/v3/repos/{owner}/{repo}/git/refs", s.requirePerm(store.ScopeContents, store.PermWrite, s.requireRepoPush(s.handleCreateRef)))
	s.route("PATCH /api/v3/repos/{owner}/{repo}/git/refs/{ref...}", s.requirePerm(store.ScopeContents, store.PermWrite, s.requireRepoPush(s.handleUpdateRef)))
	s.route("DELETE /api/v3/repos/{owner}/{repo}/git/refs/{ref...}",
		s.requirePerm(store.ScopeContents, store.PermWrite, s.requireRepoPush(s.requireRefDeletionAllowed(s.handleDeleteRef))))
}

// requireRepoAdmin resolves the repository named in the path and admits only a
// credential that administers it. A repository the caller cannot read — or one
// that does not exist, which includes a case-variant spelling of a real one —
// is 404, so the answer never confirms what is there.
//
// Handlers must take the returned *Repo as the scope key rather than
// re-deriving one from the path values: a path-derived key addresses a scope no
// other code path reads.
func (s *Server) requireRepoAdmin(w http.ResponseWriter, r *http.Request) (*store.Repo, bool) {
	repo := s.store.GetRepo(r.PathValue("owner"), r.PathValue("repo"))
	if repo == nil || !s.viewerCanReadRepo(r.Context(), repo) {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return nil, false
	}
	// The scope is secrets, not administration: every caller of this is a
	// repository-secret handler, and GitHub grants those at secrets:write while
	// still demanding repository admin of a human. Naming administration here
	// would refuse an app GitHub allows.
	if !s.viewerMayActOnRepo(r.Context(), repo, store.ScopeSecrets, store.PermWrite, store.PermAdmin) {
		writeGHError(w, http.StatusForbidden, "Must have admin rights to Repository.")
		return nil, false
	}
	return repo, true
}

// requireRepoPush is the write half of requireRepoAdmin as a route decorator.
func (s *Server) requireRepoPush(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		repo := s.store.GetRepo(r.PathValue("owner"), r.PathValue("repo"))
		if repo == nil || !s.viewerCanReadRepo(r.Context(), repo) {
			writeGHError(w, http.StatusNotFound, "Not Found")
			return
		}
		if !s.viewerCanPushRepo(r.Context(), repo) {
			writeGHError(w, http.StatusForbidden, "Must have push access to repository.")
			return
		}
		next(w, r)
	}
}

// refUpdateIsFastForward reports whether moving a ref from current to target
// discards nothing the ref already reaches, which is true exactly when target
// descends from current. A target that is not a commit — a tag, a tree, a blob
// — can never descend from one, so it is not a fast forward.
func refUpdateIsFastForward(stor storer.Storer, current, target plumbing.Hash) (bool, error) {
	if current == target {
		return true, nil
	}
	base, err := object.GetCommit(stor, current)
	if err != nil {
		return false, nil
	}
	head, err := object.GetCommit(stor, target)
	if err != nil {
		return false, nil
	}
	ancestor, err := base.IsAncestor(head)
	if err != nil {
		return false, fmt.Errorf("ancestry of %s in %s: %w", current, target, err)
	}
	return ancestor, nil
}

// refWriteRefusal decides a REST ref write against the branch's protection
// rule, through the same predicate the git transports push through. Push
// access — required by the route gate — is enough for an unprotected ref.
func (s *Server) refWriteRefusal(r *http.Request, repo *store.Repo, ref plumbing.ReferenceName, kind refWriteKind, target plumbing.Hash) string {
	stor, _ := s.store.GitStorageForRepoID(repo.ID)
	return s.protectedRefWriteRefusal(r.Context(), repo, stor, ref, kind, target)
}

// requireRefDeletionAllowed guards the delete-ref route, whose handler lives
// with the other ref readers. Deleting a protected branch is the one ref write
// with no body to inspect, so it is decided here rather than in the handler.
func (s *Server) requireRefDeletionAllowed(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		repo := s.store.GetRepo(r.PathValue("owner"), r.PathValue("repo"))
		if repo == nil {
			writeGHError(w, http.StatusNotFound, "Not Found")
			return
		}
		ref := plumbing.ReferenceName("refs/" + r.PathValue("ref"))
		if refusal := s.refWriteRefusal(r, repo, ref, refDeletion, plumbing.ZeroHash); refusal != "" {
			writeGHError(w, http.StatusForbidden, refusal)
			return
		}
		next(w, r)
	}
}

func (s *Server) gitDataContext(w http.ResponseWriter, r *http.Request) (owner, repoName string, repo *store.Repo, stor gitStorage.Storer) {
	owner = r.PathValue("owner")
	repoName = r.PathValue("repo")
	repo = s.store.GetRepo(owner, repoName)
	if repo == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	stor = s.store.GetGitStorage(owner, repoName)
	if stor == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	return
}

// refuseOversizedBlob writes the refusal POST /git/blobs documents for content
// past its size ceiling.
func refuseOversizedBlob(w http.ResponseWriter) {
	store.WriteGHValidationError(w, "Blob", "content", "too_large")
}

func (s *Server) handleCreateBlob(w http.ResponseWriter, r *http.Request) {
	_, _, repo, stor := s.gitDataContext(w, r)
	if repo == nil {
		return
	}
	var req struct {
		Content  *string `json:"content"`
		Encoding string  `json:"encoding"`
	}
	// The blob body is a whole file, so it gets its own cap, and an over-cap
	// body is refused the way this operation documents a refusal — 422
	// Validation Failed with Blob/content/too_large. The generic 413 is not one
	// of the statuses the description lists (403, 404, 409, 422).
	if !decodeJSONBodyOversizeAware(w, r, maxBlobJSONBodyBytes, &req, refuseOversizedBlob) {
		return
	}
	if req.Content == nil {
		store.WriteGHValidationError(w, "Blob", "content", "missing_field")
		return
	}
	if req.Encoding == "" {
		req.Encoding = "utf-8"
	}
	if req.Encoding != "utf-8" && req.Encoding != "base64" {
		store.WriteGHValidationError(w, "Blob", "encoding", "invalid")
		return
	}
	data := []byte(*req.Content)
	if req.Encoding == "base64" {
		var err error
		data, err = base64.StdEncoding.DecodeString(*req.Content)
		if err != nil {
			store.WriteGHValidationError(w, "Blob", "content", "invalid")
			return
		}
	}

	// GitHub refuses to create a blob past the same 100 MB ceiling its read
	// side serves, whatever the transport allowed.
	if int64(len(data)) > gitBlobMaxFileBytes {
		refuseOversizedBlob(w)
		return
	}

	// Push protection scans the DECODED bytes — a base64 upload must not slip
	// a blocked secret past the same scanner the contents and git/refs writes
	// use. This operation is one of exactly two GitHub documents the
	// repository-rule-violation-error on, at 422.
	if ph := s.createSecretScanningPushProtectionPlaceholder(repo, secretScanningContentMatches(string(data))); ph != nil {
		writeSecretScanningRuleViolation(w, http.StatusUnprocessableEntity, ph)
		return
	}

	hash, err := encodeBlob(stor, data)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	base := s.baseURL(r)
	blobURL := base + "/api/v3/repos/" + repo.FullName + "/git/blobs/" + hash.String()
	writeJSONCreated(w, blobURL, map[string]interface{}{
		"sha": hash.String(),
		"url": blobURL,
	})
}

func (s *Server) handleCreateTree(w http.ResponseWriter, r *http.Request) {
	_, _, repo, stor := s.gitDataContext(w, r)
	if repo == nil {
		return
	}
	var req struct {
		BaseTree string `json:"base_tree"`
		Tree     []struct {
			Path    string          `json:"path"`
			Mode    string          `json:"mode"`
			Type    string          `json:"type"`
			SHA     json.RawMessage `json:"sha"`
			Content *string         `json:"content"`
		} `json:"tree"`
	}
	if !decodeJSONBody(w, r, &req) {
		return
	}
	if req.Tree == nil {
		store.WriteGHValidationError(w, "Tree", "tree", "missing_field")
		return
	}

	var baseTree *object.Tree
	if req.BaseTree != "" {
		if !store.ValidGitObjectID(req.BaseTree) {
			store.WriteGHValidationError(w, "Tree", "base_tree", "invalid")
			return
		}
		var err error
		baseTree, err = object.GetTree(stor, plumbing.NewHash(req.BaseTree))
		if err != nil {
			writeGHError(w, http.StatusNotFound, "Not Found")
			return
		}
	}

	files, err := flattenTreeForCreate(stor, baseTree)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	modeOverride := map[string]filemode.FileMode{
		"100644": filemode.Regular,
		"100755": filemode.Executable,
		"040000": filemode.Dir,
		"160000": filemode.Submodule,
		"120000": filemode.Symlink,
	}

	for _, ent := range req.Tree {
		if !validGitTreePath(ent.Path) {
			store.WriteGHValidationError(w, "Tree", "path", "invalid")
			return
		}

		hasSHA := len(bytes.TrimSpace(ent.SHA)) > 0
		deleteEntry := hasSHA && bytes.Equal(bytes.TrimSpace(ent.SHA), []byte("null"))
		if deleteEntry {
			if ent.Content != nil {
				writeGHError(w, http.StatusUnprocessableEntity, "sha and content are mutually exclusive")
				return
			}
			if !deleteTreePath(files, ent.Path) {
				writeGHError(w, http.StatusUnprocessableEntity, "cannot delete a non-existent path")
				return
			}
			continue
		}
		if hasSHA && ent.Content != nil {
			writeGHError(w, http.StatusUnprocessableEntity, "sha and content are mutually exclusive")
			return
		}
		if !hasSHA && ent.Content == nil {
			writeGHError(w, http.StatusUnprocessableEntity, "sha or content required")
			return
		}

		mode := filemode.Regular
		modeProvided := ent.Mode != ""
		if ent.Mode != "" {
			if m, ok := modeOverride[ent.Mode]; ok {
				mode = m
			} else {
				writeGHError(w, http.StatusUnprocessableEntity, "invalid mode: "+ent.Mode)
				return
			}
		}
		switch ent.Type {
		case "", "blob":
			if mode == filemode.Dir || mode == filemode.Submodule {
				store.WriteGHValidationError(w, "Tree", "mode", "invalid")
				return
			}
		case "tree":
			if modeProvided && mode != filemode.Dir {
				store.WriteGHValidationError(w, "Tree", "mode", "invalid")
				return
			}
			mode = filemode.Dir
		case "commit":
			if modeProvided && mode != filemode.Submodule {
				store.WriteGHValidationError(w, "Tree", "mode", "invalid")
				return
			}
			mode = filemode.Submodule
		default:
			store.WriteGHValidationError(w, "Tree", "type", "invalid")
			return
		}

		var hash plumbing.Hash
		if hasSHA {
			var sha string
			if err := json.Unmarshal(ent.SHA, &sha); err != nil || !store.ValidGitObjectID(sha) {
				store.WriteGHValidationError(w, "Tree", "sha", "invalid")
				return
			}
			hash = plumbing.NewHash(sha)
			obj, err := stor.EncodedObject(plumbing.AnyObject, hash)
			if err != nil {
				writeGHError(w, http.StatusNotFound, "Not Found")
				return
			}
			wantType := ent.Type
			if wantType == "" {
				wantType = "blob"
			}
			if objectTypeName(obj.Type()) != wantType {
				store.WriteGHValidationError(w, "Tree", "type", "invalid")
				return
			}
		} else if ent.Content != nil {
			if ent.Type != "" && ent.Type != "blob" {
				store.WriteGHValidationError(w, "Tree", "type", "invalid")
				return
			}
			hash, err = encodeBlob(stor, []byte(*ent.Content))
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
		} else {
			writeGHError(w, http.StatusUnprocessableEntity, "sha or content required")
			return
		}

		deleteTreePath(files, ent.Path)
		deleteTreePathAncestors(files, ent.Path)
		if mode == filemode.Dir {
			subtree, err := object.GetTree(stor, hash)
			if err != nil {
				writeGHError(w, http.StatusNotFound, "Not Found")
				return
			}
			nested, err := flattenTreeForCreate(stor, subtree)
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			if len(nested) == 0 {
				files[ent.Path] = object.TreeEntry{Name: path.Base(ent.Path), Mode: mode, Hash: hash}
				continue
			}
			for nestedPath, nestedEntry := range nested {
				files[path.Join(ent.Path, nestedPath)] = nestedEntry
			}
			continue
		}
		files[ent.Path] = object.TreeEntry{Name: path.Base(ent.Path), Mode: mode, Hash: hash}
	}

	hash, err := buildTreeFromPaths(stor, files)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	tree, err := object.GetTree(stor, hash)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	entries := make([]map[string]interface{}, 0, len(tree.Entries))
	for _, e := range tree.Entries {
		entries = append(entries, gitTreeEntryJSON(stor, s.baseURL(r), repo.FullName, e.Name, e))
	}
	treeURL := s.baseURL(r) + "/api/v3/repos/" + repo.FullName + "/git/trees/" + hash.String()
	writeJSONCreated(w, treeURL, map[string]interface{}{
		"sha":       hash.String(),
		"url":       treeURL,
		"tree":      entries,
		"truncated": false,
	})
}

// flattenTreeForCreate keeps empty directory entries while reducing populated
// subtrees to leaf paths. The shared merge flattener intentionally drops all
// tree entries, which would silently discard a valid empty tree supplied to
// the Git data API.
func flattenTreeForCreate(stor gitStorage.Storer, tree *object.Tree) (map[string]object.TreeEntry, error) {
	out := map[string]object.TreeEntry{}
	var walk func(*object.Tree, string) error
	walk = func(current *object.Tree, prefix string) error {
		if current == nil {
			return nil
		}
		for _, entry := range current.Entries {
			name := path.Join(prefix, entry.Name)
			if entry.Mode != filemode.Dir {
				out[name] = entry
				continue
			}
			subtree, err := object.GetTree(stor, entry.Hash)
			if err != nil {
				return err
			}
			if len(subtree.Entries) == 0 {
				out[name] = entry
				continue
			}
			if err := walk(subtree, name); err != nil {
				return err
			}
		}
		return nil
	}
	return out, walk(tree, "")
}

func validGitTreePath(value string) bool {
	if value == "" || strings.HasPrefix(value, "/") || strings.ContainsRune(value, '\x00') {
		return false
	}
	cleaned := path.Clean(value)
	return cleaned == value && cleaned != "." && cleaned != ".." && !strings.HasPrefix(cleaned, "../")
}

// deleteTreePath removes a file or every file below a directory-like path.
func deleteTreePath(files map[string]object.TreeEntry, target string) bool {
	found := false
	prefix := target + "/"
	for name := range files {
		if name == target || strings.HasPrefix(name, prefix) {
			delete(files, name)
			found = true
		}
	}
	return found
}

// A new nested path replaces a blob/submodule at any ancestor, matching
// GitHub's "entries overwrite base_tree at the same path" behavior.
func deleteTreePathAncestors(files map[string]object.TreeEntry, target string) {
	for parent := path.Dir(target); parent != "." && parent != "/"; parent = path.Dir(parent) {
		delete(files, parent)
	}
}

func (s *Server) handleCreateCommit(w http.ResponseWriter, r *http.Request) {
	_, _, repo, stor := s.gitDataContext(w, r)
	if repo == nil {
		return
	}
	var req struct {
		Message   string     `json:"message"`
		Tree      string     `json:"tree"`
		Parents   []string   `json:"parents"`
		Author    *gitPerson `json:"author"`
		Committer *gitPerson `json:"committer"`
		// The documented request body carries the caller's detached PGP
		// signature; GitHub writes it into the created commit's gpgsig header.
		// Accepting the field and dropping it wrote an object that no longer
		// matched what the caller signed, silently.
		Signature string `json:"signature"`
	}
	if !decodeJSONBody(w, r, &req) {
		return
	}
	if req.Message == "" || req.Tree == "" {
		writeGHError(w, http.StatusUnprocessableEntity, "message and tree are required")
		return
	}
	if !store.ValidGitObjectID(req.Tree) {
		store.WriteGHValidationError(w, "Commit", "tree", "invalid")
		return
	}

	treeHash := plumbing.NewHash(req.Tree)
	if _, err := object.GetTree(stor, treeHash); err != nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}

	var parentHashes []plumbing.Hash
	for _, p := range req.Parents {
		if !store.ValidGitObjectID(p) {
			store.WriteGHValidationError(w, "Commit", "parents", "invalid")
			return
		}
		h := plumbing.NewHash(p)
		if _, err := object.GetCommit(stor, h); err != nil {
			writeGHError(w, http.StatusNotFound, "Not Found")
			return
		}
		parentHashes = append(parentHashes, h)
	}

	sig := repoSignature(userDisplayName(repo), "bleephub@local")
	if req.Author != nil {
		if err := applyGitPerson(sig, *req.Author); err != nil {
			store.WriteGHValidationError(w, "Commit", "author", "invalid")
			return
		}
	}
	committerSig := *sig
	if req.Committer != nil {
		if err := applyGitPerson(&committerSig, *req.Committer); err != nil {
			store.WriteGHValidationError(w, "Commit", "committer", "invalid")
			return
		}
	}

	commit := &object.Commit{
		Author:       *sig,
		Committer:    committerSig,
		Message:      req.Message,
		TreeHash:     treeHash,
		ParentHashes: parentHashes,
		// git's gpgsig header is newline-terminated, and that is the form
		// GitHub echoes back in verification.signature. Normalizing on the way
		// in keeps the create response byte-identical to a later read of the
		// stored object.
		PGPSignature: terminatedSignature(req.Signature),
	}
	hash, err := encodeCommit(stor, commit)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	commitJSON := gitCommitToJSON(s.baseURL(r), repo.FullName, hash.String(), commit)
	writeJSONCreated(w, jsonStringField(commitJSON, "url"), commitJSON)
}

func (s *Server) handleGetCommit(w http.ResponseWriter, r *http.Request) {
	owner, repoName, _, stor := s.gitDataContext(w, r)
	if stor == nil {
		return
	}
	sha := r.PathValue("sha")
	commit, err := object.GetCommit(stor, plumbing.NewHash(sha))
	if err != nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	writeJSON(w, http.StatusOK, gitCommitToJSON(s.baseURL(r), owner+"/"+repoName, sha, commit))
}

func (s *Server) handleCreateTag(w http.ResponseWriter, r *http.Request) {
	_, _, repo, stor := s.gitDataContext(w, r)
	if repo == nil {
		return
	}
	var req struct {
		Tag     string     `json:"tag"`
		Message string     `json:"message"`
		Object  string     `json:"object"`
		Type    string     `json:"type"`
		Tagger  *gitPerson `json:"tagger"`
	}
	if !decodeJSONBody(w, r, &req) {
		return
	}
	if req.Tag == "" || req.Message == "" || req.Object == "" || req.Type == "" {
		writeGHError(w, http.StatusUnprocessableEntity, "tag, message, object, and type are required")
		return
	}
	if !store.ValidGitObjectID(req.Object) {
		store.WriteGHValidationError(w, "Tag", "object", "invalid")
		return
	}

	targetHash := plumbing.NewHash(req.Object)
	objType := plumbing.AnyObject
	switch req.Type {
	case "commit":
		objType = plumbing.CommitObject
	case "tree":
		objType = plumbing.TreeObject
	case "blob":
		objType = plumbing.BlobObject
	default:
		store.WriteGHValidationError(w, "Tag", "type", "invalid")
		return
	}
	if _, err := stor.EncodedObject(objType, targetHash); err != nil {
		store.WriteGHValidationError(w, "Tag", "object", "invalid")
		return
	}

	tagger := repoSignature(userDisplayName(repo), "bleephub@local")
	if req.Tagger != nil {
		if err := applyGitPerson(tagger, *req.Tagger); err != nil {
			store.WriteGHValidationError(w, "Tag", "tagger", "invalid")
			return
		}
	}
	tag := &object.Tag{
		Name:       req.Tag,
		Tagger:     *tagger,
		Message:    req.Message,
		Target:     targetHash,
		TargetType: objType,
	}
	hash, err := encodeTag(stor, tag)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	tagJSON := gitTagToJSON(s.baseURL(r), repo.FullName, hash.String(), tag)
	writeJSONCreated(w, jsonStringField(tagJSON, "url"), tagJSON)
}

func (s *Server) handleGetTag(w http.ResponseWriter, r *http.Request) {
	owner, repoName, _, stor := s.gitDataContext(w, r)
	if stor == nil {
		return
	}
	sha := r.PathValue("sha")
	tag, err := object.GetTag(stor, plumbing.NewHash(sha))
	if err != nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	writeJSON(w, http.StatusOK, gitTagToJSON(s.baseURL(r), owner+"/"+repoName, sha, tag))
}

// buildRefLifecyclePayload assembles the shared body of GitHub's `create` and
// `delete` webhook events so `on: create` / `on: delete` workflows fire for
// branch and tag creation/deletion (ACT-026).
func buildRefLifecyclePayload(repo *store.Repo, fullRef plumbing.ReferenceName, sender *store.User, baseURL string) map[string]interface{} {
	refType := "tag"
	if fullRef.IsBranch() {
		refType = "branch"
	}
	return map[string]interface{}{
		"ref":         fullRef.Short(),
		"ref_type":    refType,
		"pusher_type": "user",
		"repository":  repoPayload(repo, baseURL),
		"sender":      store.UserToJSON(sender, baseURL),
	}
}

func (s *Server) handleCreateRef(w http.ResponseWriter, r *http.Request) {
	_, _, repo, stor := s.gitDataContext(w, r)
	if repo == nil {
		return
	}
	var req struct {
		Ref string `json:"ref"`
		SHA string `json:"sha"`
	}
	if !decodeJSONBody(w, r, &req) {
		return
	}
	if req.Ref == "" || req.SHA == "" {
		writeGHError(w, http.StatusUnprocessableEntity, "ref and sha are required")
		return
	}
	if !validFullyQualifiedGitRef(req.Ref) {
		store.WriteGHValidationError(w, "Reference", "ref", "invalid")
		return
	}
	if !store.ValidGitObjectID(req.SHA) {
		store.WriteGHValidationError(w, "Reference", "sha", "invalid")
		return
	}
	fullRef := plumbing.ReferenceName(req.Ref)
	target := plumbing.NewHash(req.SHA)
	if _, err := stor.EncodedObject(plumbing.AnyObject, target); err != nil {
		writeGHError(w, http.StatusUnprocessableEntity, "Object does not exist")
		return
	}
	if refusal := s.refWriteRefusal(r, repo, fullRef, refCreation, target); refusal != "" {
		writeGHError(w, http.StatusForbidden, refusal)
		return
	}
	if ph, err := s.secretScanningPushProtectionPlaceholderForRef(repo, stor, fullRef, target); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	} else if ph != nil {
		writeSecretScanningPushProtectionBlocked(w, ph)
		return
	}
	if err := gitstore.CreateReferenceIfAbsent(stor, plumbing.NewHashReference(fullRef, target)); err != nil {
		if errors.Is(err, gitstore.ErrReferenceAlreadyExists) {
			writeGHError(w, http.StatusUnprocessableEntity, "Reference already exists")
			return
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if err := s.scanRefForSecretScanning(repo, stor, fullRef, target, s.baseURL(r)); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if fullRef.IsBranch() {
		s.afterCommittedRefUpdate(repo, ghUserFromContext(r.Context()), fullRef.String(), plumbing.ZeroHash.String(), target.String(), s.baseURL(r))
	}
	// The `create` event fires for a newly-created branch or tag (distinct from
	// the `push` above, which afterCommittedRefUpdate emits for branches).
	createPayload := buildRefLifecyclePayload(repo, fullRef, ghUserFromContext(r.Context()), s.baseURL(r))
	createPayload["master_branch"] = repo.DefaultBranch
	createPayload["description"] = repo.Description
	s.emitWebhookEvent(repo.FullName, "create", "", createPayload)
	ref, _ := stor.Reference(fullRef)
	refJSON := refToJSON(stor, s.baseURL(r), repo.FullName, ref)
	writeJSONCreated(w, jsonStringField(refJSON, "url"), refJSON)
}

func (s *Server) handleUpdateRef(w http.ResponseWriter, r *http.Request) {
	_, _, repo, stor := s.gitDataContext(w, r)
	if repo == nil {
		return
	}
	refPath := r.PathValue("ref")
	var req struct {
		SHA   string `json:"sha"`
		Force bool   `json:"force"`
	}
	if !decodeJSONBody(w, r, &req) {
		return
	}
	fullRef := plumbing.ReferenceName("refs/" + refPath)
	if req.SHA == "" {
		store.WriteGHValidationError(w, "Reference", "sha", "missing_field")
		return
	}
	if !validFullyQualifiedGitRef(fullRef.String()) || !store.ValidGitObjectID(req.SHA) {
		store.WriteGHValidationError(w, "Reference", "sha", "invalid")
		return
	}
	kind := refFastForward
	if req.Force {
		kind = refForcePush
	}
	newTarget := plumbing.NewHash(req.SHA)
	if refusal := s.refWriteRefusal(r, repo, fullRef, kind, newTarget); refusal != "" {
		writeGHError(w, http.StatusForbidden, refusal)
		return
	}
	oldRef, err := stor.Reference(fullRef)
	if err != nil {
		writeGHError(w, http.StatusUnprocessableEntity, "Reference does not exist")
		return
	}
	if _, err := stor.EncodedObject(plumbing.AnyObject, newTarget); err != nil {
		writeGHError(w, http.StatusUnprocessableEntity, "Object does not exist")
		return
	}
	if !req.Force && oldRef.Type() == plumbing.HashReference && newTarget != oldRef.Hash() {
		fastForward, err := refUpdateIsFastForward(stor, oldRef.Hash(), newTarget)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if !fastForward {
			writeGHError(w, http.StatusUnprocessableEntity, "Update is not a fast forward")
			return
		}
	}
	if ph, err := s.secretScanningPushProtectionPlaceholderForRef(repo, stor, fullRef, newTarget); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	} else if ph != nil {
		writeSecretScanningPushProtectionBlocked(w, ph)
		return
	}
	if err := stor.CheckAndSetReference(plumbing.NewHashReference(fullRef, newTarget), oldRef); err != nil {
		if errors.Is(err, gitStorage.ErrReferenceHasChanged) {
			writeGHError(w, http.StatusConflict, "Reference update failed: the ref changed while the request was being processed")
			return
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if err := s.scanRefForSecretScanning(repo, stor, fullRef, newTarget, s.baseURL(r)); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if fullRef.IsBranch() {
		s.afterCommittedRefUpdate(repo, ghUserFromContext(r.Context()), fullRef.String(), oldRef.Hash().String(), newTarget.String(), s.baseURL(r))
	}
	ref, _ := stor.Reference(fullRef)
	writeJSON(w, http.StatusOK, refToJSON(stor, s.baseURL(r), repo.FullName, ref))
}

func (s *Server) scanRefForSecretScanning(repo *store.Repo, stor storer.Storer, ref plumbing.ReferenceName, target plumbing.Hash, baseURL string) error {
	if !strings.HasPrefix(string(ref), "refs/heads/") {
		return nil
	}
	if _, err := object.GetCommit(stor, target); err != nil {
		return nil
	}
	return s.scanCommitForSecretScanning(repo, stor, target, baseURL)
}

type gitPerson struct {
	Name  string `json:"name"`
	Email string `json:"email"`
	Date  string `json:"date"`
}

func applyGitPerson(signature *object.Signature, person gitPerson) error {
	if strings.TrimSpace(person.Name) == "" || strings.TrimSpace(person.Email) == "" {
		return errors.New("git identity requires name and email")
	}
	signature.Name = person.Name
	signature.Email = person.Email
	if person.Date == "" {
		return nil
	}
	when, err := time.Parse(time.RFC3339, person.Date)
	if err != nil {
		return err
	}
	signature.When = when
	return nil
}

func validFullyQualifiedGitRef(value string) bool {
	if !strings.HasPrefix(value, "refs/") || strings.Count(value, "/") < 2 ||
		strings.HasSuffix(value, "/") || strings.HasSuffix(value, ".") ||
		strings.Contains(value, "..") || strings.Contains(value, "@{") ||
		strings.Contains(value, "//") || strings.Contains(value, "\\") {
		return false
	}
	for _, segment := range strings.Split(value, "/") {
		if segment == "" || strings.HasPrefix(segment, ".") || strings.HasSuffix(segment, ".lock") {
			return false
		}
	}
	for _, char := range value {
		if char <= ' ' || char == 0x7f || strings.ContainsRune("~^:?*[", char) {
			return false
		}
	}
	return true
}

func gitCommitToJSON(baseURL, fullName, sha string, c *object.Commit) map[string]interface{} {
	author := map[string]interface{}{
		"name":  c.Author.Name,
		"email": c.Author.Email,
		"date":  c.Author.When.Format(time.RFC3339),
	}
	committer := map[string]interface{}{
		"name":  c.Committer.Name,
		"email": c.Committer.Email,
		"date":  c.Committer.When.Format(time.RFC3339),
	}
	parents := make([]map[string]interface{}, 0, len(c.ParentHashes))
	for _, h := range c.ParentHashes {
		parents = append(parents, map[string]interface{}{
			"sha":      h.String(),
			"url":      baseURL + "/api/v3/repos/" + fullName + "/git/commits/" + h.String(),
			"html_url": baseURL + "/" + fullName + "/commit/" + h.String(),
		})
	}
	return map[string]interface{}{
		"sha":       sha,
		"node_id":   encodeNodeID("Commit", 0, sha),
		"url":       baseURL + "/api/v3/repos/" + fullName + "/git/commits/" + sha,
		"html_url":  baseURL + "/" + fullName + "/commit/" + sha,
		"author":    author,
		"committer": committer,
		"message":   c.Message,
		"tree": map[string]interface{}{
			"sha": c.TreeHash.String(),
			"url": baseURL + "/api/v3/repos/" + fullName + "/git/trees/" + c.TreeHash.String(),
		},
		"parents":      parents,
		"verification": gitCommitVerificationJSON(c),
	}
}

// --- signature verification ---------------------------------------------
//
// GitHub reports four things about a commit or tag signature: whether it
// verified, why, and — when one is present — the armored signature and the
// payload it covers. bleephub keeps no GPG/SSH keyring, so it can echo a
// signature but cannot check one against a registered key. That is precisely
// the state GitHub calls "unknown_key": the object carries a well-formed
// signature and no key on file matches it. Reporting "unsigned" for a signed
// object (what these renderers did unconditionally) is a false statement about
// the object; reporting "valid" without checking anything would be a worse
// one. Verifying for real needs a key store — GPG keys per user, the
// /user/gpg_keys surface behind it — which is a feature, not a rendering fix,
// so "unknown_key" stays until that exists.

// gitCommitVerificationJSON renders the `verification` member of a commit.
func gitCommitVerificationJSON(c *object.Commit) map[string]interface{} {
	if c == nil || c.PGPSignature == "" {
		return unsignedVerificationJSON()
	}
	payload, err := gitObjectSigningPayload(func(o plumbing.EncodedObject) error {
		return c.EncodeWithoutSignature(o)
	})
	if err != nil {
		return unsignedVerificationJSON()
	}
	return signedVerificationJSON(c.PGPSignature, payload)
}

// gitTagVerificationJSON renders the `verification` member of an annotated tag.
func gitTagVerificationJSON(t *object.Tag) map[string]interface{} {
	if t == nil || t.PGPSignature == "" {
		return unsignedVerificationJSON()
	}
	payload, err := gitObjectSigningPayload(func(o plumbing.EncodedObject) error {
		return t.EncodeWithoutSignature(o)
	})
	if err != nil {
		return unsignedVerificationJSON()
	}
	return signedVerificationJSON(t.PGPSignature, payload)
}

// terminatedSignature returns sig in git's canonical newline-terminated form
// (empty stays empty).
func terminatedSignature(sig string) string {
	if sig == "" || strings.HasSuffix(sig, "\n") {
		return sig
	}
	return sig + "\n"
}

func unsignedVerificationJSON() map[string]interface{} {
	return map[string]interface{}{
		"verified":    false,
		"reason":      "unsigned",
		"signature":   nil,
		"payload":     nil,
		"verified_at": nil,
	}
}

func signedVerificationJSON(signature, payload string) map[string]interface{} {
	return map[string]interface{}{
		"verified":    false,
		"reason":      "unknown_key",
		"signature":   signature,
		"payload":     payload,
		"verified_at": nil,
	}
}

// gitObjectSigningPayload returns the object's canonical text with the
// signature header removed — the bytes the signature is taken over, which is
// what GitHub echoes in verification.payload.
func gitObjectSigningPayload(encode func(plumbing.EncodedObject) error) (string, error) {
	obj := &plumbing.MemoryObject{}
	if err := encode(obj); err != nil {
		return "", err
	}
	reader, err := obj.Reader()
	if err != nil {
		return "", err
	}
	defer reader.Close()
	raw, err := io.ReadAll(reader)
	if err != nil {
		return "", err
	}
	return string(raw), nil
}

func gitTagToJSON(baseURL, fullName, sha string, t *object.Tag) map[string]interface{} {
	return map[string]interface{}{
		"sha":     sha,
		"node_id": encodeNodeID("Tag", 0, sha),
		"url":     baseURL + "/api/v3/repos/" + fullName + "/git/tags/" + sha,
		"tag":     t.Name,
		"message": t.Message,
		"tagger": map[string]interface{}{
			"name":  t.Tagger.Name,
			"email": t.Tagger.Email,
			"date":  t.Tagger.When.Format(time.RFC3339),
		},
		"object": map[string]interface{}{
			"sha":  t.Target.String(),
			"type": objectTypeName(t.TargetType),
			"url":  baseURL + "/api/v3/repos/" + fullName + "/git/" + objectTypeName(t.TargetType) + "s/" + t.Target.String(),
		},
		"verification": gitTagVerificationJSON(t),
	}
}

func objectTypeName(t plumbing.ObjectType) string {
	switch t {
	case plumbing.CommitObject:
		return "commit"
	case plumbing.TreeObject:
		return "tree"
	case plumbing.BlobObject:
		return "blob"
	case plumbing.TagObject:
		return "tag"
	default:
		return "unknown"
	}
}

func encodeBlob(stor gitStorage.Storer, data []byte) (plumbing.Hash, error) {
	obj := stor.NewEncodedObject()
	obj.SetType(plumbing.BlobObject)
	w, err := obj.Writer()
	if err != nil {
		return plumbing.ZeroHash, err
	}
	if _, err := w.Write(data); err != nil {
		_ = w.Close()
		return plumbing.ZeroHash, err
	}
	if err := w.Close(); err != nil {
		return plumbing.ZeroHash, err
	}
	return stor.SetEncodedObject(obj)
}

func encodeCommit(stor gitStorage.Storer, c *object.Commit) (plumbing.Hash, error) {
	obj := stor.NewEncodedObject()
	obj.SetType(plumbing.CommitObject)
	if err := c.Encode(obj); err != nil {
		return plumbing.ZeroHash, err
	}
	return stor.SetEncodedObject(obj)
}

func encodeTag(stor gitStorage.Storer, t *object.Tag) (plumbing.Hash, error) {
	obj := stor.NewEncodedObject()
	obj.SetType(plumbing.TagObject)
	if err := t.Encode(obj); err != nil {
		return plumbing.ZeroHash, err
	}
	return stor.SetEncodedObject(obj)
}
