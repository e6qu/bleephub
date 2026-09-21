package gitstore

import (
	"bytes"
	"compress/zlib"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"math"
	"os"
	"sort"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/format/idxfile"
	"github.com/go-git/go-git/v5/plumbing/format/packfile"
	"github.com/go-git/go-git/v5/plumbing/hash"
	"github.com/go-git/go-git/v5/plumbing/storer"
)

// Indexing a pack is what git's index-pack does, in its two passes. The first
// reads the pack from front to back: every entry is inflated, which is the only
// way to learn where the next begins, its offset and checksum are recorded, and
// an object stored whole is hashed on the way. That pass needs nothing but the
// bytes, so for a push it runs while they arrive. The second resolves the
// deltas: each base is inflated once and every delta of it applied to it, which
// names the object the delta stands for; an object stored whole with no delta of
// it is never read again, however large.
//
// A delta may name its base by id rather than by position, and a thin pack —
// what a stock git client pushes — names bases it left out because the server
// already has them. Those are the ids no object in the pack turns out to have.
// They are read from the repository and appended to the pack, whose object count
// and checksum are rewritten to match, as index-pack --fix-thin does: the pushed
// bytes are kept, and nothing is re-encoded.

// A pack begins with its signature and version, four bytes each, then the
// number of objects it holds; it ends with the SHA-1 of everything before.
const (
	packCountOffset = 8
	hashSize        = hash.Size
	// maxDeltaDepth is the longest chain of deltas a pack may hold, as go-git
	// allows.
	maxDeltaDepth = 4095
)

var errMalformedPack = errors.New("malformed pack")

// packEntry is one object of a pack.
type packEntry struct {
	offset int64
	crc    uint32
	// kind is the object's type. For a delta it is known once the delta is
	// resolved, and delta says the entry is one.
	kind  plumbing.ObjectType
	delta bool
	// A delta's base is named by its offset in the pack or by its id.
	baseOffset int64
	baseHash   plumbing.Hash
	// hash is the object's id: read for an object stored whole, resolved for a
	// delta.
	hash plumbing.Hash
}

// packLayout is what the first pass over a pack learns.
type packLayout struct {
	entries  []packEntry
	checksum plumbing.Hash
	err      error
}

// readPackLayout is the first pass. It reads pack to its end, whatever happens,
// so that whoever is sending it is never left blocked; and it holds the pack to
// its checksum, which is what the pack will be named by.
func readPackLayout(pack io.Reader) packLayout {
	summed := &checksummedReader{reader: pack, hasher: hash.New(hash.CryptoType)}
	var layout packLayout
	layout.err = func() error {
		scanner := packfile.NewScanner(summed)
		_, count, err := scanner.Header()
		if err != nil {
			return err
		}
		layout.entries = make([]packEntry, 0, min(count, 1<<20))
		for range count {
			header, err := scanner.NextObjectHeader()
			if err != nil {
				return err
			}
			entry := packEntry{offset: header.Offset, kind: header.Type}
			var content io.Writer = io.Discard
			var hasher plumbing.Hasher
			switch header.Type {
			case plumbing.OFSDeltaObject:
				entry.delta, entry.baseOffset = true, header.OffsetReference
			case plumbing.REFDeltaObject:
				entry.delta, entry.baseHash = true, header.Reference
			default:
				hasher = plumbing.NewHasher(header.Type, header.Length)
				content = hasher
			}
			if _, entry.crc, err = scanner.NextObject(content); err != nil {
				return err
			}
			if !entry.delta {
				entry.hash = hasher.Sum()
			}
			layout.entries = append(layout.entries, entry)
		}
		return nil
	}()
	if _, err := io.Copy(io.Discard, summed); err != nil && layout.err == nil {
		layout.err = err
	}
	if layout.err == nil {
		layout.checksum, layout.err = summed.verify()
	}
	return layout
}

// layoutOf is the first pass over a pack on disk.
func layoutOf(pack *os.File) packLayout {
	if _, err := pack.Seek(0, io.SeekStart); err != nil {
		return packLayout{err: err}
	}
	return readPackLayout(pack)
}

// checksummedReader passes a pack through and hashes all of it but the last
// hashSize bytes read so far, which are the checksum it claims once it ends.
type checksummedReader struct {
	reader io.Reader
	hasher hash.Hash
	tail   []byte
}

func (c *checksummedReader) Read(p []byte) (int, error) {
	n, err := c.reader.Read(p)
	if n > 0 {
		c.tail = append(c.tail, p[:n]...)
		if over := len(c.tail) - hashSize; over > 0 {
			_, _ = c.hasher.Write(c.tail[:over])
			c.tail = append(c.tail[:0], c.tail[over:]...)
		}
	}
	return n, err
}

