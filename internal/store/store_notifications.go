package store

import (
	"fmt"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
)

// ThreadSubscription tracks a user's subscription to a notification thread.
type ThreadSubscription struct {
	Subscribed bool      `json:"subscribed"`
	Ignored    bool      `json:"ignored"`
	Reason     string    `json:"reason"`
	CreatedAt  time.Time `json:"created_at"`
}

// UserNotificationsState persists per-user notification read/subscription state.
type UserNotificationsState struct {
	LastReadAt         time.Time            `json:"last_read_at,omitempty"`
	RepoLastReadAt     map[string]time.Time `json:"repo_last_read_at,omitempty"`
	ReadThreadIDs      map[string]time.Time `json:"read_thread_ids,omitempty"`
	DismissedThreadIDs map[string]bool      `json:"dismissed_thread_ids,omitempty"`
	// SavedThreadIDs is the user's bookmark set backing the web inbox's Saved
	// view (github.com-only; not part of the public REST surface).
	SavedThreadIDs map[string]bool                `json:"saved_thread_ids,omitempty"`
	Subscriptions  map[string]*ThreadSubscription `json:"subscriptions,omitempty"`
}

// notificationThreadSource is the underlying resource a notification thread points at.
type notificationThreadSource struct {
	Type        string
	ID          int
	RepoID      int
	Number      int
	Title       string
	Body        string
	UpdatedAt   time.Time
	AuthorID    int
	AssigneeIDs []int
	// RequestedReviewerIDs is set for pull requests: a user whose review is
	// requested gets a thread with reason "review_requested" (github), even
	// without watching the repo.
	RequestedReviewerIDs []int
}

// NotificationThreadRow is one accepted thread source gathered under the
// read lock, carrying everything buildThread needs so rendering can happen
// after the lock is released.
type NotificationThreadRow struct {
	src        notificationThreadSource
	Repo       *Repo `json:"-"`
	threadID   string
	reason     string
	unread     bool
	lastReadAt *time.Time
	saved      bool
}

// Saved reports whether the thread is in the user's saved (bookmark) set.
func (row NotificationThreadRow) Saved() bool { return row.saved }

// BuildNotificationThreads renders a (typically already-paginated) slice of
// rows into notification threads. buildThread is expensive per row (it embeds
// RepoToJSON and scans comments for the latest-comment URL), so callers should
// paginate the rows before calling this.
func (st *Store) BuildNotificationThreads(rows []NotificationThreadRow, baseURL string) []*NotificationThread {
	threads := make([]*NotificationThread, len(rows))
	for i, row := range rows {
		threads[i] = st.buildThread(row, baseURL)
	}
	return threads
}

