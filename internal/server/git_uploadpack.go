package bleephub

import (
	"bufio"
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/format/pktline"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/plumbing/protocol/packp"
	"github.com/go-git/go-git/v5/plumbing/protocol/packp/capability"
	"github.com/go-git/go-git/v5/plumbing/protocol/packp/sideband"
	"github.com/go-git/go-git/v5/plumbing/storer"
)

// This file is the whole server side of git-upload-pack: the ref
// advertisement, the want/have/deepen negotiation and the packfile it answers
// with. Both transports — smart HTTP (git_http.go) and SSH (git_ssh.go) — call
// serveGitUploadPack for protocol v0 and serveGitProtocolV2 for protocol v2,
// so a client cannot observe a different protocol depending on which URL scheme
// it dialed.
//
// go-git ships a server transport (plumbing/transport/server) and bleephub used
// to drive it, but that implementation refuses every shallow request outright
// ("shallow not supported") and advertises no shallow capabilities. Everything
// below is built from go-git *plumbing* — packp for the wire messages,
// object/storer for the graph, packfile for the delta encoding — so it keeps
// running against whatever storer.Storer the repository resolves to, including
// the S3-backed one. Nothing here touches a filesystem or shells out to git.

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

// setGitUploadPackCapabilities declares what this upload-pack honours, and
// every entry is backed by working code in this package:
//
//   - agent, object-format: identity and the hash algorithm every object id on
//     the wire is written in.
//   - multi_ack, multi_ack_detailed, no-done: real negotiation. The have loop
//     answers with "ACK <oid> common", "ACK <oid> ready" and a closing
//     "ACK <oid>" so a client can stop walking its history early, and no-done
//     lets a stateless client take the pack in the same round it became ready.
//   - side-band, side-band-64k, no-progress: the multiplexed reply, with pack
//     data on band 1, progress on band 2 (suppressed by no-progress) and a
//     fatal error on band 3 instead of a truncated stream.
//   - thin-pack: deltas against objects the client proved it holds.
//   - ofs-delta: the packfile encoder emits offset deltas.
//   - shallow, deepen-since, deepen-not, deepen-relative: "deepen <n>",
//     "deepen-since <ts>", "deepen-not <ref>" and depth counted from the
//     client's existing boundary, plus the "shallow <oid>" lines that carry
//     that boundary back to us.
//   - include-tag: annotated tags whose target object is in the pack.
//   - filter: partial clone, with the omitted objects genuinely absent.
//   - allow-tip-sha1-in-want, allow-reachable-sha1-in-want: a want naming any
//     object this repository holds is served, which is what makes the lazy
//     fetch of a filtered-out object work.
func setGitUploadPackCapabilities(caps *capability.List) error {
	for _, entry := range []struct {
		name   capability.Capability
		values []string
	}{
		{name: capability.MultiACK},
		{name: capability.MultiACKDetailed},
		{name: capability.NoDone},
		{name: capability.ThinPack},
		{name: capability.Sideband},
		{name: capability.Sideband64k},
		{name: capability.OFSDelta},
		{name: capability.Shallow},
		{name: capability.DeepenSince},
		{name: capability.DeepenNot},
		{name: capability.DeepenRelative},
		{name: capability.NoProgress},
		{name: capability.IncludeTag},
		{name: capability.AllowTipSHA1InWant},
		{name: capability.AllowReachableSHA1InWant},
		{name: capability.Filter},
		{name: capability.ObjectFormat, values: []string{gitObjectFormat}},
		{name: capability.Agent, values: []string{capability.DefaultAgent()}},
	} {
		if err := caps.Set(entry.name, entry.values...); err != nil {
			return err
		}
	}
	return nil
}

// gitObjectFormat is the hash algorithm every object id in this server's
// packfiles and advertisements is written in.
const gitObjectFormat = "sha1"

