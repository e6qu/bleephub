package bleephub

import (
	"testing"
	"time"
)

// An iteration named by its id keeps its identity through an
// iterationConfiguration replacement even when renamed, so item values that
// point at it stay valid; an id that is not one of the field's iterations is
// refused rather than minted under a name the caller did not choose.
func TestProjectsV2IterationKeepsItsIDAcrossReplacement(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	s.replaceClockNow(func() time.Time { return time.Date(2026, time.October, 5, 12, 0, 0, 0, time.UTC) })
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

	// Completing an iteration moves it to the completed connection without
	// changing the identity referenced by existing project items.
	s.replaceClockNow(func() time.Time { return time.Date(2026, time.October, 20, 12, 0, 0, 0, time.UTC) })
	data = s.gqlData(t, `query($id:ID!){node(id:$id){ ... on ProjectV2IterationField {
		configuration{ iterations{ id } completedIterations{ id title } }
	}}}`, map[string]interface{}{"id": field["id"]})
	configuration := data["node"].(map[string]interface{})["configuration"].(map[string]interface{})
	completed := configuration["completedIterations"].([]interface{})
	if len(configuration["iterations"].([]interface{})) != 0 || len(completed) != 1 ||
		completed[0].(map[string]interface{})["id"] != sprintID {
		t.Fatalf("completed configuration = %v, want original iteration %s", configuration, sprintID)
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

func TestProjectV2FieldNodeRequiresProjectReadAccess(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	fixture := s.newGQLProjectAuthzFixture(t, "field-node")
	query := `query($id:ID!){node(id:$id){id ... on ProjectV2Field {name}}}`
	vars := map[string]interface{}{"id": fixture.field.NodeID}
	for _, tc := range []struct {
		name, token string
		visible     bool
	}{
		{"owner", fixture.ownerToken, true},
		{"stranger", fixture.strangerToken, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			envelope := s.gqlAuthzPost(t, tc.token, query, vars)
			requireNoGQLErrors(t, envelope)
			data := envelope["data"].(map[string]interface{})
			if (data["node"] != nil) != tc.visible {
				t.Fatalf("field node visibility = %v, want %v", data["node"], tc.visible)
			}
		})
	}
}
