package bleephub

import (
	"strings"
	"testing"

	"github.com/e6qu/bleephub/internal/store"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	gitStorage "github.com/go-git/go-git/v5/storage"
)

// The git object graph over GraphQL: Repository.object/ref/refs and the
// GitObject implementations they reach. Every case runs a real query against a
// seeded repository through the /api/graphql endpoint, because the resolvers
// only mean anything in terms of what a client can read back out.

// gitGraphFixture is a repository whose git storage holds a small but complete
// object graph: nested directories, a binary file, several commits, a branch
// and an annotated tag.
type gitGraphFixture struct {
	repo       *store.Repo
	storage    gitStorage.Storer
	firstOID   string
	headOID    string
	branchOID  string
	tagOID     string
	binaryText string
}

const gitGraphBinaryContent = "PNG\x00\x01\x02binary\x00payload"

func newGitGraphFixture(t *testing.T, s *isolatedServer) *gitGraphFixture {
	t.Helper()
	name := s.createRepoWriteRepo(t, false)
	repo := s.store.GetRepo("admin", name)
	if repo == nil {
		t.Fatal("fixture repository is missing")
	}
	stor := s.store.GetGitStorage("admin", name)
	if stor == nil {
		t.Fatal("fixture repository has no git storage")
	}
	sig := repoSignature("admin", "admin@bleephub.local")

	first, err := initRepoWithFiles(stor, "main", "initial commit", map[string]string{
		"README.md":        "# fixture\n\nsecond line\n",
		"src/lib.go":       "package lib\n",
		"src/deep/one.txt": "one\n",
		"assets/logo.bin":  gitGraphBinaryContent,
	}, sig)
	if err != nil {
		t.Fatalf("seed initial commit: %v", err)
	}
	head, err := createFileCommit(stor, "main", "src/lib.go", "package lib\n\nfunc Go() {}\n", "extend lib", sig)
	if err != nil {
		t.Fatalf("seed second commit: %v", err)
	}
	if err := stor.SetReference(plumbing.NewHashReference(plumbing.NewBranchReferenceName("topic"), first)); err != nil {
		t.Fatalf("seed topic branch: %v", err)
	}
	branch, err := createFileCommit(stor, "topic", "topic.txt", "topic\n", "add topic file", sig)
	if err != nil {
		t.Fatalf("seed topic commit: %v", err)
	}

	tag := &object.Tag{
		Name:       "v1.0.0",
		Message:    "release one\n",
		Tagger:     *sig,
		Target:     head,
		TargetType: plumbing.CommitObject,
	}
	encoded := stor.NewEncodedObject()
	if err := tag.Encode(encoded); err != nil {
		t.Fatalf("encode annotated tag: %v", err)
	}
	tagHash, err := stor.SetEncodedObject(encoded)
	if err != nil {
		t.Fatalf("store annotated tag: %v", err)
	}
	if err := stor.SetReference(plumbing.NewHashReference(plumbing.NewTagReferenceName("v1.0.0"), tagHash)); err != nil {
		t.Fatalf("set annotated tag ref: %v", err)
	}

	return &gitGraphFixture{
		repo:       s.store.GetRepo("admin", name),
		storage:    stor,
		firstOID:   first.String(),
		headOID:    head.String(),
		branchOID:  branch.String(),
		tagOID:     tagHash.String(),
		binaryText: gitGraphBinaryContent,
	}
}

// gitGraphQuery runs a query against the fixture repository and returns the
// `repository` object of the response, failing on any GraphQL error.
func (f *gitGraphFixture) query(t *testing.T, s *isolatedServer, token, selection string) map[string]interface{} {
	t.Helper()
	owner, name, _ := store.SplitRepoFullName(f.repo.FullName)
	response := decodeJSONWithStatus(t, s.post(t, "/api/graphql", token, map[string]interface{}{
		"query": `query($owner:String!,$name:String!){repository(owner:$owner,name:$name)` + selection + `}`,
		"variables": map[string]interface{}{
			"owner": owner,
			"name":  name,
		},
	}), 200)
	if errs := response["errors"]; errs != nil {
		t.Fatalf("GraphQL errors = %v", errs)
	}
	data, _ := response["data"].(map[string]interface{})
	repository, _ := data["repository"].(map[string]interface{})
	if repository == nil {
		t.Fatalf("repository resolved to null: %v", response)
	}
	return repository
}