// NotificationRowsFor applies a request-credential reach check after the
// store's read lock has been released. A nil predicate exposes no rows, making
// it impossible for an HTTP caller to accidentally request a
// credential-blind human-principal view.
func (st *Store) NotificationRowsFor(user *User, opts NotificationListOptions, canRead func(*Repo) bool) []NotificationThreadRow {
	if canRead == nil {
		return nil
	}
	st.Mu.RLock()

	state := st.notificationsStateViewLocked(user.ID)
	preferences := notificationPreferencesLocked(st.Users[user.ID])
	var rows []NotificationThreadRow

	// Precompute the set of (parentType, parentID) the viewer commented on in
	// a single pass over st.Comments, so notificationReason is an O(1) map
	// lookup per thread instead of an O(all-comments) scan per thread (which
	// made this handler O((issues+PRs) × comments)).
	commentedOn := make(map[string]struct{})
	// A comment body that @-mentions the viewer gives the thread reason
	// "mention" even when the viewer never participated otherwise. Precompute
	// per-viewer in one pass so the per-thread reason stays O(1).
	mentionedInComment := make(map[string]struct{})
	for _, c := range st.Comments {
		key := strings.ToLower(c.ParentType) + "\x1f" + strconv.Itoa(c.IssueID)
		if c.AuthorID == user.ID {
			commentedOn[key] = struct{}{}
		}
		if bodyMentions(c.Body, user.Login) {
			mentionedInComment[key] = struct{}{}
		}
	}

	add := func(src notificationThreadSource) {
		repo := st.Repos[src.RepoID]
		if repo == nil {
			return
		}
		if opts.RepoScope != "" && repo.FullName != opts.RepoScope {
			return
		}

		threadID := NotificationThreadID(src.Type, src.ID)
		// The default inbox hides done ("dismissed") threads; the web-only Done
		// view lists exactly those, and the Saved view lists the bookmark set
		// regardless of done/read state.
		switch opts.View {
		case NotificationViewDone:
			if !state.DismissedThreadIDs[threadID] {
				return
			}
		case NotificationViewSaved:
			if !state.SavedThreadIDs[threadID] {
				return
			}
		default:
			if state.DismissedThreadIDs[threadID] {
				return
			}
		}

		reason := notificationReasonWithComments(user, src, commentedOn, mentionedInComment)
		// Read access alone does not subscribe a user to every issue and pull
		// request in a repository. A non-participant receives the thread only
		// after explicitly subscribing to it or watching the repository.
		// Membership in the saved/done sets is itself evidence the thread was in
		// the user's inbox, so those views skip the subscription gate (the user
		// may have unwatched the repository since).
		if reason == "subscribed" && opts.View == "" {
			threadSubscription := state.Subscriptions[threadID]
			repoSubscription := st.RepoSubscriptions[RepoSubscriptionKey(user.ID, repo.ID)]
			explicitlySubscribed := threadSubscription != nil && threadSubscription.Subscribed && !threadSubscription.Ignored
			watchingRepo := repoSubscription != nil && repoSubscription.Subscribed && !repoSubscription.Ignored
			if !explicitlySubscribed && !watchingRepo {
				return
			}
			// Watching a repository notifies about activity from that point
			// on: github.com does not backfill the inbox with every
			// pre-existing issue and pull request (measured here as 4 → 30
			// threads on a plain subscribe). An explicit per-thread subscribe
			// still surfaces its own thread — the user picked exactly that
			// one — and a zero CreatedAt (records predating the stamp) keeps
			// the old inclusive behaviour rather than hiding a thread.
			if !explicitlySubscribed && watchingRepo &&
				!repoSubscription.CreatedAt.IsZero() &&
				src.UpdatedAt.Before(repoSubscription.CreatedAt) {
				return
			}
		}
		if opts.Participating && reason == "subscribed" {
			return
		}
		// The account's notification preferences decide whether this class of
		// thread reaches the web inbox at all (Settings → Notifications). A
		// preference that nothing reads would be a lie on the settings page.
		if !preferences.NotificationDeliversWeb(src.Type, reason) {
			return
		}

		updated := src.UpdatedAt
		if !opts.Since.IsZero() && updated.Before(opts.Since) {
			return
		}
		if !opts.Before.IsZero() && updated.After(opts.Before) {
			return
		}

		lastRead := state.LastReadAt
		if opts.RepoScope != "" {
			if r, ok := state.RepoLastReadAt[opts.RepoScope]; ok {
				if lastRead.IsZero() || r.After(lastRead) {
					lastRead = r
				}
			}
		}
		unread := lastRead.IsZero() || updated.After(lastRead)
		if readAt, ok := state.ReadThreadIDs[threadID]; ok {
			if !updated.After(readAt) {
				unread = false
			}
		}
		// Saved and done threads have typically been read already; those views
		// list their whole set, so the unread filter applies only to the inbox.
		if opts.View == "" && !opts.All && !unread {
			return
		}

		rows = append(rows, NotificationThreadRow{src, repo, threadID, reason, unread, lastReadAtFor(state, threadID), state.SavedThreadIDs[threadID]})
	}

	for _, issue := range st.Issues {
		add(notificationThreadSource{
			Type:        "Issue",
			ID:          issue.ID,
			RepoID:      issue.RepoID,
			Number:      issue.Number,
			Title:       issue.Title,
			Body:        issue.Body,
			UpdatedAt:   issue.UpdatedAt,
			AuthorID:    issue.AuthorID,
			AssigneeIDs: issue.AssigneeIDs,
		})
	}
	for _, pr := range st.PullRequests {
		add(notificationThreadSource{
			Type:                 "PullRequest",
			ID:                   pr.ID,
			RepoID:               pr.RepoID,
			Number:               pr.Number,
			Title:                pr.Title,
			Body:                 pr.Body,
			UpdatedAt:            pr.UpdatedAt,
			AuthorID:             pr.AuthorID,
			AssigneeIDs:          pr.AssigneeIDs,
			RequestedReviewerIDs: pr.RequestedReviewerIDs,
		})
	}

	st.Mu.RUnlock()

	rows = slices.DeleteFunc(rows, func(row NotificationThreadRow) bool {
		return !canRead(row.Repo)
	})
	sort.Slice(rows, func(i, j int) bool {
		if !rows[i].src.UpdatedAt.Equal(rows[j].src.UpdatedAt) {
			return rows[i].src.UpdatedAt.After(rows[j].src.UpdatedAt)
		}
		if rows[i].src.ID != rows[j].src.ID {
			return rows[i].src.ID > rows[j].src.ID
		}
		return rows[i].src.Type < rows[j].src.Type
	})

	return rows
}

