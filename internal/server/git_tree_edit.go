package bleephub

import (
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/plumbing/object"
	gitStorage "github.com/go-git/go-git/v5/storage"
)

// A commit the API makes — a contents write or delete, a multi-file commit, a
// repository's first commit — is its parent's tree with a few paths changed.
// It is built by editing that tree: the blobs written, and every tree on the
// way from the root to a changed path rewritten, and nothing else read or
// written. It used to be built through a go-git worktree, which checked the
// whole repository out into memory, staged the change against it and wrote a
// staging index back to the repository, so that one small write cost what the
// repository held.

// errTreeEditPath refuses an edit whose path cannot be one (validTreeEditPath),
// or that runs through a file.
var errTreeEditPath = errors.New("invalid path")

// errTreeEditMissing refuses the deletion of a path the tree does not hold.
var errTreeEditMissing = errors.New("path does not exist")

// treeEdits are the changes to one tree: the entries of its own set to new
// content or removed (nil), and the edits of the trees below it.
type treeEdits struct {
	files map[string]*[]byte
	dirs  map[string]*treeEdits
}

func (e *treeEdits) under(name string) *treeEdits {
	if e.dirs == nil {
		e.dirs = map[string]*treeEdits{}
	}
	if e.dirs[name] == nil {
		e.dirs[name] = &treeEdits{}
	}
	return e.dirs[name]
}

// plan turns paths into edits. A deletion of a path also written is dropped:
// the write stands.
func planTreeEdits(additions map[string][]byte, deletions []string) (*treeEdits, error) {
	root := &treeEdits{}
	place := func(path string, content *[]byte) error {
		if err := validTreeEditPath(path); err != nil {
			return err
		}
		parts := strings.Split(path, "/")
		edits := root
		for _, part := range parts[:len(parts)-1] {
			if _, file := edits.files[part]; file {
				return fmt.Errorf("%w: %q runs through a file the same commit writes", errTreeEditPath, path)
			}
			edits = edits.under(part)
		}
		name := parts[len(parts)-1]
		if _, dir := edits.dirs[name]; dir {
			return fmt.Errorf("%w: %q is a directory the same commit writes into", errTreeEditPath, path)
		}
		if edits.files == nil {
			edits.files = map[string]*[]byte{}
		}
		if existing, set := edits.files[name]; set && existing != nil && content == nil {
			return nil
		}
		edits.files[name] = content
		return nil
	}
	for path, body := range additions {
		content := body
		if err := place(path, &content); err != nil {
			return nil, err
		}
	}
	for _, path := range deletions {
		if err := place(path, nil); err != nil {
			return nil, err
		}
	}
	return root, nil
}

// validTreeEditPath refuses a path git would not let into a tree: empty
// components, "." and "..", control characters, and ".git" in any of its
// disguises (HFS+ ignorables, NTFS short names and trailing dots) at any
// depth — an entry named so is an attack on whoever checks the tree out. The
// rules are git's verify_path as go-git carries them, which it exposes only by
// running them first in Tree.FindEntry: asked of an empty tree, a path it would
// accept is merely not found.
func validTreeEditPath(path string) error {
	for _, part := range strings.Split(path, "/") {
		if part == "" {
			return fmt.Errorf("%w: %q", errTreeEditPath, path)
		}
	}
	_, err := (&object.Tree{}).FindEntry(path)
	if errors.Is(err, object.ErrEntryNotFound) || errors.Is(err, object.ErrDirectoryNotFound) {
		return nil
	}
	return fmt.Errorf("%w: %w", errTreeEditPath, err)
}