func TestGraphQLRepositoryObjectReadsAFileBlob(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	fixture := newGitGraphFixture(t, s)

	repository := fixture.query(t, s, defaultToken, `{
		object(expression:"HEAD:README.md"){
			__typename
			oid
			abbreviatedOid
			commitUrl
			commitResourcePath
			... on Blob { text byteSize isBinary isTruncated }
		}
	}`)
	blob, _ := repository["object"].(map[string]interface{})
	if blob == nil {
		t.Fatalf("object(HEAD:README.md) resolved to null: %v", repository)
	}
	if blob["__typename"] != "Blob" {
		t.Fatalf("__typename = %v, want Blob", blob["__typename"])
	}
	if blob["text"] != "# fixture\n\nsecond line\n" {
		t.Fatalf("text = %q", blob["text"])
	}
	if blob["byteSize"] != float64(len("# fixture\n\nsecond line\n")) {
		t.Fatalf("byteSize = %v", blob["byteSize"])
	}
	if blob["isBinary"] != false || blob["isTruncated"] != false {
		t.Fatalf("isBinary/isTruncated = %v/%v", blob["isBinary"], blob["isTruncated"])
	}
	oid, _ := blob["oid"].(string)
	if len(oid) != 40 {
		t.Fatalf("oid = %q", oid)
	}
	if blob["abbreviatedOid"] != oid[:7] {
		t.Fatalf("abbreviatedOid = %v, want %q", blob["abbreviatedOid"], oid[:7])
	}
	if path, _ := blob["commitResourcePath"].(string); path != "/"+fixture.repo.FullName+"/commit/"+oid {
		t.Fatalf("commitResourcePath = %q", path)
	}
	if url, _ := blob["commitUrl"].(string); !strings.HasSuffix(url, "/"+fixture.repo.FullName+"/commit/"+oid) {
		t.Fatalf("commitUrl = %q", url)
	}
}

func TestGraphQLRepositoryObjectWalksTheTree(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	fixture := newGitGraphFixture(t, s)

	repository := fixture.query(t, s, defaultToken, `{
		object(expression:"HEAD:"){
			__typename
			... on Tree {
				entries {
					name
					path
					type
					mode
					extension
					object {
						__typename
						... on Blob { byteSize }
						... on Tree { entries { name path type } }
					}
				}
			}
		}
	}`)
	tree, _ := repository["object"].(map[string]interface{})
	if tree == nil || tree["__typename"] != "Tree" {
		t.Fatalf("object(HEAD:) = %v", tree)
	}
	entries, _ := tree["entries"].([]interface{})
	byName := map[string]map[string]interface{}{}
	for _, raw := range entries {
		entry, _ := raw.(map[string]interface{})
		name, _ := entry["name"].(string)
		byName[name] = entry
	}
	readme := byName["README.md"]
	if readme == nil {
		t.Fatalf("README.md missing from the root tree: %v", entries)
	}
	if readme["type"] != "blob" || readme["path"] != "README.md" || readme["extension"] != ".md" {
		t.Fatalf("README.md entry = %v", readme)
	}
	// 100644 regular-file mode, the decimal file mode GitHub reports.
	if readme["mode"] != float64(0o100644) {
		t.Fatalf("README.md mode = %v", readme["mode"])
	}
	readmeObject, _ := readme["object"].(map[string]interface{})
	if readmeObject["__typename"] != "Blob" || readmeObject["byteSize"] != float64(len("# fixture\n\nsecond line\n")) {
		t.Fatalf("README.md object = %v", readmeObject)
	}

	src := byName["src"]
	if src == nil || src["type"] != "tree" {
		t.Fatalf("src entry = %v", src)
	}
	srcObject, _ := src["object"].(map[string]interface{})
	if srcObject["__typename"] != "Tree" {
		t.Fatalf("src object = %v", srcObject)
	}
	nested, _ := srcObject["entries"].([]interface{})
	paths := map[string]string{}
	for _, raw := range nested {
		entry, _ := raw.(map[string]interface{})
		name, _ := entry["name"].(string)
		entryType, _ := entry["type"].(string)
		path, _ := entry["path"].(string)
		paths[name] = entryType + " " + path
	}
	if paths["lib.go"] != "blob src/lib.go" {
		t.Fatalf("nested lib.go = %q", paths["lib.go"])
	}
	if paths["deep"] != "tree src/deep" {
		t.Fatalf("nested deep = %q", paths["deep"])
	}
}

