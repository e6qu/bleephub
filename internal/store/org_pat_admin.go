package store

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"time"
)

// OrgPATPermissions groups the permissions a fine-grained PAT requested,
// mirroring the organization-programmatic-access-grant permissions shape.
type OrgPATPermissions struct {
	Organization map[string]string `json:"organization,omitempty"`
	Repository   map[string]string `json:"repository,omitempty"`
	Other        map[string]string `json:"other,omitempty"`
}

// OrgPATGrantRequest is a pending request for organization access via a
// fine-grained personal access token.
type OrgPATGrantRequest struct {
	ID                  int               `json:"id"`
	OrgLogin            string            `json:"org_login"`
	OwnerUserID         int               `json:"owner_user_id"`
	TokenID             int               `json:"token_id"`
	TokenName           string            `json:"token_name"`
	TokenValue          string            `json:"-"`
	Reason              *string           `json:"reason"`
	RepositorySelection string            `json:"repository_selection"` // none | all | subset
	RepositoryIDs       []int             `json:"repository_ids,omitempty"`
	Permissions         OrgPATPermissions `json:"permissions"`
	TokenExpiresAt      *time.Time        `json:"token_expires_at"`
	CreatedAt           time.Time         `json:"created_at"`
}

// OrgPATGrant is an approved fine-grained personal access token grant.
type OrgPATGrant struct {
	ID                  int               `json:"id"`
	OrgLogin            string            `json:"org_login"`
	OwnerUserID         int               `json:"owner_user_id"`
	TokenID             int               `json:"token_id"`
	TokenName           string            `json:"token_name"`
	TokenValue          string            `json:"-"`
	RepositorySelection string            `json:"repository_selection"`
	RepositoryIDs       []int             `json:"repository_ids,omitempty"`
	Permissions         OrgPATPermissions `json:"permissions"`
	TokenExpiresAt      *time.Time        `json:"token_expires_at"`
	AccessGrantedAt     time.Time         `json:"access_granted_at"`
}

func (st *Store) persistOrgPATGrantRequestsLocked(orgLogin string) {
	if st.Persist == nil {
		return
	}
	if m := st.OrgPATGrantRequests[orgLogin]; len(m) > 0 {
		st.Persist.MustPut("org_pat_grant_requests", orgLogin, m)
	} else {
		st.Persist.MustDelete("org_pat_grant_requests", orgLogin)
	}
}

func (st *Store) persistOrgPATGrantsLocked(orgLogin string) {
	if st.Persist == nil {
		return
	}
	if m := st.OrgPATGrants[orgLogin]; len(m) > 0 {
		st.Persist.MustPut("org_pat_grants", orgLogin, m)
	} else {
		st.Persist.MustDelete("org_pat_grants", orgLogin)
	}
}

// CreateOrgPATGrantRequest mints a real fine-grained token for the user and
// files the pending grant request that references it.
func (st *Store) CreateOrgPATGrantRequest(orgLogin string, ownerUserID int, tokenName string, reason *string, repositorySelection string, repositoryIDs []int, perms OrgPATPermissions, expiresAt *time.Time) (*OrgPATGrantRequest, error) {
	return st.createOrgPATGrantRequestWithRandom(orgLogin, ownerUserID, tokenName, reason, repositorySelection, repositoryIDs, perms, expiresAt, rand.Reader)
}

