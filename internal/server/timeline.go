package bleephub

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/e6qu/bleephub/internal/store"
)

const (
	// logFileCap bounds a single runner-uploaded log file; overflow is dropped
	// and marked in the stored log.
	logFileCap = 4 << 20

	// consoleLineCap bounds the live console capture per job; trimming appends
	// consoleTruncationMarker once.
	consoleLineCap     = 10000
	stepSummaryCap     = 1 << 20
	timelineRequestCap = 4 << 20
)

var (
	logTruncationMarker     = []byte("\n[bleephub] log truncated at 4 MiB\n")
	consoleTruncationMarker = fmt.Sprintf("[bleephub] console log truncated at %d lines", consoleLineCap)
)

func (s *Server) registerTimelineRoutes() {
	// Every route addresses a plan by {planId}, so each gates on that plan's job
	// token; the {scopeId} segment is not what the handlers read.

	// Timeline CRUD
	s.route("POST /_apis/v1/Timeline/{scopeId}/{hubName}/{planId}/timeline", s.requirePlanJob(s.handleCreateTimeline))
	s.route("POST /_apis/v1/Timeline/{scopeId}/{hubName}/{planId}/timeline/{timelineId}", s.requirePlanJob(s.handleCreateTimeline))
	s.route("PUT /_apis/v1/Timeline/{scopeId}/{hubName}/{planId}/timeline/{timelineId}", s.requirePlanJob(s.handleCreateTimeline))

	// Timeline records
	s.route("PATCH /_apis/v1/Timeline/{scopeId}/{hubName}/{planId}/{timelineId}", s.requirePlanJob(s.handleUpdateRecords))

	// Log files
	s.route("POST /_apis/v1/Logfiles/{scopeId}/{hubName}/{planId}", s.requirePlanJob(s.handleCreateLog))
	s.route("POST /_apis/v1/Logfiles/{scopeId}/{hubName}/{planId}/{logId}", s.requirePlanJob(s.handleUploadLog))

	// Web console log (live output)
	s.route("POST /_apis/v1/TimeLineWebConsoleLog/{scopeId}/{hubName}/{planId}/{timelineId}/{recordId}", s.requirePlanJob(s.handleWebConsoleLog))

	// Timeline attachments
	s.route("PUT /_apis/v1/Timeline/{scopeId}/{hubName}/{planId}/{timelineId}/attachments/{recordId}/{attachType}/{name}", s.requirePlanJob(s.handleTimelineAttachment))
}

func (s *Server) handleCreateTimeline(w http.ResponseWriter, r *http.Request) {
	timelineID := r.PathValue("timelineId")
	s.logger.Debug().Str("timelineId", timelineID).Msg("create/update timeline")

	// The timeline is opaque to bleephub; drain the body to free the conn.
	_, _ = io.Copy(io.Discard, r.Body)

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"id":       timelineID,
		"changeId": 1,
	})
}

func (s *Server) handleUpdateRecords(w http.ResponseWriter, r *http.Request) {
	planID := r.PathValue("planId")
	timelineID := r.PathValue("timelineId")

	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, timelineRequestCap))
	if err != nil {
		http.Error(w, "read timeline records body: "+err.Error(), http.StatusBadRequest)
		return
	}
	records, err := decodeTimelineRecords(body)
	if err != nil {
		http.Error(w, "invalid timeline records body: "+err.Error(), http.StatusBadRequest)
		return
	}

	merged := s.upsertTimelineRecords(planID, records)
	for _, rec := range merged {
		s.logger.Debug().
			Str("planId", planID).
			Str("timelineId", timelineID).
			Str("name", rec.Name).
			Str("state", rec.State).
			Str("result", rec.Result).
			Msg("timeline record update")
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"count": len(merged),
		"value": merged,
	})
}

// decodeTimelineRecords decodes a timeline-record PATCH body. actions/runner
// wraps records in a VssJsonCollectionWrapper ({"count", "value"}); a bare array
// is accepted too.
func decodeTimelineRecords(body []byte) ([]*store.TimelineRecord, error) {
	var wrapper struct {
		Value []*store.TimelineRecord `json:"value"`
	}
	if err := json.Unmarshal(body, &wrapper); err == nil && wrapper.Value != nil {
		return wrapper.Value, nil
	}
	var bare []*store.TimelineRecord
	if err := json.Unmarshal(body, &bare); err != nil {
		return nil, err
	}
	return bare, nil
}

