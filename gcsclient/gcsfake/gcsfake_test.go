package gcsfake_test

import (
	"bytes"
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/oauth2"
	"golang.org/x/oauth2/jwt"

	"github.com/e6qu/bleephub/gcsclient/gcsfake"
)

const (
	held      = "held"
	quantum   = 256 << 10
	readWrite = "https://www.googleapis.com/auth/devstorage.read_write"
)

// service is a fake and an HTTP client that carries a token the fake issued:
// the tests here speak the protocol by hand, so that what they hold the fake to
// is Google's documentation and not gcsclient's reading of it.
type service struct {
	*gcsfake.Server
	authorized *http.Client
}

func newService(t *testing.T) service {
	t.Helper()
	server := gcsfake.New()
	t.Cleanup(server.Close)
	server.CreateBucket(held)
	return service{server, authorizedFor(t, server.CredentialsJSON(), readWrite)}
}

// authorizedFor returns a client that exchanges the key file for a token of the
// scope given. It follows no redirect, since a resumable upload's 308 is not
// one.
func authorizedFor(t *testing.T, credentials []byte, scope string) *http.Client {
	t.Helper()
	config := jwtConfig(t, credentials, scope)
	return &http.Client{
		Transport:     &oauth2.Transport{Source: config.TokenSource(context.Background())},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

// jwtConfig reads a key file into the OAuth2 flow a service account uses.
func jwtConfig(t *testing.T, credentials []byte, scope string) *jwt.Config {
	t.Helper()
	var file struct {
		ClientEmail  string `json:"client_email"`
		PrivateKeyID string `json:"private_key_id"`
		PrivateKey   string `json:"private_key"`
		TokenURI     string `json:"token_uri"`
	}
	if err := json.Unmarshal(credentials, &file); err != nil {
		t.Fatalf("the key file: %v", err)
	}
	return &jwt.Config{Email: file.ClientEmail, PrivateKey: []byte(file.PrivateKey), PrivateKeyID: file.PrivateKeyID, Scopes: []string{scope}, TokenURL: file.TokenURI}
}

// plain is a client with no credentials, which follows no redirect, since a
// resumable upload's 308 is not one.
var plain = &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}

// answer is a response read to its end.
type answer struct {
	status int
	header http.Header
	body   string
}

// reason is the JSON API's error reason, or the XML API's error code.
func (a answer) reason() string {
	var envelope struct {
		Error struct {
			Errors []struct {
				Reason string `json:"reason"`
			} `json:"errors"`
		} `json:"error"`
	}
	if json.Unmarshal([]byte(a.body), &envelope) == nil && len(envelope.Error.Errors) > 0 {
		return envelope.Error.Errors[0].Reason
	}
	_, after, _ := strings.Cut(a.body, "<Code>")
	code, _, _ := strings.Cut(after, "</Code>")
	return code
}

func (a answer) json(t *testing.T) map[string]any {
	t.Helper()
	var decoded map[string]any
	if err := json.Unmarshal([]byte(a.body), &decoded); err != nil {
		t.Fatalf("the answer %d %q is not JSON: %v", a.status, a.body, err)
	}
	return decoded
}

func do(t *testing.T, client *http.Client, method, target string, header http.Header, body []byte) answer {
	t.Helper()
	request, err := http.NewRequest(method, target, bytes.NewReader(body))
	if err != nil {
		t.Fatalf("%s %s: %v", method, target, err)
	}
	for name, values := range header {
		request.Header[name] = values
	}
	response, err := client.Do(request)
	if err != nil {
		t.Fatalf("%s %s: %v", method, target, err)
	}
	content, _ := io.ReadAll(response.Body)
	_ = response.Body.Close()
	return answer{response.StatusCode, response.Header, string(content)}
}

// multipartUpload is objects.insert with uploadType=multipart, by hand.
func (s service) multipartUpload(t *testing.T, client *http.Client, name, content, query string) answer {
	t.Helper()
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	description, _ := writer.CreatePart(textproto.MIMEHeader{"Content-Type": {"application/json; charset=UTF-8"}})
	_, _ = fmt.Fprintf(description, `{"name":%q,"metadata":{"kept":"beside"}}`, name)
	media, _ := writer.CreatePart(textproto.MIMEHeader{"Content-Type": {"text/plain"}})
	_, _ = io.WriteString(media, content)
	_ = writer.Close()
	return do(t, client, http.MethodPost, s.URL()+"/upload/storage/v1/b/"+held+"/o?uploadType=multipart"+query,
		http.Header{"Content-Type": {"multipart/related; boundary=" + writer.Boundary()}}, body.Bytes())
}

// initiate opens a resumable upload by hand and returns its session URI.
func (s service) initiate(t *testing.T, name, query string, header http.Header) string {
	t.Helper()
	if header == nil {
		header = http.Header{}
	}
	header.Set("Content-Type", "application/json; charset=UTF-8")
	opened := do(t, s.authorized, http.MethodPost, s.URL()+"/upload/storage/v1/b/"+held+"/o?uploadType=resumable"+query, header, fmt.Appendf(nil, `{"name":%q}`, name))
	if opened.status != http.StatusOK || opened.header.Get("Location") == "" {
		t.Fatalf("initiate an upload of %s: %d %s", name, opened.status, opened.body)
	}
	return opened.header.Get("Location")
}

func chunk(t *testing.T, session, contentRange string, body []byte) answer {
	t.Helper()
	return do(t, plain, http.MethodPut, session, http.Header{"Content-Range": {contentRange}}, body)
}

// TestARequestNeedsATokenThisServerIssuedForAScopeThatWrites pins that the fake
// is not an open door: a client that sent no token, a made-up one, one from
// another server or one for too small a scope would work against a fake that
// did not look, and fail against the service.
func TestARequestNeedsATokenThisServerIssuedForAScopeThatWrites(t *testing.T) {
	s := newService(t)
	s.Put(held, "object", []byte("content"), nil)
	targets := map[string]string{
		"objects.get":  s.URL() + "/storage/v1/b/" + held + "/o/object",
		"objects.list": s.URL() + "/storage/v1/b/" + held + "/o",
		"a download":   s.URL() + "/" + held + "/object",
	}
	for name, target := range targets {
		if got := do(t, s.authorized, http.MethodGet, target, nil, nil); got.status != http.StatusOK {
			t.Fatalf("PREMISE: %s with a good token answered %d %s", name, got.status, got.body)
		}
	}

	other := gcsfake.New()
	t.Cleanup(other.Close)
	elsewhere := authorizedFor(t, other.CredentialsJSON(), readWrite)
	if got := do(t, elsewhere, http.MethodGet, other.URL()+"/storage/v1/b/"+held+"/o", nil, nil); got.status != http.StatusNotFound {
		t.Fatalf("PREMISE: the other server's token is good at the other server: %d %s", got.status, got.body)
	}
	readOnly := authorizedFor(t, s.CredentialsJSON(), "https://www.googleapis.com/auth/devstorage.read_only")
	for name, target := range targets {
		for who, want := range map[string]struct {
			client *http.Client
			header http.Header
			status int
		}{
			"nobody":                     {plain, nil, http.StatusUnauthorized},
			"a made-up token":            {plain, http.Header{"Authorization": {"Bearer made-up"}}, http.StatusUnauthorized},
			"a token that is not bearer": {plain, http.Header{"Authorization": {"Basic dXNlcjpwYXNz"}}, http.StatusUnauthorized},
			"another server's token":     {elsewhere, nil, http.StatusUnauthorized},
			"a token of too small scope": {readOnly, nil, http.StatusForbidden},
		} {
			if got := do(t, want.client, http.MethodGet, target, want.header, nil); got.status != want.status {
				t.Errorf("%s as %s answered %d %s, want %d", name, who, got.status, got.body, want.status)
			}
		}
	}
	if got := s.multipartUpload(t, plain, "anonymous", "x", ""); got.status != http.StatusUnauthorized {
		t.Errorf("an upload as nobody answered %d", got.status)
	}
	if got := do(t, plain, http.MethodPost, s.URL()+"/batch/storage/v1", http.Header{"Content-Type": {"multipart/mixed; boundary=x"}}, []byte("--x--\r\n")); got.status != http.StatusUnauthorized {
		t.Errorf("a batch as nobody answered %d", got.status)
	}
}

// TestATokenIsIssuedOnlyForAnAssertionTheAccountSigned pins the token endpoint:
// a key file with another key in it, or one that asks another endpoint's
// audience, gets no token — which is what makes "the client authenticated"
// mean something when a test passes.
func TestATokenIsIssuedOnlyForAnAssertionTheAccountSigned(t *testing.T) {
	s := newService(t)
	edited := func(edit func(map[string]any)) []byte {
		var file map[string]any
		if err := json.Unmarshal(s.CredentialsJSON(), &file); err != nil {
			t.Fatalf("the key file: %v", err)
		}
		edit(file)
		encoded, _ := json.Marshal(file)
		return encoded
	}
	token := func(credentials []byte) error {
		_, err := jwtConfig(t, credentials, readWrite).TokenSource(context.Background()).Token()
		return err
	}
	if err := token(s.CredentialsJSON()); err != nil {
		t.Fatalf("PREMISE: the key file as issued gets a token: %v", err)
	}
	stranger, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate a key: %v", err)
	}
	der, _ := x509.MarshalPKCS8PrivateKey(stranger)
	for name, credentials := range map[string][]byte{
		"another private key": edited(func(file map[string]any) {
			file["private_key"] = string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}))
		}),
		"another key's ID":        edited(func(file map[string]any) { file["private_key_id"] = "0123456789abcdef" }),
		"another account's email": edited(func(file map[string]any) { file["client_email"] = "someone@else.iam.gserviceaccount.com" }),
	} {
		if err := token(credentials); err == nil {
			t.Errorf("%s: a token was issued", name)
		}
	}
	for name, form := range map[string]url.Values{
		"no assertion":       {"grant_type": {"urn:ietf:params:oauth:grant-type:jwt-bearer"}},
		"not a JWT":          {"grant_type": {"urn:ietf:params:oauth:grant-type:jwt-bearer"}, "assertion": {"a.b"}},
		"another grant type": {"grant_type": {"client_credentials"}},
	} {
		if got := do(t, plain, http.MethodPost, s.URL()+"/token", http.Header{"Content-Type": {"application/x-www-form-urlencoded"}}, []byte(form.Encode())); got.status != http.StatusBadRequest {
			t.Errorf("%s answered %d %s", name, got.status, got.body)
		}
	}
}

