package bleephub

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/e6qu/bleephub/internal/server/testutil"
	"github.com/e6qu/bleephub/internal/store"
)

// Projects v2 — the read entry points, the mutation vertical and the webhook
// family, exercised the way an SDK reaches them: from the owner rather than
// from an issue that happens to already be on a project.

// --- Query entry points ----------------------------------------------------

// TestProjectsV2GraphQL_OwnerEntryPoints covers the fields `gh project list`
// and `gh project view` start from. Before these existed a project was
// reachable only through an issue already on one, so neither command could
// find a project at all.
func TestProjectsV2GraphQL_OwnerEntryPoints(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	org, project := s.seedProjectV2Org(t, "pv2-entry-org", "Roadmap")
	s.store.ProjectsV2.UpdateProjectDetails(project.ID, store.ProjectV2Update{
		ShortDescription: strPtr("the plan"),
		Readme:           strPtr("# Roadmap"),
	})

	data := s.gqlData(t, `query($login:String!){
		organization(login:$login){
			projectsV2(first:10){
				totalCount
				nodes{ id number title shortDescription readme public closed template url resourcePath
					owner{ ... on Organization { login } }
					creator{ login }
					items{ totalCount }
					fields{ totalCount }
				}
			}
			projectV2(number:1){ id title }
		}
	}`, map[string]interface{}{"login": org.Login})

	orgNode := data["organization"].(map[string]interface{})
	connection := orgNode["projectsV2"].(map[string]interface{})
	if got := int(connection["totalCount"].(float64)); got != 1 {
		t.Fatalf("projectsV2.totalCount = %d, want 1", got)
	}
	node := connection["nodes"].([]interface{})[0].(map[string]interface{})
	if node["id"] != project.NodeID || node["title"] != "Roadmap" {
		t.Fatalf("project node = %v", node)
	}
	if node["shortDescription"] != "the plan" || node["readme"] != "# Roadmap" {
		t.Errorf("description/readme = %v / %v", node["shortDescription"], node["readme"])
	}
	if node["public"] != false || node["closed"] != false || node["template"] != false {
		t.Errorf("flags = %v", node)
	}
	wantPath := "/orgs/" + org.Login + "/projects/1"
	if node["resourcePath"] != wantPath {
		t.Errorf("resourcePath = %v, want %q", node["resourcePath"], wantPath)
	}
	// url is absolute; with no external base configured it equals the path.
	if !strings.HasSuffix(node["url"].(string), wantPath) {
		t.Errorf("url = %v, want it to end with %q", node["url"], wantPath)
	}
	owner := node["owner"].(map[string]interface{})
	if owner["login"] != org.Login {
		t.Errorf("owner.login = %v, want %q", owner["login"], org.Login)
	}
	if creator := node["creator"].(map[string]interface{}); creator["login"] != "admin" {
		t.Errorf("creator.login = %v, want admin", creator["login"])
	}
	// A new project is seeded with GitHub's built-in fields, so field-list is
	// not empty on a project nobody has configured yet.
	fields := node["fields"].(map[string]interface{})
	if got, want := int(fields["totalCount"].(float64)), len(projectV2SeededFieldNames(t, s, project.ID)); got != want {
		t.Errorf("seeded fields.totalCount = %d, want %d", got, want)
	}
	if lookup := orgNode["projectV2"].(map[string]interface{}); lookup["id"] != project.NodeID {
		t.Errorf("projectV2(number:1) = %v", lookup)
	}
}

// TestProjectsV2GraphQL_UserOwnerEntryPoints covers the user-owned half of the
// same pair — `gh project list --owner @me` resolves through it.
func TestProjectsV2GraphQL_UserOwnerEntryPoints(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	admin := s.store.UsersByLogin["admin"]
	project := s.store.ProjectsV2.CreateProject(admin.ID, "User", "Personal", admin.ID)

	data := s.gqlData(t, `query($login:String!){
		user(login:$login){
			projectsV2(first:10){ totalCount nodes{ id title resourcePath } }
			projectV2(number:1){ id }
		}
	}`, map[string]interface{}{"login": admin.Login})
	userNode := data["user"].(map[string]interface{})
	connection := userNode["projectsV2"].(map[string]interface{})
	if int(connection["totalCount"].(float64)) != 1 {
		t.Fatalf("user projectsV2 = %v", connection)
	}
	node := connection["nodes"].([]interface{})[0].(map[string]interface{})
	if node["id"] != project.NodeID {
		t.Fatalf("user project node = %v", node)
	}
	if want := "/users/" + admin.Login + "/projects/1"; node["resourcePath"] != want {
		t.Errorf("user project resourcePath = %v, want %q", node["resourcePath"], want)
	}
	if lookup := userNode["projectV2"].(map[string]interface{}); lookup["id"] != project.NodeID {
		t.Errorf("user projectV2(number:1) = %v", lookup)
	}
}

// TestProjectsV2GraphQL_PrivateProjectsAreNotListedToStrangers pins that the
// entry points do not become a way to enumerate another account's private
// projects: the owner-scoped read predicate gates the connection, not just the
// per-project lookup.
func TestProjectsV2GraphQL_PrivateProjectsAreNotListedToStrangers(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	owner, ownerToken := s.seedProjectV2User(t, "pv2-private-owner")
	stranger, strangerToken := s.seedProjectV2User(t, "pv2-private-stranger")
	_ = stranger

	private := s.store.ProjectsV2.CreateProject(owner.ID, "User", "Secret plans", owner.ID)
	public := s.store.ProjectsV2.CreateProject(owner.ID, "User", "Open plans", owner.ID)
	s.store.ProjectsV2.UpdateProjectDetails(public.ID, store.ProjectV2Update{Public: boolPtr(true)})

	query := `query($login:String!){ user(login:$login){ projectsV2(first:10){ nodes{ id title } } } }`

	ownerView := s.gqlDataAs(t, ownerToken, query, map[string]interface{}{"login": owner.Login})
	ownerNodes := ownerView["user"].(map[string]interface{})["projectsV2"].(map[string]interface{})["nodes"].([]interface{})
	if len(ownerNodes) != 2 {
		t.Fatalf("the owner sees %d of their own projects, want 2", len(ownerNodes))
	}

	strangerView := s.gqlDataAs(t, strangerToken, query, map[string]interface{}{"login": owner.Login})
	strangerNodes := strangerView["user"].(map[string]interface{})["projectsV2"].(map[string]interface{})["nodes"].([]interface{})
	if len(strangerNodes) != 1 {
		t.Fatalf("a stranger sees %d projects, want only the public one: %v", len(strangerNodes), strangerNodes)
	}
	if got := strangerNodes[0].(map[string]interface{}); got["id"] != public.NodeID {
		t.Fatalf("a stranger was served the private project: %v", got)
	}
	// The per-number lookup must agree with the listing.
	env := s.gqlDoAs(t, strangerToken,
		`query($login:String!,$n:Int!){ user(login:$login){ projectV2(number:$n){ id title } } }`,
		map[string]interface{}{"login": owner.Login, "n": private.Number})
	data, _ := env["data"].(map[string]interface{})
	userNode, _ := data["user"].(map[string]interface{})
	if userNode["projectV2"] != nil {
		t.Fatalf("a stranger resolved the private project by number: %v", userNode["projectV2"])
	}
}

// --- Item content and field values -----------------------------------------

