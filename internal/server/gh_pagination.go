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

// PaginationParams holds parsed pagination query parameters.
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

// parsePagination extracts page/per_page from query string with GitHub defaults.
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

	// Guard against integer overflow from an attacker-supplied page: a very
	// large page makes (page-1)*perPage wrap negative, which would produce an
	// out-of-range slice expression. Compute in int64 and clamp.
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

// buildLinkHeader constructs an RFC 5988 Link header.
func buildLinkHeader(r *http.Request, page, perPage, lastPage int) string {
	if lastPage <= 1 {
		return ""
	}

	// GitHub emits absolute pagination targets, and clients (octokit, gh)
	// auto-follow the Link header WITH the Authorization header attached. The
	// host therefore must not come from a client-supplied X-Forwarded-Host: a
	// spoofed value would redirect the follow-up request — credentials and all —
	// to an attacker's host. Derive the host from the request's own Host header.
	// X-Forwarded-Proto only selects http/https on that same host (no exfil), so
	// it is still honored for correctness behind a TLS-terminating proxy.
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

	linkURL := func(p int) string {
		qc := make(url.Values)
		for k, v := range q {
			qc[k] = v
		}
		qc.Set("page", strconv.Itoa(p))
		qc.Set("per_page", strconv.Itoa(perPage))
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