// TestAPreconditionIsHeldOnEveryWayOfWriting pins ifGenerationMatch on a
// multipart upload and at the completion of a resumable one, with the 412 the
// service answers. A client's compare-and-swap is made of these, and a fake that
// let every contender win would pass a client that never sent the condition.
func TestAPreconditionIsHeldOnEveryWayOfWriting(t *testing.T) {
	s := newService(t)
	created := s.multipartUpload(t, s.authorized, "ref", "one", "&ifGenerationMatch=0")
	if created.status != http.StatusOK {
		t.Fatalf("create: %d %s", created.status, created.body)
	}
	generation := created.json(t)["generation"].(string)
	stale, _ := strconv.ParseInt(generation, 10, 64)
	for name, query := range map[string]string{
		"a second create":        "&ifGenerationMatch=0",
		"a swap at another":      "&ifGenerationMatch=" + strconv.FormatInt(stale+1, 10),
		"a swap at one long ago": "&ifGenerationMatch=1",
	} {
		if got := s.multipartUpload(t, s.authorized, "ref", "usurper", query); got.status != http.StatusPreconditionFailed || got.reason() != "conditionNotMet" {
			t.Errorf("%s answered %d %s, want 412 conditionNotMet", name, got.status, got.body)
		}
	}
	if got := s.multipartUpload(t, s.authorized, "never-written", "x", "&ifGenerationMatch="+generation); got.status != http.StatusPreconditionFailed {
		t.Errorf("a swap of an object that does not exist answered %d %s, want 412", got.status, got.body)
	}
	if got := s.multipartUpload(t, s.authorized, "ref", "x", "&ifGenerationMatch=soon"); got.status != http.StatusBadRequest {
		t.Errorf("a precondition that is not a number answered %d", got.status)
	}
	if content, _ := s.Contents(held, "ref"); string(content) != "one" {
		t.Fatalf("refused writes left %q", content)
	}

	// A resumable upload is let begin whatever its precondition — there is
	// nothing yet to refuse — and is held to it when it completes.
	session := s.initiate(t, "ref", "&ifGenerationMatch=0", nil)
	if got := chunk(t, session, "bytes 0-6/7", []byte("usurper")); got.status != http.StatusPreconditionFailed {
		t.Errorf("a resumable create of what exists completed with %d %s, want 412", got.status, got.body)
	}
	session = s.initiate(t, "ref", "&ifGenerationMatch="+generation, nil)
	swapped := chunk(t, session, "bytes 0-2/3", []byte("two"))
	if swapped.status != http.StatusOK || swapped.json(t)["generation"] == generation {
		t.Errorf("a resumable swap at the generation held answered %d %s", swapped.status, swapped.body)
	}
	if content, _ := s.Contents(held, "ref"); string(content) != "two" {
		t.Fatalf("after the swap the object holds %q", content)
	}
	if s.UploadsInProgress() != 0 {
		t.Errorf("%d uploads are still in progress", s.UploadsInProgress())
	}
}

