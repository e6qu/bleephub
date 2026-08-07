package bleephub

import (
	"net/http"
	"strconv"
	"testing"
	"time"
)

func TestDelegatedSecurityReviewJourneys(t *testing.T) {
	t.Parallel()
	srv := newIsolatedServer(t)
	srv.createOrg(t, "security-reviews")
	repo := decodeJSON(t, srv.post(t, "/api/v3/orgs/security-reviews/repos", defaultToken,
		map[string]interface{}{"name": "reviewed", "auto_init": true}))
	repoID := int(repo["id"].(float64))
	repoKey := "security-reviews/reviewed"
	alert := srv.store.CreateDependabotAlertIfNew(
		repoKey, "example", "npm", "package.json", "GHSA-review", "CVE-2042-0001",
		"high", "Review this dependency", "description", "< 2.0.0", "2.0.0")
	if alert == nil {
		t.Fatal("failed to seed Dependabot alert")
	}
	base := "/api/v3/repos/" + repoKey

	created := decodeJSON(t, srv.post(t, base+"/dismissal-requests/dependabot/"+
		strconv.Itoa(alert.Number), defaultToken, map[string]interface{}{
		"dismissed_reason": "tolerable_risk", "dismissed_comment": "accepted temporarily",
	}))
	if created["status"] != "pending" || created["resource_identifier"] != strconv.Itoa(alert.Number) {
		t.Fatalf("created Dependabot dismissal request = %#v", created)
	}
	expectStatus(t, srv.post(t, base+"/dismissal-requests/dependabot/"+strconv.Itoa(alert.Number),
		defaultToken, map[string]interface{}{"dismissed_reason": "not_used"}),
		http.StatusUnprocessableEntity, "duplicate pending dismissal request")
	review := decodeJSON(t, srv.patch(t, base+"/dismissal-requests/dependabot/"+
		strconv.Itoa(alert.Number), defaultToken, map[string]interface{}{
		"status": "approve", "message": "approved by security",
	}))
	if review["dismissal_review_id"] == nil {
		t.Fatalf("Dependabot dismissal review = %#v", review)
	}
	got := decodeJSON(t, srv.get(t, base+"/dismissal-requests/dependabot/"+strconv.Itoa(alert.Number), defaultToken))
	if got["status"] != "approved" || len(got["responses"].([]interface{})) != 1 {
		t.Fatalf("reviewed Dependabot dismissal request = %#v", got)
	}
	orgList := decodeJSONArray(t, srv.get(t,
		"/api/v3/orgs/security-reviews/dismissal-requests/dependabot", defaultToken))
	if len(orgList) != 1 {
		t.Fatalf("organization Dependabot review queue = %#v", orgList)
	}

	now := fixedTestTime
	adminID := srv.store.LookupUserByLogin("admin").ID
	request := func(id, number int, kind string) *SecurityReviewRequest {
		return &SecurityReviewRequest{
			ID: id, Number: number, RepoKey: repoKey, OrgLogin: "security-reviews",
			Kind: kind, RequesterID: adminID,
			ResourceID: strconv.Itoa(number), Status: "pending",
			Data: []map[string]interface{}{}, CreatedAt: now, ExpiresAt: now.Add(7 * 24 * time.Hour),
		}
	}
	srv.store.mu.Lock()
	for index, kind := range []string{
		reviewKindPushBypass, reviewKindSecretBypass, reviewKindCodeDismissal, reviewKindSecretDismissal,
	} {
		scope := securityReviewScope(repoKey, kind)
		srv.store.SecurityReviewRequests[scope] = map[int]*SecurityReviewRequest{
			index + 11: request(index+100, index+11, kind),
		}
	}
	srv.store.mu.Unlock()

	if got := decodeJSON(t, srv.get(t, base+"/bypass-requests/push-rules/11", defaultToken)); got["number"] != float64(11) {
		t.Fatalf("push bypass request = %#v", got)
	}
	secretBypassReview := decodeJSON(t, srv.patch(t, base+"/bypass-requests/secret-scanning/12",
		defaultToken, map[string]interface{}{"status": "reject", "message": "secret is live"}))
	responseID := int(secretBypassReview["bypass_review_id"].(float64))
	expectStatus(t, srv.delete(t, base+"/bypass-responses/secret-scanning/"+strconv.Itoa(responseID), defaultToken),
		http.StatusNoContent, "dismiss secret scanning bypass response")
	expectStatus(t, srv.patch(t, base+"/dismissal-requests/code-scanning/13", defaultToken,
		map[string]interface{}{"status": "deny", "message": "code path is reachable"}),
		http.StatusOK, "review code scanning dismissal")
	expectStatus(t, srv.patch(t, base+"/dismissal-requests/secret-scanning/14", defaultToken,
		map[string]interface{}{"status": "approve", "message": "credential was revoked"}),
		http.StatusOK, "review secret scanning dismissal")

	for _, path := range []string{
		"/api/v3/orgs/security-reviews/bypass-requests/push-rules",
		"/api/v3/orgs/security-reviews/bypass-requests/secret-scanning",
		"/api/v3/orgs/security-reviews/dismissal-requests/code-scanning",
		"/api/v3/orgs/security-reviews/dismissal-requests/secret-scanning",
	} {
		if queue := decodeJSONArray(t, srv.get(t, path, defaultToken)); len(queue) != 1 {
			t.Fatalf("%s = %#v", path, queue)
		}
	}
	if stored := srv.store.GetRepoByID(repoID); stored == nil {
		t.Fatal("reviewed repository disappeared")
	}
	expectStatus(t, srv.delete(t, base+"/dismissal-requests/dependabot/"+strconv.Itoa(alert.Number), defaultToken),
		http.StatusNoContent, "cancel Dependabot dismissal request")
}
