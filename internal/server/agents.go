package bleephub

import (
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/e6qu/bleephub/internal/store"
)

func (s *Server) registerAgentRoutes() {
	// Registration token for config.sh.
	s.route("POST /api/v3/repos/{owner}/{repo}/actions/runners/registration-token",
		s.requirePerm(store.ScopeAdministration, store.PermWrite, s.handleRegistrationToken))
	// Removal token (config.sh remove --token).
	s.route("POST /api/v3/repos/{owner}/{repo}/actions/runners/remove-token",
		s.requirePerm(store.ScopeAdministration, store.PermWrite, s.handleRemoveToken))
	// JIT config for an ephemeral runner.
	s.route("POST /api/v3/repos/{owner}/{repo}/actions/runners/generate-jitconfig",
		s.requirePerm(store.ScopeAdministration, store.PermWrite, s.handleGenerateJITConfig))

	// config.sh reads the pool list with its registration token, before any session exists.
	s.route("GET /_apis/v1/AgentPools", s.requireRunnerSetupCredential(runnerPurposesRegister, s.handleListPools))

	// Order matters: more specific patterns first. Most of this surface is
	// reached before an agent session exists — it is how a runner configures
	// itself — so each route authenticates on the config.sh token whose purpose
	// it names. Only GET of one agent's record is session-only.
	s.route("POST /_apis/v1/Agent/{poolId}", s.handleRegisterAgent)
	s.route("GET /_apis/v1/Agent/{poolId}/{agentId}", s.requireAgentSession(s.handleGetAgent))
	s.route("PUT /_apis/v1/Agent/{poolId}/{agentId}", s.requireRunnerSetupCredential(runnerPurposesRegister, s.handleUpdateAgent))
	s.route("DELETE /_apis/v1/Agent/{poolId}/{agentId}", s.requireRunnerSetupCredential(runnerPurposesRemove, s.handleDeleteAgent))
	s.route("GET /_apis/v1/Agent/{poolId}", s.requireRunnerSetupCredential(runnerPurposesConfig, s.handleListAgents))
}

// randomRunnerToken mints the unguessable component of a runner
// registration/removal token, in real GitHub's opaque shape ("A" + base64 blob).
func randomRunnerToken() (string, error) {
	return randomRunnerTokenFromReader(rand.Reader)
}

func randomRunnerTokenFromReader(random io.Reader) (string, error) {
	b := make([]byte, 30)
	if _, err := io.ReadFull(random, b); err != nil {
		return "", fmt.Errorf("generate runner token: %w", err)
	}
	return "A" + base64.RawURLEncoding.EncodeToString(b), nil
}

