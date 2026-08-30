package graphqlapi

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/e6qu/bleephub/internal/store"
	git "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	gitStorage "github.com/go-git/go-git/v5/storage"
	"github.com/graphql-go/graphql"
)

// addRepoGitResidualFields fills the residual members on the repos-family types
// (Repository, Commit, Ref, TreeEntry) and RepositoryConnection. Wired from
// addRepositoryPeopleFields, after every type it touches is registered.
func (s *Resolver) addRepoGitResidualFields(types *accountSurfaceTypes) {
	s.addRepositoryGitResidualFields(types)
	s.addCommitGitResidualFields(types)
	s.addRefGitResidualFields()
	s.addTreeEntryResidualFields()
	s.addRepositoryConnectionResidualFields()
}

// reach helpers

// namedUnionFromField unwraps a field's output type to the named union under it.
func namedUnionFromField(obj *graphql.Object, field string) *graphql.Union {
	if obj == nil {
		return nil
	}
	def, ok := obj.Fields()[field]
	if !ok || def == nil {
		return nil
	}
	u, _ := graphql.GetNamed(def.Type).(*graphql.Union)
	return u
}

// unionMemberObject returns the union member object with the given name.
func unionMemberObject(u *graphql.Union, name string) *graphql.Object {
	if u == nil {
		return nil
	}
	for _, t := range u.Types() {
		if t != nil && t.Name() == name {
			return t
		}
	}
	return nil
}

// residualSourceString reads a string member of a resolver source map.
func residualSourceString(source interface{}, key string) string {
	src, _ := source.(map[string]interface{})
	v, _ := src[key].(string)
	return v
}

// Repository

func (s *Resolver) addRepositoryGitResidualFields(types *accountSurfaceTypes) {
	repoType := types.repository
	if repoType == nil {
		return
	}

	s.addRepositoryDiscussionCategoryField(repoType)
	s.addRepositoryIssueTypeFields(types, repoType)
	s.addRepositoryIssueFieldsField(repoType)
	s.addRepositoryPolicyFields(repoType)
	s.addRepositoryMergeQueueField(repoType)
	// pinnedEnvironments is wired in the late pass: PinnedEnvironment is not
	// assembled until the deployments family runs.
	s.addRepositoryRecentProjectsField(repoType)
	s.addRepositoryCustomPropertyFields(repoType)
	s.addRepositorySuggestedActorsField(repoType)
	s.addRepositoryVulnerabilityAlertField(repoType)
}

// addRepositoryDiscussionCategoryField installs discussionCategory(slug), the
// single-category counterpart to discussionCategories.
func (s *Resolver) addRepositoryDiscussionCategoryField(repoType *graphql.Object) {
	categoryType := namedObjectFromField(namedObjectFromField(repoType, "discussionCategories"), "nodes")
	if categoryType == nil {
		return
	}
	repoType.AddFieldConfig("discussionCategory", &graphql.Field{
		Type: categoryType,
		Args: graphql.FieldConfigArgument{
			"slug": &graphql.ArgumentConfig{Type: graphql.NewNonNull(graphql.String)},
		},
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			repo, err := s.repoFromSource(p.Source)
			if err != nil {
				return nil, err
			}
			if !s.viewerCanReadRepo(p.Context, repo) {
				return nil, nil
			}
			slug, _ := p.Args["slug"].(string)
			for _, cat := range s.store.ListDiscussionCategories(repo.ID) {
				node := discussionCategoryToGQL(cat)
				if s, _ := node["slug"].(string); s == slug {
					return node, nil
				}
			}
			return nil, nil
		},
	})
}

// addRepositoryIssueTypeFields installs issueType(name), issueTypes and the
// organization issue-type catalogue a repository's owner defines.
func (s *Resolver) addRepositoryIssueTypeFields(types *accountSurfaceTypes, repoType *graphql.Object) {
	issueTypeObj := s.graphqlTypes.issueType
	if issueTypeObj == nil {
		return
	}
	repoType.AddFieldConfig("issueType", &graphql.Field{
		Type: issueTypeObj,
		Args: graphql.FieldConfigArgument{
			"name": &graphql.ArgumentConfig{Type: graphql.NewNonNull(graphql.String)},
		},
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			repo, err := s.repoFromSource(p.Source)
			if err != nil {
				return nil, err
			}
			owner, _, ok := store.SplitRepoFullName(repo.FullName)
			if !ok {
				return nil, nil
			}
			name, _ := p.Args["name"].(string)
			for _, it := range s.store.ListIssueTypes(owner) {
				if it.Name == name {
					return issueTypeToGQL(it), nil
				}
			}
			return nil, nil
		},
	})
	repoType.AddFieldConfig("issueTypes", &graphql.Field{
		Type: s.accountConnectionType(types, "IssueType", issueTypeObj, false, nil),
		Args: connectionArgs(graphql.FieldConfigArgument{
			"orderBy": &graphql.ArgumentConfig{Type: s.gqlOrderInput(types, "IssueTypeOrder", "IssueTypeOrderField", "CREATED_AT")},
		}),
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			repo, err := s.repoFromSource(p.Source)
			if err != nil {
				return nil, err
			}
			owner, _, ok := store.SplitRepoFullName(repo.FullName)
			if !ok {
				return paginateGQLItems(nil, p.Args), nil
			}
			issueTypes := s.store.ListIssueTypes(owner)
			items := make([]gqlConnItem, 0, len(issueTypes))
			for i := range issueTypes {
				it := issueTypes[i]
				items = append(items, gqlConnItem{
					identity: it.NodeID,
					render:   func() map[string]interface{} { return issueTypeToGQL(it) },
				})
			}
			return paginateGQLItems(items, p.Args), nil
		},
	})
}

// addRepositoryIssueFieldsField installs issueFields, the connection over the
// organization issue-field catalogue (the IssueFields union its owner defines).
func (s *Resolver) addRepositoryIssueFieldsField(repoType *graphql.Object) {
	fieldUnion := s.graphqlTypes.issueFieldsUnion
	if fieldUnion == nil {
		return
	}
	connectionType := s.gqlConnectionType("IssueFields", fieldUnion)
	repoType.AddFieldConfig("issueFields", &graphql.Field{
		Type: connectionType,
		Args: connectionArgs(graphql.FieldConfigArgument{
			"orderBy": &graphql.ArgumentConfig{Type: s.gqlOrderInput(s.accountSurfaceRegistry(), "IssueFieldOrder", "IssueFieldOrderField", "CREATED_AT")},
		}),
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			repo, err := s.repoFromSource(p.Source)
			if err != nil {
				return nil, err
			}
			owner, _, ok := store.SplitRepoFullName(repo.FullName)
			if !ok {
				return paginateGQLMaps(nil, p.Args), nil
			}
			fields := s.store.ListIssueFields(owner)
			nodes := make([]map[string]interface{}, 0, len(fields))
			for _, f := range fields {
				nodes = append(nodes, s.issueFieldToGQL(f))
			}
			return paginateGQLMaps(nodes, p.Args), nil
		},
	})
}

