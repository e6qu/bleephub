package bleephub

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/format/pktline"
	"github.com/go-git/go-git/v5/plumbing/protocol/packp/capability"
	"github.com/go-git/go-git/v5/plumbing/protocol/packp/sideband"
	"github.com/go-git/go-git/v5/plumbing/storer"
)

// Protocol v2 replaces the ref advertisement every connection used to begin
// with by a list of commands the client may call. The three it needs are
// implemented here: ls-refs, which answers the questions the v0 advertisement
// used to answer whether or not the client cared; fetch, which is the same
// exchange serveGitUploadPack runs re-spelled as one request with named
// sections; and object-info, which answers questions about objects without
// sending them. All three hand the real work to the shared boundary, filter and
// packing code, so a client cannot get a different answer by choosing a
// protocol version.

// gitProtocolHeader is the HTTP header, and the SSH environment variable, a
// client states its protocol version in.
const gitProtocolHeader = "Git-Protocol"

// gitProtocolV2Requested reports whether a "version=2" statement — the value of
// the Git-Protocol header or of the GIT_PROTOCOL environment variable — asks
// for protocol v2. The value is a colon-separated list of key=value pairs, of
// which only the version matters here.
func gitProtocolV2Requested(statement string) bool {
	for _, field := range strings.Split(statement, ":") {
		if strings.TrimSpace(field) == "version=2" {
			return true
		}
	}
	return false
}

// gitFetchCapabilityValue is the value of the fetch capability: the arguments
// the fetch command understands beyond the ones every v2 server understands.
//
//   - shallow: the "shallow", "deepen", "deepen-since", "deepen-not" and
//     "deepen-relative" arguments, answered in the shallow-info section.
//   - wait-for-done: the client will send "done" itself, so the negotiation
//     never announces it is ready and a round can carry acknowledgments alone.
//     That is what makes `git fetch --negotiate-only` work.
//   - filter: partial clone, with the omitted objects genuinely absent.
//   - ref-in-want: the "want-ref <ref>" argument and the wanted-refs section, so
//     a client can fetch by reference name without an advertisement of every
//     reference in the repository.
//   - sideband-all: the whole reply is multiplexed rather than only the
//     packfile section, so a failure in any section is a message the client
//     prints instead of a stream that stops.
const gitFetchCapabilityValue = "shallow wait-for-done filter ref-in-want sideband-all"

// writeGitV2CapabilityAdvertisement is what a v2 connection opens with, in
// place of the v0 ref advertisement.
//
// Each line is a capability the client may then use, in the order git's own
// serve.c advertises them: ls-refs reports unborn branches, fetch understands
// the arguments named above, server-option carries the client's `-o` options,
// every object id in this conversation is a SHA-1, session-id lets the two
// sides correlate their logs, and object-info answers object sizes without
// sending the objects.
func writeGitV2CapabilityAdvertisement(out io.Writer) error {
	encoder := pktline.NewEncoder(out)
	for _, line := range []string{
		"version 2\n",
		"agent=" + capability.DefaultAgent() + "\n",
		"ls-refs=unborn\n",
		"fetch=" + gitFetchCapabilityValue + "\n",
		"server-option\n",
		"object-format=" + gitObjectFormat + "\n",
		"session-id=" + gitServerSessionID() + "\n",
		"object-info=size\n",
	} {
		if err := encoder.EncodeString(line); err != nil {
			return err
		}
	}
	return encoder.Flush()
}

// gitServerSessionID is the identifier this server offers for its side of every
// protocol v2 conversation.
//
// The protocol asks for an id that identifies the process across requests, so
// that a client's trace and a server's log can be lined up against each other;
// it is therefore computed once per process rather than per request, and it is
// printable and free of whitespace as the protocol requires.
var gitServerSessionID = sync.OnceValue(func() string {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "bleephub"
	}
	return "bleephub-" + hex.EncodeToString(raw[:])
})

// gitV2Command is one decoded command request: the command name, the
// capabilities the client restated and the arguments that follow the delim-pkt.
type gitV2Command struct {
	name         string
	capabilities []string
	arguments    []string
}

