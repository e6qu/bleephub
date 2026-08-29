package store

// The GitHub Copilot organization surface: seat billing, content exclusion
// settings, and coding-agent permissions.

import (
	"fmt"
	"slices"
	"sort"
	"time"
)

// CopilotSeat is one Copilot Business seat, assigned directly (AssigningTeamSlug
// empty) or through a team. Cancellation defers to the end of the billing cycle:
// PendingCancellationDate is the YYYY-MM-DD expiry, and expired seats are
// dropped lazily on access.
type CopilotSeat struct {
	OrgLogin                string    `json:"org_login"`
	UserID                  int       `json:"user_id"`
	AssigningTeamSlug       string    `json:"assigning_team_slug"`
	PendingCancellationDate string    `json:"pending_cancellation_date"`
	CreatedAt               time.Time `json:"created_at"`
	UpdatedAt               time.Time `json:"updated_at"`
}

func copilotSeatKey(orgLogin string, userID int) string {
	return fmt.Sprintf("%s/%d", orgLogin, userID)
}

// CopilotNextCycleDate returns the first day of the next calendar month, when a
// cancelled seat lapses (GitHub bills Copilot monthly).
func CopilotNextCycleDate(now time.Time) string {
	first := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC).AddDate(0, 1, 0)
	return first.Format("2006-01-02")
}

// copilotSeatExpired reports whether a seat's pending cancellation date has
// passed. Reads filter these out; write paths prune them via
// expireCopilotSeatsBatchLocked.
func copilotSeatExpired(seat *CopilotSeat, now time.Time) bool {
	return seat.PendingCancellationDate != "" && seat.PendingCancellationDate <= now.UTC().Format("2006-01-02")
}

// expireCopilotSeatsBatchLocked drops expired seats, staging the deletes into
// batch so they commit with the seat mutation in one transaction (STORE-001/002).
// Callers hold the write lock. Write paths only, never a read: a GET must not
// perform a durable delete (STORE-034).
func (st *Store) expireCopilotSeatsBatchLocked(batch *PersistBatch, orgLogin string, now time.Time) {
	for userID, seat := range st.CopilotSeats[orgLogin] {
		if copilotSeatExpired(seat, now) {
			delete(st.CopilotSeats[orgLogin], userID)
			batch.Delete("copilot_seats", copilotSeatKey(orgLogin, userID))
		}
	}
}

// persistCopilotSeatBatchLocked stages a seat row into batch (STORE-001/002).
// Callers hold st.Mu.
func (st *Store) persistCopilotSeatBatchLocked(batch *PersistBatch, seat *CopilotSeat) {
	batch.Put("copilot_seats", copilotSeatKey(seat.OrgLogin, seat.UserID), seat)
}

// GetCopilotSeat returns the org's seat for the user, or nil. An expired seat
// reads as absent; its durable removal happens on the next seat write.
func (st *Store) GetCopilotSeat(orgLogin string, userID int) *CopilotSeat {
	st.Mu.RLock()
	defer st.Mu.RUnlock()
	seat := st.CopilotSeats[orgLogin][userID]
	if seat == nil || copilotSeatExpired(seat, st.CurrentTime()) {
		return nil
	}
	return seat
}

// ListCopilotSeats returns the org's seats by creation time (user ID tie-break)
// so pagination is stable.
func (st *Store) ListCopilotSeats(orgLogin string) []*CopilotSeat {
	st.Mu.RLock()
	defer st.Mu.RUnlock()
	now := st.CurrentTime()
	out := make([]*CopilotSeat, 0, len(st.CopilotSeats[orgLogin]))
	for _, seat := range st.CopilotSeats[orgLogin] {
		if copilotSeatExpired(seat, now) {
			continue
		}
		out = append(out, seat)
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].CreatedAt.Before(out[j].CreatedAt)
		}
		return out[i].UserID < out[j].UserID
	})
	return snapshotSlice(out)
}

// AddCopilotSeats grants seats to the users, assigned through teamSlug when
// non-empty. Active seats are skipped; pending-cancellation seats are
// reinstated. Returns the count created or reinstated (what GitHub bills). The
// expiry prune and every seat write commit in one transaction (STORE-001/002).
func (st *Store) AddCopilotSeats(orgLogin string, userIDs []int, teamSlug string) int {
	st.Mu.Lock()
	defer st.Mu.Unlock()
	now := st.CurrentTime()
	batch := NewPersistBatch(st.Persist)
	st.expireCopilotSeatsBatchLocked(batch, orgLogin, now)
	if st.CopilotSeats[orgLogin] == nil {
		st.CopilotSeats[orgLogin] = map[int]*CopilotSeat{}
	}
	created := 0
	for _, id := range userIDs {
		if seat, ok := st.CopilotSeats[orgLogin][id]; ok {
			if seat.PendingCancellationDate != "" {
				seat.PendingCancellationDate = ""
				seat.AssigningTeamSlug = teamSlug
				seat.UpdatedAt = now
				st.persistCopilotSeatBatchLocked(batch, seat)
				created++
			}
			continue
		}
		seat := &CopilotSeat{
			OrgLogin:          orgLogin,
			UserID:            id,
			AssigningTeamSlug: teamSlug,
			CreatedAt:         now,
			UpdatedAt:         now,
		}
		st.CopilotSeats[orgLogin][id] = seat
		st.persistCopilotSeatBatchLocked(batch, seat)
		created++
	}
	if err := batch.Commit(); err != nil {
		panic(&PersistenceFailure{Op: "batch", Bucket: "copilot_seats", Err: err})
	}
	return created
}

