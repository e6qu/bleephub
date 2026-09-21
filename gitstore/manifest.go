package gitstore

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/go-git/go-git/v5/plumbing"
	gitStorage "github.com/go-git/go-git/v5/storage"
)

// The manifest is a repository's only commit point. It is one small object,
// <repository>/manifest, naming everything a reader may rely on: the live packs,
// the packs a compaction replaced and when, and the references. A change to the
// repository — a push, a reference moved through the API, a compaction — is
// visible when, and only when, a conditional write of this object succeeds
// (commit.go). Nothing is discovered by listing, nothing is arbitrated by a lock
// service, and a pack that was uploaded for a change the manifest never accepted
// is simply not part of the repository.
//
// It is JSON, so that an operator can read one, and it carries a format number
// so that a replica meeting a manifest it does not understand refuses the
// repository instead of guessing. There is no migration code here: a repository
// written before the manifest existed is given one by Adopt, once, and a
// repository whose manifest says another format is an error.
//
// REFERENCES are held as a pointer to an immutable snapshot object plus the
// changes made since it was written, each a value or a tombstone. A repository
// with a hundred thousand tags would otherwise rewrite them all on every push;
// this way a push rewrites the change list, which is folded into a new snapshot
// — written before the swap that names it, under a key never used before — once
// it passes refChangeBound. It is reftable's shape with a stack one table deep.

const (
	// manifestName is the manifest's key within the repository.
	manifestName = "manifest"
	// manifestFormat is the only format this engine reads or writes. Format 1
	// kept a pack's index and filter as objects of their own, and objects written
	// through the API loose beside the packs; Adopt converts a repository
	// written that way.
	manifestFormat = 2
	// refSnapshotDirectory holds the reference snapshots. It is under objects/ so
	// that the one listing a compaction takes shows the snapshots no manifest
	// names any more beside the packs of which that is true.
	refSnapshotDirectory = "objects/refs/"
	// refChangeBound is how many references may have changed since the snapshot
	// before a commit folds them into a new one. A change is about a hundred
	// bytes of JSON, so this holds the reference part of a manifest — which every
	// commit uploads whole — under manifestReferenceBytesBound however many
	// references the repository has.
	refChangeBound = 512
	// manifestReferenceBytesBound is the size the change list of a manifest
	// stays under while reference names are of ordinary length (up to about a
	// hundred bytes).
	manifestReferenceBytesBound = 128 << 10
	// refSnapshotHeader is the first line of a snapshot object.
	refSnapshotHeader = "bleephub reference snapshot 1"
)

// What wrote a pack, as the manifest records it.
const (
	packSourcePush       = "push"
	packSourceWrite      = "write"
	packSourceCompaction = "compaction"
	packSourceAdoption   = "adoption"
)

// ErrManifestFormat reports a manifest this engine cannot read: another format
// number, or not a manifest at all. The repository is refused whole.
var ErrManifestFormat = errors.New("gitstore: unreadable repository manifest")

// ErrManifestOutdated reports a manifest of a format an earlier version of this
// engine wrote, which Adopt converts. The repository is refused whole until it
// has been.
var ErrManifestOutdated = errors.New("gitstore: the repository manifest is of an earlier format, which `adopt` converts")

// ErrNoManifest reports a repository the store holds no manifest for where one
// was required: it was never created, or it was written by a version of this
// engine that kept references as objects and has not been through Adopt.
var ErrNoManifest = errors.New("gitstore: the repository has no manifest")

// manifest is the decoded object. A manifest that has been published is never
// modified; a commit edits a copy (see draft).
type manifest struct {
	Format int `json:"format"`
	// Sequence increases by one with every successful swap. It also makes every
	// manifest's bytes differ from every earlier one's, which matters on a store
	// whose version token is a digest of the content: there, A→B→A would give A
	// its old token back.
	Sequence uint64         `json:"sequence"`
	Packs    []manifestPack `json:"packs"`
	Retired  []retiredPack  `json:"retired"`
	Refs     manifestRefs   `json:"refs"`
}

