package store

import (
	"context"
	"errors"
	"fmt"
	"path"
	"sort"
	"strings"

	"github.com/e6qu/bleephub/internal/gitstore"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/plumbing/object"
	gitStorage "github.com/go-git/go-git/v5/storage"
)

// A repository's wiki IS a git repository on github, reachable at
// `<owner>/<repo>.wiki.git` and at nothing else — there is no REST API for wiki
// content, which is why the vendored contract is silent about it. This file is
// that repository: the pages the browser UI reads are a projection of its tip
// commit, and every write the UI makes is a commit on it.
//
// One source of truth, not two. The pages map below is a memo of the pure
// function project(tip) → pages, stamped with the tip it was computed from, and
// every read re-derives it when the tip has moved. A `git push` therefore cannot
// leave the UI stale — the push moves the ref, the next read sees a tip it has
// no memo for, and rebuilds — and a UI edit cannot leave a clone stale, because
// the edit is a commit before it is anything else. There is no writer that
// updates one and not the other, so there is no state in which they can
// disagree; a memo that is out of date is *detected* by comparing object ids
// rather than invalidated by a writer that has to remember to.

// WikiStorageSuffix is appended to a repository's full name to key its wiki's
// git storage: `admin/docs` stores its wiki under `admin/docs.wiki.git`.
//
// The suffix carries `.git` deliberately. Repository names ending in `.git` are
// refused at creation (isValidNewRepoName), so no repository can ever own this
// key — where a bare `.wiki` suffix would collide with a repository someone
// legitimately named `docs.wiki` and silently serve one repository's objects as
// another's. It is also the shape of the URL clients already use, so the
// on-disk layout and the S3 prefix read the way the remote does, and the wiki
// gets the same pluggable storer, packing, compaction and caching as any other
// repository rather than a second storage tier.
const WikiStorageSuffix = ".wiki.git"

// WikiURLSuffix is the repository-name suffix a git client addresses a wiki
// with, after the transport has trimmed the trailing `.git`.
const WikiURLSuffix = ".wiki"

// WikiStorageName maps a repository's full name to its wiki's storage key.
func WikiStorageName(repoKey string) string { return repoKey + WikiStorageSuffix }

// wikiMarkupExtensions are the file extensions github renders as wiki pages.
// A file in the wiki repository with any other extension (an image, a CSS file,
// a _Sidebar asset) is content the wiki carries but not a page, so it round
// trips through clone and push untouched and never appears in the page list.
var wikiMarkupExtensions = []string{
	".md", ".markdown", ".mdown", ".mkdn",
	".asciidoc", ".adoc", ".asc",
	".creole",
	".mediawiki", ".wiki",
	".org",
	".pod",
	".rdoc",
	".rest", ".rst",
	".textile",
	".texi", ".texinfo",
	".txt",
}

// WikiPageExtension is the extension a page created through the UI is written
// with; github's own editor writes markdown too.
const WikiPageExtension = ".md"

// maxWikiHistoryCommits bounds the first-parent walk that derives page
// timestamps and revisions from the wiki's history.
const maxWikiHistoryCommits = 5000

// WikiPageFileName maps a page title to its file name in the wiki repository,
// the way github does: spaces become hyphens and the markup extension is
// appended, so "Getting Started" is `Getting-Started.md`. Path separators are
// folded into hyphens too — a title never creates a directory, because the
// title is the identity and a title that escaped into a subdirectory would no
// longer round trip back to itself.
func WikiPageFileName(title string) string {
	return WikiPageName(title) + WikiPageExtension
}

// WikiPageName is WikiPageFileName without the extension: the hyphenated name
// github addresses a page by in `/{owner}/{repo}/wiki/{Page-Name}`.
func WikiPageName(title string) string {
	var b strings.Builder
	prevHyphen := false
	for _, r := range strings.TrimSpace(title) {
		switch r {
		case ' ', '\t', '/', '\\', ':', '*', '?', '"', '<', '>', '|':
			if !prevHyphen && b.Len() > 0 {
				b.WriteByte('-')
				prevHyphen = true
			}
		default:
			b.WriteRune(r)
			prevHyphen = false
		}
	}
	return strings.Trim(b.String(), "-")
}

