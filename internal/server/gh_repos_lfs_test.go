package bleephub

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/e6qu/bleephub/internal/store"
)

// lfsMemoryByteStore is an in-process ActionsByteStore. The only shipped
// implementation is S3-backed (internal/store/object_bytes.go), and standing up
// MinIO for every LFS assertion would make the negotiation and authorization
// tests depend on Docker; this keeps them hermetic. The S3 path is covered
// separately by TestLFSObjectBytesLandInTheS3ByteStore.
type lfsMemoryByteStore struct {
	mu    sync.Mutex
	blobs map[string][]byte
}

func newLFSMemoryByteStore() *lfsMemoryByteStore {
	return &lfsMemoryByteStore{blobs: map[string][]byte{}}
}

func (m *lfsMemoryByteStore) Put(_ context.Context, key string, data []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.blobs[key] = append([]byte(nil), data...)
	return nil
}

func (m *lfsMemoryByteStore) Get(_ context.Context, key string) ([]byte, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	data, ok := m.blobs[key]
	if !ok {
		return nil, fmt.Errorf("no such object %s", key)
	}
	return append([]byte(nil), data...), nil
}

func (m *lfsMemoryByteStore) PutStream(ctx context.Context, key string, r io.Reader) error {
	data, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	return m.Put(ctx, key, data)
}

func (m *lfsMemoryByteStore) GetStream(ctx context.Context, key string) (io.ReadCloser, error) {
	data, err := m.Get(ctx, key)
	if err != nil {
		return nil, err
	}
	return io.NopCloser(bytes.NewReader(data)), nil
}

func (m *lfsMemoryByteStore) Delete(_ context.Context, key string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.blobs, key)
	return nil
}

// ─── Harness helpers ────────────────────────────────────────────────────

func newLFSServer(t *testing.T, private bool) (*isolatedServer, *lfsMemoryByteStore, repoRef) {
	t.Helper()
	srv := newIsolatedServer(t)
	byteStore := newLFSMemoryByteStore()
	srv.store.ObjectByteStore = byteStore
	name := "lfs-repo"
	resp := srv.post(t, "/api/v3/user/repos", defaultToken, map[string]interface{}{"name": name, "private": private})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("create repo = %d: %s", resp.StatusCode, body)
	}
	return srv, byteStore, repoRef{owner: "admin", name: name}
}

func lfsEndpoint(repo repoRef) string {
	return "/" + repo.fullName() + ".git/info/lfs"
}

