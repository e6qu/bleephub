package graphqlapi

// Schema-fidelity shells: the abstract-interface and union cluster. GitHub
// declares a family of interfaces and two unions over data this instance does
// not reach through any resolved field (the Copilot Agentic trait, the shared
// Comment/Updatable/Deletable traits, the profile/package/pinnable owner
// traits, the issue-field traits and the projects-v2 value union). bleephub
// does not retrofit these interfaces onto its concrete objects, so no field
// returns them; they exist only so the introspected schema matches GitHub's
// shape. graphql-go admits an interface with no possible types and a union
// whose members are otherwise reachable, so each is published through
// registerExtraSchemaType without an implementer.
//
// Every field is transcribed signature-exact against the vendored SDL
// (third_party/github-graphql-schema.graphql.gz): nullability and arguments
// included. Named field/argument types are never re-minted — they are reached
// from the concrete objects that already carry them (User, Organization,
// IssueComment, Issue, Team, Repository and the two issue-field unions) or
// obtained through the memoized enum/input constructors — so no second type of
// any name enters the schema. This builder runs last, once every family it
// reaches into is assembled.

import "github.com/graphql-go/graphql"

func init() {
	schemaShellBuilders = append(schemaShellBuilders, (*Resolver).addInterfaceShells)
}

// nilResolveType is the ResolveType every shell here carries: these interfaces
// and unions are never returned by a resolved field, so nothing dispatches
// through them.
func nilResolveType(graphql.ResolveTypeParams) *graphql.Object { return nil }

// paginationArgs is the four Relay window arguments, freshly minted so no two
// fields share an ArgumentConfig pointer.
func paginationArgs() graphql.FieldConfigArgument {
	return graphql.FieldConfigArgument{
		"after":  &graphql.ArgumentConfig{Type: graphql.String},
		"before": &graphql.ArgumentConfig{Type: graphql.String},
		"first":  &graphql.ArgumentConfig{Type: graphql.Int},
		"last":   &graphql.ArgumentConfig{Type: graphql.Int},
	}
}

// shellCopyArgs clones a concrete field definition's arguments into the config
// shape an interface field needs, reusing every argument type instance.
func shellCopyArgs(def *graphql.FieldDefinition) graphql.FieldConfigArgument {
	if def == nil || len(def.Args) == 0 {
		return nil
	}
	out := make(graphql.FieldConfigArgument, len(def.Args))
	for _, a := range def.Args {
		out[a.Name()] = &graphql.ArgumentConfig{
			Type:         a.Type,
			DefaultValue: a.DefaultValue,
			Description:  a.Description(),
		}
	}
	return out
}

// shellFieldFrom transcribes an interface field from the concrete object that
// already carries it: same output type instance, same argument set. Returns nil
// when the concrete object or field is absent so a missing dependency surfaces
// as an omitted field rather than a nil-typed one.
func shellFieldFrom(obj *graphql.Object, name string) *graphql.Field {
	if obj == nil {
		return nil
	}
	def := obj.Fields()[name]
	if def == nil {
		return nil
	}
	return &graphql.Field{Type: def.Type, Args: shellCopyArgs(def)}
}

// shellCopyFields transcribes several interface fields from one concrete object.
func shellCopyFields(obj *graphql.Object, names ...string) graphql.Fields {
	fields := graphql.Fields{}
	for _, name := range names {
		if f := shellFieldFrom(obj, name); f != nil {
			fields[name] = f
		}
	}
	return fields
}

// shellNamedOutput reaches the named output type under a concrete field (the
// connection/object below its NonNull/List wrappers) so a shell can name the
// very same instance the concrete field returns.
func shellNamedOutput(obj *graphql.Object, field string) graphql.Type {
	if obj == nil {
		return nil
	}
	def := obj.Fields()[field]
	if def == nil {
		return nil
	}
	named, _ := graphql.GetNamed(def.Type).(graphql.Type)
	return named
}

// shellArgType reaches the input type of one argument on a concrete field, so a
// shell can name the same input instance (used for DiscussionOrder, which is
// minted inline on Repository.discussions rather than memoized).
func shellArgType(obj *graphql.Object, field, arg string) graphql.Input {
	if obj == nil {
		return nil
	}
	def := obj.Fields()[field]
	if def == nil {
		return nil
	}
	for _, a := range def.Args {
		if a.Name() == arg {
			return a.Type
		}
	}
	return nil
}