// WikiTitleFromPath is the inverse mapping: the page title a wiki file's name
// carries, with hyphens read back as spaces. A page is a markup file at the
// repository root, where the mapping is a bijection and a title therefore round
// trips through its file name and back to itself. Everything else the wiki
// repository carries — an image, a stylesheet, a file in a subdirectory — is
// content it stores and serves without being a page, and reports false here.
func WikiTitleFromPath(filePath string) (string, bool) {
	if filePath == "" || strings.Contains(filePath, "/") {
		return "", false
	}
	ext := strings.ToLower(path.Ext(filePath))
	known := false
	for _, candidate := range wikiMarkupExtensions {
		if ext == candidate {
			known = true
			break
		}
	}
	if !known {
		return "", false
	}
	name := strings.TrimSuffix(filePath, path.Ext(filePath))
	if name == "" {
		return "", false
	}
	return strings.ReplaceAll(name, "-", " "), true
}

// WikiProjection is the wiki repository's tip commit read as pages: the
// derived value the browser UI is served, together with the object id it was
// derived from. A projection whose Tip is not the wiki's current tip is stale
// by construction and is thrown away rather than patched.
type WikiProjection struct {
	Tip       plumbing.Hash
	Branch    string
	Pages     map[string]*WikiPage
	Revisions map[string][]*WikiPageRevision
}

// WikiPageChange is one page a wiki commit range created, edited or deleted —
// what the gollum webhook reports.
type WikiPageChange struct {
	Slug     string
	Title    string
	PageName string
	Action   string
	SHA      string
}

// openWikiStorage opens or initializes the wiki's git storage through the same
// pluggable storer ordinary repositories use, so a wiki lands on the S3-backed
// object store, the local git directory or memory exactly as its repository
// does. Caller must hold WikiMu.
func (st *Store) openWikiStorageLocked(repoKey, defaultBranch string) gitStorage.Storer { //nolint:ireturn
	if stor, ok := st.WikiGitStorages[repoKey]; ok {
		return stor
	}
	open := st.RepoStorageOpen
	if open == nil {
		open = gitstore.OpenOrInitGitStorage
	}
	stor, err := open(context.Background(), WikiStorageName(repoKey))
	if err != nil {
		st.Logger.Error().Str("repo", repoKey).Err(err).Msg("open wiki git storage failed")
		return nil
	}
	if err := SetGitHeadBranch(stor, defaultBranch); err != nil {
		st.Logger.Error().Str("repo", repoKey).Err(err).Msg("could not point the wiki HEAD at its default branch")
	}
	st.WikiGitStorages[repoKey] = stor
	return stor
}

// wikiRepoDefaults reads the canonical repository key and the branch a fresh
// wiki should be created on, taking and releasing the store lock before any git
// I/O begins so a slow object-store round trip never holds it.
func (st *Store) wikiRepoDefaults(repoKey string) (string, string) {
	st.Mu.RLock()
	defer st.Mu.RUnlock()
	repoKey = st.canonicalRepoKeyLocked(repoKey)
	branch := "master"
	if repo := st.RepoByNameLocked(repoKey); repo != nil && repo.DefaultBranch != "" {
		branch = repo.DefaultBranch
	}
	return repoKey, branch
}

// WikiGitStorage is the storer both git transports serve a wiki from. It opens
// the wiki repository on first use, which is what lets a client push to the
// wiki of a repository whose wiki nobody has touched yet.
func (st *Store) WikiGitStorage(repoKey string) gitStorage.Storer { //nolint:ireturn
	repoKey, branch := st.wikiRepoDefaults(repoKey)
	st.WikiMu.Lock()
	defer st.WikiMu.Unlock()
	return st.openWikiStorageLocked(repoKey, branch)
}

// WikiHeadBranch is the branch a clone of the wiki checks out.
func (st *Store) WikiHeadBranch(repoKey string) string {
	repoKey, fallback := st.wikiRepoDefaults(repoKey)
	st.WikiMu.Lock()
	defer st.WikiMu.Unlock()
	return wikiHeadBranchLocked(st.openWikiStorageLocked(repoKey, fallback), fallback)
}

