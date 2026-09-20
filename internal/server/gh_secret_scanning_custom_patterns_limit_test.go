package bleephub

import (
	"fmt"
	"net/http"
	"testing"
)

// TestCustomPatternCreationIsCappedPerRequest covers the documented `maxItems`
// in both directions: a hundred patterns are created, a hundred and one are
// refused — and refused whole, leaving nothing behind.
func TestCustomPatternCreationIsCappedPerRequest(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	admin := s.store.UsersByLogin["admin"]
	repo := s.store.CreateRepo(admin, "pattern-limit", "", true)
	base := "/api/v3/repos/" + repo.FullName + "/secret-scanning/custom-patterns"
	batch := func(prefix string, n int) map[string]interface{} {
		patterns := make([]map[string]interface{}, 0, n)
		for i := range n {
			patterns = append(patterns, map[string]interface{}{
				"name": fmt.Sprintf("%s %03d", prefix, i), "pattern": fmt.Sprintf(`%s_%03d_[0-9a-f]{16}`, prefix, i),
			})
		}
		return map[string]interface{}{"patterns": patterns}
	}

	expectStatus(t, s.post(t, base, defaultToken, batch("over", maxCustomPatternsPerRequest+1)),
		http.StatusUnprocessableEntity, "create one pattern too many")
	if stored := decodeJSONArray(t, s.get(t, base+"?per_page=100", defaultToken)); len(stored) != 0 {
		t.Fatalf("a refused request stored %d patterns", len(stored))
	}

	created := decodeBody(t, s.post(t, base, defaultToken, batch("full", maxCustomPatternsPerRequest)), http.StatusCreated)
	if got := len(created["created_patterns"].([]interface{})); got != maxCustomPatternsPerRequest {
		t.Fatalf("created %d patterns, want %d", got, maxCustomPatternsPerRequest)
	}
}
