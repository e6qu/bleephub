package gitstore

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path"
	"sort"
	"strings"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/format/idxfile"
	"github.com/go-git/go-git/v5/plumbing/format/objfile"
	"github.com/go-git/go-git/v5/storage/memory"

	"github.com/e6qu/bleephub/gitstore/objstore"
)

// Earlier versions of this engine wrote two layouts it no longer reads.
//
// Before the manifest, a repository in the bucket was laid out as a bare git
// directory: a pack was live if its .pack and .idx were both there and no
// .superseded marker lay beside them, and every reference was an object of its
// own under refs/, beside HEAD and a packed-refs that was read but never
// written.
//
// Manifest format 1 named the packs and held the references, but kept each
// pack's index (.idx) and membership filter (.bfilter) as objects of their own,
// and objects written through the API loose, one key each under objects/XX/,
// found by listing.
//
// Adopt is the one piece of code that reads either: an operator runs it once
// against a store written that way, and it writes each repository the format-2
// manifest that says what the old layout said — each live pack's index and
// filter joined into its sidecar, the loose objects packed — and from then on
// the repository is like any other. It is not a fallback and nothing calls it on
// the engine's behalf: a repository without a manifest, or with one of an
// earlier format, is refused (Store.ExistingRepository), not adopted on the
// quiet. It deletes nothing; RemoveAdoptedLayout does that, as a step of its
// own.

// ErrAlreadyAdopted refuses the adoption of a repository whose manifest is of
// the current format: the manifest is the truth, and the old layout beside it is
// at best a copy of what was once true.
var ErrAlreadyAdopted = errors.New("gitstore: the repository already has a manifest of the current format")

// AdoptReport says what adopting one repository did.
type AdoptReport struct {
	// Repository is the repository's full name; a submodule's is its parent's
	// name and its own, joined as its keys are: owner/repo/modules/name.
	Repository string
	// Sequence is the sequence of the manifest written.
	Sequence uint64
	// Packs and Retired count the packs entered as live and as retired.
	Packs, Retired int
	// Loose counts the loose objects packed into a pack of their own.
	Loose int
	// References counts the references entered, HEAD among them.
	References int
	// Snapshot is the key of the reference snapshot written, if the references
	// were too many to go in the manifest.
	Snapshot string
}

// oldLayout is what a listing found of one repository written the old way.
type oldLayout struct {
	// prefix is the repository's key prefix, ending in "/".
	prefix        string
	packDirectory map[string]objstore.Entry
	// loose are the keys of the loose objects, under objects/XX/.
	loose []string
	// references are the keys, within the repository, of HEAD and everything
	// under refs/.
	references []string
	packedRefs bool
	// modules are the key prefixes of the submodule repositories kept under it.
	modules []string
}

const (
	oldPackedRefsName = "packed-refs"
	oldMarkerSuffix   = ".superseded"
)

// Repositories lists the full names of the repositories under the store's
// prefix: everything two directories down, whatever layout it is in.
func (s *Store) Repositories(ctx context.Context) ([]string, error) {
	directories := func(prefix string) ([]string, error) {
		var found []string
		err := s.shared.call(ctx, storeListTimeout, func(ctx context.Context) error {
			found = found[:0]
			return s.shared.bucket.ListDirectory(ctx, prefix, func(entry objstore.Entry) error {
				if entry.Prefix {
					found = append(found, entry.Key)
				}
				return nil
			})
		})
		return found, err
	}
	root := s.prefix
	if root != "" {
		root = strings.TrimSuffix(root, "/") + "/"
	}
	owners, err := directories(root)
	if err != nil {
		return nil, err
	}
	var names []string
	for _, owner := range owners {
		repositories, err := directories(owner)
		if err != nil {
			return nil, err
		}
		for _, repository := range repositories {
			name := strings.TrimSuffix(strings.TrimPrefix(repository, root), "/")
			// What is not a repository's name is not a repository: the startup
			// probe, for one, writes under a directory of its own.
			if ValidateRepoStorageFullName(name) == nil && !strings.HasPrefix(name, ".") {
				names = append(names, name)
			}
		}
	}
	sort.Strings(names)
	return names, nil
}