// TestAResumableUploadTakesItsChunksInOrderAndInQuanta pins the rules of a
// chunked upload the documentation states — every chunk but the last a multiple
// of 256 KiB, each starting where the last ended, the total as declared — and
// that nothing is an object until the last chunk. A client that broke one of
// them would be corrupting or losing uploads on the service.
func TestAResumableUploadTakesItsChunksInOrderAndInQuanta(t *testing.T) {
	s := newService(t)
	body := bytes.Repeat([]byte("0123456789abcdef"), (2*quantum+4096)/16)
	total := strconv.Itoa(len(body))

	session := s.initiate(t, "pack", "", http.Header{"X-Upload-Content-Length": {total}})
	first := chunk(t, session, fmt.Sprintf("bytes 0-%d/%s", quantum-1, total), body[:quantum])
	if first.status != http.StatusPermanentRedirect || first.header.Get("Range") != fmt.Sprintf("bytes=0-%d", quantum-1) {
		t.Fatalf("the first chunk answered %d with Range %q, want 308 and the bytes held", first.status, first.header.Get("Range"))
	}
	if _, ok := s.Contents(held, "pack"); ok {
		t.Fatal("an upload in progress is already an object")
	}
	for name, attempt := range map[string]struct {
		contentRange string
		body         []byte
	}{
		"a chunk sent twice":               {fmt.Sprintf("bytes 0-%d/%s", quantum-1, total), body[:quantum]},
		"a chunk that skips ahead":         {fmt.Sprintf("bytes %d-%d/%s", 2*quantum, len(body)-1, total), body[2*quantum:]},
		"a chunk that is not of 256 KiB":   {fmt.Sprintf("bytes %d-%d/%s", quantum, quantum+999, total), body[quantum : quantum+1000]},
		"a range that is not the body's":   {fmt.Sprintf("bytes %d-%d/%s", quantum, 2*quantum-1, total), body[quantum : quantum+5]},
		"a total other than the one begun": {fmt.Sprintf("bytes %d-%d/%d", quantum, 2*quantum-1, len(body)+1), body[quantum : 2*quantum]},
		"a range past its own total":       {fmt.Sprintf("bytes %d-%d/%s", quantum, len(body), total), body[quantum-1:]},
		"a range in another unit":          {"chunks 1-2/3", nil},
		"no Content-Range":                 {"", body[quantum : 2*quantum]},
	} {
		if got := chunk(t, session, attempt.contentRange, attempt.body); got.status != http.StatusBadRequest {
			t.Errorf("%s answered %d %s, want 400", name, got.status, got.body)
		}
	}
	if asked := chunk(t, session, "bytes */"+total, nil); asked.status != http.StatusPermanentRedirect || asked.header.Get("Range") != fmt.Sprintf("bytes=0-%d", quantum-1) {
		t.Errorf("after the refused chunks the upload holds %q (%d), want the first chunk alone", asked.header.Get("Range"), asked.status)
	}
	if got := chunk(t, session, fmt.Sprintf("bytes %d-%d/*", quantum, 2*quantum-1), body[quantum:2*quantum]); got.status != http.StatusPermanentRedirect {
		t.Fatalf("the second chunk, its total not yet said, answered %d %s", got.status, got.body)
	}
	last := chunk(t, session, fmt.Sprintf("bytes %d-%d/%s", 2*quantum, len(body)-1, total), body[2*quantum:])
	if last.status != http.StatusOK || last.json(t)["size"] != total {
		t.Fatalf("the last chunk answered %d %s", last.status, last.body)
	}
	if stored, _ := s.Contents(held, "pack"); !bytes.Equal(stored, body) {
		t.Fatalf("the object is %d bytes that are not the %d sent", len(stored), len(body))
	}
	if got := chunk(t, session, "bytes */"+total, nil); got.status != http.StatusNotFound {
		t.Errorf("a session that completed answered %d to another request", got.status)
	}

	// A stream of unknown length says so with "*", and ends by saying its total.
	streamed := s.initiate(t, "streamed", "", nil)
	if got := chunk(t, streamed, fmt.Sprintf("bytes 0-%d/*", quantum-1), body[:quantum]); got.status != http.StatusPermanentRedirect {
		t.Fatalf("a chunk of a stream answered %d %s", got.status, got.body)
	}
	if got := chunk(t, streamed, fmt.Sprintf("bytes */%d", quantum), nil); got.status != http.StatusOK {
		t.Fatalf("ending a stream at what it holds answered %d %s", got.status, got.body)
	}
	empty := s.initiate(t, "empty", "", nil)
	if got := chunk(t, empty, "bytes */0", nil); got.status != http.StatusOK || got.json(t)["size"] != "0" {
		t.Fatalf("an upload of nothing answered %d %s", got.status, got.body)
	}
}

