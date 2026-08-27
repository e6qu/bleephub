package store

// The export-migration state machine and its repository locks.
//
// pending -> exporting -> exported|failed. Every transition is a compare-and-set
// under the store lock, so two workers cannot both claim one migration and a
// finished migration cannot be re-entered.
//
// A repository's lock lives on the migration holding it, not on the repository:
// the repo is locked while some migration says so, and releasing the last such
// migration unlocks it.
//
// STORE-021: every getter here returns a detached snapshot.

import (
	"fmt"
	"sort"
	"strings"
)

// migrationLockOwnerLoginLocked resolves the owner whose repositories a
// migration's short-name lock keys belong to. Short names are unique only within
// an owner, so the owner must come from the migration.
func (st *Store) migrationLockOwnerLoginLocked(userID int, orgLogin string) string {
	if orgLogin != "" {
		return orgLogin
	}
	if u := st.Users[userID]; u != nil {
		return u.Login
	}
	return ""
}

// RepoLockedForMigration reports whether any migration still locks the
// repository with this full name. It stays locked until the last of any
// overlapping migrations releases it.
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
	// A GEI repository migration with lockSource freezes its source while it
	// runs. Checked here so the write path asks one question regardless of which
	// migration family froze the repo.
	for _, m := range st.RepositoryMigrations {
		if m.SourceRepoLock != "" && !GEIMigrationTerminal(m.State) && strings.EqualFold(m.SourceRepoLock, fullName) {
			return true
		}
	}
	return false
}

// ListMigrationLockedRepos returns the full names of every repository a
// migration currently locks.
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

// migrationCommonLocked returns the live MigrationCommon, or nil. Callers hold st.Mu.
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

// persistMigrationLocked writes the migration through. Callers hold st.Mu.
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

// ClaimMigrationForExport moves a pending migration into "exporting" and reports
// whether this caller claimed it. Only pending migrations are claimable, so two
// workers cannot export the same migration twice.
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

// ResetMigrationToPending returns an "exporting" migration to "pending". Run at
// boot: any migration still "exporting" is the remains of a process that died
// mid-export, and this makes its work claimable again.
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

// CompleteMigrationExport records a successful export's archive key, size, and hash.
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

// FailMigrationExport records why an export failed. Repositories stay locked, as
// on GitHub: the operator may retry the frozen state and owns the unlock call.
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

// ClearMigrationArchive marks the archive deleted and returns its key so the
// caller can delete the bytes, or "" when there is no archive.
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

// ListUnfinishedMigrations returns every non-terminal migration, oldest first
// within each family. Called at boot to resume a previous process's work.
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

// MigrationArchiveObjectKey locates an export migration's bytes. The guid is in
// the key so a reissued id can never address a deleted migration's archive.
func MigrationArchiveObjectKey(scope MigrationScope, id int, guid string) string {
	return fmt.Sprintf("migrations/%s/%d-%s.tar.gz", scope, id, guid)
}