// editTree applies edits to the tree base (the empty tree when zero) and
// returns the new tree's id, or zero when nothing is left in it — git keeps no
// empty directories. at is the path of the tree, for errors.
func editTree(stor gitStorage.Storer, base plumbing.Hash, edits *treeEdits, at string) (plumbing.Hash, error) {
	entries := map[string]object.TreeEntry{}
	if !base.IsZero() {
		tree, err := object.GetTree(stor, base)
		if err != nil {
			return plumbing.ZeroHash, err
		}
		for _, entry := range tree.Entries {
			entries[entry.Name] = entry
		}
	}
	for name, content := range edits.files {
		existing, exists := entries[name]
		if content == nil {
			if !exists {
				return plumbing.ZeroHash, fmt.Errorf("%w: %s", errTreeEditMissing, at+name)
			}
			delete(entries, name)
			continue
		}
		mode := filemode.Regular
		if exists {
			switch existing.Mode {
			case filemode.Dir, filemode.Submodule:
				return plumbing.ZeroHash, fmt.Errorf("%w: %s is a %s, not a file", errTreeEditPath, at+name, existing.Mode)
			case filemode.Executable:
				// Rewriting a file keeps it executable, as writing to it on a
				// disk does.
				mode = filemode.Executable
			}
		}
		blob, err := encodeBlob(stor, *content)
		if err != nil {
			return plumbing.ZeroHash, err
		}
		entries[name] = object.TreeEntry{Name: name, Mode: mode, Hash: blob}
	}
	for name, below := range edits.dirs {
		existing, exists := entries[name]
		var subtree plumbing.Hash
		if exists {
			if existing.Mode != filemode.Dir {
				return plumbing.ZeroHash, fmt.Errorf("%w: %s is a file", errTreeEditPath, at+name)
			}
			subtree = existing.Hash
		}
		edited, err := editTree(stor, subtree, below, at+name+"/")
		if err != nil {
			return plumbing.ZeroHash, err
		}
		if edited.IsZero() {
			delete(entries, name)
			continue
		}
		entries[name] = object.TreeEntry{Name: name, Mode: filemode.Dir, Hash: edited}
	}
	if len(entries) == 0 {
		return plumbing.ZeroHash, nil
	}
	tree := &object.Tree{Entries: make([]object.TreeEntry, 0, len(entries))}
	for _, entry := range entries {
		tree.Entries = append(tree.Entries, entry)
	}
	// git's order: by name, a directory's name compared as if it ended in "/".
	sortKey := func(entry object.TreeEntry) string {
		if entry.Mode == filemode.Dir {
			return entry.Name + "/"
		}
		return entry.Name
	}
	sort.Slice(tree.Entries, func(i, j int) bool { return sortKey(tree.Entries[i]) < sortKey(tree.Entries[j]) })
	encoded := stor.NewEncodedObject()
	if err := tree.Encode(encoded); err != nil {
		return plumbing.ZeroHash, err
	}
	return stor.SetEncodedObject(encoded)
}

// commitTreeEdits writes a commit of the edits onto parent (none for a root
// commit) and returns it; it moves no reference.
func commitTreeEdits(stor gitStorage.Storer, parent plumbing.Hash, additions map[string][]byte, deletions []string, message string, sig *object.Signature) (plumbing.Hash, error) {
	edits, err := planTreeEdits(additions, deletions)
	if err != nil {
		return plumbing.ZeroHash, err
	}
	var base plumbing.Hash
	var parents []plumbing.Hash
	if !parent.IsZero() {
		commit, err := object.GetCommit(stor, parent)
		if err != nil {
			return plumbing.ZeroHash, err
		}
		base, parents = commit.TreeHash, []plumbing.Hash{parent}
	}
	tree, err := editTree(stor, base, edits, "")
	if err != nil {
		return plumbing.ZeroHash, err
	}
	if tree.IsZero() {
		// A commit of nothing is a commit of the empty tree.
		empty := stor.NewEncodedObject()
		if err := (&object.Tree{}).Encode(empty); err != nil {
			return plumbing.ZeroHash, err
		}
		if tree, err = stor.SetEncodedObject(empty); err != nil {
			return plumbing.ZeroHash, err
		}
	}
	return encodeCommit(stor, &object.Commit{
		Author:       *sig,
		Committer:    *sig,
		Message:      message,
		TreeHash:     tree,
		ParentHashes: parents,
	})
}

// commitBranchEdits commits the edits onto branch, which must be at
// expectedParent when that is not zero, runs guard on the commit, and moves the
// branch to it only if the branch is still where it was read.
func commitBranchEdits(stor gitStorage.Storer, branch string, additions map[string][]byte, deletions []string,
	message string, sig *object.Signature, expectedParent plumbing.Hash, guard func(plumbing.Hash) error) (plumbing.Hash, error) {
	branchRef := plumbing.NewBranchReferenceName(branch)
	ref, err := stor.Reference(branchRef)
	if err != nil {
		return plumbing.ZeroHash, fmt.Errorf("resolve branch %s: %w", branch, err)
	}
	if !expectedParent.IsZero() && ref.Hash() != expectedParent {
		return plumbing.ZeroHash, gitStorage.ErrReferenceHasChanged
	}
	commitHash, err := commitTreeEdits(stor, ref.Hash(), additions, deletions, message, sig)
	if err != nil {
		return plumbing.ZeroHash, err
	}
	if guard != nil {
		if err := guard(commitHash); err != nil {
			return plumbing.ZeroHash, err
		}
	}
	if err := stor.CheckAndSetReference(plumbing.NewHashReference(branchRef, commitHash), ref); err != nil {
		return plumbing.ZeroHash, fmt.Errorf("set ref: %w", err)
	}
	return commitHash, nil
}
