package gcsclient

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// metadataHeader prefixes each item of an object's custom metadata in the
// headers of a download.
const metadataHeader = "X-Goog-Meta-"

// listFields asks a listing for the facts Object holds and no more: a page is a
// thousand objects, and most of what the service would say of each is unread.
const listFields = "items(name,size,generation,updated),prefixes,nextPageToken"

// objectFields is the same economy for an answer that is one object.
const objectFields = "bucket,name,size,generation,updated,metadata"

// Get reads a whole object. The caller closes the body.
func (c *Client) Get(ctx context.Context, bucket, name string) (io.ReadCloser, Object, error) {
	return c.download(ctx, bucket, name, "")
}

// GetRange reads length bytes from offset, or as many as the object has from
// there; a range that starts at or past the end is ErrRangeNotSatisfiable. The
// Object it returns carries the whole object's size. The caller closes the body.
func (c *Client) GetRange(ctx context.Context, bucket, name string, offset, length int64) (io.ReadCloser, Object, error) {
	if offset < 0 || length <= 0 {
		return nil, Object{}, fmt.Errorf("gcs get %s: invalid range %d+%d", named(bucket, name), offset, length)
	}
	return c.download(ctx, bucket, name, fmt.Sprintf("bytes=%d-%d", offset, offset+length-1))
}

// download is the one request of this client that is not the JSON API's. The
// JSON API's download (alt=media) documents no response headers at all, and an
// object's generation, modification time and metadata would each cost a second
// request and a race between the two. The XML API's GET Object documents them
// as headers of the download itself — x-goog-generation, Last-Modified,
// x-goog-meta-* — takes the same bearer token, and is what a signed URL
// addresses in any case.
// https://docs.cloud.google.com/storage/docs/xml-api/get-object-download
// https://docs.cloud.google.com/storage/docs/xml-api/reference-headers
func (c *Client) download(ctx context.Context, bucket, name, byteRange string) (io.ReadCloser, Object, error) {
	what := named(bucket, name)
	// Asking for gzip is asking for the stored bytes untouched: without it the
	// service decompresses an object stored with Content-Encoding: gzip and
	// ignores the range, and with it net/http leaves the body alone too.
	// https://docs.cloud.google.com/storage/docs/transcoding
	header := http.Header{"Accept-Encoding": {"gzip"}}
	expected := http.StatusOK
	if byteRange != "" {
		header.Set("Range", byteRange)
		expected = http.StatusPartialContent
	}
	response, err := c.send(ctx, "get", what, request{
		method: http.MethodGet,
		url:    c.endpoint + "/" + percentEncode(bucket, false) + "/" + percentEncode(name, true),
		header: header,
	})
	if err != nil {
		return nil, Object{}, err
	}
	if response.StatusCode != expected {
		defer func() { _ = response.Body.Close() }()
		return nil, Object{}, refusal("get", what, response.StatusCode, response.Body)
	}
	described, err := describeDownload(bucket, name, response)
	if err != nil {
		_ = response.Body.Close()
		return nil, Object{}, fmt.Errorf("gcs get %s: %w", what, err)
	}
	return response.Body, described, nil
}

// describeDownload reads an Object out of a download's headers, and refuses a
// response without one of them rather than report a zero in its place.
func describeDownload(bucket, name string, response *http.Response) (Object, error) {
	generation, err := strconv.ParseInt(response.Header.Get("X-Goog-Generation"), 10, 64)
	if err != nil || generation <= 0 {
		return Object{}, fmt.Errorf("the download came with x-goog-generation %q", response.Header.Get("X-Goog-Generation"))
	}
	updated, err := http.ParseTime(response.Header.Get("Last-Modified"))
	if err != nil {
		return Object{}, fmt.Errorf("the download came with Last-Modified %q", response.Header.Get("Last-Modified"))
	}
	size := response.ContentLength
	if response.StatusCode == http.StatusPartialContent {
		// Content-Length is the length of the range; the object's own is the total
		// in "bytes first-last/total".
		_, total, _ := strings.Cut(response.Header.Get("Content-Range"), "/")
		if size, err = strconv.ParseInt(total, 10, 64); err != nil {
			return Object{}, fmt.Errorf("the ranged download came with Content-Range %q", response.Header.Get("Content-Range"))
		}
	}
	if size < 0 {
		return Object{}, errors.New("the download came without a Content-Length")
	}
	described := Object{Bucket: bucket, Name: name, Size: size, Generation: generation, Updated: updated}
	for header, values := range response.Header {
		if item, ok := strings.CutPrefix(header, metadataHeader); ok {
			if described.Metadata == nil {
				described.Metadata = map[string]string{}
			}
			described.Metadata[strings.ToLower(item)] = values[0]
		}
	}
	return described, nil
}

// Stat describes an object without reading it.
// https://docs.cloud.google.com/storage/docs/json_api/v1/objects/get
func (c *Client) Stat(ctx context.Context, bucket, name string) (Object, error) {
	var answer objectResource
	err := c.sendJSON(ctx, "stat", named(bucket, name), request{
		method: http.MethodGet,
		url:    c.objectURL(bucket, name) + "?alt=json&fields=" + url.QueryEscape(objectFields),
	}, &answer)
	if err != nil {
		return Object{}, err
	}
	described, err := answer.object()
	if err != nil {
		return Object{}, fmt.Errorf("gcs stat %s: %w", named(bucket, name), err)
	}
	return described, nil
}