// Adopt gives the repository fullName, written in the layout that preceded the
// manifest, its manifest — and each submodule repository under it its own. It
// changes nothing else: the old reference objects and supersession markers stay
// where they are, unread, until RemoveAdoptedLayout is asked to remove them. A
// repository that already has a manifest is refused with ErrAlreadyAdopted.
func (s *Store) Adopt(ctx context.Context, fullName string) ([]AdoptReport, error) {
	if err := ValidateRepoStorageFullName(fullName); err != nil {
		return nil, err
	}
	defer s.forget(fullName)
	return s.adopt(ctx, fullName, s.repositoryPrefix(fullName))
}

func (s *Store) adopt(ctx context.Context, name, prefix string) ([]AdoptReport, error) {
	layout, err := s.readOldLayout(ctx, prefix)
	if err != nil {
		return nil, fmt.Errorf("adopt %s: %w", name, err)
	}
	report, err := s.writeAdoptedManifest(ctx, name, layout)
	if err != nil {
		return nil, fmt.Errorf("adopt %s: %w", name, err)
	}
	reports := []AdoptReport{report}
	for _, module := range layout.modules {
		nested, err := s.adopt(ctx, name+"/"+strings.TrimSuffix(strings.TrimPrefix(module, prefix), "/"), module)
		if err != nil {
			return reports, err
		}
		reports = append(reports, nested...)
	}
	return reports, nil
}

// readOldLayout lists a repository once and sorts its keys by what the old
// layout meant by them.
func (s *Store) readOldLayout(ctx context.Context, prefix string) (*oldLayout, error) {
	layout := &oldLayout{prefix: prefix, packDirectory: map[string]objstore.Entry{}}
	modules := map[string]bool{}
	err := s.shared.list(ctx, prefix, func(entry objstore.Entry) {
		relative := strings.TrimPrefix(entry.Key, prefix)
		switch {
		case strings.HasPrefix(relative, "modules/"):
			// A submodule is a repository of its own, which the old layout always
			// gave a HEAD.
			if root, isHead := strings.CutSuffix(relative, "/HEAD"); isHead {
				modules[prefix+root+"/"] = true
			}
		case strings.HasPrefix(relative, "objects/pack/"):
			layout.packDirectory[strings.TrimPrefix(relative, "objects/pack/")] = entry
		case relative == "HEAD" || strings.HasPrefix(relative, "refs/"):
			layout.references = append(layout.references, relative)
		case relative == oldPackedRefsName:
			layout.packedRefs = true
		case isLooseObjectKey(relative):
			layout.loose = append(layout.loose, entry.Key)
		}
	})
	if err != nil {
		return nil, err
	}
	for module := range modules {
		// A submodule's own submodules are found when it is adopted in its turn.
		nested := false
		for other := range modules {
			nested = nested || (other != module && strings.HasPrefix(module, other+"modules/"))
		}
		if !nested {
			layout.modules = append(layout.modules, module)
		}
	}
	sort.Strings(layout.modules)
	sort.Strings(layout.loose)
	return layout, nil
}

// isLooseObjectKey reports whether a key, within the repository, is where the
// earlier layouts kept a loose object: objects/XX/ and the other 38 hex digits.
func isLooseObjectKey(relative string) bool {
	fanout, base, found := strings.Cut(strings.TrimPrefix(relative, "objects/"), "/")
	return strings.HasPrefix(relative, "objects/") && found && len(fanout) == 2 && plumbing.IsHash(fanout+base)
}

// writeAdoptedManifest reads what the layout's keys hold and commits it as the
// repository's format-2 manifest: on the condition that it has none, or that
// the format-1 manifest it has is still the one read.
func (s *Store) writeAdoptedManifest(ctx context.Context, name string, layout *oldLayout) (AdoptReport, error) {
	report := AdoptReport{Repository: name}
	held, working, err := s.adoptedDraft(ctx, layout)
	if err != nil {
		return report, err
	}
	report.References = len(working.references())

	// Every live pack gets its sidecar: its index and filter, joined.
	for at, pack := range working.manifest.Packs {
		adopted, err := s.adoptedSidecar(ctx, layout, pack)
		if err != nil {
			return report, err
		}
		working.manifest.Packs[at] = adopted
	}

	// The loose objects go into a pack of their own, built and uploaded as any
	// other write's are.
	if len(layout.loose) > 0 {
		repository := newRepository(s.shared, name, layout.prefix)
		uploaded, err := repository.packLooseObjects(ctx, layout.loose)
		if err != nil {
			return report, err
		}
		if err := working.addPack(uploaded.entry(packSourceAdoption)); err != nil {
			return report, err
		}
		report.Loose = len(layout.loose)
	}

	committed, err := (&committer{manifests: &manifestStore{shared: s.shared, prefix: layout.prefix}}).write(ctx, held, working)
	if errors.Is(err, objstore.ErrConditionNotMet) {
		return report, fmt.Errorf("%w: it changed while it was being adopted", ErrAlreadyAdopted)
	}
	if err != nil {
		return report, err
	}
	report.Sequence = committed.manifest.Sequence
	report.Packs, report.Retired = len(committed.manifest.Packs), len(committed.manifest.Retired)
	report.Snapshot = committed.manifest.Refs.Snapshot
	return report, nil
}

