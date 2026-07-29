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

	"github.com/google/uuid"
)

func (s *Server) registerAuthRoutes() {
	// Every runner protocol credential is signed with this key. Resolving it
	// here makes a misconfigured key a startup failure instead of a runtime
	// one on the first runner connection.
	if _, err := runnerSigningKey(); err != nil {
		s.logger.Fatal().Err(err).Msg("failed to initialize the runner protocol signing key")
	}

	s.registerExternalIdentityRoutes()
	// Runner registration (GHES-style). config.sh presents the
	// administration:write-minted registration token here.
	s.route("POST /api/v3/actions/runner-registration", s.handleRunnerRegistration)

	// Connection data (service discovery). Runner-protocol allowlist entry:
	// the runner reads it before it holds any credential, and the response is
	// a fixed table of service GUIDs and paths carrying no tenant state.
	s.route("GET /_apis/connectionData", s.handleConnectionData)

	// OAuth token exchange. Runner-protocol allowlist entry: it carries its
	// own credential — an RSA client_assertion verified against the public key
	// the agent registered — so it cannot take a bearer token it has yet to be
	// issued.
	s.route("POST /_apis/v1/auth/", s.handleOAuthToken)
	s.route("POST /_apis/v1/auth", s.handleOAuthToken)
}

// handleRunnerRegistration returns the tenant URL and the credential the
// runner presents when it adds or removes its agent. The runner calls this
// during `config.sh --url <url> --token <token>` and `config.sh remove
// --token <token>`, sending the administration:write registration or removal
// token as its Authorization credential — real GitHub uses the RemoteAuth
// scheme, and octokit-shaped clients send it as a bearer token.
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

	// The tenant credential handed back may only carry the operation the
	// presented token was minted for: exchanging a removal token for one that
	// registers a runner would make the two indistinguishable.
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

	// The runner extracts org/repo from the tenant URL for display purposes.
	// If the original --url had a path (e.g. /owner/repo), preserve it.
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

// runnerSetupCredential verifies the config-time token on a request — the
// credential config.sh holds before it has exchanged its client_assertion —
// accepting the RemoteAuth scheme real GitHub's runner uses alongside the
// bearer and token schemes. A token whose purpose is not among the ones the
// caller accepts is an error, never a decoded credential.
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

// runnerPurposeForEvent maps the runner_event config.sh sends to the credential
// purpose that operation needs. An unrecognized event is refused rather than
// defaulted: defaulting would decide, for a caller that named nothing, which of
// the two operations its token authorizes.
func runnerPurposeForEvent(event string) (string, error) {
	switch event {
	case "register":
		return runnerPurposeRegistration, nil
	case "remove":
		return runnerPurposeRemoval, nil
	}
	return "", fmt.Errorf("unsupported runner_event %q", event)
}

// serviceDefinition matches the internal ServiceDefinition format
// that the runner SDK expects in ConnectionData.
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

// The token exchange the runner performs against authorizationUrl: the OAuth
// 2.0 client credentials grant, with the client authenticated by an RSA
// assertion rather than a secret. The runner builds the request from
// VssOAuthGrant.ClientCredentials and VssOAuthJwtBearerClientCredential, so
// the grant type is client_credentials and the assertion travels in
// client_assertion — not the jwt-bearer authorization grant, which no runner
// sends.
const (
	runnerGrantType           = "client_credentials"
	runnerClientAssertionType = "urn:ietf:params:oauth:client-assertion-type:jwt-bearer"
)

// agentAuthorizationURL is the token endpoint an agent exchanges its RSA
// client_assertion at. It is handed to the runner in its authorization record
// and named in the challenge unauthenticated runner requests answer with.
func (s *Server) agentAuthorizationURL(r *http.Request) string {
	return s.baseURL(r) + "/_apis/v1/auth/"
}

// handleOAuthToken exchanges a runner-signed client_assertion JWT for a
// session access token. The runner registered its RSA public key on
// /_apis/v1/Agent/{poolId}; the assertion's iss claim names the agent's
// ClientID and must verify against that stored public key.
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

