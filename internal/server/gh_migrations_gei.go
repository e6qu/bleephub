package bleephub

// The GitHub Enterprise Importer workers: what drives a repository migration
// from QUEUED through IN_PROGRESS to SUCCEEDED, FAILED or FAILED_VALIDATION,
// and an organization migration through its three phases.
//
// Nothing here is a timer. A repository migration leaves QUEUED when a worker
// claims it, and it leaves IN_PROGRESS when the source repository's git object
// graph has actually been fetched into the target's storage and the source's
// content has been materialised there. A migration that cannot read its source
// fails with the transport's own reason; one whose target already exists fails
// validation; one whose source has vanished fails validation. The state is a
// report of work that happened.
//
// The workers follow the export worker's pattern exactly (gh_migrations_export
// .go): supervised by Server.goBackground, every step governed by a context
// derived from s.lifetime so a migration cannot outlive shutdown, and
// claim-before-work so two replicas racing for one migration cannot both run
// it. Work interrupted by a shutdown is left recorded as IN_PROGRESS, and
// resumeGEIMigrations at the next boot requeues it and runs it again — which is
// safe because every step is idempotent: the target repository is created only
// when absent, the git fetch is a forced fetch of the same refs, and the
// content ingest skips records already present.

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/e6qu/bleephub/internal/store"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/format/packfile"
	"github.com/go-git/go-git/v5/plumbing/transport"
	gitHTTP "github.com/go-git/go-git/v5/plumbing/transport/http"
	gitStorage "github.com/go-git/go-git/v5/storage"
)

const (
	// geiRepositoryMigrationTimeout caps one repository migration. A source
	// that accepts the connection and then stalls must not hold a worker open
	// for the process's lifetime; the migration fails with the timeout.
	geiRepositoryMigrationTimeout = 30 * time.Minute
	// geiOrganizationMigrationTimeout caps a whole organization migration,
	// which is a fan-out over however many repositories the source has.
	geiOrganizationMigrationTimeout = 4 * time.Hour
	// geiSourceAPITimeout caps one call to the source instance's REST API —
	// the enumeration of an organization's repositories.
	geiSourceAPITimeout = 60 * time.Second
	// geiArchiveMaxBytes caps how much of a declared metadata archive is
	// read. The archive is named by the caller, so an unbounded read is an
	// invitation to exhaust the process on a source that never stops sending.
	geiArchiveMaxBytes = 2 << 30
	// geiArchiveEntryMaxBytes caps one document inside that archive.
	geiArchiveEntryMaxBytes = 64 << 20
	// geiSourceReposPerPage is the page size the organization enumeration
	// walks the source's repository list with.
	geiSourceReposPerPage = 100
	// geiSourceReposMaxPages bounds that walk, so a source that keeps
	// answering full pages cannot make the enumeration unbounded.
	geiSourceReposMaxPages = 100
)

// errGEIValidation marks a failure that is a fault in what the migration was
// given — a source that no longer exists, credentials the source rejects, a
// target name already taken — rather than a failure of the work. GitHub
// reports those as FAILED_VALIDATION, so the distinction has to survive from
// where it is discovered to where the state is recorded.
var errGEIValidation = errors.New("migration validation failed")

// --- supervision -----------------------------------------------------------

// startGEIRepositoryMigration runs one repository migration on an owned
// goroutine.
func (s *Server) startGEIRepositoryMigration(id int) {
	s.goBackground(func() { s.runGEIRepositoryMigration(id) })
}

// startGEIOrganizationMigration runs one organization migration on an owned
// goroutine.
func (s *Server) startGEIOrganizationMigration(id int) {
	s.goBackground(func() { s.runGEIOrganizationMigration(id) })
}

// resumeGEIMigrations re-runs the GEI migrations a previous process left
// unfinished. It is called once at boot, before the listener opens, when no
// worker of this process is running yet — so a migration recorded as
// IN_PROGRESS can only be the remains of a process that is gone.
//
// A repository migration that belongs to an organization migration is not
// started here: its organization migration is resumed instead and re-drives
// its own children, so the fan-out's progress accounting stays with the one
// worker that owns it.
func (s *Server) resumeGEIMigrations() {
	for _, id := range s.store.ListUnfinishedOrganizationMigrations() {
		s.store.RequeueOrganizationMigration(id)
		s.startGEIOrganizationMigration(id)
	}
	for _, id := range s.store.ListUnfinishedRepositoryMigrations() {
		m := s.store.GetRepositoryMigration(id)
		if m == nil || m.OrgMigrationID != 0 {
			continue
		}
		s.store.RequeueRepositoryMigration(id)
		s.startGEIRepositoryMigration(id)
	}
}

// --- the repository migration worker ---------------------------------------

// runGEIRepositoryMigration claims a queued repository migration, brings the
// repository across and records the outcome.
func (s *Server) runGEIRepositoryMigration(id int) {
	if !s.store.ClaimRepositoryMigration(id) {
		return
	}
	migration := s.store.GetRepositoryMigration(id)
	if migration == nil {
		s.store.SetRepositoryMigrationState(id, store.GEIMigrationStateFailed, "the migration no longer exists")
		return
	}
	ctx, cancel := context.WithTimeout(s.lifetime, geiRepositoryMigrationTimeout)
	defer cancel()

	log := &migrationLog{}
	log.printf("migration %d: %s -> %s", migration.ID, migration.SourceURL, migration.RepositoryName)
	err := s.performGEIRepositoryMigration(ctx, migration, log)
	s.recordGEIRepositoryMigrationOutcome(ctx, migration, log, err)
}