func (st *Store) createOrgPATGrantRequestWithRandom(orgLogin string, ownerUserID int, tokenName string, reason *string, repositorySelection string, repositoryIDs []int, perms OrgPATPermissions, expiresAt *time.Time, random io.Reader) (*OrgPATGrantRequest, error) {
	st.Mu.Lock()
	defer st.Mu.Unlock()

	value, err := NewFineGrainedPATTokenFromReader(random)
	if err != nil {
		return nil, fmt.Errorf("generate fine-grained token: %w", err)
	}
	now := time.Now().UTC()
	tok := &Token{
		Value: value, UserID: ownerUserID, CreatedAt: now, FineGrained: true,
		FineGrainedID: st.NextPATTokenID, Name: tokenName, ResourceOwner: orgLogin,
		RepositorySelection: repositorySelection, RepositoryIDs: append([]int(nil), repositoryIDs...),
		Permissions: perms, ExpiresAt: expiresAt,
	}
	st.Tokens[st.tokenMapKey(value)] = tok
	st.PersistTokenLocked(tok)

	req := &OrgPATGrantRequest{
		ID:                  st.NextPATRequestID,
		OrgLogin:            orgLogin,
		OwnerUserID:         ownerUserID,
		TokenID:             st.NextPATTokenID,
		TokenName:           tokenName,
		TokenValue:          value,
		Reason:              reason,
		RepositorySelection: repositorySelection,
		RepositoryIDs:       repositoryIDs,
		Permissions:         perms,
		TokenExpiresAt:      expiresAt,
		CreatedAt:           now,
	}
	st.NextPATRequestID++
	st.NextPATTokenID++
	if st.OrgPATGrantRequests[orgLogin] == nil {
		st.OrgPATGrantRequests[orgLogin] = map[int]*OrgPATGrantRequest{}
	}
	st.OrgPATGrantRequests[orgLogin][req.ID] = req
	st.persistOrgPATGrantRequestsLocked(orgLogin)
	return req, nil
}

// ReviewOrgPATGrantRequest resolves a pending request: approve converts it
// into an active grant, deny removes it. Returns false when the request
// does not exist.
func (st *Store) ReviewOrgPATGrantRequest(orgLogin string, requestID int, approve bool) bool {
	st.Mu.Lock()
	defer st.Mu.Unlock()

	req := st.OrgPATGrantRequests[orgLogin][requestID]
	if req == nil {
		return false
	}
	delete(st.OrgPATGrantRequests[orgLogin], requestID)
	st.persistOrgPATGrantRequestsLocked(orgLogin)

	if approve {
		grant := &OrgPATGrant{
			ID:                  st.NextPATGrantID,
			OrgLogin:            orgLogin,
			OwnerUserID:         req.OwnerUserID,
			TokenID:             req.TokenID,
			TokenName:           req.TokenName,
			TokenValue:          req.TokenValue,
			RepositorySelection: req.RepositorySelection,
			RepositoryIDs:       req.RepositoryIDs,
			Permissions:         req.Permissions,
			TokenExpiresAt:      req.TokenExpiresAt,
			AccessGrantedAt:     time.Now().UTC(),
		}
		st.NextPATGrantID++
		if st.OrgPATGrants[orgLogin] == nil {
			st.OrgPATGrants[orgLogin] = map[int]*OrgPATGrant{}
		}
		st.OrgPATGrants[orgLogin][grant.ID] = grant
		st.persistOrgPATGrantsLocked(orgLogin)
	}
	return true
}

// RevokeOrgPATGrant removes an active grant. Returns false when it does not
// exist.
func (st *Store) RevokeOrgPATGrant(orgLogin string, grantID int) bool {
	st.Mu.Lock()
	defer st.Mu.Unlock()
	if st.OrgPATGrants[orgLogin][grantID] == nil {
		return false
	}
	delete(st.OrgPATGrants[orgLogin], grantID)
	st.persistOrgPATGrantsLocked(orgLogin)
	return true
}

// GetOrgPATGrantRequest returns a pending grant request by ID, or nil.
func (st *Store) GetOrgPATGrantRequest(orgLogin string, id int) *OrgPATGrantRequest {
	st.Mu.RLock()
	defer st.Mu.RUnlock()
	return st.OrgPATGrantRequests[orgLogin][id]
}

// GetOrgPATGrant returns an active grant by ID, or nil.
func (st *Store) GetOrgPATGrant(orgLogin string, id int) *OrgPATGrant {
	st.Mu.RLock()
	defer st.Mu.RUnlock()
	return st.OrgPATGrants[orgLogin][id]
}

func NewFineGrainedPATTokenFromReader(random io.Reader) (string, error) {
	buf := make([]byte, 20)
	if _, err := io.ReadFull(random, buf); err != nil {
		return "", fmt.Errorf("fine-grained personal access token: %w", err)
	}
	return "github_pat_" + hex.EncodeToString(buf), nil
}
