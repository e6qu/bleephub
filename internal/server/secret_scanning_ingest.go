package bleephub

import (
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"
	"time"

	"github.com/e6qu/bleephub/internal/store"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/plumbing/storer"
)

// secretScanningMaxFileBytes caps how much of a git blob is read into memory
// when scanning; an unbounded read is a memory-exhaustion risk, and secrets are
// short and live near the top of config files.
const secretScanningMaxFileBytes = 5 << 20 // 5 MiB

type secretScanningPattern struct {
	patternID  string
	secretType string
	re         *regexp.Regexp
}

var secretScanningContentPatterns = []secretScanningPattern{
	{patternID: "ghp", secretType: "github_personal_access_token", re: regexp.MustCompile(`(?:ghp_[A-Za-z0-9_]{36,40}|github_pat_[A-Za-z0-9_]{40,128})`)},
	{patternID: "aws", secretType: "aws_access_key_id", re: regexp.MustCompile(`AKIA[0-9A-Z]{16}`)},
	{patternID: "google", secretType: "google_api_key", re: regexp.MustCompile(`AIza[0-9A-Za-z_-]{35}`)},
	{patternID: "slack", secretType: "slack_incoming_webhook_url", re: regexp.MustCompile(`https://hooks\.slack\.com/services/[A-Za-z0-9_/-]+`)},
}

type secretScanningContentMatch struct {
	patternID  string
	secretType string
}

func (s *Server) scanCommitForSecretScanning(repo *store.Repo, stor storer.Storer, commitHash plumbing.Hash, baseURL string) error {
	if !secretScanningEnabled(repo) {
		return nil
	}
	commit, err := object.GetCommit(stor, commitHash)
	if err != nil {
		return fmt.Errorf("load secret scanning commit %s: %w", commitHash, err)
	}
	tree, err := commit.Tree()
	if err != nil {
		return fmt.Errorf("load secret scanning tree %s: %w", commit.TreeHash, err)
	}
	files := tree.Files()
	defer files.Close()

	for {
		file, err := files.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("walk secret scanning tree %s: %w", commit.TreeHash, err)
		}
		body, err := readSecretScanningBlob(&file.Blob)
		if err != nil {
			return err
		}
		s.scanSecretScanningFile(repo, commitHash.String(), file.Name, file.Hash.String(), body, baseURL)
	}
	return nil
}

// scanRangeForSecretScanning scans the files that the commits reachable from
// target and not from old add or change relative to their first parent, each
// blob once however many commits carry it, and records every match against the
// commit that introduced it. The walk is bounded as push protection's is, and
// covers the newest commits first.
func (s *Server) scanRangeForSecretScanning(repo *store.Repo, stor storer.Storer, old, target plumbing.Hash, baseURL string) error {
	tip, err := object.GetCommit(stor, target)
	if err != nil {
		return fmt.Errorf("load secret scanning commit %s: %w", target, err)
	}
	iter := object.NewCommitPreorderIter(tip, nil, []plumbing.Hash{old})
	defer iter.Close()
	scanned := map[plumbing.Hash]bool{}
	commits := 0
	walkErr := iter.ForEach(func(c *object.Commit) error {
		commits++
		if commits > secretScanningPushProtectionMaxCommits {
			return storer.ErrStop
		}
		files, err := commitIntroducedFiles(c)
		if err != nil {
			return err
		}
		for _, file := range files {
			if scanned[file.hash] {
				continue
			}
			scanned[file.hash] = true
			blob, err := object.GetBlob(stor, file.hash)
			if err != nil {
				// A gitlink names a commit in another repository: nothing to read.
				continue
			}
			body, err := readSecretScanningBlob(blob)
			if err != nil {
				return err
			}
			s.scanSecretScanningFile(repo, c.Hash.String(), file.path, file.hash.String(), body, baseURL)
		}
		return nil
	})
	if walkErr != nil && !errors.Is(walkErr, storer.ErrStop) {
		return walkErr
	}
	return nil
}

// introducedFile is a path a commit adds or changes, and the blob it now holds.
type introducedFile struct {
	path string
	hash plumbing.Hash
}

// commitIntroducedFiles returns the files a commit adds or modifies relative to
// its first parent — a root commit's entire tree — with their paths.
func commitIntroducedFiles(c *object.Commit) ([]introducedFile, error) {
	changes, err := commitChanges(c)
	if err != nil {
		return nil, err
	}
	var out []introducedFile
	for _, ch := range changes {
		// The "to" side of an addition or modification; a deletion has mode 0.
		if ch.To.TreeEntry.Mode != 0 {
			out = append(out, introducedFile{path: ch.To.Name, hash: ch.To.TreeEntry.Hash})
		}
	}
	return out, nil
}