// manifestPack is one live pack. The sizes are what let a reader address the
// pack, and the index and filter in its sidecar (sidecar.go), by extent without
// asking the store about them.
type manifestPack struct {
	Name         string `json:"name"`
	Bytes        int64  `json:"bytes"`
	SidecarBytes int64  `json:"sidecar_bytes"`
	IndexBytes   int64  `json:"index_bytes"`
	FilterBytes  int64  `json:"filter_bytes"`
	Objects      int    `json:"objects"`
	Source       string `json:"source"`
	// Added is when the commit that added the pack was made, by the clock of the
	// replica that made it.
	Added time.Time `json:"added"`
}

// retiredPack is a pack no new reader adopts, kept for the grace period for the
// requests that were already reading it. Orphan marks one that no manifest ever
// named — the upload of a push that was refused, or of a process that died
// before its swap — which, unlike a pack a compaction replaced, holds objects
// that may be nowhere else.
type retiredPack struct {
	Name    string    `json:"name"`
	Retired time.Time `json:"retired"`
	Orphan  bool      `json:"orphan,omitempty"`
}

type manifestRefs struct {
	// Snapshot is the snapshot object's key within the repository, or empty.
	Snapshot string `json:"snapshot,omitempty"`
	// Changes are the references set or removed since the snapshot, one entry a
	// name, in name order.
	Changes []refChange `json:"changes"`
}

// refChange sets a reference to a value — a hash, or "ref: <target>" — or, with
// Deleted, removes one the snapshot holds.
type refChange struct {
	Name    string `json:"name"`
	Value   string `json:"value,omitempty"`
	Deleted bool   `json:"deleted,omitempty"`
}

func decodeManifest(data []byte) (*manifest, error) {
	var header struct {
		Format int `json:"format"`
	}
	if err := json.Unmarshal(data, &header); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrManifestFormat, err)
	}
	if header.Format > 0 && header.Format < manifestFormat {
		return nil, fmt.Errorf("%w: format %d, and this engine reads format %d only", ErrManifestOutdated, header.Format, manifestFormat)
	}
	if header.Format != manifestFormat {
		return nil, fmt.Errorf("%w: format %d, and this engine reads format %d only", ErrManifestFormat, header.Format, manifestFormat)
	}
	decoded := &manifest{}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(decoded); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrManifestFormat, err)
	}
	for _, pack := range decoded.Packs {
		if !validPackName(pack.Name) || pack.Bytes <= 0 || pack.IndexBytes <= 0 || pack.FilterBytes < 0 ||
			pack.SidecarBytes != sidecarBytes(pack.IndexBytes, pack.FilterBytes) {
			return nil, fmt.Errorf("%w: live pack %q of %d bytes, sidecar %d holding index %d and filter %d",
				ErrManifestFormat, pack.Name, pack.Bytes, pack.SidecarBytes, pack.IndexBytes, pack.FilterBytes)
		}
	}
	for _, pack := range decoded.Retired {
		if !validPackName(pack.Name) {
			return nil, fmt.Errorf("%w: retired pack %q", ErrManifestFormat, pack.Name)
		}
	}
	if snapshot := decoded.Refs.Snapshot; snapshot != "" && !validRefSnapshotKey(snapshot) {
		return nil, fmt.Errorf("%w: reference snapshot %q", ErrManifestFormat, snapshot)
	}
	for _, change := range decoded.Refs.Changes {
		if _, err := change.reference(); err != nil {
			return nil, fmt.Errorf("%w: %w", ErrManifestFormat, err)
		}
	}
	return decoded, nil
}

// encode writes the manifest as JSON. A list that holds nothing is written as
// one, not as null, for whoever reads a manifest by eye.
func (m *manifest) encode() ([]byte, error) {
	written := *m
	written.Packs = append([]manifestPack{}, m.Packs...)
	written.Retired = append([]retiredPack{}, m.Retired...)
	written.Refs.Changes = append([]refChange{}, m.Refs.Changes...)
	encoded, err := json.Marshal(written)
	if err != nil {
		return nil, fmt.Errorf("encode manifest: %w", err)
	}
	return encoded, nil
}

