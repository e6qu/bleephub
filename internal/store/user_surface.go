package store

import (
	"math"
	"sort"
	"strconv"
	"strings"
	"time"
)

// CountPrivateRepos returns the number of private repositories owned by
// the given account login.
func (st *Store) CountPrivateRepos(login string) int {
	st.Mu.RLock()
	defer st.Mu.RUnlock()
	prefix := login + "/"
	n := 0
	for name, r := range st.ReposByName {
		if strings.HasPrefix(name, prefix) && r.Private {
			n++
		}
	}
	return n
}

// CountSecretGists returns the number of secret (non-public) gists the
// user owns.
func (st *Store) CountSecretGists(userID int) int {
	st.Mu.RLock()
	defer st.Mu.RUnlock()
	n := 0
	for _, g := range st.Gists {
		if g.OwnerID == userID && !g.Public {
			n++
		}
	}
	return n
}

// CountRepoCollaboratorsForOwner returns the number of distinct
// collaborators across the account's repositories.
func (st *Store) CountRepoCollaboratorsForOwner(login string) int {
	st.Mu.RLock()
	defer st.Mu.RUnlock()
	prefix := login + "/"
	distinct := map[string]bool{}
	for repoKey, collabs := range st.RepoCollaborators {
		if !strings.HasPrefix(repoKey, prefix) {
			continue
		}
		for collab := range collabs {
			distinct[collab] = true
		}
	}
	return len(distinct)
}

// DiskUsageKBForOwner sums the on-disk size of the account's
// repositories in kilobytes (memory-backed git storage occupies no disk).
func (st *Store) DiskUsageKBForOwner(login string) int64 {
	st.Mu.RLock()
	prefix := login + "/"
	var names []string
	for name := range st.ReposByName {
		if strings.HasPrefix(name, prefix) {
			names = append(names, name)
		}
	}
	st.Mu.RUnlock()
	var total int64
	for _, name := range names {
		total += st.RepoSize(name)
	}
	return total
}

func (st *Store) persistUserLocked(u *User) {
	if st.Persist != nil {
		st.Persist.MustPut("users", strconv.Itoa(u.ID), u)
	}
}

// ListUserEmails returns the user's email addresses, primary first.
func (st *Store) ListUserEmails(userID int) []UserEmail {
	st.Mu.Lock()
	defer st.Mu.Unlock()
	u := st.Users[userID]
	if u == nil {
		return nil
	}
	materializeEmailsLocked(u)
	out := make([]UserEmail, len(u.Emails))
	copy(out, u.Emails)
	return out
}

// AddUserEmails appends new email addresses to the user's account.
// Returns (nil, false) when any address is already registered, matching
// real GitHub's 422 on duplicates.
func (st *Store) AddUserEmails(userID int, emails []string) ([]UserEmail, bool) {
	st.Mu.Lock()
	defer st.Mu.Unlock()
	u := st.Users[userID]
	if u == nil {
		return nil, false
	}
	materializeEmailsLocked(u)
	for _, addr := range emails {
		for _, existing := range u.Emails {
			if strings.EqualFold(existing.Email, addr) {
				return nil, false
			}
		}
	}
	added := make([]UserEmail, 0, len(emails))
	for _, addr := range emails {
		e := UserEmail{Email: addr, Primary: false, Verified: true}
		u.Emails = append(u.Emails, e)
		added = append(added, e)
	}
	u.UpdatedAt = time.Now().UTC()
	st.persistUserLocked(u)
	return added, true
}

// DeleteUserEmails removes email addresses from the user's account. The
// primary address cannot be removed.
func (st *Store) DeleteUserEmails(userID int, emails []string) deleteEmailsResult {
	st.Mu.Lock()
	defer st.Mu.Unlock()
	u := st.Users[userID]
	if u == nil {
		return deleteEmailsNotFound
	}
	materializeEmailsLocked(u)
	for _, addr := range emails {
		found := false
		for _, existing := range u.Emails {
			if strings.EqualFold(existing.Email, addr) {
				if existing.Primary {
					return DeleteEmailsPrimary
				}
				found = true
				break
			}
		}
		if !found {
			return deleteEmailsNotFound
		}
	}
	kept := u.Emails[:0]
	for _, existing := range u.Emails {
		remove := false
		for _, addr := range emails {
			if strings.EqualFold(existing.Email, addr) {
				remove = true
				break
			}
		}
		if !remove {
			kept = append(kept, existing)
		}
	}
	u.Emails = kept
	u.UpdatedAt = time.Now().UTC()
	st.persistUserLocked(u)
	return DeleteEmailsOK
}

