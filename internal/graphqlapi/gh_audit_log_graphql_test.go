package graphqlapi

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/graphql-go/graphql"

	"github.com/e6qu/bleephub/internal/store"
)

// auditLogTestSchema builds a standalone schema whose single root field,
// auditLog, returns the OrganizationAuditEntry connection type for the given
// org through organizationAuditLogConnection. The parent wires the same method
// onto Organization.auditLog; the throwaway schema lets this package assert the
// whole type graph — union, interfaces, concrete members — builds and resolves
// without depending on that wiring being present yet.
func auditLogTestSchema(t *testing.T, res *Resolver, org *store.Org) graphql.Schema {
	t.Helper()
	// Organization.auditLog is now wired in production, so the shared
	// AuditLogOrder input is reachable through the Organization type the
	// connection's nodes name; reuse it rather than minting a second type of
	// the same name (which graphql-go rejects).
	orderInput := res.mutationInputs["AuditLogOrder"]
	if orderInput == nil {
		orderInput = graphql.NewInputObject(graphql.InputObjectConfig{
			Name: "AuditLogOrder",
			Fields: graphql.InputObjectConfigFieldMap{
				"direction": &graphql.InputObjectFieldConfig{Type: graphql.String},
				"field":     &graphql.InputObjectFieldConfig{Type: graphql.String},
			},
		})
	}
	query := graphql.NewObject(graphql.ObjectConfig{
		Name: "Query",
		Fields: graphql.Fields{
			"auditLog": &graphql.Field{
				Type: res.gqlOrganizationAuditEntryConnectionType(),
				Args: graphql.FieldConfigArgument{
					"first":   &graphql.ArgumentConfig{Type: graphql.Int},
					"last":    &graphql.ArgumentConfig{Type: graphql.Int},
					"after":   &graphql.ArgumentConfig{Type: graphql.String},
					"before":  &graphql.ArgumentConfig{Type: graphql.String},
					"query":   &graphql.ArgumentConfig{Type: graphql.String},
					"orderBy": &graphql.ArgumentConfig{Type: orderInput},
				},
				Resolve: func(p graphql.ResolveParams) (interface{}, error) {
					return res.organizationAuditLogConnection(p, org)
				},
			},
		},
	})
	schema, err := graphql.NewSchema(graphql.SchemaConfig{Query: query})
	if err != nil {
		t.Fatalf("build audit-log schema: %v", err)
	}
	return schema
}

