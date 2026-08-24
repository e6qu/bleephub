package bleephub

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/e6qu/bleephub/internal/store"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/format/packfile"
	"github.com/go-git/go-git/v5/plumbing/format/pktline"
	"github.com/go-git/go-git/v5/plumbing/protocol/packp"
	"github.com/go-git/go-git/v5/plumbing/protocol/packp/capability"
	"github.com/go-git/go-git/v5/plumbing/protocol/packp/sideband"
	"github.com/go-git/go-git/v5/plumbing/storer"
)

// This file is the whole server side of git-receive-pack: the ref
// advertisement, the reference-update request, the object ingestion, the ref
// updates and the status report the client reads each ref's verdict from. Both
// transports — smart HTTP (git_http.go) and SSH (git_ssh.go) — decode with
// decodeGitReceiveRequest, decide with applyGitReceivePack and answer with
// writeGitReceivePackResponse, so a pusher cannot observe a different protocol
// depending on which URL scheme it dialed.
//
// go-git ships a server transport (plumbing/transport/server) and bleephub used
// to push through it, but that implementation advertises four capabilities,
// applies every command unconditionally, has no atomic transaction, no push
// options, no side band and no report-status-v2, and its reference-update
// decoder models the shallow boundary as a single optional hash — so a push
// from a clone with two boundaries failed outright. Everything below is built
// from go-git *plumbing* — packp for the wire messages, packfile for object
// ingestion, storer for the graph — so it keeps running against whatever
// storer.Storer the repository resolves to, including the S3-backed one.
// Nothing here touches a filesystem or shells out to git.

// capabilityReportStatusV2 is the richer per-ref report. packp's capability
// package predates it and has no constant for it; an unknown capability with no
// argument is carried verbatim by capability.List, which is exactly the wire
// spelling this server advertises and the client echoes back.
const capabilityReportStatusV2 = capability.Capability("report-status-v2")

// maxGitPushOptions and maxGitPushOptionBytes bound the push-option list one
// push may carry. The option section is read before the packfile, from a client
// that has only proven it may write to this repository, so an unbounded list is
// a way to make the server hold an arbitrary amount of attacker-chosen text.
// git's own `git push -o` usage is a handful of short key=value strings, so
// these ceilings are orders of magnitude above any real push. A client that
// exceeds them has its push refused with a reason rather than silently
// truncated, because a server-side hook acting on a truncated option list would
// act on something the pusher never sent.
const (
	maxGitPushOptions     = 64
	maxGitPushOptionBytes = 8 << 10
)

// gitPushUnpackerError is the per-ref status git reports when the packfile
// itself could not be stored: no command was even considered, so none of them
// has a verdict of its own.
const gitPushUnpackerError = "n/a (unpacker error)"

// gitAtomicPushFailure is the per-ref status the commands that were themselves
// fine receive when an atomic push is refused because a different command
// failed. It is git's own wording for the collateral refusal.
const gitAtomicPushFailure = "atomic push failure"

// gitStalePushCommand is the status a command whose asserted old object id does
// not match the reference this server holds receives. It is git's wording, and
// it says the right thing: the pusher's view of the remote is out of date, so
// it has to fetch before it can push.
const gitStalePushCommand = "fetch first"

// gitSecretScanningPushRefusal is the per-ref status a push carrying a secret
// that push protection blocks receives. The detail — which secret, and the
// placeholder id a bypass request needs — does not fit in a status line, so it
// travels on the message band.
const gitSecretScanningPushRefusal = "push declined due to repository rule violations"

