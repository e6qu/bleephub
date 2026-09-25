package azfake

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/streaming"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/blob"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/bloberror"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/blockblob"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/container"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/sas"
)

const held = "held"

func newClient(t *testing.T) (*Server, *container.Client, *container.SharedKeyCredential) {
	t.Helper()
	server := New()
	t.Cleanup(server.Close)
	server.CreateContainer(held)
	credential, err := container.NewSharedKeyCredential(server.AccountName(), server.AccountKey())
	if err != nil {
		t.Fatalf("the fake's own key was refused: %v", err)
	}
	client, err := container.NewClientWithSharedKeyCredential(server.URL()+"/"+held, credential, nil)
	if err != nil {
		t.Fatalf("client: %v", err)
	}
	return server, client, credential
}

func upload(client *container.Client, name, body string, conditions *blob.ModifiedAccessConditions, metadata map[string]*string) (blockblob.UploadResponse, error) {
	return client.NewBlockBlobClient(name).Upload(context.Background(), streaming.NopCloser(strings.NewReader(body)), &blockblob.UploadOptions{
		Metadata:         metadata,
		AccessConditions: &blob.AccessConditions{ModifiedAccessConditions: conditions},
	})
}

// TestConditionalWritesHoldTheirPreconditions pins If-None-Match and If-Match
// on a write, with the codes the service answers: a driver's compare-and-swap is
// made of these, and a fake that let every contender win would pass a driver
// that never sent the condition at all.
func TestConditionalWritesHoldTheirPreconditions(t *testing.T) {
	server, client, _ := newClient(t)
	anything := azcore.ETagAny
	ifAbsent := &blob.ModifiedAccessConditions{IfNoneMatch: &anything}

	first, err := upload(client, "lock", "holder one", ifAbsent, nil)
	if err != nil {
		t.Fatalf("taking a free lock: %v", err)
	}
	if _, err := upload(client, "lock", "holder two", ifAbsent, nil); !bloberror.HasCode(err, bloberror.BlobAlreadyExists) {
		t.Fatalf("taking a held lock answered %v, want 409 BlobAlreadyExists", err)
	}
	if _, err := upload(client, "absent", "x", &blob.ModifiedAccessConditions{IfMatch: first.ETag}, nil); !bloberror.HasCode(err, bloberror.ConditionNotMet) {
		t.Fatalf("a swap of a blob that does not exist answered %v, want 412 ConditionNotMet", err)
	}
	second, err := upload(client, "lock", "holder one", &blob.ModifiedAccessConditions{IfMatch: first.ETag}, nil)
	if err != nil {
		t.Fatalf("a swap at the current ETag: %v", err)
	}
	if *second.ETag == *first.ETag {
		t.Fatal("a rewrite with the same bytes kept its ETag; the service's changes with every write")
	}
	if _, err := upload(client, "lock", "holder three", &blob.ModifiedAccessConditions{IfMatch: first.ETag}, nil); !bloberror.HasCode(err, bloberror.ConditionNotMet) {
		t.Fatalf("a swap at a stale ETag answered %v, want 412 ConditionNotMet", err)
	}
	if names := server.BlobNames(held); len(names) != 1 || names[0] != "lock" {
		t.Fatalf("refused writes left %v", names)
	}

	// The commit of a block list is a write like any other, and is where a
	// streamed upload's condition is held.
	blocks := client.NewBlockBlobClient("lock")
	blockID := base64.StdEncoding.EncodeToString([]byte("block-0"))
	if _, err := blocks.StageBlock(context.Background(), blockID, streaming.NopCloser(strings.NewReader("usurper")), nil); err != nil {
		t.Fatalf("stage: %v", err)
	}
	if _, err := blocks.CommitBlockList(context.Background(), []string{blockID}, &blockblob.CommitBlockListOptions{
		AccessConditions: &blob.AccessConditions{ModifiedAccessConditions: ifAbsent},
	}); !bloberror.HasCode(err, bloberror.BlobAlreadyExists) {
		t.Fatalf("a conditional commit over a blob that exists answered %v, want 409 BlobAlreadyExists", err)
	}
}

