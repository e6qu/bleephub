package gitstore

import (
	"fmt"
	"io"
	"os"

	"github.com/go-git/go-git/v5/plumbing/format/packfile"
	"github.com/go-git/go-git/v5/plumbing/storer"
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
// So the pack is kept: spooled to local disk as it arrives, uploaded as an
// index, a filter and a pack, and then made part of the repository by a commit
// of the manifest that names it (commit.go). Until that commit the three objects
// are an upload nothing refers to. The spool is what makes two things possible. The pack's name is the hash of its contents, unknown until
// the last byte; and whether the pack can be stored as it stands is unknown
// until it has been parsed.
//
// THIN PACKS. A stock git client pushes a thin pack: its deltas may name bases
// the client knows the server already has and therefore left out. Such a pack
// cannot be stored as it arrived, because a pack in the repository must be
// readable on its own. It is completed as git completes one, while it is
// indexed (indexpack.go): the bases it left out are read from the repository
// and appended to it. The pushed bytes are kept, nothing is re-encoded, and
// there is still no per-object request.

var _ storer.PackfileWriter = (*atomicRefStorer)(nil)

// PackfileWriter receives a pushed packfile on the local-directory and memory
// backends, which have no pack tier of their own to publish into: the pack is
// parsed into the storage as it streams in, which is what go-git does for a
// storage without this method.
func (s *atomicRefStorer) PackfileWriter() (io.WriteCloser, error) {
	return s.newStreamingIngest(), nil
}

// PackfileWriter receives a packfile and makes it part of the repository when it
// is closed: a commit of its own, for the callers that write objects and
// references one call at a time — imports, the API, the wiki. A push goes
// through BeginPush instead, so that its pack and its references are one commit.
func (r *repository) PackfileWriter() (io.WriteCloser, error) {
	return r.newPackIngest(func(uploaded *uploadedPack) error {
		return r.commitPack(uploaded, packSourceWrite)
	})
}

// newPackIngest starts receiving a pack. uploaded is called once the pack is in
// the store, and not at all for a packfile that held no objects.
func (r *repository) newPackIngest(uploaded func(*uploadedPack) error) (*packIngest, error) {
	spool, built, err := r.stagePack("ingest-*.pack")
	if err != nil {
		return nil, err
	}
	reader, writer := io.Pipe()
	ingest := &packIngest{repository: r, spool: spool, built: built, quarantine: noQuarantine, uploaded: uploaded,
		scan: writer, layout: make(chan packLayout, 1)}
	go func() {
		layout := readPackLayout(reader)
		// Unblock a writer that is still sending once the read has failed.
		_ = reader.CloseWithError(layout.err)
		ingest.layout <- layout
	}()
	return ingest, nil
}

// commitPack makes an uploaded pack live, and asks for a compaction if that
// leaves the repository due one.
func (r *repository) commitPack(uploaded *uploadedPack, source string) error {
	state, err := r.commits.commit(func(d *draft) error { return d.addPack(uploaded.entry(source)) })
	if err != nil {
		r.manifests.withdraw(uploaded.stored.name)
		return err
	}
	r.notePackWritten(len(state.packs))
	return nil
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
// The first pass of indexing it (indexpack.go) reads it as it arrives.
type packIngest struct {
	repository *repository
	spool      *os.File
	built      *builtPack
	wrote      int64
	// quarantine are packs of the same push that are not live yet, which a thin
	// pack arriving after them may lean on.
	quarantine *quarantine
	uploaded   func(*uploadedPack) error
	// scan feeds the first pass over the arriving pack, which reports on layout.
	scan   *io.PipeWriter
	layout chan packLayout
}

func (i *packIngest) Write(p []byte) (int, error) {
	n, err := i.spool.Write(p)
	i.wrote += int64(n)
	if err != nil {
		return n, err
	}
	if _, err := i.scan.Write(p[:n]); err != nil {
		return n, fmt.Errorf("read pushed pack: %w", err)
	}
	return n, nil
}

func (i *packIngest) Close() error {
	defer i.built.cleanup()
	defer func() { _ = i.spool.Close() }()
	_ = i.scan.Close()
	layout := <-i.layout
	if i.wrote == 0 {
		// The caller reports an empty packfile; there is nothing to publish.
		return nil
	}
	bases := &quarantineView{repository: i.repository, quarantine: i.quarantine}
	if err := i.built.describe(i.spool, layout, bases); err != nil {
		return fmt.Errorf("store pushed pack: %w", err)
	}
	if i.built.objects == 0 {
		return nil
	}

	uploaded, err := i.repository.uploadPack(i.repository.shared.baseContext(), i.built)
	if err != nil {
		return err
	}
	return i.uploaded(uploaded)
}
