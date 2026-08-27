package bleephub

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"

	"github.com/e6qu/bleephub/internal/store"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/format/pktline"
	"github.com/go-git/go-git/v5/plumbing/protocol/packp"
	"github.com/go-git/go-git/v5/plumbing/protocol/packp/capability"
	"github.com/go-git/go-git/v5/plumbing/storer"
)

// uploadPackRequestCap bounds the pkt-line want/have negotiation body a
// (possibly anonymous) fetch may send, refusing an unbounded stream.
const uploadPackRequestCap = 50 << 20 // 50 MiB

// authenticateGitRequest resolves the credential on a git smart-HTTP request.
// It returns the context because git routes sit outside the /api middleware
// and the credential shape on that context decides what the caller may read.
func (s *Server) authenticateGitRequest(r *http.Request) (context.Context, *store.User) {
	ctx := s.authenticateRequest(r)
	return ctx, ghUserFromContext(ctx)
}

// tryHandleGitRequest handles a git smart-HTTP request (URLs like
// /{owner}/{repo}.git/info/refs or /git-upload-pack), returning true when it
// did.
func (s *Server) tryHandleGitRequest(w http.ResponseWriter, r *http.Request) bool {
	path := r.URL.Path

	if strings.HasSuffix(path, "/info/refs") && r.Method == "GET" {
		repoPath := strings.TrimSuffix(path, "/info/refs")
		owner, repo := splitRepoPath(repoPath)
		if owner != "" && repo != "" {
			if s.redirectMovedGitRepo(w, r, owner, repo) {
				return true
			}
			s.handleGitInfoRefs(w, r, owner, repo)
			return true
		}
	}

	if strings.HasSuffix(path, "/git-upload-pack") && r.Method == "POST" {
		repoPath := strings.TrimSuffix(path, "/git-upload-pack")
		owner, repo := splitRepoPath(repoPath)
		if owner != "" && repo != "" {
			if s.redirectMovedGitRepo(w, r, owner, repo) {
				return true
			}
			s.handleGitUploadPack(w, r, owner, repo)
			return true
		}
	}

	if strings.HasSuffix(path, "/git-receive-pack") && r.Method == "POST" {
		repoPath := strings.TrimSuffix(path, "/git-receive-pack")
		owner, repo := splitRepoPath(repoPath)
		if owner != "" && repo != "" {
			if s.redirectMovedGitRepo(w, r, owner, repo) {
				return true
			}
			s.handleGitReceivePack(w, r, owner, repo)
			return true
		}
	}

	return false
}

// splitRepoPath splits "/owner/repo.git" or "/owner/repo" into (owner, repo).
func splitRepoPath(path string) (string, string) {
	path = strings.TrimPrefix(path, "/")
	parts := strings.SplitN(path, "/", 2)
	if len(parts) != 2 {
		return "", ""
	}
	repo := strings.TrimSuffix(parts[1], ".git")
	return parts[0], repo
}

func (s *Server) resolveGitRepo(owner, repoName string) storer.Storer { //nolint:ireturn
	return s.store.GetGitStorage(owner, repoName)
}

// authorizeGitHTTP authenticates then authorizes a git request, writing the
// response and returning ok=false on failure. Anonymous requests get a 401
// and unauthorized ones a 404 whether or not the repo exists, so neither
// response discloses a private repository name. Wiki access reuses the
// repository's own contents:read.
func (s *Server) authorizeGitHTTP(w http.ResponseWriter, r *http.Request, owner, repoName string, wantWrite bool) (context.Context, *store.User, *gitTarget, bool) {
	ctx, user := s.authenticateGitRequest(r)
	// git HTTP sits outside ghHeadersMiddleware; a presented-but-invalid
	// credential earns a 401 rather than being downgraded to anonymous and,
	// on a public repo, served 200.
	if invalid, _ := ctx.Value(ctxInvalidCredential).(bool); invalid {
		w.Header().Set("WWW-Authenticate", `Basic realm="GitHub"`)
		http.Error(w, "401 Authorization Required", http.StatusUnauthorized)
		return ctx, user, nil, false
	}
	target := s.resolveGitTarget(owner, repoName)
	if !target.exists() || !s.viewerHasRepoPermission(ctx, target.repo, store.ScopeContents, store.PermRead) {
		if user == nil {
			w.Header().Set("WWW-Authenticate", `Basic realm="GitHub"`)
			http.Error(w, "401 Authorization Required", http.StatusUnauthorized)
		} else {
			http.NotFound(w, r)
		}
		return ctx, user, nil, false
	}
	if wantWrite && !s.viewerMayWriteGitTarget(ctx, target) {
		if user == nil {
			w.Header().Set("WWW-Authenticate", `Basic realm="GitHub"`)
			http.Error(w, "401 Authorization Required", http.StatusUnauthorized)
		} else {
			http.Error(w, "403 Forbidden", http.StatusForbidden)
		}
		return ctx, user, nil, false
	}
	if !s.openGitTarget(target) {
		http.NotFound(w, r)
		return ctx, user, nil, false
	}
	return ctx, user, target, true
}

