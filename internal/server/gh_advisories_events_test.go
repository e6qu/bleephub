package bleephub

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
)

// Webhook and authorization tests for the security-advisory vertical.

// advisoryEventRecorder captures whole deliveries, not just event/action, so
// a test can assert on the payload an SDK would decode.
type advisoryEventRecorder struct {
	mu        sync.Mutex
	delivered []advisoryDelivery
}

type advisoryDelivery struct {
	event   string
	action  string
	payload map[string]interface{}
}

func (r *advisoryEventRecorder) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, request *http.Request) {
		var payload map[string]interface{}
		_ = json.NewDecoder(request.Body).Decode(&payload)
		action, _ := payload["action"].(string)
		r.mu.Lock()
		r.delivered = append(r.delivered, advisoryDelivery{
			event:   request.Header.Get("X-GitHub-Event"),
			action:  action,
			payload: payload,
		})
		r.mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}
}

func (r *advisoryEventRecorder) find(event, action string) (advisoryDelivery, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, delivery := range r.delivered {
		if delivery.event == event && delivery.action == action {
			return delivery, true
		}
	}
	return advisoryDelivery{}, false
}

func (r *advisoryEventRecorder) has(event, action string) bool {
	_, ok := r.find(event, action)
	return ok
}

// subscribe points a repository hook at the recorder, subscribed to every
// event, so the test observes exactly what a real subscriber would.
func (f *advisoryFixture) subscribe(t *testing.T) *advisoryEventRecorder {
	t.Helper()
	recorder := &advisoryEventRecorder{}
	receiver := httptest.NewTLSServer(recorder.handler())
	t.Cleanup(receiver.Close)
	f.server.store.CreateHook(f.repo.FullName, receiver.URL, "", "json", "0", []string{"*"}, true)
	return recorder
}

// TestDependabotAlertWebhookLifecycle covers the four actions an alert's life
// produces and the payload each carries.
func TestDependabotAlertWebhookLifecycle(t *testing.T) {
	t.Parallel()
	f := newAdvisoryFixture(t, "events", false)
	recorder := f.subscribe(t)

	f.draftAdvisory(t)
	// A draft advisory is under embargo: no event leaves the repository and
	// no alert is derived from it.
	f.publishAdvisory(t)

	waitUntil(t, "dependabot_alert created", func() bool { return recorder.has("dependabot_alert", "created") })
	created, _ := recorder.find("dependabot_alert", "created")

	alert, _ := created.payload["alert"].(map[string]interface{})
	if alert == nil {
		t.Fatalf("dependabot_alert delivery carried no alert: %v", created.payload)
	}
	if alert["state"] != "open" {
		t.Errorf("alert state = %v, want open", alert["state"])
	}
	dependency, _ := alert["dependency"].(map[string]interface{})
	dependencyPackage, _ := dependency["package"].(map[string]interface{})
	if dependencyPackage["name"] != "lodash" || dependencyPackage["ecosystem"] != "npm" {
		t.Errorf("alert dependency package = %v", dependencyPackage)
	}
	if dependency["manifest_path"] != "package-lock.json" {
		t.Errorf("alert manifest_path = %v", dependency["manifest_path"])
	}
	advisory, _ := alert["security_advisory"].(map[string]interface{})
	if advisory["ghsa_id"] != f.ghsaID {
		t.Errorf("alert security_advisory ghsa_id = %v, want %q", advisory["ghsa_id"], f.ghsaID)
	}
	if alert["dismissed_by"] != nil || alert["dismissed_reason"] != nil {
		t.Errorf("a newly created alert carries dismissal members: %v", alert)
	}
	if created.payload["repository"] == nil {
		t.Error("dependabot_alert delivery carried no repository")
	}

	// Publication is announced too; wait, since these deliveries dispatch asynchronously and a bare read races the delivery goroutine under load.
	waitUntil(t, "repository_advisory published", func() bool {
		return recorder.has("repository_advisory", "published")
	})
	waitUntil(t, "security_advisory published", func() bool {
		return recorder.has("security_advisory", "published")
	})

	resp := f.server.patch(t, "/api/v3/repos/"+f.repo.FullName+"/dependabot/alerts/1", f.ownerToken,
		map[string]interface{}{"state": "dismissed", "dismissed_reason": "tolerable_risk", "dismissed_comment": "acceptable"})
	decodeJSONWithStatus(t, resp, http.StatusOK)
	waitUntil(t, "dependabot_alert dismissed", func() bool { return recorder.has("dependabot_alert", "dismissed") })
	dismissed, _ := recorder.find("dependabot_alert", "dismissed")
	dismissedAlert, _ := dismissed.payload["alert"].(map[string]interface{})
	if dismissedAlert["dismissed_reason"] != "tolerable_risk" {
		t.Errorf("dismissed alert reason = %v", dismissedAlert["dismissed_reason"])
	}
	// The dismisser is the whole account object, not just the login the
	// store happens to record.
	dismisser, _ := dismissedAlert["dismissed_by"].(map[string]interface{})
	if dismisser == nil || dismisser["login"] != f.owner.Login {
		t.Errorf("dismissed_by = %v, want the acting account", dismissedAlert["dismissed_by"])
	}
	if dismissed.payload["sender"] == nil {
		t.Error("a user-driven dismissal carried no sender")
	}

	resp = f.server.patch(t, "/api/v3/repos/"+f.repo.FullName+"/dependabot/alerts/1", f.ownerToken,
		map[string]interface{}{"state": "open"})
	decodeJSONWithStatus(t, resp, http.StatusOK)
	waitUntil(t, "dependabot_alert reopened", func() bool { return recorder.has("dependabot_alert", "reopened") })

	// Upgrading past the range fixes it, and downgrading reintroduces it.
	f.submitSnapshot(t, "4.17.21")
	waitUntil(t, "dependabot_alert fixed", func() bool { return recorder.has("dependabot_alert", "fixed") })
	f.submitSnapshot(t, "4.17.19")
	waitUntil(t, "dependabot_alert reintroduced", func() bool {
		return recorder.has("dependabot_alert", "reintroduced")
	})
}

