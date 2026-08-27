package bleephub

import (
	"encoding/json"
	"net/http"
	"strings"
)

// Profile README (the <login>/<login> repo for users, <org>/.github for orgs).
// A missing profile repo is the common case; answer 200 with a null readme
// rather than a 404 the SPA would log as a console error, delegating to the
// real readme handler otherwise so the payload stays byte-identical.
func (s *Server) registerGHProfileReadmeRoutes() {
	s.route("GET /ui-data/users/{login}/profile-readme", s.handleGetProfileReadme)
}

func (s *Server) handleGetProfileReadme(w http.ResponseWriter, r *http.Request) {
	login := r.PathValue("login")
	repoName := login
	if s.store.LookupUserByLogin(login) == nil {
		if s.store.GetOrg(login) == nil {
			writeGHError(w, http.StatusNotFound, "Not Found")
			return
		}
		repoName = ".github"
	}
	var readme json.RawMessage
	// Build the sub-request path from the store-owned FullName, not the
	// request-derived login: the store lookup is the request-data boundary.
	if repo := s.store.GetRepoByFullName(login + "/" + repoName); repo != nil {
		ownerPart, namePart, _ := strings.Cut(repo.FullName, "/")
		sub := uiSubGET(r, s.handleGetReadme, "/api/v3/repos/"+repo.FullName+"/readme", nil, map[string]string{
			"owner": ownerPart,
			"repo":  namePart,
		})
		if sub.status == http.StatusOK {
			readme = json.RawMessage(sub.buf.Bytes())
		}
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"readme": readme})
}