// reference reads a change as the reference it sets, or nil for a tombstone.
func (c refChange) reference() (*plumbing.Reference, error) {
	name := plumbing.ReferenceName(c.Name)
	if err := checkSafeRefName(name); err != nil {
		return nil, err
	}
	if c.Deleted {
		if c.Value != "" {
			return nil, fmt.Errorf("reference %s is both removed and set", c.Name)
		}
		return nil, nil
	}
	return parseReferenceValue(name, c.Value)
}

// parseReferenceValue reads a stored value: forty hex digits, or "ref: " and a
// reference name.
func parseReferenceValue(name plumbing.ReferenceName, value string) (*plumbing.Reference, error) {
	if target, symbolic := strings.CutPrefix(value, "ref: "); symbolic {
		if err := checkSafeRefName(plumbing.ReferenceName(target)); err != nil {
			return nil, fmt.Errorf("reference %s: %w", name, err)
		}
		return plumbing.NewSymbolicReference(name, plumbing.ReferenceName(target)), nil
	}
	if !plumbing.IsHash(value) {
		return nil, fmt.Errorf("reference %s has the value %q, which is neither a hash nor a symbolic target", name, value)
	}
	return plumbing.NewHashReference(name, plumbing.NewHash(value)), nil
}

// referenceValue writes a reference as parseReferenceValue reads it.
func referenceValue(ref *plumbing.Reference) (string, error) {
	switch ref.Type() {
	case plumbing.SymbolicReference:
		if err := checkSafeRefName(ref.Target()); err != nil {
			return "", err
		}
		return "ref: " + ref.Target().String(), nil
	case plumbing.HashReference:
		return ref.Hash().String(), nil
	default:
		return "", fmt.Errorf("reference %s has no value to store", ref.Name())
	}
}

// validRefSnapshotKey reports whether key is one this engine gives a snapshot.
// A manifest is data read from the store, and what it names is fetched.
func validRefSnapshotKey(key string) bool {
	name, ok := strings.CutPrefix(key, refSnapshotDirectory)
	if !ok || name == "" {
		return false
	}
	for _, r := range name {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') && r != '-' {
			return false
		}
	}
	return true
}

// refSnapshot is a parsed snapshot object: every reference the repository had
// when it was folded. It is immutable, and shared by every state that names it.
type refSnapshot struct {
	key  string
	refs map[plumbing.ReferenceName]*plumbing.Reference
}

func (s *refSnapshot) lookup(name plumbing.ReferenceName) *plumbing.Reference {
	if s == nil {
		return nil
	}
	return s.refs[name]
}

// encodeRefSnapshot writes references as "<value>\t<name>" lines in name order
// under a header line. A name cannot hold a tab, nor can a value.
func encodeRefSnapshot(refs map[plumbing.ReferenceName]*plumbing.Reference) ([]byte, error) {
	names := make([]string, 0, len(refs))
	for name := range refs {
		names = append(names, name.String())
	}
	sort.Strings(names)
	var encoded bytes.Buffer
	encoded.WriteString(refSnapshotHeader + "\n")
	for _, name := range names {
		value, err := referenceValue(refs[plumbing.ReferenceName(name)])
		if err != nil {
			return nil, err
		}
		encoded.WriteString(value + "\t" + name + "\n")
	}
	return encoded.Bytes(), nil
}

func decodeRefSnapshot(key string, data []byte) (*refSnapshot, error) {
	lines := bufio.NewScanner(bytes.NewReader(data))
	lines.Buffer(nil, 1<<20)
	if !lines.Scan() || lines.Text() != refSnapshotHeader {
		return nil, fmt.Errorf("%w: reference snapshot %s does not begin %q", ErrManifestFormat, key, refSnapshotHeader)
	}
	snapshot := &refSnapshot{key: key, refs: map[plumbing.ReferenceName]*plumbing.Reference{}}
	for lines.Scan() {
		value, name, found := strings.Cut(lines.Text(), "\t")
		if !found {
			return nil, fmt.Errorf("%w: reference snapshot %s: malformed line", ErrManifestFormat, key)
		}
		if err := checkSafeRefName(plumbing.ReferenceName(name)); err != nil {
			return nil, fmt.Errorf("%w: reference snapshot %s: %w", ErrManifestFormat, key, err)
		}
		ref, err := parseReferenceValue(plumbing.ReferenceName(name), value)
		if err != nil {
			return nil, fmt.Errorf("%w: reference snapshot %s: %w", ErrManifestFormat, key, err)
		}
		snapshot.refs[ref.Name()] = ref
	}
	if err := lines.Err(); err != nil {
		return nil, fmt.Errorf("%w: reference snapshot %s: %w", ErrManifestFormat, key, err)
	}
	return snapshot, nil
}

