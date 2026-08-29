package bleephub

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/e6qu/bleephub/internal/store"
	"github.com/graphql-go/graphql"
)

// Guards the authorization bypass where the GraphQL mutation surface
// authenticated callers but never checked access, letting a stranger mutate
// records they could not read. The table drives every mutation twice — a
// stranger who must be refused, and the owner who must still be served, since
// over-blocking is the recurring regression here.

// gqlAuthzFixture is a private repository with one of everything the mutation
// surface addresses, plus its owner and an unrelated account.
type gqlAuthzFixture struct {
	owner         *store.User
	ownerToken    string
	stranger      *store.User
	strangerToken string
	repo          *store.Repo
	repo2         *store.Repo // second repo under the same owner (transferIssue's destination)
	issue         *store.Issue
	comment       *store.Comment
	label         *store.IssueLabel
	milestone     *store.Milestone
	category      *store.DiscussionCategory
	discussion    *store.Discussion
	discComment   *store.DiscussionComment
	pr            *store.PullRequest
	threadNodeID  string
	reviewNodeID  string
	headSHA       string
	// Seeded by the updateTeamsRepository and deletePackageVersion setups: a
	// team grant needs an org-owned repo and a version needs a package.
	orgRepo              *store.Repo
	teamNodeID           string
	packageVersionNodeID string
	pollOptionNodeID     string
	enterpriseNodeID     string
	attributionOrgNodeID string
	mannequinNodeID      string
	// Seeded by the deleteLinkedBranch setup: an issue carries no linked
	// branch until something links one.
	linkedBranchNodeID string
	// Minted by the dismissRepositoryVulnerabilityAlert setup, not the fixture:
	// seeding an alert unconditionally would put a vulnerability on every repo.
	dependabotAlert *store.DependabotAlert
	// Seeded by the sub-issue, dependency and duplicate rows, which need a
	// second issue.
	subIssue *store.Issue
	// The seeded review thread's root comment, addressed by the PR comment
	// mutations.
	reviewCommentNodeID string
	// The org the custom-property and verifiable-domain rows create (those
	// records never belong to a user), plus the ruleset and domain the
	// update/delete rows address.
	propsOrg          *store.Org
	rulesetNodeID     string
	domainNodeID      string
	checkSuiteNodeID  string
	checkRunNodeID    string
	deploymentNodeID  string
	environmentNodeID string
	workflowRunNodeID string
	// Classic-project subjects seeded by the classic-project setups: a
	// repo-scoped board with two columns and a note card, and a user-owned
	// board for the repository-link mutations (which refuse repo-scoped boards).
	classicProject      *store.ProjectClassic
	classicColumn       *store.ProjectColumn
	classicColumn2      *store.ProjectColumn
	classicCard         *store.ProjectCard
	classicOwnerProject *store.ProjectClassic
}

// seedClassicProjectFixture arranges the repo-scoped classic board the
// classic-project mutation cases act on.
func seedClassicProjectFixture(t *testing.T, s *isolatedServer, f *gqlAuthzFixture) {
	t.Helper()
	st := s.store
	f.classicProject = st.CreateProjectClassic(f.repo, f.owner.ID, "classic board", "triage", "open")
	if f.classicProject == nil {
		t.Fatalf("could not seed the classic project")
	}
	f.classicColumn = st.CreateProjectColumn(f.classicProject.ID, "To do")
	f.classicColumn2 = st.CreateProjectColumn(f.classicProject.ID, "Done")
	f.classicCard = st.CreateProjectCard(f.classicColumn.ID, f.owner.ID, "a note", 0, 0)
	if f.classicColumn == nil || f.classicColumn2 == nil || f.classicCard == nil {
		t.Fatalf("could not seed the classic project's columns and card")
	}
}

// seedClassicOwnerProjectFixture arranges the private user-owned board the
// repository-link cases act on.
func seedClassicOwnerProjectFixture(t *testing.T, s *isolatedServer, f *gqlAuthzFixture) {
	t.Helper()
	f.classicOwnerProject = s.store.CreateProjectClassicForOwner("User", f.owner.Login, f.owner.ID, "owner board", "", false)
	if f.classicOwnerProject == nil {
		t.Fatalf("could not seed the user-owned classic project")
	}
}

func newGQLAuthzFixture(t *testing.T, srv *Server, tag string, private bool) *gqlAuthzFixture {
	t.Helper()
	st := srv.store
	now := fixedTestTime.UTC()

	mkUser := func(login string) *store.User {
		st.Mu.Lock()
		defer st.Mu.Unlock()
		u := &store.User{
			ID:        st.NextUser,
			NodeID:    fmt.Sprintf("U_authz%08d", st.NextUser),
			Login:     login,
			Type:      "User",
			CreatedAt: now,
			UpdatedAt: now,
		}
		st.Users[u.ID] = u
		st.UsersByLogin[u.Login] = u
		st.NextUser++
		return u
	}

	f := &gqlAuthzFixture{}
	f.owner = mkUser("gqlauthz-owner-" + tag)
	f.stranger = mkUser("gqlauthz-stranger-" + tag)

	f.repo = st.CreateRepo(f.owner, "gqlauthz-repo", "", private)
	if f.repo == nil {
		t.Fatalf("could not create the fixture repository for %s", tag)
	}
	f.repo2 = st.CreateRepo(f.owner, "gqlauthz-repo-two", "", private)
	if f.repo2 == nil {
		t.Fatalf("could not create the second fixture repository for %s", tag)
	}
	branches := seedPullRequestBranches(t, srv, f.repo, "feature", "spare")
	f.headSHA = branches["feature"]
	if f.headSHA == "" {
		t.Fatalf("fixture %s: feature branch has no head commit", tag)
	}

	f.issue = st.CreateIssue(f.repo.ID, f.owner.ID, "fixture issue", "body", nil, nil, 0)
	f.comment = st.CreateComment(f.issue.ID, f.owner.ID, "fixture comment")
	f.label = st.CreateLabel(f.repo.ID, "authz-label", "", "d73a4a")
	f.milestone = st.CreateMilestone(f.repo.ID, f.owner.ID, "authz milestone", "", "open", nil)
	f.category = st.CreateDiscussionCategory(f.repo.ID, "Q&A", "", "answers welcome", true)
	f.discussion = st.CreateDiscussion(f.repo.ID, f.category.ID, f.owner.ID, "fixture discussion", "body")
	f.discComment = st.CreateDiscussionComment(f.discussion.ID, f.owner.ID, "fixture answer", 0)
	f.pr = st.CreatePullRequest(f.repo.ID, f.owner.ID, "fixture pr", "body", "feature", "main", false, nil, nil, 0)
	if f.issue == nil || f.comment == nil || f.label == nil || f.milestone == nil ||
		f.category == nil || f.discussion == nil || f.discComment == nil || f.pr == nil {
		t.Fatalf("fixture %s: store refused to seed a record", tag)
	}
	root := st.PRReviewComments.CreateRootComment(f.pr.ID, f.owner.ID, "README.md", "nit", "", "RIGHT", 1, 0)
	if root == nil {
		t.Fatalf("fixture %s: could not seed a review thread", tag)
	}
	f.threadNodeID = store.PRReviewThreadNodeID(root.ID)
	f.reviewCommentNodeID = root.NodeID

	// A pending review owned by the repo owner, for the submit/dismiss cases.
	pendingReview := st.CreatePRReview(f.pr.ID, f.owner.ID, "PENDING", "pending body")
	if pendingReview == nil {
		t.Fatalf("fixture %s: could not seed a pending review", tag)
	}
	f.reviewNodeID = pendingReview.NodeID

	ownerTok := st.CreateToken(f.owner.ID, "repo")
	strangerTok := st.CreateToken(f.stranger.ID, "repo")
	if ownerTok == nil || strangerTok == nil {
		t.Fatalf("fixture %s: could not mint tokens", tag)
	}
	f.ownerToken = ownerTok.Value
	f.strangerToken = strangerTok.Value
	return f
}

func gqlAuthzErrors(env map[string]interface{}) []interface{} {
	errs, _ := env["errors"].([]interface{})
	return errs
}

// gqlMutationCase is one row of the mutation surface: the document a client
// sends and the input it carries against the fixture.
type gqlMutationCase struct {
	name  string
	doc   string
	input func(f *gqlAuthzFixture) map[string]interface{}
	// setup, when set, arranges preconditions the mutation's semantics demand
	// beyond the plain fixture (e.g. enablePullRequestAutoMerge is legal only
	// while something blocks the merge). Runs before the request in both tables.
	setup func(t *testing.T, s *isolatedServer, f *gqlAuthzFixture)
}

