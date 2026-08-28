package bleephub

import (
	"net/http"
	"testing"

	"github.com/e6qu/bleephub/internal/store"
)

// These tests drive each governance mutation family (branch protection, rulesets, custom properties, verifiable domains) over GraphQL and read the result back over REST, proving "wired" is "working" rather than assumed from the schema.

// gqlData digs the data object out of a GraphQL envelope, failing the test on any reported error.
func gqlData(t *testing.T, env map[string]interface{}) map[string]interface{} {
	t.Helper()
	if errs := gqlAuthzErrors(env); len(errs) > 0 {
		t.Fatalf("graphql errors: %v", errs)
	}
	data, _ := env["data"].(map[string]interface{})
	if data == nil {
		t.Fatalf("graphql envelope carries no data: %v", env)
	}
	return data
}

func innerObject(t *testing.T, container map[string]interface{}, keys ...string) map[string]interface{} {
	t.Helper()
	current := container
	for _, key := range keys {
		next, _ := current[key].(map[string]interface{})
		if next == nil {
			t.Fatalf("missing %q in %v", key, current)
		}
		current = next
	}
	return current
}

func TestGraphQLBranchProtectionRuleWritesTheRESTProtectionStore(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	f := newGQLAuthzFixture(t, s.Server, "bp-roundtrip", true)

	env := s.gqlAuthzPost(t, f.ownerToken,
		`mutation($input:CreateBranchProtectionRuleInput!){createBranchProtectionRule(input:$input){branchProtectionRule{
			id pattern requiresApprovingReviews requiredApprovingReviewCount
			requiresStatusChecks requiresStrictStatusChecks requiredStatusCheckContexts
			requiresLinearHistory requiresDeployments requiredDeploymentEnvironments
			creator{login} repository{name}}}}`,
		map[string]interface{}{"input": map[string]interface{}{
			"repositoryId":                   f.repo.NodeID,
			"pattern":                        "main",
			"requiresApprovingReviews":       true,
			"requiredApprovingReviewCount":   2,
			"requiresStatusChecks":           true,
			"requiresStrictStatusChecks":     true,
			"requiredStatusCheckContexts":    []interface{}{"ci"},
			"requiresLinearHistory":          true,
			"requiredDeploymentEnvironments": []interface{}{"production"},
		}},
	)
	rule := innerObject(t, gqlData(t, env), "createBranchProtectionRule", "branchProtectionRule")
	if rule["pattern"] != "main" || rule["requiresApprovingReviews"] != true ||
		rule["requiredApprovingReviewCount"] != float64(2) || rule["requiresStrictStatusChecks"] != true ||
		rule["requiresDeployments"] != true {
		t.Fatalf("createBranchProtectionRule payload = %v", rule)
	}
	if innerObject(t, rule, "creator")["login"] != f.owner.Login {
		t.Errorf("creator = %v, want the mutating owner", rule["creator"])
	}

	// The REST protection GET must serve the very rule GraphQL created.
	resp := s.get(t, "/api/v3/repos/"+f.repo.FullName+"/branches/main/protection", f.ownerToken)
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		t.Fatalf("REST protection GET after GraphQL create = %d", resp.StatusCode)
	}
	protection := decodeJSON(t, resp)
	reviews := innerObject(t, protection, "required_pull_request_reviews")
	if reviews["required_approving_review_count"] != float64(2) {
		t.Errorf("REST required_approving_review_count = %v, want 2", reviews["required_approving_review_count"])
	}
	checks := innerObject(t, protection, "required_status_checks")
	if contexts, _ := checks["contexts"].([]interface{}); len(contexts) != 1 || contexts[0] != "ci" {
		t.Errorf("REST required_status_checks.contexts = %v, want [ci]", checks["contexts"])
	}

	// Renaming to a wildcard pattern must leave the exact-name store (REST answers 404) and keep protecting through the pattern rules.
	ruleID, _ := rule["id"].(string)
	env = s.gqlAuthzPost(t, f.ownerToken,
		`mutation($input:UpdateBranchProtectionRuleInput!){updateBranchProtectionRule(input:$input){branchProtectionRule{pattern allowsDeletions}}}`,
		map[string]interface{}{"input": map[string]interface{}{
			"branchProtectionRuleId": ruleID,
			"pattern":                "release/*",
			"allowsDeletions":        true,
		}},
	)
	renamed := innerObject(t, gqlData(t, env), "updateBranchProtectionRule", "branchProtectionRule")
	if renamed["pattern"] != "release/*" || renamed["allowsDeletions"] != true {
		t.Fatalf("updateBranchProtectionRule payload = %v", renamed)
	}
	resp = s.get(t, "/api/v3/repos/"+f.repo.FullName+"/branches/main/protection", f.ownerToken)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("REST protection GET after pattern rename = %d, want 404", resp.StatusCode)
	}
	patterns := s.store.ListBranchProtectionPatterns(f.repo.ID)
	if len(patterns) != 1 || patterns[0].Pattern != "release/*" {
		t.Fatalf("pattern rules after rename = %+v", patterns)
	}

	env = s.gqlAuthzPost(t, f.ownerToken,
		`query($owner:String!,$name:String!){repository(owner:$owner,name:$name){branchProtectionRules(first:10){totalCount nodes{pattern}}}}`,
		map[string]interface{}{"owner": f.owner.Login, "name": f.repo.Name},
	)
	connection := innerObject(t, gqlData(t, env), "repository", "branchProtectionRules")
	if connection["totalCount"] != float64(1) {
		t.Fatalf("branchProtectionRules totalCount = %v, want 1", connection["totalCount"])
	}

	env = s.gqlAuthzPost(t, f.ownerToken,
		`mutation($input:DeleteBranchProtectionRuleInput!){deleteBranchProtectionRule(input:$input){clientMutationId}}`,
		map[string]interface{}{"input": map[string]interface{}{
			"branchProtectionRuleId": store.BranchProtectionRuleNodeID(f.repo.ID, "release/*"),
		}},
	)
	gqlData(t, env)
	if remaining := s.store.ListBranchProtectionPatterns(f.repo.ID); len(remaining) != 0 {
		t.Errorf("pattern rules after delete = %+v, want none", remaining)
	}
}

