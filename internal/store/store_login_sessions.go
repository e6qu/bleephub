package store

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	LoginSessionsBucket    = "login_sessions"
	oidcLogoutClaimsBucket = "oidc_logout_claims"
	loginSessionReapPeriod = time.Hour
	// LoginSessionsByUserBucket is a durable secondary index, one row per
	// session keyed by `<userID>:<sessionStorageKey>`, so a user's sessions are
	// fetched by a bounded prefix scan (STORE-025) rather than scanning the
	// whole session bucket. PutLoginSession writes the index row in the same
	// batch as the session, so it is a superset of live sessions and revocation
	// never misses one; stale rows are reclaimed on the next per-user purge.
	LoginSessionsByUserBucket = "login_sessions_by_user"
	// loginSessionUserIndexSep separates the numeric user id from the session
	// storage key. ':' is not a digit, so `1:` never overlaps `12:`, and unlike
	// NUL it carries no embedded-NUL-in-TEXT risk across SQLite/dqlite. The key
	// suffix may itself contain ':' (`hmac:v1:…`); the FIRST separator is the
	// one after the all-digit user id.
	loginSessionUserIndexSep = ":"
)

func loginSessionUserIndexKey(userID int, sessionStorageKey string) string {
	return strconv.Itoa(userID) + loginSessionUserIndexSep + sessionStorageKey
}

func LoginSessionUserIndexPrefix(userID int) string {
	return strconv.Itoa(userID) + loginSessionUserIndexSep
}

// sessionStorageKeyFromIndexKey recovers the session storage key an index row
// points at (everything after the first ':', which is unambiguously the
// separator since the user id is all digits).
func sessionStorageKeyFromIndexKey(indexKey string) string {
	if i := strings.Index(indexKey, loginSessionUserIndexSep); i >= 0 {
		return indexKey[i+len(loginSessionUserIndexSep):]
	}
	return ""
}

type oidcLogoutReplayMarker struct {
	ExpiresAt time.Time `json:"expires_at"`
}

func OidcLogoutReplayKey(provider, issuer, clientID, jti string) string {
	encoded, err := json.Marshal([4]string{provider, issuer, clientID, jti})
	if err != nil {
		panic("encode fixed OpenID Connect logout replay key: " + err.Error())
	}
	return fmt.Sprintf("%x", sha256.Sum256(encoded))
}

func oidcLoginSessionMatches(session *LoginSession, provider, issuer, sid, subject string) bool {
	if session.OIDCProvider != provider || session.OIDCIssuer != issuer {
		return false
	}
	if sid != "" {
		return session.OIDCSID == sid
	}
	return subject != "" && session.OIDCSubject == subject
}

func LoginSessionMapKey(persist *Persistence, id string) string {
	if persist == nil {
		return id
	}
	// Always digest — never honor a client-supplied "hmac:v1:" value as a
	// storage key, or a leaked row key could be presented as the cookie.
	return persist.opaqueLookupKey(LoginSessionsBucket, id)
}

func (st *Store) PutLoginSession(id string, session *LoginSession) error {
	if id == "" || session == nil {
		return fmt.Errorf("login session id and value are required")
	}
	// Hold st.Mu across both the durable Put and the map write as one critical
	// section, or two concurrent writers for the same id could leave the
	// durable row and the map disagreeing on which write won.
	st.Mu.Lock()
	defer st.Mu.Unlock()
	persist := st.Persist
	mapKey := LoginSessionMapKey(persist, id)
	if persist != nil {
		// Session row and index row commit together so the index never misses
		// an existing session (STORE-025).
		batch := NewPersistBatch(persist)
		batch.Put(LoginSessionsBucket, id, session)
		batch.Put(LoginSessionsByUserBucket, loginSessionUserIndexKey(session.UserID, mapKey), struct{}{})
		if err := batch.Commit(); err != nil {
			return fmt.Errorf("persist login session: %w", err)
		}
	}
	copy := *session
	st.LoginSessions[mapKey] = &copy
	return nil
}