// gqlMutationCases covers every mutation that names an existing resource.
// createRepository is deliberately absent: it names no repository, and its own
// resolver is what decides which owner the caller may create under.
var gqlMutationCases = []gqlMutationCase{
	{
		name: "accessUserNamespaceRepository",
		doc:  `mutation($input:AccessUserNamespaceRepositoryInput!){accessUserNamespaceRepository(input:$input){expiresAt repository{name}}}`,
		setup: func(t *testing.T, s *isolatedServer, f *gqlAuthzFixture) {
			// The stranger holds no enterprise role, so the refusal is about
			// enterprise standing, not authentication.
			e := s.store.CreateEnterprise("authz-emu-"+f.owner.Login, "EMU", "billing@bleephub.invalid")
			if e == nil {
				t.Fatal("fixture enterprise could not be created")
			}
			if s.store.SetEnterpriseMembership(e.ID, f.owner.ID, store.EnterpriseRoleOwner) == nil {
				t.Fatal("fixture enterprise owner could not be enrolled")
			}
			if s.store.SetEnterpriseIdentityProvider(e.ID, "https://idp.invalid/sso", "issuer", "cert", "", "", nil) == nil {
				t.Fatal("fixture identity provider could not be bound")
			}
			f.enterpriseNodeID = e.NodeID
		},
		input: func(f *gqlAuthzFixture) map[string]interface{} {
			return map[string]interface{}{
				"enterpriseId": f.enterpriseNodeID,
				"repositoryId": f.repo.NodeID,
			}
		},
	},
	{
		name: "createAttributionInvitation",
		doc:  `mutation($input:CreateAttributionInvitationInput!){createAttributionInvitation(input:$input){source{... on Mannequin{login}}}}`,
		setup: func(t *testing.T, s *isolatedServer, f *gqlAuthzFixture) {
			org := s.store.CreateOrg(f.owner, "authz-attrib-org-"+f.owner.Login, "", "")
			if org == nil {
				t.Fatal("fixture org could not be created")
			}
			m := s.store.EnsureMannequin(org.ID, "imported-ghost", "ghost@import.invalid")
			if m == nil {
				t.Fatal("fixture mannequin could not be created")
			}
			f.attributionOrgNodeID = org.NodeID
			f.mannequinNodeID = m.NodeID
		},
		input: func(f *gqlAuthzFixture) map[string]interface{} {
			return map[string]interface{}{
				"ownerId":  f.attributionOrgNodeID,
				"sourceId": f.mannequinNodeID,
				"targetId": f.owner.NodeID,
			}
		},
	},
	{
		name: "addDiscussionPollVote",
		doc:  `mutation($input:AddDiscussionPollVoteInput!){addDiscussionPollVote(input:$input){pollOption{totalVoteCount viewerHasVoted}}}`,
		setup: func(t *testing.T, s *isolatedServer, f *gqlAuthzFixture) {
			poll := s.store.CreateDiscussionPoll(f.discussion.ID, "authz question", []string{"yes", "no"})
			if poll == nil || len(poll.Options) == 0 {
				t.Fatal("fixture poll could not be created")
			}
			f.pollOptionNodeID = poll.Options[0].NodeID
		},
		input: func(f *gqlAuthzFixture) map[string]interface{} {
			return map[string]interface{}{"pollOptionId": f.pollOptionNodeID}
		},
	},
	{
		name: "cloneTemplateRepository",
		doc:  `mutation($input:CloneTemplateRepositoryInput!){cloneTemplateRepository(input:$input){repository{name}}}`,
		setup: func(t *testing.T, s *isolatedServer, f *gqlAuthzFixture) {
			// Make the second repo the template so the primary keeps its
			// pristine invariants for the untouched-assertion.
			seedPullRequestBranches(t, s.Server, f.repo2, "seed")
			owner, _, _ := store.SplitRepoFullName(f.repo2.FullName)
			s.store.UpdateRepo(owner, f.repo2.Name, func(rp *store.Repo) { rp.IsTemplate = true })
		},
		input: func(f *gqlAuthzFixture) map[string]interface{} {
			return map[string]interface{}{
				"repositoryId": f.repo2.NodeID,
				"ownerId":      f.owner.NodeID,
				"name":         "authz-generated",
				"visibility":   "PRIVATE",
			}
		},
	},
	{
		name: "updateRefs",
		doc:  `mutation($input:UpdateRefsInput!){updateRefs(input:$input){clientMutationId}}`,
		input: func(f *gqlAuthzFixture) map[string]interface{} {
			return map[string]interface{}{
				"repositoryId": f.repo.NodeID,
				"refUpdates": []interface{}{map[string]interface{}{
					"name": "refs/heads/authz-bulk-ref", "afterOid": f.headSHA,
				}},
			}
		},
	},
	{
		name: "updateTeamsRepository",
		doc:  `mutation($input:UpdateTeamsRepositoryInput!){updateTeamsRepository(input:$input){teams{slug}}}`,
		setup: func(t *testing.T, s *isolatedServer, f *gqlAuthzFixture) {
			// A team grant needs an org-owned repo and a team; the fixture's
			// user-owned repo cannot carry one.
			org := s.store.CreateOrg(f.owner, "authz-teams-org-"+f.owner.Login, "", "")
			if org == nil {
				t.Fatal("fixture org could not be created")
			}
			f.orgRepo = s.store.CreateOrgRepo(org, f.owner, "authz-teams-repo", "", true)
			if f.orgRepo == nil {
				t.Fatal("fixture org repo could not be created")
			}
			team := s.store.CreateTeam(org.Login, "authz-grant-team", store.TeamOptions{})
			if team == nil {
				t.Fatal("fixture team could not be created")
			}
			f.teamNodeID = team.NodeID
		},
		input: func(f *gqlAuthzFixture) map[string]interface{} {
			return map[string]interface{}{
				"repositoryId": f.orgRepo.NodeID,
				"teamIds":      []interface{}{f.teamNodeID},
				"permission":   "WRITE",
			}
		},
	},
	{
		name: "deletePackageVersion",
		doc:  `mutation($input:DeletePackageVersionInput!){deletePackageVersion(input:$input){success}}`,
		setup: func(t *testing.T, s *isolatedServer, f *gqlAuthzFixture) {
			if pkg, _ := s.store.CreatePackage("user", f.owner.Login, "npm", "authz-package", "private"); pkg == nil {
				t.Fatal("fixture package could not be created")
			}
			version, err := s.store.CreatePackageVersion("user", f.owner.Login, "npm", "authz-package", "1.0.0", "", nil, nil)
			if err != nil || version == nil {
				t.Fatalf("fixture package version could not be created: %v", err)
			}
			f.packageVersionNodeID = version.NodeID
		},
		input: func(f *gqlAuthzFixture) map[string]interface{} {
			return map[string]interface{}{"packageVersionId": f.packageVersionNodeID}
		},
	},
	{
		name: "closeDiscussion",
		doc:  `mutation($input:CloseDiscussionInput!){closeDiscussion(input:$input){discussion{closed stateReason}}}`,
		input: func(f *gqlAuthzFixture) map[string]interface{} {
			return map[string]interface{}{"discussionId": f.discussion.NodeID, "reason": "OUTDATED"}
		},
	},
	{
		name: "reopenDiscussion",
		doc:  `mutation($input:ReopenDiscussionInput!){reopenDiscussion(input:$input){discussion{closed stateReason}}}`,
		setup: func(t *testing.T, s *isolatedServer, f *gqlAuthzFixture) {
			// Close the discussion first so the mutation under test is the
			// reopen, not the close.
			if !s.store.UpdateDiscussion(f.discussion.ID, func(d *store.Discussion) {
				d.Closed = true
				d.StateReason = "RESOLVED"
			}) {
				t.Fatal("fixture discussion could not be closed")
			}
		},
		input: func(f *gqlAuthzFixture) map[string]interface{} {
			return map[string]interface{}{"discussionId": f.discussion.NodeID}
		},
	},
	{
		name: "createRef",
		doc:  `mutation($input:CreateRefInput!){createRef(input:$input){ref{name}}}`,
		input: func(f *gqlAuthzFixture) map[string]interface{} {
			return map[string]interface{}{
				"repositoryId": f.repo.NodeID, "name": "refs/heads/authz-created-ref", "oid": f.headSHA,
			}
		},
	},
	{
		name: "updateRef",
		doc:  `mutation($input:UpdateRefInput!){updateRef(input:$input){ref{name}}}`,
		input: func(f *gqlAuthzFixture) map[string]interface{} {
			return map[string]interface{}{
				"refId": store.GitObjectNodeID(store.GitRefNodeIDPrefix, f.repo.ID, "refs/heads/spare"),
				"oid":   f.headSHA,
				// The branches share no ancestry, so force the move; an unforced
				// non-fast-forward refusal would look like an authz failure in
				// the entitled case.
				"force": true,
			}
		},
	},
	{
		name: "mergeBranch",
		doc:  `mutation($input:MergeBranchInput!){mergeBranch(input:$input){clientMutationId}}`,
		input: func(f *gqlAuthzFixture) map[string]interface{} {
			return map[string]interface{}{
				"repositoryId": f.repo.NodeID, "base": "spare", "head": "feature",
				"commitMessage": "authz merge",
			}
		},
	},
	{
		name: "createCommitOnBranch",
		doc:  `mutation($input:CreateCommitOnBranchInput!){createCommitOnBranch(input:$input){commit{oid}}}`,
		input: func(f *gqlAuthzFixture) map[string]interface{} {
			return map[string]interface{}{
				"branch":          map[string]interface{}{"repositoryNameWithOwner": f.repo.FullName, "branchName": "feature"},
				"expectedHeadOid": f.headSHA,
				"message":         map[string]interface{}{"headline": "authz commit"},
				"fileChanges": map[string]interface{}{
					"additions": []interface{}{map[string]interface{}{"path": "authz-commit.txt", "contents": "YXV0aHo="}},
				},
			}
		},
	},
	{
		name: "deleteRef",
		doc:  `mutation($input:DeleteRefInput!){deleteRef(input:$input){clientMutationId}}`,
		input: func(f *gqlAuthzFixture) map[string]interface{} {
			return map[string]interface{}{
				"refId": store.GitObjectNodeID(store.GitRefNodeIDPrefix, f.repo.ID, "refs/heads/spare"),
			}
		},
	},
	{
		name: "revertPullRequest",
		doc:  `mutation($input:RevertPullRequestInput!){revertPullRequest(input:$input){revertPullRequest{number}}}`,
		setup: func(t *testing.T, s *isolatedServer, f *gqlAuthzFixture) {
			// Only a merged PR can be reverted; merge it as the owner first so
			// the stranger is refused against the same state the owner
			// succeeds against.
			resp := s.put(t, "/api/v3/repos/"+f.repo.FullName+"/pulls/"+itoa(f.pr.Number)+"/merge", f.ownerToken, map[string]interface{}{})
			defer resp.Body.Close()
			if resp.StatusCode != 200 {
				t.Fatalf("fixture pull request did not merge: %d", resp.StatusCode)
			}
		},
		input: func(f *gqlAuthzFixture) map[string]interface{} {
			return map[string]interface{}{"pullRequestId": f.pr.NodeID}
		},
	},
	{
		name: "dismissRepositoryVulnerabilityAlert",
		doc:  `mutation($input:DismissRepositoryVulnerabilityAlertInput!){dismissRepositoryVulnerabilityAlert(input:$input){repositoryVulnerabilityAlert{state}}}`,
		setup: func(_ *testing.T, s *isolatedServer, f *gqlAuthzFixture) {
			f.dependabotAlert = s.store.CreateDependabotAlertIfNew(f.repo.FullName,
				"lodash", "npm", "package-lock.json", "GHSA-authz-table", "CVE-2021-0001",
				"high", "prototype pollution", "a description", "< 4.17.21", "4.17.21")
		},
		input: func(f *gqlAuthzFixture) map[string]interface{} {
			return map[string]interface{}{
				"repositoryVulnerabilityAlertId": f.dependabotAlert.NodeID,
				"dismissReason":                  "TOLERABLE_RISK",
			}
		},
	},
	{
		name: "deleteRepository",
		doc:  `mutation($input:DeleteRepositoryInput!){deleteRepository(input:$input){clientMutationId}}`,
		input: func(f *gqlAuthzFixture) map[string]interface{} {
			return map[string]interface{}{"repositoryId": f.repo.NodeID}
		},
	},
	{
		name: "createIssue",
		doc:  `mutation($input:CreateIssueInput!){createIssue(input:$input){issue{number}}}`,
		input: func(f *gqlAuthzFixture) map[string]interface{} {
			return map[string]interface{}{"repositoryId": f.repo.NodeID, "title": "from the table"}
		},
	},
	{
		name: "createLinkedBranch",
		doc:  `mutation($input:CreateLinkedBranchInput!){createLinkedBranch(input:$input){linkedBranch{id ref{name}}}}`,
		input: func(f *gqlAuthzFixture) map[string]interface{} {
			return map[string]interface{}{
				"issueId": f.issue.NodeID, "oid": f.headSHA, "name": "authz-linked-branch",
			}
		},
	},
	{
		name: "deleteLinkedBranch",
		doc:  `mutation($input:DeleteLinkedBranchInput!){deleteLinkedBranch(input:$input){issue{number}}}`,
		setup: func(t *testing.T, s *isolatedServer, f *gqlAuthzFixture) {
			const ref = "refs/heads/authz-seeded-linked-branch"
			if found, _ := s.store.LinkIssueBranch(f.issue.ID, f.repo.ID, ref); !found {
				t.Fatalf("could not seed a linked branch on the fixture issue")
			}
			f.linkedBranchNodeID = store.LinkedBranchNodeID(f.issue.ID, ref)
		},
		input: func(f *gqlAuthzFixture) map[string]interface{} {
			return map[string]interface{}{"linkedBranchId": f.linkedBranchNodeID}
		},
	},
	{
		name: "addComment",
		doc:  `mutation($input:AddCommentInput!){addComment(input:$input){commentEdge{node{id}}}}`,
		input: func(f *gqlAuthzFixture) map[string]interface{} {
			return map[string]interface{}{"subjectId": f.issue.NodeID, "body": "from the table"}
		},
	},
	{
		name: "closeIssue",
		doc:  `mutation($input:CloseIssueInput!){closeIssue(input:$input){issue{state}}}`,
		input: func(f *gqlAuthzFixture) map[string]interface{} {
			return map[string]interface{}{"issueId": f.issue.NodeID}
		},
	},
	{
		name: "reopenIssue",
		doc:  `mutation($input:ReopenIssueInput!){reopenIssue(input:$input){issue{state}}}`,
		input: func(f *gqlAuthzFixture) map[string]interface{} {
			return map[string]interface{}{"issueId": f.issue.NodeID}
		},
	},
	{
		name: "updateIssue",
		doc:  `mutation($input:UpdateIssueInput!){updateIssue(input:$input){issue{title}}}`,
		input: func(f *gqlAuthzFixture) map[string]interface{} {
			return map[string]interface{}{"id": f.issue.NodeID, "title": "from the table"}
		},
	},
	{
		name: "pinIssue",
		doc:  `mutation($input:PinIssueInput!){pinIssue(input:$input){issue{isPinned}}}`,
		input: func(f *gqlAuthzFixture) map[string]interface{} {
			return map[string]interface{}{"issueId": f.issue.NodeID}
		},
	},
	{
		name: "unpinIssue",
		doc:  `mutation($input:UnpinIssueInput!){unpinIssue(input:$input){issue{isPinned}}}`,
		input: func(f *gqlAuthzFixture) map[string]interface{} {
			return map[string]interface{}{"issueId": f.issue.NodeID}
		},
	},
	{
		name: "transferIssue",
		doc:  `mutation($input:TransferIssueInput!){transferIssue(input:$input){issue{number}}}`,
		input: func(f *gqlAuthzFixture) map[string]interface{} {
			return map[string]interface{}{"issueId": f.issue.NodeID, "repositoryId": f.repo2.NodeID}
		},
	},
	{
		name: "deleteIssue",
		doc:  `mutation($input:DeleteIssueInput!){deleteIssue(input:$input){clientMutationId}}`,
		input: func(f *gqlAuthzFixture) map[string]interface{} {
			return map[string]interface{}{"issueId": f.issue.NodeID}
		},
	},
	{
		name: "createDiscussion",
		doc:  `mutation($input:CreateDiscussionInput!){createDiscussion(input:$input){discussion{number}}}`,
		input: func(f *gqlAuthzFixture) map[string]interface{} {
			return map[string]interface{}{
				"repositoryId": f.repo.NodeID,
				"categoryId":   store.DiscussionCategoryNodeID(f.category.ID),
				"title":        "from the table",
				"body":         "from the table",
			}
		},
	},
	{
		name: "addDiscussionComment",
		doc:  `mutation($input:AddDiscussionCommentInput!){addDiscussionComment(input:$input){comment{id}}}`,
		input: func(f *gqlAuthzFixture) map[string]interface{} {
			return map[string]interface{}{"discussionId": f.discussion.NodeID, "body": "from the table"}
		},
	},
	{
		name: "addReaction",
		doc:  `mutation($input:AddReactionInput!){addReaction(input:$input){clientMutationId}}`,
		input: func(f *gqlAuthzFixture) map[string]interface{} {
			return map[string]interface{}{"subjectId": f.discussion.NodeID, "content": "THUMBS_UP"}
		},
	},
	{
		name: "removeReaction",
		doc:  `mutation($input:RemoveReactionInput!){removeReaction(input:$input){clientMutationId}}`,
		input: func(f *gqlAuthzFixture) map[string]interface{} {
			return map[string]interface{}{"subjectId": f.discussion.NodeID, "content": "THUMBS_UP"}
		},
	},
	{
		name: "updateDiscussion",
		doc:  `mutation($input:UpdateDiscussionInput!){updateDiscussion(input:$input){discussion{title}}}`,
		input: func(f *gqlAuthzFixture) map[string]interface{} {
			return map[string]interface{}{"discussionId": f.discussion.NodeID, "title": "from the table"}
		},
	},
	{
		name: "deleteDiscussion",
		doc:  `mutation($input:DeleteDiscussionInput!){deleteDiscussion(input:$input){clientMutationId}}`,
		input: func(f *gqlAuthzFixture) map[string]interface{} {
			return map[string]interface{}{"id": f.discussion.NodeID}
		},
	},
	{
		name: "updateDiscussionComment",
		doc:  `mutation($input:UpdateDiscussionCommentInput!){updateDiscussionComment(input:$input){comment{id}}}`,
		input: func(f *gqlAuthzFixture) map[string]interface{} {
			return map[string]interface{}{"commentId": f.discComment.NodeID, "body": "from the table"}
		},
	},
	{
		name: "deleteDiscussionComment",
		doc:  `mutation($input:DeleteDiscussionCommentInput!){deleteDiscussionComment(input:$input){clientMutationId}}`,
		input: func(f *gqlAuthzFixture) map[string]interface{} {
			return map[string]interface{}{"id": f.discComment.NodeID}
		},
	},
	{
		name: "markDiscussionCommentAsAnswer",
		doc:  `mutation($input:MarkDiscussionCommentAsAnswerInput!){markDiscussionCommentAsAnswer(input:$input){discussion{id}}}`,
		input: func(f *gqlAuthzFixture) map[string]interface{} {
			return map[string]interface{}{"id": f.discComment.NodeID}
		},
	},
	{
		name: "unmarkDiscussionCommentAsAnswer",
		doc:  `mutation($input:UnmarkDiscussionCommentAsAnswerInput!){unmarkDiscussionCommentAsAnswer(input:$input){discussion{id}}}`,
		input: func(f *gqlAuthzFixture) map[string]interface{} {
			return map[string]interface{}{"id": f.discComment.NodeID}
		},
	},
	{
		name: "addUpvote",
		doc:  `mutation($input:AddUpvoteInput!){addUpvote(input:$input){subject{upvoteCount}}}`,
		input: func(f *gqlAuthzFixture) map[string]interface{} {
			return map[string]interface{}{"subjectId": f.discussion.NodeID}
		},
	},
	{
		name: "removeUpvote",
		doc:  `mutation($input:RemoveUpvoteInput!){removeUpvote(input:$input){subject{upvoteCount}}}`,
		input: func(f *gqlAuthzFixture) map[string]interface{} {
			return map[string]interface{}{"subjectId": f.discussion.NodeID}
		},
	},
	{
		name: "minimizeComment",
		doc:  `mutation($input:MinimizeCommentInput!){minimizeComment(input:$input){minimizedComment{isMinimized}}}`,
		input: func(f *gqlAuthzFixture) map[string]interface{} {
			return map[string]interface{}{"subjectId": f.comment.NodeID, "classifier": "OFF_TOPIC"}
		},
	},
	{
		name: "unminimizeComment",
		doc:  `mutation($input:UnminimizeCommentInput!){unminimizeComment(input:$input){unminimizedComment{isMinimized}}}`,
		input: func(f *gqlAuthzFixture) map[string]interface{} {
			return map[string]interface{}{"subjectId": f.comment.NodeID}
		},
	},
	{
		name: "lockLockable",
		doc:  `mutation($input:LockLockableInput!){lockLockable(input:$input){lockedRecord{locked}}}`,
		input: func(f *gqlAuthzFixture) map[string]interface{} {
			return map[string]interface{}{"lockableId": f.issue.NodeID, "lockReason": "SPAM"}
		},
	},
	{
		name: "unlockLockable",
		doc:  `mutation($input:UnlockLockableInput!){unlockLockable(input:$input){unlockedRecord{locked}}}`,
		input: func(f *gqlAuthzFixture) map[string]interface{} {
			return map[string]interface{}{"lockableId": f.issue.NodeID}
		},
	},
	{
		name: "addLabelsToLabelable",
		doc:  `mutation($input:AddLabelsToLabelableInput!){addLabelsToLabelable(input:$input){labelable{__typename}}}`,
		input: func(f *gqlAuthzFixture) map[string]interface{} {
			return map[string]interface{}{
				"labelableId": f.issue.NodeID,
				"labelIds":    []interface{}{f.label.NodeID},
			}
		},
	},
	{
		name: "removeLabelsFromLabelable",
		doc:  `mutation($input:RemoveLabelsFromLabelableInput!){removeLabelsFromLabelable(input:$input){labelable{__typename}}}`,
		input: func(f *gqlAuthzFixture) map[string]interface{} {
			return map[string]interface{}{
				"labelableId": f.issue.NodeID,
				"labelIds":    []interface{}{f.label.NodeID},
			}
		},
	},
	{
		name: "clearLabelsFromLabelable",
		doc:  `mutation($input:ClearLabelsFromLabelableInput!){clearLabelsFromLabelable(input:$input){labelable{__typename}}}`,
		input: func(f *gqlAuthzFixture) map[string]interface{} {
			return map[string]interface{}{"labelableId": f.issue.NodeID}
		},
	},
	{
		name: "createPullRequest",
		doc:  `mutation($input:CreatePullRequestInput!){createPullRequest(input:$input){pullRequest{number}}}`,
		input: func(f *gqlAuthzFixture) map[string]interface{} {
			return map[string]interface{}{
				"repositoryId": f.repo.NodeID,
				"title":        "from the table",
				"headRefName":  "spare",
				"baseRefName":  "main",
			}
		},
	},
	{
		name: "addPullRequestReview",
		doc:  `mutation($input:AddPullRequestReviewInput!){addPullRequestReview(input:$input){pullRequestReview{id}}}`,
		input: func(f *gqlAuthzFixture) map[string]interface{} {
			return map[string]interface{}{"pullRequestId": f.pr.NodeID, "event": "COMMENT", "body": "from the table"}
		},
	},
	{
		name: "submitPullRequestReview",
		doc:  `mutation($input:SubmitPullRequestReviewInput!){submitPullRequestReview(input:$input){pullRequestReview{state}}}`,
		input: func(f *gqlAuthzFixture) map[string]interface{} {
			return map[string]interface{}{"pullRequestReviewId": f.reviewNodeID, "event": "APPROVE"}
		},
	},
	{
		name: "dismissPullRequestReview",
		doc:  `mutation($input:DismissPullRequestReviewInput!){dismissPullRequestReview(input:$input){pullRequestReview{state}}}`,
		input: func(f *gqlAuthzFixture) map[string]interface{} {
			return map[string]interface{}{"pullRequestReviewId": f.reviewNodeID, "message": "stale"}
		},
	},
	{
		name: "closePullRequest",
		doc:  `mutation($input:ClosePullRequestInput!){closePullRequest(input:$input){pullRequest{state}}}`,
		input: func(f *gqlAuthzFixture) map[string]interface{} {
			return map[string]interface{}{"pullRequestId": f.pr.NodeID}
		},
	},
	{
		name: "reopenPullRequest",
		doc:  `mutation($input:ReopenPullRequestInput!){reopenPullRequest(input:$input){pullRequest{state}}}`,
		input: func(f *gqlAuthzFixture) map[string]interface{} {
			return map[string]interface{}{"pullRequestId": f.pr.NodeID}
		},
	},
	{
		name: "updatePullRequest",
		doc:  `mutation($input:UpdatePullRequestInput!){updatePullRequest(input:$input){pullRequest{title}}}`,
		input: func(f *gqlAuthzFixture) map[string]interface{} {
			return map[string]interface{}{"pullRequestId": f.pr.NodeID, "title": "from the table"}
		},
	},
	{
		name: "markPullRequestReadyForReview",
		doc:  `mutation($input:MarkPullRequestReadyForReviewInput!){markPullRequestReadyForReview(input:$input){pullRequest{isDraft}}}`,
		input: func(f *gqlAuthzFixture) map[string]interface{} {
			return map[string]interface{}{"pullRequestId": f.pr.NodeID}
		},
	},
	{
		name: "convertPullRequestToDraft",
		doc:  `mutation($input:ConvertPullRequestToDraftInput!){convertPullRequestToDraft(input:$input){pullRequest{isDraft}}}`,
		input: func(f *gqlAuthzFixture) map[string]interface{} {
			return map[string]interface{}{"pullRequestId": f.pr.NodeID}
		},
	},
	{
		name: "mergePullRequest",
		doc:  `mutation($input:MergePullRequestInput!){mergePullRequest(input:$input){clientMutationId}}`,
		input: func(f *gqlAuthzFixture) map[string]interface{} {
			return map[string]interface{}{"pullRequestId": f.pr.NodeID}
		},
	},
	{
		name: "enablePullRequestAutoMerge",
		doc:  `mutation($input:EnablePullRequestAutoMergeInput!){enablePullRequestAutoMerge(input:$input){pullRequest{autoMergeRequest{mergeMethod}}}}`,
		input: func(f *gqlAuthzFixture) map[string]interface{} {
			return map[string]interface{}{"pullRequestId": f.pr.NodeID, "mergeMethod": "SQUASH"}
		},
		// Enabling auto-merge requires the repo to allow it and the PR to be
		// blocked from merging right now (a clean PR is refused).
		setup: func(t *testing.T, s *isolatedServer, f *gqlAuthzFixture) {
			t.Helper()
			owner, name, _ := store.SplitRepoFullName(f.repo.FullName)
			s.store.UpdateRepo(owner, name, func(r *store.Repo) { r.AllowAutoMerge = true })
			s.setBranchProtection(f.repo, "main", &store.BranchProtection{
				RequiredStatusChecks: &store.BPStatusChecks{Contexts: []string{"ci"}},
				EnforceAdmins:        &store.BPEnforceAdmins{Enabled: true},
			})
		},
	},
	{
		name: "disablePullRequestAutoMerge",
		doc:  `mutation($input:DisablePullRequestAutoMergeInput!){disablePullRequestAutoMerge(input:$input){pullRequest{autoMergeRequest{mergeMethod}}}}`,
		input: func(f *gqlAuthzFixture) map[string]interface{} {
			return map[string]interface{}{"pullRequestId": f.pr.NodeID}
		},
		// Disabling requires an armed request to disarm.
		setup: func(t *testing.T, s *isolatedServer, f *gqlAuthzFixture) {
			t.Helper()
			s.store.UpdatePullRequest(f.pr.ID, func(p *store.PullRequest) {
				p.AutoMerge = &store.PullRequestAutoMerge{
					EnabledByID: f.owner.ID,
					MergeMethod: "MERGE",
					EnabledAt:   fixedTestTime.UTC(),
				}
			})
		},
	},
	{
		name: "resolveReviewThread",
		doc:  `mutation($input:ResolveReviewThreadInput!){resolveReviewThread(input:$input){thread{isResolved}}}`,
		input: func(f *gqlAuthzFixture) map[string]interface{} {
			return map[string]interface{}{"threadId": f.threadNodeID}
		},
	},
	{
		name: "unresolveReviewThread",
		doc:  `mutation($input:UnresolveReviewThreadInput!){unresolveReviewThread(input:$input){thread{isResolved}}}`,
		input: func(f *gqlAuthzFixture) map[string]interface{} {
			return map[string]interface{}{"threadId": f.threadNodeID}
		},
	},

	// checks
	{
		name: "createCheckRun",
		doc:  `mutation($input:CreateCheckRunInput!){createCheckRun(input:$input){checkRun{name status}}}`,
		input: func(f *gqlAuthzFixture) map[string]interface{} {
			return map[string]interface{}{
				"repositoryId": f.repo.NodeID, "name": "authz-check", "headSha": f.headSHA,
			}
		},
	},
	{
		name: "createCheckSuite",
		doc:  `mutation($input:CreateCheckSuiteInput!){createCheckSuite(input:$input){checkSuite{id status}}}`,
		input: func(f *gqlAuthzFixture) map[string]interface{} {
			return map[string]interface{}{"repositoryId": f.repo.NodeID, "headSha": f.headSHA}
		},
	},
	{
		name: "rerequestCheckSuite",
		doc:  `mutation($input:RerequestCheckSuiteInput!){rerequestCheckSuite(input:$input){checkSuite{status}}}`,
		setup: func(t *testing.T, s *isolatedServer, f *gqlAuthzFixture) {
			suite := s.store.CreateCheckSuite(f.repo.FullName, "", f.headSHA, 0)
			if suite == nil {
				t.Fatal("fixture check suite could not be created")
			}
			f.checkSuiteNodeID = suite.NodeID
		},
		input: func(f *gqlAuthzFixture) map[string]interface{} {
			return map[string]interface{}{"repositoryId": f.repo.NodeID, "checkSuiteId": f.checkSuiteNodeID}
		},
	},
	{
		name: "updateCheckRun",
		doc:  `mutation($input:UpdateCheckRunInput!){updateCheckRun(input:$input){checkRun{status conclusion}}}`,
		setup: func(t *testing.T, s *isolatedServer, f *gqlAuthzFixture) {
			run := s.store.CreateCheckRun(f.repo.FullName, f.headSHA, "authz-existing-check", 0, 0)
			if run == nil {
				t.Fatal("fixture check run could not be created")
			}
			f.checkRunNodeID = run.NodeID
		},
		input: func(f *gqlAuthzFixture) map[string]interface{} {
			return map[string]interface{}{
				"repositoryId": f.repo.NodeID, "checkRunId": f.checkRunNodeID,
				"status": "COMPLETED", "conclusion": "SUCCESS",
			}
		},
	},
	{
		name: "updateCheckSuitePreferences",
		doc:  `mutation($input:UpdateCheckSuitePreferencesInput!){updateCheckSuitePreferences(input:$input){repository{name}}}`,
		input: func(f *gqlAuthzFixture) map[string]interface{} {
			return map[string]interface{}{
				"repositoryId": f.repo.NodeID, "autoTriggerPreferences": []interface{}{},
			}
		},
	},

	// deployments
	{
		name: "createDeployment",
		doc:  `mutation($input:CreateDeploymentInput!){createDeployment(input:$input){autoMerged deployment{environment commitOid}}}`,
		input: func(f *gqlAuthzFixture) map[string]interface{} {
			return map[string]interface{}{
				"repositoryId": f.repo.NodeID,
				"refId":        store.GitObjectNodeID(store.GitRefNodeIDPrefix, f.repo.ID, "refs/heads/feature"),
				"environment":  "authz-env",
			}
		},
	},
	{
		name: "createDeploymentStatus",
		doc:  `mutation($input:CreateDeploymentStatusInput!){createDeploymentStatus(input:$input){deploymentStatus{state environment}}}`,
		setup: func(t *testing.T, s *isolatedServer, f *gqlAuthzFixture) {
			deployment := s.store.Deployments.CreateDeployment(f.repo.ID, f.owner.ID, "feature", f.headSHA, "deploy", "authz-env", "", nil, false, false)
			if deployment == nil {
				t.Fatal("fixture deployment could not be created")
			}
			f.deploymentNodeID = deployment.NodeID
		},
		input: func(f *gqlAuthzFixture) map[string]interface{} {
			return map[string]interface{}{"deploymentId": f.deploymentNodeID, "state": "SUCCESS"}
		},
	},
	{
		name: "deleteDeployment",
		doc:  `mutation($input:DeleteDeploymentInput!){deleteDeployment(input:$input){clientMutationId}}`,
		setup: func(t *testing.T, s *isolatedServer, f *gqlAuthzFixture) {
			deployment := s.store.Deployments.CreateDeployment(f.repo.ID, f.owner.ID, "feature", f.headSHA, "deploy", "authz-env", "", nil, false, false)
			if deployment == nil {
				t.Fatal("fixture deployment could not be created")
			}
			f.deploymentNodeID = deployment.NodeID
		},
		input: func(f *gqlAuthzFixture) map[string]interface{} {
			return map[string]interface{}{"id": f.deploymentNodeID}
		},
	},
	{
		name: "approveDeployments",
		doc:  `mutation($input:ApproveDeploymentsInput!){approveDeployments(input:$input){deployments{environment}}}`,
		setup: func(t *testing.T, s *isolatedServer, f *gqlAuthzFixture) {
			f.workflowRunNodeID = seedPendingDeploymentRun(t, s, f)
		},
		input: func(f *gqlAuthzFixture) map[string]interface{} {
			return map[string]interface{}{
				"workflowRunId":  f.workflowRunNodeID,
				"environmentIds": []interface{}{f.environmentNodeID},
			}
		},
	},
	{
		name: "rejectDeployments",
		doc:  `mutation($input:RejectDeploymentsInput!){rejectDeployments(input:$input){deployments{environment}}}`,
		setup: func(t *testing.T, s *isolatedServer, f *gqlAuthzFixture) {
			f.workflowRunNodeID = seedPendingDeploymentRun(t, s, f)
		},
		input: func(f *gqlAuthzFixture) map[string]interface{} {
			return map[string]interface{}{
				"workflowRunId":  f.workflowRunNodeID,
				"environmentIds": []interface{}{f.environmentNodeID},
				"comment":        "not this one",
			}
		},
	},

	// environments
	{
		name: "createEnvironment",
		doc:  `mutation($input:CreateEnvironmentInput!){createEnvironment(input:$input){environment{name}}}`,
		input: func(f *gqlAuthzFixture) map[string]interface{} {
			return map[string]interface{}{"repositoryId": f.repo.NodeID, "name": "authz-created-env"}
		},
	},
	{
		name: "updateEnvironment",
		doc:  `mutation($input:UpdateEnvironmentInput!){updateEnvironment(input:$input){environment{name}}}`,
		setup: func(t *testing.T, s *isolatedServer, f *gqlAuthzFixture) {
			seedAuthzEnvironment(t, s, f)
		},
		input: func(f *gqlAuthzFixture) map[string]interface{} {
			return map[string]interface{}{
				"environmentId": f.environmentNodeID, "waitTimer": 30, "preventSelfReview": true,
			}
		},
	},
	{
		name: "deleteEnvironment",
		doc:  `mutation($input:DeleteEnvironmentInput!){deleteEnvironment(input:$input){clientMutationId}}`,
		setup: func(t *testing.T, s *isolatedServer, f *gqlAuthzFixture) {
			seedAuthzEnvironment(t, s, f)
		},
		input: func(f *gqlAuthzFixture) map[string]interface{} {
			return map[string]interface{}{"id": f.environmentNodeID}
		},
	},
	{
		name: "pinEnvironment",
		doc:  `mutation($input:PinEnvironmentInput!){pinEnvironment(input:$input){environment{isPinned} pinnedEnvironment{position}}}`,
		setup: func(t *testing.T, s *isolatedServer, f *gqlAuthzFixture) {
			seedAuthzEnvironment(t, s, f)
		},
		input: func(f *gqlAuthzFixture) map[string]interface{} {
			return map[string]interface{}{"environmentId": f.environmentNodeID, "pinned": true}
		},
	},
	{
		name: "reorderEnvironment",
		doc:  `mutation($input:ReorderEnvironmentInput!){reorderEnvironment(input:$input){environment{pinnedPosition}}}`,
		setup: func(t *testing.T, s *isolatedServer, f *gqlAuthzFixture) {
			env := seedAuthzEnvironment(t, s, f)
			if s.store.Deployments.PinEnvironment(f.repo.ID, env.ID, fixedTestTime.UTC()) == nil {
				t.Fatal("fixture environment could not be pinned")
			}
		},
		input: func(f *gqlAuthzFixture) map[string]interface{} {
			return map[string]interface{}{"environmentId": f.environmentNodeID, "position": 1}
		},
	},

	// classic projects
	{
		name: "createProject",
		doc:  `mutation($input:CreateProjectInput!){createProject(input:$input){project{id name}}}`,
		input: func(f *gqlAuthzFixture) map[string]interface{} {
			return map[string]interface{}{"ownerId": f.repo.NodeID, "name": "authz classic board"}
		},
	},
	{
		name:  "updateProject",
		doc:   `mutation($input:UpdateProjectInput!){updateProject(input:$input){project{name state}}}`,
		setup: seedClassicProjectFixture,
		input: func(f *gqlAuthzFixture) map[string]interface{} {
			return map[string]interface{}{"projectId": f.classicProject.NodeID, "name": "renamed board", "state": "CLOSED"}
		},
	},
	{
		name:  "deleteProject",
		doc:   `mutation($input:DeleteProjectInput!){deleteProject(input:$input){owner{id}}}`,
		setup: seedClassicProjectFixture,
		input: func(f *gqlAuthzFixture) map[string]interface{} {
			return map[string]interface{}{"projectId": f.classicProject.NodeID}
		},
	},
	{
		name:  "cloneProject",
		doc:   `mutation($input:CloneProjectInput!){cloneProject(input:$input){project{id name}}}`,
		setup: seedClassicProjectFixture,
		input: func(f *gqlAuthzFixture) map[string]interface{} {
			return map[string]interface{}{
				"sourceId": f.classicProject.NodeID, "targetOwnerId": f.repo.NodeID,
				"name": "cloned board", "includeWorkflows": false,
			}
		},
	},
	{
		// importProject names its owner by login; the refusing stranger is not
		// that user, so the account-scoped half of the rule is what refuses.
		name: "importProject",
		doc:  `mutation($input:ImportProjectInput!){importProject(input:$input){project{id}}}`,
		input: func(f *gqlAuthzFixture) map[string]interface{} {
			return map[string]interface{}{
				"ownerName": f.owner.Login, "name": "imported board",
				"columnImports": []interface{}{},
			}
		},
	},
	{
		name:  "addProjectColumn",
		doc:   `mutation($input:AddProjectColumnInput!){addProjectColumn(input:$input){project{id} columnEdge{node{id name}}}}`,
		setup: seedClassicProjectFixture,
		input: func(f *gqlAuthzFixture) map[string]interface{} {
			return map[string]interface{}{"projectId": f.classicProject.NodeID, "name": "In review"}
		},
	},
	{
		name:  "updateProjectColumn",
		doc:   `mutation($input:UpdateProjectColumnInput!){updateProjectColumn(input:$input){projectColumn{name}}}`,
		setup: seedClassicProjectFixture,
		input: func(f *gqlAuthzFixture) map[string]interface{} {
			return map[string]interface{}{"projectColumnId": f.classicColumn.NodeID, "name": "Renamed"}
		},
	},
	{
		name:  "deleteProjectColumn",
		doc:   `mutation($input:DeleteProjectColumnInput!){deleteProjectColumn(input:$input){deletedColumnId project{id}}}`,
		setup: seedClassicProjectFixture,
		input: func(f *gqlAuthzFixture) map[string]interface{} {
			return map[string]interface{}{"columnId": f.classicColumn2.NodeID}
		},
	},
	{
		name:  "moveProjectColumn",
		doc:   `mutation($input:MoveProjectColumnInput!){moveProjectColumn(input:$input){columnEdge{node{id}}}}`,
		setup: seedClassicProjectFixture,
		input: func(f *gqlAuthzFixture) map[string]interface{} {
			return map[string]interface{}{"columnId": f.classicColumn2.NodeID}
		},
	},
	{
		name:  "addProjectCard",
		doc:   `mutation($input:AddProjectCardInput!){addProjectCard(input:$input){cardEdge{node{id note}} projectColumn{id}}}`,
		setup: seedClassicProjectFixture,
		input: func(f *gqlAuthzFixture) map[string]interface{} {
			return map[string]interface{}{"projectColumnId": f.classicColumn.NodeID, "note": "another note"}
		},
	},
	{
		name:  "updateProjectCard",
		doc:   `mutation($input:UpdateProjectCardInput!){updateProjectCard(input:$input){projectCard{note isArchived}}}`,
		setup: seedClassicProjectFixture,
		input: func(f *gqlAuthzFixture) map[string]interface{} {
			return map[string]interface{}{"projectCardId": f.classicCard.NodeID, "note": "edited note", "isArchived": true}
		},
	},
	{
		name:  "deleteProjectCard",
		doc:   `mutation($input:DeleteProjectCardInput!){deleteProjectCard(input:$input){deletedCardId column{id}}}`,
		setup: seedClassicProjectFixture,
		input: func(f *gqlAuthzFixture) map[string]interface{} {
			return map[string]interface{}{"cardId": f.classicCard.NodeID}
		},
	},
	{
		name:  "moveProjectCard",
		doc:   `mutation($input:MoveProjectCardInput!){moveProjectCard(input:$input){cardEdge{node{id}}}}`,
		setup: seedClassicProjectFixture,
		input: func(f *gqlAuthzFixture) map[string]interface{} {
			return map[string]interface{}{"cardId": f.classicCard.NodeID, "columnId": f.classicColumn2.NodeID}
		},
	},
	{
		name:  "convertProjectCardNoteToIssue",
		doc:   `mutation($input:ConvertProjectCardNoteToIssueInput!){convertProjectCardNoteToIssue(input:$input){projectCard{state}}}`,
		setup: seedClassicProjectFixture,
		input: func(f *gqlAuthzFixture) map[string]interface{} {
			return map[string]interface{}{"projectCardId": f.classicCard.NodeID, "repositoryId": f.repo.NodeID}
		},
	},
	{
		// A repo-scoped board refuses links by design, so the entitled half
		// needs a user-owned board.
		name:  "linkRepositoryToProject",
		doc:   `mutation($input:LinkRepositoryToProjectInput!){linkRepositoryToProject(input:$input){project{id} repository{name}}}`,
		setup: seedClassicOwnerProjectFixture,
		input: func(f *gqlAuthzFixture) map[string]interface{} {
			return map[string]interface{}{"projectId": f.classicOwnerProject.NodeID, "repositoryId": f.repo.NodeID}
		},
	},
	{
		name: "unlinkRepositoryFromProject",
		doc:  `mutation($input:UnlinkRepositoryFromProjectInput!){unlinkRepositoryFromProject(input:$input){project{id} repository{name}}}`,
		setup: func(t *testing.T, s *isolatedServer, f *gqlAuthzFixture) {
			t.Helper()
			seedClassicOwnerProjectFixture(t, s, f)
			if !s.store.LinkRepoToProjectClassic(f.classicOwnerProject.ID, f.repo.ID) {
				t.Fatalf("could not link the fixture repository to the owner board")
			}
		},
		input: func(f *gqlAuthzFixture) map[string]interface{} {
			return map[string]interface{}{"projectId": f.classicOwnerProject.NodeID, "repositoryId": f.repo.NodeID}
		},
	},

	// branch protection
	{
		name: "createBranchProtectionRule",
		doc:  `mutation($input:CreateBranchProtectionRuleInput!){createBranchProtectionRule(input:$input){branchProtectionRule{pattern requiresApprovingReviews requiredApprovingReviewCount}}}`,
		input: func(f *gqlAuthzFixture) map[string]interface{} {
			return map[string]interface{}{
				"repositoryId": f.repo.NodeID, "pattern": "main",
				"requiresApprovingReviews": true, "requiredApprovingReviewCount": 1,
			}
		},
	},
	{
		name: "updateBranchProtectionRule",
		doc:  `mutation($input:UpdateBranchProtectionRuleInput!){updateBranchProtectionRule(input:$input){branchProtectionRule{pattern allowsDeletions}}}`,
		setup: func(t *testing.T, s *isolatedServer, f *gqlAuthzFixture) {
			t.Helper()
			s.store.SetBranchProtection(f.repo.ID, "main", &store.BranchProtection{
				Enabled: true, RequiredLinearHistory: &store.BPEnabled{Enabled: true},
			})
		},
		input: func(f *gqlAuthzFixture) map[string]interface{} {
			return map[string]interface{}{
				"branchProtectionRuleId": store.BranchProtectionRuleNodeID(f.repo.ID, "main"),
				"allowsDeletions":        true,
			}
		},
	},
	{
		name: "deleteBranchProtectionRule",
		doc:  `mutation($input:DeleteBranchProtectionRuleInput!){deleteBranchProtectionRule(input:$input){clientMutationId}}`,
		setup: func(t *testing.T, s *isolatedServer, f *gqlAuthzFixture) {
			t.Helper()
			s.store.SetBranchProtection(f.repo.ID, "main", &store.BranchProtection{
				Enabled: true, RequiredLinearHistory: &store.BPEnabled{Enabled: true},
			})
		},
		input: func(f *gqlAuthzFixture) map[string]interface{} {
			return map[string]interface{}{
				"branchProtectionRuleId": store.BranchProtectionRuleNodeID(f.repo.ID, "main"),
			}
		},
	},

	// repository rulesets
	{
		name: "createRepositoryRuleset",
		doc:  `mutation($input:CreateRepositoryRulesetInput!){createRepositoryRuleset(input:$input){ruleset{name enforcement}}}`,
		input: func(f *gqlAuthzFixture) map[string]interface{} {
			return map[string]interface{}{
				"sourceId": f.repo.NodeID, "name": "authz-ruleset", "enforcement": "ACTIVE",
				"conditions": map[string]interface{}{
					"refName": map[string]interface{}{"include": []interface{}{"~ALL"}, "exclude": []interface{}{}},
				},
			}
		},
	},
	{
		name: "updateRepositoryRuleset",
		doc:  `mutation($input:UpdateRepositoryRulesetInput!){updateRepositoryRuleset(input:$input){ruleset{enforcement}}}`,
		setup: func(t *testing.T, s *isolatedServer, f *gqlAuthzFixture) {
			t.Helper()
			rs := s.store.CreateRuleset(f.repo, &store.Ruleset{Name: "authz-seeded-ruleset"})
			if rs == nil {
				t.Fatal("fixture ruleset could not be created")
			}
			f.rulesetNodeID = rs.NodeID
		},
		input: func(f *gqlAuthzFixture) map[string]interface{} {
			return map[string]interface{}{"repositoryRulesetId": f.rulesetNodeID, "enforcement": "DISABLED"}
		},
	},
	{
		name: "deleteRepositoryRuleset",
		doc:  `mutation($input:DeleteRepositoryRulesetInput!){deleteRepositoryRuleset(input:$input){clientMutationId}}`,
		setup: func(t *testing.T, s *isolatedServer, f *gqlAuthzFixture) {
			t.Helper()
			rs := s.store.CreateRuleset(f.repo, &store.Ruleset{Name: "authz-seeded-ruleset"})
			if rs == nil {
				t.Fatal("fixture ruleset could not be created")
			}
			f.rulesetNodeID = rs.NodeID
		},
		input: func(f *gqlAuthzFixture) map[string]interface{} {
			return map[string]interface{}{"repositoryRulesetId": f.rulesetNodeID}
		},
	},

	// repository custom properties
	//
	// These records belong to org/enterprise accounts, so the rows seed an org
	// owned by the fixture owner and widen both tokens to admin:org; the
	// stranger's refusal is about org standing, not token scope.
	{
		name: "createRepositoryCustomProperty",
		doc:  `mutation($input:CreateRepositoryCustomPropertyInput!){createRepositoryCustomProperty(input:$input){repositoryCustomProperty{propertyName valueType}}}`,
		setup: func(t *testing.T, s *isolatedServer, f *gqlAuthzFixture) {
			seedAuthzPropsOrg(t, s, f)
		},
		input: func(f *gqlAuthzFixture) map[string]interface{} {
			return map[string]interface{}{
				"sourceId": f.propsOrg.NodeID, "propertyName": "authz-prop", "valueType": "STRING",
			}
		},
	},
	{
		name: "updateRepositoryCustomProperty",
		doc:  `mutation($input:UpdateRepositoryCustomPropertyInput!){updateRepositoryCustomProperty(input:$input){repositoryCustomProperty{propertyName description}}}`,
		setup: func(t *testing.T, s *isolatedServer, f *gqlAuthzFixture) {
			seedAuthzPropsOrg(t, s, f)
			seedAuthzCustomProperty(t, s, f)
		},
		input: func(f *gqlAuthzFixture) map[string]interface{} {
			return map[string]interface{}{
				"repositoryCustomPropertyId": "RCP_" + f.propsOrg.Login + "/authz-prop",
				"description":                "edited",
			}
		},
	},
	{
		name: "deleteRepositoryCustomProperty",
		doc:  `mutation($input:DeleteRepositoryCustomPropertyInput!){deleteRepositoryCustomProperty(input:$input){repositoryCustomProperty{propertyName}}}`,
		setup: func(t *testing.T, s *isolatedServer, f *gqlAuthzFixture) {
			seedAuthzPropsOrg(t, s, f)
			seedAuthzCustomProperty(t, s, f)
		},
		input: func(f *gqlAuthzFixture) map[string]interface{} {
			return map[string]interface{}{"id": "RCP_" + f.propsOrg.Login + "/authz-prop"}
		},
	},
	{
		// Promotion writes the enterprise schema — a site admin's call — so
		// the setup grants the fixture owner that standing.
		name: "promoteRepositoryCustomProperty",
		doc:  `mutation($input:PromoteRepositoryCustomPropertyInput!){promoteRepositoryCustomProperty(input:$input){repositoryCustomProperty{propertyName}}}`,
		setup: func(t *testing.T, s *isolatedServer, f *gqlAuthzFixture) {
			seedAuthzPropsOrg(t, s, f)
			seedAuthzCustomProperty(t, s, f)
			s.store.Mu.Lock()
			s.store.Users[f.owner.ID].SiteAdmin = true
			s.store.Mu.Unlock()
		},
		input: func(f *gqlAuthzFixture) map[string]interface{} {
			return map[string]interface{}{
				"repositoryCustomPropertyId": "RCP_" + f.propsOrg.Login + "/authz-prop",
			}
		},
	},
	{
		// Values are per-repo admin; the definition lives under the repo
		// owner's own login for a user-owned repo.
		name: "setRepositoryCustomPropertyValues",
		doc:  `mutation($input:SetRepositoryCustomPropertyValuesInput!){setRepositoryCustomPropertyValues(input:$input){repository{name}}}`,
		setup: func(t *testing.T, s *isolatedServer, f *gqlAuthzFixture) {
			t.Helper()
			s.store.UpsertCustomProperty(f.owner.Login, &store.CustomProperty{
				PropertyName: "authz-prop", ValueType: "string", ValuesEditableBy: "org_actors",
			})
		},
		input: func(f *gqlAuthzFixture) map[string]interface{} {
			return map[string]interface{}{
				"repositoryId": f.repo.NodeID,
				"properties": []interface{}{
					map[string]interface{}{"propertyName": "authz-prop", "value": "authz-value"},
				},
			}
		},
	},

	// verifiable domains
	{
		name: "addVerifiableDomain",
		doc:  `mutation($input:AddVerifiableDomainInput!){addVerifiableDomain(input:$input){domain{domain isVerified}}}`,
		setup: func(t *testing.T, s *isolatedServer, f *gqlAuthzFixture) {
			seedAuthzPropsOrg(t, s, f)
		},
		input: func(f *gqlAuthzFixture) map[string]interface{} {
			return map[string]interface{}{"ownerId": f.propsOrg.NodeID, "domain": "authz.example.com"}
		},
	},
	{
		name: "approveVerifiableDomain",
		doc:  `mutation($input:ApproveVerifiableDomainInput!){approveVerifiableDomain(input:$input){domain{domain isApproved}}}`,
		setup: func(t *testing.T, s *isolatedServer, f *gqlAuthzFixture) {
			seedAuthzPropsOrg(t, s, f)
			seedAuthzVerifiableDomain(t, s, f)
		},
		input: func(f *gqlAuthzFixture) map[string]interface{} {
			return map[string]interface{}{"id": f.domainNodeID}
		},
	},
	{
		name: "verifyVerifiableDomain",
		doc:  `mutation($input:VerifyVerifiableDomainInput!){verifyVerifiableDomain(input:$input){domain{domain isVerified}}}`,
		setup: func(t *testing.T, s *isolatedServer, f *gqlAuthzFixture) {
			seedAuthzPropsOrg(t, s, f)
			seedAuthzVerifiableDomain(t, s, f)
		},
		input: func(f *gqlAuthzFixture) map[string]interface{} {
			return map[string]interface{}{"id": f.domainNodeID}
		},
	},
	{
		name: "regenerateVerifiableDomainToken",
		doc:  `mutation($input:RegenerateVerifiableDomainTokenInput!){regenerateVerifiableDomainToken(input:$input){verificationToken}}`,
		setup: func(t *testing.T, s *isolatedServer, f *gqlAuthzFixture) {
			seedAuthzPropsOrg(t, s, f)
			seedAuthzVerifiableDomain(t, s, f)
		},
		input: func(f *gqlAuthzFixture) map[string]interface{} {
			return map[string]interface{}{"id": f.domainNodeID}
		},
	},
	{
		name: "deleteVerifiableDomain",
		doc:  `mutation($input:DeleteVerifiableDomainInput!){deleteVerifiableDomain(input:$input){owner{__typename}}}`,
		setup: func(t *testing.T, s *isolatedServer, f *gqlAuthzFixture) {
			seedAuthzPropsOrg(t, s, f)
			seedAuthzVerifiableDomain(t, s, f)
		},
		input: func(f *gqlAuthzFixture) map[string]interface{} {
			return map[string]interface{}{"id": f.domainNodeID}
		},
	},
}

