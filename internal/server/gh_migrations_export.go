package bleephub

// The export-migration worker drives a migration from "pending" through
// "exporting" to "exported" or "failed". It runs under s.lifetime, so a
// shutdown cancels an in-flight export; resumeMigrationExports returns any
// interrupted "exporting" migration to "pending" and re-runs it. An export is
// idempotent: it writes one object under a key derived from the migration guid.

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/e6qu/bleephub/internal/store"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/format/idxfile"
	"github.com/go-git/go-git/v5/plumbing/format/packfile"
	gitStorage "github.com/go-git/go-git/v5/storage"
)

const (
	// migrationExportTimeout caps one export so unreadable storage cannot hold a
	// worker open indefinitely; the migration then fails with the timeout.
	migrationExportTimeout = 30 * time.Minute
	// migrationPackWindow is the pack delta window, matching the git transport.
	migrationPackWindow = 10
	// migrationArchiveSchemaVersion is stamped into the archive's schema.json.
	migrationArchiveSchemaVersion = "1.2.0"
)

// startMigrationExport runs one migration's export on an owned goroutine.
func (s *Server) startMigrationExport(scope store.MigrationScope, id int) {
	s.goBackground(func() { s.runMigrationExport(scope, id) })
}

// resumeMigrationExports re-runs exports a previous process left unfinished.
// Called once at boot before any request or worker runs, so an "exporting"
// migration can only be the remains of a gone process.
func (s *Server) resumeMigrationExports() {
	for _, pending := range s.store.ListUnfinishedMigrations() {
		s.store.ResetMigrationToPending(pending.Scope, pending.ID)
		s.startMigrationExport(pending.Scope, pending.ID)
	}
}

// runMigrationExport claims a pending migration, builds its archive, and
// records the outcome.
func (s *Server) runMigrationExport(scope store.MigrationScope, id int) {
	if !s.store.ClaimMigrationForExport(scope, id) {
		return
	}
	migration := s.store.GetMigrationCommon(scope, id)
	if migration == nil {
		s.store.FailMigrationExport(scope, id, "the migration no longer exists")
		return
	}
	ctx, cancel := context.WithTimeout(s.lifetime, migrationExportTimeout)
	defer cancel()

	key := store.MigrationArchiveObjectKey(scope, id, migration.GUID)
	size, digest, err := s.writeMigrationArchive(ctx, key, scope, migration)
	if err != nil {
		s.logger.Error().Err(err).Str("scope", string(scope)).Int("migration_id", id).
			Msg("migration export failed")
		s.store.FailMigrationExport(scope, id, err.Error())
		s.recordAuditEvent("migration.export_failed", "", "", map[string]interface{}{
			"scope": string(scope), "migration_id": id, "reason": err.Error(),
		})
		return
	}
	if !s.store.CompleteMigrationExport(scope, id, key, size, digest) {
		// The migration was deleted or re-entered while the archive was built;
		// its orphaned bytes are removed.
		if delErr := s.migrationObjectStore().Delete(ctx, key); delErr != nil {
			s.logger.Warn().Err(delErr).Str("key", key).Msg("orphaned migration archive not deleted")
		}
		return
	}
	s.recordAuditEvent("migration.exported", "", "", map[string]interface{}{
		"scope": string(scope), "migration_id": id, "archive_size": size,
	})
}

// migrationObjectStore returns the configured object store, or the local
// fallback. Never nil.
func (s *Server) migrationObjectStore() store.ActionsByteStore {
	if configured := s.store.ObjectByteStore; configured != nil {
		return configured
	}
	return s.localByteStore
}

// writeMigrationArchive streams the archive into the byte store and reports its
// size and SHA-256. The tar stream is produced and consumed through a pipe, so
// a large export costs a pipe buffer, not a full in-memory copy; size and
// digest are computed on the way past.
func (s *Server) writeMigrationArchive(ctx context.Context, key string, scope store.MigrationScope, migration *store.MigrationCommon) (int64, string, error) {
	reader, writer := io.Pipe()
	hasher := sha256.New()
	counter := &migrationByteCounter{}

	go func() {
		err := s.buildMigrationArchive(ctx, io.MultiWriter(writer, hasher, counter), scope, migration)
		_ = writer.CloseWithError(err)
	}()

	err := s.migrationObjectStore().PutStream(ctx, key, reader)
	// Unblock the producer if PutStream gave up early so its goroutine cannot
	// outlive this call.
	_ = reader.CloseWithError(err)
	if err != nil {
		return 0, "", fmt.Errorf("store migration archive: %w", err)
	}
	return counter.n, hex.EncodeToString(hasher.Sum(nil)), nil
}