// addRepositoryPolicyFields installs the creation-policy, cap-config and
// plan-feature members, which are constants on a single unrestricted instance.
func (s *Resolver) addRepositoryPolicyFields(repoType *graphql.Object) {
	issuePolicy := s.sharedEnum("IssueCreationPolicy", "ALL", "COLLABORATORS_ONLY")
	prPolicy := s.sharedEnum("PullRequestCreationPolicy", "ALL", "COLLABORATORS_ONLY")

	repoType.AddFieldConfig("issueCreationPolicy", &graphql.Field{
		Type:    issuePolicy,
		Resolve: func(graphql.ResolveParams) (interface{}, error) { return "ALL", nil },
	})
	repoType.AddFieldConfig("pullRequestCreationPolicy", &graphql.Field{
		Type:    prPolicy,
		Resolve: func(graphql.ResolveParams) (interface{}, error) { return "ALL", nil },
	})

	// pullRequestCreationCapConfig is always null here, but the type is still
	// registered so the field's shape matches GitHub's.
	capConfigType := graphql.NewObject(graphql.ObjectConfig{
		Name: "PullRequestCreationCapConfig",
		Fields: graphql.Fields{
			"bypassedUsers": &graphql.Field{
				Type: graphql.NewNonNull(s.gqlUserConnectionType(s.graphqlTypes.user)),
				Args: relayConnectionArgs(),
				Resolve: func(p graphql.ResolveParams) (interface{}, error) {
					return paginateGQLMaps(nil, p.Args), nil
				},
			},
		},
	})
	repoType.AddFieldConfig("pullRequestCreationCapConfig", &graphql.Field{
		Type:    capConfigType,
		Resolve: func(graphql.ResolveParams) (interface{}, error) { return nil, nil },
	})

	// planFeatures reflect the one plan a self-hosted instance runs: toggles on,
	// ceilings at GitHub's documented per-repository maxima.
	planFeaturesType := graphql.NewObject(graphql.ObjectConfig{
		Name: "RepositoryPlanFeatures",
		Fields: graphql.Fields{
			"codeowners":                  &graphql.Field{Type: graphql.NewNonNull(graphql.Boolean), Resolve: constResolver(true)},
			"draftPullRequests":           &graphql.Field{Type: graphql.NewNonNull(graphql.Boolean), Resolve: constResolver(true)},
			"maximumAssignees":            &graphql.Field{Type: graphql.NewNonNull(graphql.Int), Resolve: constResolver(10)},
			"maximumManualReviewRequests": &graphql.Field{Type: graphql.NewNonNull(graphql.Int), Resolve: constResolver(15)},
			"teamReviewRequests":          &graphql.Field{Type: graphql.NewNonNull(graphql.Boolean), Resolve: constResolver(true)},
		},
	})
	repoType.AddFieldConfig("planFeatures", &graphql.Field{
		Type:    graphql.NewNonNull(planFeaturesType),
		Resolve: func(graphql.ResolveParams) (interface{}, error) { return map[string]interface{}{}, nil },
	})

	// tempCloneToken: bleephub mints none, so null.
	repoType.AddFieldConfig("tempCloneToken", &graphql.Field{
		Type:    graphql.String,
		Resolve: func(graphql.ResolveParams) (interface{}, error) { return nil, nil },
	})
}

// constResolver returns a resolver that always yields v.
func constResolver(v interface{}) graphql.FieldResolveFn {
	return func(graphql.ResolveParams) (interface{}, error) { return v, nil }
}

// addRepositoryMergeQueueField installs mergeQueue(branch), a base branch's merge
// queue.
func (s *Resolver) addRepositoryMergeQueueField(repoType *graphql.Object) {
	repoType.AddFieldConfig("mergeQueue", &graphql.Field{
		Type: s.gqlMergeQueueType(),
		Args: graphql.FieldConfigArgument{
			"branch": &graphql.ArgumentConfig{Type: graphql.String},
		},
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			repo, err := s.repoFromSource(p.Source)
			if err != nil {
				return nil, err
			}
			if !s.viewerCanReadRepo(p.Context, repo) {
				return nil, nil
			}
			branch, _ := p.Args["branch"].(string)
			if branch == "" {
				branch = repo.DefaultBranch
			}
			return optionalObject(s.mergeQueueToGQL(repo, branch)), nil
		},
	})
}

// addRepositoryPinnedEnvironmentsField installs pinnedEnvironments, the
// environments an admin pinned to the repository's home page.
func (s *Resolver) addRepositoryPinnedEnvironmentsField() {
	repoType := s.graphqlTypes.repository
	pinnedType := s.graphqlTypes.pinnedEnvironment
	if repoType == nil || pinnedType == nil {
		return
	}
	repoType.AddFieldConfig("pinnedEnvironments", &graphql.Field{
		Type: s.gqlConnectionType("PinnedEnvironment", pinnedType),
		Args: connectionArgs(nil),
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			repo, err := s.repoFromSource(p.Source)
			if err != nil {
				return nil, err
			}
			if !s.viewerCanReadRepo(p.Context, repo) {
				return paginateGQLItems(nil, p.Args), nil
			}
			repoSource, _ := p.Source.(map[string]interface{})
			pins := s.store.Deployments.ListPinnedEnvironments(repo.ID)
			items := make([]gqlConnItem, 0, len(pins))
			for i := range pins {
				pin := pins[i]
				env := s.store.Deployments.GetEnvironmentByID(pin.EnvID)
				if env == nil {
					continue
				}
				items = append(items, gqlConnItem{
					identity: pin.NodeID,
					render:   func() map[string]interface{} { return s.pinnedEnvironmentSource(repo, env, pin, repoSource) },
				})
			}
			return paginateGQLItems(items, p.Args), nil
		},
	})
}

// addRepositoryRecentProjectsField installs recentProjects, the owner's recently
// touched Projects v2 (a repository has none of its own).
func (s *Resolver) addRepositoryRecentProjectsField(repoType *graphql.Object) {
	connection := s.gqlConnectionType("ProjectV2", s.graphqlTypes.projectV2Type)
	repoType.AddFieldConfig("recentProjects", &graphql.Field{
		Type: graphql.NewNonNull(connection),
		Args: relayConnectionArgs(),
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			ownerID, ownerType, err := s.repoSourceOwner(p.Source)
			if err != nil {
				return nil, err
			}
			return s.projectV2Connection(p, ownerID, ownerType), nil
		},
	})
}

// repoSourceOwner resolves the account whose projects a repository's Projects tab
// surfaces, from the source's database id.
func (s *Resolver) repoSourceOwner(source interface{}) (int, string, error) {
	src, ok := source.(map[string]interface{})
	if !ok {
		return 0, "", fmt.Errorf("resolve source: unexpected type %T", source)
	}
	repoID, _ := src["databaseId"].(int)
	repo := s.store.GetRepoByID(repoID)
	if repo == nil {
		return 0, "", fmt.Errorf("repository source missing databaseId")
	}
	if org := s.store.GetOrgByID(repo.OwnerID); org != nil {
		return org.ID, "Organization", nil
	}
	return repo.OwnerID, "User", nil
}

// addRepositoryCustomPropertyFields installs repositoryCustomPropertyValue and its
// list counterpart, from the effective (org + enterprise) property values.
func (s *Resolver) addRepositoryCustomPropertyFields(repoType *graphql.Object) {
	valueScalar := s.graphQLStringScalar("CustomPropertyValue")
	valueType := graphql.NewObject(graphql.ObjectConfig{
		Name: "RepositoryCustomPropertyValue",
		Fields: graphql.Fields{
			"propertyName": &graphql.Field{Type: graphql.NewNonNull(graphql.String), Resolve: sourceKeyResolver("propertyName")},
			"value":        &graphql.Field{Type: graphql.NewNonNull(valueScalar), Resolve: sourceKeyResolver("value")},
		},
	})
	connectionType := s.gqlConnectionType("RepositoryCustomPropertyValue", valueType)

	propertyValues := func(repo *store.Repo) []map[string]interface{} {
		owner, _, ok := store.SplitRepoFullName(repo.FullName)
		if !ok {
			return nil
		}
		raw := s.store.EffectiveRepoCustomPropertyValues(owner, repo.FullName)
		out := make([]map[string]interface{}, 0, len(raw))
		for _, m := range raw {
			out = append(out, map[string]interface{}{
				"propertyName": m["property_name"],
				"value":        m["value"],
			})
		}
		return out
	}

	repoType.AddFieldConfig("repositoryCustomPropertyValue", &graphql.Field{
		Type: valueType,
		Args: graphql.FieldConfigArgument{
			"propertyName": &graphql.ArgumentConfig{Type: graphql.NewNonNull(graphql.String)},
		},
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			repo, err := s.repoFromSource(p.Source)
			if err != nil {
				return nil, err
			}
			if !s.viewerCanReadRepo(p.Context, repo) {
				return nil, nil
			}
			name, _ := p.Args["propertyName"].(string)
			for _, v := range propertyValues(repo) {
				if pn, _ := v["propertyName"].(string); pn == name {
					return v, nil
				}
			}
			return nil, nil
		},
	})
	repoType.AddFieldConfig("repositoryCustomPropertyValues", &graphql.Field{
		Type: connectionType,
		Args: relayConnectionArgs(),
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			repo, err := s.repoFromSource(p.Source)
			if err != nil {
				return nil, err
			}
			if !s.viewerCanReadRepo(p.Context, repo) {
				return paginateGQLMaps(nil, p.Args), nil
			}
			return paginateGQLMaps(propertyValues(repo), p.Args), nil
		},
	})
}

