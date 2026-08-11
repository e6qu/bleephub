package store

import (
	"strconv"
	"time"
)

// PutRepoImport stores (creating or replacing) the repo's import record.
func (st *Store) PutRepoImport(imp *RepoImport) {
	st.Mu.Lock()
	defer st.Mu.Unlock()
	imp.UpdatedAt = time.Now().UTC()
	st.RepoImports[imp.RepoID] = imp
	if st.Persist != nil {
		st.Persist.MustPut("repo_imports", strconv.Itoa(imp.RepoID), imp)
	}
}

// ReplaceRepoImportIfCurrent publishes the outcome of a fetch only while the
// record it started from is still the one on file. A cancel or a restart
// replaces that record, and a finishing fetch must not put it back.
func (st *Store) ReplaceRepoImportIfCurrent(previous, next *RepoImport) bool {
	st.Mu.Lock()
	defer st.Mu.Unlock()
	if st.RepoImports[previous.RepoID] != previous {
		return false
	}
	next.UpdatedAt = time.Now().UTC()
	st.RepoImports[next.RepoID] = next
	if st.Persist != nil {
		st.Persist.MustPut("repo_imports", strconv.Itoa(next.RepoID), next)
	}
	return true
}

// GetRepoImport returns the repo's import record, or nil.
func (st *Store) GetRepoImport(repoID int) *RepoImport {
	st.Mu.RLock()
	defer st.Mu.RUnlock()
	return st.RepoImports[repoID]
}

// DeleteRepoImport removes the repo's import record. Returns true if it existed.
func (st *Store) DeleteRepoImport(repoID int) bool {
	st.Mu.Lock()
	defer st.Mu.Unlock()
	if _, ok := st.RepoImports[repoID]; !ok {
		return false
	}
	delete(st.RepoImports, repoID)
	if st.Persist != nil {
		st.Persist.MustDelete("repo_imports", strconv.Itoa(repoID))
	}
	return true
}

// PorterAuthor is one distinct commit author discovered by the import.
type PorterAuthor struct {
	ID         int    `json:"id"`
	RemoteID   string `json:"remote_id"`
	RemoteName string `json:"remote_name"`
	Email      string `json:"email"`
	Name       string `json:"name"`
}