func (s *Resolver) addInterfaceShells() {
	dateTime := s.graphQLStringScalar("DateTime")

	comment := s.graphqlTypes.issueComment
	org := s.graphqlTypes.organization
	user := s.graphqlTypes.user
	team := s.graphqlTypes.team
	issue := s.graphqlTypes.issue
	repo := s.graphqlTypes.repository
	issueFieldDate := unionMemberObject(s.graphqlTypes.issueFieldsUnion, "IssueFieldDate")

	// --- Agentic -----------------------------------------------------------
	// Four nullable String channel fields; no named-type dependency.
	agentic := s.mutationInterface("Agentic", func() graphql.Fields {
		return graphql.Fields{
			"viewerCopilotAgentCreatesChannel":     gqlField(graphql.String),
			"viewerCopilotAgentLogUpdatesChannel":  gqlField(graphql.String),
			"viewerCopilotAgentTaskUpdatesChannel": gqlField(graphql.String),
			"viewerCopilotAgentUpdatesChannel":     gqlField(graphql.String),
		}
	}, nilResolveType)

	// --- Comment -----------------------------------------------------------
	comment16 := []string{
		"author", "authorAssociation", "body", "bodyHTML", "bodyText",
		"createdAt", "createdViaEmail", "editor", "id", "includesCreatedEdit",
		"lastEditedAt", "publishedAt", "updatedAt", "userContentEdits",
		"viewerDidAuthor",
	}
	commentIface := s.mutationInterface("Comment", func() graphql.Fields {
		return shellCopyFields(comment, comment16...)
	}, nilResolveType)

	// --- Deletable / Updatable / UpdatableComment --------------------------
	deletable := s.mutationInterface("Deletable", func() graphql.Fields {
		return graphql.Fields{"viewerCanDelete": gqlNonNull(graphql.Boolean)}
	}, nilResolveType)

	updatable := s.mutationInterface("Updatable", func() graphql.Fields {
		return graphql.Fields{"viewerCanUpdate": gqlNonNull(graphql.Boolean)}
	}, nilResolveType)

	updatableComment := s.mutationInterface("UpdatableComment", func() graphql.Fields {
		return shellCopyFields(comment, "viewerCannotUpdateReasons")
	}, nilResolveType)

	// --- Pinnable ----------------------------------------------------------
	pinnable := s.mutationInterface("Pinnable", func() graphql.Fields {
		return graphql.Fields{
			"isPinned":       gqlField(graphql.Boolean),
			"pinnedAt":       gqlField(dateTime),
			"pinnedBy":       gqlField(user),
			"viewerCanPin":   gqlNonNull(graphql.Boolean),
			"viewerCanUnpin": gqlNonNull(graphql.Boolean),
		}
	}, nilResolveType)

	// --- IssueFieldCommon / IssueFieldValueCommon --------------------------
	issueFieldCommon := s.mutationInterface("IssueFieldCommon", func() graphql.Fields {
		return shellCopyFields(issueFieldDate,
			"createdAt", "dataType", "description", "fullDatabaseId", "name", "visibility")
	}, nilResolveType)

	issueFieldValueCommon := s.mutationInterface("IssueFieldValueCommon", func() graphql.Fields {
		return graphql.Fields{"field": gqlField(s.graphqlTypes.issueFieldsUnion)}
	}, nilResolveType)

	// --- MemberStatusable --------------------------------------------------
	// Team carries the full memberStatuses signature (pagination + orderBy:
	// UserStatusOrder), returning UserStatusConnection!.
	memberStatusable := s.mutationInterface("MemberStatusable", func() graphql.Fields {
		return shellCopyFields(team, "memberStatuses")
	}, nilResolveType)

	// --- PackageOwner ------------------------------------------------------
	// packages carries GitHub's eight arguments; the concrete Organization
	// field exposes only the four window arguments, so the remaining four are
	// reconstructed against the reused enum/input instances.
	packageOwner := s.mutationInterface("PackageOwner", func() graphql.Fields {
		packageConn := shellNamedOutput(org, "packages")
		orderDirection := s.sharedEnum("OrderDirection", "ASC", "DESC")
		packageOrderField := s.sharedEnum("PackageOrderField", "CREATED_AT")
		packageOrder := s.mutationInput("PackageOrder", graphql.InputObjectConfigFieldMap{
			"direction": gqlInputOf(orderDirection),
			"field":     gqlInputOf(packageOrderField),
		})
		packageType := s.sharedEnum("PackageType",
			"DEBIAN", "DOCKER", "MAVEN", "NPM", "NUGET", "PYPI", "RUBYGEMS")
		args := paginationArgs()
		args["names"] = &graphql.ArgumentConfig{Type: graphql.NewList(graphql.String)}
		args["orderBy"] = &graphql.ArgumentConfig{
			Type:         packageOrder,
			DefaultValue: map[string]interface{}{"field": "CREATED_AT", "direction": "DESC"},
		}
		args["packageType"] = &graphql.ArgumentConfig{Type: packageType}
		args["repositoryId"] = &graphql.ArgumentConfig{Type: graphql.ID}
		return graphql.Fields{
			"id":       gqlNonNull(graphql.ID),
			"packages": &graphql.Field{Type: graphql.NewNonNull(packageConn), Args: args},
		}
	}, nilResolveType)

	// --- ProfileOwner ------------------------------------------------------
	// Organization carries every ProfileOwner field with GitHub's exact
	// signatures (email nullable, the pinnable-item windows and their
	// PinnableItemType filters).
	profileOwner := s.mutationInterface("ProfileOwner", func() graphql.Fields {
		return shellCopyFields(org,
			"anyPinnableItems", "email", "id", "itemShowcase", "location",
			"login", "name", "pinnableItems", "pinnedItems",
			"pinnedItemsRemaining", "viewerCanChangePinnedItems", "websiteUrl")
	}, nilResolveType)

	// --- ProjectV2Recent ---------------------------------------------------
	projectV2Recent := s.mutationInterface("ProjectV2Recent", func() graphql.Fields {
		return shellCopyFields(org, "recentProjects")
	}, nilResolveType)

	// --- RepositoryDiscussionAuthor ---------------------------------------
	// repositoryDiscussions carries eight arguments; the concrete field omits
	// orderBy (DiscussionOrder) and states ([DiscussionState!]), reconstructed
	// against the reused instances (DiscussionOrder is minted inline on
	// Repository.discussions, so it is reached from there).
	repositoryDiscussionAuthor := s.mutationInterface("RepositoryDiscussionAuthor", func() graphql.Fields {
		discussionConn := s.namedObject("DiscussionConnection")
		discussionState := s.sharedEnum("DiscussionState", "CLOSED", "OPEN")
		args := paginationArgs()
		args["answered"] = &graphql.ArgumentConfig{Type: graphql.Boolean}
		if disOrder := shellArgType(repo, "discussions", "orderBy"); disOrder != nil {
			args["orderBy"] = &graphql.ArgumentConfig{
				Type:         disOrder,
				DefaultValue: map[string]interface{}{"field": "CREATED_AT", "direction": "DESC"},
			}
		}
		args["repositoryId"] = &graphql.ArgumentConfig{Type: graphql.ID}
		args["states"] = &graphql.ArgumentConfig{
			Type:         graphql.NewList(graphql.NewNonNull(discussionState)),
			DefaultValue: []interface{}{},
		}
		return graphql.Fields{
			"repositoryDiscussions": &graphql.Field{Type: graphql.NewNonNull(discussionConn), Args: args},
		}
	}, nilResolveType)

	// --- RepositoryDiscussionCommentAuthor --------------------------------
	// The concrete Organization field already matches GitHub's six-argument
	// signature exactly.
	repositoryDiscussionCommentAuthor := s.mutationInterface("RepositoryDiscussionCommentAuthor", func() graphql.Fields {
		return shellCopyFields(org, "repositoryDiscussionComments")
	}, nilResolveType)

	// --- SubscribableThread ------------------------------------------------
	subscribableThread := s.mutationInterface("SubscribableThread", func() graphql.Fields {
		fields := graphql.Fields{"id": gqlNonNull(graphql.ID)}
		for _, name := range []string{"viewerThreadSubscriptionFormAction", "viewerThreadSubscriptionStatus"} {
			if f := shellFieldFrom(issue, name); f != nil {
				fields[name] = f
			}
		}
		return fields
	}, nilResolveType)

	// --- TeamReviewRequestable --------------------------------------------
	// name is String! on this interface (Team's own name is nullable), so it
	// is built directly rather than copied.
	teamReviewRequestable := s.mutationInterface("TeamReviewRequestable", func() graphql.Fields {
		return graphql.Fields{
			"id":   gqlNonNull(graphql.ID),
			"name": gqlNonNull(graphql.String),
			"slug": gqlNonNull(graphql.String),
		}
	}, nilResolveType)

	// --- OrganizationOrUser ------------------------------------------------
	organizationOrUser := s.mutationUnion("OrganizationOrUser", func() []*graphql.Object {
		return []*graphql.Object{org, user}
	}, nilResolveType)

	// ProjectV2IssueFieldValues is deliberately not built here: the misc shell
	// cluster (gh_shells_misc_graphql.go) already mints and registers a union
	// of that name over the same five IssueField*Value members. Minting a
	// second instance would make schema construction reject the duplicate name.

	s.registerExtraSchemaType(
		agentic,
		commentIface,
		deletable,
		issueFieldCommon,
		issueFieldValueCommon,
		memberStatusable,
		packageOwner,
		pinnable,
		profileOwner,
		projectV2Recent,
		repositoryDiscussionAuthor,
		repositoryDiscussionCommentAuthor,
		subscribableThread,
		teamReviewRequestable,
		updatable,
		updatableComment,
		organizationOrUser,
	)
}
