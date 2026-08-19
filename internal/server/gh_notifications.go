package bleephub

import (
	"net/http"
	"time"

	"github.com/e6qu/bleephub/internal/store"
)

func (s *Server) registerGHNotificationsRoutes() {
	s.route("GET /api/v3/notifications", s.handleListNotifications)
	s.route("PUT /api/v3/notifications", s.handleMarkNotificationsRead)
	s.route("GET /api/v3/repos/{owner}/{repo}/notifications", s.handleListRepoNotifications)
	s.route("PUT /api/v3/repos/{owner}/{repo}/notifications", s.handleMarkRepoNotificationsRead)
	s.route("GET /api/v3/notifications/threads/{thread_id}", s.handleGetThread)
	s.route("PATCH /api/v3/notifications/threads/{thread_id}", s.handlePatchThread)
	s.route("DELETE /api/v3/notifications/threads/{thread_id}", s.handleDeleteThread)
	s.route("GET /api/v3/notifications/threads/{thread_id}/subscription", s.handleGetThreadSubscription)
	s.route("PUT /api/v3/notifications/threads/{thread_id}/subscription", s.handleSetThreadSubscription)
	s.route("DELETE /api/v3/notifications/threads/{thread_id}/subscription", s.handleDeleteThreadSubscription)

	// The web inbox's Saved (bookmark) flag and reviewable Done list exist only
	// on github.com — neither is a public REST operation — so they live under
	// the browser-only /ui-data namespace. The Done set reuses the REST
	// mark-done state (DELETE thread above); done threads are retained, not
	// deleted, which is what makes the Done view listable.
	s.route("GET /ui-data/notifications", s.handleUIListNotifications)
	s.route("PUT /ui-data/notifications/threads/{thread_id}/saved", s.handleSaveThread)
	s.route("DELETE /ui-data/notifications/threads/{thread_id}/saved", s.handleUnsaveThread)
}

func parseNotificationListOptions(w http.ResponseWriter, r *http.Request) (store.NotificationListOptions, bool) {
	opts := store.NotificationListOptions{}
	q := r.URL.Query()
	for name, target := range map[string]*bool{
		"all": &opts.All, "participating": &opts.Participating,
	} {
		if value := q.Get(name); value != "" {
			if value != "true" && value != "false" {
				store.WriteGHValidationError(w, "Notification", name, "invalid")
				return store.NotificationListOptions{}, false
			}
			*target = value == "true"
		}
	}
	if v := q.Get("since"); v != "" {
		t, err := time.Parse(time.RFC3339, v)
		if err != nil {
			store.WriteGHValidationError(w, "Notification", "since", "invalid")
			return store.NotificationListOptions{}, false
		}
		opts.Since = t
	}
	if v := q.Get("before"); v != "" {
		t, err := time.Parse(time.RFC3339, v)
		if err != nil {
			store.WriteGHValidationError(w, "Notification", "before", "invalid")
			return store.NotificationListOptions{}, false
		}
		opts.Before = t
	}
	return opts, true
}

func (s *Server) notificationRows(r *http.Request, user *store.User, opts store.NotificationListOptions) []store.NotificationThreadRow {
	return s.store.NotificationRowsFor(user, opts, func(repo *store.Repo) bool {
		return s.viewerCanReadRepo(r.Context(), repo)
	})
}

func (s *Server) notificationThread(r *http.Request, user *store.User, threadID string) *store.NotificationThread {
	return s.store.GetNotificationThreadFor(user, s.baseURL(r), threadID, func(repo *store.Repo) bool {
		return s.viewerCanReadRepo(r.Context(), repo)
	})
}

func (s *Server) handleListNotifications(w http.ResponseWriter, r *http.Request) {
	user := ghUserFromContext(r.Context())
	if user == nil {
		writeGHError(w, http.StatusUnauthorized, "Requires authentication")
		return
	}

	opts, ok := parseNotificationListOptions(w, r)
	if !ok {
		return
	}
	rows := s.notificationRows(r, user, opts)
	rows = paginateAndLink(w, r, rows)
	threads := s.store.BuildNotificationThreads(rows, s.baseURL(r))
	writeNotificationThreads(w, r, threads)
}