// TestACancelledUploadIsGoneAndLeavesNoObject pins the cancellation the
// documentation describes — a DELETE of the session that states an empty body,
// answered 499 — and that a session is its own authorization, as it is on the
// service.
func TestACancelledUploadIsGoneAndLeavesNoObject(t *testing.T) {
	s := newService(t)
	session := s.initiate(t, "abandoned", "", nil)
	if got := chunk(t, session, fmt.Sprintf("bytes 0-%d/*", quantum-1), make([]byte, quantum)); got.status != http.StatusPermanentRedirect {
		t.Fatalf("PREMISE: the upload was in progress: %d %s", got.status, got.body)
	}
	if s.UploadsInProgress() != 1 {
		t.Fatalf("PREMISE: %d uploads in progress, want 1", s.UploadsInProgress())
	}
	unstated, err := http.NewRequest(http.MethodDelete, session, nil)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	response, err := plain.Do(unstated)
	if err != nil {
		t.Fatalf("delete: %v", err)
	}
	_ = response.Body.Close()
	if response.StatusCode != http.StatusLengthRequired || s.UploadsInProgress() != 1 {
		t.Errorf("a DELETE with no Content-Length answered %d and left %d uploads, want 411 and the upload still there", response.StatusCode, s.UploadsInProgress())
	}
	stated, _ := http.NewRequest(http.MethodDelete, session, http.NoBody)
	stated.TransferEncoding = []string{"identity"}
	response, err = plain.Do(stated)
	if err != nil {
		t.Fatalf("delete: %v", err)
	}
	_ = response.Body.Close()
	if response.StatusCode != 499 || s.UploadsInProgress() != 0 {
		t.Fatalf("the cancellation answered %d and left %d uploads, want 499 and none", response.StatusCode, s.UploadsInProgress())
	}
	if got := chunk(t, session, fmt.Sprintf("bytes %d-%d/%d", quantum, quantum+9, quantum+10), make([]byte, 10)); got.status != 499 {
		t.Errorf("a chunk sent to a cancelled upload answered %d, want 499", got.status)
	}
	if names := s.ObjectNames(held); len(names) != 0 {
		t.Errorf("a cancelled upload left %v", names)
	}
}

// batchOf builds a batch request's body out of the calls given, each a request
// line.
func batchOf(calls []string) (string, []byte) {
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	for i, call := range calls {
		part, _ := writer.CreatePart(textproto.MIMEHeader{"Content-Type": {"application/http"}, "Content-ID": {fmt.Sprintf("<call+%d>", i)}})
		_, _ = io.WriteString(part, call+" HTTP/1.1\r\n\r\n")
	}
	_ = writer.Close()
	return "multipart/mixed; boundary=" + writer.Boundary(), body.Bytes()
}

// answersByID reads a batch's answer into the status of each call by its
// Content-ID.
func answersByID(t *testing.T, got answer) (map[string]int, []string) {
	t.Helper()
	_, parameters, err := mime.ParseMediaType(got.header.Get("Content-Type"))
	if err != nil {
		t.Fatalf("the batch answered as %q: %v", got.header.Get("Content-Type"), err)
	}
	statuses, order := map[string]int{}, []string{}
	reader := multipart.NewReader(strings.NewReader(got.body), parameters["boundary"])
	for {
		part, err := reader.NextPart()
		if err == io.EOF {
			return statuses, order
		}
		if err != nil {
			t.Fatalf("the batch's answer: %v", err)
		}
		content, _ := io.ReadAll(part)
		var status int
		if _, err := fmt.Sscanf(string(content), "HTTP/1.1 %d", &status); err != nil || part.Header.Get("Content-Type") != "application/http" {
			t.Fatalf("a part of the batch's answer is %q of type %q", content, part.Header.Get("Content-Type"))
		}
		statuses[part.Header.Get("Content-ID")] = status
		order = append(order, part.Header.Get("Content-ID"))
	}
}

