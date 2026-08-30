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

// Server side of git-upload-pack: ref advertisement, want/have/deepen
// negotiation and the packfile. Both transports (smart HTTP, SSH) call
// serveGitUploadPack (v0) and serveGitProtocolV2 (v2). Built from go-git
// *plumbing* rather than its server transport, which refuses every shallow
// request; this works against any storer.Storer (including S3) and never shells
// out to git.

// gitPackWindow is the delta window the encoder searches, matching go-git's server transport.
const gitPackWindow = 10

// gitHaveLineLength is the payload length of a "have <oid>" pkt-line.
const gitHaveLineLength = len("have ") + 40

// maxGitNegotiationHaves caps the haves one negotiation may offer, so a client
// that never stops talking cannot grow the server's slice without limit
// (matters most on SSH, which has no request-body cap).
const maxGitNegotiationHaves = 1 << 20

var (
	gitHavePrefix = []byte("have ")
	gitDoneLine   = []byte("done")
)

// setGitUploadPackCapabilities declares what this upload-pack honours; every entry is backed by working code here.
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

// gitObjectFormat is the hash algorithm for every object id on the wire.
const gitObjectFormat = "sha1"

// gitUploadPackAdvertisement builds the protocol v0 ref advertisement.
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

// setGitAdvertisedReferences copies every hash reference into the advertisement,
// skipping symbolic references other than HEAD.
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

// setGitAdvertisedHead resolves HEAD onto the advertisement. An empty
// repository (HEAD points at a branch with no commits) advertises no HEAD,
// which is how protocol v0 advertises an empty repo.
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

// gitUploadPackResult reports what a finished negotiation did, for transport bookkeeping.
type gitUploadPackResult struct {
	// packed is false when the client asked for no packfile (e.g. the first POST
	// of a stateless deepening fetch, which asks only for the shallow boundary).
	packed bool
	// clone is true when the client asked for objects with no have line — a fresh clone.
	clone bool
	// responded is true once any reply byte is written. Before that a transport
	// with a status code (smart HTTP) can still refuse; after, only the stream remains.
	responded bool
	// sessionID is the identifier a protocol v2 client gave for its side.
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
	// gitAckSingle: no multi_ack — one "ACK <oid>" for the first common object, nothing after.
	gitAckSingle gitAckMode = iota
	// gitAckMulti: multi_ack — "ACK <oid> continue" for every common object.
	gitAckMulti
	// gitAckDetailed: multi_ack_detailed — "ACK <oid> common" and "ACK <oid> ready".
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
// versions reduce to (v0 spells options as want-line capabilities and
// negotiates haves separately; v2 spells them as "fetch" arguments with haves
// inline). Once decoded the two are indistinguishable.
type gitUploadRequest struct {
	capabilities *capability.List
	wants        []plumbing.Hash
	shallows     []plumbing.Hash

	depth          int
	since          time.Time
	deepenNot      []string
	deepenRelative bool
	filter         gitObjectFilter
	// filtered marks a filter line already read, so a second one is refused rather than replacing the first.
	filtered bool

	// haves and done are carried inline by v2; v0 sends them in the negotiation phase.
	haves []plumbing.Hash
	done  bool

	sideband   gitSidebandMode
	noProgress bool
	includeTag bool
	thinPack   bool
	ackMode    gitAckMode
	noDone     bool

	// wantRefs are the references a v2 client asked for by name, each with its
	// resolved object, answered in the wanted-refs section.
	wantRefs []gitWantedRef
	// waitForDone suppresses the early "ready": the client will send "done"
	// itself, letting it negotiate without ever taking a pack.
	waitForDone bool
	// sidebandAll multiplexes the whole reply, so an error in any section reaches the client as a message.
	sidebandAll bool
	// packURIProtocols are the protocols the client can fetch a packfile over
	// itself; naming none opts out of the packfile-uris section.
	packURIProtocols []string
	// serverOptions are the "-o" options passed through; the protocol requires accepting options not acted on.
	serverOptions []string
	// sessionID is the identifier the client offered for its side.
	sessionID string
}

// gitWantedRef is one reference a client named in a want-ref line, resolved.
type gitWantedRef struct {
	name string
	hash plumbing.Hash
}

// deepens reports whether the request asks for a truncated history, which
// decides whether a shallow update is part of the answer.
func (r *gitUploadRequest) deepens() bool {
	return r.depth > 0 || !r.since.IsZero() || len(r.deepenNot) > 0
}

// wantedSet indexes the object ids named outright. These are sent regardless of
// the filter, which is what makes the lazy fetch of a filtered-out object work.
func (r *gitUploadRequest) wantedSet() map[plumbing.Hash]bool {
	wanted := make(map[plumbing.Hash]bool, len(r.wants))
	for _, hash := range r.wants {
		wanted[hash] = true
	}
	return wanted
}

// decodeGitUploadRequest reads a protocol v0 upload-request (wants, shallow
// boundary, deepen form, filter) up to the flush-pkt. packp's own decoder is
// not used: it collapses the three deepen forms into one and rejects the filter
// line, which is the whole of partial clone.
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

// splitGitWantCapabilities separates the first want line from its capability
// list. git writes a NUL between the object id and the capabilities; go-git
// writes a space. Both spellings are read.
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

// applyGitUploadRequestLine records one line of an upload-request or a v2
// "fetch" argument list; the two grammars share every keyword.
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
		// The encoder emits offset deltas on its own; no state needed.
	default:
		return fmt.Errorf("unexpected upload-pack line %q", line)
	}
	return nil
}