// TestStagedBlocksAreNoBlobUntilCommitted pins what makes a streamed upload
// atomic, and the rule about block IDs a driver can break without noticing:
// the service refuses IDs of differing lengths under one blob.
func TestStagedBlocksAreNoBlobUntilCommitted(t *testing.T) {
	_, client, _ := newClient(t)
	ctx := context.Background()
	blocks := client.NewBlockBlobClient("pack")
	ids := []string{base64.StdEncoding.EncodeToString([]byte("id-0")), base64.StdEncoding.EncodeToString([]byte("id-1"))}
	for i, id := range ids {
		if _, err := blocks.StageBlock(ctx, id, streaming.NopCloser(strings.NewReader(fmt.Sprintf("block %d;", i))), nil); err != nil {
			t.Fatalf("stage %d: %v", i, err)
		}
	}
	if _, err := blocks.GetProperties(ctx, nil); !bloberror.HasCode(err, bloberror.BlobNotFound) {
		t.Fatalf("a blob with only staged blocks answered %v, want BlobNotFound", err)
	}
	longer := base64.StdEncoding.EncodeToString([]byte("id-0000010"))
	if _, err := blocks.StageBlock(ctx, longer, streaming.NopCloser(strings.NewReader("x")), nil); !bloberror.HasCode(err, bloberror.InvalidBlobOrBlock) {
		t.Fatalf("a block ID of another length answered %v, want 400 InvalidBlobOrBlock", err)
	}
	if _, err := blocks.CommitBlockList(ctx, []string{ids[1], ids[0]}, nil); err != nil {
		t.Fatalf("commit: %v", err)
	}
	read, err := blocks.DownloadStream(ctx, nil)
	if err != nil {
		t.Fatalf("download: %v", err)
	}
	content, _ := io.ReadAll(read.Body)
	_ = read.Body.Close()
	if string(content) != "block 1;block 0;" {
		t.Fatalf("the blob is %q, want the blocks in the order the commit listed them", content)
	}
	if _, err := blocks.CommitBlockList(ctx, ids, nil); !bloberror.HasCode(err, bloberror.InvalidBlockList) {
		t.Fatalf("committing blocks a commit already consumed answered %v, want 400 InvalidBlockList", err)
	}
}

// TestAMetadataNameMustBeAnIdentifier pins the refusal that S3 does not make.
// The driver's own validation is stricter, so against this fake the refusal is
// never reached by a correct driver — which is the point: one that stopped
// validating would be caught here as it would be by the service.
func TestAMetadataNameMustBeAnIdentifier(t *testing.T) {
	server, client, _ := newClient(t)
	value := "v"
	for _, name := range []string{"with-hyphen", "9digit"} {
		if _, err := upload(client, "refused", "x", nil, map[string]*string{name: &value}); !bloberror.HasCode(err, bloberror.InvalidMetadata) {
			t.Errorf("metadata name %q answered %v, want 400 InvalidMetadata", name, err)
		}
	}
	if names := server.BlobNames(held); len(names) != 0 {
		t.Fatalf("a refused write left %v", names)
	}
	if _, err := upload(client, "kept", "x", nil, map[string]*string{"under_score9": &value}); err != nil {
		t.Fatalf("PREMISE: a name that is an identifier was refused too: %v", err)
	}
	properties, err := client.NewBlobClient("kept").GetProperties(context.Background(), nil)
	if err != nil {
		t.Fatalf("properties: %v", err)
	}
	found := ""
	for name, got := range properties.Metadata {
		if strings.EqualFold(name, "under_score9") {
			found = *got
		}
	}
	if found != "v" {
		t.Fatalf("metadata came back as %v", properties.Metadata)
	}
}

// TestAContainerNeverCreatedIsContainerNotFound pins the code that lets a
// driver tell a missing container from a missing blob; both are 404.
func TestAContainerNeverCreatedIsContainerNotFound(t *testing.T) {
	server, _, credential := newClient(t)
	elsewhere, err := container.NewClientWithSharedKeyCredential(server.URL()+"/never-created", credential, nil)
	if err != nil {
		t.Fatalf("client: %v", err)
	}
	if _, err := elsewhere.NewBlobClient("HEAD").GetProperties(context.Background(), nil); !bloberror.HasCode(err, bloberror.ContainerNotFound) {
		t.Fatalf("a HEAD in a missing container answered %v, want ContainerNotFound", err)
	}
	if _, err := upload(elsewhere, "HEAD", "x", nil, nil); !bloberror.HasCode(err, bloberror.ContainerNotFound) {
		t.Fatalf("a PUT in a missing container answered %v, want ContainerNotFound", err)
	}
}

