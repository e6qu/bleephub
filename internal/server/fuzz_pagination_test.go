package bleephub

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
)

// FuzzParsePagination fuzzes the page/per_page query parser. Invariant: the
// parsed page and per_page are always positive and per_page is clamped to
// GitHub's 100 ceiling, for any attacker-supplied string.
func FuzzParsePagination(f *testing.F) {
	f.Add("1", "30")
	f.Add("0", "0")
	f.Add("-1", "-100")
	f.Add("999999999999999999999", "999999999999999999999")
	f.Add("2147483648", "101")
	f.Add("abc", "def")
	f.Add("", "")
	f.Add("1.5", "3e2")
	f.Add("0x10", "010")

	f.Fuzz(func(t *testing.T, page, perPage string) {
		req := httptest.NewRequest(http.MethodGet, "/x?"+url.Values{"page": {page}, "per_page": {perPage}}.Encode(), nil)
		pp := parsePagination(req)
		if pp.Page < 1 {
			t.Fatalf("page=%q parsed to %d (want >=1)", page, pp.Page)
		}
		if pp.PerPage < 1 || pp.PerPage > 100 {
			t.Fatalf("per_page=%q parsed to %d (want 1..100)", perPage, pp.PerPage)
		}
	})
}

// FuzzPaginateAndLink drives the generic page-slicer with an attacker-supplied
// page/per_page against a fixed-size collection. Invariant: never an
// out-of-range slice panic; the returned window is a sub-slice no larger than
// per_page and never exceeds the source.
func FuzzPaginateAndLink(f *testing.F) {
	items := make([]int, 137)
	for i := range items {
		items[i] = i
	}

	f.Add("1", "30")
	f.Add("0", "0")
	f.Add("-1", "-1")
	f.Add("9223372036854775807", "100")
	f.Add("2", "50")
	f.Add("100000", "100")
	f.Add("abc", "xyz")

	f.Fuzz(func(t *testing.T, page, perPage string) {
		req := httptest.NewRequest(http.MethodGet, "/x?"+url.Values{"page": {page}, "per_page": {perPage}}.Encode(), nil)
		w := httptest.NewRecorder()
		got := paginateAndLink(w, req, items)
		if len(got) > len(items) {
			t.Fatalf("page=%q per_page=%q returned %d > %d items", page, perPage, len(got), len(items))
		}
		pp := parsePagination(req)
		if len(got) > pp.PerPage {
			t.Fatalf("returned %d items > per_page %d", len(got), pp.PerPage)
		}
	})
}
