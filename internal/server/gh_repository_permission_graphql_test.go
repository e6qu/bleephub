package bleephub

import (
	"testing"
)

// A collaborator granted triage_plus over REST reads back as TRIAGE_PLUS in
// GraphQL, not folded to READ or TRIAGE.
func TestGraphQLRepositoryPermissionCarriesTriagePlus(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	f := newGQLAuthzFixture(t, s.Server, "triage-plus", true)
	if !s.store.AddRepoCollaborator(f.owner.Login, f.repo.Name, f.stranger.Login, "triage_plus") {
		t.Fatal("could not add the triage_plus collaborator")
	}
	env := s.gqlAuthzPost(t, f.ownerToken, `query($o:String!,$n:String!){repository(owner:$o,name:$n){
		collaborators(first:10){ edges{ permission node{ login } } } }}`,
		map[string]interface{}{"o": f.owner.Login, "n": f.repo.Name})
	if errs := gqlAuthzErrors(env); len(errs) > 0 {
		t.Fatalf("collaborators query failed: %v", errs)
	}
	edges := env["data"].(map[string]interface{})["repository"].(map[string]interface{})["collaborators"].(map[string]interface{})["edges"].([]interface{})
	for _, raw := range edges {
		edge := raw.(map[string]interface{})
		if edge["node"].(map[string]interface{})["login"] == f.stranger.Login {
			if edge["permission"] != "TRIAGE_PLUS" {
				t.Fatalf("triage_plus collaborator permission = %v", edge["permission"])
			}
			return
		}
	}
	t.Fatalf("the triage_plus collaborator is not listed: %v", edges)
}
