package store

import (
	"fmt"
	"sort"
	"strconv"
	"sync"
	"time"
)

type PRReviewComment struct {
	ID                int       `json:"id"`
	NodeID            string    `json:"node_id"`
	PullRequestID     int       `json:"-"`
	ReviewID          int       `json:"pull_request_review_id"`
	InReplyToID       int       `json:"in_reply_to_id,omitempty"`
	DiffHunk          string    `json:"diff_hunk"`
	Path              string    `json:"path"`
	Position          *int      `json:"position"`
	OriginalPosition  *int      `json:"original_position"`
	Line              *int      `json:"line"`
	OriginalLine      *int      `json:"original_line"`
	StartLine         *int      `json:"start_line"`
	OriginalStartLine *int      `json:"original_start_line"`
	Side              string    `json:"side"` // LEFT | RIGHT
	StartSide         string    `json:"start_side,omitempty"`
	CommitID          string    `json:"commit_id"`
	OriginalCommitID  string    `json:"original_commit_id"`
	Body              string    `json:"body"`
	AuthorID          int       `json:"-"`
	ThreadID          int       `json:"-"` // shared by thread root + replies
	Resolved          bool      `json:"-"` // thread-level flag, stored on the root
	ResolvedByID      int       `json:"-"` // 0 when unresolved
	CreatedAt         time.Time `json:"created_at"`
	UpdatedAt         time.Time `json:"updated_at"`
}

// prReviewCommentRecord is the persistence DTO: it carries the json:"-" linkage
// fields explicitly so they survive a reload the REST shape omits.
type prReviewCommentRecord struct {
	*PRReviewComment
	PullRequestID int  `json:"pull_request_id"`
	AuthorID      int  `json:"author_id"`
	ThreadID      int  `json:"thread_id"`
	Resolved      bool `json:"resolved"`
	ResolvedByID  int  `json:"resolved_by_id,omitempty"`
}

// restore copies the record's explicit fields back onto the wrapped comment,
// which decode skips because they are json:"-".
func (r *prReviewCommentRecord) restore() *PRReviewComment {
	c := r.PRReviewComment
	c.PullRequestID = r.PullRequestID
	c.AuthorID = r.AuthorID
	c.ThreadID = r.ThreadID
	c.Resolved = r.Resolved
	c.ResolvedByID = r.ResolvedByID
	return c
}

type PRReviewCommentStore struct {
	Mu          sync.RWMutex             `json:"-"`
	ByID        map[int]*PRReviewComment `json:"-"`
	byPR        map[int][]*PRReviewComment
	threadRoots map[int]int
	NextID      int          `json:"-"`
	Persist     *Persistence `json:"-"`
}

// persistComment writes the comment as its storage record. Caller holds s.Mu.
func (s *PRReviewCommentStore) persistComment(c *PRReviewComment) {
	if s.Persist == nil {
		return
	}
	s.Persist.MustPut("pr_review_comments", strconv.Itoa(c.ID), newPRReviewCommentRecord(c))
}

func NewPRReviewCommentStore(p *Persistence) *PRReviewCommentStore {
	return &PRReviewCommentStore{
		ByID:        map[int]*PRReviewComment{},
		byPR:        map[int][]*PRReviewComment{},
		threadRoots: map[int]int{},
		NextID:      1,
		Persist:     p,
	}
}

