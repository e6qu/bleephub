// Package gcsclient is a small client for Google Cloud Storage: the object
// operations of its JSON API, and V4 signed URLs, over plain HTTP.
//
// It exists because Google's own Go client brings gRPC, xDS and OpenTelemetry
// with it — several hundred packages — and a program that reads, writes, lists,
// copies and deletes objects needs none of them. This package depends on the
// standard library and golang.org/x/oauth2, and nothing else.
//
// It does one thing one way. There is one way to authenticate, a service
// account's key; the endpoint is stated, never assumed; nothing is retried, so
// an error is the service's own answer and not the last of several; and nothing
// is looked for in the environment. What it leaves out — buckets, ACLs, object
// versions other than the live one, compose, notifications, customer-supplied
// encryption keys — it leaves out entirely.
package gcsclient

import (
	"context"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"golang.org/x/oauth2"
	"golang.org/x/oauth2/jwt"
)

// scope is the OAuth2 scope every token is asked for: reading and writing
// objects, and nothing about buckets or access control.
const scope = "https://www.googleapis.com/auth/devstorage.read_write"

const (
	// chunkQuantum is what the service requires every chunk of a resumable upload
	// but the last to be a multiple of.
	// https://docs.cloud.google.com/storage/docs/performing-resumable-uploads#chunked-upload
	chunkQuantum = 256 << 10
	// defaultChunkBytes is the chunk size a zero Options.ChunkBytes selects.
	defaultChunkBytes = 16 << 20
	// errorBodyLimit is how much of a refusal's body is read. The service's are a
	// few hundred bytes; an emulator or a proxy in the way might send anything.
	errorBodyLimit = 64 << 10
)

// Options configure a Client. Endpoint and CredentialsJSON have no default:
// which service a program talks to, and as whom, is for whoever runs it to say.
type Options struct {
	// Endpoint is the scheme and host of the service, with no path:
	// https://storage.googleapis.com for Cloud Storage itself, or an emulator's
	// URL. Signed URLs are path-style under it.
	Endpoint string
	// CredentialsJSON is a service-account key file's bytes, the one way this
	// client authenticates. The key is exchanged for OAuth2 access tokens at the
	// token_uri the file names, and it is also what signs a URL, locally. Workload
	// identity, the metadata server and Application Default Credentials are
	// deliberately not supported: none of them holds a private key, so none can
	// sign a URL without asking the IAM API to, and a client that went looking
	// for whichever was there would authenticate as something nobody chose.
	CredentialsJSON []byte
	// Transport carries every request, the token exchange included, beneath the
	// layer that adds the access token; a harness wraps it to meter requests. Nil
	// selects http.DefaultTransport.
	Transport http.RoundTripper
	// ChunkBytes is how much of a resumable upload goes in one request, which is
	// also how much of an upload is held in memory, and the size above which an
	// upload is resumable at all. Zero selects 16 MiB. It must be a multiple of
	// 256 KiB, which the service requires of every chunk but the last.
	ChunkBytes int64
	// Clock tells the time a signed URL is dated. Nil selects time.Now.
	Clock func() time.Time
}

// Client talks to one Cloud Storage endpoint as one service account. It is safe
// for concurrent use.
type Client struct {
	endpoint   string
	host       string
	http       *http.Client
	email      string
	key        *rsa.PrivateKey
	chunkBytes int64
	clock      func() time.Time
}

// serviceAccountKey is a service-account key file, as much of it as is used.
type serviceAccountKey struct {
	Type         string `json:"type"`
	ClientEmail  string `json:"client_email"`
	PrivateKeyID string `json:"private_key_id"`
	PrivateKey   string `json:"private_key"`
	TokenURI     string `json:"token_uri"`
}

