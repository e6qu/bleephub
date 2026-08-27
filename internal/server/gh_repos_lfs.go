package bleephub

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"hash"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"

	"github.com/e6qu/bleephub/internal/store"
)

// Git LFS server (the v1 batch API plus the file locking API).
//
// LFS URLs are not under /api/v3: git-lfs derives its endpoint from the git
// remote, so they live at /{owner}/{repo}[.git]/info/lfs/... and dispatch from
// the catch-all. Object bytes go to the object byte store under
// store.LFSObjectDataKey, never the git object store (which only sees the
// pointer file), and every transfer streams via PutStream/GetStream so a large
// object never lands on the heap (STORE-019). Storage is S3 when configured and
// the local fallback otherwise, so LFS works on a default deployment.

const (
	// lfsMediaType is the media type of every LFS request and response,
	// including errors: git-lfs parses an error body as this type, so a plain
	// http.Error's text/plain reads as an unparseable response.
	lfsMediaType = "application/vnd.git-lfs+json"

	lfsDocumentationURL = "https://github.com/git-lfs/git-lfs/blob/main/docs/api/README.md"

	maxLFSBatchRequestBody = 4 << 20
	maxLFSLockRequestBody  = 64 << 10

	// maxLFSObjectSize caps an upload whose batch request declared no size so an
	// unbounded body cannot fill the staging disk. GitHub's ceiling is 5 GB.
	maxLFSObjectSize = 5 << 30

	// lfsHrefExpirySeconds is advisory: hrefs are not signed URLs (transfer
	// endpoints re-authenticate every request), it only tells the client when to
	// ask for a fresh batch.
	lfsHrefExpirySeconds = 3600

	// lfsBasicTransfer is the only adapter implemented, and the one every
	// git-lfs client must support.
	lfsBasicTransfer = "basic"

	defaultLFSLockLimit = 100
)

// ─── Wire types ─────────────────────────────────────────────────────────

type lfsErrorBody struct {
	Message          string `json:"message"`
	DocumentationURL string `json:"documentation_url"`
	RequestID        string `json:"request_id"`
}

type lfsRef struct {
	Name string `json:"name"`
}

type lfsObjectSpec struct {
	OID  string `json:"oid"`
	Size int64  `json:"size"`
}

type lfsBatchRequest struct {
	Operation string          `json:"operation"`
	Transfers []string        `json:"transfers"`
	Ref       *lfsRef         `json:"ref"`
	Objects   []lfsObjectSpec `json:"objects"`
	HashAlgo  string          `json:"hash_algo"`
}

type lfsAction struct {
	Href      string            `json:"href"`
	Header    map[string]string `json:"header,omitempty"`
	ExpiresIn int               `json:"expires_in,omitempty"`
}

type lfsObjectError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type lfsBatchObject struct {
	OID           string               `json:"oid"`
	Size          int64                `json:"size"`
	Authenticated bool                 `json:"authenticated,omitempty"`
	Actions       map[string]lfsAction `json:"actions,omitempty"`
	Error         *lfsObjectError      `json:"error,omitempty"`
}

type lfsBatchResponse struct {
	Transfer string           `json:"transfer"`
	Objects  []lfsBatchObject `json:"objects"`
	HashAlgo string           `json:"hash_algo,omitempty"`
}

// lfsLockJSON is the locking API's lock shape: string id, owner as a
// single-name object.
type lfsLockJSON struct {
	ID       string           `json:"id"`
	Path     string           `json:"path"`
	LockedAt string           `json:"locked_at"`
	Owner    lfsLockOwnerJSON `json:"owner"`
}

type lfsLockOwnerJSON struct {
	Name string `json:"name"`
}

// ─── Dispatch ───────────────────────────────────────────────────────────

