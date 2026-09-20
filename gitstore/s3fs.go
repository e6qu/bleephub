package gitstore

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/go-git/go-billy/v5"
	"github.com/google/uuid"
	minio "github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
	"golang.org/x/sync/singleflight"
)

type S3FS struct {
	client   *minio.Core
	bucket   string
	prefix   string
	activeMu sync.Mutex
	active   *s3ActiveFiles
	locks    *s3KeyLocks
	sharedV  *s3Shared
}

// isNotFound reports whether an object-store error is a definite 404 (the key or
// bucket does not exist), the only failure that may map to os.ErrNotExist. Every
// other error — a transient outage, a throttle — must propagate, since go-git
// reads os.ErrNotExist as proof of absence.
func isNotFound(err error) bool {
	if err == nil {
		return false
	}
	resp := minio.ToErrorResponse(err)
	return resp.StatusCode == http.StatusNotFound || resp.Code == "NoSuchKey" || resp.Code == "NotFound"
}

// s3Shared is state one filesystem and every chroot derived from it hold in common, alongside the staging namespace and the key locks.
type s3Shared struct {
	mu sync.Mutex
	// sizes memoizes the byte length of content-addressed pack artefacts. Such a key names the hash of its own contents,
	// so a remembered size never goes stale and spares a per-handle HEAD.
	sizes map[string]int64
	// objects holds the per-repository membership index consulted before any loose-object read.
	objects map[string]*repoObjectIndex
	// opts are the resolved tunables; chunkSize is the pack read path's extent size.
	opts      Options
	chunkSize int64
	// baseCtx is the server-lifetime context every per-call timeout derives from,
	// so in-flight S3 I/O is cancelled on shutdown instead of detaching. nil until
	// the server wires it; callers fall back to context.Background().
	baseCtx context.Context
	// breaker fast-fails S3 calls during an outage so a dead store returns quickly
	// rather than every goroutine blocking the full per-call timeout under the lock.
	breaker *s3Breaker
	// handoffs carries a reference file's bytes from the Stat that fetched them
	// to the Open that follows. See statReference.
	handoffs map[string]*referenceHandoff
	// chunkFetch coalesces concurrent fetches of one pack extent. A replica that
	// starts cold under load has every clone ask for the same extents at once;
	// without this each of them pays for its own copy of identical bytes.
	chunkFetch singleflight.Group
}

func newS3Shared(opts Options) *s3Shared {
	opts = opts.resolved()
	return &s3Shared{
		sizes:     map[string]int64{},
		objects:   map[string]*repoObjectIndex{},
		handoffs:  map[string]*referenceHandoff{},
		opts:      opts,
		chunkSize: opts.ChunkBytes,
		breaker:   newS3Breaker(opts.BreakerThreshold, opts.BreakerCooldown),
	}
}

// options returns the resolved tunables this filesystem and its chroots run under.
func (f *S3FS) options() Options { return f.shared().opts }

// baseContext returns the server-lifetime context per-call timeouts derive from.
func (f *S3FS) baseContext() context.Context {
	shared := f.shared()
	shared.mu.Lock()
	defer shared.mu.Unlock()
	if shared.baseCtx != nil {
		return shared.baseCtx
	}
	return context.Background()
}

// SetBaseContext installs the server-lifetime context (cancelled on shutdown) so
// in-flight S3 I/O aborts when the process is draining. Shared across chroots.
func (f *S3FS) SetBaseContext(ctx context.Context) {
	shared := f.shared()
	shared.mu.Lock()
	shared.baseCtx = ctx
	shared.mu.Unlock()
}

func (f *S3FS) breaker() *s3Breaker { return f.shared().breaker }