// New builds a client. It sends nothing: a key the service does not know is
// found out by the first request.
func New(opts Options) (*Client, error) {
	endpoint, err := url.Parse(opts.Endpoint)
	if err != nil {
		return nil, fmt.Errorf("gcsclient: endpoint %q: %w", opts.Endpoint, err)
	}
	if (endpoint.Scheme != "http" && endpoint.Scheme != "https") || endpoint.Host == "" || strings.Trim(endpoint.Path, "/") != "" || endpoint.RawQuery != "" {
		return nil, fmt.Errorf("gcsclient: endpoint %q: want a scheme and a host and no path, as in https://storage.googleapis.com", opts.Endpoint)
	}
	if opts.ChunkBytes < 0 || opts.ChunkBytes%chunkQuantum != 0 {
		return nil, fmt.Errorf("gcsclient: chunk size %d is not a multiple of %d, which the service requires of every chunk but the last", opts.ChunkBytes, chunkQuantum)
	}
	chunkBytes := opts.ChunkBytes
	if chunkBytes == 0 {
		chunkBytes = defaultChunkBytes
	}

	var file serviceAccountKey
	if err := json.Unmarshal(opts.CredentialsJSON, &file); err != nil {
		return nil, fmt.Errorf("gcsclient: credentials: want a service-account key file: %w", err)
	}
	// The other kinds of Google credential file — a user's, an external
	// account's, an impersonation — hold no key to sign a URL with.
	if file.Type != "service_account" {
		return nil, fmt.Errorf("gcsclient: credentials: the file's type is %q, and only a service_account key is taken", file.Type)
	}
	// A key file from Google always names its account and its token endpoint,
	// and an emulator's must name its own: there is none to assume.
	if file.ClientEmail == "" || file.TokenURI == "" {
		return nil, errors.New("gcsclient: credentials: the key file has no client_email or no token_uri")
	}
	key, err := parsePrivateKey(file.PrivateKey)
	if err != nil {
		return nil, fmt.Errorf("gcsclient: credentials: %w", err)
	}
	// The two-legged OAuth2 flow: a JWT signed with the key is exchanged at the
	// token endpoint for an access token, which is reused until it expires. It is
	// x/oauth2's jwt package and not its google package that is used, because
	// that one comes with the metadata server and the search for default
	// credentials, which this client has no use for and would rather not link.
	// https://developers.google.com/identity/protocols/oauth2/service-account#httprest
	config := &jwt.Config{
		Email:        file.ClientEmail,
		PrivateKey:   []byte(file.PrivateKey),
		PrivateKeyID: file.PrivateKeyID,
		Scopes:       []string{scope},
		TokenURL:     file.TokenURI,
	}

	transport := opts.Transport
	if transport == nil {
		transport = http.DefaultTransport
	}
	clock := opts.Clock
	if clock == nil {
		clock = time.Now
	}
	// The token source fetches through the client found in its context, for as
	// long as it lives, so the context is not any one request's.
	tokens := config.TokenSource(context.WithValue(context.Background(), oauth2.HTTPClient, &http.Client{Transport: transport}))
	return &Client{
		endpoint: endpoint.Scheme + "://" + endpoint.Host,
		host:     endpoint.Host,
		http: &http.Client{
			Transport: &oauth2.Transport{Source: tokens, Base: transport},
			// A resumable upload answers 308 to every chunk but the last, and means
			// "send more" by it, not "look elsewhere". Nothing this client asks for
			// is to be followed anywhere.
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
		email:      file.ClientEmail,
		key:        key,
		chunkBytes: chunkBytes,
		clock:      clock,
	}, nil
}

// parsePrivateKey reads the PKCS #8 RSA key a service-account key file carries.
func parsePrivateKey(encoded string) (*rsa.PrivateKey, error) {
	block, _ := pem.Decode([]byte(encoded))
	if block == nil {
		return nil, errors.New("private_key is not PEM")
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("private_key: %w", err)
	}
	key, ok := parsed.(*rsa.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("private_key is a %T, and a URL is signed with RSA", parsed)
	}
	return key, nil
}

var (
	// ErrNotFound is the service's 404. For a request that names an object it is
	// the answer whether it is the object or its bucket that is missing, and the
	// JSON API gives both the same reason, "notFound"; only a download, which is
	// the XML API's, tells them apart, as "NoSuchKey" and "NoSuchBucket". For an
	// Insert or a List, which name only the bucket, it is the bucket.
	ErrNotFound = errors.New("not found")
	// ErrPreconditionFailed is the service's 412: the object was not at the
	// generation the request required of it.
	ErrPreconditionFailed = errors.New("precondition failed")
	// ErrRangeNotSatisfiable is the service's 416: a ranged read began at or past
	// the end of the object.
	ErrRangeNotSatisfiable = errors.New("range not satisfiable")
)

// Error is a refusal by the service. errors.Is matches it against ErrNotFound,
// ErrPreconditionFailed and ErrRangeNotSatisfiable by its status.
type Error struct {
	// Operation and Resource say what was being done, and to what.
	Operation string
	Resource  string
	// Status is the HTTP status of the answer.
	Status int
	// Reason is the first of the JSON API's error reasons, such as
	// "conditionNotMet", or the XML API's error code, such as "NoSuchKey", where
	// it was a download that was refused; it is empty where the answer was in
	// neither API's format.
	Reason string
	// Message is the service's own message, or the start of a body that was not
	// the JSON API's.
	Message string
}

func (e *Error) Error() string {
	text := fmt.Sprintf("gcs %s %s: HTTP %d", e.Operation, e.Resource, e.Status)
	if e.Reason != "" {
		text += " " + e.Reason
	}
	if e.Message != "" {
		text += ": " + e.Message
	}
	return text
}

// Is reports whether target is the sentinel for this refusal's status.
func (e *Error) Is(target error) bool {
	switch e.Status {
	case http.StatusNotFound:
		return target == ErrNotFound
	case http.StatusPreconditionFailed:
		return target == ErrPreconditionFailed
	case http.StatusRequestedRangeNotSatisfiable:
		return target == ErrRangeNotSatisfiable
	}
	return false
}

// errorEnvelope is the JSON API's error body.
// https://docs.cloud.google.com/storage/docs/json_api/v1/status-codes
type errorEnvelope struct {
	Error struct {
		Message string `json:"message"`
		Errors  []struct {
			Reason string `json:"reason"`
		} `json:"errors"`
	} `json:"error"`
}

// xmlError is the XML API's error body, which is what a download is refused
// with: the download is the one request made of that API.
// https://docs.cloud.google.com/storage/docs/xml-api/reference-status
type xmlError struct {
	Code    string `xml:"Code"`
	Message string `xml:"Message"`
}

// refusal turns an answer that was not the success expected into an Error. The
// status is what the error means; the reason and the message are the service's
// account of it for whoever reads the log, in whichever of its two formats the
// API asked answers in, or the start of the body where it is in neither.
func refusal(operation, resource string, status int, body io.Reader) *Error {
	refused := &Error{Operation: operation, Resource: resource, Status: status}
	text, _ := io.ReadAll(io.LimitReader(body, errorBodyLimit))
	var envelope errorEnvelope
	if json.Unmarshal(text, &envelope) == nil && envelope.Error.Message != "" {
		refused.Message = envelope.Error.Message
		if len(envelope.Error.Errors) > 0 {
			refused.Reason = envelope.Error.Errors[0].Reason
		}
		return refused
	}
	var document xmlError
	if xml.Unmarshal(text, &document) == nil && document.Code != "" {
		refused.Reason, refused.Message = document.Code, document.Message
		return refused
	}
	refused.Message = strings.TrimSpace(string(text[:min(len(text), 512)]))
	return refused
}

// request is one HTTP request in the making.
type request struct {
	method string
	url    string
	header http.Header
	body   io.Reader
	// length is the body's, which net/http cannot learn from an io.Reader and
	// the service wants stated.
	length int64
	// statedEmpty sends "Content-Length: 0" with a method net/http would send no
	// length for: the documented cancellation of an upload is a DELETE that
	// carries one.
	statedEmpty bool
}

// send performs a request and hands back the response whatever its status.
func (c *Client) send(ctx context.Context, operation, resource string, r request) (*http.Response, error) {
	built, err := http.NewRequestWithContext(ctx, r.method, r.url, r.body)
	if err != nil {
		return nil, fmt.Errorf("gcs %s %s: %w", operation, resource, err)
	}
	for name, values := range r.header {
		built.Header[name] = values
	}
	built.ContentLength = r.length
	if r.length == 0 {
		// A nil body is what makes net/http send "Content-Length: 0" on a POST or a
		// PUT rather than chunk an empty stream.
		built.Body = nil
	}
	if r.statedEmpty {
		// net/http states the length of an empty body, for a method that does not
		// usually have one, only where there is a body to be empty and the transfer
		// encoding is named as identity.
		built.Body, built.TransferEncoding = http.NoBody, []string{"identity"}
	}
	response, err := c.http.Do(built)
	if err != nil {
		return nil, fmt.Errorf("gcs %s %s: %w", operation, resource, err)
	}
	return response, nil
}

// sendJSON performs a request whose success is a 200 with a JSON body, and
// decodes it into answer.
func (c *Client) sendJSON(ctx context.Context, operation, resource string, r request, answer any) error {
	response, err := c.send(ctx, operation, resource, r)
	if err != nil {
		return err
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		return refusal(operation, resource, response.StatusCode, response.Body)
	}
	if err := json.NewDecoder(response.Body).Decode(answer); err != nil {
		return fmt.Errorf("gcs %s %s: reading the answer: %w", operation, resource, err)
	}
	return nil
}

// Object describes one object at its live generation.
type Object struct {
	Bucket string
	Name   string
	// Size is the whole object's, in bytes, whatever part of it was read.
	Size int64
	// Generation identifies this version of the object's content: every write
	// gives the object a new one, and it is what a precondition names.
	Generation int64
	// Updated is when the object last changed. Stat, List, Insert and Copy give it
	// to the millisecond; Get and GetRange learn it from Last-Modified, to the
	// second.
	Updated time.Time
	// Metadata is the object's custom metadata. A listing does not carry it. Get
	// and GetRange learn it from HTTP headers, whose names have no case, and give
	// every name in lower case; Stat gives names as they were written.
	Metadata map[string]string
}

// objectResource is the JSON API's object resource, as much of it as is read.
// https://docs.cloud.google.com/storage/docs/json_api/v1/objects#resource
type objectResource struct {
	Bucket     string            `json:"bucket"`
	Name       string            `json:"name"`
	Size       string            `json:"size"`
	Generation string            `json:"generation"`
	Updated    string            `json:"updated"`
	Metadata   map[string]string `json:"metadata"`
}

// object reads a resource, refusing one without the facts a caller revalidates
// and expires by rather than reporting zeros for them.
func (r objectResource) object() (Object, error) {
	size, err := strconv.ParseInt(r.Size, 10, 64)
	if err != nil {
		return Object{}, fmt.Errorf("object %q came with size %q", r.Name, r.Size)
	}
	generation, err := strconv.ParseInt(r.Generation, 10, 64)
	if err != nil || generation <= 0 {
		return Object{}, fmt.Errorf("object %q came with generation %q", r.Name, r.Generation)
	}
	updated, err := time.Parse(time.RFC3339Nano, r.Updated)
	if err != nil {
		return Object{}, fmt.Errorf("object %q came with update time %q", r.Name, r.Updated)
	}
	// RFC 3339 allows any offset, and the service writes UTC; a time is handed on
	// in UTC whichever it was written in, as a download's Last-Modified is.
	return Object{Bucket: r.Bucket, Name: r.Name, Size: size, Generation: generation, Updated: updated.UTC(), Metadata: r.Metadata}, nil
}

// escape percent-encodes a bucket or an object name as one segment of a JSON
// API path, where an object's "/" must be encoded too.
// https://docs.cloud.google.com/storage/docs/request-endpoints#encoding
func escape(name string) string { return percentEncode(name, false) }

// percentEncode leaves the URI unreserved characters as they are and encodes
// every other byte, with the upper-case hex digits signing requires.
func percentEncode(text string, keepSlash bool) string {
	const hexDigits = "0123456789ABCDEF"
	var out strings.Builder
	for i := 0; i < len(text); i++ {
		b := text[i]
		unreserved := (b >= 'A' && b <= 'Z') || (b >= 'a' && b <= 'z') || (b >= '0' && b <= '9') || b == '-' || b == '.' || b == '_' || b == '~'
		if unreserved || (keepSlash && b == '/') {
			out.WriteByte(b)
			continue
		}
		out.WriteByte('%')
		out.WriteByte(hexDigits[b>>4])
		out.WriteByte(hexDigits[b&0x0f])
	}
	return out.String()
}

// objectURL is the JSON API's URL for one object.
func (c *Client) objectURL(bucket, name string) string {
	return c.endpoint + "/storage/v1/b/" + escape(bucket) + "/o/" + escape(name)
}

func named(bucket, name string) string { return bucket + "/" + name }