// TestABatchAnswersEachCallByItsIDAndTakesNoMoreThanAHundred pins the batch
// endpoint: 200 for the batch whatever its calls did, each call's own answer
// under "response-" and its Content-ID, the limit of a hundred, and the calls a
// batch may not carry.
func TestABatchAnswersEachCallByItsIDAndTakesNoMoreThanAHundred(t *testing.T) {
	s := newService(t)
	var calls []string
	for i := range 100 {
		name := fmt.Sprintf("objects/%03d", i)
		if i%10 != 3 {
			s.Put(held, name, []byte("x"), nil)
		}
		calls = append(calls, "DELETE /storage/v1/b/"+held+"/o/"+url.PathEscape(name))
	}
	s.Put(held, "kept", []byte("x"), nil)
	contentType, body := batchOf(calls)
	got := do(t, s.authorized, http.MethodPost, s.URL()+"/batch/storage/v1", http.Header{"Content-Type": {contentType}}, body)
	if got.status != http.StatusOK {
		t.Fatalf("a batch of 100 answered %d %s", got.status, got.body)
	}
	statuses, order := answersByID(t, got)
	if len(statuses) != 100 {
		t.Fatalf("a batch of 100 calls was answered in %d parts", len(statuses))
	}
	for i := range 100 {
		want := http.StatusNoContent
		if i%10 == 3 {
			want = http.StatusNotFound
		}
		if status := statuses[fmt.Sprintf("<response-call+%d>", i)]; status != want {
			t.Errorf("call %d was answered %d, want %d", i, status, want)
		}
	}
	if order[0] == "<response-call+0>" {
		t.Error("the answers came in the order of the calls; this fake answers in another, so that a client cannot match them by position")
	}
	if left := s.ObjectNames(held); len(left) != 1 || left[0] != "kept" {
		t.Errorf("the batch left %v, want only the object it did not name", left)
	}

	contentType, body = batchOf(append(calls, "DELETE /storage/v1/b/"+held+"/o/kept"))
	if got := do(t, s.authorized, http.MethodPost, s.URL()+"/batch/storage/v1", http.Header{"Content-Type": {contentType}}, body); got.status != http.StatusBadRequest {
		t.Errorf("a batch of 101 calls answered %d, want 400", got.status)
	}
	if _, ok := s.Contents(held, "kept"); !ok {
		t.Error("a batch that was refused for its size performed its calls")
	}

	contentType, body = batchOf([]string{
		"DELETE " + s.URL() + "/storage/v1/b/" + held + "/o/kept",
		"GET /" + held + "/kept",
		"POST /upload/storage/v1/b/" + held + "/o?uploadType=resumable",
		"GET /storage/v1/b/" + held + "/o/kept",
	})
	statuses, _ = answersByID(t, do(t, s.authorized, http.MethodPost, s.URL()+"/batch/storage/v1", http.Header{"Content-Type": {contentType}}, body))
	for id, want := range map[string]int{"<response-call+0>": 400, "<response-call+1>": 400, "<response-call+2>": 400, "<response-call+3>": 200} {
		if statuses[id] != want {
			t.Errorf("%s was answered %d, want %d: a call names a path, and is neither an upload nor a download", id, statuses[id], want)
		}
	}
	if got := do(t, s.authorized, http.MethodPost, s.URL()+"/batch/storage/v1", http.Header{"Content-Type": {"application/json"}}, []byte("{}")); got.status != http.StatusBadRequest {
		t.Errorf("a batch that is not multipart answered %d", got.status)
	}
}

// TestAListingPagesAtAThousandAndKeepsObjectsAndPrefixesApart pins the shape of
// objects.list: items and prefixes as two arrays, a page of at most a thousand
// entries between them, a token to go on from, and a fold that skips everything
// under a prefix already given — across a page boundary too.
func TestAListingPagesAtAThousandAndKeepsObjectsAndPrefixesApart(t *testing.T) {
	s := newService(t)
	for i := range 700 {
		s.Put(held, fmt.Sprintf("repo/e%04d", i), []byte("x"), nil)
		s.Put(held, fmt.Sprintf("repo/e%04d/below/a", i), []byte("x"), nil)
		s.Put(held, fmt.Sprintf("repo/e%04d/below/b", i), []byte("x"), nil)
	}
	s.Put(held, "repo-sibling", []byte("x"), nil)
	list := func(query url.Values) (items, prefixes []string, next string) {
		got := do(t, s.authorized, http.MethodGet, s.URL()+"/storage/v1/b/"+held+"/o?"+query.Encode(), nil, nil)
		if got.status != http.StatusOK {
			t.Fatalf("list %v: %d %s", query, got.status, got.body)
		}
		var page struct {
			Items []struct {
				Name string `json:"name"`
			} `json:"items"`
			Prefixes      []string `json:"prefixes"`
			NextPageToken string   `json:"nextPageToken"`
		}
		if err := json.Unmarshal([]byte(got.body), &page); err != nil {
			t.Fatalf("list %v: %v", query, err)
		}
		for _, item := range page.Items {
			items = append(items, item.Name)
		}
		return items, page.Prefixes, page.NextPageToken
	}

	items, prefixes, next := list(url.Values{"prefix": {"repo/"}, "delimiter": {"/"}})
	if len(items) != 500 || len(prefixes) != 500 || next == "" || items[499] != "repo/e0499" || prefixes[499] != "repo/e0499/" {
		t.Fatalf("the first page holds %d items and %d prefixes, next %q", len(items), len(prefixes), next)
	}
	items, prefixes, next = list(url.Values{"prefix": {"repo/"}, "delimiter": {"/"}, "pageToken": {next}})
	if len(items) != 200 || len(prefixes) != 200 || next != "" || items[0] != "repo/e0500" || prefixes[0] != "repo/e0500/" {
		t.Fatalf("the second page holds %d items and %d prefixes from %v and %v, next %q", len(items), len(prefixes), items[:1], prefixes[:1], next)
	}

	var all []string
	pages := 0
	for token := ""; ; pages++ {
		query := url.Values{"prefix": {"repo/"}}
		if token != "" {
			query.Set("pageToken", token)
		}
		var items []string
		items, prefixes, token = list(query)
		if len(prefixes) != 0 || len(items) > 1000 {
			t.Fatalf("a page with no delimiter holds %d items and %d prefixes", len(items), len(prefixes))
		}
		all = append(all, items...)
		if token == "" {
			break
		}
	}
	if len(all) != 2100 || pages != 2 {
		t.Errorf("2100 objects were listed as %d over %d pages, want 3 pages", len(all), pages+1)
	}
	if items, _, next := list(url.Values{"maxResults": {"7"}}); len(items) != 7 || next == "" {
		t.Errorf("a page of at most 7 holds %d, next %q", len(items), next)
	}
	if got := do(t, s.authorized, http.MethodGet, s.URL()+"/storage/v1/b/"+held+"/o?pageToken=%21%21", nil, nil); got.status != http.StatusBadRequest {
		t.Errorf("a page token the server never gave answered %d", got.status)
	}
	if got := do(t, s.authorized, http.MethodGet, s.URL()+"/storage/v1/b/never-created/o", nil, nil); got.status != http.StatusNotFound || got.reason() != "notFound" {
		t.Errorf("a listing of a bucket that does not exist answered %d %s", got.status, got.body)
	}
}

