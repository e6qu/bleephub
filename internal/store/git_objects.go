package store

import (
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/plumbing/object"
	gitStorage "github.com/go-git/go-git/v5/storage"
)

// Git object-graph resolution shared by the REST handlers and the GraphQL
// resolver layer. These depend only on go-git storage.

var (
	ErrGitTreeishNotFound = errors.New("git treeish not found")
	// ErrGitTreeishInvalidObject: a revision resolved to an object that cannot
	// answer the request (a blob where a tree is required).
	ErrGitTreeishInvalidObject = errors.New("git treeish must identify a commit or tree")
)

// ValidGitObjectID reports whether value is a full-length hex object id.
func ValidGitObjectID(value string) bool {
	if len(value) != len(plumbing.ZeroHash.String()) {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

// ResolveGitObjectReference resolves a ref name to the hash it records without
// peeling annotated tags, trying `refs/...` verbatim, the HEAD symref, the
// `heads/`/`tags/` shorthands, then branch and tag short names. found is false
// when no reference carries the name (the caller may then read it as an object id).
func ResolveGitObjectReference(stor gitStorage.Storer, value string) (plumbing.Hash, bool, error) {
	if stor == nil {
		return plumbing.ZeroHash, false, nil
	}
	names := make([]plumbing.ReferenceName, 0, 4)
	add := func(name plumbing.ReferenceName) {
		for _, existing := range names {
			if existing == name {
				return
			}
		}
		names = append(names, name)
	}
	if strings.HasPrefix(value, "refs/") {
		add(plumbing.ReferenceName(value))
	} else {
		// Try HEAD first, like git, so a stray refs/heads/HEAD cannot shadow the
		// symref (github.com answers GET /git/trees/HEAD from the default branch).
		if value == string(plumbing.HEAD) {
			add(plumbing.HEAD)
		}
		if strings.HasPrefix(value, "heads/") || strings.HasPrefix(value, "tags/") {
			add(plumbing.ReferenceName("refs/" + value))
		}
		add(plumbing.NewBranchReferenceName(value))
		add(plumbing.NewTagReferenceName(value))
	}
	for _, name := range names {
		ref, err := stor.Reference(name)
		if err != nil {
			continue
		}
		hash, err := ResolvedReferenceHash(stor, ref, map[plumbing.ReferenceName]bool{})
		return hash, true, err
	}
	return plumbing.ZeroHash, false, nil
}

// ResolvedReferenceHash follows a symref chain to the hash at its end. seen
// breaks a cycle.
func ResolvedReferenceHash(stor gitStorage.Storer, ref *plumbing.Reference, seen map[plumbing.ReferenceName]bool) (plumbing.Hash, error) {
	if ref == nil || seen[ref.Name()] {
		return plumbing.ZeroHash, ErrGitTreeishNotFound
	}
	seen[ref.Name()] = true
	if ref.Type() == plumbing.HashReference {
		return ref.Hash(), nil
	}
	if ref.Type() != plumbing.SymbolicReference {
		return plumbing.ZeroHash, ErrGitTreeishNotFound
	}
	target, err := stor.Reference(ref.Target())
	if err != nil {
		return plumbing.ZeroHash, err
	}
	return ResolvedReferenceHash(stor, target, seen)
}

// PeelGitTagObjects follows annotated tag objects to the first non-tag object.
func PeelGitTagObjects(stor gitStorage.Storer, hash plumbing.Hash) (plumbing.Hash, error) {
	seen := map[plumbing.Hash]bool{}
	for {
		if hash.IsZero() || seen[hash] {
			return plumbing.ZeroHash, ErrGitTreeishNotFound
		}
		seen[hash] = true
		encoded, err := stor.EncodedObject(plumbing.AnyObject, hash)
		if err != nil {
			return plumbing.ZeroHash, err
		}
		if encoded.Type() != plumbing.TagObject {
			return hash, nil
		}
		tag, err := object.GetTag(stor, hash)
		if err != nil {
			return plumbing.ZeroHash, err
		}
		hash = tag.Target
	}
}

// ResolveGitTreeish implements GitHub's broader tree_sha contract: a tree SHA,
// commit SHA, or branch/tag name. References are dereferenced (including
// annotated tags), while a raw tag-object SHA is rejected as on github.com.
func ResolveGitTreeish(stor gitStorage.Storer, value string) (plumbing.Hash, *object.Tree, error) {
	value = strings.Trim(value, "/")
	if value == "" {
		return plumbing.ZeroHash, nil, ErrGitTreeishNotFound
	}

	hash, found, err := ResolveGitObjectReference(stor, value)
	if err != nil {
		return plumbing.ZeroHash, nil, err
	}
	if found {
		hash, err = PeelGitTagObjects(stor, hash)
		if err != nil {
			return plumbing.ZeroHash, nil, ErrGitTreeishNotFound
		}
	} else {
		if !ValidGitObjectID(value) {
			return plumbing.ZeroHash, nil, ErrGitTreeishNotFound
		}
		hash = plumbing.NewHash(value)
	}

	encoded, err := stor.EncodedObject(plumbing.AnyObject, hash)
	if err != nil {
		return plumbing.ZeroHash, nil, ErrGitTreeishNotFound
	}
	switch encoded.Type() {
	case plumbing.TreeObject:
		tree, err := object.GetTree(stor, hash)
		if err != nil {
			return plumbing.ZeroHash, nil, ErrGitTreeishNotFound
		}
		return hash, tree, nil
	case plumbing.CommitObject:
		commit, err := object.GetCommit(stor, hash)
		if err != nil {
			return plumbing.ZeroHash, nil, ErrGitTreeishNotFound
		}
		tree, err := commit.Tree()
		if err != nil {
			return plumbing.ZeroHash, nil, ErrGitTreeishNotFound
		}
		// GitHub identifies the response by the commit SHA, though the entries
		// come from that commit's root tree.
		return hash, tree, nil
	default:
		return plumbing.ZeroHash, nil, ErrGitTreeishInvalidObject
	}
}

// OpenGitBlob streams a blob's contents and reports its size, for raw file
// serving where a blob may exceed the per-request memory ReadGitBlob would use.
// The caller closes the reader.
func OpenGitBlob(stor gitStorage.Storer, hash plumbing.Hash) (io.ReadCloser, int64, error) {
	blob, err := object.GetBlob(stor, hash)
	if err != nil {
		return nil, 0, err
	}
	reader, err := blob.Reader()
	if err != nil {
		return nil, 0, err
	}
	return reader, blob.Size, nil
}

func ReadGitBlob(stor gitStorage.Storer, hash plumbing.Hash) ([]byte, error) {
	blob, err := object.GetBlob(stor, hash)
	if err != nil {
		return nil, err
	}
	reader, err := blob.Reader()
	if err != nil {
		return nil, err
	}
	defer reader.Close()
	return io.ReadAll(reader)
}

// CommitTouchesPath reports whether a commit changed the file or directory at
// requested against its first parent (or, for a root commit, contains it).
func CommitTouchesPath(commit *object.Commit, requested string) (bool, error) {
	matches := func(candidate string) bool {
		return candidate == requested || strings.HasPrefix(candidate, requested+"/")
	}
	tree, err := commit.Tree()
	if err != nil {
		return false, err
	}
	if commit.NumParents() == 0 {
		found := false
		walker := object.NewTreeWalker(tree, true, nil)
		defer walker.Close()
		for {
			name, _, err := walker.Next()
			if errors.Is(err, io.EOF) {
				return found, nil
			}
			if err != nil {
				return false, err
			}
			if matches(name) {
				found = true
			}
		}
	}
	parent, err := commit.Parent(0)
	if err != nil {
		return false, err
	}
	parentTree, err := parent.Tree()
	if err != nil {
		return false, err
	}
	changes, err := object.DiffTree(parentTree, tree)
	if err != nil {
		return false, err
	}
	for _, change := range changes {
		if matches(change.From.Name) || matches(change.To.Name) {
			return true, nil
		}
	}
	return false, nil
}

// GitCommitDiffStats returns the additions, deletions and changed-file count a
// commit introduced against its first parent. A root commit is measured against
// the empty tree, matching `git show --stat`.
func GitCommitDiffStats(commit *object.Commit) (additions, deletions, changedFiles int, err error) {
	tree, err := commit.Tree()
	if err != nil {
		return 0, 0, 0, err
	}
	var parentTree *object.Tree
	if commit.NumParents() > 0 {
		parent, err := commit.Parent(0)
		if err != nil {
			return 0, 0, 0, err
		}
		parentTree, err = parent.Tree()
		if err != nil {
			return 0, 0, 0, err
		}
	} else {
		parentTree = &object.Tree{}
	}
	changes, err := object.DiffTree(parentTree, tree)
	if err != nil {
		return 0, 0, 0, err
	}
	for _, change := range changes {
		patch, err := change.Patch()
		if err != nil {
			return 0, 0, 0, err
		}
		stats := patch.Stats()
		if len(stats) == 0 {
			continue
		}
		if change.From.TreeEntry.Hash == change.To.TreeEntry.Hash &&
			change.From.TreeEntry.Mode == change.To.TreeEntry.Mode {
			continue
		}
		additions += stats[0].Addition
		deletions += stats[0].Deletion
		changedFiles++
	}
	return additions, deletions, changedFiles, nil
}

func GitObjectTypeOf(stor gitStorage.Storer, hash plumbing.Hash) (plumbing.ObjectType, error) {
	if stor == nil {
		return plumbing.InvalidObject, ErrGitTreeishNotFound
	}
	encoded, err := stor.EncodedObject(plumbing.AnyObject, hash)
	if err != nil {
		return plumbing.InvalidObject, ErrGitTreeishNotFound
	}
	return encoded.Type(), nil
}

// GitRevision is a resolved rev-parse result: the named object and its stored type.
type GitRevision struct {
	Hash plumbing.Hash
	Type plumbing.ObjectType
}

// ResolveGitRevision resolves the `git rev-parse` grammar GitHub's
// Repository.object(expression:) accepts:
//
//	HEAD, @                     the repository's checked-out branch
//	main, v1.0                  a branch or tag short name
//	refs/heads/main             a fully qualified reference
//	heads/main, tags/v1.0       the reference shorthands
//	3f2a1b9…                    a full object id
//	3f2a1b                      an unambiguous abbreviated object id
//	<rev>~, <rev>~3             first-parent ancestry
//	<rev>^, <rev>^2, <rev>^0    the nth parent (^0 peels to the commit)
//	<rev>^{}, <rev>^{commit}    peel an annotated tag; ^{tree}, ^{blob}, ^{tag}
//	<rev>:<path>                the tree entry at path within <rev>'s tree
//	<rev>:                      <rev>'s root tree
//
// A ref resolves without peeling, so an annotated tag's name resolves to the
// tag object, as on github.com.
func ResolveGitRevision(stor gitStorage.Storer, expression string) (GitRevision, error) {
	if stor == nil || expression == "" {
		return GitRevision{}, ErrGitTreeishNotFound
	}
	// A ref name contains no `:`, `~` or `^` (git-check-ref-format), so the first
	// colon separates the revision from the path.
	rev, path, hasPath := strings.Cut(expression, ":")
	base, err := resolveGitRevisionSpec(stor, rev)
	if err != nil {
		return GitRevision{}, err
	}
	if !hasPath {
		return base, nil
	}
	return gitRevisionAtPath(stor, base, path)
}

// resolveGitRevisionSpec resolves the pre-path part of an expression: a base
// name plus any ancestry or peeling operators.
func resolveGitRevisionSpec(stor gitStorage.Storer, rev string) (GitRevision, error) {
	if rev == "" {
		return GitRevision{}, ErrGitTreeishNotFound
	}
	name, operators := rev, ""
	if _, ok := resolveGitRevisionBase(stor, rev); !ok {
		// The whole spelling names nothing; operators start at the first char git
		// forbids in a ref name.
		index := strings.IndexAny(rev, "^~")
		if index <= 0 {
			return GitRevision{}, ErrGitTreeishNotFound
		}
		name, operators = rev[:index], rev[index:]
	}
	hash, ok := resolveGitRevisionBase(stor, name)
	if !ok {
		return GitRevision{}, ErrGitTreeishNotFound
	}
	for operators != "" {
		next, rest, err := applyGitRevisionOperator(stor, hash, operators)
		if err != nil {
			return GitRevision{}, err
		}
		hash, operators = next, rest
	}
	objectType, err := GitObjectTypeOf(stor, hash)
	if err != nil {
		return GitRevision{}, err
	}
	return GitRevision{Hash: hash, Type: objectType}, nil
}

// resolveGitRevisionBase resolves a bare revision name — a reference, a HEAD
// alias, a full object id, or an unambiguous abbreviated one.
func resolveGitRevisionBase(stor gitStorage.Storer, name string) (plumbing.Hash, bool) {
	if name == "" {
		return plumbing.ZeroHash, false
	}
	// `@` aliases HEAD.
	if name == "@" {
		name = string(plumbing.HEAD)
	}
	hash, found, err := ResolveGitObjectReference(stor, name)
	if err == nil && found && !hash.IsZero() {
		if _, err := GitObjectTypeOf(stor, hash); err == nil {
			return hash, true
		}
	}
	if !isHexString(name) {
		return plumbing.ZeroHash, false
	}
	if ValidGitObjectID(name) {
		hash := plumbing.NewHash(name)
		if _, err := GitObjectTypeOf(stor, hash); err == nil {
			return hash, true
		}
		return plumbing.ZeroHash, false
	}
	// git requires at least four chars of an abbreviated object id, and rejects
	// a prefix matching more than one object.
	if len(name) < 4 {
		return plumbing.ZeroHash, false
	}
	return resolveAbbreviatedGitOID(stor, name)
}

func isHexString(value string) bool {
	if value == "" {
		return false
	}
	for _, r := range value {
		switch {
		case r >= '0' && r <= '9':
		case r >= 'a' && r <= 'f':
		case r >= 'A' && r <= 'F':
		default:
			return false
		}
	}
	return true
}

func resolveAbbreviatedGitOID(stor gitStorage.Storer, prefix string) (plumbing.Hash, bool) {
	iter, err := stor.IterEncodedObjects(plumbing.AnyObject)
	if err != nil {
		return plumbing.ZeroHash, false
	}
	defer iter.Close()
	prefix = strings.ToLower(prefix)
	matches := 0
	found := plumbing.ZeroHash
	_ = iter.ForEach(func(encoded plumbing.EncodedObject) error {
		if strings.HasPrefix(encoded.Hash().String(), prefix) {
			matches++
			found = encoded.Hash()
		}
		return nil
	})
	if matches != 1 {
		return plumbing.ZeroHash, false
	}
	return found, true
}

// applyGitRevisionOperator consumes one leading `~`/`^` operator and returns
// the object it selects plus the remaining operators.
func applyGitRevisionOperator(stor gitStorage.Storer, hash plumbing.Hash, operators string) (plumbing.Hash, string, error) {
	switch operators[0] {
	case '~':
		count, rest := gitRevisionOperatorCount(operators[1:])
		for i := 0; i < count; i++ {
			next, err := gitCommitParent(stor, hash, 1)
			if err != nil {
				return plumbing.ZeroHash, "", err
			}
			hash = next
		}
		return hash, rest, nil
	case '^':
		if strings.HasPrefix(operators[1:], "{") {
			end := strings.IndexByte(operators, '}')
			if end < 0 {
				return plumbing.ZeroHash, "", ErrGitTreeishNotFound
			}
			peeled, err := peelGitRevisionToType(stor, hash, operators[2:end])
			if err != nil {
				return plumbing.ZeroHash, "", err
			}
			return peeled, operators[end+1:], nil
		}
		count, rest := gitRevisionOperatorCount(operators[1:])
		if count == 0 {
			// `<rev>^0` names the commit itself, peeling an annotated tag.
			peeled, err := peelGitRevisionToType(stor, hash, "commit")
			if err != nil {
				return plumbing.ZeroHash, "", err
			}
			return peeled, rest, nil
		}
		next, err := gitCommitParent(stor, hash, count)
		if err != nil {
			return plumbing.ZeroHash, "", err
		}
		return next, rest, nil
	default:
		return plumbing.ZeroHash, "", ErrGitTreeishNotFound
	}
}

// gitRevisionOperatorCount reads the optional decimal count after `~` or `^`;
// absent means one, matching git.
func gitRevisionOperatorCount(operators string) (int, string) {
	end := 0
	for end < len(operators) && operators[end] >= '0' && operators[end] <= '9' {
		end++
	}
	if end == 0 {
		return 1, operators
	}
	count, err := strconv.Atoi(operators[:end])
	if err != nil {
		return 1, operators[end:]
	}
	return count, operators[end:]
}

// gitCommitParent peels hash to a commit and returns its nth parent (1-based).
func gitCommitParent(stor gitStorage.Storer, hash plumbing.Hash, n int) (plumbing.Hash, error) {
	peeled, err := PeelGitTagObjects(stor, hash)
	if err != nil {
		return plumbing.ZeroHash, ErrGitTreeishNotFound
	}
	commit, err := object.GetCommit(stor, peeled)
	if err != nil {
		return plumbing.ZeroHash, ErrGitTreeishNotFound
	}
	if n < 1 || n > commit.NumParents() {
		return plumbing.ZeroHash, ErrGitTreeishNotFound
	}
	return commit.ParentHashes[n-1], nil
}

// peelGitRevisionToType implements `<rev>^{<type>}`: an empty type peels
// annotated tags, a named type peels until the object has that type.
func peelGitRevisionToType(stor gitStorage.Storer, hash plumbing.Hash, want string) (plumbing.Hash, error) {
	peeled, err := PeelGitTagObjects(stor, hash)
	switch want {
	case "":
		if err != nil {
			return plumbing.ZeroHash, ErrGitTreeishNotFound
		}
		return peeled, nil
	case "tag":
		objectType, err := GitObjectTypeOf(stor, hash)
		if err != nil || objectType != plumbing.TagObject {
			return plumbing.ZeroHash, ErrGitTreeishNotFound
		}
		return hash, nil
	case "commit":
		if err != nil {
			return plumbing.ZeroHash, ErrGitTreeishNotFound
		}
		if objectType, err := GitObjectTypeOf(stor, peeled); err != nil || objectType != plumbing.CommitObject {
			return plumbing.ZeroHash, ErrGitTreeishNotFound
		}
		return peeled, nil
	case "tree":
		if err != nil {
			return plumbing.ZeroHash, ErrGitTreeishNotFound
		}
		return gitTreeHashOf(stor, peeled)
	case "blob":
		if err != nil {
			return plumbing.ZeroHash, ErrGitTreeishNotFound
		}
		if objectType, err := GitObjectTypeOf(stor, peeled); err != nil || objectType != plumbing.BlobObject {
			return plumbing.ZeroHash, ErrGitTreeishNotFound
		}
		return peeled, nil
	default:
		return plumbing.ZeroHash, ErrGitTreeishNotFound
	}
}

// gitTreeHashOf returns the tree a commit roots, or the tree itself.
func gitTreeHashOf(stor gitStorage.Storer, hash plumbing.Hash) (plumbing.Hash, error) {
	objectType, err := GitObjectTypeOf(stor, hash)
	if err != nil {
		return plumbing.ZeroHash, ErrGitTreeishNotFound
	}
	switch objectType {
	case plumbing.TreeObject:
		return hash, nil
	case plumbing.CommitObject:
		commit, err := object.GetCommit(stor, hash)
		if err != nil {
			return plumbing.ZeroHash, ErrGitTreeishNotFound
		}
		return commit.TreeHash, nil
	default:
		return plumbing.ZeroHash, ErrGitTreeishInvalidObject
	}
}

// gitRevisionAtPath implements the `<rev>:<path>` form: peel the revision to
// its tree, then resolve path within it. An empty path names the root tree.
func gitRevisionAtPath(stor gitStorage.Storer, base GitRevision, path string) (GitRevision, error) {
	peeled, err := PeelGitTagObjects(stor, base.Hash)
	if err != nil {
		return GitRevision{}, ErrGitTreeishNotFound
	}
	treeHash, err := gitTreeHashOf(stor, peeled)
	if err != nil {
		return GitRevision{}, err
	}
	if path == "" {
		return GitRevision{Hash: treeHash, Type: plumbing.TreeObject}, nil
	}
	tree, err := object.GetTree(stor, treeHash)
	if err != nil {
		return GitRevision{}, ErrGitTreeishNotFound
	}
	entry, err := GitTreeEntryAtPath(tree, path)
	if err != nil {
		return GitRevision{}, err
	}
	objectType, err := GitObjectTypeOf(stor, entry.Hash)
	if err != nil {
		// A gitlink names a commit in the submodule's own repository, which
		// this repository does not contain.
		return GitRevision{}, ErrGitTreeishNotFound
	}
	return GitRevision{Hash: entry.Hash, Type: objectType}, nil
}

// GitTreeEntryAtPath resolves a slash-separated path within a tree. git
// rejects empty path components and trailing slashes, so this does too.
func GitTreeEntryAtPath(tree *object.Tree, path string) (*object.TreeEntry, error) {
	if tree == nil || path == "" {
		return nil, ErrGitTreeishNotFound
	}
	for _, component := range strings.Split(path, "/") {
		if component == "" {
			return nil, ErrGitTreeishNotFound
		}
	}
	entry, err := tree.FindEntry(path)
	if err != nil {
		return nil, ErrGitTreeishNotFound
	}
	return entry, nil
}

// GitTreeEntryType renders a tree entry's mode as the "blob"/"tree"/"commit"
// discriminator GitHub reports for TreeEntry.type and the git trees API.
func GitTreeEntryType(mode filemode.FileMode) string {
	switch {
	case mode == filemode.Submodule:
		return "commit"
	case mode == filemode.Dir:
		return "tree"
	default:
		return "blob"
	}
}

// GitBlobIsBinary applies git's heuristic: a NUL byte in the leading bytes means
// binary, which GitHub reports as Blob.text null.
func GitBlobIsBinary(content []byte) bool {
	const sniff = 8000
	head := content
	if len(head) > sniff {
		head = head[:sniff]
	}
	for _, b := range head {
		if b == 0 {
			return true
		}
	}
	return false
}

// AbbreviatedGitOID is the 7-character object-id prefix GitHub renders for
// abbreviatedOid and in archive directory names.
func AbbreviatedGitOID(oid string) string {
	if len(oid) < 7 {
		return oid
	}
	return oid[:7]
}

// Git object node ids encode (repository, object id): a git object id alone is
// not globally unique on a forge, since the same blob exists in every fork.
// GitHub encodes the same pair.

const (
	// Type discriminators GitHub puts at the head of a git object's global id.
	GitCommitNodeIDPrefix = "C"
	GitBlobNodeIDPrefix   = "B"
	GitTreeNodeIDPrefix   = "T"
	GitTagNodeIDPrefix    = "TA"
	GitRefNodeIDPrefix    = "REF"

	gitNodeIDInfix = "_kwDO"
)

// GitObjectNodeID renders a git object's global id.
func GitObjectNodeID(prefix string, repoID int, oid string) string {
	payload := base64.RawURLEncoding.EncodeToString([]byte(fmt.Sprintf("%d:%s", repoID, oid)))
	return prefix + gitNodeIDInfix + payload
}

// ParseGitObjectNodeID decodes a git object global id into its type prefix,
// repository and object id (or, for a Ref, its qualified name).
func ParseGitObjectNodeID(nodeID string) (prefix string, repoID int, value string, ok bool) {
	prefix, payload, found := strings.Cut(nodeID, gitNodeIDInfix)
	if !found || prefix == "" {
		return "", 0, "", false
	}
	decoded, err := base64.RawURLEncoding.DecodeString(payload)
	if err != nil {
		return "", 0, "", false
	}
	idText, value, found := strings.Cut(string(decoded), ":")
	if !found {
		return "", 0, "", false
	}
	repoID, err = strconv.Atoi(idText)
	if err != nil || repoID <= 0 || value == "" {
		return "", 0, "", false
	}
	return prefix, repoID, value, true
}

// GitObjectNodeIDPrefixForType maps a stored object type to its node id prefix.
func GitObjectNodeIDPrefixForType(objectType plumbing.ObjectType) string {
	switch objectType {
	case plumbing.CommitObject:
		return GitCommitNodeIDPrefix
	case plumbing.TreeObject:
		return GitTreeNodeIDPrefix
	case plumbing.BlobObject:
		return GitBlobNodeIDPrefix
	case plumbing.TagObject:
		return GitTagNodeIDPrefix
	default:
		return ""
	}
}

// ListGitReferences returns references under prefix, sorted by full name.
// Symrefs (HEAD) are excluded, matching Repository.refs(refPrefix:).
func ListGitReferences(stor gitStorage.Storer, prefix string) ([]*plumbing.Reference, error) {
	if stor == nil {
		return nil, ErrGitTreeishNotFound
	}
	iter, err := stor.IterReferences()
	if err != nil {
		return nil, err
	}
	defer iter.Close()
	var refs []*plumbing.Reference
	err = iter.ForEach(func(ref *plumbing.Reference) error {
		if ref.Type() != plumbing.HashReference {
			return nil
		}
		if prefix != "" && !strings.HasPrefix(ref.Name().String(), prefix) {
			return nil
		}
		refs = append(refs, ref)
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(refs, func(a, b int) bool { return refs[a].Name().String() < refs[b].Name().String() })
	return refs, nil
}
