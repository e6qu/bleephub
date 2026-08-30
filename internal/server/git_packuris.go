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

// packfile-uris: answer a fetch with the address of a stored pack instead of
// its bytes. A stored pack is a correct answer under the same containment +
// disjointness test git_packreuse.go uses for pack reuse. The answer splits in
// two: the packfile-uris section names the packs the client fetches itself, and
// the packfile section carries the objects those packs do not supply.
//
// The split happens only when the client sends the packfile-uris argument, and
// the offer is recomputed per request over immutable packs, so a client that
// cannot fetch a URI can simply retry the fetch.
//
// AUTHORIZATION: a presigned URL is a bearer credential the object store honors
// without consulting bleephub, so one is minted only behind the read gate. Both
// transports pass it before a fetch command is decoded — smart HTTP in
// authorizeGitHTTP (PermRead on ScopeContents before handleGitUploadPack), SSH
// in the same check before serveGitProtocolV2. Nothing here is reachable
// otherwise.

// gitPackURIExpiry is how long a presigned pack URL stays usable. It must
// outlast the response (git fetches URIs after reading the reply — remainder
// stream, retries, slow link) but not much more: it is a bearer credential the
// object store honors even after the caller's session ends or access is
// revoked. Ten minutes covers an ordinary fetch.
const gitPackURIExpiry = 10 * time.Minute

// gitPackURIObjectIDLength is the width of the object id opening each
// packfile-uris line: the packfile's name, which index-pack reports for the
// downloaded bytes.
const gitPackURIObjectIDLength = 2 * len(plumbing.ZeroHash)

// gitPackURI is one packfile-uris line: the packfile name and its URL.
type gitPackURI struct {
	id  string
	uri string
}

// gitPackURIOffer is a fetch answered as a set of URIs plus a remainder.
type gitPackURIOffer struct {
	uris []gitPackURI
	plan *gitPackPlan
	// offloaded are the stored packs the URIs name; the remainder does not
	// repeat their objects.
	offloaded []*gitStoredPack
}

// remainder lists the plan objects no offloaded pack carries, in plan order.
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

// gitParsePackURIProtocols reads a packfile-uris argument: the transfer
// protocols the client will fetch a packfile over.
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

// gitPackURIOffload returns an offer when this fetch is answered with URIs, and
// nil otherwise: nothing is offered unless the client asked, the packs live in
// object storage (where a presigned GET exists), and they pass the same
// containment/disjointness test that governs pack reuse.
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
		// Record as offloaded only once it has a URI: the remainder is computed
		// from this list, so a pack here without an address would drop its
		// objects from both halves of the answer.
		offer.offloaded = append(offer.offloaded, pack)
	}
	if len(offer.uris) == 0 {
		return nil, nil
	}
	return offer, nil
}

// gitPackObjectID reads the object id from a packfile's base name — the id
// index-pack reports for the downloaded bytes. A name that is not "pack-"
// followed by lowercase-hex names a file we cannot promise that about, so it is
// carried as objects instead.
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

// gitPackURIProtocolAllowed reports whether a URL's scheme is one the client
// listed as fetchable.
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

// writeGitPackURIsSection writes the packfile-uris section: one "name url" line
// per URI.
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

// sendGitOffloadedPackfile writes the packfile section carrying only the
// objects the offloaded packs do not. An empty packfile is still sent (the
// protocol makes this section mandatory), e.g. a full clone of a compacted repo.
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

// gitPackURIFetchArgument is both the fetch argument the client sends and the
// capability this server advertises.
const gitPackURIFetchArgument = "packfile-uris"

// gitPackOffloadSupported reports whether stored packs are URL-addressable at
// all. Only object storage is (a presigned GET is its property); memory and
// local-directory backends have no such address, so neither packfile-uris nor
// bundle-uri is advertised there.
func gitPackOffloadSupported() bool {
	return gitstore.IsS3GitStorage()
}