// tryHandleLFSRequest serves /{owner}/{repo}[.git]/info/lfs/... from the
// catch-all, returning true for any LFS request — including an unimplemented
// sub-path, which earns an LFS-shaped 404 rather than the plain-text one.
func (s *Server) tryHandleLFSRequest(w http.ResponseWriter, r *http.Request) bool {
	repoPath, rest, ok := strings.Cut(r.URL.Path, "/info/lfs/")
	if !ok {
		return false
	}
	owner, name := splitRepoPath(repoPath)
	if owner == "" || name == "" {
		return false
	}
	rest = strings.Trim(rest, "/")
	switch {
	case rest == "objects/batch" && r.Method == http.MethodPost:
		s.handleLFSBatch(w, r, owner, name)
	case rest == "objects/verify" && r.Method == http.MethodPost:
		s.handleLFSVerify(w, r, owner, name)
	case strings.HasPrefix(rest, "objects/"):
		s.handleLFSTransfer(w, r, owner, name, strings.TrimPrefix(rest, "objects/"))
	case rest == "locks" && r.Method == http.MethodGet:
		s.handleLFSListLocks(w, r, owner, name)
	case rest == "locks" && r.Method == http.MethodPost:
		s.handleLFSCreateLock(w, r, owner, name)
	case rest == "locks/verify" && r.Method == http.MethodPost:
		s.handleLFSVerifyLocks(w, r, owner, name)
	case strings.HasPrefix(rest, "locks/") && strings.HasSuffix(rest, "/unlock") && r.Method == http.MethodPost:
		id := strings.TrimSuffix(strings.TrimPrefix(rest, "locks/"), "/unlock")
		s.handleLFSUnlock(w, r, owner, name, id)
	default:
		writeLFSError(w, http.StatusNotFound, "Not Found")
	}
	return true
}

// ─── Responses ──────────────────────────────────────────────────────────

// writeLFSJSON writes a response under the LFS media type. Not writeJSON: that
// stamps application/json + an ETag, and git-lfs keys parsing off the media type.
func writeLFSJSON(w http.ResponseWriter, status int, body interface{}) {
	w.Header().Set("Content-Type", lfsMediaType)
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

// writeLFSError writes the LFS error shape so git-lfs always has a message to
// show.
func writeLFSError(w http.ResponseWriter, status int, message string) {
	writeLFSJSON(w, status, lfsErrorBody{
		Message:          message,
		DocumentationURL: lfsDocumentationURL,
		RequestID:        "",
	})
}

// writeLFSAuthRequired is the 401 whose LFS-Authenticate header makes git-lfs
// ask its credential helper rather than report the refusal.
func writeLFSAuthRequired(w http.ResponseWriter) {
	w.Header().Set("LFS-Authenticate", `Basic realm="Git LFS"`)
	w.Header().Set("WWW-Authenticate", `Basic realm="Git LFS"`)
	writeLFSError(w, http.StatusUnauthorized, "Authorization required")
}

// ─── Authorization ──────────────────────────────────────────────────────

// authorizeLFS resolves the repository and enforces the git transports' access
// rules: read needs pull, write needs push, and an unreadable repo is 404 as if
// absent. An anonymous caller gets the 401 challenge so git-lfs can fetch
// credentials and retry. When ok=false the response is already written.
func (s *Server) authorizeLFS(w http.ResponseWriter, r *http.Request, owner, name string, wantWrite bool) (context.Context, *store.User, *store.Repo, bool) {
	ctx, user := s.authenticateGitRequest(r)
	// Never downgrade a presented-but-invalid credential to anonymous and then
	// serve a public repository's objects.
	if invalid, _ := ctx.Value(ctxInvalidCredential).(bool); invalid {
		writeLFSAuthRequired(w)
		return ctx, nil, nil, false
	}
	repo := s.store.GetRepo(owner, name)
	if repo == nil || !s.viewerCanReadRepo(ctx, repo) {
		if user == nil {
			writeLFSAuthRequired(w)
			return ctx, user, nil, false
		}
		writeLFSError(w, http.StatusNotFound, "Not Found")
		return ctx, user, nil, false
	}
	if wantWrite && (user == nil || !s.viewerCanPushRepo(ctx, repo)) {
		if user == nil {
			writeLFSAuthRequired(w)
			return ctx, user, nil, false
		}
		writeLFSError(w, http.StatusForbidden, "Write access to this repository is required")
		return ctx, user, nil, false
	}
	// Honour the enterprise /repos/{owner}/{repo}/lfs toggle.
	if !repo.LFSEnabled {
		writeLFSError(w, http.StatusForbidden, "Git LFS is disabled for this repository")
		return ctx, user, nil, false
	}
	return ctx, user, repo, true
}

// ─── Batch API ──────────────────────────────────────────────────────────

// lfsObjectStore returns the configured object storage, or the local fallback.
// Never nil, so LFS never has to answer "not configured".
func (s *Server) lfsObjectStore() store.ActionsByteStore {
	if configured := s.store.ObjectByteStore; configured != nil {
		return configured
	}
	return s.localByteStore
}

func (s *Server) handleLFSBatch(w http.ResponseWriter, r *http.Request, owner, name string) {
	var request lfsBatchRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, maxLFSBatchRequestBody)).Decode(&request); err != nil {
		writeLFSError(w, http.StatusUnprocessableEntity, "Could not parse the batch request")
		return
	}
	operation := strings.ToLower(strings.TrimSpace(request.Operation))
	if operation != "download" && operation != "upload" {
		writeLFSError(w, http.StatusUnprocessableEntity, "Operation must be either download or upload")
		return
	}
	_, _, repo, ok := s.authorizeLFS(w, r, owner, name, operation == "upload")
	if !ok {
		return
	}
	// Only "basic" is required of a server; a client offering none of it is
	// refused rather than handed a transfer it cannot perform.
	if !lfsTransfersIncludeBasic(request.Transfers) {
		writeLFSError(w, http.StatusUnprocessableEntity,
			"The basic transfer adapter is required; none of the requested transfers is supported")
		return
	}
	if request.HashAlgo != "" && request.HashAlgo != "sha256" {
		writeLFSError(w, http.StatusUnprocessableEntity, "Only the sha256 hash algorithm is supported")
		return
	}

	// Build hrefs from the stored full name, never the request path, so nothing
	// a caller typed is reflected back; the oid is validated as hex first.
	base := s.baseURL(r) + "/" + repo.FullName + ".git/info/lfs/objects"
	credential := lfsForwardedCredential(r)

	response := lfsBatchResponse{Transfer: lfsBasicTransfer, Objects: make([]lfsBatchObject, 0, len(request.Objects)), HashAlgo: "sha256"}
	for _, spec := range request.Objects {
		response.Objects = append(response.Objects, s.lfsBatchObject(repo, operation, spec, base, credential))
	}
	writeLFSJSON(w, http.StatusOK, response)
}

