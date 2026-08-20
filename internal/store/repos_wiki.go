package store

import (
	"sort"
	"strings"
	"time"
)

// WikiSlug normalizes a page title into a URL-safe slug, mirroring how GitHub
// wikis address pages by their title with spaces turned into hyphens.
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
	st.Mu.RLock()
	defer st.Mu.RUnlock()
	repoKey = st.canonicalRepoKeyLocked(repoKey)

	m := st.RepoWikiPages[repoKey]
	out := make([]*WikiPage, 0, len(m))
	for _, p := range m {
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
	st.Mu.RLock()
	defer st.Mu.RUnlock()
	repoKey = st.canonicalRepoKeyLocked(repoKey)
	m := st.RepoWikiPages[repoKey]
	if m == nil {
		return nil
	}
	p := m[slug]
	if p == nil {
		return nil
	}
	cp := *p
	return &cp
}

// UpsertWikiPage creates or replaces a wiki page keyed by its slug (derived from
// the title). The Home page keeps a stable "home" slug regardless of its title.
// Every write appends a revision snapshot to the page's history (message is the
// optional edit summary), capped at MaxWikiPageRevisions with the oldest dropped.
// Returns the stored page (detached copy).
func (st *Store) UpsertWikiPage(repoKey, slug, title, body, author, message string) *WikiPage {
	st.Mu.Lock()
	defer st.Mu.Unlock()
	repoKey = st.canonicalRepoKeyLocked(repoKey)

	if st.RepoWikiPages[repoKey] == nil {
		st.RepoWikiPages[repoKey] = map[string]*WikiPage{}
	}
	now := time.Now().UTC()
	existing := st.RepoWikiPages[repoKey][slug]
	page := &WikiPage{
		Slug:      slug,
		Title:     title,
		Body:      body,
		RepoKey:   repoKey,
		Author:    author,
		UpdatedAt: now,
		CreatedAt: now,
	}
	if existing != nil {
		page.CreatedAt = existing.CreatedAt
	}
	st.RepoWikiPages[repoKey][slug] = page

	if st.RepoWikiRevisions[repoKey] == nil {
		st.RepoWikiRevisions[repoKey] = map[string][]*WikiPageRevision{}
	}
	revisions := st.RepoWikiRevisions[repoKey][slug]
	nextID := 1
	if len(revisions) > 0 {
		nextID = revisions[len(revisions)-1].ID + 1
	}
	revisions = append(revisions, &WikiPageRevision{
		ID:        nextID,
		Slug:      slug,
		Title:     title,
		Body:      body,
		Editor:    author,
		Message:   message,
		CreatedAt: now,
	})
	if len(revisions) > MaxWikiPageRevisions {
		revisions = append([]*WikiPageRevision(nil), revisions[len(revisions)-MaxWikiPageRevisions:]...)
	}
	st.RepoWikiRevisions[repoKey][slug] = revisions

	if st.Persist != nil {
		st.Persist.MustPut("repo_wiki_pages", repoKey, st.RepoWikiPages[repoKey])
		st.Persist.MustPut("repo_wiki_revisions", repoKey, st.RepoWikiRevisions[repoKey])
	}
	cp := *page
	return &cp
}

// DeleteWikiPage removes a wiki page by slug, along with its revision history
// (a recreated page starts a fresh history). Returns true if it existed.
func (st *Store) DeleteWikiPage(repoKey, slug string) bool {
	st.Mu.Lock()
	defer st.Mu.Unlock()
	repoKey = st.canonicalRepoKeyLocked(repoKey)

	m := st.RepoWikiPages[repoKey]
	if m == nil {
		return false
	}
	if _, ok := m[slug]; !ok {
		return false
	}
	delete(m, slug)
	if revs := st.RepoWikiRevisions[repoKey]; revs != nil {
		delete(revs, slug)
	}
	if st.Persist != nil {
		st.Persist.MustPut("repo_wiki_pages", repoKey, st.RepoWikiPages[repoKey])
		st.Persist.MustPut("repo_wiki_revisions", repoKey, st.RepoWikiRevisions[repoKey])
	}
	return true
}

// MaxWikiPageRevisions bounds a single page's stored revision history; the
// oldest snapshots are dropped once the cap is exceeded.
const MaxWikiPageRevisions = 100

// WikiPageRevision is one saved snapshot of a wiki page edit: the full body as
// written, who wrote it, when, and the optional edit summary. Reverting is a
// client-side PUT of an old revision's body — there is no server revert.
type WikiPageRevision struct {
	ID        int       `json:"id"`
	Slug      string    `json:"slug"`
	Title     string    `json:"title"`
	Body      string    `json:"body"`
	Editor    string    `json:"editor,omitempty"`
	Message   string    `json:"message,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}

// ListWikiPageRevisions returns a page's revision history, newest first
// (detached copies).
func (st *Store) ListWikiPageRevisions(repoKey, slug string) []*WikiPageRevision {
	st.Mu.RLock()
	defer st.Mu.RUnlock()
	repoKey = st.canonicalRepoKeyLocked(repoKey)
	revisions := st.RepoWikiRevisions[repoKey][slug]
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
	st.Mu.RLock()
	defer st.Mu.RUnlock()
	repoKey = st.canonicalRepoKeyLocked(repoKey)
	for _, rev := range st.RepoWikiRevisions[repoKey][slug] {
		if rev.ID == id {
			cp := *rev
			return &cp
		}
	}
	return nil
}
