package bleephub

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/plumbing/format/packfile"
	"github.com/go-git/go-git/v5/plumbing/format/pktline"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/plumbing/protocol/packp"
	"github.com/go-git/go-git/v5/plumbing/protocol/packp/capability"
	"github.com/go-git/go-git/v5/plumbing/storer"
)

// This file is the whole server side of git-upload-pack: the ref
// advertisement, the want/have/deepen negotiation and the packfile it answers
// with. Both transports — smart HTTP (git_http.go) and SSH (git_ssh.go) — call
// serveGitUploadPack, so a client cannot observe a different protocol
// depending on which URL scheme it dialed.
//
// go-git ships a server transport (plumbing/transport/server) and bleephub used
// to drive it, but that implementation refuses every shallow request outright
// ("shallow not supported") and advertises no shallow capabilities, so
// `git clone --depth`, `--shallow-since` and `--shallow-exclude` all failed at
// the advertisement. Everything below is built from go-git *plumbing* — packp
// for the wire messages, object/storer for the graph, packfile for the pack —
// so it keeps running against whatever storer.Storer the repository resolves
// to, including the S3-backed one. Nothing here touches a filesystem or shells
// out to git.

// gitPackWindow is the delta window the packfile encoder searches, matching the
// value go-git's own server transport used, so packs stay the size they were.
const gitPackWindow = 10

// gitHaveLineLength is the payload length of a "have <oid>" pkt-line: the
// keyword, a space and a 40-character object id.
const gitHaveLineLength = len("have ") + 40

// maxGitNegotiationHaves bounds how many objects one negotiation may claim to
// hold. git derives its have lines from local references and stops well short
// of this; the ceiling exists so a client that simply never stops talking
// cannot grow the server's slice without limit, which matters most on SSH,
// where there is no request-body cap to fall back on.
const maxGitNegotiationHaves = 1 << 20

var (
	gitHavePrefix = []byte("have ")
	gitDoneLine   = []byte("done")
)

// setGitUploadPackCapabilities declares exactly what this upload-pack honours.
//
// The list is deliberately short. A capability in the advertisement is a
// promise: a client that sees "side-band-64k" will demand a multiplexed stream
// and hang on a raw one, so anything not implemented here is simply absent and
// the client falls back to the plain behaviour.
//
//   - agent, ofs-delta: unchanged from before, and the packfile encoder really
//     does emit offset deltas.
//   - shallow, deepen-since, deepen-not: the point of this file. They enable
//     "deepen <n>", "deepen-since <ts>" and "deepen-not <ref>" respectively,
//     and the client-sent "shallow <oid>" lines that carry an existing
//     boundary back to us.
//
// Deliberately omitted, with the consequence each omission has:
//   - side-band/side-band-64k: no progress channel, so a mid-pack failure
//     cannot be reported to the client. Unchanged from before.
//   - multi_ack/multi_ack_detailed: negotiation always ends in NAK (see
//     negotiateGitHaves); the client sends its haves and gives up looking for
//     a common base, which costs bandwidth but is protocol-legal.
//   - thin-pack, include-tag: this server never emits a thin pack, and never
//     volunteers tag objects the client did not ask for.
//   - no-progress: meaningless without a progress channel.
//   - deepen-relative: `git fetch --deepen=<n>` is refused client-side rather
//     than silently answered as an absolute depth.
//   - filter, allow-*-sha1-in-want, no-done: not implemented.
func setGitUploadPackCapabilities(caps *capability.List) error {
	if err := caps.Set(capability.Agent, capability.DefaultAgent()); err != nil {
		return err
	}
	if err := caps.Set(capability.OFSDelta); err != nil {
		return err
	}
	if err := caps.Set(capability.Shallow); err != nil {
		return err
	}
	if err := caps.Set(capability.DeepenSince); err != nil {
		return err
	}
	return caps.Set(capability.DeepenNot)
}

// gitUploadPackAdvertisement builds the ref advertisement for git-upload-pack.
//
// It replaces go-git's upSession.AdvertisedReferencesContext, which cannot
// advertise the shallow capabilities. The reference and HEAD handling is the
// same shape so a repository advertises the same refs it always did; callers
// still layer advertiseDefaultBranchSymref on top.
func gitUploadPackAdvertisement(stor storer.Storer) (*packp.AdvRefs, error) {
	advertisement := packp.NewAdvRefs()
	if err := setGitUploadPackCapabilities(advertisement.Capabilities); err != nil {
		return nil, err
	}
	if err := setGitAdvertisedReferences(stor, advertisement); err != nil {
		return nil, err
	}
	if err := setGitAdvertisedHead(stor, advertisement); err != nil {
		return nil, err
	}
	return advertisement, nil
}