// CancelCopilotSeatsForUsers marks the users' directly-assigned seats pending
// cancellation. If any user holds a team-assigned seat, nothing is cancelled
// and those user IDs are returned — GitHub rejects the whole request with 422.
func (st *Store) CancelCopilotSeatsForUsers(orgLogin string, userIDs []int) (cancelled int, teamAssigned []int) {
	st.Mu.Lock()
	defer st.Mu.Unlock()
	now := st.CurrentTime()
	// The prune still commits on the team-assigned early return — its deletes are
	// correct regardless of the 422 (STORE-001/002).
	batch := NewPersistBatch(st.Persist)
	st.expireCopilotSeatsBatchLocked(batch, orgLogin, now)
	for _, id := range userIDs {
		if seat, ok := st.CopilotSeats[orgLogin][id]; ok && seat.AssigningTeamSlug != "" {
			teamAssigned = append(teamAssigned, id)
		}
	}
	if len(teamAssigned) > 0 {
		if err := batch.Commit(); err != nil {
			panic(&PersistenceFailure{Op: "batch", Bucket: "copilot_seats", Err: err})
		}
		return 0, teamAssigned
	}
	date := CopilotNextCycleDate(now)
	for _, id := range userIDs {
		seat, ok := st.CopilotSeats[orgLogin][id]
		if !ok || seat.PendingCancellationDate != "" {
			continue
		}
		seat.PendingCancellationDate = date
		seat.UpdatedAt = now
		st.persistCopilotSeatBatchLocked(batch, seat)
		cancelled++
	}
	if err := batch.Commit(); err != nil {
		panic(&PersistenceFailure{Op: "batch", Bucket: "copilot_seats", Err: err})
	}
	return cancelled, nil
}

// CancelCopilotSeatsForTeam marks every team-assigned seat pending cancellation
// and returns the count affected. Prune and marks commit in one transaction
// (STORE-001/002).
func (st *Store) CancelCopilotSeatsForTeam(orgLogin, teamSlug string) int {
	st.Mu.Lock()
	defer st.Mu.Unlock()
	now := st.CurrentTime()
	batch := NewPersistBatch(st.Persist)
	st.expireCopilotSeatsBatchLocked(batch, orgLogin, now)
	date := CopilotNextCycleDate(now)
	cancelled := 0
	for _, seat := range st.CopilotSeats[orgLogin] {
		if seat.AssigningTeamSlug != teamSlug || seat.PendingCancellationDate != "" {
			continue
		}
		seat.PendingCancellationDate = date
		seat.UpdatedAt = now
		st.persistCopilotSeatBatchLocked(batch, seat)
		cancelled++
	}
	if err := batch.Commit(); err != nil {
		panic(&PersistenceFailure{Op: "batch", Bucket: "copilot_seats", Err: err})
	}
	return cancelled
}

// CopilotContentExclusion holds an org's content exclusion rules: scope
// (repository "owner/name" or "*") → rules, each a path string or an
// ifAnyMatch/ifNoneMatch object, stored as configured.
type CopilotContentExclusion struct {
	OrgLogin string                   `json:"org_login"`
	Rules    map[string][]interface{} `json:"rules"`
}

// GetCopilotContentExclusion returns the org's content exclusion rules, empty
// when unconfigured.
func (st *Store) GetCopilotContentExclusion(orgLogin string) map[string][]interface{} {
	st.Mu.RLock()
	defer st.Mu.RUnlock()
	ce := st.CopilotContentExclusions[orgLogin]
	if ce == nil {
		return map[string][]interface{}{}
	}
	out := make(map[string][]interface{}, len(ce.Rules))
	for k, v := range ce.Rules {
		out[k] = slices.Clone(v)
	}
	return out
}

