package bleephub

// The three git reference writes, factored out of their REST handlers so the
// GraphQL createRef/updateRef/deleteRef/updateRefs mutations perform exactly
// the same act.
//
// Everything a ref write carries with it lives here: the branch-protection
// refusal, the secret-scanning push-protection block, the fast-forward test,
// the compare-and-set against the reference the caller saw, the push
// machinery, and the create/delete webhooks. A second implementation of any of
// them would be a way around branch protection, which is why the mutations ask
// here rather than reaching for the storer themselves.

import (
	"context"
	"errors"
	"net/http"

	"github.com/go-git/go-git/v5/plumbing"
	gitStorage "github.com/go-git/go-git/v5/storage"

	"github.com/e6qu/bleephub/internal/gitstore"
	"github.com/e6qu/bleephub/internal/store"
)

// gitRefWriteFailure is a refused or failed reference write, carrying the HTTP
// status and message the REST route reports so the two surfaces answer the
// same refusal with the same words.
type gitRefWriteFailure struct {
	status  int
	message string
	// resource and field, when set, make this a 422 validation error rather
	// than a plain message.
	resource string
	field    string
	code     string
	// blocked, when set, is the secret-scanning push-protection placeholder
	// the push tripped; the REST route renders it in GitHub's block shape.
	blocked *store.SecretScanningPushProtectionPlaceholder
}

func (e *gitRefWriteFailure) Error() string { return e.message }

// write renders the failure onto a REST response.
func (e *gitRefWriteFailure) write(w http.ResponseWriter) {
	switch {
	case e.blocked != nil:
		writeSecretScanningPushProtectionBlocked(w, e.blocked)
	case e.resource != "":
		store.WriteGHValidationError(w, e.resource, e.field, e.code)
	default:
		writeGHError(w, e.status, e.message)
	}
}

func refWriteInvalid(resource, field, code string) *gitRefWriteFailure {
	return &gitRefWriteFailure{status: http.StatusUnprocessableEntity, message: field + " is " + code, resource: resource, field: field, code: code}
}

// createGitRef creates fullRef at target. It is the whole body of POST
// /repos/{owner}/{repo}/git/refs after the request has been decoded.
func (s *Server) createGitRef(ctx context.Context, repo *store.Repo, stor gitStorage.Storer, sender *store.User,
	fullRef plumbing.ReferenceName, target plumbing.Hash, baseURL string) *gitRefWriteFailure {
	if !validFullyQualifiedGitRef(fullRef.String()) {
		return refWriteInvalid("Reference", "ref", "invalid")
	}
	if _, err := stor.EncodedObject(plumbing.AnyObject, target); err != nil {
		return &gitRefWriteFailure{status: http.StatusUnprocessableEntity, message: "Object does not exist"}
	}
	if refusal := s.protectedRefWriteRefusal(ctx, repo, stor, fullRef, refCreation, target); refusal != "" {
		return &gitRefWriteFailure{status: http.StatusForbidden, message: refusal}
	}
	if placeholder, err := s.secretScanningPushProtectionPlaceholderForRef(repo, stor, fullRef, target); err != nil {
		return &gitRefWriteFailure{status: http.StatusInternalServerError, message: err.Error()}
	} else if placeholder != nil {
		return &gitRefWriteFailure{status: http.StatusForbidden, message: "push protection", blocked: placeholder}
	}
	if err := gitstore.CreateReferenceIfAbsent(stor, plumbing.NewHashReference(fullRef, target)); err != nil {
		if errors.Is(err, gitstore.ErrReferenceAlreadyExists) {
			return &gitRefWriteFailure{status: http.StatusUnprocessableEntity, message: "Reference already exists"}
		}
		return &gitRefWriteFailure{status: http.StatusInternalServerError, message: err.Error()}
	}
	if err := s.scanRefForSecretScanning(repo, stor, fullRef, target, baseURL); err != nil {
		return &gitRefWriteFailure{status: http.StatusInternalServerError, message: err.Error()}
	}
	if fullRef.IsBranch() {
		s.afterCommittedRefUpdate(repo, sender, fullRef.String(), plumbing.ZeroHash.String(), target.String(), baseURL)
	}
	// The `create` event fires for a newly-created branch or tag (distinct
	// from the `push` afterCommittedRefUpdate emits for branches).
	payload := buildRefLifecyclePayload(repo, fullRef, sender, baseURL)
	payload["master_branch"] = repo.DefaultBranch
	payload["description"] = repo.Description
	s.emitWebhookEvent(repo.FullName, "create", "", payload)
	return nil
}

