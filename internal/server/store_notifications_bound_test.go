package bleephub

import (
	"fmt"
	"testing"
	"time"

	"github.com/e6qu/bleephub/internal/store"
)

// TestMarkThreadReadBoundsReadMarkers pins STORE-023: the per-user read-marker set must not grow without limit; past cap plus slack the oldest markers are pruned by read-at time while the newest are kept.
func TestMarkThreadReadBoundsReadMarkers(t *testing.T) {
	st := newTestServer().store
	const userID = 1
	// Fixed base time (time.Now is banned in tests); each mark is one second newer, making "oldest" deterministic.
	base := time.Unix(1_600_000_000, 0)
	total := store.MaxReadThreadIDs + store.PruneReadThreadSlack + 100
	for i := 0; i < total; i++ {
		st.MarkThreadRead(userID, fmt.Sprintf("thread-%d", i), base.Add(time.Duration(i)*time.Second))
	}

	st.Mu.RLock()
	state := st.NotificationsState[userID]
	n := len(state.ReadThreadIDs)
	_, oldestPresent := state.ReadThreadIDs["thread-0"]
	_, newestPresent := state.ReadThreadIDs[fmt.Sprintf("thread-%d", total-1)]
	st.Mu.RUnlock()

	if n > store.MaxReadThreadIDs+store.PruneReadThreadSlack {
		t.Fatalf("ReadThreadIDs grew unbounded: %d markers, cap+slack is %d", n, store.MaxReadThreadIDs+store.PruneReadThreadSlack)
	}
	if n < store.MaxReadThreadIDs {
		t.Fatalf("pruned too aggressively: %d markers, want >= %d", n, store.MaxReadThreadIDs)
	}
	if oldestPresent {
		t.Errorf("oldest read marker (thread-0) should have been pruned once over the cap")
	}
	if !newestPresent {
		t.Errorf("newest read marker (thread-%d) must be retained", total-1)
	}
}
