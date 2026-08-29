package bleephub

import (
	"context"
	"crypto"
	"crypto/hmac"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/e6qu/bleephub/internal/store"
	"github.com/google/uuid"
)

func (s *Server) registerAuthRoutes() {
	// Resolve the runner signing key at startup so a misconfigured key fails
	// fast rather than on the first runner connection.
	if _, err := runnerSigningKey(); err != nil {
		s.logger.Fatal().Err(err).Msg("failed to initialize the runner protocol signing key")
	}

	s.registerExternalIdentityRoutes()
	// Runner registration (GHES-style): config.sh presents the
	// administration:write registration token here.
	s.route("POST /api/v3/actions/runner-registration", s.handleRunnerRegistration)

	// Five runner-protocol routes stand outside requireRunnerAuth — the runner
	// protocol allowlist. Each is reached before the caller holds a session
	// bearer token and carries its own credential (an RSA client_assertion, the
	// registration token config.sh holds, or an unguessable signed blob URL) or
	// no tenant state at all. The other three are registered next to their
	// handlers (agents.go, artifacts.go).
	// TestRunnerProtocolRejectsUnauthenticatedCalls fails if any other /_apis/
	// or /twirp/ route answers an unauthenticated call with anything but 401, so
	// a sixth cannot be added by accident.

	// Service discovery, read before the runner holds any credential; the
	// response carries no tenant state.
	s.route("GET /_apis/connectionData", s.handleConnectionData)

	// OAuth token exchange: carries its own RSA client_assertion credential.
	s.route("POST /_apis/v1/auth/", s.handleOAuthToken)
	s.route("POST /_apis/v1/auth", s.handleOAuthToken)
}

// handleRunnerRegistration returns the tenant URL and the credential the runner
// presents to add or remove its agent, authenticating on the administration:write
// registration/removal token config.sh was given.
func (s *Server) handleRunnerRegistration(w http.ResponseWriter, r *http.Request) {
	s.logger.Info().Msg("runner registration request")

	claims, err := runnerSetupCredential(r, runnerPurposesConfig)
	if err != nil {
		s.logger.Warn().Err(err).Msg("runner registration rejected")
		writeGHError(w, http.StatusUnauthorized, "Invalid runner registration token")
		return
	}

	var req struct {
		URL         string `json:"url"`
		RunnerEvent string `json:"runner_event"`
	}
	if !decodeJSONBody(w, r, &req) {
		return
	}

	// The tenant credential may carry only the operation the presented token was
	// minted for: a removal token must not be exchangeable for a registering one.
	purpose, err := runnerPurposeForEvent(req.RunnerEvent)
	if err != nil {
		writeGHError(w, http.StatusBadRequest, err.Error())
		return
	}
	if claims.Purpose != purpose {
		s.logger.Warn().Str("event", req.RunnerEvent).Str("purpose", claims.Purpose).
			Msg("runner registration rejected: token purpose does not match runner_event")
		writeGHError(w, http.StatusUnauthorized, "Invalid runner registration token")
		return
	}

	token, err := newRunnerRegistrationToken(claims.Scope, purpose)
	if err != nil {
		writeGHError(w, http.StatusInternalServerError, err.Error())
		return
	}

	serverURL := s.baseURL(r)

	// Preserve any path on the original --url (e.g. /owner/repo); the runner
	// extracts org/repo from the tenant URL for display.
	if req.URL != "" {
		if parsed, err := url.Parse(req.URL); err == nil && parsed.Path != "" {
			serverURL += parsed.Path
		}
	}

	s.logger.Info().Str("tenantUrl", serverURL).Msg("returning tenant URL")

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"url":          serverURL,
		"token_schema": "OAuthAccessToken",
		"token":        token,
	})
}

// runnerSetupCredential verifies the config-time token config.sh holds before it
// has exchanged its client_assertion, accepting the RemoteAuth, bearer, and token
// schemes. A token whose purpose is not among those the caller accepts is an error.
func runnerSetupCredential(r *http.Request, purposes []string) (runnerRegistrationClaims, error) {
	scheme, cred := authScheme(r.Header.Get("Authorization"))
	switch scheme {
	case "remoteauth", "bearer", "token":
	default:
		return runnerRegistrationClaims{}, fmt.Errorf("missing runner setup token")
	}
	if cred == "" {
		return runnerRegistrationClaims{}, fmt.Errorf("missing runner setup token")
	}
	return parseRunnerRegistrationToken(cred, purposes)
}