// TestProjectsV2GraphQL_ItemContentAndFieldValues covers what
// `gh project item-list` reads: the content union that carries the title, and
// the fieldValues connection that carries the columns.
func TestProjectsV2GraphQL_ItemContentAndFieldValues(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	org, project := s.seedProjectV2Org(t, "pv2-content-org", "Board")
	admin := s.store.UsersByLogin["admin"]
	repo := s.store.CreateOrgRepo(org, admin, "pv2-content-repo", "", false)
	issue := s.store.CreateIssue(repo.ID, admin.ID, "Ship the thing", "body", nil, nil, 0)

	item := s.store.ProjectsV2.AddItem(project.ID, "Issue", issue.ID, admin.ID)
	draft := s.store.ProjectsV2.AddDraftItem(project.ID, "A draft idea", "draft body", admin.ID)
	status := s.store.ProjectsV2.FieldByNameOnProject(project.ID, "Status")
	if status == nil {
		t.Fatal("the seeded Status field is missing")
	}
	if err := s.store.ProjectsV2.SetFieldValueAny(item.ID, status.ID, status.Options[0].ID); err != nil {
		t.Fatalf("SetFieldValueAny: %v", err)
	}

	data := s.gqlData(t, `query($login:String!){
		organization(login:$login){ projectV2(number:1){
			items(first:10){
				totalCount
				nodes{
					id type isArchived
					content{
						__typename
						... on Issue { title number }
						... on DraftIssue { title body }
					}
					fieldValues(first:10){ nodes{
						... on ProjectV2ItemFieldSingleSelectValue { name optionId }
					} }
				}
			}
		} }
	}`, map[string]interface{}{"login": org.Login})

	items := data["organization"].(map[string]interface{})["projectV2"].(map[string]interface{})["items"].(map[string]interface{})
	if got := int(items["totalCount"].(float64)); got != 2 {
		t.Fatalf("items.totalCount = %d, want 2", got)
	}
	byID := map[string]map[string]interface{}{}
	for _, raw := range items["nodes"].([]interface{}) {
		node := raw.(map[string]interface{})
		byID[node["id"].(string)] = node
	}

	issueItem := byID[item.NodeID]
	if issueItem == nil {
		t.Fatalf("the issue item is missing from %v", items["nodes"])
	}
	if issueItem["type"] != "ISSUE" || issueItem["isArchived"] != false {
		t.Errorf("issue item type/isArchived = %v / %v", issueItem["type"], issueItem["isArchived"])
	}
	content := issueItem["content"].(map[string]interface{})
	if content["__typename"] != "Issue" || content["title"] != "Ship the thing" {
		t.Errorf("issue item content = %v", content)
	}
	values := issueItem["fieldValues"].(map[string]interface{})["nodes"].([]interface{})
	sawStatus := false
	for _, raw := range values {
		if value := raw.(map[string]interface{}); value["name"] == status.Options[0].Name {
			sawStatus = true
		}
	}
	if !sawStatus {
		t.Errorf("fieldValues = %v, want the Status value that was set", values)
	}

	draftItem := byID[draft.NodeID]
	if draftItem == nil {
		t.Fatalf("the draft item is missing from %v", items["nodes"])
	}
	if draftItem["type"] != "DRAFT_ISSUE" {
		t.Errorf("draft item type = %v, want DRAFT_ISSUE", draftItem["type"])
	}
	draftContent := draftItem["content"].(map[string]interface{})
	if draftContent["__typename"] != "DraftIssue" || draftContent["title"] != "A draft idea" {
		t.Errorf("draft content = %v", draftContent)
	}
}

// --- Mutations -------------------------------------------------------------

// TestProjectsV2GraphQL_ProjectLifecycleMutations walks a project through the
// metadata, template and delete mutations and checks each is reflected in the
// store rather than only in its own payload.
func TestProjectsV2GraphQL_ProjectLifecycleMutations(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	_, project := s.seedProjectV2Org(t, "pv2-lifecycle-org", "Before")

	data := s.gqlData(t, `mutation($id:ID!){
		updateProjectV2(input:{projectId:$id,title:"After",shortDescription:"desc",readme:"# readme",public:true,closed:true}){
			projectV2{ title shortDescription readme public closed closedAt }
		}
	}`, map[string]interface{}{"id": project.NodeID})
	updated := data["updateProjectV2"].(map[string]interface{})["projectV2"].(map[string]interface{})
	if updated["title"] != "After" || updated["shortDescription"] != "desc" || updated["readme"] != "# readme" {
		t.Fatalf("updateProjectV2 payload = %v", updated)
	}
	if updated["public"] != true || updated["closed"] != true {
		t.Fatalf("updateProjectV2 flags = %v", updated)
	}
	if updated["closedAt"] == nil {
		t.Error("closing a project left closedAt null")
	}
	stored := s.store.ProjectsV2.GetProject(project.ID)
	if stored.Title != "After" || !stored.Closed || !stored.Public {
		t.Fatalf("the store did not record the update: %+v", stored)
	}

	data = s.gqlData(t, `mutation($id:ID!){ markProjectV2AsTemplate(input:{projectId:$id}){ projectV2{ template } } }`,
		map[string]interface{}{"id": project.NodeID})
	if data["markProjectV2AsTemplate"].(map[string]interface{})["projectV2"].(map[string]interface{})["template"] != true {
		t.Error("markProjectV2AsTemplate did not set template")
	}
	data = s.gqlData(t, `mutation($id:ID!){ unmarkProjectV2AsTemplate(input:{projectId:$id}){ projectV2{ template } } }`,
		map[string]interface{}{"id": project.NodeID})
	if data["unmarkProjectV2AsTemplate"].(map[string]interface{})["projectV2"].(map[string]interface{})["template"] != false {
		t.Error("unmarkProjectV2AsTemplate did not clear template")
	}

	s.gqlData(t, `mutation($id:ID!){ deleteProjectV2(input:{projectId:$id}){ projectV2{ id } } }`,
		map[string]interface{}{"id": project.NodeID})
	if s.store.ProjectsV2.GetProject(project.ID) != nil {
		t.Error("deleteProjectV2 left the project in the store")
	}
}

// TestProjectsV2GraphQL_CopyCarriesFieldsAndRemapsOptionIDs pins that a copied
// project is usable: its fields are its own, and an item's single-select value
// points at an option that exists on the copy rather than dangling at the
// source's option id.
func TestProjectsV2GraphQL_CopyCarriesFieldsAndRemapsOptionIDs(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	org, project := s.seedProjectV2Org(t, "pv2-copy-org", "Template")
	admin := s.store.UsersByLogin["admin"]

	draft := s.store.ProjectsV2.AddDraftItem(project.ID, "carried", "", admin.ID)
	status := s.store.ProjectsV2.FieldByNameOnProject(project.ID, "Status")
	if err := s.store.ProjectsV2.SetFieldValueAny(draft.ID, status.ID, status.Options[1].ID); err != nil {
		t.Fatalf("SetFieldValueAny: %v", err)
	}

	orgRecord := s.store.GetOrg(org.Login)
	data := s.gqlData(t, `mutation($id:ID!,$owner:ID!){
		copyProjectV2(input:{projectId:$id,ownerId:$owner,title:"A copy",includeDraftIssues:true}){
			projectV2{ id number title fields(first:50){ totalCount } items(first:10){ totalCount } }
		}
	}`, map[string]interface{}{"id": project.NodeID, "owner": orgRecord.NodeID})
	copied := data["copyProjectV2"].(map[string]interface{})["projectV2"].(map[string]interface{})
	if copied["title"] != "A copy" {
		t.Fatalf("copy payload = %v", copied)
	}
	if got, want := int(copied["fields"].(map[string]interface{})["totalCount"].(float64)), len(projectV2SeededFieldNames(t, s, project.ID)); got != want {
		t.Errorf("the copy carries %d fields, want %d", got, want)
	}
	if got := int(copied["items"].(map[string]interface{})["totalCount"].(float64)); got != 1 {
		t.Errorf("the copy carries %d items, want the one draft", got)
	}

	copiedProject := s.store.ProjectsV2.LookupProjectByNodeID(copied["id"].(string))
	copiedStatus := s.store.ProjectsV2.FieldByNameOnProject(copiedProject.ID, "Status")
	if copiedStatus == nil || copiedStatus.ID == status.ID {
		t.Fatalf("the copy shares the source's Status field: %v", copiedStatus)
	}
	copiedItems := s.store.ProjectsV2.ListItemsForProject(copiedProject.ID)
	if len(copiedItems) != 1 {
		t.Fatalf("the copy has %d items", len(copiedItems))
	}
	value := copiedItems[0].FieldValues[copiedStatus.ID]
	if value == nil {
		t.Fatalf("the copied item lost its Status value: %+v", copiedItems[0].FieldValues)
	}
	// The remap is the point: the id must be one of the copy's own options.
	found := false
	for _, option := range copiedStatus.Options {
		if option.ID == value.OptionID {
			found = true
		}
	}
	if !found {
		t.Errorf("the copied value points at option %q, which is not on the copy's field", value.OptionID)
	}
}

