package bleephub

import (
	"runtime"
	"sort"
	"sync"
	"time"
)

type Metrics struct {
	mu                  sync.Mutex
	WorkflowSubmissions int64            `json:"workflow_submissions"`
	JobDispatches       int64            `json:"job_dispatches"`
	JobCompletions      map[string]int64 `json:"job_completions"` // result → count
	JobDurations        []time.Duration  `json:"-"`
	ActiveWorkflows     int64            `json:"active_workflows"`
	ActiveSessions      int64            `json:"active_sessions"`
	StartedAt           time.Time        `json:"-"`
}

func NewMetrics() *Metrics {
	return &Metrics{
		JobCompletions: make(map[string]int64),
		StartedAt:      time.Now(),
	}
}

func (m *Metrics) RecordWorkflowSubmit() {
	m.mu.Lock()
	m.WorkflowSubmissions++
	m.ActiveWorkflows++
	m.mu.Unlock()
}

func (m *Metrics) RecordWorkflowComplete() {
	m.mu.Lock()
	if m.ActiveWorkflows > 0 {
		m.ActiveWorkflows--
	}
	m.mu.Unlock()
}

func (m *Metrics) RecordJobDispatch() {
	m.mu.Lock()
	m.JobDispatches++
	m.mu.Unlock()
}

// RecordJobCompletion takes the job rather than a precomputed duration.
//
// The caller used to pass time.Since(workflow.CreatedAt), which is the age of
// the whole run: every job in a multi-job or needs-chained workflow reported a
// duration inflated by all the work that preceded it. Deriving the duration
// here from the one authoritative pair of timestamps means a caller cannot
// supply the wrong clock.
func (m *Metrics) RecordJobCompletion(job *WorkflowJob) {
	if job == nil {
		return
	}
	end := job.CompletedAt
	if end.IsZero() {
		end = time.Now()
	}
	var duration time.Duration
	if !job.StartedAt.IsZero() {
		duration = end.Sub(job.StartedAt)
	}

	m.mu.Lock()
	m.JobCompletions[string(job.Result)]++
	if duration > 0 {
		if len(m.JobDurations) < 1000 {
			m.JobDurations = append(m.JobDurations, duration)
		} else {
			m.JobDurations = append(m.JobDurations[500:], duration)
		}
	}
	m.mu.Unlock()
}

// jobDurationPercentilesLocked summarizes the retained duration window.
//
// These were accumulated under a mutex on every job completion and then read
// by nothing at all — a write-only side effect that looked like observability.
// Reporting them is the smaller change; the alternative was deleting the field.
func (m *Metrics) jobDurationPercentilesLocked() (p50, p95, p99 float64) {
	if len(m.JobDurations) == 0 {
		return 0, 0, 0
	}
	sorted := append([]time.Duration(nil), m.JobDurations...)
	sort.Slice(sorted, func(a, b int) bool { return sorted[a] < sorted[b] })
	at := func(q float64) float64 {
		idx := int(q * float64(len(sorted)-1))
		return sorted[idx].Seconds()
	}
	return at(0.50), at(0.95), at(0.99)
}

func (m *Metrics) SetActiveSessions(n int64) {
	m.mu.Lock()
	m.ActiveSessions = n
	m.mu.Unlock()
}

type MetricsSnapshot struct {
	WorkflowSubmissions int64            `json:"workflow_submissions"`
	JobDispatches       int64            `json:"job_dispatches"`
	JobCompletions      map[string]int64 `json:"job_completions"`
	ActiveWorkflows     int64            `json:"active_workflows"`
	ActiveSessions      int64            `json:"active_sessions"`
	UptimeSeconds       int              `json:"uptime_seconds"`
	Goroutines          int              `json:"goroutines"`
	HeapAllocMB         float64          `json:"heap_alloc_mb"`
	JobDurationP50Sec   float64          `json:"job_duration_p50_seconds"`
	JobDurationP95Sec   float64          `json:"job_duration_p95_seconds"`
	JobDurationP99Sec   float64          `json:"job_duration_p99_seconds"`
}

func (m *Metrics) Snapshot() MetricsSnapshot {
	// ReadMemStats stops the world. Doing it before taking the lock keeps that
	// pause off the job-accounting path, which every completing job contends
	// for; previously a scrape of the metrics endpoint stalled them all.
	var memStats runtime.MemStats
	runtime.ReadMemStats(&memStats)

	m.mu.Lock()
	defer m.mu.Unlock()

	completions := make(map[string]int64, len(m.JobCompletions))
	for k, v := range m.JobCompletions {
		completions[k] = v
	}
	p50, p95, p99 := m.jobDurationPercentilesLocked()

	return MetricsSnapshot{
		JobDurationP50Sec:   p50,
		JobDurationP95Sec:   p95,
		JobDurationP99Sec:   p99,
		WorkflowSubmissions: m.WorkflowSubmissions,
		JobDispatches:       m.JobDispatches,
		JobCompletions:      completions,
		ActiveWorkflows:     m.ActiveWorkflows,
		ActiveSessions:      m.ActiveSessions,
		UptimeSeconds:       int(time.Since(m.StartedAt).Seconds()),
		Goroutines:          runtime.NumGoroutine(),
		HeapAllocMB:         float64(memStats.HeapAlloc) / (1024 * 1024),
	}
}
