package bleephub

import (
	"fmt"
	"testing"

	"github.com/e6qu/bleephub/internal/store"
)

// Repository-scoped issue mutations: the refusing caller (shared gqlAuthzFixture)
// has no access to the private repository. Organization-scoped definitions get
// their own fixture below, whose refuser owns a different organization.

var gqlIssueMutationCases = []gqlMutationCase{
	{
		name: "updateIssueComment",
		doc:  `mutation($input:UpdateIssueCommentInput!){updateIssueComment(input:$input){issueComment{body}}}`,
		input: func(f *gqlAuthzFixture) map[string]interface{} {
			return map[string]interface{}{"id": f.comment.NodeID, "body": "edited from the table"}
		},
	},
	{
		name: "deleteIssueComment",
		doc:  `mutation($input:DeleteIssueCommentInput!){deleteIssueComment(input:$input){clientMutationId}}`,
		input: func(f *gqlAuthzFixture) map[string]interface{} {
			return map[string]interface{}{"id": f.comment.NodeID}
		},
	},
	{
		name: "pinIssueComment",
		doc:  `mutation($input:PinIssueCommentInput!){pinIssueComment(input:$input){issueComment{id}}}`,
		input: func(f *gqlAuthzFixture) map[string]interface{} {
			return map[string]interface{}{"issueCommentId": f.comment.NodeID}
		},
	},
	{
		name: "unpinIssueComment",
		doc:  `mutation($input:UnpinIssueCommentInput!){unpinIssueComment(input:$input){issueComment{id}}}`,
		input: func(f *gqlAuthzFixture) map[string]interface{} {
			return map[string]interface{}{"issueCommentId": f.comment.NodeID}
		},
	},
	{
		name: "addAssigneesToAssignable",
		doc:  `mutation($input:AddAssigneesToAssignableInput!){addAssigneesToAssignable(input:$input){assignable{__typename}}}`,
		input: func(f *gqlAuthzFixture) map[string]interface{} {
			return map[string]interface{}{
				"assignableId": f.issue.NodeID,
				"assigneeIds":  []interface{}{f.owner.NodeID},
			}
		},
	},
	{
		name: "removeAssigneesFromAssignable",
		doc:  `mutation($input:RemoveAssigneesFromAssignableInput!){removeAssigneesFromAssignable(input:$input){assignable{__typename}}}`,
		input: func(f *gqlAuthzFixture) map[string]interface{} {
			return map[string]interface{}{
				"assignableId": f.issue.NodeID,
				"assigneeIds":  []interface{}{f.owner.NodeID},
			}
		},
	},
	{
		name: "replaceActorsForAssignable",
		doc:  `mutation($input:ReplaceActorsForAssignableInput!){replaceActorsForAssignable(input:$input){assignable{__typename}}}`,
		input: func(f *gqlAuthzFixture) map[string]interface{} {
			return map[string]interface{}{
				"assignableId": f.issue.NodeID,
				"actorIds":     []interface{}{f.owner.NodeID},
			}
		},
	},
	{
		name: "addSubIssue",
		doc:  `mutation($input:AddSubIssueInput!){addSubIssue(input:$input){issue{number} subIssue{number}}}`,
		setup: func(t *testing.T, s *isolatedServer, f *gqlAuthzFixture) {
			f.subIssue = s.store.CreateIssue(f.repo.ID, f.owner.ID, "child issue", "", nil, nil, 0)
			if f.subIssue == nil {
				t.Fatalf("could not seed the child issue")
			}
		},
		input: func(f *gqlAuthzFixture) map[string]interface{} {
			return map[string]interface{}{"issueId": f.issue.NodeID, "subIssueId": f.subIssue.NodeID}
		},
	},
	{
		name: "removeSubIssue",
		doc:  `mutation($input:RemoveSubIssueInput!){removeSubIssue(input:$input){issue{number}}}`,
		setup: func(t *testing.T, s *isolatedServer, f *gqlAuthzFixture) {
			f.subIssue = s.store.CreateIssue(f.repo.ID, f.owner.ID, "child issue", "", nil, nil, 0)
			if f.subIssue == nil {
				t.Fatalf("could not seed the child issue")
			}
			if err := s.store.AddSubIssue(f.issue.ID, f.subIssue.ID, false); err != nil {
				t.Fatalf("could not link the child issue: %v", err)
			}
		},
		input: func(f *gqlAuthzFixture) map[string]interface{} {
			return map[string]interface{}{"issueId": f.issue.NodeID, "subIssueId": f.subIssue.NodeID}
		},
	},
	{
		name: "reprioritizeSubIssue",
		doc:  `mutation($input:ReprioritizeSubIssueInput!){reprioritizeSubIssue(input:$input){issue{number}}}`,
		setup: func(t *testing.T, s *isolatedServer, f *gqlAuthzFixture) {
			f.subIssue = s.store.CreateIssue(f.repo.ID, f.owner.ID, "child issue", "", nil, nil, 0)
			if f.subIssue == nil {
				t.Fatalf("could not seed the child issue")
			}
			if err := s.store.AddSubIssue(f.issue.ID, f.subIssue.ID, false); err != nil {
				t.Fatalf("could not link the child issue: %v", err)
			}
		},
		input: func(f *gqlAuthzFixture) map[string]interface{} {
			return map[string]interface{}{"issueId": f.issue.NodeID, "subIssueId": f.subIssue.NodeID}
		},
	},
	{
		name: "addBlockedBy",
		doc:  `mutation($input:AddBlockedByInput!){addBlockedBy(input:$input){issue{number} blockingIssue{number}}}`,
		setup: func(t *testing.T, s *isolatedServer, f *gqlAuthzFixture) {
			f.subIssue = s.store.CreateIssue(f.repo.ID, f.owner.ID, "blocking issue", "", nil, nil, 0)
			if f.subIssue == nil {
				t.Fatalf("could not seed the blocking issue")
			}
		},
		input: func(f *gqlAuthzFixture) map[string]interface{} {
			return map[string]interface{}{"issueId": f.issue.NodeID, "blockingIssueId": f.subIssue.NodeID}
		},
	},
	{
		name: "removeBlockedBy",
		doc:  `mutation($input:RemoveBlockedByInput!){removeBlockedBy(input:$input){issue{number}}}`,
		setup: func(t *testing.T, s *isolatedServer, f *gqlAuthzFixture) {
			f.subIssue = s.store.CreateIssue(f.repo.ID, f.owner.ID, "blocking issue", "", nil, nil, 0)
			if f.subIssue == nil {
				t.Fatalf("could not seed the blocking issue")
			}
			s.store.AddIssueBlockedBy(f.issue.ID, f.subIssue.ID)
		},
		input: func(f *gqlAuthzFixture) map[string]interface{} {
			return map[string]interface{}{"issueId": f.issue.NodeID, "blockingIssueId": f.subIssue.NodeID}
		},
	},
	{
		name: "unmarkIssueAsDuplicate",
		doc:  `mutation($input:UnmarkIssueAsDuplicateInput!){unmarkIssueAsDuplicate(input:$input){duplicate{__typename}}}`,
		setup: func(t *testing.T, s *isolatedServer, f *gqlAuthzFixture) {
			f.subIssue = s.store.CreateIssue(f.repo.ID, f.owner.ID, "canonical issue", "", nil, nil, 0)
			if f.subIssue == nil {
				t.Fatalf("could not seed the canonical issue")
			}
			canonicalID := f.subIssue.ID
			s.store.UpdateIssue(f.issue.ID, func(i *store.Issue) { i.DuplicateOfID = canonicalID })
		},
		input: func(f *gqlAuthzFixture) map[string]interface{} {
			return map[string]interface{}{"canonicalId": f.subIssue.NodeID, "duplicateId": f.issue.NodeID}
		},
	},
	{
		name: "updateIssueIssueType",
		doc:  `mutation($input:UpdateIssueIssueTypeInput!){updateIssueIssueType(input:$input){issue{number}}}`,
		input: func(f *gqlAuthzFixture) map[string]interface{} {
			return map[string]interface{}{"issueId": f.issue.NodeID}
		},
	},
	{
		name: "applyPendingIssueSuggestions",
		doc:  `mutation($input:ApplyPendingIssueSuggestionsInput!){applyPendingIssueSuggestions(input:$input){issue{number}}}`,
		setup: func(t *testing.T, s *isolatedServer, f *gqlAuthzFixture) {
			seedLabelSuggestion(t, s, f)
		},
		input: func(f *gqlAuthzFixture) map[string]interface{} {
			return map[string]interface{}{
				"issueId": f.issue.NodeID,
				"actorId": f.owner.NodeID,
				"suggestions": []interface{}{map[string]interface{}{
					"kind": "LABEL", "labelId": f.label.NodeID,
				}},
			}
		},
	},
	{
		name: "rejectPendingIssueSuggestions",
		doc:  `mutation($input:RejectPendingIssueSuggestionsInput!){rejectPendingIssueSuggestions(input:$input){issue{number}}}`,
		setup: func(t *testing.T, s *isolatedServer, f *gqlAuthzFixture) {
			seedLabelSuggestion(t, s, f)
		},
		input: func(f *gqlAuthzFixture) map[string]interface{} {
			return map[string]interface{}{
				"issueId": f.issue.NodeID,
				"actorId": f.owner.NodeID,
				"suggestions": []interface{}{map[string]interface{}{
					"kind": "LABEL", "labelId": f.label.NodeID,
				}},
			}
		},
	},
}

