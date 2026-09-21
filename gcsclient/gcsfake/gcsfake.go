// Package gcsfake is an in-process Google Cloud Storage service that speaks as
// much of the JSON API, of the XML API's download and of V4 signed URLs as
// gcsclient uses, and refuses what the service refuses where a client's
// correctness turns on the refusal: a request without a token it issued, a lost
// precondition, a chunk of the wrong size or out of its place, a batch too
// large, a signed URL that does not verify. What gcsclient does not use it does
// not implement, and it says so — a parameter or a route it does not know is
// refused, never ignored — so that a client which came to depend on more would
// find out here.
//
// It is written from Google's documentation, not from the service, so it is
// evidence that a client speaks the protocol as documented and no more than
// that. The fake-gcs-server emulator and the service itself are the other two
// rungs.
package gcsfake

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	// serviceAccount is who the key the server issues authenticates as.
	serviceAccount = "gcsfake@gcsfake.iam.gserviceaccount.com"
	// pageSize is how many entries one objects.list response carries when the
	// request does not ask for fewer. It is the service's own: "the recommended
	// upper value for maxResults is 1000 objects in a single response".
	// https://docs.cloud.google.com/storage/docs/json_api/v1/objects/list
	pageSize = 1000
	// batchLimit is the most calls the service takes in one batch.
	// https://docs.cloud.google.com/storage/docs/batch
	batchLimit = 100
	// chunkQuantum is what every chunk of a resumable upload but the last must be
	// a multiple of.
	// https://docs.cloud.google.com/storage/docs/performing-resumable-uploads#chunked-upload
	chunkQuantum = 256 << 10
	// firstGeneration is where generations start. The service's are microsecond
	// timestamps; all a client may rely on is that every write gets a new one, so
	// these only look the part, and count up.
	firstGeneration = 1700000000000000
)

// signingKey is the one RSA key of the process. Generating one takes long enough
// under the race detector that a test suite which starts a server for every test
// would spend most of its time here, and nothing a server does depends on its
// key being different from another server's: a token is good only at the server
// that issued it.
var signingKey = sync.OnceValue(func() *rsa.PrivateKey {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		panic(err)
	}
	return key
})

// Server is an in-process Cloud Storage service.
type Server struct {
	server *httptest.Server
	key    *rsa.PrivateKey
	keyID  string

	mu      sync.Mutex
	buckets map[string]*bucketState
	// tokens holds every access token issued, and the scope it was issued for.
	tokens map[string]string
	// uploads holds the resumable uploads in progress by their ID, and cancelled
	// the IDs of those that were cancelled, which the service goes on answering
	// 499.
	uploads   map[string]*upload
	cancelled map[string]bool
	// rewrites holds the copies that have been started and not finished, by their
	// token.
	rewrites map[string]*rewrite
	// writes numbers every write to any object, and is where generations come
	// from.
	writes int64
	// rewriteBytesPerCall is how much of an object one call of objects.rewrite
	// copies; zero is all of it.
	rewriteBytesPerCall int64
	// emptyRangeAtEnd makes a range that starts exactly at an object's end
	// succeed with no bytes, where the service refuses it.
	emptyRangeAtEnd bool
}

type bucketState struct {
	objects map[string]*object
}

type object struct {
	data        []byte
	contentType string
	metadata    map[string]string
	generation  int64
	updated     time.Time
}

// New starts a server on a loopback port with no buckets. The caller closes it.
func New() *Server {
	id := make([]byte, 20)
	if _, err := rand.Read(id); err != nil {
		panic(err)
	}
	f := &Server{
		key:       signingKey(),
		keyID:     hex.EncodeToString(id),
		buckets:   map[string]*bucketState{},
		tokens:    map[string]string{},
		uploads:   map[string]*upload{},
		cancelled: map[string]bool{},
		rewrites:  map[string]*rewrite{},
	}
	f.server = httptest.NewServer(http.HandlerFunc(f.serve))
	return f
}

