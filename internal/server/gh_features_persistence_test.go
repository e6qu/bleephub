package bleephub

import (
	"testing"

	"github.com/e6qu/bleephub/internal/store"
)

// TestFeatureStatePersistsAcrossRestart round-trips the durable state behind
// the wiki revision history, notification saved set, org pins, and primary
// email through a real persistence close/re-open.
func TestFeatureStatePersistsAcrossRestart(t *testing.T) {
	const repoKey = "admin/persist-features"
	// A wiki's durability is its git repository's durability: the pages and
	// their history are commits in `<repo>.wiki.git`, so this test needs the
	// same real git storage a repository's own commits need to survive a
	// restart, rather than the memory storer an unset git directory selects.
	t.Setenv("BLEEPHUB_GIT_DIR", t.TempDir())
	var adminID int
	var threadID string
	st2 := reloadedStore(t, func(p *store.Persistence, st *store.Store) {
		st.SeedDefaultUser()
		admin := st.UsersByLogin["admin"]
		adminID = admin.ID
		repo := st.CreateRepo(admin, "persist-features", "", false)

		st.UpsertWikiPage(repoKey, "home", "Home", "v1", "admin", "first")
		st.UpsertWikiPage(repoKey, "home", "Home", "v2", "admin", "second")

		issue := st.CreateIssue(repo.ID, admin.ID, "bookmark", "", nil, nil, 0)
		threadID = store.NotificationThreadID("Issue", issue.ID)
		st.SetThreadSaved(admin.ID, threadID, true)

		org := st.CreateOrg(admin, "persist-pin-org", "", "")
		st.CreateOrgRepo(org, admin, "pinme", "", false)
		if _, ok := st.SetOrgPinnedRepos("persist-pin-org", []string{"persist-pin-org/pinme"}); !ok {
			t.Fatal("SetOrgPinnedRepos failed")
		}

		if _, ok := st.AddUserEmails(admin.ID, []string{"promoted@bleephub.local"}); !ok {
			t.Fatal("AddUserEmails failed")
		}
		if _, result := st.SetPrimaryUserEmail(admin.ID, "promoted@bleephub.local"); result != store.SetPrimaryEmailOK {
			t.Fatalf("SetPrimaryUserEmail result = %v", result)
		}
	})

	revisions := st2.ListWikiPageRevisions(repoKey, "home")
	if len(revisions) != 2 || revisions[0].Body != "v2" || revisions[0].Message != "second" {
		t.Fatalf("reloaded revisions = %+v", revisions)
	}

	st2.Mu.RLock()
	saved := st2.NotificationsState[adminID] != nil && st2.NotificationsState[adminID].SavedThreadIDs[threadID]
	st2.Mu.RUnlock()
	if !saved {
		t.Fatal("saved thread set did not survive reload")
	}

	pins, ok := st2.ListOrgPinnedRepos("persist-pin-org")
	if !ok || len(pins) != 1 || pins[0] != "persist-pin-org/pinme" {
		t.Fatalf("reloaded org pins = %v (ok=%v)", pins, ok)
	}

	if u := st2.GetUserByID(adminID); u == nil || u.Email != "promoted@bleephub.local" {
		t.Fatalf("reloaded primary email = %v", u)
	}
}
