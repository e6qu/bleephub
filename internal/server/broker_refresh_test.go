package bleephub

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/e6qu/bleephub/internal/store"
)

// TestAgentRefreshMessageDelivery verifies that the sim-control endpoint
// POST /internal/agents/{id}/refresh-message delivers a real
// AgentRefreshMessage to every open session for the target agent.
func TestAgentRefreshMessageDelivery(t *testing.T) {
	t.Parallel()
	srv := newIsolatedServer(t)
	agentID := 4242
	srv.store.Mu.Lock()
	srv.store.Agents[agentID] = &store.Agent{
		ID:      agentID,
		Name:    "refresh-runner",
		Version: "2.319.0",
		Status:  "online",
		Labels:  []store.Label{{Name: "self-hosted"}},
	}
	srv.store.Mu.Unlock()
	defer func() {
		srv.store.Mu.Lock()
		delete(srv.store.Agents, agentID)
		delete(srv.store.Sessions, "refresh-sess")
		srv.store.Mu.Unlock()
	}()

	sess := &store.Session{
		SessionID: "refresh-sess",
		Agent:     &store.Agent{ID: agentID, Labels: []store.Label{{Name: "self-hosted"}}},
		MsgCh:     make(chan *store.TaskAgentMessage, 10),
	}
	srv.store.Mu.Lock()
	srv.store.Sessions["refresh-sess"] = sess
	srv.store.Mu.Unlock()

	targetVersion := "2.320.0"
	resp := srv.post(t, fmt.Sprintf("/internal/agents/%d/refresh-message", agentID), defaultToken, map[string]interface{}{
		"targetVersion": targetVersion,
		"timeout":       "10m",
	})
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", resp.StatusCode)
	}
	resp.Body.Close()

	var msg *store.TaskAgentMessage
	select {
	case msg = <-sess.MsgCh:
	case <-time.After(2 * time.Second):
		t.Fatal("no AgentRefreshMessage delivered to runner session")
	}

	if msg.MessageType != "AgentRefreshMessage" {
		t.Fatalf("message type = %q, want AgentRefreshMessage", msg.MessageType)
	}
	var body struct {
		AgentID       int    `json:"agentId"`
		TargetVersion string `json:"targetVersion"`
		Timeout       string `json:"timeout"`
	}
	if err := json.Unmarshal([]byte(msg.Body), &body); err != nil {
		t.Fatalf("body unmarshal: %v (body=%s)", err, msg.Body)
	}
	if body.AgentID != agentID {
		t.Errorf("agentId = %d, want %d", body.AgentID, agentID)
	}
	if body.TargetVersion != targetVersion {
		t.Errorf("targetVersion = %q, want %q", body.TargetVersion, targetVersion)
	}
	if body.Timeout != "10m0s" {
		t.Errorf("timeout = %q, want 10m0s", body.Timeout)
	}
}

// TestAgentRefreshMessageAdminOnly verifies the refresh endpoint rejects
// non-admin callers.
func TestAgentRefreshMessageAdminOnly(t *testing.T) {
	t.Parallel()
	srv := newIsolatedServer(t)
	agentID := 4243
	srv.store.Mu.Lock()
	srv.store.Agents[agentID] = &store.Agent{ID: agentID, Name: "r"}
	srv.store.Mu.Unlock()
	defer func() {
		srv.store.Mu.Lock()
		delete(srv.store.Agents, agentID)
		srv.store.Mu.Unlock()
	}()

	// Create a non-admin user + token.
	nonAdmin := &store.User{ID: 9001, Login: "nobody", Type: "User"}
	srv.store.Mu.Lock()
	srv.store.Users[nonAdmin.ID] = nonAdmin
	tok := &store.Token{Value: "ghp_nonadmin", UserID: nonAdmin.ID, Scopes: "repo"}
	srv.store.Tokens[tok.Value] = tok
	srv.store.Mu.Unlock()
	defer func() {
		srv.store.Mu.Lock()
		delete(srv.store.Users, nonAdmin.ID)
		delete(srv.store.Tokens, tok.Value)
		srv.store.Mu.Unlock()
	}()

	resp := srv.post(t, fmt.Sprintf("/internal/agents/%d/refresh-message", agentID), tok.Value, map[string]interface{}{
		"targetVersion": "2.320.0",
	})
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("status = %d, want 403", resp.StatusCode)
	}
}

// TestAgentRefreshMessageAgentNotFound verifies 404 for unknown agents.
func TestAgentRefreshMessageAgentNotFound(t *testing.T) {
	t.Parallel()
	srv := newIsolatedServer(t)
	resp := srv.post(t, "/internal/agents/99999/refresh-message", defaultToken, map[string]interface{}{
		"targetVersion": "2.320.0",
	})
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
}