// TestProjectsV2GraphQL_ItemLifecycleMutations covers the drafts, archival,
// reorder, clear and conversion mutations as one sequence.
func TestProjectsV2GraphQL_ItemLifecycleMutations(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	org, project := s.seedProjectV2Org(t, "pv2-items-org", "Items")
	admin := s.store.UsersByLogin["admin"]
	repo := s.store.CreateOrgRepo(org, admin, "pv2-items-repo", "", false)

	data := s.gqlData(t, `mutation($id:ID!){
		addProjectV2DraftIssue(input:{projectId:$id,title:"draft one",body:"b"}){ projectItem{ id type } }
	}`, map[string]interface{}{"id": project.NodeID})
	first := data["addProjectV2DraftIssue"].(map[string]interface{})["projectItem"].(map[string]interface{})
	if first["type"] != "DRAFT_ISSUE" {
		t.Fatalf("addProjectV2DraftIssue = %v", first)
	}
	firstID := first["id"].(string)

	data = s.gqlData(t, `mutation($id:ID!){
		addProjectV2DraftIssue(input:{projectId:$id,title:"draft two"}){ projectItem{ id } }
	}`, map[string]interface{}{"id": project.NodeID})
	secondID := data["addProjectV2DraftIssue"].(map[string]interface{})["projectItem"].(map[string]interface{})["id"].(string)

	// updateProjectV2DraftIssue edits the draft in place.
	data = s.gqlData(t, `mutation($id:ID!){
		updateProjectV2DraftIssue(input:{draftIssueId:$id,title:"draft one renamed",body:"new body"}){
			draftIssue{ title body }
		}
	}`, map[string]interface{}{"id": firstID})
	draft := data["updateProjectV2DraftIssue"].(map[string]interface{})["draftIssue"].(map[string]interface{})
	if draft["title"] != "draft one renamed" || draft["body"] != "new body" {
		t.Fatalf("updateProjectV2DraftIssue = %v", draft)
	}

	// Reorder: moving the second draft to the head puts it first.
	s.gqlData(t, `mutation($p:ID!,$item:ID!){
		updateProjectV2ItemPosition(input:{projectId:$p,itemId:$item}){ items(first:10){ nodes{ id } } }
	}`, map[string]interface{}{"p": project.NodeID, "item": secondID})
	ordered := s.store.ProjectsV2.ListItemsForProject(project.ID)
	if len(ordered) != 2 || ordered[0].NodeID != secondID {
		t.Fatalf("after the reorder the order is %v, want %s first", itemNodeIDs(ordered), secondID)
	}

	// Archive / unarchive.
	data = s.gqlData(t, `mutation($p:ID!,$item:ID!){
		archiveProjectV2Item(input:{projectId:$p,itemId:$item}){ item{ id isArchived } }
	}`, map[string]interface{}{"p": project.NodeID, "item": firstID})
	if data["archiveProjectV2Item"].(map[string]interface{})["item"].(map[string]interface{})["isArchived"] != true {
		t.Error("archiveProjectV2Item did not archive the item")
	}
	data = s.gqlData(t, `mutation($p:ID!,$item:ID!){
		unarchiveProjectV2Item(input:{projectId:$p,itemId:$item}){ item{ id isArchived } }
	}`, map[string]interface{}{"p": project.NodeID, "item": firstID})
	if data["unarchiveProjectV2Item"].(map[string]interface{})["item"].(map[string]interface{})["isArchived"] != false {
		t.Error("unarchiveProjectV2Item did not restore the item")
	}

	// Set then clear a field value.
	status := s.store.ProjectsV2.FieldByNameOnProject(project.ID, "Status")
	s.gqlData(t, `mutation($p:ID!,$item:ID!,$f:ID!,$opt:String!){
		updateProjectV2ItemFieldValue(input:{projectId:$p,itemId:$item,fieldId:$f,value:{singleSelectOptionId:$opt}}){
			projectV2Item{ id fieldValueByName(name:"Status"){ ... on ProjectV2ItemFieldSingleSelectValue { name } } }
		}
	}`, map[string]interface{}{"p": project.NodeID, "item": firstID, "f": status.NodeID, "opt": status.Options[0].ID})
	item := s.store.ProjectsV2.LookupItemByNodeID(firstID)
	if item.FieldValues[status.ID] == nil {
		t.Fatal("updateProjectV2ItemFieldValue did not store the value")
	}
	s.gqlData(t, `mutation($p:ID!,$item:ID!,$f:ID!){
		clearProjectV2ItemFieldValue(input:{projectId:$p,itemId:$item,fieldId:$f}){ projectV2Item{ id } }
	}`, map[string]interface{}{"p": project.NodeID, "item": firstID, "f": status.NodeID})
	if s.store.ProjectsV2.LookupItemByNodeID(firstID).FieldValues[status.ID] != nil {
		t.Error("clearProjectV2ItemFieldValue left the value in place")
	}

	// Convert the draft into a real issue in the repository.
	repoRecord := s.store.GetRepoByID(repo.ID)
	data = s.gqlData(t, `mutation($item:ID!,$repo:ID!){
		convertProjectV2DraftIssueItemToIssue(input:{itemId:$item,repositoryId:$repo}){
			item{ id type content{ __typename ... on Issue { title } } }
		}
	}`, map[string]interface{}{"item": firstID, "repo": repoRecord.NodeID})
	converted := data["convertProjectV2DraftIssueItemToIssue"].(map[string]interface{})["item"].(map[string]interface{})
	if converted["type"] != "ISSUE" {
		t.Fatalf("conversion left the item as %v", converted["type"])
	}
	content := converted["content"].(map[string]interface{})
	if content["__typename"] != "Issue" || content["title"] != "draft one renamed" {
		t.Fatalf("converted content = %v", content)
	}
}

