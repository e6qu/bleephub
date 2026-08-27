package bleephub

import (
	"context"
	"net/http"
	"strings"

	"github.com/e6qu/bleephub/internal/store"
	"github.com/go-git/go-git/v5/plumbing/protocol/packp"
	"github.com/go-git/go-git/v5/plumbing/storer"
)

// A repository's wiki is served over git at `<owner>/<repo>.wiki.git` through the
// same upload-pack/receive-pack the repository uses. This file is only the resolution
// and authorization: which storage a `.wiki` path names, who may read or write it,
// and what a landed wiki push owes the rest of the server.

// gitTarget is what a git request addresses — not always a repository:
// `admin/docs.wiki.git` addresses the wiki of `admin/docs`, whose repository row
// governs access while the objects live in their own storage.
type gitTarget struct {
	// repo is the repository row every access decision is made against.
	repo *store.Repo
	// stor is the git storage the protocol reads and writes; nil until opened.
	stor storer.Storer
	// wiki records that the path addressed a wiki.
	wiki bool
	// storageName keys the pack-reuse cache and the compaction scheduler.
	storageName string
	// defaultBranch names the HEAD symref the ref advertisement carries.
	defaultBranch string
}

// exists reports whether the path named something. Storage is not opened until the
// caller is authorized, so probing a repository never creates its wiki.
func (t *gitTarget) exists() bool {
	if t == nil || t.repo == nil {
		return false
	}
	if t.wiki {
		return t.repo.HasWiki
	}
	return t.stor != nil
}

// openGitTarget attaches a resolved target's storage, called only after authorization
// so an unauthorized caller cannot make the server materialize a wiki by asking for one.
func (s *Server) openGitTarget(target *gitTarget) bool {
	if target.stor != nil {
		return true
	}
	if !target.wiki {
		return false
	}
	target.stor = s.store.WikiGitStorage(target.repo.FullName)
	if target.stor == nil {
		return false
	}
	target.defaultBranch = s.store.WikiHeadBranch(target.repo.FullName)
	return true
}

// resolveGitTarget maps the repository name in a git URL to what it addresses.
//
// The `.wiki` suffix wins whenever the repository it names exists (github's URL
// grammar). A repository named `docs.wiki` stays reachable but loses to the wiki of
// `docs`; the two never share storage, as a wiki's key carries a `.git` suffix no
// repository name may have.
func (s *Server) resolveGitTarget(owner, name string) *gitTarget {
	if base := strings.TrimSuffix(name, store.WikiURLSuffix); base != name && base != "" {
		if repo := s.store.GetRepo(owner, base); repo != nil {
			// Wikis disabled means no wiki remote: every transport answers as for a missing repo.
			return &gitTarget{
				repo:          repo,
				wiki:          true,
				storageName:   store.WikiStorageName(repo.FullName),
				defaultBranch: repo.DefaultBranch,
			}
		}
	}
	target := &gitTarget{repo: s.store.GetRepo(owner, name), stor: s.store.GetGitStorage(owner, name)}
	if target.repo != nil {
		target.storageName = target.repo.FullName
		target.defaultBranch = target.repo.DefaultBranch
	}
	return target
}

// viewerMayWriteGitTarget is the write decision for both transports: repository push
// access for a repository, the wiki-write policy for a wiki.
func (s *Server) viewerMayWriteGitTarget(ctx context.Context, target *gitTarget) bool {
	if target.wiki {
		return s.viewerMayEditWiki(ctx, target.repo)
	}
	return s.viewerHasRepoPermission(ctx, target.repo, store.ScopeContents, store.PermWrite)
}

// viewerMayEditWiki is the repository's wiki-write policy, decided in this one place
// so a push and a UI edit are admitted on the same terms.
//
// Default (github's): editing needs repository push access. Unchecking "restrict
// editing to collaborators only" lets any signed-in reader edit — but the credential
// still must grant contents:write, so a read-only or unscoped token cannot.
func (s *Server) viewerMayEditWiki(ctx context.Context, repo *store.Repo) bool {
	if repo == nil || !repo.HasWiki {
		return false
	}
	if s.viewerHasRepoPermission(ctx, repo, store.ScopeContents, store.PermWrite) {
		return true
	}
	if !repo.WikiEditsUnrestricted {
		return false
	}
	user := ghUserFromContext(ctx)
	if user == nil || user.Suspended {
		return false
	}
	return s.viewerMayActOnRepo(ctx, repo, store.ScopeContents, store.PermWrite, store.PermRead)
}