// TestWhatTheFakeDoesNotImplementItRefuses pins the fake's honesty: a parameter,
// a field or a method it does not know is refused, never ignored, so a client
// that came to rely on one would fail here rather than pass by being humoured.
// It pins the partial response too: what was not asked for is not sent.
func TestWhatTheFakeDoesNotImplementItRefuses(t *testing.T) {
	s := newService(t)
	s.Put(held, "object", []byte("content"), map[string]string{"kept": "beside"})
	object := s.URL() + "/storage/v1/b/" + held + "/o/object"
	for name, attempt := range map[string]struct{ method, target string }{
		"a parameter not implemented":    {http.MethodGet, object + "?ifGenerationNotMatch=5"},
		"a download by the JSON API":     {http.MethodGet, object + "?alt=media"},
		"a field that is not a field":    {http.MethodGet, object + "?fields=name,generaton"},
		"a selection left open":          {http.MethodGet, s.URL() + "/storage/v1/b/" + held + "/o?fields=items(name"},
		"a conditional delete":           {http.MethodDelete, object + "?ifGenerationMatch=5"},
		"a copy with a byte limit":       {http.MethodPost, object + "/rewriteTo/b/" + held + "/o/copy?maxBytesRewrittenPerCall=1048576"},
		"a rewrite token never given":    {http.MethodPost, object + "/rewriteTo/b/" + held + "/o/copy?rewriteToken=made-up"},
		"an upload of the media alone":   {http.MethodPost, s.URL() + "/upload/storage/v1/b/" + held + "/o?uploadType=media&name=x"},
		"a download with a parameter":    {http.MethodGet, s.URL() + "/" + held + "/object?generation=5"},
		"a name with a bare slash in it": {http.MethodGet, s.URL() + "/storage/v1/b/" + held + "/o/dir/object"},
	} {
		if got := do(t, s.authorized, attempt.method, attempt.target, nil, nil); got.status != http.StatusBadRequest && got.status != http.StatusNotFound {
			t.Errorf("%s answered %d %s, want a refusal", name, got.status, got.body)
		}
	}
	if got := do(t, s.authorized, http.MethodPatch, object, nil, []byte("{}")); got.status != http.StatusNotFound {
		t.Errorf("objects.patch answered %d", got.status)
	}
	if _, ok := s.Contents(held, "object"); !ok {
		t.Error("a refused request deleted the object")
	}

	whole := do(t, s.authorized, http.MethodGet, object, nil, nil).json(t)
	partial := do(t, s.authorized, http.MethodGet, object+"?fields=name,generation,metadata", nil, nil).json(t)
	if len(partial) != 3 || partial["generation"] != whole["generation"] || len(whole) <= 3 {
		t.Errorf("a partial response of three fields is %v, of a whole that is %v", partial, whole)
	}
	listed := do(t, s.authorized, http.MethodGet, s.URL()+"/storage/v1/b/"+held+"/o?fields=items(name,size),nextPageToken", nil, nil).json(t)
	item := listed["items"].([]any)[0].(map[string]any)
	if len(listed) != 1 || len(item) != 2 || item["size"] != "7" {
		t.Errorf("a partial listing is %v", listed)
	}
}

// TestADownloadCarriesTheObjectsFactsAndHoldsARangeToItsSize pins the download
// gcsclient depends on: the generation, the time and the metadata as headers,
// the whole size of a ranged read, 416 for a range that starts at or past the
// end, and — only when switched on — the success with no bytes an emulator may
// answer instead.
func TestADownloadCarriesTheObjectsFactsAndHoldsARangeToItsSize(t *testing.T) {
	s := newService(t)
	generation := s.Put(held, "dir/pack", []byte("0123456789"), map[string]string{"kept": "beside"})
	target := s.URL() + "/" + held + "/dir/pack"

	whole := do(t, s.authorized, http.MethodGet, target, nil, nil)
	if whole.status != http.StatusOK || whole.body != "0123456789" || whole.header.Get("x-goog-generation") != strconv.FormatInt(generation, 10) || whole.header.Get("x-goog-meta-kept") != "beside" || whole.header.Get("x-goog-stored-content-length") != "10" {
		t.Fatalf("the download answered %d %q with %v", whole.status, whole.body, whole.header)
	}
	if _, err := http.ParseTime(whole.header.Get("Last-Modified")); err != nil {
		t.Errorf("Last-Modified is %q", whole.header.Get("Last-Modified"))
	}
	if again := s.Put(held, "dir/pack", []byte("0123456789"), nil); again == generation {
		t.Error("writing the same bytes again did not change the generation")
	}
	for requested, want := range map[string]struct {
		status       int
		body         string
		contentRange string
	}{
		"bytes=2-5":   {http.StatusPartialContent, "2345", "bytes 2-5/10"},
		"bytes=8-100": {http.StatusPartialContent, "89", "bytes 8-9/10"},
		"bytes=9-":    {http.StatusPartialContent, "9", "bytes 9-9/10"},
		"bytes=10-13": {http.StatusRequestedRangeNotSatisfiable, "", "bytes */10"},
		"bytes=11-":   {http.StatusRequestedRangeNotSatisfiable, "", "bytes */10"},
		"bytes=-4":    {http.StatusRequestedRangeNotSatisfiable, "", "bytes */10"},
	} {
		got := do(t, s.authorized, http.MethodGet, target, http.Header{"Range": {requested}}, nil)
		if got.status != want.status || got.header.Get("Content-Range") != want.contentRange || (want.body != "" && got.body != want.body) {
			t.Errorf("Range %s answered %d %q with Content-Range %q, want %d %q %q", requested, got.status, got.body, got.header.Get("Content-Range"), want.status, want.body, want.contentRange)
		}
		if want.status == http.StatusRequestedRangeNotSatisfiable && got.reason() != "InvalidRange" {
			t.Errorf("Range %s was refused as %q", requested, got.reason())
		}
	}
	s.AnswerARangeAtTheEndWithNoBytes()
	if got := do(t, s.authorized, http.MethodGet, target, http.Header{"Range": {"bytes=10-13"}}, nil); got.status != http.StatusPartialContent || got.body != "" || got.header.Get("Content-Range") != "bytes */10" {
		t.Errorf("switched on, a range at the end answered %d %q %q", got.status, got.body, got.header.Get("Content-Range"))
	}
	if got := do(t, s.authorized, http.MethodGet, target, http.Header{"Range": {"bytes=11-13"}}, nil); got.status != http.StatusRequestedRangeNotSatisfiable {
		t.Errorf("switched on, a range past the end answered %d; the switch is for the range exactly at the end", got.status)
	}
	for name, want := range map[string]string{s.URL() + "/" + held + "/absent": "NoSuchKey", s.URL() + "/never-created/dir/pack": "NoSuchBucket"} {
		if got := do(t, s.authorized, http.MethodGet, name, nil, nil); got.status != http.StatusNotFound || got.reason() != want {
			t.Errorf("%s answered %d %s, want 404 %s", name, got.status, got.body, want)
		}
	}
}