// TestProjectsV2GraphQL_FieldViewStatusAndWorkflowMutations covers the
// remaining subject families in one pass.
func TestProjectsV2GraphQL_FieldViewStatusAndWorkflowMutations(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	org, project := s.seedProjectV2Org(t, "pv2-shape-org", "Shape")

	// A single-select field, then an edit that renames it and keeps the option
	// identity of the option whose name did not change.
	data := s.gqlData(t, `mutation($p:ID!){
		createProjectV2Field(input:{projectId:$p,dataType:SINGLE_SELECT,name:"Priority",
			singleSelectOptions:[{name:"High",color:RED,description:"first"},{name:"Low",color:GRAY,description:""}]}){
			projectV2Field{ ... on ProjectV2SingleSelectField { id name options{ id name color description } } }
		}
	}`, map[string]interface{}{"p": project.NodeID})
	field := data["createProjectV2Field"].(map[string]interface{})["projectV2Field"].(map[string]interface{})
	options := field["options"].([]interface{})
	if len(options) != 2 {
		t.Fatalf("createProjectV2Field options = %v", options)
	}
	highID := options[0].(map[string]interface{})["id"].(string)
	if options[0].(map[string]interface{})["color"] != "RED" || options[0].(map[string]interface{})["description"] != "first" {
		t.Errorf("option colour/description were dropped: %v", options[0])
	}

	data = s.gqlData(t, `mutation($f:ID!){
		updateProjectV2Field(input:{fieldId:$f,name:"Urgency",
			singleSelectOptions:[{name:"High",color:RED,description:"first"},{name:"Later",color:BLUE,description:""}]}){
			projectV2Field{ ... on ProjectV2SingleSelectField { name options{ id name } } }
		}
	}`, map[string]interface{}{"f": field["id"]})
	edited := data["updateProjectV2Field"].(map[string]interface{})["projectV2Field"].(map[string]interface{})
	if edited["name"] != "Urgency" {
		t.Errorf("updateProjectV2Field name = %v", edited["name"])
	}
	editedOptions := edited["options"].([]interface{})
	if editedOptions[0].(map[string]interface{})["id"] != highID {
		t.Errorf("the surviving option was re-minted: %v, want id %s", editedOptions[0], highID)
	}

	// Views.
	data = s.gqlData(t, `mutation($p:ID!){
		createProjectV2View(input:{projectId:$p,name:"Board",layout:BOARD_LAYOUT}){
			projectV2View{ id name layout number }
		}
	}`, map[string]interface{}{"p": project.NodeID})
	view := data["createProjectV2View"].(map[string]interface{})["projectV2View"].(map[string]interface{})
	if view["layout"] != "BOARD_LAYOUT" || view["name"] != "Board" {
		t.Fatalf("createProjectV2View = %v", view)
	}
	data = s.gqlData(t, `mutation($v:ID!){
		updateProjectV2View(input:{viewId:$v,name:"Renamed",layout:TABLE_LAYOUT,filter:"is:issue"}){
			projectV2View{ name layout filter }
		}
	}`, map[string]interface{}{"v": view["id"]})
	updatedView := data["updateProjectV2View"].(map[string]interface{})["projectV2View"].(map[string]interface{})
	if updatedView["name"] != "Renamed" || updatedView["layout"] != "TABLE_LAYOUT" || updatedView["filter"] != "is:issue" {
		t.Fatalf("updateProjectV2View = %v", updatedView)
	}
	s.gqlData(t, `mutation($v:ID!){ deleteProjectV2View(input:{viewId:$v}){ projectV2View{ id } } }`,
		map[string]interface{}{"v": view["id"]})
	if s.store.ProjectsV2.LookupViewByNodeID(view["id"].(string)) != nil {
		t.Error("deleteProjectV2View left the view in the store")
	}

	// Status updates.
	data = s.gqlData(t, `mutation($p:ID!){
		createProjectV2StatusUpdate(input:{projectId:$p,body:"going well",status:ON_TRACK,startDate:"2026-01-05",targetDate:"2026-02-05"}){
			statusUpdate{ id body status startDate targetDate creator{ login } }
		}
	}`, map[string]interface{}{"p": project.NodeID})
	update := data["createProjectV2StatusUpdate"].(map[string]interface{})["statusUpdate"].(map[string]interface{})
	if update["status"] != "ON_TRACK" || update["body"] != "going well" ||
		update["startDate"] != "2026-01-05" || update["targetDate"] != "2026-02-05" {
		t.Fatalf("createProjectV2StatusUpdate = %v", update)
	}
	data = s.gqlData(t, `mutation($u:ID!){
		updateProjectV2StatusUpdate(input:{statusUpdateId:$u,status:AT_RISK,body:"slipping"}){
			statusUpdate{ status body }
		}
	}`, map[string]interface{}{"u": update["id"]})
	editedUpdate := data["updateProjectV2StatusUpdate"].(map[string]interface{})["statusUpdate"].(map[string]interface{})
	if editedUpdate["status"] != "AT_RISK" || editedUpdate["body"] != "slipping" {
		t.Fatalf("updateProjectV2StatusUpdate = %v", editedUpdate)
	}
	// The connection reads them back.
	data = s.gqlData(t, `query($login:String!){
		organization(login:$login){ projectV2(number:1){ statusUpdates(first:10){ totalCount nodes{ id status } } } }
	}`, map[string]interface{}{"login": org.Login})
	statusUpdates := data["organization"].(map[string]interface{})["projectV2"].(map[string]interface{})["statusUpdates"].(map[string]interface{})
	if int(statusUpdates["totalCount"].(float64)) != 1 {
		t.Fatalf("statusUpdates = %v", statusUpdates)
	}
	s.gqlData(t, `mutation($u:ID!){ deleteProjectV2StatusUpdate(input:{statusUpdateId:$u}){ deletedStatusUpdateId } }`,
		map[string]interface{}{"u": update["id"]})
	if len(s.store.ProjectsV2.StatusUpdatesForProject(project.ID)) != 0 {
		t.Error("deleteProjectV2StatusUpdate left the status update in the store")
	}

	// Workflows: a project is seeded with GitHub's defaults, and one can be
	// deleted.
	workflows := s.store.ProjectsV2.WorkflowsForProject(project.ID)
	if len(workflows) == 0 {
		t.Fatal("the project was seeded without workflows")
	}
	s.gqlData(t, `mutation($w:ID!){ deleteProjectV2Workflow(input:{workflowId:$w}){ deletedWorkflowId } }`,
		map[string]interface{}{"w": workflows[0].NodeID})
	if got := len(s.store.ProjectsV2.WorkflowsForProject(project.ID)); got != len(workflows)-1 {
		t.Errorf("workflows after the delete = %d, want %d", got, len(workflows)-1)
	}
}

// TestProjectsV2GraphQL_LinksAndCollaborators covers the repository/team links
// and the collaborator grants, including that the link connections read back.
func TestProjectsV2GraphQL_LinksAndCollaborators(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	org, project := s.seedProjectV2Org(t, "pv2-links-org", "Links")
	admin := s.store.UsersByLogin["admin"]
	repo := s.store.CreateOrgRepo(org, admin, "pv2-links-repo", "", false)
	team := s.store.CreateTeam(org.Login, "pv2-links-team", store.TeamOptions{})
	if repo == nil || team == nil {
		t.Fatal("could not seed the repository and team")
	}
	repoRecord := s.store.GetRepoByID(repo.ID)

	s.gqlData(t, `mutation($p:ID!,$r:ID!){ linkProjectV2ToRepository(input:{projectId:$p,repositoryId:$r}){ repository{ name } } }`,
		map[string]interface{}{"p": project.NodeID, "r": repoRecord.NodeID})
	s.gqlData(t, `mutation($p:ID!,$t:ID!){ linkProjectV2ToTeam(input:{projectId:$p,teamId:$t}){ team{ slug } } }`,
		map[string]interface{}{"p": project.NodeID, "t": team.NodeID})

	data := s.gqlData(t, `query($login:String!){
		organization(login:$login){ projectV2(number:1){
			repositories(first:10){ totalCount nodes{ name } }
			teams(first:10){ totalCount nodes{ slug } }
		} }
	}`, map[string]interface{}{"login": org.Login})
	node := data["organization"].(map[string]interface{})["projectV2"].(map[string]interface{})
	repos := node["repositories"].(map[string]interface{})
	if int(repos["totalCount"].(float64)) != 1 ||
		repos["nodes"].([]interface{})[0].(map[string]interface{})["name"] != "pv2-links-repo" {
		t.Fatalf("linked repositories = %v", repos)
	}
	teams := node["teams"].(map[string]interface{})
	if int(teams["totalCount"].(float64)) != 1 ||
		teams["nodes"].([]interface{})[0].(map[string]interface{})["slug"] != team.Slug {
		t.Fatalf("linked teams = %v", teams)
	}

	// Unlinking removes them again — the link lists are sets, so this is not
	// merely additive.
	s.gqlData(t, `mutation($p:ID!,$r:ID!){ unlinkProjectV2FromRepository(input:{projectId:$p,repositoryId:$r}){ repository{ name } } }`,
		map[string]interface{}{"p": project.NodeID, "r": repoRecord.NodeID})
	s.gqlData(t, `mutation($p:ID!,$t:ID!){ unlinkProjectV2FromTeam(input:{projectId:$p,teamId:$t}){ team{ slug } } }`,
		map[string]interface{}{"p": project.NodeID, "t": team.NodeID})
	stored := s.store.ProjectsV2.GetProject(project.ID)
	if len(stored.LinkedRepoIDs) != 0 || len(stored.LinkedTeamIDs) != 0 {
		t.Fatalf("unlink left %v / %v", stored.LinkedRepoIDs, stored.LinkedTeamIDs)
	}

	// Collaborators: a grant, then its revocation through role NONE.
	collaborator, _ := s.seedProjectV2User(t, "pv2-links-collaborator")
	data = s.gqlData(t, `mutation($p:ID!,$u:ID!){
		updateProjectV2Collaborators(input:{projectId:$p,collaborators:[{userId:$u,role:WRITER}]}){
			collaborators(first:10){ totalCount nodes{ ... on User { login } } }
		}
	}`, map[string]interface{}{"p": project.NodeID, "u": collaborator.NodeID})
	actors := data["updateProjectV2Collaborators"].(map[string]interface{})["collaborators"].(map[string]interface{})
	if int(actors["totalCount"].(float64)) != 1 {
		t.Fatalf("collaborators = %v", actors)
	}
	if got := s.store.ProjectsV2.CollaboratorRole(project.ID, collaborator.ID); got != "WRITER" {
		t.Errorf("stored collaborator role = %q, want WRITER", got)
	}
	s.gqlData(t, `mutation($p:ID!,$u:ID!){
		updateProjectV2Collaborators(input:{projectId:$p,collaborators:[{userId:$u,role:NONE}]}){
			collaborators(first:10){ totalCount }
		}
	}`, map[string]interface{}{"p": project.NodeID, "u": collaborator.NodeID})
	if got := s.store.ProjectsV2.CollaboratorRole(project.ID, collaborator.ID); got != "" {
		t.Errorf("role NONE left the grant in place: %q", got)
	}
}

// --- Webhooks --------------------------------------------------------------