// readGitV2Command reads one command request. A request that is nothing but a
// flush-pkt, or a stream that has ended, closes the conversation and is
// reported as a nil command.
func readGitV2Command(pkt *gitPktReader) (*gitV2Command, error) {
	command := &gitV2Command{}
	inArguments := false
	for {
		line, kind, err := pkt.next()
		if errors.Is(err, io.EOF) {
			if command.name == "" {
				return nil, nil
			}
			return nil, errors.New("truncated protocol v2 command request")
		}
		if err != nil {
			return nil, err
		}
		switch kind {
		case gitPktFlush:
			if command.name == "" {
				return nil, nil
			}
			return command, nil
		case gitPktDelim:
			inArguments = true
		case gitPktData:
			text := string(line)
			switch {
			case inArguments:
				command.arguments = append(command.arguments, text)
			case strings.HasPrefix(text, "command="):
				command.name = strings.TrimPrefix(text, "command=")
			default:
				command.capabilities = append(command.capabilities, text)
			}
		default:
			return nil, errors.New("unexpected control pkt-line in protocol v2 command request")
		}
	}
}

// gitV2Session is what the capability half of a command request established.
type gitV2Session struct {
	// serverOptions are the client's `-o` options. The protocol requires that
	// an option a server does not act on is accepted rather than refused, so
	// they are collected and carried without being interpreted.
	serverOptions []string
	// sessionID is the identifier the client offered for its side of the
	// conversation.
	sessionID string
}

// checkGitV2Capabilities reads the capabilities a command request restates and
// refuses one this server never advertised.
//
// A client only sends what it saw in the advertisement, so anything else means
// the two sides disagree about the protocol; git refuses such a request outright
// and so does this. The one capability carrying a value that changes the wire
// format is the hash algorithm, and it is checked rather than merely accepted.
func checkGitV2Capabilities(command *gitV2Command) (gitV2Session, error) {
	var session gitV2Session
	for _, stated := range command.capabilities {
		key, value, hasValue := strings.Cut(stated, "=")
		switch key {
		case "agent":
		case "object-format":
			if value != gitObjectFormat {
				return session, fmt.Errorf("unsupported object-format: %s", value)
			}
		case "server-option":
			if hasValue {
				session.serverOptions = append(session.serverOptions, value)
			}
		case "session-id":
			session.sessionID = value
		default:
			return session, fmt.Errorf("unknown capability '%s'", stated)
		}
	}
	return session, nil
}

// serveGitProtocolV2 runs the command half of a protocol v2 connection.
//
// A stateless caller — smart HTTP, where each POST is a whole request — serves
// one command and returns. A stateful one — SSH, where the channel stays open —
// keeps serving commands until the client stops sending them, which is how a
// clone gets to run ls-refs and fetch over a single connection.
func serveGitProtocolV2(ctx context.Context, stor storer.Storer, defaultBranch string, in *bufio.Reader, sink io.Writer, stateless bool) (result gitUploadPackResult, err error) {
	out := &gitResponseWriter{out: sink}
	defer func() { result.responded = out.wrote }()

	pkt := newGitPktReader(in)
	for {
		command, err := readGitV2Command(pkt)
		if err != nil || command == nil {
			return result, err
		}
		session, err := checkGitV2Capabilities(command)
		if err != nil {
			return result, err
		}
		result.sessionID = session.sessionID
		switch command.name {
		case "ls-refs":
			if err := serveGitLsRefsV2(stor, defaultBranch, command.arguments, out); err != nil {
				return result, err
			}
		case "fetch":
			fetched, err := serveGitFetchV2(ctx, stor, session, command.arguments, out)
			if fetched.packed {
				result.packed = true
				result.clone = fetched.clone
			}
			if err != nil {
				return result, err
			}
		case "object-info":
			if err := serveGitObjectInfoV2(stor, command.arguments, out); err != nil {
				return result, err
			}
		default:
			return result, fmt.Errorf("unsupported protocol v2 command: %s", command.name)
		}
		if stateless {
			return result, nil
		}
	}
}

