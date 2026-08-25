package main

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	github "github.com/google/go-github/v88/github"
)

// runEdgeSemantics covers the behaviour clients rely on that is not any one
// resource: the 404-versus-403 rule for private resources, validation error
// shape, conditional requests, rate-limit headers, pagination bounds and the
// redirects a rename or transfer leaves behind.
func runEdgeSemantics(client *github.Client, rec *recorder, set *fixtureSet, guest *principal) {
	// --- Private resources are 404, not 403 --------------------------------
	private := newScratchPrivate(client, set.owner, "conformance-private")
	if private.ok() && guest.ok() {
		rec.check("errors", "404 (not 403) for a private repository", "GET /repos/{owner}/{repo} as a stranger", func() error {
			_, _, err := guest.client.Repositories.Get(ctx, private.owner, private.repo)
			if err == nil {
				return deviate("404 Not Found", "success",
					"a stranger can read a private repository")
			}
			var apiErr *github.ErrorResponse
			if !errors.As(err, &apiErr) || apiErr.Response == nil {
				return deviate("*github.ErrorResponse", fmt.Sprintf("%T", err),
					"the refusal did not decode into a GitHub error body")
			}
			if apiErr.Response.StatusCode == http.StatusForbidden {
				return deviate("404 Not Found", "403 Forbidden",
					"answering 403 confirms the repository exists, which is the disclosure GitHub answers 404 to avoid")
			}
			if apiErr.Response.StatusCode != http.StatusNotFound {
				return deviate("404 Not Found", fmt.Sprintf("%d", apiErr.Response.StatusCode),
					"a private repository a stranger cannot see must answer 404")
			}
			return nil
		})

		rec.check("errors", "404 for a private repository's issues", "GET /repos/{owner}/{repo}/issues as a stranger", func() error {
			_, _, err := guest.client.Issues.ListByRepo(ctx, private.owner, private.repo, nil)
			return wantHTTPError(err, http.StatusNotFound, "listing a private repository's issues as a stranger")
		})

		rec.check("errors", "403 (not 404) for a readable resource one cannot write",
			"PATCH /repos/{owner}/{repo} on a public repository as a stranger", func() error {
				if set.repo == "" {
					return deviate("a public repository fixture", "none", "no public repository fixture exists")
				}
				_, _, err := guest.client.Repositories.Edit(ctx, set.owner, set.repo, &github.Repository{
					Description: github.Ptr("edited by a stranger"),
				})
				if err == nil {
					return deviate("403 Forbidden", "success", "a stranger can edit a repository they do not own")
				}
				var apiErr *github.ErrorResponse
				if !errors.As(err, &apiErr) || apiErr.Response == nil {
					return deviate("*github.ErrorResponse", fmt.Sprintf("%T", err),
						"the refusal did not decode into a GitHub error body")
				}
				if apiErr.Response.StatusCode != http.StatusForbidden &&
					apiErr.Response.StatusCode != http.StatusNotFound {
					return deviate("403 Forbidden (or 404 where existence is secret)",
						fmt.Sprintf("%d", apiErr.Response.StatusCode),
						"a refused write on a visible repository answers neither 403 nor 404")
				}
				return nil
			})
	} else {
		skipAll(rec, "errors", "GET /repos/{owner}/{repo} as a stranger",
			"a private repository or a second account could not be provisioned",
			"404 (not 403) for a private repository", "404 for a private repository's issues",
			"403 (not 404) for a readable resource one cannot write")
	}

	// --- Validation error shape --------------------------------------------
	if set.repo != "" {
		rec.check("errors", "422 validation error carries resource/field/code",
			"POST /repos/{owner}/{repo}/labels with a missing field", func() error {
				_, _, err := client.Issues.CreateLabel(ctx, set.owner, set.repo, &github.Label{})
				if err == nil {
					return deviate("422 Unprocessable Entity", "success", "a label with no name was accepted")
				}
				var apiErr *github.ErrorResponse
				if !errors.As(err, &apiErr) {
					return deviate("*github.ErrorResponse", fmt.Sprintf("%T", err),
						"the validation failure did not decode")
				}
				if len(apiErr.Errors) == 0 {
					return deviate("a populated errors array", "empty",
						"the validation error carries no errors array, so a client cannot point at the offending field")
				}
				first := apiErr.Errors[0]
				if first.Resource == "" || first.Field == "" || first.Code == "" {
					return deviate("resource, field and code on every error entry",
						fmt.Sprintf("resource=%q field=%q code=%q", first.Resource, first.Field, first.Code),
						"an error entry omits the keys the documented error object requires")
				}
				return nil
			})

		rec.check("errors", "422 for a duplicate resource",
			"POST /repos/{owner}/{repo}/labels twice", func() error {
				if _, _, err := client.Issues.CreateLabel(ctx, set.owner, set.repo, &github.Label{
					Name: github.Ptr("conformance-duplicate"), Color: github.Ptr("ededed"),
				}); err != nil {
					return err
				}
				_, _, err := client.Issues.CreateLabel(ctx, set.owner, set.repo, &github.Label{
					Name: github.Ptr("conformance-duplicate"), Color: github.Ptr("ededed"),
				})
				if err == nil {
					return deviate("422 Unprocessable Entity", "success", "a duplicate label was accepted")
				}
				var apiErr *github.ErrorResponse
				if !errors.As(err, &apiErr) {
					return deviate("*github.ErrorResponse", fmt.Sprintf("%T", err), "the duplicate failure did not decode")
				}
				for _, entry := range apiErr.Errors {
					if entry.Code == "already_exists" {
						return nil
					}
				}
				return deviate("an errors entry with code already_exists",
					fmt.Sprintf("%v", apiErr.Errors),
					"a duplicate does not report already_exists, so a client cannot distinguish it from a bad request")
			})

		rec.check("errors", "404 for an unknown route under an existing repository",
			"GET /repos/{owner}/{repo}/definitely-not-a-resource", func() error {
				_, err := decodeInto(client, http.MethodGet,
					fmt.Sprintf("repos/%s/%s/definitely-not-a-resource", set.owner, set.repo), nil, nil)
				return wantHTTPError(err, http.StatusNotFound, "an unknown sub-resource")
			})

		rec.check("errors", "405 or 404 for a method the resource does not support",
			"DELETE /repos/{owner}/{repo}/languages", func() error {
				_, err := decodeInto(client, http.MethodDelete,
					fmt.Sprintf("repos/%s/%s/languages", set.owner, set.repo), nil, nil)
				if err == nil {
					return deviate("a refusal", "success", "an unsupported method was accepted")
				}
				var apiErr *github.ErrorResponse
				if !errors.As(err, &apiErr) || apiErr.Response == nil {
					return deviate("*github.ErrorResponse", fmt.Sprintf("%T", err), "the refusal did not decode")
				}
				switch apiErr.Response.StatusCode {
				case http.StatusNotFound, http.StatusMethodNotAllowed:
					return nil
				}
				return deviate("404 or 405", fmt.Sprintf("%d", apiErr.Response.StatusCode),
					"an unsupported method produces neither 404 nor 405")
			})
	}

	// --- Rate limit headers -------------------------------------------------
	rec.check("http", "rate limit headers on every response", "GET /user", func() error {
		request, err := client.NewRequest(ctx, http.MethodGet, "user", nil)
		if err != nil {
			return err
		}
		resp, err := client.Do(request, nil)
		if err != nil {
			return err
		}
		for _, header := range []string{"X-RateLimit-Limit", "X-RateLimit-Remaining", "X-RateLimit-Reset", "X-RateLimit-Used"} {
			if resp.Header.Get(header) == "" {
				return deviate(header+" present", "absent",
					"%s is missing, so a client cannot back off before it is throttled", header)
			}
		}
		if resp.Header.Get("X-RateLimit-Resource") == "" {
			return deviate("X-RateLimit-Resource present", "absent",
				"the response does not say which rate-limit bucket it drew from")
		}
		if resp.Rate.Limit == 0 {
			return deviate("the client's parsed rate limit", "0",
				"the client could not parse the rate-limit headers into its Rate struct")
		}
		return nil
	})

	rec.check("http", "rateLimit.get reports every documented resource", "GET /rate_limit", func() error {
		var body struct {
			Resources map[string]struct {
				Limit     int   `json:"limit"`
				Remaining int   `json:"remaining"`
				Reset     int64 `json:"reset"`
				Used      *int  `json:"used"`
			} `json:"resources"`
			Rate *struct {
				Limit int `json:"limit"`
			} `json:"rate"`
		}
		if _, err := decodeInto(client, http.MethodGet, "rate_limit", nil, &body); err != nil {
			return err
		}
		for _, resource := range []string{"core", "search", "graphql", "integration_manifest"} {
			bucket, present := body.Resources[resource]
			if !present {
				return deviate("a "+resource+" bucket", "absent",
					"/rate_limit omits the %s resource, which clients read to budget calls", resource)
			}
			if bucket.Limit == 0 {
				return deviate(resource+".limit > 0", "0", "the %s bucket reports no limit", resource)
			}
			if bucket.Used == nil {
				return deviate(resource+".used present", "absent", "the %s bucket omits used", resource)
			}
		}
		if body.Rate == nil {
			return deviate("the deprecated top-level rate object", "absent",
				"/rate_limit omits the `rate` key GitHub still sends and older clients still read")
		}
		return nil
	})

	// --- Conditional requests -----------------------------------------------
	if set.repo != "" {
		rec.check("conditional", "If-None-Match on a collection",
			"GET /repos/{owner}/{repo}/labels twice", func() error {
				path := fmt.Sprintf("repos/%s/%s/labels", set.owner, set.repo)
				request, err := client.NewRequest(ctx, http.MethodGet, path, nil)
				if err != nil {
					return err
				}
				first, err := client.Do(request, nil)
				if err != nil {
					return err
				}
				etag := first.Header.Get("ETag")
				if etag == "" {
					return deviate("an ETag on a collection", "none",
						"collections carry no ETag, so a polling client must re-download every page every time")
				}
				conditional, err := client.NewRequest(ctx, http.MethodGet, path, nil)
				if err != nil {
					return err
				}
				conditional.Header.Set("If-None-Match", etag)
				second, err := client.Do(conditional, nil)
				if second != nil && second.StatusCode == http.StatusNotModified {
					return nil
				}
				if err != nil {
					return err
				}
				return deviate("304 Not Modified", fmt.Sprintf("%d", second.StatusCode),
					"a matching ETag on a collection was not answered with 304")
			})

		rec.check("conditional", "304 does not consume rate limit",
			"GET /repos/{owner}/{repo} twice with If-None-Match", func() error {
				path := fmt.Sprintf("repos/%s/%s", set.owner, set.repo)
				request, err := client.NewRequest(ctx, http.MethodGet, path, nil)
				if err != nil {
					return err
				}
				first, err := client.Do(request, nil)
				if err != nil {
					return err
				}
				etag := first.Header.Get("ETag")
				if etag == "" {
					return deviate("an ETag", "none", "the resource carries no ETag")
				}
				before, err := strconv.Atoi(first.Header.Get("X-RateLimit-Remaining"))
				if err != nil {
					return deviate("a numeric X-RateLimit-Remaining", first.Header.Get("X-RateLimit-Remaining"),
						"the remaining count is not a number")
				}
				conditional, err := client.NewRequest(ctx, http.MethodGet, path, nil)
				if err != nil {
					return err
				}
				conditional.Header.Set("If-None-Match", etag)
				second, _ := client.Do(conditional, nil)
				if second == nil || second.StatusCode != http.StatusNotModified {
					return deviate("304 Not Modified", "a full response",
						"the conditional request was not answered with 304")
				}
				after, err := strconv.Atoi(second.Header.Get("X-RateLimit-Remaining"))
				if err != nil {
					return deviate("a numeric X-RateLimit-Remaining on the 304",
						second.Header.Get("X-RateLimit-Remaining"), "the 304 carries no usable remaining count")
				}
				if after < before {
					return deviate("the remaining count unchanged across a 304",
						fmt.Sprintf("%d then %d", before, after),
						"a 304 consumes rate limit, which removes the whole reason to send a conditional request")
				}
				return nil
			})
	}

	// --- Pagination bounds ---------------------------------------------------
	if set.repo != "" {
		rec.check("pagination", "per_page above the maximum is clamped, not refused",
			"GET /repos/{owner}/{repo}/issues?per_page=200", func() error {
				var issues []*github.Issue
				resp, err := decodeInto(client, http.MethodGet,
					fmt.Sprintf("repos/%s/%s/issues?state=all&per_page=200", set.owner, set.repo), nil, &issues)
				if err != nil {
					return err
				}
				if resp.StatusCode != http.StatusOK {
					return deviate("200 with the page size clamped to 100", fmt.Sprintf("%d", resp.StatusCode),
						"an oversized per_page is refused rather than clamped")
				}
				if len(issues) > 100 {
					return deviate("at most 100 results", fmt.Sprintf("%d", len(issues)),
						"per_page is not clamped to the documented maximum of 100")
				}
				return nil
			})

		rec.check("pagination", "Link header on the last page has prev but no next",
			"GET /repos/{owner}/{repo}/issues?per_page=1&page=N", func() error {
				issues, resp, err := client.Issues.ListByRepo(ctx, set.owner, set.repo,
					&github.IssueListByRepoOptions{State: "all", ListOptions: github.ListOptions{PerPage: 1}})
				if err != nil {
					return err
				}
				if len(issues) == 0 {
					return deviate("at least one issue to page over", "none", "there is nothing to paginate")
				}
				if resp.LastPage <= 1 {
					return deviate("a rel=last link naming a page beyond the first",
						fmt.Sprintf("last=%d", resp.LastPage),
						"the first page's Link header carries no usable rel=last, so a client cannot size the collection")
				}
				last, resp, err := client.Issues.ListByRepo(ctx, set.owner, set.repo,
					&github.IssueListByRepoOptions{
						State:       "all",
						ListOptions: github.ListOptions{PerPage: 1, Page: resp.LastPage},
					})
				if err != nil {
					return err
				}
				if len(last) == 0 {
					return deviate("a result on the last page", "none", "the page rel=last names is empty")
				}
				if resp.NextPage != 0 {
					return deviate("no rel=next on the last page", fmt.Sprintf("next=%d", resp.NextPage),
						"the last page advertises a next page, so a client paginating on rel=next never terminates")
				}
				if resp.PrevPage == 0 {
					return deviate("a rel=prev on the last page", "none",
						"the last page carries no rel=prev, so a client cannot walk backwards")
				}
				return nil
			})

		rec.check("pagination", "page beyond the end is an empty array, not an error",
			"GET /repos/{owner}/{repo}/issues?per_page=1&page=9999", func() error {
				var issues []*github.Issue
				resp, err := decodeInto(client, http.MethodGet,
					fmt.Sprintf("repos/%s/%s/issues?state=all&per_page=1&page=9999", set.owner, set.repo), nil, &issues)
				if err != nil {
					return err
				}
				if resp.StatusCode != http.StatusOK {
					return deviate("200 with an empty array", fmt.Sprintf("%d", resp.StatusCode),
						"a page past the end is an error rather than an empty page")
				}
				if len(issues) != 0 {
					return deviate("an empty array", fmt.Sprintf("%d results", len(issues)),
						"a page far past the end still returns results")
				}
				return nil
			})
	}

	// --- Redirect after a rename --------------------------------------------
	renamed := newScratch(client, set.owner, "conformance-rename-before")
	if renamed.ok() {
		rec.check("http", "301 redirect after a repository rename",
			"GET /repos/{owner}/{old_name} after PATCH name", func() error {
				if _, _, err := client.Repositories.Edit(ctx, renamed.owner, renamed.repo, &github.Repository{
					Name: github.Ptr("conformance-rename-after"),
				}); err != nil {
					return err
				}
				// go-github follows the redirect, so a successful call that
				// lands on the new name is exactly what a real client sees.
				repository, resp, err := client.Repositories.Get(ctx, renamed.owner, renamed.repo)
				if err != nil {
					return deviate("a redirect to the new name", truncate(err.Error()),
						"the old name does not redirect, so every stored URL and git remote breaks on rename")
				}
				if repository.GetName() != "conformance-rename-after" {
					return deviate("conformance-rename-after", repository.GetName(),
						"the old name resolves to something other than the renamed repository")
				}
				if resp.Request == nil || !strings.Contains(resp.Request.URL.Path, "conformance-rename-after") {
					return deviate("the request to have been redirected to the new name",
						"no redirect was followed",
						"the old name answered directly instead of redirecting")
				}
				return nil
			})
	} else {
		rec.skip1("http", "301 redirect after a repository rename", "GET /repos/{owner}/{old_name}",
			"the rename repository fixture could not be provisioned")
	}

	// --- Transfer ------------------------------------------------------------
	if set.org != "" {
		transferred := newScratch(client, set.owner, "conformance-transfer")
		if transferred.ok() {
			rec.check("repos", "repos.transfer", "POST /repos/{owner}/{repo}/transfer", func() error {
				_, resp, err := client.Repositories.Transfer(ctx, transferred.owner, transferred.repo,
					github.TransferRequest{NewOwner: set.org})
				if err != nil {
					var accepted *github.AcceptedError
					if !errors.As(err, &accepted) {
						return err
					}
				}
				if resp != nil && resp.StatusCode != http.StatusAccepted && resp.StatusCode != http.StatusOK {
					return deviate("202 Accepted", fmt.Sprintf("%d", resp.StatusCode),
						"a repository transfer answers with an undocumented status")
				}
				return pollUntil("the transferred repository appears under its new owner", 20*time.Second,
					func() (bool, error) {
						repository, _, err := client.Repositories.Get(ctx, set.org, transferred.repo)
						if err != nil {
							return false, nil
						}
						return repository.GetOwner().GetLogin() == set.org, nil
					})
			})
		} else {
			rec.skip1("repos", "repos.transfer", "POST /repos/{owner}/{repo}/transfer",
				"the transfer repository fixture could not be provisioned")
		}
	} else {
		rec.skip1("repos", "repos.transfer", "POST /repos/{owner}/{repo}/transfer",
			"the organization fixture is unavailable")
	}
}

