package graphqlapi

import (
	"testing"
)

// TestRepositoryDeploymentGraphResolvesFromTheDeploymentStore pins that
// Repository.deployments / environments / environment answer from the rows the
// REST deployment and environment routes write, including the status that last
// landed and the protection rules the environment configures.
func TestRepositoryDeploymentGraphResolvesFromTheDeploymentStore(t *testing.T) {
	h := newAccountHarness(t)
	owner := h.store.UsersByLogin["admin"]
	reviewer := h.user("approver")
	repo := h.store.CreateRepo(owner, "deployed", "", false)
	if repo == nil {
		t.Fatal("repository not created")
	}

	if h.store.Deployments.UpsertEnvironment(repo.ID, "production") == nil {
		t.Fatal("environment not created")
	}
	waitTimer := 15
	h.store.Deployments.SetEnvironmentProtection(repo.ID, "production", &waitTimer,
		[]map[string]interface{}{{"type": "User", "id": reviewer.ID}})

	deployment := h.store.Deployments.CreateDeployment(repo.ID, owner.ID, "main", "deadbeefdeadbeefdeadbeefdeadbeefdeadbeef",
		"deploy", "production", "ship it", map[string]interface{}{"unit": "web"}, true, false)
	if deployment == nil {
		t.Fatal("deployment not created")
	}
	if status, _ := h.store.Deployments.AddStatus(deployment.ID, owner.ID, "success", "live",
		"", "https://logs.example.test/1", "https://web.example.test", "production", false); status == nil {
		t.Fatal("deployment status not recorded")
	}

	data := h.query(owner, `{
	  repository(owner:"admin", name:"deployed") {
	    deployments(first:5) {
	      totalCount
	      nodes {
	        commitOid environment originalEnvironment latestEnvironment task
	        description payload state
	        creator { login }
	        repository { nameWithOwner }
	        latestStatus { state description logUrl environmentUrl creator { login } }
	        statuses(first:5) { totalCount nodes { state } }
	      }
	    }
	    environments(first:5) {
	      totalCount
	      nodes {
	        name
	        protectionRules(first:5) {
	          totalCount
	          nodes { type timeout reviewers(first:5) { nodes { ... on User { login } } } }
	        }
	        latestCompletedDeployment { environment state }
	      }
	    }
	    environment(name:"production") { name }
	    environmentAbsent: environment(name:"staging") { name }
	  }
	}`, nil)

	if got := at(t, data, "repository", "deployments", "totalCount"); got != float64(1) {
		t.Fatalf("deployments totalCount = %v, want 1", got)
	}
	nodes := at(t, data, "repository", "deployments", "nodes").([]interface{})
	node, _ := nodes[0].(map[string]interface{})
	for field, want := range map[string]interface{}{
		"commitOid":           "deadbeefdeadbeefdeadbeefdeadbeefdeadbeef",
		"environment":         "production",
		"originalEnvironment": "production",
		"latestEnvironment":   "production",
		"task":                "deploy",
		"description":         "ship it",
		"payload":             `{"unit":"web"}`,
		"state":               "SUCCESS",
	} {
		if got := node[field]; got != want {
			t.Errorf("deployment %s = %#v, want %#v", field, got, want)
		}
	}
	if got := at(t, node, "creator", "login"); got != "admin" {
		t.Errorf("deployment creator = %v, want admin", got)
	}
	if got := at(t, node, "repository", "nameWithOwner"); got != "admin/deployed" {
		t.Errorf("deployment repository = %v", got)
	}
	if got := at(t, node, "latestStatus", "state"); got != "SUCCESS" {
		t.Errorf("latestStatus.state = %v, want SUCCESS", got)
	}
	if got := at(t, node, "latestStatus", "logUrl"); got != "https://logs.example.test/1" {
		t.Errorf("latestStatus.logUrl = %v", got)
	}
	if got := at(t, node, "statuses", "totalCount"); got != float64(1) {
		t.Errorf("statuses totalCount = %v, want 1", got)
	}

	if got := at(t, data, "repository", "environments", "totalCount"); got != float64(1) {
		t.Fatalf("environments totalCount = %v, want 1", got)
	}
	environments := at(t, data, "repository", "environments", "nodes").([]interface{})
	environment, _ := environments[0].(map[string]interface{})
	if environment["name"] != "production" {
		t.Errorf("environment name = %v", environment["name"])
	}
	if got := at(t, environment, "protectionRules", "totalCount"); got != float64(2) {
		t.Errorf("protectionRules totalCount = %v, want the wait timer and the reviewers", got)
	}
	kinds := map[string]float64{}
	var reviewerLogins []string
	for _, raw := range at(t, environment, "protectionRules", "nodes").([]interface{}) {
		rule, _ := raw.(map[string]interface{})
		kind, _ := rule["type"].(string)
		timeout, _ := rule["timeout"].(float64)
		kinds[kind] = timeout
		for _, entry := range at(t, rule, "reviewers", "nodes").([]interface{}) {
			if account, _ := entry.(map[string]interface{}); account["login"] != nil {
				reviewerLogins = append(reviewerLogins, account["login"].(string))
			}
		}
	}
	if kinds["WAIT_TIMER"] != 15 {
		t.Errorf("WAIT_TIMER timeout = %v, want 15", kinds["WAIT_TIMER"])
	}
	if _, ok := kinds["REQUIRED_REVIEWERS"]; !ok {
		t.Errorf("protection rules = %#v, want a REQUIRED_REVIEWERS rule", kinds)
	}
	if len(reviewerLogins) != 1 || reviewerLogins[0] != "approver" {
		t.Errorf("required reviewers = %#v, want [approver]", reviewerLogins)
	}
	if got := at(t, environment, "latestCompletedDeployment", "state"); got != "SUCCESS" {
		t.Errorf("latestCompletedDeployment.state = %v, want SUCCESS", got)
	}

	if got := at(t, data, "repository", "environment", "name"); got != "production" {
		t.Errorf("environment(name:) = %v", got)
	}
	// An environment the repository does not configure is genuinely absent.
	repoData, _ := at(t, data, "repository").(map[string]interface{})
	if got := repoData["environmentAbsent"]; got != nil {
		t.Errorf("environment(name:\"staging\") = %#v, want null", got)
	}
}

