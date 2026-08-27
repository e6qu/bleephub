package bleephub

import (
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/e6qu/bleephub/internal/store"
)

// Dependency graph: snapshot submission, the SPDX SBOM export, and the
// dependency diff. Snapshots are the source of truth — the SBOM and compare
// diff are computed from them, never invented.

func (s *Server) registerGHDependencyGraphRoutes() {
	s.route("POST /api/v3/repos/{owner}/{repo}/dependency-graph/snapshots",
		s.requirePerm(store.ScopeContents, store.PermWrite, s.handleCreateDependencySnapshot))
	s.route("GET /api/v3/repos/{owner}/{repo}/dependency-graph/sbom", s.handleGetDependencySBOM)
	s.route("GET /api/v3/repos/{owner}/{repo}/dependency-graph/sbom/generate-report", s.handleGenerateSBOMReport)
	s.route("GET /api/v3/repos/{owner}/{repo}/dependency-graph/sbom/fetch-report/{sbom_uuid}", s.handleFetchSBOMReport)
	s.route("GET /api/v3/repos/{owner}/{repo}/dependency-graph/compare/{basehead}", s.handleDependencyGraphCompare)
}

func (s *Server) handleCreateDependencySnapshot(w http.ResponseWriter, r *http.Request) {
	repo := s.lookupRepoFromPath(r)
	if repo == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	var snap store.DependencySnapshot
	if !decodeJSONBody(w, r, &snap) {
		return
	}
	snap.RepoID = repo.ID

	if msg := validateDependencySnapshot(&snap); msg != "" {
		writeGHError(w, http.StatusUnprocessableEntity, msg)
		return
	}

	created := func(result, message string) {
		snap.Result = result
		stored := s.store.AddDependencySnapshot(&snap)
		if result == "SUCCESS" {
			s.deriveDependabotAlertsForRepository(repo)
			// Deriving only ever adds; reconcile fixes alerts whose vulnerable
			// version this submission dropped and reintroduces ones it brought
			// back.
			s.reconcileDependabotAlerts(repo)
		}
		writeJSON(w, http.StatusCreated, map[string]interface{}{
			"id":         stored.ID,
			"created_at": stored.CreatedAt.Format(time.RFC3339),
			"result":     result,
			"message":    message,
		})
	}

	if snap.Ref == "refs/heads/"+repo.DefaultBranch {
		created("SUCCESS", "Dependency results for the repo have been successfully updated.")
		return
	}
	created("ACCEPTED", "Snapshot accepted, but the repo's dependencies were not updated because the ref is not the default branch.")
}

// validateDependencySnapshot returns a message for the first problem, or "".
func validateDependencySnapshot(snap *store.DependencySnapshot) string {
	switch {
	case snap.Version == 0 && snap.Detector.Name == "" && snap.Job.ID == "":
		return "snapshot is empty"
	case snap.Detector.Name == "" || snap.Detector.Version == "" || snap.Detector.URL == "":
		return "detector name, version, and url are required"
	case snap.Job.ID == "" || snap.Job.Correlator == "":
		return "job id and correlator are required"
	case snap.Ref == "" || !strings.HasPrefix(snap.Ref, "refs/"):
		return "ref must be a fully qualified git ref (refs/...)"
	case len(snap.Sha) != 40 || !isHexString(snap.Sha):
		return "sha must be a 40-character commit SHA"
	case snap.Scanned == "":
		return "scanned timestamp is required"
	}
	if _, err := time.Parse(time.RFC3339, snap.Scanned); err != nil {
		return "scanned must be an ISO 8601 timestamp"
	}
	for key, manifest := range snap.Manifests {
		if manifest == nil {
			return "manifest " + key + " must be an object"
		}
		for purl, dep := range manifest.Resolved {
			if dep == nil {
				return "manifest " + key + " has a null resolved entry for " + purl
			}
			if dep.PackageURL == "" || !strings.HasPrefix(dep.PackageURL, "pkg:") {
				return "resolved dependency package_url must be a package-url (pkg:...)"
			}
			switch dep.Relationship {
			case "", "direct", "indirect":
			default:
				return "resolved dependency relationship must be direct or indirect"
			}
			switch dep.Scope {
			case "", "runtime", "development":
			default:
				return "resolved dependency scope must be runtime or development"
			}
		}
	}
	return ""
}

