package graphqlapi

// Projects classic (v1) — the write surface for classic boards: project
// lifecycle (create/update/delete/clone/import), columns, cards (including
// note-to-issue conversion) and repository links.
//
// Each mutation carries a graphqlMutationAuthz row: the repository's Projects
// permission at write for a repo-scoped board, or membership of the owning
// account for an account-owned one — as the classic REST handlers enforce.

import (
	"fmt"
	"sort"

	"github.com/e6qu/bleephub/internal/store"
	"github.com/graphql-go/graphql"
)

// Authorization

// projectClassicTarget is what a classic-project mutation acts on: the repo
// behind a repo-scoped board, or the owning account of an account-owned one.
// Exactly one side is populated.
type projectClassicTarget struct {
	repo       *store.Repo
	ownerType  string // "User" or "Organization" when account-scoped
	ownerLogin string
	project    *store.ProjectClassic
	// missing answers "no such node" and "you may not see it" indistinguishably.
	missing error
}

// projectClassicRule is the policy for a mutation on a classic board (or
// something inside one): the repository's Projects grant at write with push
// standing for a repo-scoped board, or the owning account's membership plus its
// Projects grant for an account-owned one.
type projectClassicRule struct {
	target func(s *Resolver, input map[string]interface{}) projectClassicTarget
}

func (r projectClassicRule) check() error {
	if r.target == nil {
		return fmt.Errorf("no classic-project target lookup")
	}
	return nil
}

func (r projectClassicRule) authorize(s *Resolver, p graphql.ResolveParams, input map[string]interface{}) error {
	target := r.target(s, input)
	if target.repo != nil {
		if !s.viewerCanReadRepo(p.Context, target.repo) {
			return target.missing
		}
		if !s.credentialGrantsRepo(p.Context, target.repo, store.ScopeProjects, store.PermWrite) {
			return &ghForbiddenError{message: "resource not accessible by integration"}
		}
		if !s.principalHoldsRepoCapability(p.Context, target.repo, store.PermWrite) {
			return &ghForbiddenError{message: "must have push access to Repository"}
		}
		return nil
	}
	if target.ownerLogin == "" {
		return target.missing
	}
	viewer := s.ghUserFromContext(p.Context)
	if target.ownerType == "Organization" {
		belongs := s.viewerIsOrgMember(p.Context, target.ownerLogin) || s.viewerCanAdminAccount(p.Context, target.ownerLogin)
		// A private board under an account the viewer does not belong to is
		// answered as absent, not forbidden — "forbidden" would confirm it exists.
		if target.project != nil && !target.project.Public && !belongs {
			return target.missing
		}
		if !belongs {
			return &ghForbiddenError{message: "must be a member of the organization to modify its projects"}
		}
		if !s.credentialGrantsAccount(p.Context, store.OrganizationAccount, target.ownerLogin, store.ScopeProjects, store.PermWrite) {
			return &ghForbiddenError{message: "resource not accessible by integration"}
		}
		return nil
	}
	isOwner := viewer != nil && viewer.Login == target.ownerLogin
	if target.project != nil && !target.project.Public && !isOwner {
		return target.missing
	}
	if !isOwner {
		return &ghForbiddenError{message: "must be the project owner to modify it"}
	}
	if !s.credentialGrantsAccount(p.Context, store.AnyAccount, target.ownerLogin, store.ScopeProjects, store.PermWrite) {
		return &ghForbiddenError{message: "resource not accessible by integration"}
	}
	return nil
}

// projectClassicTargetOwner resolves a createProject ownerId (a Repository,
// Organization or User global id) to the board's would-be owner.
func projectClassicTargetOwner(key string) func(*Resolver, map[string]interface{}) projectClassicTarget {
	return func(s *Resolver, input map[string]interface{}) projectClassicTarget {
		nodeID, _ := input[key].(string)
		target := projectClassicTarget{missing: gqlMissingNode("ProjectOwner", nodeID)}
		if repo := store.FindRepoByNodeID(s.store, nodeID); repo != nil {
			target.repo = repo
			return target
		}
		if org := s.orgByNodeID(nodeID); org != nil {
			target.ownerType, target.ownerLogin = "Organization", org.Login
			return target
		}
		if user := store.FindUserByNodeID(s.store, nodeID); user != nil {
			target.ownerType, target.ownerLogin = "User", user.Login
			return target
		}
		return target
	}
}

// projectClassicTargetOwnerName is projectClassicTargetOwner for importProject,
// whose input names the owner by login.
func projectClassicTargetOwnerName(key string) func(*Resolver, map[string]interface{}) projectClassicTarget {
	return func(s *Resolver, input map[string]interface{}) projectClassicTarget {
		login, _ := input[key].(string)
		target := projectClassicTarget{missing: &ghNotFoundError{
			message: fmt.Sprintf("Could not resolve to an owner with the login of '%s'.", login),
		}}
		if login == "" {
			return target
		}
		if org := s.store.GetOrg(login); org != nil {
			target.ownerType, target.ownerLogin = "Organization", org.Login
			return target
		}
		if user := s.store.LookupUserByLogin(login); user != nil {
			target.ownerType, target.ownerLogin = "User", user.Login
			return target
		}
		return target
	}
}