// mintRunnerToken issues the opaque registration/removal token scoped to the
// repository, org, or enterprise the request addresses. The token is signed, so
// its scope survives the round-trip through config.sh without a server-side
// registry and a forged one cannot register a runner.
func (s *Server) mintRunnerToken(w http.ResponseWriter, r *http.Request, purpose string) {
	scope, err := s.runnerScopeFromRequest(r)
	if err != nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	if purpose == runnerPurposeRegistration && !s.repositoryRunnerRegistrationAllowed(scope) {
		writeGHError(w, http.StatusForbidden, "Repository-level self-hosted runners are disabled by policy.")
		return
	}
	token, err := newRunnerRegistrationToken(scope, purpose)
	if err != nil {
		writeGHError(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.logger.Info().Str("scope", scope.String()).Str("purpose", purpose).Msg("runner token issued")
	// GitHub answers 201 with an opaque token and ~1h TTL.
	writeJSON(w, http.StatusCreated, map[string]interface{}{
		"token":      token,
		"expires_at": time.Now().Add(runnerRegistrationTTL).UTC().Format(time.RFC3339),
	})
}

func (s *Server) handleRegistrationToken(w http.ResponseWriter, r *http.Request) {
	s.mintRunnerToken(w, r, runnerPurposeRegistration)
}

// handleRemoveToken mints a runner removal token whose purpose cannot register
// a runner.
func (s *Server) handleRemoveToken(w http.ResponseWriter, r *http.Request) {
	s.mintRunnerToken(w, r, runnerPurposeRemoval)
}

// handleGenerateJITConfig mints a just-in-time config for an ephemeral runner.
// GitHub answers 201 with {runner, encoded_jit_config}, or 422 when name /
// runner_group_id / labels are missing.
func (s *Server) handleGenerateJITConfig(w http.ResponseWriter, r *http.Request) {
	// A missing target answers 404 before body validation, not 422.
	scope, err := s.runnerScopeFromRequest(r)
	if err != nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	if !s.repositoryRunnerRegistrationAllowed(scope) {
		writeGHError(w, http.StatusForbidden, "Repository-level self-hosted runners are disabled by policy.")
		return
	}

	var req struct {
		Name          string   `json:"name"`
		RunnerGroupID *int     `json:"runner_group_id"`
		Labels        []string `json:"labels"`
		WorkFolder    string   `json:"work_folder"`
	}
	if !decodeJSONBody(w, r, &req) {
		return
	}
	if req.Name == "" || req.RunnerGroupID == nil || len(req.Labels) == 0 {
		writeGHValidationErrorSimple(w, "name is missing")
		return
	}
	workFolder := req.WorkFolder
	if workFolder == "" {
		workFolder = "_work"
	}

	// JIT runners are always ephemeral (auto-removed after one job).
	var agent store.Agent
	agent.Name = req.Name
	agent.Ephemeral = true
	agent.RunnerGroupID = *req.RunnerGroupID
	for _, l := range req.Labels {
		agent.Labels = append(agent.Labels, store.Label{Name: l, Type: "custom"})
	}

	jitScope := store.CoalesceStr(store.CoalesceStr(scope.Repo, scope.Org), scope.Enterprise)
	clientID, err := newAgentClientID(scope)
	if err != nil {
		writeGHError(w, http.StatusInternalServerError, err.Error())
		return
	}
	publicKey, privateParams, err := newJITRunnerKey()
	if err != nil {
		writeGHError(w, http.StatusInternalServerError, err.Error())
		return
	}

	s.store.Mu.Lock()
	agent.ID = s.store.NextAgent
	s.store.NextAgent++
	agent.Enabled = true
	agent.Status = "online"
	agent.CreatedOn = time.Now()
	agent.Scope = scope
	agent.Authorization = &store.AgentAuthorization{
		AuthorizationURL: s.agentAuthorizationURL(r),
		ClientID:         clientID,
		PublicKey:        publicKey,
	}
	s.store.Agents[agent.ID] = &agent
	s.store.Mu.Unlock()

	encoded, err := encodeJITConfig(map[string]interface{}{
		".runner": map[string]interface{}{
			"agentId":   agent.ID,
			"agentName": agent.Name,
			"poolId":    1,
			"poolName":  "Default",
			"serverUrl": s.baseURL(r),
			"gitHubUrl": s.baseURL(r) + "/" + jitScope,
			// The runner reads this to decide where the github context's
			// server_url comes from; bleephub is never the hosted service.
			"isHostedServer": false,
			"workFolder":     workFolder,
			"ephemeral":      true,
		},
		".credentials": map[string]interface{}{
			"scheme": "OAuth",
			"data": map[string]interface{}{
				"clientId":         agent.Authorization.ClientID,
				"authorizationUrl": agent.Authorization.AuthorizationURL,
			},
		},
		".credentials_rsaparams": privateParams,
	})
	if err != nil {
		writeGHError(w, http.StatusInternalServerError, err.Error())
		return
	}

	s.logger.Info().Int("id", agent.ID).Str("name", agent.Name).Msg("JIT runner config generated")
	writeJSON(w, http.StatusCreated, map[string]interface{}{
		"runner":             runnerJSON(&agent, false),
		"encoded_jit_config": encoded,
	})
}

func (s *Server) repositoryRunnerRegistrationAllowed(scope store.RunnerScope) bool {
	if scope.Repo == "" {
		return true
	}
	owner, repoName := splitRepoFull(scope.Repo)
	repo := s.store.GetRepo(owner, repoName)
	if repo == nil || repo.OwnerType != "Organization" {
		return true
	}
	s.store.Mu.RLock()
	defer s.store.Mu.RUnlock()
	if s.store.EnterpriseSettings.ActionsDisableSelfHostedRunners {
		return false
	}
	policy := s.store.LookupOrgActionsPermissionsLocked(owner)
	if policy == nil {
		return true
	}
	switch policy.SelfHostedRunnersEnabledRepositories {
	case "none":
		return false
	case "selected":
		return slices.Contains(policy.SelfHostedRunnersSelectedRepoIDs, repo.ID)
	default:
		return true
	}
}

// encodeJITConfig renders the JIT configuration. The runner deserializes the
// decoded blob as a map of file name to the base64 of that file's contents, so
// each section must be a base64 file body, not a nested object.
func encodeJITConfig(files map[string]interface{}) (string, error) {
	blob := make(map[string]string, len(files))
	for name, contents := range files {
		body, err := json.Marshal(contents)
		if err != nil {
			return "", fmt.Errorf("encode jit config file %s: %w", name, err)
		}
		blob[name] = base64.StdEncoding.EncodeToString(body)
	}
	encoded, err := json.Marshal(blob)
	if err != nil {
		return "", fmt.Errorf("encode jit config: %w", err)
	}
	return base64.StdEncoding.EncodeToString(encoded), nil
}

// rsaParametersJSON is .NET's RSAParameters as the runner persists it in
// `.credentials_rsaparams`. Every component is fixed-width: .NET rejects a set
// whose private exponent is not exactly the modulus length or whose CRT factors
// are not half of it, so leading zero bytes are kept, not trimmed as big.Int would.
type rsaParametersJSON struct {
	D        []byte `json:"d"`
	DP       []byte `json:"dp"`
	DQ       []byte `json:"dq"`
	Exponent []byte `json:"exponent"`
	InverseQ []byte `json:"inverseQ"`
	Modulus  []byte `json:"modulus"`
	P        []byte `json:"p"`
	Q        []byte `json:"q"`
}

// newJITRunnerKey mints the RSA key pair a JIT runner authenticates with:
// the public half is recorded on the agent, the private half handed over in the
// JIT config. The runner generates no key on this path, so an agent record
// without a public key can never have its client_assertion verified.
func newJITRunnerKey() (*store.AgentPublicKey, *rsaParametersJSON, error) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return nil, nil, fmt.Errorf("generate JIT runner key: %w", err)
	}
	key.Precompute()

	size := key.Size()
	half := (key.Primes[0].BitLen() + 7) / 8
	exponent := big.NewInt(int64(key.E)).Bytes()
	params := &rsaParametersJSON{
		D:        key.D.FillBytes(make([]byte, size)),
		DP:       key.Precomputed.Dp.FillBytes(make([]byte, half)),
		DQ:       key.Precomputed.Dq.FillBytes(make([]byte, half)),
		Exponent: exponent,
		InverseQ: key.Precomputed.Qinv.FillBytes(make([]byte, half)),
		Modulus:  key.N.FillBytes(make([]byte, size)),
		P:        key.Primes[0].FillBytes(make([]byte, half)),
		Q:        key.Primes[1].FillBytes(make([]byte, half)),
	}
	public := &store.AgentPublicKey{
		Exponent: base64.StdEncoding.EncodeToString(exponent),
		Modulus:  base64.StdEncoding.EncodeToString(params.Modulus),
	}
	return public, params, nil
}

