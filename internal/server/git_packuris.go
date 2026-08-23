package bleephub

import (
	"context"
	"io"
	"net/url"
	"strings"
	"time"

	"github.com/e6qu/bleephub/internal/gitstore"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/format/packfile"
	"github.com/go-git/go-git/v5/plumbing/storer"
)

// packfile-uris: answering a fetch with the address of a packfile instead of
// its bytes.
//
// A stored pack is already an immutable object in the object store, and
// git_packreuse.go establishes when one of them is a correct answer to a
// particular fetch: every object it holds is in the plan, and no two chosen
// packs hold the same object. Those are the same two conditions a URI needs,
// because the client indexes whatever it downloads into its own object
// database — so a pack it fetches by URI delivers exactly the objects a pack
// copied onto the wire would have delivered. The difference is only which
// process moves the bytes.
//
// The answer therefore splits in two. The packfile-uris section names the
// stored packs the client fetches itself, and the packfile section that follows
// carries the objects those packs do not supply, which for a full clone of a
// compacted repository is nothing at all.
//
// The split is only ever made at the client's invitation. A client that did not
// send the packfile-uris argument is never sent the section, and is answered
// with every object it asked for the way it always was. A client that did send
// it and then cannot fetch a URI it was given ends the fetch without a complete
// object graph and has to run it again — which it can, because the offer is
// recomputed for every request and the packs it names are immutable: the same
// fetch is answerable as many times as it is asked.
//
// AUTHORIZATION
//
// A presigned URL carries its own permission: whoever holds it can GET that one
// key until it expires, and the object store never consults bleephub. So the
// URL is a credential, and the only safe place to mint one is behind the gate
// that already decided the caller may read the repository. Both transports do
// exactly that before a fetch command is ever decoded — smart HTTP in
// authorizeGitHTTP, which requires store.PermRead on store.ScopeContents before
// handleGitUploadPack reads a byte of the body, and SSH in the same permission
// check before serveGitProtocolV2 is entered. Nothing below is reachable
// without having passed one of them.

// gitPackURIExpiry is how long a presigned pack URL stays usable.
//
// It has to outlast this response: git downloads the URIs after it has finished
// reading the fetch reply, so the window has to cover the remainder pack still
// being streamed, the client's own retries, and a slow link between the two.
// It must not outlast much more than that, because for its lifetime the URL is
// a bearer credential for one object that the object store will honour without
// asking this server anything — including after the caller's session ends, its
// token is revoked or its access to the repository is withdrawn. Ten minutes is
// well past what an ordinary fetch needs and far short of the lifetime of any
// credential the URL stands in for, so a copy that escapes — into a proxy log,
// a shell history, a crash dump — has stopped working while the incident that
// produced it is still the same incident.
const gitPackURIExpiry = 10 * time.Minute

// gitPackURIObjectIDLength is the width of the object id that opens each line
// of the packfile-uris section: the packfile's own name, which is what the
// client's index-pack reports for the bytes it downloaded.
const gitPackURIObjectIDLength = 2 * len(plumbing.ZeroHash)

// gitPackURI is one line of the packfile-uris section: the name of the packfile
// behind the URL, and the URL.
type gitPackURI struct {
	id  string
	uri string
}

// gitPackURIOffer is a fetch answered as a set of URIs plus a remainder.
type gitPackURIOffer struct {
	uris []gitPackURI
	plan *gitPackPlan
	// offloaded are the stored packs the URIs name. Their objects are the ones
	// the remainder pack does not repeat.
	offloaded []*gitStoredPack
}

// remainder lists the objects of the plan that no offloaded pack carries, in
// the plan's own order.
func (o *gitPackURIOffer) remainder() []plumbing.Hash {
	carried := map[plumbing.Hash]bool{}
	for _, pack := range o.offloaded {
		for object := range pack.objects {
			carried[object] = true
		}
	}
	remainder := make([]plumbing.Hash, 0, len(o.plan.objects))
	for _, id := range o.plan.objects {
		if !carried[id] {
			remainder = append(remainder, id)
		}
	}
	return remainder
}

// gitParsePackURIProtocols reads the value of a packfile-uris fetch argument:
// the transfer protocols the client is willing to fetch a packfile over.
func gitParsePackURIProtocols(value string) []string {
	var protocols []string
	for _, field := range strings.Split(value, ",") {
		field = strings.ToLower(strings.TrimSpace(field))
		if field == "" {
			continue
		}
		protocols = append(protocols, field)
	}
	return protocols
}

