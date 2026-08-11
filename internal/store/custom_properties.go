package store

import (
	"sort"
	"strings"
)

// CustomProperty is an organization custom property definition.
type CustomProperty struct {
	PropertyName          string      `json:"property_name"`
	ValueType             string      `json:"value_type"`
	Required              bool        `json:"required"`
	DefaultValue          interface{} `json:"default_value"`
	Description           *string     `json:"description"`
	AllowedValues         []string    `json:"allowed_values"`
	ValuesEditableBy      string      `json:"values_editable_by"`
	RequireExplicitValues bool        `json:"require_explicit_values"`
}

// ListCustomProperties returns the org's property definitions sorted by name.
func (st *Store) ListCustomProperties(orgLogin string) []*CustomProperty {
	st.Mu.RLock()
	defer st.Mu.RUnlock()
	m := st.OrgCustomProperties[orgLogin]
	out := make([]*CustomProperty, 0, len(m)+len(st.EnterpriseSettings.RepositoryCustomProperties))
	for _, p := range m {
		out = append(out, p)
	}
	for name, p := range st.EnterpriseSettings.RepositoryCustomProperties {
		if m[name] == nil {
			out = append(out, p)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].PropertyName < out[j].PropertyName })
	return snapshotCustomProperties(out)
}

// GetCustomProperty returns a property definition by name, or nil.
func (st *Store) GetCustomProperty(orgLogin, name string) *CustomProperty {
	st.Mu.RLock()
	defer st.Mu.RUnlock()
	if property := st.OrgCustomProperties[orgLogin][name]; property != nil {
		return property
	}
	return st.EnterpriseSettings.RepositoryCustomProperties[name]
}

// UpsertCustomProperty creates or replaces a property definition.
func (st *Store) UpsertCustomProperty(orgLogin string, def *CustomProperty) {
	st.Mu.Lock()
	defer st.Mu.Unlock()
	if st.OrgCustomProperties[orgLogin] == nil {
		st.OrgCustomProperties[orgLogin] = map[string]*CustomProperty{}
	}
	st.OrgCustomProperties[orgLogin][def.PropertyName] = def
	if st.Persist != nil {
		st.Persist.MustPut("org_custom_properties", orgLogin, st.OrgCustomProperties[orgLogin])
	}
}

// DeleteCustomProperty removes a property definition and every repo value
// assigned under it. Returns true when the definition existed.
func (st *Store) DeleteCustomProperty(orgLogin, name string) bool {
	st.Mu.Lock()
	defer st.Mu.Unlock()
	if st.OrgCustomProperties[orgLogin][name] == nil {
		return false
	}
	// One transaction: removing the property definition and clearing its value
	// from every repo commit together, so a crash cannot leave a repo carrying a
	// value for a property that no longer exists (STORE-001/002).
	batch := NewPersistBatch(st.Persist)
	delete(st.OrgCustomProperties[orgLogin], name)
	prefix := orgLogin + "/"
	for repoKey, values := range st.RepoCustomPropertyValues {
		if !strings.HasPrefix(repoKey, prefix) {
			continue
		}
		if _, ok := values[name]; ok {
			delete(values, name)
			batch.Put("repo_custom_property_values", repoKey, values)
		}
	}
	batch.Put("org_custom_properties", orgLogin, st.OrgCustomProperties[orgLogin])
	if err := batch.Commit(); err != nil {
		panic(&PersistenceFailure{Op: "batch", Bucket: "org_custom_properties", Err: err})
	}
	return true
}

// SetRepoCustomPropertyValues applies a validated batch of values to one
// repo; null values unset.
func (st *Store) SetRepoCustomPropertyValues(repoKey string, values []CustomPropertyValuePayload) {
	st.Mu.Lock()
	defer st.Mu.Unlock()
	if st.RepoCustomPropertyValues[repoKey] == nil {
		st.RepoCustomPropertyValues[repoKey] = map[string]interface{}{}
	}
	for _, v := range values {
		if v.Value == nil {
			delete(st.RepoCustomPropertyValues[repoKey], v.PropertyName)
		} else {
			// Request arrays/maps are mutable. Adopt a deep copy at the store
			// boundary so applying one batch to several repositories cannot
			// make those repositories share a caller-owned backing array.
			st.RepoCustomPropertyValues[repoKey][v.PropertyName] = CloneCustomPropertyValue(v.Value)
		}
	}
	if st.Persist != nil {
		st.Persist.MustPut("repo_custom_property_values", repoKey, st.RepoCustomPropertyValues[repoKey])
	}
}

// EffectiveRepoCustomPropertyValues renders the repo's property values in the
// custom-property-value shape: the explicitly set value, else the property's
// default. Properties with no effective value are omitted, matching real
// GitHub.
func (st *Store) EffectiveRepoCustomPropertyValues(orgLogin, repoKey string) []map[string]interface{} {
	st.Mu.RLock()
	defer st.Mu.RUnlock()
	defs := make(map[string]*CustomProperty, len(st.EnterpriseSettings.RepositoryCustomProperties)+len(st.OrgCustomProperties[orgLogin]))
	for name, definition := range st.EnterpriseSettings.RepositoryCustomProperties {
		defs[name] = definition
	}
	for name, definition := range st.OrgCustomProperties[orgLogin] {
		defs[name] = definition
	}
	names := make([]string, 0, len(defs))
	for name := range defs {
		names = append(names, name)
	}
	sort.Strings(names)
	set := st.RepoCustomPropertyValues[repoKey]
	out := make([]map[string]interface{}, 0, len(names))
	for _, name := range names {
		value, ok := set[name]
		if !ok && !defs[name].RequireExplicitValues {
			value = defs[name].DefaultValue
		}
		if value == nil {
			continue
		}
		out = append(out, map[string]interface{}{
			"property_name": name,
			"value":         value,
		})
	}
	return out
}

// ListOrgReposForProperties returns the org's repositories, optionally
// filtered by a repository_query keyword matched against the repo name
// (the `repo:owner/name` qualifier is honored as an exact match).
func (st *Store) ListOrgReposForProperties(orgLogin, query string) []*Repo {
	st.Mu.RLock()
	defer st.Mu.RUnlock()
	prefix := orgLogin + "/"
	query = strings.TrimSpace(query)
	out := []*Repo{}
	for key, repo := range st.ReposByName {
		if !strings.HasPrefix(key, prefix) {
			continue
		}
		if query != "" {
			if full, ok := strings.CutPrefix(query, "repo:"); ok {
				if !strings.EqualFold(repo.FullName, full) {
					continue
				}
			} else if !strings.Contains(strings.ToLower(repo.Name), strings.ToLower(query)) {
				continue
			}
		}
		out = append(out, repo)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return snapshotRepos(out)
}

func CloneCustomPropertyValue(value interface{}) interface{} {
	switch typed := value.(type) {
	case []interface{}:
		cloned := make([]interface{}, len(typed))
		for index, item := range typed {
			cloned[index] = CloneCustomPropertyValue(item)
		}
		return cloned
	case map[string]interface{}:
		cloned := make(map[string]interface{}, len(typed))
		for key, item := range typed {
			cloned[key] = CloneCustomPropertyValue(item)
		}
		return cloned
	default:
		return value
	}
}

type CustomPropertyValuePayload struct {
	PropertyName string      `json:"property_name"`
	Value        interface{} `json:"value"`
}