func TestGraphQLRefResolvesACommitAndItsHistory(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	fixture := newGitGraphFixture(t, s)

	repository := fixture.query(t, s, defaultToken, `{
		ref(qualifiedName:"refs/heads/main"){
			id
			name
			prefix
			repository { nameWithOwner }
			target {
				__typename
				oid
				... on Commit {
					message
					messageHeadline
					author { name email date }
					committer { name }
					tree { __typename oid entries { name } }
					parents(first:5){ totalCount nodes { oid } }
					history(first:5){ totalCount nodes { oid messageHeadline } }
				}
			}
		}
	}`)
	ref, _ := repository["ref"].(map[string]interface{})
	if ref == nil {
		t.Fatalf("ref(refs/heads/main) resolved to null: %v", repository)
	}
	if ref["name"] != "main" || ref["prefix"] != "refs/heads/" {
		t.Fatalf("ref name/prefix = %v/%v", ref["name"], ref["prefix"])
	}
	if id, _ := ref["id"].(string); id == "" {
		t.Fatal("ref id is empty")
	}
	if repo, _ := ref["repository"].(map[string]interface{}); repo["nameWithOwner"] != fixture.repo.FullName {
		t.Fatalf("ref repository = %v", ref["repository"])
	}
	target, _ := ref["target"].(map[string]interface{})
	if target["__typename"] != "Commit" || target["oid"] != fixture.headOID {
		t.Fatalf("ref target = %v", target)
	}
	if target["messageHeadline"] != "extend lib" || target["message"] != "extend lib" {
		t.Fatalf("commit message = %v / %v", target["messageHeadline"], target["message"])
	}
	author, _ := target["author"].(map[string]interface{})
	if author["name"] != "admin" || author["email"] != "admin@bleephub.local" {
		t.Fatalf("commit author = %v", author)
	}
	if date, _ := author["date"].(string); date == "" {
		t.Fatal("commit author date is empty")
	}
	tree, _ := target["tree"].(map[string]interface{})
	if tree["__typename"] != "Tree" {
		t.Fatalf("commit tree = %v", tree)
	}
	if entries, _ := tree["entries"].([]interface{}); len(entries) != 3 {
		t.Fatalf("commit tree entries = %v", tree["entries"])
	}
	parents, _ := target["parents"].(map[string]interface{})
	if parents["totalCount"] != float64(1) {
		t.Fatalf("parents = %v", parents)
	}
	parentNodes, _ := parents["nodes"].([]interface{})
	if first, _ := parentNodes[0].(map[string]interface{}); first["oid"] != fixture.firstOID {
		t.Fatalf("parent oid = %v, want %s", parentNodes, fixture.firstOID)
	}
	history, _ := target["history"].(map[string]interface{})
	if history["totalCount"] != float64(2) {
		t.Fatalf("history totalCount = %v", history["totalCount"])
	}
	historyNodes, _ := history["nodes"].([]interface{})
	headline := func(index int) string {
		node, _ := historyNodes[index].(map[string]interface{})
		value, _ := node["messageHeadline"].(string)
		return value
	}
	if len(historyNodes) != 2 || headline(0) != "extend lib" || headline(1) != "initial commit" {
		t.Fatalf("history nodes = %v", historyNodes)
	}
}