// recordGEIRepositoryMigrationOutcome stores the migration log and lands the
// migration in its terminal state.
//
// The log is written first because it is the only account of what the worker
// did, and a migration that fails is exactly the one whose log is wanted. The
// state is recorded second and may be refused: SetRepositoryMigrationState
// will not move a migration out of a terminal state, so a worker finishing
// after an abort cannot overwrite the abort with its own verdict.
func (s *Server) recordGEIRepositoryMigrationOutcome(ctx context.Context, migration *store.RepositoryMigration, log *migrationLog, err error) {
	if err != nil {
		log.printf("migration failed: %v", err)
	} else {
		log.printf("migration succeeded")
	}
	key := store.RepositoryMigrationLogObjectKey(migration.ID)
	if putErr := s.migrationObjectStore().PutStream(ctx, key, log.reader()); putErr != nil {
		s.logger.Warn().Err(putErr).Int("migration_id", migration.ID).Msg("migration log not stored")
	} else {
		s.store.SetRepositoryMigrationLogKey(migration.ID, key)
	}

	if err == nil {
		s.store.SetRepositoryMigrationState(migration.ID, store.GEIMigrationStateSucceeded, "")
		s.recordAuditEvent("repository_migration.succeeded", "", s.orgLoginForID(migration.OwnerOrgID), map[string]interface{}{
			"migration_id": migration.ID, "repository": migration.RepositoryName,
		})
		return
	}
	state := store.GEIMigrationStateFailed
	if errors.Is(err, errGEIValidation) {
		state = store.GEIMigrationStateFailedValidation
	}
	s.logger.Error().Err(err).Int("migration_id", migration.ID).Msg("repository migration failed")
	s.store.SetRepositoryMigrationState(migration.ID, state, err.Error())
	s.recordAuditEvent("repository_migration.failed", "", s.orgLoginForID(migration.OwnerOrgID), map[string]interface{}{
		"migration_id": migration.ID, "repository": migration.RepositoryName, "reason": err.Error(),
	})
}

// performGEIRepositoryMigration does the work: freeze the source when asked,
// create the target, fetch the git object graph into it, and materialise the
// source's content.
func (s *Server) performGEIRepositoryMigration(ctx context.Context, migration *store.RepositoryMigration, log *migrationLog) error {
	source := s.store.GetMigrationSource(migration.SourceID)
	if source == nil || source.OwnerOrgID != migration.OwnerOrgID {
		return fmt.Errorf("%w: the migration source is no longer available", errGEIValidation)
	}
	org := s.store.GetOrgByID(migration.OwnerOrgID)
	if org == nil {
		return fmt.Errorf("%w: the target organization no longer exists", errGEIValidation)
	}
	log.printf("source %q (%s) at %s", source.Name, source.Type, source.URL)

	// lockSource freezes the source for the duration of the migration. It can
	// only be honoured for a source that lives on this instance; a repository
	// on another server is not ours to freeze, and saying otherwise would be a
	// lock that blocks nothing.
	sourceRepo := s.repoFromMigrationSourceURL(migration.SourceURL)
	if migration.LockSource {
		if sourceRepo == nil {
			log.printf("lockSource: the source is not hosted here, so it cannot be frozen")
			if err := s.noteGEIWarning(migration, "lockSource was requested but the source repository is not hosted on this instance"); err != nil {
				return err
			}
		} else if s.store.SetRepositoryMigrationSourceLock(migration.ID, sourceRepo.FullName) {
			log.printf("lockSource: %s is frozen for the duration of this migration", sourceRepo.FullName)
		}
	}

	target, err := s.materialiseGEITargetRepository(org, migration, log)
	if err != nil {
		return err
	}

	if err := s.fetchGEIRepositoryGit(ctx, target, migration, source, log); err != nil {
		return err
	}
	return s.ingestGEIRepositoryContent(ctx, target, migration, source, sourceRepo, log)
}

// materialiseGEITargetRepository creates the repository the migration lands
// in, or adopts the one a previous interrupted run of this same migration
// already created.
//
// A name already taken by something else is a validation failure rather than
// an overwrite: a migration must never be a way to write into a repository
// somebody else owns the name of.
func (s *Server) materialiseGEITargetRepository(org *store.Org, migration *store.RepositoryMigration, log *migrationLog) (*store.Repo, error) {
	fullName := org.Login + "/" + migration.RepositoryName
	if existing := s.store.GetRepoByFullName(fullName); existing != nil {
		// Only the repository this migration itself created may be continued
		// into. Matching by name would let anyone able to create a repository
		// in the organization plant one under the name a queued migration is
		// about to use and be handed the source's contents.
		if migration.TargetRepoID == 0 || existing.ID != migration.TargetRepoID {
			return nil, fmt.Errorf("%w: a repository named %s already exists", errGEIValidation, fullName)
		}
		log.printf("target %s was created by an earlier run of this migration; continuing into it", fullName)
		return existing, nil
	}
	private := migration.TargetRepoVisibility != "public"
	creator := s.store.GetUserByID(migration.StartedByUserID)
	if creator == nil {
		return nil, fmt.Errorf("%w: the account that started the migration no longer exists", errGEIValidation)
	}
	target := s.store.CreateOrgRepo(org, creator, migration.RepositoryName, "Migrated from "+migration.SourceURL, private)
	if target == nil {
		return nil, fmt.Errorf("%w: the target repository %s could not be created", errGEIValidation, fullName)
	}
	s.store.SetRepositoryMigrationTargetRepo(migration.ID, target.ID)
	if visibility := migration.TargetRepoVisibility; visibility != "" {
		s.store.UpdateRepo(org.Login, target.Name, func(rp *store.Repo) {
			rp.Visibility = visibility
			rp.Private = visibility != "public"
		})
	}
	log.printf("created target repository %s", target.FullName)
	return target, nil
}