// verify compares the checksum the pack ends with to its contents. A pack with
// anything after its checksum fails too, since what it ends with is then not one.
func (c *checksummedReader) verify() (plumbing.Hash, error) {
	var claimed plumbing.Hash
	if len(c.tail) != hashSize || !bytes.Equal(c.hasher.Sum(nil), c.tail) {
		return claimed, fmt.Errorf("%w: its checksum does not match its contents", errMalformedPack)
	}
	copy(claimed[:], c.tail)
	return claimed, nil
}

// indexPack is the second pass over the pack in file, whose first pass is
// layout. A base the pack lacks is read from bases and appended to the pack; with
// bases nil, the pack must hold every base itself. It returns the pack's index
// and checksum, both of the pack as it is left.
func indexPack(file *os.File, layout packLayout, bases storer.EncodedObjectStorer) (*idxfile.MemoryIndex, plumbing.Hash, error) {
	if layout.err != nil {
		return nil, plumbing.ZeroHash, layout.err
	}
	entries := layout.entries
	resolver := &deltaResolver{
		entries:     entries,
		scanner:     packfile.NewScanner(file),
		ofsChildren: map[int64][]int{},
		refChildren: map[plumbing.Hash][]int{},
	}
	byOffset := make(map[int64]bool, len(entries))
	for i, entry := range entries {
		switch {
		case !entry.delta:
		case entry.baseHash.IsZero():
			if !byOffset[entry.baseOffset] {
				return nil, plumbing.ZeroHash, fmt.Errorf("%w: the delta at %d names no object before it", errMalformedPack, entry.offset)
			}
			resolver.ofsChildren[entry.baseOffset] = append(resolver.ofsChildren[entry.baseOffset], i)
		default:
			resolver.refChildren[entry.baseHash] = append(resolver.refChildren[entry.baseHash], i)
		}
		byOffset[entry.offset] = true
	}

	for i := range entries {
		if entries[i].delta {
			continue
		}
		children := resolver.childrenOf(entries[i].offset, entries[i].hash)
		if len(children) == 0 {
			continue
		}
		content, err := resolver.read(i)
		if err != nil {
			return nil, plumbing.ZeroHash, err
		}
		if err := resolver.resolve(content, entries[i].kind, children, 1); err != nil {
			return nil, plumbing.ZeroHash, err
		}
	}

	// Whatever is still waited for is a base outside the pack. They are taken
	// in a fixed order, so that one push always makes one pack.
	missing := make([]plumbing.Hash, 0, len(resolver.refChildren))
	for base := range resolver.refChildren {
		missing = append(missing, base)
	}
	sort.Slice(missing, func(i, j int) bool { return bytes.Compare(missing[i][:], missing[j][:]) < 0 })
	appended := make([]plumbing.EncodedObject, 0, len(missing))
	for _, base := range missing {
		children := resolver.refChildren[base]
		delete(resolver.refChildren, base)
		if len(children) == 0 {
			// Resolving an earlier missing base produced this one.
			continue
		}
		object, content, err := readBase(bases, base)
		if err != nil {
			return nil, plumbing.ZeroHash, err
		}
		if err := resolver.resolve(content, object.Type(), children, 1); err != nil {
			return nil, plumbing.ZeroHash, err
		}
		appended = append(appended, object)
	}
	for _, entry := range entries {
		if entry.hash.IsZero() {
			return nil, plumbing.ZeroHash, fmt.Errorf("%w: the delta at %d does not resolve", errMalformedPack, entry.offset)
		}
	}

	checksum := layout.checksum
	var err error
	if len(appended) > 0 {
		if entries, checksum, err = appendPackBases(file, entries, appended); err != nil {
			return nil, plumbing.ZeroHash, err
		}
	}
	writer := new(idxfile.Writer)
	for _, entry := range entries {
		if entry.offset < 0 {
			return nil, plumbing.ZeroHash, fmt.Errorf("%w: an object at offset %d", errMalformedPack, entry.offset)
		}
		writer.Add(entry.hash, uint64(entry.offset), entry.crc)
	}
	if err := writer.OnFooter(checksum); err != nil {
		return nil, plumbing.ZeroHash, err
	}
	index, err := writer.Index()
	return index, checksum, err
}

