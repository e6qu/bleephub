package store

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// CommitComment is a comment on a commit.
type CommitComment struct {
	ID        int       `json:"id"`
	NodeID    string    `json:"node_id"`
	RepoID    int       `json:"repo_id"`
	CommitID  string    `json:"commit_id"`
	AuthorID  int       `json:"author_id"`
	Body      string    `json:"body"`
	Path      string    `json:"path,omitempty"`
	Position  *int      `json:"position,omitempty"`
	Line      *int      `json:"line,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// CommitCommentStore holds commit comments keyed by id, repo, and commit.
type CommitCommentStore struct {
	Mu       sync.RWMutex             `json:"-"`
	ByID     map[int]*CommitComment   `json:"-"`
	ByRepo   map[int][]*CommitComment `json:"-"`
	byCommit map[string][]*CommitComment
	NextID   int          `json:"-"`
	Persist  *Persistence `json:"-"`
}

func newCommitCommentStore(p *Persistence) *CommitCommentStore {
	return &CommitCommentStore{
		ByID:     map[int]*CommitComment{},
		ByRepo:   map[int][]*CommitComment{},
		byCommit: map[string][]*CommitComment{},
		NextID:   1,
		Persist:  p,
	}
}

func commitKey(repoID int, commitID string) string {
	return strconv.Itoa(repoID) + ":" + commitID
}

// Create adds a new commit comment.
func (s *CommitCommentStore) Create(repoID int, commitID string, authorID int, body, path string, position, line *int) *CommitComment {
	s.Mu.Lock()
	defer s.Mu.Unlock()
	id := s.NextID
	s.NextID++
	now := time.Now().UTC()
	c := &CommitComment{
		ID:        id,
		NodeID:    fmt.Sprintf("CC_kgDO%08d", id),
		RepoID:    repoID,
		CommitID:  commitID,
		AuthorID:  authorID,
		Body:      body,
		Path:      path,
		Position:  position,
		Line:      line,
		CreatedAt: now,
		UpdatedAt: now,
	}
	s.ByID[id] = c
	s.ByRepo[repoID] = append(s.ByRepo[repoID], c)
	ck := commitKey(repoID, commitID)
	s.byCommit[ck] = append(s.byCommit[ck], c)
	s.persistComment(c)
	return c
}

// Get returns a commit comment by id.
func (s *CommitCommentStore) Get(id int) *CommitComment {
	s.Mu.RLock()
	defer s.Mu.RUnlock()
	return s.ByID[id]
}

// ListForRepo returns all commit comments for a repo sorted newest-first.
func (s *CommitCommentStore) ListForRepo(repoID int) []*CommitComment {
	s.Mu.RLock()
	defer s.Mu.RUnlock()
	out := make([]*CommitComment, len(s.ByRepo[repoID]))
	copy(out, s.ByRepo[repoID])
	sort.Slice(out, func(i, j int) bool {
		return out[i].CreatedAt.After(out[j].CreatedAt)
	})
	return out
}

// ListForCommit returns commit comments for a specific commit sorted newest-first.
func (s *CommitCommentStore) ListForCommit(repoID int, commitID string) []*CommitComment {
	s.Mu.RLock()
	defer s.Mu.RUnlock()
	ck := commitKey(repoID, commitID)
	out := make([]*CommitComment, len(s.byCommit[ck]))
	copy(out, s.byCommit[ck])
	sort.Slice(out, func(i, j int) bool {
		return out[i].CreatedAt.After(out[j].CreatedAt)
	})
	return out
}

// Update modifies the body of a commit comment.
func (s *CommitCommentStore) Update(id int, body string) bool {
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

// Delete removes a commit comment.
// Delete removes a commit comment. The comment row and its reactions delete
// in one transaction, so a crash cannot durably drop the reactions while the
// comment survives (STORE-001/002).
func (s *CommitCommentStore) Delete(id int, reactions *ReactionStore) bool {
	s.Mu.Lock()
	defer s.Mu.Unlock()
	c := s.ByID[id]
	if c == nil {
		return false
	}
	delete(s.ByID, id)
	repoList := s.ByRepo[c.RepoID]
	for i, x := range repoList {
		if x.ID == id {
			s.ByRepo[c.RepoID] = append(repoList[:i], repoList[i+1:]...)
			break
		}
	}
	ck := commitKey(c.RepoID, c.CommitID)
	commitList := s.byCommit[ck]
	for i, x := range commitList {
		if x.ID == id {
			s.byCommit[ck] = append(commitList[:i], commitList[i+1:]...)
			break
		}
	}
	batch := NewPersistBatch(s.Persist)
	batch.Delete("commit_comments", strconv.Itoa(id))
	if reactions != nil {
		reactions.DeleteParentsBatch("commit_comment", map[int]bool{id: true}, batch)
	}
	if err := batch.Commit(); err != nil {
		panic(&PersistenceFailure{Op: "batch", Bucket: "commit_comments", Key: strconv.Itoa(id), Err: err})
	}
	return true
}

func (s *CommitCommentStore) IDsForRepo(repoID int) map[int]bool {
	s.Mu.RLock()
	defer s.Mu.RUnlock()
	ids := make(map[int]bool, len(s.ByRepo[repoID]))
	for _, c := range s.ByRepo[repoID] {
		ids[c.ID] = true
	}
	return ids
}

func (s *CommitCommentStore) persistComment(c *CommitComment) {
	if s.Persist == nil {
		return
	}
	s.Persist.MustPut("commit_comments", strconv.Itoa(c.ID), c)
}

func (s *CommitCommentStore) deleteRepoBatch(repoID int, batch *PersistBatch) {
	s.Mu.Lock()
	defer s.Mu.Unlock()
	for id, c := range s.ByID {
		if c.RepoID != repoID {
			continue
		}
		delete(s.ByID, id)
		if batch != nil {
			batch.Delete("commit_comments", strconv.Itoa(id))
		} else if s.Persist != nil {
			s.Persist.MustDelete("commit_comments", strconv.Itoa(id))
		}
	}
	delete(s.ByRepo, repoID)
	prefix := strconv.Itoa(repoID) + ":"
	for key := range s.byCommit {
		if strings.HasPrefix(key, prefix) {
			delete(s.byCommit, key)
		}
	}
}