// MarkLoginSessionSudo records that the session just satisfied a
// proof-of-presence challenge, reporting whether such a session existed (an
// expired one is not resurrected). It writes through PutLoginSession so the
// grant is durable and survives a replica switch.
func (st *Store) MarkLoginSessionSudo(id string, now time.Time, withSecondFactor bool) (bool, error) {
	session, err := st.GetLoginSession(id)
	if err != nil {
		return false, err
	}
	if session == nil {
		return false, nil
	}
	session.SudoAt = now.UTC()
	session.SudoMFA = withSecondFactor
	if err := st.PutLoginSession(id, session); err != nil {
		return false, err
	}
	return true, nil
}

// ReapExpiredLoginSessions bounds the in-memory index and durable bucket,
// reaping sessions past expiry and those orphaned by user deletion (whose
// durable rows would otherwise linger until natural expiry). Deletions are
// idempotent, so it is replica-safe. The caller supplies time for determinism.
func (st *Store) ReapExpiredLoginSessions(now time.Time) error {
	now = now.UTC()
	st.Mu.Lock()
	if !st.nextLoginSessionReap.IsZero() && now.Before(st.nextLoginSessionReap) {
		st.Mu.Unlock()
		return nil
	}
	st.nextLoginSessionReap = now.Add(loginSessionReapPeriod)
	persist := st.Persist
	// Snapshot live user IDs under the same lock. IDs are monotonic and never
	// reused, so a UserID absent here belongs to a gone user; a concurrently
	// created user is at worst retained one extra cycle, never dropped live.
	liveUsers := make(map[int]struct{}, len(st.Users))
	for id := range st.Users {
		liveUsers[id] = struct{}{}
	}
	st.Mu.Unlock()

	dead := func(session *LoginSession) bool {
		if !session.ExpiresAt.After(now) {
			return true
		}
		_, ok := liveUsers[session.UserID]
		return !ok
	}

	reaped := make(map[string]struct{})
	if persist != nil {
		rows, err := persist.List(LoginSessionsBucket)
		if err != nil {
			st.resetLoginSessionReap()
			return fmt.Errorf("list expired login sessions: %w", err)
		}
		entries := make([]PersistencePut, 0)
		for id, raw := range rows {
			var session LoginSession
			if err := json.Unmarshal(raw, &session); err != nil {
				st.resetLoginSessionReap()
				return fmt.Errorf("decode login session %s: %w", id, err)
			}
			if !dead(&session) {
				continue
			}
			reaped[id] = struct{}{}
			entries = append(entries, PersistencePut{Bucket: LoginSessionsBucket, Key: id})
			// Drop the index row in the same batch.
			entries = append(entries, PersistencePut{Bucket: LoginSessionsByUserBucket, Key: loginSessionUserIndexKey(session.UserID, id)})
		}
		if err := persist.DeleteBatch(entries...); err != nil {
			st.resetLoginSessionReap()
			return fmt.Errorf("delete expired login sessions: %w", err)
		}
	}

	st.Mu.Lock()
	for id, session := range st.LoginSessions {
		if _, selected := reaped[id]; selected || dead(session) {
			delete(st.LoginSessions, id)
		}
	}
	st.Mu.Unlock()
	return nil
}

// backfillLoginSessionUserIndex upserts an index row for every loaded session,
// healing sessions written before the index existed and any drift. It runs
// once per process at SetPersistence and is idempotent, so concurrent replicas
// do not clobber one another.
func (st *Store) backfillLoginSessionUserIndex() error {
	st.Mu.RLock()
	persist := st.Persist
	keys := make([]string, 0, len(st.LoginSessions))
	for storageKey, session := range st.LoginSessions {
		keys = append(keys, loginSessionUserIndexKey(session.UserID, storageKey))
	}
	st.Mu.RUnlock()
	if persist == nil || len(keys) == 0 {
		return nil
	}
	batch := NewPersistBatch(persist)
	for _, key := range keys {
		batch.Put(LoginSessionsByUserBucket, key, struct{}{})
	}
	if err := batch.Commit(); err != nil {
		return fmt.Errorf("backfill login-session user index: %w", err)
	}
	return nil
}

func (st *Store) resetLoginSessionReap() {
	st.Mu.Lock()
	st.nextLoginSessionReap = time.Time{}
	st.Mu.Unlock()
}