// fetchGEIRepositoryGit pulls the source repository's whole object graph into
// the target's git storage.
//
// It goes through fetchGitSourceInto — the same function the source-import API
// uses — so a migration and an import dial a source under one transport policy
// and one address gate rather than two that drift.
func (s *Server) fetchGEIRepositoryGit(ctx context.Context, target *store.Repo, migration *store.RepositoryMigration, source *store.MigrationSource, log *migrationLog) error {
	owner, name, ok := store.SplitRepoFullName(target.FullName)
	if !ok {
		return fmt.Errorf("%w: invalid target repository name %q", errGEIValidation, target.FullName)
	}
	stor := s.store.GetGitStorage(owner, name)
	if stor == nil {
		return fmt.Errorf("git storage for %s is unavailable", target.FullName)
	}
	// A declared git archive is the uploaded export of the source repository,
	// so it is unpacked; otherwise the source is dialled as a git remote.
	// These are the two ways GEI is actually given a repository's git data, and
	// which one applies is the migration's own declaration rather than a guess.
	if migration.GitArchiveURL != "" {
		log.printf("unpacking git data from the declared git archive")
		if err := s.ingestGEIGitArchive(ctx, stor, migration, source, log); err != nil {
			return err
		}
	} else {
		log.printf("fetching git data from %s", migration.SourceURL)
		err := fetchGitSourceInto(ctx, stor, migration.SourceURL, migrationSourceAuth(source))
		if err != nil {
			switch {
			case errors.Is(err, errUnsupportedImportProtocol):
				return fmt.Errorf("%w: %s", errGEIValidation, err)
			case errors.Is(err, transport.ErrAuthenticationRequired), errors.Is(err, transport.ErrAuthorizationFailed):
				return fmt.Errorf("%w: the migration source rejected the credentials: %s", errGEIValidation, err)
			case errors.Is(err, transport.ErrRepositoryNotFound):
				return fmt.Errorf("%w: the source repository was not found at %s", errGEIValidation, migration.SourceURL)
			}
			return fmt.Errorf("fetch git data from %s: %w", migration.SourceURL, err)
		}
	}
	pointHEADAtImportedBranch(stor, target.DefaultBranch)
	branch := migrationHeadBranch(stor)
	s.store.UpdateRepo(owner, name, func(rp *store.Repo) {
		if branch != "" {
			rp.DefaultBranch = branch
		}
		rp.PushedAt = s.store.CurrentTime()
	})
	if branch != "" && branch != target.DefaultBranch {
		log.printf("default branch set to %s", branch)
	}
	log.printf("git data fetched")
	return nil
}

// ingestGEIGitArchive unpacks an uploaded git archive into the target's
// storage.
//
// The archive is the bare repository an export migration produced: a packfile
// holding the whole object graph and a packed-refs naming what points into it.
// Both are applied here — the pack first, then the refs, because a reference
// to an object that is not there yet is a broken repository — so the target is
// the repository the archive describes rather than a directory that resembles
// one.
func (s *Server) ingestGEIGitArchive(ctx context.Context, stor gitStorage.Storer, migration *store.RepositoryMigration, source *store.MigrationSource, log *migrationLog) error {
	body, err := s.openGEIArchive(ctx, migration.GitArchiveURL, source, "git archive")
	if err != nil {
		return err
	}
	defer body.Close()
	gz, err := gzip.NewReader(io.LimitReader(body, geiArchiveMaxBytes))
	if err != nil {
		return fmt.Errorf("read git archive: %w", err)
	}
	defer gz.Close()

	reader := tar.NewReader(gz)
	var packedRefs, head string
	packs := 0
	for {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fmt.Errorf("read git archive: %w", err)
		}
		if header.Typeflag != tar.TypeReg {
			continue
		}
		switch base := path.Base(header.Name); {
		case strings.HasSuffix(base, ".pack"):
			if err := packfile.UpdateObjectStorage(stor, reader); err != nil {
				return fmt.Errorf("unpack git archive: %w", err)
			}
			packs++
		case base == "packed-refs":
			raw, err := io.ReadAll(io.LimitReader(reader, geiArchiveEntryMaxBytes))
			if err != nil {
				return fmt.Errorf("read git archive refs: %w", err)
			}
			packedRefs = string(raw)
		case base == "HEAD":
			raw, err := io.ReadAll(io.LimitReader(reader, 4096))
			if err != nil {
				return fmt.Errorf("read git archive HEAD: %w", err)
			}
			head = string(raw)
		}
	}
	if packs == 0 && packedRefs == "" {
		return fmt.Errorf("%w: the git archive holds no repository data", errGEIValidation)
	}
	refs := applyPackedRefs(stor, packedRefs)
	log.printf("unpacked %d packfile(s) and %d reference(s)", packs, refs)
	if branch := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(head), "ref: refs/heads/")); branch != "" && branch != strings.TrimSpace(head) {
		_ = store.SetGitHeadBranch(stor, branch)
	}
	return nil
}

// applyPackedRefs writes the archive's refs into storage and reports how many
// landed. A malformed line is skipped rather than failing the migration: git's
// own packed-refs carries peel lines and comments that name no reference.
func applyPackedRefs(stor gitStorage.Storer, packedRefs string) int {
	applied := 0
	for _, line := range strings.Split(packedRefs, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, "^") {
			continue
		}
		hash, name, ok := strings.Cut(line, " ")
		if !ok || !plumbing.IsHash(hash) {
			continue
		}
		ref := plumbing.NewHashReference(plumbing.ReferenceName(strings.TrimSpace(name)), plumbing.NewHash(hash))
		if err := stor.SetReference(ref); err == nil {
			applied++
		}
	}
	return applied
}