// verifyAgentClientAssertion validates an RSA JWT signed by the agent's
// registered private key. Current runners use PS256 while older supported
// runners use RS256. The JWT's iss claim must match a known agent ClientID;
// the signature is verified against that agent's public key.
func (s *Server) verifyAgentClientAssertion(token string) (*Agent, error) {
	parts := strings.SplitN(token, ".", 3)
	if len(parts) != 3 {
		return nil, fmt.Errorf("malformed JWT: expected 3 parts")
	}

	headerBytes, err := base64urlDecode(parts[0])
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

	payloadBytes, err := base64urlDecode(parts[1])
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

	sigBytes, err := base64urlDecode(parts[2])
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

// agentRSAPublicKey reconstructs an *rsa.PublicKey from the modulus+exponent
// pair the runner sent during agent registration. The runner encodes both as
// standard base64, per the Azure DevOps agent protocol.
func agentRSAPublicKey(pk *AgentPublicKey) (*rsa.PublicKey, error) {
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

// --- runner protocol credentials ---
//
// Three credentials carry runner identity, all signed with one process key:
//
//	registration token — minted by an administration:write caller, presented
//	                     by config.sh to register an agent
//	agent client id    — handed back at registration; the RSA client_assertion
//	                     names it as `iss`
//	bearer token       — the agent session token from the client_assertion
//	                     exchange, and the per-job runtime token in the job
//	                     message
//
// All three carry the repository or organization scope they may act for, so
// no route has to trust a claim it cannot verify.

const (
	// runnerTokenTTL bounds a bearer credential to the longest a GitHub
	// Actions job may run. The runner re-exchanges its client_assertion when
	// the session token expires.
	runnerTokenTTL = 6 * time.Hour

	// runnerRegistrationTTL matches the ~1h expiry real GitHub puts on a
	// runner registration/removal token.
	runnerRegistrationTTL = time.Hour

	// runnerClockSkew tolerates a small clock difference between the server
	// that minted a credential and the one verifying it.
	runnerClockSkew = 60 * time.Second

	// envRunnerSigningKey pins the signing key so replicas of one deployment
	// accept each other's runner credentials.
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

// The config-time purposes a route accepts. `config.sh` holds a registration
// token and `config.sh remove` a removal token; the one call both make — the
// pool lookup of the runner's own name — accepts either. Naming the set at the
// route keeps a registration token from deleting a runner and a removal token
// from adding one.
var (
	runnerPurposesRegister = []string{runnerPurposeRegistration}
	runnerPurposesRemove   = []string{runnerPurposeRemoval}
	runnerPurposesConfig   = []string{runnerPurposeRegistration, runnerPurposeRemoval}
)

// runnerSigningKey resolves the HMAC key backing every runner credential.
// BLEEPHUB_RUNNER_TOKEN_KEY (base64, at least 32 bytes) pins it; without it
// the key is generated once per process. Process-local is correct here for
// the same reason it is for the OIDC signing key: runner sessions and job
// tokens are only meaningful against the dispatch state that issued them, and
// that state does not survive a restart.
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

// runnerMAC signs data with the runner protocol key. registerAuthRoutes
// resolves the key at startup, so a failure here is unreachable state rather
// than a condition to degrade around.
func runnerMAC(data string) []byte {
	key, err := runnerSigningKey()
	if err != nil {
		panic("runner protocol signing key unavailable: " + err.Error())
	}
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(data))
	return mac.Sum(nil)
}

// runnerScope names the repository or organization a runner credential acts
// for. Exactly one field is set.
type runnerScope struct {
	Repo string `json:"repo,omitempty"` // owner/repo
	Org  string `json:"org,omitempty"`
}

func (sc runnerScope) String() string {
	switch {
	case sc.Repo != "":
		return "repo:" + sc.Repo
	case sc.Org != "":
		return "org:" + sc.Org
	}
	return "unscoped"
}

func (sc runnerScope) empty() bool { return sc.Repo == "" && sc.Org == "" }

// coversRepo reports whether the scope entitles its holder to act for
// repoFullName. Repository names are case-insensitive on GitHub.
func (sc runnerScope) coversRepo(repoFullName string) bool {
	if repoFullName == "" {
		return false
	}
	if sc.Repo != "" {
		return strings.EqualFold(sc.Repo, repoFullName)
	}
	if sc.Org != "" {
		owner, _, ok := strings.Cut(repoFullName, "/")
		return ok && strings.EqualFold(sc.Org, owner)
	}
	return false
}

// runnerScopeFromRequest reads the scope a registration/JIT request acts for
// off its path parameters: {owner}/{repo} for the repository surface, {org}
// for the organization surface. The named target must exist — a runner
// credential for a repository that is not there would be a credential for
// nothing, and real GitHub answers 404.
func (s *Server) runnerScopeFromRequest(r *http.Request) (runnerScope, error) {
	owner, repo := r.PathValue("owner"), r.PathValue("repo")
	if owner != "" && repo != "" {
		found := s.store.GetRepo(owner, repo)
		if found == nil {
			return runnerScope{}, fmt.Errorf("repository %s/%s not found", owner, repo)
		}
		return runnerScope{Repo: found.FullName}, nil
	}
	if org := r.PathValue("org"); org != "" {
		if s.store.GetOrg(org) == nil {
			return runnerScope{}, fmt.Errorf("organization %s not found", org)
		}
		return runnerScope{Org: org}, nil
	}
	return runnerScope{}, fmt.Errorf("request path names neither a repository nor an organization")
}

// signedBlob encodes payload and appends its MAC, producing an opaque
// credential that cannot be constructed without the signing key.
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
	sig, err := base64urlDecode(sigPart)
	if err != nil {
		return fmt.Errorf("decode runner credential signature: %w", err)
	}
	if !hmac.Equal(sig, runnerMAC(encoded)) {
		return fmt.Errorf("invalid runner credential signature")
	}
	body, err := base64urlDecode(encoded)
	if err != nil {
		return fmt.Errorf("decode runner credential payload: %w", err)
	}
	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("parse runner credential payload: %w", err)
	}
	return nil
}

