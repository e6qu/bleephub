package store

import (
	"fmt"
	"sort"
	"strconv"
	"time"
)

// DiscussionCategory is a repository discussion category.
type DiscussionCategory struct {
	ID           int       `json:"id"`
	NodeID       string    `json:"node_id"`
	RepoID       int       `json:"repo_id"`
	Name         string    `json:"name"`
	Emoji        string    `json:"emoji"`
	Description  string    `json:"description"`
	IsAnswerable bool      `json:"is_answerable"`
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
}

// Discussion is a repository discussion.
type Discussion struct {
	ID           int        `json:"id"`
	NodeID       string     `json:"node_id"`
	RepoID       int        `json:"repo_id"`
	CategoryID   int        `json:"category_id"`
	Number       int        `json:"number"`
	Title        string     `json:"title"`
	Body         string     `json:"body"`
	AuthorID     int        `json:"author_id"`
	Locked       bool       `json:"locked"`
	LockedReason string     `json:"locked_reason"`
	CreatedAt    time.Time  `json:"created_at"`
	UpdatedAt    time.Time  `json:"updated_at"`
	LastEditedAt *time.Time `json:"last_edited_at"`
	PublishedAt  *time.Time `json:"published_at"`
	Deleted      bool       `json:"deleted"`
	UpvoterIDs   []int      `json:"upvoter_ids"`
	// StateReason is github's close reason: RESOLVED, OUTDATED, DUPLICATE, or
	// REOPENED.
	Closed      bool       `json:"closed"`
	ClosedAt    *time.Time `json:"closed_at"`
	StateReason string     `json:"state_reason"`
}

// DiscussionComment is a comment on a discussion (top-level or reply).
type DiscussionComment struct {
	ID           int        `json:"id"`
	NodeID       string     `json:"node_id"`
	DiscussionID int        `json:"discussion_id"`
	AuthorID     int        `json:"author_id"`
	Body         string     `json:"body"`
	CreatedAt    time.Time  `json:"created_at"`
	UpdatedAt    time.Time  `json:"updated_at"`
	LastEditedAt *time.Time `json:"last_edited_at"`
	IsAnswer     bool       `json:"is_answer"`
	ParentID     int        `json:"parent_id"`
	Deleted      bool       `json:"deleted"`
	UpvoterIDs   []int      `json:"upvoter_ids"`
}

func DiscussionCategoryNodeID(id int) string {
	return fmt.Sprintf("DGC_kgDO%08d", id)
}

func discussionNodeID(id int) string {
	return fmt.Sprintf("D_kgDO%08d", id)
}

func discussionCommentNodeID(id int) string {
	return fmt.Sprintf("DC_kgDO%08d", id)
}

// ensureDefaultDiscussionCategoriesBatchLocked stages the default categories
// into batch so they commit with their owning repo row (STORE-001/002).
// Callers hold st.Mu.
func (st *Store) ensureDefaultDiscussionCategoriesBatchLocked(batch *PersistBatch, repoID int) {
	defaults := []struct {
		Name         string `json:"-"`
		emoji        string
		description  string
		isAnswerable bool
	}{
		{"General", ":speech_balloon:", "Chat about anything and everything here", false},
		{"Ideas", ":bulb:", "Share ideas for new features or improvements", false},
		{"Q&A", ":question:", "Ask the community for help", true},
		{"Show and tell", ":raised_hands:", "Show off something you've made", false},
		{"Polls", ":bar_chart:", "Take a vote from the community", false},
	}
	for _, d := range defaults {
		st.createDiscussionCategoryBatchLocked(batch, repoID, d.Name, d.emoji, d.description, d.isAnswerable)
	}
}

// CreateDiscussionCategory creates a discussion category.
func (st *Store) CreateDiscussionCategory(repoID int, name, emoji, description string, isAnswerable bool) *DiscussionCategory {
	st.Mu.Lock()
	defer st.Mu.Unlock()
	return st.createDiscussionCategoryLocked(repoID, name, emoji, description, isAnswerable)
}

