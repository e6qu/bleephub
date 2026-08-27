package store

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
)

// GraphQL-only account-activity state: profile status, the lists a user sorts
// starred repositories into, and the follow edges. SetFollow is the single
// primitive both the HTTP handlers and the followUser/unfollowUser mutations go
// through, so the two surfaces cannot disagree.

// --- user status ------------------------------------------------------------

// UserStatus is the message, emoji and availability a user sets on their
// profile. GitHub keeps one per account, so it lives on the account and its node
// id derives from the account's.
type UserStatus struct {
	UserID int    `json:"user_id"`
	Emoji  string `json:"emoji,omitempty"`
	// Message is the status text. An empty message with an emoji is still a
	// status, not its absence.
	Message string `json:"message,omitempty"`
	// OrganizationID scopes the status to one organization's members when set.
	OrganizationID int `json:"organization_id,omitempty"`
	// LimitedAvailability is GitHub's "busy" flag.
	LimitedAvailability bool       `json:"limited_availability,omitempty"`
	ExpiresAt           *time.Time `json:"expires_at,omitempty"`
	CreatedAt           time.Time  `json:"created_at"`
	UpdatedAt           time.Time  `json:"updated_at"`
}

// UserStatusNodeID is the GraphQL node id of a user's status.
func UserStatusNodeID(userID int) string {
	return fmt.Sprintf("US_kgDO%08d", userID)
}

func cloneUserStatus(status *UserStatus) *UserStatus {
	if status == nil {
		return nil
	}
	clone := *status
	clone.ExpiresAt = cloneTimePtr(status.ExpiresAt)
	return &clone
}

// GetUserStatus returns a detached copy of the user's status, or nil when there
// is none or it has expired.
func (st *Store) GetUserStatus(userID int) *UserStatus {
	st.Mu.RLock()
	defer st.Mu.RUnlock()
	user := st.Users[userID]
	if user == nil || user.Status == nil {
		return nil
	}
	if user.Status.ExpiresAt != nil && !user.Status.ExpiresAt.After(st.CurrentTime()) {
		return nil
	}
	return cloneUserStatus(user.Status)
}

// SetUserStatus writes the account's status and returns the stored row. A status
// with neither emoji nor message clears it.
func (st *Store) SetUserStatus(userID int, status UserStatus) *UserStatus {
	st.Mu.Lock()
	defer st.Mu.Unlock()
	user := st.Users[userID]
	if user == nil {
		return nil
	}
	now := st.CurrentTime()
	if status.Emoji == "" && status.Message == "" {
		user.Status = nil
		user.UpdatedAt = now
		if st.Persist != nil {
			st.Persist.MustPut("users", strconv.Itoa(user.ID), user)
		}
		return nil
	}
	status.UserID = userID
	status.UpdatedAt = now
	status.CreatedAt = now
	if user.Status != nil {
		status.CreatedAt = user.Status.CreatedAt
	}
	stored := cloneUserStatus(&status)
	user.Status = stored
	user.UpdatedAt = now
	if st.Persist != nil {
		st.Persist.MustPut("users", strconv.Itoa(user.ID), user)
	}
	return cloneUserStatus(stored)
}

// --- user lists -------------------------------------------------------------

// UserList is one named list a user sorts starred repositories into, public or
// private, with a slug derived from its name.
type UserList struct {
	ID          int    `json:"id"`
	NodeID      string `json:"node_id"`
	UserID      int    `json:"user_id"`
	Name        string `json:"name"`
	Slug        string `json:"slug"`
	Description string `json:"description,omitempty"`
	IsPrivate   bool   `json:"is_private"`
	// RepoIDs are the repositories on the list, in the order they were added.
	RepoIDs     []int      `json:"repo_ids,omitempty"`
	LastAddedAt *time.Time `json:"last_added_at,omitempty"`
	CreatedAt   time.Time  `json:"created_at"`
	UpdatedAt   time.Time  `json:"updated_at"`
}

func cloneUserList(list *UserList) *UserList {
	if list == nil {
		return nil
	}
	clone := *list
	clone.RepoIDs = append([]int(nil), list.RepoIDs...)
	clone.LastAddedAt = cloneTimePtr(list.LastAddedAt)
	return &clone
}