// projectClassicTargetFrom walks from a project row to the repository or account
// whose standing decides the write.
func (s *Resolver) projectClassicTargetFrom(project *store.ProjectClassic, missing error) projectClassicTarget {
	target := projectClassicTarget{missing: missing}
	if project == nil {
		return target
	}
	target.project = project
	if project.RepoKey != "" {
		target.repo = s.store.GetRepoByFullName(project.RepoKey)
		return target
	}
	target.ownerType, target.ownerLogin = project.OwnerType, project.OwnerLogin
	return target
}

func projectClassicTargetProject(key string) func(*Resolver, map[string]interface{}) projectClassicTarget {
	return func(s *Resolver, input map[string]interface{}) projectClassicTarget {
		nodeID, _ := input[key].(string)
		missing := gqlMissingNode("Project", nodeID)
		return s.projectClassicTargetFrom(store.FindProjectClassicByNodeID(s.store, nodeID), missing)
	}
}

func projectClassicTargetColumn(key string) func(*Resolver, map[string]interface{}) projectClassicTarget {
	return func(s *Resolver, input map[string]interface{}) projectClassicTarget {
		nodeID, _ := input[key].(string)
		missing := gqlMissingNode("ProjectColumn", nodeID)
		column := store.FindProjectColumnByNodeID(s.store, nodeID)
		if column == nil {
			return projectClassicTarget{missing: missing}
		}
		return s.projectClassicTargetFrom(s.store.GetProjectClassic(column.ProjectID), missing)
	}
}

func projectClassicTargetCard(key string) func(*Resolver, map[string]interface{}) projectClassicTarget {
	return func(s *Resolver, input map[string]interface{}) projectClassicTarget {
		nodeID, _ := input[key].(string)
		missing := gqlMissingNode("ProjectCard", nodeID)
		card := store.FindProjectCardByNodeID(s.store, nodeID)
		if card == nil {
			return projectClassicTarget{missing: missing}
		}
		column := s.store.GetProjectColumn(card.ColumnID)
		if column == nil {
			return projectClassicTarget{missing: missing}
		}
		return s.projectClassicTargetFrom(s.store.GetProjectClassic(column.ProjectID), missing)
	}
}

// projectClassicCloneRule is the policy for cloneProject: read on the source
// board and write on the destination owner, each enforced independently.
type projectClassicCloneRule struct{}

func (projectClassicCloneRule) check() error { return nil }

func (projectClassicCloneRule) authorize(s *Resolver, p graphql.ResolveParams, input map[string]interface{}) error {
	nodeID, _ := input["sourceId"].(string)
	source := store.FindProjectClassicByNodeID(s.store, nodeID)
	if source == nil || !s.canReadProjectClassic(p.Context, source) {
		return gqlMissingNode("Project", nodeID)
	}
	destination := projectClassicRule{target: projectClassicTargetOwner("targetOwnerId")}
	return destination.authorize(s, p, input)
}

// projectClassicConvertRule is the policy for convertProjectCardNoteToIssue:
// write on the card's board, plus the standing createIssue demands on the
// target repository.
type projectClassicConvertRule struct{}

func (projectClassicConvertRule) check() error { return nil }

func (projectClassicConvertRule) authorize(s *Resolver, p graphql.ResolveParams, input map[string]interface{}) error {
	board := projectClassicRule{target: projectClassicTargetCard("projectCardId")}
	if err := board.authorize(s, p, input); err != nil {
		return err
	}
	issueSide := repoRule{scope: store.ScopeIssues, level: mutationReadRepo, target: mutationTargetRepo("repositoryId")}
	return issueSide.authorize(s, p, input)
}

// projectClassicLinkRule is the policy for link/unlinkRepositoryToProject:
// write on the board, and the repository visible to the caller (else absent).
type projectClassicLinkRule struct{}

func (projectClassicLinkRule) check() error { return nil }

func (projectClassicLinkRule) authorize(s *Resolver, p graphql.ResolveParams, input map[string]interface{}) error {
	board := projectClassicRule{target: projectClassicTargetProject("projectId")}
	if err := board.authorize(s, p, input); err != nil {
		return err
	}
	nodeID, _ := input["repositoryId"].(string)
	repo := store.FindRepoByNodeID(s.store, nodeID)
	if repo == nil || !s.viewerCanReadRepo(p.Context, repo) {
		return gqlMissingNode("Repository", nodeID)
	}
	return nil
}

func init() {
	for name, rule := range map[string]mutationRule{
		"createProject": projectClassicRule{target: projectClassicTargetOwner("ownerId")},
		"updateProject": projectClassicRule{target: projectClassicTargetProject("projectId")},
		"deleteProject": projectClassicRule{target: projectClassicTargetProject("projectId")},
		"cloneProject":  projectClassicCloneRule{},
		"importProject": projectClassicRule{target: projectClassicTargetOwnerName("ownerName")},

		"addProjectColumn":    projectClassicRule{target: projectClassicTargetProject("projectId")},
		"updateProjectColumn": projectClassicRule{target: projectClassicTargetColumn("projectColumnId")},
		"deleteProjectColumn": projectClassicRule{target: projectClassicTargetColumn("columnId")},
		"moveProjectColumn":   projectClassicRule{target: projectClassicTargetColumn("columnId")},

		"addProjectCard":                projectClassicRule{target: projectClassicTargetColumn("projectColumnId")},
		"updateProjectCard":             projectClassicRule{target: projectClassicTargetCard("projectCardId")},
		"deleteProjectCard":             projectClassicRule{target: projectClassicTargetCard("cardId")},
		"moveProjectCard":               projectClassicRule{target: projectClassicTargetCard("cardId")},
		"convertProjectCardNoteToIssue": projectClassicConvertRule{},

		"linkRepositoryToProject":     projectClassicLinkRule{},
		"unlinkRepositoryFromProject": projectClassicLinkRule{},
	} {
		if _, exists := graphqlMutationAuthz[name]; exists {
			panic(fmt.Sprintf("graphql mutation %q already has a policy row", name))
		}
		graphqlMutationAuthz[name] = rule
	}
}

