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

// RekeyFollowLogin re-points every follow edge that names oldLogin at newLogin
// after an account rename: the accounts oldLogin followed (its outgoing set) and
// its membership in every other account's following set (its followers). The
// graph is keyed by login on both sides, so a rename that only re-keyed the user
// row would strand both directions — the renamed account would show no
// followers or following, and its old followers' counts would name a login that
// no longer resolves. A pre-existing dangling edge to newLogin is dropped rather
// than turned into a self-follow.
func (st *Store) RekeyFollowLogin(oldLogin, newLogin string) {
	if oldLogin == "" || newLogin == "" || oldLogin == newLogin {
		return
	}
	st.Misc.Mu.Lock()
	defer st.Misc.Mu.Unlock()

	changed := false
	if targets, ok := st.Misc.Follows[oldLogin]; ok {
		dst := st.Misc.Follows[newLogin]
		if dst == nil {
			dst = map[string]bool{}
			st.Misc.Follows[newLogin] = dst
		}
		for target := range targets {
			if target == newLogin {
				continue // a self-follow: SetFollow never records one, so drop it
			}
			dst[target] = true
		}
		delete(st.Misc.Follows, oldLogin)
		changed = true
	}
	for follower, targets := range st.Misc.Follows {
		if !targets[oldLogin] {
			continue
		}
		delete(targets, oldLogin)
		if follower != newLogin {
			targets[newLogin] = true
		}
		changed = true
	}
	if changed && st.Misc.Persist != nil {
		st.Misc.Persist.MustPut("misc", "follows", st.Misc.Follows)
	}
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
