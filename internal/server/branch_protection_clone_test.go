package bleephub

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"
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
		"URL":                            true,
	}
	model := reflect.TypeOf(BranchProtection{})
	if model.NumField() != len(wantFields) {
		t.Fatalf("BranchProtection has %d fields, clone guard names %d", model.NumField(), len(wantFields))
	}
	for i := 0; i < model.NumField(); i++ {
		if !wantFields[model.Field(i).Name] {
			t.Fatalf("BranchProtection field %s is not covered by the clone guard", model.Field(i).Name)
		}
	}

	appID := int64(42)
	original := &BranchProtection{
		RequiredStatusChecks: &BPStatusChecks{
			Contexts: []string{"ci"},
			Checks:   []BPCheck{{Context: "build", AppID: &appID}},
		},
		RequiredPullRequestReviews: &BPPullRequestReviews{
			DismissalRestrictions:       &BPRestrictions{Users: []BPActor{{Login: "reviewer"}}},
			BypassPullRequestAllowances: &BPBypassAllowances{Apps: []BPActor{{Login: "release-app"}}},
		},
		EnforceAdmins:                  &BPEnforceAdmins{Enabled: true},
		Restrictions:                   &BPRestrictions{Teams: []BPActor{{Login: "maintainers"}}},
		RequiredLinearHistory:          &BPEnabled{Enabled: true},
		AllowForcePushes:               &BPEnabled{Enabled: true},
		AllowDeletions:                 &BPEnabled{Enabled: true},
		BlockCreations:                 &BPEnabled{Enabled: true},
		RequiredConversationResolution: &BPEnabled{Enabled: true},
		RequiredSignatures:             &BPEnabledURL{Enabled: true},
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
	token := &InstallationToken{AppID: app.ID}
	bot := &User{ID: -app.ID, Login: app.Slug + "[bot]", Type: "Bot"}
	ctx := context.WithValue(context.Background(), ctxInstallationToken, token)
	ctx = contextWithUser(ctx, bot)

	restrictions := &BPRestrictions{Apps: []BPActor{{Login: app.Slug, ID: app.ID}}}
	if !s.viewerIsRestrictedPusher(ctx, repo, restrictions) {
		t.Fatal("installation token was not recognized as its app in branch push restrictions")
	}
}
