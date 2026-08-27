package bleephub

// Enterprise policy enforcement. A policy is a ceiling over every org the
// enterprise owns: NO_POLICY defers to the org's own setting, ENABLED/DISABLED
// override it. Enterprise owners are exempt; everyone else, org owners included,
// is bound. Each predicate is consumed at the governed action's own path.

import (
	"context"
	"net"
	"net/http"
	"strings"

	"github.com/e6qu/bleephub/internal/store"
)

// enterprisePolicyForOrg and enterprisePolicyForRepo resolve the policy
// governing an org or repo. Resolution lives in the store so REST and GraphQL
// read one answer.
func (s *Server) enterprisePolicyForOrg(org *store.Org) (store.EnterprisePolicy, *store.Enterprise) {
	if org == nil {
		return store.EnterprisePolicy{}, nil
	}
	return s.store.EnterprisePolicyForOrg(org.ID)
}

func (s *Server) enterprisePolicyForRepo(repo *store.Repo) (store.EnterprisePolicy, *store.Enterprise) {
	return s.store.EnterprisePolicyForRepo(repo)
}

// policyForbids reports whether a DISABLED policy blocks the request principal.
// Blank/NO_POLICY impose nothing; ENABLED permits; DISABLED blocks all but an
// enterprise owner.
func (s *Server) policyForbids(ctx context.Context, e *store.Enterprise, setting string) bool {
	return s.store.EnterprisePolicyForbids(e, setting, ghUserFromContext(ctx))
}

// refuseByEnterprisePolicy writes the 403 for a policy-forbidden action and
// reports whether it did.
func (s *Server) refuseByEnterprisePolicy(w http.ResponseWriter, r *http.Request, e *store.Enterprise, setting, message string) bool {
	if !s.policyForbids(r.Context(), e, setting) {
		return false
	}
	writeGHError(w, http.StatusForbidden, message)
	return true
}

// enterpriseForbidsRepositoryCreation returns the refusal message when the
// members-can-create-repositories policy forbids creating a repo of visibility
// in org, or "" when it permits. The setting is a ceiling with a per-visibility
// refinement: DISABLED forbids all, ALL/PUBLIC/PRIVATE name the broadest
// permitted, and the three booleans narrow further.
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

// enterprisePolicyForAccount resolves the policy governing a buying account: an
// org by its owning enterprise, a personal account by the instance's own
// enterprise (which owns every account, as a GHES enterprise does).
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

// refuseMarketplacePurchaseByPolicy writes the 403 when the account's governing
// enterprise has turned Marketplace purchasing off, and reports whether it did.
// Covers both spending paths: initial purchase and plan change.
func (s *Server) refuseMarketplacePurchaseByPolicy(w http.ResponseWriter, r *http.Request, account store.MarketplaceBuyerAccount) bool {
	policy, enterprise := s.enterprisePolicyForAccount(account.AccountType, account.Id)
	return s.refuseByEnterprisePolicy(w, r, enterprise, policy.MembersCanMakePurchases,
		"Marketplace purchases are disabled by an enterprise policy.")
}

// enterpriseDisallowedTwoFactorMethods reports the second-factor methods the
// governing enterprise refuses. INSECURE bans the insecure-classed methods,
// NO_POLICY bans none; the catalogue lives in the store.
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

// repoPermissionRank orders base repo permissions so a ceiling can be compared
// against a proposed value.
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

// enterpriseDefaultRepositoryPermissionCeiling reports the highest base repo
// permission an org may grant, and whether a ceiling is imposed.
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

// enterpriseClampsBasePermission returns the refusal message when an org's
// proposed base permission exceeds the enterprise ceiling, or "" when within it.
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

// enterpriseRequiresTwoFactor reports whether org's governing enterprise
// requires 2FA of its members.
func (s *Server) enterpriseRequiresTwoFactor(org *store.Org) bool {
	policy, _ := s.enterprisePolicyForOrg(org)
	return policy.TwoFactorRequired == store.EnterprisePolicyEnabled
}

// enterpriseForbidsPrivateForking returns the refusal message when the policy
// forbids forking a private/internal repo into the destination, or "" when it
// permits. The setting is the gate; the policy value narrows where a fork may
// land: SAME_ORGANIZATION keeps it in the source org, ENTERPRISE_ORGANIZATIONS
// in the enterprise, USER_ACCOUNTS in personal namespaces, *_USER_ACCOUNTS
// admits both, EVERYWHERE imposes nothing beyond the gate.
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

// enterpriseIPAllowListRejects reports whether the enterprise IP allow list is
// enabled and the request's source matches none of its active entries. An
// enabled list with no entries admits everything.
func (s *Server) enterpriseIPAllowListRejects(r *http.Request) bool {
	values, forInstalledApps := s.store.ActiveEnterpriseIPAllowList()
	if len(values) == 0 {
		return false
	}
	// An installation acts from the app's infrastructure, so the list applies to
	// installation tokens only when the enterprise opts in.
	if !forInstalledApps && ghInstallationTokenFromContext(r.Context()) != nil {
		return false
	}
	return !ipAllowListAdmits(values, requestIPAddress(r.RemoteAddr))
}

// userIPAllowListRejects reports whether the authenticated account's own IP
// allow list is enforced and the request's source matches none of its active
// entries. A token acting for a user is bound by that user's list; an account
// with no entry admits everything.
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

// ipAllowListAdmits reports whether any value covers ip. An unparsable address
// is admitted by nothing.
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

// requestIPAddress extracts the IP from a "host:port" or bare remote address.
func requestIPAddress(remoteAddr string) net.IP {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		host = remoteAddr
	}
	return net.ParseIP(strings.Trim(host, "[]"))
}

// ipAllowListValueMatches reports whether a value (a single address or CIDR
// range) covers ip.
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
// acceptable as an allow-list entry.
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

// enforceEnterpriseIPAllowList refuses an API request from an address neither
// the enterprise nor the user allow list admits. It wraps the whole API surface
// and is inert unless a list is on with at least one active entry.
func (s *Server) enforceEnterpriseIPAllowList(pattern string, next http.HandlerFunc) http.HandlerFunc {
	if !strings.Contains(pattern, " /api/") {
		return next
	}
	return func(w http.ResponseWriter, r *http.Request) {
		if s.enterpriseIPAllowListRejects(r) {
			// Do not echo the refused address: it is attacker-controlled request
			// input and reflecting it gains nothing.
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