// openGEIArchive dials a caller-named archive URL under the same address
// policy webhook delivery uses and returns its body.
func (s *Server) openGEIArchive(ctx context.Context, rawURL string, source *store.MigrationSource, what string) (io.ReadCloser, error) {
	if err := validateWebhookTargetURL(rawURL); err != nil {
		return nil, fmt.Errorf("%w: the %s URL is not a permitted target: %s", errGEIValidation, what, err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, fmt.Errorf("%w: %s", errGEIValidation, err)
	}
	if source != nil && source.AccessToken != "" {
		req.Header.Set("Authorization", "Bearer "+source.AccessToken)
	}
	resp, err := s.migrationSourceHTTPClient().Do(req)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", what, err)
	}
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		discardGEIResponse(resp)
		return nil, fmt.Errorf("%w: the %s rejected the credentials (HTTP %d)", errGEIValidation, what, resp.StatusCode)
	}
	if resp.StatusCode != http.StatusOK {
		discardGEIResponse(resp)
		return nil, fmt.Errorf("read %s: the source answered HTTP %d", what, resp.StatusCode)
	}
	return resp.Body, nil
}

// discardGEIResponse drains and closes a response this code is not going to
// read, so the connection returns to the pool instead of being torn down.
func discardGEIResponse(resp *http.Response) {
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, geiArchiveEntryMaxBytes))
	_ = resp.Body.Close()
}

// migrationSourceAuth renders a migration source's access token as git
// credentials. GitHub-family remotes accept a token as the HTTP basic
// password, which is how the exporting instance's own smart-HTTP endpoint
// authenticates a migration.
func migrationSourceAuth(source *store.MigrationSource) transport.AuthMethod {
	if source == nil || source.AccessToken == "" {
		return nil
	}
	return &gitHTTP.BasicAuth{Username: "x-access-token", Password: source.AccessToken}
}

// --- content ---------------------------------------------------------------

// ingestGEIRepositoryContent materialises everything the migration brings
// across that is not git objects: issues, pull-request records and releases.
//
// There are two real sources for it and the migration says which. A declared
// metadata archive is GEI's own mechanism — the tar.gz an export migration
// produced — and is streamed and applied. Otherwise, when the source
// repository is hosted on this instance, its records are read straight out of
// the store. A source that is neither leaves the migration a git-only one,
// which is recorded as a warning rather than pretended away.
func (s *Server) ingestGEIRepositoryContent(ctx context.Context, target *store.Repo, migration *store.RepositoryMigration, source *store.MigrationSource, sourceRepo *store.Repo, log *migrationLog) error {
	if migration.MetadataArchiveURL != "" {
		content, err := s.readGEIMetadataArchive(ctx, migration, source, log)
		if err != nil {
			return err
		}
		return s.applyGEIRepositoryContent(target, migration, content, log)
	}
	if sourceRepo != nil {
		log.printf("reading content from %s on this instance", sourceRepo.FullName)
		return s.applyGEIRepositoryContent(target, migration, s.localRepositoryContent(sourceRepo), log)
	}
	log.printf("no metadata archive was declared and the source is not hosted here; only git data was migrated")
	return s.noteGEIWarning(migration, "no metadata archive was declared, so issues, pull requests and releases were not migrated")
}

// geiRepositoryContent is the set of records a migration carries across. It is
// the export archive's per-repository documents, which is also the shape the
// store hands back for a repository hosted here — one struct, so both
// ingestion paths land in the same applier.
type geiRepositoryContent struct {
	Issues       []geiIssueRecord   `json:"issues"`
	PullRequests []geiIssueRecord   `json:"pull_requests"`
	Releases     []geiReleaseRecord `json:"releases"`
}

type geiIssueRecord struct {
	Number int    `json:"number"`
	Title  string `json:"title"`
	State  string `json:"state"`
	// User is the record's author in the export archive's own shape. An
	// author with no matching account here becomes a mannequin: github's
	// placeholder for "someone wrote this, but nobody on this instance is
	// them yet", claimable later through an attribution invitation.
	User struct {
		Login string `json:"login"`
		Email string `json:"email"`
	} `json:"user"`
}

type geiReleaseRecord struct {
	TagName    string `json:"tag_name"`
	Name       string `json:"name"`
	Draft      bool   `json:"draft"`
	Prerelease bool   `json:"prerelease"`
}

// localRepositoryContent reads the records of a repository hosted here through
// the same export projection the archive is built from, so what a migration
// copies in-instance and what it copies through an archive are the same set.
func (s *Server) localRepositoryContent(sourceRepo *store.Repo) geiRepositoryContent {
	data := s.store.MigrationRepoExportData(sourceRepo.ID)
	var content geiRepositoryContent
	encoded, err := json.Marshal(data)
	if err != nil {
		return content
	}
	_ = json.Unmarshal(encoded, &content)
	return content
}

// applyGEIRepositoryContent writes the migrated records into the target.
//
// It is idempotent by title within a kind: a resumed migration re-applying its
// content does not double the issues. A record that cannot be created is a
// warning, and a warning fails the migration when it was started with
// continueOnError false.
func (s *Server) applyGEIRepositoryContent(target *store.Repo, migration *store.RepositoryMigration, content geiRepositoryContent, log *migrationLog) error {
	actor := s.store.GetUserByID(migration.StartedByUserID)
	if actor == nil {
		return fmt.Errorf("%w: the account that started the migration no longer exists", errGEIValidation)
	}
	existing := map[string]bool{}
	for _, issue := range s.store.ListIssues(target.ID, "all") {
		existing[issue.Title] = true
	}
	migrated := 0
	for _, record := range append(append([]geiIssueRecord(nil), content.Issues...), content.PullRequests...) {
		if record.Title == "" || existing[record.Title] {
			continue
		}
		if login := record.User.Login; login != "" && s.store.LookupUserByLogin(login) == nil {
			if owner, _, ok := store.SplitRepoFullName(target.FullName); ok {
				if org := s.store.GetOrg(owner); org != nil {
					s.store.EnsureMannequin(org.ID, login, record.User.Email)
				}
			}
		}
		created := s.store.CreateIssue(target.ID, actor.ID, record.Title, "", nil, nil, 0)
		if created == nil {
			if err := s.noteGEIWarning(migration, fmt.Sprintf("issue %q could not be migrated", record.Title)); err != nil {
				return err
			}
			continue
		}
		existing[record.Title] = true
		if strings.EqualFold(record.State, "closed") {
			s.store.UpdateIssue(created.ID, func(i *store.Issue) {
				i.State = "closed"
				closedAt := s.store.CurrentTime()
				i.ClosedAt = &closedAt
			})
		}
		migrated++
	}
	log.printf("migrated %d issue records", migrated)

	if migration.SkipReleases {
		log.printf("skipReleases: releases were not migrated")
		return nil
	}
	releases := 0
	for _, record := range content.Releases {
		if record.TagName == "" || s.store.Releases.GetByTag(target.ID, record.TagName) != nil {
			continue
		}
		if s.store.Releases.Create(target.ID, actor.ID, record.TagName, "", record.Name, "", record.Draft, record.Prerelease, false) == nil {
			if err := s.noteGEIWarning(migration, fmt.Sprintf("release %q could not be migrated", record.TagName)); err != nil {
				return err
			}
			continue
		}
		releases++
	}
	log.printf("migrated %d releases", releases)
	return nil
}