// URL is the service's endpoint: what a client is given where it would be given
// https://storage.googleapis.com.
func (f *Server) URL() string { return f.server.URL }

// Close shuts the server down.
func (f *Server) Close() { f.server.Close() }

// serviceAccountKeyType is what Google writes in the "type" of a service
// account's key file. The fake states it for itself and does not take it from
// the client, which it must not import: it is the service, and a service that
// shared a definition with its client would agree with it by construction.
const serviceAccountKeyType = "service_account"

// CredentialsJSON is a service-account key file for the server's one service
// account, in the form Google issues them. Its token_uri is the server's own
// token endpoint, which issues an access token only for an assertion signed
// with this key; its private key is also what the server verifies a signed
// URL against.
func (f *Server) CredentialsJSON() []byte {
	der, err := x509.MarshalPKCS8PrivateKey(f.key)
	if err != nil {
		panic(err)
	}
	file, err := json.Marshal(map[string]string{
		"type":           serviceAccountKeyType,
		"project_id":     "gcsfake",
		"private_key_id": f.keyID,
		"private_key":    string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})),
		"client_email":   serviceAccount,
		"client_id":      "100000000000000000001",
		"token_uri":      f.tokenURL(),
	})
	if err != nil {
		panic(err)
	}
	return file
}

// ServiceAccount is the email address of the account CredentialsJSON is a key
// of, which a signed URL names as its signer.
func (f *Server) ServiceAccount() string { return serviceAccount }

func (f *Server) tokenURL() string { return f.server.URL + "/token" }

// CreateBucket makes a bucket directly. Requests that name a bucket never
// created are answered 404, as the service answers them.
func (f *Server) CreateBucket(name string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.buckets[name] = &bucketState{objects: map[string]*object{}}
}

// Put stores an object directly, without a request, in a bucket already made,
// and returns its generation.
func (f *Server) Put(bucket, name string, data []byte, metadata map[string]string) int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	held, ok := f.buckets[bucket]
	if !ok {
		panic("gcsfake: Put into bucket " + bucket + ", which was never created")
	}
	return f.store(held, name, data, "application/octet-stream", metadata).generation
}

// ObjectNames lists a bucket's objects directly, sorted.
func (f *Server) ObjectNames(bucket string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	held, ok := f.buckets[bucket]
	if !ok {
		return nil
	}
	return held.names("")
}

// Contents reads an object directly.
func (f *Server) Contents(bucket, name string) ([]byte, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	held, ok := f.buckets[bucket]
	if !ok {
		return nil, false
	}
	found, ok := held.objects[name]
	if !ok {
		return nil, false
	}
	return append([]byte(nil), found.data...), true
}

// UploadsInProgress counts the resumable uploads that were initiated and have
// neither completed nor been cancelled: what a client that walks away from an
// upload leaves the service holding for a week.
func (f *Server) UploadsInProgress() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.uploads)
}

// SetRewriteBytesPerCall makes every copy started from now on move at most that
// many bytes for each call of objects.rewrite, answering the others "not done"
// with a token to go on from. The service does this to a copy between locations
// or storage classes, by as much as it can move within one call's deadline;
// zero, the initial state, is a copy finished by its first call, which is what
// the service does within one location and class.
func (f *Server) SetRewriteBytesPerCall(limit int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rewriteBytesPerCall = limit
}

// AnswerARangeAtTheEndWithNoBytes makes a ranged read that starts exactly at an
// object's end succeed with an empty body, where the service answers 416. An
// emulator of another store was found to do this; the switch is how a client's
// handling of such an answer is tested without one that does.
func (f *Server) AnswerARangeAtTheEndWithNoBytes() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.emptyRangeAtEnd = true
}

