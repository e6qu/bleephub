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

// This file decides what a fetch answers with — which commits, which of their
// objects, and how they are packed — for both protocol versions. Everything a
// request can say about the shape of the answer (a depth, an existing shallow
// boundary, an object filter, a thin pack, extra tags) is applied here once.

// gitFetchBoundary is the slice of the commit graph one upload-pack answers
// with, together with the shallow boundary the client must record for it.
type gitFetchBoundary struct {
	// roots are the wanted commits the walk started from, with any annotated
	// tag peeled away. The negotiation reads them to decide when the client's
	// history and this one have met.
	roots []plumbing.Hash
	// order lists the commits to send in breadth-first order from the roots,
	// so the packfile built from it is stable across runs rather than being
	// ordered by Go map iteration.
	order []plumbing.Hash
	// included indexes order.
	included map[plumbing.Hash]bool
	// extras are wanted objects that are not commits — annotated tags, and any
	// tag chain above them — which the pack carries alongside the commits.
	extras []plumbing.Hash
	// shallows are the commits whose parents this response withholds, and that
	// the client must therefore record in .git/shallow.
	shallows []plumbing.Hash
	// unshallows are commits the client currently records as shallow but whose
	// parents this response does supply, so it must forget the boundary there.
	unshallows []plumbing.Hash
}

// gitDepthLimit decides which commits a deepening request keeps. A zero value
// keeps everything, which is the ordinary non-shallow fetch.
type gitDepthLimit struct {
	// maxCommits bounds the number of commits on any path from a starting
	// point; zero means unbounded. Depth 1 keeps only the starting points.
	maxCommits int
	// since keeps only commits committed at or after this instant.
	since time.Time
	// excluded holds the commits reachable from any deepen-not reference.
	excluded map[plumbing.Hash]bool
}

// keep reports whether a commit reached at the given distance from a starting
// point belongs in the response.
func (l gitDepthLimit) keep(commit *object.Commit, depth int) bool {
	if l.maxCommits > 0 && depth > l.maxCommits {
		return false
	}
	if !l.since.IsZero() && commit.Committer.When.Before(l.since) {
		return false
	}
	return !l.excluded[commit.Hash]
}

// gitDepthLimitFor turns the deepen lines of a request into the predicate the
// graph walk applies. git allows a depth, a cutoff date and any number of
// excluded references in the same request, and each one narrows the result
// further.
//
// A deepen-relative walk starts at the boundary the client already records, and
// that boundary commit is itself the first commit of the walk, so its budget is
// one larger than the number of commits the client asked to gain. A
// deepen-relative request from a client with no boundary to count from asks to
// extend a history that is already complete, so it is answered with all of it.
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

// resolveGitDeepenNot resolves the reference named by a deepen-not line.
//
// `git clone --shallow-exclude=v1` sends the short name the user typed, so the
// same expansion rules git uses are applied, and a raw object id is accepted
// too because that is also a legal --shallow-exclude argument.
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
	// git's own wording, so a user who searches the message finds the same
	// answers whichever server produced it.
	return plumbing.ZeroHash, &gitClientRefusal{reason: "git upload-pack: deepen-not is not a ref: deepen-not " + name}
}

// gitReachableCommits collects every commit reachable from a starting object,
// peeling an annotated tag first so a tag reference works as a boundary.
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
// where the request's depth says to stop.
//
// The walk is breadth-first, so the first time a commit is reached is by a
// shortest path from some starting point and its distance is final — which is
// what makes "deepen <n>" mean n commits along every path rather than n along
// whichever path happened to be explored first.
//
// A commit is a shallow boundary when it is included but at least one of its
// parents is not: that is the exact point where the client's history is
// truncated, so a root commit never becomes a boundary and a `--depth` larger
// than the history produces no boundary at all — the same answer git gives.
//
// With deepen-relative the walk starts at the boundary the client already
// records rather than at the wants, so `git fetch --deepen=<n>` gains n commits
// beyond where the clone currently stops, however deep that is.
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
		// A boundary the client already recorded is not restated, matching
		// git: the client's .git/shallow already says so.
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

