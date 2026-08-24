package graphqlapi

// The GraphQL organization audit-log surface: Organization.auditLog's return
// type graph, served from the real audit store (store.AuditEntry rows written
// by RecordAuditEntry through the REST handlers and the enterprise mutations).
//
// GitHub models the audit log as a union, OrganizationAuditEntry, of ~60
// concrete *AuditEntry objects, each implementing the AuditEntry interface
// (the fifteen shared actor/created-at members), Node, and the
// OrganizationAuditEntryData mixin (organization*), with the repo-scoped
// members additionally implementing RepositoryAuditEntryData. bleephub only
// records a handful of the actions those types describe, so only the concrete
// types whose `action` the store actually produces are built here; an audit
// entry whose action has no modeled type is omitted from the connection rather
// than rendered as the wrong type. The modeled set:
//
//	org.create                       -> OrgCreateAuditEntry
//	org.block_user                   -> OrgBlockUserAuditEntry
//	org.unblock_user                 -> OrgUnblockUserAuditEntry
//	org.remove_outside_collaborator  -> OrgRemoveOutsideCollaboratorAuditEntry
//	repo.create                      -> RepoCreateAuditEntry
//	repo.destroy                     -> RepoDestroyAuditEntry
//
// Every field on every type here is a transcription of GitHub's SDL — no field
// is invented, so the schema stays signature-exact against the union members
// it emits.

import (
	"strconv"
	"strings"

	"github.com/graphql-go/graphql"

	"github.com/e6qu/bleephub/internal/store"
)

// auditEntryTypeName maps a stored action string to the concrete
// OrganizationAuditEntry member that models it, or "" when the action has no
// modeled type (and is therefore omitted from the connection).
func auditEntryTypeName(action string) string {
	switch action {
	case "org.create":
		return "OrgCreateAuditEntry"
	case "org.block_user":
		return "OrgBlockUserAuditEntry"
	case "org.unblock_user":
		return "OrgUnblockUserAuditEntry"
	case "org.remove_outside_collaborator":
		return "OrgRemoveOutsideCollaboratorAuditEntry"
	case "repo.create":
		return "RepoCreateAuditEntry"
	case "repo.destroy":
		return "RepoDestroyAuditEntry"
	}
	return ""
}

// --- shared type/field construction ----------------------------------------

// auditObject memoizes an object type that declares interfaces. mutationObject
// cannot be used because it constructs objects without an Interfaces list;
// the shared mutationObjects memo is reused so the audit types are minted once
// like every other object in the package.
func (s *Resolver) auditObject(name string, ifaces []*graphql.Interface, fields graphql.Fields) *graphql.Object {
	if existing := s.memoizedMutationObject(name); existing != nil {
		return existing
	}
	object := graphql.NewObject(graphql.ObjectConfig{Name: name, Fields: fields, Interfaces: ifaces})
	s.mutationObjects[name] = object
	return object
}

func mergeAuditFields(dst graphql.Fields, srcs ...graphql.Fields) graphql.Fields {
	for _, src := range srcs {
		for k, v := range src {
			dst[k] = v
		}
	}
	return dst
}

// auditEntryBaseFields is the AuditEntry interface's fifteen shared members.
func (s *Resolver) auditEntryBaseFields() graphql.Fields {
	uri := s.graphQLStringScalar("URI")
	precise := s.graphQLStringScalar("PreciseDateTime")
	return graphql.Fields{
		"action":            gqlNonNull(graphql.String),
		"actor":             gqlField(s.gqlAuditEntryActorUnion()),
		"actorIp":           gqlField(graphql.String),
		"actorLocation":     gqlField(s.gqlActorLocationType()),
		"actorLogin":        gqlField(graphql.String),
		"actorResourcePath": gqlField(uri),
		"actorUrl":          gqlField(uri),
		"createdAt":         gqlNonNull(precise),
		"operationType":     gqlField(s.gqlOperationTypeEnum()),
		"user":              gqlField(s.graphqlTypes.user),
		"userLogin":         gqlField(graphql.String),
		"userResourcePath":  gqlField(uri),
		"userUrl":           gqlField(uri),
	}
}

// orgAuditDataFields is the OrganizationAuditEntryData interface's members.
func (s *Resolver) orgAuditDataFields() graphql.Fields {
	uri := s.graphQLStringScalar("URI")
	return graphql.Fields{
		"organization":             gqlField(s.graphqlTypes.organization),
		"organizationName":         gqlField(graphql.String),
		"organizationResourcePath": gqlField(uri),
		"organizationUrl":          gqlField(uri),
	}
}