func isHexString(s string) bool {
	for _, c := range s {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return len(s) > 0
}

// currentDependencies returns the dependency set for a ref+sha. Per
// (job.correlator, detector.name) only the latest snapshot counts — the
// submission API's replacement semantics. Matches by sha when given, else ref.
func (s *Server) currentDependencies(repoID int, ref, sha string) map[string]*dependencyEntry {
	latest := map[string]*store.DependencySnapshot{}
	for _, snap := range s.store.ListDependencySnapshots(repoID) {
		if snap.Result == "INVALID" {
			continue
		}
		if sha != "" && snap.Sha != sha {
			continue
		}
		if sha == "" && snap.Ref != ref {
			continue
		}
		key := snap.Job.Correlator + "\x1f" + snap.Detector.Name
		if cur, ok := latest[key]; !ok || snap.ID > cur.ID {
			latest[key] = snap
		}
	}
	deps := map[string]*dependencyEntry{}
	for _, snap := range latest {
		for _, manifest := range snap.Manifests {
			for _, dep := range manifest.Resolved {
				if dep.PackageURL == "" {
					continue
				}
				deps[dep.PackageURL] = &dependencyEntry{
					PackageURL: dep.PackageURL,
					Manifest:   manifest.Name,
					Scope:      dep.Scope,
				}
			}
		}
	}
	return deps
}

type dependencyEntry struct {
	PackageURL string
	Manifest   string
	Scope      string
}

func parsePurl(purl string) (ecosystem, name, version string) {
	rest := strings.TrimPrefix(purl, "pkg:")
	rest = strings.TrimPrefix(rest, "/")
	if i := strings.IndexByte(rest, '/'); i >= 0 {
		ecosystem = rest[:i]
		rest = rest[i+1:]
	} else {
		ecosystem = rest
		rest = ""
	}
	if i := strings.LastIndexByte(rest, '@'); i >= 0 {
		version = rest[i+1:]
		rest = rest[:i]
	}
	if decoded, err := url.PathUnescape(rest); err == nil {
		rest = decoded
	}
	name = rest
	return ecosystem, name, version
}

func (s *Server) handleGetDependencySBOM(w http.ResponseWriter, r *http.Request) {
	repo := s.lookupReadableRepoFromPath(w, r)
	if repo == nil {
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"sbom": s.buildSPDXSBOM(repo, s.baseURL(r))})
}

// buildSPDXSBOM produces an SPDX 2.3 document from the repository's recorded
// default-branch dependencies; with none it describes only the repo package.
func (s *Server) buildSPDXSBOM(repo *store.Repo, baseURL string) map[string]interface{} {
	docName := "com.github." + repo.FullName
	repoSPDXID := "SPDXRef-com.github." + strings.ReplaceAll(repo.FullName, "/", "-")

	owner, name, _ := store.SplitRepoFullName(repo.FullName)
	headSha := store.ResolveBranchSha(s.store.GetGitStorage(owner, name), repo.DefaultBranch)
	versionInfo := repo.DefaultBranch
	if headSha != "" {
		versionInfo = headSha
	}

	packages := []map[string]interface{}{{
		"SPDXID":           repoSPDXID,
		"name":             docName,
		"versionInfo":      versionInfo,
		"downloadLocation": "git+" + baseURL + "/" + repo.FullName,
		"filesAnalyzed":    false,
	}}
	relationships := []map[string]interface{}{{
		"relationshipType":   "DESCRIBES",
		"spdxElementId":      "SPDXRef-DOCUMENT",
		"relatedSpdxElement": repoSPDXID,
	}}

	deps := s.currentDependencies(repo.ID, "refs/heads/"+repo.DefaultBranch, "")
	purls := make([]string, 0, len(deps))
	for purl := range deps {
		purls = append(purls, purl)
	}
	sort.Strings(purls)
	for _, purl := range purls {
		ecosystem, depName, version := parsePurl(purl)
		spdxID := "SPDXRef-" + sanitizeSPDXIDPart(ecosystem+"-"+depName+"-"+version)
		packages = append(packages, map[string]interface{}{
			"SPDXID":           spdxID,
			"name":             ecosystem + ":" + depName,
			"versionInfo":      version,
			"downloadLocation": "NOASSERTION",
			"filesAnalyzed":    false,
			"externalRefs": []map[string]interface{}{{
				"referenceCategory": "PACKAGE-MANAGER",
				"referenceLocator":  purl,
				"referenceType":     "purl",
			}},
		})
		relationships = append(relationships, map[string]interface{}{
			"relationshipType":   "DEPENDS_ON",
			"spdxElementId":      repoSPDXID,
			"relatedSpdxElement": spdxID,
		})
	}

	return map[string]interface{}{
		"SPDXID":      "SPDXRef-DOCUMENT",
		"spdxVersion": "SPDX-2.3",
		"creationInfo": map[string]interface{}{
			"created":  time.Now().UTC().Format(time.RFC3339),
			"creators": []string{"Tool: bleephub-dependency-graph"},
		},
		"name":              docName,
		"dataLicense":       "CC0-1.0",
		"documentNamespace": baseURL + "/" + repo.FullName + "/dependency_graph/sbom",
		"packages":          packages,
		"relationships":     relationships,
	}
}

