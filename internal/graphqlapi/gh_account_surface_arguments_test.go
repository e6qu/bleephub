package graphqlapi

import (
	"testing"

	"github.com/e6qu/bleephub/internal/store"
)

// TestAccountSurfaceAcceptsGitHubsArgumentSets executes one document that
// passes every argument GitHub declares on the account-surface connections
// this package installs.
//
// A field that resolves correctly but declares fewer arguments than GitHub is
// still a broken field: an unmodified client sending `orderBy` or
// `affiliation` gets a document-validation error, which fails the whole
// request before a single resolver runs. This test is the guard for that
// class, which no per-field data assertion catches.
func TestAccountSurfaceAcceptsGitHubsArgumentSets(t *testing.T) {
	h := newAccountHarness(t)
	admin := h.store.UsersByLogin["admin"]
	org := h.store.CreateOrg(admin, "argorg", "Argument Org", "")
	if org == nil {
		t.Fatal("organization not created")
	}
	repo := h.store.CreateOrgRepo(org, admin, "arguments", "", false)
	if repo == nil {
		t.Fatal("repository not created")
	}
	if h.store.CreateTeam("argorg", "Owners", store.TeamOptions{Privacy: store.TeamPrivacyClosed}) == nil {
		t.Fatal("team not created")
	}

	// Every argument below is one GitHub declares on the same field.
	data := h.query(admin, `{
	  repository(owner:"argorg", name:"arguments") {
	    shortDescriptionHTML(limit: 40)
	    collaborators(first:10, affiliation: ALL, login:"admin", query:"adm") { totalCount }
	    mentionableUsers(first:10, query:"adm") { totalCount }
	    forks(first:10, orderBy:{field: PUSHED_AT, direction: DESC}, privacy: PUBLIC,
	          visibility: PUBLIC, isLocked:false) { totalCount }
	    deployKeys(last:5) { totalCount }
	    commitComments(first:5) { totalCount }
	    submodules(first:5) { totalCount }
	    codeowners(refName:"main") { errors { line } }
	    deployments(first:5, environments:["production"],
	                orderBy:{field: CREATED_AT, direction: DESC}) { totalCount }
	    environments(first:5, names:["production"],
	                 orderBy:{field: NAME, direction: ASC}) { totalCount }
	    environment(name:"production") { name }
	    milestone(number:1) { title }
	    label(name:"bug") { name }
	  }
	  organization(login:"argorg") {
	    membersWithRole(first:10) { totalCount }
	    pendingMembers(first:10) { totalCount }
	    teams(first:10, orderBy:{field: NAME, direction: ASC}, privacy: VISIBLE, query:"own",
	          role: ADMIN, rootTeamsOnly:true, userLogins:["admin"],
	          notificationSetting: NOTIFICATIONS_ENABLED) { totalCount }
	    team(slug:"owners") {
	      slug
	      members(first:10, membership: ALL, role: MEMBER, query:"adm") { totalCount }
	      repositories(first:10, orderBy:{field: NAME, direction: ASC}, query:"arg") { totalCount }
	      childTeams(first:10, immediateOnly:false, orderBy:{field: NAME, direction: ASC},
	                 userLogins:["admin"]) { totalCount }
	      ancestors(first:10) { totalCount }
	      avatarUrl(size: 80)
	    }
	    ipAllowListEntries(first:10, orderBy:{field: ALLOW_LIST_VALUE, direction: ASC}) { totalCount }
	    rulesets(first:10, includeParents:true, targets:[BRANCH]) { totalCount }
	    ruleset(databaseId:1, includeParents:true) { name }
	    repositoryCustomProperties(first:10) { totalCount }
	    repositoryCustomProperty(propertyName:"team") { propertyName }
	    pinnedItems(first:10, types:[REPOSITORY]) { totalCount }
	    pinnableItems(first:10, types:[REPOSITORY]) { totalCount }
	    anyPinnableItems(type: REPOSITORY)
	  }
	  user(login:"admin") {
	    followers(first:10) { totalCount }
	    following(first:10) { totalCount }
	    publicKeys(first:10) { totalCount }
	    socialAccounts(first:10) { totalCount }
	    gistComments(first:10) { totalCount }
	    commitComments(first:10) { totalCount }
	    issues(first:10, states:[OPEN], labels:["bug"],
	           orderBy:{field: CREATED_AT, direction: DESC}) { totalCount }
	    pullRequests(first:10, states:[OPEN], labels:["bug"], baseRefName:"main",
	                 headRefName:"topic", orderBy:{field: CREATED_AT, direction: DESC}) { totalCount }
	    issueComments(first:10, orderBy:{field: UPDATED_AT, direction: DESC}) { totalCount }
	    watching(first:10, affiliations:[OWNER], ownerAffiliations:[OWNER], privacy: PUBLIC,
	             visibility: PUBLIC, isLocked:false, hasIssuesEnabled:true,
	             orderBy:{field: PUSHED_AT, direction: DESC}) { totalCount }
	    repositoriesContributedTo(first:10, contributionTypes:[ISSUE, PULL_REQUEST],
	                              includeUserRepositories:true, hasIssues:true, isLocked:false,
	                              privacy: PUBLIC,
	                              orderBy:{field: NAME, direction: ASC}) { totalCount }
	    topRepositories(first:10, orderBy:{field: PUSHED_AT, direction: DESC}) { totalCount }
	    organization(login:"argorg") { login }
	    interactionAbility { limit }
	    pinnedItems(first:10, types:[REPOSITORY, GIST]) { totalCount }
	    pinnedItemsRemaining
	    itemShowcase { hasPinnedItems }
	  }
	}`, nil)

	// The document validated and executed; one spot check that it also
	// answered rather than returning an empty envelope.
	if got := at(t, data, "repository", "collaborators", "totalCount"); got != float64(1) {
		t.Errorf("collaborators totalCount = %v, want the one organization owner", got)
	}
	if got := at(t, data, "organization", "teams", "totalCount"); got != float64(0) {
		// role: ADMIN narrows to teams the viewer maintains, and the owner
		// maintains none.
		t.Errorf("teams(role: ADMIN) totalCount = %v, want 0", got)
	}
	if got := at(t, data, "user", "pinnedItemsRemaining"); got != float64(store.MaxPinnedRepos) {
		t.Errorf("pinnedItemsRemaining = %v, want %d", got, store.MaxPinnedRepos)
	}
}
