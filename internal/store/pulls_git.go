package store

import (
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	gitStorage "github.com/go-git/go-git/v5/storage"
)

// Pure git/pull-request resolution helpers shared by the REST handlers and the
// GraphQL resolver layer (ARCH-003).

// ResolveGitRef turns a ref name (branch, tag, full ref, or SHA) into a commit
// hash, returning an error describing the resolution failure.
func ResolveGitRef(stor gitStorage.Storer, ref string) (plumbing.Hash, error) {
	if ref == "" {
		return plumbing.ZeroHash, errors.New("ref is empty")
	}

	// Full reference name.
	if strings.HasPrefix(ref, "refs/") {
		r, err := stor.Reference(plumbing.ReferenceName(ref))
		if err == nil {
			return RefHash(r, stor)
		}
	}

	// The literal HEAD, then branch or tag shorthand. HEAD is a real ref name on
	// github.com resolving to the default branch. An unborn HEAD (empty repo)
	// resolves to nothing and must fall through to not-found below rather than
	// masquerading as an existing ref.
	names := []plumbing.ReferenceName{
		plumbing.NewBranchReferenceName(ref),
		plumbing.NewTagReferenceName(ref),
	}
	if ref == string(plumbing.HEAD) {
		names = append([]plumbing.ReferenceName{plumbing.HEAD}, names...)
	}
	for _, name := range names {
		r, err := stor.Reference(name)
		if err != nil {
			continue
		}
		hash, err := RefHash(r, stor)
		if err != nil {
			continue
		}
		return hash, nil
	}

	// Short or full SHA. NewHash returns ZeroHash on invalid input; the IsZero
	// guard prevents treating garbage as the zero hash.
	h := plumbing.NewHash(ref)
	if !h.IsZero() {
		if _, err := object.GetCommit(stor, h); err == nil {
			return h, nil
		}
	}

	return plumbing.ZeroHash, fmt.Errorf("ref not found: %s", ref)
}

func RefHash(r *plumbing.Reference, stor gitStorage.Storer) (plumbing.Hash, error) {
	if r.Type() == plumbing.SymbolicReference {
		target, err := stor.Reference(r.Target())
		if err != nil {
			return plumbing.ZeroHash, err
		}
		return RefHash(target, stor)
	}
	h := r.Hash()
	if tag, err := object.GetTag(stor, h); err == nil {
		if tag.TargetType != plumbing.CommitObject {
			return plumbing.ZeroHash, fmt.Errorf("tag %s points to %s, not commit", r.Name(), tag.TargetType)
		}
		return tag.Target, nil
	}
	return h, nil
}

// FindMergeBase returns the nearest common ancestor of a and b, or ZeroHash if
// none exists. It walks a's ancestor set, then walks from b until it hits it.
func FindMergeBase(stor gitStorage.Storer, a, b plumbing.Hash) (plumbing.Hash, error) {
	if a == b {
		return a, nil
	}

	ancestors := map[plumbing.Hash]bool{}
	walk := func(start plumbing.Hash) error {
		commit, err := object.GetCommit(stor, start)
		if err != nil {
			return err
		}
		iter := object.NewCommitPreorderIter(commit, nil, nil)
		defer iter.Close()
		return iter.ForEach(func(c *object.Commit) error {
			ancestors[c.Hash] = true
			return nil
		})
	}
	if err := walk(a); err != nil {
		return plumbing.ZeroHash, err
	}

	commit, err := object.GetCommit(stor, b)
	if err != nil {
		return plumbing.ZeroHash, err
	}
	iter := object.NewCommitPreorderIter(commit, nil, nil)
	defer iter.Close()
	found := plumbing.ZeroHash
	_ = iter.ForEach(func(c *object.Commit) error {
		if ancestors[c.Hash] {
			found = c.Hash
			return errors.New("stop")
		}
		return nil
	})
	return found, nil
}

// CommitsBetween returns the commits reachable from head but not base, newest
// first. If base is not an ancestor of head, the result is head's full history.
func CommitsBetween(stor gitStorage.Storer, base, head plumbing.Hash) ([]*object.Commit, error) {
	baseCommit, err := object.GetCommit(stor, base)
	if err != nil {
		return nil, err
	}
	exclude := map[plumbing.Hash]bool{baseCommit.Hash: true}
	iter := object.NewCommitPreorderIter(baseCommit, nil, nil)
	_ = iter.ForEach(func(c *object.Commit) error {
		exclude[c.Hash] = true
		return nil
	})
	iter.Close()

	headCommit, err := object.GetCommit(stor, head)
	if err != nil {
		return nil, err
	}
	iter = object.NewCommitPreorderIter(headCommit, nil, nil)
	defer iter.Close()

	var commits []*object.Commit
	_ = iter.ForEach(func(c *object.Commit) error {
		if exclude[c.Hash] {
			return nil
		}
		commits = append(commits, c)
		return nil
	})
	return commits, nil
}

