// Package azfake is an in-process Azure Blob Storage service that speaks enough
// of the Blob REST API for the official Go client as gitstore's Azure driver
// uses it, and refuses what the service refuses where a driver's correctness
// turns on the refusal. It is exported so a harness outside this module can hold
// its own code to the same stand-in.
//
// It is written from the service's REST reference, not from the service, so it
// is evidence that a driver speaks the protocol as documented and no more than
// that; the Azurite emulator and the service itself are the other two rungs.
package azfake

import (
	"bufio"
	"bytes"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/xml"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	accountName = "fakeaccount"
	// pageSize is how many results one List Blobs response carries. The service
	// promises at most 5000 and reserves the right to return fewer, with a marker,
	// whenever it likes; a thousand is a page it could send, and makes a listing
	// of the size tests write cross from one page to the next.
	pageSize = 1000
	// batchLimit is the most sub-requests the service takes in one Blob Batch.
	batchLimit = 256
	// serviceVersion is echoed in x-ms-version. The request's own is not checked.
	serviceVersion = "2026-12-06"
	// sasFormatSince is the first service version that signs a service SAS over
	// the sixteen fields verified here; an older sv signs over fewer, in a format
	// this fake does not know, and is refused rather than waved through.
	sasFormatSince = "2020-12-06"
)

// Server is an in-process Blob service holding one storage account.
type Server struct {
	server *httptest.Server
	key    []byte

	mu         sync.Mutex
	containers map[string]*containerState
	// writes numbers every change to any blob, and is where ETags come from: the
	// service's ETag changes with every write, even one that writes the same
	// bytes, and says nothing about the content.
	writes uint64
	// pendingPolls is how many times a blob copied from now on reports its copy
	// as still pending before reporting success.
	pendingPolls int
	// emptyRangeAtEnd makes a range that starts exactly at a blob's end succeed
	// with no bytes, where the service refuses it.
	emptyRangeAtEnd bool
}

type containerState struct {
	blobs map[string]*blobState
	// staged holds each blob name's uncommitted blocks by block ID. They belong
	// to the name, not to any one upload, exactly as on the service.
	staged map[string]map[string][]byte
}

type blobState struct {
	data     []byte
	metadata map[string]string
	etag     string
	modified time.Time
	// copyID is set on the destination of a copy; pendingPolls is how many more
	// reads of its properties will say the copy has not finished.
	copyID       string
	pendingPolls int
}

// New starts a server on a loopback port, with a freshly generated account key
// and no containers. The caller closes it.
func New() *Server {
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		panic(err)
	}
	f := &Server{key: key, containers: map[string]*containerState{}}
	f.server = httptest.NewServer(http.HandlerFunc(f.serve))
	return f
}

// URL is the blob service endpoint, with the account in the path as it is for
// any endpoint addressed by IP — the Azurite emulator's included.
func (f *Server) URL() string { return f.server.URL + "/" + accountName }

// Close shuts the server down.
func (f *Server) Close() { f.server.Close() }

// AccountName is the storage account the server holds.
func (f *Server) AccountName() string { return accountName }

// AccountKey is the account's shared key, base64-encoded as the service issues it.
func (f *Server) AccountKey() string { return base64.StdEncoding.EncodeToString(f.key) }

// CreateContainer makes a container directly. Requests that name a container
// never created are answered ContainerNotFound, as the service answers them.
func (f *Server) CreateContainer(name string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.containers[name] = &containerState{blobs: map[string]*blobState{}, staged: map[string]map[string][]byte{}}
}

// SetPendingCopyPolls makes every copy started from now on report itself as
// pending, to the request that starts it and to that many reads of the
// destination's properties after, before it reports success. The service copies
// asynchronously and a client has to wait; zero, the initial state, is a copy
// that has finished by the time it is acknowledged, which is what the service
// does for most copies within an account.
func (f *Server) SetPendingCopyPolls(polls int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pendingPolls = polls
}