func (s *isolatedServer) lfsDo(t *testing.T, method, path, token string, body io.Reader) *http.Response {
	t.Helper()
	req, err := http.NewRequest(method, s.baseURL+path, body)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Accept", lfsMediaType)
	if body != nil {
		req.Header.Set("Content-Type", lfsMediaType)
	}
	if token != "" {
		req.Header.Set("Authorization", "token "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func (s *isolatedServer) lfsBatch(t *testing.T, repo repoRef, token string, request map[string]interface{}) (int, lfsBatchResponse, string) {
	t.Helper()
	encoded, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	resp := s.lfsDo(t, http.MethodPost, lfsEndpoint(repo)+"/objects/batch", token, bytes.NewReader(encoded))
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if got := resp.Header.Get("Content-Type"); got != lfsMediaType {
		t.Fatalf("batch Content-Type = %q, want %q (git-lfs cannot parse anything else)", got, lfsMediaType)
	}
	var decoded lfsBatchResponse
	if resp.StatusCode == http.StatusOK {
		if err := json.Unmarshal(raw, &decoded); err != nil {
			t.Fatalf("decode batch response %s: %v", raw, err)
		}
	}
	return resp.StatusCode, decoded, string(raw)
}

func lfsOIDOf(content []byte) string {
	digest := sha256.Sum256(content)
	return hex.EncodeToString(digest[:])
}

func lfsBatchBody(operation string, content []byte) map[string]interface{} {
	return map[string]interface{}{
		"operation": operation,
		"transfers": []string{"basic"},
		"ref":       map[string]string{"name": "refs/heads/main"},
		"objects":   []map[string]interface{}{{"oid": lfsOIDOf(content), "size": len(content)}},
	}
}

func (s *isolatedServer) lfsUpload(t *testing.T, href, token string, content []byte) *http.Response {
	t.Helper()
	path := strings.TrimPrefix(href, s.baseURL)
	req, err := http.NewRequest(http.MethodPut, s.baseURL+path, bytes.NewReader(content))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/octet-stream")
	if token != "" {
		req.Header.Set("Authorization", "token "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func decodeLFSError(t *testing.T, resp *http.Response) lfsErrorBody {
	t.Helper()
	defer resp.Body.Close()
	if got := resp.Header.Get("Content-Type"); got != lfsMediaType {
		t.Fatalf("error Content-Type = %q, want %q", got, lfsMediaType)
	}
	var body lfsErrorBody
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode LFS error: %v", err)
	}
	if body.Message == "" || body.DocumentationURL == "" {
		t.Fatalf("LFS error body is not the documented shape: %+v", body)
	}
	return body
}

// ─── Batch negotiation ──────────────────────────────────────────────────

// TestLFSBatchUploadDownloadRoundTrip walks the whole basic-transfer flow: a
// GET after upload must return the object, not a pointer.
func TestLFSBatchUploadDownloadRoundTrip(t *testing.T) {
	t.Parallel()
	srv, byteStore, repo := newLFSServer(t, false)
	content := bytes.Repeat([]byte("bleephub lfs payload\n"), 512)
	oid := lfsOIDOf(content)

	status, batch, raw := srv.lfsBatch(t, repo, defaultToken, lfsBatchBody("upload", content))
	if status != http.StatusOK {
		t.Fatalf("upload batch = %d: %s", status, raw)
	}
	if batch.Transfer != "basic" {
		t.Fatalf("transfer = %q, want basic", batch.Transfer)
	}
	if len(batch.Objects) != 1 {
		t.Fatalf("batch returned %d objects, want 1", len(batch.Objects))
	}
	object := batch.Objects[0]
	if object.OID != oid || object.Size != int64(len(content)) {
		t.Fatalf("batch object = %+v, want oid %s size %d", object, oid, len(content))
	}
	upload, ok := object.Actions["upload"]
	if !ok {
		t.Fatalf("upload batch returned no upload action: %+v", object)
	}
	if !strings.Contains(upload.Href, "/info/lfs/objects/"+oid) {
		t.Fatalf("upload href = %q, want it to name the object", upload.Href)
	}
	if _, ok := object.Actions["verify"]; !ok {
		t.Fatalf("upload batch returned no verify action: %+v", object)
	}

	resp := srv.lfsUpload(t, upload.Href, defaultToken, content)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("upload transfer = %d: %s", resp.StatusCode, body)
	}

	// The bytes went to the object byte store under the content-addressed key,
	// not to the git object store or the filesystem.
	stored, err := byteStore.Get(context.Background(), store.LFSObjectDataKey(oid))
	if err != nil {
		t.Fatalf("object was not stored under %s: %v", store.LFSObjectDataKey(oid), err)
	}
	if !bytes.Equal(stored, content) {
		t.Fatalf("stored object differs from the uploaded bytes")
	}

	// A second upload batch for an object the repository already holds returns
	// no actions at all: nothing left to transfer.
	status, batch, raw = srv.lfsBatch(t, repo, defaultToken, lfsBatchBody("upload", content))
	if status != http.StatusOK {
		t.Fatalf("second upload batch = %d: %s", status, raw)
	}
	if len(batch.Objects[0].Actions) != 0 {
		t.Fatalf("re-uploading a held object still offered actions: %+v", batch.Objects[0].Actions)
	}

	status, batch, raw = srv.lfsBatch(t, repo, defaultToken, lfsBatchBody("download", content))
	if status != http.StatusOK {
		t.Fatalf("download batch = %d: %s", status, raw)
	}
	download, ok := batch.Objects[0].Actions["download"]
	if !ok {
		t.Fatalf("download batch returned no download action: %+v", batch.Objects[0])
	}
	fetched := srv.lfsDo(t, http.MethodGet, strings.TrimPrefix(download.Href, srv.baseURL), defaultToken, nil)
	defer fetched.Body.Close()
	if fetched.StatusCode != http.StatusOK {
		t.Fatalf("object download = %d", fetched.StatusCode)
	}
	got, err := io.ReadAll(fetched.Body)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, content) {
		t.Fatalf("downloaded %d bytes, want the %d uploaded bytes", len(got), len(content))
	}

	verifyBody, err := json.Marshal(map[string]interface{}{"oid": oid, "size": len(content)})
	if err != nil {
		t.Fatal(err)
	}
	verified := srv.lfsDo(t, http.MethodPost, lfsEndpoint(repo)+"/objects/verify", defaultToken, bytes.NewReader(verifyBody))
	verified.Body.Close()
	if verified.StatusCode != http.StatusOK {
		t.Fatalf("verify = %d, want 200", verified.StatusCode)
	}
}

// TestLFSBatchDownloadOfMissingObjectIsPerObject404 pins that the batch call
// itself succeeds and the individual object carries the 404.
func TestLFSBatchDownloadOfMissingObjectIsPerObject404(t *testing.T) {
	t.Parallel()
	srv, _, repo := newLFSServer(t, false)
	content := []byte("never uploaded")

	status, batch, raw := srv.lfsBatch(t, repo, defaultToken, lfsBatchBody("download", content))
	if status != http.StatusOK {
		t.Fatalf("download batch for a missing object = %d, want 200: %s", status, raw)
	}
	object := batch.Objects[0]
	if object.Error == nil || object.Error.Code != http.StatusNotFound {
		t.Fatalf("missing object = %+v, want an error with code 404", object)
	}
	if len(object.Actions) != 0 {
		t.Fatalf("missing object offered actions: %+v", object.Actions)
	}
}

// TestLFSBatchRejectsUnsupportedTransfer pins the negotiation: a client that
// offers no adapter this server implements is told so in the LFS error shape.
func TestLFSBatchRejectsUnsupportedTransfer(t *testing.T) {
	t.Parallel()
	srv, _, repo := newLFSServer(t, false)
	request := lfsBatchBody("download", []byte("x"))
	request["transfers"] = []string{"tus", "multipart"}
	status, _, raw := srv.lfsBatch(t, repo, defaultToken, request)
	if status != http.StatusUnprocessableEntity {
		t.Fatalf("unsupported transfer = %d, want 422: %s", status, raw)
	}

	// An empty list means "basic" per the spec and must still be served.
	request["transfers"] = []string{}
	if status, _, raw = srv.lfsBatch(t, repo, defaultToken, request); status != http.StatusOK {
		t.Fatalf("batch without a transfer list = %d, want 200: %s", status, raw)
	}
}

// TestLFSBatchRefusesDisabledRepository pins that the enterprise
// PUT/DELETE /repos/{owner}/{repo}/lfs toggle actually governs the LFS server.
func TestLFSBatchRefusesDisabledRepository(t *testing.T) {
	t.Parallel()
	srv, _, repo := newLFSServer(t, false)
	resp := srv.delete(t, repo.path()+"/lfs", defaultToken)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("disable LFS = %d, want 204", resp.StatusCode)
	}
	status, _, raw := srv.lfsBatch(t, repo, defaultToken, lfsBatchBody("download", []byte("x")))
	if status != http.StatusForbidden {
		t.Fatalf("batch on an LFS-disabled repository = %d, want 403: %s", status, raw)
	}

	// Re-enabling restores it, so the toggle is a toggle and not a one-way door.
	resp = srv.put(t, repo.path()+"/lfs", defaultToken, nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("enable LFS = %d, want 204", resp.StatusCode)
	}
	if status, _, raw = srv.lfsBatch(t, repo, defaultToken, lfsBatchBody("download", []byte("x"))); status != http.StatusOK {
		t.Fatalf("batch after re-enabling LFS = %d, want 200: %s", status, raw)
	}
}

// ─── Upload verification ────────────────────────────────────────────────

// TestLFSUploadRejectsContentThatDoesNotMatchItsOID is the integrity gate:
// storing bytes that hash to something other than the pointer's oid corrupts
// every future checkout of that pointer.
func TestLFSUploadRejectsContentThatDoesNotMatchItsOID(t *testing.T) {
	t.Parallel()
	srv, byteStore, repo := newLFSServer(t, false)
	promised := []byte("the bytes the client promised")
	oid := lfsOIDOf(promised)

	_, batch, _ := srv.lfsBatch(t, repo, defaultToken, lfsBatchBody("upload", promised))
	href := batch.Objects[0].Actions["upload"].Href

	// Same length, different content: only the hash can tell them apart.
	tampered := append([]byte(nil), promised...)
	tampered[0] ^= 0xff
	resp := srv.lfsUpload(t, href, defaultToken, tampered)
	if resp.StatusCode != http.StatusUnprocessableEntity {
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Fatalf("upload of mismatched content = %d, want 422: %s", resp.StatusCode, body)
	}
	decodeLFSError(t, resp)

	if _, err := byteStore.Get(context.Background(), store.LFSObjectDataKey(oid)); err == nil {
		t.Fatal("rejected upload left its bytes in the object store")
	}
	_, batch, _ = srv.lfsBatch(t, repo, defaultToken, lfsBatchBody("download", promised))
	if batch.Objects[0].Error == nil {
		t.Fatal("rejected upload was registered and is downloadable")
	}
}

// TestLFSUploadRejectsSizeMismatch pins the other half of the transfer
// contract: the body has to be as long as the batch request declared.
func TestLFSUploadRejectsSizeMismatch(t *testing.T) {
	t.Parallel()
	srv, _, repo := newLFSServer(t, false)
	content := []byte("declared as this long")

	_, batch, _ := srv.lfsBatch(t, repo, defaultToken, lfsBatchBody("upload", content))
	href := batch.Objects[0].Actions["upload"].Href

	resp := srv.lfsUpload(t, href, defaultToken, append(content, []byte(" plus more")...))
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("upload longer than declared = %d, want 422", resp.StatusCode)
	}

	resp = srv.lfsUpload(t, href, defaultToken, content[:5])
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("upload shorter than declared = %d, want 422", resp.StatusCode)
	}
}

