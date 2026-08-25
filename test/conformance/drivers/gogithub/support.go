// Shared fixtures for the domain files that sit alongside main.go.
//
// Every domain added here owns its own repository (and, where it needs one,
// its own organization or second user) so that one domain's fixture failure
// cannot cascade into another's results. That is the same isolation rule the
// original driver applies; it just needs helpers now that there is more than
// one group of operations.
package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	github "github.com/google/go-github/v88/github"
)

// scratch is a repository provisioned for exactly one group of operations,
// together with the head commit of its default branch. A zero repo field means
// provisioning failed; callers record a skip rather than a wall of failures
// that all say the same thing.
type scratch struct {
	owner  string
	repo   string
	branch string
	sha    string
	id     int64
}

func (s *scratch) ok() bool { return s != nil && s.repo != "" }

// newScratch creates an auto-initialised repository owned by the authenticated
// user and reads back the head commit of its default branch.
func newScratch(client *github.Client, owner, name string) *scratch {
	set := &scratch{owner: owner, branch: "main"}
	created, _, err := client.Repositories.Create(ctx, "", &github.Repository{
		Name:      github.Ptr(name),
		AutoInit:  github.Ptr(true),
		HasIssues: github.Ptr(true),
		HasWiki:   github.Ptr(true),
	})
	if err != nil {
		return set
	}
	set.repo = created.GetName()
	set.id = created.GetID()
	if created.GetDefaultBranch() != "" {
		set.branch = created.GetDefaultBranch()
	}
	if ref, _, err := client.Git.GetRef(ctx, owner, set.repo, "refs/heads/"+set.branch); err == nil {
		set.sha = ref.GetObject().GetSHA()
	}
	return set
}

// newScratchInOrg is newScratch for a repository owned by an organization,
// which is the only place team, restriction and outside-collaborator
// operations exist.
func newScratchInOrg(client *github.Client, org, name string) *scratch {
	set := &scratch{owner: org, branch: "main"}
	created, _, err := client.Repositories.Create(ctx, org, &github.Repository{
		Name:      github.Ptr(name),
		AutoInit:  github.Ptr(true),
		HasIssues: github.Ptr(true),
	})
	if err != nil {
		return set
	}
	set.repo = created.GetName()
	set.id = created.GetID()
	if created.GetDefaultBranch() != "" {
		set.branch = created.GetDefaultBranch()
	}
	if ref, _, err := client.Git.GetRef(ctx, org, set.repo, "refs/heads/"+set.branch); err == nil {
		set.sha = ref.GetObject().GetSHA()
	}
	return set
}

// skipAll records one skip per operation a group could not attempt, so the
// scoreboard says "did not run" for each identifier rather than silently
// shrinking.
func skipAll(rec *recorder, domain, request, why string, operations ...string) {
	for _, operation := range operations {
		rec.skip1(domain, operation, request, why)
	}
}

// wantStatus asserts the client saw a particular HTTP status. It is used where
// the status itself is the contract (204 versus 200, 201 on a rerequest), not
// as a substitute for checking the decoded value.
func wantStatus(resp *github.Response, want int, what string) error {
	if resp == nil {
		return deviate(fmt.Sprintf("%d", want), "no response", "%s produced no response", what)
	}
	if resp.StatusCode != want {
		return deviate(fmt.Sprintf("%d", want), fmt.Sprintf("%d", resp.StatusCode),
			"%s answered %d, not %d", what, resp.StatusCode, want)
	}
	return nil
}

// wantHTTPError asserts a call failed with a specific status and that the
// client decoded the body into its typed error. A bare transport failure is a
// different defect from a well-formed refusal, and clients branch on the
// difference.
func wantHTTPError(err error, want int, what string) error {
	if err == nil {
		return deviate(fmt.Sprintf("%d", want), "success", "%s succeeded when it should have been refused", what)
	}
	var apiErr *github.ErrorResponse
	if !errors.As(err, &apiErr) {
		return deviate("*github.ErrorResponse", fmt.Sprintf("%T: %v", err, err),
			"%s did not produce a decodable GitHub error body", what)
	}
	if apiErr.Response == nil || apiErr.Response.StatusCode != want {
		got := "no response"
		if apiErr.Response != nil {
			got = fmt.Sprintf("%d", apiErr.Response.StatusCode)
		}
		return deviate(fmt.Sprintf("%d", want), got, "%s answered %s, not %d", what, got, want)
	}
	if apiErr.Message == "" {
		return deviate("message populated", "empty", "%s returned an error body with no message", what)
	}
	return nil
}