func seedLabelSuggestion(t *testing.T, s *isolatedServer, f *gqlAuthzFixture) {
	t.Helper()
	labelID := f.label.ID
	actorID := f.owner.ID
	if s.store.CreateIssueSuggestion(f.repo.FullName, f.issue.ID, store.IssueSuggestion{
		Action:   "add_label",
		TargetID: &labelID,
		ActorID:  &actorID,
	}) == nil {
		t.Fatalf("could not seed the pending suggestion")
	}
}

func TestGraphQLIssueMutationsRefuseAnUnrelatedAuthenticatedUser(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	for _, tc := range gqlIssueMutationCases {
		f := newGQLAuthzFixture(t, s.Server, "issue-stranger-"+tc.name, true)
		if tc.setup != nil {
			tc.setup(t, s, f)
		}
		env := s.gqlAuthzPost(t, f.strangerToken, tc.doc, map[string]interface{}{"input": tc.input(f)})
		if len(gqlAuthzErrors(env)) == 0 {
			t.Errorf("%s: an account with no access to the repository was served: %v", tc.name, env)
		}
		s.assertGQLFixtureUntouched(t, tc.name, f)
		if comment := s.store.GetComment(f.comment.ID); comment == nil || comment.Body != "fixture comment" {
			t.Errorf("%s: the comment was edited or deleted by a stranger: %+v", tc.name, comment)
		}
		if issue := s.store.GetIssue(f.issue.ID); issue != nil && len(issue.AssigneeIDs) != 0 {
			t.Errorf("%s: the issue was assigned by a stranger: %v", tc.name, issue.AssigneeIDs)
		}
	}
}

