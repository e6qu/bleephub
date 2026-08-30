package bleephub

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/e6qu/bleephub/internal/store"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	gitStorage "github.com/go-git/go-git/v5/storage"
)

// releaseExcludedFromLatest interprets the make_latest body field, which GitHub
// accepts as "true"|"false"|"legacy" (and, from some clients, a bool). Only an
// explicit false excludes the release from "latest".
func releaseExcludedFromLatest(v interface{}) bool {
	switch t := v.(type) {
	case string:
		return t == "false"
	case bool:
		return !t
	default:
		return false
	}
}

func (s *Server) registerGHReleasesRoutes() {
	s.route("POST /api/v3/repos/{owner}/{repo}/releases",
		s.requirePerm(store.ScopeContents, store.PermWrite, s.handleCreateRelease))
	s.route("GET /api/v3/repos/{owner}/{repo}/releases",
		s.handleListReleases)
	s.route("GET /api/v3/repos/{owner}/{repo}/releases/latest",
		s.handleGetLatestRelease)
	s.route("POST /api/v3/repos/{owner}/{repo}/releases/generate-notes",
		s.requirePerm(store.ScopeContents, store.PermWrite, s.handleGenerateReleaseNotes))

	s.route("GET /api/v3/repos/{owner}/{repo}/releases/{release_id}",
		s.handleGetRelease)
	s.route("PATCH /api/v3/repos/{owner}/{repo}/releases/{release_id}",
		s.requirePerm(store.ScopeContents, store.PermWrite, s.handleUpdateRelease))
	s.route("DELETE /api/v3/repos/{owner}/{repo}/releases/{release_id}",
		s.requirePerm(store.ScopeContents, store.PermWrite, s.handleDeleteRelease))

	// Go 1.22's mux refuses to register /releases/tags/{tag} and
	// /releases/{release_id}/assets directly (they overlap without either being
	// more specific), so one dispatcher handles the prefix and routeDispatch
	// records the real endpoints for RegisteredRoutes() to enumerate.
	s.routeDispatch("GET /api/v3/repos/{owner}/{repo}/releases/{p1}/{p2}",
		s.handleReleaseTwoSegDispatch("GET"),
		"GET /api/v3/repos/{owner}/{repo}/releases/tags/{tag}",
		"GET /api/v3/repos/{owner}/{repo}/releases/assets/{asset_id}",
		"GET /api/v3/repos/{owner}/{repo}/releases/{release_id}/assets",
		"GET /api/v3/repos/{owner}/{repo}/releases/{release_id}/reactions")
	s.routeDispatch("POST /api/v3/repos/{owner}/{repo}/releases/{p1}/{p2}",
		s.handleReleaseTwoSegDispatch("POST"),
		"POST /api/v3/repos/{owner}/{repo}/releases/{release_id}/assets",
		"POST /api/v3/repos/{owner}/{repo}/releases/{release_id}/reactions")
	s.route("POST /api/uploads/repos/{owner}/{repo}/releases/{release_id}/assets",
		s.requirePerm(store.ScopeContents, store.PermWrite, s.handleUploadReleaseAsset))
	s.routeDispatch("PATCH /api/v3/repos/{owner}/{repo}/releases/{p1}/{p2}",
		s.handleReleaseTwoSegDispatch("PATCH"),
		"PATCH /api/v3/repos/{owner}/{repo}/releases/assets/{asset_id}")
	s.routeDispatch("DELETE /api/v3/repos/{owner}/{repo}/releases/{p1}/{p2}",
		s.handleReleaseTwoSegDispatch("DELETE"),
		"DELETE /api/v3/repos/{owner}/{repo}/releases/assets/{asset_id}")

	// Three segments are only release-reaction deletion.
	s.routeDispatch("DELETE /api/v3/repos/{owner}/{repo}/releases/{p1}/{p2}/{p3}",
		s.handleReleaseThreeSegDispatch("DELETE"),
		"DELETE /api/v3/repos/{owner}/{repo}/releases/{release_id}/reactions/{reaction_id}")
}