// addRepositorySuggestedActorsField installs suggestedActors: the repository's
// mentionable accounts, as the Actor interface.
func (s *Resolver) addRepositorySuggestedActorsField(repoType *graphql.Object) {
	actorConnection := s.gqlConnectionType("Actor", s.graphqlTypes.actor)
	filterEnum := s.sharedEnum("RepositorySuggestedActorFilter", "CAN_BE_ASSIGNED", "CAN_BE_AUTHOR")
	repoType.AddFieldConfig("suggestedActors", &graphql.Field{
		Type: graphql.NewNonNull(actorConnection),
		Args: connectionArgs(graphql.FieldConfigArgument{
			"capabilities": &graphql.ArgumentConfig{Type: graphql.NewNonNull(graphql.NewList(graphql.NewNonNull(filterEnum)))},
			"loginNames":   &graphql.ArgumentConfig{Type: graphql.String},
			"query":        &graphql.ArgumentConfig{Type: graphql.String},
		}),
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			repo, err := s.repoFromSource(p.Source)
			if err != nil {
				return nil, err
			}
			if !s.viewerCanReadRepo(p.Context, repo) {
				return paginateGQLMaps(nil, p.Args), nil
			}
			users := s.repositoryMentionableUsers(repo, p.Args)
			nodes := make([]map[string]interface{}, 0, len(users))
			for _, u := range users {
				nodes = append(nodes, userToGraphQL(u))
			}
			return paginateGQLMaps(nodes, p.Args), nil
		},
	})
}

// addRepositoryVulnerabilityAlertField installs vulnerabilityAlert(number), the
// single-alert counterpart to vulnerabilityAlerts.
func (s *Resolver) addRepositoryVulnerabilityAlertField(repoType *graphql.Object) {
	alertType := namedObjectFromField(namedObjectFromField(repoType, "vulnerabilityAlerts"), "nodes")
	if alertType == nil {
		return
	}
	repoType.AddFieldConfig("vulnerabilityAlert", &graphql.Field{
		Type: alertType,
		Args: graphql.FieldConfigArgument{
			"number": &graphql.ArgumentConfig{Type: graphql.NewNonNull(graphql.Int)},
		},
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			repo, err := s.repoFromSource(p.Source)
			if err != nil {
				return nil, err
			}
			if !s.viewerHasRepoSecurityAccess(p.Context, repo) {
				return nil, nil
			}
			number, _ := p.Args["number"].(int)
			alert := s.store.GetDependabotAlert(repo.FullName, number)
			if alert == nil {
				return nil, nil
			}
			return s.vulnerabilityAlertToGQL(repo, alert), nil
		},
	})
}

// Commit

func (s *Resolver) addCommitGitResidualFields(types *accountSurfaceTypes) {
	commitType := s.graphqlTypes.commit
	if commitType == nil {
		return
	}

	s.addCommitAssociatedPullRequestsField(commitType)
	s.addCommitBlameField(commitType)
	s.addCommitCheckSuitesField(commitType)
	s.addCommitCommentsField(types, commitType)
	s.addCommitStatusField(commitType)
	s.addCommitSubmodulesField(types, commitType)
	s.addCommitSubscriptionFields(commitType)
	s.addCommitSignatureField(commitType)
}

// addLateGitResidualFields installs the Commit and Repository members whose types
// the deployment family assembles after the early residual pass.
func (s *Resolver) addLateGitResidualFields() {
	s.addCommitDeploymentsField()
	s.addRepositoryPinnedEnvironmentsField()
}

// addCommitDeploymentsField installs Commit.deployments, matched by deployment SHA.
func (s *Resolver) addCommitDeploymentsField() {
	commitType := s.graphqlTypes.commit
	conn := s.namedObject("DeploymentConnection")
	if commitType == nil || conn == nil {
		return
	}
	commitType.AddFieldConfig("deployments", &graphql.Field{
		Type: conn,
		Args: relayConnectionArgs(),
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			repo, _, err := s.gitSourceRepo(p.Context, p.Source)
			if err != nil || repo == nil {
				return nil, err
			}
			sha := residualSourceString(p.Source, "oid")
			repoSource := repoToGraphQL(s.store, repo)
			var items []gqlConnItem
			for _, d := range s.store.Deployments.ListDeployments(repo.ID) {
				if sha != "" && !strings.EqualFold(d.Sha, sha) {
					continue
				}
				d := d
				items = append(items, gqlConnItem{
					identity: fmt.Sprintf("%d", d.ID),
					render:   func() map[string]interface{} { return s.deploymentSource(repo, d, repoSource) },
				})
			}
			return paginateGQLItems(items, p.Args), nil
		},
	})
}

// addCommitSignatureField installs Commit.signature. bleephub runs no
// verification service, so a signed commit reports GPGVERIFY_UNAVAILABLE /
// isValid false rather than claiming a verification; unsigned resolves null.
func (s *Resolver) addCommitSignatureField(commitType *graphql.Object) {
	sig := s.gqlGitSignatureInterface()
	commitType.AddFieldConfig("signature", &graphql.Field{
		Type: sig,
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			repo, stor, err := s.gitSourceRepo(p.Context, p.Source)
			if err != nil || repo == nil || stor == nil {
				return nil, err
			}
			sha := residualSourceString(p.Source, "oid")
			if sha == "" {
				return nil, nil
			}
			commit, err := object.GetCommit(stor, plumbing.NewHash(sha))
			if err != nil || commit == nil || strings.TrimSpace(commit.PGPSignature) == "" {
				return nil, nil
			}
			email := strings.ToLower(strings.TrimSpace(commit.Committer.Email))
			var signer interface{}
			if u := s.store.LookupUserByEmail(email); u != nil {
				signer = userToGraphQL(u)
			}
			return map[string]interface{}{
				"__typename":        gitSignatureTypename(commit.PGPSignature),
				"email":             commit.Committer.Email,
				"isValid":           false,
				"payload":           commitSignaturePayload(commit),
				"signature":         commit.PGPSignature,
				"signer":            signer,
				"state":             "GPGVERIFY_UNAVAILABLE",
				"verifiedAt":        nil,
				"wasSignedByGitHub": false,
				"keyId":             nil,
				"keyFingerprint":    nil,
			}, nil
		},
	})
}

