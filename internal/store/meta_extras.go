package store

// RevokeCredentials deletes every listed credential from all token stores
// and returns how many were revoked.
func (st *Store) RevokeCredentials(credentials []string) int {
	st.Mu.Lock()
	defer st.Mu.Unlock()
	// One transaction across the PAT, user-to-server, refresh and installation
	// buckets, so a crash cannot leave part of a revoke request alive
	// (STORE-001/002).
	batch := NewPersistBatch(st.Persist)
	revoked := 0
	for _, c := range credentials {
		if _, mapKey := st.tokenByValueLocked(c); mapKey != "" {
			st.deleteTokenMapKeyBatchLocked(batch, mapKey)
			revoked++
		}
		if _, ok := st.UserToServerTokens[c]; ok {
			delete(st.UserToServerTokens, c)
			batch.Delete("user_to_server_tokens", c)
			revoked++
		}
		if _, ok := st.RefreshTokens[c]; ok {
			delete(st.RefreshTokens, c)
			batch.Delete("refresh_tokens", c)
			revoked++
		}
		if _, ok := st.InstallationTokens[c]; ok {
			delete(st.InstallationTokens, c)
			batch.Delete("installation_tokens", c)
			revoked++
		}
	}
	if revoked > 0 {
		if err := batch.Commit(); err != nil {
			panic(&PersistenceFailure{Op: "batch", Bucket: "tokens", Err: err})
		}
	}
	return revoked
}
