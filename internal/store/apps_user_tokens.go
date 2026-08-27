package store

import (
	"fmt"
	"time"
)

// UserToServerToken is an OAuth-derived token bearing a user identity. Two
// prefix variants: gho_ (classic OAuth-App user token) sets OAuthAppClientID
// and classic Scopes; ghu_ (GitHub-App user-to-server) sets AppID and is scoped
// to installation permissions.
type UserToServerToken struct {
	Token             string            `json:"token"`
	UserID            int               `json:"user_id"`
	AppID             int               `json:"app_id"`              // set for ghu_ (GitHub App user-to-server)
	OAuthAppClientID  string            `json:"oauth_app_client_id"` // set for gho_ (OAuth App user token)
	Scopes            string            `json:"scopes"`              // classic OAuth scopes when gho_
	InstallationIDs   []int             `json:"installation_ids,omitempty"`
	Permissions       map[string]string `json:"permissions,omitempty"`    // nil means not permission-scoped
	RepositoryIDs     []int             `json:"repository_ids,omitempty"` // nil means not repository-scoped
	ExpiresAt         time.Time         `json:"expires_at"`
	RefreshTokenValue string            `json:"refresh_token_value,omitempty"`
	CreatedAt         time.Time         `json:"created_at"`
	Note              string            `json:"note,omitempty"`
	NoteURL           string            `json:"note_url,omitempty"`
	Fingerprint       string            `json:"fingerprint,omitempty"`
}

// RefreshToken mints a fresh user token past its expiry without re-running the
// OAuth flow.
type RefreshToken struct {
	Token            string
	UserID           int
	AppID            int
	OAuthAppClientID string
	Scopes           string
	ExpiresAt        time.Time // typically 6 months
	CreatedAt        time.Time
}

// CreateUserToServerToken mints a gho_/ghu_ token (+ optional ghr_ pair).
// appID > 0 yields ghu_; otherwise oauthClientID yields gho_.
func (st *Store) CreateUserToServerToken(userID, appID int, oauthClientID, scopes string, ttl time.Duration, withRefresh bool) (*UserToServerToken, *RefreshToken) {
	tok, rt, err := st.CreateUserToServerTokenE(userID, appID, oauthClientID, scopes, ttl, withRefresh)
	if err != nil {
		panic(err)
	}
	return tok, rt
}

func (st *Store) CreateUserToServerTokenE(userID, appID int, oauthClientID, scopes string, ttl time.Duration, withRefresh bool) (*UserToServerToken, *RefreshToken, error) {
	st.Mu.Lock()
	defer st.Mu.Unlock()
	return st.CreateUserToServerTokenLocked(userID, appID, oauthClientID, scopes, ttl, withRefresh)
}

func (st *Store) CreateUserToServerTokenLocked(userID, appID int, oauthClientID, scopes string, ttl time.Duration, withRefresh bool) (*UserToServerToken, *RefreshToken, error) {
	if st.UserToServerTokens == nil {
		st.UserToServerTokens = make(map[string]*UserToServerToken)
	}
	if st.RefreshTokens == nil {
		st.RefreshTokens = make(map[string]*RefreshToken)
	}

	now := st.CurrentTime()
	if ttl <= 0 {
		ttl = 8 * time.Hour
	}
	prefix := TokenPrefixOAuthUser
	if appID > 0 {
		prefix = TokenPrefixAppUser
	}
	h, err := RandomHex(20)
	if err != nil {
		return nil, nil, fmt.Errorf("generate user-to-server token: %w", err)
	}
	tokenStr := prefix + h

	tok := &UserToServerToken{
		Token:            tokenStr,
		UserID:           userID,
		AppID:            appID,
		OAuthAppClientID: oauthClientID,
		Scopes:           scopes,
		ExpiresAt:        now.Add(ttl),
		CreatedAt:        now,
	}

	// Access token and refresh token commit in one transaction so a crash cannot
	// orphan one from the other (STORE-001/002).
	batch := NewPersistBatch(st.Persist)
	var rt *RefreshToken
	if withRefresh {
		refreshHex, err := RandomHex(20)
		if err != nil {
			return nil, nil, fmt.Errorf("generate refresh token: %w", err)
		}
		rt = &RefreshToken{
			Token:            TokenPrefixRefresh + refreshHex,
			UserID:           userID,
			AppID:            appID,
			OAuthAppClientID: oauthClientID,
			Scopes:           scopes,
			ExpiresAt:        now.Add(6 * 30 * 24 * time.Hour),
			CreatedAt:        now,
		}
		tok.RefreshTokenValue = rt.Token
		st.RefreshTokens[rt.Token] = rt
		batch.Put("refresh_tokens", rt.Token, rt)
	}
	st.UserToServerTokens[tokenStr] = tok
	batch.Put("user_to_server_tokens", tokenStr, tok)
	if err := batch.Commit(); err != nil {
		return nil, nil, fmt.Errorf("persist user-to-server token: %w", err)
	}
	return CloneUserToServerToken(tok), cloneRefreshToken(rt), nil
}