// runnerPurposeForEvent maps runner_event to the credential purpose it needs. An
// unrecognized event is refused, never defaulted: defaulting would pick which
// operation an unnamed token authorizes.
func runnerPurposeForEvent(event string) (string, error) {
	switch event {
	case "register":
		return runnerPurposeRegistration, nil
	case "remove":
		return runnerPurposeRemoval, nil
	}
	return "", fmt.Errorf("unsupported runner_event %q", event)
}

// serviceDefinition is the ConnectionData ServiceDefinition shape the runner SDK expects.
type serviceDefinition struct {
	ServiceType       string        `json:"serviceType"`
	Identifier        string        `json:"identifier"`
	DisplayName       string        `json:"displayName"`
	RelativeToSetting string        `json:"relativeToSetting"`
	RelativePath      string        `json:"relativePath"`
	Description       string        `json:"description"`
	ServiceOwner      string        `json:"serviceOwner"`
	LocationMappings  []interface{} `json:"locationMappings"`
	ToolID            string        `json:"toolId"`
	Status            string        `json:"status"`
	Properties        interface{}   `json:"properties"`
	ResourceVersion   int           `json:"resourceVersion"`
	MinVersion        string        `json:"minVersion"`
	MaxVersion        string        `json:"maxVersion"`
}

func newServiceDef(name, guid, path string) serviceDefinition {
	return serviceDefinition{
		ServiceType:       name,
		Identifier:        guid,
		DisplayName:       name,
		RelativeToSetting: "fullyQualified",
		RelativePath:      path,
		Description:       name,
		ServiceOwner:      "00000000-0000-0000-0000-000000000000",
		LocationMappings:  []interface{}{},
		ToolID:            name,
		Status:            "active",
		Properties:        map[string]interface{}{},
		ResourceVersion:   1,
		MinVersion:        "1.0",
		MaxVersion:        "12.0",
	}
}

// handleConnectionData returns service location data (GUIDs → API paths).
func (s *Server) handleConnectionData(w http.ResponseWriter, r *http.Request) {
	s.logger.Debug().Msg("connection data request")

	defs := []serviceDefinition{
		newServiceDef("AgentPools", "a8c47e17-4d56-4a56-92bb-de7ea7dc65be", "/_apis/v1/AgentPools"),
		newServiceDef("Agent", "e298ef32-5878-4cab-993c-043836571f42", "/_apis/v1/Agent/{poolId}/{agentId}"),
		newServiceDef("AgentSession", "134e239e-2df3-4794-a6f6-24f1f19ec8dc", "/_apis/v1/AgentSession/{poolId}/{sessionId}"),
		newServiceDef("Message", "c3a054f6-7a8a-49c0-944e-3a8e5d7adfd7", "/_apis/v1/Message/{poolId}/{messageId}"),
		newServiceDef("AgentRequest", "fc825784-c92a-4299-9221-998a02d1b54f", "/_apis/v1/AgentRequest/{poolId}/{requestId}"),
		newServiceDef("FinishJob", "557624af-b29e-4c20-8ab0-0399d2204f3f", "/_apis/v1/FinishJob/{scopeIdentifier}/{hubName}/{planId}"),
		newServiceDef("Timeline", "83597576-cc2c-453c-bea6-2882ae6a1653", "/_apis/v1/Timeline/{scopeIdentifier}/{hubName}/{planId}/timeline/{timelineId}"),
		newServiceDef("TimelineRecords", "8893bc5b-35b2-4be7-83cb-99e683551db4", "/_apis/v1/Timeline/{scopeIdentifier}/{hubName}/{planId}/{timelineId}"),
		newServiceDef("Logfiles", "46f5667d-263a-4684-91b1-dff7fdcf64e2", "/_apis/v1/Logfiles/{scopeIdentifier}/{hubName}/{planId}/{logId}"),
		newServiceDef("TimeLineWebConsoleLog", "858983e4-19bd-4c5e-864c-507b59b58b12", "/_apis/v1/TimeLineWebConsoleLog/{scopeIdentifier}/{hubName}/{planId}/{timelineId}/{recordId}"),
		newServiceDef("ActionDownloadInfo", "27d7f831-88c1-4719-8ca1-6a061dad90eb", "/_apis/v1/ActionDownloadInfo/{scopeIdentifier}/{hubName}/{planId}"),
		newServiceDef("TimelineAttachments", "7898f959-9cdf-4096-b29e-7f293031629e", "/_apis/v1/Timeline/{scopeIdentifier}/{hubName}/{planId}/{timelineId}/attachments/{recordId}/{type}/{name}"),
		newServiceDef("CustomerIntelligence", "b5cc35c2-ff2b-491d-a085-24b6e9f396fd", "/_apis/v1/tasks"),
		newServiceDef("Tasks", "60aac929-f0cd-4bc8-9ce4-6b30e8f1b1bd", "/_apis/v1/tasks/{taskId}/{versionString}"),
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"instanceId": uuid.New().String(),
		"locationServiceData": map[string]interface{}{
			"serviceDefinitions": defs,
		},
	})
}

