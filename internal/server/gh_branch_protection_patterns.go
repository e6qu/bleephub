package bleephub

import (
	"net/http"
	"strings"

	"github.com/e6qu/bleephub/internal/store"
)

// Branch protection pattern rules are a github.com web-settings concept:
// GitHub's REST branch protection API forbids wildcards, so these routes
// live under the browser-only /ui-data namespace rather than /api/v3 —
// inventing a GitHub-namespaced path is a defect the route-definition tests
// reject. Each rule pairs an fnmatch pattern (`*` stays within a path
// segment, `**` crosses segments) with a protection config in the same
// shape the REST PUT /protection body accepts. The enforcement chokepoint
// consults an exact-name rule first and falls back to the first matching
// pattern rule (effectiveBranchProtectionFor).
// (`s.route` auto-wraps /ui-data patterns with authenticateUIData.)
func (s *Server) registerGHBranchProtectionPatternRoutes() {
	s.route("GET /ui-data/repos/{owner}/{repo}/branch-protection-patterns", s.handleBPPatternsGet)
	s.route("PUT /ui-data/repos/{owner}/{repo}/branch-protection-patterns", s.handleBPPatternsPut)
	s.route("DELETE /ui-data/repos/{owner}/{repo}/branch-protection-patterns", s.handleBPPatternsDelete)
}

// bpPatternsRepoForAdmin resolves the repo and enforces admin access —
// branch protection is repository administration, reads included (the REST
// protection surface requires the administration scope for GET too).
func (s *Server) bpPatternsRepoForAdmin(w http.ResponseWriter, r *http.Request) *store.Repo {
	repo := s.store.GetRepo(r.PathValue("owner"), r.PathValue("repo"))
	if repo == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return nil
	}
	// A private repo the viewer cannot read must not leak existence.
	if repo.Private && !s.viewerCanReadRepo(r.Context(), repo) {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return nil
	}
	if !s.viewerCanAdminRepo(r.Context(), repo) {
		writeGHError(w, http.StatusForbidden, "Must have admin rights to Repository.")
		return nil
	}
	return repo
}

func bpPatternRulesJSON(rules []*store.BranchProtectionPatternRule) []map[string]interface{} {
	out := make([]map[string]interface{}, 0, len(rules))
	for _, rule := range rules {
		out = append(out, map[string]interface{}{
			"pattern":    rule.Pattern,
			"protection": rule.Protection,
		})
	}
	return out
}

func (s *Server) handleBPPatternsGet(w http.ResponseWriter, r *http.Request) {
	repo := s.bpPatternsRepoForAdmin(w, r)
	if repo == nil {
		return
	}
	writeJSON(w, http.StatusOK, bpPatternRulesJSON(s.store.ListBranchProtectionPatterns(repo.ID)))
}

// handleBPPatternsPut replaces the repository's ordered rule list. The body
// is a JSON array of {pattern, protection} where protection carries the same
// members as the REST PUT /protection body.
func (s *Server) handleBPPatternsPut(w http.ResponseWriter, r *http.Request) {
	repo := s.bpPatternsRepoForAdmin(w, r)
	if repo == nil {
		return
	}
	var req []struct {
		Pattern    string     `json:"pattern"`
		Protection *bpRequest `json:"protection"`
	}
	if !decodeJSONBody(w, r, &req) {
		return
	}
	rules := make([]*store.BranchProtectionPatternRule, 0, len(req))
	for _, entry := range req {
		if strings.TrimSpace(entry.Pattern) == "" {
			writeGHValidationErrorSimple(w, "pattern is required for every rule")
			return
		}
		protection := &store.BranchProtection{}
		if entry.Protection != nil {
			protection = s.applyBranchProtectionRequest(protection, entry.Protection)
		}
		if !protection.IsProtected() {
			writeGHValidationErrorSimple(w, "rule "+entry.Pattern+" enables no protection")
			return
		}
		rules = append(rules, &store.BranchProtectionPatternRule{
			Pattern:    entry.Pattern,
			Protection: protection,
		})
	}
	s.store.SetBranchProtectionPatterns(repo.ID, rules)
	// A protection state change can be the event an armed auto-merge was
	// waiting for; re-evaluate the repository's open pull requests.
	s.maybeAutoMergeRepo(repo)
	writeJSON(w, http.StatusOK, bpPatternRulesJSON(s.store.ListBranchProtectionPatterns(repo.ID)))
}

func (s *Server) handleBPPatternsDelete(w http.ResponseWriter, r *http.Request) {
	repo := s.bpPatternsRepoForAdmin(w, r)
	if repo == nil {
		return
	}
	s.store.SetBranchProtectionPatterns(repo.ID, nil)
	s.maybeAutoMergeRepo(repo)
	w.WriteHeader(http.StatusNoContent)
}
