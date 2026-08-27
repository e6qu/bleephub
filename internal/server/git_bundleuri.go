package bleephub

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/e6qu/bleephub/internal/gitstore"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/format/pktline"
	"github.com/go-git/go-git/v5/plumbing/storer"
)

// bundle-uri: bootstrapping a clone from a bundle the client fetches itself.
//
// A bundle is a repository's refs plus a packfile of everything they reach. A
// client unbundles it, records the tips, and negotiates only the rest with an
// ordinary fetch, so the bulk of a clone arrives from the object store.
//
// The bundle is built from the same enumeration as a full clone, so it is exactly
// what a clone at that instant would deliver. Its name is derived from the exact
// set of tips it covers, so a repository whose refs moved has no bundle under its
// current name and the next request builds one — the name is the staleness check.
//
// Access mirrors packfile-uris: a presigned GET minted only after the caller
// passed the repository's read gate, expiring shortly after (see git_packuris.go).

// gitBundleURICommand is the protocol v2 command that serves a bundle list.
const gitBundleURICommand = "bundle-uri"

// gitBundleDirectory is where a repository's bundles live.
const gitBundleDirectory = "objects/bundle"

// gitBundleExtension is the suffix of a bundle key.
const gitBundleExtension = ".bundle"

// gitBundleRetention is how long a stale bundle is kept before sweeping. The
// floor is gitPackURIExpiry — a URL minted just before a push still runs that
// long against a now-stale bundle — plus a margin so a download started at the
// last permitted moment is not removed mid-transfer.
const gitBundleRetention = time.Hour

// gitBundleListVersion is the bundle-list format version, the only one git defines.
const gitBundleListVersion = 1

// gitBundleTip is one reference a bundle carries.
type gitBundleTip struct {
	name string
	hash plumbing.Hash
}

// serveGitBundleURIV2 answers the bundle-uri command. A repository with no refs,
// or one whose packs have no address (memory/local backend), gets an empty list —
// the protocol's "no bundles" rather than a failure.
func serveGitBundleURIV2(ctx context.Context, stor storer.Storer, arguments []string, out io.Writer) error {
	if len(arguments) > 0 {
		return fmt.Errorf("bundle-uri: unexpected argument: '%s'", arguments[0])
	}
	encoder := pktline.NewEncoder(out)
	packDir, addressable := gitPackDirOf(stor).(*gitstore.S3FS)
	if !addressable {
		return encoder.Flush()
	}
	tips, err := gitBundleTips(stor)
	if err != nil {
		return err
	}
	if len(tips) == 0 {
		return encoder.Flush()
	}

	id := gitBundleID(tips)
	name := packDir.Join(gitBundleDirectory, "bundle-"+id+gitBundleExtension)
	if _, err := packDir.Stat(name); err != nil {
		if err := publishGitBundleOnce(ctx, stor, packDir, name, tips); err != nil {
			return err
		}
	}
	uri, err := packDir.PresignedGetURL(ctx, name, gitPackURIExpiry)
	if err != nil {
		return err
	}

	for _, line := range []string{
		fmt.Sprintf("bundle.version=%d\n", gitBundleListVersion),
		"bundle.mode=all\n",
		fmt.Sprintf("bundle.%s.uri=%s\n", id, uri),
	} {
		if err := encoder.EncodeString(line); err != nil {
			return err
		}
	}
	return encoder.Flush()
}

// gitBundleTips lists the references a bundle carries, sorted by name for
// determinism. HEAD is listed alongside refs/ (as `git bundle create --all` does)
// so a client cloning straight from the bundle knows which branch to check out.
func gitBundleTips(stor storer.Storer) ([]gitBundleTip, error) {
	iter, err := stor.IterReferences()
	if err != nil {
		return nil, err
	}
	var tips []gitBundleTip
	if err := iter.ForEach(func(ref *plumbing.Reference) error {
		if ref.Type() != plumbing.HashReference || ref.Hash().IsZero() {
			return nil
		}
		if !strings.HasPrefix(ref.Name().String(), "refs/") {
			return nil
		}
		tips = append(tips, gitBundleTip{name: ref.Name().String(), hash: ref.Hash()})
		return nil
	}); err != nil {
		return nil, err
	}
	if head, err := storer.ResolveReference(stor, plumbing.HEAD); err == nil && head != nil && !head.Hash().IsZero() {
		tips = append(tips, gitBundleTip{name: plumbing.HEAD.String(), hash: head.Hash()})
	}
	sort.Slice(tips, func(i, j int) bool { return tips[i].name < tips[j].name })
	return tips, nil
}