// lfsBatchObject answers one object of a batch request. A per-object failure is
// reported in the object's own `error` while the batch call still succeeds, so
// a client can fetch the objects that do exist.
func (s *Server) lfsBatchObject(repo *store.Repo, operation string, spec lfsObjectSpec, base string, credential map[string]string) lfsBatchObject {
	object := lfsBatchObject{OID: spec.OID, Size: spec.Size}
	oid, valid := normalizeLFSOID(spec.OID)
	if !valid {
		object.OID = ""
		object.Error = &lfsObjectError{Code: http.StatusUnprocessableEntity, Message: "Invalid object id: expected a SHA-256 hex digest"}
		return object
	}
	object.OID = oid
	if spec.Size < 0 {
		object.Error = &lfsObjectError{Code: http.StatusUnprocessableEntity, Message: "Invalid object size"}
		return object
	}
	object.Authenticated = true
	stored, held := s.store.LFSObjectSize(repo.FullName, oid)
	href := base + "/" + oid

	if operation == "download" {
		if !held {
			object.Error = &lfsObjectError{Code: http.StatusNotFound, Message: "Object does not exist"}
			object.Authenticated = false
			return object
		}
		object.Size = stored
		object.Actions = map[string]lfsAction{
			"download": {Href: href, Header: credential, ExpiresIn: lfsHrefExpirySeconds},
		}
		return object
	}

	// Upload. An already-held object needs no transfer; the spec says so with an
	// entry carrying no actions, making a re-push of unchanged history free.
	if held && stored == spec.Size {
		object.Size = stored
		return object
	}
	object.Actions = map[string]lfsAction{
		// The declared size travels in the href so the transfer endpoint can
		// refuse a truncated or padded body as such, not just as a hash mismatch.
		"upload": {Href: href + "?size=" + strconv.FormatInt(spec.Size, 10), Header: credential, ExpiresIn: lfsHrefExpirySeconds},
		"verify": {Href: base + "/verify", Header: credential, ExpiresIn: lfsHrefExpirySeconds},
	}
	return object
}