// DropWikiGitStorage forgets a repository's wiki handle and projection. The
// bytes are removed by the caller that removes the repository's own storage.
func (st *Store) DropWikiGitStorage(repoKey string) {
	st.WikiMu.Lock()
	defer st.WikiMu.Unlock()
	delete(st.WikiGitStorages, repoKey)
	delete(st.WikiProjections, repoKey)
}

// RekeyWikiGitStorage follows a repository rename: the wiki handle and its
// projection are dropped so the next access reopens them against the new
// storage key. The bytes themselves are moved by MoveWikiGitStorageBytes.
func (st *Store) RekeyWikiGitStorage(oldKey, newKey string) {
	st.WikiMu.Lock()
	defer st.WikiMu.Unlock()
	delete(st.WikiGitStorages, oldKey)
	delete(st.WikiProjections, oldKey)
	delete(st.WikiGitStorages, newKey)
	delete(st.WikiProjections, newKey)
}

// wikiHeadBranchLocked reports the branch the wiki's HEAD names, falling back
// to the repository's default branch when HEAD is missing or detached.
func wikiHeadBranchLocked(stor gitStorage.Storer, fallback string) string {
	if stor == nil {
		return fallback
	}
	ref, err := stor.Reference(plumbing.HEAD)
	if err == nil && ref.Type() == plumbing.SymbolicReference && ref.Target().IsBranch() {
		return ref.Target().Short()
	}
	return fallback
}

// RepairWikiHead points a wiki's HEAD at a branch that exists, so a clone of it
// checks something out. A client is free to push whatever branch name it likes
// to a wiki nobody has written yet — the conformance harness pushes `main`,
// github's own wikis use `master` — and HEAD has to follow the branch that
// actually landed rather than the one the server guessed.
func (st *Store) RepairWikiHead(repoKey string) {
	repoKey, fallback := st.wikiRepoDefaults(repoKey)
	st.WikiMu.Lock()
	defer st.WikiMu.Unlock()
	stor := st.openWikiStorageLocked(repoKey, fallback)
	if stor == nil {
		return
	}
	current := wikiHeadBranchLocked(stor, fallback)
	if _, err := stor.Reference(plumbing.NewBranchReferenceName(current)); err == nil {
		return
	}
	candidates := []string{fallback, "master", "main"}
	branches, err := stor.IterReferences()
	if err == nil {
		_ = branches.ForEach(func(ref *plumbing.Reference) error {
			if ref.Name().IsBranch() {
				candidates = append(candidates, ref.Name().Short())
			}
			return nil
		})
	}
	for _, branch := range candidates {
		if _, err := stor.Reference(plumbing.NewBranchReferenceName(branch)); err != nil {
			continue
		}
		if err := SetGitHeadBranch(stor, branch); err != nil {
			st.Logger.Error().Str("repo", repoKey).Str("branch", branch).Err(err).
				Msg("could not point the wiki HEAD at its branch")
		}
		return
	}
}

// wikiProjectionLocked returns the projection of the wiki's current tip,
// rebuilding it when the tip has moved since the memo was taken. Caller holds
// WikiMu.
func (st *Store) wikiProjectionLocked(repoKey, fallbackBranch string) *WikiProjection {
	stor := st.openWikiStorageLocked(repoKey, fallbackBranch)
	empty := &WikiProjection{Branch: fallbackBranch, Pages: map[string]*WikiPage{}, Revisions: map[string][]*WikiPageRevision{}}
	if stor == nil {
		return empty
	}
	branch := wikiHeadBranchLocked(stor, fallbackBranch)
	empty.Branch = branch
	tip := plumbing.ZeroHash
	if ref, err := stor.Reference(plumbing.NewBranchReferenceName(branch)); err == nil {
		tip = ref.Hash()
	}
	if cached := st.WikiProjections[repoKey]; cached != nil && cached.Tip == tip && cached.Branch == branch {
		return cached
	}
	if tip.IsZero() {
		st.WikiProjections[repoKey] = empty
		return empty
	}
	projection, err := buildWikiProjection(stor, repoKey, branch, tip)
	if err != nil {
		st.Logger.Error().Str("repo", repoKey).Err(err).Msg("could not read the wiki repository")
		return empty
	}
	st.WikiProjections[repoKey] = projection
	return projection
}