// gqlGitSignatureInterface builds GitSignature and its concrete members
// (memoized), which are also registered in the schema Types list so fragments resolve.
func (s *Resolver) gqlGitSignatureInterface() *graphql.Interface {
	if existing := s.mutationInterfaces["GitSignature"]; existing != nil {
		return existing
	}
	dateTime := s.graphQLStringScalar("DateTime")
	state := s.sharedEnum("GitSignatureState",
		"BAD_CERT", "BAD_EMAIL", "EXPIRED_KEY", "GPGVERIFY_ERROR", "GPGVERIFY_UNAVAILABLE",
		"INVALID", "MALFORMED_SIG", "NOT_SIGNING_KEY", "NO_USER", "OCSP_ERROR", "OCSP_PENDING",
		"OCSP_REVOKED", "UNKNOWN_KEY", "UNKNOWN_SIG_TYPE", "UNSIGNED", "UNVERIFIED_EMAIL", "VALID")
	base := func() graphql.Fields {
		return graphql.Fields{
			"email":             &graphql.Field{Type: graphql.NewNonNull(graphql.String)},
			"isValid":           &graphql.Field{Type: graphql.NewNonNull(graphql.Boolean)},
			"payload":           &graphql.Field{Type: graphql.NewNonNull(graphql.String)},
			"signature":         &graphql.Field{Type: graphql.NewNonNull(graphql.String)},
			"signer":            &graphql.Field{Type: s.graphqlTypes.user},
			"state":             &graphql.Field{Type: graphql.NewNonNull(state)},
			"verifiedAt":        &graphql.Field{Type: dateTime},
			"wasSignedByGitHub": &graphql.Field{Type: graphql.NewNonNull(graphql.Boolean)},
		}
	}
	iface := s.mutationInterface("GitSignature", func() graphql.Fields { return base() }, func(p graphql.ResolveTypeParams) *graphql.Object {
		src, _ := p.Value.(map[string]interface{})
		name, _ := src["__typename"].(string)
		if obj := s.namedObject(name); obj != nil {
			return obj
		}
		return s.namedObject("UnknownSignature")
	})
	build := func(name string, extra graphql.Fields) {
		fields := base()
		for k, v := range extra {
			fields[k] = v
		}
		obj := graphql.NewObject(graphql.ObjectConfig{
			Name:       name,
			Interfaces: []*graphql.Interface{iface},
			Fields:     fields,
		})
		s.stashNamedObject(obj)
	}
	build("GpgSignature", graphql.Fields{"keyId": &graphql.Field{Type: graphql.String}})
	build("SshSignature", graphql.Fields{"keyFingerprint": &graphql.Field{Type: graphql.String}})
	build("SmimeSignature", nil)
	build("UnknownSignature", nil)
	return iface
}

// gitSignatureTypename picks the concrete GitSignature type from a signature's
// ASCII-armor header.
func gitSignatureTypename(armor string) string {
	switch {
	case strings.Contains(armor, "BEGIN PGP SIGNATURE"):
		return "GpgSignature"
	case strings.Contains(armor, "BEGIN SSH SIGNATURE"):
		return "SshSignature"
	case strings.Contains(armor, "BEGIN SIGNED MESSAGE"), strings.Contains(armor, "PKCS7"):
		return "SmimeSignature"
	default:
		return "UnknownSignature"
	}
}

// commitSignaturePayload reconstructs the signed payload: the commit object text
// with its signature header removed.
func commitSignaturePayload(commit *object.Commit) string {
	unsigned := *commit
	unsigned.PGPSignature = ""
	var buf bytes.Buffer
	obj := &plumbing.MemoryObject{}
	if err := unsigned.Encode(obj); err != nil {
		return ""
	}
	reader, err := obj.Reader()
	if err != nil {
		return ""
	}
	defer reader.Close()
	if _, err := buf.ReadFrom(reader); err != nil {
		return ""
	}
	return buf.String()
}

// addCommitAssociatedPullRequestsField installs associatedPullRequests, matched by
// head/merge SHA.
func (s *Resolver) addCommitAssociatedPullRequestsField(commitType *graphql.Object) {
	commitType.AddFieldConfig("associatedPullRequests", &graphql.Field{
		Type: s.graphqlTypes.pullRequestConnection,
		Args: connectionArgs(graphql.FieldConfigArgument{
			"orderBy": &graphql.ArgumentConfig{Type: s.gqlOrderInput(s.accountSurfaceRegistry(), "PullRequestOrder", "PullRequestOrderField", "CREATED_AT")},
		}),
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			repo, _, err := s.gitSourceRepo(p.Context, p.Source)
			if err != nil || repo == nil {
				return paginateGQLMaps(nil, p.Args), err
			}
			sha := residualSourceString(p.Source, "oid")
			prs := s.store.ListPullRequests(repo.ID, "")
			s.store.Mu.RLock()
			matched := make([]*store.PullRequest, 0)
			for _, pr := range prs {
				if pr.MergeCommitSHA == sha || store.PullRequestHeadSHALocked(pr, s.store) == sha {
					matched = append(matched, pr)
				}
			}
			s.store.Mu.RUnlock()
			nodes := make([]map[string]interface{}, 0, len(matched))
			for _, pr := range matched {
				nodes = append(nodes, pullRequestToGQL(pr, s.store))
			}
			return paginateGQLMaps(nodes, p.Args), nil
		},
	})
}

// addCommitBlameField installs blame(path), a real git blame over the commit's tree.
func (s *Resolver) addCommitBlameField(commitType *graphql.Object) {
	blameRangeType := graphql.NewObject(graphql.ObjectConfig{
		Name: "BlameRange",
		Fields: graphql.Fields{
			"age":          &graphql.Field{Type: graphql.NewNonNull(graphql.Int), Resolve: sourceKeyResolver("age")},
			"startingLine": &graphql.Field{Type: graphql.NewNonNull(graphql.Int), Resolve: sourceKeyResolver("startingLine")},
			"endingLine":   &graphql.Field{Type: graphql.NewNonNull(graphql.Int), Resolve: sourceKeyResolver("endingLine")},
			"commit":       &graphql.Field{Type: graphql.NewNonNull(commitType), Resolve: sourceKeyResolver("commit")},
		},
	})
	blameType := graphql.NewObject(graphql.ObjectConfig{
		Name: "Blame",
		Fields: graphql.Fields{
			"ranges": &graphql.Field{Type: graphql.NewNonNull(graphql.NewList(graphql.NewNonNull(blameRangeType))), Resolve: sourceKeyResolver("ranges")},
		},
	})
	commitType.AddFieldConfig("blame", &graphql.Field{
		Type: graphql.NewNonNull(blameType),
		Args: graphql.FieldConfigArgument{
			"path": &graphql.ArgumentConfig{Type: graphql.NewNonNull(graphql.String)},
		},
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			repo, stor, commit, err := s.gitSourceCommit(p)
			empty := map[string]interface{}{"ranges": []map[string]interface{}{}}
			if err != nil || repo == nil || stor == nil || commit == nil {
				return empty, err
			}
			path, _ := p.Args["path"].(string)
			result, blameErr := git.Blame(commit, path)
			if blameErr != nil || result == nil {
				return empty, nil
			}
			return map[string]interface{}{"ranges": s.blameRanges(p.Context, repo.FullName, stor, result)}, nil
		},
	})
}