// adoptedDraft reads what the repository says of its packs and references: its
// format-1 manifest, or, with none, the layout before the manifest. It returns
// the state the adopted manifest is written over and the draft of it, whose
// packs have no sidecars yet.
func (s *Store) adoptedDraft(ctx context.Context, layout *oldLayout) (*repoState, *draft, error) {
	now := s.shared.now()
	data, info, err := s.shared.getAll(ctx, layout.prefix+manifestName)
	if errors.Is(err, objstore.ErrNotFound) {
		absent := &repoState{manifest: &manifest{Format: manifestFormat}}
		working := newDraft(absent.manifest, nil, now)
		if err := s.readPreManifestLayout(ctx, layout, working); err != nil {
			return nil, nil, err
		}
		return absent, working, nil
	}
	if err != nil {
		return nil, nil, err
	}
	var header struct {
		Format int `json:"format"`
	}
	if err := json.Unmarshal(data, &header); err != nil {
		return nil, nil, fmt.Errorf("%w: %w", ErrManifestFormat, err)
	}
	switch header.Format {
	case manifestFormat:
		return nil, nil, ErrAlreadyAdopted
	case 1:
	default:
		return nil, nil, fmt.Errorf("%w: format %d", ErrManifestFormat, header.Format)
	}
	// Format 1 is format 2 without the sidecars' sizes, so it decodes into the
	// same structure with those left at zero, and they are filled in as the
	// sidecars are written.
	var formatOne manifest
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&formatOne); err != nil {
		return nil, nil, fmt.Errorf("%w: %w", ErrManifestFormat, err)
	}
	held := &repoState{version: info.Version, manifest: &formatOne}
	working := newDraft(&formatOne, nil, now)
	working.manifest.Format = manifestFormat
	if snapshot := formatOne.Refs.Snapshot; snapshot != "" {
		base, err := (&manifestStore{shared: s.shared, prefix: layout.prefix}).readSnapshot(snapshot)
		if err != nil {
			return nil, nil, err
		}
		working.base = base
	}
	return held, working, nil
}

// readPreManifestLayout enters into working the packs and references of a
// repository laid out as a bare git directory.
func (s *Store) readPreManifestLayout(ctx context.Context, layout *oldLayout, working *draft) error {
	for entry := range layout.packDirectory {
		packName, isPack := strings.CutSuffix(entry, ".pack")
		if !isPack || !validPackName(packName) {
			continue
		}
		index, indexed := layout.packDirectory[packName+".idx"]
		if !indexed {
			// An upload that never finished: no reader ever adopted it.
			continue
		}
		if marker, superseded := layout.packDirectory[packName+oldMarkerSuffix]; superseded {
			working.manifest.Retired = append(working.manifest.Retired, retiredPack{Name: packName, Retired: marker.ModTime.UTC()})
			continue
		}
		objects, err := s.countIndexedObjects(ctx, index.Key)
		if err != nil {
			return err
		}
		working.manifest.Packs = append(working.manifest.Packs, manifestPack{
			Name:    packName,
			Bytes:   layout.packDirectory[entry].Size,
			Objects: objects,
			Source:  packSourceAdoption,
			Added:   working.now.UTC(),
		})
	}
	sort.Slice(working.manifest.Packs, func(i, j int) bool { return working.manifest.Packs[i].Name < working.manifest.Packs[j].Name })
	sort.Slice(working.manifest.Retired, func(i, j int) bool { return working.manifest.Retired[i].Name < working.manifest.Retired[j].Name })

	// git's precedence: a reference kept under its own name shadows the packed
	// one, so the packed ones go in first and the others over them.
	if layout.packedRefs {
		packed, err := s.readOldPackedRefs(ctx, layout.prefix+oldPackedRefsName)
		if err != nil {
			return err
		}
		for _, ref := range packed {
			if err := working.set(ref); err != nil {
				return err
			}
		}
	}
	for _, relative := range layout.references {
		refName := plumbing.ReferenceName(relative)
		// A key that is not a reference name the engine would have written is
		// not one it served either.
		if !refName.IsSafe() {
			continue
		}
		data, _, err := s.shared.getAll(ctx, layout.prefix+relative)
		if err != nil {
			return err
		}
		line := strings.TrimSpace(string(data))
		if line == "" {
			return fmt.Errorf("reference %s is empty", relative)
		}
		if err := working.set(plumbing.NewReferenceFromStrings(relative, line)); err != nil {
			return err
		}
	}
	return nil
}