// serveGitLsRefsV2 answers the ls-refs command.
//
// Beyond the ref list it can name the branch HEAD points at (symrefs), resolve
// the object an annotated tag names (peel) and restrict the answer to the parts
// of the namespace the client cares about (ref-prefix). The unborn argument is
// what finally lets an empty repository state its default branch: there is no
// object id to advertise, so the answer names the branch HEAD will point at
// once the first commit lands, and the client checks that branch out.
func serveGitLsRefsV2(stor storer.Storer, defaultBranch string, arguments []string, out io.Writer) error {
	var prefixes []string
	peel, symrefs, unborn := false, false, false
	for _, argument := range arguments {
		switch {
		case argument == "peel":
			peel = true
		case argument == "symrefs":
			symrefs = true
		case argument == "unborn":
			unborn = true
		case strings.HasPrefix(argument, "ref-prefix "):
			prefixes = append(prefixes, strings.TrimPrefix(argument, "ref-prefix "))
		default:
			return fmt.Errorf("unexpected ls-refs argument %q", argument)
		}
	}

	references := map[string]plumbing.Hash{}
	iter, err := stor.IterReferences()
	if err != nil {
		return err
	}
	if err := iter.ForEach(func(ref *plumbing.Reference) error {
		if ref.Type() == plumbing.HashReference {
			references[ref.Name().String()] = ref.Hash()
		}
		return nil
	}); err != nil {
		return err
	}

	headTarget := gitHeadSymrefTarget(stor, defaultBranch)
	if _, resolved := references[plumbing.HEAD.String()]; !resolved && headTarget != "" {
		if tip, exists := references[headTarget]; exists {
			references[plumbing.HEAD.String()] = tip
		}
	}

	names := make([]string, 0, len(references))
	for name := range references {
		names = append(names, name)
	}
	sort.Strings(names)

	encoder := pktline.NewEncoder(out)
	_, headAdvertised := references[plumbing.HEAD.String()]
	if unborn && !headAdvertised && headTarget != "" && gitReferenceMatchesPrefixes(plumbing.HEAD.String(), prefixes) {
		if err := encoder.Encodef("unborn %s symref-target:%s\n", plumbing.HEAD.String(), headTarget); err != nil {
			return err
		}
	}
	for _, name := range names {
		if !gitReferenceMatchesPrefixes(name, prefixes) {
			continue
		}
		line := references[name].String() + " " + name
		if symrefs && name == plumbing.HEAD.String() && headTarget != "" {
			line += " symref-target:" + headTarget
		}
		if peel {
			if peeled, ok := peelGitTagTarget(stor, references[name]); ok {
				line += " peeled:" + peeled.String()
			}
		}
		if err := encoder.Encodef("%s\n", line); err != nil {
			return err
		}
	}
	return encoder.Flush()
}

// gitHeadSymrefTarget reports the branch HEAD points at. Storage is asked
// first, so a repository whose HEAD was moved is described accurately; a
// repository that has no HEAD object yet — one that was just created — is
// described by the default branch it was created with, which is the name its
// first commit will land on.
func gitHeadSymrefTarget(stor storer.Storer, defaultBranch string) string {
	if ref, err := stor.Reference(plumbing.HEAD); err == nil && ref.Type() == plumbing.SymbolicReference {
		return ref.Target().String()
	}
	if defaultBranch == "" {
		return ""
	}
	return plumbing.NewBranchReferenceName(defaultBranch).String()
}

// gitReferenceMatchesPrefixes reports whether a reference belongs in an ls-refs
// answer. A request that named no prefix wants everything.
func gitReferenceMatchesPrefixes(name string, prefixes []string) bool {
	if len(prefixes) == 0 {
		return true
	}
	for _, prefix := range prefixes {
		if strings.HasPrefix(name, prefix) {
			return true
		}
	}
	return false
}

// peelGitTagTarget resolves the non-tag object an annotated tag names, and
// reports false for anything that is not an annotated tag.
func peelGitTagTarget(stor storer.Storer, hash plumbing.Hash) (plumbing.Hash, bool) {
	encoded, err := stor.EncodedObject(plumbing.AnyObject, hash)
	if err != nil || encoded.Type() != plumbing.TagObject {
		return plumbing.ZeroHash, false
	}
	target, err := peelGitObjectToCommit(stor, hash)
	if err != nil {
		return plumbing.ZeroHash, false
	}
	return target, true
}

// gitV2Writer writes the pkt-lines of a protocol v2 response.
//
// With sideband-all every data line travels inside a band 1 packet, so an error
// raised part way through the reply can be sent on band 3 and printed by the
// client rather than corrupting the section it interrupted. The delim-pkts that
// separate sections and the flush-pkt that ends the reply are written plain in
// both modes, because the client's pkt-line reader recognises those before it
// demultiplexes anything.
type gitV2Writer struct {
	out io.Writer
	mux *sideband.Muxer
}

func newGitV2Writer(out io.Writer, sidebandAll bool) *gitV2Writer {
	writer := &gitV2Writer{out: out}
	if sidebandAll {
		writer.mux = sideband.NewMuxer(sideband.Sideband64k, out)
	}
	return writer
}

// line writes one data pkt-line.
func (w *gitV2Writer) line(format string, args ...any) error {
	if w.mux == nil {
		return pktline.NewEncoder(w.out).Encodef(format, args...)
	}
	_, err := w.mux.WriteChannel(sideband.PackData, []byte(fmt.Sprintf(format, args...)))
	return err
}