// TestProjectsV2Webhooks_DeliverTheEventFamily pins that the three event names
// are delivered to the owning organization's hooks with GitHub's payload
// shapes. A hook subscriber is the only way to observe a project change from
// outside, so an event that is not emitted is a feature that silently is not
// there.
func TestProjectsV2Webhooks_DeliverTheEventFamily(t *testing.T) {
	t.Parallel()

	type delivery struct {
		event   string
		payload map[string]interface{}
	}
	var mu sync.Mutex
	var seen []delivery
	receiver := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var payload map[string]interface{}
		_ = json.Unmarshal(body, &payload)
		mu.Lock()
		seen = append(seen, delivery{event: r.Header.Get("X-GitHub-Event"), payload: payload})
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer receiver.Close()

	s := newIsolatedServer(t)
	org, project := s.seedProjectV2Org(t, "pv2-hooks-org", "Hooked")
	s.store.CreateOrgHook(org.Login, receiver.URL, "", "json", "0",
		projectV2WebhookEvents(), true)

	// One event of each name: a project edit, an item add, a status update.
	s.gqlData(t, `mutation($id:ID!){ updateProjectV2(input:{projectId:$id,title:"Renamed"}){ projectV2{ id } } }`,
		map[string]interface{}{"id": project.NodeID})
	s.gqlData(t, `mutation($id:ID!){ addProjectV2DraftIssue(input:{projectId:$id,title:"hooked draft"}){ projectItem{ id } } }`,
		map[string]interface{}{"id": project.NodeID})
	s.gqlData(t, `mutation($id:ID!){ createProjectV2StatusUpdate(input:{projectId:$id,body:"fine",status:ON_TRACK}){ statusUpdate{ id } } }`,
		map[string]interface{}{"id": project.NodeID})

	byEvent := map[string]map[string]interface{}{}
	testutil.TestEventually(20*time.Second, 20*time.Millisecond, func() bool {
		mu.Lock()
		defer mu.Unlock()
		byEvent = map[string]map[string]interface{}{}
		for _, d := range seen {
			byEvent[d.event] = d.payload
		}
		for _, name := range projectV2WebhookEvents() {
			if byEvent[name] == nil {
				return false
			}
		}
		return true
	})
	for _, name := range projectV2WebhookEvents() {
		if byEvent[name] == nil {
			mu.Lock()
			arrived := len(seen)
			mu.Unlock()
			t.Fatalf("no %s delivery arrived (%d deliveries seen)", name, arrived)
		}
	}

	// projects_v2 — the project object, its owner and the sender.
	projectPayload := byEvent[store.ProjectV2EventProject]
	if projectPayload["action"] != "edited" {
		t.Errorf("projects_v2 action = %v, want edited", projectPayload["action"])
	}
	projectObject := projectPayload["projects_v2"].(map[string]interface{})
	if projectObject["node_id"] != project.NodeID || projectObject["title"] != "Renamed" {
		t.Errorf("projects_v2 object = %v", projectObject)
	}
	if projectObject["number"] != float64(project.Number) {
		t.Errorf("projects_v2 number = %v, want %d", projectObject["number"], project.Number)
	}
	owner := projectObject["owner"].(map[string]interface{})
	if owner["login"] != org.Login {
		t.Errorf("projects_v2 owner = %v", owner)
	}
	if projectPayload["organization"] == nil || projectPayload["sender"] == nil {
		t.Errorf("projects_v2 payload is missing organization/sender: %v", projectPayload)
	}
	changes, _ := projectPayload["changes"].(map[string]interface{})
	title, _ := changes["title"].(map[string]interface{})
	if title == nil || title["from"] != "Hooked" || title["to"] != "Renamed" {
		t.Errorf("projects_v2 changes = %v, want a title from/to pair", changes)
	}

	// projects_v2_item — the item, naming its project by node id.
	itemPayload := byEvent[store.ProjectV2EventItem]
	if itemPayload["action"] != "created" {
		t.Errorf("projects_v2_item action = %v, want created", itemPayload["action"])
	}
	itemObject := itemPayload["projects_v2_item"].(map[string]interface{})
	if itemObject["project_node_id"] != project.NodeID {
		t.Errorf("projects_v2_item project_node_id = %v", itemObject["project_node_id"])
	}
	if itemObject["content_type"] != "DraftIssue" {
		t.Errorf("projects_v2_item content_type = %v, want DraftIssue", itemObject["content_type"])
	}
	if itemObject["archived_at"] != nil {
		t.Errorf("a fresh item reported archived_at = %v, want null", itemObject["archived_at"])
	}

	// projects_v2_status_update — the update, naming its project.
	statusPayload := byEvent[store.ProjectV2EventStatusUpdate]
	if statusPayload["action"] != "created" {
		t.Errorf("projects_v2_status_update action = %v, want created", statusPayload["action"])
	}
	statusObject := statusPayload["projects_v2_status_update"].(map[string]interface{})
	if statusObject["project_node_id"] != project.NodeID || statusObject["status"] != "ON_TRACK" {
		t.Errorf("projects_v2_status_update object = %v", statusObject)
	}
	if statusObject["body"] != "fine" {
		t.Errorf("projects_v2_status_update body = %v", statusObject["body"])
	}
}

// TestProjectsV2Webhooks_RESTAndGraphQLAgree pins that the REST write paths
// emit the same event family the GraphQL mutations do. A client watching hooks
// must not be able to tell which surface made a change.
func TestProjectsV2Webhooks_RESTAndGraphQLAgree(t *testing.T) {
	t.Parallel()

	var mu sync.Mutex
	var actions []string
	receiver := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var payload map[string]interface{}
		_ = json.Unmarshal(body, &payload)
		if r.Header.Get("X-GitHub-Event") == store.ProjectV2EventItem {
			mu.Lock()
			actions = append(actions, fmt.Sprint(payload["action"]))
			mu.Unlock()
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer receiver.Close()

	s := newIsolatedServer(t)
	org, project := s.seedProjectV2Org(t, "pv2-resthooks-org", "REST hooks")
	s.store.CreateOrgHook(org.Login, receiver.URL, "", "json", "0",
		projectV2WebhookEvents(), true)

	base := "/api/v3/orgs/" + org.Login + "/projectsV2/" + fmt.Sprint(project.Number)
	resp := s.post(t, base+"/drafts", defaultToken, map[string]interface{}{"title": "rest draft"})
	if resp.StatusCode != http.StatusCreated {
		resp.Body.Close()
		t.Fatalf("create draft = %d, want 201", resp.StatusCode)
	}
	created := decodeJSON(t, resp)
	itemID := int(created["id"].(float64))

	resp = s.delete(t, base+"/items/"+fmt.Sprint(itemID), defaultToken)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("delete item = %d, want 204", resp.StatusCode)
	}

	testutil.TestEventually(20*time.Second, 20*time.Millisecond, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(actions) >= 2
	})
	mu.Lock()
	defer mu.Unlock()
	if len(actions) < 2 || actions[0] != "created" || actions[1] != "deleted" {
		t.Fatalf("REST item webhook actions = %v, want [created deleted]", actions)
	}
}

// --- helpers ---------------------------------------------------------------

func itemNodeIDs(items []*store.ProjectV2Item) []string {
	out := make([]string, 0, len(items))
	for _, it := range items {
		out = append(out, it.NodeID)
	}
	return out
}

// projectV2SeededFieldNames reports the fields a freshly created project
// carries, measured off the project itself rather than restated here — so a
// change to what GitHub seeds moves the assertions with it instead of
// falsifying them.
func projectV2SeededFieldNames(t *testing.T, s *isolatedServer, projectID int) []string {
	t.Helper()
	fields := s.store.ProjectsV2.FieldsForProject(projectID)
	names := make([]string, 0, len(fields))
	for _, field := range fields {
		names = append(names, field.Name)
	}
	if len(names) == 0 {
		t.Fatal("a created project carries no fields; the seed did not run")
	}
	return names
}

// projectV2WebhookEvents is the projects_v2 family, named here so the tests
// enumerate it without the production package carrying a list only they read.
func projectV2WebhookEvents() []string {
	return []string{
		store.ProjectV2EventProject,
		store.ProjectV2EventItem,
		store.ProjectV2EventStatusUpdate,
	}
}

// seedProjectV2User creates a user with a projects-scoped token, for the cases
// that need an account other than the seeded admin.
func (s *isolatedServer) seedProjectV2User(t *testing.T, login string) (*store.User, string) {
	t.Helper()
	st := s.store
	st.Mu.Lock()
	u := &store.User{
		ID:        st.NextUser,
		NodeID:    fmt.Sprintf("U_pv2surface%08d", st.NextUser),
		Login:     login,
		Type:      "User",
		CreatedAt: fixedTestTime.UTC(),
		UpdatedAt: fixedTestTime.UTC(),
	}
	st.Users[u.ID] = u
	st.UsersByLogin[u.Login] = u
	st.NextUser++
	st.Mu.Unlock()

	token := st.CreateToken(u.ID, "repo, project")
	if token == nil {
		t.Fatalf("could not mint a token for %s", login)
	}
	return u, token.Value
}

