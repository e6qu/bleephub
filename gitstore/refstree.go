package gitstore

import (
	"context"
	"fmt"
	"io"
	"os"
	"path"
	"strings"
	"sync"
	"time"

	"github.com/minio/minio-go/v7"
)

// Listing a repository's references, as every fetch and push begins by doing,
// walks refs/ the way git lays it out on a disk: a directory at a time, then a
// file at a time. Against an object store that is a LIST per directory and a
// GET per reference, so an advertisement's cost grew with the number of
// branches and tags, and was paid again by the next client a moment later.
//
// One recursive listing of refs/ names every reference, and carries each one's
// ETag. A reference file is forty-odd bytes, so what was read last time is kept
// beside the ETag it was read under, and a listing that shows the same ETag is
// the store's word that the bytes have not changed. An advertisement is then
// one LIST, plus a GET for each reference that has moved since this replica
// last read it — and the listing itself is the revalidation, so nothing is
// served on trust that the old walk would have fetched.
//
// The listing is reused for as long as a fetched reference is (referenceReadTTL)
// and no longer, a write through this filesystem discards it, and with reuse
// turned off (IndexFreshness below zero) none of this applies: the walk is the
// plain one.

// refsTree is one recursive listing of a repository's refs/ directory.
type refsTree struct {
	at    time.Time
	files map[string]refsTreeFile // by bucket key
	// prefetch reads, once, every listed reference this replica does not hold.
	prefetch sync.Once
}

type refsTreeFile struct {
	etag    string
	size    int64
	modTime time.Time
}

// referenceContent is a reference file's bytes and the ETag they were read or
// written under.
type referenceContent struct {
	etag string
	data []byte
}

// maxReferenceContents bounds the bytes-by-ETag map across every repository on
// this filesystem. Past it the map is emptied rather than trimmed: it holds
// nothing that the next advertisement cannot fetch again.
const maxReferenceContents = 1 << 17

// underRefs reports whether a repository-relative path is refs/ or inside it.
func underRefs(name string) bool {
	cleaned := path.Clean(name)
	return cleaned == "refs" || strings.HasPrefix(cleaned, "refs/")
}

func (f *S3FS) refsPrefix() string { return f.key("refs") + "/" }

// freshRefsTree returns the repository's listing if one is held and current.
func (f *S3FS) freshRefsTree() *refsTree {
	ttl, once := f.referenceReadTTL()
	if once {
		return nil
	}
	shared := f.shared()
	shared.mu.Lock()
	defer shared.mu.Unlock()
	tree := shared.refTrees[f.refsPrefix()]
	if tree == nil || time.Since(tree.at) > ttl {
		return nil
	}
	return tree
}

// loadRefsTree returns a current listing, taking one if need be. Concurrent
// callers share a single listing. It returns nil when reuse is turned off.
func (f *S3FS) loadRefsTree() (*refsTree, error) {
	if _, once := f.referenceReadTTL(); once {
		return nil, nil
	}
	if tree := f.freshRefsTree(); tree != nil {
		return tree, nil
	}
	prefix := f.refsPrefix()
	listed, err, _ := f.shared().refsList.Do(prefix, func() (any, error) {
		if berr := f.breaker().check(); berr != nil {
			return nil, berr
		}
		ctx, cancel := context.WithTimeout(f.baseContext(), 30*time.Second)
		defer cancel()
		// A write under refs/ while this listing is in flight may or may not be
		// in it. The listing still answers the walk that asked — a read racing
		// a write may see either side — but it is not kept for anyone else.
		shared := f.shared()
		shared.mu.Lock()
		generation := shared.refsWrites[prefix]
		shared.mu.Unlock()
		// Stamped before the listing starts: the bound is on how old what it
		// reports may be, and a long listing is old by the time it ends.
		tree := &refsTree{at: time.Now(), files: map[string]refsTreeFile{}}
		for object := range f.client.Client.ListObjects(ctx, f.bucket, minio.ListObjectsOptions{Prefix: prefix, Recursive: true}) {
			if object.Err != nil {
				f.breaker().record(object.Err)
				return nil, fmt.Errorf("s3 list %s: %w", prefix, object.Err)
			}
			if strings.HasSuffix(object.Key, "/") {
				continue
			}
			tree.files[object.Key] = refsTreeFile{etag: strings.Trim(object.ETag, `"`), size: object.Size, modTime: object.LastModified}
		}
		f.breaker().record(nil)
		shared.mu.Lock()
		if shared.refsWrites[prefix] == generation {
			shared.refTrees[prefix] = tree
		}
		shared.mu.Unlock()
		return tree, nil
	})
	if err != nil {
		return nil, err
	}
	return listed.(*refsTree), nil
}

