package bleephub

import (
	"compress/zlib"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/plumbing/format/packfile"
	"github.com/go-git/go-git/v5/plumbing/hash"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/plumbing/storer"
)

// Decides what a fetch answers with — which commits, which objects, and how
// they are packed — applying depth, shallow boundary, object filter, thin pack,
// and extra-tag requests.

// gitFetchBoundary is the slice of the commit graph one upload-pack answers
// with, plus the shallow boundary the client must record.
type gitFetchBoundary struct {
	// roots are the wanted commits the walk starts from, annotated tags peeled.
	roots []plumbing.Hash
	// order lists the commits to send, breadth-first from the roots, so the pack
	// is stable across runs rather than map-ordered.
	order []plumbing.Hash
	// included indexes order.
	included map[plumbing.Hash]bool
	// extras are wanted non-commit objects (annotated tags and their chain) the
	// pack carries alongside the commits.
	extras []plumbing.Hash
	// shallows are commits whose parents this response withholds; the client
	// records them in .git/shallow.
	shallows []plumbing.Hash
	// unshallows are commits the client records as shallow but whose parents
	// this response supplies, so it must forget the boundary there.
	unshallows []plumbing.Hash
}

// gitDepthLimit decides which commits a deepening request keeps. The zero value
// keeps everything (the ordinary non-shallow fetch).
type gitDepthLimit struct {
	// maxCommits bounds commits on any path from a start; zero is unbounded,
	// depth 1 keeps only the starting points.
	maxCommits int
	// since keeps only commits committed at or after this instant.
	since time.Time
	// excluded holds the commits reachable from any deepen-not reference.
	excluded map[plumbing.Hash]bool
}

// keep reports whether a commit reached at the given depth belongs in the
// response.
func (l gitDepthLimit) keep(commit *object.Commit, depth int) bool {
	if l.maxCommits > 0 && depth > l.maxCommits {
		return false
	}
	if !l.since.IsZero() && commit.Committer.When.Before(l.since) {
		return false
	}
	return !l.excluded[commit.Hash]
}

// gitDepthLimitFor turns the deepen lines of a request into the walk predicate.
//
// A deepen-relative walk starts at the boundary the client already records, and
// that boundary is itself the first commit walked, so its budget is one larger
// than the commits the client asked to gain. A deepen-relative request with no
// boundary extends an already-complete history, so it keeps all of it.
func gitDepthLimitFor(stor storer.Storer, request *gitUploadRequest, relative bool) (gitDepthLimit, error) {
	limit := gitDepthLimit{maxCommits: request.depth, since: request.since}
	switch {
	case relative && request.depth > 0:
		limit.maxCommits = request.depth + 1
	case request.deepenRelative:
		limit.maxCommits = 0
	}
	for _, name := range request.deepenNot {
		hash, err := resolveGitDeepenNot(stor, name)
		if err != nil {
			return gitDepthLimit{}, err
		}
		reachable, err := gitReachableCommits(stor, hash)
		if err != nil {
			return gitDepthLimit{}, err
		}
		if limit.excluded == nil {
			limit.excluded = make(map[plumbing.Hash]bool, len(reachable))
		}
		for commit := range reachable {
			limit.excluded[commit] = true
		}
	}
	return limit, nil
}

// resolveGitDeepenNot resolves the reference a deepen-not line names, applying
// git's short-name expansion and accepting a raw object id, both legal
// --shallow-exclude arguments.
func resolveGitDeepenNot(stor storer.Storer, name string) (plumbing.Hash, error) {
	for _, rule := range plumbing.RefRevParseRules {
		ref, err := storer.ResolveReference(stor, plumbing.ReferenceName(fmt.Sprintf(rule, name)))
		if err == nil && ref != nil {
			return ref.Hash(), nil
		}
	}
	if hash := plumbing.NewHash(name); !hash.IsZero() {
		if _, err := stor.EncodedObject(plumbing.AnyObject, hash); err == nil {
			return hash, nil
		}
	}
	// git's own wording, so an error search finds the same answers.
	return plumbing.ZeroHash, &gitClientRefusal{reason: "git upload-pack: deepen-not is not a ref: deepen-not " + name}
}

// gitReachableCommits collects every commit reachable from a starting object,
// peeling an annotated tag first.
func gitReachableCommits(stor storer.EncodedObjectStorer, from plumbing.Hash) (map[plumbing.Hash]bool, error) {
	start, err := peelGitObjectToCommit(stor, from)
	if err != nil {
		return nil, err
	}
	reachable := map[plumbing.Hash]bool{start: true}
	queue := []plumbing.Hash{start}
	for len(queue) > 0 {
		hash := queue[0]
		queue = queue[1:]
		commit, err := object.GetCommit(stor, hash)
		if err != nil {
			return nil, err
		}
		for _, parent := range commit.ParentHashes {
			if reachable[parent] {
				continue
			}
			reachable[parent] = true
			queue = append(queue, parent)
		}
	}
	return reachable, nil
}