// readBase reads a base the pack lacks.
func readBase(bases storer.EncodedObjectStorer, base plumbing.Hash) (plumbing.EncodedObject, []byte, error) {
	if bases == nil {
		return nil, nil, fmt.Errorf("%w: a delta's base %s is not in it", errMalformedPack, base)
	}
	object, err := bases.EncodedObject(plumbing.AnyObject, base)
	if err != nil {
		return nil, nil, fmt.Errorf("a delta's base %s is neither in the pack nor in the repository: %w", base, err)
	}
	content, err := readObject(object)
	return object, content, err
}

func readObject(object plumbing.EncodedObject) (content []byte, err error) {
	reader, err := object.Reader()
	if err != nil {
		return nil, err
	}
	defer func() {
		if closeErr := reader.Close(); err == nil {
			err = closeErr
		}
	}()
	return io.ReadAll(reader)
}

// deltaResolver applies the deltas of a pack to their bases.
type deltaResolver struct {
	entries []packEntry
	scanner *packfile.Scanner
	// The deltas waiting on a base, by the base's offset and by its id.
	ofsChildren map[int64][]int
	refChildren map[plumbing.Hash][]int
}

// childrenOf takes the deltas of the object at offset whose id is id. They are
// taken, not looked at: a base the pack holds twice is resolved once.
func (d *deltaResolver) childrenOf(offset int64, id plumbing.Hash) []int {
	children := d.ofsChildren[offset]
	delete(d.ofsChildren, offset)
	children = append(children, d.refChildren[id]...)
	delete(d.refChildren, id)
	return children
}

// read inflates one entry: an object stored whole, or a delta's instructions.
func (d *deltaResolver) read(i int) ([]byte, error) {
	if _, err := d.scanner.SeekObjectHeader(d.entries[i].offset); err != nil {
		return nil, err
	}
	var content bytes.Buffer
	if _, _, err := d.scanner.NextObject(&content); err != nil {
		return nil, err
	}
	return content.Bytes(), nil
}

// resolve applies each of the deltas children to base, an object of type kind,
// and then the deltas of each result to it, depth deltas from an object stored
// whole.
func (d *deltaResolver) resolve(base []byte, kind plumbing.ObjectType, children []int, depth int) error {
	if depth > maxDeltaDepth {
		return fmt.Errorf("%w: a chain of deltas is longer than %d", errMalformedPack, maxDeltaDepth)
	}
	for _, child := range children {
		instructions, err := d.read(child)
		if err != nil {
			return err
		}
		content, err := applyDelta(base, instructions)
		if err != nil {
			return fmt.Errorf("%w: the delta at %d: %w", errMalformedPack, d.entries[child].offset, err)
		}
		hasher := plumbing.NewHasher(kind, int64(len(content)))
		_, _ = hasher.Write(content)
		d.entries[child].kind, d.entries[child].hash = kind, hasher.Sum()
		if grandchildren := d.childrenOf(d.entries[child].offset, d.entries[child].hash); len(grandchildren) > 0 {
			if err := d.resolve(content, kind, grandchildren, depth+1); err != nil {
				return err
			}
		}
	}
	return nil
}

var errBadDelta = errors.New("the delta is not one")

// applyDelta returns base with delta applied: a git delta is the sizes of its
// base and its result, then instructions each of which copies a range of the
// base or inserts bytes of its own.
func applyDelta(base, delta []byte) ([]byte, error) {
	sourceSize, rest, ok := deltaSize(delta)
	if !ok || sourceSize != uint64(len(base)) {
		return nil, errBadDelta
	}
	targetSize, rest, ok := deltaSize(rest)
	if !ok {
		return nil, errBadDelta
	}
	// The size is the delta's claim, so it bounds the result but does not size
	// an allocation up front.
	out := make([]byte, 0, min(targetSize, uint64(len(base)+len(delta))))
	for len(rest) > 0 {
		op := rest[0]
		rest = rest[1:]
		switch {
		case op&0x80 != 0:
			var offset, size uint64
			for i := range 4 {
				if op&(1<<i) != 0 {
					if len(rest) == 0 {
						return nil, errBadDelta
					}
					offset |= uint64(rest[0]) << (8 * i)
					rest = rest[1:]
				}
			}
			for i := range 3 {
				if op&(0x10<<i) != 0 {
					if len(rest) == 0 {
						return nil, errBadDelta
					}
					size |= uint64(rest[0]) << (8 * i)
					rest = rest[1:]
				}
			}
			if size == 0 {
				size = 0x10000
			}
			if offset+size > uint64(len(base)) || uint64(len(out))+size > targetSize {
				return nil, errBadDelta
			}
			out = append(out, base[offset:offset+size]...)
		case op != 0:
			if int(op) > len(rest) || uint64(len(out))+uint64(op) > targetSize {
				return nil, errBadDelta
			}
			out = append(out, rest[:op]...)
			rest = rest[op:]
		default:
			return nil, errBadDelta
		}
	}
	if uint64(len(out)) != targetSize {
		return nil, errBadDelta
	}
	return out, nil
}