// setGitReceivePackCapabilities declares what this receive-pack honours, and
// every entry is backed by working code in this file:
//
//   - report-status, report-status-v2: the per-ref verdict, and the v2
//     additions — "option refname", "option old-oid", "option new-oid" and
//     "option forced-update" — that let the client print the reference the
//     server really moved, the object ids it moved it between, and whether the
//     update discarded commits.
//   - delete-refs: a command whose new object id is zero removes the reference.
//   - side-band-64k: the reply is multiplexed, so the server's messages reach
//     the client on band 2 interleaved with the report on band 1 rather than
//     arriving after it or not at all.
//   - quiet: the progress-style messages band 2 would otherwise carry are
//     suppressed. Refusal explanations are not progress and are still sent.
//   - atomic: every command is decided before any of them is applied, and a
//     single refusal leaves the repository exactly as it was.
//   - ofs-delta: the ingested packfile may carry offset deltas.
//   - push-options: the option list `git push -o` sends is read from the wire,
//     bounded, and handed to the server-side push logic.
//   - object-format: the hash algorithm every object id on the wire is written
//     in.
//   - agent: identity.
func setGitReceivePackCapabilities(caps *capability.List) error {
	for _, entry := range []struct {
		name   capability.Capability
		values []string
	}{
		{name: capability.ReportStatus},
		{name: capabilityReportStatusV2},
		{name: capability.DeleteRefs},
		{name: capability.Sideband64k},
		{name: capability.Quiet},
		{name: capability.Atomic},
		{name: capability.OFSDelta},
		{name: capability.PushOptions},
		{name: capability.ObjectFormat, values: []string{gitObjectFormat}},
		{name: capability.Agent, values: []string{capability.DefaultAgent()}},
	} {
		if err := caps.Set(entry.name, entry.values...); err != nil {
			return err
		}
	}
	return nil
}