// peelGitObjectToCommit follows a chain of annotated tags down to the commit it
// names.
func peelGitObjectToCommit(stor storer.EncodedObjectStorer, hash plumbing.Hash) (plumbing.Hash, error) {
	for {
		encoded, err := stor.EncodedObject(plumbing.AnyObject, hash)
		if err != nil {
			return plumbing.ZeroHash, err
		}
		if encoded.Type() != plumbing.TagObject {
			return hash, nil
		}
		tag, err := object.GetTag(stor, hash)
		if err != nil {
			return plumbing.ZeroHash, err
		}
		hash = tag.Target
	}
}

// gitFetchBoundaryFor walks the commit graph outward from the wants and stops
// where the request's depth says to.
//
// The walk is breadth-first, so a commit's first-reached distance is final —
// which makes "deepen <n>" mean n commits along every path. A commit is a
// shallow boundary when it is included but a parent is not, so a root or a
// --depth past the history produces no boundary. With deepen-relative the walk
// starts at the client's recorded boundary rather than the wants.
func gitFetchBoundaryFor(stor storer.Storer, request *gitUploadRequest) (*gitFetchBoundary, error) {
	roots, extras, err := peelGitWants(stor, request.wants)
	if err != nil {
		return nil, err
	}
	boundary := &gitFetchBoundary{roots: roots, included: map[plumbing.Hash]bool{}, extras: extras}

	walkFrom := roots
	relative := false
	if request.deepenRelative && len(request.shallows) > 0 {
		reachable, err := gitReachableClientBoundary(stor, roots, request.shallows)
		if err != nil {
			return nil, err
		}
		if len(reachable) > 0 {
			walkFrom, relative = reachable, true
		}
	}
	limit, err := gitDepthLimitFor(stor, request, relative)
	if err != nil {
		return nil, err
	}
	type pendingCommit struct {
		hash  plumbing.Hash
		depth int
	}
	queued := make(map[plumbing.Hash]bool, len(walkFrom))
	queue := make([]pendingCommit, 0, len(walkFrom))
	for _, root := range walkFrom {
		if queued[root] {
			continue
		}
		queued[root] = true
		queue = append(queue, pendingCommit{hash: root, depth: 1})
	}
	parents := map[plumbing.Hash][]plumbing.Hash{}
	for len(queue) > 0 {
		next := queue[0]
		queue = queue[1:]
		commit, err := object.GetCommit(stor, next.hash)
		if err != nil {
			return nil, err
		}
		if !limit.keep(commit, next.depth) {
			continue
		}
		boundary.included[next.hash] = true
		boundary.order = append(boundary.order, next.hash)
		parents[next.hash] = commit.ParentHashes
		for _, parent := range commit.ParentHashes {
			if queued[parent] {
				continue
			}
			queued[parent] = true
			queue = append(queue, pendingCommit{hash: parent, depth: next.depth + 1})
		}
	}

	truncated := make(map[plumbing.Hash]bool, len(boundary.order))
	for _, hash := range boundary.order {
		for _, parent := range parents[hash] {
			if !boundary.included[parent] {
				truncated[hash] = true
				break
			}
		}
	}
	clientShallow := make(map[plumbing.Hash]bool, len(request.shallows))
	for _, hash := range request.shallows {
		clientShallow[hash] = true
	}
	for _, hash := range boundary.order {
		// A boundary the client already recorded is not restated.
		if truncated[hash] && !clientShallow[hash] {
			boundary.shallows = append(boundary.shallows, hash)
		}
	}
	for _, hash := range request.shallows {
		if boundary.included[hash] && !truncated[hash] {
			boundary.unshallows = append(boundary.unshallows, hash)
		}
	}
	return boundary, nil
}

// gitReachableClientBoundary picks the deepen-relative walk's starting commits:
// the client's recorded shallow boundary, restricted to the part the wants
// actually reach, so a boundary from an unrelated branch pulls nothing in.
func gitReachableClientBoundary(stor storer.EncodedObjectStorer, roots, shallows []plumbing.Hash) ([]plumbing.Hash, error) {
	wanted := make(map[plumbing.Hash]bool, len(shallows))
	for _, hash := range shallows {
		wanted[hash] = true
	}
	found := make(map[plumbing.Hash]bool, len(shallows))
	var reachable []plumbing.Hash
	visited := make(map[plumbing.Hash]bool, len(roots))
	queue := append([]plumbing.Hash(nil), roots...)
	for _, root := range roots {
		visited[root] = true
	}
	for len(queue) > 0 && len(found) < len(wanted) {
		hash := queue[0]
		queue = queue[1:]
		if wanted[hash] && !found[hash] {
			found[hash] = true
			reachable = append(reachable, hash)
		}
		commit, err := object.GetCommit(stor, hash)
		if err != nil {
			return nil, err
		}
		for _, parent := range commit.ParentHashes {
			if visited[parent] {
				continue
			}
			visited[parent] = true
			queue = append(queue, parent)
		}
	}
	return reachable, nil
}