func (s *Server) handleListPools(w http.ResponseWriter, r *http.Request) {
	s.logger.Debug().Msg("list agent pools")
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"count": 1,
		"value": []map[string]interface{}{
			{"id": 1, "name": "Default", "size": 0, "isHosted": false, "poolType": "automation"},
		},
	})
}

// handleRegisterAgent adds a runner to the pool. It authenticates on the
// registration token config.sh was given (no session exists yet); that token's
// scope becomes the agent's.
func (s *Server) handleRegisterAgent(w http.ResponseWriter, r *http.Request) {
	claims, err := runnerSetupCredential(r, runnerPurposesRegister)
	if err != nil {
		s.logger.Warn().Err(err).Msg("agent registration rejected")
		writeGHError(w, http.StatusUnauthorized, "Invalid runner registration token")
		return
	}
	scope := claims.Scope

	// The runner sends extra fields not in our Agent struct.
	var raw map[string]interface{}
	if err := json.NewDecoder(r.Body).Decode(&raw); err != nil {
		s.logger.Error().Err(err).Msg("failed to parse agent registration")
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return
	}

	var agent store.Agent
	if name, ok := raw["name"].(string); ok {
		agent.Name = name
	}
	if ver, ok := raw["version"].(string); ok {
		agent.Version = ver
	}
	if desc, ok := raw["osDescription"].(string); ok {
		agent.OSDescription = desc
	}
	// config.sh --ephemeral checks the registration response for this flag and
	// aborts if the server drops it.
	if eph, ok := raw["ephemeral"].(bool); ok {
		agent.Ephemeral = eph
	}
	if labelsRaw, ok := raw["labels"].([]interface{}); ok {
		for _, l := range labelsRaw {
			if lm, ok := l.(map[string]interface{}); ok {
				label := store.Label{}
				if n, ok := lm["name"].(string); ok {
					label.Name = n
				}
				if t, ok := lm["type"].(string); ok {
					label.Type = t
				}
				agent.Labels = append(agent.Labels, label)
			}
		}
	}
	if authRaw, ok := raw["authorization"].(map[string]interface{}); ok {
		agent.Authorization = &store.AgentAuthorization{}
		if pk, ok := authRaw["publicKey"].(map[string]interface{}); ok {
			agent.Authorization.PublicKey = &store.AgentPublicKey{}
			if exp, ok := pk["exponent"].(string); ok {
				agent.Authorization.PublicKey.Exponent = exp
			}
			if mod, ok := pk["modulus"].(string); ok {
				agent.Authorization.PublicKey.Modulus = mod
			}
		}
	}

	clientID, err := newAgentClientID(scope)
	if err != nil {
		writeGHError(w, http.StatusInternalServerError, err.Error())
		return
	}

	s.store.Mu.Lock()
	agent.ID = s.store.NextAgent
	s.store.NextAgent++
	agent.Enabled = true
	agent.Status = "online"
	agent.CreatedOn = time.Now()

	if agent.Authorization == nil {
		agent.Authorization = &store.AgentAuthorization{}
	}
	agent.Scope = scope
	agent.Authorization.AuthorizationURL = s.agentAuthorizationURL(r)
	agent.Authorization.ClientID = clientID

	s.store.Agents[agent.ID] = &agent
	s.store.Mu.Unlock()

	s.logger.Info().Int("id", agent.ID).Str("name", agent.Name).Str("scope", scope.String()).Msg("agent registered")
	writeJSON(w, http.StatusOK, &agent)
}