// SetUserToServerTokenInstallations binds the token to a set of installation IDs.
func (st *Store) SetUserToServerTokenInstallations(tokenStr string, installationIDs []int) bool {
	st.Mu.Lock()
	defer st.Mu.Unlock()
	tok := st.UserToServerTokens[tokenStr]
	if tok == nil {
		return false
	}
	tok.InstallationIDs = append([]int(nil), installationIDs...)
	if st.Persist != nil {
		st.Persist.MustPut("user_to_server_tokens", tokenStr, tok)
	}
	return true
}

// ScopeUserToServerToken persists a scoped GitHub App user token's capability
// constraints. Nil permissions/repositoryIDs leave that dimension unrestricted;
// an explicit empty map/slice scopes to none.
func (st *Store) ScopeUserToServerToken(tokenStr string, installationIDs []int, permissions map[string]string, repositoryIDs []int) bool {
	st.Mu.Lock()
	defer st.Mu.Unlock()
	tok := st.UserToServerTokens[tokenStr]
	if tok == nil {
		return false
	}
	tok.InstallationIDs = append([]int(nil), installationIDs...)
	if permissions == nil {
		tok.Permissions = nil
	} else {
		tok.Permissions = make(map[string]string, len(permissions))
		for scope, level := range permissions {
			tok.Permissions[scope] = level
		}
	}
	if repositoryIDs == nil {
		tok.RepositoryIDs = nil
	} else {
		tok.RepositoryIDs = append([]int{}, repositoryIDs...)
	}
	if st.Persist != nil {
		st.Persist.MustPut("user_to_server_tokens", tokenStr, tok)
	}
	return true
}

// LookupUserToServerToken returns the token and bearing user, or nil if not found or expired.
func (st *Store) LookupUserToServerToken(tokenStr string) (*UserToServerToken, *User) {
	st.Mu.RLock()
	defer st.Mu.RUnlock()
	tok := st.UserToServerTokens[tokenStr]
	if tok == nil || st.CurrentTime().After(tok.ExpiresAt) {
		return nil, nil
	}
	token := CloneUserToServerToken(tok)
	user := st.Users[tok.UserID]
	if user == nil {
		return token, nil
	}
	userCopy := *user
	return token, &userCopy
}

// RevokeUserToServerToken drops a user-to-server token. Returns true if it existed.
func (st *Store) RevokeUserToServerToken(tokenStr string) bool {
	st.Mu.Lock()
	defer st.Mu.Unlock()
	tok := st.UserToServerTokens[tokenStr]
	if tok == nil {
		return false
	}
	// Access and refresh token revoked in one transaction so a crash cannot
	// leave one alive after the other (STORE-001/002).
	batch := NewPersistBatch(st.Persist)
	delete(st.UserToServerTokens, tokenStr)
	batch.Delete("user_to_server_tokens", tokenStr)
	if tok.RefreshTokenValue != "" {
		delete(st.RefreshTokens, tok.RefreshTokenValue)
		batch.Delete("refresh_tokens", tok.RefreshTokenValue)
	}
	if err := batch.Commit(); err != nil {
		panic(&PersistenceFailure{Op: "batch", Bucket: "user_to_server_tokens", Err: err})
	}
	return true
}