func (st *Store) GetLoginSession(id string) (*LoginSession, error) {
	st.Mu.RLock()
	persist := st.Persist
	mapKey := LoginSessionMapKey(persist, id)
	local := st.LoginSessions[mapKey]
	st.Mu.RUnlock()
	if persist == nil {
		if local == nil {
			return nil, nil
		}
		copy := *local
		return &copy, nil
	}
	// Read by the digested key: a raw client value would let persist.Get honor
	// a leaked "hmac:v1:" row key presented as the cookie.
	raw, err := persist.Get(LoginSessionsBucket, mapKey)
	if err != nil {
		return nil, fmt.Errorf("read login session: %w", err)
	}
	if raw == nil {
		st.Mu.Lock()
		delete(st.LoginSessions, mapKey)
		st.Mu.Unlock()
		return nil, nil
	}
	var session LoginSession
	if err := json.Unmarshal(raw, &session); err != nil {
		return nil, fmt.Errorf("decode login session: %w", err)
	}
	if !session.ExpiresAt.After(st.CurrentTime()) {
		if err := st.DeleteLoginSession(id); err != nil {
			return nil, err
		}
		return nil, nil
	}
	st.Mu.Lock()
	cached := session
	st.LoginSessions[mapKey] = &cached
	st.Mu.Unlock()
	// Return a distinct copy, never the pointer in the map: a caller mutating
	// the result (e.g. rotating the CSRF token) must not race a locked reader.
	result := session
	return &result, nil
}

func (st *Store) DeleteLoginSession(id string) error {
	st.Mu.RLock()
	persist := st.Persist
	cached := st.LoginSessions[LoginSessionMapKey(persist, id)]
	st.Mu.RUnlock()
	mapKey := LoginSessionMapKey(persist, id)
	if persist != nil {
		// Resolve the owner to drop the index row too, falling back to a point
		// read for a session this replica never cached.
		userID := -1
		if cached != nil {
			userID = cached.UserID
		} else if raw, err := persist.Get(LoginSessionsBucket, id); err == nil && raw != nil {
			var s LoginSession
			if json.Unmarshal(raw, &s) == nil {
				userID = s.UserID
			}
		}
		// Delete by the digested key so a client cannot delete an arbitrary
		// session via its stored "hmac:v1:" row key.
		batch := NewPersistBatch(persist)
		batch.Delete(LoginSessionsBucket, mapKey)
		if userID >= 0 {
			batch.Delete(LoginSessionsByUserBucket, loginSessionUserIndexKey(userID, mapKey))
		}
		if err := batch.Commit(); err != nil {
			return fmt.Errorf("delete login session: %w", err)
		}
	}
	st.Mu.Lock()
	delete(st.LoginSessions, mapKey)
	st.Mu.Unlock()
	return nil
}

func (st *Store) DeleteLoginSessionsForUser(userID int) error {
	st.Mu.RLock()
	persist := st.Persist
	st.Mu.RUnlock()
	if persist != nil {
		// Prefix-scan the index for this user's rows (STORE-025), reading
		// durable state so sessions created on another replica are revoked too.
		idxRows, err := persist.ListPrefix(LoginSessionsByUserBucket, LoginSessionUserIndexPrefix(userID))
		if err != nil {
			return fmt.Errorf("list login-session index for user %d: %w", userID, err)
		}
		entries := make([]PersistencePut, 0, 2*len(idxRows))
		for indexKey := range idxRows {
			sessionKey := sessionStorageKeyFromIndexKey(indexKey)
			if sessionKey == "" {
				continue
			}
			entries = append(entries,
				PersistencePut{Bucket: LoginSessionsBucket, Key: sessionKey},
				PersistencePut{Bucket: LoginSessionsByUserBucket, Key: indexKey})
		}
		if err := persist.DeleteBatch(entries...); err != nil {
			return fmt.Errorf("delete login sessions: %w", err)
		}
	}
	st.Mu.Lock()
	for id, session := range st.LoginSessions {
		if session.UserID == userID {
			delete(st.LoginSessions, id)
		}
	}
	st.Mu.Unlock()
	return nil
}