// TestLFSUploadDoesNotOverwriteStoredObjectBytes pins the content-addressed
// invariant: a second repository claiming an already-stored oid cannot replace
// the bytes every other repository's pointers resolve through.
func TestLFSUploadDoesNotOverwriteStoredObjectBytes(t *testing.T) {
	t.Parallel()
	srv, byteStore, repo := newLFSServer(t, false)
	content := []byte("shared object content")
	oid := lfsOIDOf(content)

	_, batch, _ := srv.lfsBatch(t, repo, defaultToken, lfsBatchBody("upload", content))
	resp := srv.lfsUpload(t, batch.Objects[0].Actions["upload"].Href, defaultToken, content)
	resp.Body.Close()

	second := srv.createTestRepo(t)
	_, batch, _ = srv.lfsBatch(t, second, defaultToken, lfsBatchBody("upload", content))
	href := batch.Objects[0].Actions["upload"].Href
	if href == "" {
		t.Fatal("a repository that does not hold the object was offered no upload action")
	}
	// The claim is a lie: the oid is taken, and these bytes are not it.
	tampered := bytes.Repeat([]byte("x"), len(content))
	resp = srv.lfsUpload(t, href, defaultToken, tampered)
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("upload claiming a stored oid = %d, want 422", resp.StatusCode)
	}
	stored, err := byteStore.Get(context.Background(), store.LFSObjectDataKey(oid))
	if err != nil || !bytes.Equal(stored, content) {
		t.Fatalf("the stored object was overwritten by a mismatched upload (err=%v)", err)
	}

	// An honest upload of the same content from the second repository shares
	// the stored bytes and makes it downloadable there.
	resp = srv.lfsUpload(t, href, defaultToken, content)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("deduplicated upload = %d, want 200", resp.StatusCode)
	}
	_, batch, _ = srv.lfsBatch(t, second, defaultToken, lfsBatchBody("download", content))
	if batch.Objects[0].Error != nil {
		t.Fatalf("object is not downloadable from the second repository: %+v", batch.Objects[0].Error)
	}
}