// Delete removes an object's live generation. Deleting an object that is not
// there is ErrNotFound. The documentation promises "an empty response body" and
// names no status, so either of the two that mean success with nothing to say
// is taken for it.
// https://docs.cloud.google.com/storage/docs/json_api/v1/objects/delete
func (c *Client) Delete(ctx context.Context, bucket, name string) error {
	response, err := c.send(ctx, "delete", named(bucket, name), request{method: http.MethodDelete, url: c.objectURL(bucket, name)})
	if err != nil {
		return err
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusNoContent && response.StatusCode != http.StatusOK {
		return refusal("delete", named(bucket, name), response.StatusCode, response.Body)
	}
	return nil
}

// ListOptions select what a listing holds.
type ListOptions struct {
	// Prefix restricts the listing to names that begin with it.
	Prefix string
	// Delimiter, when not empty, folds every name that has it after the prefix
	// into one entry for the prefix up to and including it, as a directory.
	Delimiter string
}

// Entry is one result of a listing: an object, or a prefix that stands for every
// object below it.
type Entry struct {
	// Object is the object, without its metadata; of a prefix it holds only the
	// Name, which ends in the delimiter.
	Object
	// Prefix marks a folded prefix.
	Prefix bool
}

// listPage is one page of objects.list.
type listPage struct {
	Items         []objectResource `json:"items"`
	Prefixes      []string         `json:"prefixes"`
	NextPageToken string           `json:"nextPageToken"`
}

// List calls visit for everything the options select, through every page, in
// byte order of the names, and stops at the first error visit returns.
//
// The service sends a page's objects and its prefixes as two lists, each in
// order; here they are merged back into one. That is sound page by page because
// a page is a contiguous run of the whole ordered listing.
// https://docs.cloud.google.com/storage/docs/json_api/v1/objects/list
func (c *Client) List(ctx context.Context, bucket string, options ListOptions, visit func(Entry) error) error {
	query := url.Values{"fields": {listFields}}
	if options.Prefix != "" {
		query.Set("prefix", options.Prefix)
	}
	if options.Delimiter != "" {
		query.Set("delimiter", options.Delimiter)
	}
	for {
		var page listPage
		err := c.sendJSON(ctx, "list", named(bucket, options.Prefix), request{
			method: http.MethodGet,
			url:    c.endpoint + "/storage/v1/b/" + escape(bucket) + "/o?" + query.Encode(),
		}, &page)
		if err != nil {
			return err
		}
		items, prefixes := page.Items, page.Prefixes
		for len(items) > 0 || len(prefixes) > 0 {
			var entry Entry
			if len(prefixes) == 0 || (len(items) > 0 && items[0].Name < prefixes[0]) {
				listed, err := items[0].object()
				if err != nil {
					return fmt.Errorf("gcs list %s: %w", named(bucket, options.Prefix), err)
				}
				listed.Bucket = bucket
				entry, items = Entry{Object: listed}, items[1:]
			} else {
				entry, prefixes = Entry{Object: Object{Bucket: bucket, Name: prefixes[0]}, Prefix: true}, prefixes[1:]
			}
			if err := visit(entry); err != nil {
				return err
			}
		}
		if page.NextPageToken == "" {
			return nil
		}
		query.Set("pageToken", page.NextPageToken)
	}
}

// rewriteAnswer is what one call of objects.rewrite says.
type rewriteAnswer struct {
	Done         bool           `json:"done"`
	RewriteToken string         `json:"rewriteToken"`
	Resource     objectResource `json:"resource"`
}

// Copy copies an object, content and metadata, without the bytes passing
// through the caller, and returns the copy. It is objects.rewrite and not
// objects.copy because a rewrite has no limit: the service copies as much as it
// can within one call's deadline and hands back a token to go on from, so a
// large object takes several calls, made here until the service says it is done.
// https://docs.cloud.google.com/storage/docs/json_api/v1/objects/rewrite
func (c *Client) Copy(ctx context.Context, sourceBucket, sourceName, destinationBucket, destinationName string) (Object, error) {
	what := named(sourceBucket, sourceName) + " to " + named(destinationBucket, destinationName)
	target := c.objectURL(sourceBucket, sourceName) + "/rewriteTo/b/" + escape(destinationBucket) + "/o/" + escape(destinationName)
	query := url.Values{"fields": {"done,rewriteToken,resource(" + objectFields + ")"}}
	for {
		var answer rewriteAnswer
		// The body is empty, which is how the destination is given the source's
		// metadata rather than a description of its own.
		if err := c.sendJSON(ctx, "copy", what, request{method: http.MethodPost, url: target + "?" + query.Encode()}, &answer); err != nil {
			return Object{}, err
		}
		if answer.Done {
			copied, err := answer.Resource.object()
			if err != nil {
				return Object{}, fmt.Errorf("gcs copy %s: %w", what, err)
			}
			return copied, nil
		}
		if answer.RewriteToken == "" {
			return Object{}, fmt.Errorf("gcs copy %s: the service said the copy was unfinished and gave no token to go on with", what)
		}
		query.Set("rewriteToken", answer.RewriteToken)
	}
}