// parseGitObjectID reads the 40-character object id a want/have/shallow line
// carries, rejecting anything else rather than defaulting to a zero hash.
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

// applyGitUploadCapabilities turns the v0 client's echoed capability list into the request options.
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
// in must start at the upload-request and be buffered, since the pkt-line
// reader hands the stream to the packfile writer without look-ahead. Both
// transports share this: end-of-stream is the protocol terminator either way,
// so a stateless deepening fetch's first POST (want/deepen/flush) answers with
// the shallow update alone, no NAK and no pack, as git expects.
func serveGitUploadPack(ctx context.Context, stor storer.Storer, in *bufio.Reader, sink io.Writer) (result gitUploadPackResult, err error) {
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
	// A request must want something; there is no pack to build otherwise.
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
		// The boundary predicate selected nothing (a --shallow-exclude/--shallow-since
		// covering the whole history). git refuses via ERR rather than omitting the wants.
		return result, writeGitProtocolError(out, "no commits selected for shallow requests")
	}

	// Send the shallow update before have negotiation, and only when deepening,
	// so the client records its new boundary before reading a pack whose oldest
	// commits have parents the pack omits.
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

// checkGitUploadPackCapabilities refuses a request asking for an unadvertised
// capability, which means the two sides disagree about the protocol.
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
	// haves is every object the client offered, whether or not this server holds it.
	haves []plumbing.Hash
	// common is the subset this server also holds, in acknowledgement order.
	common []plumbing.Hash
	// done reports whether the client asked for a packfile.
	done bool
}

// negotiateGitHaves reads and answers the have/done half of a protocol v0
// request. Each common have is acknowledged immediately in the client's
// multi_ack spelling; once every want connects to the client's history an
// "ACK <oid> ready" says so, and a no-done client then takes the pack in the
// same round. A stream that simply ends leaves done false and is not an error.
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
			// The client offered a commit this server lacks, but this is still
			// where git checks whether the wants already connect to the common
			// set, to stop the client walking a branch this server cannot share.
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

// ackGitCommonHave acknowledges one common object in the mode's spelling. A
// non-multi_ack client is told only about the first common object.
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

// gitStorerHasCommit reports whether this repository holds the named commit.
func gitStorerHasCommit(stor storer.EncodedObjectStorer, hash plumbing.Hash) bool {
	_, err := stor.EncodedObject(plumbing.CommitObject, hash)
	return err == nil
}

// gitWantsReachCommon reports whether every wanted commit has an ancestor the
// client holds — the cut git calls "ready". connected memoises wants already
// proven to reach the common set (only positive answers, since a later round can grow it).
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

// gitCommitReachesSet walks a commit's ancestry for any member of targets.
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
			// A missing ancestor edge simply leads nowhere.
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
// asked for a side band (pack on band 1, progress on 2, fatal error on 3).
// Without a side band, progress is dropped and a failure can only end the connection.
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

// progressf sends one progress line on band 2. Progress is advisory, so a write
// failure is remembered for finish rather than abandoning a usable pack.
func (b *gitBandWriter) progressf(format string, args ...any) {
	if !b.progress || b.err != nil {
		return
	}
	if _, err := b.mux.WriteChannel(sideband.ProgressMessage, []byte(fmt.Sprintf(format, args...))); err != nil {
		b.err = err
	}
}

// fatal reports a failure once the reply is under way. On a multiplexed stream
// it becomes a band 3 message plus a closing flush-pkt; on a raw stream the
// error is only returned, since any byte there would read as packfile content.
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

// finish ends a multiplexed reply with the flush-pkt marking the pack complete.
func (b *gitBandWriter) finish() error {
	if b.err != nil {
		return b.err
	}
	if b.mux == nil {
		return nil
	}
	return pktline.NewEncoder(b.raw).Flush()
}

// sendGitPackfile enumerates the objects the answer owes and writes them.
func sendGitPackfile(stor storer.Storer, out io.Writer, request *gitUploadRequest, boundary *gitFetchBoundary, mode gitSidebandMode) error {
	return sendGitPlannedPackfile(stor, out, request, boundary, nil, mode)
}

// sendGitPlannedPackfile writes the packfile, taking a pre-enumerated plan or
// nil. A v2 reply offering packfile URIs must know the plan first (that section
// precedes the pack and derives from it), so it passes the plan in; every other
// reply passes nil and the walk happens here, reported on the progress band.
func sendGitPlannedPackfile(stor storer.Storer, out io.Writer, request *gitUploadRequest, boundary *gitFetchBoundary, plan *gitPackPlan, mode gitSidebandMode) error {
	band := newGitBandWriter(out, mode, !request.noProgress)
	if plan == nil {
		counted := 0
		enumerated, err := gitObjectsToSend(stor, boundary, request, func(total int) {
			// Report often enough to move, rarely enough not to flood the band.
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

// gitProgressInterval is how many counted objects separate two progress updates.
const gitProgressInterval = 256

// gitClientRefusal is a reason the request itself carries (a missing reference,
// an unhonourable filter, an empty boundary), as opposed to a storage or
// transport failure. These travel in an ERR pkt-line so git reports them to the user.
type gitClientRefusal struct{ reason string }

func (e *gitClientRefusal) Error() string { return e.reason }

// writeGitProtocolError sends the reason in an "ERR <message>" pkt-line and
// returns it as an error so the transport logs the refusal. Safe only before
// any other response line has been written.
func writeGitProtocolError(out io.Writer, message string) error {
	if err := pktline.NewEncoder(out).Encodef("ERR %s\n", message); err != nil {
		return err
	}
	return errors.New(message)
}