func TestGraphQLIssueMutationsStillServeTheirEntitledCaller(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	for _, tc := range gqlIssueMutationCases {
		f := newGQLAuthzFixture(t, s.Server, "issue-owner-"+tc.name, true)
		if tc.setup != nil {
			tc.setup(t, s, f)
		}
		env := s.gqlAuthzPost(t, f.ownerToken, tc.doc, map[string]interface{}{"input": tc.input(f)})
		if errs := gqlAuthzErrors(env); len(errs) > 0 {
			t.Errorf("%s: the repository owner was refused: %v", tc.name, errs)
		}
	}
}

// --- organization-scoped: issue types and custom issue fields ---------------

// gqlIssueOrgFixture is an organization with a repository and an issue,
// alongside an account that owns a different organization — the caller whose
// refusal makes the entitlement cross-tenant rather than merely authenticated.
type gqlIssueOrgFixture struct {
	owner         *store.User
	ownerToken    string
	stranger      *store.User
	strangerToken string
	org           *store.Org
	repo          *store.Repo
	issue         *store.Issue
	issueType     *store.IssueType
	issueField    *store.IssueField
}

func newGQLIssueOrgFixture(t *testing.T, s *isolatedServer, tag string) *gqlIssueOrgFixture {
	t.Helper()
	st := s.store
	now := fixedTestTime.UTC()
	mkUser := func(login string) *store.User {
		st.Mu.Lock()
		defer st.Mu.Unlock()
		u := &store.User{
			ID: st.NextUser, NodeID: fmt.Sprintf("U_iorg%08d", st.NextUser),
			Login: login, Type: "User", CreatedAt: now, UpdatedAt: now,
		}
		st.Users[u.ID] = u
		st.UsersByLogin[u.Login] = u
		st.NextUser++
		return u
	}

	f := &gqlIssueOrgFixture{}
	f.owner = mkUser("gqliorg-owner-" + tag)
	f.stranger = mkUser("gqliorg-stranger-" + tag)
	f.org = st.CreateOrg(f.owner, "gqliorg-"+tag, "Issue Org", "")
	if f.org == nil {
		t.Fatalf("fixture %s: could not create the organization", tag)
	}
	// The stranger owns an organization of their own, so their refusal is
	// about this organization rather than about having no standing anywhere.
	if st.CreateOrg(f.stranger, "gqliorg-other-"+tag, "Other Org", "") == nil {
		t.Fatalf("fixture %s: could not create the second organization", tag)
	}
	f.repo = st.CreateOrgRepo(f.org, f.owner, "gqliorg-repo", "", false)
	if f.repo == nil {
		t.Fatalf("fixture %s: could not create the repository", tag)
	}
	f.issue = st.CreateIssue(f.repo.ID, f.owner.ID, "fixture issue", "", nil, nil, 0)
	if f.issue == nil {
		t.Fatalf("fixture %s: could not create the issue", tag)
	}
	enabled := "blue"
	f.issueType = st.CreateIssueType(f.org.Login, "Seeded Type", nil, &enabled, true)
	if f.issueType == nil {
		t.Fatalf("fixture %s: could not create the issue type", tag)
	}
	f.issueField = st.CreateIssueField(f.org.Login, "Seeded Field", nil, "text", "all", nil)
	if f.issueField == nil {
		t.Fatalf("fixture %s: could not create the issue field", tag)
	}

	ownerTok := st.CreateToken(f.owner.ID, "repo,admin:org")
	strangerTok := st.CreateToken(f.stranger.ID, "repo,admin:org")
	if ownerTok == nil || strangerTok == nil {
		t.Fatalf("fixture %s: could not mint tokens", tag)
	}
	f.ownerToken = ownerTok.Value
	f.strangerToken = strangerTok.Value
	return f
}

type gqlIssueOrgCase struct {
	name  string
	doc   string
	input func(f *gqlIssueOrgFixture) map[string]interface{}
}

