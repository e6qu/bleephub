package graphqlapi

import (
	"fmt"
	"strings"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/graphql-go/graphql"

	"github.com/e6qu/bleephub/internal/gitstore"
	"github.com/e6qu/bleephub/internal/store"
)

// Linked branches: the association between an issue and the branch the work on
// it happens on. GitHub exposes it as Issue.linkedBranches together with the
// createLinkedBranch and deleteLinkedBranch mutations, and it is the surface
// behind the "create a branch" control on an issue.
//
// Creating a link creates the branch: a linked branch that names no reference
// would be a link to nothing, so the reference is written into the repository
// here exactly as the reference-creation route writes one. Deleting the link
// leaves the branch alone, which is GitHub's behaviour and the reason the two
// mutations are not symmetric.

// addLinkedBranchFieldsToSchema installs the LinkedBranch type family, the
// Issue.linkedBranches connection and the two mutations.
func (s *Resolver) addLinkedBranchFieldsToSchema(issueType, mutationType *graphql.Object,
	nodeInterface *graphql.Interface, nodeTypes map[string]*graphql.Object) *graphql.Object {
	linkedBranchType := graphql.NewObject(graphql.ObjectConfig{
		Name:        "LinkedBranch",
		Description: "A branch linked to an issue.",
		Interfaces:  []*graphql.Interface{nodeInterface},
		Fields: graphql.Fields{
			"id": &graphql.Field{
				Type: graphql.NewNonNull(graphql.ID),
				Resolve: func(p graphql.ResolveParams) (interface{}, error) {
					source, ok := p.Source.(map[string]interface{})
					if !ok {
						return nil, fmt.Errorf("resolve source: unexpected type %T", p.Source)
					}
					return source["nodeID"], nil
				},
			},
			"ref": &graphql.Field{
				Type: s.gqlRefType(),
				Resolve: func(p graphql.ResolveParams) (interface{}, error) {
					source, ok := p.Source.(map[string]interface{})
					if !ok {
						return nil, fmt.Errorf("resolve source: unexpected type %T", p.Source)
					}
					ref, ok := source["ref"].(map[string]interface{})
					if !ok {
						// A link whose repository has gone answers null rather
						// than a Ref pointing at nothing.
						return nil, nil
					}
					return ref, nil
				},
			},
		},
	})
	nodeTypes["LinkedBranch"] = linkedBranchType

	linkedBranchEdgeType := graphql.NewObject(graphql.ObjectConfig{
		Name:        "LinkedBranchEdge",
		Description: "An edge in a connection.",
		Fields: graphql.Fields{
			"cursor": &graphql.Field{Type: graphql.NewNonNull(graphql.String)},
			"node":   &graphql.Field{Type: linkedBranchType},
		},
	})
	linkedBranchConnectionType := graphql.NewObject(graphql.ObjectConfig{
		Name:        "LinkedBranchConnection",
		Description: "A list of branches linked to an issue.",
		Fields: graphql.Fields{
			"edges":      &graphql.Field{Type: graphql.NewList(linkedBranchEdgeType)},
			"nodes":      &graphql.Field{Type: graphql.NewList(linkedBranchType)},
			"pageInfo":   &graphql.Field{Type: graphql.NewNonNull(s.gqlPageInfoType())},
			"totalCount": &graphql.Field{Type: graphql.NewNonNull(graphql.Int)},
		},
	})

	issueType.AddFieldConfig("linkedBranches", &graphql.Field{
		Type:        graphql.NewNonNull(linkedBranchConnectionType),
		Description: "Branches linked to this issue.",
		Args:        relayConnectionArgs(),
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			source, ok := p.Source.(map[string]interface{})
			if !ok {
				return nil, fmt.Errorf("resolve source: unexpected type %T", p.Source)
			}
			issueID, _ := source["databaseId"].(int)
			return paginateGQLItems(s.linkedBranchItems(issueID), p.Args), nil
		},
	})

	s.addCreateLinkedBranchMutation(mutationType, issueType, linkedBranchType)
	s.addDeleteLinkedBranchMutation(mutationType, issueType)
	return linkedBranchType
}

