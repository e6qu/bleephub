package store

import (
	"errors"
	"fmt"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
)

var (
	ErrTeamNotFound     = errors.New("team not found")
	ErrTeamSlugConflict = errors.New("team slug already exists")
)

// OrgRole is a user's role in an organization. The values are GitHub's
// wire enum for org-membership role.
type OrgRole string

const (
	OrgRoleAdmin  OrgRole = "admin"
	OrgRoleMember OrgRole = "member"
)

// MembershipState is the lifecycle state of an org membership: "pending"
// while an invitation awaits acceptance, "active" once accepted.
type MembershipState string

const (
	MembershipStateActive  MembershipState = "active"
	MembershipStatePending MembershipState = "pending"
)

// TeamPrivacy is GitHub's team visibility enum.
type TeamPrivacy string

const (
	TeamPrivacyClosed TeamPrivacy = "closed"
	TeamPrivacySecret TeamPrivacy = "secret"
)

// TeamPermission is the default repository permission a team confers.
type TeamPermission string

const (
	TeamPermissionPull  TeamPermission = "pull"
	TeamPermissionPush  TeamPermission = "push"
	TeamPermissionAdmin TeamPermission = "admin"
)

// TeamRole is a user's role within a team.
type TeamRole string

const (
	TeamRoleMember     TeamRole = "member"
	TeamRoleMaintainer TeamRole = "maintainer"
)

// TeamNotificationSetting is GitHub's team notification enum.
type TeamNotificationSetting string

const (
	TeamNotificationsEnabled  TeamNotificationSetting = "notifications_enabled"
	TeamNotificationsDisabled TeamNotificationSetting = "notifications_disabled"
)

