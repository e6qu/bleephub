package bleephub

import (
	"testing"

	"github.com/e6qu/bleephub/internal/store"
)

// TestVSSubscriptionsUnmatchedOnlyFiltersByUser pins that is_unmatched_only
// returns only subscriptions with no linked GitHub user — the filter previously
// keyed on the manual_match flag, so an auto-matched subscription (a username,
// manual_match=false) was wrongly returned as unmatched.
func TestVSSubscriptionsUnmatchedOnlyFiltersByUser(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	s.store.Mu.Lock()
	s.store.EnterpriseSettings.VisualStudioSubscriptions["a"] = &store.VisualStudioSubscription{
		SubscriptionID: "A", Email: "a@x.test", Username: "alice", ManualMatch: false, // matched (auto)
	}
	s.store.EnterpriseSettings.VisualStudioSubscriptions["b"] = &store.VisualStudioSubscription{
		SubscriptionID: "B", Email: "b@x.test", Username: "", ManualMatch: false, // genuinely unmatched
	}
	s.store.Mu.Unlock()

	rec := enterpriseActionsRequest(t, s.Server, "GET", "/api/v3/enterprises/bleephub/visual-studio-subscriptions?is_unmatched_only=true", nil)
	if rec.Code != 200 {
		t.Fatalf("list VS subscriptions = %d, want 200", rec.Code)
	}
	body := decodeRecorderObject(t, rec)
	rows, _ := body["visual_studio_subscription_assignments"].([]interface{})
	if len(rows) != 1 {
		t.Fatalf("is_unmatched_only returned %d rows, want 1 (only the user-less subscription)", len(rows))
	}
	if got := rows[0].(map[string]interface{})["subscriptionId"]; got != "B" {
		t.Fatalf("unmatched row = %v, want B", got)
	}
}

// TestAuditLogPhraseQualifiers pins that the audit-log phrase honors the
// actor:, action: and org: qualifiers (action:repo matching every repo.* action)
// rather than treating "actor:alice" as a literal substring that never matches.
func TestAuditLogPhraseQualifiers(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	s.createOrg(t, "audit-org")
	s.store.RecordAuditEntry("repo.create", "alice", "audit-org", nil)
	s.store.RecordAuditEntry("repo.destroy", "bob", "audit-org", nil)
	s.store.RecordAuditEntry("team.create", "alice", "audit-org", nil)

	count := func(query string) int {
		return len(decodeJSONArray(t, s.get(t, "/api/v3/orgs/audit-org/audit-log?"+query, defaultToken)))
	}

	if n := count("phrase=actor:alice"); n != 2 {
		t.Fatalf("phrase=actor:alice matched %d entries, want 2", n)
	}
	if n := count("phrase=action:repo"); n != 2 {
		t.Fatalf("phrase=action:repo matched %d entries, want 2 (all repo.*)", n)
	}
	if n := count("phrase=action:repo.create"); n != 1 {
		t.Fatalf("phrase=action:repo.create matched %d entries, want 1", n)
	}
	// Qualifiers combine (AND): alice's team.create is the only match.
	if n := count("phrase=actor:alice+action:team"); n != 1 {
		t.Fatalf("phrase=actor:alice action:team matched %d entries, want 1", n)
	}
	// A plain free-text term still works.
	if n := count("phrase=bob"); n != 1 {
		t.Fatalf("free-text phrase=bob matched %d entries, want 1", n)
	}
}