// ─── Authorization ──────────────────────────────────────────────────────

// TestLFSPrivateRepositoryAuthorization walks the matrix: anonymous gets the
// 401 challenge, an unauthorized caller cannot tell the repo exists, a puller
// may read but not write, and a pusher may do both.
func TestLFSPrivateRepositoryAuthorization(t *testing.T) {
	t.Parallel()
	srv, _, repo := newLFSServer(t, true)
	content := []byte("private object")

	// Seed one object so a download batch has something to authorize against.
	_, batch, _ := srv.lfsBatch(t, repo, defaultToken, lfsBatchBody("upload", content))
	resp := srv.lfsUpload(t, batch.Objects[0].Actions["upload"].Href, defaultToken, content)
	resp.Body.Close()

	anonymous := func(t *testing.T, operation string) *http.Response {
		t.Helper()
		encoded, err := json.Marshal(lfsBatchBody(operation, content))
		if err != nil {
			t.Fatal(err)
		}
		return srv.lfsDo(t, http.MethodPost, lfsEndpoint(repo)+"/objects/batch", "", bytes.NewReader(encoded))
	}
	for _, operation := range []string{"download", "upload"} {
		resp := anonymous(t, operation)
		if resp.StatusCode != http.StatusUnauthorized {
			resp.Body.Close()
			t.Fatalf("anonymous %s batch = %d, want 401", operation, resp.StatusCode)
		}
		if got := resp.Header.Get("LFS-Authenticate"); got != `Basic realm="Git LFS"` {
			resp.Body.Close()
			t.Fatalf("anonymous %s batch LFS-Authenticate = %q", operation, got)
		}
		decodeLFSError(t, resp)
	}

	outsider := srv.createTestUser(t, "lfs-outsider")
	outsiderToken := srv.store.CreateToken(outsider.ID, "repo").Value
	if status, _, _ := srv.lfsBatch(t, repo, outsiderToken, lfsBatchBody("download", content)); status != http.StatusNotFound {
		t.Fatalf("outsider download batch = %d, want 404 (a private repository must not be distinguishable)", status)
	}

	puller := srv.createTestUser(t, "lfs-puller")
	pullerToken := srv.store.CreateToken(puller.ID, "repo").Value
	if !srv.store.AddRepoCollaborator(repo.owner, repo.name, puller.Login, "pull") {
		t.Fatal("add pull collaborator")
	}
	if status, _, raw := srv.lfsBatch(t, repo, pullerToken, lfsBatchBody("download", content)); status != http.StatusOK {
		t.Fatalf("puller download batch = %d, want 200: %s", status, raw)
	}
	if status, _, raw := srv.lfsBatch(t, repo, pullerToken, lfsBatchBody("upload", content)); status != http.StatusForbidden {
		t.Fatalf("puller upload batch = %d, want 403: %s", status, raw)
	}

	pusher := srv.createTestUser(t, "lfs-pusher")
	pusherToken := srv.store.CreateToken(pusher.ID, "repo").Value
	if !srv.store.AddRepoCollaborator(repo.owner, repo.name, pusher.Login, "push") {
		t.Fatal("add push collaborator")
	}
	if status, _, raw := srv.lfsBatch(t, repo, pusherToken, lfsBatchBody("upload", content)); status != http.StatusOK {
		t.Fatalf("pusher upload batch = %d, want 200: %s", status, raw)
	}

	// The transfer endpoints authorize independently of the batch endpoint:
	// holding an href is not standing.
	objectPath := lfsEndpoint(repo) + "/objects/" + lfsOIDOf(content)
	anonymousGet := srv.lfsDo(t, http.MethodGet, objectPath, "", nil)
	if anonymousGet.StatusCode != http.StatusUnauthorized {
		anonymousGet.Body.Close()
		t.Fatalf("anonymous object download = %d, want 401", anonymousGet.StatusCode)
	}
	anonymousGet.Body.Close()
	outsiderGet := srv.lfsDo(t, http.MethodGet, objectPath, outsiderToken, nil)
	if outsiderGet.StatusCode != http.StatusNotFound {
		outsiderGet.Body.Close()
		t.Fatalf("outsider object download = %d, want 404", outsiderGet.StatusCode)
	}
	outsiderGet.Body.Close()
	pullerPut := srv.lfsUpload(t, srv.baseURL+objectPath, pullerToken, content)
	pullerPut.Body.Close()
	if pullerPut.StatusCode != http.StatusForbidden {
		t.Fatalf("puller object upload = %d, want 403", pullerPut.StatusCode)
	}
}