// seedAuthzPropsOrg creates the org the custom-property and verifiable-domain
// rows act on and widens both tokens to admin:org, since the entitlement under
// test is org standing, not token scope.
func seedAuthzPropsOrg(t *testing.T, s *isolatedServer, f *gqlAuthzFixture) {
	t.Helper()
	f.propsOrg = s.store.CreateOrg(f.owner, "authz-props-org-"+f.owner.Login, "", "")
	if f.propsOrg == nil {
		t.Fatal("fixture organization could not be created")
	}
	ownerTok := s.store.CreateToken(f.owner.ID, "repo,admin:org")
	strangerTok := s.store.CreateToken(f.stranger.ID, "repo,admin:org")
	if ownerTok == nil || strangerTok == nil {
		t.Fatal("fixture tokens could not be widened to admin:org")
	}
	f.ownerToken = ownerTok.Value
	f.strangerToken = strangerTok.Value
}

// seedAuthzCustomProperty defines the property the update/delete/promote
// rows address on the seeded organization.
func seedAuthzCustomProperty(t *testing.T, s *isolatedServer, f *gqlAuthzFixture) {
	t.Helper()
	s.store.UpsertCustomProperty(f.propsOrg.Login, &store.CustomProperty{
		PropertyName: "authz-prop", ValueType: "string", ValuesEditableBy: "org_actors",
	})
}