// peelGitWants splits the wanted object ids into the commits the walk starts
// from and the objects sent as-is. A wanted annotated tag is carried itself and
// the walk starts at the commit it names; a wanted tree or blob (a partial
// clone refetching a filtered object) is carried the same way.
func peelGitWants(stor storer.EncodedObjectStorer, wants []plumbing.Hash) (roots, extras []plumbing.Hash, err error) {
	seen := make(map[plumbing.Hash]bool, len(wants))
	for _, want := range wants {
		hash := want
		for !seen[hash] {
			encoded, err := stor.EncodedObject(plumbing.AnyObject, hash)
			if errors.Is(err, plumbing.ErrObjectNotFound) {
				// git's own wording, so the client prints the reason.
				return nil, nil, &gitClientRefusal{reason: "upload-pack: not our ref " + hash.String()}
			}
			if err != nil {
				return nil, nil, err
			}
			seen[hash] = true
			if encoded.Type() == plumbing.CommitObject {
				roots = append(roots, hash)
				break
			}
			extras = append(extras, hash)
			if encoded.Type() != plumbing.TagObject {
				break
			}
			tag, err := object.GetTag(stor, hash)
			if err != nil {
				return nil, nil, err
			}
			hash = tag.Target
		}
	}
	return roots, extras, nil
}

// gitPackPlan is the finished answer to "what goes in the pack".
type gitPackPlan struct {
	// objects lists the object ids to write, in the order they were found.
	objects []plumbing.Hash
	// paths records the tree path each object was reached at; two versions of a
	// file share a path, making one a good delta base for the other.
	paths map[plumbing.Hash]string
	// packed indexes objects.
	packed map[plumbing.Hash]bool
	// clientAt is the object the client already holds at each tree path — never
	// carried in this pack, so a delta against one is the thin part of a thin pack.
	clientAt map[string]plumbing.Hash
}

// gitObjectsToSend enumerates the objects the packfile must carry.
//
// Objects the client already has are subtracted first. A shallow client needs
// care: the have-walk stops at every commit it declared shallow, keeping that
// commit's tree (which it has) and leaving the parents beyond to be sent. The
// have list is what makes a fetch incremental rather than resending all history.
func gitObjectsToSend(stor storer.Storer, boundary *gitFetchBoundary, request *gitUploadRequest, count func(int)) (*gitPackPlan, error) {
	plan := &gitPackPlan{
		paths:    map[plumbing.Hash]string{},
		packed:   map[plumbing.Hash]bool{},
		clientAt: map[string]plumbing.Hash{},
	}
	seen := map[plumbing.Hash]bool{}
	stopAt := make(map[plumbing.Hash]bool, len(request.shallows))
	for _, hash := range request.shallows {
		stopAt[hash] = true
	}
	if err := collectGitHaveObjects(stor, request.haves, stopAt, seen, plan.clientAt); err != nil {
		return nil, err
	}

	wanted := request.wantedSet()
	emit := func(hash plumbing.Hash, path string) {
		plan.objects = append(plan.objects, hash)
		plan.packed[hash] = true
		plan.paths[hash] = path
		if count != nil {
			count(len(plan.objects))
		}
	}
	for _, hash := range boundary.extras {
		if seen[hash] {
			continue
		}
		encoded, err := stor.EncodedObject(plumbing.AnyObject, hash)
		if err != nil {
			return nil, err
		}
		if encoded.Type() == plumbing.TreeObject {
			// A wanted tree is a partial clone refetching a filtered-out directory;
			// owe the whole subtree below it, or the client round-trips per level.
			if err := collectGitTree(stor, hash, request.filter, wanted, seen, emit); err != nil {
				return nil, err
			}
			continue
		}
		seen[hash] = true
		emit(hash, "")
	}
	for _, hash := range boundary.order {
		if seen[hash] {
			continue
		}
		commit, err := object.GetCommit(stor, hash)
		if err != nil {
			return nil, err
		}
		seen[hash] = true
		emit(hash, "")
		if err := collectGitTree(stor, commit.TreeHash, request.filter, wanted, seen, emit); err != nil {
			return nil, err
		}
	}
	if request.includeTag {
		if err := appendGitIncludedTags(stor, plan); err != nil {
			return nil, err
		}
	}
	return plan, nil
}

// collectGitHaveObjects marks everything the client already holds, walking its
// have list back through the graph but stopping at stopAt — the commits it
// declared as its shallow boundary.
//
// It also records which object the client holds at each tree path, keeping the
// first reached: the client's most recent version, the closest delta base. A
// have naming an object this server lacks is ignored, not fatal.
func collectGitHaveObjects(stor storer.EncodedObjectStorer, haves []plumbing.Hash, stopAt, seen map[plumbing.Hash]bool, at map[string]plumbing.Hash) error {
	visited := make(map[plumbing.Hash]bool, len(haves))
	queue := append([]plumbing.Hash(nil), haves...)
	record := func(hash plumbing.Hash, path string) {
		if _, taken := at[path]; !taken {
			at[path] = hash
		}
	}
	for len(queue) > 0 {
		hash := queue[0]
		queue = queue[1:]
		if visited[hash] {
			continue
		}
		visited[hash] = true
		commit, err := object.GetCommit(stor, hash)
		if err != nil {
			continue
		}
		seen[hash] = true
		if err := collectGitTree(stor, commit.TreeHash, gitObjectFilter{}, nil, seen, record); err != nil {
			return err
		}
		if stopAt[hash] {
			continue
		}
		queue = append(queue, commit.ParentHashes...)
	}
	return nil
}