// linkedBranchItems renders an issue's links lazily, so a page of one does not
// read every linked repository.
func (s *Resolver) linkedBranchItems(issueID int) []gqlConnItem {
	linked := s.store.ListLinkedBranches(issueID)
	items := make([]gqlConnItem, 0, len(linked))
	for _, entry := range linked {
		link := entry
		items = append(items, gqlConnItem{
			identity: store.LinkedBranchNodeID(issueID, link.Ref),
			render:   func() map[string]interface{} { return s.linkedBranchSource(issueID, link) },
		})
	}
	return items
}

// linkedBranchSource renders one link, resolving the reference it names so the
// Ref it exposes reports the commit the branch actually points at.
func (s *Resolver) linkedBranchSource(issueID int, link store.LinkedBranch) map[string]interface{} {
	source := map[string]interface{}{
		"__typename": "LinkedBranch",
		"nodeID":     store.LinkedBranchNodeID(issueID, link.Ref),
	}
	repo := s.store.GetRepoByID(link.RepoID)
	if repo == nil {
		return source
	}
	owner, name, ok := store.SplitRepoFullName(repo.FullName)
	if !ok {
		return source
	}
	oid := ""
	if stor := s.store.GetGitStorage(owner, name); stor != nil {
		if reference, err := stor.Reference(plumbing.ReferenceName(link.Ref)); err == nil && reference != nil {
			oid = reference.Hash().String()
		}
	}
	source["ref"] = s.decorateRefSource(repo, gitRefSource(repo.FullName, link.Ref, oid))
	return source
}

func (s *Resolver) addCreateLinkedBranchMutation(mutationType, issueType, linkedBranchType *graphql.Object) {
	inputType := graphql.NewInputObject(graphql.InputObjectConfig{
		Name:        "CreateLinkedBranchInput",
		Description: "Autogenerated input type of CreateLinkedBranch",
		Fields: graphql.InputObjectConfigFieldMap{
			"issueId":      &graphql.InputObjectFieldConfig{Type: graphql.NewNonNull(graphql.ID)},
			"name":         &graphql.InputObjectFieldConfig{Type: graphql.String},
			"oid":          &graphql.InputObjectFieldConfig{Type: graphql.NewNonNull(s.graphQLStringScalar("GitObjectID"))},
			"repositoryId": &graphql.InputObjectFieldConfig{Type: graphql.ID},
		},
	})
	payloadType := graphql.NewObject(graphql.ObjectConfig{
		Name:        "CreateLinkedBranchPayload",
		Description: "Autogenerated return type of CreateLinkedBranch.",
		Fields: graphql.Fields{
			"issue":        &graphql.Field{Type: issueType},
			"linkedBranch": &graphql.Field{Type: linkedBranchType},
		},
	})

	s.registerMutation(mutationType, "createLinkedBranch", &graphql.Field{
		Type: payloadType,
		Args: graphql.FieldConfigArgument{
			"input": &graphql.ArgumentConfig{Type: graphql.NewNonNull(inputType)},
		},
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			input, _ := p.Args["input"].(map[string]interface{})
			issueNodeID, _ := input["issueId"].(string)
			issue := store.FindIssueByNodeID(s.store, issueNodeID)
			if issue == nil {
				return nil, gqlMissingNode("Issue", issueNodeID)
			}
			// The branch defaults to the issue's repository; a named one has to
			// exist and be one the caller may write to, which the policy row
			// cannot express because it names a single target.
			repo := s.store.GetRepoByID(issue.RepoID)
			if branchRepoID, ok := input["repositoryId"].(string); ok && branchRepoID != "" {
				named := store.FindRepoByNodeID(s.store, branchRepoID)
				if named == nil {
					return nil, gqlMissingNode("Repository", branchRepoID)
				}
				if !s.viewerCanPushRepo(p.Context, named) {
					return nil, fmt.Errorf("you do not have permission to create a branch in %s", named.FullName)
				}
				repo = named
			}
			if repo == nil {
				return nil, gqlMissingNode("Repository", "")
			}

			oid, _ := input["oid"].(string)
			if !store.ValidGitObjectID(oid) {
				return nil, fmt.Errorf("oid %q is not a commit identifier", oid)
			}
			owner, name, ok := store.SplitRepoFullName(repo.FullName)
			if !ok {
				return nil, fmt.Errorf("repository %q has no owner", repo.FullName)
			}
			stor := s.store.GetGitStorage(owner, name)
			if stor == nil {
				return nil, fmt.Errorf("repository %s has no git storage", repo.FullName)
			}
			if _, err := stor.EncodedObject(plumbing.AnyObject, plumbing.NewHash(oid)); err != nil {
				return nil, fmt.Errorf("object %s does not exist in %s", oid, repo.FullName)
			}

			branch := linkedBranchName(input, issue)
			ref := plumbing.ReferenceName("refs/heads/" + branch)
			if err := gitstore.CreateReferenceIfAbsent(stor, plumbing.NewHashReference(ref, plumbing.NewHash(oid))); err != nil {
				return nil, fmt.Errorf("create %s: %w", ref.String(), err)
			}
			if found, _ := s.store.LinkIssueBranch(issue.ID, repo.ID, ref.String()); !found {
				return nil, gqlMissingNode("Issue", issueNodeID)
			}
			link := store.LinkedBranch{RepoID: repo.ID, Ref: ref.String()}
			return map[string]interface{}{
				"issue":        issueToGQL(s.store.GetIssue(issue.ID), s.store),
				"linkedBranch": s.linkedBranchSource(issue.ID, link),
			}, nil
		},
	})
}