// gqlDoAs / gqlDataAs run a GraphQL document as a specific credential, which
// the visibility cases need: the shared helpers always use the admin token.
func (s *isolatedServer) gqlDoAs(t *testing.T, token, query string, variables map[string]interface{}) map[string]interface{} {
	t.Helper()
	body := map[string]interface{}{"query": query}
	if variables != nil {
		body["variables"] = variables
	}
	resp := s.post(t, "/api/graphql", token, body)
	return decodeJSON(t, resp)
}

func (s *isolatedServer) gqlDataAs(t *testing.T, token, query string, variables map[string]interface{}) map[string]interface{} {
	t.Helper()
	env := s.gqlDoAs(t, token, query, variables)
	if errs, ok := env["errors"]; ok && errs != nil {
		t.Fatalf("graphql errors: %v", errs)
	}
	data, _ := env["data"].(map[string]interface{})
	return data
}

// TestProjectsV2GraphQL_ZeroPageSizeIsAnEmptyWindow pins the page-size
// boundary `gh project create` depends on. gh selects the shared project
// fragment with `items(first: 0)` and `fields(first: 0)` because it wants
// neither, so a server that rejects zero — or that quietly serves a default
// page instead — refuses or over-serves the command. GitHub answers with the
// connection's metadata and no nodes.
func TestProjectsV2GraphQL_ZeroPageSizeIsAnEmptyWindow(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	org, project := s.seedProjectV2Org(t, "pv2-zero-org", "Zero")
	admin := s.store.UsersByLogin["admin"]
	s.store.ProjectsV2.AddDraftItem(project.ID, "an item", "", admin.ID)

	data := s.gqlData(t, `query($login:String!){
		organization(login:$login){ projectV2(number:1){
			items(first:0){ totalCount nodes{ id } }
			fields(first:0){ totalCount nodes{ ... on ProjectV2FieldCommon { id } } }
		} }
	}`, map[string]interface{}{"login": org.Login})

	node := data["organization"].(map[string]interface{})["projectV2"].(map[string]interface{})
	items := node["items"].(map[string]interface{})
	if got := int(items["totalCount"].(float64)); got != 1 {
		t.Errorf("items.totalCount = %d, want 1 — first:0 must not hide the count", got)
	}
	if nodes := items["nodes"].([]interface{}); len(nodes) != 0 {
		t.Errorf("items(first:0).nodes = %v, want an empty list", nodes)
	}
	fields := node["fields"].(map[string]interface{})
	if got, want := int(fields["totalCount"].(float64)), len(projectV2SeededFieldNames(t, s, project.ID)); got != want {
		t.Errorf("fields.totalCount = %d, want %d", got, want)
	}
	if nodes := fields["nodes"].([]interface{}); len(nodes) != 0 {
		t.Errorf("fields(first:0).nodes = %v, want an empty list", nodes)
	}
}

// TestProjectsV2GraphQL_PageSizeBoundsStillRefuseOutOfRange keeps the guard
// that zero now passes from admitting everything: a negative or oversized page
// is still refused.
func TestProjectsV2GraphQL_PageSizeBoundsStillRefuseOutOfRange(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	org, _ := s.seedProjectV2Org(t, "pv2-bounds-org", "Bounds")

	for _, size := range []int{-1, 101} {
		env := s.gqlDo(t, `query($login:String!,$n:Int!){
			organization(login:$login){ projectsV2(first:$n){ totalCount } }
		}`, map[string]interface{}{"login": org.Login, "n": size})
		errs, _ := env["errors"].([]interface{})
		if len(errs) == 0 {
			t.Errorf("first:%d was accepted: %v", size, env)
		}
	}
}

// TestProjectsV2GraphQL_BuiltInFieldValuesReadTheContent covers the half of
// the ProjectV2ItemFieldValue union whose value is not stored on the item: the
// built-in columns read the issue the item points at. `gh project item-list`
// selects every member of the union in one fragment, so these have to resolve
// as well as exist.
func TestProjectsV2GraphQL_BuiltInFieldValuesReadTheContent(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	org, project := s.seedProjectV2Org(t, "pv2-builtin-org", "Built-ins")
	admin := s.store.UsersByLogin["admin"]
	repo := s.store.CreateOrgRepo(org, admin, "pv2-builtin-repo", "", false)
	label := s.store.CreateLabel(repo.ID, "needs-triage", "Triage me", "d73a4a")
	milestone := s.store.CreateMilestone(repo.ID, admin.ID, "v1", "", "open", nil)
	if label == nil || milestone == nil {
		t.Fatal("could not seed the label and milestone")
	}
	issue := s.store.CreateIssue(repo.ID, admin.ID, "Built-in columns", "",
		[]int{label.ID}, []int{admin.ID}, milestone.ID)
	if issue == nil {
		t.Fatal("could not seed the issue")
	}
	item := s.store.ProjectsV2.AddItem(project.ID, "Issue", issue.ID, admin.ID)

	data := s.gqlData(t, `query($login:String!){
		organization(login:$login){ projectV2(number:1){ items(first:10){ nodes{
			id
			fieldValues(first:50){ nodes{
				__typename
				... on ProjectV2ItemFieldLabelValue { labels(first:10){ nodes{ name } } field{ ... on ProjectV2FieldCommon { name } } }
				... on ProjectV2ItemFieldUserValue { users(first:10){ nodes{ login } } }
				... on ProjectV2ItemFieldRepositoryValue { repository{ name } }
				... on ProjectV2ItemFieldMilestoneValue { milestone{ title } }
			} }
		} } } }
	}`, map[string]interface{}{"login": org.Login})

	nodes := data["organization"].(map[string]interface{})["projectV2"].(map[string]interface{})["items"].(map[string]interface{})["nodes"].([]interface{})
	var itemNode map[string]interface{}
	for _, raw := range nodes {
		if node := raw.(map[string]interface{}); node["id"] == item.NodeID {
			itemNode = node
		}
	}
	if itemNode == nil {
		t.Fatalf("the item is missing from %v", nodes)
	}
	byType := map[string]map[string]interface{}{}
	for _, raw := range itemNode["fieldValues"].(map[string]interface{})["nodes"].([]interface{}) {
		value := raw.(map[string]interface{})
		byType[value["__typename"].(string)] = value
	}

	labels := byType["ProjectV2ItemFieldLabelValue"]
	if labels == nil {
		t.Fatalf("no label value in %v", byType)
	}
	labelNodes := labels["labels"].(map[string]interface{})["nodes"].([]interface{})
	if len(labelNodes) != 1 || labelNodes[0].(map[string]interface{})["name"] != "needs-triage" {
		t.Errorf("label value = %v", labelNodes)
	}
	// The value names the field it belongs to, which is what the common
	// interface promises.
	field := labels["field"].(map[string]interface{})
	if field["name"] != "Labels" {
		t.Errorf("label value field = %v, want the Labels column", field)
	}

	users := byType["ProjectV2ItemFieldUserValue"]
	if users == nil {
		t.Fatalf("no assignee value in %v", byType)
	}
	userNodes := users["users"].(map[string]interface{})["nodes"].([]interface{})
	if len(userNodes) != 1 || userNodes[0].(map[string]interface{})["login"] != admin.Login {
		t.Errorf("assignee value = %v", userNodes)
	}

	repository := byType["ProjectV2ItemFieldRepositoryValue"]
	if repository == nil || repository["repository"].(map[string]interface{})["name"] != "pv2-builtin-repo" {
		t.Errorf("repository value = %v", repository)
	}

	milestoneValue := byType["ProjectV2ItemFieldMilestoneValue"]
	if milestoneValue == nil || milestoneValue["milestone"].(map[string]interface{})["title"] != "v1" {
		t.Errorf("milestone value = %v", milestoneValue)
	}
}

