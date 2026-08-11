package bleephub

// Store types and methods for the GitHub Copilot organization surface:
// seat billing (selected users / selected teams), content exclusion
// settings, and Copilot coding agent permissions.

import (
	"fmt"
	"slices"
	"sort"
	"time"
)

// CopilotSeat is one GitHub Copilot Business seat assignment in an
// organization. A seat is assigned either directly (AssigningTeamSlug
// empty) or through a team. Cancellation is deferred to the end of the
// billing cycle: PendingCancellationDate holds the YYYY-MM-DD on which
// the seat expires, and expired seats are dropped lazily on access.
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

// copilotNextCycleDate returns the first day of the next calendar month.
// GitHub bills Copilot monthly, so a cancelled seat stays active until
// the start of the organization's next billing cycle.
func copilotNextCycleDate(now time.Time) string {
	first := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC).AddDate(0, 1, 0)
	return first.Format("2006-01-02")
}

// copilotSeatExpired reports whether a seat's pending cancellation date has
// been reached. Reads filter these out in memory; the write paths physically
// prune them via expireCopilotSeatsBatchLocked.
func copilotSeatExpired(seat *CopilotSeat, now time.Time) bool {
	return seat.PendingCancellationDate != "" && seat.PendingCancellationDate <= now.UTC().Format("2006-01-02")
}

// expireCopilotSeatsBatchLocked drops seats whose pending cancellation date
// has been reached, staging the deletes into batch so they commit with the
// rest of the seat mutation in one transaction (STORE-001/002). Callers hold
// the write lock. It is invoked from the seat write paths (add/cancel), never
// from a read: a GET must not perform a durable delete (STORE-034).
func (st *Store) expireCopilotSeatsBatchLocked(batch *persistBatch, orgLogin string, now time.Time) {
	for userID, seat := range st.CopilotSeats[orgLogin] {
		if copilotSeatExpired(seat, now) {
			delete(st.CopilotSeats[orgLogin], userID)
			batch.Delete("copilot_seats", copilotSeatKey(orgLogin, userID))
		}
	}
}

// persistCopilotSeatBatchLocked stages a seat row into batch instead of
// committing its own transaction (STORE-001/002). Callers hold st.mu.
func (st *Store) persistCopilotSeatBatchLocked(batch *persistBatch, seat *CopilotSeat) {
	batch.Put("copilot_seats", copilotSeatKey(seat.OrgLogin, seat.UserID), seat)
}

// GetCopilotSeat returns the organization's seat for the user, or nil. An
// expired (pending-cancellation-reached) seat reads as absent; the durable
// removal happens on the next seat write, not on this read.
func (st *Store) GetCopilotSeat(orgLogin string, userID int) *CopilotSeat {
	st.mu.RLock()
	defer st.mu.RUnlock()
	seat := st.CopilotSeats[orgLogin][userID]
	if seat == nil || copilotSeatExpired(seat, st.currentTime()) {
		return nil
	}
	return seat
}

// ListCopilotSeats returns the organization's seats sorted by creation
// time (user ID as tie-break) so pagination is stable.
func (st *Store) ListCopilotSeats(orgLogin string) []*CopilotSeat {
	st.mu.RLock()
	defer st.mu.RUnlock()
	now := st.currentTime()
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

// AddCopilotSeats grants seats to the given users, assigned through
// teamSlug when non-empty. Users who already hold an active seat are
// skipped; seats pending cancellation are reinstated. Returns the
// number of seats created or reinstated — the count GitHub bills for.
// The expiry prune and every seat write commit in one transaction
// (STORE-001/002).
func (st *Store) AddCopilotSeats(orgLogin string, userIDs []int, teamSlug string) int {
	st.mu.Lock()
	defer st.mu.Unlock()
	now := st.currentTime()
	batch := newPersistBatch(st.persist)
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
		panic(&persistenceFailure{op: "batch", bucket: "copilot_seats", err: err})
	}
	return created
}

// CancelCopilotSeatsForUsers marks the users' directly-assigned seats as
// pending cancellation at the end of the billing cycle. When any of the
// users holds a seat assigned through a team, no seat is cancelled and
// the blocked user IDs are returned — GitHub rejects the whole request
// with a 422 in that case.
func (st *Store) CancelCopilotSeatsForUsers(orgLogin string, userIDs []int) (cancelled int, teamAssigned []int) {
	st.mu.Lock()
	defer st.mu.Unlock()
	now := st.currentTime()
	// One transaction: the expiry prune and every cancellation mark commit
	// together (STORE-001/002). The prune still commits on the team-assigned
	// early return — its deletes are correct regardless of the 422.
	batch := newPersistBatch(st.persist)
	st.expireCopilotSeatsBatchLocked(batch, orgLogin, now)
	for _, id := range userIDs {
		if seat, ok := st.CopilotSeats[orgLogin][id]; ok && seat.AssigningTeamSlug != "" {
			teamAssigned = append(teamAssigned, id)
		}
	}
	if len(teamAssigned) > 0 {
		if err := batch.Commit(); err != nil {
			panic(&persistenceFailure{op: "batch", bucket: "copilot_seats", err: err})
		}
		return 0, teamAssigned
	}
	date := copilotNextCycleDate(now)
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
		panic(&persistenceFailure{op: "batch", bucket: "copilot_seats", err: err})
	}
	return cancelled, nil
}