// gitUploadPackAdvertisement builds the protocol v0 ref advertisement for
// git-upload-pack. Callers layer advertiseDefaultBranchSymref on top.
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
// than failing, which is how an empty repository is advertised in protocol v0;
// protocol v2 names that branch explicitly through ls-refs "unborn".
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
	// packed is false when the client never asked for a packfile. A stateless
	// deepening fetch does exactly this on its first POST: it asks only for the
	// shallow boundary and then reconnects.
	packed bool
	// clone is true when the client asked for objects without offering a
	// single have line — a fresh clone rather than an incremental fetch.
	clone bool
	// responded is true once any byte of the reply has been written. Until it
	// is, a transport that has a status code — smart HTTP — can still refuse
	// the request with one; afterwards the only channel left is the stream
	// itself.
	responded bool
	// sessionID is the identifier a protocol v2 client gave for its side of
	// the conversation. Recording it against the request is what the
	// capability exists for: it is how a client's trace and this server's log
	// are lined up when a fetch has to be explained after the fact.
	sessionID string
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

// gitAckMode is how a negotiation acknowledges the objects a client offers.
type gitAckMode int

const (
	// gitAckSingle is the behaviour of a client that asked for neither
	// multi_ack flavour: one "ACK <oid>" for the first object found in common
	// and nothing after it.
	gitAckSingle gitAckMode = iota
	// gitAckMulti is multi_ack: "ACK <oid> continue" for every common object.
	gitAckMulti
	// gitAckDetailed is multi_ack_detailed: "ACK <oid> common" for a common
	// object and "ACK <oid> ready" once the wants are all connected to the
	// client's history.
	gitAckDetailed
)

// gitSidebandMode is which multiplexing the client asked for.
type gitSidebandMode int

const (
	gitSidebandNone gitSidebandMode = iota
	gitSideband
	gitSideband64k
)

// gitUploadRequest is one upload-pack request, in the shape both protocol
// versions reduce to. Protocol v0 spells the options as capabilities on the
// first want line and negotiates haves in a separate phase; protocol v2 spells
// them as arguments to the "fetch" command and carries the haves inline. Once
// decoded the two are indistinguishable, which is what lets the boundary,
// filter, object-selection and packing code below serve both.
type gitUploadRequest struct {
	capabilities *capability.List
	wants        []plumbing.Hash
	shallows     []plumbing.Hash

	depth          int
	since          time.Time
	deepenNot      []string
	deepenRelative bool
	filter         gitObjectFilter
	// filtered records that a filter line was already read, so a second one is
	// refused rather than silently replacing the first.
	filtered bool

	// haves and done are carried in the request itself by protocol v2. In
	// protocol v0 they arrive in the negotiation phase that follows.
	haves []plumbing.Hash
	done  bool

	sideband   gitSidebandMode
	noProgress bool
	includeTag bool
	thinPack   bool
	ackMode    gitAckMode
	noDone     bool

	// wantRefs are the references a protocol v2 client asked for by name, in
	// the order it asked, together with the object each resolved to. They are
	// answered in the wanted-refs section so the client learns the object ids
	// it never saw an advertisement for.
	wantRefs []gitWantedRef
	// waitForDone suppresses the "ready" that would end a negotiation early:
	// the client has said it will send "done" itself, which is what lets it
	// negotiate without ever taking a pack.
	waitForDone bool
	// sidebandAll multiplexes the whole reply rather than only the packfile
	// section, so an error in any section reaches the client as a message.
	sidebandAll bool
	// packURIProtocols are the transfer protocols the client said it can fetch
	// a packfile over itself. A request that named none never sees a
	// packfile-uris section, which is what makes the offload opt-in.
	packURIProtocols []string
	// serverOptions are the "-o" options the client passed through. The
	// protocol requires a server to accept options it does not act on.
	serverOptions []string
	// sessionID is the identifier the client offered for its side of the
	// conversation.
	sessionID string
}

// gitWantedRef is one reference a client named in a want-ref line, resolved.
type gitWantedRef struct {
	name string
	hash plumbing.Hash
}

// deepens reports whether the request asks for a truncated history, which is
// what decides whether a shallow update is part of the answer.
func (r *gitUploadRequest) deepens() bool {
	return r.depth > 0 || !r.since.IsZero() || len(r.deepenNot) > 0
}