// notificationReason derives the thread reason for a single thread. Callers
// hold st.Mu (it reads st.Comments directly). Used for one-off thread lookups;
// the list path uses notificationReasonWithComments with a precomputed set.
func notificationReason(st *Store, user *User, src notificationThreadSource) string {
	if src.AuthorID == user.ID {
		return "author"
	}
	for _, aid := range src.AssigneeIDs {
		if aid == user.ID {
			return "assign"
		}
	}
	for _, rid := range src.RequestedReviewerIDs {
		if rid == user.ID {
			return "review_requested"
		}
	}
	if bodyMentions(src.Body, user.Login) {
		return "mention"
	}
	parentType := strings.ToLower(src.Type)
	for _, c := range st.Comments {
		if c.AuthorID == user.ID && c.IssueID == src.ID && strings.ToLower(c.ParentType) == parentType {
			return "comment"
		}
	}
	return "subscribed"
}

// bodyMentions reports whether body @-mentions login at a word boundary (so
// "@octocat" matches but "email@octocat.com" and "@octocatx" do not).
func bodyMentions(body, login string) bool {
	if login == "" {
		return false
	}
	lb, ll := strings.ToLower(body), strings.ToLower(login)
	needle := "@" + ll
	wordByte := func(b byte) bool {
		return b == '-' || (b >= 'a' && b <= 'z') || (b >= '0' && b <= '9')
	}
	for from := 0; ; {
		i := strings.Index(lb[from:], needle)
		if i < 0 {
			return false
		}
		pos := from + i
		okBefore := pos == 0 || !wordByte(lb[pos-1])
		after := pos + len(needle)
		okAfter := after >= len(lb) || !wordByte(lb[after])
		if okBefore && okAfter {
			return true
		}
		from = pos + 1
	}
}

// notificationReasonWithComments derives the thread reason for the user using
// a precomputed set of the (parentType, parentID) pairs the user commented on
// (keyed "type\x1fid"). This keeps the per-thread cost O(1) rather than
// rescanning every comment in the store.
func notificationReasonWithComments(user *User, src notificationThreadSource, commentedOn, mentionedInComment map[string]struct{}) string {
	if src.AuthorID == user.ID {
		return "author"
	}
	for _, aid := range src.AssigneeIDs {
		if aid == user.ID {
			return "assign"
		}
	}
	for _, rid := range src.RequestedReviewerIDs {
		if rid == user.ID {
			return "review_requested"
		}
	}
	key := strings.ToLower(src.Type) + "\x1f" + strconv.Itoa(src.ID)
	if bodyMentions(src.Body, user.Login) {
		return "mention"
	}
	if _, ok := mentionedInComment[key]; ok {
		return "mention"
	}
	if _, ok := commentedOn[key]; ok {
		return "comment"
	}
	return "subscribed"
}

func NotificationThreadID(sourceType string, sourceID int) string {
	switch strings.ToLower(sourceType) {
	case "issue":
		return fmt.Sprintf("issue-%d", sourceID)
	case "pullrequest", "pull_request", "pull-request":
		return fmt.Sprintf("pull-request-%d", sourceID)
	default:
		return fmt.Sprintf("%s-%d", strings.ToLower(sourceType), sourceID)
	}
}