// gitReceivePackAdvertisement builds the ref advertisement for
// git-receive-pack. Callers layer advertiseDefaultBranchSymref on top, which is
// what names the repository's default branch to the pusher.
//
// A repository with no references at all is advertised the same way git
// advertises one: no reference lines, and the capabilities on the zero-id
// "capabilities^{}" sentinel packp.AdvRefs.Encode writes for exactly this case.
func gitReceivePackAdvertisement(stor storer.Storer) (*packp.AdvRefs, error) {
	advertisement := packp.NewAdvRefs()
	if err := setGitReceivePackCapabilities(advertisement.Capabilities); err != nil {
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

// gitReceiveRequest is one decoded reference-update request: what the client
// wants changed, what it can understand about the answer, and where the objects
// it is sending start.
type gitReceiveRequest struct {
	capabilities *capability.List
	commands     []*packp.Command

	// shallow is the boundary a pusher whose own clone is truncated declares
	// before its commands. This server holds the complete history of every
	// repository it serves, so the boundary changes nothing it decides — an
	// update's fast-forward-ness is answered from the server's own graph rather
	// than from the client's truncated one — but it is part of the request
	// grammar and has to be read off the wire, in whatever quantity the client
	// sends, before the command list can be.
	shallow []plumbing.Hash

	// pushOptions is the `git push -o` list, available to the server-side push
	// logic and echoed back to the pusher the way a hook's output is.
	pushOptions []string

	// packfile is the object stream the request ends with, or nil for a push
	// that only deletes references and therefore sends no objects.
	packfile io.Reader

	reportStatus          bool
	reportStatusV2        bool
	sideband              bool
	quiet                 bool
	atomic                bool
	pushOptionsNegotiated bool
}

// reportsStatus reports whether the client asked for a per-ref verdict at all.
// A client that did not cannot be told which of its commands were refused, so a
// refusal has to fail the whole push through the transport instead.
func (r *gitReceiveRequest) reportsStatus() bool {
	return r.reportStatus || r.reportStatusV2
}

// deleteOnly reports whether every command removes a reference. That is the
// protocol's own rule for whether a packfile follows the command list: a push
// with nothing to add sends no objects, and a server that waited for them would
// hang on a transport whose stream does not end until the client is answered.
func (r *gitReceiveRequest) deleteOnly() bool {
	for _, command := range r.commands {
		if !command.New.IsZero() {
			return false
		}
	}
	return true
}

// decodeGitReceiveRequest reads a reference-update request: the shallow
// boundary a truncated clone declares, the command list with the capabilities
// the client echoed on its first line, the push options that follow it, and the
// position the packfile starts at.
//
// packp has a decoder for this message, but it models the shallow boundary as
// one optional hash — a clone with two boundaries, which is all it takes to
// fetch a second branch at depth 1, made it read the second shallow line as a
// command and fail the push with "capabilities delimiter not found" — and it
// has no notion of the push-option section at all, which it would hand to the
// packfile parser as leading garbage. The grammar below is the one git sends.
func decodeGitReceiveRequest(in *bufio.Reader) (*gitReceiveRequest, error) {
	request := &gitReceiveRequest{capabilities: capability.NewList()}
	pkt := newGitPktReader(in)
	first := true
	for {
		line, kind, err := pkt.next()
		if err != nil {
			return nil, err
		}
		if kind == gitPktFlush {
			break
		}
		if kind != gitPktData {
			return nil, errors.New("unexpected pkt-line in a reference-update request")
		}
		if boundary, isShallow := bytes.CutPrefix(line, []byte("shallow ")); isShallow && first {
			hash, err := parseGitObjectID(string(boundary))
			if err != nil {
				return nil, err
			}
			request.shallow = append(request.shallow, hash)
			continue
		}
		body := line
		if first {
			first = false
			if separator := bytes.IndexByte(body, 0); separator >= 0 {
				if err := request.capabilities.Decode(body[separator+1:]); err != nil {
					return nil, err
				}
				body = body[:separator]
			}
		}
		command, err := parseGitPushCommand(string(body))
		if err != nil {
			return nil, err
		}
		request.commands = append(request.commands, command)
	}
	if len(request.commands) == 0 {
		return nil, packp.ErrEmptyCommands
	}
	if err := applyGitReceiveCapabilities(request); err != nil {
		return nil, err
	}
	if request.pushOptionsNegotiated {
		if err := decodeGitPushOptions(pkt, request); err != nil {
			return nil, err
		}
	}
	if !request.deleteOnly() {
		request.packfile = in
	}
	return request, nil
}

// parseGitPushCommand reads one "<old-oid> <new-oid> <refname>" command line.
func parseGitPushCommand(line string) (*packp.Command, error) {
	fields := strings.SplitN(line, " ", 3)
	if len(fields) != 3 {
		return nil, packp.ErrMalformedCommand
	}
	old, err := parseGitObjectID(fields[0])
	if err != nil {
		return nil, err
	}
	next, err := parseGitObjectID(fields[1])
	if err != nil {
		return nil, err
	}
	command := &packp.Command{Name: plumbing.ReferenceName(fields[2]), Old: old, New: next}
	if command.Action() == packp.Invalid {
		return nil, packp.ErrMalformedCommand
	}
	if !validFullyQualifiedGitRef(command.Name.String()) {
		return nil, fmt.Errorf("malformed reference name %q", command.Name)
	}
	return command, nil
}

// decodeGitPushOptions reads the option section that follows the command list
// when push-options was negotiated, enforcing the ceilings above.
func decodeGitPushOptions(pkt *gitPktReader, request *gitReceiveRequest) error {
	total := 0
	for {
		line, kind, err := pkt.next()
		if err != nil {
			return err
		}
		if kind == gitPktFlush {
			return nil
		}
		if kind != gitPktData {
			return errors.New("unexpected pkt-line in the push-option section")
		}
		if len(request.pushOptions) >= maxGitPushOptions {
			return fmt.Errorf("a push may carry at most %d push options", maxGitPushOptions)
		}
		total += len(line)
		if total > maxGitPushOptionBytes {
			return fmt.Errorf("a push may carry at most %d bytes of push options", maxGitPushOptionBytes)
		}
		request.pushOptions = append(request.pushOptions, string(line))
	}
}

// applyGitReceiveCapabilities turns the capability list the client echoed back
// into the options the rest of the exchange reads, and refuses any capability
// this server never advertised — honouring a capability that was not offered
// would mean answering in a dialect the advertisement promised not to speak.
func applyGitReceiveCapabilities(request *gitReceiveRequest) error {
	advertised := capability.NewList()
	if err := setGitReceivePackCapabilities(advertised); err != nil {
		return err
	}
	for _, requested := range request.capabilities.All() {
		if !advertised.Supports(requested) {
			return fmt.Errorf("unsupported capability: %s", requested)
		}
	}
	caps := request.capabilities
	request.reportStatus = caps.Supports(capability.ReportStatus)
	request.reportStatusV2 = caps.Supports(capabilityReportStatusV2)
	request.sideband = caps.Supports(capability.Sideband64k)
	request.quiet = caps.Supports(capability.Quiet)
	request.atomic = caps.Supports(capability.Atomic)
	request.pushOptionsNegotiated = caps.Supports(capability.PushOptions)
	if values := caps.Get(capability.ObjectFormat); len(values) > 0 && values[0] != gitObjectFormat {
		return fmt.Errorf("unsupported object-format: %s", values[0])
	}
	return nil
}

// refusedPushError is a push a rule refused on a connection with no
// report-status channel to carry the per-ref refusal. The whole push fails
// rather than reporting a success for a ref that did not move.
type refusedPushError struct{ reason string }

func (e *refusedPushError) Error() string { return e.reason }

// gitPushStatus is one command's verdict.
type gitPushStatus struct {
	command *packp.Command
	// requested is the reference name the client named, which is the name the
	// "ok"/"ng" line has to carry so the client can match the verdict to the
	// command it sent.
	requested plumbing.ReferenceName
	// applied is the reference this server actually moved. It is reported as
	// "option refname" so a client is told the truth even when the two differ.
	applied plumbing.ReferenceName
	// status is the refusal, or "" when the command succeeded.
	status string
	// forced is set when the update discards commits the reference could reach,
	// which is what makes the client print "forced update".
	forced bool
	// old and new are the object ids the reference moved between, as this
	// server holds them rather than as the client asserted them.
	old, new plumbing.Hash
	// updated is set once the reference has actually been written, so a
	// rollback knows what to undo and only genuinely applied commands raise
	// push events.
	updated bool
}

// gitPushReport is the report-status (or report-status-v2) message.
type gitPushReport struct {
	unpackStatus string
	statuses     []*gitPushStatus
	v2           bool
}

// encode writes the report. The v2 form adds the option lines that carry the
// reference the server moved, the object ids it moved between and whether the
// update was forced; they are only legal after an "ok" line, so a refused
// command carries its reason on the "ng" line alone.
func (r *gitPushReport) encode(out io.Writer) error {
	enc := pktline.NewEncoder(out)
	if err := enc.Encodef("unpack %s\n", r.unpackStatus); err != nil {
		return err
	}
	for _, status := range r.statuses {
		if status.status != "" {
			if err := enc.Encodef("ng %s %s\n", status.requested, status.status); err != nil {
				return err
			}
			continue
		}
		if err := enc.Encodef("ok %s\n", status.requested); err != nil {
			return err
		}
		if !r.v2 {
			continue
		}
		for _, option := range []string{
			fmt.Sprintf("option refname %s\n", status.applied),
			fmt.Sprintf("option old-oid %s\n", status.old),
			fmt.Sprintf("option new-oid %s\n", status.new),
		} {
			if err := enc.Encodef("%s", option); err != nil {
				return err
			}
		}
		if status.forced {
			if err := enc.Encodef("option forced-update\n"); err != nil {
				return err
			}
		}
	}
	return enc.Flush()
}

// gitReceivePackOutcome is everything a finished push produced: the report the
// client reads, the messages that travel beside it and the commands whose
// reference really moved.
type gitReceivePackOutcome struct {
	report   *gitPushReport
	messages []string
	applied  []*packp.Command
}

// messagef adds one line to the message band. Every message ends in a newline
// because git prints band 2 line by line, prefixing each with "remote: ".
func (o *gitReceivePackOutcome) messagef(format string, args ...any) {
	o.messages = append(o.messages, gitStatusLine(fmt.Sprintf(format, args...))+"\n")
}

// gitStatusLine flattens text into something a pkt-line status or a band 2
// message can carry. A newline inside either would be read as the end of the
// line and turn the remainder into a message of its own, so a multi-line error
// from storage becomes one line here rather than a corrupt report.
func gitStatusLine(text string) string {
	return strings.Join(strings.Fields(text), " ")
}

// applyGitReceivePack is the write half of both git transports: it ingests the
// pushed objects, decides every command before any of them is applied, applies
// the ones the rules allow, and builds the report the client reads each ref's
// verdict from.
//
// The objects are ingested first because whether an update discards commits —
// the question force-push protection and "forced update" both turn on — is only
// answerable once the commits the push carries are readable.
func (s *Server) applyGitReceivePack(ctx context.Context, target *gitTarget, request *gitReceiveRequest) (*gitReceivePackOutcome, error) {
	stor := target.stor
	outcome := &gitReceivePackOutcome{report: &gitPushReport{unpackStatus: "ok", v2: request.reportStatusV2}}
	// The options are reported back the way a server-side hook's output is, so
	// a pusher can see that the server read exactly what it sent.
	for _, option := range request.pushOptions {
		outcome.messagef("push option: %s", option)
	}
	for _, command := range request.commands {
		outcome.report.statuses = append(outcome.report.statuses, &gitPushStatus{
			command:   command,
			requested: command.Name,
			applied:   command.Name,
			old:       command.Old,
			new:       command.New,
		})
	}
	if err := ingestPushedObjects(stor, request.packfile); err != nil {
		outcome.report.unpackStatus = gitStatusLine(err.Error())
		outcome.messagef("error: %s", err.Error())
		for _, status := range outcome.report.statuses {
			status.status = gitPushUnpackerError
		}
		return outcome, nil
	}
	if err := s.decideGitPushCommands(ctx, target, outcome); err != nil {
		return nil, err
	}
	if !request.reportsStatus() {
		for _, status := range outcome.report.statuses {
			if status.status != "" {
				return nil, &refusedPushError{reason: status.status}
			}
		}
	}
	if request.atomic {
		refuseGitPushAtomically(outcome.report.statuses)
	}
	if err := s.applyGitPushCommands(stor, outcome, request.atomic); err != nil {
		return nil, err
	}
	if !request.quiet {
		outcome.messagef("Updated %d of %d ref(s), done.", len(outcome.applied), len(outcome.report.statuses))
	}
	// The objects this push wrote are loose, and loose is the tier a fetch
	// cannot be answered from by copying. Pack them. A push that carried no
	// packfile — a reference deletion, or an update to objects the server
	// already held — wrote nothing to pack. See git_compaction.go for why this
	// does not delay the report the caller is about to write.
	if request.packfile != nil {
		s.scheduleGitCompaction(target.storageName, stor)
	}
	return outcome, nil
}

// decideGitPushCommands gives every command its verdict without touching a
// single reference. Deciding the whole list up front is what makes an atomic
// push implementable at all — the refusal has to be known before the first
// reference moves — and it is what keeps a non-atomic push from applying a
// command whose neighbour was going to be refused by a rule that had not been
// evaluated yet.
func (s *Server) decideGitPushCommands(ctx context.Context, target *gitTarget, outcome *gitReceivePackOutcome) error {
	repo, stor := target.repo, target.stor
	for _, status := range outcome.report.statuses {
		command := status.command
		kind, err := classifyPushedRefWrite(stor, command)
		if err != nil {
			return err
		}
		status.forced = kind == refForcePush
		if stale := gitPushPreconditionRefusal(stor, command); stale != "" {
			status.status = stale
			continue
		}
		// A wiki has neither branch protection nor secret scanning on github:
		// the settings that configure them are repository settings, and the
		// scanner reads repository content. Deciding a wiki push against the
		// repository's rules would refuse pushes github accepts and would
		// attribute a wiki's text to the repository's alerts.
		if target.wiki {
			continue
		}
		if refusal := s.protectedRefWriteRefusal(ctx, repo, stor, command.Name, kind, command.New); refusal != "" {
			status.status = gitStatusLine(refusal)
			continue
		}
		placeholder, err := s.secretScanningPushProtectionPlaceholderForRef(repo, stor, command.Name, command.New)
		if err != nil {
			return err
		}
		if placeholder != nil {
			status.status = gitSecretScanningPushRefusal
			outcome.messagef("error: GH013: Repository rule violations found for %s.", command.Name)
			outcome.messagef("error: Push cannot contain secrets: %s.", placeholder.TokenType)
			outcome.messagef("error: Bypass this block with POST /repos/%s/secret-scanning/push-protection-bypasses using placeholder %s, or see %s",
				repo.FullName, placeholder.ID, secretScanningPushProtectionDocs)
		}
	}
	return nil
}

// gitPushPreconditionRefusal checks a command's asserted old object id against
// the reference this server holds. The client derived that id from an
// advertisement it may have read a long time ago, so a mismatch means its view
// of the remote is stale — applying the command anyway would silently discard
// whatever moved the reference in the meantime, which is the lost update the
// compare-and-set write below exists to prevent. Reporting it here rather than
// letting the write fail gives the client git's own answer instead of an opaque
// storage error.
func gitPushPreconditionRefusal(stor storer.Storer, command *packp.Command) string {
	current, err := stor.Reference(command.Name)
	if errors.Is(err, plumbing.ErrReferenceNotFound) {
		if command.Old.IsZero() {
			return ""
		}
		return gitStalePushCommand
	}
	if err != nil {
		return gitStatusLine(fmt.Sprintf("cannot read %s: %s", command.Name, err))
	}
	if current.Type() != plumbing.HashReference || current.Hash() != command.Old {
		return gitStalePushCommand
	}
	return ""
}

// refuseGitPushAtomically turns a partially refused push into a wholly refused
// one. A client that negotiated atomic is entitled to assume that a report with
// any refusal in it left the remote exactly as it was, so the commands that
// would otherwise have been applied are refused too rather than being applied
// behind a refusal the pusher will read as "nothing happened".
func refuseGitPushAtomically(statuses []*gitPushStatus) {
	refused := false
	for _, status := range statuses {
		if status.status != "" {
			refused = true
			break
		}
	}
	if !refused {
		return
	}
	for _, status := range statuses {
		if status.status == "" {
			status.status = gitAtomicPushFailure
		}
	}
}

// applyGitPushCommands writes the references the verdicts allow.
//
// Every write is a compare-and-set against the object id the command asserted,
// so a reference the REST lane moved between the decision above and the write
// here is not overwritten. Under atomic that failure is not survivable — the
// pusher was promised all or nothing — so the references already written are
// put back and the whole push is reported refused.
func (s *Server) applyGitPushCommands(stor storer.Storer, outcome *gitReceivePackOutcome, atomicPush bool) error {
	for _, status := range outcome.report.statuses {
		if status.status != "" {
			continue
		}
		if err := applyPushCommandAtomic(stor, status.command); err != nil {
			status.status = gitStatusLine("failed to update ref: " + err.Error())
			if !atomicPush {
				continue
			}
			s.rollbackGitPushCommands(stor, outcome.report.statuses)
			refuseGitPushAtomically(outcome.report.statuses)
			outcome.applied = nil
			return nil
		}
		status.updated = true
		outcome.applied = append(outcome.applied, status.command)
	}
	return nil
}

// rollbackGitPushCommands undoes the references an atomic push had already
// written before one of its commands failed. Each undo is itself a
// compare-and-set against the value this push wrote, so a reference something
// else has since moved is left alone rather than being dragged backwards.
func (s *Server) rollbackGitPushCommands(stor storer.Storer, statuses []*gitPushStatus) {
	for _, status := range statuses {
		if !status.updated {
			continue
		}
		status.updated = false
		if err := revertPushCommandAtomic(stor, status.command); err != nil {
			s.logger.Error().Err(err).Str("ref", status.requested.String()).
				Msg("could not roll back a reference an atomic push had already updated")
		}
	}
}

// ingestPushedObjects stores the objects a receive-pack request carries. A push
// that only deletes references carries none.
func ingestPushedObjects(stor storer.Storer, pack io.Reader) error {
	if pack == nil {
		return nil
	}
	if err := packfile.UpdateObjectStorage(stor, pack); err != nil {
		return fmt.Errorf("store pushed objects: %w", err)
	}
	return nil
}

// gitRefLifecycleStorer is the compare-and-set reference lifecycle a push needs:
// creating a reference only if it is absent and removing one only if it still
// holds the value the pusher saw.
type gitRefLifecycleStorer interface {
	CreateReference(*plumbing.Reference) error
	RemoveReferenceCAS(*plumbing.Reference) error
}

func applyPushCommandAtomic(stor storer.Storer, command *packp.Command) error {
	atomic, ok := stor.(gitRefLifecycleStorer)
	if !ok {
		return fmt.Errorf("git storer for %s has no atomic ref lifecycle support", command.Name)
	}
	switch command.Action() {
	case packp.Create:
		return atomic.CreateReference(plumbing.NewHashReference(command.Name, command.New))
	case packp.Update:
		old := plumbing.NewHashReference(command.Name, command.Old)
		next := plumbing.NewHashReference(command.Name, command.New)
		return stor.CheckAndSetReference(next, old)
	case packp.Delete:
		old := plumbing.NewHashReference(command.Name, command.Old)
		return atomic.RemoveReferenceCAS(old)
	default:
		return fmt.Errorf("invalid ref update for %s", command.Name)
	}
}

// revertPushCommandAtomic is applyPushCommandAtomic run backwards: it restores
// the value a command moved a reference away from.
func revertPushCommandAtomic(stor storer.Storer, command *packp.Command) error {
	atomic, ok := stor.(gitRefLifecycleStorer)
	if !ok {
		return fmt.Errorf("git storer for %s has no atomic ref lifecycle support", command.Name)
	}
	switch command.Action() {
	case packp.Create:
		return atomic.RemoveReferenceCAS(plumbing.NewHashReference(command.Name, command.New))
	case packp.Update:
		old := plumbing.NewHashReference(command.Name, command.Old)
		next := plumbing.NewHashReference(command.Name, command.New)
		return stor.CheckAndSetReference(old, next)
	case packp.Delete:
		return atomic.CreateReference(plumbing.NewHashReference(command.Name, command.Old))
	default:
		return fmt.Errorf("invalid ref update for %s", command.Name)
	}
}

// classifyPushedRefWrite names the allowance one pushed command needs. The
// comparison is against the ref as the server holds it, not against the old
// value the client asserted.
func classifyPushedRefWrite(stor storer.Storer, command *packp.Command) (refWriteKind, error) {
	if command.New.IsZero() {
		return refDeletion, nil
	}
	current, err := stor.Reference(command.Name)
	if errors.Is(err, plumbing.ErrReferenceNotFound) {
		return refCreation, nil
	}
	if err != nil {
		return refForcePush, fmt.Errorf("read ref %s: %w", command.Name, err)
	}
	fastForward, err := refUpdateIsFastForward(stor, current.Hash(), command.New)
	if err != nil {
		return refForcePush, err
	}
	if fastForward {
		return refFastForward, nil
	}
	return refForcePush, nil
}

// writeGitReceivePackResponse writes the answer to a push.
//
// With side-band-64k the whole answer is multiplexed: the server's messages go
// out on band 2 and the report on band 1 of the same stream, so a pusher sees
// why a reference was refused beside the refusal rather than after it, and the
// stream is terminated by a flush-pkt of its own. Without it there is no band
// to put a message on, so only the report is written — and a client that asked
// for no report at all is answered with nothing, which is what it asked for.
func writeGitReceivePackResponse(out io.Writer, request *gitReceiveRequest, outcome *gitReceivePackOutcome) error {
	if !request.sideband {
		if !request.reportsStatus() {
			return nil
		}
		return outcome.report.encode(out)
	}
	mux := sideband.NewMuxer(sideband.Sideband64k, out)
	for _, message := range outcome.messages {
		if _, err := mux.WriteChannel(sideband.ProgressMessage, []byte(message)); err != nil {
			return err
		}
	}
	if request.reportsStatus() {
		var report bytes.Buffer
		if err := outcome.report.encode(&report); err != nil {
			return err
		}
		if _, err := mux.WriteChannel(sideband.PackData, report.Bytes()); err != nil {
			return err
		}
	}
	return pktline.NewEncoder(out).Flush()
}

// afterGitReceivePack is the bookkeeping a landed push owes the rest of the
// server: the repository's push timestamp, a HEAD that still points at the
// default branch, the secret-scanning sweep of what was pushed, and the
// activity, webhook and pull-request events every committed ref update raises.
func (s *Server) afterGitReceivePack(repo *store.Repo, user *store.User, applied []*packp.Command, baseURL string) {
	owner, _ := splitRepoPath("/" + repo.FullName)
	stor := s.resolveGitRepo(owner, repo.Name)
	s.store.UpdateRepo(owner, repo.Name, func(updated *store.Repo) {
		updated.PushedAt = updated.UpdatedAt
	})
	s.repairGitHead(owner, repo, stor)
	for _, command := range applied {
		if !command.New.IsZero() {
			if err := s.scanRefForSecretScanning(repo, stor, command.Name, command.New, baseURL); err != nil {
				s.logger.Error().Err(err).Str("repo", repo.FullName).Str("ref", command.Name.String()).
					Msg("could not scan a pushed ref for secrets")
			}
		}
		s.afterCommittedRefUpdate(repo, user, command.Name.String(), command.Old.String(), command.New.String(), baseURL)
	}
}

// repairGitHead re-points a repository's git HEAD after a push.
//
// HEAD must be a symbolic reference to the default branch, because that is what
// a clone checks out. Re-pointing it on every push also heals a repository
// whose HEAD drifted — a repository restored from storage that never had one,
// or one an older write path left detached at a commit.
func (s *Server) repairGitHead(owner string, repo *store.Repo, stor storer.Storer) {
	if stor == nil || repo == nil {
		return
	}
	setHead := func(branch string) {
		if err := store.SetGitHeadBranch(stor, branch); err != nil {
			s.logger.Error().Err(err).Str("repo", repo.FullName).Str("branch", branch).
				Msg("could not point git HEAD at the default branch")
		}
	}
	if repo.DefaultBranch != "" {
		if _, err := stor.Reference(plumbing.NewBranchReferenceName(repo.DefaultBranch)); err == nil {
			setHead(repo.DefaultBranch)
			return
		}
	}
	// The recorded default branch carries no commits. Adopt the first
	// conventional branch that does, matching github, where the first branch
	// pushed to an empty repository becomes its default.
	for _, branch := range []string{"main", "master"} {
		if _, err := stor.Reference(plumbing.NewBranchReferenceName(branch)); err != nil {
			continue
		}
		setHead(branch)
		s.store.UpdateRepo(owner, repo.Name, func(updated *store.Repo) { updated.DefaultBranch = branch })
		return
	}
}