// The runner performs an OAuth 2.0 client-credentials grant with an RSA
// client_assertion rather than a secret, so grant_type is client_credentials and
// the assertion travels in client_assertion.
const (
	runnerGrantType           = "client_credentials"
	runnerClientAssertionType = "urn:ietf:params:oauth:client-assertion-type:jwt-bearer"
)

// agentAuthorizationURL is the token endpoint an agent exchanges its RSA
// client_assertion at, named in the challenge unauthenticated runners receive.
func (s *Server) agentAuthorizationURL(r *http.Request) string {
	return s.baseURL(r) + "/_apis/v1/auth/"
}

// handleOAuthToken exchanges a runner-signed client_assertion JWT for a session
// access token, verifying the assertion against the agent's registered public key.
func (s *Server) handleOAuthToken(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		writeOAuthError(w, http.StatusBadRequest, "invalid_request", "failed to parse form body: "+err.Error())
		return
	}
	if grant := r.PostFormValue("grant_type"); grant != runnerGrantType {
		writeOAuthError(w, http.StatusBadRequest, "unsupported_grant_type", fmt.Sprintf("grant_type %q is not supported", grant))
		return
	}
	if at := r.PostFormValue("client_assertion_type"); at != runnerClientAssertionType {
		writeOAuthError(w, http.StatusBadRequest, "invalid_request", fmt.Sprintf("client_assertion_type %q is not supported", at))
		return
	}
	assertion := r.PostFormValue("client_assertion")
	if assertion == "" {
		writeOAuthError(w, http.StatusBadRequest, "invalid_request", "client_assertion is required")
		return
	}

	agent, err := s.verifyAgentClientAssertion(assertion)
	if err != nil {
		s.logger.Warn().Err(err).Msg("agent client_assertion validation failed")
		writeOAuthError(w, http.StatusUnauthorized, "invalid_client", err.Error())
		return
	}

	s.logger.Debug().Int("agentId", agent.ID).Str("clientId", agent.Authorization.ClientID).Msg("oauth token issued")
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"access_token": makeJWT(agent.Authorization.ClientID, runnerAudSession),
		"expires_in":   int(runnerTokenTTL.Seconds()),
		"scope":        "/",
		"token_type":   "access_token",
	})
}

func writeOAuthError(w http.ResponseWriter, status int, code, desc string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	body, _ := json.Marshal(map[string]string{"error": code, "error_description": desc})
	_, _ = w.Write(body)
}

// verifyAgentClientAssertion validates an RSA JWT (RS256 or PS256) signed by the
// agent's registered private key; the iss claim must name a known agent ClientID.
func (s *Server) verifyAgentClientAssertion(token string) (*store.Agent, error) {
	parts := strings.SplitN(token, ".", 3)
	if len(parts) != 3 {
		return nil, fmt.Errorf("malformed JWT: expected 3 parts")
	}

	headerBytes, err := store.Base64urlDecode(parts[0])
	if err != nil {
		return nil, fmt.Errorf("decode JWT header: %w", err)
	}
	var header struct {
		Alg string `json:"alg"`
	}
	if err := json.Unmarshal(headerBytes, &header); err != nil {
		return nil, fmt.Errorf("parse JWT header: %w", err)
	}
	if header.Alg != "RS256" && header.Alg != "PS256" {
		return nil, fmt.Errorf("unsupported JWT algorithm %q (expected RS256 or PS256)", header.Alg)
	}

	payloadBytes, err := store.Base64urlDecode(parts[1])
	if err != nil {
		return nil, fmt.Errorf("decode JWT payload: %w", err)
	}
	var payload struct {
		Iss string  `json:"iss"`
		Exp float64 `json:"exp"`
	}
	if err := json.Unmarshal(payloadBytes, &payload); err != nil {
		return nil, fmt.Errorf("parse JWT payload: %w", err)
	}
	if payload.Iss == "" {
		return nil, fmt.Errorf("missing iss claim")
	}
	if exp := int64(payload.Exp); exp > 0 && time.Now().Unix() > exp {
		return nil, fmt.Errorf("JWT expired")
	}

	agent := s.store.LookupAgentByClientID(payload.Iss)
	if agent == nil {
		return nil, fmt.Errorf("no agent registered with clientId %q", payload.Iss)
	}
	if agent.Authorization == nil || agent.Authorization.PublicKey == nil {
		return nil, fmt.Errorf("agent %d has no registered public key", agent.ID)
	}
	pubKey, err := agentRSAPublicKey(agent.Authorization.PublicKey)
	if err != nil {
		return nil, fmt.Errorf("build agent public key: %w", err)
	}

	sigBytes, err := store.Base64urlDecode(parts[2])
	if err != nil {
		return nil, fmt.Errorf("decode JWT signature: %w", err)
	}
	signInput := parts[0] + "." + parts[1]
	hash := sha256.Sum256([]byte(signInput))
	var verifyErr error
	switch header.Alg {
	case "PS256":
		verifyErr = rsa.VerifyPSS(pubKey, crypto.SHA256, hash[:], sigBytes, nil)
	default:
		verifyErr = rsa.VerifyPKCS1v15(pubKey, crypto.SHA256, hash[:], sigBytes)
	}
	if verifyErr != nil {
		return nil, fmt.Errorf("invalid JWT signature: %w", verifyErr)
	}
	return agent, nil
}