// gitTreeFrame is one directory the walk is inside.
type gitTreeFrame struct {
	hash  plumbing.Hash
	path  string
	depth int
	// match is the sparse decision for this directory, inherited by a path
	// beneath it that no pattern names.
	match   gitPatternMatch
	entries []object.TreeEntry
	next    int
	// omitted records that something below was left out at this path, so the
	// directory is not treated as settled for other paths it appears at.
	omitted bool
}

// gitTreeWalk carries the state one collectGitTree walk shares.
type gitTreeWalk struct {
	stor   storer.EncodedObjectStorer
	filter gitObjectFilter
	wanted map[plumbing.Hash]bool
	seen   map[plumbing.Hash]bool
	emit   func(plumbing.Hash, string)
	// visited and complete are built only for a path-deciding filter. visited is
	// the (tree, path) pairs already walked; complete holds trees whose whole
	// subtree was included, skippable wherever else they appear.
	visited  map[string]bool
	complete map[plumbing.Hash]bool
}

// collectGitTree records a tree and everything beneath it, skipping a subtree
// already in seen. Submodule entries name commits in another repository and are
// not sent. The filter decides which objects are carried, but a wanted object
// is carried regardless, and a dropped tree is not descended into. The walk is
// depth-first (git's traversal order) and iterative, so a deep tree costs heap,
// not stack.
func collectGitTree(stor storer.EncodedObjectStorer, root plumbing.Hash, filter gitObjectFilter, wanted, seen map[plumbing.Hash]bool, emit func(plumbing.Hash, string)) error {
	walk := &gitTreeWalk{stor: stor, filter: filter, wanted: wanted, seen: seen, emit: emit}
	if filter.pathDependent() {
		walk.visited = map[string]bool{}
		walk.complete = map[plumbing.Hash]bool{}
	}
	return walk.run(root)
}

// run walks a tree, its subtrees and their blobs.
func (w *gitTreeWalk) run(root plumbing.Hash) error {
	frame, err := w.enter(root, "", 0, gitPatternNotMatched)
	if err != nil || frame == nil {
		return err
	}
	stack := []*gitTreeFrame{frame}
	for len(stack) > 0 {
		current := stack[len(stack)-1]
		if current.next >= len(current.entries) {
			stack = stack[:len(stack)-1]
			w.leave(current)
			if len(stack) > 0 && current.omitted {
				stack[len(stack)-1].omitted = true
			}
			continue
		}
		entry := current.entries[current.next]
		current.next++
		if entry.Mode == filemode.Submodule {
			continue
		}
		path := entry.Name
		if current.path != "" {
			path = current.path + "/" + entry.Name
		}
		if entry.Mode == filemode.Dir {
			child, err := w.enter(entry.Hash, path, current.depth+1, current.match)
			if err != nil {
				return err
			}
			if child != nil {
				stack = append(stack, child)
			}
			continue
		}
		omitted, err := w.blob(entry.Hash, path, current.depth+1, current.match)
		if err != nil {
			return err
		}
		if omitted {
			current.omitted = true
		}
	}
	return nil
}

// enter opens a directory, emitting its tree unless already emitted or filtered
// out, and returns the frame to continue in (nil when nothing is left below).
func (w *gitTreeWalk) enter(hash plumbing.Hash, path string, depth int, inherited gitPatternMatch) (*gitTreeFrame, error) {
	if w.visited == nil {
		if w.seen[hash] {
			return nil, nil
		}
	} else {
		// The same tree can sit at several paths, and a sparse spec may select one
		// and not another, so a tree is settled only once a visit omits nothing below.
		if w.complete[hash] {
			return nil, nil
		}
		key := hash.String() + "\x00" + path
		if w.visited[key] {
			return nil, nil
		}
		w.visited[key] = true
	}
	match := inherited
	if w.visited != nil {
		if decided := w.filter.matchSparse(path, true); decided != gitPatternUndecided {
			match = decided
		}
	}
	if w.filter.omitsTree(depth) && !w.wanted[hash] {
		w.seen[hash] = true
		return nil, nil
	}
	tree, err := object.GetTree(w.stor, hash)
	if err != nil {
		return nil, err
	}
	if !w.seen[hash] {
		w.seen[hash] = true
		w.emit(hash, path)
	}
	return &gitTreeFrame{hash: hash, path: path, depth: depth, match: match, entries: tree.Entries}, nil
}

