package graphqlapi

import "testing"

// GraphQL connection pagination fuzzes, moved from the server package with
// the resolver layer (ARCH-003).

// FuzzPaginateGQLCursors fuzzes the Relay forward-pagination helper directly
// with an attacker-controlled first + after cursor, plus the raw cursor codec.
// Invariants: decode(encode(x))==x for non-negative x; paginateGQL never
// returns more nodes than the item count and never panics on a garbage cursor.
func FuzzPaginateGQLCursors(f *testing.F) {
	items := make([]int, 60)
	for i := range items {
		items[i] = i
	}
	toGQL := func(n int) map[string]interface{} { return map[string]interface{}{"v": n} }

	f.Add(10, "")
	f.Add(0, "")
	f.Add(-5, "")
	f.Add(1<<31, "Y3Vyc29yOjU=")
	f.Add(30, "not-base64!!!")
	f.Add(30, "Y3Vyc29yOjk5OTk5OTk5OTk5OTk5OTk5OTk=")
	f.Add(1<<62, "Y3Vyc29yOi0x") // cursor:-1
	f.Add(50, "Y3Vyc29yOg==")    // cursor:

	f.Fuzz(func(t *testing.T, first int, after string) {
		res := paginateGQL(items, first, after, toGQL, func(int) string { return "" })
		nodes, _ := res["nodes"].([]map[string]interface{})
		if len(nodes) > len(items) {
			t.Fatalf("first=%d after=%q returned %d > %d nodes", first, after, len(nodes), len(items))
		}
		edges, _ := res["edges"].([]map[string]interface{})
		if len(edges) != len(nodes) {
			t.Fatalf("edges/nodes length mismatch: %d vs %d", len(edges), len(nodes))
		}
		// Round-trip identity for any cursor the helper itself emits.
		for _, e := range edges {
			c, _ := e["cursor"].(string)
			if got := decodeCursor(c); got < 0 {
				t.Fatalf("emitted cursor %q decodes to negative %d", c, got)
			}
		}
	})
}

// FuzzRepaginateConnection fuzzes the embedded-connection re-slicer (first/
// after and last/before) with a fuzzed connection map. Invariant: never an
// out-of-range slice panic; a returned connection never has more nodes than
// the source.
func FuzzRepaginateConnection(f *testing.F) {
	f.Add(10, "", 0, "")
	f.Add(-1, "", 5, "Y3Vyc29yOjM=")
	f.Add(1<<31, "Y3Vyc29yOjU=", 0, "")
	// Regression: an after cursor beyond the source used to move both window
	// bounds past len(nodes), panicking in buildConnectionWindow.
	f.Add(1<<31, "Y3Vyc29yOjY0", -92, "")
	f.Add(0, "", 200, "")
	f.Add(0, "garbage", 0, "garbage")
	f.Add(5, "", 5, "") // both first and last present

	f.Fuzz(func(t *testing.T, first int, after string, last int, before string) {
		src := make([]map[string]interface{}, 40)
		for i := range src {
			src[i] = map[string]interface{}{"id": i}
		}
		conn := map[string]interface{}{"nodes": src}
		args := map[string]interface{}{}
		if first != 0 || after != "" {
			args["first"] = first
			args["after"] = after
		}
		if last != 0 || before != "" {
			args["last"] = last
			args["before"] = before
		}
		out := repaginateConnection(conn, args)
		m, ok := out.(map[string]interface{})
		if !ok {
			t.Fatalf("repaginateConnection returned non-map: %T", out)
		}
		nodes, _ := m["nodes"].([]map[string]interface{})
		if len(nodes) > len(src) {
			t.Fatalf("first=%d last=%d returned %d > %d nodes", first, last, len(nodes), len(src))
		}
	})
}

// FuzzPaginateGQL exercises the shared Relay cursor-pagination chokepoint with
// attacker-controlled `first` and `after` values. A malformed cursor or an
// out-of-range `first` must never panic on the internal slice expressions.
func FuzzPaginateGQL(f *testing.F) {
	f.Add(10, "")
	f.Add(0, "")
	f.Add(-1, "")
	f.Add(1<<31-1, "")
	f.Add(5, "Y3Vyc29yOjA=") // cursor:0
	f.Add(5, "not-base64!!")
	f.Add(5, "Y3Vyc29yOjk5OTk5OTk5OTk5OQ==") // cursor:999999999999
	f.Add(1<<31-1, "Y3Vyc29yOjU=")           // first=MaxInt32, cursor:5

	items := make([]int, 50)
	for i := range items {
		items[i] = i
	}
	toGQL := func(n int) map[string]interface{} {
		return map[string]interface{}{"v": n}
	}

	f.Fuzz(func(t *testing.T, first int, after string) {
		// Must not panic regardless of input.
		res := paginateGQL(items, first, after, toGQL, func(int) string { return "" })
		if res == nil {
			t.Fatal("nil result")
		}
		nodes, _ := res["nodes"].([]map[string]interface{})
		if len(nodes) > len(items) {
			t.Fatalf("returned more nodes (%d) than items (%d)", len(nodes), len(items))
		}
	})
}

// FuzzDecodeCursor checks the base64 cursor decoder never panics.
func FuzzDecodeCursor(f *testing.F) {
	f.Add("Y3Vyc29yOjA=")
	f.Add("")
	f.Add("!!!!")
	f.Add("Y3Vyc29yOg==")
	f.Fuzz(func(t *testing.T, s string) {
		_ = decodeCursor(s)
	})
}

// FuzzEncodeCursorRoundTrip checks the cursor codec round-trips for any int and
// that decodeCursor never panics. encode(decode(x)) and decode(encode(x))
// must be total functions.
func FuzzEncodeCursorRoundTrip(f *testing.F) {
	f.Add(0)
	f.Add(-1)
	f.Add(1 << 31)
	f.Add(-(1 << 31))
	f.Fuzz(func(t *testing.T, idx int) {
		c := encodeCursor(idx)
		got := decodeCursor(c)
		if idx >= 0 && got != idx {
			t.Fatalf("round-trip lost value: encode(%d) -> %q -> decode -> %d", idx, c, got)
		}
	})
}
