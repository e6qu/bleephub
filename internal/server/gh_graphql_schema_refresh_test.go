package bleephub

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
)

// A global advisory published from a repository advisory carries its CVE, the
// time GitHub reviewed it, and links back to the repository advisory and the
// repository's code; isWithdrawn and severities narrow the browse listing.
func TestSecurityAdvisoryReviewFieldsAndListingFilters(t *testing.T) {
	t.Parallel()
	f := newAdvisoryFixture(t, "review-fields", false)
	f.draftAdvisory(t)
	f.publishAdvisory(t)

	data := f.server.gqlData(t, `query($ghsa:String!){
		securityAdvisory(ghsaId:$ghsa){
			cveId githubReviewedAt publishedAt nvdPublishedAt repositoryAdvisoryUrl sourceCodeLocation
		}
		high: securityAdvisories(first:10,severities:[HIGH]){ totalCount }
		low: securityAdvisories(first:10,severities:[LOW]){ totalCount }
		withdrawn: securityAdvisories(first:10,isWithdrawn:true){ totalCount }
		standing: securityAdvisories(first:10,isWithdrawn:false){ totalCount }
	}`, map[string]interface{}{"ghsa": f.ghsaID})

	advisory, _ := data["securityAdvisory"].(map[string]interface{})
	if advisory == nil {
		t.Fatalf("securityAdvisory(%s) is null", f.ghsaID)
	}
	if advisory["cveId"] != "CVE-2026-0001" {
		t.Errorf("cveId = %v, want CVE-2026-0001", advisory["cveId"])
	}
	if advisory["githubReviewedAt"] == nil || advisory["githubReviewedAt"] != advisory["publishedAt"] {
		t.Errorf("githubReviewedAt = %v, want the publication time %v", advisory["githubReviewedAt"], advisory["publishedAt"])
	}
	if advisory["nvdPublishedAt"] != nil {
		t.Errorf("nvdPublishedAt = %v, want null: no NVD record is imported", advisory["nvdPublishedAt"])
	}
	wantAdvisoryURL := "/" + f.repo.FullName + "/security/advisories/" + f.ghsaID
	if url, _ := advisory["repositoryAdvisoryUrl"].(string); !strings.HasSuffix(url, wantAdvisoryURL) {
		t.Errorf("repositoryAdvisoryUrl = %v, want …%s", advisory["repositoryAdvisoryUrl"], wantAdvisoryURL)
	}
	if source, _ := advisory["sourceCodeLocation"].(string); !strings.HasSuffix(source, "/"+f.repo.FullName) {
		t.Errorf("sourceCodeLocation = %v, want …/%s", advisory["sourceCodeLocation"], f.repo.FullName)
	}

	counts := func(field string) float64 {
		connection, _ := data[field].(map[string]interface{})
		count, _ := connection["totalCount"].(float64)
		return count
	}
	if counts("high") != 1 || counts("low") != 0 {
		t.Errorf("severities filter: HIGH=%v LOW=%v, want 1 and 0", counts("high"), counts("low"))
	}
	if counts("withdrawn") != 0 || counts("standing") != 1 {
		t.Errorf("isWithdrawn before withdrawal: true=%v false=%v, want 0 and 1", counts("withdrawn"), counts("standing"))
	}

	resp := f.server.patch(t, "/api/v3/repos/"+f.repo.FullName+"/security-advisories/"+f.ghsaID,
		f.ownerToken, map[string]interface{}{"state": "withdrawn"})
	decodeJSONWithStatus(t, resp, http.StatusOK)
	after := f.server.gqlData(t, `{
		withdrawn: securityAdvisories(first:10,isWithdrawn:true){ nodes{ ghsaId } }
		standing: securityAdvisories(first:10,isWithdrawn:false){ totalCount }
	}`, nil)
	withdrawn, _ := after["withdrawn"].(map[string]interface{})
	nodes, _ := withdrawn["nodes"].([]interface{})
	if len(nodes) != 1 || nodes[0].(map[string]interface{})["ghsaId"] != f.ghsaID {
		t.Errorf("isWithdrawn:true = %v, want only %s", nodes, f.ghsaID)
	}
	if standing, _ := after["standing"].(map[string]interface{}); standing["totalCount"].(float64) != 0 {
		t.Errorf("isWithdrawn:false still lists the withdrawn advisory: %v", standing)
	}
}