// handleReleaseTwoSegDispatch resolves
//
//	GET /releases/tags/{tag}
//	GET/PATCH/DELETE /releases/assets/{asset_id}
//	GET/POST /releases/{release_id}/assets
//	GET/POST /releases/{release_id}/reactions
func (s *Server) handleReleaseTwoSegDispatch(method string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		p1 := r.PathValue("p1")
		p2 := r.PathValue("p2")
		switch {
		case p1 == "tags" && method == "GET":
			r.SetPathValue("tag", p2)
			s.handleGetReleaseByTag(w, r)
		case p1 == "assets":
			r.SetPathValue("asset_id", p2)
			switch method {
			case "GET":
				s.handleGetReleaseAsset(w, r)
			case "PATCH":
				s.requirePerm(store.ScopeContents, store.PermWrite, s.handleUpdateReleaseAsset)(w, r)
			case "DELETE":
				s.requirePerm(store.ScopeContents, store.PermWrite, s.handleDeleteReleaseAsset)(w, r)
			default:
				writeGHError(w, http.StatusNotFound, "Not Found")
			}
		case p2 == "assets":
			r.SetPathValue("release_id", p1)
			switch method {
			case "GET":
				s.handleListReleaseAssets(w, r)
			case "POST":
				s.requirePerm(store.ScopeContents, store.PermWrite, s.handleUploadReleaseAsset)(w, r)
			default:
				writeGHError(w, http.StatusNotFound, "Not Found")
			}
		case p2 == "reactions":
			r.SetPathValue("release_id", p1)
			switch method {
			case "GET":
				s.handleListReactions("release", "release_id")(w, r)
			case "POST":
				s.requirePerm(store.ScopeReactions, store.PermWrite, s.handleCreateReaction("release", "release_id"))(w, r)
			default:
				writeGHError(w, http.StatusNotFound, "Not Found")
			}
		default:
			writeGHError(w, http.StatusNotFound, "Not Found")
		}
	}
}

// handleReleaseThreeSegDispatch resolves
//
//	DELETE /releases/{release_id}/reactions/{reaction_id}
func (s *Server) handleReleaseThreeSegDispatch(method string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		p1 := r.PathValue("p1")
		p2 := r.PathValue("p2")
		p3 := r.PathValue("p3")
		if p2 == "reactions" && method == "DELETE" {
			r.SetPathValue("release_id", p1)
			r.SetPathValue("reaction_id", p3)
			s.requirePerm(store.ScopeReactions, store.PermWrite, s.handleDeleteReaction("release", "release_id"))(w, r)
			return
		}
		writeGHError(w, http.StatusNotFound, "Not Found")
	}
}

func (s *Server) lookupRepoFromPath(r *http.Request) *store.Repo {
	return s.store.GetRepo(r.PathValue("owner"), r.PathValue("repo"))
}

// lookupReadableRepoFromPath resolves {owner}/{repo} and enforces private-repo
// visibility: a private repo the caller cannot read returns nil and a 404 (never
// 403, so existence stays hidden). Use on repo-scoped GET handlers;
// lookupRepoFromPath stays for write handlers gated by requirePerm.
func (s *Server) lookupReadableRepoFromPath(w http.ResponseWriter, r *http.Request) *store.Repo {
	repo := s.store.GetRepo(r.PathValue("owner"), r.PathValue("repo"))
	if repo == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return nil
	}
	if repo.Private && !s.viewerCanReadRepo(r.Context(), repo) {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return nil
	}
	return repo
}

// enforceRepoReadable applies the private-repo visibility gate without requiring
// a Repo record to exist: returns false (404) only for a KNOWN private repo the
// caller cannot read. Paths whose repo has no Store record (workflow-run state
// keyed by RepoFullName alone) pass through to handlers with their own not-found
// semantics. Use on repo-scoped read handlers over non-Repo-keyed state.
func (s *Server) enforceRepoReadable(w http.ResponseWriter, r *http.Request) bool {
	if !repoNamedInRequest(r) {
		return true
	}
	repo := s.repoFromPATRequest(r)
	if repo == nil {
		// Named but absent. Answering 200 for a missing repo while answering 404
		// for one the caller may not see is an existence oracle run backwards.
		writeGHError(w, http.StatusNotFound, "Not Found")
		return false
	}
	if repo.Private && !s.viewerCanReadRepo(r.Context(), repo) {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return false
	}
	return true
}

