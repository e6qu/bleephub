package gcsclient

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"strconv"
	"strings"
)

// BatchLimit is the most calls the service takes in one batch request.
// https://docs.cloud.google.com/storage/docs/batch
const BatchLimit = 100

// DeleteBatch deletes the objects named, a hundred to a request, and returns
// what became of each: outcomes[i] is nil where names[i] was deleted, and
// otherwise the service's refusal of that one call — ErrNotFound for an object
// that was not there. The error returned beside them is a batch that failed as
// a whole, and the outcomes of names in batches not yet sent are then that same
// error.
//
// The outcomes have to be read: the batch request itself succeeds whatever
// became of the calls inside it.
func (c *Client) DeleteBatch(ctx context.Context, bucket string, names []string) ([]error, error) {
	outcomes := make([]error, len(names))
	for start := 0; start < len(names); start += BatchLimit {
		end := min(start+BatchLimit, len(names))
		if err := c.deleteBatch(ctx, bucket, names[start:end], outcomes[start:end]); err != nil {
			for i := start; i < len(names); i++ {
				outcomes[i] = err
			}
			return outcomes, err
		}
	}
	return outcomes, nil
}

// deleteBatch sends one batch: a multipart/mixed body whose every part is a
// whole HTTP request, answered by a multipart/mixed body whose every part is a
// whole HTTP response. The batch's own headers, the access token among them,
// apply to each call inside it. A part's Content-ID comes back with "response-"
// before it, and it is by that, not by position, that an answer is matched to
// its call, because "the server may perform your requests in any order".
func (c *Client) deleteBatch(ctx context.Context, bucket string, names []string, outcomes []error) error {
	what := fmt.Sprintf("%d objects of %s", len(names), bucket)
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	for i, name := range names {
		part, err := writer.CreatePart(textproto.MIMEHeader{
			"Content-Type":              {"application/http"},
			"Content-Transfer-Encoding": {"binary"},
			"Content-ID":                {contentID(i)},
		})
		if err != nil {
			return fmt.Errorf("gcs delete %s: %w", what, err)
		}
		// The call is addressed by path alone; the batch's own host is its host.
		if _, err := fmt.Fprintf(part, "DELETE /storage/v1/b/%s/o/%s HTTP/1.1\r\n\r\n", escape(bucket), escape(name)); err != nil {
			return fmt.Errorf("gcs delete %s: %w", what, err)
		}
	}
	if err := writer.Close(); err != nil {
		return fmt.Errorf("gcs delete %s: %w", what, err)
	}

	response, err := c.send(ctx, "delete", what, request{
		method: http.MethodPost,
		url:    c.endpoint + "/batch/storage/v1",
		header: http.Header{"Content-Type": {"multipart/mixed; boundary=" + writer.Boundary()}},
		body:   &body,
		length: int64(body.Len()),
	})
	if err != nil {
		return err
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		return refusal("delete", what, response.StatusCode, response.Body)
	}
	_, parameters, err := mime.ParseMediaType(response.Header.Get("Content-Type"))
	if err != nil || parameters["boundary"] == "" {
		return fmt.Errorf("gcs delete %s: the batch was answered as %q, not multipart with a boundary", what, response.Header.Get("Content-Type"))
	}

	answered := make([]bool, len(names))
	parts := multipart.NewReader(response.Body, parameters["boundary"])
	for {
		part, err := parts.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("gcs delete %s: reading the batch's answer: %w", what, err)
		}
		index, err := indexOf(part.Header.Get("Content-ID"), len(names))
		if err != nil {
			return fmt.Errorf("gcs delete %s: %w", what, err)
		}
		answer, err := http.ReadResponse(bufio.NewReader(part), nil)
		if err != nil {
			return fmt.Errorf("gcs delete %s: reading the answer to one call: %w", named(bucket, names[index]), err)
		}
		if answer.StatusCode != http.StatusNoContent && answer.StatusCode != http.StatusOK {
			outcomes[index] = refusal("delete", named(bucket, names[index]), answer.StatusCode, answer.Body)
		}
		_ = answer.Body.Close()
		answered[index] = true
	}
	for i, done := range answered {
		if !done {
			return fmt.Errorf("gcs delete %s: the batch's answer says nothing of %s", what, names[i])
		}
	}
	return nil
}

// contentIDPrefix begins the Content-ID of every call; the rest is the call's
// position in its batch.
const contentIDPrefix = "<delete+"

func contentID(index int) string { return contentIDPrefix + strconv.Itoa(index) + ">" }

// indexOf reads a call's position back out of the Content-ID of its answer.
func indexOf(responseID string, calls int) (int, error) {
	position, ok := strings.CutPrefix(responseID, "<response-"+strings.TrimPrefix(contentIDPrefix, "<"))
	index, err := strconv.Atoi(strings.TrimSuffix(position, ">"))
	if !ok || err != nil || index < 0 || index >= calls {
		return 0, fmt.Errorf("the batch's answer holds a part with Content-ID %q, which answers no call made", responseID)
	}
	return index, nil
}