// repoAuditDataFields is the RepositoryAuditEntryData interface's members.
func (s *Resolver) repoAuditDataFields() graphql.Fields {
	uri := s.graphQLStringScalar("URI")
	return graphql.Fields{
		"repository":             gqlField(s.graphqlTypes.repository),
		"repositoryName":         gqlField(graphql.String),
		"repositoryResourcePath": gqlField(uri),
		"repositoryUrl":          gqlField(uri),
	}
}

func (s *Resolver) gqlActorLocationType() *graphql.Object {
	return s.mutationObject("ActorLocation", graphql.Fields{
		"city":        gqlField(graphql.String),
		"country":     gqlField(graphql.String),
		"countryCode": gqlField(graphql.String),
		"region":      gqlField(graphql.String),
		"regionCode":  gqlField(graphql.String),
	})
}

func (s *Resolver) gqlOperationTypeEnum() *graphql.Enum {
	return s.sharedEnum("OperationType",
		"ACCESS", "AUTHENTICATION", "CREATE", "MODIFY", "REMOVE", "RESTORE", "TRANSFER")
}

// gqlBotType returns the shared Bot object the pull-request review-request
// union already mints. The AuditEntryActor union names it, and graphql-go
// refuses two objects of one name, so it is reused rather than re-created.
func (s *Resolver) gqlBotType() *graphql.Object {
	if union := s.graphqlTypes.requestedReviewerUnion; union != nil {
		for _, t := range union.Types() {
			if t.Name() == "Bot" {
				return t
			}
		}
	}
	return nil
}

// gqlAuditEntryActorUnion is AuditEntryActor = Bot | Organization | User.
func (s *Resolver) gqlAuditEntryActorUnion() *graphql.Union {
	return s.mutationUnion("AuditEntryActor", func() []*graphql.Object {
		members := []*graphql.Object{s.graphqlTypes.user, s.graphqlTypes.organization}
		if bot := s.gqlBotType(); bot != nil {
			members = append(members, bot)
		}
		return members
	}, func(p graphql.ResolveTypeParams) *graphql.Object {
		if m, ok := p.Value.(map[string]interface{}); ok {
			switch m["_actorKind"] {
			case "Organization":
				return s.graphqlTypes.organization
			case "Bot":
				return s.gqlBotType()
			}
		}
		return s.graphqlTypes.user
	})
}

// resolveAuditEntryConcrete dispatches a rendered audit-entry source to its
// concrete object. It serves the union and all three interfaces the concrete
// types implement.
func (s *Resolver) resolveAuditEntryConcrete(p graphql.ResolveTypeParams) *graphql.Object {
	m, ok := p.Value.(map[string]interface{})
	if !ok {
		return nil
	}
	switch m["_auditType"] {
	case "OrgCreateAuditEntry":
		return s.gqlOrgCreateAuditEntryType()
	case "OrgBlockUserAuditEntry":
		return s.gqlOrgBlockUserAuditEntryType()
	case "OrgUnblockUserAuditEntry":
		return s.gqlOrgUnblockUserAuditEntryType()
	case "OrgRemoveOutsideCollaboratorAuditEntry":
		return s.gqlOrgRemoveOutsideCollaboratorAuditEntryType()
	case "RepoCreateAuditEntry":
		return s.gqlRepoCreateAuditEntryType()
	case "RepoDestroyAuditEntry":
		return s.gqlRepoDestroyAuditEntryType()
	}
	return nil
}

func (s *Resolver) gqlAuditEntryInterface() *graphql.Interface {
	return s.mutationInterface("AuditEntry",
		func() graphql.Fields { return s.auditEntryBaseFields() },
		s.resolveAuditEntryConcrete)
}

func (s *Resolver) gqlOrgAuditDataInterface() *graphql.Interface {
	return s.mutationInterface("OrganizationAuditEntryData",
		func() graphql.Fields { return s.orgAuditDataFields() },
		s.resolveAuditEntryConcrete)
}

func (s *Resolver) gqlRepoAuditDataInterface() *graphql.Interface {
	return s.mutationInterface("RepositoryAuditEntryData",
		func() graphql.Fields { return s.repoAuditDataFields() },
		s.resolveAuditEntryConcrete)
}

// orgAuditInterfaces / repoAuditInterfaces are the interface sets each concrete
// type declares.
func (s *Resolver) orgAuditInterfaces() []*graphql.Interface {
	return []*graphql.Interface{s.gqlAuditEntryInterface(), s.graphqlTypes.node, s.gqlOrgAuditDataInterface()}
}