// leave closes an exhausted directory, recording a subtree carried whole so no
// other path through it is walked again.
func (w *gitTreeWalk) leave(frame *gitTreeFrame) {
	if w.visited != nil && !frame.omitted {
		w.complete[frame.hash] = true
	}
}

// blob decides one file, emitting it or reporting it was left out. A blob the
// filter drops at this path is not settled when the filter reads paths, because
// the same content may sit at another selected path.
func (w *gitTreeWalk) blob(hash plumbing.Hash, path string, depth int, inherited gitPatternMatch) (bool, error) {
	if w.seen[hash] {
		return false, nil
	}
	omit, err := w.filter.omitsBlob(w.stor, hash, depth)
	if err != nil {
		return false, err
	}
	if !omit && w.visited != nil {
		match := w.filter.matchSparse(path, false)
		if match == gitPatternUndecided {
			match = inherited
		}
		omit = match != gitPatternMatched
	}
	if omit && !w.wanted[hash] {
		if w.visited == nil {
			w.seen[hash] = true
		}
		return true, nil
	}
	w.seen[hash] = true
	w.emit(hash, path)
	return false, nil
}

// appendGitIncludedTags adds the annotated tags that point into the pack — the
// include-tag capability: a branch fetch gets its tags without a second round
// trip. A tag chain is added bottom-up so a tag of a tag follows its target.
func appendGitIncludedTags(stor storer.Storer, plan *gitPackPlan) error {
	iter, err := stor.IterReferences()
	if err != nil {
		return err
	}
	return iter.ForEach(func(ref *plumbing.Reference) error {
		if ref.Type() != plumbing.HashReference || !ref.Name().IsTag() {
			return nil
		}
		var chain []plumbing.Hash
		target := ref.Hash()
		for {
			encoded, err := stor.EncodedObject(plumbing.AnyObject, target)
			if err != nil {
				return nil
			}
			if encoded.Type() != plumbing.TagObject {
				break
			}
			chain = append(chain, target)
			tag, err := object.GetTag(stor, target)
			if err != nil {
				return nil
			}
			target = tag.Target
		}
		if len(chain) == 0 || !plan.packed[target] {
			return nil
		}
		for i := len(chain) - 1; i >= 0; i-- {
			if plan.packed[chain[i]] {
				continue
			}
			plan.packed[chain[i]] = true
			plan.paths[chain[i]] = ""
			plan.objects = append(plan.objects, chain[i])
		}
		return nil
	})
}

// gitFilterKind is one of the object-filter forms git's partial clone defines.
type gitFilterKind int

const (
	gitFilterBlobNone gitFilterKind = iota
	gitFilterBlobLimit
	gitFilterTreeDepth
	gitFilterSparseOID
)

// gitFilterRule is one filter-spec. A combined spec holds several, and an
// object is omitted when any of them omits it.
type gitFilterRule struct {
	kind      gitFilterKind
	blobLimit int64
	treeDepth int
	// patterns are the sparse-checkout patterns a sparse:oid rule selects
	// paths with.
	patterns []gitSparsePattern
}

// gitObjectFilter is the partial-clone filter a request carries. The zero value
// keeps every object, which is the ordinary complete fetch.
type gitObjectFilter struct {
	rules []gitFilterRule
}

// omitsTree reports whether a tree at the given depth from the root is dropped;
// the root is depth zero, so "tree:0" leaves only commits and tags.
//
// A sparse rule never answers here: git's sparse filter shows every tree it
// walks so the client knows the directory skeleton and can widen the cone later.
func (f gitObjectFilter) omitsTree(depth int) bool {
	for _, rule := range f.rules {
		if rule.kind == gitFilterTreeDepth && depth >= rule.treeDepth {
			return true
		}
	}
	return false
}

// pathDependent reports whether the filter decides an object by its path rather
// than the object itself, which bars the shortcuts an object-only filter allows.
func (f gitObjectFilter) pathDependent() bool {
	for _, rule := range f.rules {
		if rule.kind == gitFilterSparseOID {
			return true
		}
	}
	return false
}

// matchSparse decides a path against every sparse rule; any rule that excludes
// it excludes it, and a path none name is left undecided for the enclosing
// directory to answer.
func (f gitObjectFilter) matchSparse(path string, isDir bool) gitPatternMatch {
	decision := gitPatternUndecided
	for _, rule := range f.rules {
		if rule.kind != gitFilterSparseOID {
			continue
		}
		switch matchGitSparsePath(rule.patterns, path, isDir) {
		case gitPatternNotMatched:
			return gitPatternNotMatched
		case gitPatternMatched:
			decision = gitPatternMatched
		case gitPatternUndecided:
		}
	}
	return decision
}

