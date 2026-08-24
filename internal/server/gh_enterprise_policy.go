package bleephub

// Enterprise policy enforcement.
//
// An enterprise policy is a ceiling the enterprise imposes on every
// organization it owns. NO_POLICY means the enterprise imposes nothing and
// the organization's own setting decides; ENABLED and DISABLED override the
// organization. Enterprise owners are exempt — GitHub lets the people who set
// a policy act despite it — and everyone else, organization owners included,
// is bound by it.
//
// Each predicate here is consumed at the place the governed action happens,
// so a policy that says "members cannot delete repositories" is a refusal
// from the repository-deletion path rather than a value in a settings
// response.

import (
	"context"
	"net"
	"net/http"
	"strings"

	"github.com/e6qu/bleephub/internal/store"
)

// enterprisePolicyForOrg and enterprisePolicyForRepo resolve the enterprise
// policy governing an organization or a repository. The resolution lives in
// the store so the REST handlers and the GraphQL resolvers read one answer.
func (s *Server) enterprisePolicyForOrg(org *store.Org) (store.EnterprisePolicy, *store.Enterprise) {
	if org == nil {
		return store.EnterprisePolicy{}, nil
	}
	return s.store.EnterprisePolicyForOrg(org.ID)
}

func (s *Server) enterprisePolicyForRepo(repo *store.Repo) (store.EnterprisePolicy, *store.Enterprise) {
	return s.store.EnterprisePolicyForRepo(repo)
}

// policyForbids reports whether a DISABLED enterprise policy blocks the
// request principal. A blank setting or NO_POLICY imposes nothing; ENABLED
// permits; DISABLED blocks everyone but an enterprise owner.
func (s *Server) policyForbids(ctx context.Context, e *store.Enterprise, setting string) bool {
	return s.store.EnterprisePolicyForbids(e, setting, ghUserFromContext(ctx))
}

// refuseByEnterprisePolicy writes GitHub's 403 for an action an enterprise
// policy forbids and reports whether it did.
func (s *Server) refuseByEnterprisePolicy(w http.ResponseWriter, r *http.Request, e *store.Enterprise, setting, message string) bool {
	if !s.policyForbids(r.Context(), e, setting) {
		return false
	}
	writeGHError(w, http.StatusForbidden, message)
	return true
}

// --- repository creation ---------------------------------------------------

// enterpriseForbidsRepositoryCreation reports the refusal message when the
// enterprise's members-can-create-repositories policy forbids creating a
// repository of the given visibility in org, or "" when it permits.
//
// GitHub's setting is a ceiling with a per-visibility refinement: DISABLED
// forbids all creation, ALL/PUBLIC/PRIVATE name the broadest visibility
// permitted, and the three booleans narrow it further when they are set.
func (s *Server) enterpriseForbidsRepositoryCreation(ctx context.Context, org *store.Org, visibility string) string {
	policy, enterprise := s.enterprisePolicyForOrg(org)
	if enterpriseOwnerRole(s.viewerEnterpriseRole(ctx, enterprise)) {
		return ""
	}
	const refusal = "Repository creation is disabled by an enterprise policy."
	switch policy.MembersCanCreateRepositories {
	case store.EnterprisePolicyDisabled:
		return refusal
	case "", store.EnterprisePolicyNoPolicy:
		return ""
	case "PUBLIC":
		if visibility != "public" {
			return refusal
		}
	case "PRIVATE":
		if visibility == "public" {
			return refusal
		}
	}
	switch visibility {
	case "public":
		if policy.MembersCanCreatePublicRepositories != nil && !*policy.MembersCanCreatePublicRepositories {
			return refusal
		}
	case "private":
		if policy.MembersCanCreatePrivateRepositories != nil && !*policy.MembersCanCreatePrivateRepositories {
			return refusal
		}
	case "internal":
		if policy.MembersCanCreateInternalRepositories != nil && !*policy.MembersCanCreateInternalRepositories {
			return refusal
		}
	}
	return ""
}

// --- marketplace purchases -------------------------------------------------

// enterprisePolicyForAccount resolves the policy governing a buying account:
// an organization is governed by the enterprise that owns it, and a personal
// account by the instance's own enterprise, which owns every account on the
// appliance the way a GHES enterprise does.
func (s *Server) enterprisePolicyForAccount(accountType string, accountID int) (store.EnterprisePolicy, *store.Enterprise) {
	if accountType == "Organization" {
		return s.store.EnterprisePolicyForOrg(accountID)
	}
	e := s.primaryEnterprise()
	if e == nil {
		return store.EnterprisePolicy{}, nil
	}
	return e.Policy, e
}

// refuseMarketplacePurchaseByPolicy writes GitHub's 403 when the enterprise
// governing the buying account has turned Marketplace purchasing off, and
// reports whether it did. It covers both write paths that spend money: the
// initial purchase and a later plan change.
func (s *Server) refuseMarketplacePurchaseByPolicy(w http.ResponseWriter, r *http.Request, account store.MarketplaceBuyerAccount) bool {
	policy, enterprise := s.enterprisePolicyForAccount(account.AccountType, account.Id)
	return s.refuseByEnterprisePolicy(w, r, enterprise, policy.MembersCanMakePurchases,
		"Marketplace purchases are disabled by an enterprise policy.")
}