// gitPackURIOffload decides whether this fetch is answered with URIs, and
// returns the offer when it is.
//
// Nothing is offered unless the client asked, unless the repository's packs
// live somewhere a URL can address — object storage, where a presigned GET
// exists; a memory or local-directory backend has no such address and the
// answer is simply the ordinary packfile — and unless the stored packs pass the
// same containment and disjointness test that governs pack reuse.
func gitPackURIOffload(ctx context.Context, stor storer.Storer, request *gitUploadRequest, plan *gitPackPlan) (*gitPackURIOffer, error) {
	if len(request.packURIProtocols) == 0 {
		return nil, nil
	}
	packDir, addressable := gitPackDirOf(stor).(*gitstore.S3FS)
	if !addressable {
		return nil, nil
	}
	if len(plan.objects) == 0 {
		return nil, nil
	}
	offloadable, err := gitReusablePacks(packDir, plan)
	if err != nil {
		return nil, err
	}
	if len(offloadable) == 0 {
		return nil, nil
	}
	offer := &gitPackURIOffer{plan: plan}
	for _, pack := range offloadable {
		id, named := gitPackObjectID(pack.name)
		if !named {
			continue
		}
		signed, err := packDir.PresignedGetURL(ctx, packDir.Join(gitPackDirectory, pack.name+".pack"), gitPackURIExpiry)
		if err != nil {
			return nil, err
		}
		if !gitPackURIProtocolAllowed(signed, request.packURIProtocols) {
			return nil, nil
		}
		offer.uris = append(offer.uris, gitPackURI{id: id, uri: signed})
		// A pack only counts as offloaded once there is a URI for it, because
		// what the remainder owes is decided from this list: a pack recorded
		// here without an address would have its objects left out of both
		// halves of the answer.
		offer.offloaded = append(offer.offloaded, pack)
	}
	if len(offer.uris) == 0 {
		return nil, nil
	}
	return offer, nil
}

// gitPackObjectID reads the object id out of a packfile's base name.
//
// The name is what the client is told to expect from indexing the bytes it
// downloads — index-pack reports a pack by the checksum that closes it, which
// is the same id the file is named for. So a name that is not exactly "pack-"
// followed by an object id in lowercase hex names a file this server cannot
// make that promise about, and it is carried as objects instead.
func gitPackObjectID(name string) (string, bool) {
	id, named := strings.CutPrefix(name, "pack-")
	if !named || len(id) != gitPackURIObjectIDLength {
		return "", false
	}
	for _, character := range id {
		if (character < '0' || character > '9') && (character < 'a' || character > 'f') {
			return "", false
		}
	}
	return id, true
}

// gitPackURIProtocolAllowed reports whether a URL is one the client said it can
// fetch. The client lists the transfer protocols it supports, and a URL in any
// other protocol is one it would have to refuse, so it is never offered.
func gitPackURIProtocolAllowed(signed string, protocols []string) bool {
	parsed, err := url.Parse(signed)
	if err != nil {
		return false
	}
	scheme := strings.ToLower(parsed.Scheme)
	for _, protocol := range protocols {
		if protocol == scheme {
			return true
		}
	}
	return false
}

// writeGitPackURIsSection writes the packfile-uris section: one line per URI,
// each the name of the packfile behind it followed by the URL.
func writeGitPackURIsSection(writer *gitV2Writer, offer *gitPackURIOffer) error {
	if err := writer.line("packfile-uris\n"); err != nil {
		return err
	}
	for _, uri := range offer.uris {
		if err := writer.line("%s %s\n", uri.id, uri.uri); err != nil {
			return err
		}
	}
	return writer.delim()
}

// sendGitOffloadedPackfile writes the packfile section of an answer whose
// stored packs the client is fetching for itself: the objects those packs do
// not carry, and nothing else.
//
// A full clone of a compacted repository owes nothing here, and the empty
// packfile that results — a header stating no entries and a checksum over it —
// is sent all the same, because the protocol makes the packfile section the end
// of every fetch response rather than an optional one.
func sendGitOffloadedPackfile(stor storer.Storer, out io.Writer, request *gitUploadRequest, offer *gitPackURIOffer, mode gitSidebandMode) error {
	band := newGitBandWriter(out, mode, !request.noProgress)
	remainder := offer.remainder()
	offloaded := len(offer.plan.objects) - len(remainder)
	band.progressf("Enumerating objects: %d, done.\n", len(offer.plan.objects))
	for _, uri := range offer.uris {
		band.progressf("Offering pack-%s for download\n", uri.id)
	}
	encoder := packfile.NewEncoder(band.pack(), stor, false)
	if _, err := encoder.Encode(remainder, gitPackWindow); err != nil {
		return band.fatal(err)
	}
	band.progressf("Compressing objects: 100%% (%d/%d), done.\n", len(remainder), len(remainder))
	band.progressf("Total %d, %d of them in %d packs you fetch yourself\n",
		len(offer.plan.objects), offloaded, len(offer.uris))
	return band.finish()
}

// gitPackURIFetchArgument is the fetch argument a client sends to say it will
// fetch packfiles itself, and the capability this server advertises to invite
// it.
const gitPackURIFetchArgument = "packfile-uris"

// gitPackOffloadSupported reports whether this deployment can address its
// stored packs and bundles with a URL at all. Only object storage can: a
// presigned GET is a property of the object store, and a repository kept in
// memory or in a local directory has no address a client could fetch from — so
// neither packfile-uris nor bundle-uri is advertised there.
func gitPackOffloadSupported() bool {
	return gitstore.IsS3GitStorage()
}
