package graphqlapi

import (
	"testing"
	"time"

	"github.com/e6qu/bleephub/internal/store"
)

// TestRepositorySettingsResolveFromTheStoredRow pins that the repository
// settings family answers from the repository row rather than a rendered
// source map: every value below is one the REST repository shape serves from
// the same field.
func TestRepositorySettingsResolveFromTheStoredRow(t *testing.T) {
	h := newAccountHarness(t)
	owner := h.store.UsersByLogin["admin"]
	repo := h.store.CreateRepo(owner, "settings", "A repository with **settings**", false)
	if repo == nil {
		t.Fatal("repository not created")
	}
	h.store.UpdateRepo("admin", "settings", func(r *store.Repo) {
		r.AllowAutoMerge = true
		r.AllowUpdateBranch = true
		r.UseSquashPRTitleAsDefault = true
		r.WebCommitSignoffRequired = true
		r.HasPullRequests = true
		r.MergeCommitTitle = "pr_title"
		r.MergeCommitMessage = "pr_body"
		r.SquashMergeCommitTitle = "pr_title"
		r.SquashMergeCommitMessage = "blank"
		r.AllowMergeCommit = false
		r.AllowSquashMerge = true
	})

	data := h.query(owner, `{
	  repository(owner:"admin", name:"settings") {
	    autoMergeAllowed
	    allowUpdateBranch
	    squashPrTitleUsedAsDefault
	    webCommitSignoffRequired
	    hasPullRequestsEnabled
	    mergeCommitTitle
	    mergeCommitMessage
	    squashMergeCommitTitle
	    squashMergeCommitMessage
	    isInOrganization
	    isDisabled
	    isMirror
	    mirrorUrl
	    isLocked
	    lockReason
	    descriptionHTML
	    viewerDefaultMergeMethod
	    usesCustomOpenGraphImage
	  }
	}`, nil)

	repoData, ok := at(t, data, "repository").(map[string]interface{})
	if !ok {
		t.Fatalf("repository payload is %T", at(t, data, "repository"))
	}
	for field, want := range map[string]interface{}{
		"autoMergeAllowed":           true,
		"allowUpdateBranch":          true,
		"squashPrTitleUsedAsDefault": true,
		"webCommitSignoffRequired":   true,
		"hasPullRequestsEnabled":     true,
		"mergeCommitTitle":           "PR_TITLE",
		"mergeCommitMessage":         "PR_BODY",
		"squashMergeCommitTitle":     "PR_TITLE",
		"squashMergeCommitMessage":   "BLANK",
		"isInOrganization":           false,
		"isDisabled":                 false,
		"isMirror":                   false,
		"mirrorUrl":                  nil,
		"isLocked":                   false,
		"lockReason":                 nil,
		"usesCustomOpenGraphImage":   false,
		// Merge commits are off and squash is on, so the button's first offer
		// is a squash merge.
		"viewerDefaultMergeMethod": "SQUASH",
	} {
		if got := repoData[field]; got != want {
			t.Errorf("%s = %#v, want %#v", field, got, want)
		}
	}
	// descriptionHTML runs the description through the one markdown pipeline
	// the rest of bleephub renders bodies with.
	html, _ := repoData["descriptionHTML"].(string)
	if html == "" || html == repo.Description {
		t.Errorf("descriptionHTML = %q, want the rendered markdown", html)
	}
}

// TestRepositoryIsInOrganizationFollowsOwnership pins the owner-kind
// discrimination, which is what a client uses to decide whether to ask for
// team-scoped fields at all.
func TestRepositoryIsInOrganizationFollowsOwnership(t *testing.T) {
	h := newAccountHarness(t)
	admin := h.store.UsersByLogin["admin"]
	org := h.store.CreateOrg(admin, "settingsorg", "Settings Org", "")
	if org == nil {
		t.Fatal("organization not created")
	}
	if h.store.CreateOrgRepo(org, admin, "owned", "", false) == nil {
		t.Fatal("organization repository not created")
	}

	data := h.query(admin, `{
	  repository(owner:"settingsorg", name:"owned") { isInOrganization isUserConfigurationRepository }
	}`, nil)
	if got := at(t, data, "repository", "isInOrganization"); got != true {
		t.Errorf("org repository isInOrganization = %v, want true", got)
	}
	if got := at(t, data, "repository", "isUserConfigurationRepository"); got != false {
		t.Errorf("org repository isUserConfigurationRepository = %v, want false", got)
	}

	if h.store.CreateRepo(admin, "admin", "profile", false) == nil {
		t.Fatal("profile repository not created")
	}
	profile := h.query(admin, `{
	  repository(owner:"admin", name:"admin") { isUserConfigurationRepository }
	}`, nil)
	if got := at(t, profile, "repository", "isUserConfigurationRepository"); got != true {
		t.Errorf("profile repository isUserConfigurationRepository = %v, want true", got)
	}
}

