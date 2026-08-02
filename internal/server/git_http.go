package bleephub

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/format/packfile"
	"github.com/go-git/go-git/v5/plumbing/format/pktline"
	"github.com/go-git/go-git/v5/plumbing/protocol/packp"
	"github.com/go-git/go-git/v5/plumbing/protocol/packp/capability"
	"github.com/go-git/go-git/v5/plumbing/storer"
	"github.com/go-git/go-git/v5/plumbing/transport"
	gitserver "github.com/go-git/go-git/v5/plumbing/transport/server"
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
func (s *Server) authenticateGitRequest(r *http.Request) (context.Context, *User) {
	ctx := s.authenticateRequest(r)
	return ctx, ghUserFromContext(ctx)
}

// fixedGitLoader serves exactly one already-authorized storer, ignoring the
// endpoint path. The transport must never re-resolve storage from a string:
// authorization ran against a specific (owner, repo), and a second string
// normalization (e.g. trimming a trailing ".git" again) could hand the session
// a different repository than the one the caller was cleared for.
type fixedGitLoader struct {
	storer storer.Storer
}

func (l fixedGitLoader) Load(*transport.Endpoint) (storer.Storer, error) { //nolint:ireturn
	if l.storer == nil {
		return nil, transport.ErrRepositoryNotFound
	}
	return l.storer, nil
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
func (s *Server) authorizeGitHTTP(w http.ResponseWriter, r *http.Request, owner, repoName string, wantWrite bool) (context.Context, *User, *Repo, storer.Storer, bool) {
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
	if stor == nil || repo == nil || !s.viewerHasRepoPermission(ctx, repo, scopeContents, permRead) {
		if user == nil {
			w.Header().Set("WWW-Authenticate", `Basic realm="GitHub"`)
			http.Error(w, "401 Authorization Required", http.StatusUnauthorized)
		} else {
			http.NotFound(w, r)
		}
		return ctx, user, nil, nil, false
	}
	if wantWrite && !s.viewerHasRepoPermission(ctx, repo, scopeContents, permWrite) {
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

	_, _, _, stor, ok := s.authorizeGitHTTP(w, r, owner, repoName, service == "git-receive-pack")
	if !ok {
		return
	}

	server := gitserver.NewServer(fixedGitLoader{storer: stor})

	ep, err := transport.NewEndpoint(fmt.Sprintf("/%s/%s", owner, repoName))
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
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
		sess, err := server.NewUploadPackSession(ep, nil)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		info, err := sess.AdvertisedReferencesContext(r.Context())
		if err != nil {
			if err == transport.ErrEmptyRemoteRepository {
				if flushErr := enc.Flush(); flushErr != nil {
					s.logger.Debug().Err(flushErr).Str("service", service).Msg("git-http: empty-repo flush failed")
				}
				return
			}
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if err := info.Encode(w); err != nil {
			s.logger.Error().Err(err).Msg("failed to encode advertised refs")
		}

	case "git-receive-pack":
		sess, err := server.NewReceivePackSession(ep, nil)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		info, err := sess.AdvertisedReferencesContext(r.Context())
		if err != nil {
			if err == transport.ErrEmptyRemoteRepository {
				if flushErr := enc.Flush(); flushErr != nil {
					s.logger.Debug().Err(flushErr).Str("service", service).Msg("git-http: empty-repo flush failed")
				}
				return
			}
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if err := info.Encode(w); err != nil {
			s.logger.Error().Err(err).Msg("failed to encode advertised refs")
		}
	}
}

func (s *Server) handleGitUploadPack(w http.ResponseWriter, r *http.Request, owner, repoName string) {
	_, user, repo, stor, ok := s.authorizeGitHTTP(w, r, owner, repoName, false)
	if !ok {
		return
	}

	server := gitserver.NewServer(fixedGitLoader{storer: stor})

	ep, err := transport.NewEndpoint(fmt.Sprintf("/%s/%s", owner, repoName))
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	sess, err := server.NewUploadPackSession(ep, nil)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
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

	upreq := packp.NewUploadPackRequest()
	if err := upreq.Decode(requestReader); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	resp, err := sess.UploadPack(r.Context(), upreq)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	// A fetch with no haves is a full clone; count it for the traffic API.
	// The actor identity is the authenticated login, or the remote host for
	// anonymous clones of public repos.
	if len(upreq.Haves) == 0 {
		actor := r.RemoteAddr
		if host, _, splitErr := net.SplitHostPort(r.RemoteAddr); splitErr == nil {
			actor = host
		}
		if user != nil {
			actor = user.Login
		}
		s.store.RecordRepoClone(repo.ID, actor)
	}

	w.Header().Set("Content-Type", "application/x-git-upload-pack-result")
	if err := resp.Encode(w); err != nil {
		s.logger.Error().Err(err).Msg("failed to encode upload-pack response")
	}
}

func (s *Server) handleGitReceivePack(w http.ResponseWriter, r *http.Request, owner, repoName string) {
	ctx, user, repo, stor, ok := s.authorizeGitHTTP(w, r, owner, repoName, true)
	if !ok {
		return
	}

	server := gitserver.NewServer(fixedGitLoader{storer: stor})

	ep, err := transport.NewEndpoint(fmt.Sprintf("/%s/%s", owner, repoName))
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	sess, err := server.NewReceivePackSession(ep, nil)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	req := packp.NewReferenceUpdateRequest()
	if err := req.Decode(r.Body); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	result, err := s.applyReceivePack(ctx, repo, stor, sess, req)
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

	s.afterGitReceivePack(repo, user, appliedPushCommands(req, result), s.baseURL(r))

	w.Header().Set("Content-Type", "application/x-git-receive-pack-result")
	if result != nil {
		if err := result.Encode(w); err != nil {
			s.logger.Error().Err(err).Msg("failed to encode receive-pack response")
		}
	}
}

// refusedPushError is a push branch protection refused on a connection with no
// report-status channel to carry the per-ref refusal. The whole push fails
// rather than reporting a success for a ref that did not move.
type refusedPushError struct{ reason string }

func (e *refusedPushError) Error() string { return e.reason }

// applyReceivePack is the write half of both git transports: it ingests the
// pushed objects, decides every command against branch protection before any of
// them is applied, applies the ones the rule allows, and returns the report the
// client reads each ref's verdict from.
//
// The objects are ingested first because whether an update discards commits —
// the question force-push protection turns on — is only answerable once the
// commits the push carries are readable.
func (s *Server) applyReceivePack(ctx context.Context, repo *Repo, stor storer.Storer, session transport.ReceivePackSession, request *packp.ReferenceUpdateRequest) (*packp.ReportStatus, error) {
	if err := ingestPushedObjects(stor, request); err != nil {
		return nil, err
	}
	refusals, err := s.refusedPushCommands(ctx, repo, stor, request.Commands)
	if err != nil {
		return nil, err
	}
	requested := request.Commands
	if len(refusals) > 0 {
		if !request.Capabilities.Supports(capability.ReportStatus) {
			for _, command := range requested {
				if refusal, refused := refusals[command.Name]; refused {
					return nil, &refusedPushError{reason: refusal}
				}
			}
		}
		allowed := make([]*packp.Command, 0, len(requested))
		for _, command := range requested {
			if _, refused := refusals[command.Name]; !refused {
				allowed = append(allowed, command)
			}
		}
		request.Commands = allowed
	}
	// go-git's receive-pack implementation applies every command with
	// unconditional SetReference/RemoveReference calls even though the wire
	// command carries the exact old object ID. Let it validate capabilities
	// and finish pack ingestion, but apply refs here through the same atomic
	// boundaries as REST so a concurrent API write cannot be overwritten.
	allowedCommands := request.Commands
	request.Commands = nil
	report, err := session.ReceivePack(ctx, request)
	request.Commands = allowedCommands
	if err != nil && report == nil {
		return nil, err
	}
	if err != nil {
		s.logger.Debug().Err(err).Str("repo", repo.FullName).Msg("git receive-pack reported a per-ref failure")
	}
	if report == nil {
		report = packp.NewReportStatus()
		report.UnpackStatus = "ok"
	}
	var firstRefErr error
	for _, command := range allowedCommands {
		status := "ok"
		if updateErr := applyPushCommandAtomic(stor, command); updateErr != nil {
			status = "failed to update ref"
			if firstRefErr == nil {
				firstRefErr = updateErr
			}
		}
		report.CommandStatuses = append(report.CommandStatuses, &packp.CommandStatus{
			ReferenceName: command.Name,
			Status:        status,
		})
	}
	for _, command := range requested {
		if refusal, refused := refusals[command.Name]; refused {
			report.CommandStatuses = append(report.CommandStatuses, &packp.CommandStatus{ReferenceName: command.Name, Status: refusal})
		}
	}
	return report, firstRefErr
}

func applyPushCommandAtomic(stor storer.Storer, command *packp.Command) error {
	atomic, ok := stor.(interface {
		CreateReference(*plumbing.Reference) error
		RemoveReferenceCAS(*plumbing.Reference) error
	})
	if !ok {
		return fmt.Errorf("git storer for %s has no atomic ref lifecycle support", command.Name)
	}
	switch command.Action() {
	case packp.Create:
		return atomic.CreateReference(plumbing.NewHashReference(command.Name, command.New))
	case packp.Update:
		old := plumbing.NewHashReference(command.Name, command.Old)
		next := plumbing.NewHashReference(command.Name, command.New)
		return stor.CheckAndSetReference(next, old)
	case packp.Delete:
		old := plumbing.NewHashReference(command.Name, command.Old)
		return atomic.RemoveReferenceCAS(old)
	default:
		return fmt.Errorf("invalid ref update for %s", command.Name)
	}
}

// refusedPushCommands maps each command branch protection refuses to its
// reason. The commands it does not name are the ones the rule allows.
func (s *Server) refusedPushCommands(ctx context.Context, repo *Repo, stor storer.Storer, commands []*packp.Command) (map[plumbing.ReferenceName]string, error) {
	refusals := map[plumbing.ReferenceName]string{}
	for _, command := range commands {
		kind, err := classifyPushedRefWrite(stor, command)
		if err != nil {
			return nil, err
		}
		if refusal := s.protectedRefWriteRefusal(ctx, repo, stor, command.Name, kind, command.New); refusal != "" {
			refusals[command.Name] = refusal
		}
	}
	return refusals, nil
}

// classifyPushedRefWrite names the allowance one pushed command needs. The
// comparison is against the ref as the server holds it, not against the old
// value the client asserted.
func classifyPushedRefWrite(stor storer.Storer, command *packp.Command) (refWriteKind, error) {
	if command.New.IsZero() {
		return refDeletion, nil
	}
	current, err := stor.Reference(command.Name)
	if errors.Is(err, plumbing.ErrReferenceNotFound) {
		return refCreation, nil
	}
	if err != nil {
		return refForcePush, fmt.Errorf("read ref %s: %w", command.Name, err)
	}
	fastForward, err := refUpdateIsFastForward(stor, current.Hash(), command.New)
	if err != nil {
		return refForcePush, err
	}
	if fastForward {
		return refFastForward, nil
	}
	return refForcePush, nil
}

// ingestPushedObjects stores the objects a receive-pack request carries and
// leaves the request without a packfile, so the session that applies the
// surviving commands does not read the stream a second time. A push that only
// deletes refs carries no objects at all.
func ingestPushedObjects(stor storer.Storer, request *packp.ReferenceUpdateRequest) error {
	pack := request.Packfile
	request.Packfile = nil
	if pack == nil {
		return nil
	}
	defer func() { _ = pack.Close() }()
	buffered := bufio.NewReader(pack)
	if _, err := buffered.Peek(1); err != nil {
		if errors.Is(err, io.EOF) {
			return nil
		}
		return fmt.Errorf("read pushed packfile: %w", err)
	}
	if err := packfile.UpdateObjectStorage(stor, buffered); err != nil {
		return fmt.Errorf("store pushed objects: %w", err)
	}
	return nil
}

// appliedPushCommands are the commands whose ref update the session actually
// performed. A command the report marks failed moved nothing, so it must not
// raise a push event.
func appliedPushCommands(request *packp.ReferenceUpdateRequest, report *packp.ReportStatus) []*packp.Command {
	if report == nil {
		return request.Commands
	}
	applied := make([]*packp.Command, 0, len(request.Commands))
	for _, command := range request.Commands {
		ok := true
		for _, status := range report.CommandStatuses {
			if status.ReferenceName == command.Name && status.Status != "ok" {
				ok = false
				break
			}
		}
		if ok {
			applied = append(applied, command)
		}
	}
	return applied
}

func (s *Server) afterGitReceivePack(repo *Repo, user *User, applied []*packp.Command, baseURL string) {
	owner, _ := splitRepoPath("/" + repo.FullName)
	stor := s.resolveGitRepo(owner, repo.Name)
	s.store.UpdateRepo(owner, repo.Name, func(updated *Repo) {
		updated.PushedAt = updated.UpdatedAt
	})
	if stor != nil {
		needsUpdate := false
		headRef, headErr := stor.Reference(plumbing.HEAD)
		if headErr != nil {
			needsUpdate = true
		} else if headRef.Type() == plumbing.SymbolicReference {
			_, targetErr := stor.Reference(headRef.Target())
			needsUpdate = targetErr != nil
		}
		if needsUpdate {
			for _, branch := range []string{"main", "master"} {
				ref := plumbing.NewBranchReferenceName(branch)
				if _, err := stor.Reference(ref); err == nil {
					_ = stor.SetReference(plumbing.NewSymbolicReference(plumbing.HEAD, ref))
					s.store.UpdateRepo(owner, repo.Name, func(updated *Repo) { updated.DefaultBranch = branch })
					break
				}
			}
		}
	}
	for _, command := range applied {
		s.afterCommittedRefUpdate(repo, user, command.Name.String(), command.Old.String(), command.New.String(), baseURL)
	}
}