// agentRSAPublicKey reconstructs an *rsa.PublicKey from the base64 modulus+exponent
// pair the runner sent at registration (Azure DevOps agent protocol).
func agentRSAPublicKey(pk *store.AgentPublicKey) (*rsa.PublicKey, error) {
	modBytes, err := base64.StdEncoding.DecodeString(pk.Modulus)
	if err != nil {
		return nil, fmt.Errorf("decode modulus: %w", err)
	}
	expBytes, err := base64.StdEncoding.DecodeString(pk.Exponent)
	if err != nil {
		return nil, fmt.Errorf("decode exponent: %w", err)
	}
	if len(modBytes) == 0 || len(expBytes) == 0 {
		return nil, fmt.Errorf("empty modulus or exponent")
	}
	e := 0
	for _, b := range expBytes {
		e = e<<8 | int(b)
	}
	if e == 0 {
		return nil, fmt.Errorf("invalid public exponent (zero)")
	}
	return &rsa.PublicKey{N: new(big.Int).SetBytes(modBytes), E: e}, nil
}

func base64url(data []byte) string {
	s := base64.RawURLEncoding.EncodeToString(data)
	return strings.TrimRight(s, "=")
}

// runner protocol credentials
//
// Three credentials carry runner identity, all signed with one process key: the
// registration token config.sh presents, the agent client id the client_assertion
// names as iss, and the bearer token (agent session or per-job runtime token).
// Each carries the repo/org scope it may act for, so no route trusts an
// unverifiable claim.

const (
	// runnerTokenTTL bounds a bearer credential to the longest a GitHub Actions
	// job may run.
	runnerTokenTTL = 6 * time.Hour

	// runnerRegistrationTTL matches real GitHub's ~1h registration/removal token expiry.
	runnerRegistrationTTL = time.Hour

	// runnerClockSkew tolerates clock difference between the minting and verifying server.
	runnerClockSkew = 60 * time.Second

	// envRunnerSigningKey pins the signing key so replicas accept each other's credentials.
	envRunnerSigningKey = "BLEEPHUB_RUNNER_TOKEN_KEY"
)

// Bearer audiences.
const (
	runnerAudSession = "bleephub" // agent session token
	runnerAudJob     = "actions"  // per-job runtime token
)

// Registration credential purposes. A removal token cannot register a runner.
const (
	runnerPurposeRegistration = "registration"
	runnerPurposeRemoval      = "removal"
)

// Config-time purposes a route accepts: config.sh holds a registration token,
// config.sh remove a removal token, and the shared pool lookup accepts either.
// Naming the set at the route keeps each token to its own operation.
var (
	runnerPurposesRegister = []string{runnerPurposeRegistration}
	runnerPurposesRemove   = []string{runnerPurposeRemoval}
	runnerPurposesConfig   = []string{runnerPurposeRegistration, runnerPurposeRemoval}
)

// runnerSigningKey resolves the HMAC key backing every runner credential.
// BLEEPHUB_RUNNER_TOKEN_KEY (base64, >=32 bytes) pins it; otherwise it is
// generated once per process, which is correct because runner sessions and job
// tokens only mean anything against dispatch state that does not survive a restart.
var runnerSigningKey = sync.OnceValues(loadRunnerSigningKey)

