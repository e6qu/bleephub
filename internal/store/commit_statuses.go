package store

import (
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

// CommitStatus is a single commit status context.
type CommitStatus struct {
	ID          int               `json:"id"`
	NodeID      string            `json:"node_id"`
	State       CommitStatusState `json:"state"`
	TargetURL   string            `json:"target_url"`
	Description string            `json:"description"`
	Context     string            `json:"context"`
	CreatorID   int               `json:"creator_id"`
	CreatedAt   time.Time         `json:"created_at"`
	UpdatedAt   time.Time         `json:"updated_at"`
}

// CommitStatusStore holds commit statuses keyed by repo+ref.
type CommitStatusStore struct {
	Mu      sync.RWMutex `json:"-"`
	byKey   map[string][]*CommitStatus
	NextID  int          `json:"-"`
	Persist *Persistence `json:"-"`
}

func newCommitStatusStore(p *Persistence) *CommitStatusStore {
	return &CommitStatusStore{
		byKey:   map[string][]*CommitStatus{},
		Persist: p,
	}
}

// Create appends a new status for the given repo+sha.
func (s *CommitStatusStore) Create(repoKey, sha string, creatorID int, state, targetURL, description, context string) *CommitStatus {
	s.Mu.Lock()
	defer s.Mu.Unlock()
	key := statusKey(repoKey, sha)
	if s.NextID < 1 {
		s.NextID = 1
	}
	id := s.NextID
	s.NextID++
	now := time.Now().UTC()
	st := &CommitStatus{
		ID:          id,
		NodeID:      fmt.Sprintf("CS_kgDO%08d", id),
		State:       normalizeStatusState(state),
		TargetURL:   targetURL,
		Description: description,
		Context:     CoalesceStr(context, "default"),
		CreatorID:   creatorID,
		CreatedAt:   now,
		UpdatedAt:   now,
	}
	list := append(s.byKey[key], st)
	if len(list) > maxCommitStatusesPerRef {
		list = list[len(list)-maxCommitStatusesPerRef:]
	}
	s.byKey[key] = list
	s.persistStatuses(key)
	return st
}

// List returns statuses for a repo+ref sorted newest-first.
func (s *CommitStatusStore) List(repoKey, ref string) []*CommitStatus {
	s.Mu.RLock()
	defer s.Mu.RUnlock()
	key := statusKey(repoKey, ref)
	src := s.byKey[key]
	out := make([]*CommitStatus, len(src))
	copy(out, src)
	sort.Slice(out, func(i, j int) bool {
		return out[i].CreatedAt.After(out[j].CreatedAt)
	})
	return out
}

// Combined returns the combined status state plus the latest status per context.
func (s *CommitStatusStore) Combined(repoKey, ref string) (state string, total int, statuses []*CommitStatus) {
	all := s.List(repoKey, ref)
	// latest per context (all is already newest-first)
	latestByCtx := map[string]*CommitStatus{}
	for _, st := range all {
		if _, ok := latestByCtx[st.Context]; !ok {
			latestByCtx[st.Context] = st
		}
	}
	statuses = make([]*CommitStatus, 0, len(latestByCtx))
	for _, st := range latestByCtx {
		statuses = append(statuses, st)
	}
	sort.Slice(statuses, func(i, j int) bool {
		return statuses[i].CreatedAt.After(statuses[j].CreatedAt)
	})
	total = len(statuses)
	state = computeCombinedState(statuses)
	return
}

func (s *CommitStatusStore) persistStatuses(key string) {
	if s.Persist == nil {
		return
	}
	s.Persist.MustPut("commit_statuses", key, s.byKey[key])
}

// moveRepoKeyBatch re-keys a repository's commit-status entries from oldFull to
// newFull on a rename/transfer, staging its durable re-key into batch so it
// commits in the same transaction as the rest of the move.
func (s *CommitStatusStore) moveRepoKeyBatch(oldFull, newFull string, batch *PersistBatch) {
	s.Mu.Lock()
	defer s.Mu.Unlock()
	prefix := oldFull + ":"
	for key, statuses := range s.byKey {
		if !strings.HasPrefix(key, prefix) {
			continue
		}
		newKey := newFull + strings.TrimPrefix(key, oldFull)
		s.byKey[newKey] = statuses
		delete(s.byKey, key)
		batch.Put("commit_statuses", newKey, statuses)
		batch.Delete("commit_statuses", key)
	}
}

func (s *CommitStatusStore) deleteRepoKeyBatch(fullName string, batch *PersistBatch) {
	s.Mu.Lock()
	defer s.Mu.Unlock()
	prefix := fullName + ":"
	for key := range s.byKey {
		if strings.HasPrefix(key, prefix) {
			delete(s.byKey, key)
			if batch != nil {
				batch.Delete("commit_statuses", key)
			} else if s.Persist != nil {
				s.Persist.MustDelete("commit_statuses", key)
			}
		}
	}
}

const (
	CommitStatusSuccess CommitStatusState = "success"
	CommitStatusFailure CommitStatusState = "failure"
	CommitStatusPending CommitStatusState = "pending"
	CommitStatusError   CommitStatusState = "error"
)