// afterWikiReceivePack is the bookkeeping a landed wiki push owes: repair HEAD and
// emit the gollum event. A wiki push is not a repository push — it raises no push
// event, moves no PR, does not touch pushed_at, and is not swept for secrets.
func (s *Server) afterWikiReceivePack(repo *store.Repo, user *store.User, applied []*packp.Command, baseURL string) {
	s.store.RepairWikiHead(repo.FullName)
	seen := map[string]bool{}
	var changes []store.WikiPageChange
	for _, command := range applied {
		if !command.Name.IsBranch() {
			continue
		}
		for _, change := range s.store.WikiPagesChanged(repo.FullName, command.Old.String(), command.New.String()) {
			if seen[change.PageName] {
				continue
			}
			seen[change.PageName] = true
			changes = append(changes, change)
		}
	}
	s.emitGollumEvent(repo, user, changes, baseURL)
}

// emitGollumEvent delivers the gollum webhook for pages a wiki write created or edited.
// The payload has no vocabulary for a removed page, so a removal-only write delivers nothing.
func (s *Server) emitGollumEvent(repo *store.Repo, sender *store.User, changes []store.WikiPageChange, baseURL string) {
	if repo == nil || len(changes) == 0 {
		return
	}
	s.emitWebhookEvent(repo.FullName, "gollum", "", buildGollumPayload(repo, changes, sender, baseURL))
}

// buildGollumPayload renders the gollum event body: the pages that moved, plus the
// repository and sender.
func buildGollumPayload(repo *store.Repo, changes []store.WikiPageChange, sender *store.User, baseURL string) map[string]interface{} {
	pages := make([]map[string]interface{}, 0, len(changes))
	for _, change := range changes {
		pages = append(pages, map[string]interface{}{
			"page_name": change.PageName,
			"title":     change.Title,
			"summary":   nil,
			"action":    change.Action,
			"sha":       change.SHA,
			"html_url":  baseURL + "/" + repo.FullName + "/wiki/" + change.PageName,
		})
	}
	return map[string]interface{}{
		"pages":      pages,
		"repository": repoPayload(repo, baseURL),
		"sender":     senderPayload(sender, baseURL),
	}
}

// wikiPageChangeFor describes a single page a browser edit moved, so the UI lane emits
// the same gollum event a push does.
func wikiPageChangeFor(page *store.WikiPage, action, sha string) []store.WikiPageChange {
	if page == nil {
		return nil
	}
	return []store.WikiPageChange{{
		Slug:     page.Slug,
		Title:    page.Title,
		PageName: store.WikiPageName(page.Title),
		Action:   action,
		SHA:      sha,
	}}
}

// registerGHWikiSettingsRoutes serves the wiki-write policy. github exposes it in no
// REST field, so it lives on the browser-only /ui-data lane, not under /api/v3.
func (s *Server) registerGHWikiSettingsRoutes() {
	s.route("GET /ui-data/repos/{owner}/{repo}/wiki/settings", s.handleGetWikiSettings)
	s.route("PUT /ui-data/repos/{owner}/{repo}/wiki/settings", s.handlePutWikiSettings)
}

func (s *Server) handleGetWikiSettings(w http.ResponseWriter, r *http.Request) {
	repo := s.wikiRepoForRead(w, r)
	if repo == nil {
		return
	}
	writeJSON(w, http.StatusOK, wikiSettingsJSON(repo, s.baseURL(r)))
}

func (s *Server) handlePutWikiSettings(w http.ResponseWriter, r *http.Request) {
	repo := s.store.GetRepo(r.PathValue("owner"), r.PathValue("repo"))
	if repo == nil || !repo.HasWiki {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	if repo.Private && !s.viewerCanReadRepo(r.Context(), repo) {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	// Changing who may edit the wiki is a repository setting, so it takes repo admin.
	if !s.viewerCanAdminRepo(r.Context(), repo) {
		writeGHError(w, http.StatusForbidden, "Must have admin rights to Repository.")
		return
	}
	var req struct {
		RestrictEditsToCollaborators *bool `json:"restrict_edits_to_collaborators"`
	}
	if !decodeJSONBody(w, r, &req) {
		return
	}
	if req.RestrictEditsToCollaborators == nil {
		store.WriteGHValidationError(w, "WikiSettings", "restrict_edits_to_collaborators", "missing_field")
		return
	}
	unrestricted := !*req.RestrictEditsToCollaborators
	s.store.UpdateRepo(repo.Owner.Login, repo.Name, func(updated *store.Repo) {
		updated.WikiEditsUnrestricted = unrestricted
	})
	updated := s.store.GetRepo(repo.Owner.Login, repo.Name)
	if updated == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	writeJSON(w, http.StatusOK, wikiSettingsJSON(updated, s.baseURL(r)))
}

// wikiSettingsJSON reports the policy and the wiki clone URL.
func wikiSettingsJSON(repo *store.Repo, baseURL string) map[string]interface{} {
	return map[string]interface{}{
		"has_wiki":                        repo.HasWiki,
		"restrict_edits_to_collaborators": !repo.WikiEditsUnrestricted,
		"clone_url":                       baseURL + "/" + repo.FullName + store.WikiStorageSuffix,
	}
}