func TestGraphQLCommitHistoryFiltersByPathAndAuthor(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	fixture := newGitGraphFixture(t, s)

	repository := fixture.query(t, s, defaultToken, `{
		touching: object(expression:"HEAD"){
			... on Commit { history(first:10, path:"src/lib.go"){ totalCount nodes { messageHeadline } } }
		}
		untouched: object(expression:"HEAD"){
			... on Commit { history(first:10, path:"assets"){ totalCount } }
		}
		byOther: object(expression:"HEAD"){
			... on Commit { history(first:10, author:{emails:["nobody@example.com"]}){ totalCount } }
		}
		byAuthor: object(expression:"HEAD"){
			... on Commit { history(first:10, author:{emails:["admin@bleephub.local"]}){ totalCount } }
		}
	}`)
	touching, _ := repository["touching"].(map[string]interface{})
	history, _ := touching["history"].(map[string]interface{})
	if history["totalCount"] != float64(2) {
		t.Fatalf("path-filtered history = %v", history)
	}
	untouched, _ := repository["untouched"].(map[string]interface{})
	untouchedHistory, _ := untouched["history"].(map[string]interface{})
	if untouchedHistory["totalCount"] != float64(1) {
		t.Fatalf("assets history = %v", untouchedHistory)
	}
	byOther, _ := repository["byOther"].(map[string]interface{})
	otherHistory, _ := byOther["history"].(map[string]interface{})
	if otherHistory["totalCount"] != float64(0) {
		t.Fatalf("foreign-author history = %v", otherHistory)
	}
	byAuthor, _ := repository["byAuthor"].(map[string]interface{})
	authorHistory, _ := byAuthor["history"].(map[string]interface{})
	if authorHistory["totalCount"] != float64(2) {
		t.Fatalf("author-filtered history = %v", authorHistory)
	}
}

func TestGraphQLCommitReportsItsDiffStatsAndArchives(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	fixture := newGitGraphFixture(t, s)

	repository := fixture.query(t, s, defaultToken, `{
		object(expression:"HEAD"){
			... on Commit {
				additions
				deletions
				changedFiles
				changedFilesIfAvailable
				authoredByCommitter
				tarballUrl
				zipballUrl
				treeResourcePath
				resourcePath
			}
		}
	}`)
	commit, _ := repository["object"].(map[string]interface{})
	// The second commit appended two lines to src/lib.go and touched nothing else.
	if commit["additions"] != float64(2) || commit["deletions"] != float64(0) {
		t.Fatalf("additions/deletions = %v/%v", commit["additions"], commit["deletions"])
	}
	if commit["changedFiles"] != float64(1) || commit["changedFilesIfAvailable"] != float64(1) {
		t.Fatalf("changedFiles = %v/%v", commit["changedFiles"], commit["changedFilesIfAvailable"])
	}
	if commit["authoredByCommitter"] != true {
		t.Fatalf("authoredByCommitter = %v", commit["authoredByCommitter"])
	}
	if url, _ := commit["tarballUrl"].(string); !strings.HasSuffix(url, "/"+fixture.repo.FullName+"/legacy.tar.gz/"+fixture.headOID) {
		t.Fatalf("tarballUrl = %q", url)
	}
	if url, _ := commit["zipballUrl"].(string); !strings.HasSuffix(url, "/"+fixture.repo.FullName+"/legacy.zip/"+fixture.headOID) {
		t.Fatalf("zipballUrl = %q", url)
	}
	if commit["treeResourcePath"] != "/"+fixture.repo.FullName+"/tree/"+fixture.headOID {
		t.Fatalf("treeResourcePath = %v", commit["treeResourcePath"])
	}
	if commit["resourcePath"] != "/"+fixture.repo.FullName+"/commit/"+fixture.headOID {
		t.Fatalf("resourcePath = %v", commit["resourcePath"])
	}
}