// RotateUserToServerToken mints a fresh token+refresh pair from a valid refresh
// token, revoking the old pair. Returns nil if refresh is invalid.
func (st *Store) RotateUserToServerToken(refreshTokenStr string) (*UserToServerToken, *RefreshToken) {
	tok, rt, err := st.RotateUserToServerTokenE(refreshTokenStr)
	if err != nil {
		panic(err)
	}
	return tok, rt
}

func (st *Store) RotateUserToServerTokenE(refreshTokenStr string) (*UserToServerToken, *RefreshToken, error) {
	st.Mu.Lock()
	defer st.Mu.Unlock()
	rt := st.RefreshTokens[refreshTokenStr]
	if rt == nil || st.CurrentTime().After(rt.ExpiresAt) {
		return nil, nil, nil
	}
	// Revoke the old pair in one write-through transaction so rotated-out
	// credentials stay dead across a restart (STORE-001/002). The replacement is
	// a separate transaction; a crash between the two only forces re-auth, never
	// leaves a live stale credential.
	batch := NewPersistBatch(st.Persist)
	for k, v := range st.UserToServerTokens {
		if v.RefreshTokenValue == refreshTokenStr {
			delete(st.UserToServerTokens, k)
			batch.Delete("user_to_server_tokens", k)
			break
		}
	}
	delete(st.RefreshTokens, refreshTokenStr)
	batch.Delete("refresh_tokens", refreshTokenStr)
	if err := batch.Commit(); err != nil {
		return nil, nil, fmt.Errorf("revoke rotated token: %w", err)
	}
	return st.CreateUserToServerTokenLocked(rt.UserID, rt.AppID, rt.OAuthAppClientID, rt.Scopes, 8*time.Hour, true)
}

// RevokeUserGrant deletes every user-to-server and refresh token for (clientID,
// userID). Mirrors DELETE /applications/{client_id}/grant.
func (st *Store) RevokeUserGrant(clientID string, userID int) int {
	st.Mu.Lock()
	defer st.Mu.Unlock()
	// Whole grant revoked in one transaction so a crash cannot leave part of it
	// alive (STORE-001/002).
	batch := NewPersistBatch(st.Persist)
	n := 0
	for k, v := range st.UserToServerTokens {
		hit := false
		if v.UserID == userID {
			if v.OAuthAppClientID == clientID {
				hit = true
			} else if v.AppID > 0 {
				if app := st.AppsByClientID[clientID]; app != nil && app.ID == v.AppID {
					hit = true
				}
			}
		}
		if hit {
			delete(st.UserToServerTokens, k)
			batch.Delete("user_to_server_tokens", k)
			if v.RefreshTokenValue != "" {
				delete(st.RefreshTokens, v.RefreshTokenValue)
				batch.Delete("refresh_tokens", v.RefreshTokenValue)
			}
			n++
		}
	}
	if n > 0 {
		if err := batch.Commit(); err != nil {
			panic(&PersistenceFailure{Op: "batch", Bucket: "user_to_server_tokens", Err: err})
		}
	}
	return n
}

func cloneRefreshToken(token *RefreshToken) *RefreshToken {
	if token == nil {
		return nil
	}
	copy := *token
	return &copy
}

func CloneUserToServerToken(token *UserToServerToken) *UserToServerToken {
	if token == nil {
		return nil
	}
	copy := *token
	copy.InstallationIDs = append([]int(nil), token.InstallationIDs...)
	if token.Permissions != nil {
		copy.Permissions = make(map[string]string, len(token.Permissions))
		for scope, level := range token.Permissions {
			copy.Permissions[scope] = level
		}
	}
	if token.RepositoryIDs != nil {
		copy.RepositoryIDs = append([]int{}, token.RepositoryIDs...)
	}
	return &copy
}
