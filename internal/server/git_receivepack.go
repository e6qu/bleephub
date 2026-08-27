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

// The server side of git-receive-pack. Both transports — smart HTTP and SSH —
// decode with decodeGitReceiveRequest, decide with applyGitReceivePack, and
// answer with writeGitReceivePackResponse, so a pusher sees the same protocol
// on either URL scheme.
//
// Built on go-git *plumbing* rather than its server transport: that transport
// has no atomic transaction, push options, side band, or report-status-v2, and
// its decoder fails a push from a clone with two shallow boundaries. Nothing
// here touches a filesystem or shells out to git, so it runs against any
// storer.Storer, including the S3-backed one.

// capabilityReportStatusV2 is the richer per-ref report. packp has no constant
// for it; capability.List carries this unknown no-argument capability verbatim,
// which is the wire spelling the server advertises and the client echoes.
const capabilityReportStatusV2 = capability.Capability("report-status-v2")

// maxGitPushOptions and maxGitPushOptionBytes bound the push-option list. The
// section is read before the packfile from a client that has only proven write
// access, so an unbounded list is attacker-chosen memory. Exceeding the ceiling
// refuses the push rather than truncating it, since a hook acting on a
// truncated option list would act on something the pusher never sent.
const (
	maxGitPushOptions     = 64
	maxGitPushOptionBytes = 8 << 10
)

// git's own per-ref status wordings.
const gitPushUnpackerError = "n/a (unpacker error)" // packfile could not be stored
const gitAtomicPushFailure = "atomic push failure"  // collateral refusal under atomic
const gitStalePushCommand = "fetch first"           // asserted old-oid does not match
const gitSecretScanningPushRefusal = "push declined due to repository rule violations"

// setGitReceivePackCapabilities declares what this receive-pack honours; every
// entry is backed by working code in this file.
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

// gitReceivePackAdvertisement builds the receive-pack ref advertisement. A
// repository with no references is advertised as git does: no reference lines,
// capabilities on the zero-id "capabilities^{}" sentinel.
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

// gitReceiveRequest is one decoded reference-update request.
type gitReceiveRequest struct {
	capabilities *capability.List
	commands     []*packp.Command

	// shallow is the truncated-clone boundary declared before the commands.
	// This server holds complete history, so it changes nothing it decides, but
	// it is part of the grammar and must be read off the wire first.
	shallow []plumbing.Hash

	// pushOptions is the `git push -o` list, echoed back like a hook's output.
	pushOptions []string

	// packfile is the trailing object stream, or nil for a delete-only push.
	packfile io.Reader

	reportStatus          bool
	reportStatusV2        bool
	sideband              bool
	quiet                 bool
	atomic                bool
	pushOptionsNegotiated bool
}

// reportsStatus reports whether the client asked for a per-ref verdict. Without
// it a refusal has to fail the whole push through the transport instead.
func (r *gitReceiveRequest) reportsStatus() bool {
	return r.reportStatus || r.reportStatusV2
}

// deleteOnly reports whether every command removes a reference — the protocol's
// rule for whether a packfile follows the command list. Waiting for objects
// that never come would hang the transport.
func (r *gitReceiveRequest) deleteOnly() bool {
	for _, command := range r.commands {
		if !command.New.IsZero() {
			return false
		}
	}
	return true
}

// decodeGitReceiveRequest reads a reference-update request: shallow boundary,
// command list with echoed capabilities, push options, and packfile position.
//
// packp's own decoder models the shallow boundary as one optional hash and has
// no notion of the push-option section, so it fails a two-boundary clone and
// feeds push options to the packfile parser as garbage. This grammar is git's.
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

// parseGitPushCommand reads one "<old-oid> <new-oid> <refname>" line.
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

// decodeGitPushOptions reads the option section, enforcing the ceilings above.
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

// applyGitReceiveCapabilities turns the echoed capability list into the options
// the rest of the exchange reads, and refuses any capability never advertised.
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

// refusedPushError is a rule refusal on a connection with no report-status
// channel; the whole push fails rather than reporting success for a ref that
// did not move.
type refusedPushError struct{ reason string }

func (e *refusedPushError) Error() string { return e.reason }

// gitPushStatus is one command's verdict.
type gitPushStatus struct {
	command *packp.Command
	// requested is the ref name the client named; the "ok"/"ng" line carries it
	// so the client can match the verdict to its command.
	requested plumbing.ReferenceName
	// applied is the ref the server actually moved, reported as "option refname"
	// so the client is told the truth when the two differ.
	applied plumbing.ReferenceName
	// status is the refusal, or "" on success.
	status string
	// forced is set when the update discards reachable commits ("forced update").
	forced bool
	// old and new are the ids the ref moved between, as the server holds them.
	old, new plumbing.Hash
	// updated is set once the ref is written, so rollback knows what to undo and
	// only applied commands raise push events.
	updated bool
}