// Resolver helpers

// projectClassicByNodeID answers a detached snapshot of the named board (the
// policy row already authorized the caller), or GitHub's not-found error.
func (s *Resolver) projectClassicByNodeID(nodeID string) (*store.ProjectClassic, error) {
	live := store.FindProjectClassicByNodeID(s.store, nodeID)
	if live == nil {
		return nil, gqlMissingNode("Project", nodeID)
	}
	return s.store.GetProjectClassic(live.ID), nil
}

func (s *Resolver) projectClassicColumnByNodeID(nodeID string) (*store.ProjectColumn, error) {
	live := store.FindProjectColumnByNodeID(s.store, nodeID)
	if live == nil {
		return nil, gqlMissingNode("ProjectColumn", nodeID)
	}
	return s.store.GetProjectColumn(live.ID), nil
}

func (s *Resolver) projectClassicCardByNodeID(nodeID string) (*store.ProjectCard, error) {
	live := store.FindProjectCardByNodeID(s.store, nodeID)
	if live == nil {
		return nil, gqlMissingNode("ProjectCard", nodeID)
	}
	return s.store.GetProjectCard(live.ID), nil
}

// projectClassicAdmitsRepo reports whether content from (or a link to) repo
// belongs on the board: the board's own repository, or the owning account's
// repositories and explicitly linked ones.
func projectClassicAdmitsRepo(p *store.ProjectClassic, repo *store.Repo) bool {
	if p.RepoKey != "" {
		return p.RepoKey == repo.FullName
	}
	if owner, _, ok := store.SplitRepoFullName(repo.FullName); ok && owner == p.OwnerLogin {
		return true
	}
	for _, id := range p.LinkedRepoIDs {
		if id == repo.ID {
			return true
		}
	}
	return false
}

// connectionEdgeForNode picks the edge for the given global id out of a rendered
// connection, so the payload's edge carries the cursor the connection would serve.
func connectionEdgeForNode(conn map[string]interface{}, nodeID string) interface{} {
	edges, _ := conn["edges"].([]map[string]interface{})
	for _, edge := range edges {
		if node, _ := edge["node"].(map[string]interface{}); node != nil && node["nodeID"] == nodeID {
			return edge
		}
	}
	return nil
}

func (s *Resolver) projectClassicColumnEdge(projectID int, columnNodeID string) interface{} {
	columns := s.store.ListProjectColumns(projectID)
	nodes := make([]map[string]interface{}, 0, len(columns))
	for _, c := range columns {
		nodes = append(nodes, projectClassicColumnToGQL(c))
	}
	return connectionEdgeForNode(paginateGQLMaps(nodes, nil), columnNodeID)
}

func (s *Resolver) projectClassicCardEdge(columnID int, cardNodeID string) interface{} {
	cards := s.store.ListProjectCards(columnID)
	nodes := make([]map[string]interface{}, 0, len(cards))
	for _, c := range cards {
		nodes = append(nodes, s.projectClassicCardToGQL(c))
	}
	return connectionEdgeForNode(paginateGQLMaps(nodes, nil), cardNodeID)
}

// projectClassicTemplateColumns is the column set each template seeds.
var projectClassicTemplateColumns = map[string][]string{
	"BASIC_KANBAN":             {"To do", "In progress", "Done"},
	"AUTOMATED_KANBAN_V2":      {"To do", "In progress", "Done"},
	"AUTOMATED_REVIEWS_KANBAN": {"To do", "In progress", "In review", "Done"},
	"BUG_TRIAGE":               {"Needs triage", "High priority", "Low priority", "Closed"},
}

// createProjectClassicForTarget mints the board under the authorized target's owner.
func (s *Resolver) createProjectClassicForTarget(target projectClassicTarget, creatorID int, name, body string, public bool) *store.ProjectClassic {
	if target.repo != nil {
		return s.store.CreateProjectClassic(target.repo, creatorID, name, body, "open")
	}
	return s.store.CreateProjectClassicForOwner(target.ownerType, target.ownerLogin, creatorID, name, body, public)
}

// Registration

func (s *Resolver) addProjectsClassicMutations(mutationType *graphql.Object) {
	s.addProjectClassicLifecycleMutations(mutationType)
	s.addProjectClassicColumnMutations(mutationType)
	s.addProjectClassicCardMutations(mutationType)
	s.addProjectClassicLinkMutations(mutationType)
}