// --- two-factor methods ----------------------------------------------------

// enterpriseDisallowedTwoFactorMethods reports the second-factor methods the
// enterprise governing an account refuses. GitHub's setting is a single enum:
// INSECURE bans the methods classed insecure, NO_POLICY bans none. The
// catalogue of methods, and which of them are insecure, is the store's —
// nothing here names a factor this instance cannot verify.
func (s *Server) enterpriseDisallowedTwoFactorMethods(user *store.User) []store.TwoFactorMethod {
	e := s.primaryEnterprise()
	if e == nil || e.Policy.TwoFactorDisallowedMethods != store.EnterprisePolicyDisallowInsecure {
		return nil
	}
	if enterpriseOwnerRole(s.enterpriseRoleOfUser(e, user)) {
		return nil
	}
	return store.InsecureTwoFactorMethods()
}

// --- base permission ceiling ----------------------------------------------

// repoPermissionRank orders GitHub's base repository permissions so a
// ceiling can be compared against a proposed value.
func repoPermissionRank(permission string) int {
	switch strings.ToLower(permission) {
	case "admin":
		return 3
	case "write", "push":
		return 2
	case "read", "pull":
		return 1
	default:
		return 0
	}
}

// EnterpriseDefaultRepositoryPermissionCeiling reports the highest base
// repository permission an organization in the enterprise may grant, and
// whether the enterprise imposes a ceiling at all.
func enterpriseDefaultRepositoryPermissionCeiling(policy store.EnterprisePolicy) (string, bool) {
	switch policy.DefaultRepositoryPermission {
	case "", store.EnterprisePolicyNoPolicy:
		return "", false
	case "NONE":
		return "none", true
	case "READ":
		return "read", true
	case "WRITE":
		return "write", true
	case "ADMIN":
		return "admin", true
	}
	return "", false
}

// enterpriseClampsBasePermission reports the refusal message when an
// organization's proposed base repository permission exceeds the enterprise
// ceiling, or "" when it is within it.
func (s *Server) enterpriseClampsBasePermission(ctx context.Context, org *store.Org, proposed string) string {
	policy, enterprise := s.enterprisePolicyForOrg(org)
	if enterpriseOwnerRole(s.viewerEnterpriseRole(ctx, enterprise)) {
		return ""
	}
	ceiling, imposed := enterpriseDefaultRepositoryPermissionCeiling(policy)
	if !imposed {
		return ""
	}
	if repoPermissionRank(proposed) > repoPermissionRank(ceiling) {
		return "The base permission for repositories in this organization is capped at " + ceiling + " by an enterprise policy."
	}
	return ""
}

// --- two-factor requirement ------------------------------------------------

// enterpriseRequiresTwoFactor reports whether the enterprise governing org
// requires two-factor authentication of its members.
func (s *Server) enterpriseRequiresTwoFactor(org *store.Org) bool {
	policy, _ := s.enterprisePolicyForOrg(org)
	return policy.TwoFactorRequired == store.EnterprisePolicyEnabled
}

// --- private repository forking -------------------------------------------

// enterpriseForbidsPrivateForking reports the refusal message when the
// enterprise policy forbids forking a private or internal repository into the
// requested destination, or "" when it permits.
//
// The setting is the gate and the policy value narrows where a permitted fork
// may land: SAME_ORGANIZATION keeps the fork inside the source organization,
// ENTERPRISE_ORGANIZATIONS keeps it inside the enterprise, USER_ACCOUNTS
// permits only personal namespaces, and the *_USER_ACCOUNTS variants admit
// both. EVERYWHERE imposes nothing beyond the gate.
func (s *Server) enterpriseForbidsPrivateForking(ctx context.Context, source *store.Repo, destOrg *store.Org) string {
	if source == nil || !source.Private {
		return ""
	}
	policy, enterprise := s.enterprisePolicyForRepo(source)
	if enterpriseOwnerRole(s.viewerEnterpriseRole(ctx, enterprise)) {
		return ""
	}
	const refusal = "Forking private repositories is disabled by an enterprise policy."
	if policy.AllowPrivateRepositoryForking != store.EnterprisePolicyEnabled {
		if policy.AllowPrivateRepositoryForking == store.EnterprisePolicyDisabled {
			return refusal
		}
		return ""
	}
	toUserAccount := destOrg == nil
	switch policy.AllowPrivateRepositoryForkingPolicyValue {
	case "", "EVERYWHERE":
		return ""
	case "USER_ACCOUNTS":
		if !toUserAccount {
			return refusal
		}
	case "SAME_ORGANIZATION":
		if toUserAccount || source.OwnerType != "Organization" || destOrg.ID != source.OwnerID {
			return refusal
		}
	case "SAME_ORGANIZATION_USER_ACCOUNTS":
		if !toUserAccount && (source.OwnerType != "Organization" || destOrg.ID != source.OwnerID) {
			return refusal
		}
	case "ENTERPRISE_ORGANIZATIONS":
		if toUserAccount || enterprise == nil || s.store.EnterpriseIDForOrg(destOrg.ID) != enterprise.ID {
			return refusal
		}
	case "ENTERPRISE_ORGANIZATIONS_USER_ACCOUNTS":
		if !toUserAccount && (enterprise == nil || s.store.EnterpriseIDForOrg(destOrg.ID) != enterprise.ID) {
			return refusal
		}
	}
	return ""
}