// adoptedSidecar writes a live pack's sidecar from the index and filter the
// earlier layouts kept as objects of their own, and returns the pack as the
// manifest records it.
func (s *Store) adoptedSidecar(ctx context.Context, layout *oldLayout, pack manifestPack) (manifestPack, error) {
	directory := layout.prefix + path.Join("objects", "pack") + "/"
	index, _, err := s.shared.getAll(ctx, directory+pack.Name+".idx")
	if err != nil {
		return pack, fmt.Errorf("index of %s: %w", pack.Name, err)
	}
	var filter []byte
	if _, filtered := layout.packDirectory[pack.Name+".bfilter"]; filtered {
		if filter, _, err = s.shared.getAll(ctx, directory+pack.Name+".bfilter"); err != nil {
			return pack, fmt.Errorf("filter of %s: %w", pack.Name, err)
		}
		if _, err := decodeBinaryFuseFilter(filter); err != nil {
			return pack, fmt.Errorf("filter of %s: %w", pack.Name, err)
		}
	}
	parsed := idxfile.NewMemoryIndex()
	if err := idxfile.NewDecoder(bytes.NewReader(index)).Decode(parsed); err != nil {
		return pack, fmt.Errorf("decode index of %s: %w", pack.Name, err)
	}
	checksum := plumbing.Hash(parsed.PackfileChecksum)
	if "pack-"+checksum.String() != pack.Name {
		return pack, fmt.Errorf("the index of %s indexes pack-%s", pack.Name, checksum)
	}
	sidecar := encodeSidecar(index, filter, checksum)
	if _, err := s.shared.put(ctx, storeWriteTimeout, directory+pack.Name+sidecarSuffix, bytes.NewReader(sidecar), int64(len(sidecar)), objstore.Always); err != nil {
		return pack, err
	}
	pack.IndexBytes, pack.FilterBytes, pack.SidecarBytes = int64(len(index)), int64(len(filter)), int64(len(sidecar))
	return pack, nil
}

// packLooseObjects reads loose objects the earlier layouts wrote and uploads
// them as one pack, which no manifest names yet.
func (r *repository) packLooseObjects(ctx context.Context, keys []string) (*uploadedPack, error) {
	source := memory.NewStorage()
	hashes := make([]plumbing.Hash, 0, len(keys))
	for _, key := range keys {
		data, _, err := r.shared.getAll(ctx, key)
		if err != nil {
			return nil, err
		}
		object, err := decodeLooseObject(data)
		if err != nil {
			return nil, fmt.Errorf("loose object %s: %w", key, err)
		}
		if named := strings.ReplaceAll(strings.TrimPrefix(key, r.prefix+"objects/"), "/", ""); object.Hash().String() != named {
			return nil, fmt.Errorf("loose object %s: its content hashes to %s", key, object.Hash())
		}
		if _, err := source.SetEncodedObject(object); err != nil {
			return nil, err
		}
		hashes = append(hashes, object.Hash())
	}
	built, err := r.buildPackFrom(source, hashes)
	if err != nil {
		return nil, err
	}
	defer built.cleanup()
	return r.uploadPack(ctx, built)
}