func (s *Resolver) addProjectClassicLifecycleMutations(mutationType *graphql.Object) {
	projectType := s.projectClassicType()

	s.registerMutation(mutationType, "createProject", &graphql.Field{
		Type: s.mutationPayload("CreateProjectPayload", graphql.Fields{
			"project": gqlField(projectType),
		}),
		Args: graphql.FieldConfigArgument{"input": &graphql.ArgumentConfig{Type: graphql.NewNonNull(s.mutationInput("CreateProjectInput", graphql.InputObjectConfigFieldMap{
			"body":          gqlString(),
			"name":          gqlNonNullString(),
			"ownerId":       gqlNonNullID(),
			"repositoryIds": gqlListOf(graphql.ID),
			"template":      gqlInputOf(s.projectTemplateEnum()),
		}))}},
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			user := s.ghUserFromContext(p.Context)
			input, _ := p.Args["input"].(map[string]interface{})
			name, _ := input["name"].(string)
			body, _ := gqlInputString(input, "body")

			target := projectClassicTargetOwner("ownerId")(s, input)
			if target.repo == nil && target.ownerLogin == "" {
				return nil, target.missing
			}
			project := s.createProjectClassicForTarget(target, user.ID, name, body, false)
			if project == nil {
				return nil, fmt.Errorf("project creation failed")
			}
			if template, ok := gqlInputString(input, "template"); ok {
				for _, columnName := range projectClassicTemplateColumns[template] {
					s.store.CreateProjectColumn(project.ID, columnName)
				}
			}
			if repositoryIDs, ok := gqlInputStrings(input, "repositoryIds"); ok && len(repositoryIDs) > 0 {
				if project.RepoKey != "" {
					return nil, fmt.Errorf("repositoryIds may only be provided for a user- or organization-owned project")
				}
				for _, repoNodeID := range repositoryIDs {
					repo := store.FindRepoByNodeID(s.store, repoNodeID)
					if repo == nil || !s.viewerCanReadRepo(p.Context, repo) {
						return nil, gqlMissingNode("Repository", repoNodeID)
					}
					s.store.LinkRepoToProjectClassic(project.ID, repo.ID)
				}
			}
			return map[string]interface{}{
				"project": optionalObject(s.projectClassicToGQL(s.store.GetProjectClassic(project.ID))),
			}, nil
		},
	})

	s.registerMutation(mutationType, "updateProject", &graphql.Field{
		Type: s.mutationPayload("UpdateProjectPayload", graphql.Fields{
			"project": gqlField(projectType),
		}),
		Args: graphql.FieldConfigArgument{"input": &graphql.ArgumentConfig{Type: graphql.NewNonNull(s.mutationInput("UpdateProjectInput", graphql.InputObjectConfigFieldMap{
			"body":      gqlString(),
			"name":      gqlString(),
			"projectId": gqlNonNullID(),
			"public":    gqlBool(),
			"state":     gqlInputOf(s.projectStateEnum()),
		}))}},
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			input, _ := p.Args["input"].(map[string]interface{})
			nodeID, _ := input["projectId"].(string)
			project, err := s.projectClassicByNodeID(nodeID)
			if err != nil {
				return nil, err
			}
			var state *string
			if raw, ok := gqlInputString(input, "state"); ok {
				lowered := "open"
				if raw == "CLOSED" {
					lowered = "closed"
				}
				state = &lowered
			}
			updated := s.store.UpdateProjectClassic(project,
				optionalString(input, "name"),
				optionalString(input, "body"),
				state,
				optionalBool(input, "public"))
			if updated == nil {
				return nil, gqlMissingNode("Project", nodeID)
			}
			return map[string]interface{}{
				"project": optionalObject(s.projectClassicToGQL(updated)),
			}, nil
		},
	})

	s.registerMutation(mutationType, "deleteProject", &graphql.Field{
		Type: s.mutationPayload("DeleteProjectPayload", graphql.Fields{
			"owner": gqlField(s.projectOwnerInterfaceType()),
		}),
		Args: graphql.FieldConfigArgument{"input": &graphql.ArgumentConfig{Type: graphql.NewNonNull(s.mutationInput("DeleteProjectInput", graphql.InputObjectConfigFieldMap{
			"projectId": gqlNonNullID(),
		}))}},
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			input, _ := p.Args["input"].(map[string]interface{})
			nodeID, _ := input["projectId"].(string)
			project, err := s.projectClassicByNodeID(nodeID)
			if err != nil {
				return nil, err
			}
			// Render the owner before the delete removes the row.
			owner, _ := s.projectClassicOwnerSource(project)
			if !s.store.DeleteProjectClassic(project.ID) {
				return nil, gqlMissingNode("Project", nodeID)
			}
			return map[string]interface{}{
				"owner": optionalObject(owner),
			}, nil
		},
	})

	s.registerMutation(mutationType, "cloneProject", &graphql.Field{
		Type: s.mutationPayload("CloneProjectPayload", graphql.Fields{
			"jobStatusId": gqlField(graphql.String),
			"project":     gqlField(projectType),
		}),
		Args: graphql.FieldConfigArgument{"input": &graphql.ArgumentConfig{Type: graphql.NewNonNull(s.mutationInput("CloneProjectInput", graphql.InputObjectConfigFieldMap{
			"body":             gqlString(),
			"includeWorkflows": gqlNonNullBool(),
			"name":             gqlNonNullString(),
			"public":           gqlBool(),
			"sourceId":         gqlNonNullID(),
			"targetOwnerId":    gqlNonNullID(),
		}))}},
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			user := s.ghUserFromContext(p.Context)
			input, _ := p.Args["input"].(map[string]interface{})
			sourceNodeID, _ := input["sourceId"].(string)
			source, err := s.projectClassicByNodeID(sourceNodeID)
			if err != nil {
				return nil, err
			}
			target := projectClassicTargetOwner("targetOwnerId")(s, input)
			if target.repo == nil && target.ownerLogin == "" {
				return nil, target.missing
			}
			name, _ := input["name"].(string)
			body := source.Body
			if supplied, ok := gqlInputString(input, "body"); ok {
				body = supplied
			}
			public := false
			if supplied := optionalBool(input, "public"); supplied != nil {
				public = *supplied
			}
			clone := s.createProjectClassicForTarget(target, user.ID, name, body, public)
			if clone == nil {
				return nil, fmt.Errorf("project creation failed")
			}
			// Copy columns only; GitHub's clone copies the frame, not the cards,
			// and no classic workflows exist for includeWorkflows to copy.
			for _, column := range s.store.ListProjectColumns(source.ID) {
				s.store.CreateProjectColumn(clone.ID, column.Name)
			}
			return map[string]interface{}{
				"jobStatusId": nil,
				"project":     optionalObject(s.projectClassicToGQL(s.store.GetProjectClassic(clone.ID))),
			}, nil
		},
	})

	cardImportInput := s.mutationInput("ProjectCardImport", graphql.InputObjectConfigFieldMap{
		"number":     gqlNonNullInt(),
		"repository": gqlNonNullString(),
	})
	columnImportInput := s.mutationInput("ProjectColumnImport", graphql.InputObjectConfigFieldMap{
		"columnName": gqlNonNullString(),
		"issues":     gqlListOf(cardImportInput),
		"position":   gqlNonNullInt(),
	})
	s.registerMutation(mutationType, "importProject", &graphql.Field{
		Type: s.mutationPayload("ImportProjectPayload", graphql.Fields{
			"project": gqlField(projectType),
		}),
		Args: graphql.FieldConfigArgument{"input": &graphql.ArgumentConfig{Type: graphql.NewNonNull(s.mutationInput("ImportProjectInput", graphql.InputObjectConfigFieldMap{
			"body":          gqlString(),
			"columnImports": gqlNonNullListOf(columnImportInput),
			"name":          gqlNonNullString(),
			"ownerName":     gqlNonNullString(),
			"public":        &graphql.InputObjectFieldConfig{Type: graphql.Boolean, DefaultValue: false},
		}))}},
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			user := s.ghUserFromContext(p.Context)
			input, _ := p.Args["input"].(map[string]interface{})
			target := projectClassicTargetOwnerName("ownerName")(s, input)
			if target.ownerLogin == "" {
				return nil, target.missing
			}
			name, _ := input["name"].(string)
			body, _ := gqlInputString(input, "body")
			public := false
			if supplied := optionalBool(input, "public"); supplied != nil {
				public = *supplied
			}
			project := s.createProjectClassicForTarget(target, user.ID, name, body, public)
			if project == nil {
				return nil, fmt.Errorf("project creation failed")
			}
			imports := gqlInputObjects(input, "columnImports")
			sort.SliceStable(imports, func(i, j int) bool {
				a, _ := gqlInputInt(imports[i], "position")
				b, _ := gqlInputInt(imports[j], "position")
				return a < b
			})
			for _, columnImport := range imports {
				columnName, _ := columnImport["columnName"].(string)
				column := s.store.CreateProjectColumn(project.ID, columnName)
				for _, cardImport := range gqlInputObjects(columnImport, "issues") {
					number, _ := gqlInputInt(cardImport, "number")
					fullName, _ := cardImport["repository"].(string)
					owner, repoName, ok := store.SplitRepoFullName(fullName)
					if !ok {
						return nil, fmt.Errorf("repository %q is not an owner/name pair", fullName)
					}
					repo := s.store.GetRepo(owner, repoName)
					if repo == nil || !s.viewerCanReadRepo(p.Context, repo) {
						return nil, gqlMissingNode("Repository", fullName)
					}
					issueID, pullID := 0, 0
					if issue := s.store.GetIssueByNumber(repo.ID, number); issue != nil {
						issueID = issue.ID
					} else if pr := s.store.GetPullRequestByNumber(repo.ID, number); pr != nil {
						pullID = pr.ID
					} else {
						return nil, &ghNotFoundError{
							message: fmt.Sprintf("Could not resolve to an issue or pull request %s#%d.", fullName, number),
						}
					}
					s.store.CreateProjectCard(column.ID, user.ID, "", issueID, pullID)
				}
			}
			return map[string]interface{}{
				"project": optionalObject(s.projectClassicToGQL(s.store.GetProjectClassic(project.ID))),
			}, nil
		},
	})
}

