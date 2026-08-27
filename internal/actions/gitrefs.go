package actions

// Git-plumbing helpers over a go-git Storer: resolve refs to commit shas and
// read workflow definitions from git storage.

import (
	"io"
	"strings"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	gitStorage "github.com/go-git/go-git/v5/storage"
)

// ZeroCommitSha is returned when a ref names no commit.
const ZeroCommitSha = "0000000000000000000000000000000000000000"

// ResolveRefSha resolves the commit sha ref points at; empty ref means HEAD.
// A non-empty ref must resolve exactly — never substitute a different commit.
func ResolveRefSha(stor gitStorage.Storer, ref string) string {
	resolve := func(name plumbing.ReferenceName) (plumbing.Hash, bool) {
		r, err := stor.Reference(name)
		if err != nil {
			return plumbing.Hash{}, false
		}
		if r.Type() == plumbing.SymbolicReference {
			target, err := stor.Reference(r.Target())
			if err != nil {
				return plumbing.Hash{}, false
			}
			return target.Hash(), true
		}
		return r.Hash(), true
	}
	if ref != "" {
		if h, ok := resolve(plumbing.ReferenceName(ref)); ok && !h.IsZero() {
			return h.String()
		}
		return ZeroCommitSha
	}
	if h, ok := resolve(plumbing.HEAD); ok && !h.IsZero() {
		return h.String()
	}
	return ZeroCommitSha
}

// SplitRepoKeyParts splits an "owner/name" repo key at its first slash.
func SplitRepoKeyParts(repoKey string) [2]string {
	for i, c := range repoKey {
		if c == '/' {
			return [2]string{repoKey[:i], repoKey[i+1:]}
		}
	}
	return [2]string{repoKey, ""}
}

// ListWorkflowFilesAtRef reads .github/workflows as of ref; empty ref means HEAD.
func ListWorkflowFilesAtRef(stor gitStorage.Storer, ref string) map[string][]byte {
	sha := ResolveRefSha(stor, ref)
	if sha == ZeroCommitSha {
		return nil
	}
	commitHash := plumbing.NewHash(sha)

	commit, err := object.GetCommit(stor, commitHash)
	if err != nil {
		return nil
	}

	tree, err := commit.Tree()
	if err != nil {
		return nil
	}

	ghEntry, err := tree.FindEntry(".github")
	if err != nil {
		return nil
	}
	ghTree, err := object.GetTree(stor, ghEntry.Hash)
	if err != nil {
		return nil
	}
	wfEntry, err := ghTree.FindEntry("workflows")
	if err != nil {
		return nil
	}
	wfTree, err := object.GetTree(stor, wfEntry.Hash)
	if err != nil {
		return nil
	}

	result := make(map[string][]byte)
	for _, entry := range wfTree.Entries {
		if !entry.Mode.IsFile() {
			continue
		}
		if !strings.HasSuffix(entry.Name, ".yml") && !strings.HasSuffix(entry.Name, ".yaml") {
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
		result[entry.Name] = content
	}
	return result
}