// blameRanges coalesces a git blame result into per-commit line ranges and
// assigns each an age (1 = the most recent commit that touched the file).
func (s *Resolver) blameRanges(ctx context.Context, repoFullName string, stor gitStorage.Storer, result *git.BlameResult) []map[string]interface{} {
	type span struct {
		hash       plumbing.Hash
		start, end int
	}
	var spans []span
	for i, line := range result.Lines {
		lineNo := i + 1
		if n := len(spans); n > 0 && spans[n-1].hash == line.Hash {
			spans[n-1].end = lineNo
			continue
		}
		spans = append(spans, span{hash: line.Hash, start: lineNo, end: lineNo})
	}
	// Rank distinct commits by commit date, most recent first, for the age.
	dates := map[plumbing.Hash]time.Time{}
	order := make([]plumbing.Hash, 0)
	for _, sp := range spans {
		if _, seen := dates[sp.hash]; seen {
			continue
		}
		when := time.Time{}
		if c, e := object.GetCommit(stor, sp.hash); e == nil {
			when = c.Committer.When
		}
		dates[sp.hash] = when
		order = append(order, sp.hash)
	}
	sort.SliceStable(order, func(a, b int) bool { return dates[order[a]].After(dates[order[b]]) })
	age := map[plumbing.Hash]int{}
	for rank, h := range order {
		age[h] = rank + 1
	}
	ranges := make([]map[string]interface{}, 0, len(spans))
	for _, sp := range spans {
		ranges = append(ranges, map[string]interface{}{
			"age":          age[sp.hash],
			"startingLine": sp.start,
			"endingLine":   sp.end,
			"commit":       s.commitSourceForRepoSHA(ctx, repoFullName, sp.hash.String()),
		})
	}
	return ranges
}

// addCommitCheckSuitesField installs checkSuites, the check suites recorded for
// the commit's SHA.
func (s *Resolver) addCommitCheckSuitesField(commitType *graphql.Object) {
	suiteType := s.graphqlTypes.checkSuite
	if suiteType == nil {
		return
	}
	filterInput := graphql.NewInputObject(graphql.InputObjectConfig{
		Name: "CheckSuiteFilter",
		Fields: graphql.InputObjectConfigFieldMap{
			"appId":     &graphql.InputObjectFieldConfig{Type: graphql.Int},
			"checkName": &graphql.InputObjectFieldConfig{Type: graphql.String},
		},
	})
	commitType.AddFieldConfig("checkSuites", &graphql.Field{
		Type: s.gqlConnectionType("CheckSuite", suiteType),
		Args: connectionArgs(graphql.FieldConfigArgument{
			"filterBy": &graphql.ArgumentConfig{Type: filterInput},
		}),
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			repo, _, err := s.gitSourceRepo(p.Context, p.Source)
			if err != nil || repo == nil {
				return paginateGQLItems(nil, p.Args), err
			}
			sha := residualSourceString(p.Source, "oid")
			suites := s.store.ListCheckSuitesForCommit(repo.FullName, sha, 0)
			items := make([]gqlConnItem, 0, len(suites))
			for i := range suites {
				suite := suites[i]
				items = append(items, gqlConnItem{
					identity: fmt.Sprintf("CS_%d", suite.ID),
					render:   func() map[string]interface{} { return s.checkSuiteMutationSource(suite) },
				})
			}
			return paginateGQLItems(items, p.Args), nil
		},
	})
}

// addCommitCommentsField installs comments, the commit comments on the commit.
func (s *Resolver) addCommitCommentsField(types *accountSurfaceTypes, commitType *graphql.Object) {
	commentType := s.graphqlTypes.commitComment
	if commentType == nil {
		return
	}
	commitType.AddFieldConfig("comments", &graphql.Field{
		Type: graphql.NewNonNull(s.accountConnectionType(types, "CommitComment", commentType, false, nil)),
		Args: relayConnectionArgs(),
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			repo, _, err := s.gitSourceRepo(p.Context, p.Source)
			if err != nil || repo == nil {
				return paginateGQLItems(nil, p.Args), err
			}
			sha := residualSourceString(p.Source, "oid")
			return paginateGQLItems(s.commitCommentItems(s.store.CommitComments.ListForCommit(repo.ID, sha)), p.Args), nil
		},
	})
}

// addCommitStatusField installs status, the legacy combined commit status. Null
// when no status was ever posted.
func (s *Resolver) addCommitStatusField(commitType *graphql.Object) {
	rollupType := s.graphqlTypes.statusCheckRollup
	if rollupType == nil {
		return
	}
	combinedConnType := namedObjectFromField(rollupType, "contexts")
	contextUnion := namedUnionFromField(combinedConnType, "nodes")
	statusContextType := unionMemberObject(contextUnion, "StatusContext")
	if combinedConnType == nil || statusContextType == nil {
		return
	}
	statusStateEnum := s.graphQLEnum("StatusState", "ERROR", "EXPECTED", "FAILURE", "PENDING", "SUCCESS")

	statusType := graphql.NewObject(graphql.ObjectConfig{
		Name: "Status",
		Fields: graphql.Fields{
			"id":    &graphql.Field{Type: graphql.NewNonNull(graphql.ID), Resolve: sourceKeyResolver("id")},
			"state": &graphql.Field{Type: graphql.NewNonNull(statusStateEnum), Resolve: sourceKeyResolver("state")},
			"commit": &graphql.Field{
				Type:    commitType,
				Resolve: sourceKeyResolver("commit"),
			},
			"contexts": &graphql.Field{
				Type:    graphql.NewNonNull(graphql.NewList(graphql.NewNonNull(statusContextType))),
				Resolve: sourceKeyResolver("contexts"),
			},
			"context": &graphql.Field{
				Type: statusContextType,
				Args: graphql.FieldConfigArgument{
					"name": &graphql.ArgumentConfig{Type: graphql.NewNonNull(graphql.String)},
				},
				Resolve: func(p graphql.ResolveParams) (interface{}, error) {
					src, _ := p.Source.(map[string]interface{})
					name, _ := p.Args["name"].(string)
					contexts, _ := src["contexts"].([]map[string]interface{})
					for _, c := range contexts {
						if cn, _ := c["context"].(string); cn == name {
							return c, nil
						}
					}
					return nil, nil
				},
			},
			"combinedContexts": &graphql.Field{
				Type: graphql.NewNonNull(combinedConnType),
				Args: relayConnectionArgs(),
				Resolve: func(p graphql.ResolveParams) (interface{}, error) {
					src, _ := p.Source.(map[string]interface{})
					return src["combinedContexts"], nil
				},
			},
		},
	})

	commitType.AddFieldConfig("status", &graphql.Field{
		Type: statusType,
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			repo, _, err := s.gitSourceRepo(p.Context, p.Source)
			if err != nil || repo == nil {
				return nil, err
			}
			sha := residualSourceString(p.Source, "oid")
			state, total, statuses := s.store.CommitStatuses.Combined(repo.FullName, sha)
			if total == 0 || len(statuses) == 0 {
				return nil, nil
			}
			combined := s.statusRollupContexts(repo.FullName, sha)
			contexts := make([]map[string]interface{}, 0, len(statuses))
			if combined != nil {
				if nodes, ok := combined["nodes"].([]interface{}); ok {
					for _, n := range nodes {
						if m, ok := n.(map[string]interface{}); ok {
							if tn, _ := m["__typename"].(string); tn == "StatusContext" {
								contexts = append(contexts, m)
							}
						}
					}
				}
			}
			return map[string]interface{}{
				"id":               "MDY6U3RhdHVz" + sha,
				"state":            strings.ToUpper(state),
				"commit":           s.commitSourceForRepoSHA(p.Context, repo.FullName, sha),
				"contexts":         contexts,
				"combinedContexts": combined,
			}, nil
		},
	})
}

// statusRollupContexts returns the StatusCheckRollupContextConnection source for
// a commit (StatusContext + CheckRun nodes), under the store read lock.
func (s *Resolver) statusRollupContexts(repoKey, sha string) map[string]interface{} {
	s.store.Mu.RLock()
	defer s.store.Mu.RUnlock()
	rollup, _ := statusCheckRollupSourceLocked(s.store, repoKey, sha).(map[string]interface{})
	if rollup == nil {
		return nil
	}
	contexts, _ := rollup["contexts"].(map[string]interface{})
	return contexts
}

