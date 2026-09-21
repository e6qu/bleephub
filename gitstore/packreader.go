package gitstore

import (
	"errors"
	"fmt"
	"io"
	"os"
	"sync"

	"github.com/go-git/go-billy/v5"
)

// packExtents reads one pack artefact — a pack, its index or its membership
// filter — through fixed-size extents, fetching each with a ranged GET on first
// touch and serving it from the local cache after. This is the
// read-amplification fix: an object at a byte offset inside a multi-gigabyte
// pack must cost its own bytes, not the pack's.
//
// It is sound only because such a key names the hash of its own contents: what
// is cached under it can never be stale, so the cache needs no invalidation and
// survives a restart with no validation step. The size comes from the listing
// that discovered the artefact, so nothing is ever asked of the store but bytes.
type packExtents struct {
	shared *storeShared
	key    string
	size   int64
}

// extent returns one extent, from the shared cache or the object store.
// Concurrent fetches of one extent are one request.
func (e packExtents) extent(index int64) ([]byte, error) {
	chunkSize := e.shared.opts.ChunkBytes
	bucket := e.shared.bucket.Name()
	cache := e.shared.packCache()
	if data := cache.load(bucket, e.key, chunkSize, index); data != nil {
		return data, nil
	}
	start := index * chunkSize
	if start >= e.size {
		return nil, io.EOF
	}
	length := min(chunkSize, e.size-start)
	flight := fmt.Sprintf("%s\x00%s\x00%d\x00%d", bucket, e.key, chunkSize, index)
	fetched, err, _ := e.shared.chunkFetch.Do(flight, func() (any, error) {
		// The base context, not a caller's: the fetch is shared, and one
		// reader going away must not fail the others waiting on it.
		data, err := e.shared.getRange(e.shared.baseContext(), e.key, start, length)
		if err != nil {
			return nil, err
		}
		if int64(len(data)) != length {
			return nil, fmt.Errorf("read %s: extent %d is %d bytes, want %d", e.key, index, len(data), length)
		}
		cache.store(bucket, e.key, chunkSize, index, data)
		return data, nil
	})
	if err != nil {
		return nil, err
	}
	// The bytes are shared read-only between every reader the fetch served.
	return fetched.([]byte), nil
}

// readRange returns n bytes of the artefact from off.
func (e packExtents) readRange(off, n int64) ([]byte, error) {
	if off < 0 || n < 0 || off+n > e.size {
		return nil, fmt.Errorf("read %s: range %d+%d is outside its %d bytes", e.key, off, n, e.size)
	}
	chunkSize := e.shared.opts.ChunkBytes
	data := make([]byte, 0, n)
	for position := off; position < off+n; {
		index := position / chunkSize
		extent, err := e.extent(index)
		if err != nil {
			return nil, err
		}
		within := position - index*chunkSize
		if within >= int64(len(extent)) {
			return nil, fmt.Errorf("read %s: extent %d holds %d bytes, short of offset %d", e.key, index, len(extent), within)
		}
		part := extent[within:min(int64(len(extent)), within+off+n-position)]
		data = append(data, part...)
		position += int64(len(part))
	}
	return data, nil
}

// packFile is a positioned reader over one pack: the billy.File go-git's
// packfile decoder wants, and the PackReader a caller copying stored pack bytes
// wants. It is an adapter around the extents, not a file in any filesystem.
// One is made for each call that reads a pack, because a decoder is not safe to
// share between goroutines; what they share is the extent cache underneath.
type packFile struct {
	extents packExtents
	name    string

	mu  sync.Mutex
	pos int64
	// lastIndex and last memoize the extent most recently touched, so decoding
	// many objects out of one extent does not go back to the shared cache, and
	// its lock, for each of them.
	lastIndex int64
	last      []byte
	// failed is the first error the store gave this handle. See failure.
	failed error
}

func newPackFile(extents packExtents, name string) *packFile {
	return &packFile{extents: extents, name: name, lastIndex: -1}
}

func (f *packFile) Name() string { return f.name }

func (f *packFile) extentLocked(index int64) ([]byte, error) {
	if index == f.lastIndex {
		return f.last, nil
	}
	data, err := f.extents.extent(index)
	if err != nil {
		if f.failed == nil && !errors.Is(err, io.EOF) {
			f.failed = err
		}
		return nil, err
	}
	f.lastIndex, f.last = index, data
	return data, nil
}

func (f *packFile) readAtLocked(p []byte, off int64) (int, error) {
	size := f.extents.size
	if off < 0 {
		return 0, errors.New("negative read offset")
	}
	if off >= size {
		return 0, io.EOF
	}
	chunkSize := f.extents.shared.opts.ChunkBytes
	read := 0
	for read < len(p) && off+int64(read) < size {
		position := off + int64(read)
		index := position / chunkSize
		extent, err := f.extentLocked(index)
		if err != nil {
			return read, err
		}
		within := position - index*chunkSize
		if within >= int64(len(extent)) {
			// A cached extent shorter than the listing says it must be would
			// otherwise read as a truncated pack.
			return read, fmt.Errorf("read %s: extent %d holds %d bytes, short of offset %d", f.extents.key, index, len(extent), within)
		}
		read += copy(p[read:], extent[within:])
	}
	if read < len(p) {
		return read, io.EOF
	}
	return read, nil
}

func (f *packFile) Read(p []byte) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	n, err := f.readAtLocked(p, f.pos)
	f.pos += int64(n)
	if n > 0 && errors.Is(err, io.EOF) {
		return n, nil
	}
	return n, err
}

func (f *packFile) ReadAt(p []byte, off int64) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.readAtLocked(p, off)
}

func (f *packFile) Seek(offset int64, whence int) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var pos int64
	switch whence {
	case io.SeekStart:
		pos = offset
	case io.SeekCurrent:
		pos = f.pos + offset
	case io.SeekEnd:
		pos = f.extents.size + offset
	default:
		return 0, errors.New("invalid whence")
	}
	if pos < 0 {
		return 0, errors.New("negative seek position")
	}
	f.pos = pos
	return pos, nil
}

// failure returns the first error the object store gave this handle, or nil. A
// decoder reading through the handle reports a failed read in terms of its own,
// which need not say whether the store was down or the pack was gone; this does.
func (f *packFile) failure() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.failed
}

// Close releases this handle's memoized extent; the bytes stay in the shared cache.
func (f *packFile) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lastIndex, f.last = -1, nil
	return nil
}

func (f *packFile) Write([]byte) (int, error) {
	return 0, &os.PathError{Op: "write", Path: f.name, Err: os.ErrPermission}
}

func (f *packFile) Truncate(int64) error {
	return &os.PathError{Op: "truncate", Path: f.name, Err: os.ErrPermission}
}

// Lock and Unlock are no-ops: a pack is immutable, so there is no writer to exclude.
func (f *packFile) Lock() error   { return nil }
func (f *packFile) Unlock() error { return nil }

var _ billy.File = (*packFile)(nil)
var _ PackReader = (*packFile)(nil)
