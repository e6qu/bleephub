package store

import "sort"

// Membership queries over the follow graph. Misc.Follows is keyed by login on
// both sides because an account may follow an organization as well as a user.
// Each returns a fresh sorted slice (STORE-021), keeping connection page
// boundaries stable.

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

// LoginFollows reports whether follower follows target. Unlike IsUserFollowing
// it takes logins, so it answers for organization targets too.
func (st *Store) LoginFollows(follower, target string) bool {
	if follower == "" || target == "" {
		return false
	}
	st.Misc.Mu.RLock()
	defer st.Misc.Mu.RUnlock()
	return st.Misc.Follows[follower][target]
}