func (s *Resolver) repoAuditInterfaces() []*graphql.Interface {
	return []*graphql.Interface{
		s.gqlAuditEntryInterface(), s.graphqlTypes.node,
		s.gqlOrgAuditDataInterface(), s.gqlRepoAuditDataInterface(),
	}
}

// --- concrete member types --------------------------------------------------

func (s *Resolver) gqlOrgCreateAuditEntryType() *graphql.Object {
	return s.auditObject("OrgCreateAuditEntry", s.orgAuditInterfaces(),
		mergeAuditFields(graphql.Fields{
			"id": gqlNonNull(graphql.ID),
			"billingPlan": gqlField(s.sharedEnum("OrgCreateAuditEntryBillingPlan",
				"BUSINESS", "BUSINESS_PLUS", "FREE", "TIERED_PER_SEAT", "UNLIMITED")),
		}, s.auditEntryBaseFields(), s.orgAuditDataFields()))
}

func (s *Resolver) gqlOrgBlockUserAuditEntryType() *graphql.Object {
	return s.auditObject("OrgBlockUserAuditEntry", s.orgAuditInterfaces(),
		mergeAuditFields(s.blockedUserFields(), s.auditEntryBaseFields(), s.orgAuditDataFields()))
}

func (s *Resolver) gqlOrgUnblockUserAuditEntryType() *graphql.Object {
	return s.auditObject("OrgUnblockUserAuditEntry", s.orgAuditInterfaces(),
		mergeAuditFields(s.blockedUserFields(), s.auditEntryBaseFields(), s.orgAuditDataFields()))
}

// blockedUserFields are the members OrgBlockUserAuditEntry and
// OrgUnblockUserAuditEntry declare beyond the shared set.
func (s *Resolver) blockedUserFields() graphql.Fields {
	uri := s.graphQLStringScalar("URI")
	return graphql.Fields{
		"id":                      gqlNonNull(graphql.ID),
		"blockedUser":             gqlField(s.graphqlTypes.user),
		"blockedUserName":         gqlField(graphql.String),
		"blockedUserResourcePath": gqlField(uri),
		"blockedUserUrl":          gqlField(uri),
	}
}

func (s *Resolver) gqlOrgRemoveOutsideCollaboratorAuditEntryType() *graphql.Object {
	membershipType := s.sharedEnum("OrgRemoveOutsideCollaboratorAuditEntryMembershipType",
		"BILLING_MANAGER", "OUTSIDE_COLLABORATOR", "UNAFFILIATED")
	reason := s.sharedEnum("OrgRemoveOutsideCollaboratorAuditEntryReason",
		"SAML_EXTERNAL_IDENTITY_MISSING", "TWO_FACTOR_REQUIREMENT_NON_COMPLIANCE")
	return s.auditObject("OrgRemoveOutsideCollaboratorAuditEntry", s.orgAuditInterfaces(),
		mergeAuditFields(graphql.Fields{
			"id":              gqlNonNull(graphql.ID),
			"membershipTypes": gqlFieldListOf(membershipType),
			"reason":          gqlField(reason),
		}, s.auditEntryBaseFields(), s.orgAuditDataFields()))
}

func (s *Resolver) gqlRepoCreateAuditEntryType() *graphql.Object {
	visibility := s.sharedEnum("RepoCreateAuditEntryVisibility", "INTERNAL", "PRIVATE", "PUBLIC")
	return s.auditObject("RepoCreateAuditEntry", s.repoAuditInterfaces(),
		mergeAuditFields(graphql.Fields{
			"id":             gqlNonNull(graphql.ID),
			"forkParentName": gqlField(graphql.String),
			"forkSourceName": gqlField(graphql.String),
			"visibility":     gqlField(visibility),
		}, s.auditEntryBaseFields(), s.orgAuditDataFields(), s.repoAuditDataFields()))
}

func (s *Resolver) gqlRepoDestroyAuditEntryType() *graphql.Object {
	visibility := s.sharedEnum("RepoDestroyAuditEntryVisibility", "INTERNAL", "PRIVATE", "PUBLIC")
	return s.auditObject("RepoDestroyAuditEntry", s.repoAuditInterfaces(),
		mergeAuditFields(graphql.Fields{
			"id":         gqlNonNull(graphql.ID),
			"visibility": gqlField(visibility),
		}, s.auditEntryBaseFields(), s.orgAuditDataFields(), s.repoAuditDataFields()))
}

// --- union, edge, connection -----------------------------------------------