// TestProjectsV2GraphQL_GitHubCLIProjectFragmentValidates pins the document
// `gh project` actually sends.
//
// gh builds one shared fragment naming every member of the
// ProjectV2ItemFieldValue union and both content types, and a GraphQL document
// is validated against the whole schema before any resolver runs — so a single
// missing member or field failed *every* `gh project` subcommand, not just the
// ones that read it. That is how the CLI harness reported twelve identical
// failures for one absent field. Keeping the fragment here means the next
// regression is caught by `go test` rather than only by the container harness.
func TestProjectsV2GraphQL_GitHubCLIProjectFragmentValidates(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	org, project := s.seedProjectV2Org(t, "pv2-cli-org", "CLI shape")
	admin := s.store.UsersByLogin["admin"]
	repo := s.store.CreateOrgRepo(org, admin, "pv2-cli-repo", "", false)
	issue := s.store.CreateIssue(repo.ID, admin.ID, "reachable by the CLI", "", nil, nil, 0)
	s.store.ProjectsV2.AddItem(project.ID, "Issue", issue.ID, admin.ID)
	s.store.ProjectsV2.AddDraftItem(project.ID, "a draft", "body", admin.ID)

	// gh's shared project fragment, with the item and field-value fragments it
	// selects. Field-for-field what `gh project view`/`item-list` send.
	const ghProjectQuery = `query OrgProject($login:String!,$number:Int!,$firstItems:Int!,$firstFields:Int!){
	  owner: organization(login:$login){
	    projectV2(number:$number){
	      number url shortDescription public closed title id readme
	      items(first:$firstItems){
	        totalCount
	        pageInfo{ hasNextPage endCursor }
	        nodes{
	          id
	          type
	          isArchived
	          content{
	            __typename
	            ... on Issue { title body number state url repository{ nameWithOwner } }
	            ... on PullRequest { title body number state url repository{ nameWithOwner } }
	            ... on DraftIssue { title body }
	          }
	          fieldValues(first:$firstFields){
	            nodes{
	              __typename
	              ... on ProjectV2ItemFieldDateValue { date field{ ... on ProjectV2FieldCommon { name } } }
	              ... on ProjectV2ItemFieldIterationValue { title startDate duration field{ ... on ProjectV2FieldCommon { name } } }
	              ... on ProjectV2ItemFieldLabelValue { labels(first:10){ nodes{ name } } field{ ... on ProjectV2FieldCommon { name } } }
	              ... on ProjectV2ItemFieldNumberValue { number field{ ... on ProjectV2FieldCommon { name } } }
	              ... on ProjectV2ItemFieldSingleSelectValue { name optionId field{ ... on ProjectV2FieldCommon { name } } }
	              ... on ProjectV2ItemFieldTextValue { text field{ ... on ProjectV2FieldCommon { name } } }
	              ... on ProjectV2ItemFieldMilestoneValue { milestone{ title } field{ ... on ProjectV2FieldCommon { name } } }
	              ... on ProjectV2ItemFieldPullRequestValue { pullRequests(first:10){ nodes{ url } } field{ ... on ProjectV2FieldCommon { name } } }
	              ... on ProjectV2ItemFieldRepositoryValue { repository{ url } field{ ... on ProjectV2FieldCommon { name } } }
	              ... on ProjectV2ItemFieldUserValue { users(first:10){ nodes{ login } } field{ ... on ProjectV2FieldCommon { name } } }
	              ... on ProjectV2ItemFieldReviewerValue { reviewers(first:10){ nodes{ ... on User { login } } } field{ ... on ProjectV2FieldCommon { name } } }
	              ... on ProjectV2ItemFieldMultiSelectValue { value options{ name } field{ ... on ProjectV2FieldCommon { name } } }
	            }
	            totalCount
	          }
	        }
	      }
	      fields(first:$firstFields){
	        totalCount
	        pageInfo{ hasNextPage endCursor }
	        nodes{
	          __typename
	          ... on ProjectV2Field { id name dataType }
	          ... on ProjectV2SingleSelectField { id name dataType options{ id name } }
	          ... on ProjectV2IterationField { id name dataType configuration{ duration startDay iterations{ id title startDate duration } completedIterations{ id title } } }
	        }
	      }
	      owner{ __typename ... on User { login } ... on Organization { login } }
	    }
	  }
	}`

	data := s.gqlData(t, ghProjectQuery, map[string]interface{}{
		"login": org.Login, "number": project.Number,
		"firstItems": 30, "firstFields": 30,
	})
	node := data["owner"].(map[string]interface{})["projectV2"].(map[string]interface{})
	if node["title"] != "CLI shape" || node["id"] != project.NodeID {
		t.Fatalf("project node = %v", node)
	}
	if int(node["items"].(map[string]interface{})["totalCount"].(float64)) != 2 {
		t.Errorf("items.totalCount = %v, want 2", node["items"])
	}
	if node["owner"].(map[string]interface{})["login"] != org.Login {
		t.Errorf("owner = %v", node["owner"])
	}
	// gh renders each item's title out of the content union, so a content
	// member that resolves without a title makes `gh project item-list` print
	// blank rows even though the items are there.
	titles := map[string]string{}
	for _, raw := range node["items"].(map[string]interface{})["nodes"].([]interface{}) {
		content, _ := raw.(map[string]interface{})["content"].(map[string]interface{})
		if content == nil {
			t.Fatalf("an item resolved a null content: %v", raw)
		}
		title, _ := content["title"].(string)
		if title == "" {
			t.Errorf("item content has no title: %v", content)
		}
		titles[content["__typename"].(string)] = title
	}
	if titles["DraftIssue"] != "a draft" {
		t.Errorf("draft content title = %q, want %q", titles["DraftIssue"], "a draft")
	}
	if titles["Issue"] != "reachable by the CLI" {
		t.Errorf("issue content title = %q, want %q", titles["Issue"], "reachable by the CLI")
	}

	// The same fragment with the zero page sizes `gh project create` sends.
	zero := s.gqlData(t, ghProjectQuery, map[string]interface{}{
		"login": org.Login, "number": project.Number,
		"firstItems": 0, "firstFields": 0,
	})
	zeroNode := zero["owner"].(map[string]interface{})["projectV2"].(map[string]interface{})
	if nodes := zeroNode["items"].(map[string]interface{})["nodes"].([]interface{}); len(nodes) != 0 {
		t.Errorf("items(first:0) returned %d nodes, want none", len(nodes))
	}
}

// TestProjectsV2GraphQL_MultiSelectFieldValueRoundTrips covers the one field
// data type whose value is a set rather than a scalar: it is written through
// `multiSelectOptionIds` and read back as the options plus GitHub's
// comma-joined `value` string.
func TestProjectsV2GraphQL_MultiSelectFieldValueRoundTrips(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	org, project := s.seedProjectV2Org(t, "pv2-multi-org", "Multi")
	admin := s.store.UsersByLogin["admin"]
	item := s.store.ProjectsV2.AddDraftItem(project.ID, "tagged", "", admin.ID)

	data := s.gqlData(t, `mutation($p:ID!){
		createProjectV2Field(input:{projectId:$p,dataType:MULTI_SELECT,name:"Platforms",
			singleSelectOptions:[{name:"Linux",color:BLUE,description:"penguins"},{name:"macOS",color:GRAY,description:""},{name:"Windows",color:PURPLE,description:""}]}){
			projectV2Field{ ... on ProjectV2FieldCommon { id name dataType } }
		}
	}`, map[string]interface{}{"p": project.NodeID})
	field := data["createProjectV2Field"].(map[string]interface{})["projectV2Field"].(map[string]interface{})
	if field["dataType"] != "MULTI_SELECT" {
		t.Fatalf("createProjectV2Field dataType = %v", field["dataType"])
	}
	stored := s.store.ProjectsV2.FieldByNameOnProject(project.ID, "Platforms")
	if stored == nil || len(stored.Options) != 3 {
		t.Fatalf("stored multi-select field = %+v", stored)
	}

	s.gqlData(t, `mutation($p:ID!,$item:ID!,$f:ID!,$ids:[String!]){
		updateProjectV2ItemFieldValue(input:{projectId:$p,itemId:$item,fieldId:$f,value:{multiSelectOptionIds:$ids}}){
			projectV2Item{ id }
		}
	}`, map[string]interface{}{
		"p": project.NodeID, "item": item.NodeID, "f": stored.NodeID,
		"ids": []interface{}{stored.Options[0].ID, stored.Options[2].ID},
	})

	read := s.gqlData(t, `query($login:String!){
		organization(login:$login){ projectV2(number:1){ items(first:10){ nodes{
			id fieldValues(first:50){ nodes{
				__typename
				... on ProjectV2ItemFieldMultiSelectValue { value options{ id name color description } }
			} }
		} } } }
	}`, map[string]interface{}{"login": org.Login})
	nodes := read["organization"].(map[string]interface{})["projectV2"].(map[string]interface{})["items"].(map[string]interface{})["nodes"].([]interface{})
	var multi map[string]interface{}
	for _, raw := range nodes {
		for _, rawValue := range raw.(map[string]interface{})["fieldValues"].(map[string]interface{})["nodes"].([]interface{}) {
			if value := rawValue.(map[string]interface{}); value["__typename"] == "ProjectV2ItemFieldMultiSelectValue" {
				multi = value
			}
		}
	}
	if multi == nil {
		t.Fatalf("no multi-select value in %v", nodes)
	}
	if multi["value"] != "Linux, Windows" {
		t.Errorf("multi-select value = %v, want %q", multi["value"], "Linux, Windows")
	}
	options := multi["options"].([]interface{})
	if len(options) != 2 {
		t.Fatalf("multi-select options = %v, want 2", options)
	}
	first := options[0].(map[string]interface{})
	// The colour and description come from the field's option definition, not
	// from the item's stored value.
	if first["name"] != "Linux" || first["color"] != "BLUE" || first["description"] != "penguins" {
		t.Errorf("first selected option = %v", first)
	}

	// An option id that is not on the field is refused rather than stored.
	env := s.gqlDo(t, `mutation($p:ID!,$item:ID!,$f:ID!){
		updateProjectV2ItemFieldValue(input:{projectId:$p,itemId:$item,fieldId:$f,value:{multiSelectOptionIds:["nosuchoption"]}}){
			projectV2Item{ id }
		}
	}`, map[string]interface{}{"p": project.NodeID, "item": item.NodeID, "f": stored.NodeID})
	if errs, _ := env["errors"].([]interface{}); len(errs) == 0 {
		t.Errorf("an unknown option id was accepted: %v", env)
	}
}

