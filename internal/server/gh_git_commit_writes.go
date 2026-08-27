package bleephub

// Git writes that build a commit rather than move a reference. They live here,
// not in the resolver layer, because they reach the git storer and push
// machinery (ARCH-003), and advance their branch through the same
// compare-and-set the single-file contents commit uses — an unconditional ref
// write would reopen the lost-update window that helper closes.

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/go-git/go-billy/v5/memfs"
	git "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	gitStorage "github.com/go-git/go-git/v5/storage"

	"github.com/e6qu/bleephub/internal/store"
)

// multiFileCommit writes one commit with the named additions and deletions onto
// branch and returns its hash. A non-zero expectedParent is the head the caller
// saw; a branch that moved since is refused rather than overwritten.
func multiFileCommit(stor gitStorage.Storer, branch string, additions map[string][]byte, deletions []string,
	message string, sig *object.Signature, expectedParent plumbing.Hash) (plumbing.Hash, error) {
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
	for path, body := range additions {
		if err := writeFileToWorktree(fs, wt, path, string(body)); err != nil {
			return plumbing.ZeroHash, err
		}
	}
	for _, path := range deletions {
		if _, err := fs.Stat(path); err != nil {
			return plumbing.ZeroHash, fmt.Errorf("path does not exist: %s", path)
		}
		if _, err := wt.Remove(path); err != nil {
			return plumbing.ZeroHash, fmt.Errorf("git remove %s: %w", path, err)
		}
	}
	commitHash, err := wt.Commit(message, &git.CommitOptions{
		Author:    sig,
		Committer: sig,
		Parents:   []plumbing.Hash{parentHash},
	})
	if err != nil {
		return plumbing.ZeroHash, fmt.Errorf("commit: %w", err)
	}
	if err := repo.Storer.CheckAndSetReference(plumbing.NewHashReference(branchRef, commitHash), ref); err != nil {
		return plumbing.ZeroHash, fmt.Errorf("set ref: %w", err)
	}
	return commitHash, nil
}

// createCommitOnBranch runs the branch-protection refusal, the commit, the
// secret-scanning scan, and the push machinery, in the order a ref write does.
func (s *Server) createCommitOnBranch(ctx context.Context, repo *store.Repo, stor gitStorage.Storer, sender *store.User,
	branch, expectedHeadOid string, additions map[string][]byte, deletions []string, message, baseURL string) (plumbing.Hash, *gitRefWriteFailure) {
	fullRef := plumbing.NewBranchReferenceName(branch)
	oldRef, err := stor.Reference(fullRef)
	if err != nil {
		return plumbing.ZeroHash, &gitRefWriteFailure{status: http.StatusUnprocessableEntity, message: "Reference does not exist"}
	}
	expected := plumbing.NewHash(expectedHeadOid)
	if oldRef.Hash() != expected {
		return plumbing.ZeroHash, &gitRefWriteFailure{status: http.StatusConflict,
			message: "Expected branch to point to \"" + expectedHeadOid + "\" but it did not"}
	}
	if refusal := s.protectedRefWriteRefusal(ctx, repo, stor, fullRef, refFastForward, expected); refusal != "" {
		return plumbing.ZeroHash, &gitRefWriteFailure{status: http.StatusForbidden, message: refusal}
	}
	email := sender.Email
	if email == "" {
		email = sender.Login + "@users.noreply.bleephub.local"
	}
	sig := repoSignature(store.CoalesceStr(sender.Name, sender.Login), email)
	commitHash, err := multiFileCommit(stor, branch, additions, deletions, message, sig, expected)
	if err != nil {
		if errors.Is(err, gitStorage.ErrReferenceHasChanged) {
			return plumbing.ZeroHash, &gitRefWriteFailure{status: http.StatusConflict,
				message: "the branch moved while the commit was being prepared"}
		}
		return plumbing.ZeroHash, &gitRefWriteFailure{status: http.StatusUnprocessableEntity, message: err.Error()}
	}
	if err := s.scanRefForSecretScanning(repo, stor, fullRef, commitHash, baseURL); err != nil {
		return plumbing.ZeroHash, &gitRefWriteFailure{status: http.StatusInternalServerError, message: err.Error()}
	}
	s.afterCommittedRefUpdate(repo, sender, fullRef.String(), expected.String(), commitHash.String(), baseURL)
	return commitHash, nil
}

