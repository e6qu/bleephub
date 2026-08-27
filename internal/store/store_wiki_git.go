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

// A repository's wiki is itself a git repository (`<owner>/<repo>.wiki.git`),
// with no REST API. The pages the UI reads are a projection of the tip commit
// and every UI write is a commit; the projection is a memo keyed by the tip it
// was computed from, so a stale memo is detected by comparing object ids rather
// than invalidated by a writer that has to remember to.

// WikiStorageSuffix keys a wiki's git storage: `admin/docs` stores its wiki
// under `admin/docs.wiki.git`. The `.git` is deliberate — repository names
// ending in `.git` are refused at creation, so no repository can collide with
// this key, and the wiki gets the same pluggable storer as any other repository.
const WikiStorageSuffix = ".wiki.git"

// WikiURLSuffix is the repository-name suffix a git client addresses a wiki
// with, after the transport trims the trailing `.git`.
const WikiURLSuffix = ".wiki"

// WikiStorageName maps a repository's full name to its wiki's storage key.
func WikiStorageName(repoKey string) string { return repoKey + WikiStorageSuffix }

// wikiMarkupExtensions are the extensions github renders as wiki pages. A file
// with any other extension is content the wiki carries but not a page.
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
// with (markdown, as github's editor writes).
const WikiPageExtension = ".md"

// maxWikiHistoryCommits bounds the first-parent walk that derives page
// timestamps and revisions from the wiki's history.
const maxWikiHistoryCommits = 5000

// WikiPageFileName maps a page title to its file name the way github does:
// spaces (and path separators) become hyphens and the extension is appended, so
// "Getting Started" is `Getting-Started.md`. Folding separators keeps a title
// from escaping into a subdirectory, where it would no longer round-trip.
func WikiPageFileName(title string) string {
	return WikiPageName(title) + WikiPageExtension
}

// WikiPageName is WikiPageFileName without the extension: the hyphenated name
// github addresses a page by.
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

// WikiTitleFromPath is the inverse mapping, with hyphens read back as spaces.
// Only a markup file at the repository root is a page (the mapping is a
// bijection there); anything else reports false.
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

// WikiProjection is the wiki's tip commit read as pages, stamped with the tip it
// was derived from. A projection whose Tip is not the current tip is stale and
// thrown away rather than patched.
type WikiProjection struct {
	Tip       plumbing.Hash
	Branch    string
	Pages     map[string]*WikiPage
	Revisions map[string][]*WikiPageRevision
}

// WikiPageChange is one page a wiki commit range changed, as the gollum webhook reports.
type WikiPageChange struct {
	Slug     string
	Title    string
	PageName string
	Action   string
	SHA      string
}

// openWikiStorageLocked opens or initializes the wiki's git storage through the
// same pluggable storer ordinary repositories use. Caller holds WikiMu.
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

// wikiRepoDefaults reads the canonical repo key and default branch, releasing
// st.Mu before any git I/O so a slow object-store round trip never holds it.
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

// WikiGitStorage is the storer both git transports serve a wiki from, opening
// the wiki repository on first use so a client can push to an untouched wiki.
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

// DropWikiGitStorage forgets a wiki's handle and projection; the bytes are
// removed by the caller that removes the repository's own storage.
func (st *Store) DropWikiGitStorage(repoKey string) {
	st.WikiMu.Lock()
	defer st.WikiMu.Unlock()
	delete(st.WikiGitStorages, repoKey)
	delete(st.WikiProjections, repoKey)
}

// RekeyWikiGitStorage follows a repository rename, dropping the wiki handle and
// projection so the next access reopens them against the new key. The bytes are
// moved by MoveWikiGitStorageBytes.
func (st *Store) RekeyWikiGitStorage(oldKey, newKey string) {
	st.WikiMu.Lock()
	defer st.WikiMu.Unlock()
	delete(st.WikiGitStorages, oldKey)
	delete(st.WikiProjections, oldKey)
	delete(st.WikiGitStorages, newKey)
	delete(st.WikiProjections, newKey)
}

// wikiHeadBranchLocked reports the branch the wiki's HEAD names, falling back to
// fallback when HEAD is missing or detached.
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

// RepairWikiHead points a wiki's HEAD at a branch that exists, so a clone checks
// something out. A client may push any branch name to a fresh wiki, so HEAD must
// follow the branch that actually landed rather than the one the server guessed.
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
// rebuilding it when the tip has moved. Caller holds WikiMu.
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

// buildWikiProjection replays the wiki's first-parent history oldest-first,
// comparing each commit's page files with the previous commit's: a page's
// revisions are the commits that changed its blob, and its history restarts when
// the file is deleted and written again.
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
		// Sorted so the winner is deterministic when two file names fold to
		// one slug.
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

// wikiPageFiles flattens a commit's tree into path → blob for its wiki pages.
func wikiPageFiles(stor gitStorage.Storer, commit *object.Commit) (map[string]plumbing.Hash, error) {
	files := map[string]plumbing.Hash{}
	tree, err := object.GetTree(stor, commit.TreeHash)
	if err != nil {
		return nil, fmt.Errorf("read wiki tree %s: %w", commit.TreeHash, err)
	}
	for _, entry := range tree.Entries {
		// Any regular file is a page candidate, whatever mode bits it was pushed
		// with; a directory, symlink or submodule is not.
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

// wikiCommitPageFiles is wikiPageFiles for a commit named by object id; a
// missing or unreadable commit reads as an empty wiki, so a just-created ref
// compares against nothing.
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

// WikiPagesChanged reports what a wiki commit range did to the wiki's pages, as
// the gollum webhook carries. github's payload has no vocabulary for a deletion,
// so a commit that only removes pages produces no entries.
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

// WikiTipSHA is the object id of the wiki's tip commit, or "" when it has none.
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

// wikiWriteTree stores a tree with entries in git's sort order (a directory
// sorts as though its name ended in "/") and reports whether any entries remain,
// so a caller can prune an emptied subtree.
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

// wikiCommitEdits writes one commit applying every path → content edit (nil
// content removes the path) and advances the branch with a compare-and-set
// against the tip it was built on, so a concurrent push cannot be overwritten.
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

// wikiSignature is the commit identity a UI edit is recorded under; the login is
// the author name so it shows as the page's editor in history.
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

// wikiDefaultMessage is the commit subject github writes when a page is saved
// without an edit summary.
func wikiDefaultMessage(verb, title string) string {
	return fmt.Sprintf("%s %s (markdown)", verb, title)
}