// --- IP allow list ---------------------------------------------------------

// enterpriseIPAllowListRejects reports whether the enterprise's IP allow list
// is enabled and the request's source address matches none of its active
// entries. An enabled allow list with no active entries admits everything —
// GitHub does not lock an enterprise out of its own instance for enabling the
// feature before adding a range to it.
func (s *Server) enterpriseIPAllowListRejects(r *http.Request) bool {
	values, forInstalledApps := s.store.ActiveEnterpriseIPAllowList()
	if len(values) == 0 {
		return false
	}
	// A GitHub App's installation acts from the app's own infrastructure, not
	// from the enterprise's network, so the allow list applies to installation
	// tokens only when the enterprise says it should.
	if !forInstalledApps && ghInstallationTokenFromContext(r.Context()) != nil {
		return false
	}
	return !ipAllowListAdmits(values, requestIPAddress(r.RemoteAddr))
}

// userIPAllowListRejects reports whether the account's own IP allow list is
// enforced (the enterprise turned user-level enforcement on) and the request's
// source address matches none of its active entries.
//
// It is asked of the principal the request authenticated as, so a token acting
// for a user is bound by that user's list exactly as their browser is: a list
// only the browser obeyed would not be an allow list either. An account with no
// active entry admits everything, so turning enforcement on for the enterprise
// does not lock out the accounts that never configured one.
func (s *Server) userIPAllowListRejects(r *http.Request) bool {
	user := ghUserFromContext(r.Context())
	if user == nil {
		return false
	}
	values := s.store.ActiveUserIPAllowList(user.ID)
	if len(values) == 0 {
		return false
	}
	return !ipAllowListAdmits(values, requestIPAddress(r.RemoteAddr))
}

// ipAllowListAdmits reports whether any of the allow-list values covers ip. An
// address that could not be parsed is admitted by nothing.
func ipAllowListAdmits(values []string, ip net.IP) bool {
	if ip == nil {
		return false
	}
	for _, value := range values {
		if ipAllowListValueMatches(value, ip) {
			return true
		}
	}
	return false
}

// requestIPAddress extracts the IP from a "host:port" remote address, or from
// a bare address.
func requestIPAddress(remoteAddr string) net.IP {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		host = remoteAddr
	}
	return net.ParseIP(strings.Trim(host, "[]"))
}

// ipAllowListValueMatches reports whether an allow-list value — a single
// address or a CIDR range, which is what GitHub accepts — covers ip.
func ipAllowListValueMatches(value string, ip net.IP) bool {
	value = strings.TrimSpace(value)
	if value == "" {
		return false
	}
	if strings.Contains(value, "/") {
		_, network, err := net.ParseCIDR(value)
		if err != nil {
			return false
		}
		return network.Contains(ip)
	}
	parsed := net.ParseIP(value)
	return parsed != nil && parsed.Equal(ip)
}

// ValidIPAllowListValue reports whether a value is an address or CIDR range
// GitHub would accept for an allow-list entry.
func ValidIPAllowListValue(value string) bool {
	value = strings.TrimSpace(value)
	if value == "" {
		return false
	}
	if strings.Contains(value, "/") {
		_, _, err := net.ParseCIDR(value)
		return err == nil
	}
	return net.ParseIP(value) != nil
}

// enforceEnterpriseIPAllowList refuses an API request from an address the
// enterprise's IP allow list does not admit. It wraps the API surface rather
// than individual handlers because an allow list that governed only some
// endpoints would not be an allow list.
//
// The gate is inert unless the enterprise turned the allow list on and has at
// least one active entry, so the default instance is unaffected.
func (s *Server) enforceEnterpriseIPAllowList(pattern string, next http.HandlerFunc) http.HandlerFunc {
	if !strings.Contains(pattern, " /api/") {
		return next
	}
	return func(w http.ResponseWriter, r *http.Request) {
		if s.enterpriseIPAllowListRejects(r) {
			// The refused address is deliberately not echoed: it is request
			// input, and reflecting it would put an attacker-controlled string
			// in the response body for no diagnostic gain the caller does not
			// already have.
			writeGHError(w, http.StatusForbidden,
				"Although you appear to have the correct authorization credentials, "+
					"the enterprise has an IP allow list enabled, and your source address "+
					"is not permitted to access this resource.")
			return
		}
		if s.userIPAllowListRejects(r) {
			writeGHError(w, http.StatusForbidden,
				"Although you appear to have the correct authorization credentials, "+
					"your account has an IP allow list enabled, and your source address "+
					"is not permitted to access this resource.")
			return
		}
		next(w, r)
	}
}