// lfsTransfersIncludeBasic reports whether the client offered the basic
// adapter. An empty list is basic per the spec.
func lfsTransfersIncludeBasic(transfers []string) bool {
	if len(transfers) == 0 {
		return true
	}
	for _, transfer := range transfers {
		if strings.EqualFold(strings.TrimSpace(transfer), lfsBasicTransfer) {
			return true
		}
	}
	return false
}

// lfsForwardedCredential echoes the caller's authorized Authorization header
// into the action headers. Required, not an optimization: the transfer
// endpoints re-authenticate and hrefs are unsigned, so without it git-lfs sends
// transfer requests unauthenticated and loops on the 401, hanging `git lfs push`
// (TestGitLFSClientRoundTrip). Nothing beyond what the caller just sent is echoed.
func lfsForwardedCredential(r *http.Request) map[string]string {
	authorization := r.Header.Get("Authorization")
	if authorization == "" {
		return nil
	}
	return map[string]string{"Authorization": authorization}
}

// ─── Transfer endpoints (basic adapter) ─────────────────────────────────

func (s *Server) handleLFSTransfer(w http.ResponseWriter, r *http.Request, owner, name, oid string) {
	switch r.Method {
	case http.MethodGet, http.MethodHead:
		s.handleLFSDownload(w, r, owner, name, oid)
	case http.MethodPut:
		s.handleLFSUpload(w, r, owner, name, oid)
	default:
		writeLFSError(w, http.StatusNotFound, "Not Found")
	}
}

func (s *Server) handleLFSDownload(w http.ResponseWriter, r *http.Request, owner, name, rawOID string) {
	_, _, repo, ok := s.authorizeLFS(w, r, owner, name, false)
	if !ok {
		return
	}
	oid, valid := normalizeLFSOID(rawOID)
	if !valid {
		writeLFSError(w, http.StatusUnprocessableEntity, "Invalid object id")
		return
	}
	size, held := s.store.LFSObjectSize(repo.FullName, oid)
	if !held {
		writeLFSError(w, http.StatusNotFound, "Object does not exist")
		return
	}
	body, err := s.lfsObjectStore().GetStream(r.Context(), store.LFSObjectDataKey(oid))
	if err != nil {
		s.logger.Error().Err(err).Str("repo", repo.FullName).Msg("git lfs: object bytes missing from the object store")
		writeLFSError(w, http.StatusNotFound, "Object does not exist")
		return
	}
	defer body.Close()
	// Stream store-to-client: an LFS object must never materialize on the heap
	// (STORE-019).
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Length", strconv.FormatInt(size, 10))
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(http.StatusOK)
	if r.Method == http.MethodHead {
		return
	}
	if _, err := io.Copy(w, body); err != nil {
		s.logger.Debug().Err(err).Msg("git lfs: object download write failed")
	}
}