// buildWikiProjection walks the wiki's first-parent history oldest-first and
// replays it: each commit's page files are compared with the previous commit's,
// so a page's revisions are the commits that changed its blob, its creation is
// the commit the file appeared in, and its history restarts when the file is
// deleted and written again — which is what a page's history means to someone
// reading it.
func buildWikiProjection(stor gitStorage.Storer, repoKey, branch string, tip plumbing.Hash) (*WikiProjection, error) {
	commits, err := wikiFirstParentHistory(stor, tip)
	if err != nil {
		return nil, err
	}
	previous := map[string]plumbing.Hash{}
	ordinals := map[string]int{}
	revisions := map[string][]*WikiPageRevision{}
	var latest map[string]plumbing.Hash
	for _, commit := range commits {
		current, err := wikiPageFiles(stor, commit)
		if err != nil {
			return nil, err
		}
		// Sorted, because two file names can fold to one slug and the winner
		// has to be the same on every replica and every restart.
		for _, filePath := range sortedKeys(current) {
			blob := current[filePath]
			if previous[filePath] == blob {
				continue
			}
			title, ok := WikiTitleFromPath(filePath)
			if !ok {
				continue
			}
			slug := WikiSlug(title)
			if _, existed := previous[filePath]; !existed {
				ordinals[filePath] = 0
				revisions[slug] = nil
			}
			body, err := ReadGitBlob(stor, blob)
			if err != nil {
				return nil, err
			}
			ordinals[filePath]++
			revision := &WikiPageRevision{
				ID:        ordinals[filePath],
				Slug:      slug,
				Title:     title,
				Body:      string(body),
				Editor:    commit.Author.Name,
				Message:   strings.TrimSpace(commit.Message),
				CreatedAt: commit.Author.When.UTC(),
			}
			kept := append(revisions[slug], revision)
			if len(kept) > MaxWikiPageRevisions {
				kept = append([]*WikiPageRevision(nil), kept[len(kept)-MaxWikiPageRevisions:]...)
			}
			revisions[slug] = kept
		}
		for filePath := range previous {
			if _, still := current[filePath]; still {
				continue
			}
			if title, ok := WikiTitleFromPath(filePath); ok {
				delete(revisions, WikiSlug(title))
			}
			delete(ordinals, filePath)
		}
		previous = current
		latest = current
	}

	projection := &WikiProjection{
		Tip:       tip,
		Branch:    branch,
		Pages:     map[string]*WikiPage{},
		Revisions: map[string][]*WikiPageRevision{},
	}
	for _, filePath := range sortedKeys(latest) {
		title, ok := WikiTitleFromPath(filePath)
		if !ok {
			continue
		}
		slug := WikiSlug(title)
		if _, claimed := projection.Pages[slug]; claimed {
			continue
		}
		history := revisions[slug]
		if len(history) == 0 {
			continue
		}
		newest := history[len(history)-1]
		projection.Revisions[slug] = history
		projection.Pages[slug] = &WikiPage{
			Slug:      slug,
			Title:     title,
			Body:      newest.Body,
			Path:      filePath,
			RepoKey:   repoKey,
			Author:    newest.Editor,
			CreatedAt: history[0].CreatedAt,
			UpdatedAt: newest.CreatedAt,
		}
	}
	return projection, nil
}

// sortedKeys orders a path → blob map so every walk over it is deterministic.
func sortedKeys(files map[string]plumbing.Hash) []string {
	out := make([]string, 0, len(files))
	for key := range files {
		out = append(out, key)
	}
	sort.Strings(out)
	return out
}