func (s *Resolver) addDeleteLinkedBranchMutation(mutationType, issueType *graphql.Object) {
	inputType := graphql.NewInputObject(graphql.InputObjectConfig{
		Name:        "DeleteLinkedBranchInput",
		Description: "Autogenerated input type of DeleteLinkedBranch",
		Fields: graphql.InputObjectConfigFieldMap{
			"linkedBranchId": &graphql.InputObjectFieldConfig{Type: graphql.NewNonNull(graphql.ID)},
		},
	})
	payloadType := graphql.NewObject(graphql.ObjectConfig{
		Name:        "DeleteLinkedBranchPayload",
		Description: "Autogenerated return type of DeleteLinkedBranch.",
		Fields: graphql.Fields{
			"issue": &graphql.Field{Type: issueType},
		},
	})

	s.registerMutation(mutationType, "deleteLinkedBranch", &graphql.Field{
		Type: payloadType,
		Args: graphql.FieldConfigArgument{
			"input": &graphql.ArgumentConfig{Type: graphql.NewNonNull(inputType)},
		},
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			input, _ := p.Args["input"].(map[string]interface{})
			nodeID, _ := input["linkedBranchId"].(string)
			issue, link, ok := store.FindIssueByLinkedBranchNodeID(s.store, nodeID)
			if !ok {
				return nil, gqlMissingNode("LinkedBranch", nodeID)
			}
			if !s.store.UnlinkIssueBranch(issue.ID, link.Ref) {
				return nil, gqlMissingNode("LinkedBranch", nodeID)
			}
			return map[string]interface{}{
				"issue": issueToGQL(s.store.GetIssue(issue.ID), s.store),
			}, nil
		},
	})
}

// linkedBranchName is the branch a link creates: the name the caller asked for,
// or GitHub's default of the issue number and a slug of its title.
func linkedBranchName(input map[string]interface{}, issue *store.Issue) string {
	if name, ok := input["name"].(string); ok && strings.TrimSpace(name) != "" {
		return strings.TrimPrefix(strings.TrimSpace(name), "refs/heads/")
	}
	slug := make([]rune, 0, len(issue.Title))
	previousDash := false
	for _, r := range strings.ToLower(issue.Title) {
		switch {
		case (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9'):
			slug = append(slug, r)
			previousDash = false
		case !previousDash && len(slug) > 0:
			slug = append(slug, '-')
			previousDash = true
		}
	}
	name := fmt.Sprintf("%d-%s", issue.Number, strings.Trim(string(slug), "-"))
	return strings.TrimSuffix(name, "-")
}