// draft is a manifest being edited by the mutations of one commit. It starts as
// a copy of the manifest the commit builds on, with the snapshot that manifest
// names to hand, so that a mutation can ask what a reference holds.
type draft struct {
	manifest manifest
	base     *refSnapshot
	// now is the commit's time, which is what a pack is added or retired at.
	now time.Time
}

func newDraft(from *manifest, base *refSnapshot, now time.Time) *draft {
	// A copy: a published manifest is shared with every reader and never edited.
	return (&draft{manifest: *from, base: base, now: now}).clone()
}

// clone copies the draft deeply enough that editing the copy leaves the
// original as it was, which is how one mutation of a group is undone when it
// refuses part way.
func (d *draft) clone() *draft {
	copied := *d
	copied.manifest.Packs = append([]manifestPack(nil), d.manifest.Packs...)
	copied.manifest.Retired = append([]retiredPack(nil), d.manifest.Retired...)
	copied.manifest.Refs.Changes = append([]refChange(nil), d.manifest.Refs.Changes...)
	return &copied
}

// changeIndex finds name in the change list, which is kept in name order.
func (d *draft) changeIndex(name string) (int, bool) {
	changes := d.manifest.Refs.Changes
	at := sort.Search(len(changes), func(i int) bool { return changes[i].Name >= name })
	return at, at < len(changes) && changes[at].Name == name
}

// reference returns what the draft holds under name, or nil.
func (d *draft) reference(name plumbing.ReferenceName) *plumbing.Reference {
	if at, changed := d.changeIndex(name.String()); changed {
		// A change that decoded once decodes again; see decodeManifest and set.
		ref, _ := d.manifest.Refs.Changes[at].reference()
		return ref
	}
	return d.base.lookup(name)
}

// references returns every reference of the draft, by name.
func (d *draft) references() map[plumbing.ReferenceName]*plumbing.Reference {
	return mergedReferences(d.base, d.manifest.Refs.Changes)
}

func mergedReferences(base *refSnapshot, changes []refChange) map[plumbing.ReferenceName]*plumbing.Reference {
	merged := map[plumbing.ReferenceName]*plumbing.Reference{}
	if base != nil {
		for name, ref := range base.refs {
			merged[name] = ref
		}
	}
	for _, change := range changes {
		ref, _ := change.reference()
		if ref == nil {
			delete(merged, plumbing.ReferenceName(change.Name))
			continue
		}
		merged[ref.Name()] = ref
	}
	return merged
}

func (d *draft) putChange(change refChange) {
	at, present := d.changeIndex(change.Name)
	changes := d.manifest.Refs.Changes
	if present {
		changes[at] = change
		return
	}
	changes = append(changes, refChange{})
	copy(changes[at+1:], changes[at:])
	changes[at] = change
	d.manifest.Refs.Changes = changes
}

// set writes a reference into the draft.
func (d *draft) set(ref *plumbing.Reference) error {
	if err := checkSafeRefName(ref.Name()); err != nil {
		return err
	}
	value, err := referenceValue(ref)
	if err != nil {
		return err
	}
	d.putChange(refChange{Name: ref.Name().String(), Value: value})
	return nil
}

// remove takes a reference out of the draft. One the snapshot holds needs a
// tombstone to shadow it; one that only ever lived in the change list is just
// dropped from it, so that creating and deleting branches leaves nothing behind.
func (d *draft) remove(name plumbing.ReferenceName) {
	if d.base.lookup(name) != nil {
		d.putChange(refChange{Name: name.String(), Deleted: true})
		return
	}
	if at, present := d.changeIndex(name.String()); present {
		changes := d.manifest.Refs.Changes
		d.manifest.Refs.Changes = append(changes[:at], changes[at+1:]...)
	}
}