func TestGraphQLRepositoryRefsPaginate(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	fixture := newGitGraphFixture(t, s)

	repository := fixture.query(t, s, defaultToken, `{
		refs(refPrefix:"refs/heads/", first:1){
			totalCount
			pageInfo { hasNextPage hasPreviousPage endCursor }
			edges { cursor node { name prefix target { oid } } }
			nodes { name }
		}
		tags: refs(refPrefix:"refs/tags/", first:10){
			totalCount
			nodes { name prefix target { __typename oid } }
		}
	}`)
	refs, _ := repository["refs"].(map[string]interface{})
	if refs["totalCount"] != float64(2) {
		t.Fatalf("refs totalCount = %v", refs["totalCount"])
	}
	pageInfo, _ := refs["pageInfo"].(map[string]interface{})
	if pageInfo["hasNextPage"] != true || pageInfo["hasPreviousPage"] != false {
		t.Fatalf("refs pageInfo = %v", pageInfo)
	}
	nodes, _ := refs["nodes"].([]interface{})
	if len(nodes) != 1 {
		t.Fatalf("refs first:1 returned %d nodes", len(nodes))
	}
	first, _ := nodes[0].(map[string]interface{})
	if first["name"] != "main" {
		t.Fatalf("first ref = %v", first)
	}
	edges, _ := refs["edges"].([]interface{})
	edge, _ := edges[0].(map[string]interface{})
	cursor, _ := edge["cursor"].(string)
	if cursor == "" {
		t.Fatal("ref edge has no cursor")
	}
	edgeTarget, _ := edge["node"].(map[string]interface{})["target"].(map[string]interface{})
	if edgeTarget["oid"] != fixture.headOID {
		t.Fatalf("main target = %v, want %s", edgeTarget, fixture.headOID)
	}

	// The cursor from the first page selects the second.
	owner, name, _ := store.SplitRepoFullName(fixture.repo.FullName)
	response := decodeJSONWithStatus(t, s.post(t, "/api/graphql", defaultToken, map[string]interface{}{
		"query": `query($owner:String!,$name:String!,$after:String!){
			repository(owner:$owner,name:$name){
				refs(refPrefix:"refs/heads/", first:5, after:$after){
					nodes { name target { oid } }
					pageInfo { hasNextPage hasPreviousPage }
				}
			}
		}`,
		"variables": map[string]interface{}{"owner": owner, "name": name, "after": cursor},
	}), 200)
	if errs := response["errors"]; errs != nil {
		t.Fatalf("GraphQL errors = %v", errs)
	}
	data, _ := response["data"].(map[string]interface{})
	page, _ := data["repository"].(map[string]interface{})["refs"].(map[string]interface{})
	pageNodes, _ := page["nodes"].([]interface{})
	if len(pageNodes) != 1 {
		t.Fatalf("second page = %v", pageNodes)
	}
	second, _ := pageNodes[0].(map[string]interface{})
	if second["name"] != "topic" {
		t.Fatalf("second ref = %v", second)
	}
	if target, _ := second["target"].(map[string]interface{}); target["oid"] != fixture.branchOID {
		t.Fatalf("topic target = %v, want %s", second["target"], fixture.branchOID)
	}
	if pageInfo, _ := page["pageInfo"].(map[string]interface{}); pageInfo["hasPreviousPage"] != true || pageInfo["hasNextPage"] != false {
		t.Fatalf("second page pageInfo = %v", page["pageInfo"])
	}

	tags, _ := repository["tags"].(map[string]interface{})
	if tags["totalCount"] != float64(1) {
		t.Fatalf("tag refs = %v", tags)
	}
	tagNodes, _ := tags["nodes"].([]interface{})
	tagRef, _ := tagNodes[0].(map[string]interface{})
	if tagRef["name"] != "v1.0.0" || tagRef["prefix"] != "refs/tags/" {
		t.Fatalf("tag ref = %v", tagRef)
	}
	// An annotated tag's ref points at the tag object, not the commit — the
	// same thing github.com reports.
	tagTarget, _ := tagRef["target"].(map[string]interface{})
	if tagTarget["__typename"] != "Tag" || tagTarget["oid"] != fixture.tagOID {
		t.Fatalf("tag ref target = %v", tagTarget)
	}
}