// LoginSessionSummary is one row of the account's "active sessions" list. It
// carries no credential: the storage key, CSRF token and ID token stay in the
// store, and the UI revokes by Handle, drawn independently of the cookie.
type LoginSessionSummary struct {
	Handle     string    `json:"handle"`
	CreatedAt  time.Time `json:"created_at,omitzero"`
	ExpiresAt  time.Time `json:"expires_at"`
	UserAgent  string    `json:"user_agent,omitempty"`
	SignedInIP string    `json:"signed_in_ip,omitempty"`
	// Provider names the establishing IdP, empty for a local sign-in.
	Provider string `json:"provider,omitempty"`
}

func loginSessionSummary(session *LoginSession) LoginSessionSummary {
	return LoginSessionSummary{
		Handle:     session.Handle,
		CreatedAt:  session.CreatedAt,
		ExpiresAt:  session.ExpiresAt,
		UserAgent:  session.UserAgent,
		SignedInIP: session.SignedInIP,
		Provider:   session.OIDCProvider,
	}
}

// forEachLoginSessionOfUser visits every live session the user holds. With
// persistence it reads through the durable per-user index (STORE-025), so
// sessions established on another replica are visited too; otherwise the
// in-memory map is the whole truth. Expired rows are skipped.
func (st *Store) forEachLoginSessionOfUser(userID int, now time.Time, visit func(storageKey string, session *LoginSession)) error {
	st.Mu.RLock()
	persist := st.Persist
	st.Mu.RUnlock()
	if persist == nil {
		st.Mu.RLock()
		defer st.Mu.RUnlock()
		for storageKey, session := range st.LoginSessions {
			if session.UserID != userID || !session.ExpiresAt.After(now) {
				continue
			}
			// Never hand the caller the pointer living in the map.
			detached := *session
			visit(storageKey, &detached)
		}
		return nil
	}
	rows, err := persist.ListPrefix(LoginSessionsByUserBucket, LoginSessionUserIndexPrefix(userID))
	if err != nil {
		return fmt.Errorf("list login-session index for user %d: %w", userID, err)
	}
	for indexKey := range rows {
		storageKey := sessionStorageKeyFromIndexKey(indexKey)
		if storageKey == "" {
			continue
		}
		raw, err := persist.Get(LoginSessionsBucket, storageKey)
		if err != nil {
			return fmt.Errorf("read login session: %w", err)
		}
		if raw == nil {
			// Stale index row for a session dropped elsewhere.
			continue
		}
		var session LoginSession
		if err := json.Unmarshal(raw, &session); err != nil {
			return fmt.Errorf("decode login session: %w", err)
		}
		if session.UserID != userID || !session.ExpiresAt.After(now) {
			continue
		}
		visit(storageKey, &session)
	}
	return nil
}

// ListLoginSessionsForUser returns the user's live browser sessions, newest
// first. Sessions predating the handle are reported with an empty handle and
// are not revocable by name.
func (st *Store) ListLoginSessionsForUser(userID int, now time.Time) ([]LoginSessionSummary, error) {
	var summaries []LoginSessionSummary
	err := st.forEachLoginSessionOfUser(userID, now, func(_ string, session *LoginSession) {
		summaries = append(summaries, loginSessionSummary(session))
	})
	if err != nil {
		return nil, err
	}
	sort.SliceStable(summaries, func(i, j int) bool {
		if !summaries[i].CreatedAt.Equal(summaries[j].CreatedAt) {
			return summaries[i].CreatedAt.After(summaries[j].CreatedAt)
		}
		return summaries[i].Handle < summaries[j].Handle
	})
	return summaries, nil
}

// DeleteLoginSessionByHandle revokes one of the user's sessions by its public
// handle. The handle is scoped to the user, so one account cannot revoke
// another's by guessing. Reports whether a session matched.
func (st *Store) DeleteLoginSessionByHandle(userID int, handle string, now time.Time) (bool, error) {
	if handle == "" {
		return false, nil
	}
	var targets []struct {
		storageKey string
	}
	err := st.forEachLoginSessionOfUser(userID, now, func(storageKey string, session *LoginSession) {
		if session.Handle == handle {
			targets = append(targets, struct{ storageKey string }{storageKey})
		}
	})
	if err != nil {
		return false, err
	}
	if len(targets) == 0 {
		return false, nil
	}
	st.Mu.RLock()
	persist := st.Persist
	st.Mu.RUnlock()
	if persist != nil {
		batch := NewPersistBatch(persist)
		for _, target := range targets {
			batch.Delete(LoginSessionsBucket, target.storageKey)
			batch.Delete(LoginSessionsByUserBucket, loginSessionUserIndexKey(userID, target.storageKey))
		}
		if err := batch.Commit(); err != nil {
			return false, fmt.Errorf("delete login session: %w", err)
		}
	}
	st.Mu.Lock()
	for _, target := range targets {
		delete(st.LoginSessions, target.storageKey)
	}
	st.Mu.Unlock()
	return true, nil
}