// seedAuthzVerifiableDomain adds the domain the approve/verify/regenerate/
// delete rows address to the seeded organization.
func seedAuthzVerifiableDomain(t *testing.T, s *isolatedServer, f *gqlAuthzFixture) {
	t.Helper()
	domain, err := s.store.CreateVerifiableDomain(store.VerifiableDomainOwnerOrganization, f.propsOrg.ID, "authz.example.com")
	if err != nil || domain == nil {
		t.Fatalf("fixture verifiable domain could not be created: %v", err)
	}
	f.domainNodeID = domain.NodeID
}

// seedAuthzEnvironment seeds one environment on the fixture repository and
// records its node id on the fixture.
func seedAuthzEnvironment(t *testing.T, s *isolatedServer, f *gqlAuthzFixture) *store.Environment {
	t.Helper()
	env := s.store.Deployments.UpsertEnvironment(f.repo.ID, "authz-env")
	if env == nil {
		t.Fatal("fixture environment could not be created")
	}
	f.environmentNodeID = env.NodeID
	return env
}

// seedPendingDeploymentRun seeds a reviewer-protected environment and a
// workflow run waiting on it. The run carries no jobs: the path under test is
// the authorization and pending bookkeeping.
func seedPendingDeploymentRun(t *testing.T, s *isolatedServer, f *gqlAuthzFixture) string {
	t.Helper()
	env := seedAuthzEnvironment(t, s, f)
	wf := &store.Workflow{
		ID:           "authz-run-" + f.owner.Login,
		Name:         "authz-workflow",
		RunID:        700000 + f.repo.ID,
		Jobs:         map[string]*store.WorkflowJob{},
		Status:       store.WorkflowStatusWaiting,
		RepoFullName: f.repo.FullName,
		Ref:          "feature",
		Sha:          f.headSHA,
		CreatedAt:    fixedTestTime.UTC(),
		PendingDeployments: []*store.PendingDeployment{{
			EnvID: env.ID, EnvName: env.Name, WaitTimerStartedAt: fixedTestTime.UTC(),
		}},
	}
	s.store.Mu.Lock()
	s.store.Workflows[wf.ID] = wf
	s.store.WorkflowsByRunID[wf.RunID] = wf
	s.store.Mu.Unlock()
	return "WFR_" + wf.ID
}