// TestPrivateRepositoryDeploymentsRefuseAStranger is the authorization test
// for the deployment graph: where a deployment landed and what is running
// there must not leak out of a private repository.
func TestPrivateRepositoryDeploymentsRefuseAStranger(t *testing.T) {
	h := newAccountHarness(t)
	owner := h.store.UsersByLogin["admin"]
	repo := h.store.CreateRepo(owner, "sealed-deploys", "", true)
	if repo == nil {
		t.Fatal("repository not created")
	}
	h.store.Deployments.UpsertEnvironment(repo.ID, "production")
	if h.store.Deployments.CreateDeployment(repo.ID, owner.ID, "main", "cafebabe", "deploy",
		"production", "", nil, true, false) == nil {
		t.Fatal("deployment not created")
	}
	// A collaborator can read the repository, so they see the deployments; a
	// stranger is refused the repository itself.
	collaborator := h.user("deployreader")
	if !h.store.AddRepoCollaborator("admin", "sealed-deploys", "deployreader", "pull") {
		t.Fatal("collaborator not added")
	}

	document := `{
	  repository(owner:"admin", name:"sealed-deploys") {
	    deployments(first:5) { totalCount }
	    environments(first:5) { totalCount }
	  }
	}`
	readerView := h.query(collaborator, document, nil)
	if got := at(t, readerView, "repository", "deployments", "totalCount"); got != float64(1) {
		t.Errorf("collaborator read deployments totalCount = %v, want 1", got)
	}

	_, errors := h.queryWithErrors(h.user("deploystranger"), document, nil)
	if len(errors) == 0 {
		t.Fatal("a stranger's query for a private repository's deployments succeeded")
	}
}