// CancelCopilotSeatsForTeam marks every seat assigned through the team
// as pending cancellation and returns the number of seats affected. The
// expiry prune and every cancellation mark commit in one transaction
// (STORE-001/002).
func (st *Store) CancelCopilotSeatsForTeam(orgLogin, teamSlug string) int {
	st.mu.Lock()
	defer st.mu.Unlock()
	now := st.currentTime()
	batch := newPersistBatch(st.persist)
	st.expireCopilotSeatsBatchLocked(batch, orgLogin, now)
	date := copilotNextCycleDate(now)
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
		panic(&persistenceFailure{op: "batch", bucket: "copilot_seats", err: err})
	}
	return cancelled
}

// CopilotContentExclusion holds an organization's Copilot content
// exclusion rules: scope (repository "owner/name" or "*") → list of
// rules, each a path string or an ifAnyMatch / ifNoneMatch object,
// stored exactly as configured.
type CopilotContentExclusion struct {
	OrgLogin string                   `json:"org_login"`
	Rules    map[string][]interface{} `json:"rules"`
}

// GetCopilotContentExclusion returns the organization's content
// exclusion rules; an unconfigured organization has none.
func (st *Store) GetCopilotContentExclusion(orgLogin string) map[string][]interface{} {
	st.mu.RLock()
	defer st.mu.RUnlock()
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

// SetCopilotContentExclusion replaces the organization's content
// exclusion rules.
func (st *Store) SetCopilotContentExclusion(orgLogin string, rules map[string][]interface{}) {
	st.mu.Lock()
	defer st.mu.Unlock()
	// Clone rather than adopt the caller's map (and its slices) by reference:
	// the request handler owns rules and may mutate it after this returns.
	cloned := make(map[string][]interface{}, len(rules))
	for k, v := range rules {
		cloned[k] = slices.Clone(v)
	}
	ce := &CopilotContentExclusion{OrgLogin: orgLogin, Rules: cloned}
	st.CopilotContentExclusions[orgLogin] = ce
	if st.persist != nil {
		st.persist.MustPut("copilot_content_exclusions", orgLogin, ce)
	}
}

// CopilotCodingAgentPermissions models the organization policy for which
// repositories may use Copilot cloud agent.
type CopilotCodingAgentPermissions struct {
	OrgLogin              string `json:"org_login"`
	EnabledRepositories   string `json:"enabled_repositories"` // all | selected | none
	SelectedRepositoryIDs []int  `json:"selected_repository_ids"`
}

func (st *Store) getCopilotCodingAgentPermsLocked(orgLogin string) *CopilotCodingAgentPermissions {
	if p, ok := st.CopilotCodingAgentPerms[orgLogin]; ok && p != nil {
		return p
	}
	// GitHub's default posture enables Copilot coding agent for all
	// repositories until an owner restricts it.
	p := &CopilotCodingAgentPermissions{
		OrgLogin:              orgLogin,
		EnabledRepositories:   "all",
		SelectedRepositoryIDs: []int{},
	}
	st.CopilotCodingAgentPerms[orgLogin] = p
	return p
}

func (st *Store) persistCopilotCodingAgentPermsLocked(p *CopilotCodingAgentPermissions) {
	if st.persist != nil {
		st.persist.MustPut("copilot_coding_agent_permissions", p.OrgLogin, p)
	}
}

// GetCopilotCodingAgentPermissions returns the organization's Copilot
// coding agent policy, materializing the default on first read.
func (st *Store) GetCopilotCodingAgentPermissions(orgLogin string) *CopilotCodingAgentPermissions {
	st.mu.RLock()
	defer st.mu.RUnlock()
	return st.copilotCodingAgentPermsReadLocked(orgLogin)
}

// copilotCodingAgentPermsReadLocked returns the org's stored policy, or the
// default posture computed on the fly. Unlike getCopilotCodingAgentPermsLocked
// it never materializes the default into the map, so a pure read neither takes
// the write lock nor writes a never-persisted phantom entry. Caller holds st.mu
// (read or write).
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
	st.mu.Lock()
	defer st.mu.Unlock()
	p := st.getCopilotCodingAgentPermsLocked(orgLogin)
	p.EnabledRepositories = policy
	st.persistCopilotCodingAgentPermsLocked(p)
}

// SetCopilotCodingAgentSelectedRepos replaces the selected repository list.
func (st *Store) SetCopilotCodingAgentSelectedRepos(orgLogin string, repoIDs []int) {
	st.mu.Lock()
	defer st.mu.Unlock()
	p := st.getCopilotCodingAgentPermsLocked(orgLogin)
	// Clone rather than adopt the caller's slice by reference.
	p.SelectedRepositoryIDs = append([]int(nil), repoIDs...)
	st.persistCopilotCodingAgentPermsLocked(p)
}

// AddCopilotCodingAgentSelectedRepo adds a repository to the selected
// list (no-op when already present).
func (st *Store) AddCopilotCodingAgentSelectedRepo(orgLogin string, repoID int) {
	st.mu.Lock()
	defer st.mu.Unlock()
	p := st.getCopilotCodingAgentPermsLocked(orgLogin)
	if slices.Contains(p.SelectedRepositoryIDs, repoID) {
		return
	}
	p.SelectedRepositoryIDs = append(p.SelectedRepositoryIDs, repoID)
	st.persistCopilotCodingAgentPermsLocked(p)
}

// RemoveCopilotCodingAgentSelectedRepo drops a repository from the
// selected list.
func (st *Store) RemoveCopilotCodingAgentSelectedRepo(orgLogin string, repoID int) {
	st.mu.Lock()
	defer st.mu.Unlock()
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