// wikiFirstParentHistory returns the commits reachable from tip along first
// parents, oldest first and bounded.
func wikiFirstParentHistory(stor gitStorage.Storer, tip plumbing.Hash) ([]*object.Commit, error) {
	var reversed []*object.Commit
	seen := map[plumbing.Hash]bool{}
	for hash := tip; !hash.IsZero() && !seen[hash] && len(reversed) < maxWikiHistoryCommits; {
		seen[hash] = true
		commit, err := object.GetCommit(stor, hash)
		if err != nil {
			return nil, fmt.Errorf("read wiki commit %s: %w", hash, err)
		}
		reversed = append(reversed, commit)
		if len(commit.ParentHashes) == 0 {
			break
		}
		hash = commit.ParentHashes[0]
	}
	commits := make([]*object.Commit, 0, len(reversed))
	for i := len(reversed) - 1; i >= 0; i-- {
		commits = append(commits, reversed[i])
	}
	return commits, nil
}

// wikiPageFiles flattens a commit's tree into path → blob for the files that
// are wiki pages. Anything else the repository carries is left alone.
func wikiPageFiles(stor gitStorage.Storer, commit *object.Commit) (map[string]plumbing.Hash, error) {
	files := map[string]plumbing.Hash{}
	tree, err := object.GetTree(stor, commit.TreeHash)
	if err != nil {
		return nil, fmt.Errorf("read wiki tree %s: %w", commit.TreeHash, err)
	}
	for _, entry := range tree.Entries {
		// Any regular file is a page candidate, whatever mode bits a client
		// happened to push it with; a directory, a symlink or a submodule is not.
		if entry.Mode != filemode.Regular && entry.Mode != filemode.Deprecated && entry.Mode != filemode.Executable {
			continue
		}
		if _, ok := WikiTitleFromPath(entry.Name); !ok {
			continue
		}
		files[entry.Name] = entry.Hash
	}
	return files, nil
}

// wikiCommitPageFiles is wikiPageFiles for a commit named by object id, with a
// missing or unreadable commit reading as an empty wiki so a ref that was just
// created compares against nothing.
func wikiCommitPageFiles(stor gitStorage.Storer, hash plumbing.Hash) map[string]plumbing.Hash {
	if hash.IsZero() {
		return map[string]plumbing.Hash{}
	}
	commit, err := object.GetCommit(stor, hash)
	if err != nil {
		return map[string]plumbing.Hash{}
	}
	files, err := wikiPageFiles(stor, commit)
	if err != nil {
		return map[string]plumbing.Hash{}
	}
	return files
}

// WikiPagesChanged reports what a wiki commit range did to the wiki's pages,
// which is what the gollum webhook carries. github's documented payload can
// name a page as created or edited and has no vocabulary for a deletion, so a
// commit that only removes pages produces no entries.
func (st *Store) WikiPagesChanged(repoKey, before, after string) []WikiPageChange {
	repoKey, fallback := st.wikiRepoDefaults(repoKey)
	st.WikiMu.Lock()
	defer st.WikiMu.Unlock()
	stor := st.openWikiStorageLocked(repoKey, fallback)
	if stor == nil {
		return nil
	}
	old := wikiCommitPageFiles(stor, plumbing.NewHash(before))
	current := wikiCommitPageFiles(stor, plumbing.NewHash(after))
	var changes []WikiPageChange
	for _, filePath := range sortedKeys(current) {
		blob := current[filePath]
		previous, existed := old[filePath]
		if existed && previous == blob {
			continue
		}
		title, ok := WikiTitleFromPath(filePath)
		if !ok {
			continue
		}
		action := "created"
		if existed {
			action = "edited"
		}
		changes = append(changes, WikiPageChange{
			Slug:     WikiSlug(title),
			Title:    title,
			PageName: strings.TrimSuffix(filePath, path.Ext(filePath)),
			Action:   action,
			SHA:      after,
		})
	}
	sort.Slice(changes, func(i, j int) bool { return changes[i].PageName < changes[j].PageName })
	return changes
}

// WikiTipSHA is the object id of the wiki's current tip commit, or "" when the
// wiki has no commits.
func (st *Store) WikiTipSHA(repoKey string) string {
	repoKey, fallback := st.wikiRepoDefaults(repoKey)
	st.WikiMu.Lock()
	defer st.WikiMu.Unlock()
	projection := st.wikiProjectionLocked(repoKey, fallback)
	if projection.Tip.IsZero() {
		return ""
	}
	return projection.Tip.String()
}

