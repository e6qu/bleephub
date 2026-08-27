package bleephub

import (
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/e6qu/bleephub/internal/store"
)

type PaginationParams struct {
	Page    int
	PerPage int
}

func filterSince[T any](
	w http.ResponseWriter,
	r *http.Request,
	resource string,
	items []T,
	updatedAt func(T) time.Time,
) ([]T, bool) {
	raw := r.URL.Query().Get("since")
	if raw == "" {
		return items, true
	}
	since, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		store.WriteGHValidationError(w, resource, "since", "invalid")
		return nil, false
	}
	filtered := make([]T, 0, len(items))
	for _, item := range items {
		if !updatedAt(item).Before(since) {
			filtered = append(filtered, item)
		}
	}
	return filtered, true
}

func invalidRESTPaginationQuery(r *http.Request) string {
	for _, name := range []string{"page", "per_page"} {
		value := r.URL.Query().Get(name)
		if value == "" {
			continue
		}
		number, err := strconv.Atoi(value)
		if err != nil || number < 1 {
			return name
		}
	}
	return ""
}

// parsePagination reads page/per_page with GitHub defaults (page 1, 30, cap 100).
func parsePagination(r *http.Request) PaginationParams {
	p := PaginationParams{Page: 1, PerPage: 30}
	if v := r.URL.Query().Get("page"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			p.Page = n
		}
	}
	if v := r.URL.Query().Get("per_page"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			p.PerPage = n
			if p.PerPage > 100 {
				p.PerPage = 100
			}
		}
	}
	return p
}

// paginateAndLink slices items to the current page and sets the Link header.
func paginateAndLink[T any](w http.ResponseWriter, r *http.Request, items []T) []T {
	pp := parsePagination(r)
	total := len(items)

	lastPage := 1
	if total > 0 {
		lastPage = (total + pp.PerPage - 1) / pp.PerPage
	}

	// A very large page makes (page-1)*perPage wrap negative and panic the slice.
	// Compute in int64 and clamp.
	start64 := int64(pp.Page-1) * int64(pp.PerPage)
	var start int
	switch {
	case start64 < 0:
		start = 0
	case start64 > int64(total):
		start = total
	default:
		start = int(start64)
	}
	end := start + pp.PerPage
	if end < start || end > total {
		end = total
	}
	page := items[start:end]

	if link := buildLinkHeader(r, pp.Page, pp.PerPage, lastPage); link != "" {
		w.Header().Set("Link", link)
	}

	return page
}

// searchResultWindow caps pagination: GitHub serves nothing past the first
// 1,000 search results, so the links must stop there or rel="next" walks a
// client into a 422.
const searchResultWindow = 1000

// setSearchLinkHeader sets the Link header of a search response. Search returns
// an envelope rather than a bare array, so it emits the header itself instead of
// via paginateAndLink; the rel targets are built identically.
func setSearchLinkHeader(w http.ResponseWriter, r *http.Request, page, perPage, totalCount int) {
	reachable := totalCount
	if reachable > searchResultWindow {
		reachable = searchResultWindow
	}
	lastPage := 1
	if reachable > 0 && perPage > 0 {
		lastPage = (reachable + perPage - 1) / perPage
	}
	if link := buildLinkHeader(r, page, perPage, lastPage); link != "" {
		w.Header().Set("Link", link)
	}
}

// buildLinkHeader constructs an RFC 5988 Link header.
func buildLinkHeader(r *http.Request, page, perPage, lastPage int) string {
	if lastPage <= 1 {
		return ""
	}

	// Security: clients auto-follow these absolute targets with Authorization
	// attached, so the host must come from the request's own Host, never a
	// spoofable X-Forwarded-Host. X-Forwarded-Proto only picks http/https on that
	// same host, so it stays honored behind a TLS-terminating proxy.
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	if forwarded := strings.TrimSpace(strings.Split(r.Header.Get("X-Forwarded-Proto"), ",")[0]); forwarded == "http" || forwarded == "https" {
		scheme = forwarded
	}
	base := (&url.URL{Scheme: scheme, Host: r.Host, Path: r.URL.Path}).String()
	q := r.URL.Query()
	q.Del("page")
	// Carry per_page into the rel targets only when the client sent it (echoing
	// the clamped value); an unset per_page stays absent, as GitHub does.
	clientSentPerPage := r.URL.Query().Get("per_page") != ""
	q.Del("per_page")

	linkURL := func(p int) string {
		qc := make(url.Values)
		for k, v := range q {
			qc[k] = v
		}
		qc.Set("page", strconv.Itoa(p))
		if clientSentPerPage {
			qc.Set("per_page", strconv.Itoa(perPage))
		}
		return fmt.Sprintf("<%s?%s>", base, qc.Encode())
	}

	var parts []string
	if page < lastPage {
		parts = append(parts, linkURL(page+1)+`; rel="next"`)
		parts = append(parts, linkURL(lastPage)+`; rel="last"`)
	}
	if page > 1 {
		parts = append(parts, linkURL(1)+`; rel="first"`)
		parts = append(parts, linkURL(page-1)+`; rel="prev"`)
	}
	return strings.Join(parts, ", ")
}