func parseNotificationThreadID(threadID string) (string, int, bool) {
	switch {
	case strings.HasPrefix(threadID, "issue-"):
		id, err := strconv.Atoi(strings.TrimPrefix(threadID, "issue-"))
		return "Issue", id, err == nil
	case strings.HasPrefix(threadID, "pull-request-"):
		id, err := strconv.Atoi(strings.TrimPrefix(threadID, "pull-request-"))
		return "PullRequest", id, err == nil
	default:
		id, err := strconv.Atoi(threadID)
		if err == nil {
			return "", id, true
		}
		return "", 0, false
	}
}

// lastReadAtFor derives the thread's last-read timestamp from the user's
// notification state. Callers hold st.Mu (state is store-owned).
func lastReadAtFor(state *UserNotificationsState, threadID string) *time.Time {
	if readAt, ok := state.ReadThreadIDs[threadID]; ok {
		t := readAt
		return &t
	}
	if !state.LastReadAt.IsZero() {
		t := state.LastReadAt
		return &t
	}
	return nil
}

// buildThread renders one gathered notification thread row. Must not be
// called with st.Mu held: it scans comments under its own read lock and
// embeds the repository via RepoToJSON, which derives counters under the
// store lock itself.
func (st *Store) buildThread(row NotificationThreadRow, baseURL string) *NotificationThread {
	src, repo, threadID := row.src, row.Repo, row.threadID
	base := baseURL
	apiBase := baseURL + "/api/v3"
	var subjectURL, latestCommentURL, htmlURL string
	if src.Type == "Issue" {
		subjectURL = fmt.Sprintf("%s/api/v3/repos/%s/issues/%d", base, repo.FullName, src.Number)
		latestCommentURL = subjectURL + "/comments"
		htmlURL = fmt.Sprintf("%s/%s/issues/%d", base, repo.FullName, src.Number)
	} else {
		subjectURL = fmt.Sprintf("%s/api/v3/repos/%s/pulls/%d", base, repo.FullName, src.Number)
		latestCommentURL = fmt.Sprintf("%s/api/v3/repos/%s/issues/%d/comments", base, repo.FullName, src.Number)
		htmlURL = fmt.Sprintf("%s/%s/pull/%d", base, repo.FullName, src.Number)
	}

	// Find the most recent comment to set latest_comment_url to a concrete comment when available.
	var latestCommentID int
	var latestCommentAt time.Time
	st.Mu.RLock()
	for _, c := range st.Comments {
		parentType := strings.ToLower(src.Type)
		if c.ParentType == parentType && c.IssueID == src.ID {
			if latestCommentID == 0 || c.CreatedAt.After(latestCommentAt) {
				latestCommentID = c.ID
				latestCommentAt = c.CreatedAt
			}
		}
	}
	st.Mu.RUnlock()
	if latestCommentID != 0 {
		latestCommentURL = fmt.Sprintf("%s/api/v3/repos/%s/issues/comments/%d", base, repo.FullName, latestCommentID)
	}

	return &NotificationThread{
		ID:               threadID,
		Repository:       RepoToJSON(repo, st, base),
		SubjectTitle:     src.Title,
		SubjectURL:       subjectURL,
		SubjectType:      src.Type,
		LatestCommentURL: latestCommentURL,
		HTMLURL:          htmlURL,
		Reason:           row.reason,
		Unread:           row.unread,
		UpdatedAt:        src.UpdatedAt,
		LastReadAt:       row.lastReadAt,
		SubscriptionURL:  fmt.Sprintf("%s/notifications/threads/%s/subscription", apiBase, threadID),
		URL:              fmt.Sprintf("%s/notifications/threads/%s", apiBase, threadID),
	}
}

