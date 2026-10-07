package store

import (
	"errors"
	"sort"
	"strconv"
	"strings"
)

// ExternalPropertyInstallation is a GitHub App installation registered to read
// and write an organization's external custom properties, with the values it
// has written. Values are keyed by repository ID so a rename keeps them; a
// value is a string or a list of strings, as custom-property-value allows.
type ExternalPropertyInstallation struct {
	InstallationID int                            `json:"installation_id"`
	DisplayName    string                         `json:"display_name"`
	Properties     map[string]map[int]interface{} `json:"properties"`
}

// ErrExternalPropertyInstallationRegistered and
// ErrExternalPropertyDisplayNameTaken are the two `already_exists` refusals of
// a registration.
var (
	ErrExternalPropertyInstallationRegistered = errors.New("installation already registered")
	ErrExternalPropertyDisplayNameTaken       = errors.New("display name already in use")
)

const externalPropertiesBucket = "org_external_properties"

func cloneExternalPropertyInstallation(reg *ExternalPropertyInstallation) *ExternalPropertyInstallation {
	if reg == nil {
		return nil
	}
	out := &ExternalPropertyInstallation{
		InstallationID: reg.InstallationID,
		DisplayName:    reg.DisplayName,
		Properties:     make(map[string]map[int]interface{}, len(reg.Properties)),
	}
	for name, values := range reg.Properties {
		copied := make(map[int]interface{}, len(values))
		for repoID, value := range values {
			copied[repoID] = CloneCustomPropertyValue(value)
		}
		out.Properties[name] = copied
	}
	return out
}

func (st *Store) persistExternalPropertiesLocked(batch *PersistBatch, orgLogin string) {
	regs := st.OrgExternalProperties[orgLogin]
	if len(regs) == 0 {
		delete(st.OrgExternalProperties, orgLogin)
	}
	if st.Persist == nil {
		return
	}
	if len(regs) == 0 {
		if batch != nil {
			batch.Delete(externalPropertiesBucket, orgLogin)
		} else {
			st.Persist.MustDelete(externalPropertiesBucket, orgLogin)
		}
		return
	}
	if batch != nil {
		batch.Put(externalPropertiesBucket, orgLogin, regs)
	} else {
		st.Persist.MustPut(externalPropertiesBucket, orgLogin, regs)
	}
}

// ListExternalPropertyInstallations returns the org's registrations ordered by
// installation ID.
func (st *Store) ListExternalPropertyInstallations(orgLogin string) []*ExternalPropertyInstallation {
	st.Mu.RLock()
	defer st.Mu.RUnlock()
	regs := st.OrgExternalProperties[orgLogin]
	out := make([]*ExternalPropertyInstallation, 0, len(regs))
	for _, reg := range regs {
		out = append(out, cloneExternalPropertyInstallation(reg))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].InstallationID < out[j].InstallationID })
	return out
}

// GetExternalPropertyInstallation returns one registration, or nil.
func (st *Store) GetExternalPropertyInstallation(orgLogin string, installationID int) *ExternalPropertyInstallation {
	st.Mu.RLock()
	defer st.Mu.RUnlock()
	return cloneExternalPropertyInstallation(st.OrgExternalProperties[orgLogin][installationID])
}

// RegisterExternalPropertyInstallation registers an installation once under a
// display name unique in the org, compared without regard to case.
func (st *Store) RegisterExternalPropertyInstallation(orgLogin string, installationID int, displayName string) (*ExternalPropertyInstallation, error) {
	st.Mu.Lock()
	defer st.Mu.Unlock()
	regs := st.OrgExternalProperties[orgLogin]
	if regs[installationID] != nil {
		return nil, ErrExternalPropertyInstallationRegistered
	}
	for _, reg := range regs {
		if strings.EqualFold(reg.DisplayName, displayName) {
			return nil, ErrExternalPropertyDisplayNameTaken
		}
	}
	if regs == nil {
		regs = map[int]*ExternalPropertyInstallation{}
		st.OrgExternalProperties[orgLogin] = regs
	}
	reg := &ExternalPropertyInstallation{
		InstallationID: installationID,
		DisplayName:    displayName,
		Properties:     map[string]map[int]interface{}{},
	}
	regs[installationID] = reg
	st.persistExternalPropertiesLocked(nil, orgLogin)
	return cloneExternalPropertyInstallation(reg), nil
}

