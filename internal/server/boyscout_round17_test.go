package bleephub

import (
	"io"
	"net/http"
	"testing"
)

// TestOrgSecretRejectsForeignRepo pins that an org secret's selected-repository
// set may only name repositories the org owns. A foreign repo id (here a
// user-owned repo) is rejected with 422, closing a cross-tenant disclosure
// oracle where the selected-repos listing rendered arbitrary private repos.
func TestOrgSecretRejectsForeignRepo(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	org := s.seedTestOrg(t, "acme")
	orgRepo := s.seedOrgRepo(t, org, "widget", false)
	foreign := s.seedRepo(t, "personal", true) // admin/personal — not owned by acme
	enc, keyID := s.sealForServer(t, "s3cr3t")

	put := func(repoID int) int {
		resp := s.put(t, "/api/v3/orgs/acme/actions/secrets/DEPLOY_KEY", defaultToken, map[string]interface{}{
			"encrypted_value":         enc,
			"key_id":                  keyID,
			"visibility":              "selected",
			"selected_repository_ids": []int{repoID},
		})
		resp.Body.Close()
		return resp.StatusCode
	}

	if code := put(foreign.ID); code != http.StatusUnprocessableEntity {
		t.Fatalf("org secret with a foreign repo = %d, want 422", code)
	}
	if code := put(orgRepo.ID); code != http.StatusCreated && code != http.StatusNoContent {
		t.Fatalf("org secret with an org-owned repo = %d, want 201/204", code)
	}
}

// TestPrivateContainerImageHiddenFromOtherUser pins that a pushed container
// image is private by default and its manifest is not readable by an unrelated
// authenticated user (the registry pull path now enforces canViewPackage).
func TestPrivateContainerImageHiddenFromOtherUser(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	s.publishContainerPackageVersion(t, "admin", "secret-img", "1.0")

	// The package is private by default.
	if p := s.store.GetPackage("admin", "container", "secret-img"); p == nil || p.Visibility != "private" {
		t.Fatalf("pushed container package should be private, got %+v", p)
	}

	_, strangerTok := s.newUser(t, "reg-stranger")
	get := func(tok string) int {
		req, _ := http.NewRequest(http.MethodGet, s.baseURL+"/v2/admin/secret-img/manifests/1.0", nil)
		req.Header.Set("Authorization", "Bearer "+tok)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		return resp.StatusCode
	}

	if code := get(strangerTok); code != http.StatusNotFound {
		t.Fatalf("stranger pulling a private image manifest = %d, want 404", code)
	}
	if code := get(defaultToken); code != http.StatusOK {
		t.Fatalf("owner pulling their own image manifest = %d, want 200", code)
	}
}

// TestDiscussionLockBlocksCommentAndUpvote pins that once a discussion is
// locked, a non-collaborator can neither comment on it nor upvote it via
// GraphQL (the resolvers previously ignored the lock).
func TestDiscussionLockBlocksCommentAndUpvote(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	f := newGQLAuthzFixture(t, s.Server, "disc-lock", false) // public repo

	s.store.Mu.Lock()
	s.store.Discussions[f.discussion.ID].Locked = true
	s.store.Mu.Unlock()

	comment := s.gqlAuthzPost(t, f.strangerToken,
		`mutation($input:AddDiscussionCommentInput!){addDiscussionComment(input:$input){comment{id}}}`,
		map[string]interface{}{"input": map[string]interface{}{
			"discussionId": f.discussion.NodeID, "body": "sneaking in",
		}})
	if len(gqlAuthzErrors(comment)) == 0 {
		t.Fatalf("non-collaborator commented on a locked discussion: %v", comment)
	}

	upvote := s.gqlAuthzPost(t, f.strangerToken,
		`mutation($input:AddUpvoteInput!){addUpvote(input:$input){clientMutationId}}`,
		map[string]interface{}{"input": map[string]interface{}{"subjectId": f.discussion.NodeID}})
	if len(gqlAuthzErrors(upvote)) == 0 {
		t.Fatalf("upvoted a locked discussion: %v", upvote)
	}
}

// TestDiscussionReplyToReplyRejected pins that discussion comment threads are
// exactly one level deep — replying to a reply is rejected.
func TestDiscussionReplyToReplyRejected(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	f := newGQLAuthzFixture(t, s.Server, "disc-thread", false)

	post := func(input map[string]interface{}) map[string]interface{} {
		return s.gqlAuthzPost(t, f.ownerToken,
			`mutation($input:AddDiscussionCommentInput!){addDiscussionComment(input:$input){comment{id}}}`,
			map[string]interface{}{"input": input})
	}
	top := post(map[string]interface{}{"discussionId": f.discussion.NodeID, "body": "top"})
	topID := nestedString(t, top, "data", "addDiscussionComment", "comment", "id")
	reply := post(map[string]interface{}{"discussionId": f.discussion.NodeID, "body": "reply", "replyToId": topID})
	replyID := nestedString(t, reply, "data", "addDiscussionComment", "comment", "id")

	nested := post(map[string]interface{}{"discussionId": f.discussion.NodeID, "body": "nested", "replyToId": replyID})
	if len(gqlAuthzErrors(nested)) == 0 {
		t.Fatalf("a reply-to-a-reply was accepted: %v", nested)
	}
}