// addCommitSubmodulesField installs submodules, from the commit tree's .gitmodules.
func (s *Resolver) addCommitSubmodulesField(types *accountSurfaceTypes, commitType *graphql.Object) {
	repoType := types.repository
	submoduleType := namedObjectFromField(namedObjectFromField(repoType, "submodules"), "nodes")
	if submoduleType == nil {
		return
	}
	commitType.AddFieldConfig("submodules", &graphql.Field{
		Type: graphql.NewNonNull(s.accountConnectionType(types, "Submodule", submoduleType, false, nil)),
		Args: relayConnectionArgs(),
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			repo, stor, commit, err := s.gitSourceCommit(p)
			if err != nil || repo == nil || stor == nil || commit == nil {
				return paginateGQLItems(nil, p.Args), err
			}
			tree, terr := commit.Tree()
			if terr != nil {
				return paginateGQLItems(nil, p.Args), nil
			}
			return paginateGQLItems(submoduleItemsFromTree(stor, repo.FullName, tree), p.Args), nil
		},
	})
}

// submoduleItemsFromTree pairs each .gitmodules submodule with the commit the
// tree records at its path.
func submoduleItemsFromTree(stor gitStorage.Storer, repoFullName string, tree *object.Tree) []gqlConnItem {
	content, found := readTreeFile(stor, tree, ".gitmodules")
	if !found {
		return nil
	}
	modules := parseGitmodules(string(content))
	items := make([]gqlConnItem, 0, len(modules))
	for _, module := range modules {
		row := module
		var commitOID interface{}
		if entry, err := tree.FindEntry(row.path); err == nil {
			commitOID = entry.Hash.String()
		}
		items = append(items, gqlConnItem{
			identity: repoFullName + ":" + row.name,
			render:   func() map[string]interface{} { return submoduleSource(row, commitOID) },
		})
	}
	return items
}

// submoduleSource renders a .gitmodules entry as the Submodule source map.
func submoduleSource(module gitSubmodule, commitOID interface{}) map[string]interface{} {
	return map[string]interface{}{
		"branch":              nilStr(module.branch),
		"gitUrl":              module.url,
		"name":                module.name,
		"nameRaw":             base64.StdEncoding.EncodeToString([]byte(module.name)),
		"path":                module.path,
		"pathRaw":             base64.StdEncoding.EncodeToString([]byte(module.path)),
		"subprojectCommitOid": commitOID,
	}
}

// addCommitSubscriptionFields installs viewerCanSubscribe / viewerSubscription
// for the commit's repository.
func (s *Resolver) addCommitSubscriptionFields(commitType *graphql.Object) {
	subscriptionState := s.sharedEnum("SubscriptionState", "IGNORED", "SUBSCRIBED", "UNSUBSCRIBED")
	commitType.AddFieldConfig("viewerCanSubscribe", &graphql.Field{
		Type: graphql.NewNonNull(graphql.Boolean),
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			return s.ghUserFromContext(p.Context) != nil, nil
		},
	})
	commitType.AddFieldConfig("viewerSubscription", &graphql.Field{
		Type: subscriptionState,
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			repo, _, err := s.gitSourceRepo(p.Context, p.Source)
			if err != nil || repo == nil {
				return nil, err
			}
			return s.viewerRepoSubscriptionState(p.Context, repo), nil
		},
	})
}

// viewerRepoSubscriptionState maps the viewer's repository subscription to the
// GraphQL SubscriptionState enum.
func (s *Resolver) viewerRepoSubscriptionState(ctx context.Context, repo *store.Repo) interface{} {
	viewer := s.ghUserFromContext(ctx)
	if viewer == nil {
		return nil
	}
	sub := s.store.GetRepoSubscription(viewer.ID, repo.ID)
	switch {
	case sub == nil:
		return "UNSUBSCRIBED"
	case sub.Ignored:
		return "IGNORED"
	case sub.Subscribed:
		return "SUBSCRIBED"
	default:
		return "UNSUBSCRIBED"
	}
}

// Ref

func (s *Resolver) addRefGitResidualFields() {
	refType := s.graphqlTypes.ref
	if refType == nil {
		return
	}
	s.addRefAssociatedPullRequestsField(refType)
	s.addRefCompareField(refType)
	s.addRefUpdateRuleField(refType)
}

// addRefAssociatedPullRequestsField installs associatedPullRequests, the pull
// requests whose head or base is this ref.
func (s *Resolver) addRefAssociatedPullRequestsField(refType *graphql.Object) {
	refType.AddFieldConfig("associatedPullRequests", &graphql.Field{
		Type: graphql.NewNonNull(s.graphqlTypes.pullRequestConnection),
		Args: connectionArgs(graphql.FieldConfigArgument{
			"baseRefName": &graphql.ArgumentConfig{Type: graphql.String},
			"headRefName": &graphql.ArgumentConfig{Type: graphql.String},
			"labels":      &graphql.ArgumentConfig{Type: graphql.NewList(graphql.NewNonNull(graphql.String))},
			"states":      &graphql.ArgumentConfig{Type: graphql.NewList(graphql.NewNonNull(s.sharedEnum("PullRequestState", "CLOSED", "MERGED", "OPEN")))},
			"orderBy":     &graphql.ArgumentConfig{Type: s.graphqlTypes.issueOrder},
		}),
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			repo, _, err := s.gitSourceRepo(p.Context, p.Source)
			if err != nil || repo == nil {
				return paginateGQLMaps(nil, p.Args), err
			}
			refName := residualRefName(p.Source)
			states, _ := p.Args["states"].([]interface{})
			nodes := make([]map[string]interface{}, 0)
			for _, pr := range s.store.ListPullRequests(repo.ID, "") {
				if pr.HeadRefName != refName && pr.BaseRefName != refName {
					continue
				}
				if !prMatchesStates(pr, states) {
					continue
				}
				nodes = append(nodes, pullRequestToGQL(pr, s.store))
			}
			sortGQLNodesByCreatedAt(nodes)
			return paginateGQLMaps(nodes, p.Args), nil
		},
	})
}

// residualRefName resolves the short branch/tag name a Ref source carries.
func residualRefName(source interface{}) string {
	src, _ := source.(map[string]interface{})
	if name, _ := src["name"].(string); name != "" {
		return name
	}
	qualified, _ := src["qualifiedName"].(string)
	_, name := splitGitRefName(qualified)
	return name
}

// prMatchesStates reports whether a pull request's state is in the requested
// set (an empty set matches every state).
func prMatchesStates(pr *store.PullRequest, states []interface{}) bool {
	if len(states) == 0 {
		return true
	}
	current := strings.ToUpper(pr.State)
	for _, st := range states {
		if s, _ := st.(string); s == current {
			return true
		}
	}
	return false
}