// TestRepositoryAdvisoryReportedEvent covers the one draft-stage transition
// that is announced: a private vulnerability report the maintainers must be
// told about.
func TestRepositoryAdvisoryReportedEvent(t *testing.T) {
	t.Parallel()
	f := newAdvisoryFixture(t, "reported", false)
	recorder := f.subscribe(t)

	resp := f.server.post(t, "/api/v3/repos/"+f.repo.FullName+"/security-advisories/reports", f.ownerToken,
		map[string]interface{}{
			"summary":     "Reported privately",
			"description": "Found while fuzzing.",
			"severity":    "critical",
		})
	decodeJSONWithStatus(t, resp, http.StatusCreated)

	waitUntil(t, "repository_advisory reported", func() bool {
		return recorder.has("repository_advisory", "reported")
	})
	reported, _ := recorder.find("repository_advisory", "reported")
	advisory, _ := reported.payload["repository_advisory"].(map[string]interface{})
	if advisory == nil || advisory["summary"] != "Reported privately" {
		t.Errorf("repository_advisory payload = %v", reported.payload)
	}
	// A report is not a publication: the global database must not have been
	// told about an advisory still under embargo.
	if recorder.has("security_advisory", "published") {
		t.Error("a private report announced itself to the global advisory database")
	}
}

// TestSecurityAdvisoryWithdrawalEvent covers the update and withdrawal
// actions of the global advisory event.
func TestSecurityAdvisoryWithdrawalEvent(t *testing.T) {
	t.Parallel()
	f := newAdvisoryFixture(t, "withdrawevent", false)
	f.draftAdvisory(t)
	f.publishAdvisory(t)
	recorder := f.subscribe(t)

	resp := f.server.patch(t, "/api/v3/repos/"+f.repo.FullName+"/security-advisories/"+f.ghsaID,
		f.ownerToken, map[string]interface{}{"summary": "Prototype pollution in lodash (revised)"})
	decodeJSONWithStatus(t, resp, http.StatusOK)
	waitUntil(t, "security_advisory updated", func() bool { return recorder.has("security_advisory", "updated") })

	resp = f.server.patch(t, "/api/v3/repos/"+f.repo.FullName+"/security-advisories/"+f.ghsaID,
		f.ownerToken, map[string]interface{}{"state": "withdrawn"})
	decodeJSONWithStatus(t, resp, http.StatusOK)
	waitUntil(t, "security_advisory withdrawn", func() bool {
		return recorder.has("security_advisory", "withdrawn")
	})
	withdrawn, _ := recorder.find("security_advisory", "withdrawn")
	advisory, _ := withdrawn.payload["security_advisory"].(map[string]interface{})
	if advisory == nil || advisory["withdrawn_at"] == nil {
		t.Errorf("withdrawn security_advisory carried no withdrawn_at: %v", withdrawn.payload)
	}
}