// Repository.fullDatabaseId is the repository's primary key, the same value
// as databaseId.
func TestRepositoryFullDatabaseIDIsItsPrimaryKey(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	s.createTestPRRepo(t, "full-database-id")
	repo := s.store.GetRepo("admin", "full-database-id")
	data := s.gqlData(t, `{repository(owner:"admin",name:"full-database-id"){ databaseId fullDatabaseId }}`, nil)
	got, _ := data["repository"].(map[string]interface{})
	if got == nil || got["databaseId"].(float64) != float64(repo.ID) {
		t.Fatalf("repository = %v, want databaseId %d", got, repo.ID)
	}
	if full := got["fullDatabaseId"]; full == nil || fmt.Sprint(full) != fmt.Sprint(repo.ID) {
		t.Errorf("fullDatabaseId = %v, want %d", full, repo.ID)
	}
}

// requestReviews notifies each reviewer it adds with review_requested, as the
// REST endpoint does; rerequestReviews notifies the reviewers it names again,
// even when their request is already open.
func TestGraphQLReviewRequestsDeliverReviewRequested(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	sink := &webhookActionSink{}
	receiver, cleanup := startWebhookReceiver(t, sink.handler())
	defer cleanup()

	const name = "gql-review-requests"
	const repo = "admin/" + name
	s.createTestPRRepo(t, name)
	reviewer, _ := s.newUser(t, "gql-rerequested")
	expectStatus(t, s.post(t, "/api/v3/repos/"+repo+"/pulls", defaultToken, map[string]interface{}{
		"title": "review me", "head": "feat", "base": "main",
	}), http.StatusCreated, "open PR")
	pr := s.store.GetPullRequestByNumber(s.store.GetRepo("admin", name).ID, 1)
	if pr == nil {
		t.Fatal("pull request 1 not created")
	}
	s.subscribeAllEvents(t, repo, receiver)

	requireNoGQLErrors(t, s.gqlAuthzPost(t, defaultToken,
		`mutation($input:RequestReviewsInput!){requestReviews(input:$input){pullRequest{number}}}`,
		map[string]interface{}{"input": map[string]interface{}{
			"pullRequestId": pr.NodeID, "userIds": []string{reviewer.NodeID},
		}}))
	env := s.gqlAuthzPost(t, defaultToken,
		`mutation($input:RerequestReviewsInput!){rerequestReviews(input:$input){
			actor{login} pullRequest{number} requestedReviewersEdge{node{login}}
		}}`,
		map[string]interface{}{"input": map[string]interface{}{
			"pullRequestId": pr.NodeID, "userIds": []string{reviewer.NodeID},
		}})
	requireNoGQLErrors(t, env)

	sink.requireActions(t, "pull_request", []string{"review_requested", "review_requested"})
	if got := nestedString(t, sink.payloadFor(t, "pull_request", "review_requested"), "requested_reviewer", "login"); got != reviewer.Login {
		t.Errorf("review_requested requested_reviewer.login = %q, want %q", got, reviewer.Login)
	}
	updated := s.store.GetPullRequest(pr.ID)
	if updated == nil || len(updated.RequestedReviewerIDs) != 1 || updated.RequestedReviewerIDs[0] != reviewer.ID {
		t.Errorf("after rerequest the requested reviewers are %+v, want only %s", updated, reviewer.Login)
	}

	// Naming nobody is refused rather than silently doing nothing.
	empty := s.gqlAuthzPost(t, defaultToken,
		`mutation($input:RerequestReviewsInput!){rerequestReviews(input:$input){pullRequest{number}}}`,
		map[string]interface{}{"input": map[string]interface{}{"pullRequestId": pr.NodeID}})
	if errs := gqlAuthzErrors(empty); len(errs) == 0 {
		t.Error("rerequestReviews naming no reviewer succeeded")
	}
}