// GetNotificationThreadFor is the credential-aware form used by HTTP
// handlers. The callback runs only after st.Mu is released; nil denies access.
func (st *Store) GetNotificationThreadFor(user *User, baseURL, threadID string, canRead func(*Repo) bool) *NotificationThread {
	if canRead == nil {
		return nil
	}
	sourceType, id, ok := parseNotificationThreadID(threadID)
	if !ok {
		return nil
	}

	var row *NotificationThreadRow
	st.Mu.RLock()
	if sourceType == "Issue" || sourceType == "" {
		if issue := st.Issues[id]; issue != nil {
			row = st.notificationIssueRowLocked(user, issue, NotificationThreadID("Issue", issue.ID))
		}
	}
	if row == nil && (sourceType == "PullRequest" || sourceType == "") {
		if pr := st.PullRequests[id]; pr != nil {
			row = st.notificationPullRequestRowLocked(user, pr, NotificationThreadID("PullRequest", pr.ID))
		}
	}
	st.Mu.RUnlock()

	if row == nil {
		return nil
	}
	if !canRead(row.Repo) {
		return nil
	}
	return st.buildThread(*row, baseURL)
}

func (st *Store) notificationIssueRowLocked(user *User, issue *Issue, threadID string) *NotificationThreadRow {
	repo := st.Repos[issue.RepoID]
	if repo == nil {
		return nil
	}
	src := notificationThreadSource{
		Type:        "Issue",
		ID:          issue.ID,
		RepoID:      issue.RepoID,
		Number:      issue.Number,
		Title:       issue.Title,
		UpdatedAt:   issue.UpdatedAt,
		AuthorID:    issue.AuthorID,
		AssigneeIDs: issue.AssigneeIDs,
	}
	state := st.notificationsStateViewLocked(user.ID)
	return &NotificationThreadRow{src, repo, threadID, notificationReason(st, user, src), true, lastReadAtFor(state, threadID), state.SavedThreadIDs[threadID]}
}

func (st *Store) notificationPullRequestRowLocked(user *User, pr *PullRequest, threadID string) *NotificationThreadRow {
	repo := st.Repos[pr.RepoID]
	if repo == nil {
		return nil
	}
	src := notificationThreadSource{
		Type:        "PullRequest",
		ID:          pr.ID,
		RepoID:      pr.RepoID,
		Number:      pr.Number,
		Title:       pr.Title,
		UpdatedAt:   pr.UpdatedAt,
		AuthorID:    pr.AuthorID,
		AssigneeIDs: pr.AssigneeIDs,
	}
	state := st.notificationsStateViewLocked(user.ID)
	return &NotificationThreadRow{src, repo, threadID, notificationReason(st, user, src), true, lastReadAtFor(state, threadID), state.SavedThreadIDs[threadID]}
}

// MarkNotificationsRead sets the global last-read timestamp for the user.
func (st *Store) MarkNotificationsRead(userID int, at time.Time, repoScope string) {
	st.Mu.Lock()
	defer st.Mu.Unlock()
	state := st.notificationsStateFor(userID)
	if repoScope != "" {
		if state.RepoLastReadAt == nil {
			state.RepoLastReadAt = map[string]time.Time{}
		}
		state.RepoLastReadAt[repoScope] = at
	} else {
		state.LastReadAt = at
	}
	st.persistNotificationsState(userID, state)
}

// MaxReadThreadIDs bounds the per-user read-marker set; PruneReadThreadSlack is
// how far past the cap it may grow before a prune, so the O(n log n) prune
// amortises to O(log n) per mark instead of firing on every mark at the cap.
// GitHub ages notifications out, so dropping the oldest read markers is faithful;
// without this the map — re-serialised in full on every mark — grows without
// limit as a user reads more threads (STORE-023).
const (
	MaxReadThreadIDs     = 50000
	PruneReadThreadSlack = 5000
)

// boundReadThreadIDs prunes the oldest read markers (by read-at time) once the
// set grows a slack beyond the cap. Caller holds st.Mu for writing.
func boundReadThreadIDs(state *UserNotificationsState) {
	if len(state.ReadThreadIDs) <= MaxReadThreadIDs+PruneReadThreadSlack {
		return
	}
	type marker struct {
		Id string `json:"-"`
		at time.Time
	}
	markers := make([]marker, 0, len(state.ReadThreadIDs))
	for id, at := range state.ReadThreadIDs {
		markers = append(markers, marker{id, at})
	}
	sort.Slice(markers, func(i, j int) bool { return markers[i].at.Before(markers[j].at) })
	for i := 0; i < len(markers)-MaxReadThreadIDs; i++ {
		delete(state.ReadThreadIDs, markers[i].Id)
	}
}

