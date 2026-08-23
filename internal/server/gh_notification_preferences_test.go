package bleephub

import (
	"net/http"
	"testing"

	"github.com/e6qu/bleephub/internal/store"
)

// TestNotificationPreferencesDefaultToGitHubsDefaults: an account that has
// never opened the page still gets a complete, renderable document.
func TestNotificationPreferencesDefaultToGitHubsDefaults(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)

	body := decodeBody(t, s.get(t, "/ui-data/user/notification-settings", defaultToken), http.StatusOK)
	for _, section := range []string{"participating", "watching"} {
		channels, ok := body[section].(map[string]interface{})
		if !ok || channels["email"] != true || channels["web"] != true {
			t.Fatalf("%s defaults = %v, want both channels on", section, body[section])
		}
	}
	if body["automatically_watch_repositories"] != true || body["automatically_watch_teams"] != false {
		t.Errorf("automatic-watching defaults = %v / %v", body["automatically_watch_repositories"], body["automatically_watch_teams"])
	}
	if body["actions_failed_workflows_only"] != true {
		t.Errorf("Actions should default to failed runs only: %v", body["actions_failed_workflows_only"])
	}
	events, ok := body["events"].(map[string]interface{})
	if !ok {
		t.Fatalf("no per-event-type section: %v", body)
	}
	for _, event := range store.NotificationEventTypes {
		channels, ok := events[event].(map[string]interface{})
		if !ok || channels["email"] != true || channels["web"] != true {
			t.Errorf("event %q defaults = %v, want both channels on", event, events[event])
		}
	}
}

// TestNotificationPreferencesRoundTripPerType: a per-type choice survives the
// write, and an event key the model does not define is dropped rather than
// letting a caller grow the stored document.
func TestNotificationPreferencesRoundTripPerType(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)

	payload := map[string]interface{}{
		"participating":                    map[string]bool{"email": false, "web": true},
		"watching":                         map[string]bool{"email": false, "web": false},
		"automatically_watch_repositories": false,
		"automatically_watch_teams":        true,
		"include_own_updates":              true,
		"actions_failed_workflows_only":    false,
		"dependabot_weekly_digest":         false,
		"events": map[string]interface{}{
			"issue":        map[string]bool{"email": false, "web": true},
			"pull_request": map[string]bool{"email": true, "web": false},
			"not_an_event": map[string]bool{"email": true, "web": true},
		},
	}
	written := decodeBody(t, s.put(t, "/ui-data/user/notification-settings", defaultToken, payload), http.StatusOK)
	read := decodeBody(t, s.get(t, "/ui-data/user/notification-settings", defaultToken), http.StatusOK)

	for name, body := range map[string]map[string]interface{}{"write response": written, "read back": read} {
		events, _ := body["events"].(map[string]interface{})
		if _, present := events["not_an_event"]; present {
			t.Errorf("%s kept an undefined event type: %v", name, events)
		}
		issue, _ := events["issue"].(map[string]interface{})
		if issue["email"] != false || issue["web"] != true {
			t.Errorf("%s lost the per-type issue choice: %v", name, events["issue"])
		}
		pull, _ := events["pull_request"].(map[string]interface{})
		if pull["email"] != true || pull["web"] != false {
			t.Errorf("%s lost the per-type pull-request choice: %v", name, events["pull_request"])
		}
		// An event the caller omitted comes back at its default rather than
		// vanishing, so the page always has every control to render.
		release, _ := events["release"].(map[string]interface{})
		if release["email"] != true || release["web"] != true {
			t.Errorf("%s dropped an omitted event type: %v", name, events["release"])
		}
		if body["automatically_watch_teams"] != true || body["include_own_updates"] != true {
			t.Errorf("%s lost a section toggle: %v", name, body)
		}
	}
}

// TestNotificationPreferencesGateTheWebInbox is what separates this from a
// decorative settings page: switching a delivery channel off actually stops
// those threads arriving.
func TestNotificationPreferencesGateTheWebInbox(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	repo := s.createTestRepo(t)
	s.createIssueForTest(t, repo, "authored by the viewer")

	inbox := func() []map[string]interface{} {
		resp := s.get(t, "/api/v3/notifications", defaultToken)
		return decodeJSONArray(t, resp)
	}
	if len(inbox()) == 0 {
		t.Fatal("the viewer should have a notification for the issue they authored")
	}

	// "author" is a participating reason: turning the participating web channel
	// off must empty the inbox.
	decodeBody(t, s.put(t, "/ui-data/user/notification-settings", defaultToken, map[string]interface{}{
		"participating": map[string]bool{"email": true, "web": false},
		"watching":      map[string]bool{"email": true, "web": true},
	}), http.StatusOK)
	if got := len(inbox()); got != 0 {
		t.Fatalf("participating→web off still delivered %d thread(s)", got)
	}

	// Restore the class, but silence the issue event type: same outcome, by a
	// different gate.
	decodeBody(t, s.put(t, "/ui-data/user/notification-settings", defaultToken, map[string]interface{}{
		"participating": map[string]bool{"email": true, "web": true},
		"watching":      map[string]bool{"email": true, "web": true},
		"events":        map[string]interface{}{"issue": map[string]bool{"email": true, "web": false}},
	}), http.StatusOK)
	if got := len(inbox()); got != 0 {
		t.Fatalf("issue→web off still delivered %d thread(s)", got)
	}

	// Everything back on restores delivery.
	decodeBody(t, s.put(t, "/ui-data/user/notification-settings", defaultToken, map[string]interface{}{
		"participating": map[string]bool{"email": true, "web": true},
		"watching":      map[string]bool{"email": true, "web": true},
	}), http.StatusOK)
	if len(inbox()) == 0 {
		t.Fatal("restoring the preferences should restore delivery")
	}
}

func TestNotificationEventTypeForThread(t *testing.T) {
	t.Parallel()
	cases := map[string]string{
		"Issue":       store.NotificationEventIssue,
		"PullRequest": store.NotificationEventPullRequest,
		"Release":     store.NotificationEventRelease,
		"CheckSuite":  store.NotificationEventActions,
		"Nonsense":    "",
	}
	for subject, want := range cases {
		if got := store.NotificationEventTypeForThread(subject); got != want {
			t.Errorf("NotificationEventTypeForThread(%q) = %q, want %q", subject, got, want)
		}
	}
}
