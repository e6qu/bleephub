package bleephub

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

	"github.com/e6qu/bleephub/internal/store"
	"github.com/stretchr/testify/require"
)

// TestBPReviewsPatchMergesFields pins that PATCH required_pull_request_reviews
// merges into the existing rule: a field omitted from the body is untouched, and
// a zero approving-review count is a valid setting rather than a signal to drop
// the whole rule. The handler previously replaced the object wholesale (zeroing
// unspecified fields) and deleted the rule when every field was falsy.
func TestBPReviewsPatchMergesFields(t *testing.T) {
	s := bpTestServer(t)
	admin := s.store.UsersByLogin["admin"]
	repo := s.store.CreateRepo(admin, "bp-merge", "", false)
	base := "/api/v3/repos/" + repo.FullName + "/branches/main/protection"
	doBPReq(s, adminPAT, "PUT", base, `{"enforce_admins":false}`)

	doBPReq(s, adminPAT, "PATCH", base+"/required_pull_request_reviews", `{"required_approving_review_count": 3}`)

	// A PATCH of only dismiss_stale_reviews must leave the count at 3.
	w := doBPReq(s, adminPAT, "PATCH", base+"/required_pull_request_reviews", `{"dismiss_stale_reviews": true}`)
	require.Equal(t, http.StatusOK, w.Code)
	var rev store.BPPullRequestReviews
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &rev))
	require.Equal(t, 3, rev.RequiredApprovingReviewCount, "unspecified count must be preserved")
	require.True(t, rev.DismissStaleReviews)

	// A zero count is a valid setting, not a delete: the rule stays present.
	w = doBPReq(s, adminPAT, "PATCH", base+"/required_pull_request_reviews", `{"required_approving_review_count": 0}`)
	require.Equal(t, http.StatusOK, w.Code)
	w = doBPReq(s, adminPAT, "GET", base+"/required_pull_request_reviews", "")
	require.Equal(t, http.StatusOK, w.Code, "a zero count must not drop review protection")
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &rev))
	require.Equal(t, 0, rev.RequiredApprovingReviewCount)
	require.True(t, rev.DismissStaleReviews, "dismiss_stale must survive the count-only PATCH")
}

// TestHookUpdateAddRemoveEvents pins that a webhook update honors add_events and
// remove_events, not just a wholesale events replacement. They were declared
// nowhere in the request struct and silently dropped.
func TestHookUpdateAddRemoveEvents(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	target := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		w.WriteHeader(http.StatusOK)
	}))
	defer target.Close()

	s.post(t, "/api/v3/user/repos", defaultToken, map[string]interface{}{"name": "hook-events"}).Body.Close()
	repo := "admin/hook-events"

	resp := s.post(t, "/api/v3/repos/"+repo+"/hooks", defaultToken, map[string]interface{}{
		"config": map[string]interface{}{"url": target.URL + "/h", "content_type": "json"},
		"events": []string{"push"},
	})
	created := decodeHook(t, resp)
	hookID := created.ID

	// add_events extends the subscription.
	resp = s.patch(t, "/api/v3/repos/"+repo+"/hooks/"+strconv.Itoa(hookID), defaultToken, map[string]interface{}{
		"add_events": []string{"pull_request"},
	})
	updated := decodeHook(t, resp)
	require.ElementsMatch(t, []string{"push", "pull_request"}, updated.Events, "add_events was ignored")

	// remove_events drops from it.
	resp = s.patch(t, "/api/v3/repos/"+repo+"/hooks/"+strconv.Itoa(hookID), defaultToken, map[string]interface{}{
		"remove_events": []string{"push"},
	})
	updated = decodeHook(t, resp)
	require.Equal(t, []string{"pull_request"}, updated.Events, "remove_events was ignored")
}

// TestProjectV2ItemUpdateIsAtomic pins that a multi-field item update is applied
// atomically: when a later field is invalid, none of the batch's writes persist.
// Earlier fields were previously committed before the invalid one returned 422.
func TestProjectV2ItemUpdateIsAtomic(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	org, p := s.seedProjectV2Org(t, "pv2-atomic-org", "Atomic")
	admin := s.store.UsersByLogin["admin"]
	repo := s.store.CreateOrgRepo(org, admin, "pv2-atomic-repo", "", false)
	issue := s.store.CreateIssue(repo.ID, admin.ID, "atomic", "", nil, nil, 0)
	base := "/api/v3/orgs/" + org.Login + "/projectsV2/" + strconv.Itoa(p.Number)

	resp := s.post(t, base+"/items", defaultToken, map[string]interface{}{"type": "Issue", "id": issue.ID})
	itemID := int(decodeJSON(t, resp)["id"].(float64))
	textField := s.store.ProjectsV2.CreateField(p.ID, "Notes", store.ProjectV2FieldText, nil, nil)
	numField := s.store.ProjectsV2.CreateField(p.ID, "Points", store.ProjectV2FieldNumber, nil, nil)

	// Establish a known text value.
	resp = s.patch(t, base+"/items/"+strconv.Itoa(itemID), defaultToken, map[string]interface{}{
		"fields": []map[string]interface{}{{"id": textField.ID, "value": "before"}},
	})
	resp.Body.Close()

	// A batch whose second field is type-invalid must be rejected whole.
	resp = s.patch(t, base+"/items/"+strconv.Itoa(itemID), defaultToken, map[string]interface{}{
		"fields": []map[string]interface{}{
			{"id": textField.ID, "value": "after"},
			{"id": numField.ID, "value": "not a number"},
		},
	})
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("invalid batch = %d, want 422", resp.StatusCode)
	}

	// The valid text write from the rejected batch must NOT have persisted.
	got := decodeJSON(t, s.get(t, base+"/items/"+strconv.Itoa(itemID)+"?fields="+strconv.Itoa(textField.ID), defaultToken))
	fields, _ := got["fields"].([]interface{})
	if len(fields) != 1 {
		t.Fatalf("want one selected field, got %v", got["fields"])
	}
	fv, _ := fields[0].(map[string]interface{})
	if fv["value"] != "before" {
		t.Fatalf("text value after rejected batch = %v, want unchanged \"before\" (non-atomic write)", fv["value"])
	}
}
