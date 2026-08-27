package bleephub

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/e6qu/bleephub/internal/store"
)

func (s *Server) registerGHGistRoutes() {
	s.route("GET /api/v3/gists", s.handleListGists)
	s.route("GET /api/v3/gists/public", s.handleListPublicGists)
	s.route("GET /api/v3/gists/starred", s.handleListStarredGists)
	s.route("POST /api/v3/gists", s.handleCreateGist)
	s.route("GET /api/v3/gists/{gist_id}", s.handleGetGist)
	s.route("PATCH /api/v3/gists/{gist_id}", s.handleUpdateGist)
	s.route("DELETE /api/v3/gists/{gist_id}", s.handleDeleteGist)
	s.route("PUT /api/v3/gists/{gist_id}/star", s.handleStarGist)
	s.route("DELETE /api/v3/gists/{gist_id}/star", s.handleUnstarGist)
	s.route("GET /api/v3/gists/{gist_id}/star", s.handleCheckStarredGist)
	s.route("POST /api/v3/gists/{gist_id}/forks", s.handleForkGist)
	s.route("GET /api/v3/gists/{gist_id}/forks", s.handleListGistForks)
	s.route("GET /api/v3/gists/{gist_id}/comments", s.handleListGistComments)
	s.route("POST /api/v3/gists/{gist_id}/comments", s.handleCreateGistComment)
	s.route("GET /api/v3/gists/{gist_id}/comments/{comment_id}", s.handleGetGistComment)
	s.route("PATCH /api/v3/gists/{gist_id}/comments/{comment_id}", s.handleUpdateGistComment)
	s.route("DELETE /api/v3/gists/{gist_id}/comments/{comment_id}", s.handleDeleteGistComment)
	s.route("GET /api/v3/gists/{gist_id}/commits", s.handleListGistCommits)
	s.route("GET /api/v3/gists/{gist_id}/{sha}", s.handleGetGistAtRevision)
}

// visibleGist resolves {gist_id} for the request's viewer, or answers 404.
// Every gist-by-id route must go through here so a non-public gist's contents
// never reach an anonymous caller.
func (s *Server) visibleGist(w http.ResponseWriter, r *http.Request) *store.Gist {
	g := s.store.GetGist(r.PathValue("gist_id"))
	if g == nil || !s.viewerCanSeeGist(r.Context(), g) {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return nil
	}
	return g
}

// viewerCanSeeGist: a public gist is everyone's, a non-public one only its owner's.
func (s *Server) viewerCanSeeGist(ctx context.Context, g *store.Gist) bool {
	if g == nil {
		return false
	}
	if g.Public {
		return true
	}
	user := ghUserFromContext(ctx)
	return user != nil && user.ID == g.OwnerID
}

func (s *Server) handleListGistCommits(w http.ResponseWriter, r *http.Request) {
	gist := s.visibleGist(w, r)
	if gist == nil {
		return
	}
	id := r.PathValue("gist_id")
	commits := s.store.ListGistCommits(id)
	if commits == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	base := s.baseURL(r)
	var ownerJSON interface{}
	if owner := s.store.GetUserByID(gist.OwnerID); owner != nil {
		ownerJSON = store.UserToJSON(owner, s.baseURL(r))
	}
	items := make([]map[string]interface{}, len(commits))
	for i, h := range commits {
		items[i] = map[string]interface{}{
			"url":           base + "/api/v3/gists/" + id + "/" + h.Version,
			"version":       h.Version,
			"user":          ownerJSON,
			"change_status": h.ChangeStatus,
			"committed_at":  h.CommittedAt.Format(time.RFC3339),
		}
	}
	writeJSON(w, http.StatusOK, paginateAndLink(w, r, items))
}

func (s *Server) handleGetGistAtRevision(w http.ResponseWriter, r *http.Request) {
	if s.visibleGist(w, r) == nil {
		return
	}
	id, sha := r.PathValue("gist_id"), r.PathValue("sha")
	g := s.store.GetGistAtRevision(id, sha)
	if g == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	writeJSON(w, http.StatusOK, s.gistToJSON(g, r, true))
}

func (s *Server) handleListGists(w http.ResponseWriter, r *http.Request) {
	user := ghUserFromContext(r.Context())
	if user == nil {
		writeGHError(w, http.StatusUnauthorized, "Bad credentials")
		return
	}
	since := parseSince(r)
	gists := s.store.ListGistsForUser(user.ID, since)
	writeGistList(w, r, s, gists, false)
}

