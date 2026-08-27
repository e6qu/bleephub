package bleephub

import (
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/e6qu/bleephub/internal/store"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	gitStorage "github.com/go-git/go-git/v5/storage"
)

// Create a repository from a template. The generated repo carries the template's
// files as one fresh initial commit per branch, not the template's history.

func (s *Server) registerGHRepoGenerateRoutes() {
	s.route("POST /api/v3/repos/{template_owner}/{template_repo}/generate", s.handleGenerateRepoFromTemplate)
}

func (s *Server) handleGenerateRepoFromTemplate(w http.ResponseWriter, r *http.Request) {
	user := ghUserFromContext(r.Context())
	if user == nil {
		writeGHError(w, http.StatusUnauthorized, "Bad credentials")
		return
	}
	templateOwner := r.PathValue("template_owner")
	templateName := r.PathValue("template_repo")
	template := s.store.GetRepo(templateOwner, templateName)
	if template == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	if template.Private && !s.viewerCanReadRepo(r.Context(), template) {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	if !template.IsTemplate {
		writeGHError(w, http.StatusBadRequest, "Repository is not a template.")
		return
	}

	var req struct {
		Owner              string   `json:"owner"`
		Name               string   `json:"name"`
		Description        string   `json:"description"`
		IncludeAllBranches flexBool `json:"include_all_branches"`
		Private            flexBool `json:"private"`
	}
	if !decodeJSONBody(w, r, &req) {
		return
	}
	if req.Name == "" {
		store.WriteGHValidationError(w, "Repository", "name", "missing_field")
		return
	}

	// Owner is the caller by default, or an org the caller is a member of.
	var repo *store.Repo
	switch {
	case req.Owner == "" || strings.EqualFold(req.Owner, user.Login):
		repo = s.store.CreateRepo(user, req.Name, req.Description, bool(req.Private))
	default:
		org := s.store.GetOrg(req.Owner)
		if org == nil {
			writeGHError(w, http.StatusForbidden, "You may only generate repositories for your own account or an organization you are a member of.")
			return
		}
		if !s.viewerIsOrgMember(r.Context(), org.Login) {
			writeGHError(w, http.StatusForbidden, "You must be a member of the organization to create a repository in it.")
			return
		}
		repo = s.store.CreateOrgRepo(org, user, req.Name, req.Description, bool(req.Private))
	}
	if repo == nil {
		store.WriteGHValidationError(w, "Repository", "name", "already_exists")
		return
	}

	ownerLogin, _, _ := store.SplitRepoFullName(repo.FullName)
	if template.DefaultBranch != repo.DefaultBranch {
		s.store.UpdateRepo(ownerLogin, repo.Name, func(rp *store.Repo) {
			rp.DefaultBranch = template.DefaultBranch
		})
	}

	templateStor := s.store.GetGitStorage(templateOwner, templateName)
	newStor := s.store.GetGitStorage(ownerLogin, repo.Name)
	sig := repoSignature(store.CoalesceStr(user.Name, user.Login), store.CoalesceStr(user.Email, user.Login+"@bleephub.local"))
	if err := generateFromTemplateStorage(templateStor, newStor, template.DefaultBranch, bool(req.IncludeAllBranches), sig); err != nil {
		if _, deleteErr := s.store.DeleteRepo(ownerLogin, repo.Name); deleteErr != nil {
			writeGHError(w, http.StatusInternalServerError, "repository rollback failed: "+deleteErr.Error())
			return
		}
		writeGHError(w, http.StatusUnprocessableEntity, "Could not generate repository from template: "+err.Error())
		return
	}
	s.store.UpdateRepo(ownerLogin, repo.Name, func(rp *store.Repo) {
		rp.TemplateRepoID = template.ID
		rp.PushedAt = time.Now().UTC()
	})

	repo = s.store.GetRepoByFullName(repo.FullName)
	s.recordAuditEvent("repo.generate", user.Login, "", map[string]interface{}{
		"repo": repo.FullName, "repo_id": repo.ID, "template": template.FullName,
	})
	repoJSON := fullRepoJSONForViewer(repo, s.store, s.baseURL(r), user)
	writeJSONCreated(w, jsonStringField(repoJSON, "url"), repoJSON)
}

// generateFromTemplateStorage copies the template's tree and blob objects into
// dst and roots one fresh initial commit per selected branch, authored by sig.
// The template's commit history is not carried over; HEAD ends on defaultBranch.
func generateFromTemplateStorage(src, dst gitStorage.Storer, defaultBranch string, includeAllBranches bool, sig *object.Signature) error {
	if src == nil {
		return nil
	}

	branches := map[string]plumbing.Hash{}
	refIter, err := src.IterReferences()
	if err != nil {
		return err
	}
	if err := refIter.ForEach(func(ref *plumbing.Reference) error {
		if !ref.Name().IsBranch() || ref.Type() != plumbing.HashReference {
			return nil
		}
		name := ref.Name().Short()
		if includeAllBranches || name == defaultBranch {
			branches[name] = ref.Hash()
		}
		return nil
	}); err != nil {
		refIter.Close()
		return err
	}
	refIter.Close()

	// An empty template generates an empty repository.
	if len(branches) == 0 {
		return nil
	}

	// Copy tree + blob objects (commits are minted fresh below), re-encoding
	// through dst because a storer only accepts its own object implementation.
	for _, t := range []plumbing.ObjectType{plumbing.TreeObject, plumbing.BlobObject} {
		iter, err := src.IterEncodedObjects(t)
		if err != nil {
			return err
		}
		copyErr := iter.ForEach(func(obj plumbing.EncodedObject) error {
			return copyEncodedObject(dst, obj)
		})
		iter.Close()
		if copyErr != nil {
			return copyErr
		}
	}

	for name, commitHash := range branches {
		commit, err := object.GetCommit(src, commitHash)
		if err != nil {
			return err
		}
		initial := &object.Commit{
			Author:    *sig,
			Committer: *sig,
			Message:   "Initial commit",
			TreeHash:  commit.TreeHash,
		}
		newHash, err := encodeCommit(dst, initial)
		if err != nil {
			return err
		}
		if err := dst.SetReference(plumbing.NewHashReference(plumbing.NewBranchReferenceName(name), newHash)); err != nil {
			return err
		}
	}

	headBranch := defaultBranch
	if _, ok := branches[headBranch]; !ok {
		for name := range branches {
			headBranch = name
			break
		}
	}
	return store.SetGitHeadBranch(dst, headBranch)
}

// copyEncodedObject writes one encoded git object into dst through dst's own
// object implementation.
func copyEncodedObject(dst gitStorage.Storer, obj plumbing.EncodedObject) error {
	newObj := dst.NewEncodedObject()
	newObj.SetType(obj.Type())
	newObj.SetSize(obj.Size())
	w, err := newObj.Writer()
	if err != nil {
		return err
	}
	r, err := obj.Reader()
	if err != nil {
		_ = w.Close()
		return err
	}
	if _, err := io.Copy(w, r); err != nil {
		_ = r.Close()
		_ = w.Close()
		return err
	}
	if err := r.Close(); err != nil {
		_ = w.Close()
		return err
	}
	if err := w.Close(); err != nil {
		return err
	}
	_, err = dst.SetEncodedObject(newObj)
	return err
}
