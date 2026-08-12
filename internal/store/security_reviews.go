package store

import (
	"sort"
	"strings"
	"time"
)

type SecurityReviewRequest struct {
	ID               int                      `json:"id"`
	Number           int                      `json:"number"`
	RepoKey          string                   `json:"repo_key"`
	OrgLogin         string                   `json:"org_login"`
	Kind             string                   `json:"kind"`
	RequesterID      int                      `json:"requester_id"`
	ResourceID       string                   `json:"resource_identifier"`
	Status           string                   `json:"status"`
	RequesterComment *string                  `json:"requester_comment"`
	Data             []map[string]interface{} `json:"data"`
	Responses        []SecurityReviewResponse `json:"responses"`
	ExpiresAt        time.Time                `json:"expires_at"`
	CreatedAt        time.Time                `json:"created_at"`
}

func (st *Store) ListSecurityReviewRequests(repoKey, orgLogin, kind string) []*SecurityReviewRequest {
	st.Mu.RLock()
	defer st.Mu.RUnlock()
	result := []*SecurityReviewRequest{}
	for scope, requests := range st.SecurityReviewRequests {
		scopeRepo, scopeKind, ok := strings.Cut(scope, "|")
		if !ok || scopeKind != kind || (repoKey != "" && scopeRepo != repoKey) {
			continue
		}
		for _, request := range requests {
			if orgLogin == "" || request.OrgLogin == orgLogin {
				result = append(result, CopySecurityReviewRequest(request))
			}
		}
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].CreatedAt.Equal(result[j].CreatedAt) {
			return result[i].ID > result[j].ID
		}
		return result[i].CreatedAt.After(result[j].CreatedAt)
	})
	return result
}

func CopySecurityReviewRequest(request *SecurityReviewRequest) *SecurityReviewRequest {
	if request == nil {
		return nil
	}
	result := *request
	result.Data = append([]map[string]interface{}(nil), request.Data...)
	result.Responses = append([]SecurityReviewResponse(nil), request.Responses...)
	return &result
}

type SecurityReviewResponse struct {
	ID         int       `json:"id"`
	ReviewerID int       `json:"reviewer_id"`
	Message    string    `json:"message"`
	Status     string    `json:"status"`
	CreatedAt  time.Time `json:"created_at"`
}
