package bleephub

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/google/uuid"
)

const messagePollTimeout = 30 * time.Second

func (s *Server) registerBrokerRoutes() {
	// Sessions
	s.route("POST /_apis/v1/AgentSession/{poolId}", s.requireAgentSession(s.handleCreateSession))
	s.route("DELETE /_apis/v1/AgentSession/{poolId}/{sessionId}", s.requireAgentSession(s.handleDeleteSession))

	// Message polling
	s.route("GET /_apis/v1/Message/{poolId}", s.requireAgentSession(s.handleGetMessage))
	s.route("DELETE /_apis/v1/Message/{poolId}/{messageId}", s.requireAgentSession(s.handleDeleteMessage))
}

func (s *Server) handleCreateSession(w http.ResponseWriter, r *http.Request) {
	// The registered agent behind the session token is the routing source of
	// truth: it holds the labels from config-time registration and the scope
	// that decides which jobs — and therefore which secrets — it may receive.
	caller, err := s.callerRunner(r)
	if err == nil && caller.Agent == nil {
		err = fmt.Errorf("opening a session needs an agent session token, not a job runtime token")
	}
	if err != nil {
		s.challengeRunnerAuth(w, r, err)
		return
	}
	agent := caller.Agent

	var raw map[string]interface{}
	if err := json.NewDecoder(r.Body).Decode(&raw); err != nil {
		s.logger.Error().Err(err).Msg("failed to parse session request")
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return
	}

	ownerName, _ := raw["ownerName"].(string)

	// A session request naming a different agent is refused rather than
	// quietly rebound to the authenticated one.
	if agentRaw, ok := raw["agent"].(map[string]interface{}); ok {
		if id, ok := agentRaw["id"].(float64); ok && int(id) != agent.ID {
			writeGHError(w, http.StatusForbidden, "Session agent does not match the authenticated runner")
			return
		}
	}

	sessionID := uuid.New().String()
	session := &Session{
		SessionID: sessionID,
		OwnerName: ownerName,
		Agent:     agent,
		MsgCh:     make(chan *TaskAgentMessage, 10),
	}

	s.store.Mu.Lock()
	s.store.Sessions[sessionID] = session
	s.store.Mu.Unlock()

	if s.metrics != nil {
		s.metrics.SetActiveSessions(int64(s.actions.SessionCount()))
	}

	s.logger.Info().Str("sessionId", sessionID).Msg("session created")

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"sessionId":     sessionID,
		"ownerName":     ownerName,
		"agent":         agent,
		"encryptionKey": nil,
	})
}

func (s *Server) handleDeleteSession(w http.ResponseWriter, r *http.Request) {
	sessionID := r.PathValue("sessionId")
	caller, _ := s.callerRunner(r)

	s.store.Mu.Lock()
	session, ok := s.store.Sessions[sessionID]
	ok = ok && sessionOwnedBy(session, caller)
	var retiring int
	if ok {
		close(session.MsgCh)
		delete(s.store.Sessions, sessionID)
		if session.Agent != nil {
			retiring = session.Agent.ID
		}
	}
	s.store.Mu.Unlock()

	// Deleting its session is the last thing a runner does. An ephemeral
	// runner is deregistered here rather than the moment its job finished,
	// because a registration removed mid-teardown cannot authenticate the
	// calls the runner has still to make — closing its job request and this
	// session. pullPendingMessage is what holds it to one job.
	s.removeEphemeralAgent(retiring)

	if s.metrics != nil {
		s.metrics.SetActiveSessions(int64(s.actions.SessionCount()))
	}

	s.logger.Info().Str("sessionId", sessionID).Bool("found", ok).Msg("session deleted")
	w.WriteHeader(http.StatusOK)
}

// sessionOwnedBy reports whether the runner credential on a request owns the
// session it addresses. Sessions are keyed by an id the runner echoes back;
// without this the id alone would be the credential.
func sessionOwnedBy(session *Session, caller *runnerPrincipal) bool {
	if session == nil || caller == nil || caller.Agent == nil {
		return false
	}
	return session.Agent != nil && session.Agent.ID == caller.Agent.ID
}

// handleGetMessage long-polls for a job message (30s timeout). Queued
// pending messages are PULLED here rather than pushed: a runner polls
// continuously even while running a job (cancellation channel), and the
// official runner drops job messages that land during worker teardown —
// so job delivery only happens on a poll from a free agent.
func (s *Server) handleGetMessage(w http.ResponseWriter, r *http.Request) {
	sessionID := r.URL.Query().Get("sessionId")
	caller, err := s.callerRunner(r)
	if err != nil {
		s.challengeRunnerAuth(w, r, err)
		return
	}

	s.store.Mu.RLock()
	session, ok := s.store.Sessions[sessionID]
	s.store.Mu.RUnlock()

	if !ok {
		http.Error(w, "session not found", http.StatusNotFound)
		return
	}
	if !sessionOwnedBy(session, caller) {
		writeGHError(w, http.StatusForbidden, "Session belongs to another runner")
		return
	}

	if msg := s.actions.PullPendingMessage(session, caller.Scope); msg != nil {
		s.logger.Info().Int64("messageId", msg.MessageID).Msg("delivering pending message to runner")
		if err := deliverJSON(w, msg); err != nil {
			// The queue entry is only given up once the runner has the bytes;
			// a dropped connection must not lose the job.
			s.actions.RequeuePendingMessage(msg)
			s.logger.Warn().Err(err).Int64("messageId", msg.MessageID).Msg("message delivery failed — requeued")
		}
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), messagePollTimeout)
	defer cancel()

	select {
	case msg, open := <-session.MsgCh:
		if !open || msg == nil {
			w.WriteHeader(http.StatusOK)
			return
		}
		s.logger.Info().Int64("messageId", msg.MessageID).Msg("delivering message to runner")
		writeJSON(w, http.StatusOK, msg)
	case <-ctx.Done():
		w.WriteHeader(http.StatusOK)
	}
}

// deliverJSON writes a message and pushes it out to the client, reporting
// whether the bytes actually left. Delivery of a queued job is only committed
// on success, so this cannot use the fire-and-forget writeJSON.
func deliverJSON(w http.ResponseWriter, v interface{}) error {
	body, err := json.Marshal(v)
	if err != nil {
		writeGHError(w, http.StatusInternalServerError, "encode runner message")
		return err
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Length", strconv.Itoa(len(body)+1))
	w.WriteHeader(http.StatusOK)
	if _, err := w.Write(append(body, '\n')); err != nil {
		return err
	}
	if flusher, ok := w.(http.Flusher); ok {
		flusher.Flush()
	}
	return nil
}

func (s *Server) handleDeleteMessage(w http.ResponseWriter, r *http.Request) {
	msgID := r.PathValue("messageId")
	s.logger.Debug().Str("messageId", msgID).Msg("message acknowledged")
	w.WriteHeader(http.StatusOK)
}
