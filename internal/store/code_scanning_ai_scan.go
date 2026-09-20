package store

// AI Scan on pull requests is a setting with two levels. An organization turns
// it on or off for itself; its repositories inherit that. A repository of an
// enabled organization may opt out, but one of a disabled organization cannot
// opt in — the narrower scope may restrict the broader one and never widen it,
// the rule the Actions enablement chain follows too.

const (
	CodeScanningAIScanEnabled  = "enabled"
	CodeScanningAIScanDisabled = "disabled"
)

// OrgCodeScanningAIScan reports the organization's setting.
func (st *Store) OrgCodeScanningAIScan(orgLogin string) string {
	st.Mu.RLock()
	defer st.Mu.RUnlock()
	return st.orgCodeScanningAIScanLocked(st.OrgByLoginLocked(orgLogin))
}

func (st *Store) orgCodeScanningAIScanLocked(org *Org) string {
	if org != nil && org.CodeScanningAIScan == CodeScanningAIScanEnabled {
		return CodeScanningAIScanEnabled
	}
	return CodeScanningAIScanDisabled
}

// RepoCodeScanningAIScan reports the setting in effect for the repository and
// whether its organization forces it off.
func (st *Store) RepoCodeScanningAIScan(repo *Repo) (setting string, disabledByOrg bool) {
	st.Mu.RLock()
	defer st.Mu.RUnlock()
	current := st.Repos[repo.ID]
	if current == nil {
		return CodeScanningAIScanDisabled, false
	}
	if current.OwnerType == "Organization" {
		if st.orgCodeScanningAIScanLocked(st.Orgs[current.OwnerID]) == CodeScanningAIScanDisabled {
			return CodeScanningAIScanDisabled, true
		}
		if current.CodeScanningAIScan == CodeScanningAIScanDisabled {
			return CodeScanningAIScanDisabled, false
		}
		return CodeScanningAIScanEnabled, false
	}
	if current.CodeScanningAIScan == CodeScanningAIScanEnabled {
		return CodeScanningAIScanEnabled, false
	}
	return CodeScanningAIScanDisabled, false
}