func (s *Server) handleCreateRelease(w http.ResponseWriter, r *http.Request) {
	user := ghUserFromContext(r.Context())
	if user == nil {
		writeGHError(w, http.StatusUnauthorized, "Bad credentials")
		return
	}
	repo := s.lookupRepoFromPath(r)
	if repo == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	var req struct {
		TagName                string      `json:"tag_name"`
		TargetCommitish        string      `json:"target_commitish"`
		Name                   string      `json:"name"`
		Body                   string      `json:"body"`
		Draft                  flexBool    `json:"draft"`
		Prerelease             flexBool    `json:"prerelease"`
		MakeLatest             interface{} `json:"make_latest"`
		GenerateReleaseNotes   flexBool    `json:"generate_release_notes"`
		DiscussionCategoryName string      `json:"discussion_category_name"`
	}
	if !decodeJSONBody(w, r, &req) {
		return
	}
	if req.TagName == "" {
		store.WriteGHValidationError(w, "Release", "tag_name", "missing_field")
		return
	}
	if s.store.Releases.GetByTag(repo.ID, req.TagName) != nil {
		store.WriteGHValidationError(w, "Release", "tag_name", "already_exists")
		return
	}
	target := req.TargetCommitish
	if target == "" {
		target = repo.DefaultBranch
	}
	if err := s.ensureReleaseTag(repo, req.TagName, target); err != nil {
		writeGHError(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	// discussion_category_name must name an existing category; validate first.
	var discussionCategory *store.DiscussionCategory
	if req.DiscussionCategoryName != "" {
		if discussionCategory = s.store.GetDiscussionCategoryByName(repo.ID, req.DiscussionCategoryName); discussionCategory == nil {
			store.WriteGHValidationError(w, "Release", "discussion_category_name", "invalid")
			return
		}
	}
	// generate_release_notes autogenerates the name when absent and appends notes
	// to any supplied body; the previous release's tag bounds the changelog range.
	if bool(req.GenerateReleaseNotes) {
		if req.Name == "" {
			req.Name = req.TagName
		}
		notes := s.generatedReleaseNotes(repo, req.TagName, target, s.previousReleaseTag(repo, req.TagName))
		if req.Body == "" {
			req.Body = notes
		} else {
			req.Body = req.Body + "\n\n" + notes
		}
	}
	release := s.store.Releases.Create(repo.ID, user.ID, req.TagName, target, req.Name, req.Body, bool(req.Draft), bool(req.Prerelease), releaseExcludedFromLatest(req.MakeLatest))
	if discussionCategory != nil {
		s.linkReleaseDiscussion(repo, release, user, discussionCategory)
	}
	s.emitReleaseEvents(repo, release, user, releaseCreateActions(release.Draft, release.Prerelease), s.baseURL(r))
	s.recordAuditEvent("release.create", user.Login, "", map[string]interface{}{"repo": repo.FullName, "release_id": release.ID, "tag": release.TagName})
	releaseJSON := releaseToJSON(release, s.store, s.baseURL(r), repo)
	writeJSONCreated(w, jsonStringField(releaseJSON, "url"), releaseJSON)
}

func (s *Server) handleListReleases(w http.ResponseWriter, r *http.Request) {
	repo := s.lookupReadableRepoFromPath(w, r)
	if repo == nil {
		return
	}
	releases := s.store.Releases.List(repo.ID)
	visible := make([]*store.Release, 0, len(releases))
	mayPush := s.viewerCanPushRepo(r.Context(), repo)
	for _, release := range releases {
		if release.Draft && !mayPush {
			continue
		}
		visible = append(visible, release)
	}
	page := paginateAndLink(w, r, visible)
	out := make([]map[string]interface{}, 0, len(page))
	for _, rel := range page {
		out = append(out, releaseToJSON(rel, s.store, s.baseURL(r), repo))
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleGetLatestRelease(w http.ResponseWriter, r *http.Request) {
	repo := s.lookupReadableRepoFromPath(w, r)
	if repo == nil {
		return
	}
	rel := s.store.Releases.Latest(repo.ID)
	if rel == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	writeJSON(w, http.StatusOK, releaseToJSON(rel, s.store, s.baseURL(r), repo))
}

// releaseVisibleTo reports whether a release may be shown to the caller. A draft
// is served only to users with push access (404 otherwise); applying the list
// endpoint's draft filter to every by-id/by-tag read stops a direct fetch
// bypassing it.
func (s *Server) releaseVisibleTo(r *http.Request, repo *store.Repo, rel *store.Release) bool {
	if rel == nil || repo == nil || rel.RepoID != repo.ID {
		return false
	}
	return !rel.Draft || s.viewerCanPushRepo(r.Context(), repo)
}

func (s *Server) handleGetReleaseByTag(w http.ResponseWriter, r *http.Request) {
	repo := s.lookupReadableRepoFromPath(w, r)
	if repo == nil {
		return
	}
	rel := s.store.Releases.GetByTag(repo.ID, r.PathValue("tag"))
	if !s.releaseVisibleTo(r, repo, rel) {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	writeJSON(w, http.StatusOK, releaseToJSON(rel, s.store, s.baseURL(r), repo))
}

func (s *Server) handleGetRelease(w http.ResponseWriter, r *http.Request) {
	repo := s.lookupReadableRepoFromPath(w, r)
	if repo == nil {
		return
	}
	id, err := strconv.Atoi(r.PathValue("release_id"))
	if err != nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	rel := s.store.Releases.Get(id)
	if !s.releaseVisibleTo(r, repo, rel) {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	writeJSON(w, http.StatusOK, releaseToJSON(rel, s.store, s.baseURL(r), repo))
}

func (s *Server) handleUpdateRelease(w http.ResponseWriter, r *http.Request) {
	repo := s.lookupRepoFromPath(r)
	if repo == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	id, err := strconv.Atoi(r.PathValue("release_id"))
	if err != nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	release := s.store.Releases.Get(id)
	if release == nil || release.RepoID != repo.ID {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	var req struct {
		TagName                *string     `json:"tag_name"`
		TargetCommitish        *string     `json:"target_commitish"`
		Name                   *string     `json:"name"`
		Body                   *string     `json:"body"`
		Draft                  *flexBool   `json:"draft"`
		Prerelease             *flexBool   `json:"prerelease"`
		MakeLatest             interface{} `json:"make_latest"`
		DiscussionCategoryName *string     `json:"discussion_category_name"`
	}
	if !decodeJSONBody(w, r, &req) {
		return
	}
	// A discussion_category_name naming a missing category is rejected before
	// mutating.
	var discussionCategory *store.DiscussionCategory
	if req.DiscussionCategoryName != nil && *req.DiscussionCategoryName != "" && release.DiscussionNumber == 0 {
		if discussionCategory = s.store.GetDiscussionCategoryByName(repo.ID, *req.DiscussionCategoryName); discussionCategory == nil {
			store.WriteGHValidationError(w, "Release", "discussion_category_name", "invalid")
			return
		}
	}
	wasDraft := release.Draft
	wasPrerelease := release.Prerelease
	if req.TagName != nil && *req.TagName != release.TagName {
		if *req.TagName == "" {
			store.WriteGHValidationError(w, "Release", "tag_name", "missing_field")
			return
		}
		if s.store.Releases.GetByTag(repo.ID, *req.TagName) != nil {
			store.WriteGHValidationError(w, "Release", "tag_name", "already_exists")
			return
		}
		target := release.TargetCommitish
		if req.TargetCommitish != nil {
			target = *req.TargetCommitish
		}
		if err := s.ensureReleaseTag(repo, *req.TagName, target); err != nil {
			writeGHError(w, http.StatusUnprocessableEntity, err.Error())
			return
		}
	} else if req.TargetCommitish != nil {
		if _, err := s.resolveReleaseTarget(repo, *req.TargetCommitish); err != nil {
			writeGHError(w, http.StatusUnprocessableEntity, err.Error())
			return
		}
	}
	ok := s.store.Releases.Update(id, func(rel *store.Release) {
		if req.TagName != nil {
			rel.TagName = *req.TagName
		}
		if req.TargetCommitish != nil {
			rel.TargetCommitish = *req.TargetCommitish
		}
		if req.Name != nil {
			rel.Name = *req.Name
		}
		if req.Body != nil {
			rel.Body = *req.Body
		}
		if req.Draft != nil {
			rel.Draft = bool(*req.Draft)
		}
		if req.Prerelease != nil {
			rel.Prerelease = bool(*req.Prerelease)
		}
		if req.MakeLatest != nil {
			rel.ExcludeFromLatest = releaseExcludedFromLatest(req.MakeLatest)
		}
	})
	if !ok {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	updated := s.store.Releases.Get(id)
	if discussionCategory != nil {
		s.linkReleaseDiscussion(repo, updated, ghUserFromContext(r.Context()), discussionCategory)
	}
	actions := releaseUpdateActions(wasDraft, wasPrerelease, updated.Draft, updated.Prerelease)
	s.emitReleaseEvents(repo, updated, ghUserFromContext(r.Context()), actions, s.baseURL(r))
	writeJSON(w, http.StatusOK, releaseToJSON(updated, s.store, s.baseURL(r), repo))
}

func (s *Server) resolveReleaseTarget(repo *store.Repo, target string) (plumbing.Hash, error) {
	stor, _ := s.store.GitStorageForRepoID(repo.ID)
	if stor == nil {
		return plumbing.ZeroHash, fmt.Errorf("release source git storage is unavailable")
	}
	hash, err := store.ResolveGitRef(stor, target)
	if err != nil {
		return plumbing.ZeroHash, fmt.Errorf("target_commitish %q does not resolve to a commit", target)
	}
	return hash, nil
}

func (s *Server) ensureReleaseTag(repo *store.Repo, tagName, target string) error {
	stor, _ := s.store.GitStorageForRepoID(repo.ID)
	if stor == nil {
		return fmt.Errorf("release source git storage is unavailable")
	}
	tagRef := plumbing.NewTagReferenceName(tagName)
	if existing, err := stor.Reference(tagRef); err == nil {
		_, err = store.RefHash(existing, stor)
		if err != nil {
			return fmt.Errorf("release tag %q does not resolve to a commit", tagName)
		}
		return nil
	}
	targetHash, err := s.resolveReleaseTarget(repo, target)
	if err != nil {
		return err
	}
	if err := stor.SetReference(plumbing.NewHashReference(tagRef, targetHash)); err != nil {
		return fmt.Errorf("create release tag %q: %w", tagName, err)
	}
	return nil
}

func (s *Server) handleDeleteRelease(w http.ResponseWriter, r *http.Request) {
	user := ghUserFromContext(r.Context())
	repo := s.lookupRepoFromPath(r)
	if repo == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	id, err := strconv.Atoi(r.PathValue("release_id"))
	if err != nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	rel := s.store.Releases.Get(id)
	if rel == nil || rel.RepoID != repo.ID {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	payload := s.buildReleaseEventPayload(repo, rel, user, "deleted", s.baseURL(r))
	deleted, err := s.store.Releases.Delete(id, s.store.Reactions)
	if err != nil {
		writeGHError(w, http.StatusInternalServerError, "Failed to delete release")
		return
	}
	if !deleted {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	s.emitWebhookEvent(repo.FullName, "release", "deleted", payload)
	s.recordAuditEvent("release.destroy", user.Login, "", map[string]interface{}{"repo": repo.FullName, "release_id": id})
	w.WriteHeader(http.StatusNoContent)
}

// releaseCreateActions returns the actions a create delivers. GitHub fans a
// publish into several: a draft is only `created`; a release saved straight to
// published is `created`, `published`, then `prereleased` or `released`.
func releaseCreateActions(draft, prerelease bool) []string {
	if draft {
		return []string{"created"}
	}
	return append([]string{"created"}, releasePublishActions(prerelease)...)
}

// releaseUpdateActions returns the actions an edit delivers from the
// draft/prerelease flags before and after. Publishing a draft does not repeat
// `created`.
func releaseUpdateActions(wasDraft, wasPrerelease, draft, prerelease bool) []string {
	switch {
	case !wasDraft && draft:
		return []string{"unpublished"}
	case wasDraft && !draft:
		return releasePublishActions(prerelease)
	case !draft && !wasPrerelease && prerelease:
		return []string{"prereleased"}
	case !draft && wasPrerelease && !prerelease:
		return []string{"released"}
	default:
		return []string{"edited"}
	}
}

// releasePublishActions is the pair delivered when a release becomes published:
// `published`, then the qualifier for its kind.
func releasePublishActions(prerelease bool) []string {
	if prerelease {
		return []string{"published", "prereleased"}
	}
	return []string{"published", "released"}
}

func (s *Server) emitReleaseEvent(repo *store.Repo, release *store.Release, sender *store.User, action, baseURL string) {
	payload := s.buildReleaseEventPayload(repo, release, sender, action, baseURL)
	s.emitWebhookEvent(repo.FullName, "release", action, payload)
}

// emitReleaseEvents delivers a release fan-out in order; each action gets its
// own payload.
func (s *Server) emitReleaseEvents(repo *store.Repo, release *store.Release, sender *store.User, actions []string, baseURL string) {
	for _, action := range actions {
		s.emitReleaseEvent(repo, release, sender, action, baseURL)
	}
}

func readUploadAssetBody(r *http.Request) (name, label, contentType string, data []byte, ok bool, err error) {
	q := r.URL.Query()
	name = q.Get("name")
	label = q.Get("label")
	contentType = r.Header.Get("Content-Type")
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	if name == "" {
		return "", "", "", nil, false, nil
	}
	data, err = io.ReadAll(http.MaxBytesReader(nil, r.Body, maxUploadBytes))
	if err != nil {
		return "", "", "", nil, false, err
	}
	return name, label, contentType, data, true, nil
}

func (s *Server) handleUploadReleaseAsset(w http.ResponseWriter, r *http.Request) {
	user := ghUserFromContext(r.Context())
	repo := s.lookupRepoFromPath(r)
	if repo == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	releaseID, err := strconv.Atoi(r.PathValue("release_id"))
	if err != nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	rel := s.store.Releases.Get(releaseID)
	if rel == nil || rel.RepoID != repo.ID {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	name, label, contentType, data, ok, err := readUploadAssetBody(r)
	if err != nil {
		writeGHError(w, http.StatusBadRequest, "Bad Request")
		return
	}
	if !ok {
		store.WriteGHValidationError(w, "ReleaseAsset", "name", "missing_field")
		return
	}
	asset, err := s.store.Releases.CreateReleaseAsset(releaseID, user.ID, name, label, contentType, data)
	if errors.Is(err, store.ErrReleaseAssetNameExists) {
		store.WriteGHValidationError(w, "ReleaseAsset", "name", "already_exists")
		return
	}
	if err != nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	assetJSON := releaseAssetToJSON(asset, s.store, s.baseURL(r), repo, rel)
	writeJSONCreated(w, jsonStringField(assetJSON, "url"), assetJSON)
}

func (s *Server) handleListReleaseAssets(w http.ResponseWriter, r *http.Request) {
	repo := s.lookupReadableRepoFromPath(w, r)
	if repo == nil {
		return
	}
	releaseID, err := strconv.Atoi(r.PathValue("release_id"))
	if err != nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	rel := s.store.Releases.Get(releaseID)
	if !s.releaseVisibleTo(r, repo, rel) {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	assets := s.store.Releases.ListReleaseAssets(releaseID)
	out := make([]map[string]interface{}, 0, len(assets))
	for _, a := range assets {
		out = append(out, releaseAssetToJSON(a, s.store, s.baseURL(r), repo, rel))
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleGetReleaseAsset(w http.ResponseWriter, r *http.Request) {
	repo := s.lookupReadableRepoFromPath(w, r)
	if repo == nil {
		return
	}
	assetID, err := strconv.Atoi(r.PathValue("asset_id"))
	if err != nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	asset := s.store.Releases.GetReleaseAsset(assetID)
	if asset == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	rel := s.store.Releases.Get(asset.ReleaseID)
	if !s.releaseVisibleTo(r, repo, rel) {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	if strings.Contains(r.Header.Get("Accept"), "application/octet-stream") {
		data, ok := s.store.Releases.GetAssetData(assetID)
		if !ok {
			writeGHError(w, http.StatusNotFound, "Not Found")
			return
		}
		// Conditional download (REST-031): asset bytes are immutable for a given
		// id, so (id, size) is a stable validator; a matching If-None-Match
		// returns a bodyless 304 and is not counted as a download.
		etag := fmt.Sprintf(`"asset-%d-%d"`, asset.ID, asset.Size)
		w.Header().Set("ETag", etag)
		if etagMatches(r.Header.Get("If-None-Match"), etag) {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		s.store.Releases.IncrementAssetDownloads(assetID)
		w.Header().Set("Content-Type", asset.ContentType)
		w.Header().Set("Content-Length", strconv.Itoa(len(data)))
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(data)
		return
	}
	writeJSON(w, http.StatusOK, releaseAssetToJSON(asset, s.store, s.baseURL(r), repo, rel))
}

func (s *Server) handleUpdateReleaseAsset(w http.ResponseWriter, r *http.Request) {
	repo := s.lookupRepoFromPath(r)
	if repo == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	assetID, err := strconv.Atoi(r.PathValue("asset_id"))
	if err != nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	asset := s.store.Releases.GetReleaseAsset(assetID)
	if asset == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	rel := s.store.Releases.Get(asset.ReleaseID)
	if rel == nil || rel.RepoID != repo.ID {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	var req struct {
		Name  *string `json:"name"`
		Label *string `json:"label"`
	}
	if !decodeJSONBody(w, r, &req) {
		return
	}
	name := asset.Name
	label := asset.Label
	if req.Name != nil {
		name = *req.Name
	}
	if req.Label != nil {
		label = *req.Label
	}
	updated := s.store.Releases.UpdateReleaseAsset(assetID, name, label)
	if !mutated(w, updated) {
		return
	}
	writeJSON(w, http.StatusOK, releaseAssetToJSON(updated, s.store, s.baseURL(r), repo, rel))
}

func (s *Server) handleDeleteReleaseAsset(w http.ResponseWriter, r *http.Request) {
	repo := s.lookupRepoFromPath(r)
	if repo == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	assetID, err := strconv.Atoi(r.PathValue("asset_id"))
	if err != nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	asset := s.store.Releases.GetReleaseAsset(assetID)
	if asset == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	rel := s.store.Releases.Get(asset.ReleaseID)
	if rel == nil || rel.RepoID != repo.ID {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	deleted, err := s.store.Releases.DeleteReleaseAsset(assetID)
	if err != nil {
		writeGHError(w, http.StatusInternalServerError, "Failed to delete release asset")
		return
	}
	if !deleted {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func releaseAssetToJSON(asset *store.ReleaseAsset, st *store.Store, baseURL string, repo *store.Repo, rel *store.Release) map[string]interface{} {
	var uploader map[string]interface{}
	if user := st.Users[asset.UploaderID]; user != nil {
		uploader = store.UserToJSON(user, baseURL)
	}
	downloadURL := fmt.Sprintf("%s/api/v3/repos/%s/releases/assets/%d", baseURL, repo.FullName, asset.ID)
	return map[string]interface{}{
		"id":                   asset.ID,
		"node_id":              asset.NodeID,
		"name":                 asset.Name,
		"label":                asset.Label,
		"content_type":         asset.ContentType,
		"state":                asset.State,
		"digest":               nullOrString(asset.Digest),
		"size":                 asset.Size,
		"download_count":       asset.DownloadCount,
		"created_at":           asset.CreatedAt.UTC().Format(time.RFC3339),
		"updated_at":           asset.UpdatedAt.UTC().Format(time.RFC3339),
		"url":                  downloadURL,
		"browser_download_url": downloadURL,
		"uploader":             uploader,
	}
}

func (s *Server) handleGenerateReleaseNotes(w http.ResponseWriter, r *http.Request) {
	repo := s.lookupRepoFromPath(r)
	if repo == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	var req struct {
		TagName           string `json:"tag_name"`
		TargetCommitish   string `json:"target_commitish"`
		PreviousTagName   string `json:"previous_tag_name"`
		ConfigurationFile string `json:"configuration_file_path"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.TagName == "" {
		store.WriteGHValidationError(w, "Release", "tag_name", "missing_field")
		return
	}
	notes := s.generatedReleaseNotes(repo, req.TagName, req.TargetCommitish, req.PreviousTagName)
	out := map[string]interface{}{
		"name": req.TagName,
		"body": notes,
	}
	writeJSON(w, http.StatusOK, out)
}

// previousReleaseTag returns the tag of the newest published release excluding
// excludeTag, bounding generated release notes.
func (s *Server) previousReleaseTag(repo *store.Repo, excludeTag string) string {
	if prev := s.store.Releases.Latest(repo.ID); prev != nil && prev.TagName != excludeTag {
		return prev.TagName
	}
	return ""
}

// linkReleaseDiscussion creates a discussion in the category and links it to the
// release.
func (s *Server) linkReleaseDiscussion(repo *store.Repo, release *store.Release, user *store.User, category *store.DiscussionCategory) {
	if category == nil {
		return
	}
	title := release.Name
	if title == "" {
		title = release.TagName
	}
	disc := s.store.CreateDiscussion(repo.ID, category.ID, user.ID, title, release.Body)
	if disc == nil {
		return
	}
	s.store.Releases.Update(release.ID, func(rel *store.Release) { rel.DiscussionNumber = disc.Number })
	release.DiscussionNumber = disc.Number
}

func (s *Server) generatedReleaseNotes(repo *store.Repo, tagName, targetCommitish, previousTagName string) string {
	var lines []string
	lines = append(lines, "## What's Changed", "")
	for _, pr := range s.mergedPullRequestsInRange(repo, tagName, targetCommitish, previousTagName) {
		author := ""
		if u := s.store.GetActorByID(pr.AuthorID); u != nil {
			author = u.Login
		}
		if author == "" {
			author = "ghost"
		}
		lines = append(lines, fmt.Sprintf("* %s by @%s in %s/pull/%d", pr.Title, author, repo.FullName, pr.Number))
	}
	if len(lines) == 2 {
		lines = append(lines, fmt.Sprintf("Since %s", store.CoalesceStr(previousTagName, "(no previous tag)")))
	}
	lines = append(lines, "", fmt.Sprintf("**Full Changelog**: %s...%s", store.CoalesceStr(previousTagName, ""), tagName))
	return strings.Join(lines, "\n")
}

func (s *Server) mergedPullRequestsInRange(repo *store.Repo, tagName, targetCommitish, previousTagName string) []*store.PullRequest {
	owner, name, ok := store.SplitRepoFullName(repo.FullName)
	if !ok {
		return nil
	}
	stor := s.store.GetGitStorage(owner, name)
	if stor == nil {
		return nil
	}
	targetRef := tagName
	if targetCommitish != "" {
		targetRef = targetCommitish
	}
	targetHash, err := store.ResolveGitRef(stor, targetRef)
	if err != nil && targetCommitish == "" {
		targetHash, err = store.ResolveGitRef(stor, repo.DefaultBranch)
	}
	if err != nil {
		return nil
	}
	var previousHash plumbing.Hash
	if previousTagName != "" {
		if h, err := store.ResolveGitRef(stor, previousTagName); err == nil {
			previousHash = h
		}
	}
	inRange := releaseCommitRangeSet(stor, previousHash, targetHash)
	if len(inRange) == 0 {
		return nil
	}

	var out []*store.PullRequest
	for _, pr := range s.store.ListPullRequests(repo.ID, "MERGED") {
		snap := s.store.SnapPR(pr)
		if snap.MergeCommitSHA != "" && inRange[snap.MergeCommitSHA] {
			out = append(out, snap)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].MergedAt != nil && out[j].MergedAt != nil && !out[i].MergedAt.Equal(*out[j].MergedAt) {
			return out[i].MergedAt.Before(*out[j].MergedAt)
		}
		return out[i].Number < out[j].Number
	})
	return out
}

func releaseCommitRangeSet(stor gitStorage.Storer, previousHash, targetHash plumbing.Hash) map[string]bool {
	out := map[string]bool{}
	var commits []*object.Commit
	var err error
	if previousHash.IsZero() {
		head, err := object.GetCommit(stor, targetHash)
		if err != nil {
			return out
		}
		iter := object.NewCommitPreorderIter(head, nil, nil)
		defer iter.Close()
		_ = iter.ForEach(func(c *object.Commit) error {
			commits = append(commits, c)
			return nil
		})
	} else {
		commits, err = store.CommitsBetween(stor, previousHash, targetHash)
		if err != nil {
			return out
		}
	}
	for _, c := range commits {
		out[c.Hash.String()] = true
	}
	return out
}

func releaseToJSON(rel *store.Release, st *store.Store, baseURL string, repo *store.Repo) map[string]interface{} {
	if rel == nil {
		return nil
	}
	var author map[string]interface{}
	st.Mu.RLock()
	if u := st.Users[rel.AuthorID]; u != nil {
		author = store.UserToJSON(u, baseURL)
	}
	st.Mu.RUnlock()
	publishedAt := interface{}(nil)
	if rel.PublishedAt != nil {
		publishedAt = rel.PublishedAt.UTC().Format(time.RFC3339)
	}
	reactions := st.Reactions.SummarizeReactions("release", rel.ID)
	reactions["url"] = fmt.Sprintf("%s/api/v3/repos/%s/releases/%d/reactions", baseURL, repo.FullName, rel.ID)
	var discussionURL interface{}
	if rel.DiscussionNumber > 0 {
		discussionURL = fmt.Sprintf("%s/%s/discussions/%d", baseURL, repo.FullName, rel.DiscussionNumber)
	}
	assets := make([]interface{}, 0, len(rel.Assets))
	for _, a := range rel.Assets {
		assets = append(assets, releaseAssetToJSON(a, st, baseURL, repo, rel))
	}
	m := map[string]interface{}{
		"id":               rel.ID,
		"node_id":          rel.NodeID,
		"tag_name":         rel.TagName,
		"target_commitish": rel.TargetCommitish,
		"name":             rel.Name,
		"body":             rel.Body,
		"draft":            rel.Draft,
		"prerelease":       rel.Prerelease,
		"author":           author,
		"created_at":       rel.CreatedAt.UTC().Format(time.RFC3339),
		"published_at":     publishedAt,
		"url":              fmt.Sprintf("%s/api/v3/repos/%s/releases/%d", baseURL, repo.FullName, rel.ID),
		"html_url":         fmt.Sprintf("%s/%s/releases/tag/%s", baseURL, repo.FullName, rel.TagName),
		"assets_url":       fmt.Sprintf("%s/api/v3/repos/%s/releases/%d/assets", baseURL, repo.FullName, rel.ID),
		"upload_url":       fmt.Sprintf("%s/api/uploads/repos/%s/releases/%d/assets{?name,label}", baseURL, repo.FullName, rel.ID),
		"tarball_url":      fmt.Sprintf("%s/api/v3/repos/%s/tarball/%s", baseURL, repo.FullName, rel.TagName),
		"zipball_url":      fmt.Sprintf("%s/api/v3/repos/%s/zipball/%s", baseURL, repo.FullName, rel.TagName),
		"assets":           assets,
		"reactions":        reactions,
	}
	// discussion_url is present only when the release is linked to a discussion;
	// GitHub omits it otherwise.
	if discussionURL != nil {
		m["discussion_url"] = discussionURL
	}
	return m
}

func (s *Server) buildReleaseEventPayload(repo *store.Repo, rel *store.Release, sender *store.User, action, baseURL string) map[string]interface{} {
	return attachInstallationBlock(map[string]interface{}{
		"action":     action,
		"release":    releaseToJSON(rel, s.store, baseURL, repo),
		"repository": repoPayload(repo, baseURL),
		"sender":     senderPayload(sender, baseURL),
	}, nil)
}
