package store

// The export-migration state machine and the repository lock it holds.
//
// A migration is created "pending". A worker claims it into "exporting", does
// the export, and lands it in "exported" or "failed". Every transition is a
// compare-and-set under the store lock, so two workers racing for the same
// migration cannot both claim it and a finished migration cannot be re-entered.
//
// The lock a migration takes on its repositories is stored on the migration
// itself rather than on the repository: a repository is locked because some
// migration says so, and releasing the last such migration releases the
// repository. Storing a boolean on the repository instead would make
// "unlocked" depend on nobody forgetting to clear it.
//
// STORE-021: every getter here returns a detached snapshot.

import (
	"fmt"
	"sort"
	"strings"
)

// migrationLockOwnerLogin is the account whose repositories a migration's
// short-name lock keys belong to. GitHub's unlock operation names a repository
// by its short name, which is unique only within an owner, so the owner has to
// come from the migration.
func (st *Store) migrationLockOwnerLoginLocked(userID int, orgLogin string) string {
	if orgLogin != "" {
		return orgLogin
	}
	if u := st.Users[userID]; u != nil {
		return u.Login
	}
	return ""
}

// RepoLockedForMigration reports whether any migration still holds a lock on
// the repository with this full name.
//
// This is the predicate the write path asks. It is deliberately a question
// about the repository rather than about one migration: two overlapping
// migrations may both lock a repository, and it stays locked until the last of
// them releases it.
func (st *Store) RepoLockedForMigration(fullName string) bool {
	st.Mu.RLock()
	defer st.Mu.RUnlock()
	owner, name, ok := SplitRepoFullName(fullName)
	if !ok {
		return false
	}
	for _, m := range st.UserMigrations {
		if !m.LockedRepos[name] {
			continue
		}
		if strings.EqualFold(st.migrationLockOwnerLoginLocked(m.UserID, ""), owner) {
			return true
		}
	}
	for _, m := range st.OrgMigrations {
		if m.LockedRepos[name] && strings.EqualFold(m.OrgLogin, owner) {
			return true
		}
	}
	// A GEI repository migration started with lockSource freezes its source
	// for as long as it runs. It is asked here rather than through a second
	// predicate because the write path must not have to know which of the two
	// migration families froze a repository — one question, one answer.
	for _, m := range st.RepositoryMigrations {
		if m.SourceRepoLock != "" && !GEIMigrationTerminal(m.State) && strings.EqualFold(m.SourceRepoLock, fullName) {
			return true
		}
	}
	return false
}

// ListMigrationLockedRepos returns the full names of every repository a
// migration currently locks, ordered by the migration's repository list.
func (st *Store) ListMigrationLockedRepos(scope MigrationScope, id int) []string {
	st.Mu.RLock()
	defer st.Mu.RUnlock()
	var locked map[string]bool
	owner := ""
	switch scope {
	case UserMigrationScope:
		m := st.UserMigrations[id]
		if m == nil {
			return nil
		}
		locked, owner = m.LockedRepos, st.migrationLockOwnerLoginLocked(m.UserID, "")
	case OrgMigrationScope:
		m := st.OrgMigrations[id]
		if m == nil {
			return nil
		}
		locked, owner = m.LockedRepos, m.OrgLogin
	default:
		return nil
	}
	out := make([]string, 0, len(locked))
	for name := range locked {
		out = append(out, owner+"/"+name)
	}
	sort.Strings(out)
	return out
}

// migrationCommonLocked returns a pointer to the live MigrationCommon of the
// named migration, or nil. Callers hold st.Mu.
func (st *Store) migrationCommonLocked(scope MigrationScope, id int) *MigrationCommon {
	switch scope {
	case UserMigrationScope:
		if m := st.UserMigrations[id]; m != nil {
			return &m.MigrationCommon
		}
	case OrgMigrationScope:
		if m := st.OrgMigrations[id]; m != nil {
			return &m.MigrationCommon
		}
	}
	return nil
}

// persistMigrationLocked writes the named migration through. Callers hold
// st.Mu.
func (st *Store) persistMigrationLocked(scope MigrationScope, id int) {
	switch scope {
	case UserMigrationScope:
		if m := st.UserMigrations[id]; m != nil {
			st.persistUserMigration(m)
		}
	case OrgMigrationScope:
		if m := st.OrgMigrations[id]; m != nil {
			st.persistOrgMigration(m)
		}
	}
}

// GetMigrationCommon returns a detached snapshot of a migration's shared
// fields, whichever family it belongs to, or nil.
func (st *Store) GetMigrationCommon(scope MigrationScope, id int) *MigrationCommon {
	st.Mu.RLock()
	defer st.Mu.RUnlock()
	m := st.migrationCommonLocked(scope, id)
	if m == nil {
		return nil
	}
	snapshot := cloneMigrationCommon(*m)
	return &snapshot
}

