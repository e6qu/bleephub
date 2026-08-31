package store

import (
	"fmt"
	"sort"
	"strconv"
	"time"
)

// IssueField is an organization-level issue field definition.
type IssueField struct {
	ID          int                 `json:"id"`
	NodeID      string              `json:"node_id"`
	OrgLogin    string              `json:"org_login"`
	Name        string              `json:"name"`
	Description *string             `json:"description"`
	DataType    string              `json:"data_type"`
	Visibility  string              `json:"visibility"`
	Options     []*IssueFieldOption `json:"options,omitempty"`
	CreatedAt   time.Time           `json:"created_at"`
	UpdatedAt   time.Time           `json:"updated_at"`
}

// IssueFieldOption is one option of a single/multi select field.
type IssueFieldOption struct {
	ID          int       `json:"id"`
	Name        string    `json:"name"`
	Description *string   `json:"description"`
	Color       string    `json:"color"`
	Priority    int       `json:"priority"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

// ListIssueFields returns the org's issue fields sorted by ID.
func (st *Store) ListIssueFields(orgLogin string) []*IssueField {
	st.Mu.RLock()
	defer st.Mu.RUnlock()
	m := st.OrgIssueFields[orgLogin]
	out := make([]*IssueField, 0, len(m))
	for _, f := range m {
		out = append(out, f)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return snapshotIssueFields(out)
}

// GetIssueField returns an issue field by org and ID, or nil.
func (st *Store) GetIssueField(orgLogin string, id int) *IssueField {
	st.Mu.RLock()
	defer st.Mu.RUnlock()
	return cloneIssueField(st.OrgIssueFields[orgLogin][id])
}

// buildIssueFieldOptionsLocked materializes option rows from a request,
// preserving CreatedAt for options carrying an existing ID.
func (st *Store) buildIssueFieldOptionsLocked(existing []*IssueFieldOption, reqs []IssueFieldOptionRequest) []*IssueFieldOption {
	now := time.Now().UTC()
	byID := map[int]*IssueFieldOption{}
	for _, opt := range existing {
		byID[opt.ID] = opt
	}
	out := make([]*IssueFieldOption, 0, len(reqs))
	for i, req := range reqs {
		priority := i + 1
		if req.Priority != nil {
			priority = *req.Priority
		}
		if req.ID != nil {
			if prev, ok := byID[*req.ID]; ok {
				prev.Name = *req.Name
				prev.Description = req.Description
				prev.Color = *req.Color
				prev.Priority = priority
				prev.UpdatedAt = now
				out = append(out, prev)
				continue
			}
		}
		out = append(out, &IssueFieldOption{
			ID:          st.NextIssueFieldOptionID,
			Name:        *req.Name,
			Description: req.Description,
			Color:       *req.Color,
			Priority:    priority,
			CreatedAt:   now,
			UpdatedAt:   now,
		})
		st.NextIssueFieldOptionID++
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Priority < out[j].Priority })
	return out
}

// CreateIssueField creates a new organization issue field.
func (st *Store) CreateIssueField(orgLogin, name string, description *string, dataType, visibility string, options []IssueFieldOptionRequest) *IssueField {
	st.Mu.Lock()
	defer st.Mu.Unlock()
	now := time.Now().UTC()
	f := &IssueField{
		ID:          st.NextIssueFieldID,
		NodeID:      fmt.Sprintf("IF_kwDO%08d", st.NextIssueFieldID),
		OrgLogin:    orgLogin,
		Name:        name,
		Description: description,
		DataType:    dataType,
		Visibility:  visibility,
		Options:     st.buildIssueFieldOptionsLocked(nil, options),
		CreatedAt:   now,
		UpdatedAt:   now,
	}
	st.NextIssueFieldID++
	if st.OrgIssueFields[orgLogin] == nil {
		st.OrgIssueFields[orgLogin] = map[int]*IssueField{}
	}
	st.OrgIssueFields[orgLogin][f.ID] = f
	if st.Persist != nil {
		st.Persist.MustPut("org_issue_fields", orgLogin, st.OrgIssueFields[orgLogin])
	}
	return f
}

// UpdateIssueField applies the provided fields; a non-nil options slice
// replaces the entire option set. Returns nil when the field is unknown.
func (st *Store) UpdateIssueField(orgLogin string, id int, name, description, visibility *string, options []IssueFieldOptionRequest) *IssueField {
	st.Mu.Lock()
	defer st.Mu.Unlock()
	f := st.OrgIssueFields[orgLogin][id]
	if f == nil {
		return nil
	}
	if name != nil {
		f.Name = *name
	}
	if description != nil {
		f.Description = description
	}
	if visibility != nil {
		f.Visibility = *visibility
	}
	if options != nil {
		f.Options = st.buildIssueFieldOptionsLocked(f.Options, options)
	}
	f.UpdatedAt = time.Now().UTC()
	if st.Persist != nil {
		st.Persist.MustPut("org_issue_fields", orgLogin, st.OrgIssueFields[orgLogin])
	}
	return f
}

// DeleteIssueField removes an issue field and any per-issue values that
// reference it. Returns true when the field existed.
func (st *Store) DeleteIssueField(orgLogin string, id int) bool {
	st.Mu.Lock()
	defer st.Mu.Unlock()
	if st.OrgIssueFields[orgLogin][id] == nil {
		return false
	}
	// Remove the definition and clear its value from every issue in one
	// transaction, so a crash cannot leave a value for a deleted field
	// (STORE-001/002).
	batch := NewPersistBatch(st.Persist)
	delete(st.OrgIssueFields[orgLogin], id)
	for issueID, values := range st.IssueFieldValues {
		if _, ok := values[id]; ok {
			delete(values, id)
			batch.Put("issue_field_values", strconv.Itoa(issueID), values)
		}
	}
	batch.Put("org_issue_fields", orgLogin, st.OrgIssueFields[orgLogin])
	if err := batch.Commit(); err != nil {
		panic(&PersistenceFailure{Op: "batch", Bucket: "org_issue_fields", Err: err})
	}
	return true
}

// SetIssueFieldValues replaces all field values on an issue.
func (st *Store) SetIssueFieldValues(issueID int, values map[int]interface{}) {
	st.Mu.Lock()
	defer st.Mu.Unlock()
	st.IssueFieldValues[issueID] = values
	if st.Persist != nil {
		st.Persist.MustPut("issue_field_values", strconv.Itoa(issueID), values)
	}
}

// AddIssueFieldValues merges field values into an issue's existing set.
func (st *Store) AddIssueFieldValues(issueID int, values map[int]interface{}) {
	st.Mu.Lock()
	defer st.Mu.Unlock()
	if st.IssueFieldValues[issueID] == nil {
		st.IssueFieldValues[issueID] = map[int]interface{}{}
	}
	for id, v := range values {
		st.IssueFieldValues[issueID][id] = v
	}
	if st.Persist != nil {
		st.Persist.MustPut("issue_field_values", strconv.Itoa(issueID), st.IssueFieldValues[issueID])
	}
}

func (st *Store) DeleteIssueFieldValue(issueID, fieldID int) bool {
	st.Mu.Lock()
	defer st.Mu.Unlock()
	values := st.IssueFieldValues[issueID]
	if _, ok := values[fieldID]; !ok {
		return false
	}
	delete(values, fieldID)
	if st.Persist != nil {
		st.Persist.MustPut("issue_field_values", strconv.Itoa(issueID), values)
	}
	return true
}

// ListIssueFieldValues renders an issue's field values in the REST
// issue-field-value shape, sorted by field ID. Values whose field definition
// no longer exists are skipped.
func (st *Store) ListIssueFieldValues(orgLogin string, issueID int) []map[string]interface{} {
	st.Mu.RLock()
	defer st.Mu.RUnlock()
	values := st.IssueFieldValues[issueID]
	fieldIDs := make([]int, 0, len(values))
	for id := range values {
		fieldIDs = append(fieldIDs, id)
	}
	sort.Ints(fieldIDs)
	out := make([]map[string]interface{}, 0, len(fieldIDs))
	for _, id := range fieldIDs {
		field := st.OrgIssueFields[orgLogin][id]
		if field == nil {
			continue
		}
		out = append(out, issueFieldValueJSON(field, issueID, values[id]))
	}
	return out
}

type IssueFieldOptionRequest struct {
	ID          *int    `json:"id"`
	Name        *string `json:"name"`
	Description *string `json:"description"`
	Color       *string `json:"color"`
	Priority    *int    `json:"priority"`
}

// issueFieldValueJSON renders one issue-field-value. For multi_select the option
// details ride multi_select_options and value is null (the schema's value member
// does not admit arrays).
func issueFieldValueJSON(field *IssueField, issueID int, value interface{}) map[string]interface{} {
	out := map[string]interface{}{
		"issue_field_id": field.ID,
		"node_id":        fmt.Sprintf("IFV_kwDO%08d%08d", issueID, field.ID),
		"data_type":      field.DataType,
		"value":          value,
	}
	optionJSON := func(name string) map[string]interface{} {
		for _, opt := range field.Options {
			if opt.Name == name {
				color := opt.Color
				if color == "" {
					color = "gray"
				}
				return map[string]interface{}{"id": opt.ID, "name": opt.Name, "color": color}
			}
		}
		return nil
	}
	switch field.DataType {
	case "single_select":
		if name, ok := value.(string); ok {
			if opt := optionJSON(name); opt != nil {
				out["single_select_option"] = opt
			}
		}
	case "multi_select":
		names := ToStringSlice(value)
		opts := make([]map[string]interface{}, 0, len(names))
		for _, name := range names {
			if opt := optionJSON(name); opt != nil {
				opts = append(opts, opt)
			}
		}
		out["multi_select_options"] = opts
		out["value"] = nil
	}
	return out
}

// ToStringSlice coerces a stored multi-value ([]string in memory, []interface{}
// after a persistence reload) into []string.
func ToStringSlice(value interface{}) []string {
	switch v := value.(type) {
	case []string:
		return v
	case []interface{}:
		out := make([]string, 0, len(v))
		for _, item := range v {
			if str, ok := item.(string); ok {
				out = append(out, str)
			}
		}
		return out
	}
	return nil
}