// CreateRootComment creates a top-level review comment.
func (s *PRReviewCommentStore) CreateRootComment(prID, authorID int, path, body, commitID, side string, line, startLine int) *PRReviewComment {
	s.Mu.Lock()
	defer s.Mu.Unlock()
	id := s.NextID
	s.NextID++
	now := time.Now().UTC()
	c := &PRReviewComment{
		ID:               id,
		NodeID:           fmt.Sprintf("PRRC_kgDO%08d", id),
		PullRequestID:    prID,
		Path:             path,
		Body:             body,
		CommitID:         commitID,
		OriginalCommitID: commitID,
		Side:             CoalesceStr(side, "RIGHT"),
		AuthorID:         authorID,
		ThreadID:         id,
		CreatedAt:        now,
		UpdatedAt:        now,
	}
	if line > 0 {
		c.Line = &line
		c.OriginalLine = &line
		pos := line
		c.Position = &pos
		c.OriginalPosition = &pos
	}
	if startLine > 0 {
		c.StartLine = &startLine
		c.OriginalStartLine = &startLine
	}
	s.ByID[id] = c
	s.byPR[prID] = append(s.byPR[prID], c)
	s.threadRoots[id] = id
	s.persistComment(c)
	return clonePRReviewComment(c)
}

// Reply appends a reply to a root comment.
func (s *PRReviewCommentStore) Reply(prID, rootID, authorID int, body string) *PRReviewComment {
	s.Mu.Lock()
	defer s.Mu.Unlock()
	root, ok := s.ByID[rootID]
	if !ok || root.PullRequestID != prID {
		return nil
	}
	// Walk to the true thread root: replies-to-replies share one thread.
	threadRoot := rootID
	if tr, ok := s.threadRoots[rootID]; ok {
		threadRoot = tr
	}
	id := s.NextID
	s.NextID++
	now := time.Now().UTC()
	c := &PRReviewComment{
		ID:                id,
		NodeID:            fmt.Sprintf("PRRC_kgDO%08d", id),
		PullRequestID:     prID,
		InReplyToID:       rootID,
		Path:              root.Path,
		Body:              body,
		CommitID:          root.CommitID,
		OriginalCommitID:  root.OriginalCommitID,
		Side:              root.Side,
		AuthorID:          authorID,
		Line:              root.Line,
		OriginalLine:      root.OriginalLine,
		StartLine:         root.StartLine,
		OriginalStartLine: root.OriginalStartLine,
		Position:          root.Position,
		OriginalPosition:  root.OriginalPosition,
		ThreadID:          threadRoot,
		CreatedAt:         now,
		UpdatedAt:         now,
	}
	s.ByID[id] = c
	s.byPR[prID] = append(s.byPR[prID], c)
	s.threadRoots[id] = threadRoot
	s.persistComment(c)
	return clonePRReviewComment(c)
}

// AttachToReview links a review comment to the review that created it.
func (s *PRReviewCommentStore) AttachToReview(commentID, reviewID int) bool {
	s.Mu.Lock()
	defer s.Mu.Unlock()
	c := s.ByID[commentID]
	if c == nil {
		return false
	}
	c.ReviewID = reviewID
	s.persistComment(c)
	return true
}

func (s *PRReviewCommentStore) Get(id int) *PRReviewComment {
	s.Mu.RLock()
	defer s.Mu.RUnlock()
	return clonePRReviewComment(s.ByID[id])
}

func (s *PRReviewCommentStore) ListForPR(prID int) []*PRReviewComment {
	s.Mu.RLock()
	defer s.Mu.RUnlock()
	out := make([]*PRReviewComment, len(s.byPR[prID]))
	for i, comment := range s.byPR[prID] {
		out[i] = clonePRReviewComment(comment)
	}
	return out
}

func (s *PRReviewCommentStore) Update(id int, body string) bool {
	s.Mu.Lock()
	defer s.Mu.Unlock()
	c := s.ByID[id]
	if c == nil {
		return false
	}
	c.Body = body
	c.UpdatedAt = time.Now().UTC()
	s.persistComment(c)
	return true
}