func TestGraphQLMutationsRefuseAnUnrelatedAuthenticatedUser(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	// A fixture per case: several mutations destroy what the next addresses, so
	// a shared fixture would let one success mask later refusals.
	for _, tc := range gqlMutationCases {
		f := newGQLAuthzFixture(t, s.Server, "stranger-"+tc.name, true)
		if tc.setup != nil {
			tc.setup(t, s, f)
		}
		env := s.gqlAuthzPost(t, f.strangerToken, tc.doc, map[string]interface{}{"input": tc.input(f)})
		if len(gqlAuthzErrors(env)) == 0 {
			t.Errorf("%s: an account with no access to the repository was served: %v", tc.name, env)
		}
		// The refusal has to be a refusal, not an error reported after the
		// write landed.
		s.assertGQLFixtureUntouched(t, tc.name, f)
	}
}

func (s *isolatedServer) assertGQLFixtureUntouched(t *testing.T, what string, f *gqlAuthzFixture) {
	t.Helper()
	st := s.store
	if st.GetRepoByFullName(f.repo.FullName) == nil {
		t.Errorf("%s: the repository was deleted by a stranger", what)
		return
	}
	issue := st.GetIssue(f.issue.ID)
	switch {
	case issue == nil:
		t.Errorf("%s: the issue disappeared", what)
	case issue.State != "OPEN":
		t.Errorf("%s: issue state = %q, want OPEN", what, issue.State)
	case issue.Title != "fixture issue":
		t.Errorf("%s: issue title = %q, want the seeded title", what, issue.Title)
	case issue.Locked:
		t.Errorf("%s: the issue was locked by a stranger", what)
	case issue.RepoID != f.repo.ID:
		t.Errorf("%s: the issue was transferred by a stranger", what)
	case issue.PinnedAt != nil:
		t.Errorf("%s: the issue was pinned by a stranger", what)
	}
	if issue := st.GetIssue(f.issue.ID); issue != nil && len(issue.LabelIDs) != 0 {
		t.Errorf("%s: the issue was labeled by a stranger: %v", what, issue.LabelIDs)
	}
	// Only the deleteLinkedBranch row seeds a link, and its refusal has to
	// leave that link in place; every other row must not have gained one.
	wantLinks := 0
	if what == "deleteLinkedBranch" {
		wantLinks = 1
	}
	if issue := st.GetIssue(f.issue.ID); issue != nil && len(issue.LinkedBranches) != wantLinks {
		t.Errorf("%s: linked branches = %v, want %d", what, issue.LinkedBranches, wantLinks)
	}
	if c := st.GetComment(f.comment.ID); c == nil || c.MinimizedReason != "" {
		t.Errorf("%s: the comment was moderated by a stranger: %+v", what, c)
	}
	if d := st.GetDiscussion(f.discussion.ID); d == nil || d.Deleted || d.Title != "fixture discussion" || len(d.UpvoterIDs) != 0 {
		t.Errorf("%s: the discussion was changed by a stranger: %+v", what, d)
	}
	if dc := st.GetDiscussionComment(f.discComment.ID); dc == nil || dc.IsAnswer || dc.Body != "fixture answer" {
		t.Errorf("%s: the discussion comment was changed by a stranger: %+v", what, dc)
	}
	// The revertPullRequest setup merges the PR as its owner (only merged PRs
	// revert), so MERGED is its seeded state, not a stranger's write; the
	// refusal is proved by no revert PR having been opened.
	wantPRState := "OPEN"
	if what == "revertPullRequest" {
		wantPRState = "MERGED"
	}
	pr := st.GetPullRequest(f.pr.ID)
	switch {
	case pr == nil:
		t.Errorf("%s: the pull request disappeared", what)
	case pr.State != wantPRState:
		t.Errorf("%s: pull request state = %q, want %s", what, pr.State, wantPRState)
	case pr.Title != "fixture pr":
		t.Errorf("%s: pull request title = %q, want the seeded title", what, pr.Title)
	}
	if what == "revertPullRequest" {
		if prs := st.ListPullRequests(f.repo.ID, "all"); len(prs) != 1 {
			t.Errorf("%s: a stranger's refusal still opened a pull request: %d exist, want the fixture's 1", what, len(prs))
		}
	}
	if thread := st.PRReviewComments.GetThread(parsedThreadID(t, f.threadNodeID)); thread == nil || thread.IsResolved {
		t.Errorf("%s: the review thread was resolved by a stranger: %+v", what, thread)
	}
	if f.dependabotAlert != nil {
		alert := st.GetDependabotAlert(f.repo.FullName, f.dependabotAlert.Number)
		if alert == nil || alert.State != store.DependabotStateOpen {
			t.Errorf("%s: the Dependabot alert was dismissed by a stranger: %+v", what, alert)
		}
	}
}