// runnerRegistrationClaims is the payload of the opaque one-shot token
// config.sh presents. Real GitHub's is "A" + a base64-ish blob; this one keeps
// that shape and carries its own scope and expiry so registration needs no
// server-side token registry.
type runnerRegistrationClaims struct {
	Scope   runnerScope `json:"scope"`
	Purpose string      `json:"purpose"`
	Exp     int64       `json:"exp"`
	Nonce   string      `json:"nonce"`
}

func newRunnerRegistrationToken(scope runnerScope, purpose string) (string, error) {
	if scope.empty() {
		return "", fmt.Errorf("runner registration token needs a repository or organization scope")
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
	if claims.Scope.empty() {
		return runnerRegistrationClaims{}, fmt.Errorf("runner registration token carries no scope")
	}
	if time.Now().After(time.Unix(claims.Exp, 0).Add(runnerClockSkew)) {
		return runnerRegistrationClaims{}, fmt.Errorf("runner registration token expired")
	}
	return claims, nil
}

// newAgentClientID mints the clientId the runner stores in its credentials and
// echoes back as the client_assertion issuer.
//
// It is a bare GUID because that is what the runner deserializes it into —
// anything else fails at configuration with a type conversion error, whatever
// the server thinks the field means. The agent's scope therefore lives on its
// record rather than inside this value.
//
// A GUID is guessable in a way a signed blob is not, and that is fine here:
// the clientId only names which agent's public key to verify against. What
// authenticates is the RSA signature over the assertion, which an attacker
// cannot produce without the runner's private key.
func newAgentClientID(scope runnerScope) (string, error) {
	if scope.empty() {
		return "", fmt.Errorf("agent clientId needs a repository or organization scope")
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
}

// makeJWT mints an HS256-signed bearer credential for sub. The runner parses
// it as a JWT to read its expiry; only this server can produce one, and only
// parseRunnerToken accepts one.
func makeJWT(sub, aud string) string {
	now := time.Now()
	payload, err := json.Marshal(runnerTokenClaims{
		Sub: sub,
		Iss: "bleephub",
		Aud: aud,
		Nbf: now.Add(-runnerClockSkew).Unix(),
		Exp: now.Add(runnerTokenTTL).Unix(),
		Scp: "Actions.Results:write Actions.Pipelines:read",
	})
	if err != nil {
		panic("encode runner bearer token: " + err.Error())
	}
	signing := base64url([]byte(`{"alg":"HS256","typ":"JWT"}`)) + "." + base64url(payload)
	return signing + "." + base64url(runnerMAC(signing))
}

// parseRunnerToken verifies a bearer credential's signature and time bounds
// before any claim is readable. Verification failure returns an error, never
// claims.
func parseRunnerToken(token string) (*runnerTokenClaims, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return nil, fmt.Errorf("malformed runner token: expected 3 parts")
	}
	headerBytes, err := base64urlDecode(parts[0])
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
	sig, err := base64urlDecode(parts[2])
	if err != nil {
		return nil, fmt.Errorf("decode runner token signature: %w", err)
	}
	if !hmac.Equal(sig, runnerMAC(parts[0]+"."+parts[1])) {
		return nil, fmt.Errorf("invalid runner token signature")
	}
	payloadBytes, err := base64urlDecode(parts[1])
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
	Agent  *Agent             // set for an agent session token
	Scope  runnerScope        // repository/organization the credential may act for
	Setup  bool               // the registration/removal token config.sh holds
}

// IsJobToken reports whether the caller presented a per-job runtime token
// rather than an agent session token.
func (p *runnerPrincipal) IsJobToken() bool {
	return p != nil && p.Claims != nil && p.Claims.Aud == runnerAudJob
}

const ctxRunner contextKey = "runner-principal"

// runnerFromContext returns the principal requireRunnerAuth verified, or nil
// on a route that is not gated.
func runnerFromContext(ctx context.Context) *runnerPrincipal {
	p, _ := ctx.Value(ctxRunner).(*runnerPrincipal)
	return p
}

// callerRunner resolves the verified runner behind a request: the principal
// requireRunnerAuth put in the context, or, when a handler is reached without
// it, the credential on the request itself. The handler never sees an
// unverified caller, so the authorization it performs does not depend on a
// route having been registered with the right decorator.
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

// actionArchiveUser is the user half of the basic credential the runner
// presents when it downloads an action archive. ActionManager.CreateAuthHeader
// builds `Basic base64("x-access-token:<token>")` out of the token the
// ActionDownloadInfo response handed it, so the token travels as the password.
const actionArchiveUser = "x-access-token"

// actionArchiveCredential reads the token off an action archive download.
// Unlike the rest of the runner protocol this request is made by a plain HTTP
// client rather than the runner's service stack, and it carries whatever the
// download info response named — never a bearer token it negotiated itself.
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

// authenticateRunner resolves the caller of a runner protocol route to a
// verified principal.
func (s *Server) authenticateRunner(r *http.Request) (*runnerPrincipal, error) {
	token, ok := bearerCredential(r)
	if !ok {
		return nil, fmt.Errorf("missing runner bearer token")
	}
	return s.runnerPrincipalForToken(token)
}

// runnerPrincipalForToken verifies a runner bearer credential and resolves the
// identity behind it. Every route reaches it, whichever header shape the
// credential arrived in, so a token admits exactly the same caller either way.
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
		if agent.Scope.empty() {
			return nil, fmt.Errorf("agent %d carries no scope", agent.ID)
		}
		return &runnerPrincipal{Claims: claims, Agent: agent, Scope: agent.Scope}, nil
	case runnerAudJob:
		repo, err := s.repoForJobScope(claims.Sub)
		if err != nil {
			return nil, err
		}
		return &runnerPrincipal{Claims: claims, Scope: runnerScope{Repo: repo}}, nil
	default:
		return nil, fmt.Errorf("unsupported runner token audience %q", claims.Aud)
	}
}