func TestGraphQLRulesetMutationsShareTheRESTRulesetStore(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	f := newGQLAuthzFixture(t, s.Server, "ruleset-roundtrip", true)

	env := s.gqlAuthzPost(t, f.ownerToken,
		`mutation($input:CreateRepositoryRulesetInput!){createRepositoryRuleset(input:$input){ruleset{id databaseId name target enforcement rules(first:5){totalCount nodes{type}} source{__typename ... on Repository{name}}}}}`,
		map[string]interface{}{"input": map[string]interface{}{
			"sourceId":    f.repo.NodeID,
			"name":        "graphql-ruleset",
			"enforcement": "ACTIVE",
			"target":      "BRANCH",
			"conditions": map[string]interface{}{
				"refName": map[string]interface{}{"include": []interface{}{"~DEFAULT_BRANCH"}, "exclude": []interface{}{}},
			},
			"rules": []interface{}{
				map[string]interface{}{"type": "PULL_REQUEST", "parameters": map[string]interface{}{
					"pullRequest": map[string]interface{}{
						"dismissStaleReviewsOnPush":      true,
						"requireCodeOwnerReview":         false,
						"requireLastPushApproval":        false,
						"requiredApprovingReviewCount":   1,
						"requiredReviewThreadResolution": false,
					},
				}},
				map[string]interface{}{"type": "REQUIRED_LINEAR_HISTORY"},
			},
		}},
	)
	ruleset := innerObject(t, gqlData(t, env), "createRepositoryRuleset", "ruleset")
	if ruleset["name"] != "graphql-ruleset" || ruleset["enforcement"] != "ACTIVE" {
		t.Fatalf("createRepositoryRuleset payload = %v", ruleset)
	}
	if rules := innerObject(t, ruleset, "rules"); rules["totalCount"] != float64(2) {
		t.Fatalf("created ruleset rules = %v, want 2", rules)
	}

	// The stored rule parameters carry the REST snake_case shape.
	databaseID := int(ruleset["databaseId"].(float64))
	stored := s.store.GetRuleset(databaseID)
	if stored == nil || len(stored.Rules) != 2 || stored.Rules[0].Type != "pull_request" {
		t.Fatalf("stored ruleset = %+v", stored)
	}
	if stored.Rules[0].Parameters["required_approving_review_count"] != 1 {
		t.Errorf("stored pull_request parameters = %v, want snake_case count 1", stored.Rules[0].Parameters)
	}

	// The REST ruleset list serves the GraphQL-authored ruleset.
	rows := decodeJSONArray(t, s.get(t, "/api/v3/repos/"+f.repo.FullName+"/rulesets", f.ownerToken))
	if len(rows) != 1 || rows[0]["name"] != "graphql-ruleset" {
		t.Fatalf("REST rulesets after GraphQL create = %v", rows)
	}

	rulesetNodeID, _ := ruleset["id"].(string)
	env = s.gqlAuthzPost(t, f.ownerToken,
		`mutation($input:UpdateRepositoryRulesetInput!){updateRepositoryRuleset(input:$input){ruleset{enforcement name}}}`,
		map[string]interface{}{"input": map[string]interface{}{
			"repositoryRulesetId": rulesetNodeID,
			"enforcement":         "DISABLED",
		}},
	)
	updated := innerObject(t, gqlData(t, env), "updateRepositoryRuleset", "ruleset")
	if updated["enforcement"] != "DISABLED" || updated["name"] != "graphql-ruleset" {
		t.Fatalf("updateRepositoryRuleset payload = %v", updated)
	}
	if stored := s.store.GetRuleset(databaseID); stored == nil || stored.Enforcement != "disabled" {
		t.Fatalf("stored enforcement after update = %+v", stored)
	}

	env = s.gqlAuthzPost(t, f.ownerToken,
		`mutation($input:DeleteRepositoryRulesetInput!){deleteRepositoryRuleset(input:$input){clientMutationId}}`,
		map[string]interface{}{"input": map[string]interface{}{"repositoryRulesetId": rulesetNodeID}},
	)
	gqlData(t, env)
	if s.store.GetRuleset(databaseID) != nil {
		t.Error("the ruleset survived deleteRepositoryRuleset")
	}
}