// createDiscussionCategoryLocked creates a category; callers hold st.Mu.
func (st *Store) createDiscussionCategoryLocked(repoID int, name, emoji, description string, isAnswerable bool) *DiscussionCategory {
	cat := st.buildDiscussionCategoryLocked(repoID, name, emoji, description, isAnswerable)
	st.persistDiscussionCategory(cat)
	return cat
}

// createDiscussionCategoryBatchLocked creates a category, staging its persist
// into batch rather than committing its own (STORE-001/002). Callers hold st.Mu.
func (st *Store) createDiscussionCategoryBatchLocked(batch *PersistBatch, repoID int, name, emoji, description string, isAnswerable bool) *DiscussionCategory {
	cat := st.buildDiscussionCategoryLocked(repoID, name, emoji, description, isAnswerable)
	batch.Put("discussion_categories", strconv.Itoa(cat.ID), cat)
	return cat
}

// buildDiscussionCategoryLocked allocates and indexes a category without
// persisting it; callers hold st.Mu.
func (st *Store) buildDiscussionCategoryLocked(repoID int, name, emoji, description string, isAnswerable bool) *DiscussionCategory {
	now := st.CurrentTime()
	cat := &DiscussionCategory{
		ID:           st.NextDiscussionCategoryID,
		NodeID:       DiscussionCategoryNodeID(st.NextDiscussionCategoryID),
		RepoID:       repoID,
		Name:         name,
		Emoji:        emoji,
		Description:  description,
		IsAnswerable: isAnswerable,
		CreatedAt:    now,
		UpdatedAt:    now,
	}
	st.DiscussionCategories[cat.ID] = cat
	st.NextDiscussionCategoryID++
	return cat
}

// GetDiscussionCategory returns a category by global ID.
func (st *Store) GetDiscussionCategory(id int) *DiscussionCategory {
	st.Mu.RLock()
	defer st.Mu.RUnlock()
	// Detached copy (STORE-021); all-value, so a shallow copy suffices.
	cat := st.DiscussionCategories[id]
	if cat == nil {
		return nil
	}
	clone := *cat
	return &clone
}

// GetDiscussionCategoryByName returns a category by repo and name.
func (st *Store) GetDiscussionCategoryByName(repoID int, name string) *DiscussionCategory {
	st.Mu.RLock()
	defer st.Mu.RUnlock()
	for _, cat := range st.DiscussionCategories {
		if cat.RepoID == repoID && cat.Name == name {
			clone := *cat
			return &clone
		}
	}
	return nil
}