// gitPushReport is the report-status (or report-status-v2) message.
type gitPushReport struct {
	unpackStatus string
	statuses     []*gitPushStatus
	v2           bool
}

// encode writes the report. The v2 option lines are legal only after an "ok"
// line, so a refused command carries its reason on the "ng" line alone.
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

// gitReceivePackOutcome is everything a finished push produced.
type gitReceivePackOutcome struct {
	report   *gitPushReport
	messages []string
	applied  []*packp.Command
}

// messagef adds one line to the message band. git prints band 2 line by line,
// so every message ends in a newline.
func (o *gitReceivePackOutcome) messagef(format string, args ...any) {
	o.messages = append(o.messages, gitStatusLine(fmt.Sprintf(format, args...))+"\n")
}

// gitStatusLine flattens text to a single line: an embedded newline would end
// the pkt-line status or band 2 message early and corrupt the report.
func gitStatusLine(text string) string {
	return strings.Join(strings.Fields(text), " ")
}

// applyGitReceivePack is the write half of both git transports: ingest the
// pushed objects, decide every command before applying any, apply the ones the
// rules allow, and build the report.
//
// Objects are ingested first because whether an update discards commits — what
// force-push protection and "forced update" turn on — is only answerable once
// the pushed commits are readable.
func (s *Server) applyGitReceivePack(ctx context.Context, target *gitTarget, request *gitReceiveRequest) (*gitReceivePackOutcome, error) {
	stor := target.stor
	outcome := &gitReceivePackOutcome{report: &gitPushReport{unpackStatus: "ok", v2: request.reportStatusV2}}
	// Echo the options back like a hook's output, so the pusher sees the server
	// read exactly what it sent.
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
	// Pack the loose objects this push wrote; a delete-only or objectless push
	// wrote nothing. See git_compaction.go for why this does not delay the
	// report the caller is about to write.
	if request.packfile != nil {
		s.scheduleGitCompaction(target.storageName, stor)
	}
	return outcome, nil
}

// decideGitPushCommands gives every command its verdict without touching a
// reference. Deciding the whole list up front is what makes an atomic push
// possible: the refusal must be known before the first reference moves.
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
		// A wiki has neither branch protection nor secret scanning on github;
		// deciding a wiki push against the repository's rules would refuse
		// pushes github accepts and misattribute wiki text to repo alerts.
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

// gitPushPreconditionRefusal checks a command's asserted old-oid against the
// reference the server holds. A mismatch means the client's view is stale;
// applying anyway would discard whatever moved the ref since (the lost update
// the CAS write also guards against). Reporting here gives git's own answer
// rather than an opaque storage error.
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
// one: a client that negotiated atomic may assume any refusal left the remote
// untouched, so the surviving commands are refused too.
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
// Every write is a CAS against the command's asserted old-oid, so a reference
// the REST lane moved between decision and write is not overwritten. Under
// atomic such a failure rolls back the references already written and reports
// the whole push refused.
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

// rollbackGitPushCommands undoes references an atomic push already wrote before
// a later command failed. Each undo is a CAS against the value this push wrote,
// so a reference something else has since moved is left alone.
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

// ingestPushedObjects stores the objects a receive-pack request carries; a
// delete-only push carries none.
func ingestPushedObjects(stor storer.Storer, pack io.Reader) error {
	if pack == nil {
		return nil
	}
	if err := packfile.UpdateObjectStorage(stor, pack); err != nil {
		return fmt.Errorf("store pushed objects: %w", err)
	}
	return nil
}

// gitRefLifecycleStorer is the CAS reference lifecycle a push needs: create
// only if absent, remove only if the ref still holds the value the pusher saw.
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

// revertPushCommandAtomic is applyPushCommandAtomic run backwards.
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

// classifyPushedRefWrite names the allowance a pushed command needs, comparing
// against the ref as the server holds it, not the client's asserted old value.
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
// With side-band-64k the answer is multiplexed (messages on band 2, report on
// band 1) and terminated by a flush-pkt. Without it only the report is written;
// a client that asked for no report at all is answered with nothing.
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

// afterGitReceivePack does the bookkeeping a landed push owes: push timestamp,
// HEAD repair, the secret-scanning sweep, and the activity/webhook/PR events
// every committed ref update raises.
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

// repairGitHead re-points git HEAD at the default branch after a push. HEAD
// must be a symref to the default branch (what a clone checks out); doing it
// every push also heals a repository whose HEAD drifted or was never set.
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
	// The recorded default branch has no commits; adopt the first conventional
	// branch that does, as github makes the first branch pushed the default.
	for _, branch := range []string{"main", "master"} {
		if _, err := stor.Reference(plumbing.NewBranchReferenceName(branch)); err != nil {
			continue
		}
		setHead(branch)
		s.store.UpdateRepo(owner, repo.Name, func(updated *store.Repo) { updated.DefaultBranch = branch })
		return
	}
}