// TestSecurityFindingsRequireSecurityAccess is the REST half of the
// cross-tenant isolation contract, and pins the two leaks the GraphQL work
// surfaced: on a PUBLIC repository, an unrelated authenticated account could
// read every Dependabot alert and every embargoed draft advisory.
func TestSecurityFindingsRequireSecurityAccess(t *testing.T) {
	t.Parallel()
	f := newAdvisoryFixture(t, "restisolation", false)
	f.draftAdvisory(t)

	// The draft is invisible to a stranger, both individually and in the list.
	resp := f.server.get(t, "/api/v3/repos/"+f.repo.FullName+"/security-advisories/"+f.ghsaID, f.strangerToken)
	if resp.StatusCode != http.StatusNotFound {
		resp.Body.Close()
		t.Errorf("GET a drafted advisory as a stranger = %d, want 404", resp.StatusCode)
	}
	resp.Body.Close()

	resp = f.server.get(t, "/api/v3/repos/"+f.repo.FullName+"/security-advisories", f.strangerToken)
	list := decodeJSONArrayWithStatus(t, resp, http.StatusOK)
	if len(list) != 0 {
		t.Errorf("a stranger listed %d advisories of a public repository, want 0 while all are drafts", len(list))
	}

	// The owner still sees it — a guard that refuses everybody is not a fix.
	resp = f.server.get(t, "/api/v3/repos/"+f.repo.FullName+"/security-advisories/"+f.ghsaID, f.ownerToken)
	decodeJSONWithStatus(t, resp, http.StatusOK)
	resp = f.server.get(t, "/api/v3/repos/"+f.repo.FullName+"/security-advisories", f.ownerToken)
	if owned := decodeJSONArrayWithStatus(t, resp, http.StatusOK); len(owned) != 1 {
		t.Errorf("the owner listed %d advisories, want 1", len(owned))
	}

	// Publishing makes it visible to everyone, through the repository
	// endpoint as well as the global database.
	f.publishAdvisory(t)
	resp = f.server.get(t, "/api/v3/repos/"+f.repo.FullName+"/security-advisories/"+f.ghsaID, f.strangerToken)
	decodeJSONWithStatus(t, resp, http.StatusOK)
	resp = f.server.get(t, "/api/v3/advisories/"+f.ghsaID, f.strangerToken)
	decodeJSONWithStatus(t, resp, http.StatusOK)

	// Alerts stay private whatever the advisory's state: they say which
	// vulnerable version THIS repository is running.
	resp = f.server.get(t, "/api/v3/repos/"+f.repo.FullName+"/dependabot/alerts", f.strangerToken)
	if resp.StatusCode != http.StatusNotFound {
		body := decodeJSONArrayWithStatus(t, resp, resp.StatusCode)
		t.Errorf("a stranger read %d Dependabot alerts of a public repository (status %d), want 404",
			len(body), resp.StatusCode)
	} else {
		resp.Body.Close()
	}
	resp = f.server.get(t, "/api/v3/repos/"+f.repo.FullName+"/dependabot/alerts/1", f.strangerToken)
	if resp.StatusCode != http.StatusNotFound {
		resp.Body.Close()
		t.Errorf("a stranger read a single Dependabot alert = %d, want 404", resp.StatusCode)
	} else {
		resp.Body.Close()
	}

	// The owner still reads them.
	resp = f.server.get(t, "/api/v3/repos/"+f.repo.FullName+"/dependabot/alerts", f.ownerToken)
	if alerts := decodeJSONArrayWithStatus(t, resp, http.StatusOK); len(alerts) != 1 {
		t.Errorf("the owner read %d alerts, want 1", len(alerts))
	}
}