// sanitizeSPDXIDPart keeps only the characters an SPDXID idstring allows
// (letters, digits, '.', '-').
func sanitizeSPDXIDPart(s string) string {
	var b strings.Builder
	for _, c := range s {
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '.', c == '-':
			b.WriteRune(c)
		default:
			b.WriteByte('-')
		}
	}
	return b.String()
}

func (s *Server) handleGenerateSBOMReport(w http.ResponseWriter, r *http.Request) {
	repo := s.lookupReadableRepoFromPath(w, r)
	if repo == nil {
		return
	}
	exp := s.store.AddSBOMExport(repo.ID)
	writeJSON(w, http.StatusCreated, map[string]interface{}{
		"sbom_url": s.baseURL(r) + "/api/v3/repos/" + repo.FullName + "/dependency-graph/sbom/fetch-report/" + exp.UUID,
	})
}

func (s *Server) handleFetchSBOMReport(w http.ResponseWriter, r *http.Request) {
	repo := s.lookupReadableRepoFromPath(w, r)
	if repo == nil {
		return
	}
	exp := s.store.GetSBOMExport(r.PathValue("sbom_uuid"))
	if exp == nil || exp.RepoID != repo.ID {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	// As on real GitHub, the fetch step redirects to the SBOM endpoint.
	http.Redirect(w, r, s.baseURL(r)+"/api/v3/repos/"+repo.FullName+"/dependency-graph/sbom", http.StatusFound)
}

func (s *Server) handleDependencyGraphCompare(w http.ResponseWriter, r *http.Request) {
	repo := s.lookupReadableRepoFromPath(w, r)
	if repo == nil {
		return
	}
	// An enterprise can withhold dependency insights from its members.
	policy, enterprise := s.enterprisePolicyForRepo(repo)
	if s.refuseByEnterprisePolicy(w, r, enterprise, policy.MembersCanViewDependencyInsights,
		"Dependency insights are disabled by an enterprise policy.") {
		return
	}
	basehead := r.PathValue("basehead")
	base, head, found := strings.Cut(basehead, "...")
	if !found || base == "" || head == "" {
		writeGHError(w, http.StatusBadRequest, "basehead must be in the form BASE...HEAD")
		return
	}

	baseDeps, baseOK := s.dependenciesForRevision(repo, base)
	headDeps, headOK := s.dependenciesForRevision(repo, head)
	if !baseOK || !headOK {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}

	diff := []map[string]interface{}{}
	appendChange := func(changeType string, dep *dependencyEntry) {
		ecosystem, depName, version := parsePurl(dep.PackageURL)
		scope := dep.Scope
		if scope == "" {
			scope = "unknown"
		}
		diff = append(diff, map[string]interface{}{
			"change_type":           changeType,
			"manifest":              dep.Manifest,
			"ecosystem":             ecosystem,
			"name":                  depName,
			"version":               version,
			"package_url":           dep.PackageURL,
			"license":               nil,
			"source_repository_url": nil,
			"vulnerabilities":       []map[string]interface{}{},
			"scope":                 scope,
		})
	}
	var addedPurls, removedPurls []string
	for purl := range headDeps {
		if _, ok := baseDeps[purl]; !ok {
			addedPurls = append(addedPurls, purl)
		}
	}
	for purl := range baseDeps {
		if _, ok := headDeps[purl]; !ok {
			removedPurls = append(removedPurls, purl)
		}
	}
	sort.Strings(addedPurls)
	sort.Strings(removedPurls)
	for _, purl := range addedPurls {
		appendChange("added", headDeps[purl])
	}
	for _, purl := range removedPurls {
		appendChange("removed", baseDeps[purl])
	}
	writeJSON(w, http.StatusOK, diff)
}

// dependenciesForRevision resolves one side of a basehead (a SHA, branch, or
// fully qualified ref) to its dependency set. ok is false when the revision
// matches neither git storage nor any snapshot.
func (s *Server) dependenciesForRevision(repo *store.Repo, rev string) (map[string]*dependencyEntry, bool) {
	owner, name, _ := store.SplitRepoFullName(repo.FullName)
	gitStor := s.store.GetGitStorage(owner, name)

	branch := strings.TrimPrefix(rev, "refs/heads/")
	var sha string
	if len(rev) == 40 && !strings.Contains(rev, "/") {
		sha = rev
	} else {
		sha = store.ResolveBranchSha(gitStor, branch)
	}

	if sha != "" {
		if deps := s.currentDependencies(repo.ID, "", sha); len(deps) > 0 {
			return deps, true
		}
	}
	if deps := s.currentDependencies(repo.ID, "refs/heads/"+branch, ""); len(deps) > 0 {
		return deps, true
	}
	// A revision that resolves in git but has no snapshots has an empty
	// dependency set; an unresolvable revision is a 404.
	if sha != "" {
		return map[string]*dependencyEntry{}, true
	}
	return nil, false
}
