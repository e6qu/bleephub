package gitstore

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/go-git/go-billy/v5"
	"github.com/google/uuid"
)

type S3FS struct {
	client   *s3.Client
	bucket   string
	prefix   string
	activeMu sync.Mutex
	active   *s3ActiveFiles
	locks    *s3KeyLocks
	sharedV  *s3Shared
}

// s3Shared is the state one filesystem and every chroot derived from it hold in
// common, alongside the staging namespace and the key locks.
type s3Shared struct {
	mu sync.Mutex
	// sizes memoizes the byte length of content-addressed pack artefacts. The
	// name of such a key contains the hash of its own contents, so a remembered
	// size can never go stale, and remembering it turns the HEAD that opens a
	// packfile into a one-off rather than a per-handle cost.
	sizes map[string]int64
	// objects holds the per-repository object membership index consulted before
	// any loose-object read.
	objects map[string]*repoObjectIndex
	// chunkSize is the extent size the pack read path uses, resolved once so
	// that opening a packfile does not consult the environment.
	chunkSize int64
}

func newS3Shared() *s3Shared {
	return &s3Shared{
		sizes:     map[string]int64{},
		objects:   map[string]*repoObjectIndex{},
		chunkSize: packChunkSize(),
	}
}

func (f *S3FS) shared() *s3Shared {
	f.activeMu.Lock()
	defer f.activeMu.Unlock()
	if f.sharedV == nil {
		f.sharedV = newS3Shared()
	}
	return f.sharedV
}

func (f *S3FS) cachedObjectSize(key string) (int64, bool) {
	shared := f.shared()
	shared.mu.Lock()
	defer shared.mu.Unlock()
	size, ok := shared.sizes[key]
	return size, ok
}

func (f *S3FS) rememberObjectSize(key string, size int64) {
	shared := f.shared()
	shared.mu.Lock()
	defer shared.mu.Unlock()
	shared.sizes[key] = size
}

func (f *S3FS) forgetObjectSize(key string) {
	shared := f.shared()
	shared.mu.Lock()
	defer shared.mu.Unlock()
	delete(shared.sizes, key)
}

// Client exposes the underlying S3 client for object-byte stores that share
// this filesystem's connection.
func (f *S3FS) Client() *s3.Client { return f.client }

// Bucket reports the bucket this filesystem stores objects in.
func (f *S3FS) Bucket() string { return f.bucket }

// Prefix reports the key prefix all of this filesystem's objects live under.
func (f *S3FS) Prefix() string { return f.prefix }

// GitObjectLocker grants exclusive use of one object-store key across every
// replica. Amazon S3 has no advisory locking, so go-git's compare-and-set on
// refs and its packed-refs rewrite borrow the same durable store that already
// serializes the rest of the shared state.
type GitObjectLocker interface {
	AcquireLock(name, owner string, ttl time.Duration) (bool, error)
	ReleaseLock(name, owner string) error
}

var (
	gitObjectLockerMu sync.RWMutex
	gitObjectLockerV  GitObjectLocker
)

// SetGitObjectLocker installs the durable lock manager. Until one is
// installed there is no shared durable state, so no other replica can be
// writing the same objects and the process-local lock is the whole lock.
func SetGitObjectLocker(l GitObjectLocker) {
	gitObjectLockerMu.Lock()
	defer gitObjectLockerMu.Unlock()
	gitObjectLockerV = l
}

func currentGitObjectLocker() GitObjectLocker {
	gitObjectLockerMu.RLock()
	defer gitObjectLockerMu.RUnlock()
	return gitObjectLockerV
}

// ClearGitObjectLocker uninstalls l if it is the currently installed lock
// manager. A closed locker cannot arbitrate git object locks; leaving it
// installed would fail every ref update.
func ClearGitObjectLocker(l GitObjectLocker) {
	gitObjectLockerMu.Lock()
	defer gitObjectLockerMu.Unlock()
	if gitObjectLockerV == l {
		gitObjectLockerV = nil
	}
}

const (
	gitObjectLockTTL  = 2 * time.Minute
	gitObjectLockWait = 30 * time.Second
	gitObjectLockPoll = 50 * time.Millisecond
)

// s3KeyLocks serializes writers of one bucket-absolute object key inside this
// process. Entries are reference counted so an idle key costs nothing.
type s3KeyLocks struct {
	mu    sync.Mutex
	slots map[string]*s3KeySlot
}

