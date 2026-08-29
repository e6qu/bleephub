package bleephub

// GitHub Enterprise Importer workers. Migration state reflects work that
// actually happened, not a timer: a repository migration leaves IN_PROGRESS
// only once the git graph and content have landed.
//
// Workers mirror the export worker (gh_migrations_export.go): supervised by
// goBackground, every step under a context from s.lifetime, and claim-before
// -work so racing replicas cannot double-run. Shutdown leaves work recorded
// IN_PROGRESS; resumeGEIMigrations requeues and re-runs it, safe because every
// step is idempotent.

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
	geiRepositoryMigrationTimeout   = 30 * time.Minute
	geiOrganizationMigrationTimeout = 4 * time.Hour
	geiSourceAPITimeout             = 60 * time.Second
	// geiArchiveMaxBytes caps a caller-named archive read so a source that
	// never stops sending cannot exhaust the process.
	geiArchiveMaxBytes = 2 << 30
	// geiArchiveEntryMaxBytes caps one document inside that archive.
	geiArchiveEntryMaxBytes = 64 << 20
	geiSourceReposPerPage   = 100
	// geiSourceReposMaxPages bounds the enumeration walk.
	geiSourceReposMaxPages = 100
)

// errGEIValidation marks a fault in what the migration was given (missing
// source, rejected credentials, target name taken) rather than a failure of
// the work. GitHub reports these as FAILED_VALIDATION, so the distinction must
// survive to where the state is recorded.
var errGEIValidation = errors.New("migration validation failed")

// supervision

func (s *Server) startGEIRepositoryMigration(id int) {
	s.goBackground(func() { s.runGEIRepositoryMigration(id) })
}

func (s *Server) startGEIOrganizationMigration(id int) {
	s.goBackground(func() { s.runGEIOrganizationMigration(id) })
}

// resumeGEIMigrations re-runs migrations a previous process left unfinished.
// It runs once at boot before the listener opens, so anything still IN_PROGRESS
// belongs to a process that is gone. A repository migration owned by an org
// migration is skipped here; its org migration is resumed and re-drives it, so
// the fan-out's progress accounting stays with one worker.
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

// the repository migration worker

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

// recordGEIRepositoryMigrationOutcome stores the log, then lands the terminal
// state. Log first, since it is the only account of a failed run. State second
// and may be refused: SetRepositoryMigrationState will not move a migration out
// of a terminal state, so a worker finishing after an abort cannot overwrite it.
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

// performGEIRepositoryMigration freezes the source when asked, creates the
// target, fetches its git graph, and materialises its content.
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

	// lockSource can only be honoured for a source hosted on this instance; a
	// repository on another server is not ours to freeze.
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

