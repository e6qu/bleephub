package gcsclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"net/url"
	"strconv"
)

// SizeUnknown is the InsertOptions.Size of a body whose length the caller does
// not know.
const SizeUnknown int64 = -1

// Precondition restricts a write to the state its writer expects of the object.
// The zero value places no restriction.
type Precondition struct {
	set        bool
	generation int64
}

// DoesNotExist lets a write through only if the object has no live generation.
func DoesNotExist() Precondition { return Precondition{set: true} }

// GenerationMatch lets a write through only if the object's live generation is
// the one given.
func GenerationMatch(generation int64) Precondition {
	return Precondition{set: true, generation: generation}
}

// apply states the precondition as ifGenerationMatch, whose zero means "no live
// generation": "the request only proceeds if no object with the specified name
// exists in the bucket". A precondition that does not hold is answered 412. The
// documentation does not say what a generation other than zero is answered when
// the object does not exist; 412 is what this client takes it to be, and what
// the fake-gcs-server emulator answers.
// https://docs.cloud.google.com/storage/docs/request-preconditions
func (p Precondition) apply(query url.Values) {
	if p.set {
		query.Set("ifGenerationMatch", strconv.FormatInt(p.generation, 10))
	}
}

// InsertOptions describe an object to be written.
type InsertOptions struct {
	// Size is the length of the body in bytes, or SizeUnknown. A body that turns
	// out not to be the size it was said to be is not written.
	Size int64
	// Precondition restricts the write; a write it refuses is
	// ErrPreconditionFailed.
	Precondition Precondition
	// ContentType is the object's media type. The service calls an object written
	// without one application/octet-stream.
	ContentType string
	// Metadata is the object's custom metadata.
	Metadata map[string]string
}

// description is the object resource sent with an upload.
type description struct {
	Name        string            `json:"name"`
	ContentType string            `json:"contentType,omitempty"`
	Metadata    map[string]string `json:"metadata,omitempty"`
}

// Insert writes an object, replacing any of the same name, and returns what was
// written. The object appears whole or not at all.
//
// A body of known size that fits one chunk goes in a single multipart request.
// Any other — larger, or of unknown size — goes as a resumable upload, a chunk
// at a time, holding one chunk in memory.
//
// The precondition goes with either. A resumable upload states it when the
// upload is initiated, and this client depends on the service holding the upload
// to it when the last chunk makes the object. Google's documentation does not
// say that it does: objects.insert documents ifGenerationMatch for every upload
// type, and the pages on resumable uploads and on preconditions say nothing of
// when it is applied. It is what Google's own client libraries depend on for
// every conditional write too large for one request, and what the
// fake-gcs-server emulator does; a service that applied it only at initiation
// would let a second writer in while the first was still sending.
// https://docs.cloud.google.com/storage/docs/json_api/v1/objects/insert
// https://docs.cloud.google.com/storage/docs/request-preconditions
func (c *Client) Insert(ctx context.Context, bucket, name string, body io.Reader, options InsertOptions) (Object, error) {
	if options.Size < 0 || options.Size > c.chunkBytes {
		return c.insertResumable(ctx, bucket, name, body, options)
	}
	// An io.Reader cannot be shown to be the size it was said to be without
	// reading it, and a request already sent cannot be taken back, so the body is
	// read first — which is why one request carries no more than a chunk.
	content := make([]byte, options.Size)
	if _, err := io.ReadFull(body, content); err != nil {
		return Object{}, fmt.Errorf("gcs insert %s: reading the %d bytes the body was said to be: %w", named(bucket, name), options.Size, err)
	}
	if err := exhausted(body); err != nil {
		return Object{}, fmt.Errorf("gcs insert %s: after the %d bytes the body was said to be: %w", named(bucket, name), options.Size, err)
	}
	return c.insertMultipart(ctx, bucket, name, content, options)
}

// exhausted reports an error unless the reader has nothing more to give.
func exhausted(body io.Reader) error {
	var next [1]byte
	switch _, err := io.ReadFull(body, next[:]); err {
	case io.EOF:
		return nil
	case nil:
		return errors.New("the body goes on")
	default:
		return err
	}
}