// noteGEIWarning records one recoverable problem. A migration started with
// continueOnError false treats the first such problem as its failure, which is
// what that flag means: continue, or do not.
func (s *Server) noteGEIWarning(migration *store.RepositoryMigration, warning string) error {
	s.store.RecordRepositoryMigrationWarning(migration.ID, warning)
	if !migration.ContinueOnError {
		return errors.New(warning)
	}
	return nil
}

// readGEIMetadataArchive streams the declared metadata archive and returns the
// records it carries.
//
// The archive is the tar.gz an export migration produces, so its
// per-repository documents are read by name. The stream is bounded twice —
// once over the whole body and once per entry — because the URL is the
// caller's and an unbounded read of it is an unbounded read of a stranger's
// output.
func (s *Server) readGEIMetadataArchive(ctx context.Context, migration *store.RepositoryMigration, source *store.MigrationSource, log *migrationLog) (geiRepositoryContent, error) {
	var content geiRepositoryContent
	body, err := s.openGEIArchive(ctx, migration.MetadataArchiveURL, source, "metadata archive")
	if err != nil {
		return content, err
	}
	defer body.Close()
	gz, err := gzip.NewReader(io.LimitReader(body, geiArchiveMaxBytes))
	if err != nil {
		return content, fmt.Errorf("read metadata archive: %w", err)
	}
	defer gz.Close()

	reader := tar.NewReader(gz)
	for {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return content, fmt.Errorf("read metadata archive: %w", err)
		}
		if header.Typeflag != tar.TypeReg {
			continue
		}
		switch base := path.Base(header.Name); {
		case strings.HasPrefix(base, "issues_"):
			err = decodeGEIArchiveEntry(reader, &content.Issues)
		case strings.HasPrefix(base, "pull_requests_"):
			err = decodeGEIArchiveEntry(reader, &content.PullRequests)
		case strings.HasPrefix(base, "releases_"):
			err = decodeGEIArchiveEntry(reader, &content.Releases)
		default:
			continue
		}
		if err != nil {
			if warnErr := s.noteGEIWarning(migration, fmt.Sprintf("archive entry %s could not be read: %v", header.Name, err)); warnErr != nil {
				return content, warnErr
			}
		}
	}
	log.printf("metadata archive carried %d issues, %d pull requests and %d releases",
		len(content.Issues), len(content.PullRequests), len(content.Releases))
	return content, nil
}

// decodeGEIArchiveEntry reads one bounded JSON document out of the archive.
func decodeGEIArchiveEntry(reader io.Reader, out interface{}) error {
	return json.NewDecoder(io.LimitReader(reader, geiArchiveEntryMaxBytes)).Decode(out)
}

// --- the organization migration worker --------------------------------------

// runGEIOrganizationMigration claims a queued organization migration and
// brings the whole organization across.
func (s *Server) runGEIOrganizationMigration(id int) {
	if !s.store.ClaimOrganizationMigration(id) {
		return
	}
	migration := s.store.GetOrganizationMigration(id)
	if migration == nil {
		return
	}
	ctx, cancel := context.WithTimeout(s.lifetime, geiOrganizationMigrationTimeout)
	defer cancel()

	err := s.performGEIOrganizationMigration(ctx, migration)
	if err == nil {
		s.store.UpdateOrganizationMigration(id, func(m *store.OrganizationMigration) {
			m.State = store.GEIMigrationStateSucceeded
			m.FailureReason = ""
			zero := 0
			m.RemainingRepositoriesCount = &zero
		})
		s.recordAuditEvent("organization_migration.succeeded", "", migration.TargetOrgName, map[string]interface{}{
			"migration_id": id, "source_org": migration.SourceOrgName,
		})
		return
	}
	state := store.GEIMigrationStateFailed
	if errors.Is(err, errGEIValidation) {
		state = store.GEIMigrationStateFailedValidation
	}
	s.logger.Error().Err(err).Int("organization_migration_id", id).Msg("organization migration failed")
	s.store.UpdateOrganizationMigration(id, func(m *store.OrganizationMigration) {
		m.State = state
		m.FailureReason = err.Error()
	})
	s.recordAuditEvent("organization_migration.failed", "", migration.TargetOrgName, map[string]interface{}{
		"migration_id": id, "source_org": migration.SourceOrgName, "reason": err.Error(),
	})
}

