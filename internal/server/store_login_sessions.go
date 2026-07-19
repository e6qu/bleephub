package bleephub

import (
	"encoding/json"
	"fmt"
	"time"
)

const loginSessionsBucket = "login_sessions"

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
