package bleephub

import (
	"fmt"
	"strings"
	"testing"

	"github.com/e6qu/bleephub/internal/store"
)

func TestProjectV2Store_DeleteProjectUnindexesContentItems(t *testing.T) {
	t.Parallel()
	st := store.NewProjectV2Store(nil)
	project := st.CreateProject(1, "User", "Cleanup", 1)
	item := st.AddItem(project.ID, "Issue", 42, 1)
	if item == nil {
		t.Fatal("AddItem returned nil")
	}
	if got := st.ListItemsForIssue(42); len(got) != 1 {
		t.Fatalf("precondition ListItemsForIssue = %#v, want one item", got)
	}
	if !st.DeleteProject(project.ID) {
		t.Fatal("DeleteProject returned false")
	}
	if got := st.ListItemsForIssue(42); len(got) != 0 {
		t.Fatalf("DeleteProject left stale content index entries: %#v", got)
	}
}

func TestProjectsV2GraphQL_CreateProjectRequiresResolvedOwner(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	admin := s.store.UsersByLogin["admin"]
	if admin == nil {
		t.Fatal("admin user not seeded")
	}
	before := len(s.store.ProjectsV2.ListProjectsForOwner(admin.ID, "User"))
	resp := s.gqlDo(t, `mutation($owner:ID!){
		createProjectV2(input:{ownerId:$owner,title:"Unknown owner"}){
			projectV2 { id title }
		}
	}`, map[string]interface{}{"owner": "PVTI_unknown_owner"})
	errs, _ := resp["errors"].([]interface{})
	if len(errs) == 0 {
		t.Fatalf("unknown owner unexpectedly succeeded: %v", resp)
	}
	if !strings.Contains(fmt.Sprint(errs[0]), "Could not resolve to an owner with the global id of 'PVTI_unknown_owner'.") {
		t.Fatalf("unexpected unknown-owner error: %v", errs[0])
	}
	if after := len(s.store.ProjectsV2.ListProjectsForOwner(admin.ID, "User")); after != before {
		t.Fatalf("unknown owner mutation created user-owned project: before=%d after=%d", before, after)
	}
}

func TestProjectsV2GraphQL_CreateProjectUsesResolvedUserAndOrganizationOwners(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	admin := s.store.UsersByLogin["admin"]
	if admin == nil {
		t.Fatal("admin user not seeded")
	}
	orgJSON := s.createOrg(t, "pv2-gql-owner-org")
	orgNodeID, _ := orgJSON["node_id"].(string)
	org := s.store.OrgsByLogin["pv2-gql-owner-org"]
	if org == nil || orgNodeID == "" {
		t.Fatalf("created organization missing node ID: org=%v json=%v", org, orgJSON)
	}

	userResp := s.gqlData(t, `mutation($owner:ID!){
		createProjectV2(input:{ownerId:$owner,title:"User owned"}){
			projectV2 { id title }
		}
	}`, map[string]interface{}{"owner": admin.NodeID})
	userProject := userResp["createProjectV2"].(map[string]interface{})["projectV2"].(map[string]interface{})
	if userProject["title"] != "User owned" {
		t.Fatalf("user project response = %v", userProject)
	}
	if !projectV2OwnerHasTitle(s.store, admin.ID, "User", "User owned") {
		t.Fatalf("user-owned project was not stored under admin")
	}

	orgResp := s.gqlData(t, `mutation($owner:ID!){
		createProjectV2(input:{ownerId:$owner,title:"Organization owned"}){
			projectV2 { id title }
		}
	}`, map[string]interface{}{"owner": orgNodeID})
	orgProject := orgResp["createProjectV2"].(map[string]interface{})["projectV2"].(map[string]interface{})
	if orgProject["title"] != "Organization owned" {
		t.Fatalf("organization project response = %v", orgProject)
	}
	if !projectV2OwnerHasTitle(s.store, org.ID, "Organization", "Organization owned") {
		t.Fatalf("organization-owned project was not stored under %s", org.Login)
	}
}

func projectV2OwnerHasTitle(st *store.Store, ownerID int, ownerType, title string) bool {
	for _, project := range st.ProjectsV2.ListProjectsForOwner(ownerID, ownerType) {
		if project.Title == title {
			return true
		}
	}
	return false
}

