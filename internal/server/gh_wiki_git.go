package bleephub

import (
	"context"
	"net/http"
	"strings"

	"github.com/e6qu/bleephub/internal/store"
	"github.com/go-git/go-git/v5/plumbing/protocol/packp"
	"github.com/go-git/go-git/v5/plumbing/storer"
)

// A repository's wiki is served over git at `<owner>/<repo>.wiki.git`, on both
// transports, through the same upload-pack and receive-pack implementations the
// repository itself is served through — so protocol v2, shallow and partial
// clones, thin packs and LFS work on a wiki because they are not implemented
// twice. This file is only the resolution and the authorization: which storage a
// `.wiki` path names, who may read it, who may write it, and what a landed wiki
// push owes the rest of the server.

// gitTarget is the thing a git request addresses. It is not always a repository:
// `admin/docs.wiki.git` addresses the wiki of `admin/docs`, whose repository row
// governs access while the objects live in their own storage.
type gitTarget struct {
	// repo is the repository row every access decision is made against — the
	// repository itself, or the repository whose wiki this is.
	repo *store.Repo
	// stor is the git storage the protocol reads and writes, nil when the
	// target does not exist or its wiki is disabled.
	stor storer.Storer
	// wiki records that the path addressed a wiki, which changes who may write
	// and what a landed push emits.
	wiki bool
	// storageName keys the pack-reuse cache and the compaction scheduler.
	storageName string
	// defaultBranch is the branch the ref advertisement names in its HEAD
	// symref, so a clone checks something out.
	defaultBranch string
}

// exists reports whether the path named something. A wiki exists when its
// repository does and wikis are enabled on it; its storage is not opened until
// the caller has been authorized, so probing a repository never creates one.
func (t *gitTarget) exists() bool {
	if t == nil || t.repo == nil {
		return false
	}
	if t.wiki {
		return t.repo.HasWiki
	}
	return t.stor != nil
}

// openGitTarget attaches the storage a resolved target reads and writes. It is
// called once the request has cleared authorization, so an unauthorized caller
// cannot make the server materialize a wiki repository by asking for one.
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
// The `.wiki` suffix wins whenever the repository it names exists, which is the
// grammar github's URLs use. A repository someone legitimately named `docs.wiki`
// is still reachable — it just loses to the wiki of `docs` when both could be
// meant, and the two never share storage because a wiki's storage key carries a
// `.git` suffix that no repository name may have.
func (s *Server) resolveGitTarget(owner, name string) *gitTarget {
	if base := strings.TrimSuffix(name, store.WikiURLSuffix); base != name && base != "" {
		if repo := s.store.GetRepo(owner, base); repo != nil {
			// A repository with wikis disabled has no wiki remote at all, so
			// every transport answers as it does for a repository that is not
			// there — see exists() above.
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

// viewerMayWriteGitTarget is the write decision for both transports. A
// repository is written by whoever may push to it; a wiki is written by whoever
// the repository's wiki-write policy admits.
func (s *Server) viewerMayWriteGitTarget(ctx context.Context, target *gitTarget) bool {
	if target.wiki {
		return s.viewerMayEditWiki(ctx, target.repo)
	}
	return s.viewerHasRepoPermission(ctx, target.repo, store.ScopeContents, store.PermWrite)
}

// viewerMayEditWiki is the repository's wiki-write policy, and the only place it
// is decided — the git transports and the browser lane both ask it, so a push
// and a UI edit can never be admitted on different terms.
//
// The default is github's: editing is restricted to collaborators, so writing
// the wiki needs push access to the repository. A repository that unchecks
// "restrict editing to collaborators only" lets any signed-in user who can read
// the repository edit it, which relaxes the bearer's own standing and nothing
// else: the credential still has to grant contents:write, so a read-only
// installation or a fine-grained token that was never given the repository
// cannot write a wiki its bearer could have written in a browser.
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

// afterWikiReceivePack is the bookkeeping a landed wiki push owes: a HEAD that
// names a branch the client actually pushed, and the gollum event github sends
// when wiki pages move.
//
// It is deliberately not afterGitReceivePack: a wiki push is not a push to the
// repository. It raises no push event, moves no pull request, does not touch
// pushed_at, and is not swept for secrets — github scans repository content, not
// wikis, and a wiki has no branch protection to enforce.
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

// emitGollumEvent delivers github's gollum webhook for the pages a wiki write
// created or edited. github's documented payload can say `created` or `edited`
// and has no vocabulary for a removed page, so a write that only removes pages
// delivers nothing.
func (s *Server) emitGollumEvent(repo *store.Repo, sender *store.User, changes []store.WikiPageChange, baseURL string) {
	if repo == nil || len(changes) == 0 {
		return
	}
	s.emitWebhookEvent(repo.FullName, "gollum", "", buildGollumPayload(repo, changes, sender, baseURL))
}

// buildGollumPayload renders the gollum event body: the pages that moved, then
// the repository and the sender every event carries.
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

// wikiPageChangeFor describes a single page a browser edit moved, so the UI lane
// emits the same gollum event a push does.
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

// registerGHWikiSettingsRoutes serves the wiki-write policy. github keeps this
// setting in the repository settings page and in no REST field, so it lives on
// the browser-only lane rather than under a /api/v3 path github does not define.
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
	// Who may edit the wiki is a repository setting, so changing it takes
	// repository administration rather than the wiki write it governs.
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

// wikiSettingsJSON reports the policy and the remote a client clones the wiki
// from, so the browser can show the URL rather than make the reader assemble it.
func wikiSettingsJSON(repo *store.Repo, baseURL string) map[string]interface{} {
	return map[string]interface{}{
		"has_wiki":                        repo.HasWiki,
		"restrict_edits_to_collaborators": !repo.WikiEditsUnrestricted,
		"clone_url":                       baseURL + "/" + repo.FullName + store.WikiStorageSuffix,
	}
}