// DeleteLoginSessionsForOIDC revokes the sessions selected by an OpenID Connect
// Back-Channel Logout token: sid selects one provider session, otherwise sub
// selects every session for that provider identity.
func (st *Store) DeleteLoginSessionsForOIDC(provider, issuer, sid, subject string) error {
	st.Mu.RLock()
	persist := st.Persist
	st.Mu.RUnlock()
	matches := func(session *LoginSession) bool {
		return oidcLoginSessionMatches(session, provider, issuer, sid, subject)
	}
	if persist != nil {
		rows, err := persist.List(LoginSessionsBucket)
		if err != nil {
			return fmt.Errorf("list OpenID Connect login sessions: %w", err)
		}
		type sessionRef struct {
			Id     string `json:"-"`
			userID int
		}
		var refs []sessionRef
		for id, raw := range rows {
			var session LoginSession
			if err := json.Unmarshal(raw, &session); err != nil {
				return fmt.Errorf("decode OpenID Connect login session %s: %w", id, err)
			}
			if matches(&session) {
				refs = append(refs, sessionRef{Id: id, userID: session.UserID})
			}
		}
		entries := make([]PersistencePut, 0, 2*len(refs))
		for _, ref := range refs {
			entries = append(entries,
				PersistencePut{Bucket: LoginSessionsBucket, Key: ref.Id},
				PersistencePut{Bucket: LoginSessionsByUserBucket, Key: loginSessionUserIndexKey(ref.userID, ref.Id)})
		}
		if err := persist.DeleteBatch(entries...); err != nil {
			return fmt.Errorf("delete OpenID Connect login sessions: %w", err)
		}
	}
	st.Mu.Lock()
	for id, session := range st.LoginSessions {
		if matches(session) {
			delete(st.LoginSessions, id)
		}
	}
	st.Mu.Unlock()
	return nil
}

// ClaimOIDCLogoutAndDeleteSessions atomically claims a Back-Channel Logout
// token and revokes the sessions it selects — in one DB transaction with
// persistence, otherwise under the map mutex.
func (st *Store) ClaimOIDCLogoutAndDeleteSessions(provider, issuer, clientID, jti string, expiresAt, now time.Time, sid, subject string) (bool, error) {
	if provider == "" || issuer == "" || clientID == "" || jti == "" || !expiresAt.After(now) {
		return false, fmt.Errorf("complete OpenID Connect logout replay coordinates and a future expiry are required")
	}
	replayKey := OidcLogoutReplayKey(provider, issuer, clientID, jti)
	st.Mu.RLock()
	persist := st.Persist
	st.Mu.RUnlock()
	if persist != nil {
		claimed, err := persist.ClaimOIDCLogoutAndDeleteSessions(replayKey, expiresAt, now, provider, issuer, sid, subject)
		if err != nil || !claimed {
			return claimed, err
		}
		st.Mu.Lock()
		for id, session := range st.LoginSessions {
			if oidcLoginSessionMatches(session, provider, issuer, sid, subject) {
				delete(st.LoginSessions, id)
			}
		}
		st.Mu.Unlock()
		return true, nil
	}

	st.Mu.Lock()
	defer st.Mu.Unlock()
	for key, expiry := range st.OIDCLogoutClaims {
		if !expiry.After(now) {
			delete(st.OIDCLogoutClaims, key)
		}
	}
	if _, replayed := st.OIDCLogoutClaims[replayKey]; replayed {
		return false, nil
	}
	st.OIDCLogoutClaims[replayKey] = expiresAt
	for id, session := range st.LoginSessions {
		if oidcLoginSessionMatches(session, provider, issuer, sid, subject) {
			delete(st.LoginSessions, id)
		}
	}
	return true, nil
}
