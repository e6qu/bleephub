package bleephub

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"time"
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
//
// Every route addressing a gist by id goes through here. The sub-resources —
// revisions, forks, commits, comments, stars — were each deciding visibility
// for themselves, which meant each new one started out deciding nothing, and
// `GET /gists/{id}/{sha}` handed a non-public gist's file contents to an
// anonymous caller.
func (s *Server) visibleGist(w http.ResponseWriter, r *http.Request) *Gist {
	g := s.store.GetGist(r.PathValue("gist_id"))
	if g == nil || !s.viewerCanSeeGist(r.Context(), g) {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return nil
	}
	return g
}

// viewerCanSeeGist is the visibility rule itself: a public gist is everyone's,
// a non-public one is its owner's.
func (s *Server) viewerCanSeeGist(ctx context.Context, g *Gist) bool {
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
		ownerJSON = userToJSON(owner)
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
		writeGHValidationError(w, "Gist", "files", "missing_field")
		return
	}

	files := make(map[string]*GistFile)
	for name, f := range req.Files {
		if name == "" {
			writeGHValidationError(w, "Gist", "files", "invalid")
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

	var newFiles map[string]*GistFile
	var deleteFiles []string
	if req.Files != nil {
		newFiles = make(map[string]*GistFile)
		for name, f := range req.Files {
			if f == nil {
				deleteFiles = append(deleteFiles, name)
				continue
			}
			content := ""
			if f.Content != nil {
				content = *f.Content
			}
			filename := name
			if f.Filename != nil && *f.Filename != "" {
				filename = *f.Filename
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
	// A gist with no forks has an empty fork list, not a missing one; the
	// resolver above is what decides whether the gist is there.
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
		writeGHValidationError(w, "GistComment", "body", "missing_field")
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

func writeGistList(w http.ResponseWriter, r *http.Request, s *Server, gists []*Gist, includeContent bool) {
	items := make([]map[string]interface{}, len(gists))
	for i, g := range gists {
		items[i] = s.gistToJSON(g, r, includeContent)
	}
	writeJSON(w, http.StatusOK, paginateAndLink(w, r, items))
}

// gistView is the copy a response is rendered from: the stored gist snapshotted
// under the store lock, with this request's URLs derived onto the copy.
//
// The URLs are request-derived — they carry the Host the caller asked through —
// so writing them into the stored gist would publish one caller's host to every
// other reader, and rendering the stored gist directly would let a concurrent
// edit be serialized half-applied.
func (s *Server) gistView(g *Gist, r *http.Request) *Gist {
	view := s.snapshotGist(g)
	if view == nil {
		return nil
	}
	base := s.baseURL(r)
	view.URL = base + "/api/v3/gists/" + view.ID
	view.ForksURL = view.URL + "/forks"
	view.CommitsURL = view.URL + "/commits"
	view.CommentsURL = view.URL + "/comments"
	ownerLogin := ""
	if owner := s.store.GetUserByID(view.OwnerID); owner != nil {
		ownerLogin = owner.Login
		view.HTMLURL = base + "/" + owner.Login + "/" + view.ID
	}
	view.GitPullURL = base + "/gist/" + ownerLogin + "/" + view.ID + ".git"
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
	sortHistory(view.History)
	return view
}

// snapshotGist copies a stored gist, its files and its history under the store
// lock, so nothing a writer holds is shared with the copy.
func (s *Server) snapshotGist(g *Gist) *Gist {
	if g == nil {
		return nil
	}
	s.store.Mu.RLock()
	defer s.store.Mu.RUnlock()
	view := *g
	view.Files = make(map[string]*GistFile, len(g.Files))
	for name, f := range g.Files {
		file := *f
		view.Files[name] = &file
	}
	view.History = make([]*GistHistory, len(g.History))
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

func (s *Server) gistToJSON(g *Gist, r *http.Request, includeContent bool) map[string]interface{} {
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
			fileJSON["content"] = f.Content
		}
		files[name] = fileJSON
	}

	owner := s.store.GetUserByID(g.OwnerID)
	var ownerJSON interface{}
	if owner != nil {
		ownerJSON = userToJSON(owner)
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

	return map[string]interface{}{
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
		"forks":        []interface{}{},
		"history":      history,
		"created_at":   g.CreatedAt.Format(time.RFC3339),
		"updated_at":   g.UpdatedAt.Format(time.RFC3339),
	}
}

func (s *Server) gistCommentToJSON(c *GistComment, r *http.Request) map[string]interface{} {
	base := s.baseURL(r)
	user := s.store.GetUserByID(c.UserID)
	var userJSON interface{}
	if user != nil {
		userJSON = userToJSON(user)
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

func gistFileFromInput(filename, content, base, gistID string) *GistFile {
	return &GistFile{
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