func (s *Resolver) addProjectClassicColumnMutations(mutationType *graphql.Object) {
	projectType := s.projectClassicType()
	columnType := s.projectClassicColumnType()
	_, columnEdgeType := s.projectClassicConnectionPair("ProjectColumn", columnType)

	s.registerMutation(mutationType, "addProjectColumn", &graphql.Field{
		Type: s.mutationPayload("AddProjectColumnPayload", graphql.Fields{
			"columnEdge": gqlField(columnEdgeType),
			"project":    gqlField(projectType),
		}),
		Args: graphql.FieldConfigArgument{"input": &graphql.ArgumentConfig{Type: graphql.NewNonNull(s.mutationInput("AddProjectColumnInput", graphql.InputObjectConfigFieldMap{
			"name":      gqlNonNullString(),
			"projectId": gqlNonNullID(),
		}))}},
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			input, _ := p.Args["input"].(map[string]interface{})
			nodeID, _ := input["projectId"].(string)
			project, err := s.projectClassicByNodeID(nodeID)
			if err != nil {
				return nil, err
			}
			name, _ := input["name"].(string)
			if name == "" {
				return nil, fmt.Errorf("name can't be blank")
			}
			column := s.store.CreateProjectColumn(project.ID, name)
			return map[string]interface{}{
				"columnEdge": s.projectClassicColumnEdge(project.ID, column.NodeID),
				"project":    optionalObject(s.projectClassicToGQL(s.store.GetProjectClassic(project.ID))),
			}, nil
		},
	})

	s.registerMutation(mutationType, "updateProjectColumn", &graphql.Field{
		Type: s.mutationPayload("UpdateProjectColumnPayload", graphql.Fields{
			"projectColumn": gqlField(columnType),
		}),
		Args: graphql.FieldConfigArgument{"input": &graphql.ArgumentConfig{Type: graphql.NewNonNull(s.mutationInput("UpdateProjectColumnInput", graphql.InputObjectConfigFieldMap{
			"name":            gqlNonNullString(),
			"projectColumnId": gqlNonNullID(),
		}))}},
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			input, _ := p.Args["input"].(map[string]interface{})
			nodeID, _ := input["projectColumnId"].(string)
			column, err := s.projectClassicColumnByNodeID(nodeID)
			if err != nil {
				return nil, err
			}
			name, _ := input["name"].(string)
			if name == "" {
				return nil, fmt.Errorf("name can't be blank")
			}
			updated := s.store.UpdateProjectColumn(column, name)
			if updated == nil {
				return nil, gqlMissingNode("ProjectColumn", nodeID)
			}
			return map[string]interface{}{
				"projectColumn": optionalObject(projectClassicColumnToGQL(updated)),
			}, nil
		},
	})

	s.registerMutation(mutationType, "deleteProjectColumn", &graphql.Field{
		Type: s.mutationPayload("DeleteProjectColumnPayload", graphql.Fields{
			"deletedColumnId": gqlField(graphql.ID),
			"project":         gqlField(projectType),
		}),
		Args: graphql.FieldConfigArgument{"input": &graphql.ArgumentConfig{Type: graphql.NewNonNull(s.mutationInput("DeleteProjectColumnInput", graphql.InputObjectConfigFieldMap{
			"columnId": gqlNonNullID(),
		}))}},
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			input, _ := p.Args["input"].(map[string]interface{})
			nodeID, _ := input["columnId"].(string)
			column, err := s.projectClassicColumnByNodeID(nodeID)
			if err != nil {
				return nil, err
			}
			projectID := column.ProjectID
			if !s.store.DeleteProjectColumn(column.ID) {
				return nil, gqlMissingNode("ProjectColumn", nodeID)
			}
			return map[string]interface{}{
				"deletedColumnId": column.NodeID,
				"project":         optionalObject(s.projectClassicToGQL(s.store.GetProjectClassic(projectID))),
			}, nil
		},
	})

	s.registerMutation(mutationType, "moveProjectColumn", &graphql.Field{
		Type: s.mutationPayload("MoveProjectColumnPayload", graphql.Fields{
			"columnEdge": gqlField(columnEdgeType),
		}),
		Args: graphql.FieldConfigArgument{"input": &graphql.ArgumentConfig{Type: graphql.NewNonNull(s.mutationInput("MoveProjectColumnInput", graphql.InputObjectConfigFieldMap{
			"afterColumnId": gqlID(),
			"columnId":      gqlNonNullID(),
		}))}},
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			input, _ := p.Args["input"].(map[string]interface{})
			nodeID, _ := input["columnId"].(string)
			column, err := s.projectClassicColumnByNodeID(nodeID)
			if err != nil {
				return nil, err
			}
			// A null afterColumnId places the column at the front.
			position := "first"
			if afterNodeID, ok := gqlInputString(input, "afterColumnId"); ok && afterNodeID != "" {
				after, err := s.projectClassicColumnByNodeID(afterNodeID)
				if err != nil {
					return nil, err
				}
				if after.ProjectID != column.ProjectID {
					return nil, fmt.Errorf("afterColumnId names a column in a different project")
				}
				position = fmt.Sprintf("after:%d", after.ID)
			}
			if err := s.store.MoveProjectColumn(column, position); err != nil {
				return nil, err
			}
			return map[string]interface{}{
				"columnEdge": s.projectClassicColumnEdge(column.ProjectID, column.NodeID),
			}, nil
		},
	})
}