// wantedSet indexes the object ids the client named outright. Those objects are
// sent whatever a filter says about their kind, which is what makes the lazy
// fetch of an object an earlier partial clone omitted work.
func (r *gitUploadRequest) wantedSet() map[plumbing.Hash]bool {
	wanted := make(map[plumbing.Hash]bool, len(r.wants))
	for _, hash := range r.wants {
		wanted[hash] = true
	}
	return wanted
}

// decodeGitUploadRequest reads a protocol v0 upload-request: the want list, the
// client's existing shallow boundary, the deepen form it asks for and the
// object filter, ending at the flush-pkt.
//
// packp has a decoder for this message but it models the three deepen forms as
// one value — so a request cannot carry both a depth and an exclusion — and it
// rejects the filter line outright, which is the whole of partial clone. The
// decoder here reads the grammar git actually sends.
func decodeGitUploadRequest(stor storer.Storer, pkt *gitPktReader) (*gitUploadRequest, error) {
	request := &gitUploadRequest{capabilities: capability.NewList()}
	first := true
	for {
		line, kind, err := pkt.next()
		if err != nil {
			return nil, err
		}
		if kind == gitPktFlush {
			return request, applyGitUploadCapabilities(request)
		}
		if kind != gitPktData {
			return nil, errors.New("unexpected control pkt-line in upload-request")
		}
		payload := line
		if first {
			first = false
			var capabilities []byte
			payload, capabilities = splitGitWantCapabilities(payload)
			if len(capabilities) > 0 {
				if err := request.capabilities.Decode(capabilities); err != nil {
					return nil, fmt.Errorf("invalid capabilities: %w", err)
				}
			}
		}
		if err := applyGitUploadRequestLine(stor, request, string(payload)); err != nil {
			return nil, err
		}
	}
}

// splitGitWantCapabilities separates the first want line of an upload-request
// from the capability list it carries.
//
// git writes a NUL byte between the object id and the capabilities; go-git
// writes a space. Both spellings are read, so either client is served the
// capabilities it actually asked for rather than being quietly downgraded to
// the plain protocol.
func splitGitWantCapabilities(line []byte) (want, capabilities []byte) {
	if nul := bytes.IndexByte(line, 0); nul >= 0 {
		return line[:nul], line[nul+1:]
	}
	const idEnd = len("want ") + 40
	if len(line) > idEnd && line[idEnd] == ' ' {
		return line[:idEnd], line[idEnd+1:]
	}
	return line, nil
}

// applyGitUploadRequestLine records one line of an upload-request or of a
// protocol v2 "fetch" argument list. The two grammars share every keyword,
// which is why one function reads both.
func applyGitUploadRequestLine(stor storer.Storer, request *gitUploadRequest, line string) error {
	keyword, argument, _ := strings.Cut(line, " ")
	switch keyword {
	case "want":
		hash, err := parseGitObjectID(argument)
		if err != nil {
			return err
		}
		request.wants = append(request.wants, hash)
	case "have":
		hash, err := parseGitObjectID(argument)
		if err != nil {
			return err
		}
		if len(request.haves) >= maxGitNegotiationHaves {
			return fmt.Errorf("upload-pack request offered more than %d haves", maxGitNegotiationHaves)
		}
		request.haves = append(request.haves, hash)
	case "shallow":
		hash, err := parseGitObjectID(argument)
		if err != nil {
			return err
		}
		request.shallows = append(request.shallows, hash)
	case "deepen":
		depth, err := strconv.Atoi(argument)
		if err != nil || depth < 0 {
			return fmt.Errorf("invalid deepen %q", argument)
		}
		request.depth = depth
	case "deepen-since":
		seconds, err := strconv.ParseInt(argument, 10, 64)
		if err != nil {
			return fmt.Errorf("invalid deepen-since %q", argument)
		}
		request.since = time.Unix(seconds, 0).UTC()
	case "deepen-not":
		request.deepenNot = append(request.deepenNot, argument)
	case "deepen-relative":
		request.deepenRelative = true
	case "filter":
		if request.filtered {
			return &gitClientRefusal{reason: "multiple filter-specs cannot be combined"}
		}
		filter, err := parseGitObjectFilter(stor, argument)
		if err != nil {
			return err
		}
		request.filter = filter
		request.filtered = true
	case "done":
		request.done = true
	case "thin-pack":
		request.thinPack = true
	case "no-progress":
		request.noProgress = true
	case "include-tag":
		request.includeTag = true
	case "ofs-delta":
		// The packfile encoder emits offset deltas whenever it finds one worth
		// emitting, so this argument needs no state of its own.
	default:
		return fmt.Errorf("unexpected upload-pack line %q", line)
	}
	return nil
}

