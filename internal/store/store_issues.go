package store

import (
	"fmt"
	"sort"
	"strconv"
	"time"
)

// IssueLabel is named to avoid collision with the agent Label type in store.go.
type IssueLabel struct {
	ID          int
	NodeID      string
	RepoID      int
	Name        string
	Description string
	Color       string // hex without #, e.g. "d73a4a"
	Default     bool
	Archived    bool
	CreatedAt   time.Time
}

// MilestoneState is a milestone's state; GitHub has only open and closed. (The
// "all" list filter is not a state and stays a plain string.)
type MilestoneState string

const (
	MilestoneStateOpen   MilestoneState = "open"
	MilestoneStateClosed MilestoneState = "closed"
)

type Milestone struct {
	ID          int
	NodeID      string
	RepoID      int
	Number      int // per-repo sequential
	Title       string
	Description string
	State       MilestoneState
	CreatorID   int // user who created the milestone
	DueOn       *time.Time
	ClosedAt    *time.Time // set when state transitions to "closed"
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

// LockReason is why a conversation is locked. GitHub accepts only these values
// (lowercase kebab-case in REST, uppercased in the GraphQL enum). Empty means
// locked without a stated reason.
type LockReason string

const (
	LockReasonNone      LockReason = ""
	LockReasonOffTopic  LockReason = "off-topic"
	LockReasonTooHeated LockReason = "too heated"
	LockReasonResolved  LockReason = "resolved"
	LockReasonSpam      LockReason = "spam"
)

type Issue struct {
	ID               int
	NodeID           string
	Number           int // per-repo sequential
	RepoID           int
	Title            string
	Body             string
	State            string // "OPEN", "CLOSED"
	StateReason      string // "", "COMPLETED", "NOT_PLANNED"
	AuthorID         int
	AssigneeIDs      []int
	LabelIDs         []int
	MilestoneID      int // 0 = none
	IssueTypeID      int // 0 = none; organization issue type ID
	Locked           bool
	ActiveLockReason LockReason // empty = locked without a stated reason
	CreatedAt        time.Time
	UpdatedAt        time.Time
	ClosedAt         *time.Time
	PinnedAt         *time.Time // non-nil while pinned to the repo issues list; doubles as the pin order
	PinnedByID       int        // user who pinned the issue; 0 when not pinned
	// LinkedBranches are the branches recorded as work on this issue; see linked_branches.go.
	LinkedBranches []LinkedBranch
	// DuplicateOfID is the issue this one was closed as a duplicate of (0 = none).
	DuplicateOfID int
}

// Comment is a conversation comment on an issue or PR. GitHub stores both in
// one table (PRs are issues internally); ParentType discriminates and IssueID
// holds the issue or PR database ID accordingly.
type Comment struct {
	ID              int
	NodeID          string
	ParentType      string // "issue" or "pull_request"
	IssueID         int    // issue or PR database ID per ParentType
	AuthorID        int
	Body            string
	CreatedAt       time.Time
	UpdatedAt       time.Time
	LastEditedAt    *time.Time // nil when never edited after creation
	EditorID        int        // user who performed the last edit; 0 when never edited
	MinimizedReason string     // "" when not minimized; otherwise OFF_TOPIC / OUTDATED / RESOLVED / DUPLICATE / SPAM / ABUSE
	MinimizedByID   int        // user who minimized; 0 when not minimized
	Pinned          bool       // pinned comments appear first in some GitHub UIs
}

func cloneComment(comment *Comment) *Comment {
	if comment == nil {
		return nil
	}
	copy := *comment
	if comment.LastEditedAt != nil {
		edited := *comment.LastEditedAt
		copy.LastEditedAt = &edited
	}
	return &copy
}

// IssueEvent is an event in an issue's or PR's timeline. Event matches GitHub's
// REST issue-event type names ("opened", "closed", "labeled", ...). ParentType
// selects IssueID's ID space — "issue" (st.Issues) or "pull_request"
// (st.PullRequests); the two share a per-repo number sequence but independent global IDs.
type IssueEvent struct {
	ID                  int
	NodeID              string
	RepoID              int
	ParentType          string
	IssueID             int
	ActorID             int
	Event               string
	CommitID            string
	CommitURL           string
	CreatedAt           time.Time
	LabelID             int
	AssigneeID          int
	AssignerID          int
	MilestoneID         int
	CommentID           int
	RequestedReviewerID int
	LockReason          string
	RenameFrom          string
	RenameTo            string
}

// buildIssueEventLocked registers an in-memory IssueEvent but does not persist
// it. Callers set optional fields then persist once, so the durable row is
// never first written under the wrong parent type and rewritten. Callers hold st.Mu.
func (st *Store) buildIssueEventLocked(repoID, issueID, actorID int, event, parentType string) *IssueEvent {
	e := &IssueEvent{
		ID:         st.NextIssueEventID,
		NodeID:     fmt.Sprintf("IE_kgDO%08d", st.NextIssueEventID),
		RepoID:     repoID,
		ParentType: parentType,
		IssueID:    issueID,
		ActorID:    actorID,
		Event:      event,
		CreatedAt:  st.CurrentTime(),
	}
	st.NextIssueEventID++
	st.IssueEvents[e.ID] = e
	return e
}

func (st *Store) persistIssueEventLocked(e *IssueEvent) {
	if st.Persist != nil {
		st.Persist.MustPut("issue_events", strconv.Itoa(e.ID), e)
	}
}

// recordIssueEventBatchLocked builds an issue-parented IssueEvent and stages its
// persist into batch so it commits with the issue row (STORE-001/002). Callers hold st.Mu.
func (st *Store) recordIssueEventBatchLocked(batch *PersistBatch, repoID, issueID, actorID int, event string) *IssueEvent {
	e := st.buildIssueEventLocked(repoID, issueID, actorID, event, "issue")
	batch.Put("issue_events", strconv.Itoa(e.ID), e)
	return e
}

// recordPullRequestEventLocked attaches an IssueEvent to a PR; callers hold
// st.Mu. commitID and requestedReviewerID are optional.
func (st *Store) recordPullRequestEventLocked(repoID, prID, actorID int, event, commitID string, requestedReviewerID int) *IssueEvent {
	e := st.buildIssueEventLocked(repoID, prID, actorID, event, "pull_request")
	e.CommitID = commitID
	e.RequestedReviewerID = requestedReviewerID
	st.persistIssueEventLocked(e)
	return e
}

// recordPullRequestEventBatchLocked builds a PR IssueEvent and stages its
// persist into batch so a multi-event mutation commits every event with the PR
// row atomically (STORE-001/002). Callers hold st.Mu.
func (st *Store) recordPullRequestEventBatchLocked(batch *PersistBatch, repoID, prID, actorID int, event, commitID string, requestedReviewerID int) {
	e := st.buildIssueEventLocked(repoID, prID, actorID, event, "pull_request")
	e.CommitID = commitID
	e.RequestedReviewerID = requestedReviewerID
	batch.Put("issue_events", strconv.Itoa(e.ID), e)
}

// RecordPullRequestEvent records a public issue event attached to a PR.
func (st *Store) RecordPullRequestEvent(repoID, prID, actorID int, event, commitID string, requestedReviewerID int) *IssueEvent {
	st.Mu.Lock()
	defer st.Mu.Unlock()
	return st.recordPullRequestEventLocked(repoID, prID, actorID, event, commitID, requestedReviewerID)
}

// recordIssueEventWithIDsBatchLocked builds an issue event and stages its
// persist into batch so a multi-event mutation commits every event with the
// issue row (STORE-001/002). Callers hold st.Mu.
func (st *Store) recordIssueEventWithIDsBatchLocked(batch *PersistBatch, repoID, issueID, actorID int, event string, labelID, assigneeID, assignerID, milestoneID, commentID int) {
	e := st.buildIssueEventLocked(repoID, issueID, actorID, event, "issue")
	e.LabelID = labelID
	e.AssigneeID = assigneeID
	e.AssignerID = assignerID
	e.MilestoneID = milestoneID
	e.CommentID = commentID
	batch.Put("issue_events", strconv.Itoa(e.ID), e)
}

// RecordIssueEvent records a public issue event. payload may carry optional
// related IDs under GitHub's keys: label_id, assignee_id, assigner_id,
// milestone_id, comment_id, commit_id, commit_url.
func (st *Store) RecordIssueEvent(repoID, issueID, actorID int, event string, payload map[string]interface{}) *IssueEvent {
	st.Mu.Lock()
	defer st.Mu.Unlock()
	return st.recordEventLocked(repoID, issueID, actorID, event, "issue", payload)
}

// RecordIssueOrPREvent records a timeline event against whichever of the issue
// or PR in repoID carries `number`, stamping the correct ParentType. Shared
// issue+PR endpoints (lock/unlock) must use this, not RecordIssueEvent: a PR
// event parented to "issue" is dropped from the PR timeline and can collide into an unrelated issue's.
func (st *Store) RecordIssueOrPREvent(repoID, number, actorID int, event string, payload map[string]interface{}) *IssueEvent {
	st.Mu.Lock()
	defer st.Mu.Unlock()
	parentType := "issue"
	var parentID int
	for _, i := range st.Issues {
		if i.RepoID == repoID && i.Number == number {
			parentID = i.ID
			break
		}
	}
	if parentID == 0 {
		for _, pr := range st.PullRequests {
			if pr.RepoID == repoID && pr.Number == number {
				parentID = pr.ID
				parentType = "pull_request"
				break
			}
		}
	}
	if parentID == 0 {
		return nil
	}
	return st.recordEventLocked(repoID, parentID, actorID, event, parentType, payload)
}

// recordEventLocked builds a complete IssueEvent from payload and persists it in
// one write (STORE-001/002). Callers hold st.Mu. parentType is "issue" or "pull_request".
func (st *Store) recordEventLocked(repoID, parentID, actorID int, event, parentType string, payload map[string]interface{}) *IssueEvent {
	e := st.buildIssueEventLocked(repoID, parentID, actorID, event, parentType)
	e.LabelID = intFromPayload(payload, "label_id")
	e.AssigneeID = intFromPayload(payload, "assignee_id")
	e.AssignerID = intFromPayload(payload, "assigner_id")
	e.MilestoneID = intFromPayload(payload, "milestone_id")
	e.CommentID = intFromPayload(payload, "comment_id")
	if v, ok := payload["commit_id"].(string); ok {
		e.CommitID = v
	}
	if v, ok := payload["commit_url"].(string); ok {
		e.CommitURL = v
	}
	if v, ok := payload["lock_reason"].(string); ok {
		e.LockReason = v
	}
	if v, ok := payload["rename_from"].(string); ok {
		e.RenameFrom = v
	}
	if v, ok := payload["rename_to"].(string); ok {
		e.RenameTo = v
	}
	st.persistIssueEventLocked(e)
	return e
}

// intFromPayload extracts an int from payload, tolerating float64 (JSON's default) and int.
func intFromPayload(payload map[string]interface{}, key string) int {
	v, ok := payload[key]
	if !ok {
		return 0
	}
	switch n := v.(type) {
	case int:
		return n
	case int64:
		return int(n)
	case float64:
		return int(n)
	case string:
		if i, err := strconv.Atoi(n); err == nil {
			return i
		}
	}
	return 0
}

// ListIssueEvents returns a repo's issue events, ordered by event ID. issueID 0
// spans all events including PR events (GitHub's repo-level listing does too); a
// specific issueID excludes PR events, whose global IDs can collide with issues'.
func (st *Store) ListIssueEvents(repoID, issueID int) []*IssueEvent {
	st.Mu.RLock()
	defer st.Mu.RUnlock()
	var events []*IssueEvent
	for _, e := range st.IssueEvents {
		if e.RepoID != repoID {
			continue
		}
		if issueID != 0 && (e.IssueID != issueID || e.ParentType == "pull_request") {
			continue
		}
		events = append(events, e)
	}
	sort.Slice(events, func(i, j int) bool { return events[i].ID < events[j].ID })
	return snapshotSlice(events)
}

// ListPullRequestEvents returns a PR's issue events, ordered by event ID.
func (st *Store) ListPullRequestEvents(repoID, prID int) []*IssueEvent {
	st.Mu.RLock()
	defer st.Mu.RUnlock()
	var events []*IssueEvent
	for _, e := range st.IssueEvents {
		if e.RepoID != repoID || e.ParentType != "pull_request" || e.IssueID != prID {
			continue
		}
		events = append(events, e)
	}
	sort.Slice(events, func(i, j int) bool { return events[i].ID < events[j].ID })
	return snapshotSlice(events)
}

func (st *Store) ListRepoIssueEvents(repoID int) []*IssueEvent {
	return snapshotSlice(st.ListIssueEvents(repoID, 0))
}

// GetIssueEvent returns a detached copy of an issue event by global ID (STORE-021).
func (st *Store) GetIssueEvent(id int) *IssueEvent {
	st.Mu.RLock()
	defer st.Mu.RUnlock()
	ev := st.IssueEvents[id]
	if ev == nil {
		return nil
	}
	clone := *ev
	return &clone
}

// Label CRUD

// defaultRepoLabel is one entry of GitHub's seeded-label set, using GitHub's own
// name/color/description values.
type defaultRepoLabel struct {
	name        string
	color       string
	description string
}

// defaultRepoLabels is the nine-label set GitHub seeds into every new repository,
// in creation (= id, = listing) order, reported with `"default": true`. Forks
// and template-generated repos get this same set, not a copy of the parent's labels.
var defaultRepoLabels = []defaultRepoLabel{
	{"bug", "d73a4a", "Something isn't working"},
	{"documentation", "0075ca", "Improvements or additions to documentation"},
	{"duplicate", "cfd3d7", "This issue or pull request already exists"},
	{"enhancement", "a2eeef", "New feature or request"},
	{"good first issue", "7057ff", "Good for newcomers"},
	{"help wanted", "008672", "Extra attention is needed"},
	{"invalid", "e4e669", "This doesn't seem right"},
	{"question", "d876e3", "Further information is requested"},
	{"wontfix", "ffffff", "This will not be worked on"},
}

// ensureDefaultLabelsBatchLocked seeds a new repo with GitHub's default labels,
// staging their persist into batch so repo and labels commit together
// (STORE-001/002). Callers hold st.Mu.
func (st *Store) ensureDefaultLabelsBatchLocked(batch *PersistBatch, repoID int) {
	now := st.CurrentTime()
	for _, d := range defaultRepoLabels {
		label := &IssueLabel{
			ID:          st.NextLabel,
			NodeID:      fmt.Sprintf("LA_kgDO%08d", st.NextLabel),
			RepoID:      repoID,
			Name:        d.name,
			Description: d.description,
			Color:       d.color,
			Default:     true,
			CreatedAt:   now,
		}
		st.NextLabel++
		st.Labels[label.ID] = label
		batch.Put("labels", strconv.Itoa(label.ID), label)
	}
}

// CreateLabel creates a label in the repo, or returns nil on a duplicate name.
func (st *Store) CreateLabel(repoID int, name, description, color string) *IssueLabel {
	st.Mu.Lock()
	defer st.Mu.Unlock()

	for _, l := range st.Labels {
		if l.RepoID == repoID && l.Name == name {
			return nil
		}
	}

	now := st.CurrentTime()
	label := &IssueLabel{
		ID:          st.NextLabel,
		NodeID:      fmt.Sprintf("LA_kgDO%08d", st.NextLabel),
		RepoID:      repoID,
		Name:        name,
		Description: description,
		Color:       color,
		CreatedAt:   now,
	}
	st.NextLabel++
	st.Labels[label.ID] = label
	if st.Persist != nil {
		st.Persist.MustPut("labels", strconv.Itoa(label.ID), label)
	}
	return label
}

// GetLabel returns a detached copy of a label by global ID (STORE-021).
func (st *Store) GetLabel(id int) *IssueLabel {
	st.Mu.RLock()
	defer st.Mu.RUnlock()
	lbl := st.Labels[id]
	if lbl == nil {
		return nil
	}
	clone := *lbl
	return &clone
}

func (st *Store) GetLabelByName(repoID int, name string) *IssueLabel {
	st.Mu.RLock()
	defer st.Mu.RUnlock()
	for _, l := range st.Labels {
		if l.RepoID == repoID && l.Name == name {
			clone := *l
			return &clone
		}
	}
	return nil
}

// NewLabelsAssignable reports whether every requested label not already on a
// subject belongs to the repository and remains available for assignment.
func (st *Store) NewLabelsAssignable(repoID int, current, requested []int) bool {
	st.Mu.RLock()
	defer st.Mu.RUnlock()
	return st.newLabelsAssignableLocked(repoID, current, requested)
}

func (st *Store) newLabelsAssignableLocked(repoID int, current, requested []int) bool {
	existing := make(map[int]bool, len(current))
	for _, id := range current {
		existing[id] = true
	}
	for _, id := range requested {
		if existing[id] {
			continue
		}
		label := st.Labels[id]
		if label == nil || label.RepoID != repoID || label.Archived {
			return false
		}
	}
	return true
}

// ListLabels returns a repository's labels in creation order.
func (st *Store) ListLabels(repoID int) []*IssueLabel {
	st.Mu.RLock()
	defer st.Mu.RUnlock()
	var labels []*IssueLabel
	for _, l := range st.Labels {
		if l.RepoID == repoID {
			labels = append(labels, l)
		}
	}
	// Ascending id is creation order, which GitHub's labels endpoint returns.
	sort.Slice(labels, func(i, j int) bool { return labels[i].ID < labels[j].ID })
	return snapshotSlice(labels)
}

// UpdateLabel applies fn to a label. Returns false when it does not exist.
func (st *Store) UpdateLabel(id int, fn func(*IssueLabel)) bool {
	st.Mu.Lock()
	defer st.Mu.Unlock()
	l, ok := st.Labels[id]
	if !ok {
		return false
	}
	fn(l)
	if st.Persist != nil {
		st.Persist.MustPut("labels", strconv.Itoa(l.ID), l)
	}
	return true
}

// DeleteLabel removes a label and detaches it from every issue in one
// transaction, so no issue persists referencing a deleted label.
func (st *Store) DeleteLabel(id int) bool {
	st.Mu.Lock()
	defer st.Mu.Unlock()
	if _, ok := st.Labels[id]; !ok {
		return false
	}
	batch := NewPersistBatch(st.Persist)
	delete(st.Labels, id)
	batch.Delete("labels", strconv.Itoa(id))
	for _, issue := range st.Issues {
		for i, lid := range issue.LabelIDs {
			if lid == id {
				issue.LabelIDs = append(issue.LabelIDs[:i], issue.LabelIDs[i+1:]...)
				batch.Put("issues", strconv.Itoa(issue.ID), issue)
				break
			}
		}
	}
	if err := batch.Commit(); err != nil {
		panic(&PersistenceFailure{Op: "batch", Bucket: "labels", Err: err})
	}
	return true
}

// Milestone CRUD

// CreateMilestone creates a milestone in the repo on behalf of creatorID.
func (st *Store) CreateMilestone(repoID, creatorID int, title, description, state string, dueOn *time.Time) *Milestone {
	st.Mu.Lock()
	defer st.Mu.Unlock()

	repo := st.Repos[repoID]
	if repo == nil {
		return nil
	}

	if state == "" {
		state = "open"
	}

	now := st.CurrentTime()
	ms := &Milestone{
		ID:          st.NextMilestone,
		NodeID:      fmt.Sprintf("MI_kgDO%08d", st.NextMilestone),
		RepoID:      repoID,
		Number:      repo.NextMilestoneNumber,
		Title:       title,
		Description: description,
		State:       MilestoneState(state),
		CreatorID:   creatorID,
		DueOn:       dueOn,
		CreatedAt:   now,
		UpdatedAt:   now,
	}
	repo.NextMilestoneNumber++
	st.NextMilestone++
	st.Milestones[ms.ID] = ms
	if st.Persist != nil {
		st.Persist.MustPut("milestones", strconv.Itoa(ms.ID), ms)
	}
	return ms
}

// cloneMilestone returns a detached copy safe outside the store lock (STORE-021).
func cloneMilestone(m *Milestone) *Milestone {
	if m == nil {
		return nil
	}
	clone := *m
	if m.DueOn != nil {
		due := *m.DueOn
		clone.DueOn = &due
	}
	if m.ClosedAt != nil {
		closed := *m.ClosedAt
		clone.ClosedAt = &closed
	}
	return &clone
}

func (st *Store) GetMilestone(id int) *Milestone {
	st.Mu.RLock()
	defer st.Mu.RUnlock()
	return cloneMilestone(st.Milestones[id])
}

func (st *Store) GetMilestoneByNumber(repoID, number int) *Milestone {
	st.Mu.RLock()
	defer st.Mu.RUnlock()
	for _, ms := range st.Milestones {
		if ms.RepoID == repoID && ms.Number == number {
			return cloneMilestone(ms)
		}
	}
	return nil
}

// ListMilestones returns milestones for a repository, optionally filtered by state.
func (st *Store) ListMilestones(repoID int, state string) []*Milestone {
	st.Mu.RLock()
	defer st.Mu.RUnlock()
	var milestones []*Milestone
	for _, ms := range st.Milestones {
		if ms.RepoID != repoID {
			continue
		}
		if state != "" && state != "all" && string(ms.State) != state {
			continue
		}
		milestones = append(milestones, ms)
	}
	// Number order (= creation order) gives a stable listing; map order is random.
	sort.Slice(milestones, func(i, j int) bool { return milestones[i].Number < milestones[j].Number })
	return snapshotMilestones(milestones)
}

// UpdateMilestone applies fn to a milestone. Returns false when it does not exist.
func (st *Store) UpdateMilestone(id int, fn func(*Milestone)) bool {
	st.Mu.Lock()
	defer st.Mu.Unlock()
	ms, ok := st.Milestones[id]
	if !ok {
		return false
	}
	fn(ms)
	ms.UpdatedAt = st.CurrentTime()
	if st.Persist != nil {
		st.Persist.MustPut("milestones", strconv.Itoa(ms.ID), ms)
	}
	return true
}

// DeleteMilestone removes a milestone and detaches it from every issue in one
// transaction, so no issue persists referencing a deleted milestone.
func (st *Store) DeleteMilestone(id int) bool {
	st.Mu.Lock()
	defer st.Mu.Unlock()
	if _, ok := st.Milestones[id]; !ok {
		return false
	}
	batch := NewPersistBatch(st.Persist)
	delete(st.Milestones, id)
	batch.Delete("milestones", strconv.Itoa(id))
	for _, issue := range st.Issues {
		if issue.MilestoneID == id {
			issue.MilestoneID = 0
			batch.Put("issues", strconv.Itoa(issue.ID), issue)
		}
	}
	if err := batch.Commit(); err != nil {
		panic(&PersistenceFailure{Op: "batch", Bucket: "milestones", Err: err})
	}
	return true
}

// Issue CRUD

func (st *Store) CreateIssue(repoID, authorID int, title, body string, labelIDs, assigneeIDs []int, milestoneID int) *Issue {
	st.Mu.Lock()
	defer st.Mu.Unlock()

	repo := st.Repos[repoID]
	if repo == nil || !st.newLabelsAssignableLocked(repoID, nil, labelIDs) {
		return nil
	}

	if labelIDs == nil {
		labelIDs = []int{}
	}
	if assigneeIDs == nil {
		assigneeIDs = []int{}
	}

	now := st.CurrentTime()
	issueID := st.ReserveGlobalID("next_issue_id", &st.NextIssue)
	issue := &Issue{
		ID:          issueID,
		NodeID:      fmt.Sprintf("I_kgDO%08d", issueID),
		Number:      repo.NextIssueNumber,
		RepoID:      repoID,
		Title:       title,
		Body:        body,
		State:       "OPEN",
		AuthorID:    authorID,
		AssigneeIDs: assigneeIDs,
		LabelIDs:    append([]int(nil), labelIDs...),
		MilestoneID: milestoneID,
		CreatedAt:   now,
		UpdatedAt:   now,
	}
	repo.NextIssueNumber++
	st.Issues[issue.ID] = issue
	st.indexIssueLocked(issue)
	// Issue row and "opened" event commit together (STORE-001/002).
	batch := NewPersistBatch(st.Persist)
	batch.Put("issues", strconv.Itoa(issue.ID), issue)
	st.recordIssueEventBatchLocked(batch, repoID, issue.ID, authorID, "opened")
	if err := batch.Commit(); err != nil {
		panic(&PersistenceFailure{Op: "batch", Bucket: "issues", Key: strconv.Itoa(issue.ID), Err: err})
	}
	return issue
}

// indexIssueLocked records the issue in the per-repo secondary indexes so
// lookups and creation-ordered listings avoid a full store scan. Caller holds st.Mu.
func (st *Store) indexIssueLocked(issue *Issue) {
	m := st.IssuesByRepo[issue.RepoID]
	if m == nil {
		m = make(map[int]*Issue)
		st.IssuesByRepo[issue.RepoID] = m
	}
	m[issue.Number] = issue
	// Insertion-sort into the per-repo (CreatedAt, Number) order slice: the load
	// path replays in arbitrary order, live creation appends in O(1). Both keys
	// are immutable, so the slice never needs re-sorting.
	order := st.IssueOrderByRepo[issue.RepoID]
	pos := sort.Search(len(order), func(i int) bool {
		if !order[i].CreatedAt.Equal(issue.CreatedAt) {
			return order[i].CreatedAt.After(issue.CreatedAt)
		}
		return order[i].Number > issue.Number
	})
	order = append(order, nil)
	copy(order[pos+1:], order[pos:])
	order[pos] = issue
	st.IssueOrderByRepo[issue.RepoID] = order
}

// unindexIssueLocked removes the issue from the per-repo indexes. Caller holds st.Mu.
func (st *Store) unindexIssueLocked(issue *Issue) {
	if m := st.IssuesByRepo[issue.RepoID]; m != nil {
		delete(m, issue.Number)
		if len(m) == 0 {
			delete(st.IssuesByRepo, issue.RepoID)
		}
	}
	order := st.IssueOrderByRepo[issue.RepoID]
	for i, existing := range order {
		if existing.Number == issue.Number {
			st.IssueOrderByRepo[issue.RepoID] = append(order[:i], order[i+1:]...)
			break
		}
	}
	if len(st.IssueOrderByRepo[issue.RepoID]) == 0 {
		delete(st.IssueOrderByRepo, issue.RepoID)
	}
}

// cloneIssue returns a deep copy safe outside the store lock (STORE-021);
// writes go through the keyed UpdateIssue(id, fn).
func cloneIssue(i *Issue) *Issue {
	if i == nil {
		return nil
	}
	clone := *i
	if i.AssigneeIDs != nil {
		clone.AssigneeIDs = append([]int(nil), i.AssigneeIDs...)
	}
	if i.LabelIDs != nil {
		clone.LabelIDs = append([]int(nil), i.LabelIDs...)
	}
	if i.LinkedBranches != nil {
		clone.LinkedBranches = append([]LinkedBranch(nil), i.LinkedBranches...)
	}
	if i.ClosedAt != nil {
		closed := *i.ClosedAt
		clone.ClosedAt = &closed
	}
	if i.PinnedAt != nil {
		pinned := *i.PinnedAt
		clone.PinnedAt = &pinned
	}
	return &clone
}

func (st *Store) GetIssue(id int) *Issue {
	st.Mu.RLock()
	defer st.Mu.RUnlock()
	return cloneIssue(st.Issues[id])
}

func (st *Store) GetIssueByNumber(repoID, number int) *Issue {
	st.Mu.RLock()
	defer st.Mu.RUnlock()
	return cloneIssue(st.IssuesByRepo[repoID][number])
}

// listIssuesOrderedLocked returns the repo's live issue pointers filtered by
// state in (CreatedAt, Number) order. Caller holds st.Mu and must detach results.
func (st *Store) listIssuesOrderedLocked(repoID int, state string) []*Issue {
	order := st.IssueOrderByRepo[repoID]
	issues := make([]*Issue, 0, len(order))
	for _, issue := range order {
		if state != "" && state != "all" && issue.State != state {
			continue
		}
		issues = append(issues, issue)
	}
	return issues
}

// ListIssues returns a repo's issues oldest-created first. state matches
// "OPEN"/"CLOSED"; empty or "all" returns all.
func (st *Store) ListIssues(repoID int, state string) []*Issue {
	return st.ListIssuesOrderedByCreation(repoID, state, false)
}

// ListIssuesOrderedByCreation returns a repo's issues by creation time (number
// tie-break), descending (GitHub's default) when desc is true. Detached
// snapshots (STORE-021).
func (st *Store) ListIssuesOrderedByCreation(repoID int, state string, desc bool) []*Issue {
	st.Mu.RLock()
	defer st.Mu.RUnlock()
	issues := st.listIssuesOrderedLocked(repoID, state)
	if desc {
		for i, j := 0, len(issues)-1; i < j; i, j = i+1, j-1 {
			issues[i], issues[j] = issues[j], issues[i]
		}
	}
	return snapshotIssues(issues)
}

// UpdateIssue applies fn to an issue. Returns false when it does not exist.
func (st *Store) UpdateIssue(id int, fn func(*Issue)) bool {
	st.Mu.Lock()
	defer st.Mu.Unlock()
	issue, ok := st.Issues[id]
	if !ok {
		return false
	}
	fn(issue)
	issue.UpdatedAt = st.CurrentTime()
	if st.Persist != nil {
		st.Persist.MustPut("issues", strconv.Itoa(issue.ID), issue)
	}
	return true
}

// issueByRepoKeyAndNumber resolves an issue by repo key and number; caller holds st.Mu.
func (st *Store) issueByRepoKeyAndNumber(repoKey string, number int) *Issue {
	repo := st.ReposByName[repoKey]
	if repo == nil {
		return nil
	}
	return st.IssuesByRepo[repo.ID][number]
}

// issueByRepoIDAndNumber resolves an issue by repo ID and number; caller holds st.Mu.
func (st *Store) issueByRepoIDAndNumber(repoID, number int) *Issue {
	return st.IssuesByRepo[repoID][number]
}

// AddIssueAssignees adds assignees (ignoring duplicates) with an "assigned"
// event each. Returns true when the issue exists.
func (st *Store) AddIssueAssignees(repoID int, issueNumber int, assigneeIDs []int, actorID int) bool {
	st.Mu.Lock()
	defer st.Mu.Unlock()
	issue := st.issueByRepoIDAndNumber(repoID, issueNumber)
	if issue == nil {
		return false
	}
	batch := NewPersistBatch(st.Persist)
	added := false
	for _, uid := range assigneeIDs {
		found := false
		for _, existing := range issue.AssigneeIDs {
			if existing == uid {
				found = true
				break
			}
		}
		if !found {
			issue.AssigneeIDs = append(issue.AssigneeIDs, uid)
			st.recordIssueEventWithIDsBatchLocked(batch, repoID, issue.ID, actorID, "assigned", 0, uid, actorID, 0, 0)
			added = true
		}
	}
	if added {
		// Events and issue row commit together (STORE-001/002).
		issue.UpdatedAt = st.CurrentTime()
		batch.Put("issues", strconv.Itoa(issue.ID), issue)
		if err := batch.Commit(); err != nil {
			panic(&PersistenceFailure{Op: "batch", Bucket: "issues", Err: err})
		}
	}
	return true
}

// RemoveIssueAssignees removes assignees with an "unassigned" event each.
// Returns true when the issue exists.
func (st *Store) RemoveIssueAssignees(repoID int, issueNumber int, assigneeIDs []int, actorID int) bool {
	st.Mu.Lock()
	defer st.Mu.Unlock()
	issue := st.issueByRepoIDAndNumber(repoID, issueNumber)
	if issue == nil {
		return false
	}
	batch := NewPersistBatch(st.Persist)
	removed := false
	for _, uid := range assigneeIDs {
		for idx, existing := range issue.AssigneeIDs {
			if existing == uid {
				issue.AssigneeIDs = append(issue.AssigneeIDs[:idx], issue.AssigneeIDs[idx+1:]...)
				st.recordIssueEventWithIDsBatchLocked(batch, repoID, issue.ID, actorID, "unassigned", 0, uid, actorID, 0, 0)
				removed = true
				break
			}
		}
	}
	if removed {
		// Events and issue row commit together (STORE-001/002).
		issue.UpdatedAt = st.CurrentTime()
		batch.Put("issues", strconv.Itoa(issue.ID), issue)
		if err := batch.Commit(); err != nil {
			panic(&PersistenceFailure{Op: "batch", Bucket: "issues", Err: err})
		}
	}
	return true
}

// SetIssueLabels replaces an issue's labels, recording labeled/unlabeled events
// for the deltas. Returns true when the issue exists.
func (st *Store) SetIssueLabels(repoID int, issueNumber int, labelIDs []int, actorID int) bool {
	st.Mu.Lock()
	defer st.Mu.Unlock()
	issue := st.issueByRepoIDAndNumber(repoID, issueNumber)
	if issue == nil {
		return false
	}
	if !st.newLabelsAssignableLocked(repoID, issue.LabelIDs, labelIDs) {
		return false
	}
	old := make(map[int]bool, len(issue.LabelIDs))
	for _, lid := range issue.LabelIDs {
		old[lid] = true
	}
	newSet := make(map[int]bool, len(labelIDs))
	for _, lid := range labelIDs {
		newSet[lid] = true
	}
	// Events and issue row commit together (STORE-001/002).
	batch := NewPersistBatch(st.Persist)
	for _, lid := range issue.LabelIDs {
		if !newSet[lid] {
			st.recordIssueEventWithIDsBatchLocked(batch, repoID, issue.ID, actorID, "unlabeled", lid, 0, 0, 0, 0)
		}
	}
	for _, lid := range labelIDs {
		if !old[lid] {
			st.recordIssueEventWithIDsBatchLocked(batch, repoID, issue.ID, actorID, "labeled", lid, 0, 0, 0, 0)
		}
	}
	// Clone: the caller owns labelIDs and may mutate it after this returns.
	issue.LabelIDs = append([]int(nil), labelIDs...)
	issue.UpdatedAt = st.CurrentTime()
	batch.Put("issues", strconv.Itoa(issue.ID), issue)
	if err := batch.Commit(); err != nil {
		panic(&PersistenceFailure{Op: "batch", Bucket: "issues", Err: err})
	}
	return true
}

// ClearIssueLabels removes every label from an issue with an "unlabeled" event each.
func (st *Store) ClearIssueLabels(repoID int, issueNumber int, actorID int) bool {
	st.Mu.Lock()
	defer st.Mu.Unlock()
	issue := st.issueByRepoIDAndNumber(repoID, issueNumber)
	if issue == nil {
		return false
	}
	if len(issue.LabelIDs) == 0 {
		return true
	}
	// Events and cleared issue row commit together (STORE-001/002).
	batch := NewPersistBatch(st.Persist)
	for _, lid := range issue.LabelIDs {
		st.recordIssueEventWithIDsBatchLocked(batch, repoID, issue.ID, actorID, "unlabeled", lid, 0, 0, 0, 0)
	}
	issue.LabelIDs = nil
	issue.UpdatedAt = st.CurrentTime()
	batch.Put("issues", strconv.Itoa(issue.ID), issue)
	if err := batch.Commit(); err != nil {
		panic(&PersistenceFailure{Op: "batch", Bucket: "issues", Err: err})
	}
	return true
}

// AddIssueLabels adds labels (ignoring duplicates) with a "labeled" event each.
// Returns true when the issue exists.
func (st *Store) AddIssueLabels(repoKey string, issueNumber int, labelIDs []int) bool {
	st.Mu.Lock()
	defer st.Mu.Unlock()
	issue := st.issueByRepoKeyAndNumber(repoKey, issueNumber)
	if issue == nil {
		return false
	}
	repo := st.ReposByName[repoKey]
	if repo == nil || !st.newLabelsAssignableLocked(repo.ID, issue.LabelIDs, labelIDs) {
		return false
	}
	batch := NewPersistBatch(st.Persist)
	added := false
	for _, lid := range labelIDs {
		found := false
		for _, existing := range issue.LabelIDs {
			if existing == lid {
				found = true
				break
			}
		}
		if !found {
			issue.LabelIDs = append(issue.LabelIDs, lid)
			st.recordIssueEventWithIDsBatchLocked(batch, repo.ID, issue.ID, 0, "labeled", lid, 0, 0, 0, 0)
			added = true
		}
	}
	if added {
		// Events and issue row commit together (STORE-001/002).
		issue.UpdatedAt = st.CurrentTime()
		batch.Put("issues", strconv.Itoa(issue.ID), issue)
		if err := batch.Commit(); err != nil {
			panic(&PersistenceFailure{Op: "batch", Bucket: "issues", Err: err})
		}
	}
	return true
}

// RemoveIssueLabel removes a single label from an issue by name. Returns true
// when the issue and label exist.
func (st *Store) RemoveIssueLabel(repoKey string, issueNumber int, labelName string) bool {
	st.Mu.Lock()
	defer st.Mu.Unlock()
	issue := st.issueByRepoKeyAndNumber(repoKey, issueNumber)
	if issue == nil {
		return false
	}
	repo := st.ReposByName[repoKey]
	var label *IssueLabel
	for _, l := range st.Labels {
		if l.RepoID == repo.ID && l.Name == labelName {
			label = l
			break
		}
	}
	if label == nil {
		return false
	}
	for idx, lid := range issue.LabelIDs {
		if lid == label.ID {
			issue.LabelIDs = append(issue.LabelIDs[:idx], issue.LabelIDs[idx+1:]...)
			issue.UpdatedAt = st.CurrentTime()
			// Issue row and "unlabeled" event commit together (STORE-001/002).
			batch := NewPersistBatch(st.Persist)
			batch.Put("issues", strconv.Itoa(issue.ID), issue)
			st.recordIssueEventWithIDsBatchLocked(batch, repo.ID, issue.ID, 0, "unlabeled", label.ID, 0, 0, 0, 0)
			if err := batch.Commit(); err != nil {
				panic(&PersistenceFailure{Op: "batch", Bucket: "issues", Err: err})
			}
			return true
		}
	}
	return true
}

// LockIssue locks an issue, optionally recording a lock reason. Returns true
// when the issue exists.
func (st *Store) LockIssue(repoKey string, issueNumber int, lockReason string) bool {
	st.Mu.Lock()
	defer st.Mu.Unlock()
	issue := st.issueByRepoKeyAndNumber(repoKey, issueNumber)
	if issue == nil {
		return false
	}
	issue.Locked = true
	issue.ActiveLockReason = LockReason(lockReason)
	issue.UpdatedAt = st.CurrentTime()
	// Issue row and "locked" event commit together (STORE-001/002).
	batch := NewPersistBatch(st.Persist)
	batch.Put("issues", strconv.Itoa(issue.ID), issue)
	st.recordIssueEventBatchLocked(batch, issue.RepoID, issue.ID, 0, "locked")
	if err := batch.Commit(); err != nil {
		panic(&PersistenceFailure{Op: "batch", Bucket: "issues", Key: strconv.Itoa(issue.ID), Err: err})
	}
	return true
}

// UnlockIssue unlocks an issue. Returns true when the issue exists.
func (st *Store) UnlockIssue(repoKey string, issueNumber int) bool {
	st.Mu.Lock()
	defer st.Mu.Unlock()
	issue := st.issueByRepoKeyAndNumber(repoKey, issueNumber)
	if issue == nil {
		return false
	}
	issue.Locked = false
	issue.ActiveLockReason = ""
	issue.UpdatedAt = st.CurrentTime()
	// Issue row and "unlocked" event commit together (STORE-001/002).
	batch := NewPersistBatch(st.Persist)
	batch.Put("issues", strconv.Itoa(issue.ID), issue)
	st.recordIssueEventBatchLocked(batch, issue.RepoID, issue.ID, 0, "unlocked")
	if err := batch.Commit(); err != nil {
		panic(&PersistenceFailure{Op: "batch", Bucket: "issues", Key: strconv.Itoa(issue.ID), Err: err})
	}
	return true
}

// MaxPinnedIssuesPerRepo mirrors GitHub's cap on pinned issues per repository.
const MaxPinnedIssuesPerRepo = 3

// PinIssue pins an issue on behalf of actorID; already-pinned is a no-op. The
// cap check and pin happen under one lock so two concurrent pins cannot both
// squeeze under the cap.
func (st *Store) PinIssue(issueID, actorID int) error {
	st.Mu.Lock()
	defer st.Mu.Unlock()
	issue, ok := st.Issues[issueID]
	if !ok {
		return fmt.Errorf("issue does not exist")
	}
	if issue.PinnedAt != nil {
		return nil
	}
	pinned := 0
	for _, other := range st.IssuesByRepo[issue.RepoID] {
		if other.PinnedAt != nil {
			pinned++
		}
	}
	if pinned >= MaxPinnedIssuesPerRepo {
		return fmt.Errorf("cannot pin more than %d issues to a repository", MaxPinnedIssuesPerRepo)
	}
	now := st.CurrentTime()
	issue.PinnedAt = &now
	issue.PinnedByID = actorID
	issue.UpdatedAt = now
	// Issue row and "pinned" event commit together (STORE-001/002).
	batch := NewPersistBatch(st.Persist)
	batch.Put("issues", strconv.Itoa(issue.ID), issue)
	st.recordIssueEventBatchLocked(batch, issue.RepoID, issue.ID, actorID, "pinned")
	if err := batch.Commit(); err != nil {
		panic(&PersistenceFailure{Op: "batch", Bucket: "issues", Key: strconv.Itoa(issue.ID), Err: err})
	}
	return nil
}

// UnpinIssue clears an issue's pinned state on behalf of actorID, reporting
// whether it had been pinned. Missing or unpinned issue reports false.
func (st *Store) UnpinIssue(issueID, actorID int) bool {
	st.Mu.Lock()
	defer st.Mu.Unlock()
	issue, ok := st.Issues[issueID]
	if !ok || issue.PinnedAt == nil {
		return false
	}
	issue.PinnedAt = nil
	issue.PinnedByID = 0
	issue.UpdatedAt = st.CurrentTime()
	// Issue row and "unpinned" event commit together (STORE-001/002).
	batch := NewPersistBatch(st.Persist)
	batch.Put("issues", strconv.Itoa(issue.ID), issue)
	st.recordIssueEventBatchLocked(batch, issue.RepoID, issue.ID, actorID, "unpinned")
	if err := batch.Commit(); err != nil {
		panic(&PersistenceFailure{Op: "batch", Bucket: "issues", Key: strconv.Itoa(issue.ID), Err: err})
	}
	return true
}

// ListPinnedIssues returns a repository's pinned issues, oldest pin first (GitHub's order).
func (st *Store) ListPinnedIssues(repoID int) []*Issue {
	st.Mu.RLock()
	defer st.Mu.RUnlock()
	var issues []*Issue
	for _, issue := range st.IssuesByRepo[repoID] {
		if issue.PinnedAt != nil {
			issues = append(issues, issue)
		}
	}
	sort.Slice(issues, func(i, j int) bool {
		if !issues[i].PinnedAt.Equal(*issues[j].PinnedAt) {
			return issues[i].PinnedAt.Before(*issues[j].PinnedAt)
		}
		return issues[i].ID < issues[j].ID
	})
	return snapshotIssues(issues)
}

// DeleteIssue removes an issue and everything parented to it — comments (and
// their reactions), timeline events, sub-issue links, blocked-by references,
// project items, field values, notification threads, and the issue's own
// reactions — in one transaction (STORE-001/002).
func (st *Store) DeleteIssue(issueID int) bool {
	st.Mu.Lock()
	defer st.Mu.Unlock()
	issue, ok := st.Issues[issueID]
	if !ok {
		return false
	}
	batch := NewPersistBatch(st.Persist)
	delete(st.Issues, issueID)
	st.unindexIssueLocked(issue)
	batch.Delete("issues", strconv.Itoa(issueID))
	// repoID 0 keeps the cascade's repo-wide event sweep inert (no event carries
	// RepoID 0); the issue-scoped clauses do the work.
	st.deleteRepoIssueAndPullChildrenLocked(batch, 0, map[int]bool{issueID: true}, nil)
	if err := batch.Commit(); err != nil {
		panic(&PersistenceFailure{Op: "batch", Bucket: "issues", Key: strconv.Itoa(issueID), Err: err})
	}
	return true
}

// TransferIssue moves an issue into targetRepoID with a fresh issue number.
// Labels re-match by name in the target (created when createLabelsIfMissing,
// else dropped); milestone and pinned state do not follow; timeline events are
// re-homed so history survives. Returns nil when the issue or target is missing
// or the target is the issue's own repo.
func (st *Store) TransferIssue(issueID, targetRepoID, actorID int, createLabelsIfMissing bool) *Issue {
	st.Mu.Lock()
	defer st.Mu.Unlock()
	issue, ok := st.Issues[issueID]
	if !ok {
		return nil
	}
	target := st.Repos[targetRepoID]
	if target == nil || target.ID == issue.RepoID {
		return nil
	}
	batch := NewPersistBatch(st.Persist)
	// Labels are per-repo, so source label IDs must not travel: re-match by name.
	newLabelIDs := []int{}
	for _, lid := range issue.LabelIDs {
		src := st.Labels[lid]
		if src == nil {
			continue
		}
		var match *IssueLabel
		for _, l := range st.Labels {
			if l.RepoID == target.ID && l.Name == src.Name {
				match = l
				break
			}
		}
		if match == nil && createLabelsIfMissing {
			match = &IssueLabel{
				ID:          st.NextLabel,
				NodeID:      fmt.Sprintf("LA_kgDO%08d", st.NextLabel),
				RepoID:      target.ID,
				Name:        src.Name,
				Description: src.Description,
				Color:       src.Color,
				CreatedAt:   st.CurrentTime(),
			}
			st.NextLabel++
			st.Labels[match.ID] = match
			batch.Put("labels", strconv.Itoa(match.ID), match)
		}
		if match != nil {
			newLabelIDs = append(newLabelIDs, match.ID)
		}
	}
	oldRepoID := issue.RepoID
	st.unindexIssueLocked(issue)
	issue.RepoID = target.ID
	issue.Number = target.NextIssueNumber
	target.NextIssueNumber++
	issue.LabelIDs = newLabelIDs
	issue.MilestoneID = 0 // per-repo, does not follow the issue
	issue.PinnedAt = nil  // GitHub unpins on transfer
	issue.PinnedByID = 0
	issue.UpdatedAt = st.CurrentTime()
	st.indexIssueLocked(issue)
	// Re-home the timeline so per-issue listings (filtered by RepoID) still show history.
	for _, e := range st.IssueEvents {
		if e.ParentType == "issue" && e.IssueID == issue.ID && e.RepoID == oldRepoID {
			e.RepoID = target.ID
			batch.Put("issue_events", strconv.Itoa(e.ID), e)
		}
	}
	// Moved issue, re-homed events, created labels, and "transferred" event
	// commit together (STORE-001/002).
	batch.Put("issues", strconv.Itoa(issue.ID), issue)
	st.recordIssueEventBatchLocked(batch, target.ID, issue.ID, actorID, "transferred")
	if err := batch.Commit(); err != nil {
		panic(&PersistenceFailure{Op: "batch", Bucket: "issues", Key: strconv.Itoa(issue.ID), Err: err})
	}
	return cloneIssue(issue)
}

// ListIssueComments returns an issue's conversation comments by repo key and number.
func (st *Store) ListIssueComments(repoKey string, issueNumber int) []*Comment {
	st.Mu.RLock()
	defer st.Mu.RUnlock()
	issue := st.issueByRepoKeyAndNumber(repoKey, issueNumber)
	if issue == nil {
		return nil
	}
	var comments []*Comment
	for _, c := range st.Comments {
		if c.ParentType == "issue" && c.IssueID == issue.ID {
			comments = append(comments, cloneComment(c))
		}
	}
	return snapshotComments(comments)
}

func (st *Store) GetIssueComment(id int) *Comment {
	return st.GetComment(id)
}

func (st *Store) DeleteIssueComment(id int) bool {
	return st.DeleteComment(id)
}

// ListRepoIssueComments returns all of a repo's issue comments, oldest first.
func (st *Store) ListRepoIssueComments(repoID int) []*Comment {
	st.Mu.RLock()
	defer st.Mu.RUnlock()
	var comments []*Comment
	for _, c := range st.Comments {
		if c.ParentType == "issue" && st.Issues[c.IssueID] != nil && st.Issues[c.IssueID].RepoID == repoID {
			comments = append(comments, cloneComment(c))
		}
	}
	sort.Slice(comments, func(i, j int) bool { return comments[i].CreatedAt.Before(comments[j].CreatedAt) })
	return snapshotComments(comments)
}

// PinIssueComment marks a comment pinned. Returns true when it exists.
func (st *Store) PinIssueComment(commentID int) bool {
	st.Mu.Lock()
	defer st.Mu.Unlock()
	c, ok := st.Comments[commentID]
	if !ok {
		return false
	}
	c.Pinned = true
	c.UpdatedAt = st.CurrentTime()
	if st.Persist != nil {
		st.Persist.MustPut("comments", strconv.Itoa(c.ID), c)
	}
	return true
}

// UnpinIssueComment clears a comment's pinned flag. Returns true when it exists.
func (st *Store) UnpinIssueComment(commentID int) bool {
	st.Mu.Lock()
	defer st.Mu.Unlock()
	c, ok := st.Comments[commentID]
	if !ok {
		return false
	}
	c.Pinned = false
	c.UpdatedAt = st.CurrentTime()
	if st.Persist != nil {
		st.Persist.MustPut("comments", strconv.Itoa(c.ID), c)
	}
	return true
}

// BuildIssueTimeline synthesizes an issue's timeline by interleaving events and
// comments by created_at.
func (st *Store) BuildIssueTimeline(repo *Repo, issueID int, baseURL string) []map[string]interface{} {
	// Don't hold st.Mu across these: they RLock themselves, and re-acquiring the
	// read lock can deadlock against a queued writer.
	events := st.ListIssueEvents(repo.ID, issueID)
	comments := st.ListCommentsFor("issue", issueID)

	items := make([]timelineItem, 0, len(events)+len(comments))
	for _, e := range events {
		// Skip stored "commented" events: the comment entries below carry the
		// conversation, and rendering these would duplicate every comment under
		// an event id (a different id space, whose reactions endpoint 404s).
		if e.Event == "commented" {
			continue
		}
		items = append(items, timelineItem{CreatedAt: e.CreatedAt, kind: "event", event: e})
	}
	for _, c := range comments {
		items = append(items, timelineItem{CreatedAt: c.CreatedAt, kind: "comment", comment: c})
	}
	sort.Slice(items, func(i, j int) bool {
		if !items[i].CreatedAt.Equal(items[j].CreatedAt) {
			return items[i].CreatedAt.Before(items[j].CreatedAt)
		}
		// Events before comments at equal timestamps, for stability.
		if items[i].kind != items[j].kind {
			return items[i].kind == "event"
		}
		return items[i].Id() < items[j].Id()
	})

	out := make([]map[string]interface{}, 0, len(items))
	for _, ti := range items {
		switch ti.kind {
		case "event":
			out = append(out, IssueEventForTimelineToJSON(ti.event, st, baseURL, repo.FullName))
		case "comment":
			parentNumber := 0
			if issue := st.GetIssue(ti.comment.IssueID); issue != nil {
				parentNumber = issue.Number
			}
			out = append(out, TimelineCommentToJSON(ti.comment, st, baseURL, repo.FullName, parentNumber, repo))
		}
	}
	return out
}

type timelineItem struct {
	CreatedAt time.Time `json:"-"`
	kind      string
	event     *IssueEvent
	comment   *Comment
}

func (ti timelineItem) Id() int {
	switch ti.kind {
	case "event":
		if ti.event != nil {
			return ti.event.ID
		}
	case "comment":
		if ti.comment != nil {
			return ti.comment.ID
		}
	}
	return 0
}

// Comment CRUD

// CreateComment creates a conversation comment on an issue; use CreateCommentFor for PRs.
func (st *Store) CreateComment(issueID, authorID int, body string) *Comment {
	return st.CreateCommentFor("issue", issueID, authorID, body)
}

// CreateCommentFor creates a comment on an "issue" or "pull_request" parent,
// which must already exist. Returns nil otherwise.
func (st *Store) CreateCommentFor(parentType string, parentID, authorID int, body string) *Comment {
	st.Mu.Lock()
	defer st.Mu.Unlock()
	switch parentType {
	case "issue":
		if _, ok := st.Issues[parentID]; !ok {
			return nil
		}
	case "pull_request":
		if _, ok := st.PullRequests[parentID]; !ok {
			return nil
		}
	default:
		return nil
	}

	now := st.CurrentTime()
	c := &Comment{
		ID:         st.NextComment,
		NodeID:     fmt.Sprintf("IC_kgDO%08d", st.NextComment),
		ParentType: parentType,
		IssueID:    parentID,
		AuthorID:   authorID,
		Body:       body,
		CreatedAt:  now,
		UpdatedAt:  now,
	}
	st.NextComment++
	st.Comments[c.ID] = c
	st.CommentCounts[CommentCountKey(parentType, parentID)]++
	st.indexCommentLocked(c)
	// Comment row and "commented" event commit together (STORE-001/002).
	batch := NewPersistBatch(st.Persist)
	batch.Put("comments", strconv.Itoa(c.ID), c)
	// Only issue comments get a timeline event; PR comments use review-comment machinery.
	if parentType == "issue" {
		if issue := st.Issues[parentID]; issue != nil {
			st.recordIssueEventWithIDsBatchLocked(batch, issue.RepoID, issue.ID, authorID, "commented", 0, 0, 0, 0, c.ID)
		}
	}
	if err := batch.Commit(); err != nil {
		panic(&PersistenceFailure{Op: "batch", Bucket: "comments", Key: strconv.Itoa(c.ID), Err: err})
	}
	return cloneComment(c)
}

func (st *Store) ListComments(issueID int) []*Comment {
	return snapshotComments(st.ListCommentsFor("issue", issueID))
}

func (st *Store) GetComment(id int) *Comment {
	st.Mu.RLock()
	defer st.Mu.RUnlock()
	return cloneComment(st.Comments[id])
}

// CommentRepoID returns the repo ID owning the comment's parent, or 0 when the
// parent no longer exists.
func (st *Store) CommentRepoID(c *Comment) int {
	if c == nil {
		return 0
	}
	st.Mu.RLock()
	defer st.Mu.RUnlock()
	switch c.ParentType {
	case "issue":
		if i := st.Issues[c.IssueID]; i != nil {
			return i.RepoID
		}
	case "pull_request":
		if pr := st.PullRequests[c.IssueID]; pr != nil {
			return pr.RepoID
		}
	}
	return 0
}

// DeleteComment removes a comment and its reactions in one transaction
// (STORE-001/002). Returns true if removed.
func (st *Store) DeleteComment(id int) bool {
	st.Mu.Lock()
	defer st.Mu.Unlock()
	c, ok := st.Comments[id]
	if !ok {
		return false
	}
	delete(st.Comments, id)
	st.unindexCommentLocked(c)
	batch := NewPersistBatch(st.Persist)
	st.Reactions.DeleteParentsBatch(c.ParentType+"_comment", map[int]bool{id: true}, batch)
	key := CommentCountKey(c.ParentType, c.IssueID)
	if st.CommentCounts[key] <= 1 {
		delete(st.CommentCounts, key)
	} else {
		st.CommentCounts[key]--
	}
	batch.Delete("comments", strconv.Itoa(id))
	if err := batch.Commit(); err != nil {
		panic(&PersistenceFailure{Op: "batch", Bucket: "comments", Key: strconv.Itoa(id), Err: err})
	}
	return true
}

// CommentCountKey builds the CommentCounts index key for a comment parent.
func CommentCountKey(parentType string, parentID int) string {
	return parentType + "\x1f" + strconv.Itoa(parentID)
}

// CountCommentsFor returns the comment count on a parent via the maintained
// index. Caller must NOT hold st.Mu.
func (st *Store) CountCommentsFor(parentType string, parentID int) int {
	st.Mu.RLock()
	defer st.Mu.RUnlock()
	return st.CommentCounts[CommentCountKey(parentType, parentID)]
}

// CountCommentsForLocked is the variant for callers already holding st.Mu.
func (st *Store) CountCommentsForLocked(parentType string, parentID int) int {
	return st.CommentCounts[CommentCountKey(parentType, parentID)]
}

// ResolveCommentParent resolves the issue or PR at repo + number, returning its
// kind, global ID, number, and locked flag in one read-locked pass. Read Locked
// here rather than off a shared pointer: SetIssueOrPRLock mutates it under the write lock.
func (st *Store) ResolveCommentParent(repoID, number int) (parentType string, parentID, parentNumber int, locked, found bool) {
	st.Mu.RLock()
	defer st.Mu.RUnlock()
	if i := st.IssuesByRepo[repoID][number]; i != nil {
		return "issue", i.ID, i.Number, i.Locked, true
	}
	if pr := st.PullsByRepo[repoID][number]; pr != nil {
		return "pull_request", pr.ID, pr.Number, pr.Locked, true
	}
	return "", 0, 0, false, false
}

// SetIssueOrPRLock sets the locked flag on the issue or PR at repo + number
// (reason recorded only when locked). Returns false when none matches.
func (st *Store) SetIssueOrPRLock(repoID, number int, locked bool, reason string) bool {
	st.Mu.Lock()
	defer st.Mu.Unlock()
	if i := st.IssuesByRepo[repoID][number]; i != nil {
		i.Locked = locked
		if locked {
			i.ActiveLockReason = LockReason(reason)
		} else {
			i.ActiveLockReason = ""
		}
		if st.Persist != nil {
			st.Persist.MustPut("issues", strconv.Itoa(i.ID), i)
		}
		return true
	}
	if pr := st.PullsByRepo[repoID][number]; pr != nil {
		pr.Locked = locked
		if locked {
			pr.ActiveLockReason = LockReason(reason)
		} else {
			pr.ActiveLockReason = ""
		}
		if st.Persist != nil {
			st.Persist.MustPut("pull_requests", strconv.Itoa(pr.ID), pr)
		}
		return true
	}
	return false
}

// UpdateCommentBody sets a comment's body and edit metadata (LastEditedAt,
// EditorID). Returns the updated comment or nil when no comment matches.
func (st *Store) UpdateCommentBody(id, editorID int, body string) *Comment {
	st.Mu.Lock()
	defer st.Mu.Unlock()
	c, ok := st.Comments[id]
	if !ok {
		return nil
	}
	now := st.CurrentTime()
	c.Body = body
	c.UpdatedAt = now
	c.LastEditedAt = &now
	c.EditorID = editorID
	if st.Persist != nil {
		st.Persist.MustPut("comments", strconv.Itoa(c.ID), c)
	}
	return cloneComment(c)
}

// LookupCommentByNodeID returns the comment with the given GraphQL node ID, or nil.
func (st *Store) LookupCommentByNodeID(nodeID string) *Comment {
	st.Mu.RLock()
	defer st.Mu.RUnlock()
	if id, ok := DecodeNodeDBID(nodeID, "IC_kgDO"); ok {
		if c := st.Comments[id]; c != nil && c.NodeID == nodeID {
			return cloneComment(c)
		}
	}
	for _, c := range st.Comments {
		if c.NodeID == nodeID {
			return cloneComment(c)
		}
	}
	return nil
}

// SetCommentMinimization sets or clears a comment's minimization. reason is
// OFF_TOPIC / OUTDATED / RESOLVED / DUPLICATE / SPAM / ABUSE to minimize, or
// empty to unminimize (minimizerID ignored when clearing).
func (st *Store) SetCommentMinimization(id, minimizerID int, reason string) *Comment {
	st.Mu.Lock()
	defer st.Mu.Unlock()
	c, ok := st.Comments[id]
	if !ok {
		return nil
	}
	if reason == "" {
		c.MinimizedReason = ""
		c.MinimizedByID = 0
	} else {
		c.MinimizedReason = reason
		c.MinimizedByID = minimizerID
	}
	if st.Persist != nil {
		st.Persist.MustPut("comments", strconv.Itoa(c.ID), c)
	}
	return cloneComment(c)
}

// ListCommentsFor returns the conversation comments on an "issue" or "pull_request" parent.
func (st *Store) ListCommentsFor(parentType string, parentID int) []*Comment {
	st.Mu.RLock()
	defer st.Mu.RUnlock()
	indexed := st.CommentsByParent[CommentCountKey(parentType, parentID)]
	comments := make([]*Comment, 0, len(indexed))
	for _, c := range indexed {
		comments = append(comments, cloneComment(c))
	}
	return snapshotComments(comments)
}

// indexCommentLocked / unindexCommentLocked maintain CommentsByParent so a
// parent's comments resolve without a full scan. Parents are immutable, so
// edits need no re-indexing. Caller holds st.Mu.
func (st *Store) indexCommentLocked(c *Comment) {
	key := CommentCountKey(c.ParentType, c.IssueID)
	st.CommentsByParent[key] = append(st.CommentsByParent[key], c)
}

func (st *Store) unindexCommentLocked(c *Comment) {
	key := CommentCountKey(c.ParentType, c.IssueID)
	slice := st.CommentsByParent[key]
	for i, existing := range slice {
		if existing.ID == c.ID {
			st.CommentsByParent[key] = append(slice[:i], slice[i+1:]...)
			break
		}
	}
	if len(st.CommentsByParent[key]) == 0 {
		delete(st.CommentsByParent, key)
	}
}