// TestLFSObjectsAreNotReadableThroughAnotherRepository pins that holding an oid
// in one repository does not make it readable through another, despite the
// shared content-addressed key.
func TestLFSObjectsAreNotReadableThroughAnotherRepository(t *testing.T) {
	t.Parallel()
	srv, _, private := newLFSServer(t, true)
	content := []byte("only in the private repository")
	_, batch, _ := srv.lfsBatch(t, private, defaultToken, lfsBatchBody("upload", content))
	resp := srv.lfsUpload(t, batch.Objects[0].Actions["upload"].Href, defaultToken, content)
	resp.Body.Close()

	other := srv.createTestRepo(t)
	_, batch, _ = srv.lfsBatch(t, other, defaultToken, lfsBatchBody("download", content))
	if batch.Objects[0].Error == nil || batch.Objects[0].Error.Code != http.StatusNotFound {
		t.Fatalf("an object of another repository was served: %+v", batch.Objects[0])
	}
	fetched := srv.lfsDo(t, http.MethodGet, lfsEndpoint(other)+"/objects/"+lfsOIDOf(content), defaultToken, nil)
	fetched.Body.Close()
	if fetched.StatusCode != http.StatusNotFound {
		t.Fatalf("cross-repository object download = %d, want 404", fetched.StatusCode)
	}
}