var gqlIssueOrgMutationCases = []gqlIssueOrgCase{
	{
		name: "createIssueType",
		doc:  `mutation($input:CreateIssueTypeInput!){createIssueType(input:$input){issueType{name color}}}`,
		input: func(f *gqlIssueOrgFixture) map[string]interface{} {
			return map[string]interface{}{
				"ownerId": f.org.NodeID, "name": "Regression", "color": "RED", "isEnabled": true,
			}
		},
	},
	{
		name: "updateIssueType",
		doc:  `mutation($input:UpdateIssueTypeInput!){updateIssueType(input:$input){issueType{name}}}`,
		input: func(f *gqlIssueOrgFixture) map[string]interface{} {
			return map[string]interface{}{"issueTypeId": f.issueType.NodeID, "name": "Renamed Type"}
		},
	},
	{
		name: "deleteIssueType",
		doc:  `mutation($input:DeleteIssueTypeInput!){deleteIssueType(input:$input){deletedIssueTypeId}}`,
		input: func(f *gqlIssueOrgFixture) map[string]interface{} {
			return map[string]interface{}{"issueTypeId": f.issueType.NodeID}
		},
	},
	{
		name: "createIssueField",
		doc:  `mutation($input:CreateIssueFieldInput!){createIssueField(input:$input){issueField{__typename}}}`,
		input: func(f *gqlIssueOrgFixture) map[string]interface{} {
			return map[string]interface{}{
				"ownerId": f.org.NodeID, "name": "Severity", "dataType": "TEXT",
			}
		},
	},
	{
		name: "updateIssueField",
		doc:  `mutation($input:UpdateIssueFieldInput!){updateIssueField(input:$input){issueField{__typename}}}`,
		input: func(f *gqlIssueOrgFixture) map[string]interface{} {
			return map[string]interface{}{"id": f.issueField.NodeID, "name": "Renamed Field"}
		},
	},
	{
		name: "deleteIssueField",
		doc:  `mutation($input:DeleteIssueFieldInput!){deleteIssueField(input:$input){issueField{__typename}}}`,
		input: func(f *gqlIssueOrgFixture) map[string]interface{} {
			return map[string]interface{}{"fieldId": f.issueField.NodeID}
		},
	},
	{
		name: "createIssueFieldValue",
		doc:  `mutation($input:CreateIssueFieldValueInput!){createIssueFieldValue(input:$input){issue{number} issueFieldValue{__typename}}}`,
		input: func(f *gqlIssueOrgFixture) map[string]interface{} {
			return map[string]interface{}{
				"issueId":    f.issue.NodeID,
				"issueField": map[string]interface{}{"fieldId": f.issueField.NodeID, "textValue": "high"},
			}
		},
	},
	{
		name: "updateIssueFieldValue",
		doc:  `mutation($input:UpdateIssueFieldValueInput!){updateIssueFieldValue(input:$input){issue{number}}}`,
		input: func(f *gqlIssueOrgFixture) map[string]interface{} {
			return map[string]interface{}{
				"issueId":    f.issue.NodeID,
				"issueField": map[string]interface{}{"fieldId": f.issueField.NodeID, "textValue": "low"},
			}
		},
	},
	{
		name: "setIssueFieldValue",
		doc:  `mutation($input:SetIssueFieldValueInput!){setIssueFieldValue(input:$input){issue{number} issueFieldValues{__typename}}}`,
		input: func(f *gqlIssueOrgFixture) map[string]interface{} {
			return map[string]interface{}{
				"issueId": f.issue.NodeID,
				"issueFields": []interface{}{
					map[string]interface{}{"fieldId": f.issueField.NodeID, "textValue": "medium"},
				},
			}
		},
	},
	{
		name: "deleteIssueFieldValue",
		doc:  `mutation($input:DeleteIssueFieldValueInput!){deleteIssueFieldValue(input:$input){issue{number} success}}`,
		input: func(f *gqlIssueOrgFixture) map[string]interface{} {
			return map[string]interface{}{"issueId": f.issue.NodeID, "fieldId": f.issueField.NodeID}
		},
	},
}

func TestGraphQLIssueOrgMutationsRefuseAnotherOrganizationsOwner(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	for _, tc := range gqlIssueOrgMutationCases {
		f := newGQLIssueOrgFixture(t, s, "refuse-"+tc.name)
		env := s.gqlAuthzPost(t, f.strangerToken, tc.doc, map[string]interface{}{"input": tc.input(f)})
		if len(gqlAuthzErrors(env)) == 0 {
			t.Errorf("%s: the owner of another organization was served: %v", tc.name, env)
		}
		if types := s.store.ListIssueTypes(f.org.Login); len(types) != 1 || types[0].Name != "Seeded Type" {
			t.Errorf("%s: the organization's issue types were changed: %+v", tc.name, types)
		}
		if fields := s.store.ListIssueFields(f.org.Login); len(fields) != 1 || fields[0].Name != "Seeded Field" {
			t.Errorf("%s: the organization's issue fields were changed: %+v", tc.name, fields)
		}
		if values := s.store.ListIssueFieldValues(f.org.Login, f.issue.ID); len(values) != 0 {
			t.Errorf("%s: an issue field value was written: %+v", tc.name, values)
		}
	}
}

