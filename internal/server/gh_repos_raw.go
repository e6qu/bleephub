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
// download_url/raw_url of shape {base}/{owner}/{repo}/raw/{ref}/{path}; without
// this route every such URL bleephub handed out would be a dead 404.

// tryHandleRawRequest serves /{owner}/{repo}/raw/{ref}/{path} from the
// catch-all, returning true when the request was a raw file download.
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
	// A directory has no raw form; a gitlink names a commit in another repo's
	// object store.
	if entry.Mode == filemode.Dir || entry.Mode == filemode.Submodule {
		http.NotFound(w, r)
		return
	}
	// Stream the blob rather than buffer it: one file may exceed the memory
	// affordable for a single response.
	blob, size, err := store.OpenGitBlob(stor, entry.Hash)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer blob.Close()

	// git decides text-versus-binary from the leading bytes, so only those are
	// buffered; the rest streams straight out.
	sniff := make([]byte, gitRawSniffBytes)
	read, err := io.ReadFull(blob, sniff)
	if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
		http.NotFound(w, r)
		return
	}
	sniff = sniff[:read]

	// Serve as an opaque download (text/plain + nosniff, as github.com does): a
	// browser-rendered raw .html or .svg would be stored XSS against viewers of
	// an untrusted repo.
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

// gitRawSniffBytes is how much of a blob decides its content type, matching
// git's own binary heuristic.
const gitRawSniffBytes = 8000

// resolveRawTreeAndPath splits "{ref}/{path}" at every possible boundary and
// returns the tree for the first ref that resolves. Both halves may contain
// slashes, so the split is ambiguous and is probed longest-ref-first, which
// prefers the more specific branch when two readings both name real refs.
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

// rawContentType returns text/plain for text and octet-stream otherwise, using
// git's NUL-byte heuristic for "binary".
func rawContentType(content []byte) string {
	if bytes.IndexByte(content, 0) >= 0 {
		return "application/octet-stream"
	}
	return "text/plain; charset=utf-8"
}
