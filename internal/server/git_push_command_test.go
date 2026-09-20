package bleephub

import (
	"bytes"
	"crypto/rand"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/protocol/packp"
	"github.com/go-git/go-git/v5/storage/memory"

	"github.com/e6qu/bleephub/gitstore"
)

func TestGitPushCommandsHonorWireOldObjectID(t *testing.T) {
	stor := gitstore.WrapAtomicRefStorage("owner/repo", memory.NewStorage())
	name := plumbing.NewBranchReferenceName("main")
	current := plumbing.NewHash("2222222222222222222222222222222222222222")
	if err := stor.SetReference(plumbing.NewHashReference(name, current)); err != nil {
		t.Fatal(err)
	}
	stale := plumbing.NewHash("1111111111111111111111111111111111111111")
	next := plumbing.NewHash("3333333333333333333333333333333333333333")
	for _, command := range []*packp.Command{
		{Name: name, Old: stale, New: next},
		{Name: name, Old: stale, New: plumbing.ZeroHash},
		{Name: name, Old: plumbing.ZeroHash, New: next},
	} {
		if err := applyPushCommandAtomic(stor, command); err == nil {
			t.Fatalf("%s with stale/duplicate precondition unexpectedly succeeded", command.Action())
		}
		got, err := stor.Reference(name)
		if err != nil {
			t.Fatal(err)
		}
		if got.Hash() != current {
			t.Fatalf("%s changed ref to %s, want %s", command.Action(), got.Hash(), current)
		}
	}
}

// TestAStockGitPushLargerThanThePostBufferLands pins the request git makes
// before a large push. A pack that outgrows http.postBuffer (1 MiB unless the
// user raised it) is streamed with chunked encoding, which cannot be replayed if
// the server asks for credentials part way — so git first POSTs a lone flush
// packet to receive-pack to settle authentication, and only then the push. A
// server that answers the probe as a malformed push (it carries no commands)
// fails every push over a mebibyte, and none under it, so nothing small notices.
func TestAStockGitPushLargerThanThePostBufferLands(t *testing.T) {
	t.Parallel()
	git := requireGitCLI(t)
	srv := newIsolatedServer(t)
	const name = "large-push"
	seedGitShallowRepo(t, srv.Server, name)

	cloneURL := strings.Replace(srv.baseURL, "://", "://admin:"+defaultToken+"@", 1) + "/admin/" + name + ".git"
	root := t.TempDir()
	clone := filepath.Join(root, "clone")
	git.run(root, "clone", cloneURL, clone)
	git.run(clone, "config", "user.name", "Large Pusher")
	git.run(clone, "config", "user.email", "large@bleephub.invalid")

	// Incompressible, so the pack is as large as the file: three times the
	// default post buffer. The premise is checked rather than assumed.
	payload := make([]byte, 3<<20)
	if _, err := rand.Read(payload); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(clone, "large.bin"), payload, 0o600); err != nil {
		t.Fatal(err)
	}
	git.run(clone, "add", "large.bin")
	git.run(clone, "commit", "-m", "a large file")
	pushed := strings.TrimSpace(git.run(clone, "rev-parse", "HEAD"))
	git.run(clone, "push", "origin", "HEAD:main")

	verify := filepath.Join(root, "verify")
	git.run(root, "clone", cloneURL, verify)
	if got := strings.TrimSpace(git.run(verify, "rev-parse", "HEAD")); got != pushed {
		t.Fatalf("a fresh clone is at %s, want the pushed %s", got, pushed)
	}
	landed, err := os.ReadFile(filepath.Join(verify, "large.bin"))
	if err != nil || !bytes.Equal(landed, payload) {
		t.Fatalf("the large file did not survive the round trip (err %v, %d bytes)", err, len(landed))
	}
}

// TestAReceivePackProbeIsAnsweredWithAnEmptyResult is the same contract at the
// wire: a body holding only a flush packet is git asking whether it may push,
// and the answer is an empty receive-pack result, as git-http-backend gives.
func TestAReceivePackProbeIsAnsweredWithAnEmptyResult(t *testing.T) {
	t.Parallel()
	srv := newIsolatedServer(t)
	const name = "probe"
	seedGitShallowRepo(t, srv.Server, name)

	post := func(token string) *http.Response {
		request, err := http.NewRequest(http.MethodPost, srv.baseURL+"/admin/"+name+".git/git-receive-pack", strings.NewReader("0000"))
		if err != nil {
			t.Fatal(err)
		}
		request.Header.Set("Content-Type", "application/x-git-receive-pack-request")
		if token != "" {
			request.SetBasicAuth("admin", token)
		}
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = response.Body.Close() })
		return response
	}

	response := post(defaultToken)
	body, _ := io.ReadAll(response.Body)
	if response.StatusCode != http.StatusOK || len(body) != 0 {
		t.Fatalf("probe answered %d with %q, want 200 and nothing", response.StatusCode, body)
	}
	if got := response.Header.Get("Content-Type"); got != "application/x-git-receive-pack-result" {
		t.Fatalf("probe content type %q", got)
	}
	// What the probe exists to find out: a caller who may not push is told so
	// here, before it streams anything.
	if response := post(""); response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("an anonymous probe answered %d, want 401", response.StatusCode)
	}
}