func TestGraphQLAnnotatedTagPeelsToItsCommit(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	fixture := newGitGraphFixture(t, s)

	repository := fixture.query(t, s, defaultToken, `{
		tag: object(expression:"v1.0.0"){
			__typename
			oid
			... on Tag {
				name
				message
				tagger { name email }
				target { __typename oid ... on Commit { messageHeadline } }
			}
		}
		peeled: object(expression:"v1.0.0^{}"){ __typename oid }
		throughPath: object(expression:"v1.0.0:src/lib.go"){ __typename ... on Blob { text } }
	}`)
	tag, _ := repository["tag"].(map[string]interface{})
	if tag["__typename"] != "Tag" || tag["oid"] != fixture.tagOID {
		t.Fatalf("tag object = %v", tag)
	}
	if tag["name"] != "v1.0.0" || tag["message"] != "release one\n" {
		t.Fatalf("tag name/message = %v/%v", tag["name"], tag["message"])
	}
	if tagger, _ := tag["tagger"].(map[string]interface{}); tagger["name"] != "admin" {
		t.Fatalf("tagger = %v", tag["tagger"])
	}
	target, _ := tag["target"].(map[string]interface{})
	if target["__typename"] != "Commit" || target["oid"] != fixture.headOID {
		t.Fatalf("tag target = %v", target)
	}
	if target["messageHeadline"] != "extend lib" {
		t.Fatalf("tag target headline = %v", target["messageHeadline"])
	}
	peeled, _ := repository["peeled"].(map[string]interface{})
	if peeled["__typename"] != "Commit" || peeled["oid"] != fixture.headOID {
		t.Fatalf("v1.0.0^{} = %v", peeled)
	}
	// A path lookup through an annotated tag peels the tag, then the commit.
	blob, _ := repository["throughPath"].(map[string]interface{})
	if blob["__typename"] != "Blob" || blob["text"] != "package lib\n\nfunc Go() {}\n" {
		t.Fatalf("v1.0.0:src/lib.go = %v", blob)
	}
}

func TestGraphQLBinaryBlobReturnsNullText(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	fixture := newGitGraphFixture(t, s)

	repository := fixture.query(t, s, defaultToken, `{
		object(expression:"HEAD:assets/logo.bin"){
			__typename
			... on Blob { text isBinary byteSize }
		}
	}`)
	blob, _ := repository["object"].(map[string]interface{})
	if blob["__typename"] != "Blob" {
		t.Fatalf("binary object = %v", blob)
	}
	if blob["text"] != nil {
		t.Fatalf("binary blob text = %v, want null", blob["text"])
	}
	if blob["isBinary"] != true {
		t.Fatalf("isBinary = %v", blob["isBinary"])
	}
	if blob["byteSize"] != float64(len(gitGraphBinaryContent)) {
		t.Fatalf("byteSize = %v", blob["byteSize"])
	}
}

