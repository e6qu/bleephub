package bleephub

import (
	"testing"
	"time"

	"github.com/e6qu/bleephub/internal/store"
)

// TestDeleteAppLeavesNoDanglingTokens covers the credential-safety fix (P2):
// DeleteApp used to delete the parent row and each credential bucket in
// separate transactions, so a crash mid-cascade could leave valid ghu_/ghs_/
// ghr_ tokens resolving for an app that no longer existed. The cascade now
// stages every child delete plus the parent into ONE batch (parent last).
func TestDeleteAppLeavesNoDanglingTokens(t *testing.T) {
	var appID int
	var instTok, userTok, refreshTok string

	st2 := reloadedStore(t, func(p *store.Persistence, st *store.Store) {
		st.SeedDefaultUser()
		user := st.UsersByLogin["admin"]

		app := st.CreateApp(user.ID, "cred-app", "", map[string]string{"contents": "read"}, nil)
		appID = app.ID

		inst := st.CreateInstallation(app.ID, "User", user.ID, user.Login, nil, nil)
		it := st.CreateInstallationToken(inst.ID, app.ID, nil, nil)
		instTok = it.Token

		ut, rt := st.CreateUserToServerToken(user.ID, app.ID, "", "", time.Hour, true)
		userTok = ut.Token
		if rt == nil {
			t.Fatal("expected a refresh token")
		}
		refreshTok = rt.Token

		// Sanity: tokens resolve before deletion.
		if tok, _ := st.LookupInstallationToken(instTok); tok == nil {
			t.Fatal("installation token did not resolve before delete")
		}
		if tok, _ := st.LookupUserToServerToken(userTok); tok == nil {
			t.Fatal("user-to-server token did not resolve before delete")
		}

		if !st.DeleteApp(app.ID) {
			t.Fatal("DeleteApp returned false")
		}

		// In-memory: everything for the app is gone immediately.
		if st.GetApp(appID) != nil {
			t.Error("app still present in memory after delete")
		}
		if tok, _ := st.LookupInstallationToken(instTok); tok != nil {
			t.Error("installation token still resolves in memory after delete")
		}
		if tok, _ := st.LookupUserToServerToken(userTok); tok != nil {
			t.Error("user-to-server token still resolves in memory after delete")
		}
	})

	// After reload, no durable row for the app or any of its tokens may
	// survive — the dangling-live-token window is closed.
	if st2.GetApp(appID) != nil {
		t.Error("app resurrected from persistence after delete")
	}
	if tok, _ := st2.LookupInstallationToken(instTok); tok != nil {
		t.Error("installation token resurrected from persistence (dangling ghs_ credential)")
	}
	if tok, _ := st2.LookupUserToServerToken(userTok); tok != nil {
		t.Error("user-to-server token resurrected from persistence (dangling ghu_ credential)")
	}
	st2.Mu.RLock()
	if _, ok := st2.RefreshTokens[refreshTok]; ok {
		t.Error("refresh token resurrected from persistence (dangling ghr_ credential)")
	}
	for tok, it := range st2.InstallationTokens {
		if it.AppID == appID {
			t.Errorf("installation token %s for deleted app survived reload", tok)
		}
	}
	for tok, ut := range st2.UserToServerTokens {
		if ut.AppID == appID {
			t.Errorf("user-to-server token %s for deleted app survived reload", tok)
		}
	}
	st2.Mu.RUnlock()
}
