package bleephub

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"

	"github.com/e6qu/bleephub/internal/store"
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
func (s *Server) authenticateGitRequest(r *http.Request) (context.Context, *store.User) {
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
		sess, err := s.newGitReceivePackSession(owner, repoName, stor)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		info, err := sess.AdvertisedReferencesContext(r.Context())
		if err != nil {
			if err == transport.ErrEmptyRemoteRepository {
				s.advertiseEmptyRepository(w, enc, repo, service)
				return
			}
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
// advertiseEmptyRepository answers a repository with no refs.
//
// github still names the default branch here — `git ls-remote --symref` on an
// empty repository reports it — and that is how a client's first commit lands
// on the branch the repository expects rather than on whatever its local
// init.defaultBranch happens to be. An empty flush conveys nothing, so send a
// reference-less advertisement carrying the symref capability instead.
func (s *Server) advertiseEmptyRepository(w io.Writer, enc *pktline.Encoder, repo *store.Repo, service string) {
	if repo == nil || repo.DefaultBranch == "" {
		if err := enc.Flush(); err != nil {
			s.logger.Debug().Err(err).Str("service", service).Msg("git-http: empty-repo flush failed")
		}
		return
	}
	info := packp.NewAdvRefs()
	info.Capabilities.Set(capability.SymRef,
		plumbing.HEAD.String()+":"+plumbing.NewBranchReferenceName(repo.DefaultBranch).String())
	if err := info.Encode(w); err != nil {
		s.logger.Debug().Err(err).Str("service", service).Msg("git-http: empty-repo advertisement failed")
	}
}

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
	result, err := serveGitUploadPack(r.Context(), stor, requestReader, w)
	if err != nil {
		if !result.responded {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		s.logger.Error().Err(err).Str("repo", owner+"/"+repoName).Msg("git HTTP upload-pack failed")
		return
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

// newGitReceivePackSession builds the go-git receive-pack session both
// transports push through. Only the fetch half of go-git's server transport was
// replaced (it cannot do shallow); the push half is still go-git's, wrapped by
// applyReceivePack for branch protection and atomic ref updates.
func (s *Server) newGitReceivePackSession(owner, repoName string, stor storer.Storer) (transport.ReceivePackSession, error) { //nolint:ireturn
	ep, err := transport.NewEndpoint(fmt.Sprintf("/%s/%s", owner, repoName))
	if err != nil {
		return nil, err
	}
	return gitserver.NewServer(fixedGitLoader{storer: stor}).NewReceivePackSession(ep, nil)
}

func (s *Server) handleGitReceivePack(w http.ResponseWriter, r *http.Request, owner, repoName string) {
	ctx, user, repo, stor, ok := s.authorizeGitHTTP(w, r, owner, repoName, true)
	if !ok {
		return
	}

	sess, err := s.newGitReceivePackSession(owner, repoName, stor)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	body := bufio.NewReader(r.Body)
	if err := skipPushedShallowLines(body); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	req := packp.NewReferenceUpdateRequest()
	if err := req.Decode(body); err != nil {
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
func (s *Server) applyReceivePack(ctx context.Context, repo *store.Repo, stor storer.Storer, session transport.ReceivePackSession, request *packp.ReferenceUpdateRequest) (*packp.ReportStatus, error) {
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
func (s *Server) refusedPushCommands(ctx context.Context, repo *store.Repo, stor storer.Storer, commands []*packp.Command) (map[plumbing.ReferenceName]string, error) {
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

// gitPushShallowLineLengths are the pkt-line lengths a "shallow <oid>" line
// from a shallow clone can carry: the 4-byte length prefix, the keyword, a
// space and a 40-character object id, with or without a trailing newline.
var gitPushShallowLineLengths = map[int]bool{4 + 48: true, 4 + 49: true}

// skipPushedShallowLines consumes the shallow lines a push from a shallow clone
// prefixes its reference-update request with.
//
// git sends one per boundary commit — a repository cloned with --depth 1 and
// then committed to sends two — and go-git's ReferenceUpdateRequest decoder
// models the field as a single optional hash, so it reads the second one as a
// command and fails the push with "capabilities delimiter not found". The lines
// carry nothing this server needs: it holds the complete history, so whether a
// pushed update is a fast-forward is answered from its own graph rather than
// from the client's truncated one.
//
// Only whole pkt-lines are consumed, so the reader is left positioned exactly
// at the first command line for the decoder that follows. Anything that is not
// a shallow line — including a short or truncated body — is left untouched for
// that decoder to report on.
func skipPushedShallowLines(body *bufio.Reader) error {
	for {
		header, err := body.Peek(4)
		if err != nil {
			return nil
		}
		// A pkt-line length prefix is exactly four hex digits, so it cannot
		// exceed 0xFFFF. Parsing at that width rather than 32 bits makes the
		// bound the format already guarantees explicit, and keeps the
		// conversion to int provably lossless on every platform.
		length, err := strconv.ParseUint(string(header), 16, 16)
		if err != nil || !gitPushShallowLineLengths[int(length)] {
			return nil
		}
		line, err := body.Peek(int(length))
		if err != nil {
			return nil
		}
		if !bytes.HasPrefix(line[4:], []byte("shallow ")) {
			return nil
		}
		if _, err := body.Discard(int(length)); err != nil {
			return err
		}
	}
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

func (s *Server) afterGitReceivePack(repo *store.Repo, user *store.User, applied []*packp.Command, baseURL string) {
	owner, _ := splitRepoPath("/" + repo.FullName)
	stor := s.resolveGitRepo(owner, repo.Name)
	s.store.UpdateRepo(owner, repo.Name, func(updated *store.Repo) {
		updated.PushedAt = updated.UpdatedAt
	})
	s.repairGitHead(owner, repo, stor)
	for _, command := range applied {
		s.afterCommittedRefUpdate(repo, user, command.Name.String(), command.Old.String(), command.New.String(), baseURL)
	}
}

// repairGitHead re-points a repository's git HEAD after a push.
//
// HEAD must be a symbolic reference to the default branch, because that is what
// a clone checks out. Re-pointing it on every push also heals a repository
// whose HEAD drifted — a repository restored from storage that never had one,
// or one an older write path left detached at a commit.
func (s *Server) repairGitHead(owner string, repo *store.Repo, stor storer.Storer) {
	if stor == nil || repo == nil {
		return
	}
	setHead := func(branch string) {
		if err := store.SetGitHeadBranch(stor, branch); err != nil {
			s.logger.Error().Err(err).Str("repo", repo.FullName).Str("branch", branch).
				Msg("could not point git HEAD at the default branch")
		}
	}
	if repo.DefaultBranch != "" {
		if _, err := stor.Reference(plumbing.NewBranchReferenceName(repo.DefaultBranch)); err == nil {
			setHead(repo.DefaultBranch)
			return
		}
	}
	// The recorded default branch carries no commits. Adopt the first
	// conventional branch that does, matching github, where the first branch
	// pushed to an empty repository becomes its default.
	for _, branch := range []string{"main", "master"} {
		if _, err := stor.Reference(plumbing.NewBranchReferenceName(branch)); err != nil {
			continue
		}
		setHead(branch)
		s.store.UpdateRepo(owner, repo.Name, func(updated *store.Repo) { updated.DefaultBranch = branch })
		return
	}
}