func fetch(t *testing.T, method, target string) (int, string, string) {
	t.Helper()
	request, err := http.NewRequestWithContext(context.Background(), method, target, nil)
	if err != nil {
		t.Fatalf("request %s: %v", target, err)
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("%s %s: %v", method, target, err)
	}
	body, _ := io.ReadAll(response.Body)
	_ = response.Body.Close()
	return response.StatusCode, response.Header.Get("x-ms-error-code"), string(body)
}

// TestASharedAccessSignatureIsVerified pins every way a signed URL can be wrong
// and still look like one. The driver composes what is signed — which blob,
// which permission, until when — so a fake that accepted any query string would
// pass a driver that signed the container, or forever, or for writing.
func TestASharedAccessSignatureIsVerified(t *testing.T) {
	server, _, credential := newClient(t)
	server.Put(held, "packs/one", []byte("pack one"))
	server.Put(held, "packs/two", []byte("pack two"))
	// Fixed dates either side of any day this runs on: a test may not read the clock.
	future := time.Date(2999, 1, 1, 0, 0, 0, 0, time.UTC)
	past := time.Date(2001, 1, 1, 0, 0, 0, 0, time.UTC)
	sign := func(name string, permissions sas.BlobPermissions, starts, expires time.Time) string {
		signed, err := sas.BlobSignatureValues{
			Version:       sas.Version,
			StartTime:     starts,
			ExpiryTime:    expires,
			Permissions:   permissions.String(),
			ContainerName: held,
			BlobName:      name,
		}.SignWithSharedKey(credential)
		if err != nil {
			t.Fatalf("sign: %v", err)
		}
		return signed.Encode()
	}
	one, two := server.URL()+"/"+held+"/packs%2Fone", server.URL()+"/"+held+"/packs%2Ftwo"
	read := sign("packs/one", sas.BlobPermissions{Read: true}, time.Time{}, future)

	if status, _, body := fetch(t, http.MethodGet, one+"?"+read); status != http.StatusOK || body != "pack one" {
		t.Fatalf("PREMISE: a good signature answered %d %q, so the refusals below would prove nothing", status, body)
	}
	tampered := strings.Replace(read, "sig=", "sig=AAAA", 1)
	for name, attempt := range map[string]struct{ method, target, code string }{
		"no credentials at all":         {http.MethodGet, one, "AuthenticationFailed"},
		"a signature for another blob":  {http.MethodGet, two + "?" + read, "AuthenticationFailed"},
		"a signature altered":           {http.MethodGet, one + "?" + tampered, "AuthenticationFailed"},
		"an expiry altered":             {http.MethodGet, one + "?" + strings.Replace(read, "se=2999", "se=3999", 1), "AuthenticationFailed"},
		"a signature that has expired":  {http.MethodGet, one + "?" + sign("packs/one", sas.BlobPermissions{Read: true}, time.Time{}, past), "AuthenticationFailed"},
		"a signature not yet valid":     {http.MethodGet, one + "?" + sign("packs/one", sas.BlobPermissions{Read: true}, future, future.AddDate(1, 0, 0)), "AuthenticationFailed"},
		"a signature without read":      {http.MethodGet, one + "?" + sign("packs/one", sas.BlobPermissions{Write: true}, time.Time{}, future), "AuthorizationPermissionMismatch"},
		"a read signature used to kill": {http.MethodDelete, one + "?" + read, "AuthorizationPermissionMismatch"},
	} {
		if status, code, _ := fetch(t, attempt.method, attempt.target); status != http.StatusForbidden || code != attempt.code {
			t.Errorf("%s: answered %d %s, want 403 %s", name, status, code, attempt.code)
		}
	}
	if names := server.BlobNames(held); len(names) != 2 {
		t.Fatalf("a refused request changed the store: %v", names)
	}
}