// migrationByteCounter counts the bytes written through it.
type migrationByteCounter struct{ n int64 }

func (c *migrationByteCounter) Write(p []byte) (int, error) {
	c.n += int64(len(p))
	return len(p), nil
}

// buildMigrationArchive writes the whole archive to w in GitHub's migration
// layout: a schema stamp, one JSON document per record type, and a bare git
// repository per repository. The migration's exclude_* flags are honoured here,
// so an archive asked to exclude git data does not contain it.
func (s *Server) buildMigrationArchive(ctx context.Context, w io.Writer, scope store.MigrationScope, migration *store.MigrationCommon) error {
	gz := gzip.NewWriter(w)
	tw := tar.NewWriter(gz)
	modTime := migration.CreatedAt.UTC()

	if err := s.writeMigrationArchiveMetadata(tw, scope, migration, modTime); err != nil {
		return err
	}
	for _, fullName := range migration.Repositories {
		if err := ctx.Err(); err != nil {
			return err
		}
		repo := s.store.GetRepoByFullName(fullName)
		if repo == nil {
			return fmt.Errorf("repository %s is no longer present", fullName)
		}
		if !migration.ExcludeMetadata {
			if err := s.writeMigrationRepoRecords(tw, repo, migration, modTime); err != nil {
				return err
			}
		}
		if !migration.ExcludeGitData {
			if err := s.writeMigrationRepoGit(ctx, tw, repo, modTime); err != nil {
				return err
			}
		}
	}
	if err := tw.Close(); err != nil {
		return err
	}
	return gz.Close()
}

// writeMigrationArchiveMetadata writes the archive-level documents: schema
// stamp, url templates, repository roll and account roll.
func (s *Server) writeMigrationArchiveMetadata(tw *tar.Writer, scope store.MigrationScope, migration *store.MigrationCommon, modTime time.Time) error {
	schema := map[string]interface{}{"version": migrationArchiveSchemaVersion}
	if err := addTarJSON(tw, "schema.json", schema, modTime); err != nil {
		return err
	}
	urls := map[string]interface{}{
		"repository": "/{repository}",
		"issue":      "/{repository}/issues/{number}",
		"pull":       "/{repository}/pull/{number}",
		"release":    "/{repository}/releases/tag/{tag}",
		"user":       "/{user}",
	}
	if err := addTarJSON(tw, "urls.json", urls, modTime); err != nil {
		return err
	}
	repositories := make([]map[string]interface{}, 0, len(migration.Repositories))
	owners := map[string]bool{}
	for _, fullName := range migration.Repositories {
		repo := s.store.GetRepoByFullName(fullName)
		if repo == nil {
			continue
		}
		owner, _, _ := store.SplitRepoFullName(fullName)
		owners[owner] = true
		repositories = append(repositories, map[string]interface{}{
			"type":           "repository",
			"name":           repo.Name,
			"full_name":      repo.FullName,
			"owner":          owner,
			"description":    repo.Description,
			"private":        repo.Private,
			"visibility":     repo.Visibility,
			"default_branch": repo.DefaultBranch,
			"has_issues":     repo.HasIssues,
			"has_wiki":       repo.HasWiki,
			"created_at":     repo.CreatedAt.UTC().Format(time.RFC3339),
			"archived":       repo.Archived,
		})
	}
	if err := addTarJSON(tw, "repositories_000001.json", repositories, modTime); err != nil {
		return err
	}
	accounts := make([]map[string]interface{}, 0, len(owners))
	for owner := range owners {
		accounts = append(accounts, s.migrationAccountRecord(owner))
	}
	sort.Slice(accounts, func(i, j int) bool {
		return fmt.Sprint(accounts[i]["login"]) < fmt.Sprint(accounts[j]["login"])
	})
	if err := addTarJSON(tw, "users_000001.json", accounts, modTime); err != nil {
		return err
	}
	return addTarJSON(tw, "migration.json", map[string]interface{}{
		"guid":                   migration.GUID,
		"scope":                  string(scope),
		"lock_repositories":      migration.LockRepositories,
		"exclude_metadata":       migration.ExcludeMetadata,
		"exclude_git_data":       migration.ExcludeGitData,
		"exclude_attachments":    migration.ExcludeAttachments,
		"exclude_releases":       migration.ExcludeReleases,
		"exclude_owner_projects": migration.ExcludeOwnerProjects,
		"org_metadata_only":      migration.OrgMetadataOnly,
		"created_at":             migration.CreatedAt.UTC().Format(time.RFC3339),
	}, modTime)
}

