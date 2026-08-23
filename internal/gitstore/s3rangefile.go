package gitstore

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"
	"github.com/go-git/go-billy/v5"
)

// isImmutablePackKey reports whether a repository-relative path names a
// content-addressed pack artefact. Only these paths are read through ranges and
// only these are admitted to the disk cache, because only these carry their own
// content hash in their name and therefore cannot change under a cached copy.
//
// The `.pack` and `.idx` names are git's; `.bfilter` is the membership filter
// this package writes beside them, named for the same pack hash.
func isImmutablePackKey(name string) bool {
	dir, base := path.Split(path.Clean(name))
	if strings.TrimSuffix(dir, "/") != path.Join("objects", "pack") {
		return false
	}
	if !strings.HasPrefix(base, "pack-") {
		return false
	}
	switch path.Ext(base) {
	case ".pack", ".idx", ".bfilter":
		return true
	default:
		return false
	}
}

// s3RangeFile reads one immutable object store key through fixed-size chunks,
// fetching each chunk with a ranged GET the first time it is touched and
// serving it from the local disk cache thereafter.
//
// This is the read amplification fix. Before packing, one git object was one
// whole-object GET; after packing, an object lives at a byte offset inside a
// pack that may be gigabytes long, and reading it must cost the bytes of the
// object rather than the bytes of the pack. A ranged GET is what makes packing
// a win instead of a catastrophe.
type s3RangeFile struct {
	fs        *S3FS
	name      string
	key       string
	chunkSize int64

	mu    sync.Mutex
	pos   int64
	size  int64
	sized bool
	// chunks memoizes this handle's own fetches. go-git opens a packfile,
	// seeks and reads within it, and closes it, many times per request; the
	// process-wide disk cache is what carries chunks between handles, and this
	// map is what stops one handle re-reading the disk for every object it
	// decodes out of the same chunk.
	chunks map[int64][]byte
}

func newS3RangeFile(fs *S3FS, name string) *s3RangeFile {
	return &s3RangeFile{
		fs:        fs,
		name:      name,
		key:       fs.key(name),
		chunkSize: fs.shared().chunkSize,
		chunks:    map[int64][]byte{},
	}
}

func (f *s3RangeFile) Name() string { return f.name }

// open proves the key exists and fetches its leading chunk. Doing both in one
// ranged GET rather than a HEAD followed by a read is what keeps the small
// artefacts — an index, a membership filter — at a single round trip, and the
// leading chunk of a packfile is wanted immediately anyway.
func (f *s3RangeFile) open() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, err := f.chunkAtLocked(0); err != nil {
		return err
	}
	return nil
}

// lengthLocked reports the object's total size, which is known as soon as any
// chunk has been fetched because a ranged response carries it.
func (f *s3RangeFile) lengthLocked() (int64, error) {
	if f.sized {
		return f.size, nil
	}
	if size, ok := f.fs.cachedObjectSize(f.key); ok {
		f.size, f.sized = size, true
		return f.size, nil
	}
	if _, err := f.chunkAtLocked(0); err != nil {
		return 0, err
	}
	return f.size, nil
}

// chunkAtLocked returns one chunk, consulting this handle, then the shared disk
// cache, then the object store.
func (f *s3RangeFile) chunkAtLocked(index int64) ([]byte, error) {
	if data, ok := f.chunks[index]; ok {
		return data, nil
	}
	cache := sharedPackDiskCache()
	if data := cache.load(f.fs.bucket, f.key, f.chunkSize, index); data != nil {
		f.chunks[index] = data
		if !f.sized {
			if size, ok := f.fs.cachedObjectSize(f.key); ok {
				f.size, f.sized = size, true
			} else if size, ok := cache.loadSize(f.fs.bucket, f.key, f.chunkSize); ok {
				f.size, f.sized = size, true
				f.fs.rememberObjectSize(f.key, size)
			}
		}
		if f.sized {
			return data, nil
		}
		// A chunk with no recorded length is not usable: every read is bounded
		// by the object's length, and treating an unknown length as zero would
		// silently truncate the packfile. Fall through and fetch the extent,
		// which reports the length in its response.
	}

	start := index * f.chunkSize
	if f.sized && start >= f.size {
		return nil, io.EOF
	}
	end := start + f.chunkSize

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	resp, err := f.fs.client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(f.fs.bucket),
		Key:    aws.String(f.key),
		Range:  aws.String("bytes=" + strconv.FormatInt(start, 10) + "-" + strconv.FormatInt(end-1, 10)),
	})
	if err != nil {
		if isRangeBeyondEnd(err) {
			return nil, io.EOF
		}
		return nil, translateS3NotFound(err, "s3 ranged get", f.key)
	}
	data, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil {
		return nil, fmt.Errorf("s3 ranged read %s: %w", f.key, err)
	}
	// The response states the object's total length, so the size never needs
	// a request of its own.
	if total, ok := totalFromContentRange(aws.ToString(resp.ContentRange)); ok {
		f.size, f.sized = total, true
		f.fs.rememberObjectSize(f.key, total)
	} else if !f.sized {
		f.size, f.sized = start+int64(len(data)), true
		f.fs.rememberObjectSize(f.key, f.size)
	}
	f.chunks[index] = data
	cache.store(f.fs.bucket, f.key, f.chunkSize, index, data)
	if f.sized {
		cache.storeSize(f.fs.bucket, f.key, f.chunkSize, f.size)
	}
	return data, nil
}