// TestRepositoryInteractionAbilityReportsTheActiveRestriction pins that an
// interaction limit is reported while it is in force and disappears once it
// expires — the same "no longer in effect" rule the REST interaction-limits
// endpoint applies.
func TestRepositoryInteractionAbilityReportsTheActiveRestriction(t *testing.T) {
	h := newAccountHarness(t)
	h.store.ClockNow = func() time.Time { return fixedTestTime }
	owner := h.store.UsersByLogin["admin"]
	repo := h.store.CreateRepo(owner, "limited", "", false)
	if repo == nil {
		t.Fatal("repository not created")
	}
	expiry := fixedTestTime.Add(24 * time.Hour)
	if !h.store.SetRepoInteractionLimit(repo.ID, "collaborators_only", &expiry) {
		t.Fatal("interaction limit not set")
	}

	document := `{
	  repository(owner:"admin", name:"limited") {
	    interactionAbility { limit origin expiresAt }
	  }
	}`
	data := h.query(owner, document, nil)
	if got := at(t, data, "repository", "interactionAbility", "limit"); got != "COLLABORATORS_ONLY" {
		t.Errorf("limit = %v, want COLLABORATORS_ONLY", got)
	}
	if got := at(t, data, "repository", "interactionAbility", "origin"); got != "REPOSITORY" {
		t.Errorf("origin = %v, want REPOSITORY", got)
	}

	// Past the expiry the restriction is not in effect, so the ability is
	// null rather than a stale limit.
	h.store.ClockNow = func() time.Time { return expiry.Add(time.Hour) }
	expired := h.query(owner, document, nil)
	if got := at(t, expired, "repository", "interactionAbility"); got != nil {
		t.Errorf("expired interactionAbility = %#v, want null", got)
	}
}

// TestRepositoryViewerFieldsAnswerPerViewer is the authorization test for the
// viewer* family: the owner of a repository and a stranger to it must receive
// different answers from the same query.
func TestRepositoryViewerFieldsAnswerPerViewer(t *testing.T) {
	h := newAccountHarness(t)
	owner := h.store.UsersByLogin["admin"]
	stranger := h.user("stranger")
	repo := h.store.CreateRepo(owner, "standing", "", false)
	if repo == nil {
		t.Fatal("repository not created")
	}
	if !h.store.StarRepo(owner.ID, "admin", "standing") {
		t.Fatal("star not recorded")
	}
	if !h.store.SetRepoSubscription(owner.ID, repo.ID, true, false) {
		t.Fatal("subscription not recorded")
	}

	document := `{
	  repository(owner:"admin", name:"standing") {
	    viewerCanAdminister
	    viewerCanCreateIssues
	    viewerCanUpdateTopics
	    viewerCanSubscribe
	    viewerHasStarred
	    viewerSubscription
	    viewerPermission
	  }
	}`

	ownerView := h.query(owner, document, nil)
	for field, want := range map[string]interface{}{
		"viewerCanAdminister":   true,
		"viewerCanCreateIssues": true,
		"viewerCanUpdateTopics": true,
		"viewerCanSubscribe":    true,
		"viewerHasStarred":      true,
		"viewerSubscription":    "SUBSCRIBED",
		"viewerPermission":      "ADMIN",
	} {
		if got := at(t, ownerView, "repository", field); got != want {
			t.Errorf("owner %s = %#v, want %#v", field, got, want)
		}
	}

	strangerView := h.query(stranger, document, nil)
	for field, want := range map[string]interface{}{
		"viewerCanAdminister":   false,
		"viewerCanCreateIssues": false,
		"viewerCanUpdateTopics": false,
		"viewerHasStarred":      false,
		"viewerSubscription":    nil,
		"viewerPermission":      "READ",
		// A signed-in account may watch a repository it can read.
		"viewerCanSubscribe": true,
	} {
		if got := at(t, strangerView, "repository", field); got != want {
			t.Errorf("stranger %s = %#v, want %#v", field, got, want)
		}
	}

	anonymous := h.query(nil, document, nil)
	for field, want := range map[string]interface{}{
		"viewerCanAdminister": false,
		"viewerCanSubscribe":  false,
		"viewerHasStarred":    false,
		"viewerSubscription":  nil,
	} {
		if got := at(t, anonymous, "repository", field); got != want {
			t.Errorf("anonymous %s = %#v, want %#v", field, got, want)
		}
	}
}
