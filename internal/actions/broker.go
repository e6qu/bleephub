package actions

import (
	"encoding/json"
	"strings"
	"time"
)

// PullPendingMessage hands the polling session the first queued message its
// agent can take: labels covered, agent free, and the job's repository inside
// the runner's registration scope. A job message carries that repository's
// secrets, so a runner registered elsewhere never sees it. Operator-submitted
// jobs (/internal/exec/submit) name no repository and carry no repository,
// organization or environment secrets, so any registered runner may take one.
func (s *Engine) PullPendingMessage(session *Session, scope runnerScope) *TaskAgentMessage {
	s.store.Mu.Lock()
	defer s.store.Mu.Unlock()
	if !s.AgentTakesAJobLocked(session.Agent) {
		return nil
	}
	for i, msg := range s.store.PendingMessages {
		if !AgentSatisfiesLabels(session.Agent, msg.Labels) {
			continue
		}
		if !JobSecretsEntitled(scope, jobMessageRepoName(msg.Body)) {
			continue
		}
		s.store.PendingMessages = append(s.store.PendingMessages[:i], s.store.PendingMessages[i+1:]...)
		s.RecordJobAgentLocked(msg, session)
		// Late-bind the runner context (os/arch/name) to the agent that
		// actually leased this job — it was unknown when the message was
		// queued (ACT-051). Idempotent, so a requeue+redeliver to another
		// runner rebinds afresh.
		if msg.MessageType == "PipelineAgentJobRequest" {
			msg.Body = rebindRunnerContext(msg.Body, session.Agent)
		}
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

// RequeuePendingMessage puts an undelivered job back at the head of the queue
// and releases the agent it was tentatively assigned to. The runner never
// received the message, so the assignment is fully undone — including the
// EverAssigned mark, which must not burn an ephemeral runner's single job on
// a delivery that failed.
func (s *Engine) RequeuePendingMessage(msg *TaskAgentMessage) {
	s.store.Mu.Lock()
	defer s.store.Mu.Unlock()
	if job := s.store.Jobs[msg.JobID]; job != nil {
		if agent := s.store.Agents[job.AgentID]; agent != nil && agent.AssignedJobID == job.ID {
			agent.AssignedJobID = ""
			agent.EverAssigned = false
		}
		job.AgentID = 0
	}
	s.store.PendingMessages = append([]*TaskAgentMessage{msg}, s.store.PendingMessages...)
}

// AgentTakesAJobLocked reports whether the agent may be handed a queued job.
// There must be a registered agent to hand it to — the routes that renew and
// complete a job request are gated on the runner it was assigned to, so a
// session with no agent would receive a job it could never report on. It must
// not already hold an unfinished one either: real GitHub never assigns a busy
// runner, and the official runner DROPS job messages received mid-job. An
// ephemeral runner exists for exactly one job, so a job it has already been
// assigned disqualifies it even after that job finished — and even after the
// finished job's stub has been garbage-collected, which is exactly what the
// EverAssigned flag preserves (the disqualification used to rest on the
// completed job lingering in store.Jobs forever). This is O(1) on every
// long-poll where it used to scan all of store.Jobs under the write lock.
// Callers hold the store lock.
func (s *Engine) AgentTakesAJobLocked(agent *Agent) bool {
	if agent == nil || agent.ID == 0 {
		return false
	}
	if agent.Ephemeral && agent.EverAssigned {
		return false
	}
	if agent.AssignedJobID != "" {
		if j := s.store.Jobs[agent.AssignedJobID]; j != nil && j.AgentID == agent.ID && j.Status != "completed" {
			return false
		}
	}
	return true
}

// RecordJobAgentLocked associates a delivered job with the agent that
// took it (busy tracking + the runners API's `busy`).
func (s *Engine) RecordJobAgentLocked(msg *TaskAgentMessage, session *Session) {
	if msg.JobID == "" || session.Agent == nil {
		return
	}
	if job := s.store.Jobs[msg.JobID]; job != nil {
		job.AgentID = session.Agent.ID
		session.Agent.AssignedJobID = job.ID
		session.Agent.EverAssigned = true
	}
}

// QueueJobMessage queues a job message for delivery. Job messages are
// NEVER pushed into an open long-poll: the official runner keeps a poll
// open even mid-job (its cancellation channel) and silently DROPS job
// messages that arrive while the worker is running or tearing down.
// Delivery happens exclusively in handleGetMessage — a fresh poll from a
// free, label-matching runner pulls the next queued message, exactly the
// hold-until-poll semantics real GitHub's broker has.
func (s *Engine) QueueJobMessage(msg *TaskAgentMessage) {
	s.store.Mu.Lock()
	s.store.PendingMessages = append(s.store.PendingMessages, msg)
	s.store.Mu.Unlock()
}

// AgentSatisfiesLabels reports whether an agent's registered labels
// cover every runs-on requirement (case-insensitive). GitHub-hosted
// pool aliases (ubuntu-*, macos-*, windows-*) are satisfiable by ANY
// agent: bleephub has no hosted pool, so a hosted-alias job runs on
// whatever runner connects — the same accommodation act/nektos makes.
// All other labels (self-hosted, custom) match strictly.
func AgentSatisfiesLabels(agent *Agent, required []string) bool {
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

// SendAgentRefreshMessage pushes an AgentRefreshMessage to every session
// for the given agent. Real GitHub sends this when a newer runner version
// is available; the runner's self-updater downloads the target package and
// restarts. The message rides the session channel exactly like a
// cancellation so it reaches the runner's open long-poll.
func (s *Engine) SendAgentRefreshMessage(agentID int, targetVersion string, timeout time.Duration) {
	if agentID == 0 || targetVersion == "" {
		return
	}
	s.store.Mu.Lock()
	defer s.store.Mu.Unlock()
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

// SendJobCancellation pushes a JobCancellation message at the runner
// executing the job. Unlike job requests (pull-only), cancellations go
// through the session channel: the runner's listener keeps a poll open
// during a job precisely to receive these (actions/runner
// JobCancelMessage — body {jobId, timeout}).
func (s *Engine) SendJobCancellation(jobID string) {
	s.store.Mu.Lock()
	defer s.store.Mu.Unlock()
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

func (s *Engine) NextMessageID() int64 {
	s.store.Mu.Lock()
	id := s.store.NextMsg
	s.store.NextMsg++
	s.store.Mu.Unlock()
	return id
}

func (s *Engine) NextRequestID() int64 {
	s.store.Mu.Lock()
	defer s.store.Mu.Unlock()
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

func (s *Engine) NextLogID() int {
	return s.store.ReserveLogID()
}

func (s *Engine) LookupJobByRequestID(reqID int64) *Job {
	s.store.Mu.RLock()
	defer s.store.Mu.RUnlock()
	return s.store.JobByRequestIDLocked(reqID)
}

func (s *Engine) SessionCount() int {
	s.store.Mu.RLock()
	defer s.store.Mu.RUnlock()
	return len(s.store.Sessions)
}

func (s *Engine) LookupJobByPlanID(planID string) *Job {
	s.store.Mu.RLock()
	defer s.store.Mu.RUnlock()
	return s.store.JobByPlanIDLocked(planID)
}