func parsedThreadID(t *testing.T, nodeID string) int {
	t.Helper()
	id, ok := store.ParsePRReviewThreadNodeID(nodeID)
	if !ok {
		t.Fatalf("thread node id %q does not parse", nodeID)
	}
	return id
}

// TestGraphQLMutationsStillServeTheirEntitledCaller is the positive half: the
// same table, driven by the repository's owner against a fresh fixture each
// time, must succeed. A guard that refuses everybody is not a fix.
func TestGraphQLMutationsStillServeTheirEntitledCaller(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	for _, tc := range gqlMutationCases {
		f := newGQLAuthzFixture(t, s.Server, "owner-"+tc.name, true)
		if tc.setup != nil {
			tc.setup(t, s, f)
		}
		env := s.gqlAuthzPost(t, f.ownerToken, tc.doc, map[string]interface{}{"input": tc.input(f)})
		if errs := gqlAuthzErrors(env); len(errs) > 0 {
			t.Errorf("%s: the repository owner was refused: %v", tc.name, errs)
		}
	}
}

// TestGraphQLReadLevelMutationsServeAnOutsideContributor guards the other
// direction: filing issues, commenting, proposing and reviewing PRs are how
// outside contributors participate on a public repo, so demanding push would
// wall off the path REST keeps open.
func TestGraphQLReadLevelMutationsServeAnOutsideContributor(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	f := newGQLAuthzFixture(t, s.Server, "outside", false)

	for _, tc := range gqlMutationCases {
		switch tc.name {
		case "createIssue", "addComment", "createPullRequest", "addPullRequestReview":
		default:
			continue
		}
		env := s.gqlAuthzPost(t, f.strangerToken, tc.doc, map[string]interface{}{"input": tc.input(f)})
		if errs := gqlAuthzErrors(env); len(errs) > 0 {
			t.Errorf("%s: an outside contributor was refused on a public repository: %v", tc.name, errs)
		}
	}
}

// TestGraphQLDiscussionAnswerMutationsRequireAViewer covers the two mutations
// that skipped authentication. The resolvers are driven through the schema
// directly with a viewerless context — the state an unauthenticated path hands
// them.
func TestGraphQLDiscussionAnswerMutationsRequireAViewer(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	f := newGQLAuthzFixture(t, s.Server, "anon", true)

	for _, doc := range []string{
		`mutation($input:MarkDiscussionCommentAsAnswerInput!){markDiscussionCommentAsAnswer(input:$input){discussion{id}}}`,
		`mutation($input:UnmarkDiscussionCommentAsAnswerInput!){unmarkDiscussionCommentAsAnswer(input:$input){discussion{id}}}`,
	} {
		result := graphql.Do(graphql.Params{
			Schema:        s.graphql.Schema(),
			RequestString: doc,
			VariableValues: map[string]interface{}{
				"input": map[string]interface{}{"id": f.discComment.NodeID},
			},
			Context: context.Background(),
		})
		if len(result.Errors) == 0 {
			t.Errorf("a viewerless caller was served: %v", result.Data)
		}
	}
	if dc := s.store.GetDiscussionComment(f.discComment.ID); dc == nil || dc.IsAnswer {
		t.Errorf("a viewerless caller marked the answer: %+v", dc)
	}

	// And the endpoint itself still answers 401 without a credential.
	resp := s.post(t, "/api/graphql", "", map[string]interface{}{
		"query": `mutation{markDiscussionCommentAsAnswer(input:{id:"x"}){discussion{id}}}`,
	})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("anonymous /api/graphql status = %d, want 401", resp.StatusCode)
	}
}

// TestGraphQLMergePullRequestHonoursExpectedHeadOid covers the
// --match-head-commit interlock. The argument was accepted and ignored, so a
// client that named the commit it had reviewed merged whatever had landed since.
func TestGraphQLMergePullRequestHonoursExpectedHeadOid(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	doc := `mutation($input:MergePullRequestInput!){mergePullRequest(input:$input){clientMutationId}}`

	stale := newGQLAuthzFixture(t, s.Server, "headoid-stale", true)
	env := s.gqlAuthzPost(t, stale.ownerToken, doc, map[string]interface{}{
		"input": map[string]interface{}{
			"pullRequestId":   stale.pr.NodeID,
			"expectedHeadOid": "0000000000000000000000000000000000000000",
		},
	})
	if len(gqlAuthzErrors(env)) == 0 {
		t.Errorf("a stale expectedHeadOid merged anyway: %v", env)
	}
	if pr := s.store.GetPullRequest(stale.pr.ID); pr == nil || pr.State != "OPEN" {
		t.Errorf("pull request state after the refused merge = %+v, want OPEN", pr)
	}

	fresh := newGQLAuthzFixture(t, s.Server, "headoid-fresh", true)
	env = s.gqlAuthzPost(t, fresh.ownerToken, doc, map[string]interface{}{
		"input": map[string]interface{}{
			"pullRequestId":   fresh.pr.NodeID,
			"expectedHeadOid": fresh.headSHA,
		},
	})
	if errs := gqlAuthzErrors(env); len(errs) > 0 {
		t.Fatalf("the correct expectedHeadOid was refused: %v", errs)
	}
	if pr := s.store.GetPullRequest(fresh.pr.ID); pr == nil || pr.State != "MERGED" {
		t.Errorf("pull request state after the accepted merge = %+v, want MERGED", pr)
	}
}

// TestGraphQLMergePullRequestEnforcesRequiredChecks covers the admin-bypass
// merge path: with enforce_admins off, canMergePullRequest lets an admin merge
// a red PR that REST refuses, so GraphQL would still route around REST. The
// caller here is the owner, i.e. an admin.
func TestGraphQLMergePullRequestEnforcesRequiredChecks(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	f := newGQLAuthzFixture(t, s.Server, "requiredchecks", true)
	repoPath := "/api/v3/repos/" + f.repo.FullName
	resp := s.put(t, repoPath+"/branches/main/protection", f.ownerToken, map[string]interface{}{
		"required_status_checks": map[string]interface{}{"strict": true, "contexts": []string{"ci"}},
	})
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		t.Fatalf("protecting main = %d", resp.StatusCode)
	}
	resp.Body.Close()

	doc := `mutation($input:MergePullRequestInput!){mergePullRequest(input:$input){clientMutationId}}`
	vars := map[string]interface{}{"input": map[string]interface{}{"pullRequestId": f.pr.NodeID}}

	env := s.gqlAuthzPost(t, f.ownerToken, doc, vars)
	errs := gqlAuthzErrors(env)
	if len(errs) == 0 {
		t.Errorf("a red required check did not stop the merge: %v", env)
	} else if !strings.Contains(fmt.Sprint(errs[0]), "Required status check") {
		t.Errorf("merge refusal = %v, want the required-status-check message", errs[0])
	}
	if pr := s.store.GetPullRequest(f.pr.ID); pr == nil || pr.State != "OPEN" {
		t.Fatalf("pull request state after the refused merge = %+v, want OPEN", pr)
	}

	// Turn the required check green and the same caller merges.
	resp = s.post(t, repoPath+"/check-runs", f.ownerToken, map[string]interface{}{
		"name":       "ci",
		"head_sha":   f.headSHA,
		"status":     "completed",
		"conclusion": "success",
	})
	if resp.StatusCode != http.StatusCreated {
		resp.Body.Close()
		t.Fatalf("creating the check run = %d", resp.StatusCode)
	}
	resp.Body.Close()

	env = s.gqlAuthzPost(t, f.ownerToken, doc, vars)
	if errs := gqlAuthzErrors(env); len(errs) > 0 {
		t.Fatalf("a green required check still blocked the merge: %v", errs)
	}
	if pr := s.store.GetPullRequest(f.pr.ID); pr == nil || pr.State != "MERGED" {
		t.Errorf("pull request state after the permitted merge = %+v, want MERGED", pr)
	}
}

// TestGraphQLUpdateIssueAppliesItsTriageArguments covers the payload that
// reported success for work it had not done: labelIds, assigneeIds and
// milestoneId were accepted, dropped, and the unchanged issue returned as if
// they had been applied.
func TestGraphQLUpdateIssueAppliesItsTriageArguments(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	f := newGQLAuthzFixture(t, s.Server, "triage", true)
	doc := `mutation($input:UpdateIssueInput!){updateIssue(input:$input){issue{title}}}`

	env := s.gqlAuthzPost(t, f.ownerToken, doc, map[string]interface{}{
		"input": map[string]interface{}{
			"id":          f.issue.NodeID,
			"labelIds":    []interface{}{f.label.NodeID},
			"assigneeIds": []interface{}{f.stranger.NodeID},
			"milestoneId": f.milestone.NodeID,
		},
	})
	if errs := gqlAuthzErrors(env); len(errs) > 0 {
		t.Fatalf("updateIssue refused its own arguments: %v", errs)
	}
	issue := s.store.GetIssue(f.issue.ID)
	if issue == nil {
		t.Fatalf("issue disappeared")
	}
	if len(issue.LabelIDs) != 1 || issue.LabelIDs[0] != f.label.ID {
		t.Errorf("issue labels = %v, want [%d]", issue.LabelIDs, f.label.ID)
	}
	if len(issue.AssigneeIDs) != 1 || issue.AssigneeIDs[0] != f.stranger.ID {
		t.Errorf("issue assignees = %v, want [%d]", issue.AssigneeIDs, f.stranger.ID)
	}
	if issue.MilestoneID != f.milestone.ID {
		t.Errorf("issue milestone = %d, want %d", issue.MilestoneID, f.milestone.ID)
	}

	// An id that names nothing is refused rather than dropped.
	env = s.gqlAuthzPost(t, f.ownerToken, doc, map[string]interface{}{
		"input": map[string]interface{}{
			"id":       f.issue.NodeID,
			"labelIds": []interface{}{"LA_kgDOnosuchlabel"},
		},
	})
	if len(gqlAuthzErrors(env)) == 0 {
		t.Errorf("an unresolvable labelId was accepted: %v", env)
	}
	if issue := s.store.GetIssue(f.issue.ID); issue == nil || len(issue.LabelIDs) != 1 {
		t.Errorf("the refused update still changed the labels: %+v", issue)
	}
}

// TestGraphQLUpdateIssueValidatesState covers the free-form string that was
// upper-cased and written straight into the store while the IssueState enum sat
// unused in the same file.
func TestGraphQLUpdateIssueValidatesState(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	f := newGQLAuthzFixture(t, s.Server, "state", true)
	doc := `mutation($input:UpdateIssueInput!){updateIssue(input:$input){issue{title}}}`

	env := s.gqlAuthzPost(t, f.ownerToken, doc, map[string]interface{}{
		"input": map[string]interface{}{"id": f.issue.NodeID, "state": "banana"},
	})
	if len(gqlAuthzErrors(env)) == 0 {
		t.Errorf("an invalid IssueState was accepted: %v", env)
	}
	if issue := s.store.GetIssue(f.issue.ID); issue == nil || issue.State != "OPEN" {
		t.Errorf("issue state after the refused update = %+v, want OPEN", issue)
	}

	env = s.gqlAuthzPost(t, f.ownerToken, doc, map[string]interface{}{
		"input": map[string]interface{}{"id": f.issue.NodeID, "state": "CLOSED"},
	})
	if errs := gqlAuthzErrors(env); len(errs) > 0 {
		t.Fatalf("a valid IssueState was refused: %v", errs)
	}
	issue := s.store.GetIssue(f.issue.ID)
	if issue == nil || issue.State != "CLOSED" {
		t.Fatalf("issue state after CLOSED = %+v", issue)
	}
	if issue.ClosedAt == nil || issue.StateReason != "COMPLETED" {
		t.Errorf("closing through updateIssue left the issue half-closed: %+v", issue)
	}
}

func TestGraphQLEveryMutationIsCoveredByThePolicyTable(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	schema := s.graphql.Schema()
	mutation := schema.MutationType()
	if mutation == nil {
		t.Fatalf("the schema exposes no mutation type")
	}
	fields := mutation.Fields()
	// The policy table itself lives in the graphqlapi package (ARCH-003).
	// TestMutationAuthzTableMatchesSchema there asserts schema fields and
	// table rows are identical sets, so iterating the schema's mutation
	// fields here iterates exactly the table's rows.
	if len(fields) == 0 {
		t.Fatalf("the schema exposes no mutations")
	}
	// Every mutation whose subject is an existing repo or project must be
	// exercised by a refusal case above. createRepository has no such subject
	// (its entitlement is over an account) and is covered by the account-scoped
	// cases; TestMutationAuthzAccountScopedRowsArePinned pins it as the only
	// such row, so this exemption cannot drift silently.
	inCases := map[string]bool{"createRepository": true}
	for _, tc := range gqlMutationCases {
		inCases[tc.name] = true
	}
	for _, tc := range gqlProjectMutationCases {
		inCases[tc.name] = true
	}
	// The rest of the mutation surface has its own refusal/entitled tables.
	for _, tc := range gqlSurfaceMutationCases {
		inCases[tc.name] = true
	}
	// Its account-scoped half drives a credential without a grant over the
	// account.
	for _, tc := range gqlAccountMutationCases {
		inCases[tc.name] = true
	}
	// The issue family's repository-scoped and organization-scoped halves.
	for _, tc := range gqlIssueMutationCases {
		inCases[tc.name] = true
	}
	for _, tc := range gqlIssueOrgMutationCases {
		inCases[tc.name] = true
	}
	// The pull-request family. updateTeamReviewAssignment names a team rather
	// than a repository, so its refusal is the organization-scoped one in
	// TestGraphQLUpdateTeamReviewAssignment.
	for _, tc := range gqlPullMutationCases {
		inCases[tc.name] = true
	}
	inCases["updateTeamReviewAssignment"] = true
	// The activity and account-policy family: repo-scoped and org-scoped halves.
	for _, tc := range gqlActivityMutationCases {
		inCases[tc.name] = true
	}
	for _, tc := range gqlActivityOrgMutationCases {
		inCases[tc.name] = true
	}
	// Enterprise mutations: cross-tenant refusal table in
	// gh_enterprise_graphql_test.go (a different enterprise's owner refuses).
	for _, tc := range allGQLEnterpriseMutationCases() {
		inCases[tc.name] = true
	}
	// Sponsors mutations: account-scoped refusal table in gh_sponsors_test.go.
	for _, tc := range gqlSponsorsMutationCases() {
		inCases[tc.name] = true
	}
	// Migration mutations: cross-tenant refusal table in
	// gh_migrations_gei_test.go.
	for _, tc := range gqlMigrationMutationCases() {
		inCases[tc.name] = true
	}
	for name := range fields {
		if inCases[name] {
			continue
		}
		t.Errorf("mutation %s is authorized but no refusal case exercises it", name)
	}
}