// migrationAccountRecord describes one account (org or user) the archive
// references.
func (s *Server) migrationAccountRecord(login string) map[string]interface{} {
	if org := s.store.GetOrg(login); org != nil {
		return map[string]interface{}{
			"type": "organization", "login": org.Login, "name": org.Name, "email": org.Email,
		}
	}
	if user := s.store.LookupUserByLogin(login); user != nil {
		return map[string]interface{}{
			"type": "user", "login": user.Login, "name": user.Name, "email": user.Email,
		}
	}
	return map[string]interface{}{"type": "user", "login": login}
}

// writeMigrationRepoRecords writes one repository's exported records.
func (s *Server) writeMigrationRepoRecords(tw *tar.Writer, repo *store.Repo, migration *store.MigrationCommon, modTime time.Time) error {
	base := "repositories/" + repo.FullName + "/"
	data := s.store.MigrationRepoExportData(repo.ID)
	for _, entry := range []struct {
		name string
		key  string
		skip bool
	}{
		{"issues_000001.json", "issues", false},
		{"pull_requests_000001.json", "pull_requests", false},
		{"releases_000001.json", "releases", migration.ExcludeReleases},
	} {
		if entry.skip {
			continue
		}
		value := data[entry.key]
		if value == nil {
			value = []map[string]interface{}{}
		}
		if err := addTarJSON(tw, base+entry.name, value, modTime); err != nil {
			return err
		}
	}
	if migration.ExcludeAttachments {
		return nil
	}
	return addTarJSON(tw, base+"attachments_000001.json", []map[string]interface{}{}, modTime)
}

// writeMigrationRepoGit writes a repository's git data as a real bare
// repository (HEAD, packed-refs, packfile + index) that `git clone` can restore.
func (s *Server) writeMigrationRepoGit(ctx context.Context, tw *tar.Writer, repo *store.Repo, modTime time.Time) error {
	owner, name, ok := store.SplitRepoFullName(repo.FullName)
	if !ok {
		return fmt.Errorf("invalid repository name %q", repo.FullName)
	}
	stor := s.store.GetGitStorage(owner, name)
	if stor == nil {
		return fmt.Errorf("git storage for %s is unavailable", repo.FullName)
	}
	base := "repositories/" + repo.FullName + ".git/"

	refs, head, err := migrationGitRefs(stor, repo.DefaultBranch)
	if err != nil {
		return err
	}
	if err := addTarFile(tw, base+"HEAD", []byte(head), modTime); err != nil {
		return err
	}
	if err := addTarFile(tw, base+"packed-refs", []byte(refs), modTime); err != nil {
		return err
	}
	if err := addTarFile(tw, base+"config", []byte("[core]\n\trepositoryformatversion = 0\n\tbare = true\n"), modTime); err != nil {
		return err
	}

	hashes, err := migrationGitObjectHashes(ctx, stor)
	if err != nil {
		return err
	}
	if len(hashes) == 0 {
		// An empty repository has no pack to write.
		return nil
	}
	return s.writeMigrationPack(tw, stor, hashes, base, modTime)
}