// setGitAdvertisedReferences copies every hash reference into the
// advertisement. Symbolic references other than HEAD are skipped, as a
// reference advertisement carries object ids.
func setGitAdvertisedReferences(stor storer.Storer, advertisement *packp.AdvRefs) error {
	iter, err := stor.IterReferences()
	if err != nil {
		return err
	}
	return iter.ForEach(func(ref *plumbing.Reference) error {
		if ref.Type() != plumbing.HashReference {
			return nil
		}
		advertisement.References[ref.Name().String()] = ref.Hash()
		return nil
	})
}

// setGitAdvertisedHead resolves HEAD onto the advertisement. A repository whose
// HEAD points at a branch with no commits yet advertises no HEAD at all rather
// than failing, which is how an empty repository is advertised.
func setGitAdvertisedHead(stor storer.Storer, advertisement *packp.AdvRefs) error {
	ref, err := stor.Reference(plumbing.HEAD)
	if errors.Is(err, plumbing.ErrReferenceNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if ref.Type() == plumbing.SymbolicReference {
		if err := advertisement.AddReference(ref); err != nil {
			return nil
		}
		ref, err = storer.ResolveReference(stor, ref.Target())
		if errors.Is(err, plumbing.ErrReferenceNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
	}
	if ref.Type() != plumbing.HashReference {
		return plumbing.ErrInvalidType
	}
	hash := ref.Hash()
	advertisement.Head = &hash
	return nil
}

// gitUploadPackResult reports what a finished negotiation did. The wire
// response is already written by the time it is returned; the fields exist so
// a transport can do its own bookkeeping.
type gitUploadPackResult struct {
	// packed is false when the client never said "done", so no packfile was
	// sent. A stateless deepening fetch does exactly this on its first POST:
	// it asks only for the shallow boundary and then reconnects.
	packed bool
	// clone is true when the client asked for objects without offering a
	// single have line — a fresh clone rather than an incremental fetch.
	clone bool
	// responded is true once any byte of the reply has been written. Until it
	// is, a transport that has a status code — smart HTTP — can still refuse
	// the request with one; afterwards the only channel left is the stream
	// itself.
	responded bool
}

// gitResponseWriter notes whether anything has reached the client yet.
type gitResponseWriter struct {
	out   io.Writer
	wrote bool
}

func (w *gitResponseWriter) Write(p []byte) (int, error) {
	if len(p) > 0 {
		w.wrote = true
	}
	return w.out.Write(p)
}

// serveGitUploadPack runs one complete upload-pack negotiation.
//
// in must be positioned at the start of the upload-request (the caller has
// already dealt with the flush-only request an empty repository draws), and it
// must be buffered because the pkt-line decoders hand the stream between each
// other without look-ahead. out receives the reply.
//
// The same function serves both transports because the two differ only in
// where the stream ends, and end-of-stream is already the protocol's own
// terminator. Over SSH the client keeps the channel open and finishes with
// "done"; over smart HTTP each POST body simply ends, and the loop below
// treats that end exactly as git's upload-pack does — it stops, having sent
// whatever the request had earned so far. That is what makes the first POST of
// a stateless deepening fetch (want/deepen/flush and nothing more) answer with
// the shallow update alone, with no NAK and no pack, which is what real git
// waits for.
func serveGitUploadPack(ctx context.Context, stor storer.Storer, in *bufio.Reader, sink io.Writer) (result gitUploadPackResult, err error) {
	// Named results so the deferred flag lands on what the caller receives: a
	// transport decides between a status code and a stream-level refusal on it.
	out := &gitResponseWriter{out: sink}
	defer func() { result.responded = out.wrote }()

	request := packp.NewUploadPackRequest()
	if err := request.Decode(in); err != nil {
		return result, err
	}
	// packp's own UploadRequest.Validate is not usable here: it insists the
	// request carry the "shallow" capability whenever it carries a "deepen"
	// line, and git never echoes that capability back — it only ever appears in
	// the server's advertisement. The one rule that does apply on the server
	// side is that a request must want something.
	if len(request.Wants) == 0 {
		return result, errors.New("upload-pack request has no wants")
	}
	if err := checkGitUploadPackCapabilities(request.Capabilities); err != nil {
		return result, err
	}

	boundary, boundaryErr := gitFetchBoundaryFor(stor, request)
	if boundaryErr != nil {
		var refusal *gitClientRefusal
		if errors.As(boundaryErr, &refusal) {
			return result, writeGitProtocolError(out, refusal.reason)
		}
		return result, boundaryErr
	}
	if !request.Depth.IsZero() && len(boundary.order) == 0 {
		// A boundary predicate that keeps nothing — `--shallow-exclude` naming
		// a reference that already contains every wanted commit, or a
		// `--shallow-since` newer than the whole history. git refuses this
		// rather than answering with a pack that omits the wants, and says so
		// in an ERR line so the client can print the reason instead of
		// reporting a truncated stream.
		return result, writeGitProtocolError(out, "no commits selected for shallow requests")
	}

	// The shallow update goes out before the have negotiation, and only when a
	// depth was requested: that ordering is what lets the client record its new
	// boundary before it reads a pack whose oldest commits have parents the
	// pack does not carry. A request that merely restates an existing boundary
	// without deepening gets no update at all, matching git.
	if !request.Depth.IsZero() {
		update := packp.ShallowUpdate{Shallows: boundary.shallows, Unshallows: boundary.unshallows}
		if err := update.Encode(out); err != nil {
			return result, err
		}
	}

	haves, done, negotiateErr := negotiateGitHaves(in, out)
	if negotiateErr != nil {
		return result, negotiateErr
	}
	if !done {
		return result, nil
	}
	result.packed = true
	result.clone = len(haves) == 0

	if ctxErr := ctx.Err(); ctxErr != nil {
		return result, ctxErr
	}

	// The have list is honoured only for a shallow-aware negotiation. An
	// ordinary clone or fetch keeps answering with everything reachable from
	// the wants, exactly as it did before this file existed, so the common path
	// stays byte-for-byte what it was.
	shallowAware := !request.Depth.IsZero() || len(request.Shallows) > 0
	objects, objectsErr := gitObjectsToSend(stor, boundary, haves, request.Shallows, shallowAware)
	if objectsErr != nil {
		return result, objectsErr
	}
	encoder := packfile.NewEncoder(out, stor, false)
	if _, encodeErr := encoder.Encode(objects, gitPackWindow); encodeErr != nil {
		return result, encodeErr
	}
	return result, nil
}

// checkGitUploadPackCapabilities refuses a request asking for behaviour that
// was never advertised. A client only sends capabilities it saw in the
// advertisement, so anything else means the two sides disagree about the
// protocol and continuing would produce a stream the client cannot parse.
func checkGitUploadPackCapabilities(requested *capability.List) error {
	advertised := capability.NewList()
	if err := setGitUploadPackCapabilities(advertised); err != nil {
		return err
	}
	for _, requestedCapability := range requested.All() {
		if !advertised.Supports(requestedCapability) {
			return fmt.Errorf("unsupported capability: %s", requestedCapability)
		}
	}
	return nil
}

// negotiateGitHaves reads the have/done half of the request and answers it.
//
// This server implements neither multi_ack nor multi_ack_detailed and never
// advertises them, so it never acknowledges an individual object: every
// flush-pkt and the final "done" are answered with NAK, which is the reply a
// client expects when no common base was agreed. The have lines are still
// collected, because they are what lets a deepening fetch omit objects the
// client already holds.
//
// done reports whether the client actually asked for a packfile. A stream that
// simply ends — a stateless client that only wanted the shallow boundary — is
// not an error.
func negotiateGitHaves(in io.Reader, out io.Writer) (haves []plumbing.Hash, done bool, err error) {
	scanner := pktline.NewScanner(in)
	for scanner.Scan() {
		line := bytes.TrimSuffix(scanner.Bytes(), []byte("\n"))
		switch {
		case len(line) == 0:
			if err := (&packp.ServerResponse{}).Encode(out, false); err != nil {
				return nil, false, err
			}
		case bytes.Equal(line, gitDoneLine):
			if err := (&packp.ServerResponse{}).Encode(out, false); err != nil {
				return nil, false, err
			}
			return haves, true, nil
		case bytes.HasPrefix(line, gitHavePrefix):
			if len(line) != gitHaveLineLength {
				return nil, false, fmt.Errorf("malformed have line %q", line)
			}
			if len(haves) >= maxGitNegotiationHaves {
				return nil, false, fmt.Errorf("upload-pack negotiation offered more than %d haves", maxGitNegotiationHaves)
			}
			haves = append(haves, plumbing.NewHash(string(line[len(gitHavePrefix):])))
		default:
			return nil, false, fmt.Errorf("unexpected upload-pack line %q", line)
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, false, err
	}
	return haves, false, nil
}

// gitClientRefusal is a reason the request itself carries — a reference that
// does not exist, a boundary that selects nothing — as opposed to a storage or
// transport failure. git reports these to the user, so they travel in an ERR
// pkt-line rather than dying as an opaque stream error.
type gitClientRefusal struct{ reason string }

func (e *gitClientRefusal) Error() string { return e.reason }

// writeGitProtocolError sends a reason the client can print, in the "ERR
// <message>" pkt-line both git and go-git recognise, and returns it as an error
// so the transport still logs the refusal. It is only safe before any other
// response line has been written.
func writeGitProtocolError(out io.Writer, message string) error {
	if err := pktline.NewEncoder(out).Encodef("ERR %s\n", message); err != nil {
		return err
	}
	return errors.New(message)
}

// gitFetchBoundary is the slice of the commit graph one upload-pack answers
// with, together with the shallow boundary the client must record for it.
type gitFetchBoundary struct {
	// order lists the commits to send in breadth-first order from the wants,
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
	// maxCommits bounds the number of commits on any path from a want; zero
	// means unbounded. Depth 1 keeps only the wants themselves.
	maxCommits int
	// since keeps only commits committed at or after this instant.
	since time.Time
	// excluded holds the commits reachable from a deepen-not reference.
	excluded map[plumbing.Hash]bool
}

// keep reports whether a commit reached at the given distance from a want
// belongs in the response.
func (l gitDepthLimit) keep(commit *object.Commit, depth int) bool {
	if l.maxCommits > 0 && depth > l.maxCommits {
		return false
	}
	if !l.since.IsZero() && commit.Committer.When.Before(l.since) {
		return false
	}
	return !l.excluded[commit.Hash]
}

// gitDepthLimitFor turns the depth packp parsed off the wire into the predicate
// the graph walk applies.
//
// packp models the three deepen forms as one Depth value, so a request can
// carry only one of them; git allows several deepen-not references at once and
// that combination is refused by packp's decoder before it ever reaches here.
func gitDepthLimitFor(stor storer.Storer, depth packp.Depth) (gitDepthLimit, error) {
	switch value := depth.(type) {
	case packp.DepthCommits:
		return gitDepthLimit{maxCommits: int(value)}, nil
	case packp.DepthSince:
		return gitDepthLimit{since: time.Time(value)}, nil
	case packp.DepthReference:
		hash, err := resolveGitDeepenNot(stor, string(value))
		if err != nil {
			return gitDepthLimit{}, err
		}
		excluded, err := gitReachableCommits(stor, hash)
		if err != nil {
			return gitDepthLimit{}, err
		}
		return gitDepthLimit{excluded: excluded}, nil
	default:
		return gitDepthLimit{}, nil
	}
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
// shortest path from some want and its distance is final — which is what makes
// "deepen <n>" mean n commits along every path rather than n along whichever
// path happened to be explored first.
//
// A commit is a shallow boundary when it is included but at least one of its
// parents is not: that is the exact point where the client's history is
// truncated, so a root commit never becomes a boundary and a `--depth` larger
// than the history produces no boundary at all — the same answer git gives.
func gitFetchBoundaryFor(stor storer.Storer, request *packp.UploadPackRequest) (*gitFetchBoundary, error) {
	limit, err := gitDepthLimitFor(stor, request.Depth)
	if err != nil {
		return nil, err
	}
	roots, extras, err := peelGitWants(stor, request.Wants)
	if err != nil {
		return nil, err
	}

	boundary := &gitFetchBoundary{included: map[plumbing.Hash]bool{}, extras: extras}
	type pendingCommit struct {
		hash  plumbing.Hash
		depth int
	}
	queued := make(map[plumbing.Hash]bool, len(roots))
	queue := make([]pendingCommit, 0, len(roots))
	for _, root := range roots {
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
	clientShallow := make(map[plumbing.Hash]bool, len(request.Shallows))
	for _, hash := range request.Shallows {
		clientShallow[hash] = true
	}
	for _, hash := range boundary.order {
		// A boundary the client already recorded is not restated, matching
		// git: the client's .git/shallow already says so.
		if truncated[hash] && !clientShallow[hash] {
			boundary.shallows = append(boundary.shallows, hash)
		}
	}
	for _, hash := range request.Shallows {
		if boundary.included[hash] && !truncated[hash] {
			boundary.unshallows = append(boundary.unshallows, hash)
		}
	}
	return boundary, nil
}

// peelGitWants splits the wanted object ids into the commits the graph walk
// starts from and the objects that have to be sent as they are.
//
// A client may want an annotated tag — `git clone` wants every advertised ref —
// in which case the tag object itself belongs in the pack and the walk starts
// at the commit it names, possibly through a chain of tags.
func peelGitWants(stor storer.EncodedObjectStorer, wants []plumbing.Hash) (roots, extras []plumbing.Hash, err error) {
	seen := make(map[plumbing.Hash]bool, len(wants))
	for _, want := range wants {
		hash := want
		for !seen[hash] {
			encoded, err := stor.EncodedObject(plumbing.AnyObject, hash)
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

// gitObjectsToSend enumerates the object ids the packfile must carry.
//
// Objects the client already has are subtracted first, and that subtraction is
// where a shallow client needs care: walking its have list to the root would
// mark objects it does not actually hold as already-delivered, because its
// history stops at the boundary it declared. The walk therefore stops at every
// commit the client listed as shallow — it keeps that commit's own tree, which
// the client does have, and leaves the parents beyond it to be sent.
//
// The have list is only consulted when shallowAware is set. An ordinary clone
// or fetch keeps answering with everything reachable from the wants, as it did
// before this file existed, so the common path is untouched.
func gitObjectsToSend(stor storer.EncodedObjectStorer, boundary *gitFetchBoundary, haves, clientShallows []plumbing.Hash, shallowAware bool) ([]plumbing.Hash, error) {
	seen := map[plumbing.Hash]bool{}
	if shallowAware {
		stopAt := make(map[plumbing.Hash]bool, len(clientShallows))
		for _, hash := range clientShallows {
			stopAt[hash] = true
		}
		if err := collectGitHaveObjects(stor, haves, stopAt, seen); err != nil {
			return nil, err
		}
	}

	send := make([]plumbing.Hash, 0, len(boundary.order)+len(boundary.extras))
	emit := func(hash plumbing.Hash) { send = append(send, hash) }
	for _, hash := range boundary.extras {
		if seen[hash] {
			continue
		}
		seen[hash] = true
		emit(hash)
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
		emit(hash)
		if err := collectGitTree(stor, commit.TreeHash, seen, emit); err != nil {
			return nil, err
		}
	}
	return send, nil
}

// collectGitHaveObjects marks everything the client already holds, walking its
// have list backwards through the graph but stopping at stopAt — the commits it
// told us are its shallow boundary, whose parents it does not have.
//
// A have line naming an object this server does not hold is ignored rather than
// failing the fetch: the client may have commits of its own that were never
// pushed here.
func collectGitHaveObjects(stor storer.EncodedObjectStorer, haves []plumbing.Hash, stopAt, seen map[plumbing.Hash]bool) error {
	visited := make(map[plumbing.Hash]bool, len(haves))
	queue := append([]plumbing.Hash(nil), haves...)
	discard := func(plumbing.Hash) {}
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
		if err := collectGitTree(stor, commit.TreeHash, seen, discard); err != nil {
			return err
		}
		if stopAt[hash] {
			continue
		}
		queue = append(queue, commit.ParentHashes...)
	}
	return nil
}

// collectGitTree records a tree and everything beneath it, skipping any subtree
// already in seen so a history that barely changes costs one walk, not one per
// commit. Submodule entries name commits in another repository and are not
// objects this one can send.
func collectGitTree(stor storer.EncodedObjectStorer, root plumbing.Hash, seen map[plumbing.Hash]bool, emit func(plumbing.Hash)) error {
	if seen[root] {
		return nil
	}
	tree, err := object.GetTree(stor, root)
	if err != nil {
		return err
	}
	seen[root] = true
	emit(root)
	walker := object.NewTreeWalker(tree, true, seen)
	defer walker.Close()
	for {
		_, entry, err := walker.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		if entry.Mode == filemode.Submodule || seen[entry.Hash] {
			continue
		}
		seen[entry.Hash] = true
		emit(entry.Hash)
	}
}