func TestGraphQLObjectExpressionRevisionForms(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	fixture := newGitGraphFixture(t, s)

	repository := fixture.query(t, s, defaultToken, `{
		head:        object(expression:"HEAD"){ __typename oid }
		at:          object(expression:"@"){ oid }
		branch:      object(expression:"main"){ oid }
		fullRef:     object(expression:"refs/heads/main"){ oid }
		shortRef:    object(expression:"heads/topic"){ oid }
		fullSHA:     object(expression:"`+fixture.headOID+`"){ oid }
		abbrevSHA:   object(expression:"`+fixture.headOID[:8]+`"){ oid }
		firstParent: object(expression:"HEAD~1"){ oid }
		caret:       object(expression:"HEAD^"){ oid }
		selfPeel:    object(expression:"HEAD^0"){ oid }
		rootTree:    object(expression:"HEAD:"){ __typename }
		treeSpec:    object(expression:"HEAD^{tree}"){ __typename }
		subTree:     object(expression:"HEAD:src"){ __typename }
		nestedBlob:  object(expression:"HEAD:src/deep/one.txt"){ __typename ... on Blob { text } }
		subTreePaths: object(expression:"HEAD:src"){ ... on Tree { entries { name path } } }
		byOID:       object(oid:"`+fixture.firstOID+`"){ oid }
		missingRef:  object(expression:"no-such-branch"){ oid }
		missingPath: object(expression:"HEAD:nope.txt"){ oid }
		trailing:    object(expression:"HEAD:src/"){ oid }
		emptyRev:    object(expression:":README.md"){ oid }
	}`)
	oid := func(key string) interface{} {
		value, _ := repository[key].(map[string]interface{})
		if value == nil {
			return nil
		}
		return value["oid"]
	}
	typename := func(key string) interface{} {
		value, _ := repository[key].(map[string]interface{})
		if value == nil {
			return nil
		}
		return value["__typename"]
	}
	for _, check := range []struct {
		key  string
		want interface{}
	}{
		{"head", fixture.headOID},
		{"at", fixture.headOID},
		{"branch", fixture.headOID},
		{"fullRef", fixture.headOID},
		{"shortRef", fixture.branchOID},
		{"fullSHA", fixture.headOID},
		{"abbrevSHA", fixture.headOID},
		{"firstParent", fixture.firstOID},
		{"caret", fixture.firstOID},
		{"selfPeel", fixture.headOID},
		{"byOID", fixture.firstOID},
		{"missingRef", nil},
		{"missingPath", nil},
		{"trailing", nil},
		{"emptyRev", nil},
	} {
		if got := oid(check.key); got != check.want {
			t.Fatalf("%s oid = %v, want %v", check.key, got, check.want)
		}
	}
	if typename("head") != "Commit" {
		t.Fatalf("HEAD typename = %v", typename("head"))
	}
	for _, key := range []string{"rootTree", "treeSpec", "subTree"} {
		if typename(key) != "Tree" {
			t.Fatalf("%s typename = %v, want Tree", key, typename(key))
		}
	}
	nested, _ := repository["nestedBlob"].(map[string]interface{})
	if nested["__typename"] != "Blob" || nested["text"] != "one\n" {
		t.Fatalf("HEAD:src/deep/one.txt = %v", nested)
	}
	// A subtree reached through <rev>:<path> reports repository-relative
	// entry paths, not names relative to the subtree.
	subTree, _ := repository["subTreePaths"].(map[string]interface{})
	subEntries, _ := subTree["entries"].([]interface{})
	subPaths := map[string]string{}
	for _, raw := range subEntries {
		entry, _ := raw.(map[string]interface{})
		name, _ := entry["name"].(string)
		path, _ := entry["path"].(string)
		subPaths[name] = path
	}
	if subPaths["lib.go"] != "src/lib.go" || subPaths["deep"] != "src/deep" {
		t.Fatalf("HEAD:src entry paths = %v", subPaths)
	}
}

func TestGraphQLGitObjectsAreRefetchableByNodeID(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	fixture := newGitGraphFixture(t, s)

	repository := fixture.query(t, s, defaultToken, `{
		commit: object(expression:"HEAD"){ id }
		blob:   object(expression:"HEAD:README.md"){ id }
		tree:   object(expression:"HEAD:"){ id }
		tag:    object(expression:"v1.0.0"){ id }
		ref(qualifiedName:"refs/heads/main"){ id }
	}`)
	id := func(key string) string {
		value, _ := repository[key].(map[string]interface{})
		text, _ := value["id"].(string)
		if text == "" {
			t.Fatalf("%s has no id: %v", key, value)
		}
		return text
	}
	ids := []interface{}{id("commit"), id("blob"), id("tree"), id("tag"), id("ref")}

	response := decodeJSONWithStatus(t, s.post(t, "/api/graphql", defaultToken, map[string]interface{}{
		"query": `query($ids:[ID!]!){nodes(ids:$ids){
			__typename
			... on Commit { oid }
			... on Blob { oid byteSize }
			... on Tree { oid }
			... on Tag { name }
			... on Ref { name }
		}}`,
		"variables": map[string]interface{}{"ids": ids},
	}), 200)
	if errs := response["errors"]; errs != nil {
		t.Fatalf("GraphQL errors = %v", errs)
	}
	data, _ := response["data"].(map[string]interface{})
	nodes, _ := data["nodes"].([]interface{})
	if len(nodes) != 5 {
		t.Fatalf("nodes = %v", nodes)
	}
	want := []string{"Commit", "Blob", "Tree", "Tag", "Ref"}
	for i, expected := range want {
		node, _ := nodes[i].(map[string]interface{})
		if node == nil || node["__typename"] != expected {
			t.Fatalf("nodes[%d] = %v, want %s", i, node, expected)
		}
	}
	commit, _ := nodes[0].(map[string]interface{})
	if commit["oid"] != fixture.headOID {
		t.Fatalf("refetched commit = %v", commit)
	}
	tag, _ := nodes[3].(map[string]interface{})
	if tag["name"] != "v1.0.0" {
		t.Fatalf("refetched tag = %v", tag)
	}
	ref, _ := nodes[4].(map[string]interface{})
	if ref["name"] != "main" {
		t.Fatalf("refetched ref = %v", ref)
	}
}

