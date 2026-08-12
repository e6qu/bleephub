package store

import (
	"fmt"
	"time"
)

// UserToServerToken is an OAuth-derived token bearing a user identity.
//
// Two prefix variants:
//   - gho_  — classic OAuth-App user token, classic scopes (`repo`, `read:org`, …)
//   - ghu_  — GitHub-App user-to-server token, scoped to the app's installation permissions
//
// Both carry the user identity through the request middleware. The difference is the
// scope model and whether AppID is set (ghu_) vs OAuthAppClientID (gho_).
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

// RefreshToken pairs with a UserToServerToken. Used to mint a fresh user token
// past the user token's expiry without re-running the OAuth flow.
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
// Pass appID > 0 for ghu_ (GitHub-App user-to-server) or oauthClientID for gho_ (OAuth-App user).
// If withRefresh is true, also mints a ghr_ refresh token.
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

	// One transaction: the access token and its refresh token commit together, so
	// a crash cannot persist an access token whose refresh token was lost (or a
	// refresh token orphaned from its access token) (STORE-001/002).
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

// SetUserToServerTokenInstallations binds the token to a set of installation
// IDs (used when a token is scoped down to a specific installation target).
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

// ScopeUserToServerToken atomically persists every capability constraint of a
// scoped GitHub App user token. Nil permissions/repositoryIDs mean that
// dimension was omitted and remains unrestricted; an explicit empty map/slice
// means the caller intentionally scoped the token to no non-metadata
// permission or no repository.
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

// LookupUserToServerToken returns the token + bearing user, or nil if not found/expired.
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
	// One transaction: the access token and its refresh token are revoked
	// together, so a crash cannot leave one alive after the other (STORE-001/002).
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

// RotateUserToServerToken mints a fresh user-to-server token + refresh pair from a
// valid refresh token. Old token + refresh are revoked. Returns nil if refresh is invalid.
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
	// Revoke the matching user token (find by RefreshTokenValue). The deletes
	// write through to disk so the rotated-out credentials stay dead after a
	// restart instead of resurrecting from the persisted buckets, and commit in
	// one transaction so a crash cannot leave the old access token alive without
	// its refresh token (STORE-001/002). The replacement pair is then created in
	// its own transaction; a crash between the two only forces re-authentication,
	// never a live stale credential.
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

// RevokeUserGrant deletes every user-to-server + refresh token for (clientID, userID).
// Mirrors GitHub's DELETE /applications/{client_id}/grant.
func (st *Store) RevokeUserGrant(clientID string, userID int) int {
	st.Mu.Lock()
	defer st.Mu.Unlock()
	// One transaction: every access token and its refresh token for this grant
	// are revoked together, so a crash cannot leave part of a revoked grant alive
	// (STORE-001/002).
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