func (s *Server) handleGitInfoRefs(w http.ResponseWriter, r *http.Request, owner, repoName string) {
	service := r.URL.Query().Get("service")
	if service == "" {
		http.Error(w, "service parameter required", http.StatusBadRequest)
		return
	}
	if service != "git-upload-pack" && service != "git-receive-pack" {
		http.Error(w, "unsupported service", http.StatusBadRequest)
		return
	}

	_, _, target, ok := s.authorizeGitHTTP(w, r, owner, repoName, service == "git-receive-pack")
	if !ok {
		return
	}

	w.Header().Set("Content-Type", fmt.Sprintf("application/x-%s-advertisement", service))
	w.Header().Set("Cache-Control", "no-cache")

	// Encode/Flush errors here mean the client disconnected before the
	// advertisement; log at Debug and stop.
	enc := pktline.NewEncoder(w)
	if err := enc.Encodef("# service=%s\n", service); err != nil {
		s.logger.Debug().Err(err).Str("service", service).Msg("git-http: pkt-line advertisement encode failed (client disconnected?)")
		return
	}
	if err := enc.Flush(); err != nil {
		s.logger.Debug().Err(err).Str("service", service).Msg("git-http: pkt-line advertisement flush failed (client disconnected?)")
		return
	}

	switch service {
	case "git-upload-pack":
		if gitProtocolV2Requested(r.Header.Get(gitProtocolHeader)) {
			// Protocol v2 answers info/refs with the callable commands, not
			// references; refs come from the ls-refs command on the POST.
			if err := writeGitV2CapabilityAdvertisement(w); err != nil {
				s.logger.Debug().Err(err).Msg("git-http: protocol v2 capability advertisement failed (client disconnected?)")
			}
			return
		}
		// Built here, not by go-git's transport, which cannot advertise the
		// shallow capabilities — see git_uploadpack.go.
		info, err := gitUploadPackAdvertisement(target.stor)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		advertiseDefaultBranchSymref(info, target.defaultBranch)
		if err := info.Encode(w); err != nil {
			s.logger.Error().Err(err).Msg("failed to encode advertised refs")
		}

	case "git-receive-pack":
		// Built here, not by go-git's transport, which advertises neither
		// atomic, push options, side-band nor report-status-v2 — see
		// git_receivepack.go.
		info, err := gitReceivePackAdvertisement(target.stor)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		advertiseDefaultBranchSymref(info, target.defaultBranch)
		if err := info.Encode(w); err != nil {
			s.logger.Error().Err(err).Msg("failed to encode advertised refs")
		}
	}
}

// advertiseDefaultBranchSymref sets the symref=HEAD:refs/heads/<default>
// capability so the client checks out the recorded default branch
// deterministically, rather than the first advertised ref that matches
// HEAD's object id. Set (not Add) keeps exactly one HEAD symref. An empty
// repo still advertises it (that is how `git clone` learns the branch its
// first commit lands on); a populated repo whose default branch has no
// commits does not, since pointing at a missing ref only warns.
func advertiseDefaultBranchSymref(info *packp.AdvRefs, defaultBranch string) {
	if info == nil || defaultBranch == "" {
		return
	}
	target := plumbing.NewBranchReferenceName(defaultBranch)
	tip, exists := info.References[target.String()]
	if !exists && len(info.References) > 0 {
		return
	}
	if err := info.Capabilities.Set(capability.SymRef, plumbing.HEAD.String()+":"+target.String()); err != nil {
		return
	}
	if exists {
		info.Head = &tip
	}
}

