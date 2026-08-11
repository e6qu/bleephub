package bleephub

import (
	"net/http"
	"strconv"
	"strings"
	"time"
)

// GitHub Copilot coding agent tasks — the /agents/tasks and
// /agents/repos/{owner}/{repo}/tasks surfaces. A task is created against
// a repository with a prompt; each task carries the session spawned for
// it. bleephub stores the task/session entities and their state; the
// Copilot coding agent's execution engine is not part of bleephub, so a
// created task stays "queued" (nothing dequeues it), exactly what the
// store knows to be true.

func (s *Server) registerGHAgentsTasksRoutes() {
	s.route("GET /api/v3/agents/tasks", s.handleListAgentTasks)
	s.route("GET /api/v3/agents/tasks/{task_id}", s.handleGetAgentTask)
	s.route("GET /api/v3/agents/repos/{owner}/{repo}/tasks", s.handleListAgentTasksForRepo)
	s.route("POST /api/v3/agents/repos/{owner}/{repo}/tasks", s.handleCreateAgentTaskInRepo)
	s.route("GET /api/v3/agents/repos/{owner}/{repo}/tasks/{task_id}", s.handleGetAgentTaskInRepo)
}

// --- store methods ---

// --- JSON rendering ---

func (s *Server) agentTaskJSON(t *AgentTask, baseURL string) map[string]interface{} {
	repo := s.store.GetRepoByID(t.RepoID)
	fullName := ""
	if repo != nil {
		fullName = repo.FullName
	}

	var archivedAt interface{}
	if t.ArchivedAt != nil {
		archivedAt = t.ArchivedAt.UTC().Format(time.RFC3339)
	}

	return map[string]interface{}{
		"id":            t.ID,
		"url":           baseURL + "/api/v3/agents/repos/" + fullName + "/tasks/" + t.ID,
		"html_url":      baseURL + "/" + fullName + "/copilot/tasks/" + t.ID,
		"name":          t.Name,
		"creator":       map[string]interface{}{"id": t.CreatorID},
		"creator_type":  t.CreatorType,
		"owner":         map[string]interface{}{"id": t.OwnerID},
		"repository":    map[string]interface{}{"id": t.RepoID},
		"state":         t.State,
		"session_count": len(t.Sessions),
		"artifacts":     []map[string]interface{}{},
		"archived_at":   archivedAt,
		"created_at":    t.CreatedAt.UTC().Format(time.RFC3339),
		"updated_at":    t.UpdatedAt.UTC().Format(time.RFC3339),
	}
}

func agentTaskSessionJSON(t *AgentTask, sess AgentTaskSession) map[string]interface{} {
	out := map[string]interface{}{
		"id":         sess.ID,
		"name":       sess.Name,
		"owner":      map[string]interface{}{"id": t.OwnerID},
		"user":       map[string]interface{}{"id": t.CreatorID},
		"repository": map[string]interface{}{"id": t.RepoID},
		"task_id":    t.ID,
		"state":      sess.State,
		"prompt":     sess.Prompt,
		"created_at": sess.CreatedAt.UTC().Format(time.RFC3339),
		"updated_at": sess.UpdatedAt.UTC().Format(time.RFC3339),
	}
	if sess.HeadRef != "" {
		out["head_ref"] = sess.HeadRef
	}
	if sess.BaseRef != "" {
		out["base_ref"] = sess.BaseRef
	}
	if sess.Model != "" {
		out["model"] = sess.Model
	}
	return out
}

func (s *Server) agentTaskDetailJSON(t *AgentTask, baseURL string) map[string]interface{} {
	out := s.agentTaskJSON(t, baseURL)
	sessions := make([]map[string]interface{}, 0, len(t.Sessions))
	for _, sess := range t.Sessions {
		sessions = append(sessions, agentTaskSessionJSON(t, sess))
	}
	out["sessions"] = sessions
	return out
}

// --- handlers ---

// parseAgentTaskFilter reads the documented list query parameters.
func parseAgentTaskFilter(r *http.Request) agentTaskFilter {
	q := r.URL.Query()
	f := agentTaskFilter{
		SortField: q.Get("sort"),
		Direction: q.Get("direction"),
	}
	if v := q.Get("state"); v != "" {
		for _, s := range strings.Split(v, ",") {
			if s = strings.TrimSpace(s); s != "" {
				f.States = append(f.States, s)
			}
		}
	}
	if q.Get("is_archived") == "true" {
		f.IsArchived = true
	}
	if v := q.Get("since"); v != "" {
		if ts, err := time.Parse(time.RFC3339, v); err == nil {
			f.Since = &ts
		}
	}
	for _, raw := range q["creator_id"] {
		if id, err := strconv.Atoi(raw); err == nil {
			f.CreatorIDs = append(f.CreatorIDs, id)
		}
	}
	return f
}

