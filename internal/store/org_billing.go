package store

import (
	"math"
	"sort"
	"strings"
	"time"
)

// OrgBudget is one organization spending budget.
type OrgBudget struct {
	ID                  string            `json:"id"`
	BudgetScope         string            `json:"budget_scope"` // organization | repository | multi_user_customer | user
	BudgetEntityName    string            `json:"budget_entity_name"`
	BudgetAmount        int               `json:"budget_amount"`
	PreventFurtherUsage bool              `json:"prevent_further_usage"`
	BudgetProductSKU    string            `json:"budget_product_sku"`
	BudgetType          string            `json:"budget_type"` // ProductPricing | SkuPricing
	BudgetAlerting      OrgBudgetAlerting `json:"budget_alerting"`
	CreatedAt           time.Time         `json:"created_at"`
}

func (st *Store) persistOrgBudgetsLocked(orgLogin string) {
	if st.Persist == nil {
		return
	}
	if m := st.OrgBudgets[orgLogin]; len(m) > 0 {
		st.Persist.MustPut("org_budgets", orgLogin, m)
	} else {
		st.Persist.MustDelete("org_budgets", orgLogin)
	}
}

// CreateOrgBudget stores a new budget for the organization.
func (st *Store) CreateOrgBudget(orgLogin string, b *OrgBudget) {
	st.Mu.Lock()
	defer st.Mu.Unlock()
	if st.OrgBudgets[orgLogin] == nil {
		st.OrgBudgets[orgLogin] = map[string]*OrgBudget{}
	}
	st.OrgBudgets[orgLogin][b.ID] = b
	st.persistOrgBudgetsLocked(orgLogin)
}

// GetOrgBudget returns a budget by ID, or nil.
func (st *Store) GetOrgBudget(orgLogin, id string) *OrgBudget {
	st.Mu.RLock()
	defer st.Mu.RUnlock()
	return st.OrgBudgets[orgLogin][id]
}

// ListOrgBudgets returns the org's budgets ordered by creation time then ID.
func (st *Store) ListOrgBudgets(orgLogin string) []*OrgBudget {
	st.Mu.RLock()
	defer st.Mu.RUnlock()
	out := make([]*OrgBudget, 0, len(st.OrgBudgets[orgLogin]))
	for _, b := range st.OrgBudgets[orgLogin] {
		out = append(out, b)
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].CreatedAt.Before(out[j].CreatedAt)
		}
		return out[i].ID < out[j].ID
	})
	return snapshotOrgBudgets(out)
}

// UpdateOrgBudget applies fn to a budget under the write lock. Returns the
// updated budget, or nil when it does not exist.
func (st *Store) UpdateOrgBudget(orgLogin, id string, fn func(*OrgBudget)) *OrgBudget {
	st.Mu.Lock()
	defer st.Mu.Unlock()
	b := st.OrgBudgets[orgLogin][id]
	if b == nil {
		return nil
	}
	fn(b)
	st.persistOrgBudgetsLocked(orgLogin)
	return b
}

// DeleteOrgBudget removes a budget. Returns true if it existed.
func (st *Store) DeleteOrgBudget(orgLogin, id string) bool {
	st.Mu.Lock()
	defer st.Mu.Unlock()
	if st.OrgBudgets[orgLogin][id] == nil {
		return false
	}
	delete(st.OrgBudgets[orgLogin], id)
	st.persistOrgBudgetsLocked(orgLogin)
	return true
}

// orgActionsUsageLines computes real Actions usage from the recorded
// workflow run history (current runs plus archived attempts): every
// completed job is billed per started minute, rounded up, exactly as GitHub
// meters Actions. Returns lines filtered to the requested year/month/day
// (zero month/day mean "whole year"/"whole month").
func (st *Store) OrgActionsUsageLines(orgLogin string, year, month, day int) []ActionsUsageLine {
	st.Mu.RLock()
	defer st.Mu.RUnlock()

	type key struct {
		Date string `json:"-"`
		Repo string `json:"-"`
	}
	minutes := map[key]int{}
	prefix := orgLogin + "/"
	addRun := func(wf *Workflow) {
		if !strings.HasPrefix(wf.RepoFullName, prefix) {
			return
		}
		repoName := strings.TrimPrefix(wf.RepoFullName, prefix)
		for _, job := range wf.Jobs {
			if job.StartedAt.IsZero() || job.CompletedAt.IsZero() || !job.CompletedAt.After(job.StartedAt) {
				continue
			}
			started := job.StartedAt.UTC()
			if started.Year() != year {
				continue
			}
			if month != 0 && int(started.Month()) != month {
				continue
			}
			if day != 0 && started.Day() != day {
				continue
			}
			mins := int(math.Ceil(job.CompletedAt.Sub(job.StartedAt).Minutes()))
			if mins < 1 {
				mins = 1
			}
			minutes[key{started.Format("2006-01-02"), repoName}] += mins
		}
	}
	for _, wf := range st.Workflows {
		addRun(wf)
	}
	for _, attempts := range st.WorkflowAttempts {
		for _, wf := range attempts {
			addRun(wf)
		}
	}

	out := make([]ActionsUsageLine, 0, len(minutes))
	for k, mins := range minutes {
		out = append(out, ActionsUsageLine{Date: k.Date, OrgName: orgLogin, RepoName: k.Repo, Minutes: mins})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Date != out[j].Date {
			return out[i].Date < out[j].Date
		}
		return out[i].RepoName < out[j].RepoName
	})
	return out
}

// ActionsUsageLine is one billable Actions usage line item: minutes consumed
// by workflow jobs of one repository on one date.
type ActionsUsageLine struct {
	Date     string `json:"-"`
	OrgName  string `json:"-"`
	RepoName string `json:"-"`
	Minutes  int    `json:"-"`
}

// OrgBudgetAlerting is the alert configuration on a budget.
type OrgBudgetAlerting struct {
	WillAlert       bool     `json:"will_alert"`
	AlertRecipients []string `json:"alert_recipients"`
}