type s3KeySlot struct {
	ch   chan struct{}
	refs int
}

func newS3KeyLocks() *s3KeyLocks {
	return &s3KeyLocks{slots: map[string]*s3KeySlot{}}
}

func (l *s3KeyLocks) acquire(key string, wait time.Duration) error {
	l.mu.Lock()
	slot := l.slots[key]
	if slot == nil {
		slot = &s3KeySlot{ch: make(chan struct{}, 1)}
		l.slots[key] = slot
	}
	slot.refs++
	l.mu.Unlock()

	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case slot.ch <- struct{}{}:
		return nil
	case <-timer.C:
		l.drop(key)
		return fmt.Errorf("lock %s: another writer in this process still holds it after %s", key, wait)
	}
}

func (l *s3KeyLocks) release(key string) {
	l.mu.Lock()
	slot := l.slots[key]
	l.mu.Unlock()
	if slot == nil {
		return
	}
	<-slot.ch
	l.drop(key)
}

func (l *s3KeyLocks) drop(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	slot := l.slots[key]
	if slot == nil {
		return
	}
	slot.refs--
	if slot.refs <= 0 {
		delete(l.slots, key)
	}
}

func NewS3FS(ctx context.Context, endpoint, bucket, prefix string) (*S3FS, error) {
	var opts []func(*awsconfig.LoadOptions) error
	opts = append(opts, awsconfig.WithRegion(bleephubS3Region()))
	cfg, err := awsconfig.LoadDefaultConfig(ctx, opts...)
	if err != nil {
		return nil, fmt.Errorf("s3 config: %w", err)
	}

	clientOpts := []func(*s3.Options){}
	if endpoint != "" {
		clientOpts = append(clientOpts, func(o *s3.Options) {
			o.BaseEndpoint = aws.String(endpoint)
			o.UsePathStyle = true
		})
	}
	client := s3.NewFromConfig(cfg, clientOpts...)

	return &S3FS{
		client: client,
		bucket: bucket,
		prefix: prefix,
		active: &s3ActiveFiles{files: map[string]*s3FileState{}},
		locks:  newS3KeyLocks(),
	}, nil
}

// bleephubS3Region selects the real AWS region for durable Git and service
// bytes. BLEEPHUB_S3_REGION is an explicit storage coordinate; ECS supplies
// AWS_REGION automatically, and the final default preserves local simulator
// compatibility.
func bleephubS3Region() string {
	if region := strings.TrimSpace(os.Getenv("BLEEPHUB_S3_REGION")); region != "" {
		return region
	}
	if region := strings.TrimSpace(os.Getenv("AWS_REGION")); region != "" {
		return region
	}
	return "us-east-1"
}

func (f *S3FS) key(p string) string {
	return path.Join(f.prefix, p)
}

func (f *S3FS) Create(filename string) (billy.File, error) {
	return f.newActiveFile(filename, nil), nil
}

func (f *S3FS) Open(filename string) (billy.File, error) {
	if state := f.activeFile(filename); state != nil {
		return &s3File{fs: f, name: filename, state: state}, nil
	}
	// A pack or index is opened, seeked and closed once per object decoded, so
	// reading it whole would transfer the entire pack for every object in it.
	// These keys are read through ranges instead, which is only sound because
	// their names are content addresses: see isImmutablePackKey.
	if isImmutablePackKey(filename) {
		file := newS3RangeFile(f, filename)
		if err := file.open(); err != nil {
			return nil, err
		}
		return file, nil
	}
	if absent, ok := f.looseObjectAbsent(filename); ok && absent {
		return nil, os.ErrNotExist
	}
	key := f.key(filename)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	resp, err := f.client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(f.bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		var nsk *s3types.NoSuchKey
		if errors.As(err, &nsk) {
			return nil, os.ErrNotExist
		}
		return nil, fmt.Errorf("s3 get %s: %w", key, err)
	}

	data, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil {
		return nil, fmt.Errorf("s3 read %s: %w", key, err)
	}

	return &s3File{fs: f, name: filename, state: &s3FileState{data: data}}, nil
}