// challengeRunnerAuth refuses a runner protocol request that carries no usable
// credential, and names the scheme that would satisfy it.
//
// The WWW-Authenticate header is what drives the runner's token exchange. It
// opens every session by sending a request with no credential attached and
// acquires one only when the refusal is a challenge it recognizes: a 401
// carrying a WWW-Authenticate value containing "Bearer". A bare 401 is read as
// the route's own answer, and no token is ever requested.
func (s *Server) challengeRunnerAuth(w http.ResponseWriter, r *http.Request, err error) {
	s.logger.Debug().Err(err).Str("path", r.URL.Path).Msg("runner protocol request rejected")
	w.Header().Set("WWW-Authenticate", fmt.Sprintf("Bearer authorization_uri=%q", s.agentAuthorizationURL(r)))
	writeGHError(w, http.StatusUnauthorized, "Must authenticate to use the runner protocol")
}

// requireRunnerAuth gates a runner protocol route on a verified runner
// credential and hands the principal to the handler through the request
// context. Every /_apis/ and /twirp/ route registered outside the
// registerAuthRoutes allowlist goes through here.
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

// requireJobToken gates a route on a per-job runtime token — the credential
// the runner's worker holds while executing one job. An agent session token
// is not accepted: these routes act on a single job's plan.
func (s *Server) requireJobToken(next http.HandlerFunc) http.HandlerFunc {
	return s.requireRunnerAuth(func(w http.ResponseWriter, r *http.Request) {
		if !runnerFromContext(r.Context()).IsJobToken() {
			writeGHError(w, http.StatusForbidden, "This route requires a job runtime token")
			return
		}
		next(w, r)
	})
}

// requireActionArchiveToken gates an action archive download on the job
// runtime token the ActionDownloadInfo response handed the runner.
//
// The archive is fetched by a plain HTTP client that attaches the download
// info's token as basic auth under the x-access-token user, so that shape has
// to be read here or the runner cannot present the credential it was given at
// all. What is accepted is the same job runtime token as everywhere else,
// verified the same way: the encoding differs, the entitlement does not.
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

// requireRunnerSetupCredential gates a route the runner reaches during
// `config.sh`, before it has exchanged its client_assertion: the token
// config.sh was given is accepted for the named purposes, alongside the agent
// session a configured runner holds. Either way the handler is handed a
// principal carrying the verified scope, so the tenant confinement downstream
// is the same on both paths.
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

// requireAgentSession gates a route on an agent session token — the
// credential the runner listener holds. A job runtime token is not an agent.
func (s *Server) requireAgentSession(next http.HandlerFunc) http.HandlerFunc {
	return s.requireRunnerAuth(func(w http.ResponseWriter, r *http.Request) {
		if runnerFromContext(r.Context()).Agent == nil {
			writeGHError(w, http.StatusForbidden, "This route requires an agent session token")
			return
		}
		next(w, r)
	})
}