// writeNotificationThreads renders a notification thread page, advertising the
// newest thread's UpdatedAt as Last-Modified and short-circuiting a matching
// conditional GET with a 304 (REST-031). Threads are sorted newest-first, so
// the page-one client (the polling case) sees the exact global modification
// time.
func writeNotificationThreads(w http.ResponseWriter, r *http.Request, threads []*store.NotificationThread) {
	var newest time.Time
	out := make([]map[string]interface{}, len(threads))
	for i, t := range threads {
		out[i] = threadToJSON(t)
		if t.UpdatedAt.After(newest) {
			newest = t.UpdatedAt
		}
	}
	if writeLastModified(w, r, newest) {
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleMarkNotificationsRead(w http.ResponseWriter, r *http.Request) {
	user := ghUserFromContext(r.Context())
	if user == nil {
		writeGHError(w, http.StatusUnauthorized, "Requires authentication")
		return
	}

	at := s.currentTime()
	var body struct {
		LastReadAt string `json:"last_read_at"`
	}
	if !decodeJSONBodyOptional(w, r, &body) {
		return
	}
	if body.LastReadAt != "" {
		t, err := time.Parse(time.RFC3339, body.LastReadAt)
		if err != nil {
			store.WriteGHValidationError(w, "Notification", "last_read_at", "invalid")
			return
		}
		at = t
	}
	s.store.MarkNotificationsRead(user.ID, at, "")
	w.WriteHeader(http.StatusAccepted)
}

func (s *Server) handleListRepoNotifications(w http.ResponseWriter, r *http.Request) {
	user := ghUserFromContext(r.Context())
	if user == nil {
		writeGHError(w, http.StatusUnauthorized, "Requires authentication")
		return
	}

	owner, repoName := r.PathValue("owner"), r.PathValue("repo")
	repo := s.store.GetRepoByFullName(owner + "/" + repoName)
	if repo == nil || !s.viewerCanReadRepo(r.Context(), repo) {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}

	opts, ok := parseNotificationListOptions(w, r)
	if !ok {
		return
	}
	opts.RepoScope = repo.FullName
	rows := s.notificationRows(r, user, opts)
	rows = paginateAndLink(w, r, rows)
	threads := s.store.BuildNotificationThreads(rows, s.baseURL(r))
	writeNotificationThreads(w, r, threads)
}

func (s *Server) handleMarkRepoNotificationsRead(w http.ResponseWriter, r *http.Request) {
	user := ghUserFromContext(r.Context())
	if user == nil {
		writeGHError(w, http.StatusUnauthorized, "Requires authentication")
		return
	}

	owner, repoName := r.PathValue("owner"), r.PathValue("repo")
	repo := s.store.GetRepoByFullName(owner + "/" + repoName)
	if repo == nil || !s.viewerCanReadRepo(r.Context(), repo) {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}

	at := s.currentTime()
	var body struct {
		LastReadAt string `json:"last_read_at"`
	}
	if !decodeJSONBodyOptional(w, r, &body) {
		return
	}
	if body.LastReadAt != "" {
		t, err := time.Parse(time.RFC3339, body.LastReadAt)
		if err != nil {
			store.WriteGHValidationError(w, "Notification", "last_read_at", "invalid")
			return
		}
		at = t
	}
	s.store.MarkNotificationsRead(user.ID, at, repo.FullName)
	w.WriteHeader(http.StatusAccepted)
}

func (s *Server) handleGetThread(w http.ResponseWriter, r *http.Request) {
	user := ghUserFromContext(r.Context())
	if user == nil {
		writeGHError(w, http.StatusUnauthorized, "Requires authentication")
		return
	}

	thread := s.notificationThread(r, user, r.PathValue("thread_id"))
	if thread == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	writeJSON(w, http.StatusOK, threadToJSON(thread))
}

func (s *Server) handlePatchThread(w http.ResponseWriter, r *http.Request) {
	user := ghUserFromContext(r.Context())
	if user == nil {
		writeGHError(w, http.StatusUnauthorized, "Requires authentication")
		return
	}

	threadID := r.PathValue("thread_id")
	thread := s.notificationThread(r, user, threadID)
	if thread == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}

	var body struct{}
	if !decodeJSONBodyOptional(w, r, &body) {
		return
	}

	s.store.MarkThreadRead(user.ID, threadID, s.currentTime())
	w.WriteHeader(http.StatusResetContent)
}

func (s *Server) handleGetThreadSubscription(w http.ResponseWriter, r *http.Request) {
	user := ghUserFromContext(r.Context())
	if user == nil {
		writeGHError(w, http.StatusUnauthorized, "Requires authentication")
		return
	}

	threadID := r.PathValue("thread_id")
	thread := s.notificationThread(r, user, threadID)
	if thread == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}

	sub := s.store.GetThreadSubscription(user.ID, threadID)
	if sub == nil {
		// GitHub returns 404 when no explicit subscription exists.
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	writeJSON(w, http.StatusOK, threadSubscriptionToJSON(sub, thread.SubscriptionURL))
}

func (s *Server) handleSetThreadSubscription(w http.ResponseWriter, r *http.Request) {
	user := ghUserFromContext(r.Context())
	if user == nil {
		writeGHError(w, http.StatusUnauthorized, "Requires authentication")
		return
	}

	threadID := r.PathValue("thread_id")
	thread := s.notificationThread(r, user, threadID)
	if thread == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}

	var body struct {
		Ignored bool `json:"ignored"`
	}
	if !decodeJSONBodyOptional(w, r, &body) {
		return
	}

	// github documents only `ignored` on this PUT — subscribing is implied by the
	// request itself, so a thread is subscribed unless it is explicitly ignored.
	sub := &store.ThreadSubscription{
		Subscribed: !body.Ignored,
		Ignored:    body.Ignored,
		Reason:     thread.Reason,
		CreatedAt:  s.currentTime(),
	}
	s.store.SetThreadSubscription(user.ID, threadID, sub)
	writeJSON(w, http.StatusOK, threadSubscriptionToJSON(sub, thread.SubscriptionURL))
}