// MarkThreadRead records a thread as read for the user.
func (st *Store) MarkThreadRead(userID int, threadID string, at time.Time) {
	st.Mu.Lock()
	defer st.Mu.Unlock()
	state := st.notificationsStateFor(userID)
	if state.ReadThreadIDs == nil {
		state.ReadThreadIDs = map[string]time.Time{}
	}
	state.ReadThreadIDs[threadID] = at
	boundReadThreadIDs(state)
	st.persistNotificationsState(userID, state)
}

// MarkThreadDone dismisses a thread for the user. Done threads are retained
// (not deleted) so the web-only Done view can list them for review.
func (st *Store) MarkThreadDone(userID int, threadID string) {
	st.Mu.Lock()
	defer st.Mu.Unlock()
	state := st.notificationsStateFor(userID)
	if state.DismissedThreadIDs == nil {
		state.DismissedThreadIDs = map[string]bool{}
	}
	state.DismissedThreadIDs[threadID] = true
	st.persistNotificationsState(userID, state)
}

// SetThreadSaved adds or removes a thread from the user's saved (bookmark)
// set, backing the web inbox's Saved view.
func (st *Store) SetThreadSaved(userID int, threadID string, saved bool) {
	st.Mu.Lock()
	defer st.Mu.Unlock()
	state := st.notificationsStateFor(userID)
	if saved {
		state.SavedThreadIDs[threadID] = true
	} else {
		delete(state.SavedThreadIDs, threadID)
	}
	st.persistNotificationsState(userID, state)
}

// GetThreadSubscription returns the user's subscription for a thread.
func (st *Store) GetThreadSubscription(userID int, threadID string) *ThreadSubscription {
	st.Mu.RLock()
	defer st.Mu.RUnlock()
	state := st.notificationsStateViewLocked(userID)
	return state.Subscriptions[threadID]
}

// SetThreadSubscription sets or clears a thread subscription for the user.
func (st *Store) SetThreadSubscription(userID int, threadID string, sub *ThreadSubscription) {
	st.Mu.Lock()
	defer st.Mu.Unlock()
	state := st.notificationsStateFor(userID)
	if state.Subscriptions == nil {
		state.Subscriptions = map[string]*ThreadSubscription{}
	}
	if sub == nil {
		delete(state.Subscriptions, threadID)
	} else {
		state.Subscriptions[threadID] = sub
	}
	st.persistNotificationsState(userID, state)
}

// moveNotificationRepoKeyBatchLocked re-keys per-user notification read state
// from oldFull to newFull on a repo rename/transfer, staging its durable writes
// into batch so they commit with the rest of the re-key. Caller holds st.Mu.
func (st *Store) moveNotificationRepoKeyBatchLocked(batch *PersistBatch, oldFull, newFull string) {
	for userID, state := range st.NotificationsState {
		if state == nil || state.RepoLastReadAt == nil {
			continue
		}
		if at, ok := state.RepoLastReadAt[oldFull]; ok {
			state.RepoLastReadAt[newFull] = at
			delete(state.RepoLastReadAt, oldFull)
			batch.Put("notifications_state", strconv.Itoa(userID), state)
		}
	}
}

func (st *Store) deleteNotificationRepoKeyBatchLocked(batch *PersistBatch, fullName string) {
	for userID, state := range st.NotificationsState {
		if state == nil || state.RepoLastReadAt == nil {
			continue
		}
		if _, ok := state.RepoLastReadAt[fullName]; ok {
			delete(state.RepoLastReadAt, fullName)
			if batch != nil {
				batch.Put("notifications_state", strconv.Itoa(userID), state)
			} else {
				st.persistNotificationsState(userID, state)
			}
		}
	}
}

