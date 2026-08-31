package store

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// IssueType is an organization-level issue type definition.
type IssueType struct {
	ID          int       `json:"id"`
	NodeID      string    `json:"node_id"`
	OrgLogin    string    `json:"org_login"`
	Name        string    `json:"name"`
	Description *string   `json:"description"`
	Color       *string   `json:"color"`
	IsEnabled   bool      `json:"is_enabled"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

// ListIssueTypes returns the org's issue types sorted by ID.
func (st *Store) ListIssueTypes(orgLogin string) []*IssueType {
	st.Mu.RLock()
	defer st.Mu.RUnlock()
	m := st.OrgIssueTypes[orgLogin]
	out := make([]*IssueType, 0, len(m))
	for _, it := range m {
		out = append(out, it)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return snapshotIssueTypes(out)
}

// GetAssignableIssueTypeForRepo returns an enabled issue type owned by the
// repository's organization. User-owned repositories do not have issue types.
func (st *Store) GetAssignableIssueTypeForRepo(repo *Repo, id int) *IssueType {
	if id <= 0 {
		return nil
	}
	orgLogin := OrgLoginForIssueTypeRepo(repo)
	if orgLogin == "" {
		return nil
	}
	st.Mu.RLock()
	defer st.Mu.RUnlock()
	it := st.OrgIssueTypes[orgLogin][id]
	if it == nil || !it.IsEnabled {
		return nil
	}
	return cloneIssueType(it)
}

// IssueTypeForIssueLocked resolves the issue's assigned type; call with st.Mu
// held. Returns nil when the repo is gone, not org-owned, or the type was removed.
func (st *Store) IssueTypeForIssueLocked(issue *Issue) *IssueType {
	if issue == nil || issue.IssueTypeID == 0 {
		return nil
	}
	repo := st.Repos[issue.RepoID]
	orgLogin := OrgLoginForIssueTypeRepo(repo)
	if orgLogin == "" {
		return nil
	}
	return st.OrgIssueTypes[orgLogin][issue.IssueTypeID]
}

// CreateIssueType creates a new organization issue type.
func (st *Store) CreateIssueType(orgLogin, name string, description, color *string, isEnabled bool) *IssueType {
	st.Mu.Lock()
	defer st.Mu.Unlock()
	now := time.Now().UTC()
	it := &IssueType{
		ID:          st.NextIssueTypeID,
		NodeID:      fmt.Sprintf("IT_kwDO%08d", st.NextIssueTypeID),
		OrgLogin:    orgLogin,
		Name:        name,
		Description: description,
		Color:       color,
		IsEnabled:   isEnabled,
		CreatedAt:   now,
		UpdatedAt:   now,
	}
	st.NextIssueTypeID++
	if st.OrgIssueTypes[orgLogin] == nil {
		st.OrgIssueTypes[orgLogin] = map[int]*IssueType{}
	}
	st.OrgIssueTypes[orgLogin][it.ID] = it
	st.IssueTypesByID[it.ID] = it
	if st.Persist != nil {
		st.Persist.MustPut("org_issue_types", orgLogin, st.OrgIssueTypes[orgLogin])
	}
	return it
}

// UpdateIssueType replaces the mutable fields of an issue type, or returns nil if absent.
func (st *Store) UpdateIssueType(orgLogin string, id int, name string, description, color *string, isEnabled bool) *IssueType {
	st.Mu.Lock()
	defer st.Mu.Unlock()
	it := st.OrgIssueTypes[orgLogin][id]
	if it == nil {
		return nil
	}
	it.Name = name
	it.Description = description
	it.Color = color
	it.IsEnabled = isEnabled
	it.UpdatedAt = time.Now().UTC()
	if st.Persist != nil {
		st.Persist.MustPut("org_issue_types", orgLogin, st.OrgIssueTypes[orgLogin])
	}
	return it
}

// DeleteIssueType removes an issue type. Returns true when it existed.
func (st *Store) DeleteIssueType(orgLogin string, id int) bool {
	st.Mu.Lock()
	defer st.Mu.Unlock()
	if st.OrgIssueTypes[orgLogin][id] == nil {
		return false
	}
	delete(st.OrgIssueTypes[orgLogin], id)
	delete(st.IssueTypesByID, id)
	if st.Persist != nil {
		st.Persist.MustPut("org_issue_types", orgLogin, st.OrgIssueTypes[orgLogin])
	}
	return true
}

func OrgLoginForIssueTypeRepo(repo *Repo) string {
	if repo == nil || repo.OwnerType != "Organization" {
		return ""
	}
	owner, _, ok := strings.Cut(repo.FullName, "/")
	if !ok {
		return ""
	}
	return owner
}
