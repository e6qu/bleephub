package bleephub

import (
	"net/http"
	"strconv"
	"time"

	"github.com/e6qu/bleephub/internal/store"
)

func (s *Server) registerGHRepoInvitationRoutes() {
	s.route("GET /api/v3/repos/{owner}/{repo}/invitations", s.requirePerm(store.ScopeAdministration, store.PermWrite, s.handleListRepoInvitations))
	s.route("PATCH /api/v3/repos/{owner}/{repo}/invitations/{invitation_id}", s.requirePerm(store.ScopeAdministration, store.PermWrite, s.handleUpdateRepoInvitation))
	s.route("DELETE /api/v3/repos/{owner}/{repo}/invitations/{invitation_id}", s.requirePerm(store.ScopeAdministration, store.PermWrite, s.handleCancelRepoInvitation))
	s.route("GET /api/v3/user/repository_invitations", s.handleListUserRepoInvitations)
	s.route("PATCH /api/v3/user/repository_invitations/{invitation_id}", s.handleAcceptRepoInvitation)
	s.route("DELETE /api/v3/user/repository_invitations/{invitation_id}", s.handleDeclineRepoInvitation)
}

func (s *Server) handleListRepoInvitations(w http.ResponseWriter, r *http.Request) {
	owner := r.PathValue("owner")
	name := r.PathValue("repo")
	repo := s.store.GetRepo(owner, name)
	if repo == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	if !s.viewerCanAdminRepo(r.Context(), repo) {
		writeGHError(w, http.StatusForbidden, "Must have admin rights to Repository.")
		return
	}

	invitations := s.store.ListPendingRepoInvitations(repo.FullName)
	out := make([]map[string]interface{}, 0, len(invitations))
	base := s.baseURL(r)
	for _, inv := range invitations {
		out = append(out, invitationJSON(inv, repo, s.store, base))
	}
	writeJSON(w, http.StatusOK, paginateAndLink(w, r, out))
}

func (s *Server) handleUpdateRepoInvitation(w http.ResponseWriter, r *http.Request) {
	owner := r.PathValue("owner")
	name := r.PathValue("repo")
	repo := s.store.GetRepo(owner, name)
	if repo == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	if !s.viewerCanAdminRepo(r.Context(), repo) {
		writeGHError(w, http.StatusForbidden, "Must have admin rights to Repository.")
		return
	}

	id, err := strconv.Atoi(r.PathValue("invitation_id"))
	if err != nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}

	var req struct {
		Permissions string `json:"permissions"`
	}
	if !decodeJSONBody(w, r, &req) {
		return
	}
	if req.Permissions == "" {
		store.WriteGHValidationError(w, "RepositoryInvitation", "permissions", "missing_field")
		return
	}

	updated := s.store.UpdateRepoInvitation(repo.FullName, id, req.Permissions)
	if updated == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	writeJSON(w, http.StatusOK, invitationJSON(updated, repo, s.store, s.baseURL(r)))
}

func (s *Server) handleCancelRepoInvitation(w http.ResponseWriter, r *http.Request) {
	owner := r.PathValue("owner")
	name := r.PathValue("repo")
	repo := s.store.GetRepo(owner, name)
	if repo == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	if !s.viewerCanAdminRepo(r.Context(), repo) {
		writeGHError(w, http.StatusForbidden, "Must have admin rights to Repository.")
		return
	}

	id, err := strconv.Atoi(r.PathValue("invitation_id"))
	if err != nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	if !s.store.DeleteRepoInvitation(repo.FullName, id) {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleListUserRepoInvitations(w http.ResponseWriter, r *http.Request) {
	user := ghUserFromContext(r.Context())
	if user == nil {
		writeGHError(w, http.StatusUnauthorized, "Bad credentials")
		return
	}

	invitations := s.store.ListUserRepoInvitations(user)
	out := make([]map[string]interface{}, 0, len(invitations))
	base := s.baseURL(r)
	for _, inv := range invitations {
		if repo := s.store.GetRepoByFullName(inv.RepoKey); repo != nil {
			out = append(out, invitationJSON(inv, repo, s.store, base))
		}
	}
	writeJSON(w, http.StatusOK, paginateAndLink(w, r, out))
}

func (s *Server) handleAcceptRepoInvitation(w http.ResponseWriter, r *http.Request) {
	user := ghUserFromContext(r.Context())
	if user == nil {
		writeGHError(w, http.StatusUnauthorized, "Bad credentials")
		return
	}

	id, err := strconv.Atoi(r.PathValue("invitation_id"))
	if err != nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	repoKey, ok := s.store.AcceptRepoInvitation(id, user)
	if !ok {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	// Accepting an invitation makes the user a collaborator, which fires GitHub's
	// `member` event (action "added") so `on: member` workflows run (ACT-026).
	if repo := s.store.GetRepoByFullName(repoKey); repo != nil {
		s.emitWebhookEvent(repoKey, "member", "added", map[string]interface{}{
			"action":     "added",
			"member":     store.UserToJSON(user, s.baseURL(r)),
			"repository": repoPayload(repo, s.baseURL(r)),
			"sender":     store.UserToJSON(user, s.baseURL(r)),
		})
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleDeclineRepoInvitation(w http.ResponseWriter, r *http.Request) {
	user := ghUserFromContext(r.Context())
	if user == nil {
		writeGHError(w, http.StatusUnauthorized, "Bad credentials")
		return
	}

	id, err := strconv.Atoi(r.PathValue("invitation_id"))
	if err != nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	if !s.store.DeclineRepoInvitation(id, user) {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func invitationJSON(inv *store.RepoInvitation, repo *store.Repo, st *store.Store, baseURL string) map[string]interface{} {
	invitee := map[string]interface{}(nil)
	if inv.InviteeLogin != "" {
		if u := st.LookupUserByLogin(inv.InviteeLogin); u != nil {
			invitee = store.UserToJSON(u, baseURL)
		}
	}
	inviter := map[string]interface{}(nil)
	if u := st.GetUserByID(inv.InviterID); u != nil {
		inviter = store.UserToJSON(u, baseURL)
	}
	perm := inv.Permissions
	if perm == "" {
		perm = "pull"
	}
	roleName := githubRoleName(perm)

	return map[string]interface{}{
		"id":          inv.ID,
		"node_id":     inv.NodeID,
		"repository":  store.RepoToJSON(repo, st, baseURL),
		"invitee":     invitee,
		"inviter":     inviter,
		"permissions": roleName,
		"created_at":  inv.CreatedAt.Format(time.RFC3339),
		"url":         baseURL + "/user/repository-invitations/" + strconv.Itoa(inv.ID),
		"html_url":    baseURL + "/" + inv.RepoKey + "/invitations",
		"expired":     false,
	}
}

// GetRepoByFullName lives with the other repository accessors in
// store_repos.go.