func (s *Resolver) gqlOrganizationAuditEntryUnion() *graphql.Union {
	return s.mutationUnion("OrganizationAuditEntry", func() []*graphql.Object {
		return []*graphql.Object{
			s.gqlOrgCreateAuditEntryType(),
			s.gqlOrgBlockUserAuditEntryType(),
			s.gqlOrgUnblockUserAuditEntryType(),
			s.gqlOrgRemoveOutsideCollaboratorAuditEntryType(),
			s.gqlRepoCreateAuditEntryType(),
			s.gqlRepoDestroyAuditEntryType(),
		}
	}, s.resolveAuditEntryConcrete)
}

func (s *Resolver) gqlOrganizationAuditEntryEdgeType() *graphql.Object {
	return s.mutationObject("OrganizationAuditEntryEdge", graphql.Fields{
		"cursor": gqlNonNull(graphql.String),
		"node":   gqlField(s.gqlOrganizationAuditEntryUnion()),
	})
}

// gqlOrganizationAuditEntryConnectionType is the OrganizationAuditEntryConnection
// object the parent wires Organization.auditLog to return.
func (s *Resolver) gqlOrganizationAuditEntryConnectionType() *graphql.Object {
	return s.mutationObject("OrganizationAuditEntryConnection", graphql.Fields{
		"edges":      &graphql.Field{Type: graphql.NewList(s.gqlOrganizationAuditEntryEdgeType())},
		"nodes":      &graphql.Field{Type: graphql.NewList(s.gqlOrganizationAuditEntryUnion())},
		"pageInfo":   gqlNonNull(s.gqlPageInfoType()),
		"totalCount": gqlNonNull(graphql.Int),
	})
}

// --- connection resolver ----------------------------------------------------

// organizationAuditLogConnection reads the org's stored audit events and
// returns a paginated OrganizationAuditEntryConnection source. Organization.
// auditLog is owner-only on GitHub, so a viewer who does not administer the
// organization receives an empty connection (never an error, so a broader
// query is not aborted). Entries whose action has no modeled concrete type are
// omitted. The parent hands this method the org and its own p.Args (first/last/
// after/before/orderBy/query).
func (s *Resolver) organizationAuditLogConnection(p graphql.ResolveParams, org *store.Org) (interface{}, error) {
	if org == nil || !s.viewerCanAdminAccount(p.Context, org.Login) {
		return paginateGQLItems(nil, nil), nil
	}

	entries := s.store.ListOrgAuditEntries(org.Login)

	// The store keeps the log newest-first; CREATED_AT DESC (GitHub's default)
	// keeps that order, CREATED_AT ASC reverses it.
	if orderBy, ok := p.Args["orderBy"].(map[string]interface{}); ok {
		if dir, _ := orderBy["direction"].(string); dir == "ASC" {
			for i, j := 0, len(entries)-1; i < j; i, j = i+1, j-1 {
				entries[i], entries[j] = entries[j], entries[i]
			}
		}
	}

	phrase, _ := p.Args["query"].(string)

	items := make([]gqlConnItem, 0, len(entries))
	for _, entry := range entries {
		typeName := auditEntryTypeName(entry.Action)
		if typeName == "" {
			continue
		}
		if phrase != "" && !auditEntryMatchesQuery(entry, phrase) {
			continue
		}
		e := entry
		tn := typeName
		items = append(items, gqlConnItem{
			identity: strconv.FormatInt(e.ID, 10),
			render:   func() map[string]interface{} { return s.renderAuditEntry(tn, e) },
		})
	}
	return paginateGQLItems(items, p.Args), nil
}

// auditEntryMatchesQuery is the connection's `query` filter: every whitespace
// term must appear in the entry's action, actor, org or data.
func auditEntryMatchesQuery(e *store.AuditEntry, phrase string) bool {
	text := strings.ToLower(strings.Join([]string{e.Action, e.Actor, e.Org}, " "))
	for k, v := range e.Data {
		text += " " + strings.ToLower(k)
		if sv, ok := v.(string); ok {
			text += " " + strings.ToLower(sv)
		}
	}
	for _, term := range strings.Fields(strings.ToLower(phrase)) {
		if !strings.Contains(text, term) {
			return false
		}
	}
	return true
}

// --- source rendering -------------------------------------------------------

func auditDataString(e *store.AuditEntry, key string) string {
	if e.Data == nil {
		return ""
	}
	s, _ := e.Data[key].(string)
	return s
}

func auditOperationType(action string) interface{} {
	switch {
	case strings.Contains(action, "create"):
		return "CREATE"
	case strings.Contains(action, "destroy"), strings.Contains(action, "delete"),
		strings.Contains(action, "remove"):
		return "REMOVE"
	case strings.Contains(action, "block"):
		return "MODIFY"
	}
	return nil
}

