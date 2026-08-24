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
	UpvoterIDs   []int      `json:"upvoter_ids"` // users who upvoted (addUpvote/removeUpvote)
	// Closed, ClosedAt and StateReason carry github's discussion close state:
	// a discussion closes as RESOLVED, OUTDATED or DUPLICATE, and reopening
	// records REOPENED — how both the timeline and the discussion header
	// explain the state.
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
	UpvoterIDs   []int      `json:"upvoter_ids"` // users who upvoted (addUpvote/removeUpvote)
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

// ensureDefaultDiscussionCategoriesBatchLocked creates default categories,
// staging every row into batch so they commit with the repo row that owns them
// in one transaction (STORE-001/002). Callers hold st.Mu.
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

// CreateDiscussionCategory creates a new discussion category in the given repository.
func (st *Store) CreateDiscussionCategory(repoID int, name, emoji, description string, isAnswerable bool) *DiscussionCategory {
	st.Mu.Lock()
	defer st.Mu.Unlock()
	return st.createDiscussionCategoryLocked(repoID, name, emoji, description, isAnswerable)
}

// createDiscussionCategoryLocked creates a category while the caller already holds st.Mu.
func (st *Store) createDiscussionCategoryLocked(repoID int, name, emoji, description string, isAnswerable bool) *DiscussionCategory {
	cat := st.buildDiscussionCategoryLocked(repoID, name, emoji, description, isAnswerable)
	st.persistDiscussionCategory(cat)
	return cat
}

// createDiscussionCategoryBatchLocked creates a category and stages its persist
// into batch instead of committing its own transaction (STORE-001/002).
// Callers hold st.Mu.
func (st *Store) createDiscussionCategoryBatchLocked(batch *PersistBatch, repoID int, name, emoji, description string, isAnswerable bool) *DiscussionCategory {
	cat := st.buildDiscussionCategoryLocked(repoID, name, emoji, description, isAnswerable)
	batch.Put("discussion_categories", strconv.Itoa(cat.ID), cat)
	return cat
}

// buildDiscussionCategoryLocked allocates and indexes a category without
// persisting it. Callers hold st.Mu.
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
	// A copy so a reader can't mutate the stored category through the getter
	// (STORE-021); DiscussionCategory is all-value, so a shallow copy detaches.
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
			// Detach like GetDiscussionCategory — all-value, so a shallow copy.
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

	// Per-repo numbers come from a high-water counter rather than a scan of
	// every discussion in the store: deleted discussions are tombstoned (never
	// removed from st.Discussions), so a scan-for-max grew unbounded and its
	// cost rose with every deletion. The counter only increments, so numbers
	// stay monotonic across tombstones (a deleted number is never reused).
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

// cloneDiscussion returns a copy safe to hand outside the store lock
// (STORE-021): LastEditedAt, PublishedAt and UpvoterIDs are the reference
// fields.
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

// ListDiscussions returns discussions for a repository, optionally filtered by category.
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

// UpdateDiscussion applies a mutation function to a discussion.
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
	// A copy so a reader can't mutate the stored comment through the getter
	// (STORE-021); LastEditedAt and UpvoterIDs are the reference fields.
	return cloneDiscussionComment(st.DiscussionComments[id])
}

// ListDiscussionComments returns comments for a discussion, optionally scoped to a parent.
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
	// ID tie-break: carried-over comments (issue conversion) can share a
	// CreatedAt, and sort.Slice is unstable for equal keys.
	sort.Slice(out, func(i, j int) bool {
		if out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].ID < out[j].ID
		}
		return out[i].CreatedAt.Before(out[j].CreatedAt)
	})
	return snapshotDiscussionComments(out)
}

// UpdateDiscussionComment applies a mutation function to a comment.
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

// MarkDiscussionCommentAsAnswer marks a comment as the answer, unmarking any
// other answer. The unmark and the new answer commit in one transaction so a
// crash cannot leave the discussion with no answer — or two (STORE-001/002).
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

// SetDiscussionUpvote adds (up=true) or removes (up=false) userID's upvote on
// a discussion. Idempotent both ways; reports whether the discussion exists.
// Upvotes deliberately bump neither UpdatedAt nor LastEditedAt — a vote is not
// an edit.
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

// SetDiscussionCommentUpvote adds (up=true) or removes (up=false) userID's
// upvote on a discussion comment. Idempotent both ways; reports whether the
// comment exists.
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

// setUpvoter adds or removes userID from an upvoter set, reporting whether the
// set changed.
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

// CreateDiscussionCommentAt is CreateDiscussionComment with caller-supplied
// timestamps, for flows that carry comments over from another conversation
// (issue → discussion conversion) and must preserve the original authorship
// times rather than stamping the conversion time.
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

// MaxPinnedDiscussions matches github.com's limit of four pinned discussions
// per repository.
const MaxPinnedDiscussions = 4

// ListPinnedDiscussions returns the repo's ordered pinned discussion IDs as a
// detached copy (STORE-021), dropping IDs whose discussion has since been
// deleted.
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

// SetPinnedDiscussions replaces the repo's ordered pinned discussion list.
// The caller validates membership and the MaxPinnedDiscussions cap; the store
// keeps a detached copy of ids so a caller cannot mutate the stored slice
// afterwards (STORE-021).
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

// --- Persistence helpers ---

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
