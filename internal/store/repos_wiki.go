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
// Returns the stored page (detached copy).
func (st *Store) UpsertWikiPage(repoKey, slug, title, body, author string) *WikiPage {
	st.Mu.Lock()
	defer st.Mu.Unlock()

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
	if st.Persist != nil {
		st.Persist.MustPut("repo_wiki_pages", repoKey, st.RepoWikiPages[repoKey])
	}
	cp := *page
	return &cp
}

// DeleteWikiPage removes a wiki page by slug. Returns true if it existed.
func (st *Store) DeleteWikiPage(repoKey, slug string) bool {
	st.Mu.Lock()
	defer st.Mu.Unlock()

	m := st.RepoWikiPages[repoKey]
	if m == nil {
		return false
	}
	if _, ok := m[slug]; !ok {
		return false
	}
	delete(m, slug)
	if st.Persist != nil {
		st.Persist.MustPut("repo_wiki_pages", repoKey, st.RepoWikiPages[repoKey])
	}
	return true
}
