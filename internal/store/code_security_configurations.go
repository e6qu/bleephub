package store

import (
	"net/http"
	"sort"
	"strings"
	"time"
)

// CodeSecurityConfiguration is an organization code security configuration.
type CodeSecurityConfiguration struct {
	ID                                  int       `json:"id"`
	OrgLogin                            string    `json:"org_login"`
	Name                                string    `json:"name"`
	Description                         string    `json:"description"`
	TargetType                          string    `json:"target_type"`
	AdvancedSecurity                    string    `json:"advanced_security"`
	DependencyGraph                     string    `json:"dependency_graph"`
	DependencyGraphAutosubmitAction     string    `json:"dependency_graph_autosubmit_action"`
	DependencyGraphAutosubmitLabeled    *bool     `json:"dependency_graph_autosubmit_labeled"`
	DependabotAlerts                    string    `json:"dependabot_alerts"`
	DependabotSecurityUpdates           string    `json:"dependabot_security_updates"`
	DependabotDelegatedAlertDismissal   string    `json:"dependabot_delegated_alert_dismissal"`
	CodeScanningDefaultSetup            string    `json:"code_scanning_default_setup"`
	CodeScanningRunnerType              *string   `json:"code_scanning_runner_type"`
	CodeScanningRunnerLabel             *string   `json:"code_scanning_runner_label"`
	CodeScanningDelegatedAlertDismissal string    `json:"code_scanning_delegated_alert_dismissal"`
	SecretScanning                      string    `json:"secret_scanning"`
	SecretScanningPushProtection        string    `json:"secret_scanning_push_protection"`
	SecretScanningDelegatedBypass       string    `json:"secret_scanning_delegated_bypass"`
	SecretScanningValidityChecks        string    `json:"secret_scanning_validity_checks"`
	SecretScanningNonProviderPatterns   string    `json:"secret_scanning_non_provider_patterns"`
	SecretScanningGenericSecrets        string    `json:"secret_scanning_generic_secrets"`
	SecretScanningDelegatedDismissal    string    `json:"secret_scanning_delegated_alert_dismissal"`
	SecretScanningExtendedMetadata      string    `json:"secret_scanning_extended_metadata"`
	CodeScanningAllowAdvanced           *bool     `json:"code_scanning_allow_advanced"`
	PrivateVulnerabilityReporting       string    `json:"private_vulnerability_reporting"`
	Enforcement                         string    `json:"enforcement"`
	DefaultForNewRepos                  string    `json:"default_for_new_repos"`
	CreatedAt                           time.Time `json:"created_at"`
	UpdatedAt                           time.Time `json:"updated_at"`
}

// cloneCodeSecurityConfiguration detaches a configuration from the stored row so
// a reader can't race the in-place UpdatedAt/field mutations UpdateCodeSecurityConfiguration
// applies to the live row. Its reference fields are four optional scalars.
func cloneCodeSecurityConfiguration(c *CodeSecurityConfiguration) *CodeSecurityConfiguration {
	if c == nil {
		return nil
	}
	clone := *c
	if c.DependencyGraphAutosubmitLabeled != nil {
		v := *c.DependencyGraphAutosubmitLabeled
		clone.DependencyGraphAutosubmitLabeled = &v
	}
	if c.CodeScanningRunnerType != nil {
		v := *c.CodeScanningRunnerType
		clone.CodeScanningRunnerType = &v
	}
	if c.CodeScanningRunnerLabel != nil {
		v := *c.CodeScanningRunnerLabel
		clone.CodeScanningRunnerLabel = &v
	}
	if c.CodeScanningAllowAdvanced != nil {
		v := *c.CodeScanningAllowAdvanced
		clone.CodeScanningAllowAdvanced = &v
	}
	return &clone
}

