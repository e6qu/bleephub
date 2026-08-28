package bleephub

import "testing"

// FuzzHTTPRequest drives one fuzzed HTTP request through the full wrapped
// handler chain (prefixStrip → internalAuth → ghHeaders → mux) against a freshly
// seeded fixture. Invariants (serveAndCheck): never panic, never emit HTTP 500,
// and any application/json body must parse; a 4xx for bad input is fine.
//
// The fixture is rebuilt per execution: the route vocabulary reaches DELETE
// handlers for the seed repo, org and team, so a shared fixture would let one
// input delete the data every later input depends on.
func FuzzHTTPRequest(f *testing.F) {
	// Seeds spanning diverse route families, chosen to land on many handlers.
	seeds := [][]byte{
		{},
		{0},
		{0, 0, 0},
		{1, 1, 1, 1},
		{5, 5, 5, 5, 5},
		{255, 255, 255, 255},
		{10, 0, 3, 7, 2, 9},
		{40, 1, 5, 12, 0, 4, 8},
		{80, 2, 0, 3, 1, 6, 11, 2},
		{120, 0, 1, 2, 3, 4, 5, 6, 7},
		{160, 1, 2, 0, 5, 3, 9, 1},
		{200, 0, 4, 1, 7, 2, 0, 8},
		{7, 3, 3, 3, 3, 3, 3},
		{13, 4, 4, 4, 0, 0, 0, 0},
		{22, 2, 1, 1, 2, 2, 1},
		{33, 0, 0, 1, 1, 2, 2, 3, 3},
		{44, 5, 5, 0, 0, 1, 1},
		{55, 1, 0, 2, 3, 4, 5, 6, 7, 8, 9},
		{66, 2, 3, 4, 5, 6, 7},
		{77, 0, 1, 0, 1, 0, 1, 0, 1},
		{88, 3, 2, 1, 0, 9, 8, 7},
		{99, 4, 3, 2, 1, 0, 5, 6},
		{111, 1, 2, 3, 0, 0, 0},
		{222, 0, 0, 0, 5, 5, 5},
	}
	for _, s := range seeds {
		f.Add(s)
	}

	f.Fuzz(func(t *testing.T, data []byte) {
		fx := newFuzzFixture(t)
		req := fx.decodeFuzzRequest(data)
		serveAndCheck(t, fx.handler, req)
	})
}