// otherClient builds a second go-github client bound to a different credential
// — a second user's token, or a GitHub App's JSON Web Token.
func otherClient(credential string) (*github.Client, error) {
	return github.NewClient(
		github.WithAuthToken(credential),
		github.WithEnterpriseURLs(baseURL+"/", baseURL+"/"),
	)
}

// principal is a second account the driver provisions through GitHub
// Enterprise Server's site-admin API, which is the only way a client can make
// one. Having a second identity is what makes "404 for a private repository
// you cannot see" and the whole invitation lifecycle testable at all.
type principal struct {
	login  string
	token  string
	client *github.Client
}

func (p *principal) ok() bool { return p != nil && p.token != "" && p.client != nil }

func newPrincipal(login string) *principal {
	person := &principal{}
	status, body, err := raw(http.MethodPost, "/api/v3/admin/users", map[string]any{
		"login": login, "email": login + "@conformance.invalid",
	})
	if err != nil || (status != http.StatusCreated && status != http.StatusOK) {
		return person
	}
	var created struct {
		Login string `json:"login"`
	}
	if json.Unmarshal(body, &created) != nil || created.Login == "" {
		return person
	}
	person.login = created.Login
	status, body, err = raw(http.MethodPost, "/api/v3/admin/users/"+created.Login+"/authorizations",
		map[string]any{"scopes": []string{"repo", "user", "admin:org", "workflow"}})
	if err != nil || (status != http.StatusCreated && status != http.StatusOK) {
		return person
	}
	var authorization struct {
		Token string `json:"token"`
	}
	if json.Unmarshal(body, &authorization) != nil || authorization.Token == "" {
		return person
	}
	person.token = authorization.Token
	if client, err := otherClient(authorization.Token); err == nil {
		person.client = client
	}
	return person
}

// commitFile writes a file on a branch and returns the resulting commit, so a
// group that needs real content (a workflow file, a second commit to compare)
// does not have to repeat the boilerplate.
func commitFile(client *github.Client, set *scratch, path, message, content string) (string, error) {
	result, _, err := client.Repositories.CreateFile(ctx, set.owner, set.repo, path,
		&github.RepositoryContentFileOptions{
			Message: github.Ptr(message),
			Content: []byte(content),
			Branch:  github.Ptr(set.branch),
		})
	if err != nil {
		return "", err
	}
	return result.Commit.GetSHA(), nil
}

// pollUntil retries a condition on a bounded schedule. Nothing in the harness
// sleeps for a fixed period as a substitute for synchronisation: an operation
// whose result is genuinely asynchronous polls, and records a clear failure
// when the deadline passes.
func pollUntil(what string, timeout time.Duration, condition func() (bool, error)) error {
	deadline := time.Now().Add(timeout)
	var last error
	for {
		done, err := condition()
		if err == nil && done {
			return nil
		}
		last = err
		if time.Now().After(deadline) {
			if last != nil {
				return deviate(what, last.Error(), "%s did not happen within %s: %v", what, timeout, last)
			}
			return deviate(what, "still not true", "%s did not happen within %s", what, timeout)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// decodeInto issues a request through the client's own transport and decodes
// the response into v. It is for endpoints the software development kit has no
// typed method for; the decode is still the client's, so a shape mismatch
// still fails here.
func decodeInto(client *github.Client, method, path string, body, v any) (*github.Response, error) {
	request, err := client.NewRequest(ctx, method, strings.TrimPrefix(path, "/"), body)
	if err != nil {
		return nil, err
	}
	return client.Do(request, v)
}
