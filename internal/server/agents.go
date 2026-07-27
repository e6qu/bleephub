package bleephub

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

func (s *Server) registerAgentRoutes() {
	// Registration token (for config.sh). Real GitHub gates this on
	// administration:write — 401/403 without it.
	s.route("POST /api/v3/repos/{owner}/{repo}/actions/runners/registration-token",
		s.requirePerm(scopeAdministration, permWrite, s.handleRegistrationToken))
	// Removal token (config.sh remove --token) — also administration:write.
	s.route("POST /api/v3/repos/{owner}/{repo}/actions/runners/remove-token",
		s.requirePerm(scopeAdministration, permWrite, s.handleRemoveToken))
	// JIT config (ephemeral just-in-time runner registration).
	s.route("POST /api/v3/repos/{owner}/{repo}/actions/runners/generate-jitconfig",
		s.requirePerm(scopeAdministration, permWrite, s.handleGenerateJITConfig))

	// Agent pools. config.sh reads the pool list with its registration token,
	// before it has a session.
	s.route("GET /_apis/v1/AgentPools", s.requireRunnerSetupCredential(s.handleListPools))

	// Agent CRUD — order matters: more specific patterns first.
	// Registration is the one runner-protocol route reached before an agent
	// session exists; it carries the registration token instead.
	s.route("POST /_apis/v1/Agent/{poolId}", s.handleRegisterAgent)
	s.route("GET /_apis/v1/Agent/{poolId}/{agentId}", s.requireAgentSession(s.handleGetAgent))
	s.route("PUT /_apis/v1/Agent/{poolId}/{agentId}", s.requireAgentSession(s.handleUpdateAgent))
	s.route("DELETE /_apis/v1/Agent/{poolId}/{agentId}", s.requireAgentSession(s.handleDeleteAgent))
	s.route("GET /_apis/v1/Agent/{poolId}", s.requireAgentSession(s.handleListAgents))
}

// randomRunnerToken mints the unguessable component of a runner
// registration/removal token, in the shape real GitHub's opaque token has
// ("A" + base64-ish blob). newRunnerRegistrationToken binds it to a scope and
// signs the pair, so two tokens for the same scope are never equal and a
// leaked one cannot be replayed past its expiry.
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