// PullRequestCommitObjectsFromStorage lists the PR's commits (oldest first):
// those reachable from head but not from the merge base with the base branch.
func PullRequestCommitObjectsFromStorage(stor gitStorage.Storer, pr *PullRequest) ([]*object.Commit, error) {
	headHash, err := ResolveGitRef(stor, pr.HeadRefName)
	if err != nil {
		return nil, nil
	}
	var baseHash plumbing.Hash
	if pr.BaseSHA != "" {
		baseHash = plumbing.NewHash(pr.BaseSHA)
	} else {
		baseHash, err = ResolveGitRef(stor, pr.BaseRefName)
		if err != nil {
			return nil, nil
		}
	}
	mergeBase, err := FindMergeBase(stor, baseHash, headHash)
	if err != nil {
		return nil, err
	}
	if mergeBase.IsZero() {
		return nil, nil
	}
	if mergeBase == headHash {
		return nil, nil
	}
	commits, err := CommitsBetween(stor, mergeBase, headHash)
	if err != nil {
		return nil, err
	}
	// Reverse to oldest-first, which is what the API lists.
	for i, j := 0, len(commits)-1; i < j; i, j = i+1, j-1 {
		commits[i], commits[j] = commits[j], commits[i]
	}
	return commits, nil
}

// PullRequestHeadRepoID is the repository the PR's head branch lives in — the
// fork for cross-repository PRs, the base repository otherwise.
func PullRequestHeadRepoID(pr *PullRequest) int {
	if pr == nil {
		return 0
	}
	if pr.HeadRepoID != 0 {
		return pr.HeadRepoID
	}
	return pr.RepoID
}

// PullRequestHeadRepo resolves the PR's head repository (locking).
func PullRequestHeadRepo(st *Store, pr *PullRequest) *Repo {
	st.Mu.RLock()
	defer st.Mu.RUnlock()
	return PullRequestHeadRepoLocked(st, pr)
}

// PullRequestHeadRepoLocked resolves the PR's head repository; the caller
// holds st.Mu.
func PullRequestHeadRepoLocked(st *Store, pr *PullRequest) *Repo {
	if pr == nil {
		return nil
	}
	return st.Repos[PullRequestHeadRepoID(pr)]
}

// ResolvePullRequestHead resolves a PR head spec ("branch" or "owner:branch")
// to the repository holding that branch within the base repository's fork
// network, and the branch name.
func ResolvePullRequestHead(st *Store, baseRepo *Repo, head string) (*Repo, string) {
	if baseRepo == nil || strings.TrimSpace(head) == "" {
		return nil, ""
	}
	ownerLogin := ""
	branch := head
	if idx := strings.Index(head, ":"); idx >= 0 {
		ownerLogin = head[:idx]
		branch = head[idx+1:]
	}
	if branch == "" {
		return nil, ""
	}
	if ownerLogin == "" || (baseRepo.Owner != nil && ownerLogin == baseRepo.Owner.Login) {
		return baseRepo, branch
	}

	st.Mu.RLock()
	defer st.Mu.RUnlock()
	var matches []*Repo
	networkSourceID := baseRepo.ID
	if baseRepo.SourceID != 0 {
		networkSourceID = baseRepo.SourceID
	}
	for _, repo := range st.Repos {
		if repo == nil || repo.Owner == nil || repo.Owner.Login != ownerLogin {
			continue
		}
		if repo.ID == baseRepo.ID {
			matches = append(matches, repo)
			continue
		}
		sourceID := repo.SourceID
		if sourceID == 0 {
			sourceID = repo.ID
		}
		if repo.ParentID == baseRepo.ID || sourceID == networkSourceID {
			matches = append(matches, repo)
		}
	}
	if len(matches) == 1 {
		return matches[0], branch
	}
	for _, repo := range matches {
		if repo.Name == baseRepo.Name {
			return repo, branch
		}
	}
	return nil, ""
}

// PullRequestGitStorage returns the git storage holding the PR's head branch
// plus that repository's full name.
func PullRequestGitStorage(st *Store, repo *Repo, pr *PullRequest) (gitStorage.Storer, string) {
	if repo == nil || pr == nil {
		return nil, ""
	}
	headRepo := PullRequestHeadRepo(st, pr)
	if headRepo == nil {
		return nil, ""
	}
	owner, name, ok := SplitRepoFullName(headRepo.FullName)
	if !ok {
		return nil, ""
	}
	return st.GetGitStorage(owner, name), headRepo.FullName
}

// PullRequestHeadSHALocked resolves the PR head branch's current commit SHA;
// the caller holds st.Mu.
func PullRequestHeadSHALocked(pr *PullRequest, st *Store) string {
	if pr == nil {
		return ""
	}
	repo := PullRequestHeadRepoLocked(st, pr)
	if repo == nil {
		return ""
	}
	return ResolveBranchSha(st.GitStorages[repo.FullName], pr.HeadRefName)
}

// PRReviewThreadNodeID renders the GraphQL node id of a PR review thread. Node-ID
// codecs live in store so both API surfaces share one format (ARCH-003).
func PRReviewThreadNodeID(threadID int) string {
	return fmt.Sprintf("PRT_kgDO%08d", threadID)
}

// ParsePRReviewThreadNodeID decodes a PR review thread node id.
func ParsePRReviewThreadNodeID(nodeID string) (int, bool) {
	const prefix = "PRT_kgDO"
	if !strings.HasPrefix(nodeID, prefix) {
		return 0, false
	}
	id, err := strconv.Atoi(strings.TrimPrefix(nodeID, prefix))
	if err != nil || id <= 0 {
		return 0, false
	}
	return id, true
}