// UserListSlug derives a list's slug: lower case, each run of non-alphanumerics
// collapsed to one hyphen.
func UserListSlug(name string) string {
	var out strings.Builder
	lastHyphen := true
	for _, r := range strings.ToLower(name) {
		switch {
		case (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9'):
			out.WriteRune(r)
			lastHyphen = false
		case !lastHyphen:
			out.WriteByte('-')
			lastHyphen = true
		}
	}
	return strings.Trim(out.String(), "-")
}

// CreateUserList adds a list to the account, or nil when the account is missing,
// the name is blank, or a list with the same slug already exists (a list is
// addressed by slug, so two would collide).
func (st *Store) CreateUserList(userID int, name, description string, private bool) *UserList {
	slug := UserListSlug(name)
	if slug == "" {
		return nil
	}
	st.Mu.Lock()
	defer st.Mu.Unlock()
	if st.Users[userID] == nil {
		return nil
	}
	for _, existing := range st.UserLists {
		if existing.UserID == userID && existing.Slug == slug {
			return nil
		}
	}
	now := st.CurrentTime()
	id := st.ReserveGlobalID("user_lists", &st.NextUserListID)
	list := &UserList{
		ID:          id,
		NodeID:      fmt.Sprintf("ULis_kwDO%08d", id),
		UserID:      userID,
		Name:        name,
		Slug:        slug,
		Description: description,
		IsPrivate:   private,
		CreatedAt:   now,
		UpdatedAt:   now,
	}
	st.UserLists[id] = list
	if st.Persist != nil {
		st.Persist.MustPut("user_lists", strconv.Itoa(id), list)
	}
	return cloneUserList(list)
}

// GetUserList returns a detached copy of one list.
func (st *Store) GetUserList(id int) *UserList {
	st.Mu.RLock()
	defer st.Mu.RUnlock()
	return cloneUserList(st.UserLists[id])
}

// ListUserLists returns the account's lists in creation order.
func (st *Store) ListUserLists(userID int) []*UserList {
	st.Mu.RLock()
	defer st.Mu.RUnlock()
	var out []*UserList
	for _, list := range st.UserLists {
		if list.UserID == userID {
			out = append(out, cloneUserList(list))
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// UpdateUserList applies a change and returns the stored result. A rename that
// would collide with another of the account's lists is refused.
func (st *Store) UpdateUserList(id int, apply func(*UserList)) *UserList {
	st.Mu.Lock()
	defer st.Mu.Unlock()
	list, ok := st.UserLists[id]
	if !ok {
		return nil
	}
	candidate := cloneUserList(list)
	apply(candidate)
	candidate.Slug = UserListSlug(candidate.Name)
	if candidate.Slug == "" {
		return nil
	}
	for _, other := range st.UserLists {
		if other.ID != id && other.UserID == list.UserID && other.Slug == candidate.Slug {
			return nil
		}
	}
	candidate.UpdatedAt = st.CurrentTime()
	st.UserLists[id] = candidate
	if st.Persist != nil {
		st.Persist.MustPut("user_lists", strconv.Itoa(id), candidate)
	}
	return cloneUserList(candidate)
}

// DeleteUserList removes a list.
func (st *Store) DeleteUserList(id int) bool {
	st.Mu.Lock()
	defer st.Mu.Unlock()
	if _, ok := st.UserLists[id]; !ok {
		return false
	}
	delete(st.UserLists, id)
	if st.Persist != nil {
		st.Persist.MustDelete("user_lists", strconv.Itoa(id))
	}
	return true
}

// SetUserListsForRepo puts the repository on exactly the named lists and off the
// account's others, returning the account's lists after the change.
func (st *Store) SetUserListsForRepo(userID, repoID int, listIDs []int) []*UserList {
	wanted := make(map[int]bool, len(listIDs))
	for _, id := range listIDs {
		wanted[id] = true
	}
	st.Mu.Lock()
	defer st.Mu.Unlock()
	now := st.CurrentTime()
	batch := NewPersistBatch(st.Persist)
	var out []*UserList
	for id, list := range st.UserLists {
		if list.UserID != userID {
			continue
		}
		holds := containsInt(list.RepoIDs, repoID)
		switch {
		case wanted[id] && !holds:
			updated := cloneUserList(list)
			updated.RepoIDs = append(updated.RepoIDs, repoID)
			updated.LastAddedAt = &now
			updated.UpdatedAt = now
			st.UserLists[id] = updated
			batch.Put("user_lists", strconv.Itoa(id), updated)
		case !wanted[id] && holds:
			updated := cloneUserList(list)
			updated.RepoIDs = withoutInt(updated.RepoIDs, repoID)
			updated.UpdatedAt = now
			st.UserLists[id] = updated
			batch.Put("user_lists", strconv.Itoa(id), updated)
		}
	}
	if err := batch.Commit(); err != nil {
		panic(&PersistenceFailure{Op: "batch", Bucket: "user_lists", Err: err})
	}
	for _, list := range st.UserLists {
		if list.UserID == userID {
			out = append(out, cloneUserList(list))
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

func containsInt(values []int, want int) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func withoutInt(values []int, drop int) []int {
	out := make([]int, 0, len(values))
	for _, value := range values {
		if value != drop {
			out = append(out, value)
		}
	}
	return out
}

// FindUserListByNodeID resolves a UserList global node id.
func FindUserListByNodeID(st *Store, nodeID string) *UserList {
	st.Mu.RLock()
	defer st.Mu.RUnlock()
	if id, ok := DecodeNodeDBID(nodeID, "ULis_kwDO"); ok {
		if list := st.UserLists[id]; list != nil && list.NodeID == nodeID {
			return list
		}
	}
	for _, list := range st.UserLists {
		if list.NodeID == nodeID {
			return list
		}
	}
	return nil
}

// --- follow edges -----------------------------------------------------------

// SetFollow records or removes a follow edge. The graph is keyed by login, not
// id, because a user may follow an organization.
func (st *Store) SetFollow(follower, target string, following bool) {
	if follower == "" || target == "" || follower == target {
		return
	}
	st.Misc.Mu.Lock()
	defer st.Misc.Mu.Unlock()
	if following {
		if st.Misc.Follows[follower] == nil {
			st.Misc.Follows[follower] = map[string]bool{}
		}
		st.Misc.Follows[follower][target] = true
	} else if st.Misc.Follows[follower] != nil {
		delete(st.Misc.Follows[follower], target)
	}
	if st.Misc.Persist != nil {
		st.Misc.Persist.MustPut("misc", "follows", st.Misc.Follows)
	}
}