func (st *Store) deleteNotificationThreadStateBatchLocked(batch *PersistBatch, threadIDs []string) {
	if len(threadIDs) == 0 {
		return
	}
	for userID, state := range st.NotificationsState {
		if state == nil {
			continue
		}
		changed := false
		for _, threadID := range threadIDs {
			if state.ReadThreadIDs != nil {
				if _, ok := state.ReadThreadIDs[threadID]; ok {
					delete(state.ReadThreadIDs, threadID)
					changed = true
				}
			}
			if state.DismissedThreadIDs != nil {
				if _, ok := state.DismissedThreadIDs[threadID]; ok {
					delete(state.DismissedThreadIDs, threadID)
					changed = true
				}
			}
			if state.SavedThreadIDs != nil {
				if _, ok := state.SavedThreadIDs[threadID]; ok {
					delete(state.SavedThreadIDs, threadID)
					changed = true
				}
			}
			if state.Subscriptions != nil {
				if _, ok := state.Subscriptions[threadID]; ok {
					delete(state.Subscriptions, threadID)
					changed = true
				}
			}
		}
		if changed {
			if batch != nil {
				batch.Put("notifications_state", strconv.Itoa(userID), state)
			} else {
				st.persistNotificationsState(userID, state)
			}
		}
	}
}

// notificationsStateViewLocked returns the user's notification state for
// reading. Callers hold st.Mu (read or write). Unlike notificationsStateFor
// it never mutates the store: a user with no recorded state gets a fresh
// zero-value view that is not inserted into the map (nil inner maps are safe
// to read).
func (st *Store) notificationsStateViewLocked(userID int) *UserNotificationsState {
	if state, ok := st.NotificationsState[userID]; ok {
		return state
	}
	return &UserNotificationsState{}
}

// notificationsStateFor returns the user's notification state, lazily
// creating and normalizing it. Callers hold st.Mu for WRITING — it inserts
// into st.NotificationsState and repairs nil inner maps.
func (st *Store) notificationsStateFor(userID int) *UserNotificationsState {
	if st.NotificationsState == nil {
		st.NotificationsState = map[int]*UserNotificationsState{}
	}
	state, ok := st.NotificationsState[userID]
	if !ok {
		state = &UserNotificationsState{
			RepoLastReadAt:     map[string]time.Time{},
			ReadThreadIDs:      map[string]time.Time{},
			DismissedThreadIDs: map[string]bool{},
			SavedThreadIDs:     map[string]bool{},
			Subscriptions:      map[string]*ThreadSubscription{},
		}
		st.NotificationsState[userID] = state
	}
	// Ensure maps are non-nil after loading from persistence.
	if state.RepoLastReadAt == nil {
		state.RepoLastReadAt = map[string]time.Time{}
	}
	if state.ReadThreadIDs == nil {
		state.ReadThreadIDs = map[string]time.Time{}
	}
	if state.DismissedThreadIDs == nil {
		state.DismissedThreadIDs = map[string]bool{}
	}
	if state.SavedThreadIDs == nil {
		state.SavedThreadIDs = map[string]bool{}
	}
	if state.Subscriptions == nil {
		state.Subscriptions = map[string]*ThreadSubscription{}
	}
	return state
}

func (st *Store) persistNotificationsState(userID int, state *UserNotificationsState) {
	if st.Persist != nil {
		st.Persist.MustPut("notifications_state", strconv.Itoa(userID), state)
	}
}

// NotificationThread is the wire-shape of a GitHub notification thread.
type NotificationThread struct {
	ID               string
	Repository       map[string]interface{}
	SubjectTitle     string
	SubjectURL       string
	SubjectType      string
	LatestCommentURL string
	HTMLURL          string
	Reason           string
	Unread           bool
	UpdatedAt        time.Time
	LastReadAt       *time.Time
	SubscriptionURL  string
	URL              string
}

// Web-only notification inbox views (/ui-data): the empty view is the normal
// inbox, Saved is the bookmark set, Done is the reviewable dismissed set.
const (
	NotificationViewSaved = "saved"
	NotificationViewDone  = "done"
)

// NotificationListOptions controls filtering of ListNotifications.
type NotificationListOptions struct {
	All           bool
	Participating bool
	Since         time.Time
	Before        time.Time
	RepoScope     string
	// View selects a web-only inbox view ("", NotificationViewSaved, or
	// NotificationViewDone). The public REST listings always use "".
	View string
}