// ClaimMigrationForExport moves a pending migration into "exporting" and
// reports whether this caller is the one that claimed it. A migration already
// exporting, exported or failed is not claimable, so a worker restarted
// alongside a running one cannot export the same migration twice.
func (st *Store) ClaimMigrationForExport(scope MigrationScope, id int) bool {
	st.Mu.Lock()
	defer st.Mu.Unlock()
	m := st.migrationCommonLocked(scope, id)
	if m == nil || m.State != MigrationStatePending {
		return false
	}
	m.State = MigrationStateExporting
	m.UpdatedAt = st.CurrentTime()
	st.persistMigrationLocked(scope, id)
	return true
}

// ResetMigrationToPending returns an "exporting" migration to "pending". It is
// how a process that died mid-export leaves its work claimable again: at boot
// nothing is exporting, because no worker is running, so any migration still
// recorded as exporting is the remains of a previous process.
func (st *Store) ResetMigrationToPending(scope MigrationScope, id int) bool {
	st.Mu.Lock()
	defer st.Mu.Unlock()
	m := st.migrationCommonLocked(scope, id)
	if m == nil || m.State != MigrationStateExporting {
		return false
	}
	m.State = MigrationStatePending
	m.UpdatedAt = st.CurrentTime()
	st.persistMigrationLocked(scope, id)
	return true
}

// CompleteMigrationExport records a successful export: where its bytes are,
// how many there are and what they hash to.
func (st *Store) CompleteMigrationExport(scope MigrationScope, id int, archiveKey string, size int64, sha256Hex string) bool {
	st.Mu.Lock()
	defer st.Mu.Unlock()
	m := st.migrationCommonLocked(scope, id)
	if m == nil || m.State != MigrationStateExporting {
		return false
	}
	now := st.CurrentTime()
	m.State = MigrationStateExported
	m.ArchiveKey = archiveKey
	m.ArchiveSize = size
	m.ArchiveSHA256 = sha256Hex
	m.FailureReason = ""
	m.ExportedAt = now
	m.UpdatedAt = now
	st.persistMigrationLocked(scope, id)
	return true
}

// FailMigrationExport records why an export could not be produced. The
// repositories stay locked: GitHub does not release them on failure either,
// because the operator may want to retry against the same frozen state, and
// the unlock operation is theirs to call.
func (st *Store) FailMigrationExport(scope MigrationScope, id int, reason string) bool {
	st.Mu.Lock()
	defer st.Mu.Unlock()
	m := st.migrationCommonLocked(scope, id)
	if m == nil || (m.State != MigrationStateExporting && m.State != MigrationStatePending) {
		return false
	}
	m.State = MigrationStateFailed
	m.FailureReason = reason
	m.UpdatedAt = st.CurrentTime()
	st.persistMigrationLocked(scope, id)
	return true
}

// ClearMigrationArchive marks the archive deleted and forgets where its bytes
// were, returning the key so the caller can delete them from the byte store.
// It returns "" when the migration has no archive to delete.
func (st *Store) ClearMigrationArchive(scope MigrationScope, id int) (string, bool) {
	st.Mu.Lock()
	defer st.Mu.Unlock()
	m := st.migrationCommonLocked(scope, id)
	if m == nil {
		return "", false
	}
	key := m.ArchiveKey
	m.ArchiveDeleted = true
	m.ArchiveKey = ""
	m.ArchiveSize = 0
	m.ArchiveSHA256 = ""
	m.UpdatedAt = st.CurrentTime()
	st.persistMigrationLocked(scope, id)
	return key, true
}

// UnfinishedMigration names one migration an export worker still owes work on.
type UnfinishedMigration struct {
	Scope MigrationScope
	ID    int
}

// ListUnfinishedMigrations returns every migration not yet in a terminal
// state, oldest first within each family. The server calls it at boot to
// resume work a previous process left behind.
func (st *Store) ListUnfinishedMigrations() []UnfinishedMigration {
	st.Mu.RLock()
	defer st.Mu.RUnlock()
	var out []UnfinishedMigration
	for id, m := range st.UserMigrations {
		if m.State == MigrationStatePending || m.State == MigrationStateExporting {
			out = append(out, UnfinishedMigration{Scope: UserMigrationScope, ID: id})
		}
	}
	for id, m := range st.OrgMigrations {
		if m.State == MigrationStatePending || m.State == MigrationStateExporting {
			out = append(out, UnfinishedMigration{Scope: OrgMigrationScope, ID: id})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Scope != out[j].Scope {
			return out[i].Scope < out[j].Scope
		}
		return out[i].ID < out[j].ID
	})
	return out
}

// MigrationArchiveObjectKey is where an export migration's bytes live in the
// object byte store. The guid is in the key so a deleted migration's id being
// reissued can never address the previous migration's archive.
func MigrationArchiveObjectKey(scope MigrationScope, id int, guid string) string {
	return fmt.Sprintf("migrations/%s/%d-%s.tar.gz", scope, id, guid)
}