// wikiWriteBlob stores content as a git blob and returns its object id.
func wikiWriteBlob(stor gitStorage.Storer, content []byte) (plumbing.Hash, error) {
	obj := stor.NewEncodedObject()
	obj.SetType(plumbing.BlobObject)
	obj.SetSize(int64(len(content)))
	writer, err := obj.Writer()
	if err != nil {
		return plumbing.ZeroHash, fmt.Errorf("open blob writer: %w", err)
	}
	if _, err := writer.Write(content); err != nil {
		_ = writer.Close()
		return plumbing.ZeroHash, fmt.Errorf("write blob: %w", err)
	}
	if err := writer.Close(); err != nil {
		return plumbing.ZeroHash, fmt.Errorf("close blob: %w", err)
	}
	return stor.SetEncodedObject(obj)
}

// wikiWriteTree stores a tree with the given entries, sorted the way git sorts
// them (a directory sorts as though its name ended in "/"), and reports whether
// the tree has any entries left so a caller can prune an emptied subtree.
func wikiWriteTree(stor gitStorage.Storer, entries []object.TreeEntry) (plumbing.Hash, bool, error) {
	sort.Slice(entries, func(i, j int) bool {
		return wikiTreeSortKey(entries[i]) < wikiTreeSortKey(entries[j])
	})
	tree := &object.Tree{Entries: entries}
	obj := stor.NewEncodedObject()
	if err := tree.Encode(obj); err != nil {
		return plumbing.ZeroHash, false, fmt.Errorf("encode tree: %w", err)
	}
	hash, err := stor.SetEncodedObject(obj)
	if err != nil {
		return plumbing.ZeroHash, false, fmt.Errorf("store tree: %w", err)
	}
	return hash, len(entries) > 0, nil
}

func wikiTreeSortKey(entry object.TreeEntry) string {
	if entry.Mode == filemode.Dir {
		return entry.Name + "/"
	}
	return entry.Name
}

// wikiTreeSet returns the tree that results from setting segments to blob
// inside base; a zero blob removes the entry and prunes any subtree it empties.
func wikiTreeSet(stor gitStorage.Storer, base *object.Tree, segments []string, blob plumbing.Hash) (plumbing.Hash, bool, error) {
	entries := []object.TreeEntry{}
	if base != nil {
		entries = append(entries, base.Entries...)
	}
	name := segments[0]
	index := -1
	for i := range entries {
		if entries[i].Name == name {
			index = i
			break
		}
	}
	if len(segments) == 1 {
		switch {
		case blob.IsZero():
			if index >= 0 {
				entries = append(entries[:index:index], entries[index+1:]...)
			}
		case index >= 0:
			entries[index] = object.TreeEntry{Name: name, Mode: filemode.Regular, Hash: blob}
		default:
			entries = append(entries, object.TreeEntry{Name: name, Mode: filemode.Regular, Hash: blob})
		}
		return wikiWriteTree(stor, entries)
	}

	var sub *object.Tree
	if index >= 0 && entries[index].Mode == filemode.Dir {
		var err error
		sub, err = object.GetTree(stor, entries[index].Hash)
		if err != nil {
			return plumbing.ZeroHash, false, fmt.Errorf("read subtree %s: %w", name, err)
		}
	} else if blob.IsZero() {
		return wikiWriteTree(stor, entries)
	}
	subHash, populated, err := wikiTreeSet(stor, sub, segments[1:], blob)
	if err != nil {
		return plumbing.ZeroHash, false, err
	}
	switch {
	case !populated:
		if index >= 0 {
			entries = append(entries[:index:index], entries[index+1:]...)
		}
	case index >= 0:
		entries[index] = object.TreeEntry{Name: name, Mode: filemode.Dir, Hash: subHash}
	default:
		entries = append(entries, object.TreeEntry{Name: name, Mode: filemode.Dir, Hash: subHash})
	}
	return wikiWriteTree(stor, entries)
}

