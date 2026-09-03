package store

import (
	"encoding/base64"
	"encoding/json"
	"testing"
)

// sarifPayload builds a CreateSARIFUpload payload from a set of (ruleID, path,
// line) results, each a single-location finding.
func sarifPayload(t *testing.T, commitSHA, ref string, results []map[string]interface{}) map[string]interface{} {
	t.Helper()
	sarif := map[string]interface{}{
		"version": "2.1.0",
		"runs": []interface{}{
			map[string]interface{}{
				"tool":    map[string]interface{}{"driver": map[string]interface{}{"name": "CodeQL"}},
				"results": results,
			},
		},
	}
	raw, err := json.Marshal(sarif)
	if err != nil {
		t.Fatalf("marshal sarif: %v", err)
	}
	return map[string]interface{}{
		"commit_sha": commitSHA,
		"ref":        ref,
		"sarif":      base64.StdEncoding.EncodeToString(raw),
	}
}

func sarifResult(ruleID, path string, line int) map[string]interface{} {
	return map[string]interface{}{
		"ruleId":  ruleID,
		"message": map[string]interface{}{"text": ruleID + " finding"},
		"locations": []interface{}{
			map[string]interface{}{
				"physicalLocation": map[string]interface{}{
					"artifactLocation": map[string]interface{}{"uri": path},
					"region":           map[string]interface{}{"startLine": line, "endLine": line, "startColumn": 1, "endColumn": 8},
				},
			},
		},
	}
}

func openAlertCount(alerts []*CodeScanningAlert) (total, open int) {
	for _, a := range alerts {
		total++
		if a.State == CodeScanningStateOpen {
			open++
		}
	}
	return total, open
}

// TestSARIFReuploadDedupsFixesAndReopens pins that re-uploading a SARIF analysis
// correlates results to existing alerts instead of minting duplicates, marks a
// finding that disappears as fixed, and reopens it if it recurs — matching
// GitHub. Previously every result on every upload created a brand-new alert
// number, so the count doubled on each identical re-run.
func TestSARIFReuploadDedupsFixesAndReopens(t *testing.T) {
	st := NewStore()
	repo := "octo/app"
	ref := "refs/heads/main"

	two := []map[string]interface{}{
		sarifResult("js/xss", "app.js", 10),
		sarifResult("js/sqli", "db.js", 20),
	}

	if _, err := st.CreateSARIFUpload(repo, sarifPayload(t, "sha1", ref, two)); err != nil {
		t.Fatalf("first upload: %v", err)
	}
	total, open := openAlertCount(st.ListCodeScanningAlerts(repo, "", "", "", "", "", ""))
	if total != 2 || open != 2 {
		t.Fatalf("after first upload: total=%d open=%d, want 2/2", total, open)
	}

	// Re-upload the identical analysis: no duplicates.
	if _, err := st.CreateSARIFUpload(repo, sarifPayload(t, "sha2", ref, two)); err != nil {
		t.Fatalf("re-upload: %v", err)
	}
	total, open = openAlertCount(st.ListCodeScanningAlerts(repo, "", "", "", "", "", ""))
	if total != 2 || open != 2 {
		t.Fatalf("after identical re-upload: total=%d open=%d, want 2/2 (dedup failed)", total, open)
	}

	// Upload with the sqli finding removed: it must be marked fixed, xss stays open.
	one := []map[string]interface{}{sarifResult("js/xss", "app.js", 10)}
	if _, err := st.CreateSARIFUpload(repo, sarifPayload(t, "sha3", ref, one)); err != nil {
		t.Fatalf("reduced upload: %v", err)
	}
	alerts := st.ListCodeScanningAlerts(repo, "", "", "", "", "", "")
	total, open = openAlertCount(alerts)
	if total != 2 || open != 1 {
		t.Fatalf("after removing a finding: total=%d open=%d, want 2 total / 1 open (fixed reconciliation)", total, open)
	}
	fixed := st.ListCodeScanningAlerts(repo, "fixed", "", "", "", "", "")
	if len(fixed) != 1 || fixed[0].RuleID != "js/sqli" {
		t.Fatalf("expected js/sqli fixed, got %+v", fixed)
	}

	// The finding recurs: the fixed alert reopens (no new number).
	if _, err := st.CreateSARIFUpload(repo, sarifPayload(t, "sha4", ref, two)); err != nil {
		t.Fatalf("recurrence upload: %v", err)
	}
	total, open = openAlertCount(st.ListCodeScanningAlerts(repo, "", "", "", "", "", ""))
	if total != 2 || open != 2 {
		t.Fatalf("after recurrence: total=%d open=%d, want 2/2 (reopen, not a new alert)", total, open)
	}
}