// insertMultipart sends the object's description and its content as the two
// parts of one multipart/related request.
// https://docs.cloud.google.com/storage/docs/uploading-objects#json-api-multipart-upload
// https://docs.cloud.google.com/storage/docs/json_api/v1/objects/insert
func (c *Client) insertMultipart(ctx context.Context, bucket, name string, content []byte, options InsertOptions) (Object, error) {
	described, err := json.Marshal(description{Name: name, ContentType: options.ContentType, Metadata: options.Metadata})
	if err != nil {
		return Object{}, fmt.Errorf("gcs insert %s: %w", named(bucket, name), err)
	}
	var head, tail bytes.Buffer
	writer := multipart.NewWriter(&head)
	if part, err := writer.CreatePart(textproto.MIMEHeader{"Content-Type": {"application/json; charset=UTF-8"}}); err != nil {
		return Object{}, fmt.Errorf("gcs insert %s: %w", named(bucket, name), err)
	} else if _, err := part.Write(described); err != nil {
		return Object{}, fmt.Errorf("gcs insert %s: %w", named(bucket, name), err)
	}
	if _, err := writer.CreatePart(textproto.MIMEHeader{"Content-Type": {mediaType(options)}}); err != nil {
		return Object{}, fmt.Errorf("gcs insert %s: %w", named(bucket, name), err)
	}
	// What the writer has written so far ends where the content begins, and what
	// it writes on closing is what follows the content; the content itself is
	// never copied between them.
	closing := multipart.NewWriter(&tail)
	if err := closing.SetBoundary(writer.Boundary()); err != nil {
		return Object{}, fmt.Errorf("gcs insert %s: %w", named(bucket, name), err)
	}
	if err := closing.Close(); err != nil {
		return Object{}, fmt.Errorf("gcs insert %s: %w", named(bucket, name), err)
	}

	query := url.Values{"uploadType": {"multipart"}, "fields": {objectFields}}
	options.Precondition.apply(query)
	var answer objectResource
	err = c.sendJSON(ctx, "insert", named(bucket, name), request{
		method: http.MethodPost,
		url:    c.endpoint + "/upload/storage/v1/b/" + escape(bucket) + "/o?" + query.Encode(),
		header: http.Header{"Content-Type": {"multipart/related; boundary=" + writer.Boundary()}},
		body:   io.MultiReader(&head, bytes.NewReader(content), &tail),
		length: int64(head.Len() + len(content) + tail.Len()),
	}, &answer)
	if err != nil {
		return Object{}, err
	}
	return written(named(bucket, name), answer)
}

func mediaType(options InsertOptions) string {
	if options.ContentType == "" {
		return "application/octet-stream"
	}
	return options.ContentType
}

func written(resource string, answer objectResource) (Object, error) {
	object, err := answer.object()
	if err != nil {
		return Object{}, fmt.Errorf("gcs insert %s: the write was accepted, but %w", resource, err)
	}
	return object, nil
}

// insertResumable initiates a resumable upload and sends the body through it a
// chunk at a time. Nothing appears under the name until the last chunk is
// accepted — "only a completed resumable upload appears in your bucket" — and an
// upload that fails before then is cancelled, so that the service is not left
// holding its chunks for the week it would otherwise keep them.
// https://docs.cloud.google.com/storage/docs/resumable-uploads
// https://docs.cloud.google.com/storage/docs/performing-resumable-uploads
func (c *Client) insertResumable(ctx context.Context, bucket, name string, body io.Reader, options InsertOptions) (Object, error) {
	resource := named(bucket, name)
	session, err := c.initiate(ctx, bucket, name, options)
	if err != nil {
		return Object{}, err
	}
	object, err := c.sendChunks(ctx, resource, session, body, options.Size)
	if err != nil {
		// The caller's context may be why the upload failed, and the cancellation
		// has to be sent all the same.
		if cancelErr := c.cancel(context.WithoutCancel(ctx), resource, session); cancelErr != nil {
			return Object{}, errors.Join(err, cancelErr)
		}
		return Object{}, err
	}
	return object, nil
}

// initiate opens an upload session and returns its URI.
func (c *Client) initiate(ctx context.Context, bucket, name string, options InsertOptions) (string, error) {
	resource := named(bucket, name)
	described, err := json.Marshal(description{Name: name, ContentType: options.ContentType, Metadata: options.Metadata})
	if err != nil {
		return "", fmt.Errorf("gcs insert %s: %w", resource, err)
	}
	query := url.Values{"uploadType": {"resumable"}}
	options.Precondition.apply(query)
	header := http.Header{
		"Content-Type":          {"application/json; charset=UTF-8"},
		"X-Upload-Content-Type": {mediaType(options)},
	}
	if options.Size >= 0 {
		// Stated, the service holds the upload to it: an upload that completes at
		// any other length is refused.
		header.Set("X-Upload-Content-Length", strconv.FormatInt(options.Size, 10))
	}
	response, err := c.send(ctx, "insert", resource, request{
		method: http.MethodPost,
		url:    c.endpoint + "/upload/storage/v1/b/" + escape(bucket) + "/o?" + query.Encode(),
		header: header,
		body:   bytes.NewReader(described),
		length: int64(len(described)),
	})
	if err != nil {
		return "", err
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		return "", refusal("insert", resource, response.StatusCode, response.Body)
	}
	session := response.Header.Get("Location")
	if session == "" {
		return "", fmt.Errorf("gcs insert %s: the upload was initiated without a session URI", resource)
	}
	return session, nil
}

