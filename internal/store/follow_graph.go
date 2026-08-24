package store

import "sort"

// The follow graph, read as a graph.
//
// Misc.Follows is keyed by login on both sides because an account may follow
// an organization as well as a user, so the edges cannot be expressed as user
// ids. CountFollowers / CountFollowing already answer the cardinality
// questions; these answer the membership ones, which the GraphQL
// User.followers / User.following connections and the viewerIsFollowing
// members need.
//
// Each returns a fresh slice, sorted, so a caller cannot reach the map the
// store keeps (STORE-021) and a connection's page boundaries are stable
// between requests.

// FollowerLoginsOf returns the logins that follow the given account.
func (st *Store) FollowerLoginsOf(login string) []string {
	st.Misc.Mu.RLock()
	defer st.Misc.Mu.RUnlock()
	var out []string
	for follower, follows := range st.Misc.Follows {
		if follows[login] {
			out = append(out, follower)
		}
	}
	sort.Strings(out)
	return out
}

// FollowingLoginsOf returns the logins the given account follows.
func (st *Store) FollowingLoginsOf(login string) []string {
	st.Misc.Mu.RLock()
	defer st.Misc.Mu.RUnlock()
	out := make([]string, 0, len(st.Misc.Follows[login]))
	for target := range st.Misc.Follows[login] {
		out = append(out, target)
	}
	sort.Strings(out)
	return out
}

// LoginFollows reports whether follower follows target. Unlike
// IsUserFollowing it takes logins, so it answers for an organization target
// as well as a user one.
func (st *Store) LoginFollows(follower, target string) bool {
	if follower == "" || target == "" {
		return false
	}
	st.Misc.Mu.RLock()
	defer st.Misc.Mu.RUnlock()
	return st.Misc.Follows[follower][target]
}