func TestGraphQLPrivateRepositoryObjectGraphIsHiddenFromAStranger(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	fixture := newGQLAuthzFixture(t, s.Server, "git-objects", true)

	selection := `{
		object(expression:"HEAD:README.md"){ __typename ... on Blob { text } }
		ref(qualifiedName:"refs/heads/main"){ name target { oid } }
		refs(refPrefix:"refs/heads/", first:5){ totalCount nodes { name } }
	}`
	run := func(token string) map[string]interface{} {
		t.Helper()
		return decodeJSONWithStatus(t, s.post(t, "/api/graphql", token, map[string]interface{}{
			"query": `query($owner:String!,$name:String!){repository(owner:$owner,name:$name)` + selection + `}`,
			"variables": map[string]interface{}{
				"owner": fixture.owner.Login,
				"name":  fixture.repo.Name,
			},
		}), 200)
	}

	ownerResponse := run(fixture.ownerToken)
	if errs := ownerResponse["errors"]; errs != nil {
		t.Fatalf("GraphQL errors = %v", errs)
	}
	owned, _ := ownerResponse["data"].(map[string]interface{})
	repository, _ := owned["repository"].(map[string]interface{})
	if repository == nil {
		t.Fatalf("the owner cannot read their own repository: %v", owned)
	}
	blob, _ := repository["object"].(map[string]interface{})
	if blob["__typename"] != "Blob" || blob["text"] != "base\n" {
		t.Fatalf("owner blob = %v", blob)
	}
	if refs, _ := repository["refs"].(map[string]interface{}); refs["totalCount"] == float64(0) {
		t.Fatalf("owner refs = %v", repository["refs"])
	}

	// A stranger gets GitHub's NOT_FOUND masking, not the object graph.
	strangerResponse := run(fixture.strangerToken)
	stranger, _ := strangerResponse["data"].(map[string]interface{})
	if repository := stranger["repository"]; repository != nil {
		t.Fatalf("a stranger read a private repository's object graph: %v", repository)
	}
	errs, _ := strangerResponse["errors"].([]interface{})
	if len(errs) != 1 {
		t.Fatalf("stranger errors = %v", strangerResponse["errors"])
	}
	if first, _ := errs[0].(map[string]interface{}); first["type"] != "NOT_FOUND" {
		t.Fatalf("stranger error = %v", errs[0])
	}

	// The object graph is also unreachable by refetching a global id taken
	// from the owner's response.
	blobID, _ := blob["id"].(string)
	if blobID == "" {
		blobID = gitObjectNodeIDForTest(t, s, fixture.repo, "HEAD:README.md")
	}
	response := decodeJSONWithStatus(t, s.post(t, "/api/graphql", fixture.strangerToken, map[string]interface{}{
		"query":     `query($id:ID!){node(id:$id){__typename}}`,
		"variables": map[string]interface{}{"id": blobID},
	}), 200)
	if errs := response["errors"]; errs != nil {
		t.Fatalf("GraphQL errors = %v", errs)
	}
	data, _ := response["data"].(map[string]interface{})
	if node := data["node"]; node != nil {
		t.Fatalf("a stranger refetched a private repository's blob: %v", node)
	}
}

// gitObjectNodeIDForTest renders the global id of the object an expression
// names, without going through a query the test is about to assert on.
func gitObjectNodeIDForTest(t *testing.T, s *isolatedServer, repo *store.Repo, expression string) string {
	t.Helper()
	owner, name, ok := store.SplitRepoFullName(repo.FullName)
	if !ok {
		t.Fatalf("invalid repository name %q", repo.FullName)
	}
	stor := s.store.GetGitStorage(owner, name)
	revision, err := store.ResolveGitRevision(stor, expression)
	if err != nil {
		t.Fatalf("resolve %q: %v", expression, err)
	}
	return store.GitObjectNodeID(store.GitObjectNodeIDPrefixForType(revision.Type), repo.ID, revision.Hash.String())
}
