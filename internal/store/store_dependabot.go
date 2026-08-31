package store

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
)

// DependabotAlertState is a Dependabot alert's lifecycle state. Only open ⇄
// dismissed transitions are user-driven; fixed and auto_dismissed are
// platform-produced.
type DependabotAlertState string

const (
	DependabotStateOpen          DependabotAlertState = "open"
	DependabotStateDismissed     DependabotAlertState = "dismissed"
	DependabotStateFixed         DependabotAlertState = "fixed"
	DependabotStateAutoDismissed DependabotAlertState = "auto_dismissed"
)

type DependabotAlert struct {
	ID                     int                  `json:"id"`
	NodeID                 string               `json:"node_id"`
	Number                 int                  `json:"number"`
	RepoKey                string               `json:"repo_key"`
	PackageName            string               `json:"package_name"`
	PackageEcosystem       string               `json:"package_ecosystem"`
	ManifestPath           string               `json:"manifest_path"`
	VulnerabilityID        string               `json:"vulnerability_id"` // GHSA id
	CVEID                  string               `json:"cve_id"`
	Severity               string               `json:"severity"`
	State                  DependabotAlertState `json:"state"`
	DismissedReason        string               `json:"dismissed_reason"`
	DismissedComment       string               `json:"dismissed_comment"`
	DismissedByLogin       string               `json:"dismissed_by_login"`
	DismissedAt            *time.Time           `json:"dismissed_at"`
	FixedAt                *time.Time           `json:"fixed_at"`
	AutoDismissedAt        *time.Time           `json:"auto_dismissed_at"`
	Summary                string               `json:"summary"`
	Description            string               `json:"description"`
	VulnerableVersionRange string               `json:"vulnerable_version_range"`
	FirstPatchedVersion    string               `json:"first_patched_version"`
	CreatedAt              time.Time            `json:"created_at"`
	UpdatedAt              time.Time            `json:"updated_at"`
}