// TestAListingIsPagedOrderedAndFolded pins the listing a driver walks: more than
// one page, joined by the marker with nothing lost or repeated at the seam — a
// folded prefix least of all, since every key under it sorts on both sides of a
// page boundary that falls inside it — and blobs and prefixes as one sequence
// in order of name, which is how the service sends them.
func TestAListingIsPagedOrderedAndFolded(t *testing.T) {
	server, client, _ := newClient(t)
	ctx := context.Background()
	var want []string
	for i := range 2 * pageSize {
		name := fmt.Sprintf("repo/objects/%05d", i)
		server.Put(held, name, []byte("x"))
		want = append(want, name)
	}
	for _, name := range []string{"repo/HEAD", "repo/objects-not-a-directory", "repo/refs/heads/main", "elsewhere/HEAD"} {
		server.Put(held, name, []byte("x"))
	}
	want = append(want, "repo/HEAD", "repo/objects-not-a-directory", "repo/refs/heads/main")
	sort.Strings(want)

	prefix := "repo/"
	var got []string
	pages := 0
	flat := client.NewListBlobsFlatPager(&container.ListBlobsFlatOptions{Prefix: &prefix})
	for flat.More() {
		page, err := flat.NextPage(ctx)
		if err != nil {
			t.Fatalf("page %d: %v", pages, err)
		}
		pages++
		for _, item := range page.Segment.BlobItems {
			got = append(got, *item.Name)
			if strings.Contains(string(*item.Properties.ETag), `"`) || *item.Properties.ContentLength != 1 || item.Properties.LastModified.IsZero() {
				t.Fatalf("%s listed with ETag %s, length %d, modified %s", *item.Name, *item.Properties.ETag, *item.Properties.ContentLength, item.Properties.LastModified)
			}
		}
	}
	if pages != 3 {
		t.Fatalf("PREMISE: %d blobs came in %d pages, want 3 — paging was not exercised", len(want), pages)
	}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Fatalf("the flat listing returned %d names, want the %d written, in order, each once", len(got), len(want))
	}

	// Folded, the 2000 objects are one prefix, and the listing is four entries.
	status, _, document := fetch(t, http.MethodGet, server.URL()+"/"+held+"?restype=container&comp=list&delimiter=%2F&prefix=repo%2F&"+containerSAS(t, server))
	if status != http.StatusForbidden {
		t.Fatalf("a listing with a signature for a blob answered %d, want 403: %s", status, document)
	}
	var folded []string
	hierarchy := client.NewListBlobsHierarchyPager("/", &container.ListBlobsHierarchyOptions{Prefix: &prefix})
	for hierarchy.More() {
		page, err := hierarchy.NextPage(ctx)
		if err != nil {
			t.Fatalf("hierarchy page: %v", err)
		}
		for _, item := range page.Segment.BlobItems {
			folded = append(folded, *item.Name)
		}
		for _, item := range page.Segment.BlobPrefixes {
			folded = append(folded, *item.Name+" (prefix)")
		}
	}
	sort.Strings(folded)
	if strings.Join(folded, ", ") != "repo/HEAD, repo/objects-not-a-directory, repo/objects/ (prefix), repo/refs/ (prefix)" {
		t.Fatalf("the folded listing is %v", folded)
	}
}

// containerSAS signs for a blob named like the container's listing, which is
// the nearest thing to a listing a blob signature can be pointed at.
func containerSAS(t *testing.T, server *Server) string {
	t.Helper()
	credential, err := container.NewSharedKeyCredential(server.AccountName(), server.AccountKey())
	if err != nil {
		t.Fatal(err)
	}
	signed, err := sas.BlobSignatureValues{
		Version:       sas.Version,
		ExpiryTime:    time.Date(2999, 1, 1, 0, 0, 0, 0, time.UTC),
		Permissions:   (&sas.BlobPermissions{Read: true}).String(),
		ContainerName: held,
		BlobName:      "repo/HEAD",
	}.SignWithSharedKey(credential)
	if err != nil {
		t.Fatal(err)
	}
	return signed.Encode()
}

