package bleephub

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// The registered route table is the authority on what surface exists, so it is
// also the right thing to drive an authorization check from: a test that walks
// s.routePatterns cannot go stale when a route is added, the way a hand-written
// list of endpoints does.
//
// The property asserted here is deliberately narrow and absolute, so that it
// needs no per-endpoint judgement: a user who is not the owner, not a
// collaborator, not an org member and not a site admin must never receive a 2xx
// from a state-changing request against somebody else's private repository. Any
// 2xx is either a missing authorization check or a route that mutates state it
// was never asked to.

// authzMatrixSubstitute fills a registered pattern's wildcards with values that
// resolve: the target repository for {owner}/{repo}, and an innocuous literal
// everywhere else. Unresolvable ids are fine — the handler is expected to
// refuse on authorization before it ever looks the id up, and a 404 from a
// missing id is an accepted outcome.
func authzMatrixSubstitute(pattern, owner, repo string) (method, path string, ok bool) {
	method, rest, found := strings.Cut(pattern, " ")
	if !found {
		return "", "", false
	}
	segments := strings.Split(rest, "/")
	for i, seg := range segments {
		if !strings.HasPrefix(seg, "{") {
			continue
		}
		name := strings.Trim(seg, "{}")
		name = strings.TrimSuffix(name, "...")
		switch name {
		case "owner", "org", "username", "enterprise":
			segments[i] = owner
		case "repo":
			segments[i] = repo
		case "$":
			segments[i] = ""
		default:
			// Numeric-looking parameters get a number so the handler's own
			// parse succeeds and it reaches its authorization check rather
			// than bouncing off a 400.
			segments[i] = "1"
		}
	}
	return method, strings.Join(segments, "/"), true
}

func TestPrivateRepoMutationsRejectAnUnrelatedUser(t *testing.T) {
	const ownerLogin = "authzmatrix-owner"
	const repoName = "authzmatrix-private"

	store := testServer.store

	now := time.Now().UTC()
	store.mu.Lock()
	owner := &User{ID: store.NextUser, Login: ownerLogin, Type: "User", CreatedAt: now, UpdatedAt: now}
	store.Users[owner.ID] = owner
	store.UsersByLogin[owner.Login] = owner
	store.NextUser++
	stranger := &User{ID: store.NextUser, Login: "authzmatrix-stranger", Type: "User", CreatedAt: now, UpdatedAt: now}
	store.Users[stranger.ID] = stranger
	store.UsersByLogin[stranger.Login] = stranger
	store.NextUser++
	store.mu.Unlock()

	if store.CreateRepo(owner, repoName, "private fixture", true) == nil {
		t.Fatalf("could not create the private fixture repository")
	}
	strangerToken := store.CreateToken(stranger.ID, "repo, workflow, read:org, admin:org, gist")
	if strangerToken == nil {
		t.Fatalf("could not mint a token for the unrelated user")
	}

	handler := testServer.ghHeadersMiddleware(testServer.mux)

	var admitted []string
	for _, pattern := range testServer.routePatterns {
		if !strings.Contains(pattern, "/api/v3/repos/{owner}/{repo}") {
			continue
		}
		method, path, ok := authzMatrixSubstitute(pattern, ownerLogin, repoName)
		if !ok {
			continue
		}
		switch method {
		case http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
		default:
			continue
		}

		req := httptest.NewRequest(method, path, bytes.NewReader([]byte("{}")))
		req.Header.Set("Authorization", "token "+strangerToken.Value)
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)

		if w.Code >= 200 && w.Code < 300 {
			admitted = append(admitted, pattern+" -> "+http.StatusText(w.Code))
		}
	}

	if len(admitted) > 0 {
		t.Errorf("%d state-changing routes admitted a user with no access to a private repository:", len(admitted))
		for _, entry := range admitted {
			t.Errorf("  %s", entry)
		}
	}
}

// TestPrivateRepoReadsRejectAnUnrelatedUser is the read-side companion. A
// private repository must not disclose its contents, refs, labels, protection
// settings or CI results to a user with no access, and it must answer the same
// way it answers for a repository that does not exist so the response cannot be
// used to prove the repository is there.
func TestPrivateRepoReadsRejectAnUnrelatedUser(t *testing.T) {
	const ownerLogin = "authzread-owner"
	const repoName = "authzread-private"

	store := testServer.store

	now := time.Now().UTC()
	store.mu.Lock()
	owner := &User{ID: store.NextUser, Login: ownerLogin, Type: "User", CreatedAt: now, UpdatedAt: now}
	store.Users[owner.ID] = owner
	store.UsersByLogin[owner.Login] = owner
	store.NextUser++
	store.mu.Unlock()

	if store.CreateRepo(owner, repoName, "private fixture", true) == nil {
		t.Fatalf("could not create the private fixture repository")
	}

	handler := testServer.ghHeadersMiddleware(testServer.mux)

	// Anonymous, which is the sharpest case: no credential at all.
	paths := []string{
		"/api/v3/repos/" + ownerLogin + "/" + repoName + "/contents/README.md",
		"/api/v3/repos/" + ownerLogin + "/" + repoName + "/commits",
		"/api/v3/repos/" + ownerLogin + "/" + repoName + "/readme",
		"/api/v3/repos/" + ownerLogin + "/" + repoName + "/branches",
		"/api/v3/repos/" + ownerLogin + "/" + repoName + "/tags",
		"/api/v3/repos/" + ownerLogin + "/" + repoName + "/labels",
		"/api/v3/repos/" + ownerLogin + "/" + repoName + "/milestones",
	}

	var disclosed []string
	for _, path := range paths {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)
		if w.Code >= 200 && w.Code < 300 {
			disclosed = append(disclosed, path)
		}
	}

	if len(disclosed) > 0 {
		t.Errorf("%d private-repository reads answered an anonymous caller:", len(disclosed))
		for _, path := range disclosed {
			t.Errorf("  GET %s", path)
		}
	}
}