// newScratchPrivate provisions a private repository, which is what makes the
// 404-versus-403 rule observable at all.
func newScratchPrivate(client *github.Client, owner, name string) *scratch {
	set := &scratch{owner: owner, branch: "main"}
	created, _, err := client.Repositories.Create(ctx, "", &github.Repository{
		Name:     github.Ptr(name),
		Private:  github.Ptr(true),
		AutoInit: github.Ptr(true),
	})
	if err != nil {
		return set
	}
	set.repo = created.GetName()
	set.id = created.GetID()
	if created.GetDefaultBranch() != "" {
		set.branch = created.GetDefaultBranch()
	}
	return set
}

// runHypermedia asserts every link a client can follow is an absolute URI.
// The contract declares them `format: uri`, and clients build their next
// request out of them: octokit follows `url`, PyGithub builds a user's
// sub-resources from the user object's own `url`, and a relative value sends
// that request to base + relative, which resolves to a route that does not
// exist. This is checked on the objects that carry the most links.
func runHypermedia(client *github.Client, rec *recorder, set *fixtureSet) {
	const domain = "http"

	absolute := func(object, key, value string) error {
		if value == "" {
			return deviate("a populated "+key, "empty",
				"%s.%s is empty, and the schema marks it required", object, key)
		}
		if !strings.HasPrefix(value, "http://") && !strings.HasPrefix(value, "https://") {
			return deviate("an absolute URI in "+key, value,
				"%s.%s is relative, so a client that follows the object's own links resolves them "+
					"against its own base and requests a route that does not exist", object, key)
		}
		return nil
	}

	rec.check(domain, "user hypermedia is absolute", "GET /users/{username}", func() error {
		var payload map[string]any
		if _, err := decodeInto(client, http.MethodGet, "users/"+set.owner, nil, &payload); err != nil {
			return err
		}
		for _, key := range []string{"url", "html_url", "avatar_url", "repos_url", "followers_url",
			"following_url", "gists_url", "starred_url", "subscriptions_url",
			"organizations_url", "events_url", "received_events_url"} {
			value, present := payload[key]
			if !present {
				return deviate("the required key "+key, "absent",
					"simple-user marks %s required and the response omits it", key)
			}
			text, _ := value.(string)
			// The templated links carry an RFC 6570 expansion suffix; only the
			// scheme and authority are being asserted here.
			if err := absolute("user", key, strings.SplitN(text, "{", 2)[0]); err != nil {
				return err
			}
		}
		return nil
	})

	rec.check(domain, "authenticated user hypermedia is absolute", "GET /user", func() error {
		var payload map[string]any
		if _, err := decodeInto(client, http.MethodGet, "user", nil, &payload); err != nil {
			return err
		}
		for _, key := range []string{"url", "html_url", "avatar_url", "repos_url"} {
			text, _ := payload[key].(string)
			if err := absolute("user", key, strings.SplitN(text, "{", 2)[0]); err != nil {
				return err
			}
		}
		return nil
	})

	rec.check(domain, "nested owner hypermedia is absolute", "GET /repos/{owner}/{repo}", func() error {
		if set.repo == "" {
			return deviate("a repository fixture", "none", "no repository fixture exists")
		}
		var payload map[string]any
		if _, err := decodeInto(client, http.MethodGet,
			fmt.Sprintf("repos/%s/%s", set.owner, set.repo), nil, &payload); err != nil {
			return err
		}
		if err := absolute("repository", "url", asString(payload["url"])); err != nil {
			return err
		}
		if err := absolute("repository", "html_url", asString(payload["html_url"])); err != nil {
			return err
		}
		owner, _ := payload["owner"].(map[string]any)
		if owner == nil {
			return deviate("an owner object", "absent", "the repository carries no owner")
		}
		for _, key := range []string{"url", "html_url", "avatar_url"} {
			if err := absolute("repository.owner", key, strings.SplitN(asString(owner[key]), "{", 2)[0]); err != nil {
				return err
			}
		}
		return nil
	})

	rec.check(domain, "issue author hypermedia is absolute", "GET /repos/{owner}/{repo}/issues/{number}", func() error {
		if set.repo == "" || set.issue == 0 {
			return deviate("an issue fixture", "none", "no issue fixture exists")
		}
		var payload map[string]any
		if _, err := decodeInto(client, http.MethodGet,
			fmt.Sprintf("repos/%s/%s/issues/%d", set.owner, set.repo, set.issue), nil, &payload); err != nil {
			return err
		}
		if err := absolute("issue", "html_url", asString(payload["html_url"])); err != nil {
			return err
		}
		user, _ := payload["user"].(map[string]any)
		if user == nil {
			return deviate("a user object", "absent", "the issue carries no author")
		}
		for _, key := range []string{"url", "html_url", "avatar_url"} {
			if err := absolute("issue.user", key, strings.SplitN(asString(user[key]), "{", 2)[0]); err != nil {
				return err
			}
		}
		return nil
	})
}

func asString(value any) string {
	text, _ := value.(string)
	return text
}