// TestLFSUnknownSubPathIsAnLFSError pins that even a route this server does
// not implement answers in the media type git-lfs can parse, instead of the
// catch-all's plain-text "404 page not found".
func TestLFSUnknownSubPathIsAnLFSError(t *testing.T) {
	t.Parallel()
	srv, _, repo := newLFSServer(t, false)
	resp := srv.lfsDo(t, http.MethodGet, lfsEndpoint(repo)+"/nonesuch", defaultToken, nil)
	if resp.StatusCode != http.StatusNotFound {
		resp.Body.Close()
		t.Fatalf("unknown LFS sub-path = %d, want 404", resp.StatusCode)
	}
	decodeLFSError(t, resp)
}

// ─── Locking API ────────────────────────────────────────────────────────

func TestLFSLockingAPI(t *testing.T) {
	t.Parallel()
	srv, _, repo := newLFSServer(t, false)

	create := func(t *testing.T, token, path string) *http.Response {
		t.Helper()
		body, err := json.Marshal(map[string]interface{}{"path": path, "ref": map[string]string{"name": "refs/heads/main"}})
		if err != nil {
			t.Fatal(err)
		}
		return srv.lfsDo(t, http.MethodPost, lfsEndpoint(repo)+"/locks", token, bytes.NewReader(body))
	}

	resp := create(t, defaultToken, "assets/model.bin")
	var created struct {
		Lock lfsLockJSON `json:"lock"`
	}
	if resp.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Fatalf("create lock = %d, want 201: %s", resp.StatusCode, body)
	}
	if err := json.NewDecoder(resp.Body).Decode(&created); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if created.Lock.ID == "" || created.Lock.Path != "assets/model.bin" || created.Lock.Owner.Name != "admin" {
		t.Fatalf("created lock = %+v", created.Lock)
	}

	// A second lock on the same path is a conflict that names the holder.
	resp = create(t, defaultToken, "assets/model.bin")
	if resp.StatusCode != http.StatusConflict {
		resp.Body.Close()
		t.Fatalf("duplicate lock = %d, want 409", resp.StatusCode)
	}
	resp.Body.Close()

	// Someone else's lock shows up under "theirs" for the other user.
	other := srv.createTestUser(t, "lfs-locker")
	otherToken := srv.store.CreateToken(other.ID, "repo").Value
	if !srv.store.AddRepoCollaborator(repo.owner, repo.name, other.Login, "push") {
		t.Fatal("add push collaborator")
	}
	resp = create(t, otherToken, "assets/texture.bin")
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("second user's lock = %d, want 201", resp.StatusCode)
	}

	listed := srv.lfsDo(t, http.MethodGet, lfsEndpoint(repo)+"/locks", defaultToken, nil)
	var list struct {
		Locks []lfsLockJSON `json:"locks"`
	}
	if err := json.NewDecoder(listed.Body).Decode(&list); err != nil {
		t.Fatal(err)
	}
	listed.Body.Close()
	if len(list.Locks) != 2 {
		t.Fatalf("listed %d locks, want 2", len(list.Locks))
	}

	filtered := srv.lfsDo(t, http.MethodGet, lfsEndpoint(repo)+"/locks?path=assets/texture.bin", defaultToken, nil)
	list.Locks = nil
	if err := json.NewDecoder(filtered.Body).Decode(&list); err != nil {
		t.Fatal(err)
	}
	filtered.Body.Close()
	if len(list.Locks) != 1 || list.Locks[0].Path != "assets/texture.bin" {
		t.Fatalf("path filter returned %+v", list.Locks)
	}

	verified := srv.lfsDo(t, http.MethodPost, lfsEndpoint(repo)+"/locks/verify", otherToken, strings.NewReader("{}"))
	var verification struct {
		Ours   []lfsLockJSON `json:"ours"`
		Theirs []lfsLockJSON `json:"theirs"`
	}
	if err := json.NewDecoder(verified.Body).Decode(&verification); err != nil {
		t.Fatal(err)
	}
	verified.Body.Close()
	if len(verification.Ours) != 1 || verification.Ours[0].Path != "assets/texture.bin" {
		t.Fatalf("ours = %+v", verification.Ours)
	}
	if len(verification.Theirs) != 1 || verification.Theirs[0].Path != "assets/model.bin" {
		t.Fatalf("theirs = %+v", verification.Theirs)
	}

	// Releasing someone else's lock takes force.
	unlock := func(t *testing.T, token, id string, force bool) *http.Response {
		t.Helper()
		body, err := json.Marshal(map[string]interface{}{"force": force})
		if err != nil {
			t.Fatal(err)
		}
		return srv.lfsDo(t, http.MethodPost, lfsEndpoint(repo)+"/locks/"+id+"/unlock", token, bytes.NewReader(body))
	}
	resp = unlock(t, otherToken, created.Lock.ID, false)
	if resp.StatusCode != http.StatusForbidden {
		resp.Body.Close()
		t.Fatalf("unlocking another user's lock without force = %d, want 403", resp.StatusCode)
	}
	decodeLFSError(t, resp)

	resp = unlock(t, otherToken, created.Lock.ID, true)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("forced unlock = %d, want 200", resp.StatusCode)
	}
	resp = unlock(t, otherToken, created.Lock.ID, true)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("unlocking a released lock = %d, want 404", resp.StatusCode)
	}
}