func loadRunnerSigningKey() ([]byte, error) {
	if raw := strings.TrimSpace(os.Getenv(envRunnerSigningKey)); raw != "" {
		key, err := base64.StdEncoding.DecodeString(raw)
		if err != nil {
			return nil, fmt.Errorf("decode %s as base64: %w", envRunnerSigningKey, err)
		}
		if len(key) < 32 {
			return nil, fmt.Errorf("%s must decode to at least 32 bytes, got %d", envRunnerSigningKey, len(key))
		}
		return key, nil
	}
	key := make([]byte, 32)
	if _, err := io.ReadFull(rand.Reader, key); err != nil {
		return nil, fmt.Errorf("generate runner protocol signing key: %w", err)
	}
	return key, nil
}

// runnerMAC signs data with the runner protocol key, resolved at startup, so an
// error here is unreachable rather than a condition to degrade around.
func runnerMAC(data string) []byte {
	key, err := runnerSigningKey()
	if err != nil {
		panic("runner protocol signing key unavailable: " + err.Error())
	}
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(data))
	return mac.Sum(nil)
}

// runnerScopeFromRequest reads the scope a request acts for from its path params
// ({owner}/{repo}, {org}, or {enterprise}). The target must exist — a credential
// for a missing target is a credential for nothing, and GitHub answers 404.
func (s *Server) runnerScopeFromRequest(r *http.Request) (store.RunnerScope, error) {
	owner, repo := r.PathValue("owner"), r.PathValue("repo")
	if owner != "" && repo != "" {
		found := s.store.GetRepo(owner, repo)
		if found == nil {
			return store.RunnerScope{}, fmt.Errorf("repository %s/%s not found", owner, repo)
		}
		return store.RunnerScope{Repo: found.FullName}, nil
	}
	if org := r.PathValue("org"); org != "" {
		resolved := s.store.GetOrg(org)
		if resolved == nil {
			return store.RunnerScope{}, fmt.Errorf("organization %s not found", org)
		}
		// Scope by the canonical login: a scope carrying the request's casing
		// would not match later canonical-name comparisons.
		return store.RunnerScope{Org: resolved.Login}, nil
	}
	if enterprise := r.PathValue("enterprise"); enterprise != "" {
		if enterprise != s.enterpriseSlug() {
			return store.RunnerScope{}, fmt.Errorf("enterprise %s not found", enterprise)
		}
		return store.RunnerScope{Enterprise: enterprise}, nil
	}
	return store.RunnerScope{}, fmt.Errorf("request path names neither a repository, organization, nor enterprise")
}

// signedBlob encodes payload and appends its MAC into an opaque credential.
func signedBlob(prefix string, payload any) (string, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("encode runner credential: %w", err)
	}
	encoded := base64url(body)
	return prefix + encoded + "." + base64url(runnerMAC(encoded)), nil
}

// parseSignedBlob verifies a credential minted by signedBlob and decodes it
// into out. A blob that does not verify is an error, never a decoded value.
func parseSignedBlob(prefix, token string, out any) error {
	rest, ok := strings.CutPrefix(token, prefix)
	if !ok {
		return fmt.Errorf("malformed runner credential: wrong prefix")
	}
	encoded, sigPart, ok := strings.Cut(rest, ".")
	if !ok {
		return fmt.Errorf("malformed runner credential: missing signature")
	}
	sig, err := store.Base64urlDecode(sigPart)
	if err != nil {
		return fmt.Errorf("decode runner credential signature: %w", err)
	}
	if !hmac.Equal(sig, runnerMAC(encoded)) {
		return fmt.Errorf("invalid runner credential signature")
	}
	body, err := store.Base64urlDecode(encoded)
	if err != nil {
		return fmt.Errorf("decode runner credential payload: %w", err)
	}
	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("parse runner credential payload: %w", err)
	}
	return nil
}

// runnerRegistrationClaims is the payload of the opaque one-shot token config.sh
// presents; it carries its own scope and expiry so registration needs no
// server-side token registry.
type runnerRegistrationClaims struct {
	Scope   store.RunnerScope `json:"scope"`
	Purpose string            `json:"purpose"`
	Exp     int64             `json:"exp"`
	Nonce   string            `json:"nonce"`
}

func newRunnerRegistrationToken(scope store.RunnerScope, purpose string) (string, error) {
	if scope.Empty() {
		return "", fmt.Errorf("runner registration token needs a repository, organization, or enterprise scope")
	}
	nonce, err := randomRunnerToken()
	if err != nil {
		return "", err
	}
	return signedBlob("A", runnerRegistrationClaims{
		Scope:   scope,
		Purpose: purpose,
		Exp:     time.Now().Add(runnerRegistrationTTL).Unix(),
		Nonce:   nonce,
	})
}