// sendChunks reads the body a chunk at a time and sends each as it is read. One
// byte is read past every full chunk before it is sent, because the last chunk
// must say so — its Content-Range carries the total, where the others carry "*"
// or the size declared — and the only way to know a full chunk is the last is
// to find nothing after it.
func (c *Client) sendChunks(ctx context.Context, resource, session string, body io.Reader, size int64) (Object, error) {
	chunk := make([]byte, c.chunkBytes)
	var offset int64
	carried := 0
	for {
		filled, err := io.ReadFull(body, chunk[carried:])
		filled += carried
		last := errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF)
		if err != nil && !last {
			return Object{}, fmt.Errorf("gcs insert %s: reading the body after %d bytes: %w", resource, offset+int64(carried), err)
		}
		var next [1]byte
		if !last {
			switch _, err := io.ReadFull(body, next[:]); {
			case errors.Is(err, io.EOF):
				last = true
			case err != nil:
				return Object{}, fmt.Errorf("gcs insert %s: reading the body after %d bytes: %w", resource, offset+int64(filled), err)
			}
		}

		end := offset + int64(filled)
		if size >= 0 && ((last && end != size) || (!last && end >= size)) {
			return Object{}, fmt.Errorf("gcs insert %s: the body is not the %d bytes it was said to be", resource, size)
		}
		total := "*"
		if size >= 0 || last {
			total = strconv.FormatInt(max(size, end), 10)
		}
		// A range cannot be empty, so an upload with nothing left to send says
		// only what the total is.
		contentRange := fmt.Sprintf("bytes */%s", total)
		if filled > 0 {
			contentRange = fmt.Sprintf("bytes %d-%d/%s", offset, end-1, total)
		}
		object, err := c.sendChunk(ctx, resource, session, chunk[:filled], contentRange, end, last)
		if err != nil || last {
			return object, err
		}
		chunk[0], carried, offset = next[0], 1, end
	}
}

// sendChunk sends one chunk. The service answers the last with the object and
// every other with 308 and a Range header saying how much it now holds.
func (c *Client) sendChunk(ctx context.Context, resource, session string, chunk []byte, contentRange string, end int64, last bool) (Object, error) {
	response, err := c.send(ctx, "insert", resource, request{
		method: http.MethodPut,
		url:    session,
		header: http.Header{"Content-Range": {contentRange}},
		body:   bytes.NewReader(chunk),
		length: int64(len(chunk)),
	})
	if err != nil {
		return Object{}, err
	}
	defer func() { _ = response.Body.Close() }()
	if last {
		if response.StatusCode != http.StatusOK && response.StatusCode != http.StatusCreated {
			return Object{}, refusal("insert", resource, response.StatusCode, response.Body)
		}
		var answer objectResource
		if err := json.NewDecoder(response.Body).Decode(&answer); err != nil {
			return Object{}, fmt.Errorf("gcs insert %s: reading the answer: %w", resource, err)
		}
		return written(resource, answer)
	}
	if response.StatusCode != http.StatusPermanentRedirect {
		return Object{}, refusal("insert", resource, response.StatusCode, response.Body)
	}
	// The service may keep less of a chunk than it was sent, and says how much in
	// Range. Sending the rest again would be a retry, which this client leaves to
	// its caller, so keeping less is an error like any other.
	if held := response.Header.Get("Range"); held != fmt.Sprintf("bytes=0-%d", end-1) {
		return Object{}, fmt.Errorf("gcs insert %s: after %d bytes were sent the service holds %q", resource, end, held)
	}
	return Object{}, nil
}

// cancel ends an upload session, which the service confirms with 499.
// https://docs.cloud.google.com/storage/docs/performing-resumable-uploads#cancel-upload
func (c *Client) cancel(ctx context.Context, resource, session string) error {
	response, err := c.send(ctx, "cancel the upload of", resource, request{method: http.MethodDelete, url: session, statedEmpty: true})
	if err != nil {
		return err
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != 499 {
		return refusal("cancel the upload of", resource, response.StatusCode, response.Body)
	}
	return nil
}