// TestLFSLockingRequiresPushAccess pins that locks are a write operation.
func TestLFSLockingRequiresPushAccess(t *testing.T) {
	t.Parallel()
	srv, _, repo := newLFSServer(t, false)
	puller := srv.createTestUser(t, "lfs-lock-puller")
	pullerToken := srv.store.CreateToken(puller.ID, "repo").Value
	if !srv.store.AddRepoCollaborator(repo.owner, repo.name, puller.Login, "pull") {
		t.Fatal("add pull collaborator")
	}
	body, err := json.Marshal(map[string]interface{}{"path": "a.bin"})
	if err != nil {
		t.Fatal(err)
	}
	resp := srv.lfsDo(t, http.MethodPost, lfsEndpoint(repo)+"/locks", pullerToken, bytes.NewReader(body))
	if resp.StatusCode != http.StatusForbidden {
		resp.Body.Close()
		t.Fatalf("lock as a puller = %d, want 403", resp.StatusCode)
	}
	decodeLFSError(t, resp)
}

// ─── S3-backed object storage ───────────────────────────────────────────

// TestLFSObjectBytesLandInTheS3ByteStore runs the transfer against the real
// S3-backed byte store (MinIO) rather than the in-process fake, pinning that
// LFS bytes land in object storage on the implementation that ships.
func TestLFSObjectBytesLandInTheS3ByteStore(t *testing.T) {
	resetS3FSCacheForTest(t)
	objectFS, byteStore := newObjectByteStoreForTest(t)
	srv := newIsolatedServer(t)
	srv.store.ObjectByteStore = byteStore
	resp := srv.post(t, "/api/v3/user/repos", defaultToken, map[string]interface{}{"name": "lfs-s3"})
	resp.Body.Close()
	repo := repoRef{owner: "admin", name: "lfs-s3"}

	content := bytes.Repeat([]byte("s3 backed lfs object "), 4096)
	oid := lfsOIDOf(content)
	_, batch, _ := srv.lfsBatch(t, repo, defaultToken, lfsBatchBody("upload", content))
	uploaded := srv.lfsUpload(t, batch.Objects[0].Actions["upload"].Href, defaultToken, content)
	uploaded.Body.Close()
	if uploaded.StatusCode != http.StatusOK {
		t.Fatalf("upload to the S3 byte store = %d, want 200", uploaded.StatusCode)
	}

	if got := readS3TestFile(t, objectFS, store.LFSObjectDataKey(oid)); !bytes.Equal(got, content) {
		t.Fatalf("object in S3 is %d bytes, want the %d uploaded", len(got), len(content))
	}
	_, batch, _ = srv.lfsBatch(t, repo, defaultToken, lfsBatchBody("download", content))
	fetched := srv.lfsDo(t, http.MethodGet, strings.TrimPrefix(batch.Objects[0].Actions["download"].Href, srv.baseURL), defaultToken, nil)
	got, err := io.ReadAll(fetched.Body)
	fetched.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, content) {
		t.Fatal("download from the S3 byte store did not return the uploaded bytes")
	}
}