// parseGitObjectID reads the 40-character object id every want, have and
// shallow line carries, rejecting anything that is not exactly that rather than
// silently turning it into a hash of zeroes.
func parseGitObjectID(text string) (plumbing.Hash, error) {
	var hash plumbing.Hash
	if len(text) != hex.EncodedLen(len(hash)) {
		return plumbing.ZeroHash, fmt.Errorf("malformed object id %q", text)
	}
	if _, err := hex.Decode(hash[:], []byte(text)); err != nil {
		return plumbing.ZeroHash, fmt.Errorf("malformed object id %q", text)
	}
	return hash, nil
}

// applyGitUploadCapabilities turns the capability list a protocol v0 client
// echoed back into the options the rest of the exchange reads.
func applyGitUploadCapabilities(request *gitUploadRequest) error {
	caps := request.capabilities
	switch {
	case caps.Supports(capability.Sideband64k):
		request.sideband = gitSideband64k
	case caps.Supports(capability.Sideband):
		request.sideband = gitSideband
	}
	switch {
	case caps.Supports(capability.MultiACKDetailed):
		request.ackMode = gitAckDetailed
	case caps.Supports(capability.MultiACK):
		request.ackMode = gitAckMulti
	}
	request.noProgress = request.noProgress || caps.Supports(capability.NoProgress)
	request.includeTag = request.includeTag || caps.Supports(capability.IncludeTag)
	request.thinPack = request.thinPack || caps.Supports(capability.ThinPack)
	request.noDone = caps.Supports(capability.NoDone)
	request.deepenRelative = request.deepenRelative || caps.Supports(capability.DeepenRelative)
	if values := caps.Get(capability.ObjectFormat); len(values) > 0 && values[0] != gitObjectFormat {
		return fmt.Errorf("unsupported object-format: %s", values[0])
	}
	return nil
}