// mintRunnerToken issues the opaque registration/removal token in the shape
// real GitHub returns ("A" + base64-ish blob), scoped to the repository or
// organization the request addresses. The token is signed, so the scope it
// carries survives the round-trip through config.sh without a server-side
// registry, and a forged one cannot register a runner.
func (s *Server) mintRunnerToken(w http.ResponseWriter, r *http.Request, purpose string) {
	scope, err := s.runnerScopeFromRequest(r)
	if err != nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	token, err := newRunnerRegistrationToken(scope, purpose)
	if err != nil {
		writeGHError(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.logger.Info().Str("scope", scope.String()).Str("purpose", purpose).Msg("runner token issued")
	// Real GitHub: 201 Created, opaque token, ~1h TTL.
	writeJSON(w, http.StatusCreated, map[string]interface{}{
		"token":      token,
		"expires_at": time.Now().Add(runnerRegistrationTTL).UTC().Format(time.RFC3339),
	})
}

func (s *Server) handleRegistrationToken(w http.ResponseWriter, r *http.Request) {
	s.mintRunnerToken(w, r, runnerPurposeRegistration)
}

// handleRemoveToken mints a runner removal token (config.sh remove --token).
// Same opaque-token shape and TTL as the registration token, with a purpose
// that cannot register a runner.
func (s *Server) handleRemoveToken(w http.ResponseWriter, r *http.Request) {
	s.mintRunnerToken(w, r, runnerPurposeRemoval)
}

// handleGenerateJITConfig mints a just-in-time runner config for an
// ephemeral runner. Real GitHub: 201 with {runner, encoded_jit_config}
// where encoded_jit_config is a base64-encoded JSON blob the runner
// consumes via `Runner.Listener --jitconfig <blob>`. 422 when name /
// runner_group_id / labels are missing.
func (s *Server) handleGenerateJITConfig(w http.ResponseWriter, r *http.Request) {
	// The target has to exist before the body is worth validating: a
	// fabricated repository answers 404, not 422.
	scope, err := s.runnerScopeFromRequest(r)
	if err != nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
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
		writeGHValidationError(w, "Runner", "name", "missing_field")
		return
	}
	workFolder := req.WorkFolder
	if workFolder == "" {
		workFolder = "_work"
	}

	// Build the Agent the same way handleRegisterAgent does, so the
	// JIT runner appears in the runners list and the broker can route to
	// it. JIT runners are always ephemeral (auto-removed after one job).
	var agent Agent
	agent.Name = req.Name
	agent.Ephemeral = true
	agent.RunnerGroupID = *req.RunnerGroupID
	for _, l := range req.Labels {
		agent.Labels = append(agent.Labels, Label{Name: l, Type: "custom"})
	}

	jitScope := coalesceStr(scope.Repo, scope.Org)
	clientID, err := newAgentClientID(scope)
	if err != nil {
		writeGHError(w, http.StatusInternalServerError, err.Error())
		return
	}

	s.store.mu.Lock()
	agent.ID = s.store.NextAgent
	s.store.NextAgent++
	agent.Enabled = true
	agent.Status = "online"
	agent.CreatedOn = time.Now()
	agent.Authorization = &AgentAuthorization{
		AuthorizationURL: s.baseURL(r) + "/_apis/v1/auth/",
		ClientID:         clientID,
	}
	s.store.Agents[agent.ID] = &agent
	s.store.mu.Unlock()

	// encoded_jit_config: the base64 of the JSON config blob the runner's
	// JIT listener reads. It carries the agent identity + server URL +
	// auth so the runner can connect without a separate config.sh step.
	jitBlob := map[string]interface{}{
		".runner": map[string]interface{}{
			"agentId":    agent.ID,
			"agentName":  agent.Name,
			"poolId":     1,
			"poolName":   "Default",
			"serverUrl":  s.baseURL(r),
			"gitHubUrl":  s.baseURL(r) + "/" + jitScope,
			"workFolder": workFolder,
			"ephemeral":  true,
		},
		".credentials": map[string]interface{}{
			"scheme": "OAuth",
			"data": map[string]interface{}{
				"clientId":         agent.Authorization.ClientID,
				"authorizationUrl": agent.Authorization.AuthorizationURL,
			},
		},
	}
	blobBytes, err := json.Marshal(jitBlob)
	if err != nil {
		writeGHError(w, http.StatusInternalServerError, "encode jit config")
		return
	}
	encoded := base64.StdEncoding.EncodeToString(blobBytes)

	s.logger.Info().Int("id", agent.ID).Str("name", agent.Name).Msg("JIT runner config generated")
	writeJSON(w, http.StatusCreated, map[string]interface{}{
		"runner":             runnerJSON(&agent, false),
		"encoded_jit_config": encoded,
	})
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

// handleRegisterAgent adds a runner to the pool. This is the one runner
// protocol route reached before an agent session exists, so it authenticates
// on the registration token config.sh was given rather than on a bearer
// session token; the scope that token carries becomes the agent's.
func (s *Server) handleRegisterAgent(w http.ResponseWriter, r *http.Request) {
	scope, err := s.runnerRegistrationCredential(r)
	if err != nil {
		s.logger.Warn().Err(err).Msg("agent registration rejected")
		writeGHError(w, http.StatusUnauthorized, "Invalid runner registration token")
		return
	}

	// Parse as generic JSON (runner sends extra fields not in our Agent struct)
	var raw map[string]interface{}
	if err := json.NewDecoder(r.Body).Decode(&raw); err != nil {
		s.logger.Error().Err(err).Msg("failed to parse agent registration")
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return
	}

	var agent Agent
	if name, ok := raw["name"].(string); ok {
		agent.Name = name
	}
	if ver, ok := raw["version"].(string); ok {
		agent.Version = ver
	}
	if desc, ok := raw["osDescription"].(string); ok {
		agent.OSDescription = desc
	}
	// Round-trip the ephemeral flag — config.sh --ephemeral checks the
	// REGISTRATION RESPONSE for it and aborts with "The GitHub server
	// does not support configuring a self-hosted runner with
	// 'Ephemeral' flag" when the server drops it.
	if eph, ok := raw["ephemeral"].(bool); ok {
		agent.Ephemeral = eph
	}
	if labelsRaw, ok := raw["labels"].([]interface{}); ok {
		for _, l := range labelsRaw {
			if lm, ok := l.(map[string]interface{}); ok {
				label := Label{}
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
	// Preserve authorization (RSA public key) from the runner
	if authRaw, ok := raw["authorization"].(map[string]interface{}); ok {
		agent.Authorization = &AgentAuthorization{}
		if pk, ok := authRaw["publicKey"].(map[string]interface{}); ok {
			agent.Authorization.PublicKey = &AgentPublicKey{}
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

	s.store.mu.Lock()
	agent.ID = s.store.NextAgent
	s.store.NextAgent++
	agent.Enabled = true
	agent.Status = "online"
	agent.CreatedOn = time.Now()

	if agent.Authorization == nil {
		agent.Authorization = &AgentAuthorization{}
	}
	agent.Authorization.AuthorizationURL = s.baseURL(r) + "/_apis/v1/auth/"
	agent.Authorization.ClientID = clientID

	s.store.Agents[agent.ID] = &agent
	s.store.mu.Unlock()

	s.logger.Info().Int("id", agent.ID).Str("name", agent.Name).Str("scope", scope.String()).Msg("agent registered")
	writeJSON(w, http.StatusOK, &agent)
}

// LookupAgentByClientID returns the agent whose Authorization.ClientID matches,
// or nil if no agent has registered with that ClientID. Agent count is bounded
// by the number of registered runners, so the linear scan is fine.
func (st *Store) LookupAgentByClientID(clientID string) *Agent {
	if clientID == "" {
		return nil
	}
	st.mu.RLock()
	defer st.mu.RUnlock()
	for _, a := range st.Agents {
		if a.Authorization != nil && a.Authorization.ClientID == clientID {
			return a
		}
	}
	return nil
}

func (s *Server) handleListAgents(w http.ResponseWriter, r *http.Request) {
	nameFilter := r.URL.Query().Get("agentName")
	caller, _ := s.callerRunner(r)

	s.store.mu.RLock()
	agents := make([]*Agent, 0)
	for _, a := range s.store.Agents {
		if nameFilter != "" && !strings.EqualFold(a.Name, nameFilter) {
			continue
		}
		if !callerSeesAgent(caller, a) {
			continue
		}
		agents = append(agents, a)
	}
	s.store.mu.RUnlock()

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"count": len(agents),
		"value": agents,
	})
}

// callerSeesAgent reports whether a runner credential may address an agent
// record. A runner manages its own registration; the fleet-wide view lives on
// the administration:write-gated REST runners API, not here. Agents outside
// the caller's scope are invisible rather than forbidden so an agent id cannot
// be probed for existence across tenants.
func callerSeesAgent(caller *runnerPrincipal, agent *Agent) bool {
	if caller == nil || caller.Agent == nil || agent == nil {
		return false
	}
	if caller.Agent.ID == agent.ID {
		return true
	}
	if agent.Authorization == nil {
		return false
	}
	scope, err := agentClientIDScope(agent.Authorization.ClientID)
	if err != nil {
		return false
	}
	return scope == caller.Scope
}

func (s *Server) handleGetAgent(w http.ResponseWriter, r *http.Request) {
	agentID, err := strconv.Atoi(r.PathValue("agentId"))
	if err != nil {
		http.Error(w, "invalid agent ID", http.StatusBadRequest)
		return
	}
	caller, _ := s.callerRunner(r)

	s.store.mu.RLock()
	agent, ok := s.store.Agents[agentID]
	s.store.mu.RUnlock()

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

	var update Agent
	if !decodeJSONBody(w, r, &update) {
		return
	}

	s.store.mu.Lock()
	agent, ok := s.store.Agents[agentID]
	if !ok || !callerSeesAgent(caller, agent) {
		s.store.mu.Unlock()
		http.Error(w, "agent not found", http.StatusNotFound)
		return
	}
	update.ID = agent.ID
	// The registration-time authorization is server state: its clientId is the
	// agent's identity and carries its scope, so an update never restates it.
	update.Authorization = agent.Authorization
	update.CreatedOn = agent.CreatedOn
	s.store.Agents[agentID] = &update
	s.store.mu.Unlock()

	writeJSON(w, http.StatusOK, &update)
}

func (s *Server) handleDeleteAgent(w http.ResponseWriter, r *http.Request) {
	agentID, err := strconv.Atoi(r.PathValue("agentId"))
	if err != nil {
		http.Error(w, "invalid agent ID", http.StatusBadRequest)
		return
	}

	caller, _ := s.callerRunner(r)

	s.store.mu.Lock()
	agent, ok := s.store.Agents[agentID]
	ok = ok && callerSeesAgent(caller, agent)
	if ok {
		delete(s.store.Agents, agentID)
	}
	s.store.mu.Unlock()

	if !ok {
		http.Error(w, "agent not found", http.StatusNotFound)
		return
	}
	s.logger.Info().Int("id", agentID).Msg("agent unregistered")
	w.WriteHeader(http.StatusOK)
}

// removeEphemeralAgent deregisters an agent that registered with the
// ephemeral flag once its single job has finished — mirroring real
// GitHub, which auto-removes ephemeral runners after one job so the
// registration doesn't linger as an offline zombie.
func (s *Server) removeEphemeralAgent(agentID int) {
	if agentID == 0 {
		return
	}
	s.store.mu.Lock()
	agent, ok := s.store.Agents[agentID]
	if !ok || !agent.Ephemeral {
		s.store.mu.Unlock()
		return
	}
	delete(s.store.Agents, agentID)
	s.store.mu.Unlock()
	s.logger.Info().Int("id", agentID).Str("name", agent.Name).Msg("ephemeral agent deregistered after job completion")
}