// TestBlobsAndPrefixesAreSentAsOneOrderedSequence looks at the listing's XML
// itself, because the client library sorts what it parses into two lists and so
// hides whether the fake interleaves as the service does. A driver has to merge
// the two lists back together; this is what it is merging back to.
func TestBlobsAndPrefixesAreSentAsOneOrderedSequence(t *testing.T) {
	server, client, _ := newClient(t)
	for _, name := range []string{"a-blob", "b/under", "c-blob", "d/under"} {
		server.Put(held, name, []byte("x"))
	}
	var document string
	capture := &capturing{seen: &document}
	credential, err := container.NewSharedKeyCredential(server.AccountName(), server.AccountKey())
	if err != nil {
		t.Fatal(err)
	}
	observed, err := container.NewClientWithSharedKeyCredential(client.URL(), credential, &container.ClientOptions{
		ClientOptions: azcore.ClientOptions{Transport: &http.Client{Transport: capture}},
	})
	if err != nil {
		t.Fatal(err)
	}
	pager := observed.NewListBlobsHierarchyPager("/", nil)
	if _, err := pager.NextPage(context.Background()); err != nil {
		t.Fatalf("list: %v", err)
	}
	var order []int
	for _, element := range []string{"<Name>a-blob</Name>", "<BlobPrefix><Name>b/</Name>", "<Name>c-blob</Name>", "<BlobPrefix><Name>d/</Name>"} {
		order = append(order, strings.Index(document, element))
	}
	if order[0] < 0 || !sort.IntsAreSorted(order) {
		t.Fatalf("the elements sit at %v in the document, want all present and ascending:\n%s", order, document)
	}
}

// capturing keeps the body of the last response for a test to read, and hands
// the client an unread copy.
type capturing struct{ seen *string }

func (c *capturing) RoundTrip(request *http.Request) (*http.Response, error) {
	response, err := http.DefaultTransport.RoundTrip(request)
	if err != nil {
		return nil, err
	}
	body, err := io.ReadAll(response.Body)
	_ = response.Body.Close()
	if err != nil {
		return nil, err
	}
	*c.seen = string(body)
	response.Body = io.NopCloser(bytes.NewReader(body))
	return response, nil
}