// delim ends one section of the response.
func (w *gitV2Writer) delim() error {
	return writeGitPktDelim(w.out)
}

// flush ends the response.
func (w *gitV2Writer) flush() error {
	return pktline.NewEncoder(w.out).Flush()
}

// fail reports a reason the client can print. On a multiplexed reply it travels
// on band 3, and otherwise as the "ERR <message>" pkt-line both git and go-git
// recognise; either way it is returned as an error so the transport logs the
// refusal too.
func (w *gitV2Writer) fail(message string) error {
	if w.mux == nil {
		return writeGitProtocolError(w.out, message)
	}
	if _, err := w.mux.WriteChannel(sideband.ErrorMessage, []byte(message+"\n")); err != nil {
		return err
	}
	return errors.New(message)
}

// serveGitFetchV2 answers the fetch command.
//
// The response is a run of named sections, in the order the protocol fixes:
// acknowledgments reports which of the client's have lines this repository
// shares and whether the negotiation can stop, and a round that cannot stop yet
// ends there so the client asks again with more haves; shallow-info carries the
// boundary a deepening request establishes; wanted-refs names the object each
// want-ref resolved to, which is the only way a client that never asked for an
// advertisement learns them; and packfile carries the objects, always
// multiplexed, so progress and a fatal error have somewhere to go.
func serveGitFetchV2(ctx context.Context, stor storer.Storer, session gitV2Session, arguments []string, out io.Writer) (gitUploadPackResult, error) {
	var result gitUploadPackResult
	request := &gitUploadRequest{capabilities: capability.NewList()}
	request.serverOptions = session.serverOptions
	request.sessionID = session.sessionID
	writer := newGitV2Writer(out, false)
	for _, argument := range arguments {
		if err := applyGitFetchV2Argument(stor, request, argument); err != nil {
			var refusal *gitClientRefusal
			if errors.As(err, &refusal) {
				return result, writer.fail(refusal.reason)
			}
			return result, err
		}
		if request.sidebandAll && writer.mux == nil {
			writer = newGitV2Writer(out, true)
		}
	}
	// A request that wants nothing and did not promise a "done" of its own has
	// asked for nothing at all, and git answers it with silence rather than
	// with an empty pack.
	if len(request.wants) == 0 && !request.waitForDone {
		return result, nil
	}

	boundary, err := gitFetchBoundaryFor(stor, request)
	if err != nil {
		var refusal *gitClientRefusal
		if errors.As(err, &refusal) {
			return result, writer.fail(refusal.reason)
		}
		return result, err
	}
	if request.deepens() && len(boundary.order) == 0 {
		return result, writer.fail("no commits selected for shallow requests")
	}

	if len(request.haves) > 0 && !request.done {
		ready, err := writeGitAcknowledgmentsV2(stor, writer, request, boundary)
		if err != nil {
			return result, err
		}
		if !ready {
			return result, writer.flush()
		}
		if err := writer.delim(); err != nil {
			return result, err
		}
	}

	if len(boundary.shallows) > 0 || len(boundary.unshallows) > 0 {
		if err := writer.line("shallow-info\n"); err != nil {
			return result, err
		}
		for _, hash := range boundary.shallows {
			if err := writer.line("shallow %s\n", hash.String()); err != nil {
				return result, err
			}
		}
		for _, hash := range boundary.unshallows {
			if err := writer.line("unshallow %s\n", hash.String()); err != nil {
				return result, err
			}
		}
		if err := writer.delim(); err != nil {
			return result, err
		}
	}

	if len(request.wantRefs) > 0 {
		if err := writer.line("wanted-refs\n"); err != nil {
			return result, err
		}
		for _, wanted := range request.wantRefs {
			if err := writer.line("%s %s\n", wanted.hash.String(), wanted.name); err != nil {
				return result, err
			}
		}
		if err := writer.delim(); err != nil {
			return result, err
		}
	}

	if ctxErr := ctx.Err(); ctxErr != nil {
		return result, ctxErr
	}
	if err := writer.line("packfile\n"); err != nil {
		return result, err
	}
	result.packed = true
	result.clone = len(request.haves) == 0
	return result, sendGitPackfile(stor, out, request, boundary, gitSideband64k)
}