// mergeBranchRefs merges head into base, returning the merge commit's hash, or
// the zero hash when head was already an ancestor of base.
func (s *Server) mergeBranchRefs(repo *store.Repo, sender *store.User, base, head, commitMessage, authorEmail string) (plumbing.Hash, *gitRefWriteFailure) {
	owner, name, ok := store.SplitRepoFullName(repo.FullName)
	if !ok {
		return plumbing.ZeroHash, &gitRefWriteFailure{status: http.StatusNotFound, message: "Not Found"}
	}
	stor := s.store.GetGitStorage(owner, name)
	if stor == nil {
		return plumbing.ZeroHash, &gitRefWriteFailure{status: http.StatusNotFound, message: "Not Found"}
	}
	headHash, err := store.ResolveGitRef(stor, head)
	if err != nil {
		return plumbing.ZeroHash, &gitRefWriteFailure{status: http.StatusNotFound, message: "Not Found"}
	}
	baseRef := plumbing.NewBranchReferenceName(base)
	baseRefObj, err := stor.Reference(baseRef)
	if err != nil {
		return plumbing.ZeroHash, &gitRefWriteFailure{status: http.StatusNotFound, message: "Not Found"}
	}
	mergeBase, err := store.FindMergeBase(stor, baseRefObj.Hash(), headHash)
	if err != nil {
		return plumbing.ZeroHash, &gitRefWriteFailure{status: http.StatusInternalServerError, message: "merge base lookup failed"}
	}
	if mergeBase == headHash {
		// Already merged: GitHub answers 204 with no merge commit.
		return plumbing.ZeroHash, nil
	}
	email := authorEmail
	if email == "" {
		email = sender.Email
	}
	if email == "" {
		email = sender.Login + "@users.noreply.bleephub.local"
	}
	sig := repoSignature(store.CoalesceStr(sender.Name, sender.Login), email)
	commitHash, _, err := performMerge(stor, baseRef, headHash, head, commitMessage, sig)
	if err != nil {
		switch {
		case errors.Is(err, gitStorage.ErrReferenceHasChanged):
			return plumbing.ZeroHash, &gitRefWriteFailure{status: http.StatusConflict, message: "Base branch changed while the merge was being prepared"}
		case strings.Contains(err.Error(), "merge conflict"):
			return plumbing.ZeroHash, &gitRefWriteFailure{status: http.StatusConflict, message: "Merge conflict"}
		}
		return plumbing.ZeroHash, &gitRefWriteFailure{status: http.StatusInternalServerError, message: "Merge failed"}
	}
	s.store.UpdateRepo(owner, name, func(r *store.Repo) {
		r.PushedAt = s.currentTime()
	})
	return commitHash, nil
}

// createRevertBranch creates the branch whose commit undoes a merged pull
// request and returns its name. The change to undo is the merge commit's first
// parent tree against its own; the revert restores those paths to their
// pre-merge content atop the current base branch.
func (s *Server) createRevertBranch(ctx context.Context, repo *store.Repo, pr *store.PullRequest, sender *store.User, baseURL string) (string, error) {
	if pr.State != "MERGED" || pr.MergeCommitSHA == "" {
		return "", fmt.Errorf("only a merged pull request can be reverted")
	}
	owner, name, ok := store.SplitRepoFullName(repo.FullName)
	if !ok {
		//lint:ignore ST1005 GitHub API parity requires this exact upstream message.
		return "", fmt.Errorf("Repository name is invalid")
	}
	stor := s.store.GetGitStorage(owner, name)
	if stor == nil {
		return "", fmt.Errorf("the repository has no git storage")
	}
	merge, err := object.GetCommit(stor, plumbing.NewHash(pr.MergeCommitSHA))
	if err != nil {
		return "", fmt.Errorf("the merge commit is missing from git storage")
	}
	if merge.NumParents() == 0 {
		return "", fmt.Errorf("the merge commit has no parent to revert to")
	}
	before, err := merge.Parent(0)
	if err != nil {
		return "", fmt.Errorf("the merge commit's parent is missing from git storage")
	}
	beforeTree, err := before.Tree()
	if err != nil {
		return "", fmt.Errorf("the pre-merge tree is missing from git storage")
	}
	afterTree, err := merge.Tree()
	if err != nil {
		return "", fmt.Errorf("the merged tree is missing from git storage")
	}
	changes, err := object.DiffTree(beforeTree, afterTree)
	if err != nil {
		return "", fmt.Errorf("the merged change could not be read")
	}
	additions := map[string][]byte{}
	var deletions []string
	for _, change := range changes {
		from, to, err := change.Files()
		if err != nil {
			return "", fmt.Errorf("the merged change could not be read")
		}
		switch {
		case from != nil:
			// Existed before the merge: restore its old content.
			contents, err := from.Contents()
			if err != nil {
				return "", fmt.Errorf("the pre-merge content of %s could not be read", from.Name)
			}
			additions[from.Name] = []byte(contents)
			if to != nil && to.Name != from.Name {
				deletions = append(deletions, to.Name)
			}
		case to != nil:
			// Introduced by the merge: remove it.
			deletions = append(deletions, to.Name)
		}
	}

	baseRef := plumbing.NewBranchReferenceName(pr.BaseRefName)
	baseHead, err := stor.Reference(baseRef)
	if err != nil {
		return "", fmt.Errorf("the base branch no longer exists")
	}
	branch := fmt.Sprintf("revert-%d-%s", pr.Number, pr.HeadRefName)
	// Create through the same helper POST /git/refs uses, so branch and push
	// protection apply.
	if failure := s.createGitRef(ctx, repo, stor, sender,
		plumbing.NewBranchReferenceName(branch), baseHead.Hash(), baseURL); failure != nil {
		return "", errors.New(failure.message)
	}
	message := fmt.Sprintf("Revert %q", pr.Title)
	if _, failure := s.createCommitOnBranch(ctx, repo, stor, sender, branch, baseHead.Hash().String(),
		additions, deletions, message, baseURL); failure != nil {
		return "", errors.New(failure.message)
	}
	return branch, nil
}
