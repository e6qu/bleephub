package bleephub

import (
	"testing"
	"time"
)

func TestMetricsSubmitIncrement(t *testing.T) {
	m := NewMetrics()
	m.RecordWorkflowSubmit()
	m.RecordWorkflowSubmit()

	snap := m.Snapshot()
	if snap.WorkflowSubmissions != 2 {
		t.Errorf("submissions = %d, want 2", snap.WorkflowSubmissions)
	}
	if snap.ActiveWorkflows != 2 {
		t.Errorf("active = %d, want 2", snap.ActiveWorkflows)
	}
}

func TestMetricsCompletionByResult(t *testing.T) {
	m := NewMetrics()
	completed := func(result Result, d time.Duration) *WorkflowJob {
		start := time.Now().Add(-d)
		return &WorkflowJob{Result: result, StartedAt: start, CompletedAt: start.Add(d)}
	}
	m.RecordJobCompletion(completed(ResultSuccess, 100*time.Millisecond))
	m.RecordJobCompletion(completed(ResultSuccess, 200*time.Millisecond))
	m.RecordJobCompletion(completed(ResultFailure, 50*time.Millisecond))

	snap := m.Snapshot()
	if snap.JobCompletions[string(ResultSuccess)] != 2 {
		t.Errorf("success = %d, want 2", snap.JobCompletions[string(ResultSuccess)])
	}
	if snap.JobCompletions[string(ResultFailure)] != 1 {
		t.Errorf("failure = %d, want 1", snap.JobCompletions[string(ResultFailure)])
	}
	// The durations were accumulated and then read by nothing before; assert
	// they now reach the snapshot, so the field cannot quietly go write-only
	// again.
	if snap.JobDurationP50Sec <= 0 {
		t.Errorf("p50 job duration = %v, want a positive value", snap.JobDurationP50Sec)
	}
	if snap.JobDurationP99Sec < snap.JobDurationP50Sec {
		t.Errorf("p99 %v < p50 %v", snap.JobDurationP99Sec, snap.JobDurationP50Sec)
	}
}

func TestMetricsActiveSessions(t *testing.T) {
	m := NewMetrics()
	m.SetActiveSessions(3)

	snap := m.Snapshot()
	if snap.ActiveSessions != 3 {
		t.Errorf("sessions = %d, want 3", snap.ActiveSessions)
	}

	m.SetActiveSessions(1)
	snap = m.Snapshot()
	if snap.ActiveSessions != 1 {
		t.Errorf("sessions = %d, want 1", snap.ActiveSessions)
	}
}
