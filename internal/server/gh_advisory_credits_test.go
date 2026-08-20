package bleephub

import (
	"net/http"
	"testing"
)

func TestSecurityAdvisoryCredits(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	repo := s.createTestRepo(t)
	s.createTestUser(t, "credit-finder")

	created := decodeJSONWithStatus(t, s.post(t, repo.path()+"/security-advisories", defaultToken,
		map[string]interface{}{
			"summary":  "credited advisory",
			"severity": "high",
			"credits": []map[string]interface{}{
				{"login": "credit-finder", "type": "finder"},
				{"login": "admin", "type": "remediation_developer"},
			},
		}), http.StatusCreated)

	assertCredits := func(adv map[string]interface{}, context string) {
		t.Helper()
		credits, _ := adv["credits"].([]interface{})
		detailed, _ := adv["credits_detailed"].([]interface{})
		if len(credits) != 2 || len(detailed) != 2 {
			t.Fatalf("%s: credits=%d credits_detailed=%d, want 2/2", context, len(credits), len(detailed))
		}
		first, _ := credits[0].(map[string]interface{})
		if first["login"] != "credit-finder" || first["type"] != "finder" {
			t.Errorf("%s: credits[0] = %v, want {credit-finder finder}", context, first)
		}
		det, _ := detailed[0].(map[string]interface{})
		if det["type"] != "finder" || det["state"] != "accepted" {
			t.Errorf("%s: credits_detailed[0] = %v, want type finder state accepted", context, det)
		}
		user, _ := det["user"].(map[string]interface{})
		if user == nil || user["login"] != "credit-finder" {
			t.Errorf("%s: credits_detailed[0].user = %v, want the resolved credit-finder object", context, det["user"])
		}
	}
	assertCredits(created, "create response")

	// The stored credits render on reads too.
	ghsaID := created["ghsa_id"].(string)
	fetched := decodeJSONWithStatus(t, s.get(t, repo.path()+"/security-advisories/"+ghsaID, defaultToken), http.StatusOK)
	assertCredits(fetched, "get response")

	// An update without a credits member keeps them; a present list replaces.
	kept := decodeJSONWithStatus(t, s.patch(t, repo.path()+"/security-advisories/"+ghsaID, defaultToken,
		map[string]interface{}{"summary": "retitled"}), http.StatusOK)
	assertCredits(kept, "credit-less update keeps credits")
	replaced := decodeJSONWithStatus(t, s.patch(t, repo.path()+"/security-advisories/"+ghsaID, defaultToken,
		map[string]interface{}{"credits": []map[string]interface{}{{"login": "admin", "type": "reporter"}}}), http.StatusOK)
	credits, _ := replaced["credits"].([]interface{})
	if len(credits) != 1 || credits[0].(map[string]interface{})["type"] != "reporter" {
		t.Errorf("update did not replace credits: %v", replaced["credits"])
	}
	cleared := decodeJSONWithStatus(t, s.patch(t, repo.path()+"/security-advisories/"+ghsaID, defaultToken,
		map[string]interface{}{"credits": []map[string]interface{}{}}), http.StatusOK)
	if credits, _ := cleared["credits"].([]interface{}); len(credits) != 0 {
		t.Errorf("empty credits list did not clear: %v", cleared["credits"])
	}
}

func TestSecurityAdvisoryCreditsValidation(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	repo := s.createTestRepo(t)

	// Unknown login → 422.
	requireStatus(t, s.post(t, repo.path()+"/security-advisories", defaultToken,
		map[string]interface{}{
			"summary": "bad login", "severity": "low",
			"credits": []map[string]interface{}{{"login": "nobody-here", "type": "finder"}},
		}), http.StatusUnprocessableEntity)
	// Type outside the security-advisory-credit-types enum → 422.
	requireStatus(t, s.post(t, repo.path()+"/security-advisories", defaultToken,
		map[string]interface{}{
			"summary": "bad type", "severity": "low",
			"credits": []map[string]interface{}{{"login": "admin", "type": "hero"}},
		}), http.StatusUnprocessableEntity)

	// The update endpoint enforces the same constraints.
	created := decodeJSONWithStatus(t, s.post(t, repo.path()+"/security-advisories", defaultToken,
		map[string]interface{}{"summary": "no credits yet", "severity": "low"}), http.StatusCreated)
	ghsaID := created["ghsa_id"].(string)
	requireStatus(t, s.patch(t, repo.path()+"/security-advisories/"+ghsaID, defaultToken,
		map[string]interface{}{"credits": []map[string]interface{}{{"login": "admin", "type": "hero"}}}),
		http.StatusUnprocessableEntity)

	// The private-vulnerability-report request shape has no credits member
	// (spec: private-vulnerability-report-create), so a report never seeds any.
	report := decodeJSONWithStatus(t, s.post(t, repo.path()+"/security-advisories/reports", defaultToken,
		map[string]interface{}{
			"summary": "reported", "severity": "low",
			"credits": []map[string]interface{}{{"login": "admin", "type": "finder"}},
		}), http.StatusCreated)
	if credits, _ := report["credits"].([]interface{}); len(credits) != 0 {
		t.Errorf("report seeded credits: %v", report["credits"])
	}

	// A non-writer cannot create a credited advisory at all (authz unchanged:
	// the classic-scope gate refuses the under-scoped token).
	_, readerTok := s.newUser(t, "advisory-reader")
	requireStatus(t, s.post(t, repo.path()+"/security-advisories", readerTok,
		map[string]interface{}{
			"summary": "denied", "severity": "low",
			"credits": []map[string]interface{}{{"login": "admin", "type": "finder"}},
		}), http.StatusForbidden)
}
