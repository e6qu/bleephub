package bleephub

import (
	"fmt"
	"sync"
	"testing"

	"github.com/e6qu/bleephub/internal/store"
)

// TestStressCounterIntegrity hammers the store's monotonic ID allocators and
// idempotent operations from many goroutines and asserts the invariants
// concurrency must not break: every allocated ID is unique, an idempotent
// reaction POST yields exactly one reaction, and a membership upsert converges
// on a single row. A lost update on a Next* counter shows up as a duplicate ID.
func TestStressCounterIntegrity(t *testing.T) {
	s := newTestServer()
	st := s.store
	admin := st.LookupUserByLogin("admin")
	if admin == nil {
		t.Fatal("admin missing")
	}

	const workers = 32
	const perWorker = 40

	t.Run("ReserveRunID uniqueness", func(t *testing.T) {
		var mu sync.Mutex
		seen := make(map[int]bool, workers*perWorker)
		dups := 0
		var wg sync.WaitGroup
		for w := 0; w < workers; w++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				local := make([]int, 0, perWorker)
				for i := 0; i < perWorker; i++ {
					local = append(local, st.ReserveRunID())
				}
				mu.Lock()
				for _, id := range local {
					if seen[id] {
						dups++
					}
					seen[id] = true
				}
				mu.Unlock()
			}()
		}
		wg.Wait()
		if dups != 0 {
			t.Errorf("ReserveRunID handed out %d duplicate IDs across %d allocations", dups, workers*perWorker)
		}
		if len(seen) != workers*perWorker {
			t.Errorf("distinct run IDs = %d, want %d", len(seen), workers*perWorker)
		}
	})

	t.Run("repo and issue ID uniqueness", func(t *testing.T) {
		var mu sync.Mutex
		repoIDs := make(map[int]bool)
		issueIDs := make(map[int]bool)
		var wg sync.WaitGroup
		for w := 0; w < workers; w++ {
			wg.Add(1)
			go func(id int) {
				defer wg.Done()
				for i := 0; i < 4; i++ {
					repo := st.CreateRepo(admin, fmt.Sprintf("cnt-%d-%d", id, i), "", false)
					if repo == nil {
						continue
					}
					var localIssues []int
					for j := 0; j < 3; j++ {
						is := st.CreateIssue(repo.ID, admin.ID, "t", "b", nil, nil, 0)
						if is != nil {
							localIssues = append(localIssues, is.ID)
						}
					}
					mu.Lock()
					if repoIDs[repo.ID] {
						t.Errorf("duplicate repo ID %d", repo.ID)
					}
					repoIDs[repo.ID] = true
					for _, iid := range localIssues {
						if issueIDs[iid] {
							t.Errorf("duplicate issue ID %d", iid)
						}
						issueIDs[iid] = true
					}
					mu.Unlock()
				}
			}(w)
		}
		wg.Wait()
	})

	t.Run("reaction idempotency", func(t *testing.T) {
		repo := st.CreateRepo(admin, "reaction-idem", "", false)
		if repo == nil {
			t.Fatal("CreateRepo nil")
		}
		issue := st.CreateIssue(repo.ID, admin.ID, "react", "b", nil, nil, 0)
		if issue == nil {
			t.Fatal("CreateIssue nil")
		}
		var mu sync.Mutex
		ids := make(map[int]bool)
		var wg sync.WaitGroup
		for w := 0; w < workers; w++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for i := 0; i < perWorker; i++ {
					r, _, err := st.Reactions.AddReaction("issue", issue.ID, admin.ID, "+1")
					if err != nil {
						t.Errorf("AddReaction: %v", err)
						return
					}
					mu.Lock()
					ids[r.ID] = true
					mu.Unlock()
				}
			}()
		}
		wg.Wait()
		if len(ids) != 1 {
			t.Errorf("idempotent reaction produced %d distinct IDs, want 1", len(ids))
		}
		if got := st.Reactions.ListReactions("issue", issue.ID, ""); len(got) != 1 {
			t.Errorf("issue has %d reactions, want exactly 1 (idempotency broken)", len(got))
		}
	})

	t.Run("membership upsert", func(t *testing.T) {
		org := st.CreateOrg(admin, "cnt-org", "Org", "")
		if org == nil {
			t.Fatal("CreateOrg nil")
		}
		member := &store.User{ID: st.NextUser, Login: "cnt-member", Type: "User"}
		st.Mu.Lock()
		st.Users[member.ID] = member
		st.UsersByLogin[member.Login] = member
		st.NextUser++
		st.Mu.Unlock()

		var wg sync.WaitGroup
		for w := 0; w < workers; w++ {
			wg.Add(1)
			go func(n int) {
				defer wg.Done()
				for i := 0; i < perWorker; i++ {
					role := store.OrgRoleMember
					if (n+i)%2 == 0 {
						role = store.OrgRoleAdmin
					}
					if st.SetMembership(org.Login, member.ID, role, store.MembershipStateActive) == nil {
						t.Error("SetMembership returned nil under contention")
						return
					}
				}
			}(w)
		}
		wg.Wait()

		// Exactly one membership row for (org, member) — upsert must not
		// create duplicates under contention.
		st.Mu.RLock()
		count := len(st.Memberships)
		m := st.Memberships[store.MembershipKey(org.Login, member.ID)]
		st.Mu.RUnlock()
		if m == nil {
			t.Fatal("membership missing after upsert storm")
		}
		// admin (org creator) + the single member row.
		if count != 2 {
			t.Errorf("membership rows = %d, want 2 (creator + one upserted member)", count)
		}
	})
}
