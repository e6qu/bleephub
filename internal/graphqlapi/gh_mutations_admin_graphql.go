package graphqlapi

import (
	"fmt"
	"strings"

	"github.com/e6qu/bleephub/internal/store"
	"github.com/graphql-go/graphql"
)

// updateTeamsRepository and deletePackageVersion: administrative writes whose
// subjects live outside a repository row — a team grant, a package version —
// but whose authorization is still the plain question of who may administer
// the thing's owner.

func init() {
	for name, rule := range map[string]mutationRule{
		"updateTeamsRepository":   repoRule{scope: store.ScopeAdministration, level: mutationAdminRepo, target: mutationTargetRepo("repositoryId")},
		"deletePackageVersion":    packageOwnerRule{key: "packageVersionId"},
		"cloneTemplateRepository": repoRule{scope: store.ScopeContents, level: mutationReadRepo, target: mutationTargetRepo("repositoryId")},
	} {
		if _, exists := graphqlMutationAuthz[name]; exists {
			panic(fmt.Sprintf("graphql mutation %q already has a policy row", name))
		}
		graphqlMutationAuthz[name] = rule
	}
}

// packageOwnerRule authorizes a package-version mutation against the account
// that owns the package: deleting a version is package administration, which
// on github belongs to the owner (or an org's admins), not to anyone who can
// read the package.
type packageOwnerRule struct {
	key string
}

func (r packageOwnerRule) check() error {
	if r.key == "" {
		return fmt.Errorf("no package version input key")
	}
	return nil
}

func (r packageOwnerRule) authorize(s *Resolver, p graphql.ResolveParams, input map[string]interface{}) error {
	nodeID, _ := input[r.key].(string)
	_, pkg := store.FindPackageVersionByNodeID(s.store, nodeID)
	if pkg == nil {
		return gqlMissingNode("PackageVersion", nodeID)
	}
	// OwnerKey is a login, or owner/repo for a repository-scoped package;
	// either way the accountable account is its first segment.
	owner := pkg.OwnerKey
	if slash := strings.IndexByte(owner, '/'); slash > 0 {
		owner = owner[:slash]
	}
	if !s.viewerCanAdminAccount(p.Context, owner) {
		// The same answer as a missing version, so the mutation is not an
		// existence oracle for another account's packages.
		return gqlMissingNode("PackageVersion", nodeID)
	}
	return nil
}