// writeMigrationPack stages the packfile on disk, derives its index from the
// written bytes, and copies both into the archive. Staging keeps a repository
// larger than process memory off the heap.
func (s *Server) writeMigrationPack(tw *tar.Writer, stor gitStorage.Storer, hashes []plumbing.Hash, base string, modTime time.Time) error {
	temp, err := os.CreateTemp("", "bleephub-migration-*.pack")
	if err != nil {
		return fmt.Errorf("stage migration pack: %w", err)
	}
	defer os.Remove(temp.Name())
	defer temp.Close()

	checksum, err := packfile.NewEncoder(temp, stor, false).Encode(hashes, migrationPackWindow)
	if err != nil {
		return fmt.Errorf("encode migration pack: %w", err)
	}
	packSize, err := temp.Seek(0, io.SeekCurrent)
	if err != nil {
		return fmt.Errorf("size migration pack: %w", err)
	}
	if _, err := temp.Seek(0, io.SeekStart); err != nil {
		return fmt.Errorf("rewind migration pack: %w", err)
	}

	writer := new(idxfile.Writer)
	parser, err := packfile.NewParser(packfile.NewScanner(temp), writer)
	if err != nil {
		return fmt.Errorf("parse migration pack: %w", err)
	}
	if _, err := parser.Parse(); err != nil {
		return fmt.Errorf("parse migration pack: %w", err)
	}
	index, err := writer.Index()
	if err != nil {
		return fmt.Errorf("index migration pack: %w", err)
	}
	var encodedIndex strings.Builder
	if _, err := idxfile.NewEncoder(&encodedIndex).Encode(index); err != nil {
		return fmt.Errorf("encode migration pack index: %w", err)
	}

	packName := "pack-" + checksum.String()
	if err := addTarFile(tw, base+"objects/pack/"+packName+".idx", []byte(encodedIndex.String()), modTime); err != nil {
		return err
	}
	if _, err := temp.Seek(0, io.SeekStart); err != nil {
		return fmt.Errorf("rewind migration pack: %w", err)
	}
	if err := tw.WriteHeader(&tar.Header{
		Name:     base + "objects/pack/" + packName + ".pack",
		Mode:     0o644,
		Size:     packSize,
		ModTime:  modTime,
		Typeflag: tar.TypeReg,
	}); err != nil {
		return err
	}
	if _, err := io.Copy(tw, temp); err != nil {
		return fmt.Errorf("copy migration pack: %w", err)
	}
	return nil
}

// migrationGitRefs renders refs in packed-refs format and picks the archive's HEAD.
func migrationGitRefs(stor gitStorage.Storer, defaultBranch string) (string, string, error) {
	iter, err := stor.IterReferences()
	if err != nil {
		return "", "", fmt.Errorf("read refs: %w", err)
	}
	defer iter.Close()
	var lines []string
	branches := map[string]bool{}
	if err := iter.ForEach(func(ref *plumbing.Reference) error {
		if ref.Type() != plumbing.HashReference {
			return nil
		}
		lines = append(lines, ref.Hash().String()+" "+ref.Name().String())
		if ref.Name().IsBranch() {
			branches[ref.Name().Short()] = true
		}
		return nil
	}); err != nil {
		return "", "", fmt.Errorf("read refs: %w", err)
	}
	sort.Strings(lines)
	packed := "# pack-refs with: peeled fully-peeled sorted \n"
	if len(lines) > 0 {
		packed += strings.Join(lines, "\n") + "\n"
	}
	head := defaultBranch
	if !branches[head] {
		for _, candidate := range []string{"main", "master"} {
			if branches[candidate] {
				head = candidate
				break
			}
		}
	}
	return packed, "ref: refs/heads/" + head + "\n", nil
}

// migrationGitObjectHashes lists every object in a repository's storage, sorted.
func migrationGitObjectHashes(ctx context.Context, stor gitStorage.Storer) ([]plumbing.Hash, error) {
	iter, err := stor.IterEncodedObjects(plumbing.AnyObject)
	if err != nil {
		return nil, fmt.Errorf("read git objects: %w", err)
	}
	defer iter.Close()
	var hashes []plumbing.Hash
	if err := iter.ForEach(func(obj plumbing.EncodedObject) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		hashes = append(hashes, obj.Hash())
		return nil
	}); err != nil {
		return nil, err
	}
	sort.Slice(hashes, func(i, j int) bool { return hashes[i].String() < hashes[j].String() })
	return hashes, nil
}

// addTarJSON writes one JSON document into the archive.
func addTarJSON(tw *tar.Writer, name string, value interface{}, modTime time.Time) error {
	encoded, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	return addTarFile(tw, name, append(encoded, '\n'), modTime)
}
