package actions

import (
	"encoding/json"
	"strings"
	"time"

	"github.com/e6qu/bleephub/internal/store"
)

// PullPendingMessage hands the session the first queued message its agent can
// take: labels covered, agent free, job repository inside the runner's scope. A
// job message carries that repository's secrets, so a runner registered
// elsewhere never sees it; operator-submitted jobs name no repository and carry
// no secrets, so any runner may take one.
func (s *Engine) PullPendingMessage(session *store.Session, scope store.RunnerScope) *store.TaskAgentMessage {
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
		// Late-bind the runner context (os/arch/name) to the leasing agent;
		// it was unknown when the message was queued (ACT-051).
		if msg.MessageType == "PipelineAgentJobRequest" {
			msg.Body = rebindRunnerContext(msg.Body, session.Agent)
		}
		return msg
	}
	return nil
}

// jobMessageRepoName reports the repository a queued job runs as, or "" for an
// operator-submitted job.
func jobMessageRepoName(message string) string {
	_, repo := store.JobMessageScopeAndRepo(message)
	return repo
}

// RequeuePendingMessage puts an undelivered job back at the head of the queue
// and releases its tentatively-assigned agent. The assignment is fully undone,
// including EverAssigned, so a failed delivery does not burn an ephemeral
// runner's single job.
func (s *Engine) RequeuePendingMessage(msg *store.TaskAgentMessage) {
	s.store.Mu.Lock()
	defer s.store.Mu.Unlock()
	if job := s.store.Jobs[msg.JobID]; job != nil {
		if agent := s.store.Agents[job.AgentID]; agent != nil && agent.AssignedJobID == job.ID {
			agent.AssignedJobID = ""
			agent.EverAssigned = false
		}
		job.AgentID = 0
	}
	s.store.PendingMessages = append([]*store.TaskAgentMessage{msg}, s.store.PendingMessages...)
}

// AgentTakesAJobLocked reports whether the agent may be handed a queued job.
// The agent must be registered (renew/complete routes gate on the assigned
// runner) and not already hold an unfinished job (the official runner DROPS
// job messages received mid-job). An ephemeral runner runs exactly one job, so
// EverAssigned disqualifies it even after that job finished and its stub was
// garbage-collected. Callers hold the store lock.
func (s *Engine) AgentTakesAJobLocked(agent *store.Agent) bool {
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

// RecordJobAgentLocked associates a delivered job with the agent that took it.
func (s *Engine) RecordJobAgentLocked(msg *store.TaskAgentMessage, session *store.Session) {
	if msg.JobID == "" || session.Agent == nil {
		return
	}
	if job := s.store.Jobs[msg.JobID]; job != nil {
		job.AgentID = session.Agent.ID
		session.Agent.AssignedJobID = job.ID
		session.Agent.EverAssigned = true
	}
}

// QueueJobMessage queues a job message for delivery. Job messages are NEVER
// pushed into an open long-poll: the official runner keeps a poll open mid-job
// and silently DROPS job messages that arrive while the worker runs. Delivery
// happens exclusively when a fresh poll from a free runner pulls the message.
func (s *Engine) QueueJobMessage(msg *store.TaskAgentMessage) {
	s.store.Mu.Lock()
	s.store.PendingMessages = append(s.store.PendingMessages, msg)
	s.store.Mu.Unlock()
}

// AgentSatisfiesLabels reports whether an agent's labels cover every runs-on
// requirement (case-insensitive). Hosted-pool aliases (ubuntu-*, macos-*,
// windows-*) match ANY agent since bleephub has no hosted pool (as act/nektos
// do); all other labels match strictly.
func AgentSatisfiesLabels(agent *store.Agent, required []string) bool {
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

// SendAgentRefreshMessage pushes an AgentRefreshMessage to every session for
// the agent, triggering its self-updater. It rides the session channel like a
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
	msg := &store.TaskAgentMessage{
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

// SendJobCancellation pushes a JobCancellation at the runner executing the job.
// Unlike pull-only job requests, cancellations go through the session channel:
// the runner keeps a poll open mid-job precisely to receive these.
func (s *Engine) SendJobCancellation(jobID string) {
	s.store.Mu.Lock()
	defer s.store.Mu.Unlock()
	job := s.store.Jobs[jobID]
	if job == nil || job.AgentID == 0 {
		return
	}
	var target *store.Session
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
	msg := &store.TaskAgentMessage{
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

func (s *Engine) LookupJobByRequestID(reqID int64) *store.Job {
	s.store.Mu.RLock()
	defer s.store.Mu.RUnlock()
	return s.store.JobByRequestIDLocked(reqID)
}

func (s *Engine) SessionCount() int {
	s.store.Mu.RLock()
	defer s.store.Mu.RUnlock()
	return len(s.store.Sessions)
}

func (s *Engine) LookupJobByPlanID(planID string) *store.Job {
	s.store.Mu.RLock()
	defer s.store.Mu.RUnlock()
	return s.store.JobByPlanIDLocked(planID)
}
