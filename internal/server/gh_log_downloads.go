package bleephub

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/e6qu/bleephub/internal/store"
)

// GitHub answers a log download with 302 and a Location that serves the bytes
// for a minute without the caller's credential; clients such as go-github read
// that Location and refuse any other status. A logDownload is what such a
// link grants, signed with a per-process key, so a ticket outlives neither its
// minute nor the process.
type logDownload struct {
	Kind    string `json:"k"` // "job", "step", "run" or "attempt"
	Repo    string `json:"r"`
	JobID   int64  `json:"j,omitempty"`
	Step    int    `json:"s,omitempty"`
	RunID   int    `json:"u,omitempty"`
	Attempt int    `json:"a,omitempty"`
	Expires int64  `json:"e"`
}

const logDownloadLifetime = time.Minute

func (s *Server) logDownloadMAC(payload string) []byte {
	mac := hmac.New(sha256.New, s.logDownloadKey)
	mac.Write([]byte(payload))
	return mac.Sum(nil)
}

// redirectToLogDownload answers the API request with the 302 GitHub sends.
func (s *Server) redirectToLogDownload(w http.ResponseWriter, r *http.Request, d logDownload) {
	d.Expires = time.Now().Add(logDownloadLifetime).Unix()
	ticket, err := s.signLogDownload(d)
	if err != nil {
		writeGHError(w, http.StatusInternalServerError, "encode log download: "+err.Error())
		return
	}
	http.Redirect(w, r, s.baseURL(r)+"/_logs/"+ticket, http.StatusFound)
}

func (s *Server) signLogDownload(d logDownload) (string, error) {
	raw, err := json.Marshal(d)
	if err != nil {
		return "", err
	}
	payload := base64.RawURLEncoding.EncodeToString(raw)
	return payload + "." + base64.RawURLEncoding.EncodeToString(s.logDownloadMAC(payload)), nil
}

func (s *Server) handleLogDownload(w http.ResponseWriter, r *http.Request) {
	payload, sig, ok := strings.Cut(r.PathValue("ticket"), ".")
	got, err := base64.RawURLEncoding.DecodeString(sig)
	if !ok || err != nil || !hmac.Equal(got, s.logDownloadMAC(payload)) {
		http.Error(w, "invalid log download link", http.StatusNotFound)
		return
	}
	raw, err := base64.RawURLEncoding.DecodeString(payload)
	var d logDownload
	if err != nil || json.Unmarshal(raw, &d) != nil {
		http.Error(w, "invalid log download link", http.StatusNotFound)
		return
	}
	if time.Now().Unix() > d.Expires {
		http.Error(w, "log download link expired", http.StatusGone)
		return
	}
	switch d.Kind {
	case "job":
		_, job := s.findJobByStableIDInRepo(d.JobID, d.Repo)
		if job == nil {
			http.Error(w, "logs not found", http.StatusNotFound)
			return
		}
		content, found, err := s.jobLogContent(r.Context(), job.JobID)
		s.writeLogText(w, content, found, err)
	case "step":
		_, job := s.findJobByStableIDInRepo(d.JobID, d.Repo)
		if job == nil {
			http.Error(w, "logs not found", http.StatusNotFound)
			return
		}
		content, found, err := s.stepLogContent(r, job.JobID, d.Step)
		s.writeLogText(w, content, found, err)
	case "run":
		wf := s.findWorkflowByRunIDInRepo(d.RunID, d.Repo)
		if wf == nil {
			http.Error(w, "logs not found", http.StatusNotFound)
			return
		}
		s.writeRunLogsZip(r.Context(), w, wf, d.RunID)
	case "attempt":
		wf := s.findRunAttempt(d.RunID, d.Attempt, d.Repo)
		if wf == nil {
			http.Error(w, "logs not found", http.StatusNotFound)
			return
		}
		s.writeRunLogsZip(r.Context(), w, wf, d.RunID)
	default:
		http.Error(w, "invalid log download link", http.StatusNotFound)
	}
}

// runHasLogs reports whether any job of the run has a runner-uploaded log, so
// a run without one answers 404 itself instead of a link to nothing.
func (s *Server) runHasLogs(wf *store.Workflow) bool {
	s.store.Mu.RLock()
	defer s.store.Mu.RUnlock()
	for _, job := range wf.Jobs {
		if len(s.jobLogRefsLocked(job.JobID)) > 0 {
			return true
		}
	}
	return false
}

func (s *Server) writeLogText(w http.ResponseWriter, content []byte, found bool, err error) {
	if err != nil {
		http.Error(w, "log byte-store read: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if !found {
		http.Error(w, "logs not found", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(content)
}

// stepLogContent reads the log of the step at a zero-based position in the
// job, the same order the job's `steps` array numbers from one.
func (s *Server) stepLogContent(r *http.Request, jobUUID string, position int) ([]byte, bool, error) {
	s.store.Mu.RLock()
	tasks := s.taskRecordsForJobLocked(jobUUID)
	if position < 0 || position >= len(tasks) || tasks[position].Log == nil {
		s.store.Mu.RUnlock()
		return nil, false, nil
	}
	ref := jobLogRef{ID: tasks[position].Log.ID, Name: tasks[position].Name}
	memoryLogs := s.memoryLogFilesForDownloadLocked([]jobLogRef{ref})
	s.store.Mu.RUnlock()
	return s.logFileContent(r.Context(), ref.ID, memoryLogs[ref.ID])
}
