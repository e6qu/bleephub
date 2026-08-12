package store

import (
	"crypto/rand"
	"time"
)

func (st *Store) CreateUserFineGrainedPAT(userID int, body CreatePersonalAccessTokenWebRequest) (*Token, error) {
	value, err := NewFineGrainedPATTokenFromReader(rand.Reader)
	if err != nil {
		return nil, err
	}
	st.Mu.Lock()
	defer st.Mu.Unlock()
	token := &Token{Value: value, UserID: userID, CreatedAt: time.Now().UTC(), FineGrained: true, FineGrainedID: st.NextPATTokenID, Name: body.Name, ResourceOwner: body.ResourceOwner, RepositorySelection: body.RepositorySelection, RepositoryIDs: append([]int(nil), body.RepositoryIDs...), Permissions: body.Permissions, ExpiresAt: body.ExpiresAt}
	st.NextPATTokenID++
	st.Tokens[st.tokenMapKey(value)] = token
	st.PersistTokenLocked(token)
	return token, nil
}

func (st *Store) CountFineGrainedPATs(userID int) int {
	st.Mu.RLock()
	defer st.Mu.RUnlock()
	count := 0
	for _, token := range st.Tokens {
		if token.UserID == userID && token.FineGrained {
			count++
		}
	}
	return count
}

func (st *Store) DeleteFineGrainedPAT(userID, tokenID int) bool {
	st.Mu.Lock()
	defer st.Mu.Unlock()
	value := ""
	for candidate, token := range st.Tokens {
		if token.UserID == userID && token.FineGrained && token.FineGrainedID == tokenID {
			value = candidate
			break
		}
	}
	if value == "" {
		return false
	}
	st.DeleteTokenMapKeyLocked(value)
	for org, requests := range st.OrgPATGrantRequests {
		for id, request := range requests {
			if request.TokenID == tokenID {
				delete(requests, id)
				st.persistOrgPATGrantRequestsLocked(org)
			}
		}
	}
	for org, grants := range st.OrgPATGrants {
		for id, grant := range grants {
			if grant.TokenID == tokenID {
				delete(grants, id)
				st.persistOrgPATGrantsLocked(org)
			}
		}
	}
	return true
}

type CreatePersonalAccessTokenWebRequest struct {
	Name                string            `json:"name"`
	ResourceOwner       string            `json:"resource_owner"`
	RepositorySelection string            `json:"repository_selection"`
	RepositoryIDs       []int             `json:"repository_ids"`
	Permissions         OrgPATPermissions `json:"permissions"`
	ExpiresAt           *time.Time        `json:"expires_at"`
	Reason              *string           `json:"reason"`
}