// signer makes V4 signed URLs by hand, from the documented recipe, with every
// ingredient open to being got wrong.
type signer struct {
	key         *rsa.PrivateKey
	account     string
	host        string
	verb        string
	location    string
	signedAt    time.Time
	expires     int
	signedPath  string
	extraHeader string
}

func (s signer) sign(endpoint, path string) string {
	timestamp := s.signedAt.UTC().Format("20060102T150405Z")
	scope := s.signedAt.UTC().Format("20060102") + "/" + s.location + "/storage/goog4_request"
	signedHeaders := "host"
	headers := "host:" + s.host + "\n"
	if s.extraHeader != "" {
		signedHeaders = "host;" + s.extraHeader
		headers += s.extraHeader + ":stated\n"
	}
	query := "X-Goog-Algorithm=GOOG4-RSA-SHA256&X-Goog-Credential=" + url.QueryEscape(s.account+"/"+scope) +
		"&X-Goog-Date=" + timestamp + "&X-Goog-Expires=" + strconv.Itoa(s.expires) + "&X-Goog-SignedHeaders=" + url.QueryEscape(signedHeaders)
	signedPath := path
	if s.signedPath != "" {
		signedPath = s.signedPath
	}
	canonical := strings.Join([]string{s.verb, signedPath, query, headers, signedHeaders, "UNSIGNED-PAYLOAD"}, "\n")
	hashed := sha256.Sum256([]byte(canonical))
	digest := sha256.Sum256([]byte("GOOG4-RSA-SHA256\n" + timestamp + "\n" + scope + "\n" + hex.EncodeToString(hashed[:])))
	signature, err := rsa.SignPKCS1v15(rand.Reader, s.key, crypto.SHA256, digest[:])
	if err != nil {
		panic(err)
	}
	return endpoint + path + "?" + query + "&X-Goog-Signature=" + hex.EncodeToString(signature)
}