// updateGitRef moves fullRef to target. It is the whole body of PATCH
// /repos/{owner}/{repo}/git/refs/{ref} after the request has been decoded.
func (s *Server) updateGitRef(ctx context.Context, repo *store.Repo, stor gitStorage.Storer, sender *store.User,
	fullRef plumbing.ReferenceName, target plumbing.Hash, force bool, baseURL string) *gitRefWriteFailure {
	if !validFullyQualifiedGitRef(fullRef.String()) {
		return refWriteInvalid("Reference", "sha", "invalid")
	}
	kind := refFastForward
	if force {
		kind = refForcePush
	}
	if refusal := s.protectedRefWriteRefusal(ctx, repo, stor, fullRef, kind, target); refusal != "" {
		return &gitRefWriteFailure{status: http.StatusForbidden, message: refusal}
	}
	oldRef, err := stor.Reference(fullRef)
	if err != nil {
		return &gitRefWriteFailure{status: http.StatusUnprocessableEntity, message: "Reference does not exist"}
	}
	if _, err := stor.EncodedObject(plumbing.AnyObject, target); err != nil {
		return &gitRefWriteFailure{status: http.StatusUnprocessableEntity, message: "Object does not exist"}
	}
	if !force && oldRef.Type() == plumbing.HashReference && target != oldRef.Hash() {
		fastForward, err := refUpdateIsFastForward(stor, oldRef.Hash(), target)
		if err != nil {
			return &gitRefWriteFailure{status: http.StatusInternalServerError, message: err.Error()}
		}
		if !fastForward {
			return &gitRefWriteFailure{status: http.StatusUnprocessableEntity, message: "Update is not a fast forward"}
		}
	}
	if placeholder, err := s.secretScanningPushProtectionPlaceholderForRef(repo, stor, fullRef, target); err != nil {
		return &gitRefWriteFailure{status: http.StatusInternalServerError, message: err.Error()}
	} else if placeholder != nil {
		return &gitRefWriteFailure{status: http.StatusForbidden, message: "push protection", blocked: placeholder}
	}
	if err := stor.CheckAndSetReference(plumbing.NewHashReference(fullRef, target), oldRef); err != nil {
		if errors.Is(err, gitStorage.ErrReferenceHasChanged) {
			return &gitRefWriteFailure{status: http.StatusConflict, message: "Reference update failed: the ref changed while the request was being processed"}
		}
		return &gitRefWriteFailure{status: http.StatusInternalServerError, message: err.Error()}
	}
	if err := s.scanRefForSecretScanning(repo, stor, fullRef, target, baseURL); err != nil {
		return &gitRefWriteFailure{status: http.StatusInternalServerError, message: err.Error()}
	}
	if fullRef.IsBranch() {
		s.afterCommittedRefUpdate(repo, sender, fullRef.String(), oldRef.Hash().String(), target.String(), baseURL)
	}
	return nil
}

// deleteGitRef removes fullRef. It is the whole body of DELETE
// /repos/{owner}/{repo}/git/refs/{ref}, including the branch-protection
// refusal the route decorator asks before the handler runs.
func (s *Server) deleteGitRef(ctx context.Context, repo *store.Repo, stor gitStorage.Storer, sender *store.User,
	fullRef plumbing.ReferenceName, baseURL string) *gitRefWriteFailure {
	if !validFullyQualifiedGitRef(fullRef.String()) {
		return refWriteInvalid("Reference", "ref", "invalid")
	}
	if refusal := s.protectedRefWriteRefusal(ctx, repo, stor, fullRef, refDeletion, plumbing.ZeroHash); refusal != "" {
		return &gitRefWriteFailure{status: http.StatusForbidden, message: refusal}
	}
	oldRef, err := stor.Reference(fullRef)
	if err != nil {
		return &gitRefWriteFailure{status: http.StatusUnprocessableEntity, message: "Reference does not exist"}
	}
	if err := gitstore.RemoveReferenceCAS(stor, oldRef); err != nil {
		if errors.Is(err, gitStorage.ErrReferenceHasChanged) {
			return &gitRefWriteFailure{status: http.StatusConflict, message: "Reference changed while it was being deleted"}
		}
		return &gitRefWriteFailure{status: http.StatusInternalServerError, message: err.Error()}
	}
	if fullRef.IsBranch() {
		s.afterCommittedRefUpdate(repo, sender, fullRef.String(), oldRef.Hash().String(), plumbing.ZeroHash.String(), baseURL)
	}
	// The `delete` event fires for a removed branch or tag.
	s.emitWebhookEvent(repo.FullName, "delete", "", buildRefLifecyclePayload(repo, fullRef, sender, baseURL))
	return nil
}