func (s *Server) handleListPublicGists(w http.ResponseWriter, r *http.Request) {
	since := parseSince(r)
	gists := s.store.ListPublicGists(since)
	writeGistList(w, r, s, gists, false)
}

func (s *Server) handleListStarredGists(w http.ResponseWriter, r *http.Request) {
	user := ghUserFromContext(r.Context())
	if user == nil {
		writeGHError(w, http.StatusUnauthorized, "Bad credentials")
		return
	}
	gists := s.store.ListStarredGists(user.ID)
	writeGistList(w, r, s, gists, false)
}

func (s *Server) handleCreateGist(w http.ResponseWriter, r *http.Request) {
	user := ghUserFromContext(r.Context())
	if user == nil {
		writeGHError(w, http.StatusUnauthorized, "Bad credentials")
		return
	}

	var req struct {
		Description string `json:"description"`
		Public      bool   `json:"public"`
		Files       map[string]struct {
			Content string `json:"content"`
		} `json:"files"`
	}
	if !decodeJSONBody(w, r, &req) {
		return
	}
	if len(req.Files) == 0 {
		store.WriteGHValidationError(w, "Gist", "files", "missing_field")
		return
	}

	files := make(map[string]*store.GistFile)
	for name, f := range req.Files {
		if name == "" {
			store.WriteGHValidationError(w, "Gist", "files", "invalid")
			return
		}
		files[name] = gistFileFromInput(name, f.Content, s.baseURL(r), "")
	}

	g, err := s.store.CreateGistE(user, req.Description, req.Public, files)
	if err != nil {
		writeGHError(w, http.StatusInternalServerError, err.Error())
		return
	}
	gistJSON := s.gistToJSON(g, r, true)
	writeJSONCreated(w, jsonStringField(gistJSON, "url"), gistJSON)
}

func (s *Server) handleGetGist(w http.ResponseWriter, r *http.Request) {
	g := s.visibleGist(w, r)
	if g == nil {
		return
	}
	writeJSON(w, http.StatusOK, s.gistToJSON(g, r, true))
}