func TestGraphQLIssueOrgMutationsStillServeTheOrganizationOwner(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	for _, tc := range gqlIssueOrgMutationCases {
		f := newGQLIssueOrgFixture(t, s, "serve-"+tc.name)
		env := s.gqlAuthzPost(t, f.ownerToken, tc.doc, map[string]interface{}{"input": tc.input(f)})
		if errs := gqlAuthzErrors(env); len(errs) > 0 {
			t.Errorf("%s: the organization's owner was refused: %v", tc.name, errs)
		}
	}
}

// --- behavioural ------------------------------------------------------------

func TestGraphQLIssueCommentMutationsWriteTheStore(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	f := newGQLAuthzFixture(t, s.Server, "comment-crud", false)

	env := s.gqlAuthzPost(t, f.ownerToken,
		`mutation($input:UpdateIssueCommentInput!){updateIssueComment(input:$input){issueComment{body}}}`,
		map[string]interface{}{"input": map[string]interface{}{
			"id": f.comment.NodeID, "body": "a considered revision",
		}})
	if errs := gqlAuthzErrors(env); len(errs) > 0 {
		t.Fatalf("updateIssueComment: %v", errs)
	}
	comment := s.store.GetComment(f.comment.ID)
	if comment == nil || comment.Body != "a considered revision" {
		t.Fatalf("the comment body was not written: %+v", comment)
	}
	if comment.LastEditedAt == nil || comment.EditorID != f.owner.ID {
		t.Errorf("the edit was not attributed: %+v", comment)
	}

	env = s.gqlAuthzPost(t, f.ownerToken,
		`mutation($input:PinIssueCommentInput!){pinIssueComment(input:$input){issueComment{id}}}`,
		map[string]interface{}{"input": map[string]interface{}{"issueCommentId": f.comment.NodeID}})
	if errs := gqlAuthzErrors(env); len(errs) > 0 {
		t.Fatalf("pinIssueComment: %v", errs)
	}
	if comment := s.store.GetComment(f.comment.ID); comment == nil || !comment.Pinned {
		t.Errorf("pinIssueComment did not pin: %+v", comment)
	}
	env = s.gqlAuthzPost(t, f.ownerToken,
		`mutation($input:UnpinIssueCommentInput!){unpinIssueComment(input:$input){issueComment{id}}}`,
		map[string]interface{}{"input": map[string]interface{}{"issueCommentId": f.comment.NodeID}})
	if errs := gqlAuthzErrors(env); len(errs) > 0 {
		t.Fatalf("unpinIssueComment: %v", errs)
	}
	if comment := s.store.GetComment(f.comment.ID); comment == nil || comment.Pinned {
		t.Errorf("unpinIssueComment did not unpin: %+v", comment)
	}

	env = s.gqlAuthzPost(t, f.ownerToken,
		`mutation($input:DeleteIssueCommentInput!){deleteIssueComment(input:$input){clientMutationId}}`,
		map[string]interface{}{"input": map[string]interface{}{"id": f.comment.NodeID}})
	if errs := gqlAuthzErrors(env); len(errs) > 0 {
		t.Fatalf("deleteIssueComment: %v", errs)
	}
	if s.store.GetComment(f.comment.ID) != nil {
		t.Errorf("deleteIssueComment left the comment in the store")
	}
}

func TestGraphQLAssignmentMutationsWriteTheAssigneeSet(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	f := newGQLAuthzFixture(t, s.Server, "assignment", false)

	post := func(doc string, input map[string]interface{}) map[string]interface{} {
		t.Helper()
		env := s.gqlAuthzPost(t, f.ownerToken, doc, map[string]interface{}{"input": input})
		if errs := gqlAuthzErrors(env); len(errs) > 0 {
			t.Fatalf("%s: %v", doc, errs)
		}
		return env
	}

	post(`mutation($input:AddAssigneesToAssignableInput!){addAssigneesToAssignable(input:$input){assignable{__typename}}}`,
		map[string]interface{}{
			"assignableId": f.issue.NodeID,
			"assigneeIds":  []interface{}{f.owner.NodeID, f.stranger.NodeID},
		})
	issue := s.store.GetIssue(f.issue.ID)
	if issue == nil || len(issue.AssigneeIDs) != 2 {
		t.Fatalf("addAssigneesToAssignable did not assign both: %+v", issue)
	}

	post(`mutation($input:RemoveAssigneesFromAssignableInput!){removeAssigneesFromAssignable(input:$input){assignable{__typename}}}`,
		map[string]interface{}{
			"assignableId": f.issue.NodeID,
			"assigneeIds":  []interface{}{f.stranger.NodeID},
		})
	issue = s.store.GetIssue(f.issue.ID)
	if issue == nil || len(issue.AssigneeIDs) != 1 || issue.AssigneeIDs[0] != f.owner.ID {
		t.Fatalf("removeAssigneesFromAssignable did not remove exactly one: %+v", issue)
	}

	// replaceActorsForAssignable is a replace, not an add: naming only the
	// stranger leaves the owner off.
	post(`mutation($input:ReplaceActorsForAssignableInput!){replaceActorsForAssignable(input:$input){assignable{__typename}}}`,
		map[string]interface{}{
			"assignableId": f.issue.NodeID,
			"actorLogins":  []interface{}{f.stranger.Login},
		})
	issue = s.store.GetIssue(f.issue.ID)
	if issue == nil || len(issue.AssigneeIDs) != 1 || issue.AssigneeIDs[0] != f.stranger.ID {
		t.Fatalf("replaceActorsForAssignable did not replace the set: %+v", issue)
	}

	// A pull request is an Assignable too, and it goes through the pull
	// request's own update primitive.
	post(`mutation($input:AddAssigneesToAssignableInput!){addAssigneesToAssignable(input:$input){assignable{__typename}}}`,
		map[string]interface{}{
			"assignableId": f.pr.NodeID,
			"assigneeIds":  []interface{}{f.owner.NodeID},
		})
	if pr := s.store.GetPullRequest(f.pr.ID); pr == nil || len(pr.AssigneeIDs) != 1 {
		t.Fatalf("the pull request was not assigned: %+v", pr)
	}

	// An assignee offered with suggest: true is queued rather than assigned.
	post(`mutation($input:AddAssigneesToAssignableInput!){addAssigneesToAssignable(input:$input){assignable{__typename}}}`,
		map[string]interface{}{
			"assignableId": f.issue.NodeID,
			"assignees": []interface{}{map[string]interface{}{
				"actorId": f.owner.NodeID, "suggest": true,
			}},
		})
	issue = s.store.GetIssue(f.issue.ID)
	if issue == nil || len(issue.AssigneeIDs) != 1 || issue.AssigneeIDs[0] != f.stranger.ID {
		t.Errorf("a suggested assignee was applied: %+v", issue)
	}
	suggestions := s.store.ListIssueSuggestions(f.repo.FullName, f.issue.ID)
	if len(suggestions) != 1 || suggestions[0].Action != "add_assignee" {
		t.Errorf("the suggestion was not queued: %+v", suggestions)
	}
}