func TestProjectsV2GraphQL_FieldValueKinds(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	sweep := s.sweepRepo(t, "gql-project-v2-fields")
	owner, repoName := sweep.owner, sweep.name
	issue := decodeJSONWithStatus(t, s.post(t, "/api/v3/repos/"+owner+"/"+repoName+"/issues", defaultToken, map[string]interface{}{
		"title": "project item",
	}), 201)
	issueNumber := int(issue["number"].(float64))
	repo := s.store.GetRepo(owner, repoName)
	admin := s.store.UsersByLogin["admin"]
	project := s.store.ProjectsV2.CreateProject(admin.ID, "User", "GraphQL fields", admin.ID)
	item := s.store.ProjectsV2.AddItem(project.ID, "Issue", int(issue["id"].(float64)), admin.ID)

	textField := s.store.ProjectsV2.CreateField(project.ID, "Notes", store.ProjectV2FieldText, nil, nil)
	numberField := s.store.ProjectsV2.CreateField(project.ID, "Effort", store.ProjectV2FieldNumber, nil, nil)
	dateField := s.store.ProjectsV2.CreateField(project.ID, "Due", store.ProjectV2FieldDate, nil, nil)
	selectField := s.store.ProjectsV2.CreateField(project.ID, "Priority", store.ProjectV2FieldSingleSelect, []*store.ProjectV2SingleSelectOption{
		{Name: "High", Color: "RED"},
		{Name: "Low", Color: "GREEN"},
	}, nil)
	iterationField := s.store.ProjectsV2.CreateField(project.ID, "Sprint", store.ProjectV2FieldIteration, nil, &store.ProjectV2IterationConfiguration{
		StartDate: "2026-07-06",
		Duration:  7,
		Iterations: []*store.ProjectV2Iteration{
			{Title: "Sprint 1", StartDate: "2026-07-06", Duration: 7},
			{Title: "Sprint 2", StartDate: "2026-07-13", Duration: 7},
		},
	})

	update := func(field *store.ProjectV2Field, value map[string]interface{}) {
		t.Helper()
		data := s.gqlData(t, `mutation($project:ID!,$item:ID!,$field:ID!,$value:ProjectV2FieldValue!){
			updateProjectV2ItemFieldValue(input:{projectId:$project,itemId:$item,fieldId:$field,value:$value}){
				projectV2Item { id }
			}
		}`, map[string]interface{}{
			"project": project.NodeID,
			"item":    item.NodeID,
			"field":   field.NodeID,
			"value":   value,
		})
		got := data["updateProjectV2ItemFieldValue"].(map[string]interface{})["projectV2Item"].(map[string]interface{})["id"]
		if got != item.NodeID {
			t.Fatalf("updated item id = %v, want %s", got, item.NodeID)
		}
	}
	update(textField, map[string]interface{}{"text": "ready"})
	update(numberField, map[string]interface{}{"number": 8})
	update(dateField, map[string]interface{}{"date": "2030-12-31"})
	update(selectField, map[string]interface{}{"singleSelectOptionId": selectField.Options[0].ID})
	update(iterationField, map[string]interface{}{"iterationId": iterationField.Iteration.Iterations[1].ID})

	query := `query($owner:String!,$name:String!,$number:Int!){
		repository(owner:$owner,name:$name){
			issue(number:$number){
				projectItems(first:10){
					totalCount
					nodes{
						notes: fieldValueByName(name:"Notes"){ __typename ... on ProjectV2ItemFieldTextValue { text } }
						effort: fieldValueByName(name:"Effort"){ __typename ... on ProjectV2ItemFieldNumberValue { number } }
						due: fieldValueByName(name:"Due"){ __typename ... on ProjectV2ItemFieldDateValue { date } }
						priority: fieldValueByName(name:"Priority"){ __typename ... on ProjectV2ItemFieldSingleSelectValue { optionId name } }
						sprint: fieldValueByName(name:"Sprint"){ __typename ... on ProjectV2ItemFieldIterationValue { iterationId title startDate duration } }
					}
				}
			}
		}
	}`
	data := s.gqlData(t, query, map[string]interface{}{"owner": owner, "name": repoName, "number": issueNumber})
	items := data["repository"].(map[string]interface{})["issue"].(map[string]interface{})["projectItems"].(map[string]interface{})
	if got := int(items["totalCount"].(float64)); got != 1 {
		t.Fatalf("projectItems.totalCount = %d, want 1: %v", got, items)
	}
	node := items["nodes"].([]interface{})[0].(map[string]interface{})
	if got := node["notes"].(map[string]interface{}); got["__typename"] != "ProjectV2ItemFieldTextValue" || got["text"] != "ready" {
		t.Fatalf("notes value = %v", got)
	}
	if got := node["effort"].(map[string]interface{}); got["__typename"] != "ProjectV2ItemFieldNumberValue" || got["number"].(float64) != 8 {
		t.Fatalf("effort value = %v", got)
	}
	if got := node["due"].(map[string]interface{}); got["__typename"] != "ProjectV2ItemFieldDateValue" || got["date"] != "2030-12-31" {
		t.Fatalf("due value = %v", got)
	}
	if got := node["priority"].(map[string]interface{}); got["__typename"] != "ProjectV2ItemFieldSingleSelectValue" || got["optionId"] != selectField.Options[0].ID || got["name"] != "High" {
		t.Fatalf("priority value = %v", got)
	}
	if got := node["sprint"].(map[string]interface{}); got["__typename"] != "ProjectV2ItemFieldIterationValue" || got["iterationId"] != iterationField.Iteration.Iterations[1].ID || got["title"] != "Sprint 2" || got["startDate"] != "2026-07-13" || got["duration"].(float64) != 7 {
		t.Fatalf("sprint value = %v", got)
	}

	resp := s.gqlDo(t, `mutation($project:ID!,$item:ID!,$field:ID!){
		updateProjectV2ItemFieldValue(input:{projectId:$project,itemId:$item,fieldId:$field,value:{text:"wrong"}}){
			projectV2Item { id }
		}
	}`, map[string]interface{}{"project": project.NodeID, "item": item.NodeID, "field": numberField.NodeID})
	if errs, ok := resp["errors"]; !ok || errs == nil {
		t.Fatalf("wrong value kind unexpectedly succeeded: %v", resp)
	}
	if repo == nil {
		t.Fatal("repo disappeared during Projects v2 GraphQL test")
	}
}