func TestOrganizationAuditLogConnectionServesModeledEntries(t *testing.T) {
	h := newAccountHarness(t)
	admin := h.store.UsersByLogin["admin"]
	org := h.store.CreateOrg(admin, "acme", "Acme", "")
	if org == nil {
		t.Fatal("org not created")
	}
	// A second account the entries reference, so blockedUser/user resolve.
	h.user("mallory")
	repo := h.store.CreateOrgRepo(org, admin, "widgets", "", true)
	if repo == nil {
		t.Fatal("repo not created")
	}

	// Record one entry per modeled concrete type, plus one whose action has no
	// modeled type (it must be omitted from the connection).
	h.store.RecordAuditEntry("org.create", "admin", "acme", map[string]interface{}{"org_id": org.ID, "billing_plan": "FREE"})
	h.store.RecordAuditEntry("org.invite_member", "admin", "acme", map[string]interface{}{"invitation_id": 1, "role": "direct_member", "email": "invitee@example.com"})
	h.store.RecordAuditEntry("org.block_user", "admin", "acme", map[string]interface{}{"blocked_user": "mallory"})
	h.store.RecordAuditEntry("org.unblock_user", "admin", "acme", map[string]interface{}{"blocked_user": "mallory"})
	h.store.RecordAuditEntry("org.remove_outside_collaborator", "admin", "acme", map[string]interface{}{"user": "mallory"})
	h.store.RecordAuditEntry("repo.create", "admin", "acme", map[string]interface{}{"repo": "acme/widgets", "repo_id": repo.ID})
	h.store.RecordAuditEntry("team.create", "admin", "acme", map[string]interface{}{"team_slug": "eng"}) // unmodeled

	schema := auditLogTestSchema(t, h.res, org)

	document := `{
	  auditLog(first: 50, orderBy: {field: CREATED_AT, direction: ASC}) {
	    totalCount
	    pageInfo { hasNextPage hasPreviousPage }
	    edges { cursor node { __typename } }
	    nodes {
	      __typename
	      ... on AuditEntry { action actorLogin createdAt operationType actor { __typename ... on User { login } } }
	      ... on OrganizationAuditEntryData { organizationName organization { login } }
	      ... on OrgCreateAuditEntry { billingPlan }
	      ... on OrgInviteMemberAuditEntry { email }
	      ... on OrgBlockUserAuditEntry { blockedUserName blockedUser { login } }
	      ... on OrgUnblockUserAuditEntry { blockedUserName }
	      ... on OrgRemoveOutsideCollaboratorAuditEntry { userLogin user { login } }
	      ... on RepoCreateAuditEntry { visibility repositoryName repository { name } }
	    }
	  }
	}`

	ctx := context.WithValue(context.Background(), accountViewerKey{}, admin)
	result := graphql.Do(graphql.Params{Schema: schema, RequestString: document, Context: ctx})
	if len(result.Errors) != 0 {
		t.Fatalf("graphql errors: %v", result.Errors)
	}
	data := decodeAuditResult(t, result.Data)

	conn, _ := data["auditLog"].(map[string]interface{})
	if got, _ := conn["totalCount"].(float64); int(got) != 6 {
		t.Fatalf("totalCount = %v, want 6 (team.create must be omitted)", conn["totalCount"])
	}
	nodes, _ := conn["nodes"].([]interface{})
	if len(nodes) != 6 {
		t.Fatalf("nodes = %d, want 6", len(nodes))
	}

	byType := map[string]map[string]interface{}{}
	for _, n := range nodes {
		node, _ := n.(map[string]interface{})
		tn, _ := node["__typename"].(string)
		byType[tn] = node
		// Every node's shared AuditEntry members must resolve.
		if node["action"] == nil || node["createdAt"] == nil {
			t.Errorf("%s: missing shared AuditEntry members: %#v", tn, node)
		}
		if node["organizationName"] != "acme" {
			t.Errorf("%s: organizationName = %#v, want acme", tn, node["organizationName"])
		}
		if org, _ := node["organization"].(map[string]interface{}); org["login"] != "acme" {
			t.Errorf("%s: organization.login = %#v", tn, org["login"])
		}
	}

	for _, want := range []string{
		"OrgCreateAuditEntry", "OrgInviteMemberAuditEntry", "OrgBlockUserAuditEntry",
		"OrgUnblockUserAuditEntry", "OrgRemoveOutsideCollaboratorAuditEntry", "RepoCreateAuditEntry",
	} {
		if byType[want] == nil {
			t.Errorf("missing node of type %s", want)
		}
	}

	if email := byType["OrgInviteMemberAuditEntry"]["email"]; email != "invitee@example.com" {
		t.Errorf("OrgInviteMemberAuditEntry.email = %#v, want invitee@example.com", email)
	}

	if bp := byType["OrgCreateAuditEntry"]["billingPlan"]; bp != "FREE" {
		t.Errorf("OrgCreateAuditEntry.billingPlan = %#v, want FREE", bp)
	}
	if op := byType["OrgCreateAuditEntry"]["operationType"]; op != "CREATE" {
		t.Errorf("OrgCreateAuditEntry.operationType = %#v, want CREATE", op)
	}
	if bu := byType["OrgBlockUserAuditEntry"]["blockedUserName"]; bu != "mallory" {
		t.Errorf("OrgBlockUserAuditEntry.blockedUserName = %#v, want mallory", bu)
	}
	if blocked, _ := byType["OrgBlockUserAuditEntry"]["blockedUser"].(map[string]interface{}); blocked["login"] != "mallory" {
		t.Errorf("OrgBlockUserAuditEntry.blockedUser.login = %#v", blocked["login"])
	}
	repoNode := byType["RepoCreateAuditEntry"]
	if repoNode["visibility"] != "PRIVATE" {
		t.Errorf("RepoCreateAuditEntry.visibility = %#v, want PRIVATE", repoNode["visibility"])
	}
	if repoNode["repositoryName"] != "widgets" {
		t.Errorf("RepoCreateAuditEntry.repositoryName = %#v, want widgets", repoNode["repositoryName"])
	}
	if repository, _ := repoNode["repository"].(map[string]interface{}); repository["name"] != "widgets" {
		t.Errorf("RepoCreateAuditEntry.repository.name = %#v", repository["name"])
	}
	// The actor union resolves to User for a login-actor entry.
	if actor, _ := byType["OrgCreateAuditEntry"]["actor"].(map[string]interface{}); actor["__typename"] != "User" || actor["login"] != "admin" {
		t.Errorf("OrgCreateAuditEntry.actor = %#v, want User{login:admin}", byType["OrgCreateAuditEntry"]["actor"])
	}
}

// TestOrganizationAuditLogIsOwnerOnly pins the owner-only access rule: a viewer
// who does not administer the org receives an empty connection, never an error.
func TestOrganizationAuditLogIsOwnerOnly(t *testing.T) {
	h := newAccountHarness(t)
	admin := h.store.UsersByLogin["admin"]
	org := h.store.CreateOrg(admin, "acme", "Acme", "")
	h.store.RecordAuditEntry("org.create", "admin", "acme", map[string]interface{}{"org_id": org.ID})

	schema := auditLogTestSchema(t, h.res, org)
	document := `{ auditLog(first: 10) { totalCount nodes { __typename } } }`

	stranger := h.user("passerby")
	ctx := context.WithValue(context.Background(), accountViewerKey{}, stranger)
	result := graphql.Do(graphql.Params{Schema: schema, RequestString: document, Context: ctx})
	if len(result.Errors) != 0 {
		t.Fatalf("graphql errors: %v", result.Errors)
	}
	data := decodeAuditResult(t, result.Data)
	conn, _ := data["auditLog"].(map[string]interface{})
	if got, _ := conn["totalCount"].(float64); int(got) != 0 {
		t.Fatalf("non-admin totalCount = %v, want 0 (owner-only)", conn["totalCount"])
	}
	if nodes, _ := conn["nodes"].([]interface{}); len(nodes) != 0 {
		t.Fatalf("non-admin nodes = %d, want 0", len(nodes))
	}
}

func decodeAuditResult(t *testing.T, raw interface{}) map[string]interface{} {
	t.Helper()
	body, err := json.Marshal(raw)
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]interface{}
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatal(err)
	}
	return out
}
