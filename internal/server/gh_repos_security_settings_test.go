package bleephub

import (
	"testing"
	"time"
)

func TestRepoVulnerabilityAlerts_CheckToggle(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	repo := s.createTestRepo(t)
	path := "/api/v3/repos/" + repo + "/vulnerability-alerts"

	resp := s.get(t, path, defaultToken)
	requireStatus(t, resp, 404)

	resp = s.put(t, path, defaultToken, nil)
	requireStatus(t, resp, 204)
	resp = s.get(t, path, defaultToken)
	requireStatus(t, resp, 204)

	resp = s.delete(t, path, defaultToken)
	requireStatus(t, resp, 204)
	resp = s.get(t, path, defaultToken)
	requireStatus(t, resp, 404)
}

func TestRepoAutomatedSecurityFixes_Check(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	repo := s.createTestRepo(t)
	path := "/api/v3/repos/" + repo + "/automated-security-fixes"

	resp := s.get(t, path, defaultToken)
	data := decodeJSONWithStatus(t, resp, 200)
	if data["enabled"] != false || data["paused"] != false {
		t.Fatalf("initial state = %v, want enabled false paused false", data)
	}

	resp = s.put(t, path, defaultToken, nil)
	requireStatus(t, resp, 204)
	resp = s.get(t, path, defaultToken)
	data = decodeJSONWithStatus(t, resp, 200)
	if data["enabled"] != true {
		t.Fatalf("enabled = %v after PUT, want true", data["enabled"])
	}
}

func TestRepoPrivateVulnerabilityReporting_Check(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	repo := s.createTestRepo(t)
	path := "/api/v3/repos/" + repo + "/private-vulnerability-reporting"

	resp := s.get(t, path, defaultToken)
	data := decodeJSONWithStatus(t, resp, 200)
	if data["enabled"] != false {
		t.Fatalf("initial enabled = %v, want false", data["enabled"])
	}

	resp = s.put(t, path, defaultToken, nil)
	requireStatus(t, resp, 204)
	resp = s.get(t, path, defaultToken)
	data = decodeJSONWithStatus(t, resp, 200)
	if data["enabled"] != true {
		t.Fatalf("enabled = %v after PUT, want true", data["enabled"])
	}
}

func TestRepoInteractionLimits_RoundTrip(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	repo := s.createTestRepo(t)
	path := "/api/v3/repos/" + repo + "/interaction-limits"

	// No restriction in effect → empty object.
	resp := s.get(t, path, defaultToken)
	data := decodeJSONWithStatus(t, resp, 200)
	if len(data) != 0 {
		t.Fatalf("initial interaction limits = %v, want {}", data)
	}

	resp = s.put(t, path, defaultToken, map[string]interface{}{
		"limit":  "collaborators_only",
		"expiry": "one_week",
	})
	set := decodeJSONWithStatus(t, resp, 200)
	if set["limit"] != "collaborators_only" || set["origin"] != "repository" {
		t.Fatalf("set response = %v", set)
	}
	expiresAt, err := time.Parse(time.RFC3339, set["expires_at"].(string))
	if err != nil {
		t.Fatalf("expires_at unparsable: %v", err)
	}
	want := fixedTestTime.Add(7 * 24 * time.Hour)
	if diff := expiresAt.Sub(want); diff < -time.Minute || diff > time.Minute {
		t.Fatalf("expires_at = %v, want ~one week out", expiresAt)
	}

	resp = s.get(t, path, defaultToken)
	got := decodeJSONWithStatus(t, resp, 200)
	if got["limit"] != "collaborators_only" || got["origin"] != "repository" {
		t.Fatalf("read-back = %v", got)
	}

	// Invalid enum values are validation failures.
	resp = s.put(t, path, defaultToken, map[string]interface{}{"limit": "everyone"})
	requireStatus(t, resp, 422)
	resp = s.put(t, path, defaultToken, map[string]interface{}{"limit": "existing_users", "expiry": "forever"})
	requireStatus(t, resp, 422)

	resp = s.delete(t, path, defaultToken)
	requireStatus(t, resp, 204)
	resp = s.get(t, path, defaultToken)
	data = decodeJSONWithStatus(t, resp, 200)
	if len(data) != 0 {
		t.Fatalf("after DELETE = %v, want {}", data)
	}
}
