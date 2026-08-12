package store

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"
)

const (
	LoginSessionsBucket    = "login_sessions"
	oidcLogoutClaimsBucket = "oidc_logout_claims"
	loginSessionReapPeriod = time.Hour
	// LoginSessionsByUserBucket is a durable secondary index: one row per
	// session keyed by `<userID>\x00<sessionStorageKey>`, so every session a
	// user holds is fetched by a bounded prefix scan (STORE-025) instead of
	// listing and decoding the whole session bucket on a per-user logout. It is
	// a superset of live sessions — PutLoginSession writes the index row in the
	// same batch as the session, so revocation never misses a live session;
	// harmless stale rows (a session dropped by the bespoke OIDC back-channel
	// transaction) are reclaimed the next time that user's sessions are purged.
	LoginSessionsByUserBucket = "login_sessions_by_user"
	// loginSessionUserIndexSep separates the numeric user id from the session
	// storage key. ':' is not a digit, so it cleanly bounds one user's prefix
	// range (`1:` never overlaps `12:`), and — unlike a NUL byte — it carries no
	// embedded-NUL-in-TEXT risk across SQLite/dqlite drivers. The session key
	// suffix may itself contain ':' (it is `hmac:v1:…`); the FIRST separator is
	// always the one after the all-digit user id.
	loginSessionUserIndexSep = ":"
)

// loginSessionUserIndexKey composes the secondary-index row key.
func loginSessionUserIndexKey(userID int, sessionStorageKey string) string {
	return strconv.Itoa(userID) + loginSessionUserIndexSep + sessionStorageKey
}

func LoginSessionUserIndexPrefix(userID int) string {
	return strconv.Itoa(userID) + loginSessionUserIndexSep
}

// sessionStorageKeyFromIndexKey recovers the session storage key an index row
// points at (everything after the first separator; the user id is all digits so
// the first ':' is unambiguously the separator).
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
	// Always digest — never honor a client-supplied "hmac:v1:" cookie value as a
	// storage key, or a leaked session-row key could be presented as the cookie.
	return persist.opaqueLookupKey(LoginSessionsBucket, id)
}

func (st *Store) PutLoginSession(id string, session *LoginSession) error {
	if id == "" || session == nil {
		return fmt.Errorf("login session id and value are required")
	}
	// Hold st.Mu across both the durable Put and the in-memory map write so the
	// two land as one critical section. Splitting them lets two concurrent
	// writers for the same id interleave (A persists v1, B persists v2, B stores
	// v2 in the map, A stores v1) and leave the durable row and the map
	// disagreeing on which write won.
	st.Mu.Lock()
	defer st.Mu.Unlock()
	persist := st.Persist
	mapKey := LoginSessionMapKey(persist, id)
	if persist != nil {
		// The session row and its secondary-index row commit together so the
		// index can never miss a session that exists (STORE-025).
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

// ReapExpiredLoginSessions bounds both the in-memory session index and its
// durable bucket. It reaps two kinds of dead session: those past their expiry,
// and those orphaned by user deletion — a deleted user is dropped from the
// in-memory index on reload, but its durable rows otherwise linger until
// natural expiry. It is safe across replicas: every deletion is idempotent and
// the persistence batch commits all selected rows together. The caller supplies
// time so tests and scheduler execution remain deterministic.
func (st *Store) ReapExpiredLoginSessions(now time.Time) error {
	now = now.UTC()
	st.Mu.Lock()
	if !st.nextLoginSessionReap.IsZero() && now.Before(st.nextLoginSessionReap) {
		st.Mu.Unlock()
		return nil
	}
	st.nextLoginSessionReap = now.Add(loginSessionReapPeriod)
	persist := st.Persist
	// Snapshot the live user IDs under the same lock so orphaned sessions are
	// reaped alongside expired ones. User IDs are monotonic and never reused, so
	// a UserID absent from this snapshot belongs to a user that is gone for good;
	// a concurrently created user can at worst make us retain its session one
	// extra cycle, never drop a live one.
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
			// Drop the session's secondary-index row in the same batch (id is the
			// session's storage key, exactly what the index row embeds).
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

// backfillLoginSessionUserIndex upserts a secondary-index row for every session
// currently loaded, so the per-user index is complete after startup regardless
// of history: sessions written before the index existed (an upgrade) and any
// drift are healed here. It runs once per process at SetPersistence, never on a
// request-time refresh, and is idempotent — the rows are upserts. A dqlite peer
// starting concurrently writes the same rows to the same keys, so replicas do
// not clobber one another.
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
	// Read by the already-digested Key: passing the raw client value would let
	// persist.Get honor a leaked "hmac:v1:" row key presented as the cookie.
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
	// Return a distinct copy, never the pointer now living in the map: a caller
	// that mutates the result (e.g. rotating the CSRF token) must not race a
	// concurrent locked reader of the map entry.
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
		// Resolve the owner so the secondary-index row can be dropped too. Prefer
		// the in-memory copy; fall back to a point read for a session this
		// replica never cached.
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
		// session by presenting its stored "hmac:v1:" row key.
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
		// The secondary index gives this user's session rows by a bounded prefix
		// scan, so a per-user purge no longer lists and decodes the entire
		// session bucket (STORE-025). It reads durable state, so it revokes
		// sessions this replica never cached, including ones created elsewhere.
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

// DeleteLoginSessionsForOIDC revokes the browser sessions selected by an
// OpenID Connect Back-Channel Logout token. A sid selects one provider session;
// without sid, sub selects every local session for that provider identity.
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
// token and revokes the sessions it selects. A persistent store performs both
// operations in one database transaction; an ephemeral store performs both
// while holding its map mutex.
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