// materialiseGEITargetRepository creates the target repository, or adopts the
// one an earlier interrupted run of this same migration created. A name taken
// by anything else is a validation failure, never an overwrite.
func (s *Server) materialiseGEITargetRepository(org *store.Org, migration *store.RepositoryMigration, log *migrationLog) (*store.Repo, error) {
	fullName := org.Login + "/" + migration.RepositoryName
	if existing := s.store.GetRepoByFullName(fullName); existing != nil {
		// Continue only into the repository this migration itself created;
		// matching by name would let someone plant a repo under a queued
		// migration's target name and be handed the source's contents.
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

// fetchGEIRepositoryGit pulls the source's whole object graph into the target's
// git storage via fetchGitSourceInto, the same path the source-import API uses,
// so migration and import share one transport policy and address gate.
func (s *Server) fetchGEIRepositoryGit(ctx context.Context, target *store.Repo, migration *store.RepositoryMigration, source *store.MigrationSource, log *migrationLog) error {
	owner, name, ok := store.SplitRepoFullName(target.FullName)
	if !ok {
		return fmt.Errorf("%w: invalid target repository name %q", errGEIValidation, target.FullName)
	}
	stor := s.store.GetGitStorage(owner, name)
	if stor == nil {
		return fmt.Errorf("git storage for %s is unavailable", target.FullName)
	}
	// A declared git archive is the uploaded export; otherwise dial the source
	// as a git remote. The migration's own declaration picks which.
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

// ingestGEIGitArchive unpacks an export migration's bare-repo archive (a
// packfile plus packed-refs) into the target's storage. The pack is applied
// before the refs: a ref to an absent object is a broken repository.
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

// applyPackedRefs writes the archive's refs and reports how many landed.
// Malformed lines are skipped: git's packed-refs carries peel lines and
// comments that name no reference.
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

// openGEIArchive dials a caller-named archive URL under the same address policy
// webhook delivery uses and returns its body.
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

// discardGEIResponse drains and closes an unread response so the connection
// returns to the pool.
func discardGEIResponse(resp *http.Response) {
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, geiArchiveEntryMaxBytes))
	_ = resp.Body.Close()
}

// migrationSourceAuth renders a source's access token as git basic-auth, the
// form GitHub-family smart-HTTP endpoints accept.
func migrationSourceAuth(source *store.MigrationSource) transport.AuthMethod {
	if source == nil || source.AccessToken == "" {
		return nil
	}
	return &gitHTTP.BasicAuth{Username: "x-access-token", Password: source.AccessToken}
}

// content

// ingestGEIRepositoryContent materialises the non-git records: issues, pull
// requests and releases. A declared metadata archive is streamed and applied;
// otherwise an in-instance source is read from the store; a source that is
// neither leaves a git-only migration, recorded as a warning.
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

// geiRepositoryContent is the per-repository record set both ingestion paths
// (archive and in-instance store) produce, so they share one applier.
type geiRepositoryContent struct {
	Issues       []geiIssueRecord   `json:"issues"`
	PullRequests []geiIssueRecord   `json:"pull_requests"`
	Releases     []geiReleaseRecord `json:"releases"`
}

type geiIssueRecord struct {
	Number int    `json:"number"`
	Title  string `json:"title"`
	State  string `json:"state"`
	// User is the record's author. An author with no matching account here
	// becomes a mannequin: GitHub's placeholder for an unmatched author,
	// claimable later through an attribution invitation.
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

// localRepositoryContent reads an in-instance repository through the same
// export projection the archive is built from, so both paths copy the same set.
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

// applyGEIRepositoryContent writes the migrated records into the target. It is
// idempotent by title within a kind, so a resumed migration does not double
// records. A record that cannot be created is a warning (see noteGEIWarning).
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

// noteGEIWarning records one recoverable problem, returning it as an error when
// the migration was started with continueOnError false.
func (s *Server) noteGEIWarning(migration *store.RepositoryMigration, warning string) error {
	s.store.RecordRepositoryMigrationWarning(migration.ID, warning)
	if !migration.ContinueOnError {
		return errors.New(warning)
	}
	return nil
}

// readGEIMetadataArchive streams the declared metadata archive and returns the
// records it carries. The caller-named URL is bounded twice (whole body and
// per entry) so it cannot be read unboundedly.
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

func decodeGEIArchiveEntry(reader io.Reader, out interface{}) error {
	return json.NewDecoder(io.LimitReader(reader, geiArchiveEntryMaxBytes)).Decode(out)
}

// the organization migration worker

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

// performGEIOrganizationMigration walks GitHub's three OrganizationMigrationState
// phases: pre-repo setup, the repository fan-out, and post-repo.
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

// setOrganizationMigrationState advances the phase; a no-op once terminal, so
// an abort stays final.
func (s *Server) setOrganizationMigrationState(id int, state string) {
	s.store.UpdateOrganizationMigration(id, func(m *store.OrganizationMigration) { m.State = state })
}

// materialiseGEITargetOrganization finds or creates the target organization and
// attaches it to the migration's enterprise.
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
// repository migrations start from. Matching by derived name keeps a resumed
// org migration from minting a second source each run.
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

// reconcileGEIOrganizationChildren returns the org migration's child repository
// migrations in source order, creating the missing ones. A resumed migration
// re-drives its existing children instead of queueing a second set.
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

// geiSourceRepository is one source-org repository as its REST API describes it.
type geiSourceRepository struct {
	Name       string `json:"name"`
	CloneURL   string `json:"clone_url"`
	Visibility string `json:"visibility"`
}

// enumerateGEISourceRepositories asks the source instance's REST API which
// repositories the organization has, so totalRepositoriesCount reflects a real
// list rather than an invented total.
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

// migrationSourceOrgReposURL maps a source org's human URL to its repos REST
// endpoint: api.github.com for github.com, /api/v3 on the same origin for GHES.
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

// migrationSourceHTTPClient dials caller-named migration sources under the same
// address policy webhook delivery and the source-import API use.
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

// shared helpers

// repoFromMigrationSourceURL resolves a source URL to a repository on this
// instance, or nil when it names somewhere else. lockSource and the in-instance
// content copy act only on a repository this server owns.
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

// migrationHeadBranch reports the branch HEAD resolves to after the fetch (the
// source's default branch), or "" when HEAD is not a branch.
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

func (s *Server) orgLoginForID(orgID int) string {
	if org := s.store.GetOrgByID(orgID); org != nil {
		return org.Login
	}
	return ""
}

// migrationLog accumulates one line per step, held in memory and streamed into
// the byte store once.
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

// the browser surface
//
// GitHub exposes GEI entities only over GraphQL, so the web UI's migration
// status is served under /ui-data rather than invented under /api/v3. Every
// route resolves the org and gates on viewerMayMigrateOrg, the same predicate
// the REST and GraphQL migration surfaces use.

func (s *Server) registerGEIMigrationUIRoutes() {
	s.route("GET /ui-data/orgs/{org}/migrations/repositories", s.handleUIListRepositoryMigrations)
	s.route("GET /ui-data/orgs/{org}/migrations/repositories/{migration_id}/log", s.handleUIRepositoryMigrationLog)
	s.route("GET /ui-data/orgs/{org}/migrations/sources", s.handleUIListMigrationSources)
	s.route("GET /ui-data/orgs/{org}/migrations/migrators", s.handleUIListOrgMigrators)
}

// resolveMigrationOrg gates every /ui-data migration route. A caller without
// migrator standing gets 404, not 403: the org's very having of migrations is
// part of what the standing protects.
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
// The source is rendered by name and URL only; its access token and PAT are
// never served.
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