func (s *Resolver) addProjectClassicCardMutations(mutationType *graphql.Object) {
	columnType := s.projectClassicColumnType()
	cardType := s.projectClassicCardType()
	_, cardEdgeType := s.projectClassicConnectionPair("ProjectCard", cardType)

	s.registerMutation(mutationType, "addProjectCard", &graphql.Field{
		Type: s.mutationPayload("AddProjectCardPayload", graphql.Fields{
			"cardEdge":      gqlField(cardEdgeType),
			"projectColumn": gqlField(columnType),
		}),
		Args: graphql.FieldConfigArgument{"input": &graphql.ArgumentConfig{Type: graphql.NewNonNull(s.mutationInput("AddProjectCardInput", graphql.InputObjectConfigFieldMap{
			"contentId":       gqlID(),
			"note":            gqlString(),
			"projectColumnId": gqlNonNullID(),
		}))}},
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			user := s.ghUserFromContext(p.Context)
			input, _ := p.Args["input"].(map[string]interface{})
			nodeID, _ := input["projectColumnId"].(string)
			column, err := s.projectClassicColumnByNodeID(nodeID)
			if err != nil {
				return nil, err
			}
			project := s.store.GetProjectClassic(column.ProjectID)
			if project == nil {
				return nil, gqlMissingNode("ProjectColumn", nodeID)
			}
			note, hasNote := gqlInputString(input, "note")
			contentNodeID, hasContent := gqlInputString(input, "contentId")
			hasNote = hasNote && note != ""
			hasContent = hasContent && contentNodeID != ""
			if hasNote == hasContent {
				return nil, fmt.Errorf("exactly one of note and contentId must be provided")
			}
			issueID, pullID := 0, 0
			if hasContent {
				contentType, contentID, ok := resolveContentByNodeID(s.store, contentNodeID)
				if !ok || !s.viewerCanReadProjectContent(p.Context, contentType, contentID) {
					return nil, &ghNotFoundError{
						message: fmt.Sprintf("Could not resolve to an issue or pull request with the global id of '%s'.", contentNodeID),
					}
				}
				var contentRepoID int
				if contentType == "Issue" {
					issueID = contentID
					if issue := s.store.GetIssue(contentID); issue != nil {
						contentRepoID = issue.RepoID
					}
				} else {
					pullID = contentID
					if pr := s.store.GetPullRequest(contentID); pr != nil {
						contentRepoID = pr.RepoID
					}
				}
				repo := s.store.GetRepoByID(contentRepoID)
				if repo == nil || !projectClassicAdmitsRepo(project, repo) {
					return nil, fmt.Errorf("the content must belong to the project's repository or owner")
				}
			}
			card := s.store.CreateProjectCard(column.ID, user.ID, note, issueID, pullID)
			return map[string]interface{}{
				"cardEdge":      s.projectClassicCardEdge(column.ID, card.NodeID),
				"projectColumn": optionalObject(projectClassicColumnToGQL(s.store.GetProjectColumn(column.ID))),
			}, nil
		},
	})

	s.registerMutation(mutationType, "updateProjectCard", &graphql.Field{
		Type: s.mutationPayload("UpdateProjectCardPayload", graphql.Fields{
			"projectCard": gqlField(cardType),
		}),
		Args: graphql.FieldConfigArgument{"input": &graphql.ArgumentConfig{Type: graphql.NewNonNull(s.mutationInput("UpdateProjectCardInput", graphql.InputObjectConfigFieldMap{
			"isArchived":    gqlBool(),
			"note":          gqlString(),
			"projectCardId": gqlNonNullID(),
		}))}},
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			input, _ := p.Args["input"].(map[string]interface{})
			nodeID, _ := input["projectCardId"].(string)
			card, err := s.projectClassicCardByNodeID(nodeID)
			if err != nil {
				return nil, err
			}
			updated := s.store.UpdateProjectCard(card,
				optionalString(input, "note"),
				optionalBool(input, "isArchived"))
			if updated == nil {
				return nil, gqlMissingNode("ProjectCard", nodeID)
			}
			return map[string]interface{}{
				"projectCard": optionalObject(s.projectClassicCardToGQL(updated)),
			}, nil
		},
	})

	s.registerMutation(mutationType, "deleteProjectCard", &graphql.Field{
		Type: s.mutationPayload("DeleteProjectCardPayload", graphql.Fields{
			"column":        gqlField(columnType),
			"deletedCardId": gqlField(graphql.ID),
		}),
		Args: graphql.FieldConfigArgument{"input": &graphql.ArgumentConfig{Type: graphql.NewNonNull(s.mutationInput("DeleteProjectCardInput", graphql.InputObjectConfigFieldMap{
			"cardId": gqlNonNullID(),
		}))}},
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			input, _ := p.Args["input"].(map[string]interface{})
			nodeID, _ := input["cardId"].(string)
			card, err := s.projectClassicCardByNodeID(nodeID)
			if err != nil {
				return nil, err
			}
			columnID := card.ColumnID
			if !s.store.DeleteProjectCard(card.ID) {
				return nil, gqlMissingNode("ProjectCard", nodeID)
			}
			return map[string]interface{}{
				"column":        optionalObject(projectClassicColumnToGQL(s.store.GetProjectColumn(columnID))),
				"deletedCardId": card.NodeID,
			}, nil
		},
	})

	s.registerMutation(mutationType, "moveProjectCard", &graphql.Field{
		Type: s.mutationPayload("MoveProjectCardPayload", graphql.Fields{
			"cardEdge": gqlField(cardEdgeType),
		}),
		Args: graphql.FieldConfigArgument{"input": &graphql.ArgumentConfig{Type: graphql.NewNonNull(s.mutationInput("MoveProjectCardInput", graphql.InputObjectConfigFieldMap{
			"afterCardId": gqlID(),
			"cardId":      gqlNonNullID(),
			"columnId":    gqlNonNullID(),
		}))}},
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			input, _ := p.Args["input"].(map[string]interface{})
			nodeID, _ := input["cardId"].(string)
			card, err := s.projectClassicCardByNodeID(nodeID)
			if err != nil {
				return nil, err
			}
			columnNodeID, _ := input["columnId"].(string)
			column, err := s.projectClassicColumnByNodeID(columnNodeID)
			if err != nil {
				return nil, err
			}
			// A null afterCardId places the card at the top.
			position := "first"
			if afterNodeID, ok := gqlInputString(input, "afterCardId"); ok && afterNodeID != "" {
				after, err := s.projectClassicCardByNodeID(afterNodeID)
				if err != nil {
					return nil, err
				}
				position = fmt.Sprintf("after:%d", after.ID)
			}
			// The store refuses a destination column in another project.
			if err := s.store.MoveProjectCard(card, column.ID, position); err != nil {
				return nil, err
			}
			moved := s.store.GetProjectCard(card.ID)
			if moved == nil {
				return nil, gqlMissingNode("ProjectCard", nodeID)
			}
			return map[string]interface{}{
				"cardEdge": s.projectClassicCardEdge(moved.ColumnID, moved.NodeID),
			}, nil
		},
	})

	s.registerMutation(mutationType, "convertProjectCardNoteToIssue", &graphql.Field{
		Type: s.mutationPayload("ConvertProjectCardNoteToIssuePayload", graphql.Fields{
			"projectCard": gqlField(cardType),
		}),
		Args: graphql.FieldConfigArgument{"input": &graphql.ArgumentConfig{Type: graphql.NewNonNull(s.mutationInput("ConvertProjectCardNoteToIssueInput", graphql.InputObjectConfigFieldMap{
			"body":          gqlString(),
			"projectCardId": gqlNonNullID(),
			"repositoryId":  gqlNonNullID(),
			"title":         gqlString(),
		}))}},
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			user := s.ghUserFromContext(p.Context)
			input, _ := p.Args["input"].(map[string]interface{})
			nodeID, _ := input["projectCardId"].(string)
			card, err := s.projectClassicCardByNodeID(nodeID)
			if err != nil {
				return nil, err
			}
			if card.IssueID != 0 || card.PullRequestID != 0 || card.Note == "" {
				return nil, fmt.Errorf("only note cards can be converted to issues")
			}
			repoNodeID, _ := input["repositoryId"].(string)
			repo := store.FindRepoByNodeID(s.store, repoNodeID)
			if repo == nil {
				return nil, gqlMissingNode("Repository", repoNodeID)
			}
			column := s.store.GetProjectColumn(card.ColumnID)
			if column == nil {
				return nil, gqlMissingNode("ProjectCard", nodeID)
			}
			project := s.store.GetProjectClassic(column.ProjectID)
			if project == nil || !projectClassicAdmitsRepo(project, repo) {
				return nil, fmt.Errorf("the issue must be created in the project's repository or under its owner")
			}
			// The title defaults to the card's note text.
			title := card.Note
			if supplied, ok := gqlInputString(input, "title"); ok && supplied != "" {
				title = supplied
			}
			body, _ := gqlInputString(input, "body")
			issue := s.store.CreateIssue(repo.ID, user.ID, title, body, nil, nil, 0)
			if issue == nil {
				return nil, fmt.Errorf("issue creation failed")
			}
			// As createIssue's GraphQL path does: fire the issues/opened webhook.
			s.emitWebhookEvent(repo.FullName, "issues", "opened", s.buildIssuesPayload(repo, issue, user, "opened"))
			converted := s.store.ConvertProjectCardToIssue(card, issue.ID)
			if converted == nil {
				return nil, gqlMissingNode("ProjectCard", nodeID)
			}
			return map[string]interface{}{
				"projectCard": optionalObject(s.projectClassicCardToGQL(converted)),
			}, nil
		},
	})
}