// deltaSize reads one of a delta's sizes: seven bits a byte, least significant
// first, while the top bit is set.
func deltaSize(delta []byte) (uint64, []byte, bool) {
	var size uint64
	for i, b := range delta {
		if i == binary.MaxVarintLen64 {
			break
		}
		size |= uint64(b&0x7f) << (7 * i)
		if b&0x80 == 0 {
			return size, delta[i+1:], true
		}
	}
	return 0, nil, false
}

// appendPackBases appends objects whole to the pack in file, which holds
// entries, and rewrites its object count and checksum to take them in. It
// returns the pack's entries and its new checksum.
func appendPackBases(file *os.File, entries []packEntry, objects []plumbing.EncodedObject) ([]packEntry, plumbing.Hash, error) {
	total := uint64(len(entries)) + uint64(len(objects))
	if total > math.MaxUint32 {
		return nil, plumbing.ZeroHash, fmt.Errorf("%d objects do not fit a pack", total)
	}
	size, err := file.Seek(0, io.SeekEnd)
	if err != nil {
		return nil, plumbing.ZeroHash, err
	}
	offset := size - hashSize
	for _, object := range objects {
		sum := crc32.NewIEEE()
		written, err := writePackObject(io.NewOffsetWriter(file, offset), sum, object)
		if err != nil {
			return nil, plumbing.ZeroHash, fmt.Errorf("append base %s: %w", object.Hash(), err)
		}
		entries = append(entries, packEntry{offset: offset, crc: sum.Sum32(), kind: object.Type(), hash: object.Hash()})
		offset += written
	}
	if err := file.Truncate(offset); err != nil {
		return nil, plumbing.ZeroHash, err
	}

	var count [4]byte
	binary.BigEndian.PutUint32(count[:], uint32(total))
	if _, err := file.WriteAt(count[:], packCountOffset); err != nil {
		return nil, plumbing.ZeroHash, err
	}
	summer := hash.New(hash.CryptoType)
	if _, err := io.Copy(summer, io.NewSectionReader(file, 0, offset)); err != nil {
		return nil, plumbing.ZeroHash, err
	}
	var checksum plumbing.Hash
	copy(checksum[:], summer.Sum(nil))
	if _, err := file.WriteAt(checksum[:], offset); err != nil {
		return nil, plumbing.ZeroHash, err
	}
	return entries, checksum, nil
}

// packTypeCodes are the type codes a pack entry of a whole object carries.
var packTypeCodes = map[plumbing.ObjectType]byte{
	plumbing.CommitObject: 1,
	plumbing.TreeObject:   2,
	plumbing.BlobObject:   3,
	plumbing.TagObject:    4,
}

// writePackObject writes one object whole as a pack entry — its type and size,
// then its content deflated, streamed however large — and returns how many bytes
// that took. Everything written also goes to sum, the entry's checksum.
func writePackObject(file io.Writer, sum io.Writer, object plumbing.EncodedObject) (int64, error) {
	code, known := packTypeCodes[object.Type()]
	if !known {
		return 0, fmt.Errorf("an object of type %s has no place in a pack", object.Type())
	}
	counted := &countingWriter{writer: io.MultiWriter(file, sum)}
	size := object.Size()
	var header [binary.MaxVarintLen64 + 1]byte
	n := 0
	header[n] = code<<4 | byte(size&0x0f)
	for size >>= 4; size > 0; size >>= 7 {
		header[n] |= 0x80
		n++
		header[n] = byte(size & 0x7f)
	}
	if _, err := counted.Write(header[:n+1]); err != nil {
		return 0, err
	}
	content, err := object.Reader()
	if err != nil {
		return 0, err
	}
	deflate := zlib.NewWriter(counted)
	_, err = io.Copy(deflate, content)
	if closeErr := content.Close(); err == nil {
		err = closeErr
	}
	if closeErr := deflate.Close(); err == nil {
		err = closeErr
	}
	return counted.written, err
}

// countingWriter counts what passes through it.
type countingWriter struct {
	writer  io.Writer
	written int64
}

func (c *countingWriter) Write(p []byte) (int, error) {
	n, err := c.writer.Write(p)
	c.written += int64(n)
	return n, err
}