func (s *Server) writeAgentTaskList(w http.ResponseWriter, r *http.Request, f agentTaskFilter) {
	tasks, totalActive, totalArchived := s.store.ListAgentTasks(f)
	page := paginateAndLink(w, r, tasks)
	baseURL := s.baseURL(r)
	out := make([]map[string]interface{}, 0, len(page))
	for _, t := range page {
		out = append(out, s.agentTaskJSON(t, baseURL))
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"tasks":                out,
		"total_active_count":   totalActive,
		"total_archived_count": totalArchived,
	})
}

// handleListAgentTasks lists the authenticated user's tasks.
func (s *Server) handleListAgentTasks(w http.ResponseWriter, r *http.Request) {
	user := ghUserFromContext(r.Context())
	if user == nil {
		writeGHError(w, http.StatusUnauthorized, "Requires authentication")
		return
	}
	f := parseAgentTaskFilter(r)
	f.CreatorID = user.ID
	s.writeAgentTaskList(w, r, f)
}

func (s *Server) handleGetAgentTask(w http.ResponseWriter, r *http.Request) {
	user := ghUserFromContext(r.Context())
	if user == nil {
		writeGHError(w, http.StatusUnauthorized, "Requires authentication")
		return
	}
	task := s.store.GetAgentTask(r.PathValue("task_id"))
	if task == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	repo := s.store.GetRepoByID(task.RepoID)
	if repo == nil || !s.viewerCanReadRepo(r.Context(), repo) {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	writeJSON(w, http.StatusOK, s.agentTaskDetailJSON(task, s.baseURL(r)))
}

func (s *Server) handleListAgentTasksForRepo(w http.ResponseWriter, r *http.Request) {
	user := ghUserFromContext(r.Context())
	if user == nil {
		writeGHError(w, http.StatusUnauthorized, "Requires authentication")
		return
	}
	repo := s.store.GetRepo(r.PathValue("owner"), r.PathValue("repo"))
	if repo == nil || !s.viewerCanReadRepo(r.Context(), repo) {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	f := parseAgentTaskFilter(r)
	f.RepoID = repo.ID
	s.writeAgentTaskList(w, r, f)
}

func (s *Server) handleCreateAgentTaskInRepo(w http.ResponseWriter, r *http.Request) {
	user := ghUserFromContext(r.Context())
	if user == nil {
		writeGHError(w, http.StatusUnauthorized, "Requires authentication")
		return
	}
	repo := s.store.GetRepo(r.PathValue("owner"), r.PathValue("repo"))
	if repo == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	if !s.viewerCanPushRepo(r.Context(), repo) {
		writeGHError(w, http.StatusForbidden, "Must have write access to Repository.")
		return
	}

	var req struct {
		Prompt            string `json:"prompt"`
		Model             string `json:"model"`
		CreatePullRequest bool   `json:"create_pull_request"`
		BaseRef           string `json:"base_ref"`
		HeadRef           string `json:"head_ref"`
	}
	if !decodeJSONBody(w, r, &req) {
		return
	}
	if req.Prompt == "" {
		// This op's documented 422 items declare only code/message — the
		// resource/field members of the generic helper are undeclared there.
		writeJSON(w, http.StatusUnprocessableEntity, map[string]interface{}{
			"message":           "Validation Failed",
			"documentation_url": "https://docs.github.com/rest",
			"errors": []map[string]string{
				{"code": "missing_field", "message": "prompt is required"},
			},
		})
		return
	}

	task := s.store.CreateAgentTask(repo, user, req.Prompt, req.Model, req.CreatePullRequest, req.BaseRef, req.HeadRef)
	writeJSON(w, http.StatusCreated, s.agentTaskJSON(task, s.baseURL(r)))
}

func (s *Server) handleGetAgentTaskInRepo(w http.ResponseWriter, r *http.Request) {
	user := ghUserFromContext(r.Context())
	if user == nil {
		writeGHError(w, http.StatusUnauthorized, "Requires authentication")
		return
	}
	repo := s.store.GetRepo(r.PathValue("owner"), r.PathValue("repo"))
	if repo == nil || !s.viewerCanReadRepo(r.Context(), repo) {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	task := s.store.GetAgentTask(r.PathValue("task_id"))
	if task == nil || task.RepoID != repo.ID {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	writeJSON(w, http.StatusOK, s.agentTaskDetailJSON(task, s.baseURL(r)))
}
