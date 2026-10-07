package bleephub

import (
	"testing"
)

// An iteration named by its id keeps its identity through an
// iterationConfiguration replacement even when renamed, so item values that
// point at it stay valid; an id that is not one of the field's iterations is
// refused rather than minted under a name the caller did not choose.
func TestProjectsV2IterationKeepsItsIDAcrossReplacement(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	_, project := s.seedProjectV2Org(t, "pv2-iteration-org", "Iterations")

	data := s.gqlData(t, `mutation($p:ID!){
		createProjectV2Field(input:{projectId:$p,dataType:ITERATION,name:"Sprint",
			iterationConfiguration:{startDate:"2026-10-05",duration:14,iterations:[{title:"Sprint 1",startDate:"2026-10-05",duration:14}]}}){
			projectV2Field{ ... on ProjectV2IterationField { id configuration{ iterations{ id title } } } }
		}
	}`, map[string]interface{}{"p": project.NodeID})
	field := data["createProjectV2Field"].(map[string]interface{})["projectV2Field"].(map[string]interface{})
	iterations := field["configuration"].(map[string]interface{})["iterations"].([]interface{})
	if len(iterations) != 1 {
		t.Fatalf("created iterations = %v", iterations)
	}
	sprintID := iterations[0].(map[string]interface{})["id"].(string)

	data = s.gqlData(t, `mutation($f:ID!,$id:String!){
		updateProjectV2Field(input:{fieldId:$f,
			iterationConfiguration:{startDate:"2026-10-05",duration:14,iterations:[{id:$id,title:"Sprint One",startDate:"2026-10-05",duration:14}]}}){
			projectV2Field{ ... on ProjectV2IterationField { configuration{ iterations{ id title } } } }
		}
	}`, map[string]interface{}{"f": field["id"], "id": sprintID})
	renamed := data["updateProjectV2Field"].(map[string]interface{})["projectV2Field"].(map[string]interface{})["configuration"].(map[string]interface{})["iterations"].([]interface{})
	got := renamed[0].(map[string]interface{})
	if got["id"] != sprintID || got["title"] != "Sprint One" {
		t.Fatalf("renamed iteration = %v, want id %s kept", got, sprintID)
	}

	env := s.gqlDo(t, `mutation($f:ID!){
		updateProjectV2Field(input:{fieldId:$f,
			iterationConfiguration:{startDate:"2026-10-05",duration:14,iterations:[{id:"notreal1",title:"Sprint X",startDate:"2026-10-05",duration:14}]}}){
			projectV2Field{ ... on ProjectV2IterationField { id } }
		}
	}`, map[string]interface{}{"f": field["id"]})
	if errs, _ := env["errors"].([]interface{}); len(errs) == 0 {
		t.Fatalf("an unknown iteration id was accepted: %v", env)
	}
}