// SetPrimaryEmailVisibility updates the visibility of the primary email
// address and returns the updated entries, or nil when the user has no
// primary email.
func (st *Store) SetPrimaryEmailVisibility(userID int, visibility string) []UserEmail {
	st.Mu.Lock()
	defer st.Mu.Unlock()
	u := st.Users[userID]
	if u == nil {
		return nil
	}
	materializeEmailsLocked(u)
	var updated []UserEmail
	for i := range u.Emails {
		if u.Emails[i].Primary {
			u.Emails[i].Visibility = visibility
			updated = append(updated, u.Emails[i])
		}
	}
	if updated == nil {
		return nil
	}
	u.UpdatedAt = time.Now().UTC()
	st.persistUserLocked(u)
	return updated
}

// setPrimaryEmailResult reports the outcome of SetPrimaryUserEmail. Exported
// values mirror the deleteEmailsResult convention: only the outcomes callers
// branch on are exported.
type setPrimaryEmailResult int

const (
	SetPrimaryEmailOK setPrimaryEmailResult = iota
	setPrimaryEmailNotFound
	SetPrimaryEmailUnknown
	SetPrimaryEmailUnverified
)

// SetPrimaryUserEmail promotes one of the user's existing verified email
// addresses to primary (the github.com Settings → Emails web action; the REST
// API cannot change the primary address). The demoted primary keeps its entry;
// the promoted address inherits the old primary's visibility when it has none
// of its own. Returns the updated entries, primary first.
func (st *Store) SetPrimaryUserEmail(userID int, email string) ([]UserEmail, setPrimaryEmailResult) {
	st.Mu.Lock()
	defer st.Mu.Unlock()
	u := st.Users[userID]
	if u == nil {
		return nil, setPrimaryEmailNotFound
	}
	materializeEmailsLocked(u)
	target := -1
	oldVisibility := ""
	for i := range u.Emails {
		if u.Emails[i].Primary {
			oldVisibility = u.Emails[i].Visibility
		}
		if strings.EqualFold(u.Emails[i].Email, email) {
			target = i
		}
	}
	if target < 0 {
		return nil, SetPrimaryEmailUnknown
	}
	if !u.Emails[target].Verified {
		return nil, SetPrimaryEmailUnverified
	}
	for i := range u.Emails {
		u.Emails[i].Primary = i == target
	}
	if u.Emails[target].Visibility == "" {
		u.Emails[target].Visibility = oldVisibility
	}
	u.Email = u.Emails[target].Email
	u.UpdatedAt = time.Now().UTC()
	st.persistUserLocked(u)

	out := make([]UserEmail, 0, len(u.Emails))
	out = append(out, u.Emails[target])
	for i, e := range u.Emails {
		if i != target {
			out = append(out, e)
		}
	}
	return out, SetPrimaryEmailOK
}

// setPrimaryEmailLocked changes the account's primary email address
// (PATCH /user `email`). Caller must hold st.Mu.
func (st *Store) SetPrimaryEmailLocked(u *User, email string) {
	u.Email = email
	materializeEmailsLocked(u)
	for i := range u.Emails {
		if u.Emails[i].Primary {
			u.Emails[i].Email = email
			return
		}
	}
	u.Emails = append(u.Emails, UserEmail{Email: email, Primary: true, Verified: true, Visibility: "private"})
}

// UpdateUserProfile applies fn to the user under the store lock, bumps
// UpdatedAt, and persists. Returns nil when the user does not exist.
func (st *Store) UpdateUserProfile(userID int, fn func(*User)) *User {
	st.Mu.Lock()
	defer st.Mu.Unlock()
	u := st.Users[userID]
	if u == nil {
		return nil
	}
	fn(u)
	u.UpdatedAt = time.Now().UTC()
	st.persistUserLocked(u)
	return u
}

// IsInteractionGroup reports whether name is one of the three groups GitHub
// accepts as an interaction limit. The REST routes and the three GraphQL
// set-limit mutations both ask it, so neither can accept a limit the other
// refuses.
func IsInteractionGroup(limit string) bool {
	switch limit {
	case "existing_users", "contributors_only", "collaborators_only":
		return true
	}
	return false
}

// InteractionLimitExpiry translates GitHub's expiry vocabulary into the
// instant a limit set at from lapses. An empty expiry is GitHub's default of
// one day.
func InteractionLimitExpiry(expiry string, from time.Time) (time.Time, bool) {
	switch expiry {
	case "", "one_day":
		return from.Add(24 * time.Hour), true
	case "three_days":
		return from.Add(3 * 24 * time.Hour), true
	case "one_week":
		return from.Add(7 * 24 * time.Hour), true
	case "one_month":
		return from.AddDate(0, 1, 0), true
	case "six_months":
		return from.AddDate(0, 6, 0), true
	}
	return time.Time{}, false
}

// SetUserInteractionLimit records (or clears, with limit == "") the
// account-level interaction limit.
func (st *Store) SetUserInteractionLimit(userID int, limit string, expiresAt *time.Time) bool {
	st.Mu.Lock()
	defer st.Mu.Unlock()
	u := st.Users[userID]
	if u == nil {
		return false
	}
	u.InteractionLimit = limit
	u.InteractionLimitExpiry = expiresAt
	st.persistUserLocked(u)
	return true
}