func TestGraphQLCustomPropertyMutationsShareTheRESTSchema(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	f := newGQLAuthzFixture(t, s.Server, "props-roundtrip", true)
	seedAuthzPropsOrg(t, s, f)
	orgRepo := s.store.CreateOrgRepo(f.propsOrg, f.owner, "props-repo", "", true)
	if orgRepo == nil {
		t.Fatal("org repo could not be created")
	}

	env := s.gqlAuthzPost(t, f.ownerToken,
		`mutation($input:CreateRepositoryCustomPropertyInput!){createRepositoryCustomProperty(input:$input){repositoryCustomProperty{id propertyName valueType allowedValues required source{__typename}}}}`,
		map[string]interface{}{"input": map[string]interface{}{
			"sourceId":      f.propsOrg.NodeID,
			"propertyName":  "env",
			"valueType":     "SINGLE_SELECT",
			"allowedValues": []interface{}{"prod", "staging"},
		}},
	)
	property := innerObject(t, gqlData(t, env), "createRepositoryCustomProperty", "repositoryCustomProperty")
	if property["propertyName"] != "env" || property["valueType"] != "SINGLE_SELECT" {
		t.Fatalf("createRepositoryCustomProperty payload = %v", property)
	}
	if innerObject(t, property, "source")["__typename"] != "Organization" {
		t.Errorf("property source = %v, want Organization", property["source"])
	}

	// The REST schema surface serves the definition GraphQL created.
	rows := decodeJSONArray(t, s.get(t, "/api/v3/orgs/"+f.propsOrg.Login+"/properties/schema", f.ownerToken))
	if len(rows) != 1 || rows[0]["property_name"] != "env" || rows[0]["value_type"] != "single_select" {
		t.Fatalf("REST schema after GraphQL create = %v", rows)
	}

	propertyID, _ := property["id"].(string)
	env = s.gqlAuthzPost(t, f.ownerToken,
		`mutation($input:UpdateRepositoryCustomPropertyInput!){updateRepositoryCustomProperty(input:$input){repositoryCustomProperty{description}}}`,
		map[string]interface{}{"input": map[string]interface{}{
			"repositoryCustomPropertyId": propertyID,
			"description":                "deploy environment",
		}},
	)
	if got := innerObject(t, gqlData(t, env), "updateRepositoryCustomProperty", "repositoryCustomProperty")["description"]; got != "deploy environment" {
		t.Fatalf("updated description = %v", got)
	}

	// Values set over GraphQL are the values the REST values route reads.
	env = s.gqlAuthzPost(t, f.ownerToken,
		`mutation($input:SetRepositoryCustomPropertyValuesInput!){setRepositoryCustomPropertyValues(input:$input){repository{name}}}`,
		map[string]interface{}{"input": map[string]interface{}{
			"repositoryId": orgRepo.NodeID,
			"properties": []interface{}{
				map[string]interface{}{"propertyName": "env", "value": "prod"},
			},
		}},
	)
	gqlData(t, env)
	values := decodeJSONArray(t, s.get(t, "/api/v3/repos/"+orgRepo.FullName+"/properties/values", f.ownerToken))
	if len(values) != 1 || values[0]["property_name"] != "env" || values[0]["value"] != "prod" {
		t.Fatalf("REST values after GraphQL set = %v", values)
	}

	// A value the schema refuses over REST is refused over GraphQL too.
	env = s.gqlAuthzPost(t, f.ownerToken,
		`mutation($input:SetRepositoryCustomPropertyValuesInput!){setRepositoryCustomPropertyValues(input:$input){repository{name}}}`,
		map[string]interface{}{"input": map[string]interface{}{
			"repositoryId": orgRepo.NodeID,
			"properties": []interface{}{
				map[string]interface{}{"propertyName": "env", "value": "not-an-option"},
			},
		}},
	)
	if len(gqlAuthzErrors(env)) == 0 {
		t.Error("a value outside allowed_values was accepted")
	}

	// Deleting the definition clears the values it governed (the same
	// transactional clear the REST DELETE performs).
	env = s.gqlAuthzPost(t, f.ownerToken,
		`mutation($input:DeleteRepositoryCustomPropertyInput!){deleteRepositoryCustomProperty(input:$input){repositoryCustomProperty{propertyName}}}`,
		map[string]interface{}{"input": map[string]interface{}{"id": propertyID}},
	)
	gqlData(t, env)
	if s.store.GetCustomProperty(f.propsOrg.Login, "env") != nil {
		t.Error("the definition survived deleteRepositoryCustomProperty")
	}
	if values := decodeJSONArray(t, s.get(t, "/api/v3/repos/"+orgRepo.FullName+"/properties/values", f.ownerToken)); len(values) != 0 {
		t.Errorf("values after definition delete = %v, want none", values)
	}
}

