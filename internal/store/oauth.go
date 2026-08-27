package store

import "time"

// AuthCode is a one-time-use OAuth authorization code keyed off a client_id + state pair.
type AuthCode struct {
	Code        string
	ClientID    string
	RedirectURI string
	Scopes      string
	State       string
	UserID      int
	CreatedAt   time.Time
	ExpiresAt   time.Time
}

// CreateTokenLocked generates a new token; caller holds st.Mu write lock.
func (st *Store) CreateTokenLocked(userID int, scopes string) *Token {
	value, err := generateTokenValue()
	if err != nil {
		panic(err)
	}
	t := &Token{
		Value:     value,
		UserID:    userID,
		Scopes:    scopes,
		CreatedAt: time.Now(),
	}
	st.Tokens[st.tokenMapKey(t.Value)] = t
	st.PersistTokenLocked(t)
	return t
}