// Projects v2
//
// A project is owned by a user or org, so repository predicates say nothing
// about it. These mutations authenticated then acted: any signed-in account
// created projects under anybody's name and pulled a stranger's private issue
// into a project to read its title.

type gqlProjectAuthzFixture struct {
	owner         *store.User
	ownerToken    string
	stranger      *store.User
	strangerToken string
	project       *store.ProjectV2
	item          *store.ProjectV2Item
	field         *store.ProjectV2Field
	issue         *store.Issue
	spareIssue    *store.Issue
	strangerIssue *store.Issue
	org           *store.Org
	// Subjects the rest of the Projects v2 surface names (links, views, status
	// updates, workflows).
	ownerRepo    *store.Repo
	draftItem    *store.ProjectV2Item
	view         *store.ProjectV2View
	statusUpdate *store.ProjectV2StatusUpdate
	workflow     *store.ProjectV2Workflow
	// A team link is only meaningful on an organization-owned project, so the
	// team cases act on orgProject rather than the user-owned one.
	orgProject *store.ProjectV2
	team       *store.Team
	issueField *store.IssueField
}

func (s *isolatedServer) newGQLProjectAuthzFixture(t *testing.T, tag string) *gqlProjectAuthzFixture {
	t.Helper()
	st := s.store
	now := fixedTestTime.UTC()

	mkUser := func(login string) *store.User {
		st.Mu.Lock()
		defer st.Mu.Unlock()
		u := &store.User{
			ID:        st.NextUser,
			NodeID:    fmt.Sprintf("U_pauthz%08d", st.NextUser),
			Login:     login,
			Type:      "User",
			CreatedAt: now,
			UpdatedAt: now,
		}
		st.Users[u.ID] = u
		st.UsersByLogin[u.Login] = u
		st.NextUser++
		return u
	}

	f := &gqlProjectAuthzFixture{}
	f.owner = mkUser("gqlpauthz-owner-" + tag)
	f.stranger = mkUser("gqlpauthz-stranger-" + tag)

	ownerRepo := st.CreateRepo(f.owner, "gqlpauthz-repo", "", false)
	strangerRepo := st.CreateRepo(f.stranger, "gqlpauthz-secret", "", true)
	if ownerRepo == nil || strangerRepo == nil {
		t.Fatalf("fixture %s: could not create the repositories", tag)
	}
	f.issue = st.CreateIssue(ownerRepo.ID, f.owner.ID, "project fixture issue", "", nil, nil, 0)
	// A second readable issue: AddItem is idempotent per content, so re-adding
	// the existing item could not distinguish a refusal from a duplicate.
	f.spareIssue = st.CreateIssue(ownerRepo.ID, f.owner.ID, "project spare issue", "", nil, nil, 0)
	f.strangerIssue = st.CreateIssue(strangerRepo.ID, f.stranger.ID, "not yours", "", nil, nil, 0)

	f.project = st.ProjectsV2.CreateProject(f.owner.ID, "User", "fixture project", f.owner.ID)
	if f.project == nil {
		t.Fatalf("fixture %s: could not create the project", tag)
	}
	f.item = st.ProjectsV2.AddItem(f.project.ID, "Issue", f.issue.ID, f.owner.ID)
	f.field = st.ProjectsV2.CreateField(f.project.ID, "Notes", store.ProjectV2FieldText, nil, nil)
	if f.item == nil || f.field == nil {
		t.Fatalf("fixture %s: could not seed the project item or field", tag)
	}

	f.org = st.CreateOrg(f.owner, "gqlpauthz-org-"+tag, "Project Org", "")
	if f.org == nil {
		t.Fatalf("fixture %s: could not create the organization", tag)
	}

	f.ownerRepo = ownerRepo
	f.draftItem = st.ProjectsV2.AddDraftItem(f.project.ID, "a draft", "body", f.owner.ID)
	// Every project is seeded with a table view and the default workflows, so
	// the view and workflow subjects come from the project itself.
	views := st.ProjectsV2.ViewsForProject(f.project.ID)
	workflows := st.ProjectsV2.WorkflowsForProject(f.project.ID)
	if len(views) == 0 || len(workflows) == 0 {
		t.Fatalf("fixture %s: the project was created without its seeded view and workflows", tag)
	}
	f.view = views[0]
	f.workflow = workflows[0]
	f.statusUpdate = st.ProjectsV2.CreateStatusUpdate(f.project.ID, f.owner.ID, "on track", "ON_TRACK", "", "")
	if f.draftItem == nil || f.statusUpdate == nil {
		t.Fatalf("fixture %s: could not seed the draft item or status update", tag)
	}

	f.orgProject = st.ProjectsV2.CreateProject(f.org.ID, "Organization", "org fixture project", f.owner.ID)
	f.team = st.CreateTeam(f.org.Login, "gqlpauthz-team", store.TeamOptions{})
	if f.orgProject == nil || f.team == nil {
		t.Fatalf("fixture %s: could not seed the org project or team", tag)
	}
	optionName, optionColor := "Todo", "GRAY"
	f.issueField = st.CreateIssueField(f.org.Login, "Stage-"+tag, nil, "single_select", "private",
		[]store.IssueFieldOptionRequest{{Name: &optionName, Color: &optionColor}})
	if f.issueField == nil {
		t.Fatalf("fixture %s: could not seed the organization issue field", tag)
	}

	ownerTok := st.CreateToken(f.owner.ID, "repo, project")
	strangerTok := st.CreateToken(f.stranger.ID, "repo, project")
	if ownerTok == nil || strangerTok == nil {
		t.Fatalf("fixture %s: could not mint tokens", tag)
	}
	f.ownerToken = ownerTok.Value
	f.strangerToken = strangerTok.Value
	return f
}

type gqlProjectMutationCase struct {
	name  string
	doc   string
	input func(f *gqlProjectAuthzFixture) map[string]interface{}
}

var gqlProjectMutationCases = []gqlProjectMutationCase{
	{
		name: "createProjectV2",
		doc:  `mutation($input:CreateProjectV2Input!){createProjectV2(input:$input){projectV2{id}}}`,
		input: func(f *gqlProjectAuthzFixture) map[string]interface{} {
			return map[string]interface{}{"ownerId": f.owner.NodeID, "title": "from the table"}
		},
	},
	{
		name: "addProjectV2ItemById",
		doc:  `mutation($input:AddProjectV2ItemByIdInput!){addProjectV2ItemById(input:$input){item{id}}}`,
		input: func(f *gqlProjectAuthzFixture) map[string]interface{} {
			return map[string]interface{}{"projectId": f.project.NodeID, "contentId": f.spareIssue.NodeID}
		},
	},
	{
		name: "deleteProjectV2Item",
		doc:  `mutation($input:DeleteProjectV2ItemInput!){deleteProjectV2Item(input:$input){deletedItemId}}`,
		input: func(f *gqlProjectAuthzFixture) map[string]interface{} {
			return map[string]interface{}{"projectId": f.project.NodeID, "itemId": f.item.NodeID}
		},
	},
	{
		name: "createProjectV2Field",
		doc:  `mutation($input:CreateProjectV2FieldInput!){createProjectV2Field(input:$input){projectV2Field{... on ProjectV2FieldCommon{id}}}}`,
		input: func(f *gqlProjectAuthzFixture) map[string]interface{} {
			return map[string]interface{}{"projectId": f.project.NodeID, "dataType": "TEXT", "name": "from the table"}
		},
	},
	{
		name: "updateProjectV2ItemFieldValue",
		doc:  `mutation($input:UpdateProjectV2ItemFieldValueInput!){updateProjectV2ItemFieldValue(input:$input){projectV2Item{id}}}`,
		input: func(f *gqlProjectAuthzFixture) map[string]interface{} {
			return map[string]interface{}{
				"projectId": f.project.NodeID,
				"itemId":    f.item.NodeID,
				"fieldId":   f.field.NodeID,
				"value":     map[string]interface{}{"text": "from the table"},
			}
		},
	},
	{
		name: "updateProjectV2",
		doc:  `mutation($input:UpdateProjectV2Input!){updateProjectV2(input:$input){projectV2{id}}}`,
		input: func(f *gqlProjectAuthzFixture) map[string]interface{} {
			return map[string]interface{}{"projectId": f.project.NodeID, "title": "renamed by the table"}
		},
	},
	{
		name: "deleteProjectV2",
		doc:  `mutation($input:DeleteProjectV2Input!){deleteProjectV2(input:$input){projectV2{id}}}`,
		input: func(f *gqlProjectAuthzFixture) map[string]interface{} {
			return map[string]interface{}{"projectId": f.project.NodeID}
		},
	},
	{
		name: "copyProjectV2",
		doc:  `mutation($input:CopyProjectV2Input!){copyProjectV2(input:$input){projectV2{id}}}`,
		input: func(f *gqlProjectAuthzFixture) map[string]interface{} {
			return map[string]interface{}{"projectId": f.project.NodeID, "ownerId": f.owner.NodeID, "title": "a copy"}
		},
	},
	{
		name: "markProjectV2AsTemplate",
		doc:  `mutation($input:MarkProjectV2AsTemplateInput!){markProjectV2AsTemplate(input:$input){projectV2{id}}}`,
		input: func(f *gqlProjectAuthzFixture) map[string]interface{} {
			return map[string]interface{}{"projectId": f.project.NodeID}
		},
	},
	{
		name: "unmarkProjectV2AsTemplate",
		doc:  `mutation($input:UnmarkProjectV2AsTemplateInput!){unmarkProjectV2AsTemplate(input:$input){projectV2{id}}}`,
		input: func(f *gqlProjectAuthzFixture) map[string]interface{} {
			return map[string]interface{}{"projectId": f.project.NodeID}
		},
	},
	{
		name: "linkProjectV2ToRepository",
		doc:  `mutation($input:LinkProjectV2ToRepositoryInput!){linkProjectV2ToRepository(input:$input){repository{id}}}`,
		input: func(f *gqlProjectAuthzFixture) map[string]interface{} {
			return map[string]interface{}{"projectId": f.project.NodeID, "repositoryId": f.ownerRepo.NodeID}
		},
	},
	{
		name: "unlinkProjectV2FromRepository",
		doc:  `mutation($input:UnlinkProjectV2FromRepositoryInput!){unlinkProjectV2FromRepository(input:$input){repository{id}}}`,
		input: func(f *gqlProjectAuthzFixture) map[string]interface{} {
			return map[string]interface{}{"projectId": f.project.NodeID, "repositoryId": f.ownerRepo.NodeID}
		},
	},
	{
		name: "linkProjectV2ToTeam",
		doc:  `mutation($input:LinkProjectV2ToTeamInput!){linkProjectV2ToTeam(input:$input){team{id}}}`,
		input: func(f *gqlProjectAuthzFixture) map[string]interface{} {
			return map[string]interface{}{"projectId": f.orgProject.NodeID, "teamId": f.team.NodeID}
		},
	},
	{
		name: "unlinkProjectV2FromTeam",
		doc:  `mutation($input:UnlinkProjectV2FromTeamInput!){unlinkProjectV2FromTeam(input:$input){team{id}}}`,
		input: func(f *gqlProjectAuthzFixture) map[string]interface{} {
			return map[string]interface{}{"projectId": f.orgProject.NodeID, "teamId": f.team.NodeID}
		},
	},
	{
		name: "updateProjectV2Collaborators",
		doc:  `mutation($input:UpdateProjectV2CollaboratorsInput!){updateProjectV2Collaborators(input:$input){collaborators(first:5){totalCount}}}`,
		input: func(f *gqlProjectAuthzFixture) map[string]interface{} {
			return map[string]interface{}{
				"projectId":     f.project.NodeID,
				"collaborators": []interface{}{map[string]interface{}{"userId": f.stranger.NodeID, "role": "READER"}},
			}
		},
	},
	{
		name: "addProjectV2DraftIssue",
		doc:  `mutation($input:AddProjectV2DraftIssueInput!){addProjectV2DraftIssue(input:$input){projectItem{id}}}`,
		input: func(f *gqlProjectAuthzFixture) map[string]interface{} {
			return map[string]interface{}{"projectId": f.project.NodeID, "title": "from the table"}
		},
	},
	{
		name: "updateProjectV2DraftIssue",
		doc:  `mutation($input:UpdateProjectV2DraftIssueInput!){updateProjectV2DraftIssue(input:$input){draftIssue{id}}}`,
		input: func(f *gqlProjectAuthzFixture) map[string]interface{} {
			return map[string]interface{}{"draftIssueId": f.draftItem.NodeID, "title": "renamed by the table"}
		},
	},
	{
		name: "convertProjectV2DraftIssueItemToIssue",
		doc:  `mutation($input:ConvertProjectV2DraftIssueItemToIssueInput!){convertProjectV2DraftIssueItemToIssue(input:$input){item{id}}}`,
		input: func(f *gqlProjectAuthzFixture) map[string]interface{} {
			return map[string]interface{}{"itemId": f.draftItem.NodeID, "repositoryId": f.ownerRepo.NodeID}
		},
	},
	{
		name: "archiveProjectV2Item",
		doc:  `mutation($input:ArchiveProjectV2ItemInput!){archiveProjectV2Item(input:$input){item{id}}}`,
		input: func(f *gqlProjectAuthzFixture) map[string]interface{} {
			return map[string]interface{}{"projectId": f.project.NodeID, "itemId": f.item.NodeID}
		},
	},
	{
		name: "unarchiveProjectV2Item",
		doc:  `mutation($input:UnarchiveProjectV2ItemInput!){unarchiveProjectV2Item(input:$input){item{id}}}`,
		input: func(f *gqlProjectAuthzFixture) map[string]interface{} {
			return map[string]interface{}{"projectId": f.project.NodeID, "itemId": f.item.NodeID}
		},
	},
	{
		name: "clearProjectV2ItemFieldValue",
		doc:  `mutation($input:ClearProjectV2ItemFieldValueInput!){clearProjectV2ItemFieldValue(input:$input){projectV2Item{id}}}`,
		input: func(f *gqlProjectAuthzFixture) map[string]interface{} {
			return map[string]interface{}{"projectId": f.project.NodeID, "itemId": f.item.NodeID, "fieldId": f.field.NodeID}
		},
	},
	{
		name: "updateProjectV2ItemPosition",
		doc:  `mutation($input:UpdateProjectV2ItemPositionInput!){updateProjectV2ItemPosition(input:$input){items(first:5){totalCount}}}`,
		input: func(f *gqlProjectAuthzFixture) map[string]interface{} {
			return map[string]interface{}{"projectId": f.project.NodeID, "itemId": f.item.NodeID, "afterId": f.draftItem.NodeID}
		},
	},
	{
		name: "updateProjectV2Field",
		doc:  `mutation($input:UpdateProjectV2FieldInput!){updateProjectV2Field(input:$input){projectV2Field{... on ProjectV2FieldCommon{id}}}}`,
		input: func(f *gqlProjectAuthzFixture) map[string]interface{} {
			return map[string]interface{}{"fieldId": f.field.NodeID, "name": "renamed by the table"}
		},
	},
	{
		name: "deleteProjectV2Field",
		doc:  `mutation($input:DeleteProjectV2FieldInput!){deleteProjectV2Field(input:$input){projectV2Field{... on ProjectV2FieldCommon{id}}}}`,
		input: func(f *gqlProjectAuthzFixture) map[string]interface{} {
			return map[string]interface{}{"fieldId": f.field.NodeID}
		},
	},
	{
		name: "createProjectV2IssueField",
		doc:  `mutation($input:CreateProjectV2IssueFieldInput!){createProjectV2IssueField(input:$input){projectV2Field{... on ProjectV2FieldCommon{id}}}}`,
		input: func(f *gqlProjectAuthzFixture) map[string]interface{} {
			return map[string]interface{}{"projectId": f.orgProject.NodeID, "issueFieldId": f.issueField.NodeID}
		},
	},
	{
		name: "createProjectV2View",
		doc:  `mutation($input:CreateProjectV2ViewInput!){createProjectV2View(input:$input){projectV2View{id}}}`,
		input: func(f *gqlProjectAuthzFixture) map[string]interface{} {
			return map[string]interface{}{"projectId": f.project.NodeID, "name": "from the table", "layout": "BOARD_LAYOUT"}
		},
	},
	{
		name: "updateProjectV2View",
		doc:  `mutation($input:UpdateProjectV2ViewInput!){updateProjectV2View(input:$input){projectV2View{id}}}`,
		input: func(f *gqlProjectAuthzFixture) map[string]interface{} {
			return map[string]interface{}{"viewId": f.view.NodeID, "name": "renamed by the table"}
		},
	},
	{
		name: "deleteProjectV2View",
		doc:  `mutation($input:DeleteProjectV2ViewInput!){deleteProjectV2View(input:$input){projectV2View{id}}}`,
		input: func(f *gqlProjectAuthzFixture) map[string]interface{} {
			return map[string]interface{}{"viewId": f.view.NodeID}
		},
	},
	{
		name: "createProjectV2StatusUpdate",
		doc:  `mutation($input:CreateProjectV2StatusUpdateInput!){createProjectV2StatusUpdate(input:$input){statusUpdate{id}}}`,
		input: func(f *gqlProjectAuthzFixture) map[string]interface{} {
			return map[string]interface{}{"projectId": f.project.NodeID, "body": "from the table", "status": "ON_TRACK"}
		},
	},
	{
		name: "updateProjectV2StatusUpdate",
		doc:  `mutation($input:UpdateProjectV2StatusUpdateInput!){updateProjectV2StatusUpdate(input:$input){statusUpdate{id}}}`,
		input: func(f *gqlProjectAuthzFixture) map[string]interface{} {
			return map[string]interface{}{"statusUpdateId": f.statusUpdate.NodeID, "body": "edited by the table"}
		},
	},
	{
		name: "deleteProjectV2StatusUpdate",
		doc:  `mutation($input:DeleteProjectV2StatusUpdateInput!){deleteProjectV2StatusUpdate(input:$input){deletedStatusUpdateId}}`,
		input: func(f *gqlProjectAuthzFixture) map[string]interface{} {
			return map[string]interface{}{"statusUpdateId": f.statusUpdate.NodeID}
		},
	},
	{
		name: "deleteProjectV2Workflow",
		doc:  `mutation($input:DeleteProjectV2WorkflowInput!){deleteProjectV2Workflow(input:$input){deletedWorkflowId}}`,
		input: func(f *gqlProjectAuthzFixture) map[string]interface{} {
			return map[string]interface{}{"workflowId": f.workflow.NodeID}
		},
	},
}

