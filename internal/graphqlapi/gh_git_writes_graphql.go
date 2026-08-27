package graphqlapi

import (
	"encoding/base64"
	"fmt"
	"strings"

	"github.com/e6qu/bleephub/internal/store"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/graphql-go/graphql"
)

// The git-write mutations: createRef, updateRef, deleteRef, mergeBranch,
// createCommitOnBranch and revertPullRequest. Each works through the Repos
// seam — the same branch-protection, secret-scanning and push machinery the
// REST git-database routes run — so a ref moved through GraphQL is
// indistinguishable from one moved through REST.

func init() {
	for name, rule := range map[string]mutationRule{
		"createRef":            repoRule{scope: store.ScopeContents, level: mutationPushRepo, target: mutationTargetRepo("repositoryId")},
		"updateRef":            repoRule{scope: store.ScopeContents, level: mutationPushRepo, target: mutationTargetRef("refId")},
		"deleteRef":            repoRule{scope: store.ScopeContents, level: mutationPushRepo, target: mutationTargetRef("refId")},
		"mergeBranch":          repoRule{scope: store.ScopeContents, level: mutationPushRepo, target: mutationTargetRepo("repositoryId")},
		"updateRefs":           repoRule{scope: store.ScopeContents, level: mutationPushRepo, target: mutationTargetRepo("repositoryId")},
		"createCommitOnBranch": repoRule{scope: store.ScopeContents, level: mutationPushRepo, target: mutationTargetCommittableBranch("branch")},
		"revertPullRequest":    repoRule{scope: store.ScopeContents, level: mutationPushRepo, target: mutationTargetPullRequest("pullRequestId")},
	} {
		if _, exists := graphqlMutationAuthz[name]; exists {
			panic(fmt.Sprintf("graphql mutation %q already has a policy row", name))
		}
		graphqlMutationAuthz[name] = rule
	}
}

// mutationTargetRef resolves a Ref global id to its repository. Ref existence
// is the resolver's question; authz only needs the repository.
func mutationTargetRef(key string) func(*Resolver, map[string]interface{}) mutationTarget {
	return func(s *Resolver, input map[string]interface{}) mutationTarget {
		nodeID, _ := input[key].(string)
		target := mutationTarget{missing: gqlMissingNode("Ref", nodeID)}
		prefix, repoID, _, ok := store.ParseGitObjectNodeID(nodeID)
		if ok && prefix == store.GitRefNodeIDPrefix {
			target.repo = s.store.GetRepoByID(repoID)
		}
		return target
	}
}

// mutationTargetCommittableBranch resolves createCommitOnBranch's branch input
// (a Ref id or a repositoryNameWithOwner) to the repository being written.
func mutationTargetCommittableBranch(key string) func(*Resolver, map[string]interface{}) mutationTarget {
	return func(s *Resolver, input map[string]interface{}) mutationTarget {
		branch, _ := input[key].(map[string]interface{})
		target := mutationTarget{missing: gqlMissingNode("Repository", "")}
		if branch == nil {
			return target
		}
		if nodeID, _ := branch["id"].(string); nodeID != "" {
			target.missing = gqlMissingNode("Ref", nodeID)
			if prefix, repoID, _, ok := store.ParseGitObjectNodeID(nodeID); ok && prefix == store.GitRefNodeIDPrefix {
				target.repo = s.store.GetRepoByID(repoID)
			}
			return target
		}
		fullName, _ := branch["repositoryNameWithOwner"].(string)
		target.missing = gqlMissingNode("Repository", fullName)
		if owner, name, ok := store.SplitRepoFullName(fullName); ok {
			target.repo = s.store.GetRepo(owner, name)
		}
		return target
	}
}

// committableBranch answers the repository and qualified branch name a
// CommittableBranch input addresses, for use after the policy row authorized it.
func (s *Resolver) committableBranch(branch map[string]interface{}) (*store.Repo, string, error) {
	if nodeID, _ := branch["id"].(string); nodeID != "" {
		prefix, repoID, qualified, ok := store.ParseGitObjectNodeID(nodeID)
		if !ok || prefix != store.GitRefNodeIDPrefix {
			return nil, "", gqlMissingNode("Ref", nodeID)
		}
		repo := s.store.GetRepoByID(repoID)
		if repo == nil {
			return nil, "", gqlMissingNode("Ref", nodeID)
		}
		return repo, qualified, nil
	}
	fullName, _ := branch["repositoryNameWithOwner"].(string)
	branchName, _ := branch["branchName"].(string)
	owner, name, ok := store.SplitRepoFullName(fullName)
	if !ok || branchName == "" {
		return nil, "", fmt.Errorf("the branch input must carry either a ref id or a repositoryNameWithOwner and branchName")
	}
	repo := s.store.GetRepo(owner, name)
	if repo == nil {
		return nil, "", gqlMissingNode("Repository", fullName)
	}
	return repo, "refs/heads/" + branchName, nil
}