func TestGraphQLSubIssueAndDependencyMutations(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	f := newGQLAuthzFixture(t, s.Server, "sub-issues", false)
	child := s.store.CreateIssue(f.repo.ID, f.owner.ID, "child", "", nil, nil, 0)
	second := s.store.CreateIssue(f.repo.ID, f.owner.ID, "second child", "", nil, nil, 0)
	if child == nil || second == nil {
		t.Fatalf("could not seed the child issues")
	}

	post := func(doc string, input map[string]interface{}) {
		t.Helper()
		env := s.gqlAuthzPost(t, f.ownerToken, doc, map[string]interface{}{"input": input})
		if errs := gqlAuthzErrors(env); len(errs) > 0 {
			t.Fatalf("%s: %v", doc, errs)
		}
	}

	post(`mutation($input:AddSubIssueInput!){addSubIssue(input:$input){issue{number} subIssue{number}}}`,
		map[string]interface{}{"issueId": f.issue.NodeID, "subIssueId": child.NodeID})
	post(`mutation($input:AddSubIssueInput!){addSubIssue(input:$input){issue{number}}}`,
		map[string]interface{}{"issueId": f.issue.NodeID, "subIssueId": second.NodeID})
	if subs := s.store.ListSubIssues(f.issue.ID); len(subs) != 2 || subs[0] != child.ID {
		t.Fatalf("sub-issues = %v, want the two children in order", subs)
	}

	post(`mutation($input:ReprioritizeSubIssueInput!){reprioritizeSubIssue(input:$input){issue{number}}}`,
		map[string]interface{}{
			"issueId": f.issue.NodeID, "subIssueId": second.NodeID, "beforeId": child.NodeID,
		})
	if subs := s.store.ListSubIssues(f.issue.ID); len(subs) != 2 || subs[0] != second.ID {
		t.Fatalf("reprioritizeSubIssue did not reorder: %v", subs)
	}

	post(`mutation($input:RemoveSubIssueInput!){removeSubIssue(input:$input){issue{number}}}`,
		map[string]interface{}{"issueId": f.issue.NodeID, "subIssueId": second.NodeID})
	if subs := s.store.ListSubIssues(f.issue.ID); len(subs) != 1 || subs[0] != child.ID {
		t.Fatalf("removeSubIssue did not detach: %v", subs)
	}

	post(`mutation($input:AddBlockedByInput!){addBlockedBy(input:$input){issue{number} blockingIssue{number}}}`,
		map[string]interface{}{"issueId": f.issue.NodeID, "blockingIssueId": child.NodeID})
	if blockers := s.store.ListIssueBlockedBy(f.issue.ID); len(blockers) != 1 || blockers[0] != child.ID {
		t.Fatalf("addBlockedBy did not record the dependency: %v", blockers)
	}
	post(`mutation($input:RemoveBlockedByInput!){removeBlockedBy(input:$input){issue{number}}}`,
		map[string]interface{}{"issueId": f.issue.NodeID, "blockingIssueId": child.NodeID})
	if blockers := s.store.ListIssueBlockedBy(f.issue.ID); len(blockers) != 0 {
		t.Fatalf("removeBlockedBy left the dependency: %v", blockers)
	}
}