// upsertTimelineRecords folds the PATCHed records into the plan's stored set,
// keyed by record ID, returning copies for the response body.
func (s *Server) upsertTimelineRecords(planID string, records []*store.TimelineRecord) []*store.TimelineRecord {
	s.store.Mu.Lock()
	defer s.store.Mu.Unlock()
	out := make([]*store.TimelineRecord, 0, len(records))
	for _, rec := range records {
		if rec == nil || rec.ID == "" {
			continue
		}
		var stored *store.TimelineRecord
		for _, existing := range s.store.TimelineRecords[planID] {
			if existing.ID == rec.ID {
				stored = existing
				break
			}
		}
		if stored == nil {
			stored = &store.TimelineRecord{ID: rec.ID}
			s.store.TimelineRecords[planID] = append(s.store.TimelineRecords[planID], stored)
		}
		mergeTimelineRecord(stored, rec)
		cp := *stored
		out = append(out, &cp)
	}
	if s.store.Persist != nil && planID != "" {
		s.store.Persist.MustPut("timeline_records", planID, s.store.TimelineRecords[planID])
	}
	return out
}

// mergeTimelineRecord folds a newer runner update into the stored record. The
// runner re-PATCHes the same record as state advances, often omitting unchanged
// fields, so a present field never regresses to empty.
func mergeTimelineRecord(stored, incoming *store.TimelineRecord) {
	if incoming.ParentID != "" {
		stored.ParentID = incoming.ParentID
	}
	if incoming.Type != "" {
		stored.Type = incoming.Type
	}
	if incoming.Name != "" {
		stored.Name = incoming.Name
	}
	if incoming.RefName != "" {
		stored.RefName = incoming.RefName
	}
	if incoming.Order != 0 {
		stored.Order = incoming.Order
	}
	if incoming.State != "" {
		stored.State = incoming.State
	}
	if incoming.Result != "" {
		stored.Result = incoming.Result
	}
	if incoming.StartTime != "" {
		stored.StartTime = incoming.StartTime
	}
	if incoming.FinishTime != "" {
		stored.FinishTime = incoming.FinishTime
	}
	if incoming.Log != nil {
		stored.Log = incoming.Log
	}
}

func (s *Server) handleCreateLog(w http.ResponseWriter, r *http.Request) {
	logID := s.actions.NextLogID()
	// Log ids come from one counter shared by every plan; record which plan
	// reserved it so a job cannot reach another job's log by id alone.
	s.artifactStore.ClaimLog(logID, r.PathValue("planId"))
	s.logger.Debug().Int("logId", logID).Msg("create log container")

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"id":        logID,
		"path":      fmt.Sprintf("logs/%d", logID),
		"createdOn": "2026-01-01T00:00:00Z",
		"lineCount": 0,
	})
}

func (s *Server) handleUploadLog(w http.ResponseWriter, r *http.Request) {
	logID, err := strconv.Atoi(r.PathValue("logId"))
	if err != nil {
		http.Error(w, "invalid log ID", http.StatusBadRequest)
		return
	}
	if !s.artifactStore.LogBelongsToPlan(logID, r.PathValue("planId")) {
		http.Error(w, "log not found", http.StatusNotFound)
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, logFileCap+1))
	if err != nil {
		http.Error(w, "read log content: "+err.Error(), http.StatusBadRequest)
		return
	}

	// The runner may upload a log in multiple blocks; append, capping at
	// logFileCap and marking the cut.
	s.store.Mu.Lock()
	existing := s.store.LogFiles[logID]
	next := append(append([]byte(nil), existing...), body...)
	next = s.store.RedactLogBytesLocked(r.PathValue("planId"), next)
	switch {
	case bytes.HasSuffix(existing, logTruncationMarker):
		// Already capped; drop later blocks.
		next = append([]byte(nil), existing...)
	case len(next) <= logFileCap:
	default:
		next = next[:logFileCap]
		next = append(next, logTruncationMarker...)
	}
	storedData := append([]byte(nil), next...)
	stored := len(next)

	if err := s.artifactStore.WriteLogData(r.Context(), logID, storedData); err != nil {
		s.store.Mu.Unlock()
		http.Error(w, "log byte-store write: "+err.Error(), http.StatusInternalServerError)
		return
	}
	s.store.LogFiles[logID] = next
	s.store.Mu.Unlock()

	s.logger.Debug().Int("logId", logID).Int("uploadBytes", len(body)).Int("storedBytes", stored).Msg("log upload")

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"id":        logID,
		"path":      fmt.Sprintf("logs/%d", logID),
		"createdOn": "2026-01-01T00:00:00Z",
		"lineCount": bytes.Count(body, []byte{'\n'}),
	})
}