// refPayloadSource renders the Ref member a git-write payload carries.
func (s *Resolver) refPayloadSource(repo *store.Repo, qualifiedName, oid string) interface{} {
	source := s.decorateRefSource(repo, gitRefSource(repo.FullName, qualifiedName, oid))
	source["__typename"] = "Ref"
	return source
}

// commitPayloadSource renders a Commit member from the repository's storage.
func (s *Resolver) commitPayloadSource(repo *store.Repo, oid string) interface{} {
	owner, name, ok := store.SplitRepoFullName(repo.FullName)
	if !ok {
		return nil
	}
	stor := s.store.GetGitStorage(owner, name)
	if stor == nil {
		return nil
	}
	return optionalObject(gitObjectSourceOfType(stor, s.store, repo.FullName, plumbing.NewHash(oid), plumbing.CommitObject))
}

func (s *Resolver) addGitWriteMutationsToSchema(mutationType *graphql.Object) {
	gitObjectID := s.graphQLStringScalar("GitObjectID")
	base64String := s.graphQLStringScalar("Base64String")

	createRefInput := graphql.NewInputObject(graphql.InputObjectConfig{
		Name: "CreateRefInput",
		Fields: graphql.InputObjectConfigFieldMap{
			"clientMutationId": &graphql.InputObjectFieldConfig{Type: graphql.String},
			"name":             &graphql.InputObjectFieldConfig{Type: graphql.NewNonNull(graphql.String)},
			"oid":              &graphql.InputObjectFieldConfig{Type: graphql.NewNonNull(gitObjectID)},
			"repositoryId":     &graphql.InputObjectFieldConfig{Type: graphql.NewNonNull(graphql.ID)},
		},
	})
	createRefPayload := graphql.NewObject(graphql.ObjectConfig{
		Name: "CreateRefPayload",
		Fields: graphql.Fields{
			"clientMutationId": &graphql.Field{Type: graphql.String},
			"ref":              &graphql.Field{Type: s.graphqlTypes.ref},
		},
	})
	s.registerMutation(mutationType, "createRef", &graphql.Field{
		Type: createRefPayload,
		Args: graphql.FieldConfigArgument{
			"input": &graphql.ArgumentConfig{Type: graphql.NewNonNull(createRefInput)},
		},
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			user := s.ghUserFromContext(p.Context)
			input, _ := p.Args["input"].(map[string]interface{})
			repo := store.FindRepoByNodeID(s.store, str(input["repositoryId"]))
			if repo == nil {
				return nil, gqlMissingNode("Repository", str(input["repositoryId"]))
			}
			name := str(input["name"])
			oid := str(input["oid"])
			if err := s.repos.CreateGitRef(p.Context, repo, user, name, oid); err != nil {
				return nil, err
			}
			return map[string]interface{}{
				"clientMutationId": input["clientMutationId"],
				"ref":              s.refPayloadSource(repo, name, oid),
			}, nil
		},
	})

	updateRefInput := graphql.NewInputObject(graphql.InputObjectConfig{
		Name: "UpdateRefInput",
		Fields: graphql.InputObjectConfigFieldMap{
			"clientMutationId": &graphql.InputObjectFieldConfig{Type: graphql.String},
			"force":            &graphql.InputObjectFieldConfig{Type: graphql.Boolean, DefaultValue: false},
			"oid":              &graphql.InputObjectFieldConfig{Type: graphql.NewNonNull(gitObjectID)},
			"refId":            &graphql.InputObjectFieldConfig{Type: graphql.NewNonNull(graphql.ID)},
		},
	})
	updateRefPayload := graphql.NewObject(graphql.ObjectConfig{
		Name: "UpdateRefPayload",
		Fields: graphql.Fields{
			"clientMutationId": &graphql.Field{Type: graphql.String},
			"ref":              &graphql.Field{Type: s.graphqlTypes.ref},
		},
	})
	s.registerMutation(mutationType, "updateRef", &graphql.Field{
		Type: updateRefPayload,
		Args: graphql.FieldConfigArgument{
			"input": &graphql.ArgumentConfig{Type: graphql.NewNonNull(updateRefInput)},
		},
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			user := s.ghUserFromContext(p.Context)
			input, _ := p.Args["input"].(map[string]interface{})
			nodeID := str(input["refId"])
			prefix, repoID, qualified, ok := store.ParseGitObjectNodeID(nodeID)
			if !ok || prefix != store.GitRefNodeIDPrefix {
				return nil, gqlMissingNode("Ref", nodeID)
			}
			repo := s.store.GetRepoByID(repoID)
			if repo == nil {
				return nil, gqlMissingNode("Ref", nodeID)
			}
			oid := str(input["oid"])
			force, _ := input["force"].(bool)
			if err := s.repos.UpdateGitRef(p.Context, repo, user, qualified, oid, force); err != nil {
				return nil, err
			}
			return map[string]interface{}{
				"clientMutationId": input["clientMutationId"],
				"ref":              s.refPayloadSource(repo, qualified, oid),
			}, nil
		},
	})

	deleteRefInput := graphql.NewInputObject(graphql.InputObjectConfig{
		Name: "DeleteRefInput",
		Fields: graphql.InputObjectConfigFieldMap{
			"clientMutationId": &graphql.InputObjectFieldConfig{Type: graphql.String},
			"refId":            &graphql.InputObjectFieldConfig{Type: graphql.NewNonNull(graphql.ID)},
		},
	})
	deleteRefPayload := graphql.NewObject(graphql.ObjectConfig{
		Name: "DeleteRefPayload",
		Fields: graphql.Fields{
			"clientMutationId": &graphql.Field{Type: graphql.String},
		},
	})
	s.registerMutation(mutationType, "deleteRef", &graphql.Field{
		Type: deleteRefPayload,
		Args: graphql.FieldConfigArgument{
			"input": &graphql.ArgumentConfig{Type: graphql.NewNonNull(deleteRefInput)},
		},
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			user := s.ghUserFromContext(p.Context)
			input, _ := p.Args["input"].(map[string]interface{})
			nodeID := str(input["refId"])
			prefix, repoID, qualified, ok := store.ParseGitObjectNodeID(nodeID)
			if !ok || prefix != store.GitRefNodeIDPrefix {
				return nil, gqlMissingNode("Ref", nodeID)
			}
			repo := s.store.GetRepoByID(repoID)
			if repo == nil {
				return nil, gqlMissingNode("Ref", nodeID)
			}
			if err := s.repos.DeleteGitRef(p.Context, repo, user, qualified); err != nil {
				return nil, err
			}
			return map[string]interface{}{
				"clientMutationId": input["clientMutationId"],
			}, nil
		},
	})

	mergeBranchInput := graphql.NewInputObject(graphql.InputObjectConfig{
		Name: "MergeBranchInput",
		Fields: graphql.InputObjectConfigFieldMap{
			"authorEmail":      &graphql.InputObjectFieldConfig{Type: graphql.String},
			"base":             &graphql.InputObjectFieldConfig{Type: graphql.NewNonNull(graphql.String)},
			"clientMutationId": &graphql.InputObjectFieldConfig{Type: graphql.String},
			"commitMessage":    &graphql.InputObjectFieldConfig{Type: graphql.String},
			"head":             &graphql.InputObjectFieldConfig{Type: graphql.NewNonNull(graphql.String)},
			"repositoryId":     &graphql.InputObjectFieldConfig{Type: graphql.NewNonNull(graphql.ID)},
		},
	})
	mergeBranchPayload := graphql.NewObject(graphql.ObjectConfig{
		Name: "MergeBranchPayload",
		Fields: graphql.Fields{
			"clientMutationId": &graphql.Field{Type: graphql.String},
			"mergeCommit":      &graphql.Field{Type: s.graphqlTypes.commit},
		},
	})
	s.registerMutation(mutationType, "mergeBranch", &graphql.Field{
		Type: mergeBranchPayload,
		Args: graphql.FieldConfigArgument{
			"input": &graphql.ArgumentConfig{Type: graphql.NewNonNull(mergeBranchInput)},
		},
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			user := s.ghUserFromContext(p.Context)
			input, _ := p.Args["input"].(map[string]interface{})
			repo := store.FindRepoByNodeID(s.store, str(input["repositoryId"]))
			if repo == nil {
				return nil, gqlMissingNode("Repository", str(input["repositoryId"]))
			}
			oid, err := s.repos.MergeBranch(p.Context, repo, user,
				str(input["base"]), str(input["head"]), str(input["commitMessage"]), str(input["authorEmail"]))
			if err != nil {
				return nil, err
			}
			// An already-merged head answers no merge commit (REST's 204).
			var mergeCommit interface{}
			if oid != "" {
				mergeCommit = s.commitPayloadSource(repo, oid)
			}
			return map[string]interface{}{
				"clientMutationId": input["clientMutationId"],
				"mergeCommit":      mergeCommit,
			}, nil
		},
	})

	fileAdditionInput := graphql.NewInputObject(graphql.InputObjectConfig{
		Name: "FileAddition",
		Fields: graphql.InputObjectConfigFieldMap{
			"contents": &graphql.InputObjectFieldConfig{Type: graphql.NewNonNull(base64String)},
			"path":     &graphql.InputObjectFieldConfig{Type: graphql.NewNonNull(graphql.String)},
		},
	})
	fileDeletionInput := graphql.NewInputObject(graphql.InputObjectConfig{
		Name: "FileDeletion",
		Fields: graphql.InputObjectConfigFieldMap{
			"path": &graphql.InputObjectFieldConfig{Type: graphql.NewNonNull(graphql.String)},
		},
	})
	fileChangesInput := graphql.NewInputObject(graphql.InputObjectConfig{
		Name: "FileChanges",
		Fields: graphql.InputObjectConfigFieldMap{
			"additions": &graphql.InputObjectFieldConfig{Type: graphql.NewList(graphql.NewNonNull(fileAdditionInput))},
			"deletions": &graphql.InputObjectFieldConfig{Type: graphql.NewList(graphql.NewNonNull(fileDeletionInput))},
		},
	})
	committableBranchInput := graphql.NewInputObject(graphql.InputObjectConfig{
		Name: "CommittableBranch",
		Fields: graphql.InputObjectConfigFieldMap{
			"branchName":              &graphql.InputObjectFieldConfig{Type: graphql.String},
			"id":                      &graphql.InputObjectFieldConfig{Type: graphql.ID},
			"repositoryNameWithOwner": &graphql.InputObjectFieldConfig{Type: graphql.String},
		},
	})
	commitMessageInput := graphql.NewInputObject(graphql.InputObjectConfig{
		Name: "CommitMessage",
		Fields: graphql.InputObjectConfigFieldMap{
			"body":     &graphql.InputObjectFieldConfig{Type: graphql.String},
			"headline": &graphql.InputObjectFieldConfig{Type: graphql.NewNonNull(graphql.String)},
		},
	})
	createCommitInput := graphql.NewInputObject(graphql.InputObjectConfig{
		Name: "CreateCommitOnBranchInput",
		Fields: graphql.InputObjectConfigFieldMap{
			"branch":           &graphql.InputObjectFieldConfig{Type: graphql.NewNonNull(committableBranchInput)},
			"clientMutationId": &graphql.InputObjectFieldConfig{Type: graphql.String},
			"expectedHeadOid":  &graphql.InputObjectFieldConfig{Type: graphql.NewNonNull(gitObjectID)},
			"fileChanges":      &graphql.InputObjectFieldConfig{Type: fileChangesInput},
			"message":          &graphql.InputObjectFieldConfig{Type: graphql.NewNonNull(commitMessageInput)},
		},
	})
	createCommitPayload := graphql.NewObject(graphql.ObjectConfig{
		Name: "CreateCommitOnBranchPayload",
		Fields: graphql.Fields{
			"clientMutationId": &graphql.Field{Type: graphql.String},
			"commit":           &graphql.Field{Type: s.graphqlTypes.commit},
			"ref":              &graphql.Field{Type: s.graphqlTypes.ref},
		},
	})
	s.registerMutation(mutationType, "createCommitOnBranch", &graphql.Field{
		Type: createCommitPayload,
		Args: graphql.FieldConfigArgument{
			"input": &graphql.ArgumentConfig{Type: graphql.NewNonNull(createCommitInput)},
		},
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			user := s.ghUserFromContext(p.Context)
			input, _ := p.Args["input"].(map[string]interface{})
			branchInput, _ := input["branch"].(map[string]interface{})
			repo, qualified, err := s.committableBranch(branchInput)
			if err != nil {
				return nil, err
			}
			additions := map[string][]byte{}
			var deletions []string
			if changes, _ := input["fileChanges"].(map[string]interface{}); changes != nil {
				if raw, _ := changes["additions"].([]interface{}); raw != nil {
					for _, entry := range raw {
						addition, _ := entry.(map[string]interface{})
						path := str(addition["path"])
						decoded, err := base64.StdEncoding.DecodeString(str(addition["contents"]))
						if err != nil {
							return nil, fmt.Errorf("contents for %q is not valid base64", path)
						}
						additions[path] = decoded
					}
				}
				if raw, _ := changes["deletions"].([]interface{}); raw != nil {
					for _, entry := range raw {
						deletion, _ := entry.(map[string]interface{})
						deletions = append(deletions, str(deletion["path"]))
					}
				}
			}
			message, _ := input["message"].(map[string]interface{})
			oid, err := s.repos.CreateCommitOnBranch(p.Context, repo, user, qualified, str(input["expectedHeadOid"]),
				additions, deletions, str(message["headline"]), str(message["body"]))
			if err != nil {
				return nil, err
			}
			return map[string]interface{}{
				"clientMutationId": input["clientMutationId"],
				"commit":           s.commitPayloadSource(repo, oid),
				"ref":              s.refPayloadSource(repo, qualified, oid),
			}, nil
		},
	})

	refUpdateInput := graphql.NewInputObject(graphql.InputObjectConfig{
		Name: "RefUpdate",
		Fields: graphql.InputObjectConfigFieldMap{
			"afterOid":  &graphql.InputObjectFieldConfig{Type: graphql.NewNonNull(gitObjectID)},
			"beforeOid": &graphql.InputObjectFieldConfig{Type: gitObjectID},
			"force":     &graphql.InputObjectFieldConfig{Type: graphql.Boolean, DefaultValue: false},
			"name":      &graphql.InputObjectFieldConfig{Type: graphql.NewNonNull(s.graphQLStringScalar("GitRefname"))},
		},
	})
	updateRefsInput := graphql.NewInputObject(graphql.InputObjectConfig{
		Name: "UpdateRefsInput",
		Fields: graphql.InputObjectConfigFieldMap{
			"clientMutationId": &graphql.InputObjectFieldConfig{Type: graphql.String},
			"refUpdates":       &graphql.InputObjectFieldConfig{Type: graphql.NewNonNull(graphql.NewList(graphql.NewNonNull(refUpdateInput)))},
			"repositoryId":     &graphql.InputObjectFieldConfig{Type: graphql.NewNonNull(graphql.ID)},
		},
	})
	updateRefsPayload := graphql.NewObject(graphql.ObjectConfig{
		Name: "UpdateRefsPayload",
		Fields: graphql.Fields{
			"clientMutationId": &graphql.Field{Type: graphql.String},
		},
	})
	s.registerMutation(mutationType, "updateRefs", &graphql.Field{
		Type: updateRefsPayload,
		Args: graphql.FieldConfigArgument{
			"input": &graphql.ArgumentConfig{Type: graphql.NewNonNull(updateRefsInput)},
		},
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			user := s.ghUserFromContext(p.Context)
			input, _ := p.Args["input"].(map[string]interface{})
			repo := store.FindRepoByNodeID(s.store, str(input["repositoryId"]))
			if repo == nil {
				return nil, gqlMissingNode("Repository", str(input["repositoryId"]))
			}
			owner, name, ok := store.SplitRepoFullName(repo.FullName)
			if !ok {
				return nil, gqlMissingNode("Repository", str(input["repositoryId"]))
			}
			stor := s.store.GetGitStorage(owner, name)
			if stor == nil {
				return nil, fmt.Errorf("the repository has no git storage")
			}
			updates, _ := input["refUpdates"].([]interface{})
			for _, raw := range updates {
				update, _ := raw.(map[string]interface{})
				refName := str(update["name"])
				if !strings.HasPrefix(refName, "refs/") {
					refName = "refs/heads/" + refName
				}
				afterOid := str(update["afterOid"])
				force, _ := update["force"].(bool)

				current, currentErr := stor.Reference(plumbing.ReferenceName(refName))
				// beforeOid is the head the client saw; a reference that has
				// moved since is refused rather than silently overwritten.
				if beforeOid := str(update["beforeOid"]); beforeOid != "" {
					if currentErr != nil {
						return nil, fmt.Errorf("%s does not exist but beforeOid was supplied", refName)
					}
					if current.Hash().String() != beforeOid {
						return nil, fmt.Errorf("expected %s to point to %q but it did not", refName, beforeOid)
					}
				}

				var err error
				switch {
				case afterOid == plumbing.ZeroHash.String():
					err = s.repos.DeleteGitRef(p.Context, repo, user, refName)
				case currentErr != nil:
					err = s.repos.CreateGitRef(p.Context, repo, user, refName, afterOid)
				default:
					err = s.repos.UpdateGitRef(p.Context, repo, user, refName, afterOid, force)
				}
				if err != nil {
					return nil, err
				}
			}
			return map[string]interface{}{
				"clientMutationId": input["clientMutationId"],
			}, nil
		},
	})

	revertInput := graphql.NewInputObject(graphql.InputObjectConfig{
		Name: "RevertPullRequestInput",
		Fields: graphql.InputObjectConfigFieldMap{
			"body":             &graphql.InputObjectFieldConfig{Type: graphql.String},
			"clientMutationId": &graphql.InputObjectFieldConfig{Type: graphql.String},
			"draft":            &graphql.InputObjectFieldConfig{Type: graphql.Boolean, DefaultValue: false},
			"pullRequestId":    &graphql.InputObjectFieldConfig{Type: graphql.NewNonNull(graphql.ID)},
			"title":            &graphql.InputObjectFieldConfig{Type: graphql.String},
		},
	})
	revertPayload := graphql.NewObject(graphql.ObjectConfig{
		Name: "RevertPullRequestPayload",
		Fields: graphql.Fields{
			"clientMutationId":  &graphql.Field{Type: graphql.String},
			"pullRequest":       &graphql.Field{Type: s.graphqlTypes.pullRequest},
			"revertPullRequest": &graphql.Field{Type: s.graphqlTypes.pullRequest},
		},
	})
	s.registerMutation(mutationType, "revertPullRequest", &graphql.Field{
		Type: revertPayload,
		Args: graphql.FieldConfigArgument{
			"input": &graphql.ArgumentConfig{Type: graphql.NewNonNull(revertInput)},
		},
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			user := s.ghUserFromContext(p.Context)
			input, _ := p.Args["input"].(map[string]interface{})
			nodeID := str(input["pullRequestId"])
			pr := store.FindPullRequestByNodeID(s.store, nodeID)
			if pr == nil {
				return nil, gqlMissingNode("PullRequest", nodeID)
			}
			repo := s.store.GetRepoByID(pr.RepoID)
			if repo == nil {
				return nil, gqlMissingNode("PullRequest", nodeID)
			}
			title := str(input["title"])
			if title == "" {
				title = fmt.Sprintf("Revert %q", pr.Title)
			}
			body := str(input["body"])
			if body == "" {
				body = fmt.Sprintf("Reverts %s#%d", repo.FullName, pr.Number)
			}
			draft, _ := input["draft"].(bool)
			revertID, err := s.repos.RevertPullRequest(p.Context, repo, pr, user, title, body, draft)
			if err != nil {
				return nil, err
			}
			return map[string]interface{}{
				"clientMutationId":  input["clientMutationId"],
				"pullRequest":       optionalRendered(s.store.GetPullRequest(pr.ID), func(p *store.PullRequest) map[string]interface{} { return pullRequestToGQL(p, s.store) }),
				"revertPullRequest": optionalRendered(s.store.GetPullRequest(revertID), func(p *store.PullRequest) map[string]interface{} { return pullRequestToGQL(p, s.store) }),
			}, nil
		},
	})
}

// str reads a string member of a decoded input, treating absent/null as "".
func str(v interface{}) string {
	s, _ := v.(string)
	return strings.TrimSpace(s)
}