// addRefCompareField installs compare(headRef), the two-dot comparison between
// this ref and another, computed over the real commit graph.
func (s *Resolver) addRefCompareField(refType *graphql.Object) {
	comparisonStatus := s.sharedEnum("ComparisonStatus", "AHEAD", "BEHIND", "DIVERGED", "IDENTICAL")
	// ComparisonCommitConnection carries the standard CommitEdge plus authorCount;
	// build it by hand so the factory does not mint an unnamed ComparisonCommitEdge.
	// gqlCommitConnectionType populates s.graphqlTypes.commitEdge.
	s.gqlCommitConnectionType("CommitConnection")
	commitConnection := graphql.NewObject(graphql.ObjectConfig{
		Name: "ComparisonCommitConnection",
		Fields: graphql.Fields{
			"authorCount": &graphql.Field{Type: graphql.NewNonNull(graphql.Int), Resolve: sourceKeyResolver("authorCount")},
			"edges":       &graphql.Field{Type: graphql.NewList(s.graphqlTypes.commitEdge)},
			"nodes":       &graphql.Field{Type: graphql.NewList(s.graphqlTypes.commit)},
			"pageInfo":    &graphql.Field{Type: graphql.NewNonNull(s.gqlPageInfoType())},
			"totalCount":  &graphql.Field{Type: graphql.NewNonNull(graphql.Int)},
		},
	})
	comparisonType := graphql.NewObject(graphql.ObjectConfig{
		Name:       "Comparison",
		Interfaces: []*graphql.Interface{s.graphqlTypes.node},
		Fields: graphql.Fields{
			"id":         &graphql.Field{Type: graphql.NewNonNull(graphql.ID), Resolve: sourceKeyResolver("id")},
			"aheadBy":    &graphql.Field{Type: graphql.NewNonNull(graphql.Int), Resolve: sourceKeyResolver("aheadBy")},
			"behindBy":   &graphql.Field{Type: graphql.NewNonNull(graphql.Int), Resolve: sourceKeyResolver("behindBy")},
			"status":     &graphql.Field{Type: graphql.NewNonNull(comparisonStatus), Resolve: sourceKeyResolver("status")},
			"baseTarget": &graphql.Field{Type: graphql.NewNonNull(s.gqlGitObjectInterface()), Resolve: sourceKeyResolver("baseTarget")},
			"headTarget": &graphql.Field{Type: graphql.NewNonNull(s.gqlGitObjectInterface()), Resolve: sourceKeyResolver("headTarget")},
			"commits": &graphql.Field{
				Type: graphql.NewNonNull(commitConnection),
				Args: relayConnectionArgs(),
				Resolve: func(p graphql.ResolveParams) (interface{}, error) {
					src, _ := p.Source.(map[string]interface{})
					conn := paginateGQLMaps(src["commits"].([]map[string]interface{}), p.Args)
					conn["authorCount"] = src["authorCount"]
					return conn, nil
				},
			},
		},
	})
	refType.AddFieldConfig("compare", &graphql.Field{
		Type: comparisonType,
		Args: graphql.FieldConfigArgument{
			"headRef": &graphql.ArgumentConfig{Type: graphql.NewNonNull(graphql.String)},
		},
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			repo, stor, err := s.gitSourceRepo(p.Context, p.Source)
			if err != nil || repo == nil || stor == nil {
				return nil, err
			}
			baseOID := s.refSourceCommitOID(p.Source, stor)
			headArg, _ := p.Args["headRef"].(string)
			headOID := resolveRefCommitOID(stor, headArg)
			if baseOID == "" || headOID == "" {
				return nil, nil
			}
			return s.buildComparison(p.Context, repo.FullName, stor, baseOID, headOID), nil
		},
	})
}

// refSourceCommitOID resolves the commit a Ref points at (dereferencing an
// annotated tag), from its recorded target or its qualified name.
func (s *Resolver) refSourceCommitOID(source interface{}, stor gitStorage.Storer) string {
	if oid := residualSourceString(source, "targetOID"); oid != "" {
		return dereferenceToCommit(stor, plumbing.NewHash(oid))
	}
	qualified, _ := gitRefQualifiedName(source)
	return resolveRefCommitOID(stor, qualified)
}

// resolveRefCommitOID resolves a ref expression (short name, qualified name or
// oid) to the commit it names, or "" when it cannot be resolved.
func resolveRefCommitOID(stor gitStorage.Storer, expr string) string {
	if expr == "" {
		return ""
	}
	candidates := []string{expr, "refs/heads/" + expr, "refs/tags/" + expr}
	for _, candidate := range candidates {
		if hash, found, err := store.ResolveGitObjectReference(stor, candidate); err == nil && found {
			return dereferenceToCommit(stor, hash)
		}
	}
	// Fall back to treating the expression as a raw object id.
	return dereferenceToCommit(stor, plumbing.NewHash(expr))
}

// dereferenceToCommit resolves a hash (commit or annotated tag) to a commit id.
func dereferenceToCommit(stor gitStorage.Storer, hash plumbing.Hash) string {
	if hash.IsZero() {
		return ""
	}
	if commit, err := object.GetCommit(stor, hash); err == nil {
		return commit.Hash.String()
	}
	if tag, err := object.GetTag(stor, hash); err == nil {
		if commit, err := tag.Commit(); err == nil {
			return commit.Hash.String()
		}
	}
	return ""
}

// buildComparison renders a Comparison between two commits from the commit graph.
func (s *Resolver) buildComparison(ctx context.Context, repoFullName string, stor gitStorage.Storer, baseOID, headOID string) map[string]interface{} {
	baseHash := plumbing.NewHash(baseOID)
	headHash := plumbing.NewHash(headOID)
	baseAncestors := commitAncestors(stor, baseHash)
	headAncestors := commitAncestors(stor, headHash)

	aheadCommits := make([]map[string]interface{}, 0)
	aheadBy := 0
	authors := map[string]struct{}{}
	for h := range headAncestors {
		if _, inBase := baseAncestors[h]; !inBase {
			aheadBy++
			if src := s.commitSourceForRepoSHA(ctx, repoFullName, h.String()); src != nil {
				aheadCommits = append(aheadCommits, src.(map[string]interface{}))
			}
			// authorCount: distinct authors and co-authors across the commits ahead.
			if commit, err := object.GetCommit(stor, h); err == nil {
				if email := strings.ToLower(strings.TrimSpace(commit.Author.Email)); email != "" {
					authors[email] = struct{}{}
				}
				for _, coauthor := range coAuthorEmails(commit.Message) {
					authors[coauthor] = struct{}{}
				}
			}
		}
	}
	behindBy := 0
	for h := range baseAncestors {
		if _, inHead := headAncestors[h]; !inHead {
			behindBy++
		}
	}
	sort.Slice(aheadCommits, func(a, b int) bool {
		return residualSourceString(aheadCommits[a], "committedDate") < residualSourceString(aheadCommits[b], "committedDate")
	})

	status := "IDENTICAL"
	switch {
	case aheadBy > 0 && behindBy > 0:
		status = "DIVERGED"
	case aheadBy > 0:
		status = "AHEAD"
	case behindBy > 0:
		status = "BEHIND"
	}
	return map[string]interface{}{
		"id":          "CMP_" + base64RawURL(repoFullName+":"+baseOID+"..."+headOID),
		"aheadBy":     aheadBy,
		"behindBy":    behindBy,
		"status":      status,
		"baseTarget":  optionalObject(gitObjectSource(stor, s.store, repoFullName, baseHash)),
		"headTarget":  optionalObject(gitObjectSource(stor, s.store, repoFullName, headHash)),
		"commits":     aheadCommits,
		"authorCount": len(authors),
	}
}

// coAuthorEmails extracts lowercased emails from a message's "Co-authored-by:
// Name <email>" trailers.
func coAuthorEmails(message string) []string {
	var out []string
	for _, line := range strings.Split(message, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(strings.ToLower(line), "co-authored-by:") {
			continue
		}
		if open := strings.LastIndex(line, "<"); open != -1 {
			if close := strings.Index(line[open:], ">"); close != -1 {
				email := strings.ToLower(strings.TrimSpace(line[open+1 : open+close]))
				if email != "" {
					out = append(out, email)
				}
			}
		}
	}
	return out
}