// ExternalPropertyNames lists the properties a registered installation has
// defined in the org, sorted; nil when the installation is not registered.
func (st *Store) ExternalPropertyNames(orgLogin string, installationID int) []string {
	st.Mu.RLock()
	defer st.Mu.RUnlock()
	reg := st.OrgExternalProperties[orgLogin][installationID]
	if reg == nil {
		return nil
	}
	names := make([]string, 0, len(reg.Properties))
	for name := range reg.Properties {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// ExternalPropertyValue is one property value for one repository; a nil Value
// unsets it.
type ExternalPropertyValue struct {
	PropertyName string
	RepoID       int
	Value        interface{}
}

// SetExternalPropertyValues applies a batch for a registered installation. A
// non-null value defines the property if it is new; a null unsets the value
// and never defines a property. Returns false when the installation is not
// registered in the org.
func (st *Store) SetExternalPropertyValues(orgLogin string, installationID int, values []ExternalPropertyValue) bool {
	st.Mu.Lock()
	defer st.Mu.Unlock()
	reg := st.OrgExternalProperties[orgLogin][installationID]
	if reg == nil {
		return false
	}
	for _, v := range values {
		property := reg.Properties[v.PropertyName]
		if v.Value == nil {
			delete(property, v.RepoID)
			continue
		}
		if property == nil {
			property = map[int]interface{}{}
			reg.Properties[v.PropertyName] = property
		}
		property[v.RepoID] = CloneCustomPropertyValue(v.Value)
	}
	st.persistExternalPropertiesLocked(nil, orgLogin)
	return true
}

// DeleteExternalProperty removes a property and every value it holds. Returns
// false when the installation has no such property.
func (st *Store) DeleteExternalProperty(orgLogin string, installationID int, propertyName string) bool {
	st.Mu.Lock()
	defer st.Mu.Unlock()
	reg := st.OrgExternalProperties[orgLogin][installationID]
	if reg == nil {
		return false
	}
	if _, ok := reg.Properties[propertyName]; !ok {
		return false
	}
	delete(reg.Properties, propertyName)
	st.persistExternalPropertiesLocked(nil, orgLogin)
	return true
}

// dropExternalPropertyInstallationLocked unregisters an uninstalled
// installation everywhere; its external properties go with it.
func (st *Store) dropExternalPropertyInstallationLocked(installationID int) {
	for orgLogin, regs := range st.OrgExternalProperties {
		if regs[installationID] == nil {
			continue
		}
		delete(regs, installationID)
		st.persistExternalPropertiesLocked(nil, orgLogin)
	}
}

// dropRepoExternalPropertyValuesLocked removes a repository's values from an
// org's external properties, when the repository is deleted or leaves the org.
func (st *Store) dropRepoExternalPropertyValuesLocked(batch *PersistBatch, orgLogin string, repoID int) {
	changed := false
	for _, reg := range st.OrgExternalProperties[orgLogin] {
		for _, values := range reg.Properties {
			if _, ok := values[repoID]; ok {
				delete(values, repoID)
				changed = true
			}
		}
	}
	if changed {
		st.persistExternalPropertiesLocked(batch, orgLogin)
	}
}

func loadExternalProperties(st *Store) func(key string, raw []byte) error {
	return func(key string, raw []byte) error {
		var m map[int]*ExternalPropertyInstallation
		if err := LoadJSON(raw, &m); err != nil {
			return err
		}
		for id, reg := range m {
			if reg == nil || reg.InstallationID != id {
				return errors.New("external property registration " + strconv.Itoa(id) + " does not name its own installation")
			}
			if reg.Properties == nil {
				reg.Properties = map[string]map[int]interface{}{}
			}
		}
		st.OrgExternalProperties[key] = m
		return nil
	}
}
