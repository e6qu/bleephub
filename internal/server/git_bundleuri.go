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
// A bundle is a repository's refs and a packfile of everything they reach, in
// one file. A client that has one can unbundle it, record the tips it carried
// and then negotiate the rest with an ordinary fetch — so the bulk of a clone
// arrives from the object store and only what has happened since the bundle was
// made travels through this server.
//
// WHAT MAKES THE BUNDLE CORRECT
//
// It is built out of the same enumeration a full clone is built from: the tips
// of every reference, the objects they reach, and the packfile that carries
// them, which the pack-reuse path assembles from the stored packs. So a bundle
// is exactly what a clone at that instant would have delivered, and a client
// that unbundles it holds a complete, connected object graph — nothing is
// promised to it that the fetch that follows would not also have supplied.
//
// It goes stale the moment a reference moves, and its name says so: the key is
// derived from the exact set of tips it covers, so a repository whose refs have
// changed has no bundle under the name its current tips hash to, and the next
// request builds one. Nothing has to remember when a bundle was made or compare
// it against anything — the name is the comparison — and a bundle no longer
// named by any live ref state is swept once it is older than any URL that could
// still be pointing at it.
//
// The access model is the one packfile-uris uses: a presigned GET, minted only
// after the caller has passed the repository's read gate, expiring shortly
// after. git_packuris.go carries that argument in full.

// gitBundleURICommand is the protocol v2 command that serves a bundle list, and
// the capability that advertises it.
const gitBundleURICommand = "bundle-uri"

// gitBundleDirectory is where a repository's bundles live, beside the packs
// they are built from.
const gitBundleDirectory = "objects/bundle"

// gitBundleExtension is the suffix of a bundle key.
const gitBundleExtension = ".bundle"

// gitBundleRetention is how long a bundle no longer named by the current ref
// state is kept before it is swept.
//
// A URL handed out just before a push lands still has gitPackURIExpiry left to
// run against a bundle that is stale the instant it was issued, so the floor is
// that expiry; the margin above it is what keeps a client that started its
// download at the last permitted moment from having the object removed out from
// under it mid-transfer.
const gitBundleRetention = time.Hour

// gitBundleListVersion is the version of the bundle-list format this server
// speaks, and the only one git defines.
const gitBundleListVersion = 1

// gitBundleTip is one reference a bundle carries.
type gitBundleTip struct {
	name string
	hash plumbing.Hash
}

// serveGitBundleURIV2 answers the bundle-uri command with the bundles a client
// may bootstrap from.
//
// A repository with no references has nothing to bootstrap from, and one whose
// packs have no address — a memory or local-directory backend — has no bundle a
// client could fetch; both are answered with an empty list, which the protocol
// defines as "no bundles" rather than as a failure.
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

// gitBundleTips lists the references a bundle carries, sorted by name so the
// same repository state always produces the same bundle.
//
// HEAD is listed alongside the references under refs/, which is what `git
// bundle create --all` does and what makes the bundle a repository on its own:
// a client cloning straight from it has no other way to learn which branch to
// check out. The bundle-uri client ignores it — it turns only refs/heads/ into
// the refs/bundles/ entries it negotiates from — so naming it costs that path
// nothing.
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

// gitBundleID names a bundle by the ref state it covers.
//
// Every object a bundle carries is reachable from its tips, so two bundles over
// the same tips carry the same objects and one name is enough to decide whether
// the stored bundle still describes the repository. A digest rather than the
// list itself because the name is a key in the object store, and a repository
// with many references has a ref list far longer than a key may be.
func gitBundleID(tips []gitBundleTip) string {
	digest := sha256.New()
	for _, tip := range tips {
		_, _ = fmt.Fprintf(digest, "%s %s\n", tip.hash.String(), tip.name)
	}
	return hex.EncodeToString(digest.Sum(nil))
}

// gitBundlePublications serializes the publication of one bundle key inside
// this process, and reference-counts its entries so an idle key costs nothing.
//
// The keys are what make it necessary. A bundle is named by the ref state it
// covers, so every client that clones between two pushes wants the same one,
// and without a gate a burst of them would each read the repository and write
// the same object back — the cost of publishing multiplied by the number of
// people who happened to arrive before the first publication finished.
var gitBundlePublications = struct {
	mu    sync.Mutex
	gates map[string]*gitBundleGate
}{gates: map[string]*gitBundleGate{}}

// gitBundleGate is one key's gate and the number of callers holding a reference
// to it.
type gitBundleGate struct {
	mu      sync.Mutex
	waiting int
}

// publishGitBundleOnce publishes a bundle unless another caller in this process
// is already publishing that exact one, in which case it waits and takes the
// bundle that caller published.
//
// The object store is re-consulted after the wait rather than before, because
// what the waiter is waiting for is precisely the key appearing; a caller that
// finds it there has nothing left to do, and one that does not — the publisher
// failed, or the object was swept between the two — publishes it itself.
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

// publishGitBundle builds the bundle for a ref state and writes it to the
// object store under its own name.
//
// The bundle is streamed rather than assembled: the pack it ends with is the
// repository's, and holding a repository in memory to hand it to the uploader
// would make the cost of serving a bundle the size of the repository rather
// than the size of a buffer.
func publishGitBundle(ctx context.Context, stor storer.Storer, packDir *gitstore.S3FS, name string, tips []gitBundleTip) error {
	reader, writer := io.Pipe()
	go func() {
		_ = writer.CloseWithError(writeGitBundle(writer, stor, tips))
	}()
	err := packDir.PutStream(ctx, name, reader)
	// Closing the read end unblocks a writer that is still producing bytes the
	// upload will never take, which is what keeps a failed upload from leaving
	// the goroutine parked on a pipe nobody reads.
	_ = reader.CloseWithError(err)
	if err != nil {
		return fmt.Errorf("publish bundle %s: %w", name, err)
	}
	return nil
}

// gitBundleSignature opens a version 2 bundle.
const gitBundleSignature = "# v2 git bundle\n"

// writeGitBundle writes a bundle: the signature, the reference each object id
// is reached by, a blank line, and the packfile of everything they reach.
//
// The pack is built by the ordinary fetch path, so it is the same pack a clone
// of this repository would receive — including the stored entry regions the
// reuse path copies rather than re-encodes. The bundle lists no prerequisites,
// which is what makes it usable by a client holding nothing at all.
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

// pruneGitBundles removes the bundles a repository no longer has a use for:
// everything but the one just published, once it is older than any URL that
// could still be pointing at it.
//
// A failure to remove one is not a failure of the request that found it. The
// bundle it could not delete is an object in a store that already holds the
// repository, the next publication tries again, and the client waiting on this
// answer has nothing to gain from being told.
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