// DependabotSecret is a repository-level Dependabot secret. Value is the
// client's libsodium sealed-box ciphertext, never decrypted here.
type DependabotSecret struct {
	Name      string    `json:"name"`
	Value     string    `json:"value"` // encrypted (base64 sealed box)
	KeyID     string    `json:"key_id"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// DependabotOrgSecret is an org-level Dependabot secret with visibility scoping.
type DependabotOrgSecret struct {
	DependabotSecret
	Visibility      string `json:"visibility"`
	SelectedRepoIDs []int  `json:"selected_repository_ids,omitempty"`
}

func (st *Store) CreateDependabotAlertIfNew(repoKey, pkgName, ecosystem, manifest, vulnID, cveID, severity, summary, description, vulnRange, patched string) *DependabotAlert {
	st.Mu.Lock()
	defer st.Mu.Unlock()

	for _, alert := range st.DependabotAlertsByRepo[repoKey] {
		if strings.EqualFold(alert.PackageName, pkgName) &&
			strings.EqualFold(alert.PackageEcosystem, ecosystem) &&
			alert.ManifestPath == manifest &&
			alert.VulnerabilityID == vulnID {
			return cloneDependabotAlert(alert)
		}
	}
	return st.CreateDependabotAlertLocked(repoKey, pkgName, ecosystem, manifest, vulnID, cveID, severity, "open", summary, description, vulnRange, patched)
}

func (st *Store) CreateDependabotAlertLocked(repoKey, pkgName, ecosystem, manifest, vulnID, cveID, severity, state, summary, description, vulnRange, patched string) *DependabotAlert {
	if st.DependabotAlertsByRepo[repoKey] == nil {
		st.DependabotAlertsByRepo[repoKey] = make(map[int]*DependabotAlert)
	}
	if st.DependabotNextNumber[repoKey] == 0 {
		st.DependabotNextNumber[repoKey] = 1
	}

	now := st.CurrentTime()
	if state == "" {
		state = "open"
	}

	number := st.DependabotNextNumber[repoKey]
	st.DependabotNextNumber[repoKey] = number + 1

	a := &DependabotAlert{
		ID:                     st.NextDependabotAlertID,
		NodeID:                 fmt.Sprintf("DPA_%d", st.NextDependabotAlertID),
		Number:                 number,
		RepoKey:                repoKey,
		PackageName:            pkgName,
		PackageEcosystem:       ecosystem,
		ManifestPath:           manifest,
		VulnerabilityID:        vulnID,
		CVEID:                  cveID,
		Severity:               severity,
		State:                  DependabotAlertState(state),
		Summary:                summary,
		Description:            description,
		VulnerableVersionRange: vulnRange,
		FirstPatchedVersion:    patched,
		CreatedAt:              now,
		UpdatedAt:              now,
	}
	st.NextDependabotAlertID++

	st.DependabotAlerts[a.ID] = a
	st.DependabotAlertsByRepo[repoKey][number] = a
	st.persistDependabotAlert(a)
	return cloneDependabotAlert(a)
}

// cloneDependabotAlert returns a detached copy safe outside the store lock
// (STORE-021); the three *time.Time fields are the only reference fields.
func cloneDependabotAlert(a *DependabotAlert) *DependabotAlert {
	if a == nil {
		return nil
	}
	clone := *a
	if a.DismissedAt != nil {
		dismissed := *a.DismissedAt
		clone.DismissedAt = &dismissed
	}
	if a.FixedAt != nil {
		fixed := *a.FixedAt
		clone.FixedAt = &fixed
	}
	if a.AutoDismissedAt != nil {
		auto := *a.AutoDismissedAt
		clone.AutoDismissedAt = &auto
	}
	return &clone
}

func (st *Store) GetDependabotAlert(repoKey string, number int) *DependabotAlert {
	st.Mu.RLock()
	defer st.Mu.RUnlock()
	return cloneDependabotAlert(st.DependabotAlertsByRepo[repoKey][number])
}

// ListDependabotAlerts returns repo alerts filtered and sorted per GitHub's list endpoint.
func (st *Store) ListDependabotAlerts(repoKey, state, severity, packageName, ecosystem, manifest, sortField, direction string) []*DependabotAlert {
	st.Mu.RLock()
	defer st.Mu.RUnlock()

	byRepo := st.DependabotAlertsByRepo[repoKey]
	out := make([]*DependabotAlert, 0, len(byRepo))
	for _, a := range byRepo {
		if state != "" && a.State != DependabotAlertState(state) {
			continue
		}
		if severity != "" && a.Severity != severity {
			continue
		}
		if packageName != "" && !strings.EqualFold(a.PackageName, packageName) {
			continue
		}
		if ecosystem != "" && !strings.EqualFold(a.PackageEcosystem, ecosystem) {
			continue
		}
		if manifest != "" && a.ManifestPath != manifest {
			continue
		}
		out = append(out, a)
	}

	if sortField == "" {
		sortField = "created"
	}
	if direction == "" {
		direction = "desc"
	}

	sort.SliceStable(out, func(i, j int) bool {
		var less bool
		switch sortField {
		case "updated":
			less = out[i].UpdatedAt.Before(out[j].UpdatedAt)
		default:
			less = out[i].CreatedAt.Before(out[j].CreatedAt)
		}
		if direction == "asc" {
			return less
		}
		return !less
	})
	return snapshotDependabotAlerts(out)
}

// UpdateDependabotAlert applies a state/dismissed_reason transition to one alert.
func (st *Store) UpdateDependabotAlert(a *DependabotAlert, state, dismissedReason, dismissedComment string, dismissedBy *User) error {
	st.Mu.Lock()
	defer st.Mu.Unlock()

	// `a` is a detached clone from GetDependabotAlert; mutate the live row and
	// write a fresh snapshot back into `a` for the caller (STORE-021).
	live := st.DependabotAlertsByRepo[a.RepoKey][a.Number]
	if live == nil {
		return fmt.Errorf("dependabot alert %s#%d not found", a.RepoKey, a.Number)
	}
	if err := validateDependabotTransition(string(live.State), state, dismissedReason); err != nil {
		return err
	}

	now := st.CurrentTime()
	switch state {
	case "dismissed":
		live.State = DependabotStateDismissed
		live.DismissedReason = dismissedReason
		live.DismissedComment = dismissedComment
		live.DismissedAt = &now
		if dismissedBy != nil {
			live.DismissedByLogin = dismissedBy.Login
		}
		live.FixedAt = nil
	case "open":
		live.State = DependabotStateOpen
		live.DismissedReason = ""
		live.DismissedComment = ""
		live.DismissedAt = nil
		live.DismissedByLogin = ""
		live.FixedAt = nil
	}
	live.UpdatedAt = now
	st.persistDependabotAlert(live)
	*a = *cloneDependabotAlert(live)
	return nil
}

func validateDependabotTransition(currentState, newState, dismissedReason string) error {
	if newState != "" && newState != "open" && newState != "dismissed" {
		return fmt.Errorf("invalid state %q", newState)
	}
	if newState == "dismissed" && !isValidDependabotDismissedReason(dismissedReason) {
		return fmt.Errorf("invalid dismissed_reason %q", dismissedReason)
	}
	if newState == currentState {
		return nil
	}
	if newState == "dismissed" && currentState == "open" {
		return nil
	}
	if newState == "open" && currentState == "dismissed" {
		return nil
	}
	return fmt.Errorf("invalid transition from %q to %q", currentState, newState)
}

func isValidDependabotDismissedReason(r string) bool {
	switch r {
	case "fix_started", "inaccurate", "no_bandwidth", "not_used", "tolerable_risk":
		return true
	}
	return false
}

func (st *Store) persistDependabotAlert(a *DependabotAlert) {
	if st.Persist != nil {
		st.Persist.MustPut("dependabot_alerts", strconv.Itoa(a.ID), a)
	}
}

func (st *Store) UpsertDependabotSecret(repoKey, name, value, keyID string) bool {
	st.Mu.Lock()
	defer st.Mu.Unlock()

	now := st.CurrentTime()
	m := st.DependabotSecrets[repoKey]
	if m == nil {
		m = make(map[string]*DependabotSecret)
		st.DependabotSecrets[repoKey] = m
	}
	existing := m[name]
	if existing != nil {
		existing.Value = value
		existing.KeyID = keyID
		existing.UpdatedAt = now
	} else {
		m[name] = &DependabotSecret{Name: name, Value: value, KeyID: keyID, CreatedAt: now, UpdatedAt: now}
	}
	if st.Persist != nil {
		st.Persist.MustPut("dependabot_secrets", repoKey, m)
	}
	return existing == nil
}

func (st *Store) DeleteDependabotSecret(repoKey, name string) bool {
	st.Mu.Lock()
	defer st.Mu.Unlock()

	m, ok := st.DependabotSecrets[repoKey]
	if !ok || m[name] == nil {
		return false
	}
	delete(m, name)
	if st.Persist != nil {
		if len(m) > 0 {
			st.Persist.MustPut("dependabot_secrets", repoKey, m)
		} else {
			st.Persist.MustDelete("dependabot_secrets", repoKey)
		}
	}
	return true
}

func (st *Store) UpsertDependabotOrgSecret(orgLogin, name, value, keyID, visibility string, selectedRepoIDs []int) bool {
	st.Mu.Lock()
	defer st.Mu.Unlock()

	// Clone: the slice is stored on the secret below, so don't adopt the
	// caller's backing array.
	selectedRepoIDs = append([]int(nil), selectedRepoIDs...)
	now := st.CurrentTime()
	m := st.DependabotOrgSecrets[orgLogin]
	if m == nil {
		m = make(map[string]*DependabotOrgSecret)
		st.DependabotOrgSecrets[orgLogin] = m
	}
	if visibility != "selected" {
		selectedRepoIDs = nil
	}
	existing := m[name]
	if existing != nil {
		existing.Value = value
		existing.KeyID = keyID
		existing.Visibility = visibility
		existing.SelectedRepoIDs = selectedRepoIDs
		existing.UpdatedAt = now
	} else {
		m[name] = &DependabotOrgSecret{
			DependabotSecret: DependabotSecret{Name: name, Value: value, KeyID: keyID, CreatedAt: now, UpdatedAt: now},
			Visibility:       visibility,
			SelectedRepoIDs:  selectedRepoIDs,
		}
	}
	if st.Persist != nil {
		st.Persist.MustPut("dependabot_org_secrets", orgLogin, m)
	}
	return existing == nil
}

func (st *Store) DeleteDependabotOrgSecret(orgLogin, name string) bool {
	st.Mu.Lock()
	defer st.Mu.Unlock()

	m, ok := st.DependabotOrgSecrets[orgLogin]
	if !ok || m[name] == nil {
		return false
	}
	delete(m, name)
	if st.Persist != nil {
		if len(m) > 0 {
			st.Persist.MustPut("dependabot_org_secrets", orgLogin, m)
		} else {
			st.Persist.MustDelete("dependabot_org_secrets", orgLogin)
		}
	}
	return true
}

// SetDependabotOrgSecretSelectedRepos replaces an org secret's selected repository IDs.
func (st *Store) SetDependabotOrgSecretSelectedRepos(orgLogin, name string, ids []int) (*DependabotOrgSecret, bool) {
	st.Mu.Lock()
	defer st.Mu.Unlock()

	m := st.DependabotOrgSecrets[orgLogin]
	if m == nil || m[name] == nil {
		return nil, false
	}
	sec := m[name]
	sec.SelectedRepoIDs = append([]int(nil), ids...)
	sec.UpdatedAt = st.CurrentTime()
	if st.Persist != nil {
		st.Persist.MustPut("dependabot_org_secrets", orgLogin, m)
	}
	return sec, true
}

// user secrets

type DependabotUserSecret struct {
	DependabotSecret
}

func (st *Store) UpsertDependabotUserSecret(userLogin, name, value, keyID string) bool {
	st.Mu.Lock()
	defer st.Mu.Unlock()

	now := st.CurrentTime()
	m := st.DependabotUserSecrets[userLogin]
	if m == nil {
		m = make(map[string]*DependabotUserSecret)
		st.DependabotUserSecrets[userLogin] = m
	}
	existing := m[name]
	if existing != nil {
		existing.Value = value
		existing.KeyID = keyID
		existing.UpdatedAt = now
	} else {
		m[name] = &DependabotUserSecret{DependabotSecret{Name: name, Value: value, KeyID: keyID, CreatedAt: now, UpdatedAt: now}}
	}
	if st.Persist != nil {
		st.Persist.MustPut("dependabot_user_secrets", userLogin, m)
	}
	return existing == nil
}

func (st *Store) DeleteDependabotUserSecret(userLogin, name string) bool {
	st.Mu.Lock()
	defer st.Mu.Unlock()

	m, ok := st.DependabotUserSecrets[userLogin]
	if !ok || m[name] == nil {
		return false
	}
	delete(m, name)
	if st.Persist != nil {
		if len(m) > 0 {
			st.Persist.MustPut("dependabot_user_secrets", userLogin, m)
		} else {
			st.Persist.MustDelete("dependabot_user_secrets", userLogin)
		}
	}
	return true
}

func (st *Store) GetDependabotUserSecret(userLogin, name string) *DependabotUserSecret {
	st.Mu.RLock()
	defer st.Mu.RUnlock()
	// Detach: the struct has no reference fields, so a value copy is a full
	// snapshot (STORE-021).
	s := st.DependabotUserSecrets[userLogin][name]
	if s == nil {
		return nil
	}
	clone := *s
	return &clone
}

// ListDependabotUserSecrets returns a user's Dependabot secrets sorted by name.
func (st *Store) ListDependabotUserSecrets(userLogin string) []*DependabotUserSecret {
	st.Mu.RLock()
	defer st.Mu.RUnlock()

	m := st.DependabotUserSecrets[userLogin]
	out := make([]*DependabotUserSecret, 0, len(m))
	for _, sec := range m {
		out = append(out, sec)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return snapshotSlice(out)
}

// org repository access

// SetDependabotRepositoryAccess replaces an org's repository access list,
// returning true when no list previously existed.
func (st *Store) SetDependabotRepositoryAccess(orgLogin string, repoIDs []int) bool {
	st.Mu.Lock()
	defer st.Mu.Unlock()

	_, existed := st.DependabotRepositoryAccess[orgLogin]
	st.DependabotRepositoryAccess[orgLogin] = append([]int(nil), repoIDs...)
	if st.Persist != nil {
		if len(repoIDs) > 0 {
			st.Persist.MustPut("dependabot_repo_access", orgLogin, st.DependabotRepositoryAccess[orgLogin])
		} else {
			st.Persist.MustDelete("dependabot_repo_access", orgLogin)
		}
	}
	return !existed
}

func (st *Store) GetDependabotRepositoryAccess(orgLogin string) []int {
	st.Mu.RLock()
	defer st.Mu.RUnlock()
	return append([]int(nil), st.DependabotRepositoryAccess[orgLogin]...)
}

// org alerts

// ListDependabotAlertsByOrg returns alerts for an org's repos, filtered and
// sorted per GitHub's query parameters. Unknown filter values match nothing
// rather than 400.
func (st *Store) ListDependabotAlertsByOrg(orgID int, state, ecosystem, packageName, sortField, direction string) []*DependabotAlert {
	st.Mu.RLock()
	defer st.Mu.RUnlock()

	var out []*DependabotAlert
	for repoKey, byNumber := range st.DependabotAlertsByRepo {
		repo := st.ReposByName[repoKey]
		if repo == nil || repo.OwnerType != "Organization" || repo.OwnerID != orgID {
			continue
		}
		for _, a := range byNumber {
			if state != "" && a.State != DependabotAlertState(state) {
				continue
			}
			if ecosystem != "" && !strings.EqualFold(a.PackageEcosystem, ecosystem) {
				continue
			}
			if packageName != "" && !strings.HasPrefix(a.PackageName, packageName) {
				continue
			}
			out = append(out, a)
		}
	}

	if sortField == "" {
		sortField = "created"
	}
	if direction == "" {
		direction = "desc"
	}
	sort.SliceStable(out, func(i, j int) bool {
		var less bool
		switch sortField {
		case "updated":
			less = out[i].UpdatedAt.Before(out[j].UpdatedAt)
		default:
			less = out[i].CreatedAt.Before(out[j].CreatedAt)
		}
		if direction == "asc" {
			return less
		}
		return !less
	})
	return snapshotDependabotAlerts(out)
}
