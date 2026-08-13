package bleephub

import (
	"net/http"
	"time"

	"github.com/e6qu/bleephub/internal/store"
)

// Wiki is git-backed on real GitHub with no REST API. The simulator exposes a
// small page-store surface under the repo so the /ui wiki tab has something to
// drive: list/read for anyone who can read the repo, create/update/delete for
// anyone with push access. A repository whose wiki is disabled (has_wiki=false)
// reports 404 for the whole surface, matching a disabled wiki on github.com.
func (s *Server) registerGHWikiRoutes() {
	s.route("GET /api/v3/repos/{owner}/{repo}/wiki/pages", s.handleListWikiPages)
	s.route("GET /api/v3/repos/{owner}/{repo}/wiki/pages/{slug}", s.handleGetWikiPage)
	s.route("PUT /api/v3/repos/{owner}/{repo}/wiki/pages/{slug}", s.handlePutWikiPage)
	s.route("DELETE /api/v3/repos/{owner}/{repo}/wiki/pages/{slug}", s.handleDeleteWikiPage)
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
	page := s.store.UpsertWikiPage(repo.FullName, slug, req.Title, req.Body, author)
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
