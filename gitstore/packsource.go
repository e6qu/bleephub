package gitstore

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/format/idxfile"
)

// PackSource is a repository whose stored packs can be read as they stand. A
// server answers a clone far more cheaply by copying the bytes of a stored pack
// than by encoding the same objects again; this is how it reaches them, the same
// way whether the repository is in a bucket or a directory.
type PackSource interface {
	// StoredPacks lists the live packs: those a reader arriving now would adopt.
	StoredPacks(ctx context.Context) ([]StoredPack, error)
	// PackIndex returns a stored pack's index. It is shared and read-only.
	PackIndex(ctx context.Context, name string) (idxfile.Index, error)
	// OpenPack opens a stored pack's bytes. On the object store they are read in
	// ranges, through the local cache.
	OpenPack(ctx context.Context, name string) (PackReader, error)
}

// StoredPack describes one stored pack.
type StoredPack struct {
	// Name is the pack's base name, pack-<hash>.
	Name string
	// Size is the size of the packfile in bytes.
	Size int64
	// Objects is the number of objects the pack holds.
	Objects int
}

// PackReader reads a stored pack's bytes at any offset.
type PackReader interface {
	io.ReaderAt
	io.Closer
}

// validPackName reports whether name is a pack's base name and nothing else: it
// is joined onto a key or a path, and must not be able to name anything but a
// pack of this repository.
func validPackName(name string) bool {
	hash, ok := strings.CutPrefix(name, "pack-")
	return ok && plumbing.IsHash(hash)
}

func errNoSuchPack(name string) error {
	return fmt.Errorf("stored pack %q: %w", name, os.ErrNotExist)
}

// StoredPacks lists the packs of the manifest held, with the sizes and object
// counts it records: nothing in the store is asked, unless this handle has
// never read the manifest.
func (r *repository) StoredPacks(context.Context) ([]StoredPack, error) {
	state, err := r.manifests.held()
	if err != nil {
		return nil, err
	}
	packs := make([]StoredPack, 0, len(state.packs))
	for _, pack := range state.packs {
		packs = append(packs, StoredPack{Name: pack.name, Size: pack.pack.size, Objects: pack.objects})
	}
	sort.Slice(packs, func(i, j int) bool { return packs[i].Name < packs[j].Name })
	return packs, nil
}

func (r *repository) storedPack(name string) (*storedPack, error) {
	state, err := r.manifests.held()
	if err != nil {
		return nil, err
	}
	pack := state.pack(name)
	if pack == nil {
		return nil, errNoSuchPack(name)
	}
	return pack, nil
}

// PackIndex returns the parsed index of one of the live packs, reading it
// if nothing has needed it yet.
func (r *repository) PackIndex(_ context.Context, name string) (idxfile.Index, error) { //nolint:ireturn
	pack, err := r.storedPack(name)
	if err != nil {
		return nil, err
	}
	return pack.loadIndex()
}

// OpenPack opens one of the live packs for ranged, cached reads.
func (r *repository) OpenPack(_ context.Context, name string) (PackReader, error) { //nolint:ireturn
	pack, err := r.storedPack(name)
	if err != nil {
		return nil, err
	}
	return newPackFile(pack.pack, pack.name+".pack"), nil
}

// dirStorer is the local-directory backend: go-git's own storage over a real
// directory, behind the same guard as the memory backend, plus the directory's
// packs offered as a PackSource.
type dirStorer struct {
	*atomicRefStorer
	// packDirectory is <repository>/objects/pack on local disk.
	packDirectory string

	indexMu sync.Mutex
	// indexes holds the pack indexes parsed so far. A pack's name is the hash of
	// its contents, so an index parsed once is right for as long as it is wanted.
	indexes map[string]*idxfile.MemoryIndex
}

var _ PackSource = (*dirStorer)(nil)

// openPackDirectory scopes access to the pack directory, so that no name — and
// no symbolic link planted in it — reaches outside. A repository that has never
// been packed has no such directory, which is reported as os.ErrNotExist.
func (d *dirStorer) openPackDirectory() (*os.Root, error) {
	return os.OpenRoot(d.packDirectory)
}

// StoredPacks lists the directory's packs. Listing a local directory is cheap
// enough to do each time; what is kept between calls is the parsed indexes.
func (d *dirStorer) StoredPacks(context.Context) ([]StoredPack, error) {
	root, err := d.openPackDirectory()
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer func() { _ = root.Close() }()
	entries, err := fs.ReadDir(root.FS(), ".")
	if err != nil {
		return nil, err
	}
	var packs []StoredPack
	for _, entry := range entries {
		name, isIndex := strings.CutSuffix(entry.Name(), ".idx")
		if !isIndex || !validPackName(name) {
			continue
		}
		// git writes the index last, so an index without its pack is not one
		// git left; whatever did, there are no bytes behind it to serve.
		info, err := root.Stat(name + ".pack")
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		index, err := d.packIndex(root, name)
		if err != nil {
			return nil, err
		}
		count, err := index.Count()
		if err != nil {
			return nil, fmt.Errorf("index of %s: %w", name, err)
		}
		packs = append(packs, StoredPack{Name: name, Size: info.Size(), Objects: int(count)})
	}
	return packs, nil
}

func (d *dirStorer) packIndex(root *os.Root, name string) (*idxfile.MemoryIndex, error) {
	d.indexMu.Lock()
	defer d.indexMu.Unlock()
	if index, ok := d.indexes[name]; ok {
		return index, nil
	}
	file, err := root.Open(name + ".idx")
	if err != nil {
		return nil, err
	}
	defer func() { _ = file.Close() }()
	index := idxfile.NewMemoryIndex()
	if err := idxfile.NewDecoder(file).Decode(index); err != nil {
		return nil, fmt.Errorf("decode index of %s: %w", name, err)
	}
	if d.indexes == nil {
		d.indexes = map[string]*idxfile.MemoryIndex{}
	}
	d.indexes[name] = index
	return index, nil
}

// PackIndex returns the parsed index of one of the directory's packs.
func (d *dirStorer) PackIndex(_ context.Context, name string) (idxfile.Index, error) { //nolint:ireturn
	if !validPackName(name) {
		return nil, errNoSuchPack(name)
	}
	root, err := d.openPackDirectory()
	if err != nil {
		return nil, err
	}
	defer func() { _ = root.Close() }()
	return d.packIndex(root, name)
}

// OpenPack opens one of the directory's packs.
func (d *dirStorer) OpenPack(_ context.Context, name string) (PackReader, error) { //nolint:ireturn
	if !validPackName(name) {
		return nil, errNoSuchPack(name)
	}
	root, err := d.openPackDirectory()
	if err != nil {
		return nil, err
	}
	defer func() { _ = root.Close() }()
	// The file outlives the root it was opened through.
	return root.Open(name + ".pack")
}

func newDirStorer(repoDir string, wrapped *atomicRefStorer) *dirStorer {
	return &dirStorer{atomicRefStorer: wrapped, packDirectory: filepath.Join(repoDir, "objects", "pack")}
}