// refsDirectory answers a ReadDir of refs/ or a directory inside it from the
// recursive listing. ok is false when the plain walk should answer instead.
func (f *S3FS) refsDirectory(dirname string) (entries map[string]os.FileInfo, ok bool, err error) {
	if !underRefs(dirname) {
		return nil, false, nil
	}
	tree, err := f.loadRefsTree()
	if err != nil || tree == nil {
		return nil, false, err
	}
	prefix := f.key(dirname) + "/"
	entries = map[string]os.FileInfo{}
	for key, file := range tree.files {
		relative, inside := strings.CutPrefix(key, prefix)
		if !inside || relative == "" {
			continue
		}
		name, rest, _ := strings.Cut(relative, "/")
		if rest != "" {
			entries[name] = &s3FileInfo{name: name, mode: 0o755 | os.ModeDir, isDir: true}
			continue
		}
		entries[name] = &s3FileInfo{name: name, size: file.size, mode: 0o644, modTime: file.modTime}
	}
	return entries, true, nil
}

// referenceFromTree answers a read of a reference under refs/ from the listing
// and the bytes kept by ETag. answered is false when the store must be asked:
// no current listing, or a reference this replica has not read at that ETag.
func (f *S3FS) referenceFromTree(filename string) (data []byte, absent, answered bool) {
	if !underRefs(filename) {
		return nil, false, false
	}
	tree := f.freshRefsTree()
	if tree == nil {
		return nil, false, false
	}
	key := f.key(filename)
	file, listed := tree.files[key]
	if !listed {
		return nil, true, true
	}
	if data, held := f.heldReference(key, file); held {
		return data, false, true
	}
	// Whoever lists references goes on to read them, one after another. The
	// listing names them all, so the ones not held are read together instead:
	// the same requests, without each waiting for the one before.
	tree.prefetch.Do(func() { f.prefetchReferences(tree) })
	if data, held := f.heldReference(key, file); held {
		return data, false, true
	}
	return nil, false, false
}

// heldReference returns the bytes kept for a reference if they were read or
// written under the ETag the listing shows.
func (f *S3FS) heldReference(key string, file refsTreeFile) ([]byte, bool) {
	shared := f.shared()
	shared.mu.Lock()
	defer shared.mu.Unlock()
	content, held := shared.refContents[key]
	if !held || file.etag == "" || content.etag != file.etag || int64(len(content.data)) != file.size {
		return nil, false
	}
	return content.data, true
}

// referencePrefetchWorkers bounds the concurrent reads of a prefetch.
const referencePrefetchWorkers = 16

// prefetchReferences reads every listed reference not held at its listed ETag.
// A read that fails is left for the caller that wants that reference, which
// reads it itself and reports the error in its own terms.
func (f *S3FS) prefetchReferences(tree *refsTree) {
	var missing []string
	for key, file := range tree.files {
		if _, held := f.heldReference(key, file); !held {
			missing = append(missing, key)
		}
	}
	// One reference is read by the caller that asked for it.
	if len(missing) < 2 || f.breaker().check() != nil {
		return
	}
	keys := make(chan string)
	var workers sync.WaitGroup
	for range min(referencePrefetchWorkers, len(missing)) {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for key := range keys {
				f.prefetchReference(key)
			}
		}()
	}
	for _, key := range missing {
		keys <- key
	}
	close(keys)
	workers.Wait()

	// The walk that asked for this has yet to read what was fetched, and a
	// listing that lapsed while it was being filled would send that walk back
	// to the store for every reference it had just been given. The listing is
	// as old as it is — but a walk reading a reference at a time was never
	// fresher than its own duration either, and this one is a fraction of it.
	shared := f.shared()
	shared.mu.Lock()
	tree.at = time.Now()
	shared.mu.Unlock()
}

func (f *S3FS) prefetchReference(key string) {
	ctx, cancel := context.WithTimeout(f.baseContext(), 30*time.Second)
	defer cancel()
	body, info, _, err := f.client.GetObject(ctx, f.bucket, key, minio.GetObjectOptions{})
	if err != nil {
		if !isNotFound(err) {
			f.breaker().record(err)
		}
		return
	}
	data, err := io.ReadAll(body)
	_ = body.Close()
	f.breaker().record(err)
	if err != nil {
		return
	}
	f.rememberContentAt(key, info.ETag, data)
}

// rememberReferenceContent keeps a reference's bytes under the ETag the store
// gave for them.
func (f *S3FS) rememberReferenceContent(filename, etag string, data []byte) {
	if !underRefs(filename) {
		return
	}
	f.rememberContentAt(f.key(filename), etag, data)
}

func (f *S3FS) rememberContentAt(key, etag string, data []byte) {
	etag = strings.Trim(etag, `"`)
	if etag == "" {
		return
	}
	if _, once := f.referenceReadTTL(); once {
		return
	}
	shared := f.shared()
	shared.mu.Lock()
	defer shared.mu.Unlock()
	if len(shared.refContents) >= maxReferenceContents {
		shared.refContents = map[string]referenceContent{}
	}
	shared.refContents[key] = referenceContent{etag: etag, data: append([]byte(nil), data...)}
}

// dropRefsTree discards the listing a write under refs/ has made stale.
func (f *S3FS) dropRefsTree(filename string) {
	if !underRefs(filename) {
		return
	}
	prefix := f.refsPrefix()
	shared := f.shared()
	shared.mu.Lock()
	delete(shared.refTrees, prefix)
	shared.refsWrites[prefix]++
	shared.mu.Unlock()
}