// readSecretScanningBlob reads as much of a blob as secret scanning looks at.
func readSecretScanningBlob(blob *object.Blob) (string, error) {
	reader, err := blob.Reader()
	if err != nil {
		return "", fmt.Errorf("read secret scanning blob %s: %w", blob.Hash, err)
	}
	body, readErr := io.ReadAll(io.LimitReader(reader, secretScanningMaxFileBytes))
	closeErr := reader.Close()
	if readErr != nil {
		return "", fmt.Errorf("read secret scanning blob %s: %w", blob.Hash, readErr)
	}
	if closeErr != nil {
		return "", fmt.Errorf("close secret scanning blob %s: %w", blob.Hash, closeErr)
	}
	return string(body), nil
}

func (s *Server) scanSecretScanningFile(repo *store.Repo, commitSHA, path, blobSHA, body, baseURL string) {
	for _, pattern := range secretScanningContentPatterns {
		for _, match := range pattern.re.FindAllStringIndex(body, -1) {
			startLine, startColumn, endLine, endColumn := secretScanningMatchPosition(body, match[0], match[1])
			location := store.SecretScanningLocation{
				Type: "commit",
				Details: store.SecretScanningLocationDetails{
					Path:        path,
					StartLine:   startLine,
					EndLine:     endLine,
					StartColumn: startColumn,
					EndColumn:   endColumn,
					BlobSHA:     blobSHA,
					BlobURL:     fmt.Sprintf("%s/api/v3/repos/%s/git/blobs/%s", baseURL, repo.FullName, blobSHA),
					CommitSHA:   commitSHA,
					CommitURL:   fmt.Sprintf("%s/api/v3/repos/%s/commits/%s", baseURL, repo.FullName, commitSHA),
					HTMLURL:     fmt.Sprintf("%s/%s/blob/%s/%s#L%d", baseURL, repo.FullName, commitSHA, path, startLine),
				},
			}
			s.store.CreateSecretScanningAlertIfNew(repo.FullName, pattern.secretType, []store.SecretScanningLocation{location})
		}
	}
}

func secretScanningContentMatches(body string) []secretScanningContentMatch {
	seen := map[string]bool{}
	var out []secretScanningContentMatch
	for _, pattern := range secretScanningContentPatterns {
		if !pattern.re.MatchString(body) {
			continue
		}
		if seen[pattern.secretType] {
			continue
		}
		seen[pattern.secretType] = true
		out = append(out, secretScanningContentMatch{patternID: pattern.patternID, secretType: pattern.secretType})
	}
	return out
}

// secretScanningPushProtectionMaxCommits bounds the commit walk a single push
// triggers. A push introducing more commits than this (a first push of a very
// long history, say) scans the newest commits up to the cap rather than turning
// receive-pack into an unbounded scan; the pre-order walk visits the push tip
// first, so it is the most recent commits that are covered.
const secretScanningPushProtectionMaxCommits = 2000

// secretScanningPushProtectionMatchesForRange scans the content a push of
// old..new introduces: for every commit reachable from new but not from old,
// the blobs it adds or modifies relative to its first parent (a root commit's
// whole tree). Scanning the introduced diff — rather than only the tip tree —
// blocks a secret committed in an intermediate commit of a multi-commit push
// even when a later commit in the same push deletes it (the secret still enters
// the repository's history), and conversely never blocks a push over a
// pre-existing secret sitting in a file the push does not touch.
func (s *Server) secretScanningPushProtectionMatchesForRange(stor storer.Storer, old, target plumbing.Hash) ([]secretScanningContentMatch, error) {
	newCommit, err := object.GetCommit(stor, target)
	if err != nil {
		// The tip is not a commit (e.g. a lightweight-tag target); nothing to scan.
		return nil, nil
	}

	var ignore []plumbing.Hash
	if !old.IsZero() {
		if _, err := object.GetCommit(stor, old); err == nil {
			ignore = []plumbing.Hash{old}
		}
	}
	iter := object.NewCommitPreorderIter(newCommit, nil, ignore)
	defer iter.Close()

	scannedBlobs := map[plumbing.Hash]bool{}
	seenTypes := map[string]bool{}
	var out []secretScanningContentMatch
	commits := 0
	walkErr := iter.ForEach(func(c *object.Commit) error {
		commits++
		if commits > secretScanningPushProtectionMaxCommits {
			return storer.ErrStop
		}
		blobs, err := commitIntroducedBlobs(c)
		if err != nil {
			return err
		}
		for _, h := range blobs {
			if scannedBlobs[h] {
				continue
			}
			scannedBlobs[h] = true
			matches, err := s.scanBlobForSecretMatches(stor, h, seenTypes)
			if err != nil {
				return err
			}
			out = append(out, matches...)
		}
		return nil
	})
	if walkErr != nil && walkErr != storer.ErrStop {
		return nil, walkErr
	}
	return out, nil
}

// commitIntroducedBlobs returns the hashes of the blobs a commit adds or
// modifies relative to its first parent — a root commit's entire tree.
func commitIntroducedBlobs(c *object.Commit) ([]plumbing.Hash, error) {
	files, err := commitIntroducedFiles(c)
	if err != nil {
		return nil, err
	}
	out := make([]plumbing.Hash, 0, len(files))
	for _, file := range files {
		out = append(out, file.hash)
	}
	return out, nil
}