func (s *Resolver) addAdminMutationsToSchema(mutationType *graphql.Object) {
	repositoryPermission := s.sharedEnum("RepositoryPermission", "ADMIN", "MAINTAIN", "READ", "TRIAGE", "WRITE")

	updateTeamsInput := graphql.NewInputObject(graphql.InputObjectConfig{
		Name: "UpdateTeamsRepositoryInput",
		Fields: graphql.InputObjectConfigFieldMap{
			"clientMutationId": &graphql.InputObjectFieldConfig{Type: graphql.String},
			"permission":       &graphql.InputObjectFieldConfig{Type: graphql.NewNonNull(repositoryPermission)},
			"repositoryId":     &graphql.InputObjectFieldConfig{Type: graphql.NewNonNull(graphql.ID)},
			"teamIds":          &graphql.InputObjectFieldConfig{Type: graphql.NewNonNull(graphql.NewList(graphql.NewNonNull(graphql.ID)))},
		},
	})
	updateTeamsPayload := graphql.NewObject(graphql.ObjectConfig{
		Name: "UpdateTeamsRepositoryPayload",
		Fields: graphql.Fields{
			"clientMutationId": &graphql.Field{Type: graphql.String},
			"repository":       &graphql.Field{Type: s.graphqlTypes.repository},
			"teams":            &graphql.Field{Type: graphql.NewList(graphql.NewNonNull(s.graphqlTypes.team))},
		},
	})
	s.registerMutation(mutationType, "updateTeamsRepository", &graphql.Field{
		Type: updateTeamsPayload,
		Args: graphql.FieldConfigArgument{
			"input": &graphql.ArgumentConfig{Type: graphql.NewNonNull(updateTeamsInput)},
		},
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			input, _ := p.Args["input"].(map[string]interface{})
			repo := store.FindRepoByNodeID(s.store, str(input["repositoryId"]))
			if repo == nil {
				return nil, gqlMissingNode("Repository", str(input["repositoryId"]))
			}
			// github's five-value permission vocabulary maps onto the team
			// grant levels the REST team-repository surface stores; MAINTAIN
			// and TRIAGE fold to their nearest grant the way the REST
			// permission parameter does.
			var perm store.TeamPermission
			switch str(input["permission"]) {
			case "ADMIN":
				perm = store.TeamPermissionAdmin
			case "WRITE", "MAINTAIN":
				perm = store.TeamPermissionPush
			default:
				perm = store.TeamPermissionPull
			}
			rawIDs, _ := input["teamIds"].([]interface{})
			teams := make([]interface{}, 0, len(rawIDs))
			for _, raw := range rawIDs {
				nodeID, _ := raw.(string)
				team, org := store.FindTeamByNodeID(s.store, nodeID)
				if team == nil || org == nil {
					return nil, gqlMissingNode("Team", nodeID)
				}
				if !strings.HasPrefix(repo.FullName, org.Login+"/") {
					return nil, fmt.Errorf("team %s belongs to a different organization than the repository", team.Slug)
				}
				if !s.store.SetTeamRepoPermission(org.Login, team.Slug, repo.FullName, perm) {
					return nil, fmt.Errorf("the team grant could not be recorded")
				}
				teams = append(teams, s.teamSource(team, org))
			}
			return map[string]interface{}{
				"clientMutationId": input["clientMutationId"],
				"repository":       optionalObject(repoToGraphQL(s.store, s.store.GetRepoByID(repo.ID))),
				"teams":            teams,
			}, nil
		},
	})

	deleteVersionInput := graphql.NewInputObject(graphql.InputObjectConfig{
		Name: "DeletePackageVersionInput",
		Fields: graphql.InputObjectConfigFieldMap{
			"clientMutationId": &graphql.InputObjectFieldConfig{Type: graphql.String},
			"packageVersionId": &graphql.InputObjectFieldConfig{Type: graphql.NewNonNull(graphql.ID)},
		},
	})
	deleteVersionPayload := graphql.NewObject(graphql.ObjectConfig{
		Name: "DeletePackageVersionPayload",
		Fields: graphql.Fields{
			"clientMutationId": &graphql.Field{Type: graphql.String},
			"success":          &graphql.Field{Type: graphql.Boolean},
		},
	})
	cloneTemplateInput := graphql.NewInputObject(graphql.InputObjectConfig{
		Name: "CloneTemplateRepositoryInput",
		Fields: graphql.InputObjectConfigFieldMap{
			"clientMutationId":   &graphql.InputObjectFieldConfig{Type: graphql.String},
			"description":        &graphql.InputObjectFieldConfig{Type: graphql.String},
			"includeAllBranches": &graphql.InputObjectFieldConfig{Type: graphql.Boolean, DefaultValue: false},
			"name":               &graphql.InputObjectFieldConfig{Type: graphql.NewNonNull(graphql.String)},
			"ownerId":            &graphql.InputObjectFieldConfig{Type: graphql.NewNonNull(graphql.ID)},
			"repositoryId":       &graphql.InputObjectFieldConfig{Type: graphql.NewNonNull(graphql.ID)},
			"visibility":         &graphql.InputObjectFieldConfig{Type: graphql.NewNonNull(s.sharedEnum("RepositoryVisibility", "INTERNAL", "PRIVATE", "PUBLIC"))},
		},
	})
	cloneTemplatePayload := graphql.NewObject(graphql.ObjectConfig{
		Name: "CloneTemplateRepositoryPayload",
		Fields: graphql.Fields{
			"clientMutationId": &graphql.Field{Type: graphql.String},
			"repository":       &graphql.Field{Type: s.graphqlTypes.repository},
		},
	})
	s.registerMutation(mutationType, "cloneTemplateRepository", &graphql.Field{
		Type: cloneTemplatePayload,
		Args: graphql.FieldConfigArgument{
			"input": &graphql.ArgumentConfig{Type: graphql.NewNonNull(cloneTemplateInput)},
		},
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			user := s.ghUserFromContext(p.Context)
			input, _ := p.Args["input"].(map[string]interface{})
			template := store.FindRepoByNodeID(s.store, str(input["repositoryId"]))
			if template == nil {
				return nil, gqlMissingNode("Repository", str(input["repositoryId"]))
			}
			if !template.IsTemplate {
				return nil, fmt.Errorf("repository is not a template")
			}
			ownerID, ownerType, ok := resolveProjectOwner(s.store, str(input["ownerId"]))
			if !ok {
				return nil, gqlMissingNode("RepositoryOwner", str(input["ownerId"]))
			}
			ownerLogin := ""
			if ownerType == "Organization" {
				if org := s.store.GetOrgByID(ownerID); org != nil {
					ownerLogin = org.Login
				}
			} else if owner := s.store.GetUserByID(ownerID); owner != nil {
				ownerLogin = owner.Login
				// Generating under a different user's personal account is not
				// a thing github permits; only the caller's own account or an
				// organization qualifies.
				if user == nil || owner.ID != user.ID {
					return nil, fmt.Errorf("you may only generate repositories for your own account or an organization you are a member of")
				}
			}
			if ownerLogin == "" {
				return nil, gqlMissingNode("RepositoryOwner", str(input["ownerId"]))
			}
			includeAll, _ := input["includeAllBranches"].(bool)
			private := str(input["visibility"]) != "PUBLIC"
			repo, err := s.repos.GenerateFromTemplate(p.Context, template, user, ownerLogin,
				str(input["name"]), str(input["description"]), includeAll, private)
			if err != nil {
				return nil, err
			}
			return map[string]interface{}{
				"clientMutationId": input["clientMutationId"],
				"repository":       optionalObject(repoToGraphQL(s.store, repo)),
			}, nil
		},
	})

	s.registerMutation(mutationType, "deletePackageVersion", &graphql.Field{
		Type: deleteVersionPayload,
		Args: graphql.FieldConfigArgument{
			"input": &graphql.ArgumentConfig{Type: graphql.NewNonNull(deleteVersionInput)},
		},
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			input, _ := p.Args["input"].(map[string]interface{})
			nodeID := str(input["packageVersionId"])
			version, pkg := store.FindPackageVersionByNodeID(s.store, nodeID)
			if version == nil || pkg == nil {
				return nil, gqlMissingNode("PackageVersion", nodeID)
			}
			if !s.store.DeletePackageVersion(version.ID) {
				return nil, fmt.Errorf("the package version could not be deleted")
			}
			return map[string]interface{}{
				"clientMutationId": input["clientMutationId"],
				"success":          true,
			}, nil
		},
	})
}