// omitsBlob reports whether a blob at the given depth is dropped. The size is
// read only when a rule needs it, so "blob:none" costs no extra lookups.
func (f gitObjectFilter) omitsBlob(stor storer.EncodedObjectStorer, blob plumbing.Hash, depth int) (bool, error) {
	needsSize := false
	for _, rule := range f.rules {
		switch rule.kind {
		case gitFilterBlobNone:
			return true, nil
		case gitFilterTreeDepth:
			// A blob sits one level below the tree that names it, so the same
			// depth cut that drops a tree drops the blobs it holds.
			if depth >= rule.treeDepth {
				return true, nil
			}
		case gitFilterBlobLimit:
			needsSize = true
		}
	}
	if !needsSize {
		return false, nil
	}
	encoded, err := stor.EncodedObject(plumbing.BlobObject, blob)
	if err != nil {
		return false, err
	}
	for _, rule := range f.rules {
		if rule.kind == gitFilterBlobLimit && encoded.Size() >= rule.blobLimit {
			return true, nil
		}
	}
	return false, nil
}

// parseGitObjectFilter reads a filter-spec, refusing an unsupported one with
// git's own message so the client prints a reason. stor is needed because
// "sparse:oid" names its patterns by a blob-ish only this repository resolves.
func parseGitObjectFilter(stor storer.Storer, spec string) (gitObjectFilter, error) {
	if spec == "" {
		return gitObjectFilter{}, nil
	}
	if rest, found := strings.CutPrefix(spec, "combine:"); found {
		var combined gitObjectFilter
		for _, part := range strings.Split(rest, "+") {
			unescaped, err := url.QueryUnescape(part)
			if err != nil {
				return gitObjectFilter{}, &gitClientRefusal{reason: "invalid filter-spec '" + spec + "'"}
			}
			nested, err := parseGitObjectFilter(stor, unescaped)
			if err != nil {
				return gitObjectFilter{}, err
			}
			combined.rules = append(combined.rules, nested.rules...)
		}
		return combined, nil
	}
	switch {
	case spec == "blob:none":
		return gitObjectFilter{rules: []gitFilterRule{{kind: gitFilterBlobNone}}}, nil
	case strings.HasPrefix(spec, "blob:limit="):
		limit, err := parseGitBlobLimit(strings.TrimPrefix(spec, "blob:limit="))
		if err != nil {
			return gitObjectFilter{}, &gitClientRefusal{reason: "invalid filter-spec '" + spec + "'"}
		}
		return gitObjectFilter{rules: []gitFilterRule{{kind: gitFilterBlobLimit, blobLimit: limit}}}, nil
	case strings.HasPrefix(spec, "tree:"):
		depth, err := strconv.Atoi(strings.TrimPrefix(spec, "tree:"))
		if err != nil || depth < 0 {
			return gitObjectFilter{}, &gitClientRefusal{reason: "invalid filter-spec '" + spec + "'"}
		}
		return gitObjectFilter{rules: []gitFilterRule{{kind: gitFilterTreeDepth, treeDepth: depth}}}, nil
	case strings.HasPrefix(spec, "sparse:oid="):
		name := strings.TrimPrefix(spec, "sparse:oid=")
		content, err := readGitBlobIsh(stor, name)
		if err != nil {
			// git's own wording.
			return gitObjectFilter{}, &gitClientRefusal{reason: "unable to access sparse blob in '" + name + "'"}
		}
		return gitObjectFilter{rules: []gitFilterRule{{kind: gitFilterSparseOID, patterns: parseGitSparsePatterns(content)}}}, nil
	}
	return gitObjectFilter{}, &gitClientRefusal{reason: "invalid filter-spec '" + spec + "'"}
}

// readGitBlobIsh resolves the blob a "sparse:oid" filter names and returns its
// content. git resolves the argument as a blob-ish, so both a raw object id and
// a "<rev>:<path>" expression (a pattern file committed to the repo) work.
func readGitBlobIsh(stor storer.Storer, name string) ([]byte, error) {
	hash, err := resolveGitBlobIshHash(stor, name)
	if err != nil {
		return nil, err
	}
	blob, err := object.GetBlob(stor, hash)
	if err != nil {
		return nil, err
	}
	reader, err := blob.Reader()
	if err != nil {
		return nil, err
	}
	defer func() { _ = reader.Close() }()
	return io.ReadAll(io.LimitReader(reader, gitMaxSparseFilterBlobSize))
}

// gitMaxSparseFilterBlobSize bounds how much of a named pattern file is read,
// so a client cannot make the server hold an arbitrary blob in memory.
const gitMaxSparseFilterBlobSize = 8 << 20

// resolveGitBlobIshHash resolves a blob-ish expression to a blob's object id,
// following annotated tags and reading a path out of a tree when named.
func resolveGitBlobIshHash(stor storer.Storer, name string) (plumbing.Hash, error) {
	if revision, path, found := strings.Cut(name, ":"); found {
		start, err := resolveGitRevision(stor, revision)
		if err != nil {
			return plumbing.ZeroHash, err
		}
		commit, err := peelGitObjectToCommit(stor, start)
		if err != nil {
			return plumbing.ZeroHash, err
		}
		tree, err := gitTreeOfCommitObject(stor, commit)
		if err != nil {
			return plumbing.ZeroHash, err
		}
		entry, err := tree.FindEntry(path)
		if err != nil {
			return plumbing.ZeroHash, err
		}
		return entry.Hash, nil
	}
	hash, err := resolveGitRevision(stor, name)
	if err != nil {
		return plumbing.ZeroHash, err
	}
	for {
		encoded, err := stor.EncodedObject(plumbing.AnyObject, hash)
		if err != nil {
			return plumbing.ZeroHash, err
		}
		if encoded.Type() != plumbing.TagObject {
			return hash, nil
		}
		tag, err := object.GetTag(stor, hash)
		if err != nil {
			return plumbing.ZeroHash, err
		}
		hash = tag.Target
	}
}