func (s *Server) handleWebConsoleLog(w http.ResponseWriter, r *http.Request) {
	planID := r.PathValue("planId")
	recordID := r.PathValue("recordId")

	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, timelineRequestCap))
	if err != nil {
		http.Error(w, "read console log body: "+err.Error(), http.StatusBadRequest)
		return
	}
	lines, err := decodeConsoleLines(body)
	if err != nil {
		http.Error(w, "invalid console log body: "+err.Error(), http.StatusBadRequest)
		return
	}

	// Capture log lines keyed by jobID, capped at consoleLineCap.
	if planID != "" && len(lines) > 0 {
		job := s.actions.LookupJobByPlanID(planID)
		s.store.Mu.Lock()
		lines = s.store.RedactLogLinesLocked(planID, lines)
		if job != nil {
			existing := s.store.LogLines[job.ID]
			switch {
			case len(existing) > 0 && existing[len(existing)-1] == consoleTruncationMarker:
				// Already capped; drop later lines.
			case len(existing)+len(lines) <= consoleLineCap:
				s.store.LogLines[job.ID] = append(existing, lines...)
			default:
				if keep := consoleLineCap - len(existing); keep > 0 {
					existing = append(existing, lines[:keep]...)
				}
				s.store.LogLines[job.ID] = append(existing, consoleTruncationMarker)
			}
		}
		s.store.Mu.Unlock()
	}
	for _, line := range lines {
		s.logger.Info().Str("recordId", recordID).Str("line", line).Msg("console")
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{"count": len(lines)})
}

// decodeConsoleLines decodes a web-console-log POST body. actions/runner sends a
// TimelineRecordFeedLinesWrapper ({"count", "value", "stepId"}); a bare line
// array is accepted too.
func decodeConsoleLines(body []byte) ([]string, error) {
	var wrapper struct {
		Value []string `json:"value"`
	}
	if err := json.Unmarshal(body, &wrapper); err == nil && wrapper.Value != nil {
		return wrapper.Value, nil
	}
	var bare []string
	if err := json.Unmarshal(body, &bare); err != nil {
		return nil, err
	}
	return bare, nil
}

func (s *Server) handleTimelineAttachment(w http.ResponseWriter, r *http.Request) {
	attachType := r.PathValue("attachType")
	name := r.PathValue("name")
	s.logger.Debug().Str("type", attachType).Str("name", name).Msg("timeline attachment")

	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, stepSummaryCap+1))
	if err != nil {
		s.logger.Error().Err(err).Str("type", attachType).Str("name", name).Msg("timeline attachment: read body")
		writeJSON(w, http.StatusBadRequest, map[string]interface{}{"status": "error", "message": err.Error()})
		return
	}
	if len(body) > stepSummaryCap {
		writeJSON(w, http.StatusRequestEntityTooLarge, map[string]interface{}{"status": "error", "message": "timeline attachment exceeds 1 MiB"})
		return
	}
	if strings.Contains(strings.ToLower(attachType), "summary") {
		planID := r.PathValue("planId")
		s.store.Mu.Lock()
		for _, wf := range s.store.Workflows {
			for _, job := range wf.Jobs {
				if job.PlanID != planID {
					continue
				}
				if job.Summary != "" && len(body) > 0 {
					body = append([]byte{'\n'}, body...)
				}
				if len(job.Summary)+len(body) > stepSummaryCap {
					s.store.Mu.Unlock()
					writeJSON(w, http.StatusRequestEntityTooLarge, map[string]interface{}{"status": "error", "message": "job summary exceeds 1 MiB"})
					return
				}
				job.Summary += string(body)
				s.store.PersistWorkflowRecord(wf)
				s.store.Mu.Unlock()
				writeJSON(w, http.StatusOK, map[string]interface{}{"status": "ok"})
				return
			}
		}
		s.store.Mu.Unlock()
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"status": "ok"})
}
