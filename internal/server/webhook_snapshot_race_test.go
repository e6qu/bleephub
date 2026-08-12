package bleephub

import (
	"fmt"
	"sync"
	"testing"

	"github.com/e6qu/bleephub/internal/store"
)

// TestWebhookReadersReceiveDeepSnapshots covers both repository and
// organization hooks. Updates mutate store-owned hooks in place, while API
// rendering and delivery run after the store lock has been released; getters
// therefore must return detached snapshots rather than shared pointers.
func TestWebhookReadersReceiveDeepSnapshots(t *testing.T) {
	st := store.NewStore()
	repoHook := st.CreateHook("admin/repo", "https://one.example", "one", "json", "0", []string{"one"}, true)
	orgHook := st.CreateOrgHook("octo", "https://one.example", "one", "json", "0", []string{"one"}, true)

	const writes = 1000
	start := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(3)

	go func() {
		defer wg.Done()
		<-start
		for i := range writes {
			value := fmt.Sprintf("%d", i)
			st.UpdateHook("admin/repo", repoHook.ID, func(h *store.Webhook) {
				h.URL = "https://" + value + ".example"
				h.Secret = value
				h.Events = []string{value}
			})
			st.UpdateOrgHook("octo", orgHook.ID, func(h *store.Webhook) {
				h.URL = "https://" + value + ".example"
				h.Secret = value
				h.Events = []string{value}
			})
		}
	}()

	check := func(get func() *store.Webhook) {
		defer wg.Done()
		<-start
		for range writes {
			hook := get()
			if hook == nil || len(hook.Events) != 1 {
				t.Errorf("invalid hook snapshot: %#v", hook)
				return
			}
			wantURL := "https://" + hook.Secret + ".example"
			if hook.URL != wantURL || hook.Events[0] != hook.Secret {
				t.Errorf("torn hook snapshot: url=%q secret=%q events=%v", hook.URL, hook.Secret, hook.Events)
				return
			}
			hook.Events[0] = "caller mutation"
		}
	}
	go check(func() *store.Webhook { return st.GetHook("admin/repo", repoHook.ID) })
	go check(func() *store.Webhook { return st.GetOrgHook("octo", orgHook.ID) })

	close(start)
	wg.Wait()

	if got := st.GetHook("admin/repo", repoHook.ID); got.Events[0] == "caller mutation" {
		t.Fatal("repository hook getter returned a store-owned events slice")
	}
	if got := st.GetOrgHook("octo", orgHook.ID); got.Events[0] == "caller mutation" {
		t.Fatal("organization hook getter returned a store-owned events slice")
	}
}