// resolveGitRevision turns a reference name or object id into the object it
// names, using git's rev-parse short-name expansion.
func resolveGitRevision(stor storer.Storer, revision string) (plumbing.Hash, error) {
	for _, rule := range plumbing.RefRevParseRules {
		ref, err := storer.ResolveReference(stor, plumbing.ReferenceName(fmt.Sprintf(rule, revision)))
		if err == nil && ref != nil {
			return ref.Hash(), nil
		}
	}
	hash, err := parseGitObjectID(revision)
	if err != nil {
		return plumbing.ZeroHash, err
	}
	if _, err := stor.EncodedObject(plumbing.AnyObject, hash); err != nil {
		return plumbing.ZeroHash, err
	}
	return hash, nil
}

// gitTreeOfCommitObject reads the tree a commit points at.
func gitTreeOfCommitObject(stor storer.Storer, hash plumbing.Hash) (*object.Tree, error) {
	commit, err := object.GetCommit(stor, hash)
	if err != nil {
		return nil, err
	}
	return object.GetTree(stor, commit.TreeHash)
}

// parseGitBlobLimit reads the size a "blob:limit=" spec cuts at, in the plain,
// k, m, and g spellings git accepts. The width is int64 (an object's size), and
// a spec whose product would overflow is refused rather than wrapping negative.
func parseGitBlobLimit(text string) (int64, error) {
	multiplier := int64(1)
	if len(text) > 0 {
		switch text[len(text)-1] {
		case 'k', 'K':
			multiplier = 1 << 10
		case 'm', 'M':
			multiplier = 1 << 20
		case 'g', 'G':
			multiplier = 1 << 30
		}
		if multiplier != 1 {
			text = text[:len(text)-1]
		}
	}
	value, err := strconv.ParseInt(text, 10, 64)
	if err != nil {
		return 0, err
	}
	if value < 0 {
		return 0, errors.New("blob size limit is negative")
	}
	if value > math.MaxInt64/multiplier {
		return 0, errors.New("blob size limit out of range")
	}
	return value * multiplier, nil
}

// gitMaxDeltaObjectSize bounds objects a thin pack will deltify; computing a
// delta loads base and target into memory, so past this the object is sent whole.
const gitMaxDeltaObjectSize = 16 << 20

// gitPackVersion is the packfile format version every git client understands.
const gitPackVersion = 2

// writeGitPackfile writes the packfile a finished plan describes: the thin
// encoder when the client asked for a thin pack and holds delta bases, else
// reused stored packfiles when they cover the plan (see git_packreuse.go), else
// go-git's encoder.
func writeGitPackfile(band *gitBandWriter, stor storer.EncodedObjectStorer, plan *gitPackPlan, thin bool) error {
	if thin && len(plan.clientAt) > 0 {
		return writeGitThinPack(band, stor, plan)
	}
	if packDir := gitPackDirOf(stor); packDir != nil && len(plan.objects) > 0 {
		reusable, err := gitReusablePacks(packDir, plan)
		if err != nil {
			return err
		}
		if len(reusable) > 0 {
			return writeGitReusedPackfile(band, stor, packDir, reusable, plan)
		}
	}
	encoder := packfile.NewEncoder(band.pack(), stor, false)
	if _, err := encoder.Encode(plan.objects, gitPackWindow); err != nil {
		return err
	}
	band.progressf("Compressing objects: 100%% (%d/%d), done.\n", len(plan.objects), len(plan.objects))
	return nil
}

// writeGitThinPack writes a packfile whose entries may be deltas against objects
// not in it. The base is the object the client holds at the same tree path,
// always resolvable because its have lines put it in the table; nothing in the
// table is also in the pack, so every delta is genuinely external.
//
// go-git's packfile.Encoder cannot express an externally based delta (the entry
// points that could are unexported), so the entries are written here.
func writeGitThinPack(band *gitBandWriter, stor storer.EncodedObjectStorer, plan *gitPackPlan) error {
	out := band.pack()
	digest := plumbing.Hasher{Hash: hash.New(hash.CryptoType)}
	stream := io.MultiWriter(out, digest)
	if err := writeGitPackHeader(stream, len(plan.objects)); err != nil {
		return err
	}
	compressor := zlib.NewWriter(stream)
	for index, id := range plan.objects {
		encoded, err := stor.EncodedObject(plumbing.AnyObject, id)
		if err != nil {
			return err
		}
		delta, base, err := gitThinDelta(stor, plan, id, encoded)
		if err != nil {
			return err
		}
		if delta != nil {
			err = writeGitPackDelta(stream, compressor, base, delta)
		} else {
			err = writeGitPackObject(stream, compressor, encoded)
		}
		if err != nil {
			return err
		}
		if (index+1)%gitProgressInterval == 0 {
			band.progressf("Compressing objects: %d%% (%d/%d)\r",
				(index+1)*100/len(plan.objects), index+1, len(plan.objects))
		}
	}
	band.progressf("Compressing objects: 100%% (%d/%d), done.\n", len(plan.objects), len(plan.objects))
	trailer := digest.Sum()
	_, err := out.Write(trailer[:])
	return err
}