// TestABatchAnswersForEachPartAndRefusesTooMany pins the two things a bulk
// delete's correctness rests on: a part that failed says so only in its own
// answer, under a batch that succeeded, and a batch over the limit is refused
// whole — it must not delete the first 256 and fail.
func TestABatchAnswersForEachPartAndRefusesTooMany(t *testing.T) {
	server, client, _ := newClient(t)
	ctx := context.Background()
	for i := range batchLimit + 1 {
		server.Put(held, fmt.Sprintf("objects/%03d", i), []byte("x"))
	}

	mixed, err := client.NewBatchBuilder()
	if err != nil {
		t.Fatal(err)
	}
	named := []string{"objects/000", "objects/never-there", "objects/001"}
	for _, name := range named {
		if err := mixed.Delete(name, nil); err != nil {
			t.Fatal(err)
		}
	}
	answered, err := client.SubmitBatch(ctx, mixed, nil)
	if err != nil {
		t.Fatalf("batch: %v", err)
	}
	var outcomes []string
	for _, part := range answered.Responses {
		outcome := "deleted"
		if part.Error != nil {
			outcome = "refused"
			if bloberror.HasCode(part.Error, bloberror.BlobNotFound) {
				outcome = "BlobNotFound"
			}
		}
		// A part is matched to its request by Content-ID. The client's own BlobName
		// is read out of the URL on the assumption that the account is in the host.
		outcomes = append(outcomes, named[*part.ContentID]+"="+outcome)
	}
	sort.Strings(outcomes)
	if got := strings.Join(outcomes, " "); got != "objects/000=deleted objects/001=deleted objects/never-there=BlobNotFound" {
		t.Fatalf("the parts answered %q", got)
	}

	before := len(server.BlobNames(held))
	tooMany, err := client.NewBatchBuilder()
	if err != nil {
		t.Fatal(err)
	}
	for i := range batchLimit + 1 {
		if err := tooMany.Delete(fmt.Sprintf("objects/%03d", i), nil); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := client.SubmitBatch(ctx, tooMany, nil); err == nil {
		t.Fatal("a batch of 257 was accepted; the service takes 256")
	}
	if after := len(server.BlobNames(held)); before != batchLimit-1 || after != before {
		t.Fatalf("the store held %d blobs before the refused batch and %d after, want %d both times", before, after, batchLimit-1)
	}
}

// TestACopyCarriesBytesAndMetadataAndReportsItsProgress pins Copy Blob as the
// driver uses it: a missing source is BlobNotFound, the destination gets the
// source's metadata, and a copy made to report itself pending does so for
// exactly as many reads of the destination as it was told to.
func TestACopyCarriesBytesAndMetadataAndReportsItsProgress(t *testing.T) {
	server, client, _ := newClient(t)
	ctx := context.Background()
	value := "beside"
	if _, err := upload(client, "source", "a pack", nil, map[string]*string{"kept": &value}); err != nil {
		t.Fatal(err)
	}
	source := client.NewBlobClient("source").URL()
	// The service refuses a copy of what is not there as unable to verify its
	// source, and names the source's own refusal in headers.
	_, err := client.NewBlobClient("nowhere").StartCopyFromURL(ctx, client.NewBlobClient("absent").URL(), nil)
	var refused *azcore.ResponseError
	if !errors.As(err, &refused) || refused.ErrorCode != string(bloberror.CannotVerifyCopySource) {
		t.Fatalf("copying what is not there answered %v, want CannotVerifyCopySource", err)
	}
	if status, code := refused.RawResponse.Header.Get("x-ms-copy-source-status-code"), refused.RawResponse.Header.Get("x-ms-copy-source-error-code"); status != "404" || code != string(bloberror.BlobNotFound) {
		t.Errorf("the source's refusal was named %q %q, want 404 BlobNotFound", status, code)
	}

	server.SetPendingCopyPolls(2)
	destination := client.NewBlobClient("fork/destination")
	started, err := destination.StartCopyFromURL(ctx, source, nil)
	if err != nil {
		t.Fatalf("copy: %v", err)
	}
	statuses := []string{string(*started.CopyStatus)}
	for range 3 {
		properties, err := destination.GetProperties(ctx, nil)
		if err != nil {
			t.Fatalf("properties: %v", err)
		}
		if *properties.CopyID != *started.CopyID {
			t.Fatalf("the destination names copy %s, the copy started was %s", *properties.CopyID, *started.CopyID)
		}
		statuses = append(statuses, string(*properties.CopyStatus))
	}
	if got := strings.Join(statuses, " "); got != "pending pending pending success" {
		t.Fatalf("the copy reported %q", got)
	}
	read, err := destination.DownloadStream(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	content, _ := io.ReadAll(read.Body)
	_ = read.Body.Close()
	if string(content) != "a pack" || len(read.Metadata) != 1 {
		t.Fatalf("the copy holds %q with metadata %v", content, read.Metadata)
	}
}

// TestARangeIsCutAtTheEndAndRefusedBeyondIt pins ranged reads: the whole size
// in Content-Range, a range running past the end shortened, one starting at the
// end refused as InvalidRange, and an empty blob readable whole.
func TestARangeIsCutAtTheEndAndRefusedBeyondIt(t *testing.T) {
	server, client, _ := newClient(t)
	ctx := context.Background()
	server.Put(held, "pack", []byte("0123456789"))
	server.Put(held, "empty", nil)

	tail, err := client.NewBlobClient("pack").DownloadStream(ctx, &blob.DownloadStreamOptions{Range: blob.HTTPRange{Offset: 8, Count: 100}})
	if err != nil {
		t.Fatalf("a range past the end: %v", err)
	}
	content, _ := io.ReadAll(tail.Body)
	_ = tail.Body.Close()
	if string(content) != "89" || *tail.ContentRange != "bytes 8-9/10" {
		t.Fatalf("bytes 8-107 of ten returned %q as %q", content, *tail.ContentRange)
	}
	if _, err := client.NewBlobClient("pack").DownloadStream(ctx, &blob.DownloadStreamOptions{Range: blob.HTTPRange{Offset: 10, Count: 4}}); !bloberror.HasCode(err, bloberror.InvalidRange) {
		t.Fatalf("a range starting at the end answered %v, want 416 InvalidRange", err)
	}
	whole, err := client.NewBlobClient("empty").DownloadStream(ctx, nil)
	if err != nil {
		t.Fatalf("an empty blob: %v", err)
	}
	content, _ = io.ReadAll(whole.Body)
	_ = whole.Body.Close()
	if len(content) != 0 || *whole.ContentLength != 0 {
		t.Fatalf("an empty blob read as %q of %d", content, *whole.ContentLength)
	}
}