// gitReachableClientBoundary picks the commits a deepen-relative walk starts
// from: the shallow boundary the client recorded, restricted to the part of it
// the wanted commits actually reach. A boundary left over from a branch this
// request does not ask for would otherwise pull unrelated history in.
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

// peelGitWants splits the wanted object ids into the commits the graph walk
// starts from and the objects that have to be sent as they are.
//
// A client may want an annotated tag — `git clone` wants every advertised ref —
// in which case the tag object itself belongs in the pack and the walk starts
// at the commit it names, possibly through a chain of tags. A want naming a
// tree or a blob outright, which is how a partial clone fetches an object it
// had filtered out, is carried the same way.
func peelGitWants(stor storer.EncodedObjectStorer, wants []plumbing.Hash) (roots, extras []plumbing.Hash, err error) {
	seen := make(map[plumbing.Hash]bool, len(wants))
	for _, want := range wants {
		hash := want
		for !seen[hash] {
			encoded, err := stor.EncodedObject(plumbing.AnyObject, hash)
			if errors.Is(err, plumbing.ErrObjectNotFound) {
				// git's own wording for a want this repository cannot serve,
				// so the client prints the reason rather than reporting a
				// transport failure.
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
	// paths records the tree path each object was reached at. Two versions of
	// the same file share a path, which is what makes one a good delta base
	// for the other.
	paths map[plumbing.Hash]string
	// packed indexes objects.
	packed map[plumbing.Hash]bool
	// clientAt is the object the client already holds at each tree path. Every
	// entry is an object this pack does not carry, so a delta against one is
	// exactly the "thin" part of a thin pack.
	clientAt map[string]plumbing.Hash
}

// gitObjectsToSend enumerates the objects the packfile must carry.
//
// Objects the client already has are subtracted first, and that subtraction is
// where a shallow client needs care: walking its have list to the root would
// mark objects it does not actually hold as already-delivered, because its
// history stops at the boundary it declared. The walk therefore stops at every
// commit the client listed as shallow — it keeps that commit's own tree, which
// the client does have, and leaves the parents beyond it to be sent.
//
// The have list is consulted for every fetch, which is what makes a fetch
// incremental: without it a client that is one commit behind is sent the entire
// history again, every time.
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
			// A want naming a tree is a partial clone reaching for a directory
			// an earlier filter left out, and git's lazy fetch asks for it with
			// blob:none — so the answer owes the subtree below it, not just the
			// one object, or the client spends a round trip per level.
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
// have list backwards through the graph but stopping at stopAt — the commits it
// told us are its shallow boundary, whose parents it does not have.
//
// Alongside the exclusion it records which object the client holds at each tree
// path, keeping the first one reached. The walk starts at the client's tips, so
// that first object is its most recent version of the path and therefore the
// closest delta base for the version being sent.
//
// A have line naming an object this server does not hold is ignored rather than
// failing the fetch: the client may have commits of its own that were never
// pushed here.
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
	// match is what the sparse specification said about this directory, and it
	// is what a path beneath it inherits when no pattern names that path.
	match   gitPatternMatch
	entries []object.TreeEntry
	next    int
	// omitted records that some object below this directory was left out at
	// the path it was reached by, which is what stops the walk from treating
	// the directory as settled for every other path it appears at.
	omitted bool
}

// gitTreeWalk carries the state one collectGitTree walk shares.
type gitTreeWalk struct {
	stor   storer.EncodedObjectStorer
	filter gitObjectFilter
	wanted map[plumbing.Hash]bool
	seen   map[plumbing.Hash]bool
	emit   func(plumbing.Hash, string)
	// visited and complete are only built for a filter that decides objects by
	// path. visited is the set of (tree, path) pairs already walked, and
	// complete holds the trees whose whole subtree was included, which may
	// therefore be skipped wherever else they appear.
	visited  map[string]bool
	complete map[plumbing.Hash]bool
}

// collectGitTree records a tree and everything beneath it, skipping any subtree
// already in seen so a history that barely changes costs one walk, not one per
// commit. Submodule entries name commits in another repository and are not
// objects this one can send.
//
// The filter decides which of those objects the answer actually carries; an
// object the client named in a want line is carried whatever the filter says,
// because that request is the client asking for exactly the object an earlier
// filtered fetch left out. A tree the filter drops is not descended into, so
// nothing below it can be reached either.
//
// The walk is depth-first, which is the order git's own traversal visits paths
// in, and it is iterative, so a deeply nested tree costs heap rather than stack.
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

// enter opens a directory, emitting the tree object unless it has already been
// emitted or the filter drops it, and reports the frame the walk continues in.
// A nil frame means there is nothing below this directory left to do.
func (w *gitTreeWalk) enter(hash plumbing.Hash, path string, depth int, inherited gitPatternMatch) (*gitTreeFrame, error) {
	if w.visited == nil {
		if w.seen[hash] {
			return nil, nil
		}
	} else {
		// The same tree object can sit at several paths — a directory copied
		// with no other change — and a sparse specification may select one of
		// those paths and not another, so a tree is only settled once a visit
		// found nothing to omit below it.
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

// leave closes a directory whose entries are exhausted, recording a subtree
// that was carried whole so no other path through it has to be walked again.
func (w *gitTreeWalk) leave(frame *gitTreeFrame) {
	if w.visited != nil && !frame.omitted {
		w.complete[frame.hash] = true
	}
}

// blob decides one file, emitting it or reporting that it was left out.
//
// A blob the filter drops at this path is not recorded as settled when the
// filter reads paths, because the same content may sit at another path the
// specification does select, and that occurrence must still be sent.
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

// appendGitIncludedTags adds the annotated tag objects that point into the
// pack, which is what the include-tag capability promises: a client fetching a
// branch gets the tags on it without a second round trip.
//
// A chain of tags is added from the bottom up, so a tag of a tag arrives after
// the tag it names.
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

// omitsTree reports whether a tree at the given distance from the root tree is
// left out. The root tree is at depth zero, so "tree:0" empties the pack of
// everything but commits and tags.
//
// A sparse rule never answers here: git's sparse filter shows every tree it
// walks through, whether or not the directory is in the sparse specification,
// because the client needs the whole directory skeleton to know what it is
// missing and to widen the cone later without another traversal.
func (f gitObjectFilter) omitsTree(depth int) bool {
	for _, rule := range f.rules {
		if rule.kind == gitFilterTreeDepth && depth >= rule.treeDepth {
			return true
		}
	}
	return false
}

// pathDependent reports whether the filter decides an object by the path it was
// reached at rather than by the object itself. A path-dependent filter makes
// the same blob answerable two ways, so the traversal that applies it may not
// take the shortcuts an object-only filter allows.
func (f gitObjectFilter) pathDependent() bool {
	for _, rule := range f.rules {
		if rule.kind == gitFilterSparseOID {
			return true
		}
	}
	return false
}

// matchSparse decides a path against every sparse rule the filter carries. A
// path any one of them excludes is excluded, so combining a sparse filter with
// another one narrows the answer rather than widening it, and a path none of
// them names is left undecided for the enclosing directory to answer.
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

// omitsBlob reports whether a blob found at the given depth from the root tree
// is left out. The size is only read when a rule needs it, so "blob:none"
// costs no extra object lookups.
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

// parseGitObjectFilter reads a filter-spec. A spec this server cannot honour is
// refused with the message git uses for the same mistake, so the client prints
// a reason instead of receiving a pack that quietly ignores the filter it
// asked for.
//
// The repository is a parameter because "sparse:oid" names its patterns by a
// blob-ish expression that only this repository can resolve.
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
			// git's own wording, so the user who reads it finds the same
			// answers whichever server produced the refusal.
			return gitObjectFilter{}, &gitClientRefusal{reason: "unable to access sparse blob in '" + name + "'"}
		}
		return gitObjectFilter{rules: []gitFilterRule{{kind: gitFilterSparseOID, patterns: parseGitSparsePatterns(content)}}}, nil
	}
	return gitObjectFilter{}, &gitClientRefusal{reason: "invalid filter-spec '" + spec + "'"}
}

// readGitBlobIsh resolves the blob a "sparse:oid" filter names and returns its
// content.
//
// git resolves that argument as a blob-ish, so both spellings a user can write
// reach the same blob: a raw object id, which is what `git clone
// --filter=sparse:oid=<oid>` sends, and a "<rev>:<path>" expression naming a
// pattern file committed to the repository, which is how a monorepo keeps its
// sparse specifications under review alongside the code they select.
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

// gitMaxSparseFilterBlobSize bounds how much of a named pattern file is read.
// A sparse specification is a list of paths; this is far past any real one and
// keeps a client from making the server hold an arbitrary blob in memory by
// pointing the filter at it.
const gitMaxSparseFilterBlobSize = 8 << 20

// resolveGitBlobIshHash resolves a blob-ish expression to the object id of a
// blob, following annotated tags and reading a path out of a tree when the
// expression names one.
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

// resolveGitRevision turns a reference name or an object id into the object it
// names, using the same short-name expansion git's rev-parse applies.
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

// parseGitBlobLimit reads the size a blob:limit filter cuts at, in the plain,
// kibibyte, mebibyte and gibibyte spellings git accepts.
// parseGitBlobLimit reads the size a "blob:limit=" spec names. The limit is an
// int64 because that is what it is compared against — an object's size — so the
// suffix is applied in the same width the comparison happens in and a spec whose
// product would not fit is refused instead of wrapping to a negative limit that
// would omit every blob.
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

// gitMaxDeltaObjectSize bounds the objects a thin pack will try to deltify.
// Computing a delta loads both the base and the target into memory, so past
// this size the object is written whole and the pack pays bytes rather than the
// server paying heap.
const gitMaxDeltaObjectSize = 16 << 20

// gitPackVersion is the packfile format version every git client understands.
const gitPackVersion = 2

// writeGitPackfile writes the packfile a finished plan describes.
//
// A client that asked for a thin pack and proved it holds objects this pack can
// delta against gets the thin encoder. Otherwise the answer is built from the
// packfiles storage already holds when they cover it — see git_packreuse.go for
// what makes that a correct answer — and from go-git's encoder when they do
// not, which searches a delta window across the objects in the pack and emits
// offset deltas.
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

// writeGitThinPack writes a packfile whose entries may be deltas against
// objects that are not in it.
//
// The base of such a delta is the object the client already holds at the same
// tree path — the previous version of the file or directory being sent — which
// the client can always resolve because the have lines it sent are what put the
// object in that table in the first place. Nothing in the table is ever also in
// the pack: everything the client holds was subtracted from the object list
// before it was built, so every delta written here is genuinely external and
// the pack is genuinely thin.
//
// go-git's packfile.Encoder cannot express this. Its only entry point takes the
// list of object ids to pack and runs its own delta selector over exactly that
// list, and writeBaseIfDelta puts any base it chose into the pack; the
// encode([]*ObjectToPack) form that could accept an externally based delta is
// unexported, as is any way to reach ObjectToPack.SetDelta from outside the
// package. So the entries are written here.
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

// gitThinDelta computes the delta that turns the client's version of an object
// into the one being sent, and reports it only when it is genuinely smaller
// than the object itself.
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

// gitPackMaxEntries is the largest object count a packfile can state. The count
// is a four-byte field, so a pack holding more objects than this has no header
// that describes it.
const gitPackMaxEntries = 1<<32 - 1

// writeGitPackHeader writes the twelve bytes that open a packfile: the
// signature, the format version and the number of entries that follow.
//
// A count the field cannot hold is an error rather than a truncation, because a
// wrapped count produces a pack whose header disagrees with its body, and the
// client would index it as a shorter pack and silently lose objects.
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

// writeGitPackObject writes one whole object: its type-and-size header followed
// by the deflated content.
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

// writeGitPackEntryHeader writes a packfile entry header: the object type in
// three bits and the uncompressed size in a little-endian base-128 run, four
// bits of it in the first byte and seven in each byte after.
//
// The run ends when the size has been shifted down to zero, which a negative
// size never reaches, so a negative size is refused before the loop rather than
// spinning in it.
func writeGitPackEntryHeader(w io.Writer, objectType plumbing.ObjectType, size int64) error {
	if size < 0 {
		return fmt.Errorf("pack entry header cannot state a size of %d", size)
	}
	header := make([]byte, 0, 10)
	// Each byte of the run carries seven bits of size, and the first carries the
	// type above the four bits of size it has room for; the masks are what make
	// each one a byte.
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