func (f *S3FS) OpenFile(filename string, flag int, perm os.FileMode) (billy.File, error) {
	if flag&(os.O_CREATE|os.O_TRUNC) == os.O_CREATE|os.O_TRUNC && flag&os.O_EXCL == 0 {
		return f.Create(filename)
	}

	file, err := f.Open(filename)
	switch {
	case err == nil:
		if flag&(os.O_CREATE|os.O_EXCL) == os.O_CREATE|os.O_EXCL {
			return nil, &os.PathError{Op: "open", Path: filename, Err: os.ErrExist}
		}
	case errors.Is(err, os.ErrNotExist) && flag&os.O_CREATE != 0:
		file, err = f.Create(filename)
		if err != nil {
			return nil, err
		}
	default:
		return nil, err
	}

	sf, ok := file.(*s3File)
	if !ok {
		return nil, fmt.Errorf("OpenFile %s: unexpected file type %T", filename, file)
	}
	if flag&os.O_TRUNC != 0 {
		sf = f.newActiveFile(filename, nil)
	}
	if flag&os.O_APPEND != 0 {
		sf.state.mu.Lock()
		sf.pos = len(sf.state.data)
		sf.state.mu.Unlock()
	}
	sf.writer = flag&(os.O_WRONLY|os.O_RDWR) != 0
	return sf, nil
}

// The staging map is shared by a filesystem and every chroot derived from it,
// so entries are keyed by the bucket-absolute object key. A chroot-relative
// key such as "config" names a different object in every repository, and one
// repository's reader would otherwise be handed another repository's bytes.
func (f *S3FS) activeFile(filename string) *s3FileState {
	active := f.activeFiles()
	active.mu.Lock()
	defer active.mu.Unlock()
	return active.files[f.key(filename)]
}

func (f *S3FS) newActiveFile(filename string, data []byte) *s3File {
	state := &s3FileState{data: data, dirty: true}
	active := f.activeFiles()
	active.mu.Lock()
	active.files[f.key(filename)] = state
	active.mu.Unlock()
	return &s3File{fs: f, name: filename, state: state, writer: true}
}

func (f *S3FS) removeActiveFile(filename string, state *s3FileState) {
	active := f.activeFiles()
	key := f.key(filename)
	active.mu.Lock()
	if active.files[key] == state {
		delete(active.files, key)
	}
	active.mu.Unlock()
}

func (f *S3FS) activeFiles() *s3ActiveFiles {
	f.activeMu.Lock()
	defer f.activeMu.Unlock()
	if f.active != nil {
		return f.active
	}
	f.active = &s3ActiveFiles{files: map[string]*s3FileState{}}
	return f.active
}

func (f *S3FS) Stat(filename string) (os.FileInfo, error) {
	if state := f.activeFile(filename); state != nil {
		state.mu.Lock()
		size := int64(len(state.data))
		state.mu.Unlock()
		return &s3FileInfo{
			name:    path.Base(filename),
			size:    size,
			mode:    0o644,
			modTime: time.Time{},
		}, nil
	}

	key := f.key(filename)
	if isImmutablePackKey(filename) {
		if size, ok := f.cachedObjectSize(key); ok {
			return &s3FileInfo{name: path.Base(filename), size: size, mode: 0o644}, nil
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	resp, err := f.client.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket: aws.String(f.bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		var nsk *s3types.NoSuchKey
		if errors.As(err, &nsk) {
			return nil, os.ErrNotExist
		}
		var nfe *s3types.NotFound
		if errors.As(err, &nfe) {
			return nil, os.ErrNotExist
		}
		return nil, fmt.Errorf("s3 head %s: %w", key, err)
	}

	size := aws.ToInt64(resp.ContentLength)
	if isImmutablePackKey(filename) {
		f.rememberObjectSize(key, size)
	}
	return &s3FileInfo{
		name:    path.Base(filename),
		size:    size,
		mode:    0o644,
		modTime: aws.ToTime(resp.LastModified),
		isDir:   false,
	}, nil
}

func (f *S3FS) Rename(oldpath, newpath string) error {
	srcKey := f.key(oldpath)
	dstKey := f.key(newpath)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	_, err := f.client.CopyObject(ctx, &s3.CopyObjectInput{
		Bucket:     aws.String(f.bucket),
		Key:        aws.String(dstKey),
		CopySource: aws.String(f.bucket + "/" + srcKey),
	})
	if err != nil {
		return fmt.Errorf("s3 copy %s -> %s: %w", srcKey, dstKey, err)
	}

	_, err = f.client.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: aws.String(f.bucket),
		Key:    aws.String(srcKey),
	})
	if err == nil {
		// dotgit lands a loose object by renaming a temporary file onto its
		// final name, so this is where the object index learns that this
		// process created it.
		f.noteLooseRemoved(oldpath)
		f.noteLooseWrite(newpath)
	}
	if err != nil {
		// A rename that returns an error must leave the destination absent.
		// Otherwise a caller retry observes two names and may treat the copy as
		// a committed move. Report both failures if compensation also fails.
		_, rollbackErr := f.client.DeleteObject(ctx, &s3.DeleteObjectInput{
			Bucket: aws.String(f.bucket),
			Key:    aws.String(dstKey),
		})
		if rollbackErr != nil {
			return fmt.Errorf("s3 delete %s after copy: %w (rollback destination %s: %v)", srcKey, err, dstKey, rollbackErr)
		}
		return fmt.Errorf("s3 delete %s after copy (destination rolled back): %w", srcKey, err)
	}

	return nil
}