// auditEntryBaseSource renders the AuditEntry, Node and OrganizationAuditEntryData
// members shared by every modeled type. Optional object members are held as an
// untyped nil (absent key) rather than a typed-nil map, per the connection
// typed-nil contract.
func (s *Resolver) auditEntryBaseSource(typeName string, e *store.AuditEntry) map[string]interface{} {
	m := map[string]interface{}{
		"_auditType":    typeName,
		"action":        e.Action,
		"createdAt":     e.Timestamp,
		"id":            "AE_" + strconv.FormatInt(e.ID, 10),
		"operationType": auditOperationType(e.Action),
	}

	if e.Actor != "" {
		m["actorLogin"] = e.Actor
		m["actorResourcePath"] = "/" + e.Actor
		m["actorUrl"] = externalURL("/" + e.Actor)
		if u := s.store.LookupUserByLogin(e.Actor); u != nil {
			actor := userToGraphQL(u)
			actor["_actorKind"] = "User"
			m["actor"] = actor
		} else if o := s.store.GetOrg(e.Actor); o != nil {
			actor := orgToGraphQL(o)
			actor["_actorKind"] = "Organization"
			m["actor"] = actor
		}
	}

	// The organization the entry is attributed to: its own org, or — for the
	// repo-scoped entries the store records with no org — the owner of the
	// repository the data names.
	orgLogin := e.Org
	if orgLogin == "" {
		if full := auditDataString(e, "repo"); full != "" {
			if owner, _, ok := store.SplitRepoFullName(full); ok {
				orgLogin = owner
			}
		}
	}
	if orgLogin != "" {
		m["organizationName"] = orgLogin
		m["organizationResourcePath"] = "/" + orgLogin
		m["organizationUrl"] = externalURL("/" + orgLogin)
		if o := s.store.GetOrg(orgLogin); o != nil {
			m["organization"] = orgToGraphQL(o)
		}
	}
	return m
}

// setAuditUser fills the shared `user` (the user affected by the action) from a
// login, when one is stored.
func (s *Resolver) setAuditUser(m map[string]interface{}, login string) {
	if login == "" {
		return
	}
	m["userLogin"] = login
	m["userResourcePath"] = "/" + login
	m["userUrl"] = externalURL("/" + login)
	if u := s.store.LookupUserByLogin(login); u != nil {
		m["user"] = userToGraphQL(u)
	}
}

func (s *Resolver) renderAuditEntry(typeName string, e *store.AuditEntry) map[string]interface{} {
	m := s.auditEntryBaseSource(typeName, e)
	switch typeName {
	case "OrgCreateAuditEntry":
		switch auditDataString(e, "billing_plan") {
		case "BUSINESS", "BUSINESS_PLUS", "FREE", "TIERED_PER_SEAT", "UNLIMITED":
			m["billingPlan"] = e.Data["billing_plan"]
		}
	case "OrgBlockUserAuditEntry", "OrgUnblockUserAuditEntry":
		if blocked := auditDataString(e, "blocked_user"); blocked != "" {
			m["blockedUserName"] = blocked
			m["blockedUserResourcePath"] = "/" + blocked
			m["blockedUserUrl"] = externalURL("/" + blocked)
			if u := s.store.LookupUserByLogin(blocked); u != nil {
				m["blockedUser"] = userToGraphQL(u)
			}
		}
	case "OrgRemoveOutsideCollaboratorAuditEntry":
		s.setAuditUser(m, auditDataString(e, "user"))
	case "RepoCreateAuditEntry":
		s.fillRepositoryAuditData(m, e)
	case "RepoDestroyAuditEntry":
		s.fillRepositoryAuditData(m, e)
	}
	return m
}

// fillRepositoryAuditData renders the RepositoryAuditEntryData members and the
// repo visibility from the repository the entry's data names.
func (s *Resolver) fillRepositoryAuditData(m map[string]interface{}, e *store.AuditEntry) {
	full := auditDataString(e, "repo")
	if full == "" {
		return
	}
	m["repositoryName"] = full
	if _, name, ok := store.SplitRepoFullName(full); ok {
		m["repositoryName"] = name
	}
	m["repositoryResourcePath"] = "/" + full
	m["repositoryUrl"] = externalURL("/" + full)
	if repo := s.store.GetRepoByFullName(full); repo != nil {
		m["repository"] = repoToGraphQL(s.store, repo)
		switch strings.ToUpper(repo.Visibility) {
		case "INTERNAL", "PRIVATE", "PUBLIC":
			m["visibility"] = strings.ToUpper(repo.Visibility)
		}
	}
}