func TestGraphQLPromoteRepositoryCustomPropertyLandsInTheEnterpriseSchema(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	f := newGQLAuthzFixture(t, s.Server, "props-promote", true)
	seedAuthzPropsOrg(t, s, f)
	seedAuthzCustomProperty(t, s, f)
	s.store.Mu.Lock()
	s.store.Users[f.owner.ID].SiteAdmin = true
	s.store.Mu.Unlock()

	env := s.gqlAuthzPost(t, f.ownerToken,
		`mutation($input:PromoteRepositoryCustomPropertyInput!){promoteRepositoryCustomProperty(input:$input){repositoryCustomProperty{propertyName}}}`,
		map[string]interface{}{"input": map[string]interface{}{
			"repositoryCustomPropertyId": "RCP_" + f.propsOrg.Login + "/authz-prop",
		}},
	)
	gqlData(t, env)
	if s.store.GetEnterpriseCustomProperty("authz-prop") == nil {
		t.Fatal("the promoted definition did not land in the enterprise schema")
	}
}

func TestGraphQLVerifiableDomainLifecycle(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	f := newGQLAuthzFixture(t, s.Server, "domains", true)
	seedAuthzPropsOrg(t, s, f)

	env := s.gqlAuthzPost(t, f.ownerToken,
		`mutation($input:AddVerifiableDomainInput!){addVerifiableDomain(input:$input){domain{id domain isVerified isApproved verificationToken tokenExpirationTime owner{__typename ... on Organization{login}}}}}`,
		map[string]interface{}{"input": map[string]interface{}{
			"ownerId": f.propsOrg.NodeID, "domain": "Mail.Example.COM",
		}},
	)
	domain := innerObject(t, gqlData(t, env), "addVerifiableDomain", "domain")
	if domain["domain"] != "mail.example.com" || domain["isVerified"] != false {
		t.Fatalf("addVerifiableDomain payload = %v", domain)
	}
	token, _ := domain["verificationToken"].(string)
	if token == "" || domain["tokenExpirationTime"] == nil {
		t.Fatalf("the added domain carries no verification token lifecycle: %v", domain)
	}
	if owner := innerObject(t, domain, "owner"); owner["login"] != f.propsOrg.Login {
		t.Errorf("domain owner = %v, want the organization", owner)
	}

	env = s.gqlAuthzPost(t, f.ownerToken,
		`mutation($input:AddVerifiableDomainInput!){addVerifiableDomain(input:$input){domain{id}}}`,
		map[string]interface{}{"input": map[string]interface{}{
			"ownerId": f.propsOrg.NodeID, "domain": "mail.example.com",
		}},
	)
	if len(gqlAuthzErrors(env)) == 0 {
		t.Error("a duplicate domain was accepted")
	}

	domainID, _ := domain["id"].(string)
	env = s.gqlAuthzPost(t, f.ownerToken,
		`mutation($input:RegenerateVerifiableDomainTokenInput!){regenerateVerifiableDomainToken(input:$input){verificationToken}}`,
		map[string]interface{}{"input": map[string]interface{}{"id": domainID}},
	)
	regenerated, _ := gqlData(t, env)["regenerateVerifiableDomainToken"].(map[string]interface{})
	if fresh, _ := regenerated["verificationToken"].(string); fresh == "" || fresh == token {
		t.Fatalf("regenerateVerifiableDomainToken answered %q, want a fresh token", fresh)
	}

	env = s.gqlAuthzPost(t, f.ownerToken,
		`mutation($input:VerifyVerifiableDomainInput!){verifyVerifiableDomain(input:$input){domain{isVerified hasFoundVerificationToken}}}`,
		map[string]interface{}{"input": map[string]interface{}{"id": domainID}},
	)
	verified := innerObject(t, gqlData(t, env), "verifyVerifiableDomain", "domain")
	if verified["isVerified"] != true || verified["hasFoundVerificationToken"] != true {
		t.Fatalf("verifyVerifiableDomain payload = %v", verified)
	}

	env = s.gqlAuthzPost(t, f.ownerToken,
		`mutation($input:DeleteVerifiableDomainInput!){deleteVerifiableDomain(input:$input){owner{__typename}}}`,
		map[string]interface{}{"input": map[string]interface{}{"id": domainID}},
	)
	if owner := innerObject(t, gqlData(t, env), "deleteVerifiableDomain", "owner"); owner["__typename"] != "Organization" {
		t.Fatalf("deleteVerifiableDomain owner = %v", owner)
	}
	if rows := s.store.ListVerifiableDomains(store.VerifiableDomainOwnerOrganization, f.propsOrg.ID); len(rows) != 0 {
		t.Errorf("domains after delete = %+v, want none", rows)
	}
}

