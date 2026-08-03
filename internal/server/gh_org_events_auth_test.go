package bleephub

import (
	"net/http"
	"testing"
)

// TestOrgEvents_AuthRequired verifies GET /orgs/{org}/events
// requires an authenticated caller (GitHub rejects anonymous access), while
// GET /events remains a public, no-auth feed. The org feed stays
// public-repo-only for now; the fix here is the missing authentication gate.
func TestOrgEvents_AuthRequired(t *testing.T) {
	s := newTestServer()
	initPhase8Routes(s)
	// The per-org feed lives in registerGHOrgEventsRoutes; the global public
	// feed (GET /api/v3/events) lives in registerGHEventsFeedsRoutes. Both
	// are wired here so the test exercises the same surface production does.
	s.registerGHOrgEventsRoutes()
	s.registerGHEventsFeedsRoutes()

	admin := s.store.LookupUserByLogin("admin")
	org := s.store.CreateOrg(admin, "auth-events-org", "Auth Events Org", "")
	if org == nil {
		t.Fatal("create org failed")
	}
	repo := s.store.CreateOrgRepo(org, admin, "auth-events-repo", "", false)
	if repo == nil {
		t.Fatal("create org repo failed")
	}
	// Seed a real issue so the authenticated feed is non-empty.
	if issue := s.store.CreateIssue(repo.ID, admin.ID, "seed issue", "", nil, nil, 0); issue == nil {
		t.Fatal("create seed issue failed")
	}

	token := adminTokenFor(s)
	orgPath := "/api/v3/orgs/auth-events-org/events"

	// Anonymous GET /orgs/{org}/events must be rejected: GitHub requires auth.
	w := pagedJSONRequest(t, s, "GET", orgPath, "", nil)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous org events = %d, want 401", w.Code)
	}

	// Authenticated GET /orgs/{org}/events succeeds and surfaces the activity.
	w = pagedJSONRequest(t, s, "GET", orgPath, token, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("authenticated org events = %d, want 200", w.Code)
	}
	events := mustDecodeJSONList(t, w.Body.Bytes())
	if len(events) == 0 {
		t.Fatalf("authenticated org events feed empty, want the seeded IssuesEvent")
	}

	// GET /events is the public global feed and does not require auth.
	w = pagedJSONRequest(t, s, "GET", "/api/v3/events", "", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("public events = %d, want 200", w.Code)
	}
	publicEvents := mustDecodeJSONList(t, w.Body.Bytes())
	if len(publicEvents) == 0 {
		t.Fatalf("public events feed empty, want the seeded IssuesEvent")
	}
}