// Org represents a GitHub organization account.
//
// MembersCanCreateRepositories is a pointer because GitHub's default is
// true: a nil value (including rows persisted before the field existed)
// means "default", not false.
type Org struct {
	ID                           int    `json:"id"`
	NodeID                       string `json:"node_id"`
	Login                        string `json:"login"`
	Name                         string `json:"name"`
	Description                  string `json:"description"`
	Email                        string `json:"email"`
	AvatarURL                    string `json:"avatar_url"`
	Type                         string `json:"type"`
	Company                      string `json:"company"`
	Blog                         string `json:"blog"`
	Location                     string `json:"location"`
	TwitterUsername              string `json:"twitter_username"`
	BillingEmail                 string `json:"billing_email"`
	DefaultRepositoryPermission  string `json:"default_repository_permission"` // "" = GitHub default "read"
	MembersCanCreateRepositories *bool  `json:"members_can_create_repositories"`
	// Granular member-privilege toggles. nil = GitHub's default for that field
	// (true for repos/pages/teams, false for forking private repos).
	MembersCanCreatePublicRepositories  *bool `json:"members_can_create_public_repositories"`
	MembersCanCreatePrivateRepositories *bool `json:"members_can_create_private_repositories"`
	MembersCanCreatePages               *bool `json:"members_can_create_pages"`
	MembersCanForkPrivateRepositories   *bool `json:"members_can_fork_private_repositories"`
	MembersCanCreateTeams               *bool `json:"members_can_create_teams"`
	WebCommitSignoffRequired            bool  `json:"web_commit_signoff_required"`
	// PinnedRepos is the org profile's ordered pinned-repository full names
	// (max MaxPinnedRepos, all owned by the org); a GraphQL/web-only feature,
	// served under /ui-data like user pins.
	PinnedRepos []string  `json:"pinned_repos,omitempty"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

// Membership represents a user's membership in an organization.
type Membership struct {
	OrgID  int             `json:"org_id"`
	UserID int             `json:"user_id"`
	Role   OrgRole         `json:"role"`
	State  MembershipState `json:"state"`
	Public bool            `json:"public"` // publicized via PUT /orgs/{org}/public_members/{username}
}

// Team represents a team within an organization.
type Team struct {
	ID                  int                       `json:"id"`
	NodeID              string                    `json:"node_id"`
	OrgID               int                       `json:"org_id"`
	Name                string                    `json:"name"`
	Slug                string                    `json:"slug"`
	Description         string                    `json:"description"`
	Privacy             TeamPrivacy               `json:"privacy"`
	Permission          TeamPermission            `json:"permission"`
	NotificationSetting TeamNotificationSetting   `json:"notification_setting"`
	ParentID            int                       `json:"parent_id"` // 0 = no parent team
	MemberIDs           []int                     `json:"member_ids"`
	MaintainerIDs       []int                     `json:"maintainer_ids"`   // subset of MemberIDs with the maintainer role
	RepoNames           []string                  `json:"repo_names"`       // "owner/name" entries
	RepoPermissions     map[string]TeamPermission `json:"repo_permissions"` // per-repo override; nil/missing entry uses Permission
	CreatedAt           time.Time                 `json:"created_at"`
	UpdatedAt           time.Time                 `json:"updated_at"`
}

// MembershipKey returns the map key for org/user membership lookups.
func MembershipKey(orgLogin string, userID int) string {
	return fmt.Sprintf("%s/%d", orgLogin, userID)
}

// TeamSlugKey returns the map key for org/team slug lookups.
func TeamSlugKey(orgLogin, slug string) string {
	return orgLogin + "/" + slug
}

// Slugify converts a team name to a URL-safe slug.
func Slugify(name string) string {
	s := strings.ToLower(name)
	s = strings.ReplaceAll(s, " ", "-")
	return s
}

// CreateOrg creates an organization and adds the creator as an admin member.
func (st *Store) CreateOrg(creator *User, login, name, description string) *Org {
	st.Mu.Lock()
	defer st.Mu.Unlock()

	// Folded existence check: GitHub rejects an organization whose login
	// differs from an existing one only by case, and the folded index
	// (name_fold.go) relies on canonical logins never colliding under folding.
	if st.OrgByLoginLocked(login) != nil {
		return nil
	}

	now := st.CurrentTime()
	orgID := st.ReserveGlobalID("next_org", &st.NextOrg)
	org := &Org{
		ID:          orgID,
		NodeID:      fmt.Sprintf("O_kgDO%08d", orgID),
		Login:       login,
		Name:        name,
		Description: description,
		Type:        "Organization",
		CreatedAt:   now,
		UpdatedAt:   now,
	}

	st.Orgs[org.ID] = org
	st.OrgsByLogin[login] = org
	st.IndexOrgLoginLocked(login)

	// Add creator as admin
	key := MembershipKey(login, creator.ID)
	m := &Membership{
		OrgID:  org.ID,
		UserID: creator.ID,
		Role:   OrgRoleAdmin,
		State:  MembershipStateActive,
	}
	st.Memberships[key] = m

	// One transaction: an org must never persist without its creator's admin
	// membership, which would leave it with no administrator.
	batch := NewPersistBatch(st.Persist)
	batch.Put("orgs", strconv.Itoa(org.ID), org)
	batch.Put("memberships", key, m)
	if err := batch.Commit(); err != nil {
		panic(&PersistenceFailure{Op: "batch", Bucket: "orgs", Err: err})
	}

	return org
}

// GetOrg returns an organization by login, or nil if not found.
// cloneOrg returns a copy safe to hand outside the store lock (STORE-021):
// the member-privilege *bool flags are the reference fields. Org writes go
// through the keyed UpdateOrg or mutate the live st.Orgs row directly.
func cloneOrg(o *Org) *Org {
	if o == nil {
		return nil
	}
	clone := *o
	dup := func(p *bool) *bool {
		if p == nil {
			return nil
		}
		v := *p
		return &v
	}
	clone.MembersCanCreateRepositories = dup(o.MembersCanCreateRepositories)
	clone.MembersCanCreatePublicRepositories = dup(o.MembersCanCreatePublicRepositories)
	clone.MembersCanCreatePrivateRepositories = dup(o.MembersCanCreatePrivateRepositories)
	clone.MembersCanCreatePages = dup(o.MembersCanCreatePages)
	clone.MembersCanForkPrivateRepositories = dup(o.MembersCanForkPrivateRepositories)
	clone.MembersCanCreateTeams = dup(o.MembersCanCreateTeams)
	clone.PinnedRepos = append([]string(nil), o.PinnedRepos...)
	return &clone
}

func (st *Store) GetOrg(login string) *Org {
	st.Mu.RLock()
	defer st.Mu.RUnlock()
	return cloneOrg(st.OrgByLoginLocked(login))
}

// GetOrgByID returns an organization by its numeric ID, or nil.
func (st *Store) GetOrgByID(id int) *Org {
	st.Mu.RLock()
	defer st.Mu.RUnlock()
	return cloneOrg(st.Orgs[id])
}

// ListTeamsByUser returns every team across all orgs that the given user is a member of.
func (st *Store) ListTeamsByUser(userID int) []*Team {
	st.Mu.RLock()
	defer st.Mu.RUnlock()

	var teams []*Team
	for _, t := range st.Teams {
		for _, mid := range t.MemberIDs {
			if mid == userID {
				teams = append(teams, t)
				break
			}
		}
	}
	return snapshotTeams(teams)
}

// UpdateOrg applies a mutation function to an organization.
func (st *Store) UpdateOrg(login string, fn func(*Org)) bool {
	st.Mu.Lock()
	defer st.Mu.Unlock()

	org := st.OrgByLoginLocked(login)
	if org == nil {
		return false
	}
	fn(org)
	org.UpdatedAt = st.CurrentTime()
	if st.Persist != nil {
		st.Persist.MustPut("orgs", strconv.Itoa(org.ID), org)
	}
	return true
}

// DeleteOrg removes an organization and everything scoped to it.
func (st *Store) DeleteOrg(login string) bool {
	deleted, err := st.DeleteOrgWithError(login)
	if err != nil {
		panic(&PersistenceFailure{Op: "delete", Bucket: "orgs", Key: login, Err: err})
	}
	return deleted
}

// DeleteOrgWithError removes an organization, its memberships, its teams and
// its repositories. The repositories are part of the cascade because an
// organization row that goes away while its repositories stay behind leaves
// every later start rejecting those rows as an unknown owner.
func (st *Store) DeleteOrgWithError(login string) (bool, error) {
	// Canonicalize up front: the deletion intent recorded by
	// deleteOrgMetadata and the intent cleared below must use one key.
	if org := st.GetOrg(login); org != nil {
		login = org.Login
	}
	repoIntents, orgIntent, deleted, err := st.deleteOrgMetadata(login)
	if err != nil || !deleted {
		return deleted, err
	}
	for _, intent := range repoIntents {
		if err := st.PurgeDeletedRepoBytes(intent.Name, intent); err != nil {
			return true, fmt.Errorf("delete organization %s: %w", login, err)
		}
	}
	// Reclaim the bytes of the org's directly-owned packages (kind-agnostic
	// cleanup; runs without st.Mu like the repo intents above).
	if err := st.CleanupDeletedRepo(orgIntent); err != nil {
		return true, fmt.Errorf("delete organization %s external data: %w", login, err)
	}
	if err := st.Persist.Delete(PendingDeletionsBucket, PendingOrgDeletionKey(login)); err != nil {
		return true, fmt.Errorf("delete organization %s: clear deletion intent: %w", login, err)
	}
	return true, nil
}

// deleteOrgMetadata purges the organization from memory and, in one
// transaction, from the database. It returns the repositories whose bytes the
// caller must still destroy.
func (st *Store) deleteOrgMetadata(login string) ([]PendingDeletion, PendingDeletion, bool, error) {
	st.Mu.Lock()
	defer st.Mu.Unlock()

	org := st.OrgByLoginLocked(login)
	if org == nil {
		return nil, PendingDeletion{}, false, nil
	}
	login = org.Login

	// Build the org's own deletion intent, scheduling the file bytes of packages
	// owned directly by the organization (not via one of its repositories) for
	// external cleanup — otherwise they orphan when the org row goes away
	// (STORE-028). Package rows are dropped from st.Packages in the batch below.
	orgIntent := PendingDeletion{Kind: "org", Name: login, StartedAt: st.CurrentTime()}
	addPackageFile := func(path string) {
		if path == "" {
			return
		}
		if st.ObjectByteStore != nil {
			orgIntent.ObjectKeys = append(orgIntent.ObjectKeys, path)
		} else {
			orgIntent.LocalFiles = append(orgIntent.LocalFiles, path)
		}
	}
	var orgPackageIDs []int
	for pkgID, pkg := range st.Packages {
		if pkg.OwnerType != "Organization" || pkg.OwnerKey != login {
			continue
		}
		orgPackageIDs = append(orgPackageIDs, pkgID)
		for versionID := range st.PackageVersionsByPackage[pkgID] {
			for _, file := range st.PackageFilesByVersion[versionID] {
				addPackageFile(file.StoragePath)
			}
		}
	}
	sort.Strings(orgIntent.ObjectKeys)
	sort.Strings(orgIntent.LocalFiles)

	if err := st.Persist.Put(PendingDeletionsBucket, PendingOrgDeletionKey(login), orgIntent); err != nil {
		return nil, PendingDeletion{}, true, fmt.Errorf("delete organization %s: record deletion intent: %w", login, err)
	}

	var repoNames []string
	for fullName, repo := range st.ReposByName {
		if repo.OwnerType == "Organization" && repo.OwnerID == org.ID {
			repoNames = append(repoNames, fullName)
		}
	}
	sort.Strings(repoNames)
	repoIntents := make([]PendingDeletion, 0, len(repoNames))
	for _, fullName := range repoNames {
		owner, name, ok := SplitRepoFullName(fullName)
		if !ok {
			return nil, PendingDeletion{}, true, fmt.Errorf("delete organization %s: repository key %q is not an owner/name pair", login, fullName)
		}
		_, intent, err := st.deleteRepoLocked(owner, name)
		if err != nil {
			return nil, PendingDeletion{}, true, fmt.Errorf("delete organization %s: %w", login, err)
		}
		repoIntents = append(repoIntents, intent)
	}

	batch := NewPersistBatch(st.Persist)

	// Cascade the org's directly-owned packages, versions and files (STORE-028).
	for _, pkgID := range orgPackageIDs {
		delete(st.Packages, pkgID)
		batch.Delete("packages", strconv.Itoa(pkgID))
		for versionID := range st.PackageVersionsByPackage[pkgID] {
			delete(st.PackageVersions, versionID)
			batch.Delete("package_versions", strconv.Itoa(versionID))
			for fileID, file := range st.PackageFiles {
				if file.VersionID == versionID {
					delete(st.PackageFiles, fileID)
					batch.Delete("package_files", strconv.Itoa(fileID))
				}
			}
			delete(st.PackageFilesByVersion, versionID)
		}
		delete(st.PackageVersionsByPackage, pkgID)
	}
	delete(st.PackagesByOwnerKey, login)

	// Cancel the org's Marketplace purchases so a deleted org keeps no live
	// subscription (STORE-028). Store→Misc is the required lock order, and
	// st.Mu is held here, so taking Misc.Mu is safe.
	st.Misc.Mu.Lock()
	for k, purchase := range st.Misc.MarketplacePurchases {
		if purchase.AccountType == "Organization" && purchase.AccountID == org.ID {
			delete(st.Misc.MarketplacePurchases, k)
			batch.Delete("marketplace_purchases", k)
		}
	}
	st.Misc.Mu.Unlock()

	for k, m := range st.Memberships {
		if m.OrgID == org.ID {
			delete(st.Memberships, k)
			batch.Delete("memberships", k)
		}
	}
	for k, t := range st.TeamsBySlug {
		if t.OrgID == org.ID {
			delete(st.Teams, t.ID)
			delete(st.TeamsBySlug, k)
			batch.Delete("teams", strconv.Itoa(t.ID))
		}
	}

	delete(st.Orgs, org.ID)
	delete(st.OrgsByLogin, login)
	st.UnindexOrgLoginLocked(login)
	batch.Delete("orgs", strconv.Itoa(org.ID))
	if err := batch.Commit(); err != nil {
		return nil, PendingDeletion{}, true, fmt.Errorf("delete organization %s: %w", login, err)
	}
	return repoIntents, orgIntent, true, nil
}

// deleteUserOwnedResourcesLocked cascades the resources a deleted user account
// owns — repositories, directly-owned packages (with their file bytes) and
// Marketplace purchases — plus its organization memberships, mirroring the
// organization cascade so a user deletion leaves no orphaned rows or object
// bytes (STORE-028). The caller holds st.Mu; it returns the repository intents
// and the user's own package-byte intent for the caller to drain after
// releasing the lock.
func (st *Store) DeleteUserOwnedResourcesLocked(u *User) ([]PendingDeletion, PendingDeletion, error) {
	var repoNames []string
	for fullName, repo := range st.ReposByName {
		if repo.OwnerType == "User" && repo.OwnerID == u.ID {
			repoNames = append(repoNames, fullName)
		}
	}
	sort.Strings(repoNames)
	repoIntents := make([]PendingDeletion, 0, len(repoNames))
	for _, fullName := range repoNames {
		owner, name, ok := SplitRepoFullName(fullName)
		if !ok {
			return nil, PendingDeletion{}, fmt.Errorf("delete user %s: repository key %q is not an owner/name pair", u.Login, fullName)
		}
		_, intent, err := st.deleteRepoLocked(owner, name)
		if err != nil {
			return nil, PendingDeletion{}, fmt.Errorf("delete user %s: %w", u.Login, err)
		}
		repoIntents = append(repoIntents, intent)
	}

	userIntent := PendingDeletion{Kind: "user", Name: u.Login, StartedAt: st.CurrentTime()}
	addPackageFile := func(path string) {
		if path == "" {
			return
		}
		if st.ObjectByteStore != nil {
			userIntent.ObjectKeys = append(userIntent.ObjectKeys, path)
		} else {
			userIntent.LocalFiles = append(userIntent.LocalFiles, path)
		}
	}
	var userPackageIDs []int
	for pkgID, pkg := range st.Packages {
		if pkg.OwnerType != "User" || pkg.OwnerKey != u.Login {
			continue
		}
		userPackageIDs = append(userPackageIDs, pkgID)
		for versionID := range st.PackageVersionsByPackage[pkgID] {
			for _, file := range st.PackageFilesByVersion[versionID] {
				addPackageFile(file.StoragePath)
			}
		}
	}
	sort.Strings(userIntent.ObjectKeys)
	sort.Strings(userIntent.LocalFiles)
	if len(userIntent.ObjectKeys) > 0 || len(userIntent.LocalFiles) > 0 {
		if err := st.Persist.Put(PendingDeletionsBucket, PendingUserDeletionKey(u.Login), userIntent); err != nil {
			return nil, PendingDeletion{}, fmt.Errorf("delete user %s: record deletion intent: %w", u.Login, err)
		}
	}

	batch := NewPersistBatch(st.Persist)
	for _, pkgID := range userPackageIDs {
		delete(st.Packages, pkgID)
		batch.Delete("packages", strconv.Itoa(pkgID))
		for versionID := range st.PackageVersionsByPackage[pkgID] {
			delete(st.PackageVersions, versionID)
			batch.Delete("package_versions", strconv.Itoa(versionID))
			for fileID, file := range st.PackageFiles {
				if file.VersionID == versionID {
					delete(st.PackageFiles, fileID)
					batch.Delete("package_files", strconv.Itoa(fileID))
				}
			}
			delete(st.PackageFilesByVersion, versionID)
		}
		delete(st.PackageVersionsByPackage, pkgID)
	}
	delete(st.PackagesByOwnerKey, u.Login)

	st.Misc.Mu.Lock()
	for k, purchase := range st.Misc.MarketplacePurchases {
		if purchase.AccountType == "User" && purchase.AccountID == u.ID {
			delete(st.Misc.MarketplacePurchases, k)
			batch.Delete("marketplace_purchases", k)
		}
	}
	st.Misc.Mu.Unlock()

	for k, m := range st.Memberships {
		if m.UserID == u.ID {
			delete(st.Memberships, k)
			batch.Delete("memberships", k)
		}
	}

	if err := batch.Commit(); err != nil {
		return nil, PendingDeletion{}, fmt.Errorf("delete user %s: %w", u.Login, err)
	}
	return repoIntents, userIntent, nil
}

// ListOrgsByUser returns all organizations the user belongs to, in
// ascending id order like real GitHub. The memberships map iterates in
// random order; the /user/orgs handlers paginate over this list, and
// offset pagination over an unstable order would skip or duplicate orgs
// across pages.
func (st *Store) ListOrgsByUser(userID int) []*Org {
	st.Mu.RLock()
	defer st.Mu.RUnlock()

	return snapshotOrgs(st.listOrgsByUserLocked(userID, false))
}

// ListPublicOrgsByUser returns only active organization memberships that the
// user has explicitly publicized. This is the visibility contract behind
// GET /users/{username}/orgs; the authenticated /user/orgs endpoint uses
// ListOrgsByUser and therefore continues to include concealed memberships.
func (st *Store) ListPublicOrgsByUser(userID int) []*Org {
	st.Mu.RLock()
	defer st.Mu.RUnlock()

	return snapshotOrgs(st.listOrgsByUserLocked(userID, true))
}

func (st *Store) listOrgsByUserLocked(userID int, publicOnly bool) []*Org {
	var orgs []*Org
	for _, m := range st.Memberships {
		if m.UserID == userID && m.State == MembershipStateActive && (!publicOnly || m.Public) {
			if org, ok := st.Orgs[m.OrgID]; ok {
				orgs = append(orgs, org)
			}
		}
	}
	sort.Slice(orgs, func(i, j int) bool { return orgs[i].ID < orgs[j].ID })
	return orgs
}

// SetMembership upserts a user's membership in an organization with the
// given role and state. An existing membership keeps its Public flag.
// Returns the stored membership, or nil if the org doesn't exist.
func (st *Store) SetMembership(orgLogin string, userID int, role OrgRole, state MembershipState) *Membership {
	st.Mu.Lock()
	defer st.Mu.Unlock()

	org := st.OrgByLoginLocked(orgLogin)
	if org == nil {
		return nil
	}
	orgLogin = org.Login

	orgLogin = st.canonicalOrgLoginLocked(orgLogin)
	key := MembershipKey(orgLogin, userID)
	m := st.Memberships[key]
	if m == nil {
		m = &Membership{OrgID: org.ID, UserID: userID}
		st.Memberships[key] = m
	}
	m.Role = role
	m.State = state
	if st.Persist != nil {
		st.Persist.MustPut("memberships", key, m)
	}
	// An activated membership completes any organization invitation the
	// user held: the invitee joins the invited teams and the invitation
	// row is consumed.
	if state == MembershipStateActive {
		st.consumeOrgInvitationsForUserLocked(orgLogin, userID)
	}
	return m
}

// SetMembershipPublic flips the membership's public-member flag. Returns
// false when no active membership exists.
func (st *Store) SetMembershipPublic(orgLogin string, userID int, public bool) bool {
	st.Mu.Lock()
	defer st.Mu.Unlock()

	orgLogin = st.canonicalOrgLoginLocked(orgLogin)
	key := MembershipKey(orgLogin, userID)
	m := st.Memberships[key]
	if m == nil || m.State != MembershipStateActive {
		return false
	}
	m.Public = public
	if st.Persist != nil {
		st.Persist.MustPut("memberships", key, m)
	}
	return true
}

// ListPublicOrgMembers returns active members who publicized their membership.
func (st *Store) ListPublicOrgMembers(orgLogin string) []*User {
	st.Mu.RLock()
	defer st.Mu.RUnlock()

	org := st.OrgByLoginLocked(orgLogin)
	if org == nil {
		return nil
	}
	var users []*User
	for _, m := range st.Memberships {
		if m.OrgID == org.ID && m.State == MembershipStateActive && m.Public {
			if u, ok := st.Users[m.UserID]; ok {
				users = append(users, u)
			}
		}
	}
	return snapshotUsers(users)
}

// ListMembershipsByUser returns the user's memberships across all orgs,
// optionally filtered by state ("" = all).
func (st *Store) ListMembershipsByUser(userID int, state MembershipState) []*Membership {
	st.Mu.RLock()
	defer st.Mu.RUnlock()

	var out []*Membership
	for _, m := range st.Memberships {
		if m.UserID != userID {
			continue
		}
		if state != "" && m.State != state {
			continue
		}
		out = append(out, m)
	}
	return snapshotSlice(out)
}

// ListOrgsAll returns every organization with ID greater than `since`,
// ordered by ID ascending — the GET /organizations contract.
func (st *Store) ListOrgsAll(since int) []*Org {
	st.Mu.RLock()
	defer st.Mu.RUnlock()

	var orgs []*Org
	for _, o := range st.Orgs {
		if o.ID > since {
			orgs = append(orgs, o)
		}
	}
	sort.Slice(orgs, func(i, j int) bool { return orgs[i].ID < orgs[j].ID })
	return snapshotOrgs(orgs)
}

// GetMembership returns a user's membership in an organization, or nil.
func (st *Store) GetMembership(orgLogin string, userID int) *Membership {
	st.Mu.RLock()
	defer st.Mu.RUnlock()
	// A copy so a reader can't mutate the stored membership through the getter
	// (STORE-021); Membership is all-value, so a shallow copy detaches. Its 24
	// callers only read State/Role, and writes go through keyed store methods.
	m := st.Memberships[MembershipKey(st.canonicalOrgLoginLocked(orgLogin), userID)]
	if m == nil {
		return nil
	}
	clone := *m
	return &clone
}

// RemoveMembership removes a user's membership from an organization.
func (st *Store) RemoveMembership(orgLogin string, userID int) bool {
	st.Mu.Lock()
	defer st.Mu.Unlock()

	orgLogin = st.canonicalOrgLoginLocked(orgLogin)
	key := MembershipKey(orgLogin, userID)
	if _, ok := st.Memberships[key]; !ok {
		return false
	}
	// One transaction: the membership removal and every team it drops the user
	// from commit together, so a crash cannot leave the user out of the org yet
	// still listed on its teams.
	batch := NewPersistBatch(st.Persist)
	delete(st.Memberships, key)
	batch.Delete("memberships", key)

	org := st.OrgByLoginLocked(orgLogin)
	if org != nil {
		for _, t := range st.TeamsBySlug {
			if t.OrgID == org.ID {
				for i, mid := range t.MemberIDs {
					if mid == userID {
						t.MemberIDs = append(t.MemberIDs[:i], t.MemberIDs[i+1:]...)
						batch.Put("teams", strconv.Itoa(t.ID), t)
						break
					}
				}
			}
		}
	}
	if err := batch.Commit(); err != nil {
		panic(&PersistenceFailure{Op: "batch", Bucket: "memberships", Err: err})
	}

	return true
}

// ListOrgMembers returns all users who are active members of an organization.
func (st *Store) ListOrgMembers(orgLogin string) []*User {
	st.Mu.RLock()
	defer st.Mu.RUnlock()

	org := st.OrgByLoginLocked(orgLogin)
	if org == nil {
		return nil
	}

	var users []*User
	for _, m := range st.Memberships {
		if m.OrgID == org.ID && m.State == "active" {
			if u, ok := st.Users[m.UserID]; ok {
				users = append(users, u)
			}
		}
	}
	return snapshotUsers(users)
}

// TeamOptions carries the optional attributes of team creation.
type TeamOptions struct {
	Description         string
	Privacy             TeamPrivacy
	Permission          TeamPermission
	NotificationSetting TeamNotificationSetting
	ParentID            int
}

// CreateTeam creates a team within an organization. A non-zero ParentID
// must reference an existing team in the same org.
func (st *Store) CreateTeam(orgLogin, name string, opts TeamOptions) *Team {
	st.Mu.Lock()
	defer st.Mu.Unlock()

	org := st.OrgByLoginLocked(orgLogin)
	if org == nil {
		return nil
	}
	orgLogin = org.Login

	slug := Slugify(name)
	orgLogin = st.canonicalOrgLoginLocked(orgLogin)
	key := TeamSlugKey(orgLogin, slug)
	if _, exists := st.TeamsBySlug[key]; exists {
		return nil
	}

	if opts.Privacy == "" {
		opts.Privacy = TeamPrivacyClosed
	}
	if opts.Permission == "" {
		opts.Permission = TeamPermissionPull
	}
	if opts.NotificationSetting == "" {
		opts.NotificationSetting = TeamNotificationsEnabled
	}
	if opts.ParentID != 0 {
		parent := st.Teams[opts.ParentID]
		if parent == nil || parent.OrgID != org.ID {
			return nil
		}
	}

	now := st.CurrentTime()
	teamID := st.ReserveGlobalID("next_team", &st.NextTeam)
	team := &Team{
		ID:                  teamID,
		NodeID:              fmt.Sprintf("T_kgDO%08d", teamID),
		OrgID:               org.ID,
		Name:                name,
		Slug:                slug,
		Description:         opts.Description,
		Privacy:             opts.Privacy,
		Permission:          opts.Permission,
		NotificationSetting: opts.NotificationSetting,
		ParentID:            opts.ParentID,
		MemberIDs:           []int{},
		MaintainerIDs:       []int{},
		RepoNames:           []string{},
		RepoPermissions:     map[string]TeamPermission{},
		CreatedAt:           now,
		UpdatedAt:           now,
	}

	st.Teams[team.ID] = team
	st.TeamsBySlug[key] = team
	if st.Persist != nil {
		st.Persist.MustPut("teams", strconv.Itoa(team.ID), team)
	}
	return team
}

// GetTeam returns a team by org login and slug, or nil.
// cloneTeam returns a deep copy safe to hand outside the store lock
// (STORE-021): MemberIDs, MaintainerIDs, RepoNames and RepoPermissions are the
// reference fields. Team writes go through the keyed UpdateTeam (or mutate the
// live st.Teams row directly), never a getter result.
func cloneTeam(t *Team) *Team {
	if t == nil {
		return nil
	}
	clone := *t
	if t.MemberIDs != nil {
		clone.MemberIDs = append([]int(nil), t.MemberIDs...)
	}
	if t.MaintainerIDs != nil {
		clone.MaintainerIDs = append([]int(nil), t.MaintainerIDs...)
	}
	if t.RepoNames != nil {
		clone.RepoNames = append([]string(nil), t.RepoNames...)
	}
	if t.RepoPermissions != nil {
		clone.RepoPermissions = make(map[string]TeamPermission, len(t.RepoPermissions))
		for k, v := range t.RepoPermissions {
			clone.RepoPermissions[k] = v
		}
	}
	return &clone
}

func (st *Store) GetTeam(orgLogin, slug string) *Team {
	st.Mu.RLock()
	defer st.Mu.RUnlock()
	return cloneTeam(st.TeamsBySlug[TeamSlugKey(st.canonicalOrgLoginLocked(orgLogin), slug)])
}

// GetTeamByID returns a team by its numeric ID, or nil.
func (st *Store) GetTeamByID(id int) *Team {
	st.Mu.RLock()
	defer st.Mu.RUnlock()
	return cloneTeam(st.Teams[id])
}

// UpdateTeam applies a mutation function to a team. When the mutation
// changes the slug (team rename), the slug index is re-keyed so the old
// slug stops resolving and the new one does.
func (st *Store) UpdateTeam(orgLogin, slug string, fn func(*Team)) bool {
	return st.UpdateTeamChecked(orgLogin, slug, fn) == nil
}

// UpdateTeamChecked applies a team mutation atomically and refuses a rename
// whose derived slug is already occupied. The callback receives a detached
// copy, so validation failure cannot partially mutate the live team or its
// secondary slug index.
func (st *Store) UpdateTeamChecked(orgLogin, slug string, fn func(*Team)) error {
	st.Mu.Lock()
	defer st.Mu.Unlock()

	orgLogin = st.canonicalOrgLoginLocked(orgLogin)
	key := TeamSlugKey(orgLogin, slug)
	team, ok := st.TeamsBySlug[key]
	if !ok {
		return ErrTeamNotFound
	}
	updated := *team
	updated.MemberIDs = append([]int(nil), team.MemberIDs...)
	updated.MaintainerIDs = append([]int(nil), team.MaintainerIDs...)
	updated.RepoNames = append([]string(nil), team.RepoNames...)
	updated.RepoPermissions = make(map[string]TeamPermission, len(team.RepoPermissions))
	for name, permission := range team.RepoPermissions {
		updated.RepoPermissions[name] = permission
	}
	fn(&updated)
	if updated.Slug != slug {
		newKey := TeamSlugKey(orgLogin, updated.Slug)
		if occupied := st.TeamsBySlug[newKey]; occupied != nil && occupied.ID != team.ID {
			return ErrTeamSlugConflict
		}
		delete(st.TeamsBySlug, key)
		st.TeamsBySlug[newKey] = team
	}
	updated.UpdatedAt = st.CurrentTime()
	*team = updated
	if st.Persist != nil {
		st.Persist.MustPut("teams", strconv.Itoa(team.ID), team)
	}
	return nil
}

// TeamParentWouldCycle reports whether re-parenting team `teamID` under
// `parentID` would create a cycle in the team hierarchy.
func (st *Store) TeamParentWouldCycle(teamID, parentID int) bool {
	st.Mu.RLock()
	defer st.Mu.RUnlock()

	for id := parentID; id != 0; {
		if id == teamID {
			return true
		}
		parent := st.Teams[id]
		if parent == nil {
			return false
		}
		id = parent.ParentID
	}
	return false
}

// ListChildTeams returns the teams whose parent is the given team.
func (st *Store) ListChildTeams(orgLogin string, parentID int) []*Team {
	st.Mu.RLock()
	defer st.Mu.RUnlock()

	org := st.OrgByLoginLocked(orgLogin)
	if org == nil {
		return nil
	}
	var out []*Team
	for _, t := range st.TeamsBySlug {
		if t.OrgID == org.ID && t.ParentID == parentID {
			out = append(out, t)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return snapshotTeams(out)
}

// DeleteTeam removes a team from an organization.
func (st *Store) DeleteTeam(orgLogin, slug string) bool {
	st.Mu.Lock()
	defer st.Mu.Unlock()

	orgLogin = st.canonicalOrgLoginLocked(orgLogin)
	key := TeamSlugKey(orgLogin, slug)
	team, ok := st.TeamsBySlug[key]
	if !ok {
		return false
	}

	delete(st.Teams, team.ID)
	delete(st.TeamsBySlug, key)

	// One transaction: deleting the team and re-parenting its children must not
	// disagree across a crash, or a surviving child would point at a team that
	// no longer exists.
	batch := NewPersistBatch(st.Persist)
	batch.Delete("teams", strconv.Itoa(team.ID))
	// Children of a deleted team move up to the deleted team's parent
	// (real GitHub re-parents rather than orphaning).
	for _, t := range st.Teams {
		if t.ParentID == team.ID {
			t.ParentID = team.ParentID
			batch.Put("teams", strconv.Itoa(t.ID), t)
		}
	}
	if err := batch.Commit(); err != nil {
		panic(&PersistenceFailure{Op: "batch", Bucket: "teams", Err: err})
	}
	return true
}

// ListTeams returns all teams in an organization.
func (st *Store) ListTeams(orgLogin string) []*Team {
	st.Mu.RLock()
	defer st.Mu.RUnlock()

	org := st.OrgByLoginLocked(orgLogin)
	if org == nil {
		return nil
	}

	var teams []*Team
	for _, t := range st.TeamsBySlug {
		if t.OrgID == org.ID {
			teams = append(teams, t)
		}
	}
	return snapshotTeams(teams)
}

// ListTeamMembers returns the users who are members of a team.
func (st *Store) ListTeamMembers(orgLogin, slug string) []*User {
	st.Mu.RLock()
	defer st.Mu.RUnlock()

	team := st.TeamsBySlug[TeamSlugKey(st.canonicalOrgLoginLocked(orgLogin), slug)]
	if team == nil {
		return nil
	}
	members := make([]*User, 0, len(team.MemberIDs))
	for _, uid := range team.MemberIDs {
		if u, ok := st.Users[uid]; ok {
			members = append(members, u)
		}
	}
	return snapshotUsers(members)
}

// GetTeamMembership returns a user's role in a team and whether they are a
// member at all. The role is empty when the user is not a member.
func (st *Store) GetTeamMembership(orgLogin, slug string, userID int) (TeamRole, bool) {
	st.Mu.RLock()
	defer st.Mu.RUnlock()

	team := st.TeamsBySlug[TeamSlugKey(st.canonicalOrgLoginLocked(orgLogin), slug)]
	if team == nil {
		return "", false
	}
	return team.RoleOf(userID)
}

// SetTeamMembership upserts a user's team membership with the given role.
func (st *Store) SetTeamMembership(orgLogin, slug string, userID int, role TeamRole) bool {
	st.Mu.Lock()
	defer st.Mu.Unlock()

	team := st.TeamsBySlug[TeamSlugKey(st.canonicalOrgLoginLocked(orgLogin), slug)]
	if team == nil {
		return false
	}

	if !slices.Contains(team.MemberIDs, userID) {
		team.MemberIDs = append(team.MemberIDs, userID)
	}
	switch role {
	case TeamRoleMaintainer:
		if !slices.Contains(team.MaintainerIDs, userID) {
			team.MaintainerIDs = append(team.MaintainerIDs, userID)
		}
	default:
		team.MaintainerIDs = intSliceRemove(team.MaintainerIDs, userID)
	}
	team.UpdatedAt = st.CurrentTime()
	if st.Persist != nil {
		st.Persist.MustPut("teams", strconv.Itoa(team.ID), team)
	}
	return true
}

// RemoveTeamMembership removes a user from a team.
func (st *Store) RemoveTeamMembership(orgLogin, slug string, userID int) bool {
	st.Mu.Lock()
	defer st.Mu.Unlock()

	team := st.TeamsBySlug[TeamSlugKey(st.canonicalOrgLoginLocked(orgLogin), slug)]
	if team == nil {
		return false
	}
	if !slices.Contains(team.MemberIDs, userID) {
		return false
	}
	team.MemberIDs = intSliceRemove(team.MemberIDs, userID)
	team.MaintainerIDs = intSliceRemove(team.MaintainerIDs, userID)
	team.UpdatedAt = st.CurrentTime()
	if st.Persist != nil {
		st.Persist.MustPut("teams", strconv.Itoa(team.ID), team)
	}
	return true
}

// ListTeamRepos returns the repositories linked to a team.
func (st *Store) ListTeamRepos(orgLogin, slug string) []*Repo {
	st.Mu.RLock()
	defer st.Mu.RUnlock()

	team := st.TeamsBySlug[TeamSlugKey(st.canonicalOrgLoginLocked(orgLogin), slug)]
	if team == nil {
		return nil
	}
	repos := make([]*Repo, 0, len(team.RepoNames))
	for _, fullName := range team.RepoNames {
		owner, name, ok := strings.Cut(fullName, "/")
		if !ok {
			continue
		}
		if repo := st.ReposByName[owner+"/"+name]; repo != nil {
			repos = append(repos, repo)
		}
	}
	return snapshotRepos(repos)
}

// GetTeamRepoPermission returns the effective permission a team confers on a
// repository. The second value is false when the repository is not linked to
// the team. A nil/missing per-repo override falls back to the team's default
// Permission.
func (st *Store) GetTeamRepoPermission(orgLogin, slug, fullName string) (TeamPermission, bool) {
	st.Mu.RLock()
	defer st.Mu.RUnlock()

	team := st.TeamsBySlug[TeamSlugKey(st.canonicalOrgLoginLocked(orgLogin), slug)]
	if team == nil {
		return "", false
	}
	if repo := st.RepoByNameLocked(fullName); repo != nil {
		fullName = repo.FullName
	}
	if !slices.Contains(team.RepoNames, fullName) {
		return "", false
	}
	if team.RepoPermissions != nil {
		if perm, ok := team.RepoPermissions[fullName]; ok {
			return perm, true
		}
	}
	return team.Permission, true
}

// SetTeamRepoPermission links a repository to a team and records an explicit
// permission override. An empty permission uses the team's default.
func (st *Store) SetTeamRepoPermission(orgLogin, slug, fullName string, perm TeamPermission) bool {
	st.Mu.Lock()
	defer st.Mu.Unlock()

	team := st.TeamsBySlug[TeamSlugKey(st.canonicalOrgLoginLocked(orgLogin), slug)]
	if team == nil {
		return false
	}
	// Store the canonical repo key: the permission lattice (rbac.go) compares
	// team.RepoNames against repo.FullName.
	repo := st.RepoByNameLocked(fullName)
	if repo == nil {
		return false
	}
	fullName = repo.FullName

	found := false
	for _, rn := range team.RepoNames {
		if rn == fullName {
			found = true
			break
		}
	}
	if !found {
		team.RepoNames = append(team.RepoNames, fullName)
	}
	if perm != "" {
		if team.RepoPermissions == nil {
			team.RepoPermissions = map[string]TeamPermission{}
		}
		team.RepoPermissions[fullName] = perm
	}
	team.UpdatedAt = st.CurrentTime()
	if st.Persist != nil {
		st.Persist.MustPut("teams", strconv.Itoa(team.ID), team)
	}
	return true
}

// roleOf returns the user's role in the team, and whether they're a member.
func (t *Team) RoleOf(userID int) (TeamRole, bool) {
	if !slices.Contains(t.MemberIDs, userID) {
		return "", false
	}
	if slices.Contains(t.MaintainerIDs, userID) {
		return TeamRoleMaintainer, true
	}
	return TeamRoleMember, true
}

func intSliceRemove(s []int, v int) []int {
	if i := slices.Index(s, v); i >= 0 {
		return slices.Delete(s, i, i+1)
	}
	return s
}

// AddTeamRepo adds a repository to a team's access list.
func (st *Store) AddTeamRepo(orgLogin, slug, repoFullName string) bool {
	st.Mu.Lock()
	defer st.Mu.Unlock()

	team := st.TeamsBySlug[TeamSlugKey(st.canonicalOrgLoginLocked(orgLogin), slug)]
	if team == nil {
		return false
	}
	// Store the canonical repo key: the permission lattice (rbac.go) compares
	// team.RepoNames against repo.FullName.
	if repo := st.RepoByNameLocked(repoFullName); repo != nil {
		repoFullName = repo.FullName
	}

	for _, rn := range team.RepoNames {
		if rn == repoFullName {
			return true // already added
		}
	}

	team.RepoNames = append(team.RepoNames, repoFullName)
	if team.RepoPermissions == nil {
		team.RepoPermissions = map[string]TeamPermission{}
	}
	team.UpdatedAt = st.CurrentTime()
	if st.Persist != nil {
		st.Persist.MustPut("teams", strconv.Itoa(team.ID), team)
	}
	return true
}

// RemoveTeamRepo removes a repository from a team's access list.
func (st *Store) RemoveTeamRepo(orgLogin, slug, repoFullName string) bool {
	st.Mu.Lock()
	defer st.Mu.Unlock()

	team := st.TeamsBySlug[TeamSlugKey(st.canonicalOrgLoginLocked(orgLogin), slug)]
	if team == nil {
		return false
	}
	if repo := st.RepoByNameLocked(repoFullName); repo != nil {
		repoFullName = repo.FullName
	}

	for i, rn := range team.RepoNames {
		if rn == repoFullName {
			team.RepoNames = append(team.RepoNames[:i], team.RepoNames[i+1:]...)
			if team.RepoPermissions != nil {
				delete(team.RepoPermissions, repoFullName)
			}
			team.UpdatedAt = st.CurrentTime()
			if st.Persist != nil {
				st.Persist.MustPut("teams", strconv.Itoa(team.ID), team)
			}
			return true
		}
	}
	return false
}

// CreateOrgRepo creates a repository owned by an organization.
func (st *Store) CreateOrgRepo(org *Org, creator *User, name, description string, private bool) *Repo {
	return st.createRepo(org.Login+"/"+name, name, description, private, org.ID, "Organization", nil)
}
