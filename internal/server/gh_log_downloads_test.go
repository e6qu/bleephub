package bleephub

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

// A step's log is served on its own, by the step's zero-based position, through
// the same one-minute link the job's log uses; a position past the job's steps,
// or one without an uploaded log, is 404.
func TestStepLogsDownloadOneStep(t *testing.T) {
	s := newTimelineTestServer()
	_, wfJob := seedRun(t, s, "octo/repo", "completed", "success")
	planID, timelineID := linkJobToPlan(t, s, wfJob)
	first := createLogFile(t, s, planID)
	uploadLogBlock(t, s, planID, first, []byte("checkout output\n"))
	second := createLogFile(t, s, planID)
	uploadLogBlock(t, s, planID, second, []byte("build output\n"))
	patchTimelineRecords(t, s, planID, timelineID, true, []map[string]any{
		{"id": uuid.New().String(), "type": "Task", "name": "checkout", "order": 1,
			"state": "completed", "result": "succeeded", "log": map[string]any{"id": first}},
		{"id": uuid.New().String(), "type": "Task", "name": "build", "order": 2,
			"state": "completed", "result": "succeeded", "log": map[string]any{"id": second}},
		{"id": uuid.New().String(), "type": "Task", "name": "pending", "order": 3, "state": "inProgress"},
	})
	base := fmt.Sprintf("/api/v3/repos/octo/repo/actions/jobs/%d/steps", stableJobID(wfJob.JobID))

	for position, want := range []string{"checkout output\n", "build output\n"} {
		w := followLogDownload(t, s, runRequest(s, "GET", fmt.Sprintf("%s/%d/logs", base, position)))
		body, _ := io.ReadAll(w.Body)
		if w.Code != http.StatusOK || string(body) != want {
			t.Errorf("step %d: status %d body %q, want %q", position, w.Code, body, want)
		}
		if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
			t.Errorf("step %d: Content-Type = %q", position, ct)
		}
	}
	for _, position := range []string{"2", "3", "-1", "x"} {
		if w := runRequest(s, "GET", base+"/"+position+"/logs"); w.Code != http.StatusNotFound {
			t.Errorf("step %s: status %d, want 404", position, w.Code)
		}
	}
}

// The link a log download redirects to is the whole credential: a tampered or
// expired one serves nothing.
func TestLogDownloadLinkCannotBeForgedOrReused(t *testing.T) {
	s := newTimelineTestServer()
	_, wfJob := seedRun(t, s, "octo/repo", "completed", "success")
	planID, timelineID := linkJobToPlan(t, s, wfJob)
	logID := createLogFile(t, s, planID)
	uploadLogBlock(t, s, planID, logID, []byte("secret output\n"))
	patchTimelineRecords(t, s, planID, timelineID, true, []map[string]any{
		{"id": uuid.New().String(), "type": "Task", "name": "only", "order": 1,
			"state": "completed", "result": "succeeded", "log": map[string]any{"id": logID}},
	})
	w := runRequest(s, "GET", fmt.Sprintf("/api/v3/repos/octo/repo/actions/jobs/%d/logs", stableJobID(wfJob.JobID)))
	if w.Code != http.StatusFound {
		t.Fatalf("job logs status = %d, want 302", w.Code)
	}
	location, _ := url.Parse(w.Header().Get("Location"))
	ticket := strings.TrimPrefix(location.Path, "/_logs/")
	payload, sig, _ := strings.Cut(ticket, ".")

	fetch := func(ticket string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, "/_logs/"+ticket, nil)
		req.SetPathValue("ticket", ticket)
		got := httptest.NewRecorder()
		s.handleLogDownload(got, req)
		return got
	}
	if got := fetch(payload + "." + strings.Repeat("A", len(sig))); got.Code != http.StatusNotFound {
		t.Errorf("a forged signature served status %d", got.Code)
	}
	expired := s.signLogDownloadForTest(t, logDownload{Kind: "job", Repo: "octo/repo", JobID: stableJobID(wfJob.JobID), Expires: s.currentTime().Add(-time.Minute).Unix()})
	if got := fetch(expired); got.Code != http.StatusGone {
		t.Errorf("an expired link served status %d, want 410", got.Code)
	}
}

func (s *Server) signLogDownloadForTest(t *testing.T, d logDownload) string {
	t.Helper()
	ticket, err := s.signLogDownload(d)
	if err != nil {
		t.Fatal(err)
	}
	return ticket
}
