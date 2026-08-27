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

// Protocol v2 command layer: ls-refs, fetch, and object-info, all delegating to
// the shared boundary/filter/packing code so the protocol version cannot change
// the answer. bundle-uri lives in git_bundleuri.go.

const gitProtocolHeader = "Git-Protocol"

// gitProtocolV2Requested reports whether a colon-separated "version=2" statement
// (the Git-Protocol header or GIT_PROTOCOL env var) asks for protocol v2.
func gitProtocolV2Requested(statement string) bool {
	for _, field := range strings.Split(statement, ":") {
		if strings.TrimSpace(field) == "version=2" {
			return true
		}
	}
	return false
}

// gitFetchCapabilityValue lists the fetch arguments this server understands
// beyond the v2 baseline: shallow/deepen (shallow-info), wait-for-done (drives
// `git fetch --negotiate-only`), filter (partial clone), ref-in-want (fetch by
// ref name without a full advertisement), sideband-all (whole reply multiplexed
// so any section's failure reaches the client).
const gitFetchCapabilityValue = "shallow wait-for-done filter ref-in-want sideband-all"

// gitFetchCapabilityLine adds packfile-uris only when the deployment can offer
// it — a presigned object-store GET, absent when repos are not in object
// storage (git_packuris.go).
func gitFetchCapabilityLine(offload bool) string {
	if !offload {
		return gitFetchCapabilityValue
	}
	return gitFetchCapabilityValue + " " + gitPackURIFetchArgument
}

// writeGitV2CapabilityAdvertisement opens a v2 connection in place of the v0
// ref advertisement, listing each capability in git serve.c's order.
func writeGitV2CapabilityAdvertisement(out io.Writer) error {
	offload := gitPackOffloadSupported()
	lines := []string{
		"version 2\n",
		"agent=" + capability.DefaultAgent() + "\n",
		"ls-refs=unborn\n",
		"fetch=" + gitFetchCapabilityLine(offload) + "\n",
		"server-option\n",
		"object-format=" + gitObjectFormat + "\n",
		"session-id=" + gitServerSessionID() + "\n",
		"object-info=size\n",
	}
	if offload {
		lines = append(lines, gitBundleURICommand+"\n")
	}
	encoder := pktline.NewEncoder(out)
	for _, line := range lines {
		if err := encoder.EncodeString(line); err != nil {
			return err
		}
	}
	return encoder.Flush()
}

// gitServerSessionID identifies this server's side of every v2 conversation.
// The protocol wants a process-stable, whitespace-free id, so it is computed
// once per process.
var gitServerSessionID = sync.OnceValue(func() string {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "bleephub"
	}
	return "bleephub-" + hex.EncodeToString(raw[:])
})

// gitV2Command is one decoded command request.
type gitV2Command struct {
	name         string
	capabilities []string
	arguments    []string
}

// readGitV2Command reads one command request. A bare flush-pkt or an ended
// stream closes the conversation, reported as a nil command.
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
	// serverOptions are the client's `-o` options, carried uninterpreted: the
	// protocol requires accepting an option the server does not act on.
	serverOptions []string
	sessionID     string
}

// checkGitV2Capabilities refuses any restated capability this server never
// advertised, and rejects a hash algorithm that would change the wire format.
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

