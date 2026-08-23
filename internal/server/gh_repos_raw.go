package bleephub

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/e6qu/bleephub/internal/store"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/plumbing/object"
	gitStorage "github.com/go-git/go-git/v5/storage"
)

// Raw file contents. Every content-bearing REST response advertises a
// download_url/raw_url of the shape {base}/{owner}/{repo}/raw/{ref}/{path} --
// the contents API, commit and pull-request file lists, and compare all build
// one. github.com answers those from raw.githubusercontent.com; without a
// route for that shape here every URL bleephub handed out was a dead 404, so a
// README image, a `curl $(gh api ... --jq .download_url)`, or any CI script
// fetching a file by its advertised link failed even though the API response
// itself looked correct.

// tryHandleRawRequest serves /{owner}/{repo}/raw/{ref}/{path} from the
// catch-all, beside the git smart HTTP protocol and the legacy archives.
// Returns true when the request was a raw file download.
func (s *Server) tryHandleRawRequest(w http.ResponseWriter, r *http.Request) bool {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		return false
	}
	parts := strings.SplitN(strings.TrimPrefix(r.URL.Path, "/"), "/", 4)
	if len(parts) < 4 || parts[2] != "raw" {
		return false
	}
	s.serveRawFile(w, r, parts[0], parts[1], parts[3])
	return true
}

func (s *Server) serveRawFile(w http.ResponseWriter, r *http.Request, owner, name, refAndPath string) {
	repo := s.store.GetRepo(owner, name)
	if repo == nil {
		http.NotFound(w, r)
		return
	}
	ctx, _ := s.authenticateGitRequest(r)
	if repo.Private && !s.viewerCanReadRepo(ctx, repo) {
		http.NotFound(w, r)
		return
	}
	stor := s.store.GetGitStorage(owner, name)
	if stor == nil {
		http.NotFound(w, r)
		return
	}

	tree, path := resolveRawTreeAndPath(stor, refAndPath)
	if tree == nil || path == "" {
		http.NotFound(w, r)
		return
	}
	entry, err := tree.FindEntry(path)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	// A directory has no raw representation, and a gitlink names a commit that
	// lives in another repository's object store entirely.
	if entry.Mode == filemode.Dir || entry.Mode == filemode.Submodule {
		http.NotFound(w, r)
		return
	}
	// The blob is streamed rather than read into memory: a raw request names
	// one file, and that file may be far larger than the memory a server can
	// afford to spend on one response.
	blob, size, err := store.OpenGitBlob(stor, entry.Hash)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer blob.Close()

	// git decides text-versus-binary from the start of the content, so the
	// content type only needs the leading bytes and the rest can go straight
	// out without ever being held.
	sniff := make([]byte, gitRawSniffBytes)
	read, err := io.ReadFull(blob, sniff)
	if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
		http.NotFound(w, r)
		return
	}
	sniff = sniff[:read]

	// raw.githubusercontent.com serves everything as an opaque download rather
	// than letting the browser interpret it: a raw .html or .svg rendered
	// in-origin would be stored XSS against any viewer of an untrusted
	// repository. text/plain plus nosniff is what github.com sends.
	w.Header().Set("Content-Type", rawContentType(sniff))
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Length", strconv.FormatInt(size, 10))
	w.WriteHeader(http.StatusOK)
	if r.Method == http.MethodHead {
		return
	}
	if _, err := w.Write(sniff); err != nil {
		s.logger.Debug().Err(err).Msg("raw file write failed")
		return
	}
	if _, err := io.Copy(w, blob); err != nil {
		s.logger.Debug().Err(err).Msg("raw file write failed")
	}
}

// gitRawSniffBytes is how much of a blob decides its content type. git's own
// binary heuristic reads the start of the content and no further.
const gitRawSniffBytes = 8000

// resolveRawTreeAndPath splits "{ref}/{path}" at every boundary a ref could
// end at and returns the tree for the first ref that resolves. Both halves can
// contain slashes -- "feature/x/README.md" is a file in branch "feature/x" or
// a file "x/README.md" in branch "feature" -- so the split is ambiguous and
// has to be probed. Longest ref first: it resolves both readings correctly
// whenever only one of them names a real ref, and prefers the more specific
// branch when somehow both do.
func resolveRawTreeAndPath(stor gitStorage.Storer, refAndPath string) (*object.Tree, string) {
	segments := strings.Split(strings.Trim(refAndPath, "/"), "/")
	for cut := len(segments) - 1; cut >= 1; cut-- {
		ref := strings.Join(segments[:cut], "/")
		hash, err := store.ResolveGitRef(stor, ref)
		if err != nil {
			continue
		}
		commit, err := object.GetCommit(stor, hash)
		if err != nil {
			continue
		}
		tree, err := commit.Tree()
		if err != nil {
			continue
		}
		return tree, strings.Join(segments[cut:], "/")
	}
	return nil, ""
}

// rawContentType mirrors raw.githubusercontent.com: text/plain for text,
// application/octet-stream once the bytes stop being valid text. A NUL byte is
// git's own heuristic for "binary" and is the one used here.
func rawContentType(content []byte) string {
	if bytes.IndexByte(content, 0) >= 0 {
		return "application/octet-stream"
	}
	return "text/plain; charset=utf-8"
}
