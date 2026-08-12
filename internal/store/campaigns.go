package store

import (
	"sort"
	"strings"
	"time"
)

// Campaign is an organization security campaign.
type Campaign struct {
	Number             int           `json:"number"`
	OrgLogin           string        `json:"org_login"`
	Name               string        `json:"name"`
	Description        string        `json:"description"`
	ManagerLogins      []string      `json:"manager_logins"`
	TeamManagerSlugs   []string      `json:"team_manager_slugs"`
	EndsAt             time.Time     `json:"ends_at"`
	ContactLink        *string       `json:"contact_link"`
	State              string        `json:"state"`
	PublishedAt        time.Time     `json:"published_at"`
	ClosedAt           *time.Time    `json:"closed_at"`
	CreatedAt          time.Time     `json:"created_at"`
	UpdatedAt          time.Time     `json:"updated_at"`
	CodeScanningAlerts map[int][]int `json:"code_scanning_alerts"` // repo ID → alert numbers
}

// ListCampaigns returns the org's campaigns sorted by number.
func (st *Store) ListCampaigns(orgLogin string) []*Campaign {
	st.Mu.RLock()
	defer st.Mu.RUnlock()
	m := st.OrgCampaigns[orgLogin]
	out := make([]*Campaign, 0, len(m))
	for _, c := range m {
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Number < out[j].Number })
	return snapshotCampaigns(out)
}

// GetCampaign returns a campaign by org and number, or nil.
func (st *Store) GetCampaign(orgLogin string, number int) *Campaign {
	st.Mu.RLock()
	defer st.Mu.RUnlock()
	return st.OrgCampaigns[orgLogin][number]
}

// CreateCampaign creates an open campaign with the next per-org number.
func (st *Store) CreateCampaign(orgLogin, name, description string, managers, teamManagers []string, endsAt time.Time, contactLink *string, alerts map[int][]int) *Campaign {
	st.Mu.Lock()
	defer st.Mu.Unlock()
	number := 1
	for n := range st.OrgCampaigns[orgLogin] {
		if n >= number {
			number = n + 1
		}
	}
	now := time.Now().UTC()
	c := &Campaign{
		Number:             number,
		OrgLogin:           orgLogin,
		Name:               name,
		Description:        description,
		ManagerLogins:      managers,
		TeamManagerSlugs:   teamManagers,
		EndsAt:             endsAt.UTC(),
		ContactLink:        contactLink,
		State:              "open",
		PublishedAt:        now,
		CreatedAt:          now,
		UpdatedAt:          now,
		CodeScanningAlerts: alerts,
	}
	if st.OrgCampaigns[orgLogin] == nil {
		st.OrgCampaigns[orgLogin] = map[int]*Campaign{}
	}
	st.OrgCampaigns[orgLogin][number] = c
	if st.Persist != nil {
		st.Persist.MustPut("org_campaigns", orgLogin, st.OrgCampaigns[orgLogin])
	}
	return c
}

// UpdateCampaign applies fn to the campaign under the store lock.
func (st *Store) UpdateCampaign(orgLogin string, number int, fn func(*Campaign)) *Campaign {
	st.Mu.Lock()
	defer st.Mu.Unlock()
	c := st.OrgCampaigns[orgLogin][number]
	if c == nil {
		return nil
	}
	fn(c)
	c.UpdatedAt = time.Now().UTC()
	if st.Persist != nil {
		st.Persist.MustPut("org_campaigns", orgLogin, st.OrgCampaigns[orgLogin])
	}
	return c
}

// DeleteCampaign removes a campaign.
func (st *Store) DeleteCampaign(orgLogin string, number int) {
	st.Mu.Lock()
	defer st.Mu.Unlock()
	delete(st.OrgCampaigns[orgLogin], number)
	if st.Persist != nil {
		st.Persist.MustPut("org_campaigns", orgLogin, st.OrgCampaigns[orgLogin])
	}
}

// RepoBelongsToOrg reports whether the repository lives under the org's
// namespace.
func (st *Store) RepoBelongsToOrg(repo *Repo, orgLogin string) bool {
	owner, _, _ := strings.Cut(repo.FullName, "/")
	return owner == orgLogin
}

// GetCodeScanningAlertForCampaign returns the repo's code scanning alert by
// number, or nil.
func (st *Store) GetCodeScanningAlertForCampaign(repoKey string, number int) *CodeScanningAlert {
	st.Mu.RLock()
	defer st.Mu.RUnlock()
	return st.CodeScanningAlertsByRepo[repoKey][number]
}

// CampaignAlertCounts derives open/closed counts from the current states of
// the campaign's linked code scanning alerts.
func (st *Store) CampaignAlertCounts(c *Campaign) (open, closed int) {
	st.Mu.RLock()
	defer st.Mu.RUnlock()
	for repoID, numbers := range c.CodeScanningAlerts {
		repo := st.Repos[repoID]
		if repo == nil {
			continue
		}
		for _, number := range numbers {
			alert := st.CodeScanningAlertsByRepo[repo.FullName][number]
			if alert == nil {
				continue
			}
			if alert.State == "open" {
				open++
			} else {
				closed++
			}
		}
	}
	return open, closed
}