// handleLFSUpload streams an object into the byte store while hashing it, and
// refuses it unless the bytes hash to the advertised oid and match the declared
// size. The oid is the pointer file's only name, so mismatched content would
// silently corrupt every future checkout.
func (s *Server) handleLFSUpload(w http.ResponseWriter, r *http.Request, owner, name, rawOID string) {
	_, _, repo, ok := s.authorizeLFS(w, r, owner, name, true)
	if !ok {
		return
	}
	oid, valid := normalizeLFSOID(rawOID)
	if !valid {
		writeLFSError(w, http.StatusUnprocessableEntity, "Invalid object id")
		return
	}
	declared, declaredOK := lfsDeclaredSize(r)
	if !declaredOK {
		writeLFSError(w, http.StatusUnprocessableEntity, "Invalid object size")
		return
	}
	if declared >= 0 && r.ContentLength >= 0 && r.ContentLength != declared {
		writeLFSError(w, http.StatusUnprocessableEntity, "Object size does not match the size declared in the batch request")
		return
	}

	limit := int64(maxLFSObjectSize)
	if declared >= 0 {
		// One byte past the declaration distinguishes "too long" from "exactly
		// as promised" without an unbounded read.
		limit = declared + 1
	}
	counted := &lfsHashingReader{inner: io.LimitReader(r.Body, limit), hash: sha256.New()}

	// The key is content-addressed, so bytes already stored under this oid are
	// the same object and are verified. Verify the stream and record this
	// repository's membership instead of overwriting them.
	if _, exists := s.store.LFSObjectStoredAnywhere(oid); exists {
		if _, err := io.Copy(io.Discard, counted); err != nil {
			writeLFSError(w, http.StatusInternalServerError, "Failed to read the uploaded object")
			return
		}
		if !s.finishLFSUpload(w, repo, oid, declared, counted, false) {
			return
		}
		writeLFSJSON(w, http.StatusOK, struct{}{})
		return
	}

	key := store.LFSObjectDataKey(oid)
	if err := s.lfsObjectStore().PutStream(r.Context(), key, counted); err != nil {
		s.logger.Error().Err(err).Str("repo", repo.FullName).Msg("git lfs: object upload failed")
		writeLFSError(w, http.StatusInternalServerError, "Failed to store the uploaded object")
		return
	}
	if !s.finishLFSUpload(w, repo, oid, declared, counted, true) {
		return
	}
	writeLFSJSON(w, http.StatusOK, struct{}{})
}

// finishLFSUpload verifies the read bytes and only then registers the object.
// Registration is the upload's commit point: unverified bytes are never
// registered and nothing unregistered is served, so a rejected upload is
// undownloadable. Mismatched bytes written under the key are deleted.
func (s *Server) finishLFSUpload(w http.ResponseWriter, repo *store.Repo, oid string, declared int64, counted *lfsHashingReader, wrote bool) bool {
	digest := hex.EncodeToString(counted.hash.Sum(nil))
	discard := func(status int, message string) bool {
		if wrote {
			if err := s.lfsObjectStore().Delete(context.Background(), store.LFSObjectDataKey(oid)); err != nil {
				s.logger.Error().Err(err).Str("repo", repo.FullName).Msg("git lfs: discarding a rejected object failed")
			}
		}
		writeLFSError(w, status, message)
		return false
	}
	if declared >= 0 && counted.read != declared {
		return discard(http.StatusUnprocessableEntity, "Object size does not match the size declared in the batch request")
	}
	if digest != oid {
		return discard(http.StatusUnprocessableEntity, "Object content does not match its oid")
	}
	s.store.RegisterLFSObject(repo.FullName, oid, counted.read)
	return true
}

// lfsHashingReader hashes and counts a stream in passing, verifying an upload
// without a second pass or holding the object.
type lfsHashingReader struct {
	inner io.Reader
	hash  hash.Hash
	read  int64
}

func (r *lfsHashingReader) Read(p []byte) (int, error) {
	n, err := r.inner.Read(p)
	if n > 0 {
		r.read += int64(n)
		_, _ = r.hash.Write(p[:n])
	}
	return n, err
}

// lfsDeclaredSize reads the size the batch response put in the upload href.
// Absent is allowed (-1, an older batch's href); malformed is a refusal.
func lfsDeclaredSize(r *http.Request) (int64, bool) {
	raw := r.URL.Query().Get("size")
	if raw == "" {
		return -1, true
	}
	size, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || size < 0 {
		return -1, false
	}
	return size, true
}

// handleLFSVerify answers the batch verify action: the object is present at the
// size the client believes it uploaded.
func (s *Server) handleLFSVerify(w http.ResponseWriter, r *http.Request, owner, name string) {
	_, _, repo, ok := s.authorizeLFS(w, r, owner, name, false)
	if !ok {
		return
	}
	var spec lfsObjectSpec
	if err := json.NewDecoder(io.LimitReader(r.Body, maxLFSLockRequestBody)).Decode(&spec); err != nil {
		writeLFSError(w, http.StatusUnprocessableEntity, "Could not parse the verify request")
		return
	}
	oid, valid := normalizeLFSOID(spec.OID)
	if !valid {
		writeLFSError(w, http.StatusUnprocessableEntity, "Invalid object id")
		return
	}
	size, held := s.store.LFSObjectSize(repo.FullName, oid)
	if !held {
		writeLFSError(w, http.StatusNotFound, "Object does not exist")
		return
	}
	if spec.Size != 0 && spec.Size != size {
		writeLFSError(w, http.StatusUnprocessableEntity, "Object size does not match the stored object")
		return
	}
	writeLFSJSON(w, http.StatusOK, lfsObjectSpec{OID: oid, Size: size})
}