// TestASignedURLIsVerifiedInFull pins that the fake checks a V4 signed URL the
// way the documentation says one is made, so that a client which signed the
// wrong object, the wrong verb, the wrong host, as the wrong account, with the
// wrong key or for too long is caught here. fake-gcs-server checks none of this,
// so this fake is the only place short of the service where it is checked.
func TestASignedURLIsVerifiedInFull(t *testing.T) {
	s := newService(t)
	s.Put(held, "packs/pack 1", []byte("fetched without credentials"), nil)
	s.Put(held, "packs/pack 2", []byte("another object"), nil)
	var file struct {
		PrivateKey string `json:"private_key"`
	}
	if err := json.Unmarshal(s.CredentialsJSON(), &file); err != nil {
		t.Fatalf("the key file: %v", err)
	}
	block, _ := pem.Decode([]byte(file.PrivateKey))
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		t.Fatalf("the key: %v", err)
	}
	stranger, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate a key: %v", err)
	}
	// The server's own clock is the only one there is, and a token it issues says
	// what it reads, to the second: that is the moment these URLs are signed at.
	response, err := plain.Get(s.URL() + "/" + held + "/x")
	if err != nil {
		t.Fatalf("ask the time: %v", err)
	}
	_ = response.Body.Close()
	now, err := http.ParseTime(response.Header.Get("Date"))
	if err != nil {
		t.Fatalf("the server's Date: %v", err)
	}
	good := signer{key: parsed.(*rsa.PrivateKey), account: s.ServiceAccount(), host: strings.TrimPrefix(s.URL(), "http://"), verb: http.MethodGet, location: "auto", signedAt: now.Add(-time.Minute), expires: 3600}
	const path = "/" + held + "/packs/pack%201"

	if got := do(t, plain, http.MethodGet, good.sign(s.URL(), path), nil, nil); got.status != http.StatusOK || got.body != "fetched without credentials" {
		t.Fatalf("PREMISE: a URL signed by the recipe answered %d %s, so the refusals below would prove nothing", got.status, got.body)
	}
	withHeader := good
	withHeader.extraHeader = "x-goog-custom"
	if got := do(t, plain, http.MethodGet, withHeader.sign(s.URL(), path), http.Header{"X-Goog-Custom": {"stated"}}, nil); got.status != http.StatusOK {
		t.Errorf("a URL that signs a second header, fetched with it, answered %d %s", got.status, got.body)
	}
	if got := do(t, plain, http.MethodGet, withHeader.sign(s.URL(), path), nil, nil); got.status != http.StatusForbidden {
		t.Errorf("a URL that signs a second header, fetched without it, answered %d", got.status)
	}

	for name, attempt := range map[string]struct {
		change func(*signer)
		status int
		code   string
	}{
		"signed with another key":    {func(s *signer) { s.key = stranger }, http.StatusForbidden, "SignatureDoesNotMatch"},
		"signed for another object":  {func(s *signer) { s.signedPath = "/" + held + "/packs/pack%202" }, http.StatusForbidden, "SignatureDoesNotMatch"},
		"signed for another verb":    {func(s *signer) { s.verb = http.MethodPut }, http.StatusForbidden, "SignatureDoesNotMatch"},
		"signed for another host":    {func(s *signer) { s.host = "storage.googleapis.com" }, http.StatusForbidden, "SignatureDoesNotMatch"},
		"signed as another account":  {func(s *signer) { s.account = "someone@else.iam.gserviceaccount.com" }, http.StatusBadRequest, "AuthenticationRequired"},
		"signed for an eighth day":   {func(s *signer) { s.expires = 604801 }, http.StatusBadRequest, "AuthenticationRequired"},
		"signed for no time at all":  {func(s *signer) { s.expires = 0 }, http.StatusBadRequest, "AuthenticationRequired"},
		"expired an hour ago":        {func(s *signer) { s.signedAt = now.Add(-2 * time.Hour) }, http.StatusBadRequest, "ExpiredToken"},
		"not yet usable":             {func(s *signer) { s.signedAt = now.Add(time.Hour) }, http.StatusForbidden, "AccessDenied"},
		"scoped to another service ": {func(s *signer) { s.location = "auto/compute" }, http.StatusBadRequest, "AuthenticationRequired"},
	} {
		bad := good
		attempt.change(&bad)
		if got := do(t, plain, http.MethodGet, bad.sign(s.URL(), path), nil, nil); got.status != attempt.status || got.reason() != attempt.code {
			t.Errorf("a URL %s answered %d %s, want %d %s", name, got.status, got.body, attempt.status, attempt.code)
		}
	}
	signed := good.sign(s.URL(), path)
	for name, tampered := range map[string]string{
		"turned to another object": strings.Replace(signed, "pack%201", "pack%202", 1),
		"given a longer life":      strings.Replace(signed, "X-Goog-Expires=3600", "X-Goog-Expires=360000", 1),
		"given another parameter":  signed + "&generation=5",
		"with its signature cut":   signed[:len(signed)-2],
		"with no signature":        signed[:strings.Index(signed, "&X-Goog-Signature")],
	} {
		if got := do(t, plain, http.MethodGet, tampered, nil, nil); got.status != http.StatusForbidden && got.status != http.StatusBadRequest {
			t.Errorf("a signed URL %s answered %d %s", name, got.status, got.body)
		}
	}
	if got := do(t, plain, http.MethodGet, good.sign(s.URL(), "/"+held+"/packs/absent"), nil, nil); got.status != http.StatusNotFound || got.reason() != "NoSuchKey" {
		t.Errorf("a good signature for an object that is not there answered %d %s, want 404", got.status, got.body)
	}
}

// TestACopyIsMadeInAsManyCallsAsItIsToldTo pins objects.rewrite: the copy with
// its metadata in one call, or, told to, in several that each hand back a token
// — and no destination until the last, and no going on with a token when the
// source has changed beneath it.
func TestACopyIsMadeInAsManyCallsAsItIsToldTo(t *testing.T) {
	s := newService(t)
	s.Put(held, "source", bytes.Repeat([]byte("x"), 2500), map[string]string{"kept": "beside"})
	target := s.URL() + "/storage/v1/b/" + held + "/o/source/rewriteTo/b/" + held + "/o/"

	once := do(t, s.authorized, http.MethodPost, target+"fork", nil, nil).json(t)
	if once["done"] != true || once["resource"].(map[string]any)["metadata"].(map[string]any)["kept"] != "beside" || once["rewriteToken"] != nil {
		t.Fatalf("a copy in one call answered %v", once)
	}

	s.SetRewriteBytesPerCall(1000)
	token := ""
	for call := 1; ; call++ {
		query := ""
		if token != "" {
			query = "?rewriteToken=" + token
		}
		got := do(t, s.authorized, http.MethodPost, target+"slow"+query, nil, nil).json(t)
		if got["done"] == true {
			if call != 3 || got["totalBytesRewritten"] != "2500" {
				t.Fatalf("the copy was done at call %d: %v", call, got)
			}
			break
		}
		if _, ok := s.Contents(held, "slow"); ok || got["resource"] != nil || got["objectSize"] != "2500" {
			t.Fatalf("a copy not yet done answered %v, or is already an object", got)
		}
		token = got["rewriteToken"].(string)
	}
	if copied, _ := s.Contents(held, "slow"); len(copied) != 2500 {
		t.Fatalf("the copy holds %d bytes", len(copied))
	}

	begun := do(t, s.authorized, http.MethodPost, target+"raced", nil, nil).json(t)
	s.Put(held, "source", []byte("changed beneath the copy"), nil)
	if got := do(t, s.authorized, http.MethodPost, target+"raced?rewriteToken="+begun["rewriteToken"].(string), nil, nil); got.status != http.StatusBadRequest {
		t.Errorf("going on with a copy whose source changed answered %d %s", got.status, got.body)
	}
	if got := do(t, s.authorized, http.MethodPost, s.URL()+"/storage/v1/b/"+held+"/o/absent/rewriteTo/b/"+held+"/o/x", nil, nil); got.status != http.StatusNotFound {
		t.Errorf("a copy of what is not there answered %d", got.status)
	}
	if got := do(t, s.authorized, http.MethodPost, target+"described", http.Header{"Content-Type": {"application/json"}}, []byte(`{"metadata":{"other":"value"}}`)); got.status != http.StatusBadRequest {
		t.Errorf("a copy that describes its destination answered %d; this fake only copies metadata", got.status)
	}
}