// gitThinDelta computes the delta from the client's version of an object to the
// one being sent, returning it only when genuinely smaller than the object.
func gitThinDelta(stor storer.EncodedObjectStorer, plan *gitPackPlan, id plumbing.Hash, target plumbing.EncodedObject) (plumbing.EncodedObject, plumbing.Hash, error) {
	if target.Type() != plumbing.BlobObject && target.Type() != plumbing.TreeObject {
		return nil, plumbing.ZeroHash, nil
	}
	if target.Size() <= 0 || target.Size() > gitMaxDeltaObjectSize {
		return nil, plumbing.ZeroHash, nil
	}
	baseHash, known := plan.clientAt[plan.paths[id]]
	if !known || baseHash == id {
		return nil, plumbing.ZeroHash, nil
	}
	base, err := stor.EncodedObject(target.Type(), baseHash)
	if err != nil || base.Size() <= 0 || base.Size() > gitMaxDeltaObjectSize {
		return nil, plumbing.ZeroHash, nil
	}
	delta, err := packfile.GetDelta(base, target)
	if err != nil {
		return nil, plumbing.ZeroHash, err
	}
	if delta.Size() >= target.Size() {
		return nil, plumbing.ZeroHash, nil
	}
	return delta, baseHash, nil
}

// gitPackMaxEntries is the largest object count a pack header can state (a
// four-byte field).
const gitPackMaxEntries = 1<<32 - 1

// writeGitPackHeader writes the twelve opening bytes: signature, version, and
// entry count. A count the field cannot hold is an error, not a truncation,
// which would desync the header from the body and lose objects.
func writeGitPackHeader(w io.Writer, entries int) error {
	if entries < 0 || int64(entries) > gitPackMaxEntries {
		return fmt.Errorf("pack header cannot state %d entries", entries)
	}
	header := make([]byte, 12)
	copy(header, "PACK")
	binary.BigEndian.PutUint32(header[4:], gitPackVersion)
	binary.BigEndian.PutUint32(header[8:], uint32(entries))
	_, err := w.Write(header)
	return err
}

// writeGitPackObject writes one whole object: its type-and-size header and the
// deflated content.
func writeGitPackObject(w io.Writer, compressor *zlib.Writer, encoded plumbing.EncodedObject) error {
	if err := writeGitPackEntryHeader(w, encoded.Type(), encoded.Size()); err != nil {
		return err
	}
	reader, err := encoded.Reader()
	if err != nil {
		return err
	}
	defer func() { _ = reader.Close() }()
	compressor.Reset(w)
	if _, err := io.Copy(compressor, reader); err != nil {
		return err
	}
	return compressor.Close()
}

// writeGitPackDelta writes one reference delta: the header, the object id of
// the base the client already holds, and the deflated delta instructions.
func writeGitPackDelta(w io.Writer, compressor *zlib.Writer, base plumbing.Hash, delta plumbing.EncodedObject) error {
	if err := writeGitPackEntryHeader(w, plumbing.REFDeltaObject, delta.Size()); err != nil {
		return err
	}
	if _, err := w.Write(base[:]); err != nil {
		return err
	}
	reader, err := delta.Reader()
	if err != nil {
		return err
	}
	defer func() { _ = reader.Close() }()
	compressor.Reset(w)
	if _, err := io.Copy(compressor, reader); err != nil {
		return err
	}
	return compressor.Close()
}

// writeGitPackEntryHeader writes an entry header: the object type in three bits
// and the uncompressed size as a little-endian base-128 run. The run ends when
// the size shifts to zero, which a negative size never reaches, so it is refused.
func writeGitPackEntryHeader(w io.Writer, objectType plumbing.ObjectType, size int64) error {
	if size < 0 {
		return fmt.Errorf("pack entry header cannot state a size of %d", size)
	}
	header := make([]byte, 0, 10)
	// The first byte carries the type plus four bits of size; each later byte
	// carries seven more.
	current := byte((int64(objectType)<<4 | size&0x0f) & 0xff)
	size >>= 4
	for size != 0 {
		header = append(header, current|0x80)
		current = byte(size & 0x7f)
		size >>= 7
	}
	header = append(header, current)
	_, err := w.Write(header)
	return err
}
