package bleephub

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
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

	s.store.mu.Lock()
	s.store.Sessions[sessionID] = session
	s.store.mu.Unlock()

	if s.metrics != nil {
		s.metrics.SetActiveSessions(int64(s.sessionCount()))
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

	s.store.mu.Lock()
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
	s.store.mu.Unlock()

	// Deleting its session is the last thing a runner does. An ephemeral
	// runner is deregistered here rather than the moment its job finished,
	// because a registration removed mid-teardown cannot authenticate the
	// calls the runner has still to make — closing its job request and this
	// session. pullPendingMessage is what holds it to one job.
	s.removeEphemeralAgent(retiring)

	if s.metrics != nil {
		s.metrics.SetActiveSessions(int64(s.sessionCount()))
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

	s.store.mu.RLock()
	session, ok := s.store.Sessions[sessionID]
	s.store.mu.RUnlock()

	if !ok {
		http.Error(w, "session not found", http.StatusNotFound)
		return
	}
	if !sessionOwnedBy(session, caller) {
		writeGHError(w, http.StatusForbidden, "Session belongs to another runner")
		return
	}

	if msg := s.pullPendingMessage(session, caller.Scope); msg != nil {
		s.logger.Info().Int64("messageId", msg.MessageID).Msg("delivering pending message to runner")
		if err := deliverJSON(w, msg); err != nil {
			// The queue entry is only given up once the runner has the bytes;
			// a dropped connection must not lose the job.
			s.requeuePendingMessage(msg)
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

// pullPendingMessage hands the polling session the first queued message its
// agent can take: labels covered, agent free, and the job's repository inside
// the runner's registration scope. A job message carries that repository's
// secrets, so a runner registered elsewhere never sees it. Operator-submitted
// jobs (/internal/exec/submit) name no repository and carry no repository,
// organization or environment secrets, so any registered runner may take one.
func (s *Server) pullPendingMessage(session *Session, scope runnerScope) *TaskAgentMessage {
	s.store.mu.Lock()
	defer s.store.mu.Unlock()
	if !s.agentTakesAJobLocked(session.Agent) {
		return nil
	}
	for i, msg := range s.store.PendingMessages {
		if !agentSatisfiesLabels(session.Agent, msg.Labels) {
			continue
		}
		if !jobSecretsEntitled(scope, jobMessageRepoName(msg.Body)) {
			continue
		}
		s.store.PendingMessages = append(s.store.PendingMessages[:i], s.store.PendingMessages[i+1:]...)
		s.recordJobAgentLocked(msg, session)
		return msg
	}
	return nil
}

// jobMessageRepoName reports the repository a queued job message runs as, or
// "" for an operator-submitted job that names none.
func jobMessageRepoName(message string) string {
	_, repo := jobMessageScopeAndRepo(message)
	return repo
}

// requeuePendingMessage puts an undelivered job back at the head of the queue
// and releases the agent it was tentatively assigned to.
func (s *Server) requeuePendingMessage(msg *TaskAgentMessage) {
	s.store.mu.Lock()
	defer s.store.mu.Unlock()
	if job := s.store.Jobs[msg.JobID]; job != nil {
		job.AgentID = 0
	}
	s.store.PendingMessages = append([]*TaskAgentMessage{msg}, s.store.PendingMessages...)
}

// agentTakesAJobLocked reports whether the agent may be handed a queued job.
// There must be a registered agent to hand it to — the routes that renew and
// complete a job request are gated on the runner it was assigned to, so a
// session with no agent would receive a job it could never report on. It must
// not already hold an unfinished one either: real GitHub never assigns a busy
// runner, and the official runner DROPS job messages received mid-job. An
// ephemeral runner exists for exactly one job, so a job it has already been
// assigned disqualifies it even after that job finished. Callers hold the
// store lock.
func (s *Server) agentTakesAJobLocked(agent *Agent) bool {
	if agent == nil || agent.ID == 0 {
		return false
	}
	for _, j := range s.store.Jobs {
		if j.AgentID != agent.ID {
			continue
		}
		if agent.Ephemeral || j.Status != "completed" {
			return false
		}
	}
	return true
}

// recordJobAgentLocked associates a delivered job with the agent that
// took it (busy tracking + the runners API's `busy`).
func (s *Server) recordJobAgentLocked(msg *TaskAgentMessage, session *Session) {
	if msg.JobID == "" || session.Agent == nil {
		return
	}
	if job := s.store.Jobs[msg.JobID]; job != nil {
		job.AgentID = session.Agent.ID
	}
}

func (s *Server) handleDeleteMessage(w http.ResponseWriter, r *http.Request) {
	msgID := r.PathValue("messageId")
	s.logger.Debug().Str("messageId", msgID).Msg("message acknowledged")
	w.WriteHeader(http.StatusOK)
}

// queueJobMessage queues a job message for delivery. Job messages are
// NEVER pushed into an open long-poll: the official runner keeps a poll
// open even mid-job (its cancellation channel) and silently DROPS job
// messages that arrive while the worker is running or tearing down.
// Delivery happens exclusively in handleGetMessage — a fresh poll from a
// free, label-matching runner pulls the next queued message, exactly the
// hold-until-poll semantics real GitHub's broker has.
func (s *Server) queueJobMessage(msg *TaskAgentMessage) {
	s.store.mu.Lock()
	s.store.PendingMessages = append(s.store.PendingMessages, msg)
	s.store.mu.Unlock()
}

// agentSatisfiesLabels reports whether an agent's registered labels
// cover every runs-on requirement (case-insensitive). GitHub-hosted
// pool aliases (ubuntu-*, macos-*, windows-*) are satisfiable by ANY
// agent: bleephub has no hosted pool, so a hosted-alias job runs on
// whatever runner connects — the same accommodation act/nektos makes.
// All other labels (self-hosted, custom) match strictly.
func agentSatisfiesLabels(agent *Agent, required []string) bool {
	if len(required) == 0 {
		return true
	}
	var have map[string]bool
	if agent != nil {
		have = make(map[string]bool, len(agent.Labels))
		for _, l := range agent.Labels {
			have[strings.ToLower(l.Name)] = true
		}
	}
	for _, req := range required {
		lower := strings.ToLower(req)
		if isHostedPoolAlias(lower) {
			continue
		}
		if !have[lower] {
			return false
		}
	}
	return true
}

func isHostedPoolAlias(lower string) bool {
	return strings.HasPrefix(lower, "ubuntu-") ||
		strings.HasPrefix(lower, "macos-") ||
		strings.HasPrefix(lower, "windows-")
}

// sendAgentRefreshMessage pushes an AgentRefreshMessage to every session
// for the given agent. Real GitHub sends this when a newer runner version
// is available; the runner's self-updater downloads the target package and
// restarts. The message rides the session channel exactly like a
// cancellation so it reaches the runner's open long-poll.
func (s *Server) sendAgentRefreshMessage(agentID int, targetVersion string, timeout time.Duration) {
	if agentID == 0 || targetVersion == "" {
		return
	}
	s.store.mu.Lock()
	defer s.store.mu.Unlock()
	body, err := json.Marshal(map[string]interface{}{
		"agentId":       agentID,
		"targetVersion": targetVersion,
		"timeout":       timeout.String(),
	})
	if err != nil {
		s.logger.Error().Err(err).Int("agentId", agentID).Msg("failed to marshal AgentRefreshMessage")
		return
	}
	msg := &TaskAgentMessage{
		MessageID:   s.store.NextMsg,
		MessageType: "AgentRefreshMessage",
		Body:        string(body),
	}
	s.store.NextMsg++
	for _, sess := range s.store.Sessions {
		if sess.Agent != nil && sess.Agent.ID == agentID {
			select {
			case sess.MsgCh <- msg:
				s.logger.Info().Int("agentId", agentID).Str("version", targetVersion).Msg("AgentRefreshMessage sent to runner")
			default:
				s.logger.Error().Int("agentId", agentID).Msg("AgentRefreshMessage channel full")
			}
		}
	}
}

// sendJobCancellation pushes a JobCancellation message at the runner
// executing the job. Unlike job requests (pull-only), cancellations go
// through the session channel: the runner's listener keeps a poll open
// during a job precisely to receive these (actions/runner
// JobCancelMessage — body {jobId, timeout}).
func (s *Server) sendJobCancellation(jobID string) {
	s.store.mu.Lock()
	defer s.store.mu.Unlock()
	job := s.store.Jobs[jobID]
	if job == nil || job.AgentID == 0 {
		return
	}
	var target *Session
	for _, sess := range s.store.Sessions {
		if sess.Agent != nil && sess.Agent.ID == job.AgentID {
			target = sess
			break
		}
	}
	if target == nil {
		s.logger.Warn().Str("jobId", jobID).Int("agentId", job.AgentID).
			Msg("job cancellation: runner session gone")
		return
	}
	body, _ := json.Marshal(map[string]interface{}{
		"jobId":   jobID,
		"timeout": "00:05:00",
	})
	msg := &TaskAgentMessage{
		MessageID:   s.store.NextMsg,
		MessageType: "JobCancellation",
		Body:        string(body),
	}
	s.store.NextMsg++
	select {
	case target.MsgCh <- msg:
		s.logger.Info().Str("jobId", jobID).Int("agentId", job.AgentID).
			Msg("job cancellation sent to runner")
	default:
		s.logger.Error().Str("jobId", jobID).Int("agentId", job.AgentID).
			Msg("job cancellation channel full — runner will finish the job")
	}
}

func (s *Server) nextMessageID() int64 {
	s.store.mu.Lock()
	id := s.store.NextMsg
	s.store.NextMsg++
	s.store.mu.Unlock()
	return id
}

func (s *Server) nextRequestID() int64 {
	s.store.mu.Lock()
	defer s.store.mu.Unlock()
	if s.store.NextReqID < 1 {
		s.store.NextReqID = 1
	}
	for {
		id := s.store.NextReqID
		s.store.NextReqID++
		collision := false
		for _, job := range s.store.Jobs {
			if int64(job.RequestID) == id {
				collision = true
				break
			}
		}
		if !collision {
			return id
		}
	}
}

func (s *Server) nextLogID() int {
	return s.store.ReserveLogID()
}

func (s *Server) lookupJobByRequestID(reqID int64) *Job {
	s.store.mu.RLock()
	defer s.store.mu.RUnlock()
	for _, j := range s.store.Jobs {
		if j.RequestID == reqID {
			return j
		}
	}
	return nil
}

func (s *Server) sessionCount() int {
	s.store.mu.RLock()
	defer s.store.mu.RUnlock()
	return len(s.store.Sessions)
}

func (s *Server) lookupJobByPlanID(planID string) *Job {
	s.store.mu.RLock()
	defer s.store.mu.RUnlock()
	for _, j := range s.store.Jobs {
		if j.PlanID == planID {
			return j
		}
	}
	return nil
}