// ─── End-to-end with the real git-lfs client ────────────────────────────

// TestGitLFSClientRoundTrip drives the real git and git-lfs binaries against
// the server — the only way to know discovery, pre-push hook, batch
// negotiation and smudge filter agree — so a fresh clone contains the file's
// real bytes, not its 130-byte pointer.
func TestGitLFSClientRoundTrip(t *testing.T) {
	t.Parallel()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	if _, err := exec.LookPath("git-lfs"); err != nil {
		t.Skip("git-lfs is not installed")
	}
	srv, byteStore, repo := newLFSServer(t, false)

	temp := t.TempDir()
	// A credential helper beats prompting: the test must never block on a
	// terminal, and git-lfs authenticates its batch call separately from the
	// git transport.
	credentialHelper := fmt.Sprintf("!f() { echo username=admin; echo password=%s; }; f", defaultToken)
	remote := srv.baseURL + "/" + repo.fullName() + ".git"
	runGit := func(dir string, args ...string) string {
		t.Helper()
		full := append([]string{"-c", "credential.helper=" + credentialHelper}, args...)
		command := exec.Command("git", full...)
		command.Dir = dir
		command.Env = append(os.Environ(), hermeticGitTestEnv(temp)...)
		command.Env = append(command.Env,
			"GIT_AUTHOR_NAME=Bleephub LFS test", "GIT_AUTHOR_EMAIL=lfs@bleephub.invalid",
			"GIT_COMMITTER_NAME=Bleephub LFS test", "GIT_COMMITTER_EMAIL=lfs@bleephub.invalid")
		output, err := command.CombinedOutput()
		if err != nil {
			t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, output)
		}
		return string(output)
	}

	// The clone below runs the smudge filter only if git-lfs is installed for
	// the user; HOME points at the test's temp dir, so this touches nothing.
	runGit(temp, "lfs", "install")

	worktree := filepath.Join(temp, "worktree")
	runGit(temp, "init", "--initial-branch=main", worktree)
	runGit(worktree, "lfs", "install", "--local")
	runGit(worktree, "lfs", "track", "*.bin")
	// Big enough that a pointer file is unmistakably not the content.
	payload := bytes.Repeat([]byte("bleephub git-lfs end to end payload\n"), 4096)
	if err := os.WriteFile(filepath.Join(worktree, "asset.bin"), payload, 0o600); err != nil {
		t.Fatal(err)
	}
	runGit(worktree, "add", ".gitattributes", "asset.bin")
	runGit(worktree, "commit", "-m", "Add an LFS-tracked asset")
	runGit(worktree, "remote", "add", "origin", remote)
	runGit(worktree, "push", "origin", "HEAD:main")

	if _, err := byteStore.Get(context.Background(), store.LFSObjectDataKey(lfsOIDOf(payload))); err != nil {
		t.Fatalf("git lfs push did not store the object: %v", err)
	}

	clone := filepath.Join(temp, "clone")
	runGit(temp, "clone", remote, clone)
	got, err := os.ReadFile(filepath.Join(clone, "asset.bin"))
	if err != nil {
		t.Fatalf("read the cloned asset: %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatalf("cloned asset is %d bytes (%.60q…), want the %d-byte payload — a pointer file was checked out",
			len(got), got, len(payload))
	}

	if out := runGit(clone, "lfs", "lock", "asset.bin"); !strings.Contains(out, "asset.bin") {
		t.Fatalf("git lfs lock said %q", out)
	}
	if out := runGit(clone, "lfs", "locks"); !strings.Contains(out, "asset.bin") {
		t.Fatalf("git lfs locks said %q", out)
	}
	runGit(clone, "lfs", "unlock", "asset.bin")
}