func (s *Server) handleListAgents(w http.ResponseWriter, r *http.Request) {
	nameFilter := r.URL.Query().Get("agentName")
	caller, _ := s.callerRunner(r)

	s.store.Mu.RLock()
	agents := make([]*store.Agent, 0)
	for _, a := range s.store.Agents {
		if nameFilter != "" && !strings.EqualFold(a.Name, nameFilter) {
			continue
		}
		if !callerSeesAgent(caller, a) {
			continue
		}
		agents = append(agents, a)
	}
	s.store.Mu.RUnlock()

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"count": len(agents),
		"value": agents,
	})
}

// callerSeesAgent reports whether a runner credential may address an agent
// record: only the runner itself or the config-time token that created it (a
// per-job runtime token sees nothing). Out-of-scope agents are invisible rather
// than forbidden, so an agent id cannot be probed across tenants. An agent
// without an authorization has no identity and is addressable by nobody.
func callerSeesAgent(caller *runnerPrincipal, agent *store.Agent) bool {
	if caller == nil || agent == nil || agent.Authorization == nil {
		return false
	}
	if caller.Agent == nil && !caller.Setup {
		return false
	}
	if caller.Agent != nil && caller.Agent.ID == agent.ID {
		return true
	}
	if agent.Scope.Empty() {
		return false
	}
	return agent.Scope == caller.Scope
}

