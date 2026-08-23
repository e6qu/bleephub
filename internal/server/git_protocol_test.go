package bleephub

import (
	"bufio"
	"bytes"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/e6qu/bleephub/internal/store"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/format/packfile"
	"github.com/go-git/go-git/v5/plumbing/format/pktline"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/plumbing/protocol/packp/sideband"
	"github.com/go-git/go-git/v5/plumbing/storer"
)

// The tests in this file drive the upload-pack wire format directly, without a
// git client, so they can ask for one capability at a time and then look inside
// the packfile that comes back. The end-to-end matrix in
// git_protocol_cli_test.go proves real git is happy with the same answers.

// gitPktScript builds a pkt-line request by hand, so a test can ask for a
// capability combination no git client would pick on its own.
type gitPktScript struct{ out bytes.Buffer }

func (s *gitPktScript) linef(format string, args ...any) *gitPktScript {
	if err := pktline.NewEncoder(&s.out).Encodef(format, args...); err != nil {
		panic(err)
	}
	return s
}

func (s *gitPktScript) flush() *gitPktScript {
	if err := pktline.NewEncoder(&s.out).Flush(); err != nil {
		panic(err)
	}
	return s
}

func (s *gitPktScript) delim() *gitPktScript {
	s.out.Write(gitPktDelimLine)
	return s
}