func (s *Server) handleGitUploadPack(w http.ResponseWriter, r *http.Request, owner, repoName string) {
	_, user, target, ok := s.authorizeGitHTTP(w, r, owner, repoName, false)
	if !ok {
		return
	}

	// A public repo serves this to anonymous callers, so cap the request body
	// against an unbounded stream.
	requestReader := bufio.NewReader(http.MaxBytesReader(w, r.Body, uploadPackRequestCap))
	empty, err := flushOnlyGitRequest(requestReader)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if empty {
		w.Header().Set("Content-Type", "application/x-git-upload-pack-result")
		if err := pktline.NewEncoder(w).Flush(); err != nil {
			s.logger.Error().Err(err).Msg("failed to encode empty upload-pack response")
		}
		return
	}

	// The answer is a 200 whose body is the pkt-line exchange: once the first
	// line is out there is no status code left, so a later refusal travels in
	// the stream. A request rejected before that is still answerable with 400.
	w.Header().Set("Content-Type", "application/x-git-upload-pack-result")
	stor := gitStorerWithPackReuse(r.Context(), target.storageName, target.stor)
	var result gitUploadPackResult
	if gitRequestUsesProtocolV2(r, requestReader) {
		// Each smart-HTTP POST is one whole request, so the v2 command loop
		// serves a single command and returns.
		result, err = serveGitProtocolV2(r.Context(), stor, target.defaultBranch, requestReader, w, true)
	} else {
		result, err = serveGitUploadPack(r.Context(), stor, requestReader, w)
	}
	if err != nil {
		if !result.responded {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		s.logger.Error().Err(err).Str("repo", target.storageName).Msg("git HTTP upload-pack failed")
		return
	}
	if result.sessionID != "" {
		s.logger.Debug().
			Str("repo", target.storageName).
			Str("client_session_id", result.sessionID).
			Str("server_session_id", gitServerSessionID()).
			Msg("git HTTP upload-pack session")
	}

	// A fetch with no haves is a full clone; count it for the traffic API,
	// but only once the client asked for a pack (a stateless deepening
	// fetch's first request asks only for the shallow boundary and is the
	// same clone as the one that follows). Actor is the login, or the remote
	// host for anonymous clones. Wiki clones are not counted.
	if result.packed && result.clone && !target.wiki {
		actor := r.RemoteAddr
		if host, _, splitErr := net.SplitHostPort(r.RemoteAddr); splitErr == nil {
			actor = host
		}
		if user != nil {
			actor = user.Login
		}
		s.store.RecordRepoClone(target.repo.ID, actor)
	}
}

// gitRequestUsesProtocolV2 reports whether an upload-pack POST is protocol
// v2, from either the request header or a body that opens with "command=".
func gitRequestUsesProtocolV2(r *http.Request, body *bufio.Reader) bool {
	if gitProtocolV2Requested(r.Header.Get(gitProtocolHeader)) {
		return true
	}
	head, err := body.Peek(len("0000") + len("command="))
	return err == nil && bytes.HasSuffix(head, []byte("command="))
}

func (s *Server) handleGitReceivePack(w http.ResponseWriter, r *http.Request, owner, repoName string) {
	ctx, user, target, ok := s.authorizeGitHTTP(w, r, owner, repoName, true)
	if !ok {
		return
	}

	// A malformed request is still answerable with a status code; once the
	// report is under way the only channel left is the stream.
	request, err := decodeGitReceiveRequest(bufio.NewReader(r.Body))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	outcome, err := s.applyGitReceivePack(ctx, target, request)
	if err != nil {
		var refused *refusedPushError
		if errors.As(err, &refused) {
			http.Error(w, refused.reason, http.StatusForbidden)
			return
		}
		s.logger.Error().Err(err).Str("repo", target.storageName).Msg("git HTTP receive-pack failed")
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	if target.wiki {
		s.afterWikiReceivePack(target.repo, user, outcome.applied, s.baseURL(r))
	} else {
		s.afterGitReceivePack(target.repo, user, outcome.applied, s.baseURL(r))
	}

	w.Header().Set("Content-Type", "application/x-git-receive-pack-result")
	if err := writeGitReceivePackResponse(w, request, outcome); err != nil {
		s.logger.Error().Err(err).Msg("failed to encode receive-pack response")
	}
}