func (f *S3FS) Remove(filename string) error {
	key := f.key(filename)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	_, err := f.client.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: aws.String(f.bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		return fmt.Errorf("s3 delete %s: %w", key, err)
	}
	f.noteLooseRemoved(filename)
	f.forgetObjectSize(key)
	return nil
}

func (f *S3FS) Join(elem ...string) string {
	return path.Join(elem...)
}

func (f *S3FS) TempFile(dir, prefix string) (billy.File, error) {
	name := path.Join(dir, prefix+uuid.New().String())
	return f.Create(name)
}

func (f *S3FS) ReadDir(dirname string) ([]os.FileInfo, error) {
	prefix := f.key(dirname)
	if !strings.HasSuffix(prefix, "/") {
		prefix += "/"
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	var contents []s3types.Object
	var commonPrefixes []s3types.CommonPrefix
	var continuation *string
	for {
		resp, err := f.client.ListObjectsV2(ctx, &s3.ListObjectsV2Input{
			Bucket:            aws.String(f.bucket),
			Prefix:            aws.String(prefix),
			Delimiter:         aws.String("/"),
			ContinuationToken: continuation,
		})
		if err != nil {
			return nil, fmt.Errorf("s3 list %s: %w", prefix, err)
		}
		contents = append(contents, resp.Contents...)
		commonPrefixes = append(commonPrefixes, resp.CommonPrefixes...)
		if !aws.ToBool(resp.IsTruncated) {
			break
		}
		continuation = resp.NextContinuationToken
	}

	entriesByName := map[string]os.FileInfo{}
	baseLen := len(f.prefix)
	if f.prefix != "" {
		baseLen++
	}

	for _, obj := range contents {
		key := aws.ToString(obj.Key)
		if len(key) <= baseLen {
			continue
		}
		relKey := key[baseLen:]
		info := &s3FileInfo{
			name:    path.Base(relKey),
			size:    aws.ToInt64(obj.Size),
			mode:    0o644,
			modTime: aws.ToTime(obj.LastModified),
			isDir:   false,
		}
		entriesByName[info.name] = info
	}

	for _, cp := range commonPrefixes {
		p := aws.ToString(cp.Prefix)
		if len(p) <= baseLen {
			continue
		}
		relKey := p[baseLen:]
		info := &s3FileInfo{
			name:    path.Base(relKey),
			size:    0,
			mode:    0o755 | os.ModeDir,
			modTime: time.Time{},
			isDir:   true,
		}
		entriesByName[info.name] = info
	}

	// S3 cannot list an object until its upload completes. Billy users,
	// notably go-git, expect the filesystem namespace to include an open file,
	// so merge the process-wide staging namespace into the remote listing.
	active := f.activeFiles()
	active.mu.Lock()
	for key, state := range active.files {
		if !strings.HasPrefix(key, prefix) {
			continue
		}
		relative := strings.TrimPrefix(key, prefix)
		if relative == "" {
			continue
		}
		name, rest, _ := strings.Cut(relative, "/")
		if rest != "" {
			entriesByName[name] = &s3FileInfo{name: name, mode: 0o755 | os.ModeDir, isDir: true}
			continue
		}
		state.mu.Lock()
		size := int64(len(state.data))
		state.mu.Unlock()
		entriesByName[name] = &s3FileInfo{name: name, size: size, mode: 0o644}
	}
	active.mu.Unlock()

	entries := make([]os.FileInfo, 0, len(entriesByName))
	for _, entry := range entriesByName {
		entries = append(entries, entry)
	}
	slices.SortFunc(entries, func(a, b os.FileInfo) int {
		if a.IsDir() != b.IsDir() {
			if a.IsDir() {
				return -1
			}
			return 1
		}
		return strings.Compare(a.Name(), b.Name())
	})

	return entries, nil
}

func (f *S3FS) MkdirAll(filename string, perm os.FileMode) error {
	return nil
}

func (f *S3FS) Lstat(filename string) (os.FileInfo, error) {
	return f.Stat(filename)
}

func (f *S3FS) Symlink(target, link string) error {
	return billy.ErrNotSupported
}

func (f *S3FS) Readlink(link string) (string, error) {
	return "", billy.ErrNotSupported
}

func (f *S3FS) Chroot(path string) (billy.Filesystem, error) {
	return &S3FS{
		client:  f.client,
		bucket:  f.bucket,
		prefix:  f.key(path),
		active:  f.activeFiles(),
		locks:   f.keyLocks(),
		sharedV: f.shared(),
	}, nil
}

func (f *S3FS) keyLocks() *s3KeyLocks {
	f.activeMu.Lock()
	defer f.activeMu.Unlock()
	if f.locks == nil {
		f.locks = newS3KeyLocks()
	}
	return f.locks
}

func (f *S3FS) Root() string {
	return f.prefix
}

// CopyRepoPrefix copies every object under oldFull's prefix to newFull's,
// leaving the source intact. STORE-013 runs this outside the store lock so both
// prefixes coexist during the copy and readers at the old name keep working;
// the caller purges the old prefix after swapping metadata under the lock.
func (f *S3FS) CopyRepoPrefix(oldFull, newFull string) error {
	oldPrefix := f.key(oldFull) + "/"
	newPrefix := f.key(newFull) + "/"
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	var continuation *string
	var oldKeys []string
	for {
		resp, err := f.client.ListObjectsV2(ctx, &s3.ListObjectsV2Input{
			Bucket:            aws.String(f.bucket),
			Prefix:            aws.String(oldPrefix),
			ContinuationToken: continuation,
		})
		if err != nil {
			return fmt.Errorf("s3 list %s: %w", oldPrefix, err)
		}
		for _, obj := range resp.Contents {
			oldKeys = append(oldKeys, aws.ToString(obj.Key))
		}
		if !aws.ToBool(resp.IsTruncated) {
			break
		}
		continuation = resp.NextContinuationToken
	}
	for _, oldKey := range oldKeys {
		rel := strings.TrimPrefix(oldKey, oldPrefix)
		newKey := newPrefix + rel
		if _, err := f.client.CopyObject(ctx, &s3.CopyObjectInput{
			Bucket:     aws.String(f.bucket),
			CopySource: aws.String(f.bucket + "/" + oldKey),
			Key:        aws.String(newKey),
		}); err != nil {
			return fmt.Errorf("s3 copy %s -> %s: %w", oldKey, newKey, err)
		}
	}
	return nil
}

// RenameRepoPrefix moves an object prefix in place (copy then delete). It is the
// single-shot form used when the move is fast enough to hold the store lock; the
// live-repo path uses CopyRepoPrefix + DeleteRepoPrefix around the metadata swap.
func (f *S3FS) RenameRepoPrefix(oldFull, newFull string) error {
	if err := f.CopyRepoPrefix(oldFull, newFull); err != nil {
		return err
	}
	return f.DeleteRepoPrefix(oldFull)
}

func (f *S3FS) DeleteRepoPrefix(fullName string) error {
	prefix := f.key(fullName) + "/"
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	for {
		resp, err := f.client.ListObjectsV2(ctx, &s3.ListObjectsV2Input{
			Bucket: aws.String(f.bucket),
			Prefix: aws.String(prefix),
		})
		if err != nil {
			return fmt.Errorf("s3 list %s: %w", prefix, err)
		}
		keys := make([]string, 0, len(resp.Contents))
		for _, obj := range resp.Contents {
			keys = append(keys, aws.ToString(obj.Key))
		}
		if len(keys) == 0 {
			return nil
		}
		if err := f.deleteObjectKeys(ctx, keys); err != nil {
			return err
		}
	}
}

func (f *S3FS) deleteObjectKeys(ctx context.Context, keys []string) error {
	const maxDeleteObjects = 1000
	for start := 0; start < len(keys); start += maxDeleteObjects {
		end := min(start+maxDeleteObjects, len(keys))
		objects := make([]s3types.ObjectIdentifier, 0, end-start)
		for _, key := range keys[start:end] {
			objects = append(objects, s3types.ObjectIdentifier{Key: aws.String(key)})
		}
		response, err := f.client.DeleteObjects(ctx, &s3.DeleteObjectsInput{
			Bucket: aws.String(f.bucket),
			Delete: &s3types.Delete{Objects: objects, Quiet: aws.Bool(true)},
		})
		if err != nil {
			return fmt.Errorf("s3 delete %d objects: %w", len(objects), err)
		}
		if len(response.Errors) > 0 {
			first := response.Errors[0]
			return fmt.Errorf("s3 delete %s: %s", aws.ToString(first.Key), aws.ToString(first.Message))
		}
	}
	return nil
}

type s3File struct {
	fs        *S3FS
	name      string
	state     *s3FileState
	pos       int
	closed    bool
	writer    bool
	mu        sync.Mutex
	lockMu    sync.Mutex
	lockOwner string
}

// s3FileState is a request-lifetime staging buffer for one active object
// write. go-git concurrently reads a packfile while it streams the pack into
// it, which Amazon S3 cannot expose until a completed object exists. Readers
// of a live writer share this buffer; after its writer closes, the completed
// bytes are committed to S3 and all subsequent reads use S3 directly.
type s3FileState struct {
	data  []byte
	dirty bool
	mu    sync.Mutex
}

type s3ActiveFiles struct {
	mu    sync.Mutex
	files map[string]*s3FileState
}

func (sf *s3File) Name() string {
	return sf.name
}

func (sf *s3File) Write(p []byte) (n int, err error) {
	sf.mu.Lock()
	defer sf.mu.Unlock()
	if !sf.writer {
		return 0, &os.PathError{Op: "write", Path: sf.name, Err: os.ErrPermission}
	}
	sf.state.mu.Lock()
	defer sf.state.mu.Unlock()
	sf.state.dirty = true
	// Writing past the end zero-fills the gap, matching os.File semantics.
	if sf.pos > len(sf.state.data) {
		sf.state.data = append(sf.state.data, make([]byte, sf.pos-len(sf.state.data))...)
	}
	n = copy(sf.state.data[sf.pos:], p)
	if n < len(p) {
		sf.state.data = append(sf.state.data, p[n:]...)
	}
	sf.pos += len(p)
	return len(p), nil
}

func (sf *s3File) Read(p []byte) (n int, err error) {
	sf.mu.Lock()
	defer sf.mu.Unlock()
	sf.state.mu.Lock()
	defer sf.state.mu.Unlock()
	if sf.pos >= len(sf.state.data) {
		return 0, io.EOF
	}
	n = copy(p, sf.state.data[sf.pos:])
	sf.pos += n
	return n, nil
}

func (sf *s3File) ReadAt(p []byte, off int64) (n int, err error) {
	sf.mu.Lock()
	defer sf.mu.Unlock()
	sf.state.mu.Lock()
	defer sf.state.mu.Unlock()
	if off >= int64(len(sf.state.data)) {
		return 0, io.EOF
	}
	n = copy(p, sf.state.data[off:])
	if n < len(p) {
		err = io.EOF
	}
	return n, err
}

func (sf *s3File) Seek(offset int64, whence int) (int64, error) {
	sf.mu.Lock()
	defer sf.mu.Unlock()
	sf.state.mu.Lock()
	defer sf.state.mu.Unlock()
	var pos int
	switch whence {
	case io.SeekStart:
		pos = int(offset)
	case io.SeekCurrent:
		pos = sf.pos + int(offset)
	case io.SeekEnd:
		pos = len(sf.state.data) + int(offset)
	default:
		return 0, errors.New("invalid whence")
	}
	if pos < 0 {
		return 0, errors.New("negative seek position")
	}
	sf.pos = pos
	return int64(sf.pos), nil
}

// Close flushes and then releases any lock this handle holds. go-git relies on
// that ordering: it takes the lock, writes, and lets the deferred Close both
// commit the bytes and drop the lock, so the next writer cannot observe a
// half-written object.
func (sf *s3File) Close() error {
	sf.mu.Lock()
	if sf.closed {
		sf.mu.Unlock()
		return nil
	}
	sf.closed = true
	var err error
	if sf.writer {
		// The staging entry is dropped even when the flush fails: leaving it
		// behind would serve the bytes of a write that never landed to every
		// later reader of the same key.
		err = sf.flush()
		sf.fs.removeActiveFile(sf.name, sf.state)
	}
	sf.mu.Unlock()

	sf.lockMu.Lock()
	unlockErr := sf.unlockLocked()
	sf.lockMu.Unlock()
	if err != nil {
		return err
	}
	return unlockErr
}

func (sf *s3File) flush() error {
	key := sf.fs.key(sf.name)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	sf.state.mu.Lock()
	data := append([]byte(nil), sf.state.data...)
	dirty := sf.state.dirty
	sf.state.mu.Unlock()
	if !dirty {
		return nil
	}
	_, err := sf.fs.client.PutObject(ctx, &s3.PutObjectInput{
		Bucket: aws.String(sf.fs.bucket),
		Key:    aws.String(key),
		Body:   bytes.NewReader(data),
	})
	if err != nil {
		return fmt.Errorf("s3 put %s: %w", key, err)
	}
	sf.fs.noteLooseWrite(sf.name)
	return nil
}

// Lock takes exclusive ownership of this object key. go-git relies on it for
// the compare-and-set behind every ref update and for the packed-refs rewrite;
// two concurrent pushes that both believed they held it would lose refs.
func (sf *s3File) Lock() error {
	sf.lockMu.Lock()
	defer sf.lockMu.Unlock()
	if sf.lockOwner != "" {
		return fmt.Errorf("lock %s: already locked by this file handle", sf.name)
	}
	key := sf.fs.key(sf.name)
	if err := sf.fs.keyLocks().acquire(key, gitObjectLockWait); err != nil {
		return err
	}
	owner := uuid.New().String()
	if err := sf.acquireDurable(key, owner); err != nil {
		sf.fs.keyLocks().release(key)
		return err
	}
	sf.lockOwner = owner
	return nil
}

func (sf *s3File) acquireDurable(key, owner string) error {
	locker := currentGitObjectLocker()
	if locker == nil {
		return nil
	}
	deadline := time.Now().Add(gitObjectLockWait)
	for {
		acquired, err := locker.AcquireLock(gitObjectLockName(key), owner, gitObjectLockTTL)
		if err != nil {
			return fmt.Errorf("lock %s: %w", key, err)
		}
		if acquired {
			return nil
		}
		if !time.Now().Before(deadline) {
			return fmt.Errorf("lock %s: another replica still holds it after %s", key, gitObjectLockWait)
		}
		time.Sleep(gitObjectLockPoll)
	}
}

func gitObjectLockName(key string) string { return "git-object:" + key }

func (sf *s3File) Unlock() error {
	sf.lockMu.Lock()
	defer sf.lockMu.Unlock()
	return sf.unlockLocked()
}

func (sf *s3File) unlockLocked() error {
	if sf.lockOwner == "" {
		return nil
	}
	owner := sf.lockOwner
	sf.lockOwner = ""
	key := sf.fs.key(sf.name)
	defer sf.fs.keyLocks().release(key)
	if locker := currentGitObjectLocker(); locker != nil {
		if err := locker.ReleaseLock(gitObjectLockName(key), owner); err != nil {
			return fmt.Errorf("unlock %s: %w", key, err)
		}
	}
	return nil
}

func (sf *s3File) Truncate(size int64) error {
	sf.mu.Lock()
	defer sf.mu.Unlock()
	if !sf.writer {
		return &os.PathError{Op: "truncate", Path: sf.name, Err: os.ErrPermission}
	}
	sf.state.mu.Lock()
	defer sf.state.mu.Unlock()
	sf.state.dirty = true
	switch {
	case size < int64(len(sf.state.data)):
		sf.state.data = sf.state.data[:size]
	case size > int64(len(sf.state.data)):
		sf.state.data = append(sf.state.data, make([]byte, size-int64(len(sf.state.data)))...)
	}
	return nil
}

type s3FileInfo struct {
	name    string
	size    int64
	mode    os.FileMode
	modTime time.Time
	isDir   bool
}

func (fi *s3FileInfo) Name() string       { return fi.name }
func (fi *s3FileInfo) Size() int64        { return fi.size }
func (fi *s3FileInfo) Mode() os.FileMode  { return fi.mode }
func (fi *s3FileInfo) ModTime() time.Time { return fi.modTime }
func (fi *s3FileInfo) IsDir() bool        { return fi.isDir }
func (fi *s3FileInfo) Sys() interface{}   { return nil }

var _ billy.Filesystem = (*S3FS)(nil)
var _ billy.File = (*s3File)(nil)