func TestProjectsV2GraphQL_ProjectLevelConnections(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	sweep := s.sweepRepo(t, "gql-project-v2-project-connections")
	owner, repoName := sweep.owner, sweep.name
	issue := decodeJSONWithStatus(t, s.post(t, "/api/v3/repos/"+owner+"/"+repoName+"/issues", defaultToken, map[string]interface{}{
		"title": "project item",
	}), 201)
	issueID := int(issue["id"].(float64))
	issueNumber := int(issue["number"].(float64))
	admin := s.store.UsersByLogin["admin"]
	project := s.store.ProjectsV2.CreateProject(admin.ID, "User", "GraphQL project", admin.ID)
	s.store.ProjectsV2.AddItem(project.ID, "Issue", issueID, admin.ID)
	s.store.ProjectsV2.AddDraftItem(project.ID, "Draft item", "Body", admin.ID)
	stage := s.store.ProjectsV2.CreateField(project.ID, "Stage", store.ProjectV2FieldSingleSelect, []*store.ProjectV2SingleSelectOption{
		{Name: "Todo", Color: "GRAY", Description: "ready to schedule"},
		{Name: "Done", Color: "GREEN"},
	}, nil)
	sprint := s.store.ProjectsV2.CreateField(project.ID, "Sprint", store.ProjectV2FieldIteration, nil, &store.ProjectV2IterationConfiguration{
		StartDate: "2026-07-06",
		Duration:  14,
		Iterations: []*store.ProjectV2Iteration{
			{Title: "Sprint 1", StartDate: "2026-07-06", Duration: 14},
		},
	})
	filter := "is:issue"
	view := s.store.ProjectsV2.CreateView(project.ID, "Issues board", "board", &filter, []int{stage.ID, sprint.ID}, admin.ID)
	if view == nil {
		t.Fatal("CreateView returned nil")
	}

	query := `query($owner:String!,$name:String!,$number:Int!){
		repository(owner:$owner,name:$name){
			issue(number:$number){
				projectItems(first:10){
					nodes{
						project{
							fields(first:50){
								totalCount
								nodes{
									... on ProjectV2FieldCommon { id name dataType }
									... on ProjectV2SingleSelectField { options { id name color description } }
									... on ProjectV2IterationField { configuration { startDay duration iterations { id title startDate duration } completedIterations { id title startDate duration } } }
								}
							}
							views(first:10){
								totalCount
								nodes{ id number name layout filter fields(first:50){ nodes { ... on ProjectV2FieldCommon { id } } } }
							}
							items(first:10){
								totalCount
								nodes{ id }
								pageInfo{ hasNextPage hasPreviousPage startCursor endCursor }
							}
						}
					}
				}
			}
		}
	}`
	data := s.gqlData(t, query, map[string]interface{}{"owner": owner, "name": repoName, "number": issueNumber})
	projectNode := data["repository"].(map[string]interface{})["issue"].(map[string]interface{})["projectItems"].(map[string]interface{})["nodes"].([]interface{})[0].(map[string]interface{})["project"].(map[string]interface{})

	fields := projectNode["fields"].(map[string]interface{})
	// The project carries the seeded defaults plus the two this test created.
	if got, want := int(fields["totalCount"].(float64)), len(s.store.ProjectsV2.FieldsForProject(project.ID)); got != want {
		t.Fatalf("fields.totalCount = %d, want %d: %v", got, want, fields)
	}
	fieldNodes := fields["nodes"].([]interface{})
	byNodeID := map[string]map[string]interface{}{}
	for _, raw := range fieldNodes {
		node := raw.(map[string]interface{})
		byNodeID[node["id"].(string)] = node
	}
	firstField := byNodeID[stage.NodeID]
	if firstField == nil || firstField["name"] != "Stage" || firstField["dataType"] != string(store.ProjectV2FieldSingleSelect) {
		t.Fatalf("stage field = %v", firstField)
	}
	options := firstField["options"].([]interface{})
	if len(options) != 2 || options[0].(map[string]interface{})["name"] != "Todo" || options[0].(map[string]interface{})["description"] != "ready to schedule" {
		t.Fatalf("stage options = %v", options)
	}
	secondField := byNodeID[sprint.NodeID]
	if secondField == nil || secondField["dataType"] != string(store.ProjectV2FieldIteration) {
		t.Fatalf("sprint field = %v", secondField)
	}
	iteration := secondField["configuration"].(map[string]interface{})
	// 2026-07-06 is a Monday (startDay 1); Sprint 1 ended 2026-07-20, so it
	// reports under completedIterations, exactly as real GitHub splits them.
	if iteration["startDay"].(float64) != 1 || iteration["duration"].(float64) != 14 {
		t.Fatalf("iteration configuration = %v", iteration)
	}
	if active := iteration["iterations"].([]interface{}); len(active) != 0 {
		t.Fatalf("iterations = %v, want none active", active)
	}
	completed := iteration["completedIterations"].([]interface{})
	if len(completed) != 1 || completed[0].(map[string]interface{})["title"] != "Sprint 1" ||
		completed[0].(map[string]interface{})["startDate"] != "2026-07-06" {
		t.Fatalf("completedIterations = %v", completed)
	}

	views := projectNode["views"].(map[string]interface{})
	// The seeded "View 1" every project starts with, plus the one this test
	// created.
	if got := int(views["totalCount"].(float64)); got != 2 {
		t.Fatalf("views.totalCount = %d, want 2: %v", got, views)
	}
	var viewNode map[string]interface{}
	for _, raw := range views["nodes"].([]interface{}) {
		if node := raw.(map[string]interface{}); node["id"] == view.NodeID {
			viewNode = node
		}
	}
	if viewNode == nil || viewNode["name"] != "Issues board" || viewNode["layout"] != "BOARD_LAYOUT" || viewNode["filter"] != "is:issue" {
		t.Fatalf("view node = %v", viewNode)
	}
	viewFields := viewNode["fields"].(map[string]interface{})["nodes"].([]interface{})
	if len(viewFields) != 2 ||
		viewFields[0].(map[string]interface{})["id"] != stage.NodeID ||
		viewFields[1].(map[string]interface{})["id"] != sprint.NodeID {
		t.Fatalf("view fields = %v", viewFields)
	}

	items := projectNode["items"].(map[string]interface{})
	if got := int(items["totalCount"].(float64)); got != 2 {
		t.Fatalf("items.totalCount = %d, want 2: %v", got, items)
	}
	if got := len(items["nodes"].([]interface{})); got != 2 {
		t.Fatalf("items nodes len = %d, want 2: %v", got, items)
	}
	pageInfo := items["pageInfo"].(map[string]interface{})
	if pageInfo["hasNextPage"] != false || pageInfo["hasPreviousPage"] != false || pageInfo["startCursor"] == nil || pageInfo["endCursor"] == nil {
		t.Fatalf("items pageInfo = %v", pageInfo)
	}
}