// SetCopilotContentExclusion replaces the org's content exclusion rules.
func (st *Store) SetCopilotContentExclusion(orgLogin string, rules map[string][]interface{}) {
	st.Mu.Lock()
	defer st.Mu.Unlock()
	// Clone: the caller owns rules and may mutate it after this returns.
	cloned := make(map[string][]interface{}, len(rules))
	for k, v := range rules {
		cloned[k] = slices.Clone(v)
	}
	ce := &CopilotContentExclusion{OrgLogin: orgLogin, Rules: cloned}
	st.CopilotContentExclusions[orgLogin] = ce
	if st.Persist != nil {
		st.Persist.MustPut("copilot_content_exclusions", orgLogin, ce)
	}
}

// CopilotCodingAgentPermissions is the org policy for which repositories may
// use Copilot cloud agent.
type CopilotCodingAgentPermissions struct {
	OrgLogin              string `json:"org_login"`
	EnabledRepositories   string `json:"enabled_repositories"` // all | selected | none
	SelectedRepositoryIDs []int  `json:"selected_repository_ids"`
}

func (st *Store) getCopilotCodingAgentPermsLocked(orgLogin string) *CopilotCodingAgentPermissions {
	if p, ok := st.CopilotCodingAgentPerms[orgLogin]; ok && p != nil {
		return p
	}
	// Default posture: enabled for all repositories until an owner restricts it.
	p := &CopilotCodingAgentPermissions{
		OrgLogin:              orgLogin,
		EnabledRepositories:   "all",
		SelectedRepositoryIDs: []int{},
	}
	st.CopilotCodingAgentPerms[orgLogin] = p
	return p
}

func (st *Store) persistCopilotCodingAgentPermsLocked(p *CopilotCodingAgentPermissions) {
	if st.Persist != nil {
		st.Persist.MustPut("copilot_coding_agent_permissions", p.OrgLogin, p)
	}
}

// GetCopilotCodingAgentPermissions returns the org's coding-agent policy.
func (st *Store) GetCopilotCodingAgentPermissions(orgLogin string) *CopilotCodingAgentPermissions {
	st.Mu.RLock()
	defer st.Mu.RUnlock()
	return st.copilotCodingAgentPermsReadLocked(orgLogin)
}

// copilotCodingAgentPermsReadLocked returns the org's stored policy or the
// default computed on the fly. Unlike getCopilotCodingAgentPermsLocked it never
// materializes the default into the map, so a pure read writes no phantom entry.
// Caller holds st.Mu.
func (st *Store) copilotCodingAgentPermsReadLocked(orgLogin string) *CopilotCodingAgentPermissions {
	if p, ok := st.CopilotCodingAgentPerms[orgLogin]; ok && p != nil {
		return p
	}
	return &CopilotCodingAgentPermissions{
		OrgLogin:              orgLogin,
		EnabledRepositories:   "all",
		SelectedRepositoryIDs: []int{},
	}
}

// SetCopilotCodingAgentPolicy sets the enabled_repositories policy.
func (st *Store) SetCopilotCodingAgentPolicy(orgLogin, policy string) {
	st.Mu.Lock()
	defer st.Mu.Unlock()
	p := st.getCopilotCodingAgentPermsLocked(orgLogin)
	p.EnabledRepositories = policy
	st.persistCopilotCodingAgentPermsLocked(p)
}

// SetCopilotCodingAgentSelectedRepos replaces the selected repository list.
func (st *Store) SetCopilotCodingAgentSelectedRepos(orgLogin string, repoIDs []int) {
	st.Mu.Lock()
	defer st.Mu.Unlock()
	p := st.getCopilotCodingAgentPermsLocked(orgLogin)
	// Clone: the caller owns repoIDs.
	p.SelectedRepositoryIDs = append([]int(nil), repoIDs...)
	st.persistCopilotCodingAgentPermsLocked(p)
}

// AddCopilotCodingAgentSelectedRepo adds a repository to the selected list
// (no-op when already present).
func (st *Store) AddCopilotCodingAgentSelectedRepo(orgLogin string, repoID int) {
	st.Mu.Lock()
	defer st.Mu.Unlock()
	p := st.getCopilotCodingAgentPermsLocked(orgLogin)
	if slices.Contains(p.SelectedRepositoryIDs, repoID) {
		return
	}
	p.SelectedRepositoryIDs = append(p.SelectedRepositoryIDs, repoID)
	st.persistCopilotCodingAgentPermsLocked(p)
}

// RemoveCopilotCodingAgentSelectedRepo drops a repository from the selected list.
func (st *Store) RemoveCopilotCodingAgentSelectedRepo(orgLogin string, repoID int) {
	st.Mu.Lock()
	defer st.Mu.Unlock()
	p := st.getCopilotCodingAgentPermsLocked(orgLogin)
	kept := p.SelectedRepositoryIDs[:0]
	for _, id := range p.SelectedRepositoryIDs {
		if id != repoID {
			kept = append(kept, id)
		}
	}
	p.SelectedRepositoryIDs = kept
	st.persistCopilotCodingAgentPermsLocked(p)
}