// ListDiscussionCategories returns all categories for a repository.
func (st *Store) ListDiscussionCategories(repoID int) []*DiscussionCategory {
	st.Mu.RLock()
	defer st.Mu.RUnlock()
	var out []*DiscussionCategory
	for _, cat := range st.DiscussionCategories {
		if cat.RepoID == repoID {
			out = append(out, cat)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return snapshotSlice(out)
}

// CreateDiscussion creates a new discussion in the given repository.
func (st *Store) CreateDiscussion(repoID, categoryID, authorID int, title, body string) *Discussion {
	st.Mu.Lock()
	defer st.Mu.Unlock()

	// A high-water counter, not a scan-for-max: deleted discussions are
	// tombstoned, so numbers stay monotonic and a deleted number is never reused.
	number := st.NextDiscussionNumber[repoID]
	if number == 0 {
		number = 1
	}

	now := st.CurrentTime()
	d := &Discussion{
		ID:          st.NextDiscussionID,
		NodeID:      discussionNodeID(st.NextDiscussionID),
		RepoID:      repoID,
		CategoryID:  categoryID,
		Number:      number,
		Title:       title,
		Body:        body,
		AuthorID:    authorID,
		CreatedAt:   now,
		UpdatedAt:   now,
		PublishedAt: &now,
	}
	st.Discussions[d.ID] = d
	st.NextDiscussionID++
	st.NextDiscussionNumber[repoID] = number + 1
	st.persistDiscussion(d)
	return d
}

// cloneDiscussion returns a detached copy (STORE-021), deep-copying the
// LastEditedAt, PublishedAt and UpvoterIDs reference fields.
func cloneDiscussion(d *Discussion) *Discussion {
	if d == nil {
		return nil
	}
	clone := *d
	if d.LastEditedAt != nil {
		edited := *d.LastEditedAt
		clone.LastEditedAt = &edited
	}
	if d.PublishedAt != nil {
		published := *d.PublishedAt
		clone.PublishedAt = &published
	}
	if d.UpvoterIDs != nil {
		clone.UpvoterIDs = append([]int(nil), d.UpvoterIDs...)
	}
	return &clone
}

// GetDiscussion returns a discussion by global ID.
func (st *Store) GetDiscussion(id int) *Discussion {
	st.Mu.RLock()
	defer st.Mu.RUnlock()
	return cloneDiscussion(st.Discussions[id])
}

// GetDiscussionByNumber returns a discussion by repo and number.
func (st *Store) GetDiscussionByNumber(repoID, number int) *Discussion {
	st.Mu.RLock()
	defer st.Mu.RUnlock()
	for _, d := range st.Discussions {
		if d.RepoID == repoID && d.Number == number && !d.Deleted {
			return cloneDiscussion(d)
		}
	}
	return nil
}

// ListDiscussions returns a repository's discussions, optionally filtered by category.
func (st *Store) ListDiscussions(repoID, categoryID int) []*Discussion {
	st.Mu.RLock()
	defer st.Mu.RUnlock()
	var out []*Discussion
	for _, d := range st.Discussions {
		if d.RepoID != repoID || d.Deleted {
			continue
		}
		if categoryID != 0 && d.CategoryID != categoryID {
			continue
		}
		out = append(out, d)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	return snapshotDiscussions(out)
}

// UpdateDiscussion applies fn to a discussion.
func (st *Store) UpdateDiscussion(id int, fn func(*Discussion)) bool {
	st.Mu.Lock()
	defer st.Mu.Unlock()
	d, ok := st.Discussions[id]
	if !ok || d.Deleted {
		return false
	}
	fn(d)
	now := st.CurrentTime()
	if d.LastEditedAt == nil {
		d.LastEditedAt = &now
	} else {
		*d.LastEditedAt = now
	}
	d.UpdatedAt = now
	st.persistDiscussion(d)
	return true
}

// DeleteDiscussion soft-deletes a discussion.
func (st *Store) DeleteDiscussion(id int) bool {
	st.Mu.Lock()
	defer st.Mu.Unlock()
	d, ok := st.Discussions[id]
	if !ok || d.Deleted {
		return false
	}
	d.Deleted = true
	d.UpdatedAt = st.CurrentTime()
	st.persistDiscussion(d)
	return true
}

// CreateDiscussionComment creates a new top-level comment or reply on a discussion.
func (st *Store) CreateDiscussionComment(discussionID, authorID int, body string, parentID int) *DiscussionComment {
	st.Mu.Lock()
	defer st.Mu.Unlock()

	now := st.CurrentTime()
	c := &DiscussionComment{
		ID:           st.NextDiscussionCommentID,
		NodeID:       discussionCommentNodeID(st.NextDiscussionCommentID),
		DiscussionID: discussionID,
		AuthorID:     authorID,
		Body:         body,
		CreatedAt:    now,
		UpdatedAt:    now,
		ParentID:     parentID,
	}
	st.DiscussionComments[c.ID] = c
	st.NextDiscussionCommentID++
	st.persistDiscussionComment(c)
	return c
}

// GetDiscussionComment returns a comment by global ID.
func (st *Store) GetDiscussionComment(id int) *DiscussionComment {
	st.Mu.RLock()
	defer st.Mu.RUnlock()
	return cloneDiscussionComment(st.DiscussionComments[id])
}

// ListDiscussionComments returns a discussion's comments, optionally scoped to a parent.
func (st *Store) ListDiscussionComments(discussionID, parentID int) []*DiscussionComment {
	st.Mu.RLock()
	defer st.Mu.RUnlock()
	var out []*DiscussionComment
	for _, c := range st.DiscussionComments {
		if c.DiscussionID != discussionID || c.Deleted {
			continue
		}
		if c.ParentID != parentID {
			continue
		}
		out = append(out, c)
	}
	// ID tie-break: issue-conversion comments can share a CreatedAt and
	// sort.Slice is unstable.
	sort.Slice(out, func(i, j int) bool {
		if out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].ID < out[j].ID
		}
		return out[i].CreatedAt.Before(out[j].CreatedAt)
	})
	return snapshotDiscussionComments(out)
}

// UpdateDiscussionComment applies fn to a comment.
func (st *Store) UpdateDiscussionComment(id int, fn func(*DiscussionComment)) bool {
	st.Mu.Lock()
	defer st.Mu.Unlock()
	c, ok := st.DiscussionComments[id]
	if !ok || c.Deleted {
		return false
	}
	fn(c)
	now := st.CurrentTime()
	if c.LastEditedAt == nil {
		c.LastEditedAt = &now
	} else {
		*c.LastEditedAt = now
	}
	c.UpdatedAt = now
	st.persistDiscussionComment(c)
	return true
}

// DeleteDiscussionComment soft-deletes a comment.
func (st *Store) DeleteDiscussionComment(id int) bool {
	st.Mu.Lock()
	defer st.Mu.Unlock()
	c, ok := st.DiscussionComments[id]
	if !ok || c.Deleted {
		return false
	}
	c.Deleted = true
	c.UpdatedAt = st.CurrentTime()
	st.persistDiscussionComment(c)
	return true
}

// MarkDiscussionCommentAsAnswer marks a comment as the answer. The unmark of
// any prior answer and the new answer commit in one transaction, so a crash
// cannot leave zero or two answers (STORE-001/002).
func (st *Store) MarkDiscussionCommentAsAnswer(id int) bool {
	st.Mu.Lock()
	defer st.Mu.Unlock()
	c, ok := st.DiscussionComments[id]
	if !ok || c.Deleted {
		return false
	}
	batch := NewPersistBatch(st.Persist)
	for _, other := range st.DiscussionComments {
		if other.DiscussionID == c.DiscussionID && other.IsAnswer {
			other.IsAnswer = false
			other.UpdatedAt = st.CurrentTime()
			batch.Put("discussion_comments", strconv.Itoa(other.ID), other)
		}
	}
	c.IsAnswer = true
	c.UpdatedAt = st.CurrentTime()
	batch.Put("discussion_comments", strconv.Itoa(c.ID), c)
	if err := batch.Commit(); err != nil {
		panic(&PersistenceFailure{Op: "batch", Bucket: "discussion_comments", Key: strconv.Itoa(c.ID), Err: err})
	}
	return true
}

// SetDiscussionUpvote adds (up) or removes userID's upvote, idempotently,
// reporting whether the discussion exists. A vote is not an edit, so it bumps
// neither UpdatedAt nor LastEditedAt.
func (st *Store) SetDiscussionUpvote(id, userID int, up bool) bool {
	st.Mu.Lock()
	defer st.Mu.Unlock()
	d, ok := st.Discussions[id]
	if !ok || d.Deleted {
		return false
	}
	if changed, next := setUpvoter(d.UpvoterIDs, userID, up); changed {
		d.UpvoterIDs = next
		st.persistDiscussion(d)
	}
	return true
}

// SetDiscussionCommentUpvote adds (up) or removes userID's upvote,
// idempotently, reporting whether the comment exists.
func (st *Store) SetDiscussionCommentUpvote(id, userID int, up bool) bool {
	st.Mu.Lock()
	defer st.Mu.Unlock()
	c, ok := st.DiscussionComments[id]
	if !ok || c.Deleted {
		return false
	}
	if changed, next := setUpvoter(c.UpvoterIDs, userID, up); changed {
		c.UpvoterIDs = next
		st.persistDiscussionComment(c)
	}
	return true
}

// setUpvoter adds or removes userID, reporting whether the set changed.
func setUpvoter(ids []int, userID int, up bool) (bool, []int) {
	for i, existing := range ids {
		if existing != userID {
			continue
		}
		if up {
			return false, ids
		}
		return true, append(ids[:i], ids[i+1:]...)
	}
	if !up {
		return false, ids
	}
	return true, append(ids, userID)
}

// UnmarkDiscussionCommentAsAnswer unmarks a comment as the answer.
func (st *Store) UnmarkDiscussionCommentAsAnswer(id int) bool {
	st.Mu.Lock()
	defer st.Mu.Unlock()
	c, ok := st.DiscussionComments[id]
	if !ok || c.Deleted || !c.IsAnswer {
		return false
	}
	c.IsAnswer = false
	c.UpdatedAt = st.CurrentTime()
	st.persistDiscussionComment(c)
	return true
}

// CreateDiscussionCommentAt is CreateDiscussionComment with a caller-supplied
// timestamp, so issue→discussion conversion preserves original authorship times.
func (st *Store) CreateDiscussionCommentAt(discussionID, authorID int, body string, parentID int, createdAt time.Time) *DiscussionComment {
	st.Mu.Lock()
	defer st.Mu.Unlock()

	c := &DiscussionComment{
		ID:           st.NextDiscussionCommentID,
		NodeID:       discussionCommentNodeID(st.NextDiscussionCommentID),
		DiscussionID: discussionID,
		AuthorID:     authorID,
		Body:         body,
		CreatedAt:    createdAt,
		UpdatedAt:    createdAt,
		ParentID:     parentID,
	}
	st.DiscussionComments[c.ID] = c
	st.NextDiscussionCommentID++
	st.persistDiscussionComment(c)
	return c
}

// MaxPinnedDiscussions is github.com's per-repository pinned-discussion limit.
const MaxPinnedDiscussions = 4

// ListPinnedDiscussions returns the repo's ordered pinned discussion IDs
// (detached, STORE-021), dropping any whose discussion was deleted.
func (st *Store) ListPinnedDiscussions(repoID int) []int {
	st.Mu.RLock()
	defer st.Mu.RUnlock()
	ids := st.PinnedDiscussions[repoID]
	out := make([]int, 0, len(ids))
	for _, id := range ids {
		if d := st.Discussions[id]; d != nil && !d.Deleted && d.RepoID == repoID {
			out = append(out, id)
		}
	}
	return out
}

// SetPinnedDiscussions replaces the repo's ordered pinned list. The caller
// validates membership and the MaxPinnedDiscussions cap; the store keeps a
// detached copy (STORE-021).
func (st *Store) SetPinnedDiscussions(repoID int, ids []int) []int {
	st.Mu.Lock()
	defer st.Mu.Unlock()
	stored := append([]int(nil), ids...)
	st.PinnedDiscussions[repoID] = stored
	if st.Persist != nil {
		st.Persist.MustPut("pinned_discussions", strconv.Itoa(repoID), stored)
	}
	return append([]int(nil), stored...)
}

// Persistence helpers

func (st *Store) persistDiscussionCategory(cat *DiscussionCategory) {
	if st.Persist != nil {
		st.Persist.MustPut("discussion_categories", strconv.Itoa(cat.ID), cat)
	}
}

func (st *Store) persistDiscussion(d *Discussion) {
	if st.Persist != nil {
		st.Persist.MustPut("discussions", strconv.Itoa(d.ID), d)
	}
}

func (st *Store) persistDiscussionComment(c *DiscussionComment) {
	if st.Persist != nil {
		st.Persist.MustPut("discussion_comments", strconv.Itoa(c.ID), c)
	}
}

// DiscussionPoll is a discussion's optional poll. VotesByUser keys on user id,
// making github's one-vote-per-poll rule structural.
type DiscussionPoll struct {
	ID           int    `json:"id"`
	NodeID       string `json:"node_id"`
	DiscussionID int    `json:"discussion_id"`
	Question     string `json:"question"`
	// Options in authored order; vote-count order is derived at read time.
	Options     []*DiscussionPollOption `json:"options"`
	VotesByUser map[int]int             `json:"votes_by_user"`
}

// DiscussionPollOption is one answer in a discussion poll.
type DiscussionPollOption struct {
	ID     int    `json:"id"`
	NodeID string `json:"node_id"`
	PollID int    `json:"poll_id"`
	Option string `json:"option"`
}

// CreateDiscussionPoll attaches a poll to a discussion, refusing a second so
// cast votes are never replaced.
func (st *Store) CreateDiscussionPoll(discussionID int, question string, options []string) *DiscussionPoll {
	st.Mu.Lock()
	defer st.Mu.Unlock()
	discussion := st.Discussions[discussionID]
	if discussion == nil || question == "" || len(options) == 0 {
		return nil
	}
	for _, poll := range st.DiscussionPolls {
		if poll.DiscussionID == discussionID {
			return nil
		}
	}
	poll := &DiscussionPoll{
		ID:           st.NextDiscussionPollID,
		NodeID:       fmt.Sprintf("DP_kwDO%08d", st.NextDiscussionPollID),
		DiscussionID: discussionID,
		Question:     question,
		VotesByUser:  map[int]int{},
	}
	st.NextDiscussionPollID++
	for _, option := range options {
		poll.Options = append(poll.Options, &DiscussionPollOption{
			ID:     st.NextDiscussionPollOptionID,
			NodeID: fmt.Sprintf("DPO_kwDO%08d", st.NextDiscussionPollOptionID),
			PollID: poll.ID,
			Option: option,
		})
		st.NextDiscussionPollOptionID++
	}
	st.DiscussionPolls[poll.ID] = poll
	st.persistDiscussionPollLocked(poll)
	return cloneDiscussionPoll(poll)
}

// GetDiscussionPoll returns a discussion's poll as a detached snapshot, or nil.
func (st *Store) GetDiscussionPoll(discussionID int) *DiscussionPoll {
	st.Mu.RLock()
	defer st.Mu.RUnlock()
	for _, poll := range st.DiscussionPolls {
		if poll.DiscussionID == discussionID {
			return cloneDiscussionPoll(poll)
		}
	}
	return nil
}

// FindDiscussionPollOptionByNodeID resolves an option's global id to the live
// option and its poll (Find* live-row convention).
func FindDiscussionPollOptionByNodeID(st *Store, nodeID string) (*DiscussionPollOption, *DiscussionPoll) {
	if nodeID == "" {
		return nil, nil
	}
	st.Mu.RLock()
	defer st.Mu.RUnlock()
	for _, poll := range st.DiscussionPolls {
		for _, option := range poll.Options {
			if option.NodeID == nodeID {
				return option, poll
			}
		}
	}
	return nil, nil
}

// CastDiscussionPollVote records userID's vote, replacing any earlier vote in
// the same poll (github lets a voter change their mind, not vote twice).
func (st *Store) CastDiscussionPollVote(pollID, optionID, userID int) bool {
	st.Mu.Lock()
	defer st.Mu.Unlock()
	poll := st.DiscussionPolls[pollID]
	if poll == nil {
		return false
	}
	valid := false
	for _, option := range poll.Options {
		if option.ID == optionID {
			valid = true
			break
		}
	}
	if !valid {
		return false
	}
	poll.VotesByUser[userID] = optionID
	st.persistDiscussionPollLocked(poll)
	return true
}

func clone_int_map(m map[int]int) map[int]int {
	out := make(map[int]int, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

func cloneDiscussionPoll(poll *DiscussionPoll) *DiscussionPoll {
	if poll == nil {
		return nil
	}
	out := *poll
	out.Options = make([]*DiscussionPollOption, len(poll.Options))
	for i, option := range poll.Options {
		copied := *option
		out.Options[i] = &copied
	}
	out.VotesByUser = clone_int_map(poll.VotesByUser)
	return &out
}

func (st *Store) persistDiscussionPollLocked(poll *DiscussionPoll) {
	if st.Persist != nil {
		st.Persist.MustPut("discussion_polls", strconv.Itoa(poll.ID), poll)
	}
}