// parseRunnerRegistrationToken verifies a config-time token and returns its
// claims, provided its purpose is one the caller named. An empty purpose list
// accepts nothing.
func parseRunnerRegistrationToken(token string, purposes []string) (runnerRegistrationClaims, error) {
	var claims runnerRegistrationClaims
	if err := parseSignedBlob("A", token, &claims); err != nil {
		return runnerRegistrationClaims{}, err
	}
	if !slices.Contains(purposes, claims.Purpose) {
		return runnerRegistrationClaims{}, fmt.Errorf("runner token purpose %q is not one of %v", claims.Purpose, purposes)
	}
	if claims.Scope.Empty() {
		return runnerRegistrationClaims{}, fmt.Errorf("runner registration token carries no scope")
	}
	if time.Now().After(time.Unix(claims.Exp, 0).Add(runnerClockSkew)) {
		return runnerRegistrationClaims{}, fmt.Errorf("runner registration token expired")
	}
	return claims, nil
}

// newAgentClientID mints the clientId the runner stores and echoes as the
// client_assertion issuer. It is a bare GUID because that is what the runner
// deserializes it into; the GUID only names which agent's public key to verify
// against — the RSA signature is what authenticates, so guessability is harmless.
func newAgentClientID(scope store.RunnerScope) (string, error) {
	if scope.Empty() {
		return "", fmt.Errorf("agent clientId needs a repository, organization, or enterprise scope")
	}
	return uuid.New().String(), nil
}

// runnerTokenClaims is the payload of a bearer credential (agent session or
// per-job runtime token).
type runnerTokenClaims struct {
	Sub string `json:"sub"`
	Iss string `json:"iss"`
	Aud string `json:"aud"`
	Nbf int64  `json:"nbf"`
	Exp int64  `json:"exp"`
	Scp string `json:"scp"`
	// Repo and Perms are carried only by a per-job runtime token (aud=="actions"),
	// binding it to one repo and the workflow's least-privilege permission set so
	// the REST gate can scope its /api/v3 access (ACT-014). Absent on session tokens.
	Repo  string            `json:"repo,omitempty"`
	Perms map[string]string `json:"perms,omitempty"`
}

const runnerTokenScp = "Actions.Results:write Actions.Pipelines:read"

// signRunnerClaims HS256-signs a runner bearer credential; only parseRunnerToken accepts one.
func signRunnerClaims(claims runnerTokenClaims) string {
	payload, err := json.Marshal(claims)
	if err != nil {
		panic("encode runner bearer token: " + err.Error())
	}
	signing := base64url([]byte(`{"alg":"HS256","typ":"JWT"}`)) + "." + base64url(payload)
	return signing + "." + base64url(runnerMAC(signing))
}

// makeJWT mints an HS256 bearer credential for sub.
func makeJWT(sub, aud string) string {
	now := time.Now()
	return signRunnerClaims(runnerTokenClaims{
		Sub: sub,
		Iss: "bleephub",
		Aud: aud,
		Nbf: now.Add(-runnerClockSkew).Unix(),
		Exp: now.Add(runnerTokenTTL).Unix(),
		Scp: runnerTokenScp,
	})
}

// makeJobJWT mints a per-job runtime token (GITHUB_TOKEN) bound to one repo and
// the workflow's least-privilege permission set, which the REST gate reads to
// scope /api/v3 access (ACT-014).
func makeJobJWT(sub, repo string, perms map[string]string) string {
	now := time.Now()
	return signRunnerClaims(runnerTokenClaims{
		Sub:   sub,
		Iss:   "bleephub",
		Aud:   runnerAudJob,
		Nbf:   now.Add(-runnerClockSkew).Unix(),
		Exp:   now.Add(runnerTokenTTL).Unix(),
		Scp:   runnerTokenScp,
		Repo:  repo,
		Perms: perms,
	})
}

