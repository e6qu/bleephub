package bleephub

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"runtime"
	"strings"
	"time"
)

const monitoringSchemaVersion = "e6qu.monitoring/v2"

type monitoringObservation struct {
	SchemaVersion string               `json:"schema_version"`
	ObservedAt    time.Time            `json:"observed_at"`
	Resources     []monitoringResource `json:"resources"`
}

type monitoringResource struct {
	ID      string             `json:"id"`
	Name    string             `json:"name"`
	Kind    string             `json:"kind"`
	Health  string             `json:"health"`
	Metrics []monitoringMetric `json:"metrics"`
}

type monitoringMetric struct {
	Name   string  `json:"name"`
	Label  string  `json:"label"`
	Value  float64 `json:"value"`
	Unit   string  `json:"unit"`
	Status string  `json:"status"`
}

func availableMetric(name, label string, value float64, unit string) monitoringMetric {
	return monitoringMetric{Name: name, Label: label, Value: value, Unit: unit, Status: "available"}
}

// MonitoringTokenFromEnvironment validates BLEEPHUB_MONITORING_TOKEN and
// returns a construction option that retains only its digest. The variable is
// optional so non-e6qu installations do not acquire a deployment-specific
// requirement; when absent the endpoint consistently refuses every caller.
func MonitoringTokenFromEnvironment() (ServerOption, error) {
	token, present := os.LookupEnv("BLEEPHUB_MONITORING_TOKEN")
	if !present {
		return func(*Server) {}, nil
	}
	digest, err := monitoringDigest(token)
	if err != nil {
		return nil, err
	}
	return func(server *Server) { server.monitoringTokenDigest = &digest }, nil
}

func monitoringDigest(token string) ([sha256.Size]byte, error) {
	if len(token) < 32 || strings.IndexFunc(token, func(character rune) bool {
		return character <= ' ' || character == '\u007f'
	}) >= 0 {
		return [sha256.Size]byte{}, errors.New("BLEEPHUB_MONITORING_TOKEN must contain at least 32 non-whitespace characters")
	}
	return sha256.Sum256([]byte(token)), nil
}

func (s *Server) monitoringAuthorized(header string) bool {
	if s.monitoringTokenDigest == nil || !strings.HasPrefix(header, "Bearer ") {
		return false
	}
	actual := sha256.Sum256([]byte(strings.TrimPrefix(header, "Bearer ")))
	return subtle.ConstantTimeCompare(s.monitoringTokenDigest[:], actual[:]) == 1
}

func monitoringUnauthorized(w http.ResponseWriter) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("WWW-Authenticate", `Bearer realm="bleephub-monitoring"`)
	http.Error(w, "unauthorized", http.StatusUnauthorized)
}

func (s *Server) handleMonitoringObservation(w http.ResponseWriter, r *http.Request) {
	if !s.monitoringAuthorized(r.Header.Get("Authorization")) {
		monitoringUnauthorized(w)
		return
	}

	ctx, cancel := contextWithMonitoringTimeout(r)
	defer cancel()
	health := "healthy"
	if err := s.store.PersistenceReady(ctx); err != nil {
		health = "unhealthy"
		s.logger.Warn().Err(err).Msg("monitoring persistence check failed")
	}
	snapshot := s.metrics.Snapshot()
	completedJobs := int64(0)
	for _, count := range snapshot.JobCompletions {
		completedJobs += count
	}

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	if err := json.NewEncoder(w).Encode(monitoringObservation{
		SchemaVersion: monitoringSchemaVersion,
		ObservedAt:    s.currentTime(),
		Resources: []monitoringResource{{
			ID: "bleephub-process", Name: "Bleephub", Kind: "application", Health: health,
			Metrics: []monitoringMetric{
				availableMetric("workflows.active", "Active workflows", float64(snapshot.ActiveWorkflows), "workflows"),
				availableMetric("sessions.active", "Connected runners", float64(snapshot.ActiveSessions), "sessions"),
				availableMetric("jobs.dispatched", "Dispatched jobs", float64(snapshot.JobDispatches), "jobs"),
				availableMetric("jobs.completed", "Completed jobs", float64(completedJobs), "jobs"),
				availableMetric("process.goroutines", "Process goroutines", float64(runtime.NumGoroutine()), "goroutines"),
				availableMetric("process.heap", "Allocated heap", snapshot.HeapAllocMB, "MiB"),
				availableMetric("process.uptime", "Process uptime", float64(snapshot.UptimeSeconds), "seconds"),
			},
		}},
	}); err != nil {
		s.logger.Error().Err(err).Msg("write monitoring observation")
	}
}

func contextWithMonitoringTimeout(r *http.Request) (context.Context, context.CancelFunc) {
	return context.WithTimeout(r.Context(), 2*time.Second)
}
