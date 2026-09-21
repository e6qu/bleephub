package gitstore

import (
	"errors"
	"fmt"
	"io"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/format/packfile"
)

// streamedObjectBytes is the size above which a whole object stored in a pack is
// handed out as a stream rather than as bytes in memory. A repository may hold a
// blob of any size, a server reads on behalf of anyone who asks, and an object
// returned in memory costs its whole size in heap for every reader of it at
// once. Below this, holding the bytes is cheaper than reading them twice, and
// the object cache can keep them.
const streamedObjectBytes = 8 << 20

// streamedObject is a large object that is stored whole in a pack: not as a
// delta, so its content is one zlib stream at a known offset, which can be
// inflated as it is read. It holds no content. Every Reader opens the pack for
// itself, through the extent cache all readers share, so a streamed object may
// be read by many goroutines at once and any number of times.
//
// A large object stored as a delta is still rebuilt in memory, as it is by
// every git implementation: applying a delta needs its base to hand.
type streamedObject struct {
	pack   *storedPack
	offset int64
	hash   plumbing.Hash
	kind   plumbing.ObjectType
	size   int64
}

var _ plumbing.EncodedObject = (*streamedObject)(nil)

func (o *streamedObject) Hash() plumbing.Hash       { return o.hash }
func (o *streamedObject) Type() plumbing.ObjectType { return o.kind }
func (o *streamedObject) Size() int64               { return o.size }

// SetType and SetSize do nothing: the object is what the pack says it is.
func (o *streamedObject) SetType(plumbing.ObjectType) {}
func (o *streamedObject) SetSize(int64)               {}

// Writer refuses: a stored object is not written to.
func (o *streamedObject) Writer() (io.WriteCloser, error) {
	return nil, fmt.Errorf("object %s is stored and cannot be written", o.hash)
}

// Reader inflates the object from the pack as it is read.
func (o *streamedObject) Reader() (io.ReadCloser, error) {
	file := newPackFile(o.pack.pack, o.pack.name+".pack")
	scanner := packfile.NewScanner(file)
	if _, err := scanner.SeekObjectHeader(o.offset); err != nil {
		return nil, streamFailure(file, fmt.Errorf("object %s: %w", o.hash, err))
	}
	content, err := scanner.ReadObject()
	if err != nil {
		return nil, streamFailure(file, fmt.Errorf("object %s: %w", o.hash, err))
	}
	return &streamedContent{content: content, file: file}, nil
}

// streamedContent is one reading of a streamed object. The scanner that found
// the object is deliberately not closed, here or in streamable: go-git's
// Scanner.Close drains its reader to the end, which is right for a pipe and,
// over a pack in an object store, downloads the rest of the pack. It holds
// nothing else to release.
type streamedContent struct {
	content io.ReadCloser
	file    *packFile
}

func (c *streamedContent) Read(p []byte) (int, error) {
	n, err := c.content.Read(p)
	if err != nil && !errors.Is(err, io.EOF) {
		err = streamFailure(c.file, err)
	}
	return n, err
}

func (c *streamedContent) Close() error {
	return errors.Join(c.content.Close(), c.file.Close())
}

// streamFailure reports what the store said in place of what the decoder made
// of it, for the reason packHandle.failure gives: an outage must be heard as an
// outage, never as a corrupt or missing object.
func streamFailure(file *packFile, err error) error {
	if stored := file.failure(); stored != nil {
		return stored
	}
	return err
}

// streamable reports the header of the object at offset if it is one to stream:
// stored whole, and larger than streamedObjectBytes. It reads the header through
// a handle of its own, so the decoder's position is not disturbed; the extent it
// touches is the one the decoder would read next, so it costs no request.
func streamable(pack *storedPack, offset int64) (*packfile.ObjectHeader, bool, error) {
	file := newPackFile(pack.pack, pack.name+".pack")
	scanner := packfile.NewScanner(file)
	defer func() { _ = file.Close() }()
	header, err := scanner.SeekObjectHeader(offset)
	if err != nil {
		return nil, false, streamFailure(file, err)
	}
	return header, !header.Type.IsDelta() && header.Length > streamedObjectBytes, nil
}