func (f *S3FS) shared() *s3Shared {
	f.activeMu.Lock()
	defer f.activeMu.Unlock()
	if f.sharedV == nil {
		f.sharedV = newS3Shared(Options{})
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

// Client exposes the underlying object-store client to object-byte stores sharing this filesystem's connection.
func (f *S3FS) Client() *minio.Core { return f.client }

// Bucket reports the bucket this filesystem stores objects in.
func (f *S3FS) Bucket() string { return f.bucket }

// Prefix reports the key prefix all of this filesystem's objects live under.
func (f *S3FS) Prefix() string { return f.prefix }

// GitObjectLocker grants exclusive use of one object-store key across replicas. S3 has no advisory locking, so go-git's
// ref compare-and-set and packed-refs rewrite borrow the durable store that already serializes shared state.
type GitObjectLocker interface {
	AcquireLock(name, owner string, ttl time.Duration) (bool, error)
	ReleaseLock(name, owner string) error
}

var (
	gitObjectLockerMu sync.RWMutex
	gitObjectLockerV  GitObjectLocker
)

// SetGitObjectLocker installs the durable lock manager. Until one is installed there is no shared durable state, so the process-local lock is the whole lock.
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

// ClearGitObjectLocker uninstalls l if it is the currently installed manager. A closed locker left installed would fail every ref update.
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

// s3KeyLocks serializes writers of one bucket-absolute object key within this process. Entries are reference counted so an idle key costs nothing.
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

// NewS3FS builds a filesystem over bucket, rooted at prefix. An empty endpoint
// targets AWS S3 in opts.Region; anything else is an S3-compatible store
// addressed path-style.
func NewS3FS(ctx context.Context, endpoint, bucket, prefix string, opts Options) (*S3FS, error) {
	opts = opts.resolved()
	region := opts.Region

	// minio-go wants the endpoint as host[:port] without a scheme, and derives
	// TLS from a separate Secure flag rather than the scheme. An explicit
	// endpoint (a local simulator, a non-Amazon store) is addressed path-style,
	// matching the old UsePathStyle=true; an empty endpoint targets real AWS S3.
	host := fmt.Sprintf("s3.%s.amazonaws.com", region)
	secure := true
	pathStyle := false
	if endpoint != "" {
		host = endpoint
		if strings.Contains(endpoint, "://") {
			parsed, err := url.Parse(endpoint)
			if err != nil {
				return nil, fmt.Errorf("s3 endpoint %q: %w", endpoint, err)
			}
			secure = parsed.Scheme == "https"
			host = parsed.Host
		}
		pathStyle = true
	}

	creds := opts.Credentials
	if creds == nil {
		creds = credentials.NewEnvAWS()
	}
	clientOpts := &minio.Options{
		Creds:     creds,
		Secure:    secure,
		Region:    region,
		Transport: opts.Transport,
	}
	if pathStyle {
		clientOpts.BucketLookup = minio.BucketLookupPath
	}
	client, err := minio.NewCore(host, clientOpts)
	if err != nil {
		return nil, fmt.Errorf("s3 client: %w", err)
	}
	return NewS3FSWithClient(client, bucket, prefix, opts), nil
}

// NewS3FSWithClient builds a filesystem over a client the caller configured,
// for an endpoint NewS3FS's addressing rules do not fit or a client shared with
// other stores. Of opts, the connection fields (Region, Credentials, Transport)
// are the client's business and are ignored here.
func NewS3FSWithClient(client *minio.Core, bucket, prefix string, opts Options) *S3FS {
	return &S3FS{
		client:  client,
		bucket:  bucket,
		prefix:  prefix,
		active:  &s3ActiveFiles{files: map[string]*s3FileState{}},
		locks:   newS3KeyLocks(),
		sharedV: newS3Shared(opts),
	}
}

func (f *S3FS) key(p string) string {
	return path.Join(f.prefix, p)
}

func (f *S3FS) Create(filename string) (billy.File, error) {
	return f.newActiveFile(filename, nil), nil
}

func (f *S3FS) Open(filename string) (billy.File, error) {
	return f.open(filename, true)
}

// open reads a file. takeHandoff lets it use bytes a Stat has just fetched; a
// caller that is about to write must not, because it is reading in order to
// compare, and has to compare against the store as it is now.
func (f *S3FS) open(filename string, takeHandoff bool) (billy.File, error) {
	if state := f.activeFile(filename); state != nil {
		return &s3File{fs: f, name: filename, state: state}, nil
	}
	if takeHandoff {
		if data, ok := f.takeReferenceHandoff(filename); ok {
			return &s3File{fs: f, name: filename, state: &s3FileState{data: data}}, nil
		}
	}
	// A pack or index is opened once per object decoded, so read it through ranges rather than whole.
	// Sound only because its name is a content address: see isImmutablePackKey.
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
	if berr := f.breaker().check(); berr != nil {
		return nil, berr
	}
	ctx, cancel := context.WithTimeout(f.baseContext(), 30*time.Second)
	defer cancel()

	obj, err := f.client.Client.GetObject(ctx, f.bucket, key, minio.GetObjectOptions{})
	if err != nil {
		f.breaker().record(err)
		if isNotFound(err) {
			return nil, os.ErrNotExist
		}
		return nil, fmt.Errorf("s3 get %s: %w", key, err)
	}

	data, err := io.ReadAll(obj)
	_ = obj.Close()
	f.breaker().record(err)
	if err != nil {
		if isNotFound(err) {
			return nil, os.ErrNotExist
		}
		return nil, fmt.Errorf("s3 read %s: %w", key, err)
	}

	return &s3File{fs: f, name: filename, state: &s3FileState{data: data}}, nil
}

func (f *S3FS) OpenFile(filename string, flag int, perm os.FileMode) (billy.File, error) {
	if flag&(os.O_CREATE|os.O_TRUNC) == os.O_CREATE|os.O_TRUNC && flag&os.O_EXCL == 0 {
		return f.Create(filename)
	}

	file, err := f.open(filename, flag&(os.O_WRONLY|os.O_RDWR) == 0)
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

// activeFile looks up the staging entry. The staging map is shared across every chroot, so it is keyed by the bucket-absolute
// key: a chroot-relative name like "config" names a different object per repository and would otherwise cross the streams.
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
	if isReferenceFile(filename) {
		return f.statReference(filename)
	}
	if berr := f.breaker().check(); berr != nil {
		return nil, berr
	}
	ctx, cancel := context.WithTimeout(f.baseContext(), 15*time.Second)
	defer cancel()

	info, err := f.client.Client.StatObject(ctx, f.bucket, key, minio.StatObjectOptions{})
	f.breaker().record(err)
	if err != nil {
		if isNotFound(err) {
			return nil, os.ErrNotExist
		}
		return nil, fmt.Errorf("s3 head %s: %w", key, err)
	}

	size := info.Size
	if isImmutablePackKey(filename) {
		f.rememberObjectSize(key, size)
	}
	return &s3FileInfo{
		name:    path.Base(filename),
		size:    size,
		mode:    0o644,
		modTime: info.LastModified,
		isDir:   false,
	}, nil
}

func (f *S3FS) Rename(oldpath, newpath string) error {
	f.dropReferenceHandoff(oldpath)
	f.dropReferenceHandoff(newpath)
	if state := f.parkedTemp(oldpath); state != nil {
		return f.landParkedTemp(oldpath, newpath, state)
	}
	srcKey := f.key(oldpath)
	dstKey := f.key(newpath)
	ctx, cancel := context.WithTimeout(f.baseContext(), 30*time.Second)
	defer cancel()

	_, err := f.client.Client.CopyObject(ctx,
		minio.CopyDestOptions{Bucket: f.bucket, Object: dstKey},
		minio.CopySrcOptions{Bucket: f.bucket, Object: srcKey})
	if err != nil {
		return fmt.Errorf("s3 copy %s -> %s: %w", srcKey, dstKey, err)
	}

	err = f.client.Client.RemoveObject(ctx, f.bucket, srcKey, minio.RemoveObjectOptions{})
	if err == nil {
		// dotgit lands a loose object by renaming a temp file onto its final name, so the object index learns of the write here.
		f.noteLooseRemoved(oldpath)
		f.noteLooseWrite(newpath)
	}
	if err != nil {
		// A failed rename must leave the destination absent, or a retry sees two names and may treat the copy as a committed
		// move. Report both failures if compensation also fails.
		rollbackErr := f.client.Client.RemoveObject(ctx, f.bucket, dstKey, minio.RemoveObjectOptions{})
		if rollbackErr != nil {
			return fmt.Errorf("s3 delete %s after copy: %w (rollback destination %s: %v)", srcKey, err, dstKey, rollbackErr)
		}
		return fmt.Errorf("s3 delete %s after copy (destination rolled back): %w", srcKey, err)
	}

	return nil
}

// landParkedTemp uploads a parked temp file to the name it is being renamed to.
// The staging entry is dropped only once the upload has succeeded, so a failed
// rename can be retried, exactly as a failed rename on a disk leaves the source.
func (f *S3FS) landParkedTemp(oldpath, newpath string, state *s3FileState) error {
	key := f.key(newpath)
	state.mu.Lock()
	data := append([]byte(nil), state.data...)
	state.mu.Unlock()

	if berr := f.breaker().check(); berr != nil {
		return berr
	}
	ctx, cancel := context.WithTimeout(f.baseContext(), 60*time.Second)
	defer cancel()
	_, err := f.client.Client.PutObject(ctx, f.bucket, key, bytes.NewReader(data), int64(len(data)), minio.PutObjectOptions{})
	f.breaker().record(err)
	if err != nil {
		return fmt.Errorf("s3 put %s: %w", key, err)
	}
	f.removeActiveFile(oldpath, state)
	f.noteLooseWrite(newpath)
	return nil
}

func (f *S3FS) Remove(filename string) error {
	f.dropReferenceHandoff(filename)
	if state := f.parkedTemp(filename); state != nil {
		// It never reached the bucket, so there is nothing there to delete.
		f.removeActiveFile(filename, state)
		return nil
	}
	key := f.key(filename)
	ctx, cancel := context.WithTimeout(f.baseContext(), 15*time.Second)
	defer cancel()

	err := f.client.Client.RemoveObject(ctx, f.bucket, key, minio.RemoveObjectOptions{})
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
	file := f.newActiveFile(name, nil)
	file.state.temp = true
	return file, nil
}

// parkTemp keeps a closed temp file's bytes staged until it is renamed or
// removed, and drops any that were abandoned.
func (f *S3FS) parkTemp(state *s3FileState) {
	now := time.Now()
	state.mu.Lock()
	state.parkedAt = now
	state.mu.Unlock()

	active := f.activeFiles()
	active.mu.Lock()
	defer active.mu.Unlock()
	for key, other := range active.files {
		if other == state {
			continue
		}
		other.mu.Lock()
		abandoned := other.temp && !other.parkedAt.IsZero() && now.Sub(other.parkedAt) > parkedTempRetention
		other.mu.Unlock()
		if abandoned {
			delete(active.files, key)
		}
	}
}

// parkedTemp returns the staged state of a temp file that has been closed and
// not yet renamed.
func (f *S3FS) parkedTemp(filename string) *s3FileState {
	state := f.activeFile(filename)
	if state == nil {
		return nil
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	if !state.temp || state.parkedAt.IsZero() {
		return nil
	}
	return state
}

// hideSupersededPacks drops from a listing of objects/pack every pack a merge
// has rewritten. Such a pack is kept for a retention window so that a request
// which began before the merge can finish reading it — by key, which still
// works — but a reader arriving now must not adopt it: every object in it is in
// the pack that replaced it, and each adopted pack costs the reader an index
// and a filter it has no use for.
func hideSupersededPacks(entries map[string]os.FileInfo) {
	for name := range entries {
		pack, ok := strings.CutSuffix(name, ".superseded")
		if !ok {
			continue
		}
		for _, extension := range []string{".pack", ".idx", ".bfilter"} {
			delete(entries, pack+extension)
		}
	}
}

func (f *S3FS) ReadDir(dirname string) ([]os.FileInfo, error) {
	prefix := f.key(dirname)
	if !strings.HasSuffix(prefix, "/") {
		prefix += "/"
	}

	ctx, cancel := context.WithTimeout(f.baseContext(), 30*time.Second)
	defer cancel()

	entriesByName := map[string]os.FileInfo{}
	baseLen := len(f.prefix)
	if f.prefix != "" {
		baseLen++
	}

	// A non-recursive listing yields a directory as an ObjectInfo whose Key ends
	// in "/" (the delimiter's common prefix) and a file as an ObjectInfo
	// otherwise; ObjectInfo.Err flags a mid-stream listing failure.
	for object := range f.client.Client.ListObjects(ctx, f.bucket, minio.ListObjectsOptions{Prefix: prefix}) {
		if object.Err != nil {
			return nil, fmt.Errorf("s3 list %s: %w", prefix, object.Err)
		}
		key := object.Key
		if len(key) <= baseLen {
			continue
		}
		relKey := key[baseLen:]
		if strings.HasSuffix(key, "/") {
			info := &s3FileInfo{
				name:    path.Base(relKey),
				size:    0,
				mode:    0o755 | os.ModeDir,
				modTime: time.Time{},
				isDir:   true,
			}
			entriesByName[info.name] = info
			continue
		}
		info := &s3FileInfo{
			name:    path.Base(relKey),
			size:    object.Size,
			mode:    0o644,
			modTime: object.LastModified,
			isDir:   false,
		}
		entriesByName[info.name] = info
	}

	if path.Clean(dirname) == path.Join("objects", "pack") {
		hideSupersededPacks(entriesByName)
	}

	// S3 cannot list an object until its upload completes, but go-git expects an open file to appear in the namespace, so merge the staging namespace in.
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

// referenceHandoff is a reference file's bytes, fetched by a Stat and waiting
// for the Open that follows it.
type referenceHandoff struct {
	data []byte
	at   time.Time
}

// referenceHandoffTTL bounds how long fetched bytes may wait. git stats a
// reference and opens it in the same breath, so a handoff older than this was
// never going to be collected by the read that made it.
const referenceHandoffTTL = 100 * time.Millisecond

// isReferenceFile reports whether a path is one of the small files git reads to
// resolve a reference: HEAD, packed-refs, or anything under refs/.
func isReferenceFile(name string) bool {
	cleaned := path.Clean(name)
	return cleaned == "HEAD" || cleaned == "packed-refs" || strings.HasPrefix(cleaned, "refs/")
}

// statReference answers a Stat of a reference file with the GET that the Open
// after it would have made, and keeps the bytes for that Open. go-git stats a
// reference before every read of it; against a disk that is free, and against
// an object store it doubled the cost of resolving a reference, which a server
// does many times in one push. Read together, the two are also a consistent
// pair, where a HEAD and a later GET could straddle another writer's update.
//
// What keeps this from weakening the compare-and-set: an Open that means to
// write never takes a handoff (see open), every write, rename and removal here
// discards the one for its key, and an uncollected handoff expires at once.
func (f *S3FS) statReference(filename string) (os.FileInfo, error) {
	key := f.key(filename)
	if berr := f.breaker().check(); berr != nil {
		return nil, berr
	}
	ctx, cancel := context.WithTimeout(f.baseContext(), 30*time.Second)
	defer cancel()
	body, info, _, err := f.client.GetObject(ctx, f.bucket, key, minio.GetObjectOptions{})
	if err != nil {
		f.breaker().record(err)
		if isNotFound(err) {
			return nil, os.ErrNotExist
		}
		return nil, fmt.Errorf("s3 get %s: %w", key, err)
	}
	data, err := io.ReadAll(body)
	_ = body.Close()
	f.breaker().record(err)
	if err != nil {
		if isNotFound(err) {
			return nil, os.ErrNotExist
		}
		return nil, fmt.Errorf("s3 read %s: %w", key, err)
	}

	now := time.Now()
	shared := f.shared()
	shared.mu.Lock()
	for other, handoff := range shared.handoffs {
		if now.Sub(handoff.at) > referenceHandoffTTL {
			delete(shared.handoffs, other)
		}
	}
	shared.handoffs[key] = &referenceHandoff{data: data, at: now}
	shared.mu.Unlock()
	return &s3FileInfo{name: path.Base(filename), size: int64(len(data)), mode: 0o644, modTime: info.LastModified}, nil
}

// takeReferenceHandoff collects the bytes a Stat just fetched, once.
func (f *S3FS) takeReferenceHandoff(filename string) ([]byte, bool) {
	if !isReferenceFile(filename) {
		return nil, false
	}
	key := f.key(filename)
	shared := f.shared()
	shared.mu.Lock()
	defer shared.mu.Unlock()
	handoff := shared.handoffs[key]
	if handoff == nil {
		return nil, false
	}
	delete(shared.handoffs, key)
	if time.Since(handoff.at) > referenceHandoffTTL {
		return nil, false
	}
	return handoff.data, true
}

// dropReferenceHandoff discards bytes that a write to the key has made stale.
func (f *S3FS) dropReferenceHandoff(filename string) {
	if !isReferenceFile(filename) {
		return
	}
	shared := f.shared()
	shared.mu.Lock()
	delete(shared.handoffs, f.key(filename))
	shared.mu.Unlock()
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

// CopyRepoPrefix copies every object under oldFull's prefix to newFull's, leaving the source intact. STORE-013 runs it
// outside the store lock so both prefixes coexist and readers at the old name keep working; the caller purges the old prefix after swapping metadata under the lock.
func (f *S3FS) CopyRepoPrefix(oldFull, newFull string) error {
	oldPrefix := f.key(oldFull) + "/"
	newPrefix := f.key(newFull) + "/"
	ctx, cancel := context.WithTimeout(f.baseContext(), 120*time.Second)
	defer cancel()

	var oldKeys []string
	for object := range f.client.Client.ListObjects(ctx, f.bucket, minio.ListObjectsOptions{Prefix: oldPrefix, Recursive: true}) {
		if object.Err != nil {
			return fmt.Errorf("s3 list %s: %w", oldPrefix, object.Err)
		}
		oldKeys = append(oldKeys, object.Key)
	}
	for _, oldKey := range oldKeys {
		rel := strings.TrimPrefix(oldKey, oldPrefix)
		newKey := newPrefix + rel
		if _, err := f.client.Client.CopyObject(ctx,
			minio.CopyDestOptions{Bucket: f.bucket, Object: newKey},
			minio.CopySrcOptions{Bucket: f.bucket, Object: oldKey}); err != nil {
			return fmt.Errorf("s3 copy %s -> %s: %w", oldKey, newKey, err)
		}
	}
	return nil
}

// RenameRepoPrefix moves an object prefix (copy then delete). Single-shot form for a move fast enough to hold the store
// lock; the live-repo path uses CopyRepoPrefix + DeleteRepoPrefix around the metadata swap.
func (f *S3FS) RenameRepoPrefix(oldFull, newFull string) error {
	if err := f.CopyRepoPrefix(oldFull, newFull); err != nil {
		return err
	}
	return f.DeleteRepoPrefix(oldFull)
}

func (f *S3FS) DeleteRepoPrefix(fullName string) error {
	prefix := f.key(fullName) + "/"
	ctx, cancel := context.WithTimeout(f.baseContext(), 60*time.Second)
	defer cancel()

	for {
		var keys []string
		for object := range f.client.Client.ListObjects(ctx, f.bucket, minio.ListObjectsOptions{Prefix: prefix, Recursive: true}) {
			if object.Err != nil {
				return fmt.Errorf("s3 list %s: %w", prefix, object.Err)
			}
			keys = append(keys, object.Key)
		}
		if len(keys) == 0 {
			return nil
		}
		if err := f.deleteObjectKeys(ctx, keys); err != nil {
			return err
		}
	}
}

// deleteObjectKeys removes many keys in one batched multi-delete. minio-go
// batches the channel it drains into thousand-key requests internally, matching
// the old explicit batching, and reports the first failure via its error channel.
func (f *S3FS) deleteObjectKeys(ctx context.Context, keys []string) error {
	objectsCh := make(chan minio.ObjectInfo)
	go func() {
		defer close(objectsCh)
		for _, key := range keys {
			select {
			case objectsCh <- minio.ObjectInfo{Key: key}:
			case <-ctx.Done():
				return
			}
		}
	}()
	for removeErr := range f.client.Client.RemoveObjects(ctx, f.bucket, objectsCh, minio.RemoveObjectsOptions{}) {
		if removeErr.Err != nil {
			return fmt.Errorf("s3 delete %s: %w", removeErr.ObjectName, removeErr.Err)
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

// s3FileState is a request-lifetime staging buffer for one active object write. go-git reads a packfile while streaming it,
// which S3 cannot expose until a completed object exists, so readers of a live writer share this buffer; after the writer closes, the bytes are committed and later reads go to S3 directly.
type s3FileState struct {
	data  []byte
	dirty bool
	mu    sync.Mutex
	// temp marks a file made by TempFile. git writes an object to a temporary
	// name and renames it onto its final one, which on a disk makes the object
	// appear atomically. A PUT is already atomic, so here the temporary name
	// never reaches the bucket: closing the file parks its bytes in the staging
	// area, and Rename uploads them once, straight to the final key. Uploading
	// the temporary key too would cost a PUT, a COPY and a DELETE per object
	// where one PUT does.
	temp bool
	// parkedAt is when a temp file was closed, for sweeping one its writer
	// abandoned without renaming or removing it.
	parkedAt time.Time
}

// parkedTempRetention is how long a closed temp file may wait for its rename.
// git renames within the same call that closed it, so anything older was
// abandoned on an error path and would otherwise hold its bytes forever.
const parkedTempRetention = 10 * time.Minute

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
	// Writing past the end zero-fills the gap, matching os.File.
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

// Close flushes, then releases any lock this handle holds. go-git relies on that order — flush before unlock — so the next writer never observes a half-written object.
func (sf *s3File) Close() error {
	sf.mu.Lock()
	if sf.closed {
		sf.mu.Unlock()
		return nil
	}
	sf.closed = true
	var err error
	switch {
	case sf.writer && sf.state.temp:
		sf.fs.parkTemp(sf.state)
	case sf.writer:
		// Drop the staging entry even when the flush fails, or later readers of the key would be served the bytes of a write that never landed.
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
	ctx, cancel := context.WithTimeout(sf.fs.baseContext(), 60*time.Second)
	defer cancel()

	sf.state.mu.Lock()
	data := append([]byte(nil), sf.state.data...)
	dirty := sf.state.dirty
	sf.state.mu.Unlock()
	if !dirty {
		return nil
	}
	sf.fs.dropReferenceHandoff(sf.name)
	if berr := sf.fs.breaker().check(); berr != nil {
		return berr
	}
	_, err := sf.fs.client.Client.PutObject(ctx, sf.fs.bucket, key, bytes.NewReader(data), int64(len(data)), minio.PutObjectOptions{})
	sf.fs.breaker().record(err)
	if err != nil {
		return fmt.Errorf("s3 put %s: %w", key, err)
	}
	// Again, for a Stat that fetched the old bytes while the upload was in flight.
	sf.fs.dropReferenceHandoff(sf.name)
	sf.fs.noteLooseWrite(sf.name)
	return nil
}

// Lock takes exclusive ownership of this object key. go-git uses it for the compare-and-set behind every ref update and the packed-refs rewrite; two pushes both believing they held it would lose refs.
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
