package store

import (
	"sort"
	"strconv"
	"time"
)

// APIRequestRecord is one observed, attributed /api/v3 request.
type APIRequestRecord struct {
	ID          int64                  `json:"id"`
	Timestamp   time.Time              `json:"timestamp"`
	Method      string                 `json:"method"`
	Route       string                 `json:"route"` // route template relative to /api/v3, e.g. "/repos/{owner}/{repo}"
	StatusCode  int                    `json:"status_code"`
	RateLimited bool                   `json:"rate_limited"`
	ActorType   APIInsightsActorType   `json:"actor_type"` // installation | classic_pat | fine_grained_pat | oauth_app | github_app_user_to_server
	ActorID     int64                  `json:"actor_id"`
	ActorName   string                 `json:"actor_name"`
	SubjectType APIInsightsSubjectType `json:"subject_type"` // "user" | "installation"
	SubjectID   int64                  `json:"subject_id"`
	SubjectName string                 `json:"subject_name"`
	UserID      int                    `json:"user_id,omitempty"` // 0 for installation tokens
	// GitHub App / OAuth app identity behind an app-derived actor.
	IntegrationID *int64 `json:"integration_id,omitempty"`
	OAuthAppID    *int64 `json:"oauth_application_id,omitempty"`
	// Orgs attributed at request time: the actor's active memberships, or the
	// installation's target org.
	OrgLogins []string `json:"org_logins,omitempty"`
}

// maxAPIRequestRecords caps the request log; oldest evicted FIFO.
const maxAPIRequestRecords = 10000

// ActiveOrgLoginsForUser returns the logins of every org where the user holds
// an active membership.
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

// PATIdentityByTokenValue resolves a fine-grained PAT value to its token ID and name.
func (st *Store) PATIdentityByTokenValue(value string) (int, string, bool) {
	st.Mu.RLock()
	defer st.Mu.RUnlock()
	if token, _ := st.tokenByValueLocked(value); token != nil && token.FineGrained {
		return token.FineGrainedID, token.Name, true
	}
	return 0, "", false
}

// RecordAPIRequest appends an attributed request record and persists it, with
// FIFO eviction at the cap.
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
	// Insert and evictions commit in one transaction so a crash cannot leave the
	// bucket over its cap or missing the just-served request (STORE-001/002).
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

// ApiInsightsRecords returns the org's records inside [minT, maxT], oldest first.
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

// APIInsightsActorType is the credential taxonomy of a request's actor;
// APIInsightsSubjectType is the account it ran on behalf of.
type APIInsightsActorType string

type APIInsightsSubjectType string