// TestGraphQLEnterpriseVerifiableDomainFeedsTheVerifiedDomainList proves the enterprise half writes the same ledger the notification-delivery restriction and the /ui-data verified-domains surface read.
func TestGraphQLEnterpriseVerifiableDomainFeedsTheVerifiedDomainList(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	f := newGQLAuthzFixture(t, s.Server, "ent-domains", true)
	enterprise := s.store.CreateEnterprise("authz-domains-ent", "Authz Domains", "")
	if enterprise == nil {
		t.Fatal("fixture enterprise could not be created")
	}
	s.store.SetEnterpriseMembership(enterprise.ID, f.owner.ID, store.EnterpriseRoleOwner)

	env := s.gqlAuthzPost(t, f.ownerToken,
		`mutation($input:AddVerifiableDomainInput!){addVerifiableDomain(input:$input){domain{id owner{__typename ... on Enterprise{slug}}}}}`,
		map[string]interface{}{"input": map[string]interface{}{
			"ownerId": enterprise.NodeID, "domain": "corp.example.com",
		}},
	)
	domain := innerObject(t, gqlData(t, env), "addVerifiableDomain", "domain")
	if owner := innerObject(t, domain, "owner"); owner["slug"] != enterprise.Slug {
		t.Fatalf("domain owner = %v, want the enterprise", owner)
	}
	// An unverified, unapproved domain is not yet a verified domain.
	if e := s.store.GetEnterprise(enterprise.Slug); len(e.VerifiedDomains) != 0 {
		t.Fatalf("verified domains before verification = %v, want none", e.VerifiedDomains)
	}

	domainID, _ := domain["id"].(string)
	env = s.gqlAuthzPost(t, f.ownerToken,
		`mutation($input:VerifyVerifiableDomainInput!){verifyVerifiableDomain(input:$input){domain{isVerified}}}`,
		map[string]interface{}{"input": map[string]interface{}{"id": domainID}},
	)
	gqlData(t, env)
	if e := s.store.GetEnterprise(enterprise.Slug); len(e.VerifiedDomains) != 1 || e.VerifiedDomains[0] != "corp.example.com" {
		t.Fatalf("verified domains after verification = %v, want [corp.example.com]", e.VerifiedDomains)
	}

	env = s.gqlAuthzPost(t, f.ownerToken,
		`mutation($input:DeleteVerifiableDomainInput!){deleteVerifiableDomain(input:$input){owner{__typename}}}`,
		map[string]interface{}{"input": map[string]interface{}{"id": domainID}},
	)
	gqlData(t, env)
	if e := s.store.GetEnterprise(enterprise.Slug); len(e.VerifiedDomains) != 0 {
		t.Fatalf("verified domains after delete = %v, want none", e.VerifiedDomains)
	}
}