// performGEIOrganizationMigration walks the three phases GitHub's
// OrganizationMigrationState names: everything that has to exist before any
// repository can land, the repository fan-out itself, and what is finished
// afterwards.
func (s *Server) performGEIOrganizationMigration(ctx context.Context, migration *store.OrganizationMigration) error {
	s.setOrganizationMigrationState(migration.ID, store.OrgMigrationStatePreRepoMigration)

	targetOrg, err := s.materialiseGEITargetOrganization(migration)
	if err != nil {
		return err
	}
	source, err := s.migrationSourceForOrganization(migration, targetOrg)
	if err != nil {
		return err
	}
	sourceRepos, err := s.enumerateGEISourceRepositories(ctx, migration)
	if err != nil {
		return err
	}
	total := len(sourceRepos)
	s.store.UpdateOrganizationMigration(migration.ID, func(m *store.OrganizationMigration) {
		copyTotal, copyRemaining := total, total
		m.TotalRepositoriesCount = &copyTotal
		m.RemainingRepositoriesCount = &copyRemaining
	})

	s.setOrganizationMigrationState(migration.ID, store.OrgMigrationStateRepoMigration)
	children := s.reconcileGEIOrganizationChildren(migration, targetOrg, source, sourceRepos)
	failures := 0
	for index, childID := range children {
		if err := ctx.Err(); err != nil {
			return err
		}
		s.store.RequeueRepositoryMigration(childID)
		s.runGEIRepositoryMigration(childID)
		if child := s.store.GetRepositoryMigration(childID); child == nil || child.State != store.GEIMigrationStateSucceeded {
			failures++
		}
		remaining := total - index - 1
		s.store.UpdateOrganizationMigration(migration.ID, func(m *store.OrganizationMigration) {
			copyRemaining := remaining
			m.RemainingRepositoriesCount = &copyRemaining
		})
	}
	if failures > 0 {
		return fmt.Errorf("%d of %d repositories could not be migrated", failures, total)
	}

	s.setOrganizationMigrationState(migration.ID, store.OrgMigrationStatePostRepoMigration)
	return nil
}

// setOrganizationMigrationState moves an organization migration to its next
// phase. It is a no-op once the migration is terminal, which is how an abort
// stays final.
func (s *Server) setOrganizationMigrationState(id int, state string) {
	s.store.UpdateOrganizationMigration(id, func(m *store.OrganizationMigration) { m.State = state })
}

// materialiseGEITargetOrganization finds or creates the organization the
// migration lands in and attaches it to the enterprise that owns the
// migration.
func (s *Server) materialiseGEITargetOrganization(migration *store.OrganizationMigration) (*store.Org, error) {
	if existing := s.store.GetOrg(migration.TargetOrgName); existing != nil {
		if enterpriseID := s.store.EnterpriseIDForOrg(existing.ID); enterpriseID != 0 && enterpriseID != migration.EnterpriseID {
			return nil, fmt.Errorf("%w: %s already belongs to another enterprise", errGEIValidation, existing.Login)
		}
		s.store.AddEnterpriseOrganization(migration.EnterpriseID, existing.ID)
		s.store.UpdateOrganizationMigration(migration.ID, func(m *store.OrganizationMigration) { m.TargetOrgID = existing.ID })
		return existing, nil
	}
	creator := s.store.GetUserByID(migration.StartedByUserID)
	if creator == nil {
		return nil, fmt.Errorf("%w: the account that started the migration no longer exists", errGEIValidation)
	}
	org := s.store.CreateOrg(creator, migration.TargetOrgName, migration.TargetOrgName, "Migrated from "+migration.SourceOrgURL)
	if org == nil {
		return nil, fmt.Errorf("%w: the target organization %s could not be created", errGEIValidation, migration.TargetOrgName)
	}
	s.store.AddEnterpriseOrganization(migration.EnterpriseID, org.ID)
	s.store.UpdateOrganizationMigration(migration.ID, func(m *store.OrganizationMigration) { m.TargetOrgID = org.ID })
	return org, nil
}

// migrationSourceForOrganization finds or creates the source the fan-out's
// repository migrations are started from. An organization migration names its
// source by URL rather than by id, so the source record is derived from it
// once and reused, which is what keeps a resumed migration from minting a
// second source every time it runs.
func (s *Server) migrationSourceForOrganization(migration *store.OrganizationMigration, targetOrg *store.Org) (*store.MigrationSource, error) {
	name := "organization-migration-" + migration.SourceOrgName
	for _, existing := range s.store.ListMigrationSources(targetOrg.ID) {
		if existing.Name == name {
			return existing, nil
		}
	}
	source := s.store.CreateMigrationSource(targetOrg.ID, name, store.MigrationSourceTypeGitHubArchive,
		migration.SourceOrgURL, migration.SourceAccessToken, "")
	if source == nil {
		return nil, fmt.Errorf("%w: the migration source could not be recorded", errGEIValidation)
	}
	return source, nil
}

// reconcileGEIOrganizationChildren returns the repository migrations of this
// organization migration, in the source's order, creating the ones that do not
// exist yet. A resumed organization migration re-drives the children it
// already has instead of queueing a second set.
func (s *Server) reconcileGEIOrganizationChildren(migration *store.OrganizationMigration, targetOrg *store.Org, source *store.MigrationSource, sourceRepos []geiSourceRepository) []int {
	byName := map[string]int{}
	for _, child := range s.store.ListRepositoryMigrationsForOrgMigration(migration.ID) {
		byName[child.RepositoryName] = child.ID
	}
	ids := make([]int, 0, len(sourceRepos))
	for _, repo := range sourceRepos {
		if id, ok := byName[repo.Name]; ok {
			ids = append(ids, id)
			continue
		}
		child := s.store.CreateRepositoryMigration(store.NewRepositoryMigration{
			OwnerOrgID:           targetOrg.ID,
			SourceID:             source.ID,
			RepositoryName:       repo.Name,
			SourceURL:            repo.CloneURL,
			ContinueOnError:      true,
			TargetRepoVisibility: repo.Visibility,
			OrgMigrationID:       migration.ID,
			StartedByUserID:      migration.StartedByUserID,
		})
		if child != nil {
			ids = append(ids, child.ID)
		}
	}
	return ids
}

