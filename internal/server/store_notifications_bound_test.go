package bleephub

import (
	"fmt"
	"testing"
	"time"
)

// TestMarkThreadReadBoundsReadMarkers pins STORE-023: the per-user read-marker
// set (re-serialised in full on every mark) must not grow without limit as a
// user reads more and more threads. Once it exceeds the cap plus slack the
// oldest markers are pruned by read-at time, and the newest are always kept.
func TestMarkThreadReadBoundsReadMarkers(t *testing.T) {
	st := newTestServer().store
	const userID = 1
	// A fixed base time (time.Now is banned in tests); each mark is one second
	// newer than the last, so "oldest" is deterministic.
	base := time.Unix(1_600_000_000, 0)
	total := maxReadThreadIDs + pruneReadThreadSlack + 100
	for i := 0; i < total; i++ {
		st.MarkThreadRead(userID, fmt.Sprintf("thread-%d", i), base.Add(time.Duration(i)*time.Second))
	}

	st.Mu.RLock()
	state := st.NotificationsState[userID]
	n := len(state.ReadThreadIDs)
	_, oldestPresent := state.ReadThreadIDs["thread-0"]
	_, newestPresent := state.ReadThreadIDs[fmt.Sprintf("thread-%d", total-1)]
	st.Mu.RUnlock()

	if n > maxReadThreadIDs+pruneReadThreadSlack {
		t.Fatalf("ReadThreadIDs grew unbounded: %d markers, cap+slack is %d", n, maxReadThreadIDs+pruneReadThreadSlack)
	}
	if n < maxReadThreadIDs {
		t.Fatalf("pruned too aggressively: %d markers, want >= %d", n, maxReadThreadIDs)
	}
	if oldestPresent {
		t.Errorf("oldest read marker (thread-0) should have been pruned once over the cap")
	}
	if !newestPresent {
		t.Errorf("newest read marker (thread-%d) must be retained", total-1)
	}
}
