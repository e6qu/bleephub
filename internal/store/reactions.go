package store

import (
	"fmt"
	"sync"
	"time"
)

// Reaction represents a single user reaction on some parent entity.
//
// ParentType/ParentID/UserID carry real json names so persistence
// round-trips the linkage (the reload path re-indexes byParent from them).
// Client responses never marshal this struct — reactionToJSON emits an
// explicit map.
type Reaction struct {
	ID         int       `json:"id"`
	ParentType string    `json:"parent_type"`
	ParentID   int       `json:"parent_id"`
	Content    string    `json:"content"`
	UserID     int       `json:"user_id"`
	CreatedAt  time.Time `json:"created_at"`
}

// ReactionStore holds reactions keyed by (parentType, parentID).
type ReactionStore struct {
	Mu       sync.RWMutex `json:"-"`
	byParent map[string][]*Reaction
	ByID     map[int]*Reaction `json:"-"`
	NextID   int               `json:"-"`
	Persist  *Persistence      `json:"-"`
}

func newReactionStore(p *Persistence) *ReactionStore {
	return &ReactionStore{
		byParent: make(map[string][]*Reaction),
		ByID:     make(map[int]*Reaction),
		NextID:   1,
		Persist:  p,
	}
}

func reactionParentKey(parentType string, parentID int) string {
	return fmt.Sprintf("%s:%d", parentType, parentID)
}

// AddReaction creates or returns the existing (userID, content) reaction.
// Real GitHub returns the same id on repeat POST (idempotent).
func (rs *ReactionStore) AddReaction(parentType string, parentID int, userID int, content string) (*Reaction, bool, error) {
	if !ValidReactionContent[content] {
		return nil, false, fmt.Errorf("invalid reaction content: %s", content)
	}
	rs.Mu.Lock()
	defer rs.Mu.Unlock()
	key := reactionParentKey(parentType, parentID)
	for _, r := range rs.byParent[key] {
		if r.UserID == userID && r.Content == content {
			return r, true, nil // already exists
		}
	}
	r := &Reaction{
		ID:         rs.NextID,
		ParentType: parentType,
		ParentID:   parentID,
		Content:    content,
		UserID:     userID,
		CreatedAt:  time.Now().UTC(),
	}
	rs.NextID++
	rs.byParent[key] = append(rs.byParent[key], r)
	rs.ByID[r.ID] = r
	if rs.Persist != nil {
		rs.Persist.MustPut("reactions", reactionParentKey(parentType, parentID), rs.byParent[key])
	}
	return r, false, nil
}

// ListReactions returns reactions on a parent, optionally filtered by content.
func (rs *ReactionStore) ListReactions(parentType string, parentID int, contentFilter string) []*Reaction {
	rs.Mu.RLock()
	defer rs.Mu.RUnlock()
	src := rs.byParent[reactionParentKey(parentType, parentID)]
	if contentFilter == "" {
		out := make([]*Reaction, len(src))
		copy(out, src)
		return out
	}
	out := []*Reaction{}
	for _, r := range src {
		if r.Content == contentFilter {
			out = append(out, r)
		}
	}
	return out
}

// DeleteReactionByUser removes a reaction only when it belongs to userID.
// The ownership comparison and deletion happen under one lock so a request
// cannot pass a stale authorization check and delete a replaced record.
func (rs *ReactionStore) DeleteReactionByUser(parentType string, parentID, reactionID, userID int) bool {
	rs.Mu.Lock()
	defer rs.Mu.Unlock()
	r := rs.ByID[reactionID]
	if r == nil || r.ParentType != parentType || r.ParentID != parentID || r.UserID != userID {
		return false
	}
	key := reactionParentKey(parentType, parentID)
	src := rs.byParent[key]
	for i, x := range src {
		if x.ID == reactionID {
			rs.byParent[key] = append(src[:i], src[i+1:]...)
			break
		}
	}
	delete(rs.ByID, reactionID)
	if rs.Persist != nil {
		if len(rs.byParent[key]) > 0 {
			rs.Persist.MustPut("reactions", key, rs.byParent[key])
		} else {
			rs.Persist.MustDelete("reactions", key)
		}
	}
	return true
}

// DeleteParentsBatch removes every reaction attached to the given parent
// entities. A non-nil batch stages the durable deletes into the caller's
// transaction so they commit with the parent rows they belong to
// (STORE-001/002); a nil batch commits each delete independently.
func (rs *ReactionStore) DeleteParentsBatch(parentType string, parentIDs map[int]bool, batch *PersistBatch) {
	if len(parentIDs) == 0 {
		return
	}
	rs.Mu.Lock()
	defer rs.Mu.Unlock()
	for parentID := range parentIDs {
		key := reactionParentKey(parentType, parentID)
		for _, r := range rs.byParent[key] {
			delete(rs.ByID, r.ID)
		}
		delete(rs.byParent, key)
		if batch != nil {
			batch.Delete("reactions", key)
		} else if rs.Persist != nil {
			rs.Persist.MustDelete("reactions", key)
		}
	}
}

// SummarizeReactions computes the per-content counts + total used by
// real GitHub's reactions{url, total_count, +1, ...} block embedded in
// issue / comment / release JSON.
func (rs *ReactionStore) SummarizeReactions(parentType string, parentID int) map[string]interface{} {
	rs.Mu.RLock()
	defer rs.Mu.RUnlock()
	counts := map[string]int{
		"+1": 0, "-1": 0, "laugh": 0, "confused": 0,
		"heart": 0, "hooray": 0, "rocket": 0, "eyes": 0,
	}
	total := 0
	for _, r := range rs.byParent[reactionParentKey(parentType, parentID)] {
		counts[r.Content]++
		total++
	}
	return map[string]interface{}{
		"url":         "", // caller fills in the absolute URL
		"total_count": total,
		"+1":          counts["+1"],
		"-1":          counts["-1"],
		"laugh":       counts["laugh"],
		"confused":    counts["confused"],
		"heart":       counts["heart"],
		"hooray":      counts["hooray"],
		"rocket":      counts["rocket"],
		"eyes":        counts["eyes"],
	}
}

// ValidReactionContent is the canonical set real GitHub accepts.
var ValidReactionContent = map[string]bool{
	"+1":       true,
	"-1":       true,
	"laugh":    true,
	"confused": true,
	"heart":    true,
	"hooray":   true,
	"rocket":   true,
	"eyes":     true,
}
