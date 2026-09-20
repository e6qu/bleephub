package gitstore

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/go-git/go-billy/v5/osfs"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/cache"
	"github.com/go-git/go-git/v5/plumbing/format/packfile"
	"github.com/go-git/go-git/v5/plumbing/storer"
	gitFilesystem "github.com/go-git/go-git/v5/storage/filesystem"
)

// A push arrives as a packfile, and on object storage it must stay one.
//
// go-git hands an incoming pack to storage through storer.PackfileWriter when
// the storage offers it, and otherwise parses the pack and stores each object
// separately. On a disk the second path is merely slower. On an object store it
// turns one upload into several requests per object — a probe, a staged write,
// a copy onto the final name, a delete — so a push of a few thousand objects
// costs tens of thousands of round trips, and leaves a loose tier every read
// pays for one GET at a time until a compaction packs it again.
//
// So the pack is kept: spooled to local disk as it arrives, then published
// through the same index → filter → pack sequence compaction uses, with the
// same crash-safety argument (see compact.go). The spool is what makes two
// things possible. The pack's name is the hash of its contents, unknown until
// the last byte; and whether the pack can be stored as it stands is unknown
// until it has been parsed.
//
// THIN PACKS. A stock git client pushes a thin pack: its deltas may name bases
// the client knows the server already has and therefore left out. Such a pack
// cannot be stored verbatim, because a pack in the repository must be readable
// on its own. It is completed instead: parsed against the repository, which
// resolves every delta to a whole object (reading only the few bases it names),
// then re-encoded as a self-contained pack of exactly the objects pushed. That
// costs CPU in proportion to the push, and still no per-object request.

// packIngestTimeout bounds the publication of one pushed pack. Like the
// compaction timeout it is generous: a ceiling on a wedged upload, not a pace.
const packIngestTimeout = 30 * time.Minute

var _ storer.PackfileWriter = (*atomicRefStorer)(nil)

// PackfileWriter receives a pushed packfile. On object storage the pack is
// published as a pack; on the other backends it is parsed into the storage as
// it streams in, which is what go-git does for a storage without this method.
func (s *atomicRefStorer) PackfileWriter() (io.WriteCloser, error) {
	if s.fs == nil {
		return s.newStreamingIngest(), nil
	}
	spool, built, err := s.stagePack("ingest-*.pack")
	if err != nil {
		return nil, err
	}
	return &packIngest{storer: s, spool: spool, built: built}, nil
}

// streamingIngest parses a pack into the storage while it is still arriving, so
// nothing is spooled and memory stays bounded by the parser's own window.
type streamingIngest struct {
	writer *io.PipeWriter
	done   chan error
}

func (s *atomicRefStorer) newStreamingIngest() *streamingIngest {
	reader, writer := io.Pipe()
	ingest := &streamingIngest{writer: writer, done: make(chan error, 1)}
	go func() {
		err := parsePackInto(reader, s)
		// Unblock a writer that is still sending once the parse has failed.
		_ = reader.CloseWithError(err)
		ingest.done <- err
	}()
	return ingest
}

func (i *streamingIngest) Write(p []byte) (int, error) { return i.writer.Write(p) }

func (i *streamingIngest) Close() error {
	_ = i.writer.Close()
	return <-i.done
}

func parsePackInto(pack io.Reader, into storer.EncodedObjectStorer) error {
	parser, err := packfile.NewParserWithStorage(packfile.NewScanner(pack), into)
	if err != nil {
		return err
	}
	_, err = parser.Parse()
	return err
}

// packIngest spools a pushed pack and publishes it when the push has sent it all.
type packIngest struct {
	storer *atomicRefStorer
	spool  *os.File
	built  *builtPack
	wrote  int64
}

func (i *packIngest) Write(p []byte) (int, error) {
	n, err := i.spool.Write(p)
	i.wrote += int64(n)
	return n, err
}

func (i *packIngest) Close() error {
	defer i.built.cleanup()
	defer func() { _ = i.spool.Close() }()
	if i.wrote == 0 {
		// The caller reports an empty packfile; there is nothing to publish.
		return nil
	}

	ready := i.built
	if err := i.built.describe(i.spool); err != nil {
		completed, completeErr := i.storer.completeThinPack(i.spool)
		if completeErr != nil {
			// Whatever was wrong with the pack, the parse against the
			// repository is the authoritative account of it.
			return fmt.Errorf("store pushed pack: %w", completeErr)
		}
		defer completed.cleanup()
		ready = completed
	}
	if ready.objects == 0 {
		return nil
	}

	ctx, cancel := context.WithTimeout(i.storer.fs.baseContext(), packIngestTimeout)
	defer cancel()
	if err := i.storer.publishPack(ctx, ready); err != nil {
		return err
	}
	i.storer.adoptPack(ready)
	i.storer.notePackWritten()
	return nil
}

// completeThinPack turns a pack that leans on objects already in the repository
// into a self-contained one holding exactly the objects pushed.
func (s *atomicRefStorer) completeThinPack(spool *os.File) (*builtPack, error) {
	staging, err := s.compactionScratchDir()
	if err != nil {
		return nil, err
	}
	scratchDir, err := os.MkdirTemp(staging, "ingest-objects-*")
	if err != nil {
		return nil, fmt.Errorf("stage pushed objects: %w", err)
	}
	defer func() { _ = os.RemoveAll(scratchDir) }()

	// The pushed objects land on local disk, not in memory: a push has no size
	// limit, and nothing here may hold one whole in the heap.
	scratch := gitFilesystem.NewStorage(osfs.New(scratchDir), cache.NewObjectLRUDefault())
	if _, err := spool.Seek(0, io.SeekStart); err != nil {
		return nil, fmt.Errorf("rewind pushed pack: %w", err)
	}
	if err := parsePackInto(spool, &thinBaseOverlay{Storage: scratch, repository: s}); err != nil {
		return nil, err
	}

	var hashes []plumbing.Hash
	objects, err := scratch.IterEncodedObjects(plumbing.AnyObject)
	if err != nil {
		return nil, err
	}
	if err := objects.ForEach(func(object plumbing.EncodedObject) error {
		hashes = append(hashes, object.Hash())
		return nil
	}); err != nil {
		return nil, err
	}
	if len(hashes) == 0 {
		return &builtPack{}, nil
	}
	return s.buildPackFrom(scratch, hashes)
}

// thinBaseOverlay is where a thin pack is parsed: every object the pack carries
// is written to the scratch storage, and a delta base the pack left out is read
// from the repository.
type thinBaseOverlay struct {
	*gitFilesystem.Storage
	repository storer.EncodedObjectStorer
}

func (o *thinBaseOverlay) EncodedObject(kind plumbing.ObjectType, hash plumbing.Hash) (plumbing.EncodedObject, error) { //nolint:ireturn
	object, err := o.Storage.EncodedObject(kind, hash)
	if errors.Is(err, plumbing.ErrObjectNotFound) {
		return o.repository.EncodedObject(kind, hash)
	}
	return object, err
}

func (o *thinBaseOverlay) HasEncodedObject(hash plumbing.Hash) error {
	if err := o.Storage.HasEncodedObject(hash); !errors.Is(err, plumbing.ErrObjectNotFound) {
		return err
	}
	return o.repository.HasEncodedObject(hash)
}

func (o *thinBaseOverlay) EncodedObjectSize(hash plumbing.Hash) (int64, error) {
	size, err := o.Storage.EncodedObjectSize(hash)
	if errors.Is(err, plumbing.ErrObjectNotFound) {
		return o.repository.EncodedObjectSize(hash)
	}
	return size, err
}