func TestGraphQLIssueTypeAndFieldMutationsWriteTheOrganization(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	f := newGQLIssueOrgFixture(t, s, "org-defs")

	post := func(doc string, input map[string]interface{}) map[string]interface{} {
		t.Helper()
		env := s.gqlAuthzPost(t, f.ownerToken, doc, map[string]interface{}{"input": input})
		if errs := gqlAuthzErrors(env); len(errs) > 0 {
			t.Fatalf("%s: %v", doc, errs)
		}
		return env
	}

	env := post(`mutation($input:CreateIssueTypeInput!){createIssueType(input:$input){issueType{id name color description}}}`,
		map[string]interface{}{
			"ownerId": f.org.NodeID, "name": "Incident",
			"color": "RED", "description": "unplanned work", "isEnabled": true,
		})
	nodeID := nestedString(t, env, "data", "createIssueType", "issueType", "id")
	created := store.FindIssueTypeByNodeID(s.store, nodeID)
	if created == nil || created.Name != "Incident" {
		t.Fatalf("createIssueType stored nothing: %+v", created)
	}
	if created.Color == nil || *created.Color != "red" {
		t.Errorf("colour = %v, want red", created.Color)
	}

	// The issue can then carry it, which is the repository-scoped half.
	post(`mutation($input:UpdateIssueIssueTypeInput!){updateIssueIssueType(input:$input){issue{number}}}`,
		map[string]interface{}{"issueId": f.issue.NodeID, "issueTypeId": nodeID})
	if issue := s.store.GetIssue(f.issue.ID); issue == nil || issue.IssueTypeID != created.ID {
		t.Fatalf("updateIssueIssueType did not set the type: %+v", issue)
	}
	// Naming no type clears it.
	post(`mutation($input:UpdateIssueIssueTypeInput!){updateIssueIssueType(input:$input){issue{number}}}`,
		map[string]interface{}{"issueId": f.issue.NodeID})
	if issue := s.store.GetIssue(f.issue.ID); issue == nil || issue.IssueTypeID != 0 {
		t.Fatalf("updateIssueIssueType did not clear the type: %+v", issue)
	}

	post(`mutation($input:UpdateIssueTypeInput!){updateIssueType(input:$input){issueType{name}}}`,
		map[string]interface{}{"issueTypeId": nodeID, "name": "Outage"})
	if updated := store.FindIssueTypeByNodeID(s.store, nodeID); updated == nil || updated.Name != "Outage" {
		t.Fatalf("updateIssueType did not rename: %+v", updated)
	}
	post(`mutation($input:DeleteIssueTypeInput!){deleteIssueType(input:$input){deletedIssueTypeId}}`,
		map[string]interface{}{"issueTypeId": nodeID})
	if store.FindIssueTypeByNodeID(s.store, nodeID) != nil {
		t.Errorf("deleteIssueType left the type in the store")
	}

	// Custom fields: create a single-select, put a value on the issue, then
	// clear it.
	env = post(`mutation($input:CreateIssueFieldInput!){createIssueField(input:$input){issueField{`+
		`... on IssueFieldSingleSelect{id name options{id name}}}}}`,
		map[string]interface{}{
			"ownerId": f.org.NodeID, "name": "Priority", "dataType": "SINGLE_SELECT",
			"options": []interface{}{
				map[string]interface{}{"name": "P0", "color": "RED", "priority": 1},
				map[string]interface{}{"name": "P1", "color": "ORANGE", "priority": 2},
			},
		})
	fieldNodeID := nestedString(t, env, "data", "createIssueField", "issueField", "id")
	fields := s.store.ListIssueFields(f.org.Login)
	var created2 *store.IssueField
	for _, field := range fields {
		if field.NodeID == fieldNodeID {
			created2 = field
		}
	}
	if created2 == nil || len(created2.Options) != 2 {
		t.Fatalf("createIssueField stored no single-select field: %+v", fields)
	}
	post(`mutation($input:SetIssueFieldValueInput!){setIssueFieldValue(input:$input){issue{number}}}`,
		map[string]interface{}{
			"issueId": f.issue.NodeID,
			"issueFields": []interface{}{map[string]interface{}{
				"fieldId":              fieldNodeID,
				"singleSelectOptionId": fmt.Sprintf("IFO_kwDO%08d", created2.Options[0].ID),
			}},
		})
	values := s.store.ListIssueFieldValues(f.org.Login, f.issue.ID)
	if len(values) != 1 {
		t.Fatalf("setIssueFieldValue wrote %d values, want 1: %+v", len(values), values)
	}

	post(`mutation($input:DeleteIssueFieldValueInput!){deleteIssueFieldValue(input:$input){success}}`,
		map[string]interface{}{"issueId": f.issue.NodeID, "fieldId": fieldNodeID})
	if values := s.store.ListIssueFieldValues(f.org.Login, f.issue.ID); len(values) != 0 {
		t.Fatalf("deleteIssueFieldValue left the value: %+v", values)
	}

	post(`mutation($input:DeleteIssueFieldInput!){deleteIssueField(input:$input){issueField{__typename}}}`,
		map[string]interface{}{"fieldId": fieldNodeID})
	for _, field := range s.store.ListIssueFields(f.org.Login) {
		if field.NodeID == fieldNodeID {
			t.Errorf("deleteIssueField left the definition in the store")
		}
	}
}