func (d *draft) livePack(name string) bool {
	for _, pack := range d.manifest.Packs {
		if pack.Name == name {
			return true
		}
	}
	return false
}

func (d *draft) retiredIndex(name string) int {
	for i, pack := range d.manifest.Retired {
		if pack.Name == name {
			return i
		}
	}
	return -1
}

// errPackBeingSwept refuses the addition of a pack whose name the manifest has
// listed as an orphan for so long that a sweep may be deleting its keys. Pack
// names are digests of their contents, so this is someone pushing, byte for
// byte, a pack that was uploaded and never committed more than a grace period
// ago; the push is refused rather than let it name keys that are about to go.
var errPackBeingSwept = errors.New("gitstore: a pack of this name is being swept as an orphan; push again once it has gone")

// addPack makes a pack live. A pack that is live already is left as it is. One a
// compaction retired is not brought back: a retired pack's objects are all in
// the pack that replaced it, so the repository already holds what is being
// added. One retired as an orphan holds objects that may be nowhere else, and is
// brought back if its retirement is young enough that no sweep can be deleting
// it — a sweep deletes only what has been retired a whole grace period.
func (d *draft) addPack(pack manifestPack) error {
	if d.livePack(pack.Name) {
		return nil
	}
	if at := d.retiredIndex(pack.Name); at >= 0 {
		retired := d.manifest.Retired[at]
		if !retired.Orphan {
			return nil
		}
		if d.now.Sub(retired.Retired) >= retiredPackGrace/2 {
			return fmt.Errorf("%w: %s", errPackBeingSwept, pack.Name)
		}
		d.manifest.Retired = append(d.manifest.Retired[:at], d.manifest.Retired[at+1:]...)
	}
	pack.Added = d.now.UTC()
	d.manifest.Packs = append(d.manifest.Packs, pack)
	sort.Slice(d.manifest.Packs, func(i, j int) bool { return d.manifest.Packs[i].Name < d.manifest.Packs[j].Name })
	return nil
}

// errCompactionLostRace refuses a compaction's commit because a pack it merged
// is no longer live: another replica's compaction replaced it first.
var errCompactionLostRace = errors.New("gitstore: another compaction replaced these packs first")

// retire moves live packs to the retired list. Every one of them must be live.
func (d *draft) retire(names []string, orphan bool) error {
	for _, name := range names {
		if !d.livePack(name) {
			return fmt.Errorf("%w: %s is not live", errCompactionLostRace, name)
		}
	}
	gone := make(map[string]bool, len(names))
	for _, name := range names {
		gone[name] = true
		d.manifest.Retired = append(d.manifest.Retired, retiredPack{Name: name, Retired: d.now.UTC(), Orphan: orphan})
	}
	kept := d.manifest.Packs[:0:0]
	for _, pack := range d.manifest.Packs {
		if !gone[pack.Name] {
			kept = append(kept, pack)
		}
	}
	d.manifest.Packs = kept
	return nil
}

// ReferenceUpdate is one reference change of a push: move Name from Old to New.
// A zero Old means the reference must not exist, and a zero New removes it.
type ReferenceUpdate struct {
	Name plumbing.ReferenceName
	Old  plumbing.Hash
	New  plumbing.Hash
}

// apply makes the update in the draft if the reference is where the update
// expects it, and refuses with the error a caller of the one-at-a-time
// reference methods would have been given.
func (u ReferenceUpdate) apply(d *draft) error {
	if err := checkSafeRefName(u.Name); err != nil {
		return err
	}
	if u.Old.IsZero() && u.New.IsZero() {
		return fmt.Errorf("reference update of %s neither creates, moves nor removes it", u.Name)
	}
	current := d.reference(u.Name)
	switch {
	case u.Old.IsZero() && current != nil:
		return ErrReferenceAlreadyExists
	case !u.Old.IsZero() && (current == nil || current.Type() != plumbing.HashReference || current.Hash() != u.Old):
		return gitStorage.ErrReferenceHasChanged
	}
	if u.New.IsZero() {
		d.remove(u.Name)
		return nil
	}
	return d.set(plumbing.NewHashReference(u.Name, u.New))
}
