package bleephub

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"time"
)

const (
	loginSessionsBucket    = "login_sessions"
	oidcLogoutClaimsBucket = "oidc_logout_claims"
)

type oidcLogoutReplayMarker struct {
	ExpiresAt time.Time `json:"expires_at"`
}

func oidcLogoutReplayKey(provider, issuer, clientID, jti string) string {
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

func (st *Store) PutLoginSession(id string, session *LoginSession) error {
	if id == "" || session == nil {
		return fmt.Errorf("login session id and value are required")
	}
	st.mu.RLock()
	persist := st.persist
	st.mu.RUnlock()
	if persist != nil {
		if err := persist.Put(loginSessionsBucket, id, session); err != nil {
			return fmt.Errorf("persist login session: %w", err)
		}
	}
	st.mu.Lock()
	copy := *session
	st.LoginSessions[id] = &copy
	st.mu.Unlock()
	return nil
}

func (st *Store) GetLoginSession(id string) (*LoginSession, error) {
	st.mu.RLock()
	persist := st.persist
	local := st.LoginSessions[id]
	st.mu.RUnlock()
	if persist == nil {
		if local == nil {
			return nil, nil
		}
		copy := *local
		return &copy, nil
	}
	raw, err := persist.Get(loginSessionsBucket, id)
	if err != nil {
		return nil, fmt.Errorf("read login session: %w", err)
	}
	if raw == nil {
		st.mu.Lock()
		delete(st.LoginSessions, id)
		st.mu.Unlock()
		return nil, nil
	}
	var session LoginSession
	if err := json.Unmarshal(raw, &session); err != nil {
		return nil, fmt.Errorf("decode login session: %w", err)
	}
	if !session.ExpiresAt.After(time.Now()) {
		if err := st.DeleteLoginSession(id); err != nil {
			return nil, err
		}
		return nil, nil
	}
	st.mu.Lock()
	st.LoginSessions[id] = &session
	st.mu.Unlock()
	return &session, nil
}

func (st *Store) DeleteLoginSession(id string) error {
	st.mu.RLock()
	persist := st.persist
	st.mu.RUnlock()
	if persist != nil {
		if err := persist.Delete(loginSessionsBucket, id); err != nil {
			return fmt.Errorf("delete login session: %w", err)
		}
	}
	st.mu.Lock()
	delete(st.LoginSessions, id)
	st.mu.Unlock()
	return nil
}

func (st *Store) DeleteLoginSessionsForUser(userID int) error {
	st.mu.RLock()
	persist := st.persist
	st.mu.RUnlock()
	ids := map[string]struct{}{}
	if persist != nil {
		rows, err := persist.List(loginSessionsBucket)
		if err != nil {
			return fmt.Errorf("list login sessions: %w", err)
		}
		for id, raw := range rows {
			var session LoginSession
			if err := json.Unmarshal(raw, &session); err != nil {
				return fmt.Errorf("decode login session %s: %w", id, err)
			}
			if session.UserID == userID {
				ids[id] = struct{}{}
			}
		}
		entries := make([]persistencePut, 0, len(ids))
		for id := range ids {
			entries = append(entries, persistencePut{bucket: loginSessionsBucket, key: id})
		}
		if err := persist.DeleteBatch(entries...); err != nil {
			return fmt.Errorf("delete login sessions: %w", err)
		}
	}
	st.mu.Lock()
	for id, session := range st.LoginSessions {
		if session.UserID == userID {
			delete(st.LoginSessions, id)
		}
	}
	st.mu.Unlock()
	return nil
}

// DeleteLoginSessionsForOIDC revokes the browser sessions selected by an
// OpenID Connect Back-Channel Logout token. A sid selects one provider session;
// without sid, sub selects every local session for that provider identity.
func (st *Store) DeleteLoginSessionsForOIDC(provider, issuer, sid, subject string) error {
	st.mu.RLock()
	persist := st.persist
	st.mu.RUnlock()
	matches := func(session *LoginSession) bool {
		return oidcLoginSessionMatches(session, provider, issuer, sid, subject)
	}
	ids := map[string]struct{}{}
	if persist != nil {
		rows, err := persist.List(loginSessionsBucket)
		if err != nil {
			return fmt.Errorf("list OpenID Connect login sessions: %w", err)
		}
		for id, raw := range rows {
			var session LoginSession
			if err := json.Unmarshal(raw, &session); err != nil {
				return fmt.Errorf("decode OpenID Connect login session %s: %w", id, err)
			}
			if matches(&session) {
				ids[id] = struct{}{}
			}
		}
		entries := make([]persistencePut, 0, len(ids))
		for id := range ids {
			entries = append(entries, persistencePut{bucket: loginSessionsBucket, key: id})
		}
		if err := persist.DeleteBatch(entries...); err != nil {
			return fmt.Errorf("delete OpenID Connect login sessions: %w", err)
		}
	}
	st.mu.Lock()
	for id, session := range st.LoginSessions {
		if matches(session) {
			delete(st.LoginSessions, id)
		}
	}
	st.mu.Unlock()
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
	replayKey := oidcLogoutReplayKey(provider, issuer, clientID, jti)
	st.mu.RLock()
	persist := st.persist
	st.mu.RUnlock()
	if persist != nil {
		claimed, err := persist.ClaimOIDCLogoutAndDeleteSessions(replayKey, expiresAt, now, provider, issuer, sid, subject)
		if err != nil || !claimed {
			return claimed, err
		}
		st.mu.Lock()
		for id, session := range st.LoginSessions {
			if oidcLoginSessionMatches(session, provider, issuer, sid, subject) {
				delete(st.LoginSessions, id)
			}
		}
		st.mu.Unlock()
		return true, nil
	}

	st.mu.Lock()
	defer st.mu.Unlock()
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
