package bleephub

import (
	"context"
	"crypto"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/e6qu/bleephub/internal/store"
)

func (s *Server) registerGHPagesDeploymentRoutes() {
	s.route("POST /api/v3/repos/{owner}/{repo}/pages/deployments",
		s.requirePerm(store.ScopePages, store.PermWrite, s.handlePagesDeploymentCreate))
	s.route("GET /api/v3/repos/{owner}/{repo}/pages/deployments/{pages_deployment_id}", s.requirePagesRead(s.handlePagesDeploymentStatus))
	s.route("POST /api/v3/repos/{owner}/{repo}/pages/deployments/{pages_deployment_id}/cancel",
		s.requirePerm(store.ScopePages, store.PermWrite, s.handlePagesDeploymentCancel))
	s.route("GET /api/v3/repos/{owner}/{repo}/pages/health", s.requirePagesRead(s.handlePagesHealthCheck))
}

func (s *Server) requirePagesRead(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		repo := s.lookupRepoFromPath(r)
		if repo != nil {
			s.store.Misc.Mu.RLock()
			site := s.store.Misc.PagesByRepo[repo.ID]
			s.store.Misc.Mu.RUnlock()
			if !repo.Private && (site == nil || site.Public) {
				next(w, r)
				return
			}
		}
		s.requirePerm(store.ScopePages, store.PermRead, next)(w, r)
	}
}