// normalizeLFSOID canonicalises an LFS oid (a SHA-256 hex digest, optionally
// "sha256:"-prefixed) to lowercase hex, refusing everything else. This also
// keeps caller-supplied text out of hrefs, storage keys and response bodies.
func normalizeLFSOID(oid string) (string, bool) {
	oid = strings.ToLower(strings.TrimSpace(oid))
	oid = strings.TrimPrefix(oid, "sha256:")
	if len(oid) != sha256.Size*2 {
		return "", false
	}
	if _, err := hex.DecodeString(oid); err != nil {
		return "", false
	}
	return oid, true
}

// ─── Locking API ────────────────────────────────────────────────────────

func (s *Server) handleLFSCreateLock(w http.ResponseWriter, r *http.Request, owner, name string) {
	_, user, repo, ok := s.authorizeLFS(w, r, owner, name, true)
	if !ok {
		return
	}
	var request struct {
		Path string  `json:"path"`
		Ref  *lfsRef `json:"ref"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, maxLFSLockRequestBody)).Decode(&request); err != nil {
		writeLFSError(w, http.StatusUnprocessableEntity, "Could not parse the lock request")
		return
	}
	path := strings.TrimSpace(request.Path)
	if path == "" {
		writeLFSError(w, http.StatusUnprocessableEntity, "A path is required")
		return
	}
	ref := ""
	if request.Ref != nil {
		ref = request.Ref.Name
	}
	lock, created := s.store.CreateLFSLock(repo.FullName, path, ref, user.ID, user.Login)
	if !created {
		// The spec's conflict shape carries the blocking lock so the client can
		// name its holder.
		writeLFSJSON(w, http.StatusConflict, struct {
			Lock             lfsLockJSON `json:"lock"`
			Message          string      `json:"message"`
			DocumentationURL string      `json:"documentation_url"`
			RequestID        string      `json:"request_id"`
		}{
			Lock:             lfsLockToJSON(lock),
			Message:          "Path is already locked",
			DocumentationURL: lfsDocumentationURL,
		})
		return
	}
	writeLFSJSON(w, http.StatusCreated, struct {
		Lock lfsLockJSON `json:"lock"`
	}{Lock: lfsLockToJSON(lock)})
}

func (s *Server) handleLFSListLocks(w http.ResponseWriter, r *http.Request, owner, name string) {
	_, _, repo, ok := s.authorizeLFS(w, r, owner, name, false)
	if !ok {
		return
	}
	query := r.URL.Query()
	locks := s.filterLFSLocks(repo.FullName, query.Get("path"), query.Get("id"))
	page, next := paginateLFSLocks(locks, query.Get("cursor"), query.Get("limit"))
	writeLFSJSON(w, http.StatusOK, struct {
		Locks      []lfsLockJSON `json:"locks"`
		NextCursor string        `json:"next_cursor,omitempty"`
	}{Locks: lfsLocksToJSON(page), NextCursor: next})
}

// handleLFSVerifyLocks answers the pre-push check, splitting locks into "ours"
// and "theirs"; git-lfs locally refuses a push touching a path in "theirs".
func (s *Server) handleLFSVerifyLocks(w http.ResponseWriter, r *http.Request, owner, name string) {
	_, user, repo, ok := s.authorizeLFS(w, r, owner, name, true)
	if !ok {
		return
	}
	var request struct {
		Cursor string  `json:"cursor"`
		Limit  int     `json:"limit"`
		Ref    *lfsRef `json:"ref"`
	}
	// The body is optional; ignore a decode failure on an empty one.
	_ = json.NewDecoder(io.LimitReader(r.Body, maxLFSLockRequestBody)).Decode(&request)
	limit := ""
	if request.Limit > 0 {
		limit = strconv.Itoa(request.Limit)
	}
	page, next := paginateLFSLocks(s.store.ListLFSLocks(repo.FullName), request.Cursor, limit)
	ours, theirs := []*store.LFSLock{}, []*store.LFSLock{}
	for _, lock := range page {
		if lock.OwnerID == user.ID {
			ours = append(ours, lock)
		} else {
			theirs = append(theirs, lock)
		}
	}
	writeLFSJSON(w, http.StatusOK, struct {
		Ours       []lfsLockJSON `json:"ours"`
		Theirs     []lfsLockJSON `json:"theirs"`
		NextCursor string        `json:"next_cursor,omitempty"`
	}{Ours: lfsLocksToJSON(ours), Theirs: lfsLocksToJSON(theirs), NextCursor: next})
}

func (s *Server) handleLFSUnlock(w http.ResponseWriter, r *http.Request, owner, name, rawID string) {
	_, user, repo, ok := s.authorizeLFS(w, r, owner, name, true)
	if !ok {
		return
	}
	id, err := strconv.Atoi(rawID)
	if err != nil || id <= 0 {
		writeLFSError(w, http.StatusNotFound, "Not Found")
		return
	}
	var request struct {
		Force bool `json:"force"`
	}
	_ = json.NewDecoder(io.LimitReader(r.Body, maxLFSLockRequestBody)).Decode(&request)
	lock := s.store.GetLFSLock(repo.FullName, id)
	if lock == nil {
		writeLFSError(w, http.StatusNotFound, "Lock does not exist")
		return
	}
	// Releasing another user's lock requires a deliberate force.
	if lock.OwnerID != user.ID && !request.Force {
		writeLFSError(w, http.StatusForbidden, "Lock is held by "+lock.OwnerName+"; unlock with force to release it")
		return
	}
	released := s.store.DeleteLFSLock(repo.FullName, id)
	if released == nil {
		writeLFSError(w, http.StatusNotFound, "Lock does not exist")
		return
	}
	writeLFSJSON(w, http.StatusOK, struct {
		Lock lfsLockJSON `json:"lock"`
	}{Lock: lfsLockToJSON(released)})
}

func (s *Server) filterLFSLocks(repoKey, path, id string) []*store.LFSLock {
	locks := s.store.ListLFSLocks(repoKey)
	if path == "" && id == "" {
		return locks
	}
	filtered := make([]*store.LFSLock, 0, len(locks))
	for _, lock := range locks {
		if path != "" && lock.Path != path {
			continue
		}
		if id != "" && strconv.Itoa(lock.ID) != id {
			continue
		}
		filtered = append(filtered, lock)
	}
	return filtered
}

// paginateLFSLocks pages a sorted lock list: the cursor is the first lock's id
// and next_cursor names the first lock that did not fit.
func paginateLFSLocks(locks []*store.LFSLock, cursor, limit string) ([]*store.LFSLock, string) {
	sort.Slice(locks, func(i, j int) bool { return locks[i].ID < locks[j].ID })
	if cursor != "" {
		start, err := strconv.Atoi(cursor)
		if err == nil {
			for len(locks) > 0 && locks[0].ID < start {
				locks = locks[1:]
			}
		}
	}
	size := defaultLFSLockLimit
	if limit != "" {
		if parsed, err := strconv.Atoi(limit); err == nil && parsed > 0 && parsed < defaultLFSLockLimit {
			size = parsed
		}
	}
	if len(locks) > size {
		return locks[:size], strconv.Itoa(locks[size].ID)
	}
	return locks, ""
}

func lfsLockToJSON(lock *store.LFSLock) lfsLockJSON {
	return lfsLockJSON{
		ID:       strconv.Itoa(lock.ID),
		Path:     lock.Path,
		LockedAt: lock.LockedAt.UTC().Format("2006-01-02T15:04:05Z"),
		Owner:    lfsLockOwnerJSON{Name: lock.OwnerName},
	}
}

func lfsLocksToJSON(locks []*store.LFSLock) []lfsLockJSON {
	out := make([]lfsLockJSON, 0, len(locks))
	for _, lock := range locks {
		out = append(out, lfsLockToJSON(lock))
	}
	return out
}