// geiSourceRepository is one repository the source organization holds, as its
// REST API describes it.
type geiSourceRepository struct {
	Name       string `json:"name"`
	CloneURL   string `json:"clone_url"`
	Visibility string `json:"visibility"`
}

// enumerateGEISourceRepositories asks the source instance which repositories
// the organization has.
//
// This is a real call to the source's REST API rather than a guess: an
// organization migration that cannot see the source's repository list does not
// know what it is migrating, and reporting a total it invented would make
// totalRepositoriesCount a lie.
func (s *Server) enumerateGEISourceRepositories(ctx context.Context, migration *store.OrganizationMigration) ([]geiSourceRepository, error) {
	endpoint, err := migrationSourceOrgReposURL(migration.SourceOrgURL)
	if err != nil {
		return nil, fmt.Errorf("%w: %s", errGEIValidation, err)
	}
	client := s.migrationSourceHTTPClient()
	var out []geiSourceRepository
	for page := 1; page <= geiSourceReposMaxPages; page++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		pageURL := fmt.Sprintf("%s?per_page=%d&page=%d", endpoint, geiSourceReposPerPage, page)
		batch, err := s.fetchGEISourceRepositoryPage(ctx, client, pageURL, migration.SourceAccessToken)
		if err != nil {
			return nil, err
		}
		out = append(out, batch...)
		if len(batch) < geiSourceReposPerPage {
			break
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// fetchGEISourceRepositoryPage reads one page of the source organization's
// repository list.
func (s *Server) fetchGEISourceRepositoryPage(ctx context.Context, client *http.Client, pageURL, token string) ([]geiSourceRepository, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, pageURL, nil)
	if err != nil {
		return nil, fmt.Errorf("%w: %s", errGEIValidation, err)
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("enumerate source repositories: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return nil, fmt.Errorf("%w: the source organization rejected the access token (HTTP %d)", errGEIValidation, resp.StatusCode)
	}
	if resp.StatusCode == http.StatusNotFound {
		return nil, fmt.Errorf("%w: the source organization was not found", errGEIValidation)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("enumerate source repositories: the source answered HTTP %d", resp.StatusCode)
	}
	var batch []geiSourceRepository
	if err := json.NewDecoder(io.LimitReader(resp.Body, geiArchiveEntryMaxBytes)).Decode(&batch); err != nil {
		return nil, fmt.Errorf("enumerate source repositories: %w", err)
	}
	return batch, nil
}

// migrationSourceOrgReposURL turns the human-facing URL of a source
// organization into the REST endpoint that lists its repositories. github.com
// serves it from api.github.com; every GitHub Enterprise Server — including
// this one — serves it from /api/v3 on the same origin.
func migrationSourceOrgReposURL(raw string) (string, error) {
	parsed, err := url.Parse(strings.TrimSuffix(raw, "/"))
	if err != nil {
		return "", fmt.Errorf("the source organization URL is not a URL: %w", err)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return "", fmt.Errorf("unsupported source organization URL scheme %q", parsed.Scheme)
	}
	name := path.Base(parsed.Path)
	if name == "" || name == "." || name == "/" {
		return "", fmt.Errorf("the source organization URL names no organization")
	}
	if strings.EqualFold(parsed.Host, "github.com") || strings.EqualFold(parsed.Host, "www.github.com") {
		return "https://api.github.com/orgs/" + name + "/repos", nil
	}
	return parsed.Scheme + "://" + parsed.Host + "/api/v3/orgs/" + name + "/repos", nil
}

// migrationSourceHTTPClient dials migration sources under the same address
// policy webhook delivery and the source-import API use. A migration source is
// named by the caller, so it gets the caller-named-target treatment rather
// than the trust an internal call would carry.
func (s *Server) migrationSourceHTTPClient() *http.Client {
	return &http.Client{
		Timeout:   geiSourceAPITimeout,
		Transport: newAddressCheckedHTTPTransport(false),
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 5 {
				return errors.New("stopped after 5 redirects")
			}
			_, err := parseWebhookTargetURL(req.URL.String())
			return err
		},
	}
}

// --- shared helpers ---------------------------------------------------------

// repoFromMigrationSourceURL resolves a migration's source URL to a repository
// on this instance, or nil when it names somewhere else. It is what makes
// lockSource and the in-instance content copy possible: both are operations on
// a repository this server owns, and neither may be claimed for one it does
// not.
func (s *Server) repoFromMigrationSourceURL(raw string) *store.Repo {
	parsed, err := url.Parse(raw)
	if err != nil {
		return nil
	}
	trimmed := strings.Trim(strings.TrimSuffix(parsed.Path, ".git"), "/")
	owner, name, ok := store.SplitRepoFullName(trimmed)
	if !ok {
		return nil
	}
	return s.store.GetRepoByFullName(owner + "/" + name)
}

// migrationHeadBranch reports the branch HEAD resolves to after the fetch, or
// "" when HEAD is not a branch. It is what tells the target repository which
// default branch the source actually had.
func migrationHeadBranch(stor gitStorage.Storer) string {
	head, err := stor.Reference(plumbing.HEAD)
	if err != nil || head == nil || head.Type() != plumbing.SymbolicReference {
		return ""
	}
	if !head.Target().IsBranch() {
		return ""
	}
	return head.Target().Short()
}

// orgLoginForID renders an organization id as the login the audit log records.
func (s *Server) orgLoginForID(orgID int) string {
	if org := s.store.GetOrgByID(orgID); org != nil {
		return org.Login
	}
	return ""
}

// migrationLog accumulates a migration's account of itself. It is small by
// construction — one line per step — so it is held in memory and streamed into
// the byte store once, rather than being appended to an object per line.
type migrationLog struct {
	lines []string
}

func (l *migrationLog) printf(format string, args ...interface{}) {
	l.lines = append(l.lines, fmt.Sprintf(format, args...))
}

func (l *migrationLog) reader() io.Reader {
	if len(l.lines) == 0 {
		return strings.NewReader("")
	}
	return strings.NewReader(strings.Join(l.lines, "\n") + "\n")
}

// --- the browser surface ----------------------------------------------------
//
// GitHub has no REST routes for the GEI entities — its whole surface is
// GraphQL — so the migration status and history the web UI renders is served
// under /ui-data rather than invented under /api/v3.
//
// Every route here resolves the organization first and asks
// viewerMayMigrateOrg about it, which is the same predicate the REST migration
// routes and the GraphQL migration surface ask. A migration exposes an entire
// organization's data, and there is exactly one answer to who may see it.

func (s *Server) registerGEIMigrationUIRoutes() {
	s.route("GET /ui-data/orgs/{org}/migrations/repositories", s.handleUIListRepositoryMigrations)
	s.route("GET /ui-data/orgs/{org}/migrations/repositories/{migration_id}/log", s.handleUIRepositoryMigrationLog)
	s.route("GET /ui-data/orgs/{org}/migrations/sources", s.handleUIListMigrationSources)
	s.route("GET /ui-data/orgs/{org}/migrations/migrators", s.handleUIListOrgMigrators)
}

// resolveMigrationOrg is the gate every /ui-data migration route passes
// through. A caller without migrator standing is answered 404 rather than 403:
// whether an organization has migrations at all is part of what the standing
// protects.
func (s *Server) resolveMigrationOrg(w http.ResponseWriter, r *http.Request) *store.Org {
	org := s.store.GetOrg(r.PathValue("org"))
	if org == nil || !s.viewerMayMigrateOrg(r.Context(), org) {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return nil
	}
	return org
}

func (s *Server) handleUIListRepositoryMigrations(w http.ResponseWriter, r *http.Request) {
	org := s.resolveMigrationOrg(w, r)
	if org == nil {
		return
	}
	migrations := s.store.ListRepositoryMigrations(org.ID)
	out := make([]map[string]interface{}, 0, len(migrations))
	for i := len(migrations) - 1; i >= 0; i-- {
		out = append(out, s.repositoryMigrationUIJSON(migrations[i], org))
	}
	writeJSON(w, http.StatusOK, paginateAndLink(w, r, out))
}

// repositoryMigrationUIJSON renders one repository migration for the browser.
// The source is rendered by name and URL only: its access token and PAT are
// never served anywhere, and this surface is no exception.
func (s *Server) repositoryMigrationUIJSON(m *store.RepositoryMigration, org *store.Org) map[string]interface{} {
	out := map[string]interface{}{
		"id":                m.ID,
		"node_id":           m.NodeID,
		"repository_name":   m.RepositoryName,
		"source_url":        m.SourceURL,
		"state":             m.State,
		"failure_reason":    m.FailureReason,
		"warnings_count":    m.WarningsCount,
		"warning_log":       jsonArray(m.WarningLog),
		"continue_on_error": m.ContinueOnError,
		"lock_source":       m.LockSource,
		"skip_releases":     m.SkipReleases,
		"locked_repository": m.SourceRepoLock,
		"org_migration_id":  m.OrgMigrationID,
		"created_at":        m.CreatedAt.Format(time.RFC3339),
		"updated_at":        m.UpdatedAt.Format(time.RFC3339),
		"source":            nil,
		"log_url":           nil,
	}
	if src := s.store.GetMigrationSource(m.SourceID); src != nil {
		out["source"] = map[string]interface{}{"id": src.ID, "name": src.Name, "type": src.Type, "url": src.URL}
	}
	if m.MigrationLogKey != "" {
		out["log_url"] = fmt.Sprintf("/ui-data/orgs/%s/migrations/repositories/%d/log", org.Login, m.ID)
	}
	return out
}

func (s *Server) handleUIRepositoryMigrationLog(w http.ResponseWriter, r *http.Request) {
	org := s.resolveMigrationOrg(w, r)
	if org == nil {
		return
	}
	id, err := strconv.Atoi(r.PathValue("migration_id"))
	if err != nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	migration := s.store.GetRepositoryMigration(id)
	if migration == nil || migration.OwnerOrgID != org.ID || migration.MigrationLogKey == "" {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	body, err := s.migrationObjectStore().GetStream(r.Context(), migration.MigrationLogKey)
	if err != nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	defer body.Close()
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	if _, err := io.Copy(w, body); err != nil {
		s.logger.Warn().Err(err).Int("migration_id", id).Msg("migration log download interrupted")
	}
}

func (s *Server) handleUIListMigrationSources(w http.ResponseWriter, r *http.Request) {
	org := s.resolveMigrationOrg(w, r)
	if org == nil {
		return
	}
	sources := s.store.ListMigrationSources(org.ID)
	out := make([]map[string]interface{}, 0, len(sources))
	for _, src := range sources {
		out = append(out, map[string]interface{}{
			"id": src.ID, "node_id": src.NodeID, "name": src.Name,
			"type": src.Type, "url": src.URL,
			"created_at": src.CreatedAt.Format(time.RFC3339),
		})
	}
	writeJSON(w, http.StatusOK, paginateAndLink(w, r, out))
}

func (s *Server) handleUIListOrgMigrators(w http.ResponseWriter, r *http.Request) {
	org := s.resolveMigrationOrg(w, r)
	if org == nil {
		return
	}
	roles := s.store.ListOrgMigratorRoles(org.ID)
	out := make([]map[string]interface{}, 0, len(roles))
	for _, role := range roles {
		out = append(out, map[string]interface{}{
			"actor_type": role.ActorType,
			"actor":      role.Actor,
			"created_at": role.CreatedAt.Format(time.RFC3339),
		})
	}
	writeJSON(w, http.StatusOK, paginateAndLink(w, r, out))
}