func (s *Server) handlePagesDeploymentCreate(w http.ResponseWriter, r *http.Request) {
	repo := s.lookupRepoFromPath(r)
	if repo == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	s.store.Misc.Mu.RLock()
	site := s.store.Misc.PagesByRepo[repo.ID]
	s.store.Misc.Mu.RUnlock()
	if site == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}

	var req struct {
		ArtifactID        *int64 `json:"artifact_id"`
		ArtifactURL       string `json:"artifact_url"`
		Environment       string `json:"environment"`
		PagesBuildVersion string `json:"pages_build_version"`
		OIDCToken         string `json:"oidc_token"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil && !errors.Is(err, io.EOF) {
		writeGHError(w, http.StatusBadRequest, "Problems parsing JSON")
		return
	}
	if req.PagesBuildVersion == "" {
		store.WriteGHValidationError(w, "PageDeployment", "pages_build_version", "missing_field")
		return
	}
	if req.OIDCToken == "" {
		store.WriteGHValidationError(w, "PageDeployment", "oidc_token", "missing_field")
		return
	}
	if req.ArtifactID == nil && req.ArtifactURL == "" {
		writeGHError(w, http.StatusBadRequest, "Either artifact_id or artifact_url is required.")
		return
	}
	if req.ArtifactID != nil {
		if !s.repoOwnsFinalizedArtifact(repo.FullName, *req.ArtifactID) {
			writeGHError(w, http.StatusBadRequest, "The artifact could not be found or does not belong to this repository.")
			return
		}
	}
	environment := store.CoalesceStr(req.Environment, "github-pages")
	if err := s.verifyPagesOIDCToken(r, req.OIDCToken, repo, environment, req.PagesBuildVersion, site); err != nil {
		writeGHError(w, http.StatusBadRequest, "Invalid OIDC token: "+err.Error())
		return
	}

	artifactBytes, err := s.readPagesDeploymentArtifact(r.Context(), repo.FullName, req.ArtifactID, req.ArtifactURL)
	if err != nil {
		writeGHError(w, http.StatusBadGateway, err.Error())
		return
	}
	if err := validatePagesArtifact(artifactBytes); err != nil {
		writeGHError(w, http.StatusUnprocessableEntity, "Invalid Pages artifact: "+err.Error())
		return
	}
	d, err := s.publishPagesArtifact(r.Context(), repo.ID, environment, req.PagesBuildVersion, artifactBytes)
	if err != nil {
		writeGHError(w, http.StatusBadGateway, err.Error())
		return
	}

	// Publish is synchronous, so the deployment is already terminal here.
	s.store.Misc.Mu.Lock()
	site.Status = "built"
	if s.store.Misc.Persist != nil {
		s.store.Misc.Persist.MustPut("pages_sites", strconv.Itoa(repo.ID), site)
	}
	s.store.Misc.Mu.Unlock()

	user := ghUserFromContext(r.Context())
	if user != nil {
		s.recordAuditEvent("pages.deployment", user.Login, "", map[string]interface{}{"repo": repo.FullName, "deployment_id": d.ID})
	}

	base := s.baseURL(r)
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"id":         d.BuildVersion,
		"status_url": base + "/api/v3/repos/" + repo.FullName + "/pages/deployments/" + d.BuildVersion,
		"page_url":   site.HTMLURL,
	})
}

type pagesOIDCClaims struct {
	Issuer       string `json:"iss"`
	Audience     string `json:"aud"`
	ExpiresAt    int64  `json:"exp"`
	IssuedAt     int64  `json:"iat"`
	NotBefore    int64  `json:"nbf"`
	Repository   string `json:"repository"`
	RepositoryID string `json:"repository_id"`
	Environment  string `json:"environment"`
	Ref          string `json:"ref"`
	SHA          string `json:"sha"`
}

func (s *Server) verifyPagesOIDCToken(r *http.Request, token string, repo *store.Repo, environment, buildVersion string, site *store.PagesSite) error {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return errors.New("token is not a three-part JWT")
	}
	var header struct {
		Algorithm string `json:"alg"`
		KeyID     string `json:"kid"`
	}
	headerBytes, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return fmt.Errorf("decode JWT header: %w", err)
	}
	if err := json.Unmarshal(headerBytes, &header); err != nil {
		return fmt.Errorf("decode JWT header JSON: %w", err)
	}
	if header.Algorithm != "RS256" || header.KeyID != "bleephub-oidc" {
		return errors.New("token must use Bleephub RS256 signing key")
	}
	payloadBytes, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return fmt.Errorf("decode JWT payload: %w", err)
	}
	signature, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return fmt.Errorf("decode JWT signature: %w", err)
	}
	key, err := s.oidcKeyE()
	if err != nil {
		return fmt.Errorf("load OpenID Connect signing key: %w", err)
	}
	digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if err := rsa.VerifyPKCS1v15(&key.PublicKey, crypto.SHA256, digest[:], signature); err != nil {
		return errors.New("token signature is invalid")
	}
	var claims pagesOIDCClaims
	if err := json.Unmarshal(payloadBytes, &claims); err != nil {
		return fmt.Errorf("decode JWT claims: %w", err)
	}
	now := s.currentTime().Unix()
	if claims.ExpiresAt <= now || claims.NotBefore > now || claims.IssuedAt > now {
		return errors.New("token is expired or not yet valid")
	}
	if claims.Issuer != s.actionsOIDCIssuer(r) {
		return fmt.Errorf("issuer %q does not match Bleephub", claims.Issuer)
	}
	wantAudience := "https://github.com/" + repo.FullName
	if claims.Audience != wantAudience {
		return fmt.Errorf("audience %q does not match %q", claims.Audience, wantAudience)
	}
	if claims.Repository != repo.FullName || claims.RepositoryID != strconv.Itoa(repo.ID) {
		return errors.New("repository claims do not match the deployment repository")
	}
	if claims.Environment != environment {
		return fmt.Errorf("environment %q does not match %q", claims.Environment, environment)
	}
	if claims.SHA != buildVersion {
		return errors.New("build version does not match the workflow SHA")
	}
	buildType := "legacy"
	if site.BuildType != nil {
		buildType = *site.BuildType
	}
	if buildType != "workflow" {
		branch, _ := site.Source["branch"].(string)
		if branch != "" && claims.Ref != "refs/heads/"+branch {
			return fmt.Errorf("workflow ref %q does not match Pages source branch %q", claims.Ref, branch)
		}
	}
	return nil
}

func (s *Server) readPagesDeploymentArtifact(ctx context.Context, repoFullName string, artifactID *int64, artifactURL string) ([]byte, error) {
	if artifactID != nil {
		art, ok := s.artifactStore.ArtifactByID(*artifactID)
		if !ok || !s.artifactBelongsToRepo(art, repoFullName) {
			return nil, fmt.Errorf("pages deployment artifact %d was not found for repository %s", *artifactID, repoFullName)
		}
		data := append([]byte(nil), art.Data...)
		if len(data) == 0 && art.Size > 0 {
			if s.artifactStore.ByteStore == nil {
				return nil, fmt.Errorf("pages deployment artifact %d bytes require configured object storage", art.ID)
			}
			var err error
			data, err = s.artifactStore.ByteStore.Get(ctx, store.ArtifactDataKey(art.ID))
			if err != nil {
				return nil, fmt.Errorf("read Pages deployment artifact %d: %w", art.ID, err)
			}
		}
		if int64(len(data)) != art.Size {
			return nil, fmt.Errorf("pages deployment artifact %d size mismatch: metadata=%d bytes=%d", art.ID, art.Size, len(data))
		}
		return data, nil
	}

	fetchCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(fetchCtx, http.MethodGet, artifactURL, nil)
	if err != nil {
		return nil, fmt.Errorf("create Pages deployment artifact request: %w", err)
	}
	// Route through the SSRF address gate: the artifact URL comes from the
	// deployment run and must not dial an internal/metadata address.
	pagesClient := &http.Client{Transport: newAddressCheckedHTTPTransport(false)}
	resp, err := pagesClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch Pages deployment artifact: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, fmt.Errorf("fetch Pages deployment artifact: status %d", resp.StatusCode)
	}
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read Pages deployment artifact: %w", err)
	}
	return data, nil
}

func (s *Server) handlePagesDeploymentStatus(w http.ResponseWriter, r *http.Request) {
	repo := s.lookupRepoFromPath(r)
	if repo == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	d := s.store.GetPagesDeploymentByIdentifier(repo.ID, r.PathValue("pages_deployment_id"))
	if d == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"status": d.Status})
}

func (s *Server) handlePagesDeploymentCancel(w http.ResponseWriter, r *http.Request) {
	repo := s.lookupRepoFromPath(r)
	if repo == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	d := s.store.GetPagesDeploymentByIdentifier(repo.ID, r.PathValue("pages_deployment_id"))
	if d == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	if pagesDeploymentTerminal(d.Status) {
		writeGHError(w, http.StatusUnprocessableEntity, "Deployment cannot be cancelled")
		return
	}
	s.store.SetPagesDeploymentStatus(repo.ID, d.ID, "deployment_cancelled")
	w.WriteHeader(http.StatusNoContent)
}

func pagesDeploymentTerminal(status string) bool {
	switch status {
	case "succeed", "deployment_cancelled", "deployment_failed",
		"deployment_content_failed", "deployment_attempt_error", "deployment_lost":
		return true
	}
	return false
}

func (s *Server) repoOwnsFinalizedArtifact(repoFullName string, artifactID int64) bool {
	s.artifactStore.Mu.RLock()
	defer s.artifactStore.Mu.RUnlock()
	for _, art := range s.artifactStore.Artifacts {
		if art.ID == artifactID && art.RepoFullName == repoFullName && art.Finalized {
			return true
		}
	}
	return false
}

// Pages health check

func (s *Server) handlePagesHealthCheck(w http.ResponseWriter, r *http.Request) {
	repo := s.lookupRepoFromPath(r)
	if repo == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	s.store.Misc.Mu.RLock()
	site := s.store.Misc.PagesByRepo[repo.ID]
	var cname string
	var httpsEnforced bool
	if site != nil {
		cname = site.CNAME
		httpsEnforced = site.HTTPSEnforced
	}
	s.store.Misc.Mu.RUnlock()
	if site == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	if cname == "" {
		writeGHError(w, http.StatusBadRequest, "There isn't a custom domain on this Pages site")
		return
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"domain":     pagesDomainHealthJSON(r.Context(), cname, httpsEnforced),
		"alt_domain": nil,
	})
}

// pagesDomainHealthJSON runs the checks bleephub can perform locally (DNS
// resolution, syntactic classification). Checks needing GitHub's Pages edge are
// omitted rather than fabricated; every member is optional in the schema.
func pagesDomainHealthJSON(ctx context.Context, host string, httpsEnforced bool) map[string]interface{} {
	lookupCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	addrs, err := net.DefaultResolver.LookupHost(lookupCtx, host)
	dnsResolves := err == nil && len(addrs) > 0

	isValidDomain := validPagesDomain(host)
	isApex := isValidDomain && strings.Count(host, ".") == 1
	isPagesDomain := strings.HasSuffix(host, ".github.io") || host == "github.io"

	var reason interface{}
	isValid := isValidDomain && dnsResolves
	if !isValidDomain {
		reason = "invalid domain"
	} else if !dnsResolves {
		reason = "domain does not resolve"
	}

	return map[string]interface{}{
		"host":            host,
		"uri":             "http://" + host + "/",
		"nameservers":     "default",
		"dns_resolves":    dnsResolves,
		"is_valid_domain": isValidDomain,
		"is_apex_domain":  isApex,
		"is_pages_domain": isPagesDomain,
		"is_valid":        isValid,
		"reason":          reason,
		"enforces_https":  httpsEnforced,
	}
}

// validPagesDomain checks RFC 1123 label syntax.
func validPagesDomain(host string) bool {
	if host == "" || len(host) > 253 {
		return false
	}
	for _, label := range strings.Split(host, ".") {
		if label == "" || len(label) > 63 {
			return false
		}
		for i, c := range label {
			isAlnum := (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9')
			if !isAlnum && c != '-' {
				return false
			}
			if c == '-' && (i == 0 || i == len(label)-1) {
				return false
			}
		}
	}
	return true
}