// GetUserInteractionLimit returns the active limit and its expiry, or
// ("", zero) when no unexpired limit is set.
func (st *Store) GetUserInteractionLimit(userID int) (string, time.Time) {
	st.Mu.RLock()
	defer st.Mu.RUnlock()
	u := st.Users[userID]
	if u == nil || u.InteractionLimit == "" || u.InteractionLimitExpiry == nil {
		return "", time.Time{}
	}
	if st.CurrentTime().After(*u.InteractionLimitExpiry) {
		return "", time.Time{}
	}
	return u.InteractionLimit, *u.InteractionLimitExpiry
}

// ListUserFilteredIssues returns issues visible through GET /user/issues
// for the given filter (assigned, created, mentioned, subscribed, repos,
// all). Repository read access is checked by the caller.
func (st *Store) ListUserFilteredIssues(user *User, filter string) []IssueWithRepo {
	st.Mu.RLock()
	defer st.Mu.RUnlock()

	subscribed := func(repoID int) bool {
		sub := st.RepoSubscriptions[RepoSubscriptionKey(user.ID, repoID)]
		return sub != nil && sub.Subscribed
	}
	matches := func(issue *Issue, repo *Repo) bool {
		assigned := false
		for _, aid := range issue.AssigneeIDs {
			if aid == user.ID {
				assigned = true
				break
			}
		}
		created := issue.AuthorID == user.ID
		mentioned := strings.Contains(issue.Body, "@"+user.Login)
		switch filter {
		case "created":
			return created
		case "mentioned":
			return mentioned
		case "subscribed":
			return subscribed(repo.ID)
		case "repos":
			return repo.OwnerID == user.ID
		case "all":
			return assigned || created || mentioned || subscribed(repo.ID)
		default: // "assigned"
			return assigned
		}
	}

	var out []IssueWithRepo
	for _, issue := range st.Issues {
		repo := st.Repos[issue.RepoID]
		if repo == nil {
			continue
		}
		if matches(issue, repo) {
			out = append(out, IssueWithRepo{Issue: issue, Repo: repo})
		}
	}
	return out
}

// CountIssueComments returns the number of conversation comments on an issue.
func (st *Store) CountIssueComments(issueID int) int {
	st.Mu.RLock()
	defer st.Mu.RUnlock()
	n := 0
	for _, c := range st.Comments {
		if c.ParentType == "issue" && c.IssueID == issueID {
			n++
		}
	}
	return n
}

// ActionsBillingUsageForOwner derives GitHub Actions usage line items
// from completed workflow-run jobs in repositories owned by the account.
// Quantities are per-job minutes rounded up, matching GitHub's metering.
func (st *Store) ActionsBillingUsageForOwner(ownerLogin string) []BillingUsageItem {
	st.Mu.RLock()
	defer st.Mu.RUnlock()
	prefix := ownerLogin + "/"
	var out []BillingUsageItem
	for _, wf := range st.Workflows {
		if !strings.HasPrefix(wf.RepoFullName, prefix) {
			continue
		}
		for _, job := range wf.Jobs {
			if job.StartedAt.IsZero() || job.CompletedAt.IsZero() || job.CompletedAt.Before(job.StartedAt) {
				continue
			}
			minutes := int(math.Ceil(job.CompletedAt.Sub(job.StartedAt).Minutes()))
			if minutes < 1 {
				minutes = 1
			}
			out = append(out, BillingUsageItem{
				Date:         job.StartedAt.UTC(),
				Product:      "Actions",
				SKU:          "Actions Linux",
				RepoFullName: wf.RepoFullName,
				Quantity:     minutes,
				UnitType:     "minutes",
				PricePerUnit: actionsLinuxPricePerMinute,
			})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Date.Before(out[j].Date) })
	return out
}

// actionsLinuxPricePerMinute is GitHub's published Linux-runner
// per-minute price used on billing usage reports.
const actionsLinuxPricePerMinute = 0.008

// BillingUsageItem is one metered usage line derived from real run state.
type BillingUsageItem struct {
	Date         time.Time
	Product      string
	SKU          string
	RepoFullName string
	Quantity     int
	UnitType     string
	PricePerUnit float64
}

type deleteEmailsResult int

type IssueWithRepo struct {
	Issue *Issue `json:"-"`
	Repo  *Repo  `json:"-"`
}

// materializeEmailsLocked seeds the multi-email list from the legacy
// single Email field the first time email state is touched. Caller must
// hold st.Mu.
func materializeEmailsLocked(u *User) {
	if len(u.Emails) == 0 && u.Email != "" {
		u.Emails = []UserEmail{{Email: u.Email, Primary: true, Verified: true, Visibility: "private"}}
	}
}

const (
	DeleteEmailsOK deleteEmailsResult = iota
	deleteEmailsNotFound
	DeleteEmailsPrimary
)