// commitChanges diffs a commit's tree against its first parent's, or against an
// empty tree for a root commit. Equal subtrees are skipped by hash, so the cost
// follows the size of the change, not of the tree.
func commitChanges(c *object.Commit) (object.Changes, error) {
	headTree, err := c.Tree()
	if err != nil {
		return nil, fmt.Errorf("load secret scanning tree %s: %w", c.TreeHash, err)
	}
	parentTree := &object.Tree{}
	if c.NumParents() > 0 {
		parent, err := c.Parent(0)
		if err != nil {
			return nil, fmt.Errorf("load secret scanning parent of %s: %w", c.Hash, err)
		}
		parentTree, err = parent.Tree()
		if err != nil {
			return nil, fmt.Errorf("load secret scanning parent tree %s: %w", parent.TreeHash, err)
		}
	}
	changes, err := object.DiffTree(parentTree, headTree)
	if err != nil {
		return nil, fmt.Errorf("diff secret scanning commit %s: %w", c.Hash, err)
	}
	return changes, nil
}

// scanBlobForSecretMatches reads one blob (bounded) and returns the secret
// patterns it contains whose type is not already in seenTypes, recording those
// it reports so a repeated type is surfaced once per push. A hash that is not a
// readable blob (a submodule gitlink, say) contributes nothing.
func (s *Server) scanBlobForSecretMatches(stor storer.Storer, hash plumbing.Hash, seenTypes map[string]bool) ([]secretScanningContentMatch, error) {
	blob, err := object.GetBlob(stor, hash)
	if err != nil {
		return nil, nil
	}
	body, err := readSecretScanningBlob(blob)
	if err != nil {
		return nil, err
	}
	var out []secretScanningContentMatch
	for _, match := range secretScanningContentMatches(body) {
		if seenTypes[match.secretType] {
			continue
		}
		seenTypes[match.secretType] = true
		out = append(out, match)
	}
	return out, nil
}

func (s *Server) createSecretScanningPushProtectionPlaceholder(repo *store.Repo, matches []secretScanningContentMatch) *store.SecretScanningPushProtectionPlaceholder {
	for _, match := range matches {
		if !s.store.SecretScanningPushProtectionEnabled(repo, match.patternID) {
			continue
		}
		if s.store.HasActiveSecretScanningPushProtectionBypass(repo.FullName, match.secretType, time.Now().UTC()) {
			continue
		}
		return s.store.CreateSecretScanningPushProtectionPlaceholder(repo.FullName, match.secretType)
	}
	return nil
}

// secretScanningPushProtectionCanBlock reports whether any pattern could block
// a push to repo.
func (s *Server) secretScanningPushProtectionCanBlock(repo *store.Repo) bool {
	for _, pattern := range secretScanningContentPatterns {
		if s.store.SecretScanningPushProtectionEnabled(repo, pattern.patternID) {
			return true
		}
	}
	return false
}

func (s *Server) secretScanningPushProtectionPlaceholderForRef(repo *store.Repo, stor storer.Storer, ref plumbing.ReferenceName, old, target plumbing.Hash) (*store.SecretScanningPushProtectionPlaceholder, error) {
	if !strings.HasPrefix(string(ref), "refs/heads/") {
		return nil, nil
	}
	// A match can only block through a pattern push protection is enabled for
	// on this repository; with none, everything the scan found would be
	// discarded, and it would have read every blob the push brings to find it —
	// the whole history, on a repository's first push.
	if !s.secretScanningPushProtectionCanBlock(repo) {
		return nil, nil
	}
	matches, err := s.secretScanningPushProtectionMatchesForRange(stor, old, target)
	if err != nil {
		return nil, err
	}
	return s.createSecretScanningPushProtectionPlaceholder(repo, matches), nil
}

func secretScanningMatchPosition(body string, start, end int) (startLine, startColumn, endLine, endColumn int) {
	startLine, startColumn = secretScanningOffsetPosition(body, start)
	endLine, endColumn = secretScanningOffsetPosition(body, end)
	return startLine, startColumn, endLine, endColumn
}

func secretScanningOffsetPosition(body string, offset int) (line, column int) {
	line = 1
	lastLineStart := 0
	for {
		next := strings.IndexByte(body[lastLineStart:offset], '\n')
		if next < 0 {
			break
		}
		line++
		lastLineStart += next + 1
	}
	return line, offset - lastLineStart
}

// secretScanningEnabled reports whether a repository's content is scanned for
// secrets: its security_and_analysis.secret_scanning setting. GitHub scans a
// public repository automatically, and a private one once Secret Protection is
// enabled for it, and "a repository administrator can choose to disable secret
// scanning for a repository at any time" (docs: About secret scanning; Enabling
// secret scanning for your repository). New repositories start with it on when
// public and off when private (store.CreateRepo); the setting decides from then.
func secretScanningEnabled(repo *store.Repo) bool {
	return repo != nil && repo.SecretScanningEnabled
}