// serveGitUploadPack runs one complete protocol v0 upload-pack negotiation.
//
// in must be positioned at the start of the upload-request (the caller has
// already dealt with the flush-only request an empty repository draws), and it
// must be buffered because the pkt-line reader hands the stream on to the
// packfile writer without look-ahead. out receives the reply.
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

	pkt := newGitPktReader(in)
	request, err := decodeGitUploadRequest(stor, pkt)
	if err != nil {
		var refusal *gitClientRefusal
		if errors.As(err, &refusal) {
			return result, writeGitProtocolError(out, refusal.reason)
		}
		return result, err
	}
	// A request must want something; git's own upload-pack answers a want-less
	// request with nothing at all, and there is no pack to build from it.
	if len(request.wants) == 0 {
		return result, errors.New("upload-pack request has no wants")
	}
	if err := checkGitUploadPackCapabilities(request.capabilities); err != nil {
		return result, err
	}

	boundary, err := gitFetchBoundaryFor(stor, request)
	if err != nil {
		var refusal *gitClientRefusal
		if errors.As(err, &refusal) {
			return result, writeGitProtocolError(out, refusal.reason)
		}
		return result, err
	}
	if request.deepens() && len(boundary.order) == 0 {
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
	if request.deepens() {
		update := packp.ShallowUpdate{Shallows: boundary.shallows, Unshallows: boundary.unshallows}
		if err := update.Encode(out); err != nil {
			return result, err
		}
	}

	negotiation, err := negotiateGitHaves(stor, pkt, out, request, boundary)
	if err != nil {
		return result, err
	}
	if !negotiation.done {
		return result, nil
	}
	result.packed = true
	result.clone = len(negotiation.haves) == 0

	if ctxErr := ctx.Err(); ctxErr != nil {
		return result, ctxErr
	}
	request.haves = negotiation.haves
	return result, sendGitPackfile(stor, out, request, boundary, request.sideband)
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

// gitNegotiation is what one protocol v0 have/done exchange established.
type gitNegotiation struct {
	// haves is every object the client offered, whether or not this server
	// holds it: they are what lets an incremental fetch omit objects the client
	// already has.
	haves []plumbing.Hash
	// common is the subset this server also holds, in the order they were
	// acknowledged.
	common []plumbing.Hash
	// done reports whether the client actually asked for a packfile.
	done bool
}

// negotiateGitHaves reads the have/done half of a protocol v0 request and
// answers it.
//
// Each have line the server also holds is acknowledged immediately, in the
// spelling the client's multi_ack flavour asked for, so the client can stop
// walking that branch of its history instead of sending every commit it owns.
// Once every want is connected to something the client holds, an
// "ACK <oid> ready" says so; a client that also sent no-done then gets the
// closing ACK and the packfile in the same round rather than spending another
// request on "done".
//
// A stream that simply ends — a stateless client that only wanted the shallow
// boundary, or that is still negotiating — leaves done false and is not an
// error.
func negotiateGitHaves(stor storer.EncodedObjectStorer, pkt *gitPktReader, out io.Writer, request *gitUploadRequest, boundary *gitFetchBoundary) (gitNegotiation, error) {
	encoder := pktline.NewEncoder(out)
	var negotiation gitNegotiation
	common := map[plumbing.Hash]bool{}
	connected := map[plumbing.Hash]bool{}
	var last plumbing.Hash
	sentReady := false

	giveUp := func() (bool, error) {
		if len(common) == 0 {
			return false, nil
		}
		return gitWantsReachCommon(stor, boundary.roots, common, connected)
	}

	for {
		line, kind, err := pkt.next()
		if errors.Is(err, io.EOF) {
			return negotiation, nil
		}
		if err != nil {
			return negotiation, err
		}
		switch {
		case kind == gitPktFlush:
			if request.ackMode == gitAckDetailed && !sentReady {
				ready, err := giveUp()
				if err != nil {
					return negotiation, err
				}
				if ready {
					sentReady = true
					if err := encoder.Encodef("ACK %s ready\n", last.String()); err != nil {
						return negotiation, err
					}
				}
			}
			if len(negotiation.common) == 0 || request.ackMode != gitAckSingle {
				if err := encoder.Encodef("NAK\n"); err != nil {
					return negotiation, err
				}
			}
			if request.noDone && sentReady {
				if err := encoder.Encodef("ACK %s\n", last.String()); err != nil {
					return negotiation, err
				}
				negotiation.done = true
				return negotiation, nil
			}
		case kind != gitPktData:
			return negotiation, errors.New("unexpected control pkt-line in upload-pack negotiation")
		case bytes.Equal(line, gitDoneLine):
			switch {
			case len(negotiation.common) == 0:
				if err := encoder.Encodef("NAK\n"); err != nil {
					return negotiation, err
				}
			case request.ackMode != gitAckSingle:
				if err := encoder.Encodef("ACK %s\n", last.String()); err != nil {
					return negotiation, err
				}
			}
			negotiation.done = true
			return negotiation, nil
		case bytes.HasPrefix(line, gitHavePrefix):
			if len(line) != gitHaveLineLength {
				return negotiation, fmt.Errorf("malformed have line %q", line)
			}
			if len(negotiation.haves) >= maxGitNegotiationHaves {
				return negotiation, fmt.Errorf("upload-pack negotiation offered more than %d haves", maxGitNegotiationHaves)
			}
			hash := plumbing.NewHash(string(line[len(gitHavePrefix):]))
			negotiation.haves = append(negotiation.haves, hash)
			if gitStorerHasCommit(stor, hash) {
				common[hash] = true
				negotiation.common = append(negotiation.common, hash)
				last = hash
				if err := ackGitCommonHave(encoder, request.ackMode, hash, len(negotiation.common)); err != nil {
					return negotiation, err
				}
				continue
			}
			// The client offered a commit this server does not hold. That is
			// still the moment git checks whether the wants are already
			// connected to what it does hold, because saying so early is what
			// stops the client walking further down a branch this server can
			// never share.
			if request.ackMode == gitAckSingle || sentReady {
				continue
			}
			ready, err := giveUp()
			if err != nil {
				return negotiation, err
			}
			if !ready {
				continue
			}
			if request.ackMode == gitAckDetailed {
				sentReady = true
				if err := encoder.Encodef("ACK %s ready\n", hash.String()); err != nil {
					return negotiation, err
				}
				continue
			}
			if err := encoder.Encodef("ACK %s continue\n", hash.String()); err != nil {
				return negotiation, err
			}
		default:
			return negotiation, fmt.Errorf("unexpected upload-pack line %q", line)
		}
	}
}

// ackGitCommonHave acknowledges one object found in common, in the spelling the
// negotiation mode calls for. A client that asked for neither multi_ack flavour
// is told about the first common object only; anything after it would be a line
// that client cannot parse.
func ackGitCommonHave(encoder *pktline.Encoder, mode gitAckMode, hash plumbing.Hash, commonSoFar int) error {
	switch mode {
	case gitAckDetailed:
		return encoder.Encodef("ACK %s common\n", hash.String())
	case gitAckMulti:
		return encoder.Encodef("ACK %s continue\n", hash.String())
	default:
		if commonSoFar == 1 {
			return encoder.Encodef("ACK %s\n", hash.String())
		}
		return nil
	}
}

// gitStorerHasCommit reports whether this repository holds the commit an object
// id names. A have line for anything else — an object from another repository,
// or a commit that was garbage collected — is not a common base.
func gitStorerHasCommit(stor storer.EncodedObjectStorer, hash plumbing.Hash) bool {
	_, err := stor.EncodedObject(plumbing.CommitObject, hash)
	return err == nil
}

// gitWantsReachCommon reports whether every wanted commit has an ancestor the
// client already holds. That is the cut git calls "ready": from here the server
// can build a complete pack, so it can tell the client to stop negotiating.
//
// connected memoises the wants already proven to reach the common set. A want
// that did not reach it can still do so once a later round grows that set, so
// only the positive answers are remembered.
func gitWantsReachCommon(stor storer.EncodedObjectStorer, roots []plumbing.Hash, common, connected map[plumbing.Hash]bool) (bool, error) {
	for _, root := range roots {
		if connected[root] {
			continue
		}
		reached, err := gitCommitReachesSet(stor, root, common)
		if err != nil {
			return false, err
		}
		if !reached {
			return false, nil
		}
		connected[root] = true
	}
	return true, nil
}

// gitCommitReachesSet walks a commit's ancestry looking for any member of the
// target set, stopping as soon as one is found.
func gitCommitReachesSet(stor storer.EncodedObjectStorer, from plumbing.Hash, targets map[plumbing.Hash]bool) (bool, error) {
	if targets[from] {
		return true, nil
	}
	visited := map[plumbing.Hash]bool{from: true}
	queue := []plumbing.Hash{from}
	for len(queue) > 0 {
		hash := queue[0]
		queue = queue[1:]
		commit, err := object.GetCommit(stor, hash)
		if err != nil {
			// A want may name a commit whose ancestry this repository only
			// partly holds; the missing edge simply leads nowhere.
			continue
		}
		for _, parent := range commit.ParentHashes {
			if targets[parent] {
				return true, nil
			}
			if visited[parent] {
				continue
			}
			visited[parent] = true
			queue = append(queue, parent)
		}
	}
	return false, nil
}

// gitBandWriter is the writing half of a reply, multiplexed when the client
// asked for a side band.
//
// With a side band the packfile travels on band 1, progress on band 2 and a
// fatal error on band 3, so a failure part way through a pack reaches the
// client as a message it prints rather than as a stream that stops mid-object.
// Without one the packfile is the raw remainder of the stream and there is
// nowhere else to put anything, so progress is dropped and a failure can only
// end the connection.
type gitBandWriter struct {
	raw      io.Writer
	mux      *sideband.Muxer
	progress bool
	err      error
}

func newGitBandWriter(out io.Writer, mode gitSidebandMode, wantProgress bool) *gitBandWriter {
	band := &gitBandWriter{raw: out, progress: wantProgress}
	switch mode {
	case gitSideband64k:
		band.mux = sideband.NewMuxer(sideband.Sideband64k, out)
	case gitSideband:
		band.mux = sideband.NewMuxer(sideband.Sideband, out)
	}
	if band.mux == nil {
		band.progress = false
	}
	return band
}

// pack returns the writer the packfile bytes go to.
func (b *gitBandWriter) pack() io.Writer {
	if b.mux != nil {
		return b.mux
	}
	return b.raw
}

// progressf sends one progress line on band 2. Progress is advisory, so a
// failure to write it is remembered and reported by finish rather than
// abandoning a pack the client may still be able to use.
func (b *gitBandWriter) progressf(format string, args ...any) {
	if !b.progress || b.err != nil {
		return
	}
	if _, err := b.mux.WriteChannel(sideband.ProgressMessage, []byte(fmt.Sprintf(format, args...))); err != nil {
		b.err = err
	}
}

// fatal reports a failure that happened once the reply was already under way.
// On a multiplexed stream it becomes a band 3 message followed by the
// flush-pkt that ends the response, so the client prints the reason; on a raw
// stream the error is only returned, because a byte written there would be read
// as packfile content.
func (b *gitBandWriter) fatal(cause error) error {
	if b.mux == nil {
		return cause
	}
	if _, err := b.mux.WriteChannel(sideband.ErrorMessage, []byte(cause.Error()+"\n")); err != nil {
		return errors.Join(cause, err)
	}
	if err := pktline.NewEncoder(b.raw).Flush(); err != nil {
		return errors.Join(cause, err)
	}
	return cause
}

// finish ends a multiplexed reply with the flush-pkt that tells the client the
// pack is complete.
func (b *gitBandWriter) finish() error {
	if b.err != nil {
		return b.err
	}
	if b.mux == nil {
		return nil
	}
	return pktline.NewEncoder(b.raw).Flush()
}

// sendGitPackfile enumerates the objects the answer owes and writes them, with
// the counting and compressing progress the client prints on the way.
func sendGitPackfile(stor storer.Storer, out io.Writer, request *gitUploadRequest, boundary *gitFetchBoundary, mode gitSidebandMode) error {
	return sendGitPlannedPackfile(stor, out, request, boundary, nil, mode)
}

// sendGitPlannedPackfile writes the packfile for an answer whose object list
// may already have been enumerated.
//
// A protocol v2 reply that offers packfile URIs has to know the plan before the
// packfile section begins, because the section that names the URIs comes first
// and is derived from it — so that reply hands the plan in rather than having
// it walked twice. Every other reply passes nil and the walk happens here,
// where its position can be reported on the progress band as it goes.
func sendGitPlannedPackfile(stor storer.Storer, out io.Writer, request *gitUploadRequest, boundary *gitFetchBoundary, plan *gitPackPlan, mode gitSidebandMode) error {
	band := newGitBandWriter(out, mode, !request.noProgress)
	if plan == nil {
		counted := 0
		enumerated, err := gitObjectsToSend(stor, boundary, request, func(total int) {
			// Real progress: the count is the walk's own position, reported
			// often enough to move and rarely enough not to flood the band.
			if total-counted >= gitProgressInterval {
				counted = total
				band.progressf("Counting objects: %d\r", total)
			}
		})
		if err != nil {
			return band.fatal(err)
		}
		plan = enumerated
	}
	band.progressf("Enumerating objects: %d, done.\n", len(plan.objects))
	band.progressf("Counting objects: 100%% (%d/%d), done.\n", len(plan.objects), len(plan.objects))
	if err := writeGitPackfile(band, stor, plan, request.thinPack); err != nil {
		return band.fatal(err)
	}
	band.progressf("Total %d\n", len(plan.objects))
	return band.finish()
}

// gitProgressInterval is how many freshly counted objects separate two counting
// updates on the progress band.
const gitProgressInterval = 256

// gitClientRefusal is a reason the request itself carries — a reference that
// does not exist, a filter this server cannot honour, a boundary that selects
// nothing — as opposed to a storage or transport failure. git reports these to
// the user, so they travel in an ERR pkt-line rather than dying as an opaque
// stream error.
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