// serveGitProtocolV2 runs the command half of a protocol v2 connection. A
// stateless caller (smart HTTP) serves one command and returns; a stateful one
// (SSH) keeps serving until the client stops, letting one connection run both
// ls-refs and fetch.
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
		case gitBundleURICommand:
			if err := serveGitBundleURIV2(ctx, stor, command.arguments, out); err != nil {
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

// serveGitLsRefsV2 answers the ls-refs command, supporting symrefs (name HEAD's
// branch), peel (resolve an annotated tag's target), ref-prefix (restrict the
// answer), and unborn — which lets an empty repository state the default branch
// its first commit will land on, when there is no object id to advertise.
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

// gitHeadSymrefTarget reports the branch HEAD points at, preferring storage so
// a moved HEAD is accurate, and falling back to defaultBranch for a
// just-created repository with no HEAD object yet.
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
// answer; no prefix matches everything.
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

// peelGitTagTarget resolves the non-tag object an annotated tag names, or false
// for anything that is not an annotated tag.
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

// gitV2Writer writes the pkt-lines of a protocol v2 response. Under sideband-all
// data lines travel on band 1, so a mid-reply error can go on band 3 instead of
// corrupting the section. delim- and flush-pkts are written plain in both modes,
// since the client's reader recognises them before demultiplexing.
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

func (w *gitV2Writer) line(format string, args ...any) error {
	if w.mux == nil {
		return pktline.NewEncoder(w.out).Encodef(format, args...)
	}
	_, err := w.mux.WriteChannel(sideband.PackData, []byte(fmt.Sprintf(format, args...)))
	return err
}

func (w *gitV2Writer) delim() error {
	return writeGitPktDelim(w.out)
}

func (w *gitV2Writer) flush() error {
	return pktline.NewEncoder(w.out).Flush()
}

// fail reports a client-printable reason on band 3 (multiplexed) or as an
// "ERR <message>" pkt-line, and returns it as an error so the transport logs it.
func (w *gitV2Writer) fail(message string) error {
	if w.mux == nil {
		return writeGitProtocolError(w.out, message)
	}
	if _, err := w.mux.WriteChannel(sideband.ErrorMessage, []byte(message+"\n")); err != nil {
		return err
	}
	return errors.New(message)
}

// serveGitFetchV2 answers the fetch command with named sections in the
// protocol's fixed order: acknowledgments (shared haves and whether negotiation
// can stop), shallow-info (deepening boundary), wanted-refs (the object each
// want-ref resolved to), and packfile (the objects, always multiplexed).
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
	// A request with no wants and no promised "done" is answered with silence,
	// not an empty pack, as git does.
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

	// For a client that offered to fetch packfiles itself, the packfile-uris
	// section is derived from the object walk, so the walk happens here (before
	// the packfile section) and its plan is handed to the writer rather than
	// walked again. A walk failure is reported on band 3 inside the packfile
	// section, the only channel left once the sections above are written.
	var plan *gitPackPlan
	var offer *gitPackURIOffer
	var planErr error
	if len(request.packURIProtocols) > 0 {
		if plan, planErr = gitObjectsToSend(stor, boundary, request, nil); planErr == nil {
			offer, err = gitPackURIOffload(ctx, stor, request, plan)
			if err != nil {
				return result, err
			}
			if offer != nil {
				if err := writeGitPackURIsSection(writer, offer); err != nil {
					return result, err
				}
			}
		}
	}

	if err := writer.line("packfile\n"); err != nil {
		return result, err
	}
	result.packed = true
	result.clone = len(request.haves) == 0
	switch {
	case planErr != nil:
		return result, newGitBandWriter(out, gitSideband64k, !request.noProgress).fatal(planErr)
	case offer != nil:
		return result, sendGitOffloadedPackfile(stor, out, request, offer, gitSideband64k)
	}
	return result, sendGitPlannedPackfile(stor, out, request, boundary, plan, gitSideband64k)
}

// applyGitFetchV2Argument records one fetch-command argument. The v2-only
// arguments are handled here; the rest go to the shared decoder so v2 and v0
// cannot disagree about what a want, have, deepen, or filter means.
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
	case strings.HasPrefix(argument, gitPackURIFetchArgument+" "):
		request.packURIProtocols = gitParsePackURIProtocols(strings.TrimPrefix(argument, gitPackURIFetchArgument+" "))
		return nil
	}
	return applyGitUploadRequestLine(stor, request, argument)
}

// applyGitWantRef resolves one want-ref line, letting a client that skipped the
// advertisement still learn the object id it is sent. An unknown or duplicate
// ref is refused with git's own wording so the client prints the reason.
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
// whether negotiation may proceed to a packfile in this response. A wait-for-done
// client is never told the server is ready, since it ends negotiation itself.
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

// serveGitObjectInfoV2 answers the object-info command, letting a partial clone
// learn an object's size before fetching its content. An unknown id is answered
// with the id alone — the protocol's "unknown" without failing the request.
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