func TestGraphQLProjectV2MutationsRefuseAnUnrelatedAuthenticatedUser(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	st := s.store
	for _, tc := range gqlProjectMutationCases {
		f := s.newGQLProjectAuthzFixture(t, "stranger-"+tc.name)
		projects := len(st.ProjectsV2.ListProjectsForOwner(f.owner.ID, "User"))
		items := len(st.ProjectsV2.ListItemsForProject(f.project.ID))
		fields := len(st.ProjectsV2.FieldsForProject(f.project.ID))
		values := len(st.ProjectsV2.GetItem(f.item.ID).FieldValues)

		env := s.gqlAuthzPost(t, f.strangerToken, tc.doc, map[string]interface{}{"input": tc.input(f)})
		if len(gqlAuthzErrors(env)) == 0 {
			t.Errorf("%s: an account with no access to the project was served: %v", tc.name, env)
		}

		if got := len(st.ProjectsV2.ListProjectsForOwner(f.owner.ID, "User")); got != projects {
			t.Errorf("%s: owner project count %d → %d", tc.name, projects, got)
		}
		if got := len(st.ProjectsV2.ListItemsForProject(f.project.ID)); got != items {
			t.Errorf("%s: project item count %d → %d", tc.name, items, got)
		}
		if got := len(st.ProjectsV2.FieldsForProject(f.project.ID)); got != fields {
			t.Errorf("%s: project field count %d → %d", tc.name, fields, got)
		}
		if got := len(st.ProjectsV2.GetItem(f.item.ID).FieldValues); got != values {
			t.Errorf("%s: item field-value count %d → %d", tc.name, values, got)
		}
	}
}

func TestGraphQLProjectV2MutationsStillServeTheirEntitledCaller(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	for _, tc := range gqlProjectMutationCases {
		f := s.newGQLProjectAuthzFixture(t, "owner-"+tc.name)
		env := s.gqlAuthzPost(t, f.ownerToken, tc.doc, map[string]interface{}{"input": tc.input(f)})
		if errs := gqlAuthzErrors(env); len(errs) > 0 {
			t.Errorf("%s: the project's owner was refused: %v", tc.name, errs)
		}
	}
}

// TestGraphQLCreateProjectV2HonoursOrganizationMembership covers the owner
// branch the user-owned cases cannot reach: a project under an organization is
// for its members, and membership is what decides — not merely holding a token.
func TestGraphQLCreateProjectV2HonoursOrganizationMembership(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	f := s.newGQLProjectAuthzFixture(t, "orgowner")
	doc := `mutation($input:CreateProjectV2Input!){createProjectV2(input:$input){projectV2{id}}}`
	input := map[string]interface{}{"ownerId": f.org.NodeID, "title": "org project"}

	// The fixture already owns an org project, so assert the delta, not a count.
	before := len(s.store.ProjectsV2.ListProjectsForOwner(f.org.ID, "Organization"))

	env := s.gqlAuthzPost(t, f.strangerToken, doc, map[string]interface{}{"input": input})
	if len(gqlAuthzErrors(env)) == 0 {
		t.Errorf("a non-member created a project under the organization: %v", env)
	}
	if got := len(s.store.ProjectsV2.ListProjectsForOwner(f.org.ID, "Organization")); got != before {
		t.Fatalf("organization project count after the refusal = %d, want %d", got, before)
	}

	// The organization's admin, who created it, still may.
	env = s.gqlAuthzPost(t, f.ownerToken, doc, map[string]interface{}{"input": input})
	if errs := gqlAuthzErrors(env); len(errs) > 0 {
		t.Fatalf("an organization admin was refused: %v", errs)
	}
	if got := len(s.store.ProjectsV2.ListProjectsForOwner(f.org.ID, "Organization")); got != before+1 {
		t.Errorf("organization project count after the accepted create = %d, want %d", got, before+1)
	}
}

// TestGraphQLAddProjectV2ItemRequiresReadingTheContent covers the second half of
// that mutation: the caller owns the project, so project write is satisfied,
// but the content belongs to a private repository they cannot read. Adding it
// would republish its title through the project.
func TestGraphQLAddProjectV2ItemRequiresReadingTheContent(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	f := s.newGQLProjectAuthzFixture(t, "content")
	doc := `mutation($input:AddProjectV2ItemByIdInput!){addProjectV2ItemById(input:$input){item{id}}}`

	env := s.gqlAuthzPost(t, f.ownerToken, doc, map[string]interface{}{
		"input": map[string]interface{}{"projectId": f.project.NodeID, "contentId": f.strangerIssue.NodeID},
	})
	if len(gqlAuthzErrors(env)) == 0 {
		t.Errorf("an unreadable issue was pulled into a project: %v", env)
	}
	if got := len(s.store.ProjectsV2.ListItemsForIssue(f.strangerIssue.ID)); got != 0 {
		t.Errorf("the private issue is indexed against %d project items, want 0", got)
	}

	// Content the caller can read still goes in.
	second := s.store.CreateIssue(f.issue.RepoID, f.owner.ID, "another readable issue", "", nil, nil, 0)
	env = s.gqlAuthzPost(t, f.ownerToken, doc, map[string]interface{}{
		"input": map[string]interface{}{"projectId": f.project.NodeID, "contentId": second.NodeID},
	})
	if errs := gqlAuthzErrors(env); len(errs) > 0 {
		t.Fatalf("readable content was refused: %v", errs)
	}
}

// TestGraphQLDeleteProjectV2ItemRemovesTheItem covers the mutation backing
// `gh project item-delete`: the owner deletes an item, the payload echoes the
// item's node id, and the item is gone from the project afterwards. An itemId
// naming an item outside the addressed project is not found.
func TestGraphQLDeleteProjectV2ItemRemovesTheItem(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	f := s.newGQLProjectAuthzFixture(t, "delitem")
	doc := `mutation($input:DeleteProjectV2ItemInput!){deleteProjectV2Item(input:$input){deletedItemId}}`

	// An item that belongs to another project is not this project's to delete.
	other := s.store.ProjectsV2.CreateProject(f.owner.ID, "User", "other", f.owner.ID)
	otherItem := s.store.ProjectsV2.AddItem(other.ID, "Issue", f.issue.ID, f.owner.ID)
	env := s.gqlAuthzPost(t, f.ownerToken, doc, map[string]interface{}{
		"input": map[string]interface{}{"projectId": f.project.NodeID, "itemId": otherItem.NodeID},
	})
	if len(gqlAuthzErrors(env)) == 0 {
		t.Errorf("an item from a different project was deletable: %v", env)
	}
	if s.store.ProjectsV2.GetItem(otherItem.ID) == nil {
		t.Errorf("the other project's item was deleted through the wrong project")
	}

	env = s.gqlAuthzPost(t, f.ownerToken, doc, map[string]interface{}{
		"input": map[string]interface{}{"projectId": f.project.NodeID, "itemId": f.item.NodeID},
	})
	if errs := gqlAuthzErrors(env); len(errs) > 0 {
		t.Fatalf("the owner was refused deleting their own item: %v", errs)
	}
	data, _ := env["data"].(map[string]interface{})
	payload, _ := data["deleteProjectV2Item"].(map[string]interface{})
	if got := payload["deletedItemId"]; got != f.item.NodeID {
		t.Errorf("deletedItemId = %v, want %q", got, f.item.NodeID)
	}
	if s.store.ProjectsV2.GetItem(f.item.ID) != nil {
		t.Errorf("the item survived the delete mutation")
	}
}

// TestGraphQLSubmitAndDismissPullRequestReview covers the review-lifecycle
// mutations end to end: a pending review is submitted (APPROVE → APPROVED) and
// then dismissed (→ DISMISSED with the message).
func TestGraphQLSubmitAndDismissPullRequestReview(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	s.createTestPRRepo(t, "gql-review-lifecycle")
	prJSON := decodeJSONWithStatus(t, s.post(t, "/api/v3/repos/admin/gql-review-lifecycle/pulls", defaultToken, map[string]interface{}{
		"title": "lifecycle", "head": "feat", "base": "main",
	}), 201)
	prID := int(prJSON["id"].(float64))
	admin := s.store.UsersByLogin["admin"]
	review := s.store.CreatePRReview(prID, admin.ID, "PENDING", "pending")
	if review == nil {
		t.Fatal("could not seed pending review")
	}

	data := s.gqlData(t, `mutation($input:SubmitPullRequestReviewInput!){submitPullRequestReview(input:$input){pullRequestReview{state}}}`,
		map[string]interface{}{"input": map[string]interface{}{"pullRequestReviewId": review.NodeID, "event": "APPROVE"}})
	submitted, _ := data["submitPullRequestReview"].(map[string]interface{})["pullRequestReview"].(map[string]interface{})
	if submitted["state"] != "APPROVED" {
		t.Errorf("submitted review state = %v, want APPROVED", submitted["state"])
	}

	data = s.gqlData(t, `mutation($input:DismissPullRequestReviewInput!){dismissPullRequestReview(input:$input){pullRequestReview{state}}}`,
		map[string]interface{}{"input": map[string]interface{}{"pullRequestReviewId": review.NodeID, "message": "no longer relevant"}})
	dismissed, _ := data["dismissPullRequestReview"].(map[string]interface{})["pullRequestReview"].(map[string]interface{})
	if dismissed["state"] != "DISMISSED" {
		t.Errorf("dismissed review state = %v, want DISMISSED", dismissed["state"])
	}
}