// TestAdvisoryCollaboratorsAndPrivateFork covers the drafting-workspace
// members and the temporary fork, both of which the response used to report
// as permanently empty regardless of what the request asked for.
func TestAdvisoryCollaboratorsAndPrivateFork(t *testing.T) {
	t.Parallel()
	f := newAdvisoryFixture(t, "collaborators", false)
	helper := f.server.createTestUser(t, "adv-helper-collaborators")

	resp := f.server.post(t, "/api/v3/repos/"+f.repo.FullName+"/security-advisories", f.ownerToken,
		map[string]interface{}{
			"summary":             "Needs a fix together",
			"description":         "d",
			"severity":            "high",
			"cve_id":              "CVE-2026-4242",
			"cvss_vector_string":  "CVSS:3.1/AV:N/AC:L/PR:N/UI:N/S:U/C:H/I:H/A:H",
			"collaborating_users": []string{helper.Login},
			"start_private_fork":  true,
			"vulnerabilities": []map[string]interface{}{{
				"package":                  map[string]interface{}{"ecosystem": "npm", "name": "lodash"},
				"vulnerable_version_range": "< 4.17.21",
				"vulnerable_functions":     []string{"merge", "set"},
			}},
		})
	created := decodeJSONWithStatus(t, resp, http.StatusCreated)

	// cve_id and cvss_vector_string are the spec's member names; both used to
	// be dropped silently because the decoder looked for other spellings.
	if created["cve_id"] != "CVE-2026-4242" {
		t.Errorf("cve_id = %v, want the requested identifier", created["cve_id"])
	}
	cvss, _ := created["cvss"].(map[string]interface{})
	if cvss["vector_string"] != "CVSS:3.1/AV:N/AC:L/PR:N/UI:N/S:U/C:H/I:H/A:H" {
		t.Errorf("cvss.vector_string = %v", cvss["vector_string"])
	}
	// The score is computed from the vector rather than reported as absent.
	if score, ok := cvss["score"].(float64); !ok || score != 9.8 {
		t.Errorf("cvss.score = %v, want 9.8 computed from the vector", cvss["score"])
	}

	collaborators, _ := created["collaborating_users"].([]interface{})
	if len(collaborators) != 1 {
		t.Fatalf("collaborating_users = %v, want the one requested account", created["collaborating_users"])
	}
	collaborator, _ := collaborators[0].(map[string]interface{})
	if collaborator["login"] != helper.Login {
		t.Errorf("collaborating user = %v, want %q", collaborator["login"], helper.Login)
	}

	if created["private_fork"] == nil {
		t.Error("start_private_fork produced no private fork")
	}

	vulnerabilities, _ := created["vulnerabilities"].([]interface{})
	if len(vulnerabilities) != 1 {
		t.Fatalf("vulnerabilities = %v", created["vulnerabilities"])
	}
	vulnerability, _ := vulnerabilities[0].(map[string]interface{})
	functions, _ := vulnerability["vulnerable_functions"].([]interface{})
	if len(functions) != 2 || functions[0] != "merge" {
		t.Errorf("vulnerable_functions = %v, want the two requested names", vulnerability["vulnerable_functions"])
	}

	// The update path accepts the same members, and [] clears the list.
	ghsaID, _ := created["ghsa_id"].(string)
	resp = f.server.patch(t, "/api/v3/repos/"+f.repo.FullName+"/security-advisories/"+ghsaID, f.ownerToken,
		map[string]interface{}{"collaborating_users": []string{}, "cve_id": "CVE-2026-9999"})
	updated := decodeJSONWithStatus(t, resp, http.StatusOK)
	if remaining, _ := updated["collaborating_users"].([]interface{}); len(remaining) != 0 {
		t.Errorf("collaborating_users after clearing = %v", updated["collaborating_users"])
	}
	if updated["cve_id"] != "CVE-2026-9999" {
		t.Errorf("cve_id after update = %v", updated["cve_id"])
	}
}