// ListCodeSecurityConfigurations returns the org's configurations sorted by ID.
func (st *Store) ListCodeSecurityConfigurations(orgLogin string) []*CodeSecurityConfiguration {
	st.Mu.RLock()
	defer st.Mu.RUnlock()
	m := st.CodeSecurityConfigs[orgLogin]
	out := make([]*CodeSecurityConfiguration, 0, len(m))
	for _, c := range m {
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return snapshotCodeSecurityConfigurations(out)
}

// GetCodeSecurityConfiguration returns a configuration by org and ID, or nil.
func (st *Store) GetCodeSecurityConfiguration(orgLogin string, id int) *CodeSecurityConfiguration {
	st.Mu.RLock()
	defer st.Mu.RUnlock()
	return cloneCodeSecurityConfiguration(st.CodeSecurityConfigs[orgLogin][id])
}

// GetCodeSecurityConfigurationByName returns a configuration by name, or nil.
func (st *Store) GetCodeSecurityConfigurationByName(orgLogin, name string) *CodeSecurityConfiguration {
	st.Mu.RLock()
	defer st.Mu.RUnlock()
	for _, c := range st.CodeSecurityConfigs[orgLogin] {
		if c.Name == name {
			return cloneCodeSecurityConfiguration(c)
		}
	}
	return nil
}

// CreateCodeSecurityConfiguration materializes a configuration with the
// documented per-field creation defaults, then applies the request.
func (st *Store) CreateCodeSecurityConfiguration(orgLogin string, req *CodeSecurityConfigurationRequest) *CodeSecurityConfiguration {
	st.Mu.Lock()
	defer st.Mu.Unlock()
	now := time.Now().UTC()
	c := &CodeSecurityConfiguration{
		ID:                                  st.NextCodeSecurityConfigID,
		OrgLogin:                            orgLogin,
		TargetType:                          "organization",
		AdvancedSecurity:                    "disabled",
		DependencyGraph:                     "enabled",
		DependencyGraphAutosubmitAction:     "disabled",
		DependabotAlerts:                    "disabled",
		DependabotSecurityUpdates:           "disabled",
		DependabotDelegatedAlertDismissal:   "disabled",
		CodeScanningDefaultSetup:            "disabled",
		CodeScanningDelegatedAlertDismissal: "not_set",
		SecretScanning:                      "disabled",
		SecretScanningPushProtection:        "disabled",
		SecretScanningDelegatedBypass:       "disabled",
		SecretScanningValidityChecks:        "disabled",
		SecretScanningNonProviderPatterns:   "disabled",
		SecretScanningGenericSecrets:        "disabled",
		SecretScanningDelegatedDismissal:    "disabled",
		PrivateVulnerabilityReporting:       "disabled",
		Enforcement:                         "enforced",
		DefaultForNewRepos:                  "none",
		CreatedAt:                           now,
		UpdatedAt:                           now,
	}
	st.NextCodeSecurityConfigID++
	req.apply(c)
	if st.CodeSecurityConfigs[orgLogin] == nil {
		st.CodeSecurityConfigs[orgLogin] = map[int]*CodeSecurityConfiguration{}
	}
	st.CodeSecurityConfigs[orgLogin][c.ID] = c
	if st.Persist != nil {
		st.Persist.MustPut("code_security_configurations", orgLogin, st.CodeSecurityConfigs[orgLogin])
	}
	return c
}

// UpdateCodeSecurityConfiguration applies the request; the bool reports
// whether anything actually changed.
func (st *Store) UpdateCodeSecurityConfiguration(orgLogin string, id int, req *CodeSecurityConfigurationRequest) (*CodeSecurityConfiguration, bool) {
	st.Mu.Lock()
	defer st.Mu.Unlock()
	c := st.CodeSecurityConfigs[orgLogin][id]
	if c == nil {
		return nil, false
	}
	if !req.apply(c) {
		return c, false
	}
	c.UpdatedAt = time.Now().UTC()
	if st.Persist != nil {
		st.Persist.MustPut("code_security_configurations", orgLogin, st.CodeSecurityConfigs[orgLogin])
	}
	return c, true
}

// DeleteCodeSecurityConfiguration removes a configuration; repositories it
// was attached to retain their settings but lose the association.
func (st *Store) DeleteCodeSecurityConfiguration(orgLogin string, id int) {
	st.Mu.Lock()
	defer st.Mu.Unlock()
	delete(st.CodeSecurityConfigs[orgLogin], id)
	attachments := st.CodeSecurityRepoAttachments[orgLogin]
	for repoID, configID := range attachments {
		if configID == id {
			delete(attachments, repoID)
		}
	}
	// One transaction: dropping the configuration and detaching it from every
	// repo commit together, so a crash cannot leave a repo attached to a deleted
	// configuration (STORE-001/002).
	batch := NewPersistBatch(st.Persist)
	batch.Put("code_security_configurations", orgLogin, st.CodeSecurityConfigs[orgLogin])
	batch.Put("code_security_repo_attachments", orgLogin, attachments)
	if err := batch.Commit(); err != nil {
		panic(&PersistenceFailure{Op: "batch", Bucket: "code_security_configurations", Err: err})
	}
}

// AttachCodeSecurityConfiguration applies the configuration to the repos the
// scope selects. Returns false when a selected repository ID is not an org
// repository.
func (st *Store) AttachCodeSecurityConfiguration(orgLogin string, id int, scope string, selectedIDs []int) bool {
	st.Mu.Lock()
	defer st.Mu.Unlock()
	prefix := orgLogin + "/"
	orgRepos := map[int]*Repo{}
	for key, repo := range st.ReposByName {
		if strings.HasPrefix(key, prefix) {
			orgRepos[repo.ID] = repo
		}
	}
	if st.CodeSecurityRepoAttachments[orgLogin] == nil {
		st.CodeSecurityRepoAttachments[orgLogin] = map[int]int{}
	}
	attachments := st.CodeSecurityRepoAttachments[orgLogin]

	var targets []int
	switch scope {
	case "selected":
		for _, repoID := range selectedIDs {
			if orgRepos[repoID] == nil {
				return false
			}
			targets = append(targets, repoID)
		}
	default:
		for repoID, repo := range orgRepos {
			switch scope {
			case "all":
				targets = append(targets, repoID)
			case "all_without_configurations":
				if _, ok := attachments[repoID]; !ok {
					targets = append(targets, repoID)
				}
			case "public":
				if !repo.Private {
					targets = append(targets, repoID)
				}
			case "private_or_internal":
				if repo.Private {
					targets = append(targets, repoID)
				}
			}
		}
	}
	for _, repoID := range targets {
		attachments[repoID] = id
	}
	if st.Persist != nil {
		st.Persist.MustPut("code_security_repo_attachments", orgLogin, attachments)
	}
	return true
}

// DetachCodeSecurityConfigurations removes the configuration association
// from the given repositories.
func (st *Store) DetachCodeSecurityConfigurations(orgLogin string, repoIDs []int) {
	st.Mu.Lock()
	defer st.Mu.Unlock()
	attachments := st.CodeSecurityRepoAttachments[orgLogin]
	for _, repoID := range repoIDs {
		delete(attachments, repoID)
	}
	if st.Persist != nil {
		st.Persist.MustPut("code_security_repo_attachments", orgLogin, attachments)
	}
}

// SetCodeSecurityConfigurationAsDefault records the configuration's
// default-for-new-repositories policy, clearing overlapping defaults from
// other configurations.
func (st *Store) SetCodeSecurityConfigurationAsDefault(orgLogin string, id int, defaultFor string) *CodeSecurityConfiguration {
	st.Mu.Lock()
	defer st.Mu.Unlock()
	c := st.CodeSecurityConfigs[orgLogin][id]
	if c == nil {
		return nil
	}
	if defaultFor != "none" {
		for _, other := range st.CodeSecurityConfigs[orgLogin] {
			if other.ID == id || other.DefaultForNewRepos == "none" || other.DefaultForNewRepos == "" {
				continue
			}
			if defaultFor == "all" || other.DefaultForNewRepos == "all" || other.DefaultForNewRepos == defaultFor {
				other.DefaultForNewRepos = "none"
			}
		}
	}
	c.DefaultForNewRepos = defaultFor
	c.UpdatedAt = time.Now().UTC()
	if st.Persist != nil {
		st.Persist.MustPut("code_security_configurations", orgLogin, st.CodeSecurityConfigs[orgLogin])
	}
	return c
}

// ListCodeSecurityConfigurationRepos returns the repositories attached to
// the configuration, sorted by repo ID.
func (st *Store) ListCodeSecurityConfigurationRepos(orgLogin string, id int) []*Repo {
	st.Mu.RLock()
	defer st.Mu.RUnlock()
	out := []*Repo{}
	for repoID, configID := range st.CodeSecurityRepoAttachments[orgLogin] {
		if configID == id {
			if repo := st.Repos[repoID]; repo != nil {
				out = append(out, repo)
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return snapshotRepos(out)
}

// GetRepoCodeSecurityConfiguration returns the configuration attached to the
// repository, or nil.
func (st *Store) GetRepoCodeSecurityConfiguration(orgLogin string, repoID int) *CodeSecurityConfiguration {
	st.Mu.RLock()
	defer st.Mu.RUnlock()
	configID, ok := st.CodeSecurityRepoAttachments[orgLogin][repoID]
	if !ok {
		return nil
	}
	return st.CodeSecurityConfigs[orgLogin][configID]
}

// CodeSecurityConfigurationRequest is the create/update wire shape. The
// code_security / secret_protection members are write-only granular toggles
// real GitHub folds into advanced_security.
type CodeSecurityConfigurationRequest struct {
	Name                             *string `json:"name"`
	Description                      *string `json:"description"`
	AdvancedSecurity                 *string `json:"advanced_security"`
	CodeSecurity                     *string `json:"code_security"`
	SecretProtection                 *string `json:"secret_protection"`
	DependencyGraph                  *string `json:"dependency_graph"`
	DependencyGraphAutosubmitAction  *string `json:"dependency_graph_autosubmit_action"`
	DependencyGraphAutosubmitOptions *struct {
		LabeledRunners *bool `json:"labeled_runners"`
	} `json:"dependency_graph_autosubmit_action_options"`
	DependabotAlerts                  *string `json:"dependabot_alerts"`
	DependabotSecurityUpdates         *string `json:"dependabot_security_updates"`
	DependabotDelegatedAlertDismissal *string `json:"dependabot_delegated_alert_dismissal"`
	CodeScanningDefaultSetup          *string `json:"code_scanning_default_setup"`
	CodeScanningDefaultSetupOptions   *struct {
		RunnerType  *string `json:"runner_type"`
		RunnerLabel *string `json:"runner_label"`
	} `json:"code_scanning_default_setup_options"`
	CodeScanningDelegatedAlertDismissal *string `json:"code_scanning_delegated_alert_dismissal"`
	SecretScanning                      *string `json:"secret_scanning"`
	SecretScanningPushProtection        *string `json:"secret_scanning_push_protection"`
	SecretScanningDelegatedBypass       *string `json:"secret_scanning_delegated_bypass"`
	SecretScanningValidityChecks        *string `json:"secret_scanning_validity_checks"`
	SecretScanningNonProviderPatterns   *string `json:"secret_scanning_non_provider_patterns"`
	SecretScanningGenericSecrets        *string `json:"secret_scanning_generic_secrets"`
	SecretScanningDelegatedDismissal    *string `json:"secret_scanning_delegated_alert_dismissal"`
	SecretScanningExtendedMetadata      *string `json:"secret_scanning_extended_metadata"`
	CodeScanningOptions                 *struct {
		AllowAdvanced *bool `json:"allow_advanced"`
	} `json:"code_scanning_options"`
	PrivateVulnerabilityReporting *string `json:"private_vulnerability_reporting"`
	Enforcement                   *string `json:"enforcement"`
}

// validateEnums checks every provided enum member; returns false after
// writing the validation error.
func (req *CodeSecurityConfigurationRequest) ValidateEnums(w http.ResponseWriter) bool {
	enums := map[string]*string{
		"code_security":                             req.CodeSecurity,
		"secret_protection":                         req.SecretProtection,
		"dependency_graph":                          req.DependencyGraph,
		"dependency_graph_autosubmit_action":        req.DependencyGraphAutosubmitAction,
		"dependabot_alerts":                         req.DependabotAlerts,
		"dependabot_security_updates":               req.DependabotSecurityUpdates,
		"dependabot_delegated_alert_dismissal":      req.DependabotDelegatedAlertDismissal,
		"code_scanning_default_setup":               req.CodeScanningDefaultSetup,
		"code_scanning_delegated_alert_dismissal":   req.CodeScanningDelegatedAlertDismissal,
		"secret_scanning":                           req.SecretScanning,
		"secret_scanning_push_protection":           req.SecretScanningPushProtection,
		"secret_scanning_delegated_bypass":          req.SecretScanningDelegatedBypass,
		"secret_scanning_validity_checks":           req.SecretScanningValidityChecks,
		"secret_scanning_non_provider_patterns":     req.SecretScanningNonProviderPatterns,
		"secret_scanning_generic_secrets":           req.SecretScanningGenericSecrets,
		"secret_scanning_delegated_alert_dismissal": req.SecretScanningDelegatedDismissal,
		"secret_scanning_extended_metadata":         req.SecretScanningExtendedMetadata,
		"private_vulnerability_reporting":           req.PrivateVulnerabilityReporting,
	}
	for field, v := range enums {
		if v != nil && !codeSecurityEnablement[*v] {
			WriteGHValidationError(w, "CodeSecurityConfiguration", field, "invalid")
			return false
		}
	}
	if req.AdvancedSecurity != nil {
		switch *req.AdvancedSecurity {
		case "enabled", "disabled", "code_security", "secret_protection":
		default:
			WriteGHValidationError(w, "CodeSecurityConfiguration", "advanced_security", "invalid")
			return false
		}
	}
	if req.Enforcement != nil && *req.Enforcement != "enforced" && *req.Enforcement != "unenforced" {
		WriteGHValidationError(w, "CodeSecurityConfiguration", "enforcement", "invalid")
		return false
	}
	return true
}

// apply copies every provided member onto the configuration and reports
// whether anything changed.
func (req *CodeSecurityConfigurationRequest) apply(c *CodeSecurityConfiguration) bool {
	changed := false
	setStr := func(dst *string, v *string) {
		if v != nil && *dst != *v {
			*dst = *v
			changed = true
		}
	}
	setStr(&c.Name, req.Name)
	setStr(&c.Description, req.Description)
	setStr(&c.AdvancedSecurity, req.AdvancedSecurity)
	// The granular code_security / secret_protection toggles fold into
	// advanced_security when it is not itself provided.
	if req.AdvancedSecurity == nil && (req.CodeSecurity != nil || req.SecretProtection != nil) {
		cs := req.CodeSecurity != nil && *req.CodeSecurity == "enabled"
		sp := req.SecretProtection != nil && *req.SecretProtection == "enabled"
		folded := "disabled"
		switch {
		case cs && sp:
			folded = "enabled"
		case cs:
			folded = "code_security"
		case sp:
			folded = "secret_protection"
		}
		if c.AdvancedSecurity != folded {
			c.AdvancedSecurity = folded
			changed = true
		}
	}
	setStr(&c.DependencyGraph, req.DependencyGraph)
	setStr(&c.DependencyGraphAutosubmitAction, req.DependencyGraphAutosubmitAction)
	if req.DependencyGraphAutosubmitOptions != nil && req.DependencyGraphAutosubmitOptions.LabeledRunners != nil {
		v := *req.DependencyGraphAutosubmitOptions.LabeledRunners
		if c.DependencyGraphAutosubmitLabeled == nil || *c.DependencyGraphAutosubmitLabeled != v {
			c.DependencyGraphAutosubmitLabeled = &v
			changed = true
		}
	}
	setStr(&c.DependabotAlerts, req.DependabotAlerts)
	setStr(&c.DependabotSecurityUpdates, req.DependabotSecurityUpdates)
	setStr(&c.DependabotDelegatedAlertDismissal, req.DependabotDelegatedAlertDismissal)
	setStr(&c.CodeScanningDefaultSetup, req.CodeScanningDefaultSetup)
	if req.CodeScanningDefaultSetupOptions != nil {
		if v := req.CodeScanningDefaultSetupOptions.RunnerType; v != nil {
			if c.CodeScanningRunnerType == nil || *c.CodeScanningRunnerType != *v {
				c.CodeScanningRunnerType = v
				changed = true
			}
		}
		if v := req.CodeScanningDefaultSetupOptions.RunnerLabel; v != nil {
			if c.CodeScanningRunnerLabel == nil || *c.CodeScanningRunnerLabel != *v {
				c.CodeScanningRunnerLabel = v
				changed = true
			}
		}
	}
	setStr(&c.CodeScanningDelegatedAlertDismissal, req.CodeScanningDelegatedAlertDismissal)
	setStr(&c.SecretScanning, req.SecretScanning)
	setStr(&c.SecretScanningPushProtection, req.SecretScanningPushProtection)
	setStr(&c.SecretScanningDelegatedBypass, req.SecretScanningDelegatedBypass)
	setStr(&c.SecretScanningValidityChecks, req.SecretScanningValidityChecks)
	setStr(&c.SecretScanningNonProviderPatterns, req.SecretScanningNonProviderPatterns)
	setStr(&c.SecretScanningGenericSecrets, req.SecretScanningGenericSecrets)
	setStr(&c.SecretScanningDelegatedDismissal, req.SecretScanningDelegatedDismissal)
	setStr(&c.SecretScanningExtendedMetadata, req.SecretScanningExtendedMetadata)
	if req.CodeScanningOptions != nil && req.CodeScanningOptions.AllowAdvanced != nil {
		v := *req.CodeScanningOptions.AllowAdvanced
		if c.CodeScanningAllowAdvanced == nil || *c.CodeScanningAllowAdvanced != v {
			c.CodeScanningAllowAdvanced = &v
			changed = true
		}
	}
	setStr(&c.PrivateVulnerabilityReporting, req.PrivateVulnerabilityReporting)
	setStr(&c.Enforcement, req.Enforcement)
	return changed
}

var codeSecurityEnablement = map[string]bool{"enabled": true, "disabled": true, "not_set": true}
