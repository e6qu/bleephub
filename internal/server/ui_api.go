package bleephub

import (
	"errors"
	"net/http"
)

func (s *Server) registerUIAPIRoutes() {
	s.route("GET /ui-data/repos/{owner}/{repo}/commits", s.handleUIListCommits)
	s.route("GET /ui-data/repos/{owner}/{repo}/viewer", s.handleUIRepoViewer)

	// GitHub's web package views expose repository-associated packages and
	// version assets that its public REST API does not. Keep those browser
	// adapters outside /api/v3 so the REST namespace remains spec-exact.
	s.route("GET /ui-data/repos/{owner}/{repo}/packages", s.handleListRepoPackages)
	s.route("GET /ui-data/repos/{owner}/{repo}/packages/{package_type}/{package_name}", s.handleGetRepoPackage)
	s.route("DELETE /ui-data/repos/{owner}/{repo}/packages/{package_type}/{package_name}", s.handleDeleteRepoPackage)
	s.route("GET /ui-data/repos/{owner}/{repo}/packages/{package_type}/{package_name}/versions", s.handleListRepoPackageVersions)
	s.route("GET /ui-data/repos/{owner}/{repo}/packages/{package_type}/{package_name}/versions/{package_version_id}", s.handleGetRepoPackageVersion)
	s.route("DELETE /ui-data/repos/{owner}/{repo}/packages/{package_type}/{package_name}/versions/{package_version_id}", s.handleDeleteRepoPackageVersion)
	s.route("GET /ui-data/repos/{owner}/{repo}/packages/{package_type}/{package_name}/versions/{package_version_id}/files", s.handleListRepoPackageFiles)
	s.route("GET /ui-data/repos/{owner}/{repo}/packages/{package_type}/{package_name}/versions/{package_version_id}/files/{file_id}", s.handleDownloadRepoPackageFile)
	s.route("GET /ui-data/users/{username}/packages/{package_type}/{package_name}/versions/{package_version_id}/files", s.handleListUserPackageFiles)
	s.route("GET /ui-data/users/{username}/packages/{package_type}/{package_name}/versions/{package_version_id}/files/{file_id}", s.handleDownloadUserPackageFile)
	s.route("GET /ui-data/orgs/{org}/packages/{package_type}/{package_name}/versions/{package_version_id}/files", s.handleListOrgPackageFiles)
	s.route("GET /ui-data/orgs/{org}/packages/{package_type}/{package_name}/versions/{package_version_id}/files/{file_id}", s.handleDownloadOrgPackageFile)
}

// handleUIRepoViewer gives the browser one successful read for viewer-specific
// repository chrome. GitHub's public Star and Subscription existence checks
// correctly return 404 when absent; issuing those expected checks as page
// resources would still produce browser console errors. Mutations continue to
// use the public GitHub REST endpoints.
func (s *Server) handleUIRepoViewer(w http.ResponseWriter, r *http.Request) {
	ctx := s.authenticateRequest(r)
	user := ghUserFromContext(ctx)
	if user == nil {
		writeGHError(w, http.StatusUnauthorized, "Requires authentication")
		return
	}
	if suspended, _ := ctx.Value(ctxSuspendedUser).(bool); suspended {
		writeGHError(w, http.StatusForbidden, "This account has been suspended")
		return
	}

	owner := r.PathValue("owner")
	repoName := r.PathValue("repo")
	repo := s.store.GetRepo(owner, repoName)
	if repo == nil || (repo.Private && !s.viewerCanReadRepo(ctx, repo)) {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	subscription := s.store.GetRepoSubscription(user.ID, repo.ID)
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"starred":    s.store.IsRepoStarredBy(user.ID, owner, repoName),
		"subscribed": subscription != nil && subscription.Subscribed,
	})
}

func (s *Server) handleUIListCommits(w http.ResponseWriter, r *http.Request) {
	ctx := s.authenticateRequest(r)
	if ghUserFromContext(ctx) == nil && ghInstallationTokenFromContext(ctx) == nil && ghUserToServerTokenFromContext(ctx) == nil {
		writeGHError(w, http.StatusUnauthorized, "Requires authentication")
		return
	}
	r = r.WithContext(ctx)
	if suspended, _ := ctx.Value(ctxSuspendedInstallation).(bool); suspended {
		writeGHError(w, http.StatusForbidden, "This installation has been suspended")
		return
	}
	if suspended, _ := ctx.Value(ctxSuspendedUser).(bool); suspended {
		writeGHError(w, http.StatusForbidden, "This account has been suspended")
		return
	}

	owner := r.PathValue("owner")
	repoName := r.PathValue("repo")

	repo := s.store.GetRepo(owner, repoName)
	if repo == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}

	commits, err := s.listRepoCommits(repo, owner, repoName, "", s.baseURL(r))
	if err != nil {
		switch {
		case errors.Is(err, errRepoGitRepositoryEmpty):
			writeJSON(w, http.StatusOK, []map[string]interface{}{})
		case errors.Is(err, errRepoGitObjectUnavailable):
			writeGHError(w, http.StatusInternalServerError, "Git object unavailable")
		default:
			writeGHError(w, http.StatusInternalServerError, "Git storage unavailable")
		}
		return
	}
	writeJSON(w, http.StatusOK, paginateAndLink(w, r, commits))
}