func (s *Server) handleGetAgent(w http.ResponseWriter, r *http.Request) {
	agentID, err := strconv.Atoi(r.PathValue("agentId"))
	if err != nil {
		http.Error(w, "invalid agent ID", http.StatusBadRequest)
		return
	}
	caller, _ := s.callerRunner(r)

	s.store.Mu.RLock()
	agent, ok := s.store.Agents[agentID]
	s.store.Mu.RUnlock()

	if !ok || !callerSeesAgent(caller, agent) {
		http.Error(w, "agent not found", http.StatusNotFound)
		return
	}
	writeJSON(w, http.StatusOK, agent)
}

func (s *Server) handleUpdateAgent(w http.ResponseWriter, r *http.Request) {
	agentID, err := strconv.Atoi(r.PathValue("agentId"))
	if err != nil {
		http.Error(w, "invalid agent ID", http.StatusBadRequest)
		return
	}
	caller, _ := s.callerRunner(r)

	var update store.Agent
	if !decodeJSONBody(w, r, &update) {
		return
	}

	s.store.Mu.Lock()
	agent, ok := s.store.Agents[agentID]
	if !ok || !callerSeesAgent(caller, agent) {
		s.store.Mu.Unlock()
		http.Error(w, "agent not found", http.StatusNotFound)
		return
	}
	update.ID = agent.ID
	// clientId and scope are the agent's identity, not the caller's to restate,
	// so carry them over. The public key does change: `config.sh --replace`
	// signs its next client_assertion with a fresh key pair, so keeping the old
	// key would lock the re-registered runner out of a session.
	update.Scope = agent.Scope
	authorization := *agent.Authorization
	if update.Authorization != nil && update.Authorization.PublicKey != nil {
		authorization.PublicKey = update.Authorization.PublicKey
	}
	update.Authorization = &authorization
	update.CreatedOn = agent.CreatedOn
	s.store.Agents[agentID] = &update
	s.store.Mu.Unlock()

	writeJSON(w, http.StatusOK, &update)
}

func (s *Server) handleDeleteAgent(w http.ResponseWriter, r *http.Request) {
	agentID, err := strconv.Atoi(r.PathValue("agentId"))
	if err != nil {
		http.Error(w, "invalid agent ID", http.StatusBadRequest)
		return
	}

	caller, _ := s.callerRunner(r)

	s.store.Mu.Lock()
	agent, ok := s.store.Agents[agentID]
	ok = ok && callerSeesAgent(caller, agent)
	if ok {
		delete(s.store.Agents, agentID)
	}
	s.store.Mu.Unlock()

	if !ok {
		http.Error(w, "agent not found", http.StatusNotFound)
		return
	}
	s.logger.Info().Int("id", agentID).Msg("agent unregistered")
	w.WriteHeader(http.StatusOK)
}

// removeEphemeralAgent deregisters an ephemeral agent once its single job has
// finished, as GitHub auto-removes ephemeral runners after one job.
func (s *Server) removeEphemeralAgent(agentID int) {
	if agentID == 0 {
		return
	}
	s.store.Mu.Lock()
	agent, ok := s.store.Agents[agentID]
	if !ok || !agent.Ephemeral {
		s.store.Mu.Unlock()
		return
	}
	delete(s.store.Agents, agentID)
	s.store.Mu.Unlock()
	s.logger.Info().Int("id", agentID).Str("name", agent.Name).Msg("ephemeral agent deregistered after job completion")
}
