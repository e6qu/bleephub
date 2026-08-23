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

// uploadPackRequestCap bounds the pkt-line want/have negotiation body an
// (possibly anonymous) fetch may send. Real negotiations are kilobytes; this is
// generous headroom while still refusing an unbounded stream.
const uploadPackRequestCap = 50 << 20 // 50 MiB

// authenticateGitRequest resolves the credential on a git smart-HTTP request.
// It returns the context rather than only the user because the git routes sit
// outside the /api middleware and the credential shape — installation token,
// user-to-server token, PAT, session — lives on that context and decides what
// the caller may read.
func (s *Server) authenticateGitRequest(r *http.Request) (context.Context, *store.User) {
	ctx := s.authenticateRequest(r)
	return ctx, ghUserFromContext(ctx)
}

// tryHandleGitRequest checks if the request is a git smart HTTP request and handles it.
// Returns true if handled, false otherwise.
// Git URLs look like: /{owner}/{repo}.git/info/refs, /{owner}/{repo}.git/git-upload-pack, etc.
func (s *Server) tryHandleGitRequest(w http.ResponseWriter, r *http.Request) bool {
	path := r.URL.Path

	// Match /{owner}/{repo}/info/refs or /{owner}/{repo}.git/info/refs
	if strings.HasSuffix(path, "/info/refs") && r.Method == "GET" {
		repoPath := strings.TrimSuffix(path, "/info/refs")
		owner, repo := splitRepoPath(repoPath)
		if owner != "" && repo != "" {
			s.handleGitInfoRefs(w, r, owner, repo)
			return true
		}
	}

	// Match /{owner}/{repo}/git-upload-pack
	if strings.HasSuffix(path, "/git-upload-pack") && r.Method == "POST" {
		repoPath := strings.TrimSuffix(path, "/git-upload-pack")
		owner, repo := splitRepoPath(repoPath)
		if owner != "" && repo != "" {
			s.handleGitUploadPack(w, r, owner, repo)
			return true
		}
	}

	// Match /{owner}/{repo}/git-receive-pack
	if strings.HasSuffix(path, "/git-receive-pack") && r.Method == "POST" {
		repoPath := strings.TrimSuffix(path, "/git-receive-pack")
		owner, repo := splitRepoPath(repoPath)
		if owner != "" && repo != "" {
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

// authorizeGitHTTP authenticates the request before resolving the repository:
// anonymous requests receive the same 401 challenge whether or not the
// repository exists, and authenticated-but-unauthorized requests receive the
// same 404 as a nonexistent repository, so neither response discloses whether
// a private repository name is taken. When it returns ok=false the response
// has already been written.
func (s *Server) authorizeGitHTTP(w http.ResponseWriter, r *http.Request, owner, repoName string, wantWrite bool) (context.Context, *store.User, *store.Repo, storer.Storer, bool) {
	ctx, user := s.authenticateGitRequest(r)
	// git HTTP sits outside ghHeadersMiddleware, so an invalid or revoked
	// credential would otherwise be silently downgraded to anonymous and, on a
	// public repo, served 200. A presented-but-invalid credential earns a 401,
	// matching GitHub, rather than being treated as if no credential was given.
	if invalid, _ := ctx.Value(ctxInvalidCredential).(bool); invalid {
		w.Header().Set("WWW-Authenticate", `Basic realm="GitHub"`)
		http.Error(w, "401 Authorization Required", http.StatusUnauthorized)
		return ctx, user, nil, nil, false
	}
	stor := s.resolveGitRepo(owner, repoName)
	repo := s.store.GetRepo(owner, repoName)
	if stor == nil || repo == nil || !s.viewerHasRepoPermission(ctx, repo, store.ScopeContents, store.PermRead) {
		if user == nil {
			w.Header().Set("WWW-Authenticate", `Basic realm="GitHub"`)
			http.Error(w, "401 Authorization Required", http.StatusUnauthorized)
		} else {
			http.NotFound(w, r)
		}
		return ctx, user, nil, nil, false
	}
	if wantWrite && !s.viewerHasRepoPermission(ctx, repo, store.ScopeContents, store.PermWrite) {
		if user == nil {
			w.Header().Set("WWW-Authenticate", `Basic realm="GitHub"`)
			http.Error(w, "401 Authorization Required", http.StatusUnauthorized)
		} else {
			http.Error(w, "403 Forbidden", http.StatusForbidden)
		}
		return ctx, user, nil, nil, false
	}
	return ctx, user, repo, stor, true
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

	_, _, repo, stor, ok := s.authorizeGitHTTP(w, r, owner, repoName, service == "git-receive-pack")
	if !ok {
		return
	}

	w.Header().Set("Content-Type", fmt.Sprintf("application/x-%s-advertisement", service))
	w.Header().Set("Cache-Control", "no-cache")

	// Write pkt-line header. Encode/Flush errors at this point mean the
	// response connection dropped before we sent the advertisement — log
	// at Debug (the client is gone, nothing further to do).
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
			// Protocol v2 answers info/refs with the commands the client may
			// call rather than with references; the references themselves come
			// from the ls-refs command on the POST that follows.
			if err := writeGitV2CapabilityAdvertisement(w); err != nil {
				s.logger.Debug().Err(err).Msg("git-http: protocol v2 capability advertisement failed (client disconnected?)")
			}
			return
		}
		// Built here rather than by go-git's server transport, which cannot
		// advertise the shallow capabilities — see git_uploadpack.go.
		info, err := gitUploadPackAdvertisement(stor)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		advertiseDefaultBranchSymref(info, repo)
		if err := info.Encode(w); err != nil {
			s.logger.Error().Err(err).Msg("failed to encode advertised refs")
		}

	case "git-receive-pack":
		// Built here rather than by go-git's server transport, which advertises
		// neither atomic, push options, a side band nor report-status-v2 — see
		// git_receivepack.go.
		info, err := gitReceivePackAdvertisement(stor)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		advertiseDefaultBranchSymref(info, repo)
		if err := info.Encode(w); err != nil {
			s.logger.Error().Err(err).Msg("failed to encode advertised refs")
		}
	}
}

// advertiseDefaultBranchSymref names the repository's default branch in the ref
// advertisement, the way github does.
//
// Without the symref=HEAD:refs/heads/<default> capability a client cannot be
// told which branch to check out: it falls back to matching the advertised HEAD
// object id against the ref list and takes the first branch that matches, so
// two branches sharing a tip — or a HEAD that drifted off the default branch —
// silently produce a clone on the wrong branch. Deriving both the capability
// and the advertised HEAD id from the repository's recorded default branch
// makes the answer deterministic rather than a property of ref ordering.
//
// Set (not Add) replaces whatever HEAD symref go-git derived from storage, so
// exactly one is advertised. A repository with no refs at all still advertises
// the capability — that is how `git clone` of an empty repository learns the
// branch name its first commit should land on — but one whose default branch
// simply carries no commits yet does not, because pointing a client at a ref
// that is missing from an otherwise populated advertisement only draws a
// warning.
func advertiseDefaultBranchSymref(info *packp.AdvRefs, repo *store.Repo) {
	if info == nil || repo == nil || repo.DefaultBranch == "" {
		return
	}
	target := plumbing.NewBranchReferenceName(repo.DefaultBranch)
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
	_, user, repo, stor, ok := s.authorizeGitHTTP(w, r, owner, repoName, false)
	if !ok {
		return
	}

	// The upload-pack negotiation (a pkt-line want/have list) is small; a public
	// repo serves it to anonymous callers, so cap the request body to keep an
	// unbounded stream from exhausting memory. Real negotiations are kilobytes.
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

	// A smart-HTTP upload-pack answer is a 200 whose body is the pkt-line
	// exchange, so the header is set up front; once the first shallow or NAK
	// line is out there is no status code left to fail with, and the refusal
	// has to travel in the stream. A request rejected before that — a malformed
	// pkt-line, no wants, a capability never advertised — is still answerable
	// with a 400.
	w.Header().Set("Content-Type", "application/x-git-upload-pack-result")
	var result gitUploadPackResult
	if gitRequestUsesProtocolV2(r, requestReader) {
		// Each POST of a smart-HTTP conversation is one whole request, so the
		// v2 command loop serves a single command and returns.
		result, err = serveGitProtocolV2(r.Context(), stor, repo.DefaultBranch, requestReader, w, true)
	} else {
		result, err = serveGitUploadPack(r.Context(), stor, requestReader, w)
	}
	if err != nil {
		if !result.responded {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		s.logger.Error().Err(err).Str("repo", owner+"/"+repoName).Msg("git HTTP upload-pack failed")
		return
	}
	if result.sessionID != "" {
		s.logger.Debug().
			Str("repo", owner+"/"+repoName).
			Str("client_session_id", result.sessionID).
			Str("server_session_id", gitServerSessionID()).
			Msg("git HTTP upload-pack session")
	}

	// A fetch that carried no haves is a full clone; count it for the traffic
	// API, but only once the client actually asked for a pack — the first
	// request of a stateless deepening fetch asks for the shallow boundary
	// alone and is the same clone as the request that follows it. The actor
	// identity is the authenticated login, or the remote host for anonymous
	// clones of public repos.
	if result.packed && result.clone {
		actor := r.RemoteAddr
		if host, _, splitErr := net.SplitHostPort(r.RemoteAddr); splitErr == nil {
			actor = host
		}
		if user != nil {
			actor = user.Login
		}
		s.store.RecordRepoClone(repo.ID, actor)
	}
}

// gitRequestUsesProtocolV2 reports whether an upload-pack POST carries a
// protocol v2 command request. git states the version in a request header, and
// the body states it too: a v2 request opens with a "command=" pkt-line where a
// v0 one opens with a want line.
func gitRequestUsesProtocolV2(r *http.Request, body *bufio.Reader) bool {
	if gitProtocolV2Requested(r.Header.Get(gitProtocolHeader)) {
		return true
	}
	head, err := body.Peek(len("0000") + len("command="))
	return err == nil && bytes.HasSuffix(head, []byte("command="))
}

func (s *Server) handleGitReceivePack(w http.ResponseWriter, r *http.Request, owner, repoName string) {
	ctx, user, repo, stor, ok := s.authorizeGitHTTP(w, r, owner, repoName, true)
	if !ok {
		return
	}

	// A malformed request is still answerable with a status code, because
	// nothing of the reply has been written yet. Once the report is under way
	// the only channel left is the stream itself.
	request, err := decodeGitReceiveRequest(bufio.NewReader(r.Body))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	outcome, err := s.applyGitReceivePack(ctx, repo, stor, request)
	if err != nil {
		var refused *refusedPushError
		if errors.As(err, &refused) {
			http.Error(w, refused.reason, http.StatusForbidden)
			return
		}
		s.logger.Error().Err(err).Str("repo", owner+"/"+repoName).Msg("git HTTP receive-pack failed")
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	s.afterGitReceivePack(repo, user, outcome.applied, s.baseURL(r))

	w.Header().Set("Content-Type", "application/x-git-receive-pack-result")
	if err := writeGitReceivePackResponse(w, request, outcome); err != nil {
		s.logger.Error().Err(err).Msg("failed to encode receive-pack response")
	}
}