// AnswerARangeAtTheEndWithNoBytes makes a ranged read that starts exactly at a
// blob's end succeed with an empty body, where the service answers 416. Azurite,
// Microsoft's emulator, was found to do this when the driver first met it in
// CI; the switch is how a client's handling of such an answer is tested without
// the emulator. What Azurite's response looks like beyond that is not claimed
// here.
func (f *Server) AnswerARangeAtTheEndWithNoBytes() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.emptyRangeAtEnd = true
}

// Put stores a blob directly, without a request, in a container already made.
func (f *Server) Put(containerName, blobName string, data []byte) {
	f.mu.Lock()
	defer f.mu.Unlock()
	held, ok := f.containers[containerName]
	if !ok {
		panic("azfake: Put into container " + containerName + ", which was never created")
	}
	held.blobs[blobName] = &blobState{
		data:     append([]byte(nil), data...),
		metadata: map[string]string{},
		etag:     f.nextETag(),
		modified: time.Now().UTC(),
	}
}

// BlobNames lists a container's committed blobs directly, sorted.
func (f *Server) BlobNames(containerName string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var names []string
	if held, ok := f.containers[containerName]; ok {
		for name := range held.blobs {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	return names
}

// storageError is a refusal in the service's terms: a status, and the error
// code clients actually go by.
type storageError struct {
	status  int
	code    string
	message string
}

var (
	errBlobNotFound      = &storageError{http.StatusNotFound, "BlobNotFound", "The specified blob does not exist."}
	errContainerNotFound = &storageError{http.StatusNotFound, "ContainerNotFound", "The specified container does not exist."}
	errConditionNotMet   = &storageError{http.StatusPreconditionFailed, "ConditionNotMet", "The condition specified using HTTP conditional header(s) is not met."}
	// errNotModified is a read whose If-None-Match named the blob's own ETag. The
	// service documents 304 for a read and the same code as a lost write.
	// https://learn.microsoft.com/en-us/rest/api/storageservices/specifying-conditional-headers-for-blob-service-operations
	errNotModified       = &storageError{http.StatusNotModified, "ConditionNotMet", "The condition specified using HTTP conditional header(s) is not met."}
	errBlobAlreadyExists = &storageError{http.StatusConflict, "BlobAlreadyExists", "The specified blob already exists."}
	// errCannotVerifyCopySource is a copy whose source is not there. The service
	// names the source's own failure only in headers, which copyBlob sets.
	// https://learn.microsoft.com/en-us/rest/api/storageservices/status-and-error-codes2#copy-api-error-response
	errCannotVerifyCopySource = &storageError{http.StatusNotFound, "CannotVerifyCopySource", "The specified blob does not exist."}
	errAuthentication         = &storageError{http.StatusForbidden, "AuthenticationFailed", "Server failed to authenticate the request. Make sure the value of Authorization header is formed correctly including the signature."}
	errPermission             = &storageError{http.StatusForbidden, "AuthorizationPermissionMismatch", "This request is not authorized to perform this operation using this permission."}
	errUnsupported            = &storageError{http.StatusBadRequest, "UnsupportedOperation", "azfake does not implement this operation."}
)

type errorBody struct {
	XMLName xml.Name `xml:"Error"`
	Code    string
	Message string
}

// writeError answers with the code both in x-ms-error-code and in the body: the
// header is the only place a HEAD's refusal can say what it is.
func writeError(w http.ResponseWriter, r *http.Request, refusal *storageError) {
	w.Header().Set("x-ms-error-code", refusal.code)
	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(refusal.status)
	// A 304 has no body, by HTTP's own rule; its code is in the header alone.
	if r.Method != http.MethodHead && refusal.status != http.StatusNotModified {
		_, _ = io.WriteString(w, xml.Header)
		_ = xml.NewEncoder(w).Encode(errorBody{Code: refusal.code, Message: refusal.message})
	}
}

// split takes "/account/container/blob/name" apart. Go has already undone the
// path's escaping, so a blob name sent with its slashes as %2F arrives whole.
func split(path string) (account, containerName, blobName string) {
	account, rest, _ := strings.Cut(strings.TrimPrefix(path, "/"), "/")
	containerName, blobName, _ = strings.Cut(rest, "/")
	return account, containerName, blobName
}

func (f *Server) serve(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("x-ms-version", serviceVersion)
	account, containerName, blobName := split(r.URL.Path)
	query := r.URL.Query()
	if refusal := f.authorize(r, account, containerName, blobName); refusal != nil {
		_, _ = io.Copy(io.Discard, r.Body)
		writeError(w, r, refusal)
		return
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeError(w, r, &storageError{http.StatusBadRequest, "InvalidInput", err.Error()})
		return
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	held, ok := f.containers[containerName]
	if !ok {
		writeError(w, r, errContainerNotFound)
		return
	}
	var refusal *storageError
	switch {
	case blobName == "" && r.Method == http.MethodGet && query.Get("comp") == "list":
		f.serveList(w, r, held, containerName)
	case blobName == "" && r.Method == http.MethodPost && query.Get("comp") == "batch":
		refusal = f.serveBatch(w, r, held, account, containerName, body)
	case blobName == "":
		refusal = errUnsupported
	case r.Method == http.MethodPut && query.Get("comp") == "block":
		refusal = held.stageBlock(w, blobName, query.Get("blockid"), body)
	case r.Method == http.MethodPut && query.Get("comp") == "blocklist":
		refusal = f.commitBlockList(w, r, held, blobName, body)
	case r.Method == http.MethodPut && query.Has("comp"):
		refusal = errUnsupported
	case r.Method == http.MethodPut && r.Header.Get("x-ms-copy-source") != "":
		refusal = f.copyBlob(w, r, held, account, containerName, blobName)
	case r.Method == http.MethodPut:
		refusal = f.putBlob(w, r, held, blobName, body)
	case r.Method == http.MethodGet && !query.Has("comp"):
		refusal = held.getBlob(w, r, blobName, f.emptyRangeAtEnd)
	case r.Method == http.MethodHead && !query.Has("comp"):
		refusal = held.getProperties(w, blobName)
	case r.Method == http.MethodDelete:
		refusal = held.deleteBlob(blobName)
		if refusal == nil {
			w.Header().Set("x-ms-delete-type-permanent", "true")
			w.WriteHeader(http.StatusAccepted)
		}
	default:
		refusal = errUnsupported
	}
	if refusal != nil {
		writeError(w, r, refusal)
	}
}

// authorize lets through a request signed with the shared key, or one carrying
// a shared access signature that checks out. The Authorization header is only
// looked at — its signature is the client library's work, not the driver's —
// but a SAS is verified in full, because the driver composes what is signed: a
// fake that took any query string would pass a driver that signed the wrong
// blob, the wrong permission or the wrong expiry.
func (f *Server) authorize(r *http.Request, account, containerName, blobName string) *storageError {
	if account != accountName {
		return errAuthentication
	}
	if r.URL.Query().Has("sig") {
		return f.verifySAS(r, containerName, blobName)
	}
	if !strings.HasPrefix(r.Header.Get("Authorization"), "SharedKey "+accountName+":") {
		return errAuthentication
	}
	return nil
}

// verifySAS recomputes a service SAS from its query parameters and the resource
// the request names, the way the service does: the signature covers the blob's
// canonical name, so a SAS for one blob opens no other.
func (f *Server) verifySAS(r *http.Request, containerName, blobName string) *storageError {
	query := r.URL.Query()
	if query.Get("sv") < sasFormatSince || query.Get("sr") != "b" {
		return errAuthentication
	}
	stringToSign := strings.Join([]string{
		query.Get("sp"),
		query.Get("st"),
		query.Get("se"),
		"/blob/" + accountName + "/" + containerName + "/" + blobName,
		query.Get("si"),
		query.Get("sip"),
		query.Get("spr"),
		query.Get("sv"),
		query.Get("sr"),
		"", // the snapshot time, which a SAS for a blob itself does not have
		query.Get("ses"),
		query.Get("rscc"),
		query.Get("rscd"),
		query.Get("rsce"),
		query.Get("rscl"),
		query.Get("rsct"),
	}, "\n")
	mac := hmac.New(sha256.New, f.key)
	mac.Write([]byte(stringToSign))
	presented, err := base64.StdEncoding.DecodeString(query.Get("sig"))
	if err != nil || !hmac.Equal(presented, mac.Sum(nil)) {
		return errAuthentication
	}
	expires, err := time.Parse(time.RFC3339, query.Get("se"))
	if err != nil || !time.Now().Before(expires) {
		return errAuthentication
	}
	if start := query.Get("st"); start != "" {
		starts, err := time.Parse(time.RFC3339, start)
		if err != nil || time.Now().Before(starts) {
			return errAuthentication
		}
	}
	reads := r.Method == http.MethodGet || r.Method == http.MethodHead
	if !reads || !strings.Contains(query.Get("sp"), "r") {
		return errPermission
	}
	return nil
}

// nextETag must be called with f.mu held.
func (f *Server) nextETag() string {
	f.writes++
	return fmt.Sprintf(`"0x%015X"`, 0x8DC000000000000+f.writes)
}

// checkConditions applies If-None-Match and If-Match to a write, with the
// service's own answers: creating what exists is 409 BlobAlreadyExists, and
// every other lost condition — a swap of a blob that is not there included — is
// 412 ConditionNotMet. ETags are compared as sent, quotes and all.
func (held *containerState) checkConditions(r *http.Request, blobName string) *storageError {
	existing, exists := held.blobs[blobName]
	if match := r.Header.Get("If-None-Match"); match != "" && exists {
		if match == "*" {
			return errBlobAlreadyExists
		}
		if match == existing.etag {
			return errConditionNotMet
		}
	}
	if match := r.Header.Get("If-Match"); match != "" {
		if !exists || (match != "*" && match != existing.etag) {
			return errConditionNotMet
		}
	}
	return nil
}

// metadataOf collects a request's x-ms-meta-* headers. The service requires a
// name to be a C# identifier and answers 400 to one that is not; S3 has no such
// rule, which is why a name with a hyphen in it has to fail here.
func metadataOf(header http.Header) (map[string]string, *storageError) {
	metadata := map[string]string{}
	for name, values := range header {
		name, ok := strings.CutPrefix(strings.ToLower(name), "x-ms-meta-")
		if !ok {
			continue
		}
		if !identifier(name) {
			return nil, &storageError{http.StatusBadRequest, "InvalidMetadata", "The metadata specified is invalid. It has characters that are not permitted."}
		}
		metadata[name] = values[0]
	}
	return metadata, nil
}

func identifier(name string) bool {
	for i, r := range name {
		letter := r == '_' || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z')
		if !letter && (i == 0 || r < '0' || r > '9') {
			return false
		}
	}
	return name != ""
}

// write replaces a blob and everything staged under its name, which the service
// discards once a write to the name commits. It must be called with f.mu held.
func (f *Server) write(w http.ResponseWriter, held *containerState, blobName string, data []byte, metadata map[string]string) *blobState {
	written := &blobState{data: data, metadata: metadata, etag: f.nextETag(), modified: time.Now().UTC()}
	held.blobs[blobName] = written
	delete(held.staged, blobName)
	w.Header().Set("ETag", written.etag)
	w.Header().Set("Last-Modified", written.modified.Format(http.TimeFormat))
	return written
}

func (f *Server) putBlob(w http.ResponseWriter, r *http.Request, held *containerState, blobName string, body []byte) *storageError {
	if r.Header.Get("x-ms-blob-type") != "BlockBlob" {
		return &storageError{http.StatusBadRequest, "InvalidHeaderValue", "azfake holds block blobs only; x-ms-blob-type must say BlockBlob."}
	}
	metadata, refusal := metadataOf(r.Header)
	if refusal != nil {
		return refusal
	}
	if refusal := held.checkConditions(r, blobName); refusal != nil {
		return refusal
	}
	f.write(w, held, blobName, body, metadata)
	w.WriteHeader(http.StatusCreated)
	return nil
}

// stageBlock holds a block until a commit names it. The service requires every
// block ID of one blob to be base64 of the same length, and a driver that
// numbered its blocks "9" then "10" would find that out only in production.
func (held *containerState) stageBlock(w http.ResponseWriter, blobName, blockID string, body []byte) *storageError {
	decoded, err := base64.StdEncoding.DecodeString(blockID)
	if err != nil || len(decoded) == 0 || len(decoded) > 64 {
		return &storageError{http.StatusBadRequest, "InvalidQueryParameterValue", "The block ID is not base64 of at most 64 bytes."}
	}
	for staged := range held.staged[blobName] {
		if len(staged) != len(blockID) {
			return &storageError{http.StatusBadRequest, "InvalidBlobOrBlock", "The specified blob or block content is invalid: block IDs of one blob must be the same length."}
		}
	}
	if held.staged[blobName] == nil {
		held.staged[blobName] = map[string][]byte{}
	}
	held.staged[blobName][blockID] = body
	w.WriteHeader(http.StatusCreated)
	return nil
}

type blockList struct {
	XMLName xml.Name `xml:"BlockList"`
	Latest  []string `xml:"Latest"`
}

// commitBlockList assembles a blob from staged blocks. Only <Latest> entries
// naming uncommitted blocks are understood: the fake does not remember which
// blocks a committed blob was made of, so it cannot re-commit one.
func (f *Server) commitBlockList(w http.ResponseWriter, r *http.Request, held *containerState, blobName string, body []byte) *storageError {
	var list blockList
	if err := xml.Unmarshal(body, &list); err != nil {
		return &storageError{http.StatusBadRequest, "InvalidXmlDocument", err.Error()}
	}
	metadata, refusal := metadataOf(r.Header)
	if refusal != nil {
		return refusal
	}
	var assembled []byte
	for _, blockID := range list.Latest {
		block, ok := held.staged[blobName][blockID]
		if !ok {
			return &storageError{http.StatusBadRequest, "InvalidBlockList", "The specified block list is invalid."}
		}
		assembled = append(assembled, block...)
	}
	if refusal := held.checkConditions(r, blobName); refusal != nil {
		return refusal
	}
	f.write(w, held, blobName, assembled, metadata)
	w.WriteHeader(http.StatusCreated)
	return nil
}

func (blob *blobState) describe(w http.ResponseWriter) {
	w.Header().Set("ETag", blob.etag)
	w.Header().Set("Last-Modified", blob.modified.Format(http.TimeFormat))
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("x-ms-blob-type", "BlockBlob")
	for name, value := range blob.metadata {
		w.Header().Set("x-ms-meta-"+name, value)
	}
}

func (held *containerState) getBlob(w http.ResponseWriter, r *http.Request, blobName string, emptyAtEnd bool) *storageError {
	blob, ok := held.blobs[blobName]
	if !ok {
		return errBlobNotFound
	}
	if match := r.Header.Get("If-None-Match"); match != "" && match == blob.etag {
		return errNotModified
	}
	size := int64(len(blob.data))
	// x-ms-range is the service's own header and wins where both are sent.
	requested := r.Header.Get("x-ms-range")
	if requested == "" {
		requested = r.Header.Get("Range")
	}
	if requested == "" {
		blob.describe(w)
		w.Header().Set("Content-Length", strconv.FormatInt(size, 10))
		_, _ = w.Write(blob.data)
		return nil
	}
	start, end, ok := byteRange(requested, size)
	if !ok && emptyAtEnd && strings.HasPrefix(requested, fmt.Sprintf("bytes=%d-", size)) {
		blob.describe(w)
		w.Header().Set("Content-Length", "0")
		w.Header().Set("Content-Range", fmt.Sprintf("bytes */%d", size))
		w.WriteHeader(http.StatusPartialContent)
		return nil
	}
	if !ok {
		w.Header().Set("Content-Range", fmt.Sprintf("bytes */%d", size))
		return &storageError{http.StatusRequestedRangeNotSatisfiable, "InvalidRange", "The range specified is invalid for the current size of the resource."}
	}
	blob.describe(w)
	w.Header().Set("Content-Length", strconv.FormatInt(end-start, 10))
	w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end-1, size))
	w.WriteHeader(http.StatusPartialContent)
	_, _ = w.Write(blob.data[start:end])
	return nil
}

// byteRange reads "bytes=first-" or "bytes=first-last" against a blob's size,
// cutting a range that runs past the end and refusing one that starts there.
func byteRange(header string, size int64) (start, end int64, ok bool) {
	spec, found := strings.CutPrefix(header, "bytes=")
	first, last, dashed := strings.Cut(spec, "-")
	start, err := strconv.ParseInt(first, 10, 64)
	if !found || !dashed || err != nil || start < 0 || start >= size {
		return 0, 0, false
	}
	end = size
	if last != "" {
		parsed, err := strconv.ParseInt(last, 10, 64)
		if err != nil || parsed < start {
			return 0, 0, false
		}
		end = min(parsed+1, size)
	}
	return start, end, true
}

func (held *containerState) getProperties(w http.ResponseWriter, blobName string) *storageError {
	blob, ok := held.blobs[blobName]
	if !ok {
		return errBlobNotFound
	}
	blob.describe(w)
	w.Header().Set("Content-Length", strconv.Itoa(len(blob.data)))
	if blob.copyID != "" {
		w.Header().Set("x-ms-copy-id", blob.copyID)
		w.Header().Set("x-ms-copy-status", blob.copyStatus())
		blob.pendingPolls = max(blob.pendingPolls-1, 0)
	}
	return nil
}

func (blob *blobState) copyStatus() string {
	if blob.pendingPolls > 0 {
		return "pending"
	}
	return "success"
}

func (held *containerState) deleteBlob(blobName string) *storageError {
	if _, ok := held.blobs[blobName]; !ok {
		return errBlobNotFound
	}
	delete(held.blobs, blobName)
	return nil
}

// copyBlob is Copy Blob from a source in this account and container. The bytes
// are in place at once; what a test can hold back is only the announcement, by
// SetPendingCopyPolls.
func (f *Server) copyBlob(w http.ResponseWriter, r *http.Request, held *containerState, account, containerName, blobName string) *storageError {
	source, err := url.Parse(r.Header.Get("x-ms-copy-source"))
	if err != nil {
		return &storageError{http.StatusBadRequest, "InvalidHeaderValue", err.Error()}
	}
	sourceAccount, sourceContainer, sourceName := split(source.Path)
	if sourceAccount != account || sourceContainer != containerName {
		return &storageError{http.StatusBadRequest, "UnsupportedOperation", "azfake copies within one container only."}
	}
	from, ok := held.blobs[sourceName]
	if !ok {
		w.Header().Set("x-ms-copy-source-status-code", strconv.Itoa(http.StatusNotFound))
		w.Header().Set("x-ms-copy-source-error-code", errBlobNotFound.code)
		return errCannotVerifyCopySource
	}
	metadata := map[string]string{}
	for name, value := range from.metadata {
		metadata[name] = value
	}
	identity := make([]byte, 16)
	if _, err := rand.Read(identity); err != nil {
		return &storageError{http.StatusInternalServerError, "InternalError", err.Error()}
	}
	copied := f.write(w, held, blobName, append([]byte(nil), from.data...), metadata)
	copied.copyID = hex.EncodeToString(identity)
	copied.pendingPolls = f.pendingPolls
	w.Header().Set("x-ms-copy-id", copied.copyID)
	w.Header().Set("x-ms-copy-status", copied.copyStatus())
	w.WriteHeader(http.StatusAccepted)
	return nil
}

type enumerationResults struct {
	XMLName         xml.Name `xml:"EnumerationResults"`
	ServiceEndpoint string   `xml:"ServiceEndpoint,attr"`
	ContainerName   string   `xml:"ContainerName,attr"`
	Prefix          string   `xml:"Prefix"`
	Marker          string   `xml:"Marker"`
	Delimiter       string   `xml:"Delimiter,omitempty"`
	// Blobs holds listedBlob and listedPrefix values in one sequence, because
	// that is how the service sends them: in order of name, interleaved.
	Blobs      []any  `xml:"Blobs>_"`
	NextMarker string `xml:"NextMarker"`
}

type listedBlob struct {
	XMLName    xml.Name `xml:"Blob"`
	Name       string
	Properties listedProperties
}

type listedProperties struct {
	LastModified  string `xml:"Last-Modified"`
	Etag          string
	ContentLength int64  `xml:"Content-Length"`
	ContentType   string `xml:"Content-Type"`
	BlobType      string
}

type listedPrefix struct {
	XMLName xml.Name `xml:"BlobPrefix"`
	Name    string
}

// serveList is List Blobs, flat or folded at a delimiter, a page at a time. The
// marker is the last name of the page before, encoded so that nothing is
// tempted to read it: the service's is opaque.
func (f *Server) serveList(w http.ResponseWriter, r *http.Request, held *containerState, containerName string) {
	query := r.URL.Query()
	prefix, delimiter := query.Get("prefix"), query.Get("delimiter")
	after, err := base64.URLEncoding.DecodeString(query.Get("marker"))
	if err != nil {
		writeError(w, r, &storageError{http.StatusBadRequest, "InvalidQueryParameterValue", "The marker is not one this service issued."})
		return
	}

	// Folding keys at the delimiter keeps them in order, so the names can be
	// sorted once folded, and a page boundary falls between two entries.
	folded := map[string]bool{}
	for name := range held.blobs {
		rest, ok := strings.CutPrefix(name, prefix)
		if !ok {
			continue
		}
		if cut := strings.Index(rest, delimiter); delimiter != "" && cut >= 0 {
			folded[prefix+rest[:cut+len(delimiter)]] = true
			continue
		}
		folded[name] = false
	}
	names := make([]string, 0, len(folded))
	for name := range folded {
		if name > string(after) {
			names = append(names, name)
		}
	}
	sort.Strings(names)

	results := enumerationResults{
		ServiceEndpoint: f.URL(),
		ContainerName:   containerName,
		Prefix:          prefix,
		Marker:          query.Get("marker"),
		Delimiter:       delimiter,
	}
	if len(names) > pageSize {
		names = names[:pageSize]
		results.NextMarker = base64.URLEncoding.EncodeToString([]byte(names[pageSize-1]))
	}
	for _, name := range names {
		if folded[name] {
			results.Blobs = append(results.Blobs, listedPrefix{Name: name})
			continue
		}
		blob := held.blobs[name]
		results.Blobs = append(results.Blobs, listedBlob{Name: name, Properties: listedProperties{
			LastModified: blob.modified.Format(http.TimeFormat),
			// A listing gives the ETag bare, where a header gives it quoted.
			Etag:          strings.Trim(blob.etag, `"`),
			ContentLength: int64(len(blob.data)),
			ContentType:   "application/octet-stream",
			BlobType:      "BlockBlob",
		}})
	}
	w.Header().Set("Content-Type", "application/xml")
	_, _ = io.WriteString(w, xml.Header)
	_ = xml.NewEncoder(w).Encode(results)
}

// serveBatch is Blob Batch: a multipart body of whole HTTP requests, each a
// Delete Blob here, answered by a multipart body of whole HTTP responses. The
// batch itself succeeds whatever its parts did; a part that failed says so in
// its own status, and a client that read only the outer one would think every
// delete had worked.
func (f *Server) serveBatch(w http.ResponseWriter, r *http.Request, held *containerState, account, containerName string, body []byte) *storageError {
	_, parameters, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || parameters["boundary"] == "" {
		return &storageError{http.StatusBadRequest, "InvalidHeaderValue", "A batch is multipart/mixed with a boundary."}
	}
	// The whole body is read before any of it is acted on: a batch the service
	// refuses, for holding too much, deletes nothing.
	var subRequests []*http.Request
	var answers []batchAnswer
	parts := multipart.NewReader(bytes.NewReader(body), parameters["boundary"])
	for {
		part, err := parts.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			return &storageError{http.StatusBadRequest, "InvalidInput", err.Error()}
		}
		sub, err := http.ReadRequest(bufio.NewReader(part))
		if err != nil {
			return &storageError{http.StatusBadRequest, "InvalidInput", err.Error()}
		}
		subRequests = append(subRequests, sub)
		answers = append(answers, batchAnswer{contentID: part.Header.Get("Content-ID")})
	}
	if len(subRequests) > batchLimit {
		return &storageError{http.StatusBadRequest, "InvalidInput", "A batch holds at most 256 sub-requests."}
	}
	for i, sub := range subRequests {
		subAccount, subContainer, blobName := split(sub.URL.Path)
		switch {
		case sub.Method != http.MethodDelete || blobName == "":
			answers[i].refusal = errUnsupported
		case subAccount != account || subContainer != containerName:
			// A batch sent to a container may touch that container's blobs only.
			answers[i].refusal = errAuthentication
		case f.authorize(sub, subAccount, subContainer, blobName) != nil:
			// Each sub-request is authorized on its own, as a request of its own.
			answers[i].refusal = errAuthentication
		default:
			answers[i].refusal = held.deleteBlob(blobName)
		}
	}

	identity := make([]byte, 16)
	if _, err := rand.Read(identity); err != nil {
		return &storageError{http.StatusInternalServerError, "InternalError", err.Error()}
	}
	var response bytes.Buffer
	writer := multipart.NewWriter(&response)
	if err := writer.SetBoundary("batchresponse_" + hex.EncodeToString(identity)); err != nil {
		return &storageError{http.StatusInternalServerError, "InternalError", err.Error()}
	}
	for _, answer := range answers {
		part, err := writer.CreatePart(textproto.MIMEHeader{
			"Content-Type": {"application/http"},
			"Content-ID":   {answer.contentID},
		})
		if err != nil {
			return &storageError{http.StatusInternalServerError, "InternalError", err.Error()}
		}
		_, _ = part.Write(answer.bytes())
	}
	_ = writer.Close()
	w.Header().Set("Content-Type", "multipart/mixed; boundary="+writer.Boundary())
	w.WriteHeader(http.StatusAccepted)
	_, _ = w.Write(response.Bytes())
	return nil
}

// batchAnswer is the outcome of one sub-request of a batch.
type batchAnswer struct {
	contentID string
	refusal   *storageError
}

// bytes writes the answer as the HTTP response it would have been on its own.
func (a batchAnswer) bytes() []byte {
	var out bytes.Buffer
	if a.refusal == nil {
		fmt.Fprintf(&out, "HTTP/1.1 202 Accepted\r\nx-ms-delete-type-permanent: true\r\nx-ms-version: %s\r\n\r\n", serviceVersion)
		return out.Bytes()
	}
	var body bytes.Buffer
	body.WriteString(xml.Header)
	_ = xml.NewEncoder(&body).Encode(errorBody{Code: a.refusal.code, Message: a.refusal.message})
	fmt.Fprintf(&out, "HTTP/1.1 %d %s\r\nx-ms-error-code: %s\r\nx-ms-version: %s\r\nContent-Type: application/xml\r\nContent-Length: %d\r\n\r\n",
		a.refusal.status, http.StatusText(a.refusal.status), a.refusal.code, serviceVersion, body.Len())
	out.Write(body.Bytes())
	return out.Bytes()
}
