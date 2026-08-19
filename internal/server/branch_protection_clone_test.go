package bleephub

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"

	"github.com/e6qu/bleephub/internal/store"
)

// TestBranchProtectionCloneCoversEveryField keeps the clone in step with the
// stored model. A new field must be named here and, when it can carry shared
// state, exercised by the mutation check below.
func TestBranchProtectionCloneCoversEveryField(t *testing.T) {
	wantFields := map[string]bool{
		"RequiredStatusChecks":           true,
		"RequiredPullRequestReviews":     true,
		"EnforceAdmins":                  true,
		"Restrictions":                   true,
		"RequiredLinearHistory":          true,
		"AllowForcePushes":               true,
		"AllowDeletions":                 true,
		"BlockCreations":                 true,
		"RequiredConversationResolution": true,
		"RequiredSignatures":             true,
		"LockBranch":                     true,
		"AllowForkSyncing":               true,
		"URL":                            true,
	}
	model := reflect.TypeOf(store.BranchProtection{})
	if model.NumField() != len(wantFields) {
		t.Fatalf("BranchProtection has %d fields, clone guard names %d", model.NumField(), len(wantFields))
	}
	for i := 0; i < model.NumField(); i++ {
		if !wantFields[model.Field(i).Name] {
			t.Fatalf("BranchProtection field %s is not covered by the clone guard", model.Field(i).Name)
		}
	}

	appID := int64(42)
	original := &store.BranchProtection{
		RequiredStatusChecks: &store.BPStatusChecks{
			Contexts: []string{"ci"},
			Checks:   []store.BPCheck{{Context: "build", AppID: &appID}},
		},
		RequiredPullRequestReviews: &store.BPPullRequestReviews{
			DismissalRestrictions:       &store.BPRestrictions{Users: []store.BPActor{{Login: "reviewer"}}},
			BypassPullRequestAllowances: &store.BPBypassAllowances{Apps: []store.BPActor{{Login: "release-app"}}},
		},
		EnforceAdmins:                  &store.BPEnforceAdmins{Enabled: true},
		Restrictions:                   &store.BPRestrictions{Teams: []store.BPActor{{Login: "maintainers"}}},
		RequiredLinearHistory:          &store.BPEnabled{Enabled: true},
		AllowForcePushes:               &store.BPEnabled{Enabled: true},
		AllowDeletions:                 &store.BPEnabled{Enabled: true},
		BlockCreations:                 &store.BPEnabled{Enabled: true},
		RequiredConversationResolution: &store.BPEnabled{Enabled: true},
		RequiredSignatures:             &store.BPEnabledURL{Enabled: true},
		LockBranch:                     &store.BPEnabled{Enabled: true},
		AllowForkSyncing:               &store.BPEnabled{Enabled: true},
		URL:                            "https://one.example/protection",
	}
	before, err := json.Marshal(original)
	if err != nil {
		t.Fatal(err)
	}

	cloned := cloneBranchProtection(original)
	cloned.RequiredStatusChecks.Contexts[0] = "changed"
	*cloned.RequiredStatusChecks.Checks[0].AppID = 99
	cloned.RequiredPullRequestReviews.DismissalRestrictions.Users[0].Login = "changed"
	cloned.RequiredPullRequestReviews.BypassPullRequestAllowances.Apps[0].Login = "changed"
	cloned.EnforceAdmins.Enabled = false
	cloned.Restrictions.Teams[0].Login = "changed"
	cloned.RequiredLinearHistory.Enabled = false
	cloned.AllowForcePushes.Enabled = false
	cloned.AllowDeletions.Enabled = false
	cloned.BlockCreations.Enabled = false
	cloned.RequiredConversationResolution.Enabled = false
	cloned.RequiredSignatures.Enabled = false
	cloned.LockBranch.Enabled = false
	cloned.AllowForkSyncing.Enabled = false
	cloned.URL = "https://two.example/protection"

	after, err := json.Marshal(original)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Fatalf("mutating a branch-protection clone changed the stored value\nbefore: %s\nafter:  %s", before, after)
	}
}

func TestRestrictedPusherRecognizesInstallationApp(t *testing.T) {
	s := newTestServer()
	admin := s.store.LookupUserByLogin("admin")
	repo := s.store.CreateRepo(admin, "app-restricted-push", "", false)
	app := s.store.CreateApp(admin.ID, "Release App", "", map[string]string{"contents": "write"}, nil)
	token := &store.InstallationToken{AppID: app.ID}
	bot := &store.User{ID: -app.ID, Login: app.Slug + "[bot]", Type: "Bot"}
	ctx := context.WithValue(context.Background(), ctxInstallationToken, token)
	ctx = contextWithUser(ctx, bot)

	restrictions := &store.BPRestrictions{Apps: []store.BPActor{{Login: app.Slug, ID: app.ID}}}
	if !s.viewerIsRestrictedPusher(ctx, repo, restrictions) {
		t.Fatal("installation token was not recognized as its app in branch push restrictions")
	}
}