// wikiCommitEdits writes one commit that applies every path → content edit in
// edits (a nil content removes the path) and advances the branch to it with a
// compare-and-set against the tip it was built on, so a concurrent push cannot
// be overwritten.
func (st *Store) wikiCommitEdits(stor gitStorage.Storer, branch string, edits map[string][]byte, sig object.Signature, message string) (plumbing.Hash, error) {
	branchRef := plumbing.NewBranchReferenceName(branch)
	var parents []plumbing.Hash
	var base *object.Tree
	current, err := stor.Reference(branchRef)
	switch {
	case err == nil:
		parents = []plumbing.Hash{current.Hash()}
		commit, err := object.GetCommit(stor, current.Hash())
		if err != nil {
			return plumbing.ZeroHash, fmt.Errorf("read wiki tip: %w", err)
		}
		base, err = object.GetTree(stor, commit.TreeHash)
		if err != nil {
			return plumbing.ZeroHash, fmt.Errorf("read wiki tree: %w", err)
		}
	case errors.Is(err, plumbing.ErrReferenceNotFound):
		current = nil
	default:
		return plumbing.ZeroHash, fmt.Errorf("read wiki branch %s: %w", branch, err)
	}

	treeHash := plumbing.ZeroHash
	paths := make([]string, 0, len(edits))
	for editPath := range edits {
		paths = append(paths, editPath)
	}
	sort.Strings(paths)
	for _, editPath := range paths {
		blob := plumbing.ZeroHash
		if edits[editPath] != nil {
			blob, err = wikiWriteBlob(stor, edits[editPath])
			if err != nil {
				return plumbing.ZeroHash, err
			}
		}
		treeHash, _, err = wikiTreeSet(stor, base, strings.Split(editPath, "/"), blob)
		if err != nil {
			return plumbing.ZeroHash, err
		}
		base, err = object.GetTree(stor, treeHash)
		if err != nil {
			return plumbing.ZeroHash, fmt.Errorf("reread wiki tree: %w", err)
		}
	}
	if treeHash.IsZero() {
		treeHash, _, err = wikiWriteTree(stor, nil)
		if err != nil {
			return plumbing.ZeroHash, err
		}
	}

	commit := &object.Commit{
		Author:       sig,
		Committer:    sig,
		Message:      message,
		TreeHash:     treeHash,
		ParentHashes: parents,
	}
	obj := stor.NewEncodedObject()
	if err := commit.Encode(obj); err != nil {
		return plumbing.ZeroHash, fmt.Errorf("encode wiki commit: %w", err)
	}
	commitHash, err := stor.SetEncodedObject(obj)
	if err != nil {
		return plumbing.ZeroHash, fmt.Errorf("store wiki commit: %w", err)
	}
	next := plumbing.NewHashReference(branchRef, commitHash)
	if current == nil {
		if err := gitstore.CreateReferenceIfAbsent(stor, next); err != nil {
			return plumbing.ZeroHash, fmt.Errorf("create wiki branch %s: %w", branch, err)
		}
		if err := SetGitHeadBranch(stor, branch); err != nil {
			return plumbing.ZeroHash, fmt.Errorf("point wiki HEAD at %s: %w", branch, err)
		}
		return commitHash, nil
	}
	if err := stor.CheckAndSetReference(next, current); err != nil {
		return plumbing.ZeroHash, fmt.Errorf("advance wiki branch %s: %w", branch, err)
	}
	return commitHash, nil
}

// wikiSignature is the commit identity a UI edit is recorded under. The login
// is the author name because that is what a page's history shows as its editor.
func (st *Store) wikiSignature(author string) object.Signature {
	if author == "" {
		author = "bleephub"
	}
	return object.Signature{
		Name:  author,
		Email: author + "@users.noreply.bleephub.local",
		When:  st.CurrentTime().UTC(),
	}
}

// wikiDefaultMessage is the commit subject github writes when an editor saves a
// page without an edit summary.
func wikiDefaultMessage(verb, title string) string {
	return fmt.Sprintf("%s %s (markdown)", verb, title)
}
