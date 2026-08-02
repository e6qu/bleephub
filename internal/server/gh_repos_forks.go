package bleephub

import (
	"net/http"
	"strconv"
)

func (s *Server) handleCreateFork(w http.ResponseWriter, r *http.Request) {
	user := ghUserFromContext(r.Context())
	if user == nil {
		writeGHError(w, http.StatusUnauthorized, "Bad credentials")
		return
	}

	owner := r.PathValue("owner")
	name := r.PathValue("repo")
	sourceRepo := s.store.GetRepo(owner, name)
	if sourceRepo == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}

	// GitHub allows forking public repos and private repos the user can read.
	if sourceRepo.Private && !s.viewerCanReadRepo(r.Context(), sourceRepo) {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}

	var req struct {
		Name          string `json:"name"`
		DefaultBranch string `json:"default_branch"`
		Organization  string `json:"organization"`
	}
	if !decodeJSONBody(w, r, &req) {
		return
	}

	forkName := req.Name
	if forkName == "" {
		forkName = sourceRepo.Name
	}

	// Only user-owned forks are supported in this slice; organization forks are
	// a future extension.
	if req.Organization != "" {
		writeGHError(w, http.StatusUnprocessableEntity, "Organization forks are not supported.")
		return
	}

	fork := s.store.ForkRepo(user, sourceRepo, forkName)
	if fork == nil {
		writeGHError(w, http.StatusUnprocessableEntity, "Fork failed.")
		return
	}

	s.recordAuditEvent("repo.fork", user.Login, "", map[string]interface{}{
		"source": sourceRepo.FullName,
		"fork":   fork.FullName,
	})
	writeJSON(w, http.StatusAccepted, fullRepoJSONForViewer(fork, s.store, s.baseURL(r), user))
}

func (s *Server) handleListForks(w http.ResponseWriter, r *http.Request) {
	owner := r.PathValue("owner")
	name := r.PathValue("repo")
	sourceRepo := s.store.GetRepo(owner, name)
	if sourceRepo == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}

	user := ghUserFromContext(r.Context())
	if sourceRepo.Private && !s.viewerCanReadRepo(r.Context(), sourceRepo) {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}

	sort := r.URL.Query().Get("sort")
	if sort == "" {
		sort = "newest"
	}

	opts := RepoListOptions{Sort: sort, Direction: "desc", NoPaginate: true}
	if raw := r.URL.Query().Get("per_page"); raw != "" {
		perPage, err := strconv.Atoi(raw)
		if err != nil {
			writeGHError(w, http.StatusBadRequest, "Invalid per_page parameter")
			return
		}
		if perPage > 0 {
			opts.PerPage = perPage
		}
	}
	if raw := r.URL.Query().Get("page"); raw != "" {
		page, err := strconv.Atoi(raw)
		if err != nil {
			writeGHError(w, http.StatusBadRequest, "Invalid page parameter")
			return
		}
		if page > 0 {
			opts.Page = page
		}
	}

	forks := s.store.ListForks(sourceRepo.ID, opts)
	result := make([]map[string]interface{}, 0, len(forks))
	base := s.baseURL(r)
	for _, fork := range forks {
		result = append(result, repoToJSONForViewer(fork, s.store, base, user))
	}
	writeJSON(w, http.StatusOK, paginateAndLink(w, r, result))
}
