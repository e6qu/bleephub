package gitstore

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"path"
	"sort"
	"strings"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/format/idxfile"

	"github.com/e6qu/bleephub/gitstore/objstore"
)

// Before the manifest, a repository in the bucket was laid out as a bare git
// directory: a pack was live if its .pack and .idx were both there and no
// .superseded marker lay beside them, and every reference was an object of its
// own under refs/, beside HEAD and a packed-refs that was read but never
// written. The engine does not read that layout. Adopt is the one piece of code
// that does: an operator runs it once against a store written that way, it
// writes each repository the manifest that says what the old layout said, and
// from then on the repository is like any other. It is not a fallback and
// nothing calls it on the engine's behalf: a repository without a manifest is
// refused (Store.ExistingRepository), not adopted on the quiet.

// ErrAlreadyAdopted refuses the adoption of a repository that has a manifest:
// the manifest is the truth, and the old layout beside it is at best a copy of
// what was once true.
var ErrAlreadyAdopted = errors.New("gitstore: the repository already has a manifest")

// AdoptReport says what adopting one repository did.
type AdoptReport struct {
	// Repository is the repository's full name; a submodule's is its parent's
	// name and its own, joined as its keys are: owner/repo/modules/name.
	Repository string
	// Sequence is the sequence of the manifest written, which is 1.
	Sequence uint64
	// Packs and Retired count the packs entered as live and as retired.
	Packs, Retired int
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
	return layout, nil
}

// writeAdoptedManifest reads what the layout's keys hold and commits it as the
// repository's first manifest, on the condition that it has none.
func (s *Store) writeAdoptedManifest(ctx context.Context, name string, layout *oldLayout) (AdoptReport, error) {
	report := AdoptReport{Repository: name}
	manifests := &manifestStore{shared: s.shared, prefix: layout.prefix}
	absent, err := manifests.read(nil)
	if err != nil {
		return report, err
	}
	if absent.exists() {
		return report, ErrAlreadyAdopted
	}

	working := newDraft(absent.manifest, nil, s.shared.now())
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
			return report, err
		}
		working.manifest.Packs = append(working.manifest.Packs, manifestPack{
			Name:        packName,
			Bytes:       layout.packDirectory[entry].Size,
			IndexBytes:  index.Size,
			FilterBytes: layout.packDirectory[packName+".bfilter"].Size,
			Objects:     objects,
			Source:      packSourceAdoption,
			Added:       working.now.UTC(),
		})
	}
	sort.Slice(working.manifest.Packs, func(i, j int) bool { return working.manifest.Packs[i].Name < working.manifest.Packs[j].Name })
	sort.Slice(working.manifest.Retired, func(i, j int) bool { return working.manifest.Retired[i].Name < working.manifest.Retired[j].Name })

	// git's precedence: a reference kept under its own name shadows the packed
	// one, so the packed ones go in first and the others over them.
	if layout.packedRefs {
		packed, err := s.readOldPackedRefs(ctx, layout.prefix+oldPackedRefsName)
		if err != nil {
			return report, err
		}
		for _, ref := range packed {
			if err := working.set(ref); err != nil {
				return report, err
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
			return report, err
		}
		line := strings.TrimSpace(string(data))
		if line == "" {
			return report, fmt.Errorf("reference %s is empty", relative)
		}
		if err := working.set(plumbing.NewReferenceFromStrings(relative, line)); err != nil {
			return report, err
		}
	}
	report.References = len(working.manifest.Refs.Changes)

	committed, err := (&committer{manifests: manifests}).write(ctx, absent, working)
	if errors.Is(err, objstore.ErrConditionNotMet) {
		return report, ErrAlreadyAdopted
	}
	if err != nil {
		return report, err
	}
	report.Sequence = committed.manifest.Sequence
	report.Packs, report.Retired = len(committed.manifest.Packs), len(committed.manifest.Retired)
	report.Snapshot = committed.manifest.Refs.Snapshot
	return report, nil
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

// RemoveAdoptedLayout deletes, from a repository that has a manifest and from
// the submodule repositories under it, the keys only the old layout used: HEAD,
// packed-refs, everything under refs/, and the supersession markers. It is a
// separate step from Adopt, and refuses a repository without a manifest, so
// that the old references are removed only once something else holds them. It
// returns how many keys it removed.
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
		if strings.HasSuffix(entry, oldMarkerSuffix) {
			doomed = append(doomed, prefix+path.Join("objects", "pack", entry))
		}
	}
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
