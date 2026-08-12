package store

import (
	"sort"
	"strconv"
	"time"
)

// APIRequestRecord is one observed, attributed /api/v3 request.
type APIRequestRecord struct {
	ID          int64     `json:"id"`
	Timestamp   time.Time `json:"timestamp"`
	Method      string    `json:"method"`
	Route       string    `json:"route"` // route template relative to /api/v3, e.g. "/repos/{owner}/{repo}"
	StatusCode  int       `json:"status_code"`
	RateLimited bool      `json:"rate_limited"`
	// Actor identifies the credential that made the request, using the
	// actor taxonomy of GitHub's API insights.
	ActorType APIInsightsActorType `json:"actor_type"` // installation | classic_pat | fine_grained_pat | oauth_app | github_app_user_to_server
	ActorID   int64                `json:"actor_id"`
	ActorName string               `json:"actor_name"`
	// Subject is the account on whose behalf the request ran.
	SubjectType APIInsightsSubjectType `json:"subject_type"` // "user" | "installation"
	SubjectID   int64                  `json:"subject_id"`
	SubjectName string                 `json:"subject_name"`
	// UserID is the authenticated user's ID (0 for installation tokens).
	UserID int `json:"user_id,omitempty"`
	// IntegrationID / OAuthAppID carry the GitHub App / OAuth app identity
	// behind app-derived actors, when one exists.
	IntegrationID *int64 `json:"integration_id,omitempty"`
	OAuthAppID    *int64 `json:"oauth_application_id,omitempty"`
	// OrgLogins are the organizations this request was attributed to at
	// request time (the actor's active memberships, or the installation's
	// target organization).
	OrgLogins []string `json:"org_logins,omitempty"`
}

// maxAPIRequestRecords caps the durable in-memory request log; once the cap
// is reached the oldest records are evicted FIFO so unbounded traffic cannot
// grow the store without limit.
const maxAPIRequestRecords = 10000

// ActiveOrgLoginsForUser returns the logins of every organization where the
// user holds an active membership.
func (st *Store) ActiveOrgLoginsForUser(userID int) []string {
	st.Mu.RLock()
	defer st.Mu.RUnlock()
	var out []string
	for _, m := range st.Memberships {
		if m.UserID != userID || m.State != MembershipStateActive {
			continue
		}
		if org := st.Orgs[m.OrgID]; org != nil {
			out = append(out, org.Login)
		}
	}
	sort.Strings(out)
	return out
}

// PATIdentityByTokenValue resolves a fine-grained personal access token
// value to its token ID + name via the PAT grant/request tables.
func (st *Store) PATIdentityByTokenValue(value string) (int, string, bool) {
	st.Mu.RLock()
	defer st.Mu.RUnlock()
	if token, _ := st.tokenByValueLocked(value); token != nil && token.FineGrained {
		return token.FineGrainedID, token.Name, true
	}
	return 0, "", false
}

// RecordAPIRequest appends an attributed request record and persists it.
// The log is capped at maxAPIRequestRecords with FIFO eviction.
func (st *Store) RecordAPIRequest(rec *APIRequestRecord) {
	st.apiInsightsMu.Lock()
	defer st.apiInsightsMu.Unlock()
	rec.ID = st.NextAPIRequestID
	st.NextAPIRequestID++
	recordCap := st.ApiRequestRecordCap
	if recordCap <= 0 {
		recordCap = maxAPIRequestRecords
	}
	st.APIRequestRecords = append(st.APIRequestRecords, rec)
	// Commit the new record together with any FIFO evictions in one transaction
	// so a crash can't apply the insert without the eviction (or vice versa),
	// leaving the durable bucket over its cap or missing the just-served request
	// (STORE-001/002; the eviction itself is STORE-024). The defer releases the
	// lock if the commit panics.
	batch := NewPersistBatch(st.Persist)
	if overflow := len(st.APIRequestRecords) - recordCap; overflow > 0 {
		for _, evicted := range st.APIRequestRecords[:overflow] {
			batch.Delete("api_insights_requests", strconv.FormatInt(evicted.ID, 10))
		}
		st.APIRequestRecords = append([]*APIRequestRecord(nil), st.APIRequestRecords[overflow:]...)
	}
	batch.Put("api_insights_requests", strconv.FormatInt(rec.ID, 10), rec)
	if err := batch.Commit(); err != nil {
		panic(&PersistenceFailure{Op: "batch", Bucket: "api_insights_requests", Key: strconv.FormatInt(rec.ID, 10), Err: err})
	}
}

// apiInsightsRecords returns the org's attributed records inside [minT, maxT],
// oldest first.
func (st *Store) ApiInsightsRecords(orgLogin string, minT, maxT time.Time) []*APIRequestRecord {
	st.apiInsightsMu.RLock()
	defer st.apiInsightsMu.RUnlock()
	var out []*APIRequestRecord
	for _, rec := range st.APIRequestRecords {
		if rec.Timestamp.Before(minT) || rec.Timestamp.After(maxT) {
			continue
		}
		for _, login := range rec.OrgLogins {
			if login == orgLogin {
				out = append(out, rec)
				break
			}
		}
	}
	return out
}

// APIInsightsActorType is the credential taxonomy of an observed request's
// actor; APIInsightsSubjectType is the account it ran on behalf of. Both are
// produced internally from credential analysis (never unmarshaled from a
// request), and a typed string marshals to JSON identically to a plain string.
type APIInsightsActorType string

type APIInsightsSubjectType string