func TestGraphQLPendingIssueSuggestionsApplyAndReject(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	f := newGQLAuthzFixture(t, s.Server, "suggestions", false)
	seedLabelSuggestion(t, s, f)

	env := s.gqlAuthzPost(t, f.ownerToken,
		`mutation($input:ApplyPendingIssueSuggestionsInput!){applyPendingIssueSuggestions(input:$input){issue{number}}}`,
		map[string]interface{}{"input": map[string]interface{}{
			"issueId": f.issue.NodeID,
			"actorId": f.owner.NodeID,
			"suggestions": []interface{}{map[string]interface{}{
				"kind": "LABEL", "labelId": f.label.NodeID,
			}},
		}})
	if errs := gqlAuthzErrors(env); len(errs) > 0 {
		t.Fatalf("applyPendingIssueSuggestions: %v", errs)
	}
	issue := s.store.GetIssue(f.issue.ID)
	if issue == nil || len(issue.LabelIDs) != 1 || issue.LabelIDs[0] != f.label.ID {
		t.Fatalf("applying the suggestion did not label the issue: %+v", issue)
	}
	for _, suggestion := range s.store.ListIssueSuggestions(f.repo.FullName, f.issue.ID) {
		if suggestion.State != "approved" {
			t.Errorf("suggestion state = %q, want approved", suggestion.State)
		}
	}

	// A second, rejected suggestion leaves the issue alone.
	second := s.store.CreateLabel(f.repo.ID, "needs-design", "", "ffffff")
	if second == nil {
		t.Fatalf("could not seed the second label")
	}
	labelID := second.ID
	actorID := f.owner.ID
	s.store.CreateIssueSuggestion(f.repo.FullName, f.issue.ID, store.IssueSuggestion{
		Action: "add_label", TargetID: &labelID, ActorID: &actorID,
	})
	env = s.gqlAuthzPost(t, f.ownerToken,
		`mutation($input:RejectPendingIssueSuggestionsInput!){rejectPendingIssueSuggestions(input:$input){issue{number}}}`,
		map[string]interface{}{"input": map[string]interface{}{
			"issueId": f.issue.NodeID,
			"actorId": f.owner.NodeID,
			"suggestions": []interface{}{map[string]interface{}{
				"kind": "LABEL", "labelId": second.NodeID,
			}},
		}})
	if errs := gqlAuthzErrors(env); len(errs) > 0 {
		t.Fatalf("rejectPendingIssueSuggestions: %v", errs)
	}
	issue = s.store.GetIssue(f.issue.ID)
	if issue == nil || len(issue.LabelIDs) != 1 {
		t.Fatalf("rejecting a suggestion applied it: %+v", issue)
	}
}

func TestGraphQLUnmarkIssueAsDuplicateClearsTheRelation(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	f := newGQLAuthzFixture(t, s.Server, "duplicate", false)
	canonical := s.store.CreateIssue(f.repo.ID, f.owner.ID, "canonical", "", nil, nil, 0)
	if canonical == nil {
		t.Fatalf("could not seed the canonical issue")
	}
	canonicalID := canonical.ID
	s.store.UpdateIssue(f.issue.ID, func(i *store.Issue) { i.DuplicateOfID = canonicalID })

	// Naming the wrong canonical issue is refused rather than silently
	// clearing whatever relation the issue does have.
	other := s.store.CreateIssue(f.repo.ID, f.owner.ID, "unrelated", "", nil, nil, 0)
	env := s.gqlAuthzPost(t, f.ownerToken,
		`mutation($input:UnmarkIssueAsDuplicateInput!){unmarkIssueAsDuplicate(input:$input){duplicate{__typename}}}`,
		map[string]interface{}{"input": map[string]interface{}{
			"canonicalId": other.NodeID, "duplicateId": f.issue.NodeID,
		}})
	if len(gqlAuthzErrors(env)) == 0 {
		t.Errorf("unmarkIssueAsDuplicate accepted the wrong canonical issue")
	}
	if issue := s.store.GetIssue(f.issue.ID); issue == nil || issue.DuplicateOfID != canonicalID {
		t.Fatalf("the relation was cleared by the refused call: %+v", issue)
	}

	env = s.gqlAuthzPost(t, f.ownerToken,
		`mutation($input:UnmarkIssueAsDuplicateInput!){unmarkIssueAsDuplicate(input:$input){duplicate{__typename}}}`,
		map[string]interface{}{"input": map[string]interface{}{
			"canonicalId": canonical.NodeID, "duplicateId": f.issue.NodeID,
		}})
	if errs := gqlAuthzErrors(env); len(errs) > 0 {
		t.Fatalf("unmarkIssueAsDuplicate: %v", errs)
	}
	if issue := s.store.GetIssue(f.issue.ID); issue == nil || issue.DuplicateOfID != 0 {
		t.Fatalf("the duplicate relation was not cleared: %+v", issue)
	}
}