// commitAncestors returns the commits reachable from start (inclusive), bounded
// so a pathological history cannot run away.
func commitAncestors(stor gitStorage.Storer, start plumbing.Hash) map[plumbing.Hash]struct{} {
	seen := map[plumbing.Hash]struct{}{}
	if start.IsZero() {
		return seen
	}
	const cap = 5000
	queue := []plumbing.Hash{start}
	for len(queue) > 0 && len(seen) < cap {
		h := queue[0]
		queue = queue[1:]
		if _, ok := seen[h]; ok {
			continue
		}
		seen[h] = struct{}{}
		commit, err := object.GetCommit(stor, h)
		if err != nil {
			continue
		}
		queue = append(queue, commit.ParentHashes...)
	}
	return seen
}

// base64RawURL is the raw-url base64 of s, used for synthesized node ids.
func base64RawURL(s string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(s))
}

// addRefUpdateRuleField installs refUpdateRule, the branch-protection rule for
// this ref, or null when the branch is unprotected.
func (s *Resolver) addRefUpdateRuleField(refType *graphql.Object) {
	ruleType := graphql.NewObject(graphql.ObjectConfig{
		Name: "RefUpdateRule",
		Fields: graphql.Fields{
			"allowsDeletions":                &graphql.Field{Type: graphql.NewNonNull(graphql.Boolean), Resolve: sourceKeyResolver("allowsDeletions")},
			"allowsForcePushes":              &graphql.Field{Type: graphql.NewNonNull(graphql.Boolean), Resolve: sourceKeyResolver("allowsForcePushes")},
			"blocksCreations":                &graphql.Field{Type: graphql.NewNonNull(graphql.Boolean), Resolve: sourceKeyResolver("blocksCreations")},
			"pattern":                        &graphql.Field{Type: graphql.NewNonNull(graphql.String), Resolve: sourceKeyResolver("pattern")},
			"requiredApprovingReviewCount":   &graphql.Field{Type: graphql.Int, Resolve: sourceKeyResolver("requiredApprovingReviewCount")},
			"requiredStatusCheckContexts":    &graphql.Field{Type: graphql.NewList(graphql.String), Resolve: sourceKeyResolver("requiredStatusCheckContexts")},
			"requiresCodeOwnerReviews":       &graphql.Field{Type: graphql.NewNonNull(graphql.Boolean), Resolve: sourceKeyResolver("requiresCodeOwnerReviews")},
			"requiresConversationResolution": &graphql.Field{Type: graphql.NewNonNull(graphql.Boolean), Resolve: sourceKeyResolver("requiresConversationResolution")},
			"requiresLinearHistory":          &graphql.Field{Type: graphql.NewNonNull(graphql.Boolean), Resolve: sourceKeyResolver("requiresLinearHistory")},
			"requiresSignatures":             &graphql.Field{Type: graphql.NewNonNull(graphql.Boolean), Resolve: sourceKeyResolver("requiresSignatures")},
			"viewerAllowedToDismissReviews":  &graphql.Field{Type: graphql.NewNonNull(graphql.Boolean), Resolve: sourceKeyResolver("viewerAllowedToDismissReviews")},
			"viewerCanPush":                  &graphql.Field{Type: graphql.NewNonNull(graphql.Boolean), Resolve: sourceKeyResolver("viewerCanPush")},
		},
	})
	refType.AddFieldConfig("refUpdateRule", &graphql.Field{
		Type: ruleType,
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			repo, _, err := s.gitSourceRepo(p.Context, p.Source)
			if err != nil || repo == nil {
				return nil, err
			}
			branch := residualRefName(p.Source)
			bp := s.store.GetBranchProtection(repo.ID, branch)
			if bp == nil || !bp.IsProtected() {
				return nil, nil
			}
			return refUpdateRuleSource(bp, branch, s.viewerCanPushRepo(p.Context, repo)), nil
		},
	})
}

// refUpdateRuleSource maps a branch-protection record to the RefUpdateRule
// source shape.
func refUpdateRuleSource(bp *store.BranchProtection, pattern string, viewerCanPush bool) map[string]interface{} {
	enabled := func(v *store.BPEnabled) bool { return v != nil && v.Enabled }
	var requiredReviews interface{}
	var requiresCodeOwner bool
	if bp.RequiredPullRequestReviews != nil {
		requiredReviews = bp.RequiredPullRequestReviews.RequiredApprovingReviewCount
		requiresCodeOwner = bp.RequiredPullRequestReviews.RequireCodeOwnerReviews
	}
	var checks []interface{}
	if bp.RequiredStatusChecks != nil {
		checks = make([]interface{}, 0, len(bp.RequiredStatusChecks.Contexts))
		for _, c := range bp.RequiredStatusChecks.Contexts {
			checks = append(checks, c)
		}
	}
	return map[string]interface{}{
		"allowsDeletions":                enabled(bp.AllowDeletions),
		"allowsForcePushes":              enabled(bp.AllowForcePushes),
		"blocksCreations":                enabled(bp.BlockCreations),
		"pattern":                        pattern,
		"requiredApprovingReviewCount":   requiredReviews,
		"requiredStatusCheckContexts":    checks,
		"requiresCodeOwnerReviews":       requiresCodeOwner,
		"requiresConversationResolution": enabled(bp.RequiredConversationResolution),
		"requiresLinearHistory":          enabled(bp.RequiredLinearHistory),
		"requiresSignatures":             bp.RequiredSignatures != nil && bp.RequiredSignatures.Enabled,
		"viewerAllowedToDismissReviews":  viewerCanPush,
		"viewerCanPush":                  viewerCanPush,
	}
}

// TreeEntry

func (s *Resolver) addTreeEntryResidualFields() {
	entryType := s.graphqlTypes.treeEntry
	if entryType == nil {
		return
	}
	// isGenerated: bleephub ships no linguist/.gitattributes model, so false.
	entryType.AddFieldConfig("isGenerated", &graphql.Field{
		Type:    graphql.NewNonNull(graphql.Boolean),
		Resolve: func(graphql.ResolveParams) (interface{}, error) { return false, nil },
	})

	repoType := s.graphqlTypes.repository
	submoduleType := namedObjectFromField(namedObjectFromField(repoType, "submodules"), "nodes")
	if submoduleType == nil {
		return
	}
	entryType.AddFieldConfig("submodule", &graphql.Field{
		Type: submoduleType,
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			src, _ := p.Source.(map[string]interface{})
			// A submodule is a gitlink: a tree entry whose object is a commit.
			if t, _ := src["type"].(string); t != "commit" {
				return nil, nil
			}
			repo, stor, err := s.gitSourceRepo(p.Context, p.Source)
			if err != nil || repo == nil || stor == nil {
				return nil, err
			}
			path := residualSourceString(p.Source, "path")
			_, tree, ok := s.repositoryDefaultTree(repo)
			if !ok {
				return nil, nil
			}
			content, found := readTreeFile(stor, tree, ".gitmodules")
			if !found {
				return nil, nil
			}
			for _, module := range parseGitmodules(string(content)) {
				if module.path == path {
					oid := residualSourceString(p.Source, "oid")
					return submoduleSource(module, oid), nil
				}
			}
			return nil, nil
		},
	})
}

// RepositoryConnection

// addRepositoryConnectionResidualFields installs totalDiskUsage, the summed
// on-disk size of the connection's repositories.
func (s *Resolver) addRepositoryConnectionResidualFields() {
	repoConn := s.graphqlTypes.repositoryConnection
	if repoConn == nil {
		return
	}
	repoConn.AddFieldConfig("totalDiskUsage", &graphql.Field{
		Type: graphql.NewNonNull(graphql.Int),
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			src, _ := p.Source.(map[string]interface{})
			nodes, _ := src["nodes"].([]map[string]interface{})
			total := 0
			for _, node := range nodes {
				full, _ := node["nameWithOwner"].(string)
				if full == "" {
					continue
				}
				total += int(s.store.RepoSize(full))
			}
			return total, nil
		},
	})
}
