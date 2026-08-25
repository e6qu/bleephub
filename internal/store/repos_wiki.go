package store

import (
	"sort"
	"strings"
	"time"
)

// The accessors here read and write the wiki git repository described in
// store_wiki_git.go. They are the browser UI's view of it, not a second copy of
// it: a read projects the current tip, a write is a commit.

// WikiSlug normalizes a page title into the URL-safe key the browser UI
// addresses a page by, mirroring how GitHub wikis address pages by their title
// with spaces turned into hyphens. It is derived from the title, and the title
// is derived from the page's file name, so the key of a page is a function of
// the wiki repository alone.
func WikiSlug(title string) string {
	var b strings.Builder
	prevHyphen := false
	for _, r := range strings.TrimSpace(title) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			prevHyphen = false
		case r >= 'A' && r <= 'Z':
			b.WriteRune(r + ('a' - 'A'))
			prevHyphen = false
		case r == ' ' || r == '-' || r == '_' || r == '/':
			if !prevHyphen && b.Len() > 0 {
				b.WriteByte('-')
				prevHyphen = true
			}
		}
	}
	slug := strings.Trim(b.String(), "-")
	return slug
}

// ListWikiPages returns a repository's wiki pages. "Home" sorts first (GitHub's
// landing page), the rest alphabetically by title.
func (st *Store) ListWikiPages(repoKey string) []*WikiPage {
	repoKey, branch := st.wikiRepoDefaults(repoKey)
	st.WikiMu.Lock()
	defer st.WikiMu.Unlock()
	projection := st.wikiProjectionLocked(repoKey, branch)

	out := make([]*WikiPage, 0, len(projection.Pages))
	for _, p := range projection.Pages {
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool {
		hi, hj := out[i].Slug == "home", out[j].Slug == "home"
		if hi != hj {
			return hi
		}
		return strings.ToLower(out[i].Title) < strings.ToLower(out[j].Title)
	})
	return snapshotSlice(out)
}

// GetWikiPage returns a detached copy of one wiki page, or nil if absent.
func (st *Store) GetWikiPage(repoKey, slug string) *WikiPage {
	repoKey, branch := st.wikiRepoDefaults(repoKey)
	st.WikiMu.Lock()
	defer st.WikiMu.Unlock()
	page := st.wikiProjectionLocked(repoKey, branch).Pages[slug]
	if page == nil {
		return nil
	}
	cp := *page
	return &cp
}

// UpsertWikiPage creates or replaces a wiki page by committing its file to the
// wiki repository. The page the caller names by slug is the page being edited;
// the page's own identity is its title, so a title whose file name differs from
// the one on disk renames the page — the new file and the removal of the old one
// land in one commit, exactly as renaming a page on github does.
//
// message is the commit subject: the edit summary when the editor wrote one,
// github's default subject when they did not. Returns the stored page (detached
// copy), or nil when the wiki repository could not be written.
func (st *Store) UpsertWikiPage(repoKey, slug, title, body, author, message string) *WikiPage {
	repoKey, fallback := st.wikiRepoDefaults(repoKey)
	st.WikiMu.Lock()
	defer st.WikiMu.Unlock()

	stor := st.openWikiStorageLocked(repoKey, fallback)
	if stor == nil {
		return nil
	}
	projection := st.wikiProjectionLocked(repoKey, fallback)
	newPath := WikiPageFileName(title)
	edits := map[string][]byte{newPath: []byte(body)}
	verb := "Created"
	if existing := projection.Pages[slug]; existing != nil {
		verb = "Updated"
		if existing.Path != newPath {
			edits[existing.Path] = nil
		}
	} else if projection.Pages[WikiSlug(title)] != nil {
		verb = "Updated"
	}
	if strings.TrimSpace(message) == "" {
		message = wikiDefaultMessage(verb, title)
	}
	if _, err := st.wikiCommitEdits(stor, projection.Branch, edits, st.wikiSignature(author), message); err != nil {
		st.Logger.Error().Str("repo", repoKey).Err(err).Msg("could not commit the wiki page")
		return nil
	}
	page := st.wikiProjectionLocked(repoKey, fallback).Pages[WikiSlug(title)]
	if page == nil {
		return nil
	}
	cp := *page
	return &cp
}

// DeleteWikiPage removes a wiki page by committing the removal of its file.
// Returns true if the page existed.
func (st *Store) DeleteWikiPage(repoKey, slug string) bool {
	repoKey, fallback := st.wikiRepoDefaults(repoKey)
	st.WikiMu.Lock()
	defer st.WikiMu.Unlock()

	stor := st.openWikiStorageLocked(repoKey, fallback)
	if stor == nil {
		return false
	}
	projection := st.wikiProjectionLocked(repoKey, fallback)
	page := projection.Pages[slug]
	if page == nil {
		return false
	}
	edits := map[string][]byte{page.Path: nil}
	message := wikiDefaultMessage("Destroyed", page.Title)
	if _, err := st.wikiCommitEdits(stor, projection.Branch, edits, st.wikiSignature(page.Author), message); err != nil {
		st.Logger.Error().Str("repo", repoKey).Err(err).Msg("could not commit the wiki page deletion")
		return false
	}
	st.wikiProjectionLocked(repoKey, fallback)
	return true
}

// MaxWikiPageRevisions bounds how much of a single page's history is projected;
// only the newest snapshots are kept, and their IDs still count from the
// page's first revision.
const MaxWikiPageRevisions = 100

// WikiPageRevision is one commit that changed a wiki page: the full body as it
// stood at that commit, who wrote it, when, and the commit subject. Reverting is
// a client-side PUT of an old revision's body — there is no server revert.
type WikiPageRevision struct {
	ID        int       `json:"id"`
	Slug      string    `json:"slug"`
	Title     string    `json:"title"`
	Body      string    `json:"body"`
	Editor    string    `json:"editor,omitempty"`
	Message   string    `json:"message,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}

// ListWikiPageRevisions returns a page's history, newest first (detached
// copies). The history starts at the commit the page's file was last created
// in, so a page that was deleted and written again reads as a new page.
func (st *Store) ListWikiPageRevisions(repoKey, slug string) []*WikiPageRevision {
	repoKey, branch := st.wikiRepoDefaults(repoKey)
	st.WikiMu.Lock()
	defer st.WikiMu.Unlock()
	revisions := st.wikiProjectionLocked(repoKey, branch).Revisions[slug]
	out := make([]*WikiPageRevision, 0, len(revisions))
	for i := len(revisions) - 1; i >= 0; i-- {
		cp := *revisions[i]
		out = append(out, &cp)
	}
	return out
}

// GetWikiPageRevision returns one revision of a page by its ID (detached
// copy), or nil if absent.
func (st *Store) GetWikiPageRevision(repoKey, slug string, id int) *WikiPageRevision {
	repoKey, branch := st.wikiRepoDefaults(repoKey)
	st.WikiMu.Lock()
	defer st.WikiMu.Unlock()
	for _, rev := range st.wikiProjectionLocked(repoKey, branch).Revisions[slug] {
		if rev.ID == id {
			cp := *rev
			return &cp
		}
	}
	return nil
}