// decodeLooseObject reads git's loose object format: a zlib stream of the type,
// the size and the content.
func decodeLooseObject(data []byte) (plumbing.EncodedObject, error) { //nolint:ireturn
	reader, err := objfile.NewReader(bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	defer func() { _ = reader.Close() }()
	kind, size, err := reader.Header()
	if err != nil {
		return nil, err
	}
	object := &plumbing.MemoryObject{}
	object.SetType(kind)
	object.SetSize(size)
	writer, err := object.Writer()
	if err != nil {
		return nil, err
	}
	if _, err := io.Copy(writer, reader); err != nil {
		return nil, err
	}
	return object, writer.Close()
}

// countIndexedObjects reads a pack's index for the number of objects in it,
// which the manifest records and the old layout did not.
func (s *Store) countIndexedObjects(ctx context.Context, key string) (int, error) {
	raw, _, err := s.shared.getAll(ctx, key)
	if err != nil {
		return 0, err
	}
	index := idxfile.NewMemoryIndex()
	if err := idxfile.NewDecoder(bytes.NewReader(raw)).Decode(index); err != nil {
		return 0, fmt.Errorf("decode %s: %w", key, err)
	}
	count, err := index.Count()
	if err != nil {
		return 0, fmt.Errorf("count %s: %w", key, err)
	}
	return int(count), nil
}

// readOldPackedRefs parses git's packed-refs format. A comment, and the "^"
// line that gives the commit an annotated tag peels to, name no reference.
func (s *Store) readOldPackedRefs(ctx context.Context, key string) ([]*plumbing.Reference, error) {
	data, _, err := s.shared.getAll(ctx, key)
	if err != nil {
		return nil, err
	}
	var refs []*plumbing.Reference
	lines := bufio.NewScanner(bytes.NewReader(data))
	for lines.Scan() {
		line := lines.Text()
		if line == "" || line[0] == '#' || line[0] == '^' {
			continue
		}
		hash, name, found := strings.Cut(line, " ")
		if !found || strings.Contains(name, " ") || !plumbing.IsHash(hash) {
			return nil, fmt.Errorf("%s: malformed line %q", key, line)
		}
		refs = append(refs, plumbing.NewHashReference(plumbing.ReferenceName(name), plumbing.NewHash(hash)))
	}
	if err := lines.Err(); err != nil {
		return nil, fmt.Errorf("%s: %w", key, err)
	}
	return refs, nil
}

// RemoveAdoptedLayout deletes, from a repository that has a manifest of the
// current format and from the submodule repositories under it, the keys only
// the earlier layouts used: HEAD, packed-refs, everything under refs/, the
// supersession markers, the packs' separate indexes and filters, and the loose
// objects. It is a separate step from Adopt, and refuses a repository without
// such a manifest, so that nothing is removed before something else holds it.
// It returns how many keys it removed.
func (s *Store) RemoveAdoptedLayout(ctx context.Context, fullName string) (int, error) {
	if err := ValidateRepoStorageFullName(fullName); err != nil {
		return 0, err
	}
	return s.removeAdoptedLayout(ctx, fullName, s.repositoryPrefix(fullName))
}

func (s *Store) removeAdoptedLayout(ctx context.Context, name, prefix string) (int, error) {
	state, err := (&manifestStore{shared: s.shared, prefix: prefix}).read(nil)
	if err != nil {
		return 0, fmt.Errorf("remove the old layout of %s: %w", name, err)
	}
	if !state.exists() {
		return 0, fmt.Errorf("remove the old layout of %s: %w", name, ErrNoManifest)
	}
	layout, err := s.readOldLayout(ctx, prefix)
	if err != nil {
		return 0, fmt.Errorf("remove the old layout of %s: %w", name, err)
	}
	var doomed []string
	for _, relative := range layout.references {
		doomed = append(doomed, prefix+relative)
	}
	if layout.packedRefs {
		doomed = append(doomed, prefix+oldPackedRefsName)
	}
	for entry := range layout.packDirectory {
		for _, suffix := range []string{oldMarkerSuffix, ".idx", ".bfilter"} {
			if strings.HasSuffix(entry, suffix) {
				doomed = append(doomed, prefix+path.Join("objects", "pack", entry))
			}
		}
	}
	doomed = append(doomed, layout.loose...)
	sort.Strings(doomed)
	if len(doomed) > 0 {
		if err := s.shared.deleteMany(ctx, doomed); err != nil {
			return 0, fmt.Errorf("remove the old layout of %s: %w", name, err)
		}
	}
	removed := len(doomed)
	for _, module := range layout.modules {
		nested, err := s.removeAdoptedLayout(ctx, name+"/"+strings.TrimSuffix(strings.TrimPrefix(module, prefix), "/"), module)
		removed += nested
		if err != nil {
			return removed, err
		}
	}
	return removed, nil
}
