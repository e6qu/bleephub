package store

import (
	"fmt"
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
	// Regex is the pattern a `string` property's values must match. It is a
	// GraphQL-surface member (createRepositoryCustomProperty carries it);
	// GitHub's REST schema shape does not include it, so the REST renderers
	// leave it out.
	Regex *string `json:"regex,omitempty"`
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

// GetCustomProperty returns a detached snapshot of a property definition by
// name, or nil (STORE-021).
func (st *Store) GetCustomProperty(orgLogin, name string) *CustomProperty {
	st.Mu.RLock()
	defer st.Mu.RUnlock()
	if property := st.OrgCustomProperties[orgLogin][name]; property != nil {
		return cloneCustomProperty(property)
	}
	return cloneCustomProperty(st.EnterpriseSettings.RepositoryCustomProperties[name])
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

// OrgOwnsCustomProperty reports whether the definition is the organization's
// own rather than one inherited from the enterprise schema. The GraphQL
// mutations need the distinction because editing an enterprise-level
// definition is the enterprise owner's call, not the organization's.
func (st *Store) OrgOwnsCustomProperty(orgLogin, name string) bool {
	st.Mu.RLock()
	defer st.Mu.RUnlock()
	return st.OrgCustomProperties[orgLogin][name] != nil
}

// GetEnterpriseCustomProperty returns a detached snapshot of the
// enterprise-level repository property definition by name, or nil.
func (st *Store) GetEnterpriseCustomProperty(name string) *CustomProperty {
	st.Mu.RLock()
	defer st.Mu.RUnlock()
	return cloneCustomProperty(st.EnterpriseSettings.RepositoryCustomProperties[name])
}

// UpsertEnterpriseCustomProperty creates or replaces an enterprise-level
// repository property definition — the same map the enterprise properties
// REST surface writes.
func (st *Store) UpsertEnterpriseCustomProperty(def *CustomProperty) {
	st.Mu.Lock()
	defer st.Mu.Unlock()
	st.EnterpriseSettings.RepositoryCustomProperties[def.PropertyName] = def
	st.PersistEnterpriseSettings()
}

// DeleteEnterpriseCustomProperty removes an enterprise-level repository
// property definition. Returns true when the definition existed.
func (st *Store) DeleteEnterpriseCustomProperty(name string) bool {
	st.Mu.Lock()
	defer st.Mu.Unlock()
	if st.EnterpriseSettings.RepositoryCustomProperties[name] == nil {
		return false
	}
	delete(st.EnterpriseSettings.RepositoryCustomProperties, name)
	st.PersistEnterpriseSettings()
	return true
}

// PromoteCustomProperty copies an organization's property definition into the
// enterprise schema — the same write PUT /enterprises/{e}/properties/schema/
// organizations/{org}/{name}/promote performs — and returns the promoted
// definition, or nil when the organization holds no such definition.
func (st *Store) PromoteCustomProperty(orgLogin, name string) *CustomProperty {
	st.Mu.Lock()
	defer st.Mu.Unlock()
	property := st.OrgCustomProperties[orgLogin][name]
	if property == nil {
		return nil
	}
	promoted := cloneCustomProperty(property)
	st.EnterpriseSettings.RepositoryCustomProperties[name] = promoted
	st.PersistEnterpriseSettings()
	return cloneCustomProperty(promoted)
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

// ValidateCustomPropertyValue checks a non-null value against the property's
// value type (and allowed values for the select types). The REST values
// routes and the GraphQL setRepositoryCustomPropertyValues mutation both ask
// it, so the two surfaces cannot drift on what a value may be.
func ValidateCustomPropertyValue(def *CustomProperty, value interface{}) error {
	allowed := func(str string) bool {
		for _, v := range def.AllowedValues {
			if v == str {
				return true
			}
		}
		return false
	}
	switch def.ValueType {
	case "string", "url":
		if _, ok := value.(string); !ok {
			return fmt.Errorf("property %q expects a string value", def.PropertyName)
		}
	case "true_false":
		str, ok := value.(string)
		if !ok || (str != "true" && str != "false") {
			return fmt.Errorf("property %q expects \"true\" or \"false\"", def.PropertyName)
		}
	case "single_select":
		str, ok := value.(string)
		if !ok || !allowed(str) {
			return fmt.Errorf("property %q expects one of its allowed values", def.PropertyName)
		}
	case "multi_select":
		switch v := value.(type) {
		case string:
			// A bare string is accepted as a one-element selection.
			if !allowed(v) {
				return fmt.Errorf("property %q expects a subset of its allowed values", def.PropertyName)
			}
		case []interface{}:
			for _, item := range v {
				str, ok := item.(string)
				if !ok || !allowed(str) {
					return fmt.Errorf("property %q expects a subset of its allowed values", def.PropertyName)
				}
			}
		case []string:
			for _, item := range v {
				if !allowed(item) {
					return fmt.Errorf("property %q expects a subset of its allowed values", def.PropertyName)
				}
			}
		default:
			return fmt.Errorf("property %q expects an array of allowed values", def.PropertyName)
		}
	default:
		return fmt.Errorf("property %q has unsupported value type %q", def.PropertyName, def.ValueType)
	}
	return nil
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