func (s *Server) handleDeleteThreadSubscription(w http.ResponseWriter, r *http.Request) {
	user := ghUserFromContext(r.Context())
	if user == nil {
		writeGHError(w, http.StatusUnauthorized, "Requires authentication")
		return
	}

	threadID := r.PathValue("thread_id")
	if s.notificationThread(r, user, threadID) == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}

	s.store.SetThreadSubscription(user.ID, threadID, nil)
	w.WriteHeader(http.StatusNoContent)
}

func threadToJSON(t *store.NotificationThread) map[string]interface{} {
	m := map[string]interface{}{
		"id":         t.ID,
		"repository": t.Repository,
		"subject": map[string]interface{}{
			"title":              t.SubjectTitle,
			"url":                t.SubjectURL,
			"latest_comment_url": t.LatestCommentURL,
			"type":               t.SubjectType,
		},
		"reason":           t.Reason,
		"unread":           t.Unread,
		"updated_at":       t.UpdatedAt.UTC().Format(time.RFC3339),
		"last_read_at":     nil,
		"subscription_url": t.SubscriptionURL,
		"url":              t.URL,
	}
	if t.LastReadAt != nil {
		m["last_read_at"] = t.LastReadAt.UTC().Format(time.RFC3339)
	}
	return m
}

func threadSubscriptionToJSON(sub *store.ThreadSubscription, url string) map[string]interface{} {
	return map[string]interface{}{
		"subscribed": sub.Subscribed,
		"ignored":    sub.Ignored,
		"reason":     sub.Reason,
		"created_at": sub.CreatedAt.UTC().Format(time.RFC3339),
		"url":        url,
		"thread_url": url,
	}
}

// handleUIListNotifications serves the web-only inbox views: ?view=saved lists
// the viewer's bookmarked threads, ?view=done the threads marked done. Items
// are REST thread shapes plus a simulator-only `saved` flag.
func (s *Server) handleUIListNotifications(w http.ResponseWriter, r *http.Request) {
	user := ghUserFromContext(r.Context())
	if user == nil {
		writeGHError(w, http.StatusUnauthorized, "Requires authentication")
		return
	}

	view := r.URL.Query().Get("view")
	if view != store.NotificationViewSaved && view != store.NotificationViewDone {
		store.WriteGHValidationError(w, "Notification", "view", "invalid")
		return
	}
	opts, ok := parseNotificationListOptions(w, r)
	if !ok {
		return
	}
	opts.View = view

	rows := s.notificationRows(r, user, opts)
	rows = paginateAndLink(w, r, rows)
	threads := s.store.BuildNotificationThreads(rows, s.baseURL(r))
	out := make([]map[string]interface{}, len(threads))
	for i, t := range threads {
		out[i] = threadToJSON(t)
		out[i]["saved"] = rows[i].Saved()
	}
	writeJSON(w, http.StatusOK, out)
}

// handleSaveThread implements PUT /ui-data/notifications/threads/{id}/saved:
// bookmark the thread for the viewer (the web inbox's Saved view).
func (s *Server) handleSaveThread(w http.ResponseWriter, r *http.Request) {
	s.setThreadSaved(w, r, true)
}

// handleUnsaveThread removes the bookmark set by handleSaveThread.
func (s *Server) handleUnsaveThread(w http.ResponseWriter, r *http.Request) {
	s.setThreadSaved(w, r, false)
}

func (s *Server) setThreadSaved(w http.ResponseWriter, r *http.Request, saved bool) {
	user := ghUserFromContext(r.Context())
	if user == nil {
		writeGHError(w, http.StatusUnauthorized, "Requires authentication")
		return
	}

	threadID := r.PathValue("thread_id")
	// Resolving via notificationThread enforces the viewer's repository reach:
	// a thread in a repository they cannot read does not exist for them.
	if s.notificationThread(r, user, threadID) == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	s.store.SetThreadSaved(user.ID, threadID, saved)
	w.WriteHeader(http.StatusNoContent)
}

// handleDeleteThread implements DELETE /notifications/threads/{thread_id}
// ("Mark a thread as done"): the thread is dismissed from the user's
// notification list.
func (s *Server) handleDeleteThread(w http.ResponseWriter, r *http.Request) {
	user := ghUserFromContext(r.Context())
	if user == nil {
		writeGHError(w, http.StatusUnauthorized, "Requires authentication")
		return
	}

	threadID := r.PathValue("thread_id")
	thread := s.notificationThread(r, user, threadID)
	if thread == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	s.store.MarkThreadDone(user.ID, threadID)
	w.WriteHeader(http.StatusNoContent)
}
