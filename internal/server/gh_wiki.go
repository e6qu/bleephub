package bleephub

import (
	"net/http"
	"strconv"
	"time"
	"unicode/utf8"

	"github.com/e6qu/bleephub/internal/store"
)

// Wiki is git-backed on real GitHub with NO REST API, so these routes live
// under the browser-only /ui-data namespace rather than /api/v3 — inventing a
// GitHub-namespaced path is a defect the route-definition tests reject. The
// simulator exposes a small page store so the /ui wiki tab has something to
// drive: list/read for anyone who can read the repo, create/update/delete for
// anyone with push access. A repository whose wiki is disabled (has_wiki=false)
// reports 404 for the whole surface, matching a disabled wiki on github.com.
// (`s.route` auto-wraps /ui-data patterns with authenticateUIData.)
func (s *Server) registerGHWikiRoutes() {
	s.route("GET /ui-data/repos/{owner}/{repo}/wiki/pages", s.handleListWikiPages)
	s.route("GET /ui-data/repos/{owner}/{repo}/wiki/pages/{slug}", s.handleGetWikiPage)
	s.route("PUT /ui-data/repos/{owner}/{repo}/wiki/pages/{slug}", s.handlePutWikiPage)
	s.route("DELETE /ui-data/repos/{owner}/{repo}/wiki/pages/{slug}", s.handleDeleteWikiPage)
	s.route("GET /ui-data/repos/{owner}/{repo}/wiki/pages/{slug}/revisions", s.handleListWikiPageRevisions)
	s.route("GET /ui-data/repos/{owner}/{repo}/wiki/pages/{slug}/revisions/{revision_id}", s.handleGetWikiPageRevision)
}

// wikiRepoForRead resolves the repo and enforces read access + wiki-enabled,
// writing the 404 itself when the caller should not see it.
func (s *Server) wikiRepoForRead(w http.ResponseWriter, r *http.Request) *store.Repo {
	repo := s.store.GetRepo(r.PathValue("owner"), r.PathValue("repo"))
	if repo == nil || !repo.HasWiki {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return nil
	}
	if repo.Private && !s.viewerCanReadRepo(r.Context(), repo) {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return nil
	}
	return repo
}

// wikiRepoForWrite resolves the repo and enforces push access + wiki-enabled.
func (s *Server) wikiRepoForWrite(w http.ResponseWriter, r *http.Request) *store.Repo {
	repo := s.store.GetRepo(r.PathValue("owner"), r.PathValue("repo"))
	if repo == nil || !repo.HasWiki {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return nil
	}
	// A private repo the viewer cannot read must not leak existence.
	if repo.Private && !s.viewerCanReadRepo(r.Context(), repo) {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return nil
	}
	if !s.viewerCanPushRepo(r.Context(), repo) {
		writeGHError(w, http.StatusForbidden, "Must have push access to edit the wiki.")
		return nil
	}
	return repo
}

func (s *Server) handleListWikiPages(w http.ResponseWriter, r *http.Request) {
	repo := s.wikiRepoForRead(w, r)
	if repo == nil {
		return
	}
	pages := s.store.ListWikiPages(repo.FullName)
	out := make([]map[string]interface{}, 0, len(pages))
	for _, p := range pages {
		out = append(out, wikiPageJSON(p))
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleGetWikiPage(w http.ResponseWriter, r *http.Request) {
	repo := s.wikiRepoForRead(w, r)
	if repo == nil {
		return
	}
	page := s.store.GetWikiPage(repo.FullName, store.WikiSlug(r.PathValue("slug")))
	if page == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	writeJSON(w, http.StatusOK, wikiPageJSON(page))
}

func (s *Server) handlePutWikiPage(w http.ResponseWriter, r *http.Request) {
	repo := s.wikiRepoForWrite(w, r)
	if repo == nil {
		return
	}
	slug := store.WikiSlug(r.PathValue("slug"))
	if slug == "" {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	var req struct {
		Title string `json:"title"`
		Body  string `json:"body"`
		// Message is the optional edit summary recorded on the revision.
		Message string `json:"message"`
	}
	if !decodeJSONBody(w, r, &req) {
		return
	}
	if req.Title == "" {
		store.WriteGHValidationError(w, "WikiPage", "title", "missing_field")
		return
	}

	existed := s.store.GetWikiPage(repo.FullName, slug) != nil
	author := ""
	if u := ghUserFromContext(r.Context()); u != nil {
		author = u.Login
	}
	page := s.store.UpsertWikiPage(repo.FullName, slug, req.Title, req.Body, author, req.Message)
	status := http.StatusOK
	if !existed {
		status = http.StatusCreated
	}
	writeJSON(w, status, wikiPageJSON(page))
}

func (s *Server) handleDeleteWikiPage(w http.ResponseWriter, r *http.Request) {
	repo := s.wikiRepoForWrite(w, r)
	if repo == nil {
		return
	}
	if !s.store.DeleteWikiPage(repo.FullName, store.WikiSlug(r.PathValue("slug"))) {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleListWikiPageRevisions lists a page's edit history newest first. Rows
// carry a short body preview rather than the full body; the single-revision
// read below returns the complete snapshot.
func (s *Server) handleListWikiPageRevisions(w http.ResponseWriter, r *http.Request) {
	repo := s.wikiRepoForRead(w, r)
	if repo == nil {
		return
	}
	slug := store.WikiSlug(r.PathValue("slug"))
	if s.store.GetWikiPage(repo.FullName, slug) == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	revisions := s.store.ListWikiPageRevisions(repo.FullName, slug)
	out := make([]map[string]interface{}, 0, len(revisions))
	for _, rev := range revisions {
		item := wikiRevisionJSON(rev, false)
		item["body_preview"] = wikiBodyPreview(rev.Body)
		out = append(out, item)
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleGetWikiPageRevision(w http.ResponseWriter, r *http.Request) {
	repo := s.wikiRepoForRead(w, r)
	if repo == nil {
		return
	}
	slug := store.WikiSlug(r.PathValue("slug"))
	id, err := strconv.Atoi(r.PathValue("revision_id"))
	if err != nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	rev := s.store.GetWikiPageRevision(repo.FullName, slug, id)
	if rev == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	writeJSON(w, http.StatusOK, wikiRevisionJSON(rev, true))
}

func wikiRevisionJSON(rev *store.WikiPageRevision, withBody bool) map[string]interface{} {
	out := map[string]interface{}{
		"id":         rev.ID,
		"slug":       rev.Slug,
		"title":      rev.Title,
		"editor":     rev.Editor,
		"message":    rev.Message,
		"created_at": rev.CreatedAt.Format(time.RFC3339),
	}
	if withBody {
		out["body"] = rev.Body
	}
	return out
}

// wikiBodyPreview truncates a revision body to a short listing preview on a
// rune boundary.
func wikiBodyPreview(body string) string {
	const max = 140
	if len(body) <= max {
		return body
	}
	cut := max
	for cut > 0 && !utf8.RuneStart(body[cut]) {
		cut--
	}
	return body[:cut] + "…"
}

func wikiPageJSON(p *store.WikiPage) map[string]interface{} {
	return map[string]interface{}{
		"slug":       p.Slug,
		"title":      p.Title,
		"body":       p.Body,
		"author":     p.Author,
		"created_at": p.CreatedAt.Format(time.RFC3339),
		"updated_at": p.UpdatedAt.Format(time.RFC3339),
	}
}