// gitBundleID names a bundle by the ref state it covers. Two bundles over the same
// tips carry the same objects, so the name alone decides whether the stored bundle
// still describes the repository. A digest, not the list, because the name is an
// object-store key and a ref list can exceed a key's length.
func gitBundleID(tips []gitBundleTip) string {
	digest := sha256.New()
	for _, tip := range tips {
		_, _ = fmt.Fprintf(digest, "%s %s\n", tip.hash.String(), tip.name)
	}
	return hex.EncodeToString(digest.Sum(nil))
}

// gitBundlePublications serializes publication of one bundle key within this
// process and reference-counts entries so an idle key costs nothing. Without the
// gate, a burst of clients between two pushes would each read the repository and
// write back the same object.
var gitBundlePublications = struct {
	mu    sync.Mutex
	gates map[string]*gitBundleGate
}{gates: map[string]*gitBundleGate{}}

// gitBundleGate is one key's gate and its holder count.
type gitBundleGate struct {
	mu      sync.Mutex
	waiting int
}

// publishGitBundleOnce publishes a bundle unless another caller in this process is
// already publishing that exact one, in which case it waits and takes theirs. The
// store is re-consulted after the wait: a waiter that finds the key is done, one
// that does not (the publisher failed, or it was swept) publishes it itself.
func publishGitBundleOnce(ctx context.Context, stor storer.Storer, packDir *gitstore.S3FS, name string, tips []gitBundleTip) error {
	gitBundlePublications.mu.Lock()
	gate := gitBundlePublications.gates[name]
	if gate == nil {
		gate = &gitBundleGate{}
		gitBundlePublications.gates[name] = gate
	}
	gate.waiting++
	gitBundlePublications.mu.Unlock()

	gate.mu.Lock()
	defer func() {
		gate.mu.Unlock()
		gitBundlePublications.mu.Lock()
		gate.waiting--
		if gate.waiting == 0 {
			delete(gitBundlePublications.gates, name)
		}
		gitBundlePublications.mu.Unlock()
	}()

	if _, err := packDir.Stat(name); err == nil {
		return nil
	}
	if err := publishGitBundle(ctx, stor, packDir, name, tips); err != nil {
		return err
	}
	pruneGitBundles(packDir, name)
	return nil
}

// publishGitBundle builds the bundle for a ref state and writes it to the object
// store. It streams rather than buffering, so serving a bundle costs a buffer
// rather than the size of the repository.
func publishGitBundle(ctx context.Context, stor storer.Storer, packDir *gitstore.S3FS, name string, tips []gitBundleTip) error {
	reader, writer := io.Pipe()
	go func() {
		_ = writer.CloseWithError(writeGitBundle(writer, stor, tips))
	}()
	err := packDir.PutStream(ctx, name, reader)
	// Closing the read end unblocks a writer still producing bytes the upload
	// will never take, so a failed upload does not park the goroutine on a dead pipe.
	_ = reader.CloseWithError(err)
	if err != nil {
		return fmt.Errorf("publish bundle %s: %w", name, err)
	}
	return nil
}

// gitBundleSignature opens a version 2 bundle.
const gitBundleSignature = "# v2 git bundle\n"

// writeGitBundle writes a bundle: signature, the ref each object id is reached by,
// a blank line, and the packfile of everything they reach. The pack comes from the
// ordinary fetch path (same as a clone would receive), and the bundle lists no
// prerequisites, so a client holding nothing can use it.
func writeGitBundle(out io.Writer, stor storer.Storer, tips []gitBundleTip) error {
	if _, err := io.WriteString(out, gitBundleSignature); err != nil {
		return err
	}
	wants := make([]plumbing.Hash, 0, len(tips))
	for _, tip := range tips {
		if _, err := fmt.Fprintf(out, "%s %s\n", tip.hash.String(), tip.name); err != nil {
			return err
		}
		wants = append(wants, tip.hash)
	}
	if _, err := io.WriteString(out, "\n"); err != nil {
		return err
	}

	request := &gitUploadRequest{wants: wants, done: true, noProgress: true}
	boundary, err := gitFetchBoundaryFor(stor, request)
	if err != nil {
		return err
	}
	plan, err := gitObjectsToSend(stor, boundary, request, nil)
	if err != nil {
		return err
	}
	return writeGitPackfile(newGitBandWriter(out, gitSidebandNone, false), stor, plan, false)
}

// pruneGitBundles removes every bundle but the one just published, once it is
// older than any URL that could still point at it. A failed removal is not this
// request's failure: the next publication retries.
func pruneGitBundles(packDir *gitstore.S3FS, keep string) {
	entries, err := packDir.ReadDir(gitBundleDirectory)
	if err != nil {
		return
	}
	cutoff := time.Now().Add(-gitBundleRetention)
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), gitBundleExtension) {
			continue
		}
		name := packDir.Join(gitBundleDirectory, entry.Name())
		if name == keep || !entry.ModTime().Before(cutoff) {
			continue
		}
		_ = packDir.Remove(name)
	}
}
