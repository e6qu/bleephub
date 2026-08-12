package store

import "time"

// AuthCode is a one-time-use OAuth authorization code keyed off a
// client_id + state pair. Used by the web-flow endpoints below.
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

// createTokenLocked generates a new token (caller must hold st.Mu write lock).
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