// applyGitFetchV2Argument records one argument of a protocol v2 fetch command.
//
// The arguments protocol v0 spells as capabilities on its first want line are
// read by the shared decoder, so the two versions cannot disagree about what a
// want, a have, a deepen or a filter means. The ones only v2 defines are read
// here.
func applyGitFetchV2Argument(stor storer.Storer, request *gitUploadRequest, argument string) error {
	switch {
	case argument == "wait-for-done":
		request.waitForDone = true
		return nil
	case argument == "sideband-all":
		request.sidebandAll = true
		return nil
	case strings.HasPrefix(argument, "want-ref "):
		return applyGitWantRef(stor, request, strings.TrimPrefix(argument, "want-ref "))
	}
	return applyGitUploadRequestLine(stor, request, argument)
}

// applyGitWantRef resolves one want-ref line.
//
// The reference is resolved here and answered in the wanted-refs section, so a
// client that skipped the ref advertisement altogether — the only affordable
// way to fetch from a repository carrying very many references — still learns
// the object id it is being sent. A reference this repository does not hold, or
// one named twice, is refused with git's own wording so the client prints the
// reason rather than reporting a truncated stream.
func applyGitWantRef(stor storer.Storer, request *gitUploadRequest, name string) error {
	for _, existing := range request.wantRefs {
		if existing.name == name {
			return &gitClientRefusal{reason: "duplicate want-ref " + name}
		}
	}
	ref, err := storer.ResolveReference(stor, plumbing.ReferenceName(name))
	if err != nil || ref == nil || ref.Hash().IsZero() {
		return &gitClientRefusal{reason: "unknown ref " + name}
	}
	request.wantRefs = append(request.wantRefs, gitWantedRef{name: name, hash: ref.Hash()})
	request.wants = append(request.wants, ref.Hash())
	return nil
}

// writeGitAcknowledgmentsV2 writes the acknowledgments section and reports
// whether the negotiation may proceed to a packfile in this same response.
//
// A client that asked for wait-for-done is never told the server is ready: it
// has undertaken to end the negotiation itself, which is what lets it run a
// negotiation that never takes a pack at all.
func writeGitAcknowledgmentsV2(stor storer.Storer, writer *gitV2Writer, request *gitUploadRequest, boundary *gitFetchBoundary) (bool, error) {
	if err := writer.line("acknowledgments\n"); err != nil {
		return false, err
	}
	common := map[plumbing.Hash]bool{}
	acknowledged := 0
	for _, hash := range request.haves {
		if !gitStorerHasCommit(stor, hash) {
			continue
		}
		common[hash] = true
		acknowledged++
		if err := writer.line("ACK %s\n", hash.String()); err != nil {
			return false, err
		}
	}
	if acknowledged == 0 {
		return false, writer.line("NAK\n")
	}
	if request.waitForDone {
		return false, nil
	}
	ready, err := gitWantsReachCommon(stor, boundary.roots, common, map[plumbing.Hash]bool{})
	if err != nil {
		return false, err
	}
	if !ready {
		return false, nil
	}
	return true, writer.line("ready\n")
}

// serveGitObjectInfoV2 answers the object-info command.
//
// A partial clone uses it to learn how large an object is without fetching its
// content, which is what makes a blob:limit clone predictable and a lazy fetch
// affordable: the client can decide whether it wants the bytes before it pays
// for them. An object id this repository does not hold is answered with the id
// and nothing after it, which is how the protocol says "unknown" without
// failing the whole request.
func serveGitObjectInfoV2(stor storer.Storer, arguments []string, out io.Writer) error {
	encoder := pktline.NewEncoder(out)
	wantSize := false
	var ids []string
	for _, argument := range arguments {
		switch {
		case argument == "size":
			wantSize = true
		case strings.HasPrefix(argument, "oid "):
			ids = append(ids, strings.TrimPrefix(argument, "oid "))
		default:
			if err := encoder.Encodef("ERR object-info: unexpected line: '%s'\n", argument); err != nil {
				return err
			}
		}
	}
	if len(ids) == 0 {
		return encoder.Flush()
	}
	if wantSize {
		if err := encoder.EncodeString("size\n"); err != nil {
			return err
		}
	}
	for _, id := range ids {
		hash, err := parseGitObjectID(id)
		if err != nil {
			if err := encoder.Encodef("ERR object-info: protocol error, expected to get oid, not '%s'\n", id); err != nil {
				return err
			}
			continue
		}
		encoded, err := stor.EncodedObject(plumbing.AnyObject, hash)
		if err != nil {
			if err := encoder.Encodef("%s \n", id); err != nil {
				return err
			}
			continue
		}
		if !wantSize {
			if err := encoder.Encodef("%s\n", id); err != nil {
				return err
			}
			continue
		}
		if err := encoder.Encodef("%s %d\n", id, encoded.Size()); err != nil {
			return err
		}
	}
	return encoder.Flush()
}