// Delete removes a review comment. The comment row and its reactions delete in
// one transaction (STORE-001/002).
func (s *PRReviewCommentStore) Delete(id int, reactions *ReactionStore) bool {
	s.Mu.Lock()
	defer s.Mu.Unlock()
	c := s.ByID[id]
	if c == nil {
		return false
	}
	delete(s.ByID, id)
	src := s.byPR[c.PullRequestID]
	for i, x := range src {
		if x.ID == id {
			s.byPR[c.PullRequestID] = append(src[:i], src[i+1:]...)
			break
		}
	}
	delete(s.threadRoots, id)
	batch := NewPersistBatch(s.Persist)
	batch.Delete("pr_review_comments", strconv.Itoa(id))
	if reactions != nil {
		reactions.DeleteParentsBatch("pull_request_review_comment", map[int]bool{id: true}, batch)
	}
	if err := batch.Commit(); err != nil {
		panic(&PersistenceFailure{Op: "batch", Bucket: "pr_review_comments", Key: strconv.Itoa(id), Err: err})
	}
	return true
}

func (s *PRReviewCommentStore) IDsForPR(prID int) map[int]bool {
	s.Mu.RLock()
	defer s.Mu.RUnlock()
	ids := make(map[int]bool, len(s.byPR[prID]))
	for _, c := range s.byPR[prID] {
		ids[c.ID] = true
	}
	return ids
}

func (s *PRReviewCommentStore) DeleteForPR(prID int) {
	s.DeleteForPRBatch(prID, nil)
}

func (s *PRReviewCommentStore) DeleteForPRBatch(prID int, batch *PersistBatch) {
	s.Mu.Lock()
	defer s.Mu.Unlock()
	for _, c := range s.byPR[prID] {
		delete(s.ByID, c.ID)
		delete(s.threadRoots, c.ID)
		if batch != nil {
			batch.Delete("pr_review_comments", strconv.Itoa(c.ID))
		} else if s.Persist != nil {
			s.Persist.MustDelete("pr_review_comments", strconv.Itoa(c.ID))
		}
	}
	delete(s.byPR, prID)
}

// ResolveThread sets the thread root's Resolved flag and resolver. Unresolving
// clears the resolver.
func (s *PRReviewCommentStore) ResolveThread(threadID int, resolved bool, resolverID int) bool {
	s.Mu.Lock()
	defer s.Mu.Unlock()
	root := s.ByID[threadID]
	if root == nil {
		return false
	}
	root.Resolved = resolved
	if resolved {
		root.ResolvedByID = resolverID
	} else {
		root.ResolvedByID = 0
	}
	root.UpdatedAt = time.Now().UTC()
	s.persistComment(root)
	return true
}

func (s *PRReviewCommentStore) GetThread(threadID int) *ReviewThread {
	s.Mu.RLock()
	defer s.Mu.RUnlock()
	root := s.ByID[threadID]
	if root == nil {
		return nil
	}
	thread := &ReviewThread{ID: threadID, IsResolved: root.Resolved, ResolvedByID: root.ResolvedByID}
	for _, c := range s.byPR[root.PullRequestID] {
		if c.ID == threadID || c.ThreadID == threadID {
			thread.Comments = append(thread.Comments, clonePRReviewComment(c))
		}
	}
	return thread
}

func (s *PRReviewCommentStore) ListThreads(prID int) []*ReviewThread {
	s.Mu.RLock()
	defer s.Mu.RUnlock()
	threads := map[int]*ReviewThread{}
	for _, c := range s.byPR[prID] {
		threadID := c.ThreadID
		if threadID == 0 {
			threadID = c.ID
		}
		t, ok := threads[threadID]
		if !ok {
			t = &ReviewThread{ID: threadID}
			threads[threadID] = t
			if root := s.ByID[threadID]; root != nil {
				t.IsResolved = root.Resolved
				t.ResolvedByID = root.ResolvedByID
			}
		}
		t.Comments = append(t.Comments, clonePRReviewComment(c))
	}
	out := make([]*ReviewThread, 0, len(threads))
	for _, t := range threads {
		out = append(out, t)
	}
	// Thread ID is the root comment's ID, so sorting by it yields stable
	// creation order — a map range would otherwise reshuffle the reviewThreads
	// connection (and its pagination cursors) on every call.
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}
