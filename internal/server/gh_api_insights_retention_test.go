package bleephub

import (
	"testing"
)

// TestAPIInsightsDurableBucketStaysBounded pins STORE-024: FIFO eviction of the
// request log must reclaim the durable rows too (runtime and at load), or the
// api_insights_requests bucket grows by one row per request ever served even
// though the in-memory log is capped.
func TestAPIInsightsDurableBucketStaysBounded(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("BLEEPHUB_PERSIST", "true")
	t.Setenv("BLEEPHUB_DATA_DIR", dir)

	record := func(st *Store, n int) {
		for i := 0; i < n; i++ {
			st.RecordAPIRequest(&APIRequestRecord{Method: "GET", Route: "/x"})
		}
	}

	// Session 1: record with the default cap so the durable bucket holds all 8.
	p1, err := NewPersistence()
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	st1 := NewStore()
	if err := st1.SetPersistence(p1); err != nil {
		t.Fatalf("SetPersistence: %v", err)
	}
	record(st1, 8)
	rows, err := p1.List("api_insights_requests")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(rows) != 8 {
		t.Fatalf("durable rows after 8 records (cap 10000) = %d, want 8", len(rows))
	}
	if err := p1.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	// Session 2: reopen with a small cap. Load-time pruning must converge the
	// leaked durable bucket down to the cap, keeping the newest records.
	p2, err := NewPersistence()
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	t.Cleanup(func() { _ = p2.Close() })
	st2 := NewStore()
	st2.apiRequestRecordCap = 3
	if err := st2.SetPersistence(p2); err != nil {
		t.Fatalf("reload: %v", err)
	}
	if got := len(st2.APIRequestRecords); got != 3 {
		t.Fatalf("in-memory records after load = %d, want 3", got)
	}
	rows, err = p2.List("api_insights_requests")
	if err != nil {
		t.Fatalf("list after reload: %v", err)
	}
	if len(rows) != 3 {
		t.Fatalf("durable rows after load prune (cap 3) = %d, want 3", len(rows))
	}

	// Runtime FIFO eviction keeps the durable bucket bounded going forward, and
	// the survivors are the newest — recording more never grows it past the cap.
	record(st2, 5)
	if got := len(st2.APIRequestRecords); got != 3 {
		t.Fatalf("in-memory records after more traffic = %d, want 3", got)
	}
	rows, err = p2.List("api_insights_requests")
	if err != nil {
		t.Fatalf("list after more traffic: %v", err)
	}
	if len(rows) != 3 {
		t.Fatalf("durable rows after more traffic = %d, want 3 (FIFO must delete durable rows)", len(rows))
	}
	// The surviving records are the three most recent by id.
	ids := map[int64]bool{}
	for _, r := range st2.APIRequestRecords {
		ids[r.ID] = true
	}
	maxID := st2.NextAPIRequestID - 1
	for _, want := range []int64{maxID, maxID - 1, maxID - 2} {
		if !ids[want] {
			t.Fatalf("record id %d missing; survivors must be the newest three (%v)", want, ids)
		}
	}
}