// postGitUploadPack sends a handcrafted request to the smart-HTTP upload-pack
// endpoint and returns the whole reply.
func postGitUploadPack(t *testing.T, srv *isolatedServer, repoName string, script *gitPktScript, header map[string]string) []byte {
	t.Helper()
	request, err := http.NewRequest(http.MethodPost, srv.baseURL+"/admin/"+repoName+".git/git-upload-pack", bytes.NewReader(script.out.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "token "+defaultToken)
	request.Header.Set("Content-Type", "application/x-git-upload-pack-request")
	for name, value := range header {
		request.Header.Set(name, value)
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusOK {
		t.Fatalf("upload-pack POST = %d: %s", response.StatusCode, body)
	}
	return body
}

// splitGitUploadPackResponse separates the pkt-line half of a protocol v0
// reply — the shallow update and the ACK/NAK negotiation — from whatever
// follows it, which is either a raw packfile or the multiplexed stream.
//
// The two halves are told apart by looking at the byte after a pkt-line length
// prefix: a raw packfile opens with "PACK", a multiplexed one opens with a band
// number, and every control line opens with printable text.
func splitGitUploadPackResponse(t *testing.T, body []byte) (control []string, rest []byte) {
	t.Helper()
	reader := bufio.NewReader(bytes.NewReader(body))
	pkt := newGitPktReader(reader)
	for {
		head, err := reader.Peek(5)
		if err != nil || string(head[:4]) == "PACK" || head[4] <= 3 {
			break
		}
		line, kind, err := pkt.next()
		if err != nil {
			t.Fatalf("decode upload-pack reply: %v", err)
		}
		if kind != gitPktData {
			control = append(control, "")
			continue
		}
		control = append(control, string(line))
	}
	remainder, err := io.ReadAll(reader)
	if err != nil {
		t.Fatalf("read packfile half: %v", err)
	}
	return control, remainder
}

// demuxGitSideband splits a multiplexed reply into its packfile and its
// progress text, and fails the test if the server reported a fatal error on
// band 3.
func demuxGitSideband(t *testing.T, stream []byte) (pack []byte, progress string) {
	t.Helper()
	pack, progress, err := tryDemuxGitSideband(stream)
	if err != nil {
		t.Fatalf("demultiplex reply: %v", err)
	}
	return pack, progress
}

func tryDemuxGitSideband(stream []byte) (pack []byte, progress string, err error) {
	var progressOut bytes.Buffer
	demuxer := sideband.NewDemuxer(sideband.Sideband64k, bytes.NewReader(stream))
	demuxer.Progress = &progressOut
	var packOut bytes.Buffer
	_, err = packOut.ReadFrom(demuxer)
	return packOut.Bytes(), progressOut.String(), err
}

// scanGitPack walks a packfile, verifying its trailing checksum, and reports
// how many objects of each type it carries and which object ids its reference
// deltas are based on.
func scanGitPack(t *testing.T, pack []byte) (counts map[plumbing.ObjectType]int, deltaBases []plumbing.Hash) {
	t.Helper()
	scanner := packfile.NewScanner(bytes.NewReader(pack))
	_, total, err := scanner.Header()
	if err != nil {
		t.Fatalf("read packfile header: %v", err)
	}
	counts = map[plumbing.ObjectType]int{}
	for i := uint32(0); i < total; i++ {
		header, err := scanner.NextObjectHeader()
		if err != nil {
			t.Fatalf("read packfile entry %d: %v", i, err)
		}
		counts[header.Type]++
		if header.Type == plumbing.REFDeltaObject {
			deltaBases = append(deltaBases, header.Reference)
		}
		if _, _, err := scanner.NextObject(io.Discard); err != nil {
			t.Fatalf("read packfile entry %d body: %v", i, err)
		}
	}
	if _, err := scanner.Checksum(); err != nil {
		t.Fatalf("packfile checksum: %v", err)
	}
	return counts, deltaBases
}

// gitSeedTip is the tip of the history seedGitShallowRepo builds, and gitSeedNth
// is the commit n steps behind it.
func gitSeedTip(t *testing.T, srv *isolatedServer, repoName string) plumbing.Hash {
	t.Helper()
	return gitSeedNth(t, srv, repoName, 0)
}

func gitSeedNth(t *testing.T, srv *isolatedServer, repoName string, back int) plumbing.Hash {
	t.Helper()
	stor := srv.store.GetGitStorage("admin", repoName)
	ref, err := storer.ResolveReference(stor, plumbing.NewBranchReferenceName("main"))
	if err != nil {
		t.Fatalf("resolve main: %v", err)
	}
	hash := ref.Hash()
	for i := 0; i < back; i++ {
		commit, err := object.GetCommit(stor, hash)
		if err != nil {
			t.Fatal(err)
		}
		if len(commit.ParentHashes) == 0 {
			t.Fatalf("commit %s has no parent", hash)
		}
		hash = commit.ParentHashes[0]
	}
	return hash
}

// gitTreeOfCommit reads the tree a commit points at.
func gitTreeOfCommit(stor storer.Storer, hash plumbing.Hash) (*object.Tree, error) {
	commit, err := object.GetCommit(stor, hash)
	if err != nil {
		return nil, err
	}
	return commit.Tree()
}

// seedGitAnnotatedTag writes an annotated tag object and the reference that
// names it. The tagger time is fixed because the wall-clock gate forbids
// reading the real clock from a test.
func seedGitAnnotatedTag(t *testing.T, srv *isolatedServer, repoName, tagName string, target plumbing.Hash) plumbing.Hash {
	t.Helper()
	stor := srv.store.GetGitStorage("admin", repoName)
	tag := &object.Tag{
		Name:       tagName,
		Message:    "release " + tagName + "\n",
		TargetType: plumbing.CommitObject,
		Target:     target,
		Tagger: object.Signature{
			Name:  "Tag Fixture",
			Email: "tags@bleephub.invalid",
			When:  time.Date(2020, time.June, 1, 0, 0, 0, 0, time.UTC),
		},
	}
	hash, err := encodeTag(stor, tag)
	if err != nil {
		t.Fatalf("write the tag object: %v", err)
	}
	if err := stor.SetReference(plumbing.NewHashReference(plumbing.NewTagReferenceName(tagName), hash)); err != nil {
		t.Fatalf("write the tag reference: %v", err)
	}
	return hash
}

// TestGitUploadPackSidebandCarriesPackAndProgress proves the multiplexed reply:
// the packfile arrives on band 1 while the counting and compressing lines the
// user sees arrive on band 2 of the very same stream.
func TestGitUploadPackSidebandCarriesPackAndProgress(t *testing.T) {
	t.Parallel()
	srv := newIsolatedServer(t)
	const name = "sideband-progress"
	seedGitShallowRepo(t, srv.Server, name)
	tip := gitSeedTip(t, srv, name)

	script := (&gitPktScript{}).
		linef("want %s\x00side-band-64k ofs-delta agent=test\n", tip).
		flush().
		linef("done\n")
	control, rest := splitGitUploadPackResponse(t, postGitUploadPack(t, srv, name, script, nil))
	if len(control) == 0 || control[len(control)-1] != "NAK" {
		t.Fatalf("negotiation lines = %v, want it to end in NAK", control)
	}
	pack, progress := demuxGitSideband(t, rest)
	counts, _ := scanGitPack(t, pack)
	if counts[plumbing.CommitObject] != gitShallowHistoryCommits {
		t.Fatalf("pack carries %d commits, want %d", counts[plumbing.CommitObject], gitShallowHistoryCommits)
	}
	for _, want := range []string{"Enumerating objects:", "Counting objects:", "Compressing objects:", "Total "} {
		if !strings.Contains(progress, want) {
			t.Errorf("band 2 progress %q does not mention %q", progress, want)
		}
	}
}

// TestGitUploadPackNoProgressSilencesBandTwo pins the other half of the
// contract: a client that said no-progress gets the pack and nothing else.
func TestGitUploadPackNoProgressSilencesBandTwo(t *testing.T) {
	t.Parallel()
	srv := newIsolatedServer(t)
	const name = "sideband-quiet"
	seedGitShallowRepo(t, srv.Server, name)
	tip := gitSeedTip(t, srv, name)

	script := (&gitPktScript{}).
		linef("want %s\x00side-band-64k no-progress ofs-delta agent=test\n", tip).
		flush().
		linef("done\n")
	_, rest := splitGitUploadPackResponse(t, postGitUploadPack(t, srv, name, script, nil))
	pack, progress := demuxGitSideband(t, rest)
	if progress != "" {
		t.Fatalf("no-progress reply still carried band 2 text %q", progress)
	}
	if counts, _ := scanGitPack(t, pack); counts[plumbing.CommitObject] == 0 {
		t.Fatal("no-progress reply carried no commits")
	}
}

// TestGitUploadPackMultiAckDetailedNegotiates pins the negotiation a client
// uses to stop early: the object it already holds is acknowledged as common,
// the server says it is ready as soon as the wants are connected to that
// object, and the exchange closes with a final ACK rather than a bare NAK.
func TestGitUploadPackMultiAckDetailedNegotiates(t *testing.T) {
	t.Parallel()
	srv := newIsolatedServer(t)
	const name = "multi-ack"
	seedGitShallowRepo(t, srv.Server, name)
	tip := gitSeedTip(t, srv, name)
	held := gitSeedNth(t, srv, name, 2)

	script := (&gitPktScript{}).
		linef("want %s\x00multi_ack_detailed side-band-64k ofs-delta agent=test\n", tip).
		flush().
		linef("have %s\n", held).
		flush().
		linef("done\n")
	control, rest := splitGitUploadPackResponse(t, postGitUploadPack(t, srv, name, script, nil))
	want := []string{
		"ACK " + held.String() + " common",
		"ACK " + held.String() + " ready",
		"NAK",
		"ACK " + held.String(),
	}
	var got []string
	for _, line := range control {
		if line != "" {
			got = append(got, line)
		}
	}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("negotiation = %v, want %v", got, want)
	}
	pack, _ := demuxGitSideband(t, rest)
	counts, _ := scanGitPack(t, pack)
	if counts[plumbing.CommitObject] != 2 {
		t.Fatalf("incremental pack carries %d commits, want the 2 the client lacks", counts[plumbing.CommitObject])
	}
}

// TestGitUploadPackNoDoneSendsPackWithoutDone proves the round trip no-done
// saves: the client never says "done" and still gets its packfile.
func TestGitUploadPackNoDoneSendsPackWithoutDone(t *testing.T) {
	t.Parallel()
	srv := newIsolatedServer(t)
	const name = "no-done"
	seedGitShallowRepo(t, srv.Server, name)
	tip := gitSeedTip(t, srv, name)
	held := gitSeedNth(t, srv, name, 1)

	script := (&gitPktScript{}).
		linef("want %s\x00multi_ack_detailed no-done side-band-64k ofs-delta agent=test\n", tip).
		flush().
		linef("have %s\n", held).
		flush()
	control, rest := splitGitUploadPackResponse(t, postGitUploadPack(t, srv, name, script, nil))
	if control[len(control)-1] != "ACK "+held.String() {
		t.Fatalf("no-done negotiation = %v, want it to close with a bare ACK", control)
	}
	pack, _ := demuxGitSideband(t, rest)
	if counts, _ := scanGitPack(t, pack); counts[plumbing.CommitObject] != 1 {
		t.Fatalf("no-done pack carries %d commits, want 1", counts[plumbing.CommitObject])
	}
}

// TestGitUploadPackThinPackDeltasAgainstClientObjects proves the pack really is
// thin: it carries a reference delta whose base is an object the client proved
// it holds and which the pack itself does not contain.
func TestGitUploadPackThinPackDeltasAgainstClientObjects(t *testing.T) {
	t.Parallel()
	srv := newIsolatedServer(t)
	const name = "thin-pack"
	held, tip := seedGitThinPackRepo(t, srv, name)
	heldBlob := gitBlobAtPath(t, srv, name, held, "big.txt")

	script := (&gitPktScript{}).
		linef("want %s\x00thin-pack side-band-64k ofs-delta agent=test\n", tip).
		flush().
		linef("have %s\n", held).
		linef("done\n")
	_, rest := splitGitUploadPackResponse(t, postGitUploadPack(t, srv, name, script, nil))
	pack, _ := demuxGitSideband(t, rest)
	counts, bases := scanGitPack(t, pack)
	if counts[plumbing.REFDeltaObject] == 0 {
		t.Fatalf("thin-pack reply carried no reference deltas: %v", counts)
	}
	found := false
	for _, base := range bases {
		if base == heldBlob {
			found = true
		}
	}
	if !found {
		t.Fatalf("reference delta bases %v do not include the blob %s the client already holds", bases, heldBlob)
	}
	thinSize := len(pack)

	// Without thin-pack the same fetch must be self-contained, and pay for it.
	plain := (&gitPktScript{}).
		linef("want %s\x00side-band-64k ofs-delta agent=test\n", tip).
		flush().
		linef("have %s\n", held).
		linef("done\n")
	_, rest = splitGitUploadPackResponse(t, postGitUploadPack(t, srv, name, plain, nil))
	pack, _ = demuxGitSideband(t, rest)
	if _, bases := scanGitPack(t, pack); len(bases) != 0 {
		t.Fatalf("a fetch that did not ask for a thin pack still got external deltas %v", bases)
	}
	if thinSize >= len(pack) {
		t.Fatalf("the thin pack is %d bytes and the self-contained one %d; the delta saved nothing", thinSize, len(pack))
	}
}

// seedGitThinPackRepo builds a two-commit history whose file is large enough
// for a delta to beat sending it whole, and reports both commits.
func seedGitThinPackRepo(t *testing.T, srv *isolatedServer, name string) (older, newer plumbing.Hash) {
	t.Helper()
	admin := srv.store.LookupUserByLogin("admin")
	if srv.store.CreateRepo(admin, name, "thin pack fixture", false) == nil {
		t.Fatalf("create repo %s", name)
	}
	stor := srv.store.GetGitStorage("admin", name)
	signature := &object.Signature{
		Name:  "Thin Fixture",
		Email: "thin@bleephub.invalid",
		When:  time.Date(2020, time.January, 1, 0, 0, 0, 0, time.UTC),
	}
	body := strings.Repeat("the quick brown fox jumps over the lazy dog\n", 200)
	older, err := initRepoWithFiles(stor, "main", "base", map[string]string{"big.txt": body}, signature)
	if err != nil {
		t.Fatalf("seed the base commit: %v", err)
	}
	newer, err = createFileCommit(stor, "main", "big.txt", body+"one more line\n", "extend", signature)
	if err != nil {
		t.Fatalf("seed the second commit: %v", err)
	}
	if err := store.SetGitHeadBranch(stor, "main"); err != nil {
		t.Fatalf("point git HEAD at main: %v", err)
	}
	return older, newer
}

// gitBlobAtPath reports the blob a commit's tree holds at a path.
func gitBlobAtPath(t *testing.T, srv *isolatedServer, repoName string, commit plumbing.Hash, path string) plumbing.Hash {
	t.Helper()
	stor := srv.store.GetGitStorage("admin", repoName)
	tree, err := gitTreeOfCommit(stor, commit)
	if err != nil {
		t.Fatal(err)
	}
	entry, err := tree.FindEntry(path)
	if err != nil {
		t.Fatalf("find %s: %v", path, err)
	}
	return entry.Hash
}

// TestGitUploadPackFilterOmitsObjects walks the three partial-clone filters and
// then proves the object a filter omitted can still be fetched by name, which
// is what makes a partial clone usable.
func TestGitUploadPackFilterOmitsObjects(t *testing.T) {
	t.Parallel()
	srv := newIsolatedServer(t)
	const name = "partial-clone"
	seedGitShallowRepo(t, srv.Server, name)
	tip := gitSeedTip(t, srv, name)
	tipBlob := gitBlobAtPath(t, srv, name, tip, "f.txt")

	fetch := func(filter string) map[plumbing.ObjectType]int {
		t.Helper()
		script := (&gitPktScript{}).
			linef("want %s\x00filter side-band-64k ofs-delta agent=test\n", tip)
		if filter != "" {
			script.linef("filter %s\n", filter)
		}
		script.flush().linef("done\n")
		_, rest := splitGitUploadPackResponse(t, postGitUploadPack(t, srv, name, script, nil))
		pack, _ := demuxGitSideband(t, rest)
		counts, _ := scanGitPack(t, pack)
		return counts
	}

	full := fetch("")
	if full[plumbing.BlobObject] == 0 || full[plumbing.TreeObject] == 0 {
		t.Fatalf("unfiltered fetch is missing objects: %v", full)
	}
	blobNone := fetch("blob:none")
	if blobNone[plumbing.BlobObject] != 0 {
		t.Errorf("blob:none still sent %d blobs", blobNone[plumbing.BlobObject])
	}
	if blobNone[plumbing.TreeObject] != full[plumbing.TreeObject] {
		t.Errorf("blob:none changed the tree count: %d, want %d", blobNone[plumbing.TreeObject], full[plumbing.TreeObject])
	}
	// Every seeded blob is at least three bytes, so a one-byte ceiling omits
	// all of them while a very large one omits none.
	if small := fetch("blob:limit=1"); small[plumbing.BlobObject] != 0 {
		t.Errorf("blob:limit=1 still sent %d blobs", small[plumbing.BlobObject])
	}
	if large := fetch("blob:limit=1m"); large[plumbing.BlobObject] != full[plumbing.BlobObject] {
		t.Errorf("blob:limit=1m sent %d blobs, want %d", large[plumbing.BlobObject], full[plumbing.BlobObject])
	}
	treeZero := fetch("tree:0")
	if treeZero[plumbing.TreeObject] != 0 || treeZero[plumbing.BlobObject] != 0 {
		t.Errorf("tree:0 sent %d trees and %d blobs, want none of either", treeZero[plumbing.TreeObject], treeZero[plumbing.BlobObject])
	}
	if treeZero[plumbing.CommitObject] != full[plumbing.CommitObject] {
		t.Errorf("tree:0 changed the commit count: %d, want %d", treeZero[plumbing.CommitObject], full[plumbing.CommitObject])
	}

	// The lazy fetch a partial clone makes when it finally needs an omitted
	// blob: the object is named outright, and the filter must not swallow it.
	script := (&gitPktScript{}).
		linef("want %s\x00filter side-band-64k ofs-delta agent=test\n", tipBlob).
		linef("filter blob:none\n").
		flush().
		linef("done\n")
	_, rest := splitGitUploadPackResponse(t, postGitUploadPack(t, srv, name, script, nil))
	pack, _ := demuxGitSideband(t, rest)
	if counts, _ := scanGitPack(t, pack); counts[plumbing.BlobObject] != 1 {
		t.Fatalf("lazy fetch of a filtered blob returned %v, want exactly one blob", counts)
	}
}

// TestGitUploadPackRefusesAnUnknownFilter keeps a filter this server cannot
// honour from being silently ignored, which would hand the client a complete
// clone it did not ask for.
func TestGitUploadPackRefusesAnUnknownFilter(t *testing.T) {
	t.Parallel()
	srv := newIsolatedServer(t)
	const name = "bad-filter"
	seedGitShallowRepo(t, srv.Server, name)
	tip := gitSeedTip(t, srv, name)

	// The spec has to be one the server does not recognise at all. sparse:oid=
	// is deliberately not used here: it is now a supported filter, so a bad oid
	// is a *resolution* failure with its own message rather than an unknown
	// spec, and asserting on it here would stop testing this refusal.
	script := (&gitPktScript{}).
		linef("want %s\x00filter agent=test\n", tip).
		linef("filter nosuchfilter:7\n").
		flush().
		linef("done\n")
	body := postGitUploadPack(t, srv, name, script, nil)
	if !strings.Contains(string(body), "invalid filter-spec") {
		t.Fatalf("unknown filter answered with %q", body)
	}
}

// TestGitUploadPackRefusesASparseFilterItCannotResolve covers the other refusal
// the sparse filter can produce: a well-formed sparse:oid= naming a blob this
// repository does not hold. The client has to be told, rather than served a
// pack silently filtered by nothing.
func TestGitUploadPackRefusesASparseFilterItCannotResolve(t *testing.T) {
	t.Parallel()
	srv := newIsolatedServer(t)
	const name = "bad-sparse-filter"
	seedGitShallowRepo(t, srv.Server, name)
	tip := gitSeedTip(t, srv, name)

	script := (&gitPktScript{}).
		linef("want %s\x00filter agent=test\n", tip).
		linef("filter sparse:oid=deadbeef\n").
		flush().
		linef("done\n")
	body := postGitUploadPack(t, srv, name, script, nil)
	if !strings.Contains(string(body), "sparse") {
		t.Fatalf("unresolvable sparse filter answered with %q", body)
	}
}

// TestGitUploadPackRefusesAnUnknownWant keeps a want for an object this
// repository does not hold from ending as an opaque transport failure.
func TestGitUploadPackRefusesAnUnknownWant(t *testing.T) {
	t.Parallel()
	srv := newIsolatedServer(t)
	const name = "unknown-want"
	seedGitShallowRepo(t, srv.Server, name)

	missing := plumbing.NewHash("1111111111111111111111111111111111111111")
	script := (&gitPktScript{}).
		linef("want %s\x00agent=test\n", missing).
		flush().
		linef("done\n")
	body := postGitUploadPack(t, srv, name, script, nil)
	if !strings.Contains(string(body), "not our ref "+missing.String()) {
		t.Fatalf("a want for an unknown object answered with %q", body)
	}
}

// TestGitUploadPackIncludeTagAddsAnnotatedTags pins include-tag: a client
// fetching a branch also receives the annotated tags that point into it,
// without asking for them and without a second round trip.
func TestGitUploadPackIncludeTagAddsAnnotatedTags(t *testing.T) {
	t.Parallel()
	srv := newIsolatedServer(t)
	const name = "include-tag"
	seedGitShallowRepo(t, srv.Server, name)
	tip := gitSeedTip(t, srv, name)
	seedGitAnnotatedTag(t, srv, name, "v1", tip)

	withTag := (&gitPktScript{}).
		linef("want %s\x00include-tag side-band-64k ofs-delta agent=test\n", tip).
		flush().
		linef("done\n")
	_, rest := splitGitUploadPackResponse(t, postGitUploadPack(t, srv, name, withTag, nil))
	pack, _ := demuxGitSideband(t, rest)
	if counts, _ := scanGitPack(t, pack); counts[plumbing.TagObject] != 1 {
		t.Fatalf("include-tag fetch carried %d tag objects, want 1", counts[plumbing.TagObject])
	}

	withoutTag := (&gitPktScript{}).
		linef("want %s\x00side-band-64k ofs-delta agent=test\n", tip).
		flush().
		linef("done\n")
	_, rest = splitGitUploadPackResponse(t, postGitUploadPack(t, srv, name, withoutTag, nil))
	pack, _ = demuxGitSideband(t, rest)
	if counts, _ := scanGitPack(t, pack); counts[plumbing.TagObject] != 0 {
		t.Fatalf("a fetch that did not ask for tags carried %d of them", counts[plumbing.TagObject])
	}
}

// TestGitUploadPackFatalErrorTravelsOnBandThree is the reason side-band exists:
// a storage failure once pack bytes are already flowing has to reach the client
// as a message it can print, not as a stream that simply stops.
func TestGitUploadPackFatalErrorTravelsOnBandThree(t *testing.T) {
	t.Parallel()
	srv := newIsolatedServer(t)
	const name = "band-three"
	seedGitShallowRepo(t, srv.Server, name)
	tip := gitSeedTip(t, srv, name)
	held := gitSeedNth(t, srv, name, 1)
	stor := failingGitStorer{
		Storer: srv.store.GetGitStorage("admin", name),
		fail:   gitBlobAtPath(t, srv, name, tip, "f.txt"),
	}

	script := (&gitPktScript{}).
		linef("want %s\x00thin-pack side-band-64k ofs-delta agent=test\n", tip).
		flush().
		linef("have %s\n", held).
		linef("done\n")
	var reply bytes.Buffer
	_, err := serveGitUploadPack(t.Context(), stor, bufio.NewReader(bytes.NewReader(script.out.Bytes())), &reply)
	if err == nil {
		t.Fatal("a failing storer produced no error")
	}
	_, rest := splitGitUploadPackResponse(t, reply.Bytes())
	pack, _, demuxErr := tryDemuxGitSideband(rest)
	if demuxErr == nil || !strings.Contains(demuxErr.Error(), "simulated storage failure") {
		t.Fatalf("band 3 carried %v, want the storage failure", demuxErr)
	}
	if !bytes.HasPrefix(pack, []byte("PACK")) {
		t.Fatal("the failure did not happen mid-pack, so band 3 was not exercised where it matters")
	}
}

// failingGitStorer is a repository whose storage refuses one object, which is
// how a test reproduces a read that fails after the reply has begun.
type failingGitStorer struct {
	storer.Storer
	fail plumbing.Hash
}

func (f failingGitStorer) EncodedObject(objectType plumbing.ObjectType, hash plumbing.Hash) (plumbing.EncodedObject, error) {
	if hash == f.fail {
		return nil, errors.New("simulated storage failure")
	}
	return f.Storer.EncodedObject(objectType, hash)
}

// TestGitProtocolV2LsRefsAndFetch drives protocol v2 over smart HTTP the way a
// v2 client does: the advertisement lists commands rather than refs, ls-refs
// answers the ref question, and fetch answers with a packfile section.
func TestGitProtocolV2LsRefsAndFetch(t *testing.T) {
	t.Parallel()
	srv := newIsolatedServer(t)
	const name = "protocol-v2"
	seedGitShallowRepo(t, srv.Server, name)
	tip := gitSeedTip(t, srv, name)
	seedGitAnnotatedTag(t, srv, name, "v1", tip)

	advertisement := gitV2Advertisement(t, srv, name)
	for _, want := range []string{"version 2", "ls-refs=unborn", "object-format=sha1"} {
		if !containsGitLine(advertisement, want) {
			t.Errorf("v2 advertisement %v omits %q", advertisement, want)
		}
	}
	// The fetch command's arguments are asserted individually rather than as
	// one fixed string: they are a set, the server may legitimately grow it,
	// and pinning the exact rendering makes every added argument look like a
	// regression.
	fetchLine := ""
	for _, line := range advertisement {
		if strings.HasPrefix(strings.TrimSpace(line), "fetch=") {
			fetchLine = strings.TrimSpace(line)
			break
		}
	}
	if fetchLine == "" {
		t.Fatalf("v2 advertisement %v has no fetch command", advertisement)
	}
	for _, arg := range []string{"shallow", "filter"} {
		if !strings.Contains(fetchLine, arg) {
			t.Errorf("v2 fetch command %q omits %q", fetchLine, arg)
		}
	}

	lsRefs := (&gitPktScript{}).
		linef("command=ls-refs\n").
		linef("object-format=sha1\n").
		delim().
		linef("peel\n").
		linef("symrefs\n").
		linef("ref-prefix HEAD\n").
		linef("ref-prefix refs/\n").
		flush()
	lines := gitPktLines(t, postGitUploadPack(t, srv, name, lsRefs, map[string]string{gitProtocolHeader: "version=2"}))
	if !containsGitLine(lines, tip.String()+" HEAD symref-target:refs/heads/main") {
		t.Errorf("ls-refs %v does not name the default branch on HEAD", lines)
	}
	if !containsGitPrefix(lines, "refs/tags/v1") && !containsGitSuffix(lines, "peeled:"+tip.String()) {
		t.Errorf("ls-refs %v does not peel the annotated tag", lines)
	}
	for _, line := range lines {
		if strings.HasSuffix(line, " refs/heads/base") {
			// ref-prefix refs/ keeps every branch, which is the control for
			// the prefix filter below.
			break
		}
	}

	narrow := (&gitPktScript{}).
		linef("command=ls-refs\n").
		delim().
		linef("ref-prefix refs/tags/\n").
		flush()
	lines = gitPktLines(t, postGitUploadPack(t, srv, name, narrow, map[string]string{gitProtocolHeader: "version=2"}))
	for _, line := range lines {
		if line != "" && !strings.Contains(line, " refs/tags/") {
			t.Errorf("ref-prefix refs/tags/ still returned %q", line)
		}
	}

	fetch := (&gitPktScript{}).
		linef("command=fetch\n").
		linef("object-format=sha1\n").
		delim().
		linef("want %s\n", tip).
		linef("done\n").
		flush()
	body := postGitUploadPack(t, srv, name, fetch, map[string]string{gitProtocolHeader: "version=2"})
	sections, stream := splitGitV2Fetch(t, body)
	if len(sections) == 0 || sections[len(sections)-1] != "packfile" {
		t.Fatalf("v2 fetch sections = %v, want it to end at packfile", sections)
	}
	pack, _ := demuxGitSideband(t, stream)
	if counts, _ := scanGitPack(t, pack); counts[plumbing.CommitObject] != gitShallowHistoryCommits {
		t.Fatalf("v2 fetch carried %d commits, want %d", counts[plumbing.CommitObject], gitShallowHistoryCommits)
	}
}

// TestGitProtocolV2AcknowledgmentsSection pins the negotiation half of v2: a
// round that carries have lines and no "done" answers with acknowledgments and
// stops there unless the server is ready to pack.
func TestGitProtocolV2AcknowledgmentsSection(t *testing.T) {
	t.Parallel()
	srv := newIsolatedServer(t)
	const name = "protocol-v2-acks"
	seedGitShallowRepo(t, srv.Server, name)
	tip := gitSeedTip(t, srv, name)
	held := gitSeedNth(t, srv, name, 1)

	ready := (&gitPktScript{}).
		linef("command=fetch\n").
		delim().
		linef("want %s\n", tip).
		linef("have %s\n", held).
		flush()
	body := postGitUploadPack(t, srv, name, ready, map[string]string{gitProtocolHeader: "version=2"})
	sections, stream := splitGitV2Fetch(t, body)
	if !containsGitLine(sections, "acknowledgments") || !containsGitLine(sections, "ACK "+held.String()) || !containsGitLine(sections, "ready") {
		t.Fatalf("v2 acknowledgments = %v, want an ACK and a ready", sections)
	}
	if pack, _ := demuxGitSideband(t, stream); !bytes.HasPrefix(pack, []byte("PACK")) {
		t.Fatal("a ready acknowledgment was not followed by a packfile")
	}

	unrelated := (&gitPktScript{}).
		linef("command=fetch\n").
		delim().
		linef("want %s\n", tip).
		linef("have %s\n", plumbing.NewHash("1111111111111111111111111111111111111111")).
		flush()
	body = postGitUploadPack(t, srv, name, unrelated, map[string]string{gitProtocolHeader: "version=2"})
	sections, stream = splitGitV2Fetch(t, body)
	if !containsGitLine(sections, "NAK") {
		t.Fatalf("v2 acknowledgments for an unknown have = %v, want NAK", sections)
	}
	if len(stream) != 0 {
		t.Fatalf("a round that is not ready still sent %d bytes of packfile", len(stream))
	}
}

// TestGitProtocolV2UnbornHeadOnAnEmptyRepository is what v2 finally makes
// possible: a repository with no commits can still name the branch its first
// commit will land on.
func TestGitProtocolV2UnbornHeadOnAnEmptyRepository(t *testing.T) {
	t.Parallel()
	srv := newIsolatedServer(t)
	const name = "protocol-v2-empty"
	admin := srv.store.LookupUserByLogin("admin")
	if srv.store.CreateRepo(admin, name, "empty", false) == nil {
		t.Fatal("create the empty repository")
	}

	script := (&gitPktScript{}).
		linef("command=ls-refs\n").
		delim().
		linef("unborn\n").
		linef("symrefs\n").
		linef("ref-prefix HEAD\n").
		flush()
	lines := gitPktLines(t, postGitUploadPack(t, srv, name, script, map[string]string{gitProtocolHeader: "version=2"}))
	repo := srv.store.GetRepo("admin", name)
	want := "unborn HEAD symref-target:refs/heads/" + repo.DefaultBranch
	if !containsGitLine(lines, want) {
		t.Fatalf("ls-refs of an empty repository = %v, want %q", lines, want)
	}
}

// gitV2Advertisement fetches the protocol v2 capability advertisement from
// info/refs.
func gitV2Advertisement(t *testing.T, srv *isolatedServer, repoName string) []string {
	t.Helper()
	request, err := http.NewRequest(http.MethodGet, srv.baseURL+"/admin/"+repoName+".git/info/refs?service=git-upload-pack", nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "token "+defaultToken)
	request.Header.Set(gitProtocolHeader, "version=2")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusOK {
		t.Fatalf("v2 info/refs = %d", response.StatusCode)
	}
	return gitPktLines(t, body)
}

// gitPktLines decodes a reply that is nothing but pkt-lines.
func gitPktLines(t *testing.T, body []byte) []string {
	t.Helper()
	pkt := newGitPktReader(bufio.NewReader(bytes.NewReader(body)))
	var lines []string
	for {
		line, kind, err := pkt.next()
		if errors.Is(err, io.EOF) {
			return lines
		}
		if err != nil {
			t.Fatalf("decode pkt-lines: %v", err)
		}
		if kind == gitPktData {
			lines = append(lines, string(line))
		}
	}
}

// splitGitV2Fetch separates the named sections of a v2 fetch response from the
// multiplexed packfile that follows the "packfile" line.
func splitGitV2Fetch(t *testing.T, body []byte) (sections []string, stream []byte) {
	t.Helper()
	reader := bufio.NewReader(bytes.NewReader(body))
	pkt := newGitPktReader(reader)
	for {
		line, kind, err := pkt.next()
		if errors.Is(err, io.EOF) {
			return sections, nil
		}
		if err != nil {
			t.Fatalf("decode v2 fetch reply: %v", err)
		}
		if kind != gitPktData {
			continue
		}
		sections = append(sections, string(line))
		if string(line) == "packfile" {
			rest, err := io.ReadAll(reader)
			if err != nil {
				t.Fatal(err)
			}
			return sections, rest
		}
	}
}

func containsGitLine(lines []string, want string) bool {
	for _, line := range lines {
		if line == want {
			return true
		}
	}
	return false
}

func containsGitPrefix(lines []string, want string) bool {
	for _, line := range lines {
		if strings.Contains(line, want) {
			return true
		}
	}
	return false
}

func containsGitSuffix(lines []string, want string) bool {
	for _, line := range lines {
		if strings.HasSuffix(line, want) {
			return true
		}
	}
	return false
}