// totalFromContentRange reads the object length out of a "bytes a-b/total"
// response header.
func totalFromContentRange(header string) (int64, bool) {
	_, total, found := strings.Cut(header, "/")
	if !found {
		return 0, false
	}
	parsed, err := strconv.ParseInt(strings.TrimSpace(total), 10, 64)
	if err != nil {
		return 0, false
	}
	return parsed, true
}

// isRangeBeyondEnd recognises a request for bytes past the end of the object,
// which is the ordinary way a sequential read discovers it has finished.
func isRangeBeyondEnd(err error) bool {
	var apiErr smithy.APIError
	if errors.As(err, &apiErr) {
		return apiErr.ErrorCode() == "InvalidRange"
	}
	return false
}

func (f *s3RangeFile) readAtLocked(p []byte, off int64) (int, error) {
	size, err := f.lengthLocked()
	if err != nil {
		return 0, err
	}
	if off >= size {
		return 0, io.EOF
	}
	read := 0
	for read < len(p) && off+int64(read) < size {
		position := off + int64(read)
		index := position / f.chunkSize
		chunk, err := f.chunkAtLocked(index)
		if err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return read, err
		}
		within := position - index*f.chunkSize
		if within >= int64(len(chunk)) {
			break
		}
		copied := copy(p[read:], chunk[within:])
		read += copied
	}
	if read < len(p) {
		return read, io.EOF
	}
	return read, nil
}

func (f *s3RangeFile) Read(p []byte) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	n, err := f.readAtLocked(p, f.pos)
	f.pos += int64(n)
	if n > 0 && errors.Is(err, io.EOF) {
		return n, nil
	}
	return n, err
}

func (f *s3RangeFile) ReadAt(p []byte, off int64) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.readAtLocked(p, off)
}

func (f *s3RangeFile) Seek(offset int64, whence int) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var pos int64
	switch whence {
	case io.SeekStart:
		pos = offset
	case io.SeekCurrent:
		pos = f.pos + offset
	case io.SeekEnd:
		size, err := f.lengthLocked()
		if err != nil {
			return 0, err
		}
		pos = size + offset
	default:
		return 0, errors.New("invalid whence")
	}
	if pos < 0 {
		return 0, errors.New("negative seek position")
	}
	f.pos = pos
	return pos, nil
}

// Close releases this handle's memoized chunks. The chunks themselves stay in
// the shared cache, which is what carries them to the next handle: go-git opens
// and closes a packfile once per object it decodes, so a Close that dropped the
// bytes for good would make the cache useless.
func (f *s3RangeFile) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.chunks = map[int64][]byte{}
	return nil
}

func (f *s3RangeFile) Write(p []byte) (int, error) {
	return 0, &os.PathError{Op: "write", Path: f.name, Err: os.ErrPermission}
}

func (f *s3RangeFile) Truncate(size int64) error {
	return &os.PathError{Op: "truncate", Path: f.name, Err: os.ErrPermission}
}

// Lock and Unlock are no-ops because the keys this handle serves are
// content-addressed and immutable: there is no writer to exclude.
func (f *s3RangeFile) Lock() error   { return nil }
func (f *s3RangeFile) Unlock() error { return nil }

var _ billy.File = (*s3RangeFile)(nil)
var _ io.ReaderAt = (*s3RangeFile)(nil)

// translateS3NotFound maps only a definite absence to os.ErrNotExist. Every
// other failure stays an error: go-git reads a missing pack as "this pack does
// not exist" and a transient outage reported that way would silently narrow a
// clone to the objects that happened to be reachable.
func translateS3NotFound(err error, op, key string) error {
	var noSuchKey *s3types.NoSuchKey
	if errors.As(err, &noSuchKey) {
		return os.ErrNotExist
	}
	var notFound *s3types.NotFound
	if errors.As(err, &notFound) {
		return os.ErrNotExist
	}
	return fmt.Errorf("%s %s: %w", op, key, err)
}