// TestGraphQLResourceResolvesAPastedURL covers Query.resource, the lookup
// `gh project item-add --url` runs before it can add anything: without it the
// command has no way to turn the URL a person pasted into a node id, and fails
// however complete the Projects v2 surface is.
func TestGraphQLResourceResolvesAPastedURL(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	admin := s.store.UsersByLogin["admin"]
	repo := s.store.CreateRepo(admin, "resource-repo", "", false)
	issue := s.store.CreateIssue(repo.ID, admin.ID, "findable by URL", "", nil, nil, 0)

	const query = `query($url:URI!){ resource(url:$url){
		__typename resourcePath url
		... on Issue { id number title }
		... on Repository { id name }
	} }`

	data := s.gqlData(t, query, map[string]interface{}{
		"url": "https://bleephub.example/" + repo.FullName + "/issues/" + fmt.Sprint(issue.Number),
	})
	resource := data["resource"].(map[string]interface{})
	if resource["__typename"] != "Issue" || resource["id"] != issue.NodeID {
		t.Fatalf("issue URL resolved to %v", resource)
	}
	if want := "/" + repo.FullName + "/issues/" + fmt.Sprint(issue.Number); resource["resourcePath"] != want {
		t.Errorf("resourcePath = %v, want %q", resource["resourcePath"], want)
	}

	// A repository URL resolves to the repository.
	data = s.gqlData(t, query, map[string]interface{}{"url": "https://bleephub.example/" + repo.FullName})
	resource = data["resource"].(map[string]interface{})
	if resource["__typename"] != "Repository" || resource["name"] != "resource-repo" {
		t.Fatalf("repository URL resolved to %v", resource)
	}

	// Nothing that does not name a resource resolves.
	for _, raw := range []string{
		"https://bleephub.example/" + repo.FullName + "/issues/9999",
		"https://bleephub.example/" + repo.FullName + "/settings/hooks",
		"https://bleephub.example/nobody/nothing",
		"not a url at all",
	} {
		data = s.gqlData(t, query, map[string]interface{}{"url": raw})
		if data["resource"] != nil {
			t.Errorf("resource(%q) = %v, want null", raw, data["resource"])
		}
	}
}

// TestGraphQLResourceHidesUnreadableRepositories pins that the URL lookup is
// not an existence oracle: a private repository the caller cannot read answers
// the same null a repository that does not exist does.
func TestGraphQLResourceHidesUnreadableRepositories(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	owner, _ := s.seedProjectV2User(t, "resource-owner")
	_, strangerToken := s.seedProjectV2User(t, "resource-stranger")
	repo := s.store.CreateRepo(owner, "resource-secret", "", true)
	issue := s.store.CreateIssue(repo.ID, owner.ID, "not yours", "", nil, nil, 0)

	const query = `query($url:URI!){ resource(url:$url){ __typename ... on Issue { id } } }`
	vars := map[string]interface{}{
		"url": "https://bleephub.example/" + repo.FullName + "/issues/" + fmt.Sprint(issue.Number),
	}

	data := s.gqlDataAs(t, strangerToken, query, vars)
	if data["resource"] != nil {
		t.Fatalf("a stranger resolved a private repository's issue by URL: %v", data["resource"])
	}
}

// TestProjectsV2GraphQL_ItemsConnectionFiltersAndOrders covers the arguments
// ProjectV2.items declares beyond the Relay window: the archived-state filter
// (whose schema default is NOT_ARCHIVED, so filed-away work stays out of a
// listing that did not ask for it) and the free-text query.
func TestProjectsV2GraphQL_ItemsConnectionFiltersAndOrders(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	org, project := s.seedProjectV2Org(t, "pv2-filter-org", "Filters")
	admin := s.store.UsersByLogin["admin"]
	kept := s.store.ProjectsV2.AddDraftItem(project.ID, "still open", "", admin.ID)
	filed := s.store.ProjectsV2.AddDraftItem(project.ID, "filed away", "", admin.ID)
	if s.store.ProjectsV2.ArchiveItem(filed.ID, true) == nil {
		t.Fatal("could not archive the item")
	}

	itemsQuery := func(args string) []interface{} {
		t.Helper()
		data := s.gqlData(t, `query($login:String!){
			organization(login:$login){ projectV2(number:1){ items(`+args+`){ totalCount nodes{ id } } } }
		}`, map[string]interface{}{"login": org.Login})
		return data["organization"].(map[string]interface{})["projectV2"].(map[string]interface{})["items"].(map[string]interface{})["nodes"].([]interface{})
	}

	// The default excludes the archived item.
	nodes := itemsQuery("first:10")
	if len(nodes) != 1 || nodes[0].(map[string]interface{})["id"] != kept.NodeID {
		t.Fatalf("default items = %v, want only the unarchived one", nodes)
	}

	// Asking for archived items returns the archived one only.
	nodes = itemsQuery("first:10,archivedStates:[ARCHIVED]")
	if len(nodes) != 1 || nodes[0].(map[string]interface{})["id"] != filed.NodeID {
		t.Fatalf("archived items = %v, want only the archived one", nodes)
	}

	// Asking for both returns both.
	if nodes = itemsQuery("first:10,archivedStates:[ARCHIVED,NOT_ARCHIVED]"); len(nodes) != 2 {
		t.Fatalf("both states returned %d items, want 2", len(nodes))
	}

	// The free-text query matches the item's title.
	nodes = itemsQuery(`first:10,query:"still"`)
	if len(nodes) != 1 || nodes[0].(map[string]interface{})["id"] != kept.NodeID {
		t.Fatalf("query items = %v, want the matching one", nodes)
	}
	if nodes = itemsQuery(`first:10,query:"nothing matches this"`); len(nodes) != 0 {
		t.Fatalf("a non-matching query returned %v", nodes)
	}

	// An ordered fields connection sorts by the named field.
	data := s.gqlData(t, `query($login:String!){
		organization(login:$login){ projectV2(number:1){
			fields(first:50,orderBy:{field:NAME,direction:ASC}){ nodes{ ... on ProjectV2FieldCommon { name } } }
		} }
	}`, map[string]interface{}{"login": org.Login})
	fieldNodes := data["organization"].(map[string]interface{})["projectV2"].(map[string]interface{})["fields"].(map[string]interface{})["nodes"].([]interface{})
	names := make([]string, 0, len(fieldNodes))
	for _, raw := range fieldNodes {
		names = append(names, raw.(map[string]interface{})["name"].(string))
	}
	if !sort.SliceIsSorted(names, func(i, j int) bool { return strings.ToLower(names[i]) < strings.ToLower(names[j]) }) {
		t.Errorf("fields ordered by NAME ASC came back as %v", names)
	}
}