// parseRunnerToken verifies a bearer credential's signature and time bounds before
// any claim is readable; failure returns an error, never claims.
func parseRunnerToken(token string) (*runnerTokenClaims, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return nil, fmt.Errorf("malformed runner token: expected 3 parts")
	}
	headerBytes, err := store.Base64urlDecode(parts[0])
	if err != nil {
		return nil, fmt.Errorf("decode runner token header: %w", err)
	}
	var header struct {
		Alg string `json:"alg"`
	}
	if err := json.Unmarshal(headerBytes, &header); err != nil {
		return nil, fmt.Errorf("parse runner token header: %w", err)
	}
	if header.Alg != "HS256" {
		return nil, fmt.Errorf("unsupported runner token algorithm %q (expected HS256)", header.Alg)
	}
	sig, err := store.Base64urlDecode(parts[2])
	if err != nil {
		return nil, fmt.Errorf("decode runner token signature: %w", err)
	}
	if !hmac.Equal(sig, runnerMAC(parts[0]+"."+parts[1])) {
		return nil, fmt.Errorf("invalid runner token signature")
	}
	payloadBytes, err := store.Base64urlDecode(parts[1])
	if err != nil {
		return nil, fmt.Errorf("decode runner token payload: %w", err)
	}
	var claims runnerTokenClaims
	if err := json.Unmarshal(payloadBytes, &claims); err != nil {
		return nil, fmt.Errorf("parse runner token payload: %w", err)
	}
	if claims.Sub == "" {
		return nil, fmt.Errorf("runner token has no sub claim")
	}
	now := time.Now()
	if claims.Exp <= 0 || now.After(time.Unix(claims.Exp, 0).Add(runnerClockSkew)) {
		return nil, fmt.Errorf("runner token expired")
	}
	if claims.Nbf > 0 && now.Add(runnerClockSkew).Before(time.Unix(claims.Nbf, 0)) {
		return nil, fmt.Errorf("runner token is not valid yet")
	}
	return &claims, nil
}

// runnerPrincipal is the verified identity behind a runner protocol call.
type runnerPrincipal struct {
	Claims *runnerTokenClaims // nil for a config-time setup credential
	Agent  *store.Agent       // set for an agent session token
	Scope  store.RunnerScope  // repository/organization the credential may act for
	Setup  bool               // the registration/removal token config.sh holds
}

// IsJobToken reports whether the caller presented a per-job runtime token.
func (p *runnerPrincipal) IsJobToken() bool {
	return p != nil && p.Claims != nil && p.Claims.Aud == runnerAudJob
}

const ctxRunner contextKey = "runner-principal"

// runnerFromContext returns the principal requireRunnerAuth verified, or nil.
func runnerFromContext(ctx context.Context) *runnerPrincipal {
	p, _ := ctx.Value(ctxRunner).(*runnerPrincipal)
	return p
}

// callerRunner resolves the verified runner behind a request: the context
// principal, or the request's own credential when a handler is reached without
// one, so authorization never depends on the route's decorator.
func (s *Server) callerRunner(r *http.Request) (*runnerPrincipal, error) {
	if p := runnerFromContext(r.Context()); p != nil {
		return p, nil
	}
	return s.authenticateRunner(r)
}

func bearerCredential(r *http.Request) (string, bool) {
	scheme, cred := authScheme(r.Header.Get("Authorization"))
	if scheme != "bearer" || cred == "" {
		return "", false
	}
	return cred, true
}

// actionArchiveUser is the user half of the basic credential the runner presents
// to download an action archive; the token travels as the password.
const actionArchiveUser = "x-access-token"

// actionArchiveCredential reads the token off an action archive download, made by
// a plain HTTP client that sends whatever the download-info response named.
func actionArchiveCredential(r *http.Request) (string, bool) {
	scheme, cred := authScheme(r.Header.Get("Authorization"))
	if scheme != "basic" || cred == "" {
		return "", false
	}
	decoded, err := base64.StdEncoding.DecodeString(cred)
	if err != nil {
		return "", false
	}
	user, token, ok := strings.Cut(string(decoded), ":")
	if !ok || user != actionArchiveUser || token == "" {
		return "", false
	}
	return token, true
}

// authenticateRunner resolves a runner protocol caller to a verified principal.
func (s *Server) authenticateRunner(r *http.Request) (*runnerPrincipal, error) {
	raw := r.Header.Get("Authorization")
	if raw == "" {
		return nil, fmt.Errorf("missing runner bearer token")
	}
	scheme, token := authScheme(raw)
	if scheme == "" || token == "" {
		return nil, fmt.Errorf("malformed runner authorization header")
	}
	if scheme != "bearer" {
		return nil, fmt.Errorf("unsupported runner authorization scheme %q", scheme)
	}
	return s.runnerPrincipalForToken(token)
}