func (s *Server) handleUpdateGist(w http.ResponseWriter, r *http.Request) {
	user := ghUserFromContext(r.Context())
	if user == nil {
		writeGHError(w, http.StatusUnauthorized, "Bad credentials")
		return
	}
	id := r.PathValue("gist_id")
	g := s.visibleGist(w, r)
	if g == nil {
		return
	}
	if user.ID != g.OwnerID {
		writeGHError(w, http.StatusForbidden, "Must have admin rights to Gist.")
		return
	}

	var req struct {
		Description *string `json:"description"`
		Files       map[string]*struct {
			Content  *string `json:"content"`
			Filename *string `json:"filename"`
		} `json:"files"`
	}
	if !decodeJSONBody(w, r, &req) {
		return
	}

	var newFiles map[string]*store.GistFile
	var deleteFiles []string
	if req.Files != nil {
		// Carry forward existing content when a file entry omits `content`
		// (e.g. a pure rename must not blank the body).
		current := s.store.GetGist(id)
		newFiles = make(map[string]*store.GistFile)
		for name, f := range req.Files {
			if f == nil {
				deleteFiles = append(deleteFiles, name)
				continue
			}
			content := ""
			if f.Content != nil {
				content = *f.Content
			} else if current != nil {
				if existing, ok := current.Files[name]; ok {
					content = existing.Content
				}
			}
			filename := name
			if f.Filename != nil && *f.Filename != "" {
				filename = *f.Filename
			}
			// A rename removes the file under its old name.
			if filename != name {
				deleteFiles = append(deleteFiles, name)
			}
			newFiles[filename] = gistFileFromInput(filename, content, s.baseURL(r), id)
		}
	}

	updated, ok, err := s.store.UpdateGistE(id, req.Description, newFiles, deleteFiles)
	if err != nil {
		writeGHError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if !ok {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	writeJSON(w, http.StatusOK, s.gistToJSON(updated, r, true))
}

func (s *Server) handleDeleteGist(w http.ResponseWriter, r *http.Request) {
	user := ghUserFromContext(r.Context())
	if user == nil {
		writeGHError(w, http.StatusUnauthorized, "Bad credentials")
		return
	}
	id := r.PathValue("gist_id")
	g := s.visibleGist(w, r)
	if g == nil {
		return
	}
	if user.ID != g.OwnerID {
		writeGHError(w, http.StatusForbidden, "Must have admin rights to Gist.")
		return
	}
	s.store.DeleteGist(id)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleStarGist(w http.ResponseWriter, r *http.Request) {
	user := ghUserFromContext(r.Context())
	if user == nil {
		writeGHError(w, http.StatusUnauthorized, "Bad credentials")
		return
	}
	if s.visibleGist(w, r) == nil {
		return
	}
	id := r.PathValue("gist_id")
	if !s.store.StarGist(user.ID, id) {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleUnstarGist(w http.ResponseWriter, r *http.Request) {
	user := ghUserFromContext(r.Context())
	if user == nil {
		writeGHError(w, http.StatusUnauthorized, "Bad credentials")
		return
	}
	if s.visibleGist(w, r) == nil {
		return
	}
	id := r.PathValue("gist_id")
	if !s.store.UnstarGist(user.ID, id) {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleCheckStarredGist(w http.ResponseWriter, r *http.Request) {
	user := ghUserFromContext(r.Context())
	if user == nil {
		writeGHError(w, http.StatusUnauthorized, "Bad credentials")
		return
	}
	if s.visibleGist(w, r) == nil {
		return
	}
	id := r.PathValue("gist_id")
	if !s.store.IsGistStarred(user.ID, id) {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleForkGist(w http.ResponseWriter, r *http.Request) {
	user := ghUserFromContext(r.Context())
	if user == nil {
		writeGHError(w, http.StatusUnauthorized, "Bad credentials")
		return
	}
	if s.visibleGist(w, r) == nil {
		return
	}
	id := r.PathValue("gist_id")
	fork, ok, err := s.store.ForkGistE(user, id)
	if err != nil {
		writeGHError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if !ok {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	forkJSON := s.gistToJSON(fork, r, false)
	writeJSONCreated(w, jsonStringField(forkJSON, "url"), forkJSON)
}

func (s *Server) handleListGistForks(w http.ResponseWriter, r *http.Request) {
	if s.visibleGist(w, r) == nil {
		return
	}
	forks := s.store.ListGistForks(r.PathValue("gist_id"))
	items := make([]map[string]interface{}, len(forks))
	for i, f := range forks {
		items[i] = s.gistToJSON(f, r, false)
	}
	writeJSON(w, http.StatusOK, paginateAndLink(w, r, items))
}

func (s *Server) handleListGistComments(w http.ResponseWriter, r *http.Request) {
	if s.visibleGist(w, r) == nil {
		return
	}
	comments := s.store.ListGistComments(r.PathValue("gist_id"))
	items := make([]map[string]interface{}, len(comments))
	for i, c := range comments {
		items[i] = s.gistCommentToJSON(c, r)
	}
	writeJSON(w, http.StatusOK, paginateAndLink(w, r, items))
}

func (s *Server) handleCreateGistComment(w http.ResponseWriter, r *http.Request) {
	user := ghUserFromContext(r.Context())
	if user == nil {
		writeGHError(w, http.StatusUnauthorized, "Bad credentials")
		return
	}
	if s.visibleGist(w, r) == nil {
		return
	}
	id := r.PathValue("gist_id")
	var req struct {
		Body string `json:"body"`
	}
	if !decodeJSONBody(w, r, &req) {
		return
	}
	if req.Body == "" {
		store.WriteGHValidationError(w, "GistComment", "body", "missing_field")
		return
	}
	c := s.store.CreateGistComment(id, user, req.Body)
	if c == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	gistCommentJSON := s.gistCommentToJSON(c, r)
	writeJSONCreated(w, jsonStringField(gistCommentJSON, "url"), gistCommentJSON)
}

func (s *Server) handleGetGistComment(w http.ResponseWriter, r *http.Request) {
	if s.visibleGist(w, r) == nil {
		return
	}
	id, err := strconv.Atoi(r.PathValue("comment_id"))
	if err != nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	c := s.store.GetGistComment(id)
	if c == nil || c.GistID != r.PathValue("gist_id") {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	writeJSON(w, http.StatusOK, s.gistCommentToJSON(c, r))
}

func (s *Server) handleUpdateGistComment(w http.ResponseWriter, r *http.Request) {
	user := ghUserFromContext(r.Context())
	if user == nil {
		writeGHError(w, http.StatusUnauthorized, "Bad credentials")
		return
	}
	if s.visibleGist(w, r) == nil {
		return
	}
	gistID := r.PathValue("gist_id")
	commentID, err := strconv.Atoi(r.PathValue("comment_id"))
	if err != nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	c := s.store.GetGistComment(commentID)
	if c == nil || c.GistID != gistID {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	if user.ID != c.UserID {
		writeGHError(w, http.StatusForbidden, "Must have admin rights to GistComment.")
		return
	}
	var req struct {
		Body string `json:"body"`
	}
	if !decodeJSONBody(w, r, &req) {
		return
	}
	updated, ok := s.store.UpdateGistComment(commentID, req.Body)
	if !ok {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	writeJSON(w, http.StatusOK, s.gistCommentToJSON(updated, r))
}

func (s *Server) handleDeleteGistComment(w http.ResponseWriter, r *http.Request) {
	user := ghUserFromContext(r.Context())
	if user == nil {
		writeGHError(w, http.StatusUnauthorized, "Bad credentials")
		return
	}
	if s.visibleGist(w, r) == nil {
		return
	}
	gistID := r.PathValue("gist_id")
	commentID, err := strconv.Atoi(r.PathValue("comment_id"))
	if err != nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	c := s.store.GetGistComment(commentID)
	if c == nil || c.GistID != gistID {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	if user.ID != c.UserID {
		writeGHError(w, http.StatusForbidden, "Must have admin rights to GistComment.")
		return
	}
	s.store.DeleteGistComment(commentID)
	w.WriteHeader(http.StatusNoContent)
}

func writeGistList(w http.ResponseWriter, r *http.Request, s *Server, gists []*store.Gist, includeContent bool) {
	items := make([]map[string]interface{}, len(gists))
	for i, g := range gists {
		items[i] = s.gistToJSON(g, r, includeContent)
	}
	writeJSON(w, http.StatusOK, paginateAndLink(w, r, items))
}

// gistView returns a locked snapshot of the gist with this request's URLs
// derived onto the copy — never onto the stored gist, whose host must not
// carry one caller's Host to every other reader.
func (s *Server) gistView(g *store.Gist, r *http.Request) *store.Gist {
	view := s.snapshotGist(g)
	if view == nil {
		return nil
	}
	base := s.baseURL(r)
	view.URL = base + "/api/v3/gists/" + view.ID
	view.ForksURL = view.URL + "/forks"
	view.CommitsURL = view.URL + "/commits"
	view.CommentsURL = view.URL + "/comments"
	// A gist's web and clone URLs are keyed by the gist id alone, never the owner
	// — GHES serves /gist/{id}[.git]; an owner-scoped path breaks `gh gist view
	// --web` and `git clone <git_pull_url>`.
	view.HTMLURL = base + "/gist/" + view.ID
	view.GitPullURL = view.HTMLURL + ".git"
	view.GitPushURL = view.GitPullURL
	for name, f := range view.Files {
		f.RawURL = base + "/raw/" + view.ID + "/" + name
		if f.Filename == "" {
			f.Filename = name
		}
		if f.Type == "" {
			f.Type = detectGistFileType(f.Filename)
		}
		if f.Language == "" {
			f.Language = detectGistLanguage(f.Filename)
		}
		f.Size = len(f.Content)
	}
	store.SortHistory(view.History)
	return view
}

// snapshotGist deep-copies a stored gist under the store lock, sharing nothing
// mutable with a concurrent writer.
func (s *Server) snapshotGist(g *store.Gist) *store.Gist {
	if g == nil {
		return nil
	}
	s.store.Mu.RLock()
	defer s.store.Mu.RUnlock()
	view := *g
	view.Files = make(map[string]*store.GistFile, len(g.Files))
	for name, f := range g.Files {
		file := *f
		view.Files[name] = &file
	}
	view.History = make([]*store.GistHistory, len(g.History))
	for i, h := range g.History {
		entry := *h
		entry.ChangeStatus = make(map[string]int, len(h.ChangeStatus))
		for k, v := range h.ChangeStatus {
			entry.ChangeStatus[k] = v
		}
		view.History[i] = &entry
	}
	view.ForkIDs = append([]string(nil), g.ForkIDs...)
	return &view
}

func (s *Server) gistToJSON(g *store.Gist, r *http.Request, includeContent bool) map[string]interface{} {
	g = s.gistView(g, r)
	base := s.baseURL(r)
	files := make(map[string]interface{}, len(g.Files))
	for name, f := range g.Files {
		fileJSON := map[string]interface{}{
			"filename": f.Filename,
			"type":     f.Type,
			"language": f.Language,
			"raw_url":  f.RawURL,
			"size":     f.Size,
		}
		if includeContent {
			// content + truncated are gist-simple-only; the base-gist list omits both.
			fileJSON["content"] = f.Content
			fileJSON["truncated"] = false
		}
		files[name] = fileJSON
	}

	forks := []interface{}{}
	for _, f := range s.store.ListGistForks(g.ID) {
		var forkOwnerJSON interface{}
		if forkOwner := s.store.GetUserByID(f.OwnerID); forkOwner != nil {
			// gist-simple forks[].user is a full public-user, not simple-user.
			forkOwnerJSON = s.fullUserJSON(forkOwner, s.baseURL(r))
		}
		forks = append(forks, map[string]interface{}{
			"id":         f.ID,
			"url":        base + "/api/v3/gists/" + f.ID,
			"user":       forkOwnerJSON,
			"created_at": f.CreatedAt.Format(time.RFC3339),
			"updated_at": f.UpdatedAt.Format(time.RFC3339),
		})
	}

	owner := s.store.GetUserByID(g.OwnerID)
	var ownerJSON interface{}
	if owner != nil {
		ownerJSON = store.UserToJSON(owner, s.baseURL(r))
	}

	history := make([]map[string]interface{}, len(g.History))
	for i, h := range g.History {
		history[i] = map[string]interface{}{
			"url":           base + "/api/v3/gists/" + g.ID + "/" + h.Version,
			"version":       h.Version,
			"user":          ownerJSON,
			"change_status": h.ChangeStatus,
			"committed_at":  h.CommittedAt.Format(time.RFC3339),
		}
	}

	out := map[string]interface{}{
		"url":          g.URL,
		"forks_url":    g.ForksURL,
		"commits_url":  g.CommitsURL,
		"id":           g.ID,
		"node_id":      g.NodeID,
		"git_pull_url": g.GitPullURL,
		"git_push_url": g.GitPushURL,
		"html_url":     g.HTMLURL,
		"files":        files,
		"public":       g.Public,
		"description":  g.Description,
		"comments":     g.Comments,
		"user":         nil,
		"comments_url": g.CommentsURL,
		"owner":        ownerJSON,
		"truncated":    false,
		"forks":        forks,
		"history":      history,
		"created_at":   g.CreatedAt.Format(time.RFC3339),
		"updated_at":   g.UpdatedAt.Format(time.RFC3339),
	}
	// fork_of is gist-simple-only — emit it only on single-gist responses.
	if includeContent && g.ForkOfID != "" {
		if parent := s.store.GetGist(g.ForkOfID); parent != nil {
			out["fork_of"] = s.gistToJSON(parent, r, false)
		}
	}
	return out
}

func (s *Server) gistCommentToJSON(c *store.GistComment, r *http.Request) map[string]interface{} {
	base := s.baseURL(r)
	user := s.store.GetUserByID(c.UserID)
	var userJSON interface{}
	if user != nil {
		userJSON = store.UserToJSON(user, s.baseURL(r))
	}
	return map[string]interface{}{
		"id":                 c.ID,
		"node_id":            c.NodeID,
		"url":                base + "/api/v3/gists/" + c.GistID + "/comments/" + strconv.Itoa(c.ID),
		"body":               c.Body,
		"user":               userJSON,
		"created_at":         c.CreatedAt.Format(time.RFC3339),
		"updated_at":         c.UpdatedAt.Format(time.RFC3339),
		"author_association": c.AuthorAssociation,
	}
}

func gistFileFromInput(filename, content, base, gistID string) *store.GistFile {
	return &store.GistFile{
		Filename: filename,
		Type:     detectGistFileType(filename),
		Language: detectGistLanguage(filename),
		RawURL:   base + "/raw/" + gistID + "/" + filename,
		Size:     len(content),
		Content:  content,
	}
}

func detectGistFileType(name string) string {
	if strings.HasSuffix(name, ".md") || strings.HasSuffix(name, ".markdown") {
		return "text/markdown"
	}
	if strings.HasSuffix(name, ".json") {
		return "application/json"
	}
	if strings.HasSuffix(name, ".txt") {
		return "text/plain"
	}
	return "text/plain"
}

func detectGistLanguage(name string) string {
	switch {
	case strings.HasSuffix(name, ".go"):
		return "Go"
	case strings.HasSuffix(name, ".js"):
		return "JavaScript"
	case strings.HasSuffix(name, ".py"):
		return "Python"
	case strings.HasSuffix(name, ".md"), strings.HasSuffix(name, ".markdown"):
		return "Markdown"
	case strings.HasSuffix(name, ".json"):
		return "JSON"
	case strings.HasSuffix(name, ".txt"):
		return "Text"
	}
	return ""
}

func parseSince(r *http.Request) time.Time {
	if v := r.URL.Query().Get("since"); v != "" {
		if t, err := time.Parse(time.RFC3339, v); err == nil {
			return t
		}
	}
	return time.Time{}
}