// store writes an object and gives it a new generation. The caller holds the
// lock.
func (f *Server) store(held *bucketState, name string, data []byte, contentType string, metadata map[string]string) *object {
	f.writes++
	written := &object{
		data:        append([]byte(nil), data...),
		contentType: contentType,
		metadata:    map[string]string{},
		generation:  firstGeneration + f.writes,
		updated:     time.Now().UTC(),
	}
	for name, value := range metadata {
		written.metadata[name] = value
	}
	held.objects[name] = written
	return written
}

// names returns the bucket's object names that begin with prefix, in byte
// order.
func (held *bucketState) names(prefix string) []string {
	var names []string
	for name := range held.objects {
		if strings.HasPrefix(name, prefix) {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	return names
}

// serve routes a request to the API it addresses. Everything the JSON API does
// is under /storage/v1, /upload/storage/v1 or /batch/storage/v1; any other path
// is the XML API's /bucket/object.
// https://docs.cloud.google.com/storage/docs/request-endpoints
func (f *Server) serve(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeError(w, &apiError{http.StatusBadRequest, "badRequest", "the request body could not be read: " + err.Error()})
		return
	}
	path := r.URL.EscapedPath()
	switch {
	case path == "/token":
		f.issueToken(w, r, body)
	case path == "/batch/storage/v1":
		f.batch(w, r, body)
	case strings.HasPrefix(path, "/upload/storage/v1/"):
		f.serveUpload(w, r, body)
	case strings.HasPrefix(path, "/storage/v1/"):
		f.serveObjects(w, r, body)
	default:
		f.download(w, r)
	}
}

// segments splits an escaped path below a prefix into its unescaped segments.
// An object's name is one segment of a JSON API path, its "/" encoded, so a
// client that left one bare addresses something else, here as on the service.
func segments(escapedPath, prefix string) ([]string, bool) {
	parts := strings.Split(strings.TrimPrefix(escapedPath, prefix), "/")
	for i, part := range parts {
		unescaped, err := url.PathUnescape(part)
		if err != nil {
			return nil, false
		}
		parts[i] = unescaped
	}
	return parts, true
}

// apiError is a refusal in the JSON API's terms.
type apiError struct {
	status  int
	reason  string
	message string
}

var (
	errNoSuchBucket = &apiError{http.StatusNotFound, "notFound", "The specified bucket does not exist."}
	errNoSuchRoute  = &apiError{http.StatusNotFound, "notFound", "gcsfake serves no such method"}
	errCondition    = &apiError{http.StatusPreconditionFailed, "conditionNotMet", "At least one of the pre-conditions you specified did not hold."}
)

func errNoSuchObject(bucket, name string) *apiError {
	return &apiError{http.StatusNotFound, "notFound", "No such object: " + bucket + "/" + name}
}

// writeError answers in the JSON API's error format.
// https://docs.cloud.google.com/storage/docs/json_api/v1/status-codes
func writeError(w http.ResponseWriter, refusal *apiError) {
	writeJSON(w, refusal.status, map[string]any{"error": map[string]any{
		"code":    refusal.status,
		"message": refusal.message,
		"errors":  []any{map[string]any{"message": refusal.message, "domain": "global", "reason": refusal.reason}},
	}})
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	encoded, err := json.Marshal(body)
	if err != nil {
		panic(err)
	}
	w.Header().Set("Content-Type", "application/json; charset=UTF-8")
	w.WriteHeader(status)
	_, _ = w.Write(encoded)
}

// unknownParameter refuses a query parameter the fake does not implement. The
// service implements many more than gcsclient sends; ignoring one here would let
// a client believe it had asked for something.
func unknownParameter(query url.Values, known ...string) *apiError {
	for name := range query {
		implemented := false
		for _, candidate := range known {
			implemented = implemented || name == candidate
		}
		if !implemented {
			return &apiError{http.StatusBadRequest, "invalidParameter", "gcsfake does not implement the parameter " + name + " of this method"}
		}
	}
	return nil
}