// runnerPrincipalForToken verifies a runner bearer credential and resolves the
// identity behind it, so a token admits the same caller whichever header shape
// carried it.
func (s *Server) runnerPrincipalForToken(token string) (*runnerPrincipal, error) {
	claims, err := parseRunnerToken(token)
	if err != nil {
		return nil, err
	}
	switch claims.Aud {
	case runnerAudSession:
		agent := s.store.LookupAgentByClientID(claims.Sub)
		if agent == nil {
			return nil, fmt.Errorf("no agent registered with clientId %q", claims.Sub)
		}
		if agent.Scope.Empty() {
			return nil, fmt.Errorf("agent %d carries no scope", agent.ID)
		}
		return &runnerPrincipal{Claims: claims, Agent: agent, Scope: agent.Scope}, nil
	case runnerAudJob:
		repo, err := s.repoForJobScope(claims.Sub)
		if err != nil {
			return nil, err
		}
		return &runnerPrincipal{Claims: claims, Scope: store.RunnerScope{Repo: repo}}, nil
	default:
		return nil, fmt.Errorf("unsupported runner token audience %q", claims.Aud)
	}
}

// challengeRunnerAuth refuses a runner request that carries no usable credential.
// The WWW-Authenticate: Bearer header is what drives the runner's token exchange —
// it opens every session with no credential and acquires one only from a 401 whose
// WWW-Authenticate contains "Bearer"; a bare 401 is read as the route's own answer.
func (s *Server) challengeRunnerAuth(w http.ResponseWriter, r *http.Request, err error) {
	s.logger.Debug().Err(err).Str("path", r.URL.Path).Msg("runner protocol request rejected")
	w.Header().Set("WWW-Authenticate", fmt.Sprintf("Bearer authorization_uri=%q", s.agentAuthorizationURL(r)))
	writeGHError(w, http.StatusUnauthorized, "Must authenticate to use the runner protocol")
}

// requireRunnerAuth gates a runner protocol route on a verified credential and
// hands the principal to the handler via context. Every /_apis/ and /twirp/ route
// outside the five-entry allowlist (see registerAuthRoutes) passes through here.
func (s *Server) requireRunnerAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		principal, err := s.authenticateRunner(r)
		if err != nil {
			s.challengeRunnerAuth(w, r, err)
			return
		}
		next(w, r.WithContext(context.WithValue(r.Context(), ctxRunner, principal)))
	}
}

// requireJobToken gates a route on a per-job runtime token; an agent session token
// is not accepted, since these routes act on a single job's plan.
func (s *Server) requireJobToken(next http.HandlerFunc) http.HandlerFunc {
	return s.requireRunnerAuth(func(w http.ResponseWriter, r *http.Request) {
		if !runnerFromContext(r.Context()).IsJobToken() {
			writeGHError(w, http.StatusForbidden, "This route requires a job runtime token")
			return
		}
		next(w, r)
	})
}

// requireActionArchiveToken gates an action archive download on the job runtime
// token the ActionDownloadInfo response handed the runner. The archive is fetched
// by a plain HTTP client sending the token as basic auth under x-access-token, so
// that shape is read here; the entitlement is the same job token verified the same way.
func (s *Server) requireActionArchiveToken(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		token, ok := actionArchiveCredential(r)
		if !ok {
			s.requireJobToken(next)(w, r)
			return
		}
		principal, err := s.runnerPrincipalForToken(token)
		if err != nil {
			s.logger.Debug().Err(err).Str("path", r.URL.Path).Msg("action archive download rejected")
			writeGHError(w, http.StatusUnauthorized, "Must authenticate to use the runner protocol")
			return
		}
		if !principal.IsJobToken() {
			writeGHError(w, http.StatusForbidden, "This route requires a job runtime token")
			return
		}
		next(w, r.WithContext(context.WithValue(r.Context(), ctxRunner, principal)))
	}
}

// requireRunnerSetupCredential gates a route the runner reaches during config.sh,
// before it has exchanged its client_assertion: the config-time token is accepted
// for the named purposes, as is a configured runner's agent session. Both paths
// hand the handler a principal carrying the verified scope.
func (s *Server) requireRunnerSetupCredential(purposes []string, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		claims, err := runnerSetupCredential(r, purposes)
		if err == nil {
			principal := &runnerPrincipal{Scope: claims.Scope, Setup: true}
			next(w, r.WithContext(context.WithValue(r.Context(), ctxRunner, principal)))
			return
		}
		s.requireAgentSession(next)(w, r)
	}
}

// requireAgentSession gates a route on an agent session token; a job runtime token
// is not an agent.
func (s *Server) requireAgentSession(next http.HandlerFunc) http.HandlerFunc {
	return s.requireRunnerAuth(func(w http.ResponseWriter, r *http.Request) {
		if runnerFromContext(r.Context()).Agent == nil {
			writeGHError(w, http.StatusForbidden, "This route requires an agent session token")
			return
		}
		next(w, r)
	})
}