func (s *Resolver) addProjectClassicLinkMutations(mutationType *graphql.Object) {
	projectType := s.projectClassicType()

	register := func(name, inputName, payloadName string, link bool) {
		s.registerMutation(mutationType, name, &graphql.Field{
			Type: s.mutationPayload(payloadName, graphql.Fields{
				"project":    gqlField(projectType),
				"repository": gqlField(s.graphqlTypes.repository),
			}),
			Args: graphql.FieldConfigArgument{"input": &graphql.ArgumentConfig{Type: graphql.NewNonNull(s.mutationInput(inputName, graphql.InputObjectConfigFieldMap{
				"projectId":    gqlNonNullID(),
				"repositoryId": gqlNonNullID(),
			}))}},
			Resolve: func(p graphql.ResolveParams) (interface{}, error) {
				input, _ := p.Args["input"].(map[string]interface{})
				nodeID, _ := input["projectId"].(string)
				project, err := s.projectClassicByNodeID(nodeID)
				if err != nil {
					return nil, err
				}
				repoNodeID, _ := input["repositoryId"].(string)
				repo := store.FindRepoByNodeID(s.store, repoNodeID)
				if repo == nil {
					return nil, gqlMissingNode("Repository", repoNodeID)
				}
				if project.RepoKey != "" {
					return nil, fmt.Errorf("a repository-owned project cannot have linked repositories")
				}
				if link {
					if owner, _, ok := store.SplitRepoFullName(repo.FullName); !ok || owner != project.OwnerLogin {
						return nil, fmt.Errorf("the repository must belong to the project's owner")
					}
					if !s.store.LinkRepoToProjectClassic(project.ID, repo.ID) {
						return nil, gqlMissingNode("Project", nodeID)
					}
				} else if !s.store.UnlinkRepoFromProjectClassic(project.ID, repo.ID) {
					return nil, gqlMissingNode("Project", nodeID)
				}
				return map[string]interface{}{
					"project":    optionalObject(s.projectClassicToGQL(s.store.GetProjectClassic(project.ID))),
					"repository": optionalObject(repoToGraphQL(s.store, s.store.SnapRepo(repo))),
				}, nil
			},
		})
	}
	register("linkRepositoryToProject", "LinkRepositoryToProjectInput", "LinkRepositoryToProjectPayload", true)
	register("unlinkRepositoryFromProject", "UnlinkRepositoryFromProjectInput", "UnlinkRepositoryFromProjectPayload", false)
}
