package graphqlapi

import (
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/e6qu/bleephub/internal/store"
	git "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/graphql-go/graphql"
)

// addPullRequestFieldsToSchema adds PR types, queries, and mutations to the schema.
func (s *Resolver) addPullRequestFieldsToSchema(userType, issueType, milestoneType, repoType, mutationType, queryType *graphql.Object, nodeInterface *graphql.Interface) *graphql.Object {
	dateTime := s.graphQLStringScalar("DateTime")
	uri := s.graphQLStringScalar("URI")
	gitObjectID := s.graphQLStringScalar("GitObjectID")
	commentAuthorAssociationEnum := s.graphQLEnum(
		"CommentAuthorAssociation",
		"COLLABORATOR", "CONTRIBUTOR", "FIRST_TIMER", "FIRST_TIME_CONTRIBUTOR",
		"MANNEQUIN", "MEMBER", "NONE", "OWNER",
	)
	statusStateEnum := s.graphQLEnum("StatusState", "ERROR", "EXPECTED", "FAILURE", "PENDING", "SUCCESS")
	checkStatusStateEnum := s.graphQLEnum("CheckStatusState", "COMPLETED", "IN_PROGRESS", "PENDING", "QUEUED", "REQUESTED", "WAITING")
	checkConclusionStateEnum := s.graphQLEnum(
		"CheckConclusionState",
		"ACTION_REQUIRED", "CANCELLED", "FAILURE", "NEUTRAL", "SKIPPED", "STALE",
		"STARTUP_FAILURE", "SUCCESS", "TIMED_OUT",
	)
	checkRunStateEnum := s.graphQLEnum(
		"CheckRunState",
		"ACTION_REQUIRED", "CANCELLED", "COMPLETED", "FAILURE", "IN_PROGRESS",
		"NEUTRAL", "PENDING", "QUEUED", "SKIPPED", "STALE", "STARTUP_FAILURE",
		"SUCCESS", "TIMED_OUT", "WAITING",
	)
	pullRequestReviewStateEnum := s.graphQLEnum(
		"PullRequestReviewState",
		"APPROVED", "CHANGES_REQUESTED", "COMMENTED", "DISMISSED", "PENDING",
	)
	pullRequestReviewCommentStateEnum := s.graphQLEnum(
		"PullRequestReviewCommentState",
		"PENDING", "SUBMITTED",
	)
	patchStatusEnum := s.graphQLEnum(
		"PatchStatus",
		"ADDED", "CHANGED", "COPIED", "DELETED", "MODIFIED", "RENAMED",
	)
	// Enums
	pullRequestStateEnum := s.sharedEnum("PullRequestState", "OPEN", "CLOSED", "MERGED")

	mergeableStateEnum := graphql.NewEnum(graphql.EnumConfig{
		Name: "MergeableState",
		Values: graphql.EnumValueConfigMap{
			"MERGEABLE":   &graphql.EnumValueConfig{Value: "MERGEABLE"},
			"CONFLICTING": &graphql.EnumValueConfig{Value: "CONFLICTING"},
			"UNKNOWN":     &graphql.EnumValueConfig{Value: "UNKNOWN"},
		},
	})

	// MergeStateStatus declares GitHub's full value set, but only
	// CLEAN/DIRTY/UNKNOWN are derived (from the PR's stored mergeability).
	mergeStateStatusEnum := graphql.NewEnum(graphql.EnumConfig{
		Name: "MergeStateStatus",
		Values: graphql.EnumValueConfigMap{
			"BEHIND":    &graphql.EnumValueConfig{Value: "BEHIND"},
			"BLOCKED":   &graphql.EnumValueConfig{Value: "BLOCKED"},
			"CLEAN":     &graphql.EnumValueConfig{Value: "CLEAN"},
			"DIRTY":     &graphql.EnumValueConfig{Value: "DIRTY"},
			"DRAFT":     &graphql.EnumValueConfig{Value: "DRAFT"},
			"HAS_HOOKS": &graphql.EnumValueConfig{Value: "HAS_HOOKS"},
			"UNKNOWN":   &graphql.EnumValueConfig{Value: "UNKNOWN"},
			"UNSTABLE":  &graphql.EnumValueConfig{Value: "UNSTABLE"},
		},
	})

	// Shared: Repository.viewerDefaultMergeMethod names the same enum.
	pullRequestMergeMethodEnum := s.sharedEnum("PullRequestMergeMethod", "MERGE", "SQUASH", "REBASE")

	pullRequestReviewDecisionEnum := graphql.NewEnum(graphql.EnumConfig{
		Name: "PullRequestReviewDecision",
		Values: graphql.EnumValueConfigMap{
			"APPROVED":          &graphql.EnumValueConfig{Value: "APPROVED"},
			"CHANGES_REQUESTED": &graphql.EnumValueConfig{Value: "CHANGES_REQUESTED"},
			"REVIEW_REQUIRED":   &graphql.EnumValueConfig{Value: "REVIEW_REQUIRED"},
		},
	})

	// Shared with issues, from the registry.
	prLabelConnectionType := s.gqlLabelConnectionType()
	prAssigneeConnectionType := s.gqlUserConnectionType(userType)
	prReactionGroupType := s.gqlReactionGroupType()

	// Commit + status-check rollup types
	// gh selects PR check state through commits(last:1){commit{statusCheckRollup
	// {contexts{...on StatusContext, ...on CheckRun}}}}. CheckRun nodes come from
	// the checks store, StatusContext nodes from the REST commit-status store.
	statusContextType := graphql.NewObject(graphql.ObjectConfig{
		Name:       "StatusContext",
		Interfaces: []*graphql.Interface{s.gqlRequirableByPullRequestInterface()},
		Fields: graphql.Fields{
			"context":     &graphql.Field{Type: graphql.NewNonNull(graphql.String)},
			"state":       &graphql.Field{Type: graphql.NewNonNull(statusStateEnum)},
			"targetUrl":   &graphql.Field{Type: uri},
			"createdAt":   &graphql.Field{Type: graphql.NewNonNull(dateTime)},
			"description": &graphql.Field{Type: graphql.String},
			"isRequired":  s.gqlIsRequiredField("context"),
		},
	})

	// WorkflowRun and Workflow are minted here as the minimal shells the
	// CheckSuite rollup references, then registered so addActionsFamilyFields
	// finishes them with the full field set on the same objects. Only the
	// members the rollup source populates are declared here.
	workflowFileType := graphql.NewObject(graphql.ObjectConfig{
		Name: "Workflow",
		Fields: graphql.Fields{
			"name": &graphql.Field{Type: graphql.NewNonNull(graphql.String)},
		},
	})
	workflowRunType := graphql.NewObject(graphql.ObjectConfig{
		Name: "WorkflowRun",
		Fields: graphql.Fields{
			"workflow": &graphql.Field{
				Type: graphql.NewNonNull(workflowFileType),
				Resolve: func(p graphql.ResolveParams) (interface{}, error) {
					run, ok := p.Source.(map[string]interface{})
					if !ok {
						return nil, fmt.Errorf("resolve source: unexpected type %T", p.Source)
					}
					return run["workflow"], nil
				},
			},
		},
	})
	s.graphqlTypes.workflow = workflowFileType
	s.graphqlTypes.workflowRun = workflowRunType

	checkSuiteType := graphql.NewObject(graphql.ObjectConfig{
		Name: "CheckSuite",
		Fields: graphql.Fields{
			"id":         &graphql.Field{Type: graphql.NewNonNull(graphql.ID)},
			"databaseId": &graphql.Field{Type: graphql.Int},
			"status":     &graphql.Field{Type: graphql.NewNonNull(checkStatusStateEnum)},
			"conclusion": &graphql.Field{Type: checkConclusionStateEnum},
			"createdAt":  &graphql.Field{Type: graphql.NewNonNull(dateTime)},
			"updatedAt":  &graphql.Field{Type: graphql.NewNonNull(dateTime)},
			"workflowRun": &graphql.Field{
				Type: workflowRunType,
				Resolve: func(p graphql.ResolveParams) (interface{}, error) {
					suite, ok := p.Source.(map[string]interface{})
					if !ok {
						return nil, fmt.Errorf("resolve source: unexpected type %T", p.Source)
					}
					return suite["workflowRun"], nil
				},
			},
		},
	})

	checkRunType := graphql.NewObject(graphql.ObjectConfig{
		Name:       "CheckRun",
		Interfaces: []*graphql.Interface{s.gqlRequirableByPullRequestInterface()},
		Fields: graphql.Fields{
			"id":         &graphql.Field{Type: graphql.NewNonNull(graphql.ID)},
			"databaseId": &graphql.Field{Type: graphql.Int},
			"name":       &graphql.Field{Type: graphql.NewNonNull(graphql.String)},
			"isRequired": s.gqlIsRequiredField("name"),
			"status":     &graphql.Field{Type: graphql.NewNonNull(checkStatusStateEnum)},
			"conclusion": &graphql.Field{Type: checkConclusionStateEnum},
			"startedAt":  &graphql.Field{Type: dateTime},
			"completedAt": &graphql.Field{
				Type: dateTime,
				Resolve: func(p graphql.ResolveParams) (interface{}, error) {
					cr, ok := p.Source.(map[string]interface{})
					if !ok {
						return nil, fmt.Errorf("resolve source: unexpected type %T", p.Source)
					}
					return cr["completedAt"], nil
				},
			},
			"detailsUrl": &graphql.Field{Type: uri},
			"externalId": &graphql.Field{Type: graphql.String},
			"title":      &graphql.Field{Type: graphql.String},
			"summary":    &graphql.Field{Type: graphql.String},
			"text":       &graphql.Field{Type: graphql.String},
			"checkSuite": &graphql.Field{
				Type: graphql.NewNonNull(checkSuiteType),
				Resolve: func(p graphql.ResolveParams) (interface{}, error) {
					cr, ok := p.Source.(map[string]interface{})
					if !ok {
						return nil, fmt.Errorf("resolve source: unexpected type %T", p.Source)
					}
					return cr["checkSuite"], nil
				},
			},
		},
	})

	s.graphqlTypes.statusContext = statusContextType
	s.graphqlTypes.checkRun = checkRunType
	s.graphqlTypes.checkSuite = checkSuiteType

	statusCheckRollupContextUnion := graphql.NewUnion(graphql.UnionConfig{
		Name:  "StatusCheckRollupContext",
		Types: []*graphql.Object{statusContextType, checkRunType},
		ResolveType: func(p graphql.ResolveTypeParams) *graphql.Object {
			if m, ok := p.Value.(map[string]interface{}); ok {
				if tn, _ := m["__typename"].(string); tn == "StatusContext" {
					return statusContextType
				}
			}
			return checkRunType
		},
	})

	statusCheckStateCountType := func(name string) *graphql.Object {
		stateType := graphql.Output(statusStateEnum)
		if name == "CheckRunStateCount" {
			stateType = checkRunStateEnum
		}
		return graphql.NewObject(graphql.ObjectConfig{
			Name: name,
			Fields: graphql.Fields{
				"state": &graphql.Field{Type: graphql.NewNonNull(stateType)},
				"count": &graphql.Field{Type: graphql.NewNonNull(graphql.Int)},
			},
		})
	}

	statusCheckRollupContextConnectionType := graphql.NewObject(graphql.ObjectConfig{
		Name: "StatusCheckRollupContextConnection",
		Fields: graphql.Fields{
			"nodes":                      &graphql.Field{Type: graphql.NewList(statusCheckRollupContextUnion)},
			"totalCount":                 &graphql.Field{Type: graphql.NewNonNull(graphql.Int)},
			"checkRunCount":              &graphql.Field{Type: graphql.NewNonNull(graphql.Int)},
			"checkRunCountsByState":      &graphql.Field{Type: graphql.NewList(graphql.NewNonNull(statusCheckStateCountType("CheckRunStateCount")))},
			"statusContextCount":         &graphql.Field{Type: graphql.NewNonNull(graphql.Int)},
			"statusContextCountsByState": &graphql.Field{Type: graphql.NewList(graphql.NewNonNull(statusCheckStateCountType("StatusContextStateCount")))},
			"pageInfo":                   &graphql.Field{Type: graphql.NewNonNull(s.gqlPageInfoType())},
			"edges":                      gqlEdgesField(s.simpleEdgeType("StatusCheckRollupContextEdge", statusCheckRollupContextUnion)),
		},
	})

	statusCheckRollupType := graphql.NewObject(graphql.ObjectConfig{
		Name: "StatusCheckRollup",
		Fields: graphql.Fields{
			"state": &graphql.Field{Type: graphql.NewNonNull(statusStateEnum)},
			"contexts": &graphql.Field{
				Type: graphql.NewNonNull(statusCheckRollupContextConnectionType),
				Args: graphql.FieldConfigArgument{
					"first": &graphql.ArgumentConfig{Type: graphql.Int},
					"after": &graphql.ArgumentConfig{Type: graphql.String},
				},
				Resolve: func(p graphql.ResolveParams) (interface{}, error) {
					r, ok := p.Source.(map[string]interface{})
					if !ok {
						return nil, fmt.Errorf("resolve source: unexpected type %T", p.Source)
					}
					return r["contexts"], nil
				},
			},
		},
	})
	s.graphqlTypes.statusCheckRollup = statusCheckRollupType

	gitActorConnectionType := graphql.NewObject(graphql.ObjectConfig{
		Name: "GitActorConnection",
		Fields: graphql.Fields{
			"nodes":      &graphql.Field{Type: graphql.NewList(s.gqlGitActorType())},
			"totalCount": &graphql.Field{Type: graphql.NewNonNull(graphql.Int)},
			"edges":      gqlEdgesField(s.simpleEdgeType("GitActorEdge", s.gqlGitActorType())),
			"pageInfo":   s.gqlPageInfoField(),
		},
	})

	// One Commit type serves the PR commit lists and the git object graph. The
	// git-graph members live with the object graph; the two below are the
	// check-rollup surface the PR queries add.
	commitType := s.gqlCommitType()
	commitType.AddFieldConfig("authors", &graphql.Field{
		Type: graphql.NewNonNull(gitActorConnectionType),
		Args: graphql.FieldConfigArgument{
			"first": &graphql.ArgumentConfig{Type: graphql.Int},
		},
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			c, ok := p.Source.(map[string]interface{})
			if !ok {
				return nil, fmt.Errorf("resolve source: unexpected type %T", p.Source)
			}
			if authors, ok := c["authors"]; ok && authors != nil {
				return authors, nil
			}
			// A commit from the object graph carries its git author, not a
			// pre-rendered connection.
			nodes := []interface{}{}
			if author, ok := c["author"].(map[string]interface{}); ok {
				nodes = append(nodes, author)
			}
			return map[string]interface{}{"nodes": nodes, "totalCount": len(nodes)}, nil
		},
	})
	commitType.AddFieldConfig("statusCheckRollup", &graphql.Field{
		// Null when the commit has no statuses or check runs.
		Type: statusCheckRollupType,
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			c, ok := p.Source.(map[string]interface{})
			if !ok {
				return nil, fmt.Errorf("resolve source: unexpected type %T", p.Source)
			}
			if rollup, ok := c["statusCheckRollup"]; ok && rollup != nil {
				return rollup, nil
			}
			repoFullName, _ := c["repoFullName"].(string)
			oid, _ := c["oid"].(string)
			if repoFullName == "" || oid == "" {
				return nil, nil
			}
			s.store.Mu.RLock()
			defer s.store.Mu.RUnlock()
			return statusCheckRollupSourceLocked(s.store, repoFullName, oid), nil
		},
	})

	pullRequestCommitType := graphql.NewObject(graphql.ObjectConfig{
		Name: "PullRequestCommit",
		Fields: graphql.Fields{
			"commit": &graphql.Field{
				Type: graphql.NewNonNull(commitType),
				Resolve: func(p graphql.ResolveParams) (interface{}, error) {
					n, ok := p.Source.(map[string]interface{})
					if !ok {
						return nil, fmt.Errorf("resolve source: unexpected type %T", p.Source)
					}
					return n["commit"], nil
				},
			},
			// Fall back to the embedded commit's oid so a timeline-sourced node
			// (only nodeID + commit) still answers these non-null fields.
			"id": &graphql.Field{
				Type: graphql.NewNonNull(graphql.ID),
				Resolve: func(p graphql.ResolveParams) (interface{}, error) {
					n, _ := p.Source.(map[string]interface{})
					if v, ok := n["nodeID"].(string); ok && v != "" {
						return v, nil
					}
					if v, ok := n["id"].(string); ok && v != "" {
						return v, nil
					}
					return "PRC_" + prCommitOID(n), nil
				},
			},
			"resourcePath": &graphql.Field{
				Type: graphql.NewNonNull(uri),
				Resolve: func(p graphql.ResolveParams) (interface{}, error) {
					n, _ := p.Source.(map[string]interface{})
					if v, ok := n["resourcePath"].(string); ok && v != "" {
						return v, nil
					}
					return prCommitFallbackPath(n), nil
				},
			},
			"url": &graphql.Field{
				Type: graphql.NewNonNull(uri),
				Resolve: func(p graphql.ResolveParams) (interface{}, error) {
					n, _ := p.Source.(map[string]interface{})
					if v, ok := n["url"].(string); ok && v != "" {
						return v, nil
					}
					return externalURL(prCommitFallbackPath(n)), nil
				},
			},
		},
	})

	// Review types
	prReviewType := graphql.NewObject(graphql.ObjectConfig{
		Name:       "PullRequestReview",
		Interfaces: []*graphql.Interface{s.graphqlTypes.reactable},
		Fields: graphql.Fields{
			"id": &graphql.Field{
				Type: graphql.NewNonNull(graphql.ID),
				Resolve: func(p graphql.ResolveParams) (interface{}, error) {
					r, ok := p.Source.(map[string]interface{})
					if !ok {
						return nil, fmt.Errorf("resolve source: unexpected type %T", p.Source)
					}
					return r["nodeID"], nil
				},
			},
			"body":  &graphql.Field{Type: graphql.NewNonNull(graphql.String)},
			"state": &graphql.Field{Type: graphql.NewNonNull(pullRequestReviewStateEnum)},
			"author": &graphql.Field{
				Type: s.graphqlTypes.actor,
				Resolve: func(p graphql.ResolveParams) (interface{}, error) {
					r, ok := p.Source.(map[string]interface{})
					if !ok {
						return nil, fmt.Errorf("resolve source: unexpected type %T", p.Source)
					}
					return r["author"], nil
				},
			},
			"authorAssociation": &graphql.Field{Type: graphql.NewNonNull(commentAuthorAssociationEnum)},
			"createdAt":         &graphql.Field{Type: graphql.NewNonNull(dateTime)},
			"updatedAt":         &graphql.Field{Type: graphql.NewNonNull(dateTime)},
			// Reviews are submitted on creation, so submittedAt == createdAt;
			// commit is the PR head the review was recorded against (REST's
			// commit_id).
			"submittedAt": &graphql.Field{
				Type: dateTime,
				Resolve: func(p graphql.ResolveParams) (interface{}, error) {
					r, ok := p.Source.(map[string]interface{})
					if !ok {
						return nil, fmt.Errorf("resolve source: unexpected type %T", p.Source)
					}
					return r["submittedAt"], nil
				},
			},
			"commit": &graphql.Field{
				Type: commitType,
				Resolve: func(p graphql.ResolveParams) (interface{}, error) {
					r, ok := p.Source.(map[string]interface{})
					if !ok {
						return nil, fmt.Errorf("resolve source: unexpected type %T", p.Source)
					}
					return r["commit"], nil
				},
			},
			"reactionGroups": &graphql.Field{
				Type: graphql.NewList(graphql.NewNonNull(prReactionGroupType)),
				Resolve: func(p graphql.ResolveParams) (interface{}, error) {
					r, ok := p.Source.(map[string]interface{})
					if !ok {
						return nil, fmt.Errorf("resolve source: unexpected type %T", p.Source)
					}
					return r["reactionGroups"], nil
				},
			},
		},
	})

	prReviewConnectionType := graphql.NewObject(graphql.ObjectConfig{
		Name: "PullRequestReviewConnection",
		Fields: graphql.Fields{
			"nodes":      &graphql.Field{Type: graphql.NewList(prReviewType)},
			"totalCount": &graphql.Field{Type: graphql.NewNonNull(graphql.Int)},
			"pageInfo":   &graphql.Field{Type: graphql.NewNonNull(s.gqlPageInfoType())},
			"edges":      gqlEdgesField(s.simpleEdgeType("PullRequestReviewEdge", prReviewType)),
		},
	})

	// Review request types
	// gh's reviewRequests fragment unions User/Bot/Team. Bot and Team exist so
	// the fragments type-check; only user review requests are stored.
	botType := graphql.NewObject(graphql.ObjectConfig{
		Name: "Bot",
		Fields: graphql.Fields{
			"id":    &graphql.Field{Type: graphql.NewNonNull(graphql.ID)},
			"login": &graphql.Field{Type: graphql.NewNonNull(graphql.String)},
			// No Bot source currently arises; these read the source where a
			// renderer would supply it, else derive from the login so the
			// non-null fields never abort.
			"databaseId": &graphql.Field{
				Type: graphql.Int,
				Resolve: func(p graphql.ResolveParams) (interface{}, error) {
					return srcMap(p)["databaseId"], nil
				},
			},
			"avatarUrl": &graphql.Field{
				Type: graphql.NewNonNull(uri),
				Args: graphql.FieldConfigArgument{
					"size": &graphql.ArgumentConfig{Type: graphql.Int},
				},
				Resolve: func(p graphql.ResolveParams) (interface{}, error) {
					if v, ok := srcMap(p)["avatarUrl"].(string); ok && v != "" {
						return v, nil
					}
					return externalURL("/" + botLoginFromSource(srcMap(p)) + ".png"), nil
				},
			},
			"resourcePath": &graphql.Field{
				Type: graphql.NewNonNull(uri),
				Resolve: func(p graphql.ResolveParams) (interface{}, error) {
					return "/apps/" + botLoginFromSource(srcMap(p)), nil
				},
			},
			"url": &graphql.Field{
				Type: graphql.NewNonNull(uri),
				Resolve: func(p graphql.ResolveParams) (interface{}, error) {
					return externalURL("/apps/" + botLoginFromSource(srcMap(p))), nil
				},
			},
			"createdAt": &graphql.Field{
				Type: graphql.NewNonNull(dateTime),
				Resolve: func(p graphql.ResolveParams) (interface{}, error) {
					return botTimestampFromSource(srcMap(p), "createdAt"), nil
				},
			},
			"updatedAt": &graphql.Field{
				Type: graphql.NewNonNull(dateTime),
				Resolve: func(p graphql.ResolveParams) (interface{}, error) {
					return botTimestampFromSource(srcMap(p), "updatedAt"), nil
				},
			},
		},
	})
	teamType := graphql.NewObject(graphql.ObjectConfig{
		Name: "Team",
		Fields: graphql.Fields{
			"id": &graphql.Field{Type: graphql.NewNonNull(graphql.ID)},
			// Nullable (GitHub: String!) to satisfy graphql-go's stricter
			// SameResponseShape merge rule against User.name; never null here.
			"name": &graphql.Field{Type: graphql.String},
			"slug": &graphql.Field{Type: graphql.NewNonNull(graphql.String)},
			// The shared Organization type, not a login-only fork.
			"organization": &graphql.Field{
				Type: graphql.NewNonNull(s.graphqlTypes.organization),
			},
		},
	})
	s.graphqlTypes.team = teamType
	requestedReviewerUnion := graphql.NewUnion(graphql.UnionConfig{
		Name:  "RequestedReviewer",
		Types: []*graphql.Object{userType, botType, teamType},
		ResolveType: func(p graphql.ResolveTypeParams) *graphql.Object {
			switch requestedReviewerTypeName(p.Value) {
			case "Bot":
				return botType
			case "Team":
				return teamType
			default:
				return userType
			}
		},
	})
	s.graphqlTypes.requestedReviewerUnion = requestedReviewerUnion
	reviewRequestType := graphql.NewObject(graphql.ObjectConfig{
		Name: "ReviewRequest",
		Fields: graphql.Fields{
			"requestedReviewer": &graphql.Field{
				Type: requestedReviewerUnion,
				Resolve: func(p graphql.ResolveParams) (interface{}, error) {
					r, ok := p.Source.(map[string]interface{})
					if !ok {
						return nil, fmt.Errorf("resolve source: unexpected type %T", p.Source)
					}
					return r["requestedReviewer"], nil
				},
			},
			"id": &graphql.Field{
				Type: graphql.NewNonNull(graphql.ID),
				Resolve: func(p graphql.ResolveParams) (interface{}, error) {
					return srcMap(p)["nodeID"], nil
				},
			},
			"databaseId": &graphql.Field{
				Type: graphql.Int,
				Resolve: func(p graphql.ResolveParams) (interface{}, error) {
					return srcMap(p)["databaseId"], nil
				},
			},
			"asCodeOwner": &graphql.Field{
				Type: graphql.NewNonNull(graphql.Boolean),
				Resolve: func(p graphql.ResolveParams) (interface{}, error) {
					v, _ := srcMap(p)["asCodeOwner"].(bool)
					return v, nil
				},
			},
		},
	})

	reviewRequestConnectionType := graphql.NewObject(graphql.ObjectConfig{
		Name: "ReviewRequestConnection",
		Fields: graphql.Fields{
			"nodes":      &graphql.Field{Type: graphql.NewList(reviewRequestType)},
			"totalCount": &graphql.Field{Type: graphql.NewNonNull(graphql.Int)},
			"edges":      gqlEdgesField(s.simpleEdgeType("ReviewRequestEdge", reviewRequestType)),
			"pageInfo":   s.gqlPageInfoField(),
		},
	})

	// PR Comment connection
	// PR conversation comments are IssueComment (PRs are issues internally), so
	// the shared IssueCommentConnection serves gh's merged Issue|PullRequest
	// `comments` fragments. Nodes are commentToGQLLocked maps.
	prCommentConnectionType := s.gqlIssueCommentConnectionType()

	// PR Review thread types
	prReviewCommentType := graphql.NewObject(graphql.ObjectConfig{
		Name:       "PullRequestReviewComment",
		Interfaces: []*graphql.Interface{s.graphqlTypes.reactable},
		Fields: graphql.Fields{
			"id": &graphql.Field{
				Type: graphql.NewNonNull(graphql.ID),
				Resolve: func(p graphql.ResolveParams) (interface{}, error) {
					c, ok := p.Source.(map[string]interface{})
					if !ok {
						return nil, fmt.Errorf("resolve source: unexpected type %T", p.Source)
					}
					return c["nodeID"], nil
				},
			},
			"databaseId": &graphql.Field{Type: graphql.Int},
			"body":       &graphql.Field{Type: graphql.NewNonNull(graphql.String)},
			"path":       &graphql.Field{Type: graphql.NewNonNull(graphql.String)},
			"diffHunk":   &graphql.Field{Type: graphql.NewNonNull(graphql.String)},
			"line":       &graphql.Field{Type: graphql.Int},
			"position":   &graphql.Field{Type: graphql.Int},
			"createdAt":  &graphql.Field{Type: graphql.NewNonNull(dateTime)},
			"updatedAt":  &graphql.Field{Type: graphql.NewNonNull(dateTime)},
			"state":      &graphql.Field{Type: graphql.NewNonNull(pullRequestReviewCommentStateEnum)},
			"author": &graphql.Field{
				Type: s.graphqlTypes.actor,
				Resolve: func(p graphql.ResolveParams) (interface{}, error) {
					c, ok := p.Source.(map[string]interface{})
					if !ok {
						return nil, fmt.Errorf("resolve source: unexpected type %T", p.Source)
					}
					return c["author"], nil
				},
			},
		},
	})
	prReviewCommentConnectionType := graphql.NewObject(graphql.ObjectConfig{
		Name: "PullRequestReviewCommentConnection",
		Fields: graphql.Fields{
			"nodes":      &graphql.Field{Type: graphql.NewList(prReviewCommentType)},
			"totalCount": &graphql.Field{Type: graphql.NewNonNull(graphql.Int)},
			"pageInfo":   &graphql.Field{Type: graphql.NewNonNull(s.gqlPageInfoType())},
			"edges":      gqlEdgesField(s.simpleEdgeType("PullRequestReviewCommentEdge", prReviewCommentType)),
		},
	})
	prReviewThreadType := graphql.NewObject(graphql.ObjectConfig{
		Name: "PullRequestReviewThread",
		Fields: graphql.Fields{
			"id":         &graphql.Field{Type: graphql.NewNonNull(graphql.ID)},
			"isResolved": &graphql.Field{Type: graphql.NewNonNull(graphql.Boolean)},
			"isOutdated": &graphql.Field{Type: graphql.NewNonNull(graphql.Boolean)},
			"resolvedBy": &graphql.Field{Type: userType},
			"path":       &graphql.Field{Type: graphql.NewNonNull(graphql.String)},
			"line":       &graphql.Field{Type: graphql.Int},
			"comments": &graphql.Field{
				Type: graphql.NewNonNull(prReviewCommentConnectionType),
				// The SPA queries comments(first: N) and errored without the args.
				Args: relayConnectionArgs(),
				Resolve: func(p graphql.ResolveParams) (interface{}, error) {
					thread, ok := p.Source.(map[string]interface{})
					if !ok {
						return nil, fmt.Errorf("resolve source: unexpected type %T", p.Source)
					}
					return repaginateConnection(thread["comments"], p.Args), nil
				},
			},
		},
	})
	prReviewThreadConnectionType := graphql.NewObject(graphql.ObjectConfig{
		Name: "PullRequestReviewThreadConnection",
		Fields: graphql.Fields{
			"nodes":      &graphql.Field{Type: graphql.NewList(prReviewThreadType)},
			"totalCount": &graphql.Field{Type: graphql.NewNonNull(graphql.Int)},
			"pageInfo":   &graphql.Field{Type: graphql.NewNonNull(s.gqlPageInfoType())},
			"edges":      gqlEdgesField(s.simpleEdgeType("PullRequestReviewThreadEdge", prReviewThreadType)),
		},
	})

	// PR Commit connection
	prCommitConnectionType := graphql.NewObject(graphql.ObjectConfig{
		Name: "PullRequestCommitConnection",
		Fields: graphql.Fields{
			"totalCount": &graphql.Field{Type: graphql.NewNonNull(graphql.Int)},
			"nodes":      &graphql.Field{Type: graphql.NewList(pullRequestCommitType)},
			"pageInfo":   &graphql.Field{Type: graphql.NewNonNull(s.gqlPageInfoType())},
			"edges":      gqlEdgesField(s.simpleEdgeType("PullRequestCommitEdge", pullRequestCommitType)),
		},
	})

	// Auto-merge request type
	// Local so its PullRequest back-reference can be attached once that exists.
	autoMergeRequestType := graphql.NewObject(graphql.ObjectConfig{
		Name: "AutoMergeRequest",
		Fields: graphql.Fields{
			"authorEmail":    &graphql.Field{Type: graphql.String},
			"commitBody":     &graphql.Field{Type: graphql.String},
			"commitHeadline": &graphql.Field{Type: graphql.String},
			"mergeMethod":    &graphql.Field{Type: graphql.NewNonNull(pullRequestMergeMethodEnum)},
			"enabledAt":      &graphql.Field{Type: dateTime},
			"enabledBy":      &graphql.Field{Type: s.graphqlTypes.actor},
		},
	})

	// Changed-file types
	// viewerViewedState is VIEWED for a file the viewer marked, else UNVIEWED;
	// DISMISSED (changed since last viewed) is not tracked.
	fileViewedStateEnum := s.sharedEnum("FileViewedState", "DISMISSED", "UNVIEWED", "VIEWED")
	changedFileType := graphql.NewObject(graphql.ObjectConfig{
		Name: "PullRequestChangedFile",
		Fields: graphql.Fields{
			"additions":  &graphql.Field{Type: graphql.NewNonNull(graphql.Int)},
			"deletions":  &graphql.Field{Type: graphql.NewNonNull(graphql.Int)},
			"path":       &graphql.Field{Type: graphql.NewNonNull(graphql.String)},
			"changeType": &graphql.Field{Type: graphql.NewNonNull(patchStatusEnum)},
			"viewerViewedState": &graphql.Field{
				Type: graphql.NewNonNull(fileViewedStateEnum),
				Resolve: func(p graphql.ResolveParams) (interface{}, error) {
					src := srcMap(p)
					path, _ := src["path"].(string)
					prID, _ := src["prID"].(int)
					viewer := s.ghUserFromContext(p.Context)
					if viewer == nil || prID == 0 || path == "" {
						return "UNVIEWED", nil
					}
					for _, seen := range s.store.PullRequestViewedFiles(prID, viewer.ID) {
						if seen == path {
							return "VIEWED", nil
						}
					}
					return "UNVIEWED", nil
				},
			},
		},
	})
	changedFileConnType := graphql.NewObject(graphql.ObjectConfig{
		Name: "PullRequestChangedFileConnection",
		Fields: graphql.Fields{
			"nodes":      &graphql.Field{Type: graphql.NewList(changedFileType)},
			"totalCount": &graphql.Field{Type: graphql.NewNonNull(graphql.Int)},
			"edges":      gqlEdgesField(s.simpleEdgeType("PullRequestChangedFileEdge", changedFileType)),
			"pageInfo":   s.gqlPageInfoField(),
		},
	})

	// PullRequest type
	pullRequestType := graphql.NewObject(graphql.ObjectConfig{
		Name: "PullRequest",
		// Closable and Assignable are claimed so a timeline ClosedEvent or
		// AssignedEvent can resolve its subject through them.
		Interfaces: []*graphql.Interface{
			nodeInterface, s.gqlLockableInterface(), s.gqlLabelableInterface(),
			s.graphqlTypes.reactable, s.uniformResourceLocatableInterface(),
			s.gqlClosableInterface(), s.gqlAssignableInterface(),
		},
		Fields: graphql.Fields{
			"id": &graphql.Field{
				Type: graphql.NewNonNull(graphql.ID),
				Resolve: func(p graphql.ResolveParams) (interface{}, error) {
					pr, ok := p.Source.(map[string]interface{})
					if !ok {
						return nil, fmt.Errorf("resolve source: unexpected type %T", p.Source)
					}
					return pr["nodeID"], nil
				},
			},
			"databaseId":       &graphql.Field{Type: graphql.Int},
			"number":           &graphql.Field{Type: graphql.NewNonNull(graphql.Int)},
			"title":            &graphql.Field{Type: graphql.NewNonNull(graphql.String)},
			"body":             &graphql.Field{Type: graphql.NewNonNull(graphql.String)},
			"state":            &graphql.Field{Type: graphql.NewNonNull(pullRequestStateEnum)},
			"isDraft":          &graphql.Field{Type: graphql.NewNonNull(graphql.Boolean)},
			"url":              &graphql.Field{Type: graphql.NewNonNull(uri)},
			"resourcePath":     &graphql.Field{Type: graphql.NewNonNull(uri)},
			"headRefName":      &graphql.Field{Type: graphql.NewNonNull(graphql.String)},
			"baseRefName":      &graphql.Field{Type: graphql.NewNonNull(graphql.String)},
			"headRefOid":       &graphql.Field{Type: graphql.NewNonNull(gitObjectID)},
			"mergeable":        &graphql.Field{Type: graphql.NewNonNull(mergeableStateEnum)},
			"merged":           &graphql.Field{Type: graphql.NewNonNull(graphql.Boolean)},
			"mergedAt":         &graphql.Field{Type: dateTime},
			"additions":        &graphql.Field{Type: graphql.NewNonNull(graphql.Int)},
			"deletions":        &graphql.Field{Type: graphql.NewNonNull(graphql.Int)},
			"changedFiles":     &graphql.Field{Type: graphql.NewNonNull(graphql.Int)},
			"locked":           &graphql.Field{Type: graphql.NewNonNull(graphql.Boolean)},
			"activeLockReason": &graphql.Field{Type: s.graphQLEnum("LockReason", "OFF_TOPIC", "RESOLVED", "SPAM", "TOO_HEATED")},
			"createdAt":        &graphql.Field{Type: graphql.NewNonNull(dateTime)},
			"updatedAt":        &graphql.Field{Type: graphql.NewNonNull(dateTime)},
			"closedAt":         &graphql.Field{Type: dateTime},
			"reviewDecision":   &graphql.Field{Type: pullRequestReviewDecisionEnum},
			"author": &graphql.Field{
				Type: s.graphqlTypes.actor,
				Resolve: func(p graphql.ResolveParams) (interface{}, error) {
					pr, ok := p.Source.(map[string]interface{})
					if !ok {
						return nil, fmt.Errorf("resolve source: unexpected type %T", p.Source)
					}
					return pr["author"], nil
				},
			},
			"mergedBy": &graphql.Field{
				Type: s.graphqlTypes.actor,
				Resolve: func(p graphql.ResolveParams) (interface{}, error) {
					pr, ok := p.Source.(map[string]interface{})
					if !ok {
						return nil, fmt.Errorf("resolve source: unexpected type %T", p.Source)
					}
					return pr["mergedBy"], nil
				},
			},
			"labels": &graphql.Field{
				Type: prLabelConnectionType,
				Args: relayConnectionArgs(),
				Resolve: func(p graphql.ResolveParams) (interface{}, error) {
					pr, ok := p.Source.(map[string]interface{})
					if !ok {
						return nil, fmt.Errorf("resolve source: unexpected type %T", p.Source)
					}
					return repaginateConnection(pr["labels"], p.Args), nil
				},
			},
			"assignees": &graphql.Field{
				Type: graphql.NewNonNull(prAssigneeConnectionType),
				Args: relayConnectionArgs(),
				Resolve: func(p graphql.ResolveParams) (interface{}, error) {
					pr, ok := p.Source.(map[string]interface{})
					if !ok {
						return nil, fmt.Errorf("resolve source: unexpected type %T", p.Source)
					}
					return repaginateConnection(pr["assignees"], p.Args), nil
				},
			},
			"reviews": &graphql.Field{
				Type: prReviewConnectionType,
				Args: graphql.FieldConfigArgument{
					"first":  &graphql.ArgumentConfig{Type: graphql.Int},
					"last":   &graphql.ArgumentConfig{Type: graphql.Int},
					"after":  &graphql.ArgumentConfig{Type: graphql.String},
					"before": &graphql.ArgumentConfig{Type: graphql.String},
				},
				Resolve: func(p graphql.ResolveParams) (interface{}, error) {
					pr, ok := p.Source.(map[string]interface{})
					if !ok {
						return nil, fmt.Errorf("resolve source: unexpected type %T", p.Source)
					}
					return repaginateConnection(pr["reviews"], p.Args), nil
				},
			},
			"reviewRequests": &graphql.Field{
				Type: reviewRequestConnectionType,
				Args: relayConnectionArgs(),
				Resolve: func(p graphql.ResolveParams) (interface{}, error) {
					pr, ok := p.Source.(map[string]interface{})
					if !ok {
						return nil, fmt.Errorf("resolve source: unexpected type %T", p.Source)
					}
					return repaginateConnection(pr["reviewRequests"], p.Args), nil
				},
			},
			"comments": &graphql.Field{
				Type: graphql.NewNonNull(prCommentConnectionType),
				Args: graphql.FieldConfigArgument{
					"first":  &graphql.ArgumentConfig{Type: graphql.Int},
					"last":   &graphql.ArgumentConfig{Type: graphql.Int},
					"after":  &graphql.ArgumentConfig{Type: graphql.String},
					"before": &graphql.ArgumentConfig{Type: graphql.String},
				},
				Resolve: func(p graphql.ResolveParams) (interface{}, error) {
					pr, ok := p.Source.(map[string]interface{})
					if !ok {
						return nil, fmt.Errorf("resolve source: unexpected type %T", p.Source)
					}
					return repaginateConnection(pr["comments"], p.Args), nil
				},
			},
			"reviewThreads": &graphql.Field{
				Type: graphql.NewNonNull(prReviewThreadConnectionType),
				Args: relayConnectionArgs(),
				Resolve: func(p graphql.ResolveParams) (interface{}, error) {
					pr, ok := p.Source.(map[string]interface{})
					if !ok {
						return nil, fmt.Errorf("resolve source: unexpected type %T", p.Source)
					}
					return repaginateConnection(pr["reviewThreads"], p.Args), nil
				},
			},
			// The real items the PR was added to via addProjectV2ItemById.
			"projectItems": &graphql.Field{
				Type: s.projectV2ItemConnectionType(),
				Args: relayConnectionArgs(),
				Resolve: func(p graphql.ResolveParams) (interface{}, error) {
					pr, ok := p.Source.(map[string]interface{})
					if !ok {
						return nil, fmt.Errorf("resolve source: unexpected type %T", p.Source)
					}
					prID, _ := pr["databaseId"].(int)
					items := s.store.ProjectsV2.ListItemsForPR(prID)
					nodes := make([]map[string]interface{}, 0, len(items))
					for _, it := range items {
						nodes = append(nodes, projectV2ItemToGQL(it, s.store))
					}
					return paginateGQLMaps(nodes, p.Args), nil
				},
			},
			// pullRequestToGQL embeds the resolved milestone map (or nil).
			"milestone": &graphql.Field{
				Type: milestoneType,
				Resolve: func(p graphql.ResolveParams) (interface{}, error) {
					pr, ok := p.Source.(map[string]interface{})
					if !ok {
						return nil, fmt.Errorf("resolve source: unexpected type %T", p.Source)
					}
					m, ok := pr["milestone"].(map[string]interface{})
					if !ok || m == nil {
						return nil, nil
					}
					return m, nil
				},
			},
			// gh reads check state through commits(last:1){commit{...}};
			// PullRequest has no top-level statusCheckRollup. Nodes carry the
			// same real git commits the REST surface reports.
			"commits": &graphql.Field{
				Type: graphql.NewNonNull(prCommitConnectionType),
				Args: relayConnectionArgs(),
				Resolve: func(p graphql.ResolveParams) (interface{}, error) {
					pr, ok := p.Source.(map[string]interface{})
					if !ok {
						return nil, fmt.Errorf("resolve source: unexpected type %T", p.Source)
					}
					return repaginateConnection(pr["commits"], p.Args), nil
				},
			},
			"reactionGroups": &graphql.Field{
				Type: graphql.NewList(graphql.NewNonNull(prReactionGroupType)),
				Resolve: func(p graphql.ResolveParams) (interface{}, error) {
					pr, ok := p.Source.(map[string]interface{})
					if !ok {
						return nil, fmt.Errorf("resolve source: unexpected type %T", p.Source)
					}
					return pr["reactionGroups"], nil
				},
			},
			"closed": &graphql.Field{
				Type: graphql.NewNonNull(graphql.Boolean),
				Resolve: func(p graphql.ResolveParams) (interface{}, error) {
					pr, ok := p.Source.(map[string]interface{})
					if !ok {
						return nil, fmt.Errorf("resolve source: unexpected type %T", p.Source)
					}
					state, _ := pr["state"].(string)
					return state == "CLOSED" || state == "MERGED", nil
				},
			},
			"headRepository": &graphql.Field{
				Type: repoType,
				Resolve: func(p graphql.ResolveParams) (interface{}, error) {
					pr, ok := p.Source.(map[string]interface{})
					if !ok {
						return nil, fmt.Errorf("resolve source: unexpected type %T", p.Source)
					}
					return pr["headRepository"], nil
				},
			},
			// The repository opened against, distinct from headRepository (the
			// fork). gh's shared project-item fragment selects it.
			"repository": &graphql.Field{
				Type: graphql.NewNonNull(repoType),
				Resolve: func(p graphql.ResolveParams) (interface{}, error) {
					pr, ok := p.Source.(map[string]interface{})
					if !ok {
						return nil, fmt.Errorf("resolve source: unexpected type %T", p.Source)
					}
					return pr["repository"], nil
				},
			},
			"headRepositoryOwner": &graphql.Field{
				Type: s.graphqlTypes.repositoryOwner,
				Resolve: func(p graphql.ResolveParams) (interface{}, error) {
					pr, ok := p.Source.(map[string]interface{})
					if !ok {
						return nil, fmt.Errorf("resolve source: unexpected type %T", p.Source)
					}
					return pr["headRepositoryOwner"], nil
				},
			},
			"isCrossRepository": &graphql.Field{
				Type: graphql.NewNonNull(graphql.Boolean),
				Resolve: func(p graphql.ResolveParams) (interface{}, error) {
					pr, ok := p.Source.(map[string]interface{})
					if !ok {
						return nil, fmt.Errorf("resolve source: unexpected type %T", p.Source)
					}
					v, ok := pr["isCrossRepository"].(bool)
					if !ok {
						return nil, fmt.Errorf("pull request source missing isCrossRepository")
					}
					return v, nil
				},
			},
			"maintainerCanModify": &graphql.Field{
				Type: graphql.NewNonNull(graphql.Boolean),
				Resolve: func(p graphql.ResolveParams) (interface{}, error) {
					pr, ok := p.Source.(map[string]interface{})
					if !ok {
						return nil, fmt.Errorf("resolve source: unexpected type %T", p.Source)
					}
					v, ok := pr["maintainerCanModify"].(bool)
					if !ok {
						return nil, fmt.Errorf("pull request source missing maintainerCanModify")
					}
					return v, nil
				},
			},
			"autoMergeRequest": &graphql.Field{
				// The armed auto-merge request, or null when not enabled.
				Type: autoMergeRequestType,
				Resolve: func(p graphql.ResolveParams) (interface{}, error) {
					pr, _, err := pullRequestAndRepoFromGQLSource(p.Source, s.store)
					if err != nil {
						return nil, err
					}
					// A typed-nil map would make graphql-go descend into it.
					if request := autoMergeRequestToGQL(pr, s.store); request != nil {
						return request, nil
					}
					return nil, nil
				},
			},
			"baseRefOid": &graphql.Field{Type: graphql.NewNonNull(gitObjectID)},
			"fullDatabaseId": &graphql.Field{
				// BigInt scalar — serializes as a string.
				Type: s.graphQLStringScalar("BigInt"),
				Resolve: func(p graphql.ResolveParams) (interface{}, error) {
					pr, ok := p.Source.(map[string]interface{})
					if !ok {
						return nil, fmt.Errorf("resolve source: unexpected type %T", p.Source)
					}
					id, _ := pr["databaseId"].(int)
					return strconv.Itoa(id), nil
				},
			},
			"files": &graphql.Field{
				Type: changedFileConnType,
				Args: graphql.FieldConfigArgument{
					"first": &graphql.ArgumentConfig{Type: graphql.Int},
					"after": &graphql.ArgumentConfig{Type: graphql.String},
				},
				Resolve: func(p graphql.ResolveParams) (interface{}, error) {
					pr, repo, err := pullRequestAndRepoFromGQLSource(p.Source, s.store)
					if err != nil {
						return nil, err
					}
					files, err := s.pullRequestChangedFiles(repo, pr, "")
					if err != nil {
						return nil, err
					}
					nodes := make([]map[string]interface{}, 0, len(files))
					for _, file := range files {
						node := pullRequestChangedFileToGQL(file)
						// prID travels with each node for viewerViewedState.
						node["prID"] = pr.ID
						nodes = append(nodes, node)
					}
					return repaginateConnection(map[string]interface{}{"nodes": nodes}, p.Args), nil
				},
			},
			"closingIssuesReferences": &graphql.Field{
				// The shared IssueConnection; nodes are full issueToGQL maps.
				Type: s.gqlIssueConnectionType(issueType),
				Args: graphql.FieldConfigArgument{
					"first": &graphql.ArgumentConfig{Type: graphql.Int},
					"after": &graphql.ArgumentConfig{Type: graphql.String},
				},
				Resolve: func(p graphql.ResolveParams) (interface{}, error) {
					pr, repo, err := pullRequestAndRepoFromGQLSource(p.Source, s.store)
					if err != nil {
						return nil, err
					}
					issues := closingIssuesForPullRequest(s.store, repo, pr)
					first, _ := intArg(p.Args, "first")
					after, _ := p.Args["after"].(string)
					return paginateGQL(issues, first, after, func(issue *store.Issue) map[string]interface{} {
						return issueToGQL(issue, s.store)
					}, func(issue *store.Issue) string { return issue.NodeID }), nil
				},
			},
			"mergeCommit": &graphql.Field{
				Type: commitType,
				Resolve: func(p graphql.ResolveParams) (interface{}, error) {
					source, ok := p.Source.(map[string]interface{})
					if !ok {
						return nil, fmt.Errorf("resolve source: unexpected type %T", p.Source)
					}
					return source["mergeCommit"], nil
				},
			},
			"potentialMergeCommit": &graphql.Field{
				Type: commitType,
				Resolve: func(p graphql.ResolveParams) (interface{}, error) {
					return nil, nil
				},
			},
			// The newest review per author. Shares PullRequestReviewConnection
			// with `reviews`; repaginateConnection synthesizes the pageInfo.
			"latestReviews": &graphql.Field{
				Type: prReviewConnectionType,
				Args: relayConnectionArgs(),
				Resolve: func(p graphql.ResolveParams) (interface{}, error) {
					pr, ok := p.Source.(map[string]interface{})
					if !ok {
						return nil, fmt.Errorf("resolve source: unexpected type %T", p.Source)
					}
					return repaginateConnection(pr["latestReviews"], p.Args), nil
				},
			},
			"mergeStateStatus": &graphql.Field{
				Type: graphql.NewNonNull(mergeStateStatusEnum),
				Resolve: func(p graphql.ResolveParams) (interface{}, error) {
					pr, ok := p.Source.(map[string]interface{})
					if !ok {
						return nil, fmt.Errorf("resolve source: unexpected type %T", p.Source)
					}
					return pr["mergeStateStatus"], nil
				},
			},
			// gh pr status selects baseRef{branchProtectionRule{...}}; the rule
			// is resolved eagerly and embedded in the source map.
			"baseRef": &graphql.Field{
				Type: s.gqlRefType(),
				Resolve: func(p graphql.ResolveParams) (interface{}, error) {
					pr, ok := p.Source.(map[string]interface{})
					if !ok {
						return nil, fmt.Errorf("resolve source: unexpected type %T", p.Source)
					}
					base, _ := pr["baseRefName"].(string)
					ref := map[string]interface{}{
						"name":          base,
						"prefix":        "refs/heads/",
						"qualifiedName": "refs/heads/" + base,
					}
					prID, _ := pr["databaseId"].(int)
					if prObj := s.store.GetPullRequest(prID); prObj != nil {
						if repo := s.store.GetRepoByID(prObj.RepoID); repo != nil {
							ref["repoFullName"] = repo.FullName
							if rule := s.branchProtectionRuleForPR(repo, prObj.BaseRefName); rule != nil {
								ref["branchProtectionRule"] = rule
							}
						}
					}
					return ref, nil
				},
			},
		},
	})

	// PullRequest back-references for types minted before pullRequestType,
	// resolving from the "prID" the source carries.
	prBackRef := func() *graphql.Field {
		return &graphql.Field{
			Type: graphql.NewNonNull(pullRequestType),
			Resolve: func(p graphql.ResolveParams) (interface{}, error) {
				if prID, ok := srcMap(p)["prID"].(int); ok && prID > 0 {
					if pr := s.store.GetPullRequest(prID); pr != nil {
						return pullRequestToGQL(pr, s.store), nil
					}
				}
				return nil, nil
			},
		}
	}
	pullRequestCommitType.AddFieldConfig("pullRequest", prBackRef())
	reviewRequestType.AddFieldConfig("pullRequest", prBackRef())
	autoMergeRequestType.AddFieldConfig("pullRequest", prBackRef())

	// Registered for interface ResolveType dispatch (Lockable).
	s.graphqlTypes.pullRequest = pullRequestType
	s.graphqlTypes.pullRequestReview = prReviewType
	s.graphqlTypes.pullRequestReviewComment = prReviewCommentType
	// Memoized: the addPullRequestReviewThread payload returns it.
	s.graphqlTypes.pullRequestReviewThread = prReviewThreadType
	// Both are members of PullRequestTimelineItems, which is assembled later.
	s.graphqlTypes.pullRequestCommit = pullRequestCommitType
	s.graphqlTypes.pullRequestReviewThread = prReviewThreadType
	s.addReactableFields(pullRequestType, "pull_request")
	s.addReactableFields(prReviewType, "pull_request_review")
	s.addReactableFields(prReviewCommentType, "pull_request_review_comment")

	// Complete the three pull-request types' remaining fields
	// (gh_pulls_fields_graphql.go).
	s.addPullRequestSurfaceFields(prSurfaceDeps{
		pullRequest:          pullRequestType,
		review:               prReviewType,
		thread:               prReviewThreadType,
		reviewConnection:     prReviewConnectionType,
		reviewCommentConn:    prReviewCommentConnectionType,
		statusCheckRollup:    statusCheckRollupType,
		reviewRequest:        reviewRequestType,
		userType:             userType,
		repoType:             repoType,
		uri:                  uri,
		dateTime:             dateTime,
		bigInt:               s.graphQLStringScalar("BigInt"),
		htmlScalar:           s.graphQLStringScalar("HTML"),
		commentAuthorAssoc:   commentAuthorAssociationEnum,
		subscriptionState:    s.sharedEnum("SubscriptionState", "IGNORED", "SUBSCRIBED", "UNSUBSCRIBED"),
		pullRequestMergeEnum: pullRequestMergeMethodEnum,
	})

	// PR Connection
	prEdgeType := graphql.NewObject(graphql.ObjectConfig{
		Name: "PullRequestEdge",
		Fields: graphql.Fields{
			"node":   &graphql.Field{Type: pullRequestType},
			"cursor": &graphql.Field{Type: graphql.NewNonNull(graphql.String)},
		},
	})

	prConnectionType := graphql.NewObject(graphql.ObjectConfig{
		Name: "PullRequestConnection",
		Fields: graphql.Fields{
			"nodes":      &graphql.Field{Type: graphql.NewList(pullRequestType)},
			"edges":      &graphql.Field{Type: graphql.NewList(prEdgeType)},
			"totalCount": &graphql.Field{Type: graphql.NewNonNull(graphql.Int)},
			"pageInfo":   &graphql.Field{Type: graphql.NewNonNull(s.gqlPageInfoType())},
		},
	})
	s.graphqlTypes.pullRequestConnection = prConnectionType

	// Add fields to Repository type

	repoType.AddFieldConfig("pullRequests", &graphql.Field{
		Type: graphql.NewNonNull(prConnectionType),
		Args: graphql.FieldConfigArgument{
			"first":       &graphql.ArgumentConfig{Type: graphql.Int},
			"after":       &graphql.ArgumentConfig{Type: graphql.String},
			"last":        &graphql.ArgumentConfig{Type: graphql.Int},
			"before":      &graphql.ArgumentConfig{Type: graphql.String},
			"states":      &graphql.ArgumentConfig{Type: graphql.NewList(graphql.NewNonNull(pullRequestStateEnum))},
			"labels":      &graphql.ArgumentConfig{Type: graphql.NewList(graphql.NewNonNull(graphql.String))},
			"headRefName": &graphql.ArgumentConfig{Type: graphql.String},
			"baseRefName": &graphql.ArgumentConfig{Type: graphql.String},
			// gh sends orderBy as literal enum names, so field/direction must be
			// enums (string inputs fail validation), like IssueOrder.
			"orderBy": &graphql.ArgumentConfig{Type: s.graphqlTypes.issueOrder},
		},
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			repo, ok := p.Source.(map[string]interface{})
			if !ok {
				return nil, fmt.Errorf("resolve source: unexpected type %T", p.Source)
			}
			repoID, _ := repo["databaseId"].(int)

			prs := s.store.ListPullRequests(repoID, "")

			if states, ok := p.Args["states"].([]interface{}); ok && len(states) > 0 {
				stateMap := make(map[string]bool)
				for _, st := range states {
					stateMap[fmt.Sprintf("%v", st)] = true
				}
				var filtered []*store.PullRequest
				for _, pr := range prs {
					if stateMap[pr.State] {
						filtered = append(filtered, pr)
					}
				}
				prs = filtered
			}

			if labelNames, ok := p.Args["labels"].([]interface{}); ok && len(labelNames) > 0 {
				var names []string
				for _, ln := range labelNames {
					names = append(names, fmt.Sprintf("%v", ln))
				}
				var filtered []*store.PullRequest
				for _, pr := range prs {
					if prHasAllLabels(s.store, pr, names) {
						filtered = append(filtered, pr)
					}
				}
				prs = filtered
			}

			if head, ok := p.Args["headRefName"].(string); ok && head != "" {
				var filtered []*store.PullRequest
				for _, pr := range prs {
					if pr.HeadRefName == head {
						filtered = append(filtered, pr)
					}
				}
				prs = filtered
			}

			if base, ok := p.Args["baseRefName"].(string); ok && base != "" {
				var filtered []*store.PullRequest
				for _, pr := range prs {
					if pr.BaseRefName == base {
						filtered = append(filtered, pr)
					}
				}
				prs = filtered
			}

			sortField := "CREATED_AT"
			sortDescending := true
			if order, ok := p.Args["orderBy"].(map[string]interface{}); ok {
				if field, ok := order["field"].(string); ok && field != "" {
					sortField = field
				}
				if direction, ok := order["direction"].(string); ok {
					sortDescending = direction != "ASC"
				}
			}
			sort.Slice(prs, func(a, b int) bool {
				left, right := prs[a].CreatedAt, prs[b].CreatedAt
				if sortField == "UPDATED_AT" {
					left, right = prs[a].UpdatedAt, prs[b].UpdatedAt
				}
				if left.Equal(right) {
					if sortDescending {
						return prs[a].ID > prs[b].ID
					}
					return prs[a].ID < prs[b].ID
				}
				if sortDescending {
					return left.After(right)
				}
				return left.Before(right)
			})
			nodes := make([]map[string]interface{}, 0, len(prs))
			for _, pr := range prs {
				nodes = append(nodes, pullRequestToGQL(pr, s.store))
			}
			return paginateGQLMaps(nodes, p.Args), nil
		},
	})

	repoType.AddFieldConfig("pullRequest", &graphql.Field{
		Type: pullRequestType,
		Args: graphql.FieldConfigArgument{
			"number": &graphql.ArgumentConfig{Type: graphql.NewNonNull(graphql.Int)},
		},
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			repo, ok := p.Source.(map[string]interface{})
			if !ok {
				return nil, fmt.Errorf("resolve source: unexpected type %T", p.Source)
			}
			repoID, _ := repo["databaseId"].(int)
			number, _ := intArg(p.Args, "number")

			pr := s.store.GetPullRequestByNumber(repoID, number)
			if pr == nil {
				// Typed NOT_FOUND, not bare null.
				return nil, &ghNotFoundError{
					message: fmt.Sprintf("Could not resolve to a PullRequest with the number of %d.", number),
				}
			}
			return pullRequestToGQL(pr, s.store), nil
		},
	})

	// A real Issue|PullRequest union so gh's `...on Issue`/`...on PullRequest`
	// fragments type-check.
	issueOrPRUnion := graphql.NewUnion(graphql.UnionConfig{
		Name:        "IssueOrPullRequest",
		Description: "Either an Issue or a PullRequest (matches GitHub's polymorphic lookup by number).",
		Types:       []*graphql.Object{issueType, pullRequestType},
		ResolveType: func(p graphql.ResolveTypeParams) *graphql.Object {
			if m, ok := p.Value.(map[string]interface{}); ok {
				if tn, _ := m["__typename"].(string); tn == "PullRequest" {
					return pullRequestType
				}
			}
			return issueType
		},
	})
	// ClosedEvent.duplicateOf names the same union.
	s.graphqlTypes.issueOrPullRequest = issueOrPRUnion
	repoType.AddFieldConfig("issueOrPullRequest", &graphql.Field{
		Type: issueOrPRUnion,
		Args: graphql.FieldConfigArgument{
			"number": &graphql.ArgumentConfig{Type: graphql.NewNonNull(graphql.Int)},
		},
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			repo, ok := p.Source.(map[string]interface{})
			if !ok {
				return nil, fmt.Errorf("resolve source: unexpected type %T", p.Source)
			}
			repoID, _ := repo["databaseId"].(int)
			number, _ := intArg(p.Args, "number")

			if issue := s.store.GetIssueByNumber(repoID, number); issue != nil {
				result := issueToGQL(issue, s.store)
				result["__typename"] = "Issue"
				return result, nil
			}
			if pr := s.store.GetPullRequestByNumber(repoID, number); pr != nil {
				result := pullRequestToGQL(pr, s.store)
				result["__typename"] = "PullRequest"
				return result, nil
			}
			// Typed NOT_FOUND, not bare null.
			return nil, &ghNotFoundError{
				message: fmt.Sprintf("Could not resolve to an issue or pull request with the number of %d.", number),
			}
		},
	})

	// Query.search
	// SearchType omits ISSUE_ADVANCED deliberately: gh introspects the enum and
	// opts into it only when present, so omitting it keeps gh on the plain
	// ISSUE backend.
	searchTypeEnum := graphql.NewEnum(graphql.EnumConfig{
		Name: "SearchType",
		Values: graphql.EnumValueConfigMap{
			"ISSUE":      &graphql.EnumValueConfig{Value: "ISSUE"},
			"REPOSITORY": &graphql.EnumValueConfig{Value: "REPOSITORY"},
		},
	})
	repositoryType := s.graphqlTypes.repository
	searchResultItemUnion := graphql.NewUnion(graphql.UnionConfig{
		Name:  "SearchResultItem",
		Types: []*graphql.Object{issueType, pullRequestType, repositoryType},
		ResolveType: func(p graphql.ResolveTypeParams) *graphql.Object {
			if m, ok := p.Value.(map[string]interface{}); ok {
				switch tn, _ := m["__typename"].(string); tn {
				case "PullRequest":
					return pullRequestType
				case "Repository":
					return repositoryType
				}
			}
			return issueType
		},
	})
	// TextMatch highlighting is unmodeled, so textMatches is always empty; the
	// types exist so the field type-checks.
	textMatchHighlightType := graphql.NewObject(graphql.ObjectConfig{
		Name: "TextMatchHighlight",
		Fields: graphql.Fields{
			"beginIndice": &graphql.Field{Type: graphql.NewNonNull(graphql.Int)},
			"endIndice":   &graphql.Field{Type: graphql.NewNonNull(graphql.Int)},
			"text":        &graphql.Field{Type: graphql.NewNonNull(graphql.String)},
		},
	})
	textMatchType := graphql.NewObject(graphql.ObjectConfig{
		Name: "TextMatch",
		Fields: graphql.Fields{
			"fragment":   &graphql.Field{Type: graphql.NewNonNull(graphql.String)},
			"property":   &graphql.Field{Type: graphql.NewNonNull(graphql.String)},
			"highlights": &graphql.Field{Type: graphql.NewNonNull(graphql.NewList(graphql.NewNonNull(textMatchHighlightType)))},
		},
	})
	searchResultEdgeType := graphql.NewObject(graphql.ObjectConfig{
		Name: "SearchResultItemEdge",
		Fields: graphql.Fields{
			"node":   &graphql.Field{Type: searchResultItemUnion},
			"cursor": &graphql.Field{Type: graphql.NewNonNull(graphql.String)},
			"textMatches": &graphql.Field{
				Type: graphql.NewList(textMatchType),
				Resolve: func(p graphql.ResolveParams) (interface{}, error) {
					return []interface{}{}, nil
				},
			},
		},
	})
	// Search resolves only ISSUE and REPOSITORY kinds, so
	// code/discussion/user/wiki counts are zero, and issueSearchType /
	// lexicalFallbackReason (one lexical pass, no typed engine) are null.
	searchCountField := func(sourceKey string) *graphql.Field {
		return &graphql.Field{
			Type: graphql.NewNonNull(graphql.Int),
			Resolve: func(p graphql.ResolveParams) (interface{}, error) {
				if c, ok := p.Source.(map[string]interface{}); ok {
					if v, ok := c[sourceKey]; ok && v != nil {
						return v, nil
					}
				}
				return 0, nil
			},
		}
	}
	issueSearchTypeEnum := s.sharedEnum("IssueSearchType", "HYBRID", "LEXICAL", "SEMANTIC")
	lexicalFallbackReasonEnum := s.sharedEnum("LexicalFallbackReason",
		"NON_ISSUE_TARGET", "NO_ACCESSIBLE_REPOS", "NO_TEXT_TERMS",
		"ONLY_NON_SEMANTIC_FIELDS_REQUESTED", "OR_BOOLEAN_NOT_SUPPORTED",
		"QUOTED_TEXT", "SERVER_ERROR")
	searchResultConnectionType := graphql.NewObject(graphql.ObjectConfig{
		Name: "SearchResultItemConnection",
		Fields: graphql.Fields{
			"issueCount": &graphql.Field{
				Type: graphql.NewNonNull(graphql.Int),
				Resolve: func(p graphql.ResolveParams) (interface{}, error) {
					c, ok := p.Source.(map[string]interface{})
					if !ok {
						return nil, fmt.Errorf("resolve source: unexpected type %T", p.Source)
					}
					return c["totalCount"], nil
				},
			},
			"repositoryCount": &graphql.Field{
				Type: graphql.NewNonNull(graphql.Int),
				Resolve: func(p graphql.ResolveParams) (interface{}, error) {
					c, ok := p.Source.(map[string]interface{})
					if !ok {
						return nil, fmt.Errorf("resolve source: unexpected type %T", p.Source)
					}
					return c["repositoryCount"], nil
				},
			},
			"codeCount":       searchCountField("codeCount"),
			"discussionCount": searchCountField("discussionCount"),
			"userCount":       searchCountField("userCount"),
			"wikiCount":       searchCountField("wikiCount"),
			"issueSearchType": &graphql.Field{
				Type:    issueSearchTypeEnum,
				Resolve: func(p graphql.ResolveParams) (interface{}, error) { return nil, nil },
			},
			"lexicalFallbackReason": &graphql.Field{
				Type:    graphql.NewList(graphql.NewNonNull(lexicalFallbackReasonEnum)),
				Resolve: func(p graphql.ResolveParams) (interface{}, error) { return nil, nil },
			},
			"nodes":    &graphql.Field{Type: graphql.NewList(searchResultItemUnion)},
			"edges":    &graphql.Field{Type: graphql.NewList(searchResultEdgeType)},
			"pageInfo": &graphql.Field{Type: graphql.NewNonNull(s.gqlPageInfoType())},
		},
	})
	queryType.AddFieldConfig("search", &graphql.Field{
		Type: graphql.NewNonNull(searchResultConnectionType),
		Args: graphql.FieldConfigArgument{
			"query":  &graphql.ArgumentConfig{Type: graphql.NewNonNull(graphql.String)},
			"type":   &graphql.ArgumentConfig{Type: graphql.NewNonNull(searchTypeEnum)},
			"first":  &graphql.ArgumentConfig{Type: graphql.Int},
			"last":   &graphql.ArgumentConfig{Type: graphql.Int},
			"after":  &graphql.ArgumentConfig{Type: graphql.String},
			"before": &graphql.ArgumentConfig{Type: graphql.String},
		},
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			q, _ := p.Args["query"].(string)
			searchType, _ := p.Args["type"].(string)
			viewer := s.ghUserFromContext(p.Context)
			switch searchType {
			case "ISSUE":
				connection := paginateGQLItems(s.searchIssuesAndPRs(q, viewer), p.Args)
				connection["repositoryCount"] = 0
				return connection, nil
			case "REPOSITORY":
				items := s.searchRepositories(p.Context, q, viewer)
				connection := paginateGQLItems(items, p.Args)
				// Each count is its own kind; the unsearched one is zero.
				connection["repositoryCount"] = connection["totalCount"]
				connection["totalCount"] = 0
				return connection, nil
			}
			empty := paginateGQLMaps(nil, p.Args)
			empty["repositoryCount"] = 0
			return empty, nil
		},
	})

	// Mutations

	createPRInputType := graphql.NewInputObject(graphql.InputObjectConfig{
		Name: "CreatePullRequestInput",
		Fields: graphql.InputObjectConfigFieldMap{
			"repositoryId":        &graphql.InputObjectFieldConfig{Type: graphql.NewNonNull(graphql.ID)},
			"title":               &graphql.InputObjectFieldConfig{Type: graphql.NewNonNull(graphql.String)},
			"body":                &graphql.InputObjectFieldConfig{Type: graphql.String},
			"headRefName":         &graphql.InputObjectFieldConfig{Type: graphql.NewNonNull(graphql.String)},
			"headRepositoryId":    &graphql.InputObjectFieldConfig{Type: graphql.ID},
			"baseRefName":         &graphql.InputObjectFieldConfig{Type: graphql.NewNonNull(graphql.String)},
			"draft":               &graphql.InputObjectFieldConfig{Type: graphql.Boolean},
			"maintainerCanModify": &graphql.InputObjectFieldConfig{Type: graphql.Boolean},
		},
	})

	createPRPayloadType := graphql.NewObject(graphql.ObjectConfig{
		Name: "CreatePullRequestPayload",
		Fields: graphql.Fields{
			"pullRequest": &graphql.Field{Type: pullRequestType},
		},
	})

	s.registerMutation(mutationType, "createPullRequest", &graphql.Field{
		Type: createPRPayloadType,
		Args: graphql.FieldConfigArgument{
			"input": &graphql.ArgumentConfig{Type: graphql.NewNonNull(createPRInputType)},
		},
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			user := s.ghUserFromContext(p.Context)

			input, _ := p.Args["input"].(map[string]interface{})
			repoNodeID, _ := input["repositoryId"].(string)
			title, _ := input["title"].(string)
			body, _ := input["body"].(string)
			headRefName, _ := input["headRefName"].(string)
			baseRefName, _ := input["baseRefName"].(string)
			draft, _ := input["draft"].(bool)
			maintainerCanModify, _ := input["maintainerCanModify"].(bool)

			repo := store.FindRepoByNodeID(s.store, repoNodeID)
			if repo == nil {
				return nil, gqlMissingNode("Repository", repoNodeID)
			}

			headRepo, headRefName := store.ResolvePullRequestHead(s.store, repo, headRefName)
			if headRepo == nil || headRefName == "" {
				return nil, fmt.Errorf("pull request creation failed")
			}
			pr, err := s.store.CreatePullRequestChecked(repo.ID, user.ID, title, body, headRefName, baseRefName, draft, nil, nil, 0, store.PullRequestOptions{
				HeadRepoID:          headRepo.ID,
				MaintainerCanModify: maintainerCanModify,
			})
			if errors.Is(err, store.ErrOpenPullRequestExists) {
				return nil, fmt.Errorf("a pull request already exists for this head and base")
			}
			if pr == nil {
				return nil, fmt.Errorf("pull request creation failed")
			}
			// Collect CODEOWNERS reviewers, as REST does.
			s.autoRequestCodeOwners(repo, pr, user)
			if updated := s.store.GetPullRequest(pr.ID); updated != nil {
				pr = updated
			}

			return map[string]interface{}{
				"pullRequest": pullRequestToGQL(pr, s.store),
			}, nil
		},
	})

	closePRInputType := graphql.NewInputObject(graphql.InputObjectConfig{
		Name: "ClosePullRequestInput",
		Fields: graphql.InputObjectConfigFieldMap{
			"pullRequestId": &graphql.InputObjectFieldConfig{Type: graphql.NewNonNull(graphql.ID)},
		},
	})

	closePRPayloadType := graphql.NewObject(graphql.ObjectConfig{
		Name: "ClosePullRequestPayload",
		Fields: graphql.Fields{
			"pullRequest": &graphql.Field{Type: pullRequestType},
		},
	})

	s.registerMutation(mutationType, "closePullRequest", &graphql.Field{
		Type: closePRPayloadType,
		Args: graphql.FieldConfigArgument{
			"input": &graphql.ArgumentConfig{Type: graphql.NewNonNull(closePRInputType)},
		},
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			user := s.ghUserFromContext(p.Context)

			input, _ := p.Args["input"].(map[string]interface{})
			prNodeID, _ := input["pullRequestId"].(string)

			pr := store.FindPullRequestByNodeID(s.store, prNodeID)
			if pr == nil {
				return nil, gqlMissingNodeType("PullRequest")
			}

			priorState := pr.State
			s.store.UpdatePullRequest(pr.ID, func(p *store.PullRequest) {
				p.State = "CLOSED"
				now := time.Now()
				p.ClosedAt = &now
			})
			if priorState == "OPEN" {
				s.store.RecordPullRequestEvent(pr.RepoID, pr.ID, user.ID, "closed", "", 0)
			}

			updated := s.store.GetPullRequest(pr.ID)
			s.emitPullRequestAction(updated, user, "closed", priorState == "OPEN")
			return map[string]interface{}{
				"pullRequest": pullRequestToGQL(updated, s.store),
			}, nil
		},
	})

	readyForReviewInputType := graphql.NewInputObject(graphql.InputObjectConfig{
		Name: "MarkPullRequestReadyForReviewInput",
		Fields: graphql.InputObjectConfigFieldMap{
			"pullRequestId":    &graphql.InputObjectFieldConfig{Type: graphql.NewNonNull(graphql.ID)},
			"clientMutationId": &graphql.InputObjectFieldConfig{Type: graphql.String},
		},
	})
	readyForReviewPayloadType := graphql.NewObject(graphql.ObjectConfig{
		Name: "MarkPullRequestReadyForReviewPayload",
		Fields: graphql.Fields{
			"pullRequest":      &graphql.Field{Type: pullRequestType},
			"clientMutationId": &graphql.Field{Type: graphql.String},
		},
	})
	s.registerMutation(mutationType, "markPullRequestReadyForReview", &graphql.Field{
		Type: readyForReviewPayloadType,
		Args: graphql.FieldConfigArgument{
			"input": &graphql.ArgumentConfig{Type: graphql.NewNonNull(readyForReviewInputType)},
		},
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			user := s.ghUserFromContext(p.Context)
			input, _ := p.Args["input"].(map[string]interface{})
			prNodeID, _ := input["pullRequestId"].(string)
			pr := store.FindPullRequestByNodeID(s.store, prNodeID)
			if pr == nil {
				return nil, gqlMissingNodeType("PullRequest")
			}
			wasDraft := pr.IsDraft
			s.store.UpdatePullRequest(pr.ID, func(p *store.PullRequest) { p.IsDraft = false })
			if wasDraft && user != nil {
				s.store.RecordPullRequestEvent(pr.RepoID, pr.ID, user.ID, "ready_for_review", "", 0)
			}
			s.emitPullRequestAction(s.store.GetPullRequest(pr.ID), user, "ready_for_review", wasDraft)
			return map[string]interface{}{
				"pullRequest":      pullRequestToGQL(s.store.GetPullRequest(pr.ID), s.store),
				"clientMutationId": input["clientMutationId"],
			}, nil
		},
	})

	convertToDraftInputType := graphql.NewInputObject(graphql.InputObjectConfig{
		Name: "ConvertPullRequestToDraftInput",
		Fields: graphql.InputObjectConfigFieldMap{
			"pullRequestId":    &graphql.InputObjectFieldConfig{Type: graphql.NewNonNull(graphql.ID)},
			"clientMutationId": &graphql.InputObjectFieldConfig{Type: graphql.String},
		},
	})
	convertToDraftPayloadType := graphql.NewObject(graphql.ObjectConfig{
		Name: "ConvertPullRequestToDraftPayload",
		Fields: graphql.Fields{
			"pullRequest":      &graphql.Field{Type: pullRequestType},
			"clientMutationId": &graphql.Field{Type: graphql.String},
		},
	})
	s.registerMutation(mutationType, "convertPullRequestToDraft", &graphql.Field{
		Type: convertToDraftPayloadType,
		Args: graphql.FieldConfigArgument{
			"input": &graphql.ArgumentConfig{Type: graphql.NewNonNull(convertToDraftInputType)},
		},
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			user := s.ghUserFromContext(p.Context)
			input, _ := p.Args["input"].(map[string]interface{})
			prNodeID, _ := input["pullRequestId"].(string)
			pr := store.FindPullRequestByNodeID(s.store, prNodeID)
			if pr == nil {
				return nil, gqlMissingNodeType("PullRequest")
			}
			wasReady := !pr.IsDraft
			s.store.UpdatePullRequest(pr.ID, func(p *store.PullRequest) { p.IsDraft = true })
			if wasReady && user != nil {
				s.store.RecordPullRequestEvent(pr.RepoID, pr.ID, user.ID, "convert_to_draft", "", 0)
			}
			// Timeline event `convert_to_draft`, webhook action `converted_to_draft`.
			s.emitPullRequestAction(s.store.GetPullRequest(pr.ID), user, "converted_to_draft", wasReady)
			return map[string]interface{}{
				"pullRequest":      pullRequestToGQL(s.store.GetPullRequest(pr.ID), s.store),
				"clientMutationId": input["clientMutationId"],
			}, nil
		},
	})

	reopenPRInputType := graphql.NewInputObject(graphql.InputObjectConfig{
		Name: "ReopenPullRequestInput",
		Fields: graphql.InputObjectConfigFieldMap{
			"pullRequestId": &graphql.InputObjectFieldConfig{Type: graphql.NewNonNull(graphql.ID)},
		},
	})

	reopenPRPayloadType := graphql.NewObject(graphql.ObjectConfig{
		Name: "ReopenPullRequestPayload",
		Fields: graphql.Fields{
			"pullRequest": &graphql.Field{Type: pullRequestType},
		},
	})

	s.registerMutation(mutationType, "reopenPullRequest", &graphql.Field{
		Type: reopenPRPayloadType,
		Args: graphql.FieldConfigArgument{
			"input": &graphql.ArgumentConfig{Type: graphql.NewNonNull(reopenPRInputType)},
		},
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			user := s.ghUserFromContext(p.Context)

			input, _ := p.Args["input"].(map[string]interface{})
			prNodeID, _ := input["pullRequestId"].(string)

			pr := store.FindPullRequestByNodeID(s.store, prNodeID)
			if pr == nil {
				return nil, gqlMissingNodeType("PullRequest")
			}

			if pr.State == "MERGED" {
				return nil, fmt.Errorf("pull request is merged and cannot be reopened")
			}

			priorState := pr.State
			s.store.UpdatePullRequest(pr.ID, func(p *store.PullRequest) {
				p.State = "OPEN"
				p.ClosedAt = nil
			})
			if priorState == "CLOSED" {
				s.store.RecordPullRequestEvent(pr.RepoID, pr.ID, user.ID, "reopened", "", 0)
			}

			updated := s.store.GetPullRequest(pr.ID)
			s.emitPullRequestAction(updated, user, "reopened", priorState == "CLOSED")
			return map[string]interface{}{
				"pullRequest": pullRequestToGQL(updated, s.store),
			}, nil
		},
	})

	mergePRInputType := graphql.NewInputObject(graphql.InputObjectConfig{
		Name: "MergePullRequestInput",
		Fields: graphql.InputObjectConfigFieldMap{
			"pullRequestId":    &graphql.InputObjectFieldConfig{Type: graphql.NewNonNull(graphql.ID)},
			"mergeMethod":      &graphql.InputObjectFieldConfig{Type: pullRequestMergeMethodEnum},
			"commitHeadline":   &graphql.InputObjectFieldConfig{Type: graphql.String},
			"commitBody":       &graphql.InputObjectFieldConfig{Type: graphql.String},
			"authorEmail":      &graphql.InputObjectFieldConfig{Type: graphql.String},
			"expectedHeadOid":  &graphql.InputObjectFieldConfig{Type: gitObjectID},
			"clientMutationId": &graphql.InputObjectFieldConfig{Type: graphql.String},
		},
	})

	mergePRPayloadType := graphql.NewObject(graphql.ObjectConfig{
		Name: "MergePullRequestPayload",
		Fields: graphql.Fields{
			"pullRequest": &graphql.Field{Type: pullRequestType},
			"actor":       &graphql.Field{Type: s.graphqlTypes.actor},
			"clientMutationId": &graphql.Field{
				Type: graphql.String,
				Resolve: func(p graphql.ResolveParams) (interface{}, error) {
					m, ok := p.Source.(map[string]interface{})
					if !ok {
						return nil, fmt.Errorf("resolve source: unexpected type %T", p.Source)
					}
					return m["clientMutationId"], nil
				},
			},
		},
	})

	s.registerMutation(mutationType, "mergePullRequest", &graphql.Field{
		Type: mergePRPayloadType,
		Args: graphql.FieldConfigArgument{
			"input": &graphql.ArgumentConfig{Type: graphql.NewNonNull(mergePRInputType)},
		},
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			user := s.ghUserFromContext(p.Context)

			input, _ := p.Args["input"].(map[string]interface{})
			prNodeID, _ := input["pullRequestId"].(string)

			pr := store.FindPullRequestByNodeID(s.store, prNodeID)
			if pr == nil {
				return nil, gqlMissingNodeType("PullRequest")
			}

			if pr.State != "OPEN" {
				return nil, fmt.Errorf("pull request is not open")
			}

			repo := s.store.GetRepoByID(pr.RepoID)
			if repo == nil {
				return nil, gqlMissingNodeType("Repository")
			}

			// expectedHeadOid (--match-head-commit): a moved or unresolvable
			// head refuses rather than merge code nobody reviewed.
			if expected, ok := input["expectedHeadOid"].(string); ok && expected != "" {
				head := s.prHeadSha(repo, pr)
				if head == "" || !strings.EqualFold(head, expected) {
					//lint:ignore ST1005 GitHub GraphQL parity requires this exact upstream message.
					return nil, fmt.Errorf("Head branch was modified. Review and try the merge again.")
				}
			}

			// Evaluate required checks unconditionally, not only via
			// canMergePullRequest (which sits behind branch protection's admin
			// bypass) — otherwise an admin could merge a red PR through GraphQL
			// that REST refuses.
			if headSha := s.prHeadSha(repo, pr); headSha != "" {
				if missing := s.missingRequiredChecks(repo, pr.BaseRefName, headSha); len(missing) > 0 {
					//lint:ignore ST1005 GitHub GraphQL parity requires this exact upstream message.
					return nil, fmt.Errorf("Required status check %q is expected.", missing[0])
				}
			}

			if ok, msg := s.canMergePullRequest(p.Context, repo, pr); !ok {
				if msg == "" {
					msg = "Pull Request is not mergeable"
				}
				return nil, fmt.Errorf("%s", msg)
			}

			method := "merge"
			if v, ok := input["mergeMethod"].(string); ok && v != "" {
				method = strings.ToLower(v)
			}
			commitHeadline, _ := input["commitHeadline"].(string)
			commitBody, _ := input["commitBody"].(string)
			// authorEmail is the merge commit's recorded address.
			merger := *user
			if v, ok := input["authorEmail"].(string); ok && v != "" {
				merger.Email = v
			}
			if _, errMsg := s.completePullRequestMerge(repo, pr, &merger, method, commitHeadline, commitBody, s.prHeadSha(repo, pr)); errMsg != "" {
				return nil, fmt.Errorf("%s", errMsg)
			}

			updated := s.store.GetPullRequest(pr.ID)
			mergedPayload := s.buildPullRequestPayload(repo, updated, user, "closed")
			s.emitWebhookEvent(repo.FullName, "pull_request", "closed", mergedPayload)

			var clientMutationID interface{}
			if v, ok := input["clientMutationId"].(string); ok && v != "" {
				clientMutationID = v
			}
			return map[string]interface{}{
				"pullRequest":      pullRequestToGQL(updated, s.store),
				"actor":            optionalRendered(user, userToGraphQL),
				"clientMutationId": clientMutationID,
			}, nil
		},
	})

	// enablePullRequestAutoMerge / disablePullRequestAutoMerge
	// Auto-merge arms a merge that runs later through the REST-shared merge gate.
	// It can only arm while something blocks the merge — a PR that could merge
	// now is refused ("Pull request is in clean status") — so an enable never
	// races a green check.
	enableAutoMergeInputType := graphql.NewInputObject(graphql.InputObjectConfig{
		Name: "EnablePullRequestAutoMergeInput",
		Fields: graphql.InputObjectConfigFieldMap{
			"pullRequestId":    &graphql.InputObjectFieldConfig{Type: graphql.NewNonNull(graphql.ID)},
			"mergeMethod":      &graphql.InputObjectFieldConfig{Type: pullRequestMergeMethodEnum, DefaultValue: "MERGE"},
			"commitHeadline":   &graphql.InputObjectFieldConfig{Type: graphql.String},
			"commitBody":       &graphql.InputObjectFieldConfig{Type: graphql.String},
			"authorEmail":      &graphql.InputObjectFieldConfig{Type: graphql.String},
			"expectedHeadOid":  &graphql.InputObjectFieldConfig{Type: gitObjectID},
			"clientMutationId": &graphql.InputObjectFieldConfig{Type: graphql.String},
		},
	})
	enableAutoMergePayloadType := graphql.NewObject(graphql.ObjectConfig{
		Name: "EnablePullRequestAutoMergePayload",
		Fields: graphql.Fields{
			"actor":            &graphql.Field{Type: s.graphqlTypes.actor},
			"pullRequest":      &graphql.Field{Type: pullRequestType},
			"clientMutationId": &graphql.Field{Type: graphql.String},
		},
	})
	s.registerMutation(mutationType, "enablePullRequestAutoMerge", &graphql.Field{
		Type: enableAutoMergePayloadType,
		Args: graphql.FieldConfigArgument{
			"input": &graphql.ArgumentConfig{Type: graphql.NewNonNull(enableAutoMergeInputType)},
		},
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			user := s.ghUserFromContext(p.Context)
			input, _ := p.Args["input"].(map[string]interface{})
			prNodeID, _ := input["pullRequestId"].(string)

			pr := store.FindPullRequestByNodeID(s.store, prNodeID)
			if pr == nil {
				return nil, gqlMissingNodeType("PullRequest")
			}
			if pr.State != "OPEN" {
				return nil, fmt.Errorf("pull request is not open")
			}
			repo := s.store.GetRepoByID(pr.RepoID)
			if repo == nil {
				return nil, gqlMissingNodeType("Repository")
			}
			if !repo.AllowAutoMerge {
				//lint:ignore ST1005 GitHub GraphQL parity requires this exact upstream message.
				return nil, fmt.Errorf("Auto merge is not allowed for this repository")
			}

			method := "MERGE"
			if v, ok := input["mergeMethod"].(string); ok && v != "" {
				method = v
			}
			// Refuse an explicit merge method the repository has disabled, as
			// the REST merge endpoint does.
			var disallowed string
			switch {
			case method == "MERGE" && !repo.AllowMergeCommit:
				disallowed = "Merge commits are not allowed on this repository."
			case method == "SQUASH" && !repo.AllowSquashMerge:
				disallowed = "Squash merges are not allowed on this repository."
			case method == "REBASE" && !repo.AllowRebaseMerge:
				disallowed = "Rebase merges are not allowed on this repository."
			}
			if disallowed != "" {
				return nil, fmt.Errorf("%s", disallowed)
			}

			headSha := s.prHeadSha(repo, pr)
			// The same moved-head interlock mergePullRequest honours.
			if expected, ok := input["expectedHeadOid"].(string); ok && expected != "" {
				if headSha == "" || !strings.EqualFold(headSha, expected) {
					//lint:ignore ST1005 GitHub GraphQL parity requires this exact upstream message.
					return nil, fmt.Errorf("Head branch was modified. Review and try the merge again.")
				}
			}

			// Only arms while a blocking condition exists; a PR mergeable now
			// is refused, so there is no armed-after-green race.
			checksClean := headSha == "" || len(s.missingRequiredChecks(repo, pr.BaseRefName, headSha)) == 0
			if mergeable, _ := s.canMergePullRequest(p.Context, repo, pr); mergeable && checksClean {
				//lint:ignore ST1005 GitHub GraphQL parity requires this exact upstream message.
				return nil, fmt.Errorf("Pull request is in clean status")
			}

			commitHeadline, _ := input["commitHeadline"].(string)
			commitBody, _ := input["commitBody"].(string)
			authorEmail, _ := input["authorEmail"].(string)
			s.store.UpdatePullRequest(pr.ID, func(p *store.PullRequest) {
				p.AutoMerge = &store.PullRequestAutoMerge{
					EnabledByID:    user.ID,
					MergeMethod:    method,
					CommitHeadline: commitHeadline,
					CommitBody:     commitBody,
					AuthorEmail:    authorEmail,
					EnabledAt:      s.store.CurrentTime(),
				}
			})
			s.store.RecordPullRequestEvent(pr.RepoID, pr.ID, user.ID, "auto_merge_enabled", "", 0)
			s.emitPullRequestAction(s.store.GetPullRequest(pr.ID), user, "auto_merge_enabled", true)

			return map[string]interface{}{
				"actor":            userToGraphQL(user),
				"pullRequest":      pullRequestToGQL(s.store.GetPullRequest(pr.ID), s.store),
				"clientMutationId": clientMutationID(input),
			}, nil
		},
	})

	disableAutoMergeInputType := graphql.NewInputObject(graphql.InputObjectConfig{
		Name: "DisablePullRequestAutoMergeInput",
		Fields: graphql.InputObjectConfigFieldMap{
			"pullRequestId":    &graphql.InputObjectFieldConfig{Type: graphql.NewNonNull(graphql.ID)},
			"clientMutationId": &graphql.InputObjectFieldConfig{Type: graphql.String},
		},
	})
	disableAutoMergePayloadType := graphql.NewObject(graphql.ObjectConfig{
		Name: "DisablePullRequestAutoMergePayload",
		Fields: graphql.Fields{
			"actor":            &graphql.Field{Type: s.graphqlTypes.actor},
			"pullRequest":      &graphql.Field{Type: pullRequestType},
			"clientMutationId": &graphql.Field{Type: graphql.String},
		},
	})
	s.registerMutation(mutationType, "disablePullRequestAutoMerge", &graphql.Field{
		Type: disableAutoMergePayloadType,
		Args: graphql.FieldConfigArgument{
			"input": &graphql.ArgumentConfig{Type: graphql.NewNonNull(disableAutoMergeInputType)},
		},
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			user := s.ghUserFromContext(p.Context)
			input, _ := p.Args["input"].(map[string]interface{})
			prNodeID, _ := input["pullRequestId"].(string)

			pr := store.FindPullRequestByNodeID(s.store, prNodeID)
			if pr == nil {
				return nil, gqlMissingNodeType("PullRequest")
			}
			if pr.AutoMerge == nil {
				return nil, fmt.Errorf("auto-merge is not enabled for this pull request")
			}
			s.store.UpdatePullRequest(pr.ID, func(p *store.PullRequest) {
				p.AutoMerge = nil
			})
			s.store.RecordPullRequestEvent(pr.RepoID, pr.ID, user.ID, "auto_merge_disabled", "", 0)
			s.emitPullRequestAction(s.store.GetPullRequest(pr.ID), user, "auto_merge_disabled", true)

			return map[string]interface{}{
				"actor":            userToGraphQL(user),
				"pullRequest":      pullRequestToGQL(s.store.GetPullRequest(pr.ID), s.store),
				"clientMutationId": clientMutationID(input),
			}, nil
		},
	})

	// addPullRequestReview (gh pr review)
	// Mapped onto the same review store as REST POST .../pulls/{n}/reviews.
	pullRequestReviewEventEnum := graphql.NewEnum(graphql.EnumConfig{
		Name: "PullRequestReviewEvent",
		Values: graphql.EnumValueConfigMap{
			"APPROVE":         &graphql.EnumValueConfig{Value: "APPROVE"},
			"COMMENT":         &graphql.EnumValueConfig{Value: "COMMENT"},
			"DISMISS":         &graphql.EnumValueConfig{Value: "DISMISS"},
			"REQUEST_CHANGES": &graphql.EnumValueConfig{Value: "REQUEST_CHANGES"},
		},
	})

	// The draft line-comment / thread inputs, declared for schema completeness;
	// the resolver reads only pullRequestId/event/body, so a draft comment
	// array is accepted and ignored.
	diffSideForReview := s.sharedEnum("DiffSide", "LEFT", "RIGHT")
	draftReviewCommentInput := graphql.NewInputObject(graphql.InputObjectConfig{
		Name: "DraftPullRequestReviewComment",
		Fields: graphql.InputObjectConfigFieldMap{
			"body":     &graphql.InputObjectFieldConfig{Type: graphql.NewNonNull(graphql.String)},
			"path":     &graphql.InputObjectFieldConfig{Type: graphql.NewNonNull(graphql.String)},
			"position": &graphql.InputObjectFieldConfig{Type: graphql.NewNonNull(graphql.Int)},
		},
	})
	draftReviewThreadInput := graphql.NewInputObject(graphql.InputObjectConfig{
		Name: "DraftPullRequestReviewThread",
		Fields: graphql.InputObjectConfigFieldMap{
			"body":      &graphql.InputObjectFieldConfig{Type: graphql.NewNonNull(graphql.String)},
			"path":      &graphql.InputObjectFieldConfig{Type: graphql.String},
			"line":      &graphql.InputObjectFieldConfig{Type: graphql.Int},
			"side":      &graphql.InputObjectFieldConfig{Type: diffSideForReview, DefaultValue: "RIGHT"},
			"startLine": &graphql.InputObjectFieldConfig{Type: graphql.Int},
			"startSide": &graphql.InputObjectFieldConfig{Type: diffSideForReview, DefaultValue: "RIGHT"},
		},
	})
	addPRReviewInputType := graphql.NewInputObject(graphql.InputObjectConfig{
		Name: "AddPullRequestReviewInput",
		Fields: graphql.InputObjectConfigFieldMap{
			"pullRequestId":    &graphql.InputObjectFieldConfig{Type: graphql.NewNonNull(graphql.ID)},
			"event":            &graphql.InputObjectFieldConfig{Type: pullRequestReviewEventEnum},
			"body":             &graphql.InputObjectFieldConfig{Type: graphql.String},
			"commitOID":        &graphql.InputObjectFieldConfig{Type: gitObjectID},
			"comments":         &graphql.InputObjectFieldConfig{Type: graphql.NewList(draftReviewCommentInput)},
			"threads":          &graphql.InputObjectFieldConfig{Type: graphql.NewList(draftReviewThreadInput)},
			"clientMutationId": &graphql.InputObjectFieldConfig{Type: graphql.String},
		},
	})

	addPRReviewPayloadType := graphql.NewObject(graphql.ObjectConfig{
		Name: "AddPullRequestReviewPayload",
		Fields: graphql.Fields{
			"pullRequestReview": &graphql.Field{Type: prReviewType},
			// addPullRequestReview returns the review directly, so this alternate
			// edge view is null.
			"reviewEdge": &graphql.Field{
				Type:    s.simpleEdgeType("PullRequestReviewEdge", prReviewType),
				Resolve: func(graphql.ResolveParams) (interface{}, error) { return nil, nil },
			},
			"clientMutationId": &graphql.Field{
				Type: graphql.String,
				Resolve: func(p graphql.ResolveParams) (interface{}, error) {
					m, ok := p.Source.(map[string]interface{})
					if !ok {
						return nil, fmt.Errorf("resolve source: unexpected type %T", p.Source)
					}
					return m["clientMutationId"], nil
				},
			},
		},
	})

	s.registerMutation(mutationType, "addPullRequestReview", &graphql.Field{
		Type: addPRReviewPayloadType,
		Args: graphql.FieldConfigArgument{
			"input": &graphql.ArgumentConfig{Type: graphql.NewNonNull(addPRReviewInputType)},
		},
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			user := s.ghUserFromContext(p.Context)

			input, _ := p.Args["input"].(map[string]interface{})
			prNodeID, _ := input["pullRequestId"].(string)
			event, _ := input["event"].(string)
			body, _ := input["body"].(string)

			pr := store.FindPullRequestByNodeID(s.store, prNodeID)
			if pr == nil {
				return nil, &ghNotFoundError{
					message: fmt.Sprintf("Could not resolve to a node with the global id of '%s'", prNodeID),
				}
			}

			state := "COMMENTED"
			switch event {
			case "APPROVE":
				state = "APPROVED"
			case "REQUEST_CHANGES":
				state = "CHANGES_REQUESTED"
			case "DISMISS":
				state = "DISMISSED"
			}

			review := s.store.CreatePRReview(pr.ID, user.ID, state, body)
			if review == nil {
				return nil, fmt.Errorf("review creation failed")
			}
			// An approval can release an armed auto-merge.
			if state == "APPROVED" {
				s.maybeAutoMerge(pr.ID)
			}

			var clientMutationID interface{}
			if v, ok := input["clientMutationId"].(string); ok && v != "" {
				clientMutationID = v
			}
			return map[string]interface{}{
				"pullRequestReview": prReviewToGQL(review, s.store),
				"clientMutationId":  clientMutationID,
			}, nil
		},
	})

	resolveReviewThreadInputType := graphql.NewInputObject(graphql.InputObjectConfig{
		Name: "ResolveReviewThreadInput",
		Fields: graphql.InputObjectConfigFieldMap{
			"threadId":         &graphql.InputObjectFieldConfig{Type: graphql.NewNonNull(graphql.ID)},
			"clientMutationId": &graphql.InputObjectFieldConfig{Type: graphql.String},
		},
	})
	resolveReviewThreadPayloadType := graphql.NewObject(graphql.ObjectConfig{
		Name: "ResolveReviewThreadPayload",
		Fields: graphql.Fields{
			"thread":           &graphql.Field{Type: prReviewThreadType},
			"clientMutationId": &graphql.Field{Type: graphql.String},
		},
	})
	unresolveReviewThreadInputType := graphql.NewInputObject(graphql.InputObjectConfig{
		Name: "UnresolveReviewThreadInput",
		Fields: graphql.InputObjectConfigFieldMap{
			"threadId":         &graphql.InputObjectFieldConfig{Type: graphql.NewNonNull(graphql.ID)},
			"clientMutationId": &graphql.InputObjectFieldConfig{Type: graphql.String},
		},
	})
	unresolveReviewThreadPayloadType := graphql.NewObject(graphql.ObjectConfig{
		Name: "UnresolveReviewThreadPayload",
		Fields: graphql.Fields{
			"thread":           &graphql.Field{Type: prReviewThreadType},
			"clientMutationId": &graphql.Field{Type: graphql.String},
		},
	})

	s.registerMutation(mutationType, "resolveReviewThread", &graphql.Field{
		Type: resolveReviewThreadPayloadType,
		Args: graphql.FieldConfigArgument{
			"input": &graphql.ArgumentConfig{Type: graphql.NewNonNull(resolveReviewThreadInputType)},
		},
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			return s.resolveReviewThreadGraphQL(p, true)
		},
	})
	s.registerMutation(mutationType, "unresolveReviewThread", &graphql.Field{
		Type: unresolveReviewThreadPayloadType,
		Args: graphql.FieldConfigArgument{
			"input": &graphql.ArgumentConfig{Type: graphql.NewNonNull(unresolveReviewThreadInputType)},
		},
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			return s.resolveReviewThreadGraphQL(p, false)
		},
	})

	// submitPullRequestReview (submit a pending review)
	// Both id args are nullable: submit by review id, or by PR id (the viewer's
	// pending review); event is required.
	submitPRReviewInputType := graphql.NewInputObject(graphql.InputObjectConfig{
		Name: "SubmitPullRequestReviewInput",
		Fields: graphql.InputObjectConfigFieldMap{
			"pullRequestId":       &graphql.InputObjectFieldConfig{Type: graphql.ID},
			"pullRequestReviewId": &graphql.InputObjectFieldConfig{Type: graphql.ID},
			"event":               &graphql.InputObjectFieldConfig{Type: graphql.NewNonNull(pullRequestReviewEventEnum)},
			"body":                &graphql.InputObjectFieldConfig{Type: graphql.String},
			"clientMutationId":    &graphql.InputObjectFieldConfig{Type: graphql.String},
		},
	})
	submitPRReviewPayloadType := graphql.NewObject(graphql.ObjectConfig{
		Name: "SubmitPullRequestReviewPayload",
		Fields: graphql.Fields{
			"pullRequestReview": &graphql.Field{Type: prReviewType},
			"clientMutationId":  &graphql.Field{Type: graphql.String},
		},
	})
	s.registerMutation(mutationType, "submitPullRequestReview", &graphql.Field{
		Type: submitPRReviewPayloadType,
		Args: graphql.FieldConfigArgument{
			"input": &graphql.ArgumentConfig{Type: graphql.NewNonNull(submitPRReviewInputType)},
		},
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			user := s.ghUserFromContext(p.Context)
			input, _ := p.Args["input"].(map[string]interface{})
			reviewNodeID, _ := input["pullRequestReviewId"].(string)
			event, _ := input["event"].(string)
			var review *store.PullRequestReview
			if reviewNodeID != "" {
				review = store.FindReviewByNodeID(s.store, reviewNodeID)
			} else if prNodeID, _ := input["pullRequestId"].(string); prNodeID != "" {
				if pr := store.FindPullRequestByNodeID(s.store, prNodeID); pr != nil && user != nil {
					review = s.store.PendingReviewForAuthor(pr.ID, user.ID)
				}
			}
			if review == nil {
				return nil, gqlMissingNode("PullRequestReview", reviewNodeID)
			}
			// Only a pending review can be submitted; DISMISS is its own mutation.
			if !s.store.SubmitPullRequestReview(review.ID, event) {
				return nil, fmt.Errorf("cannot submit a review that is not pending")
			}
			// An approval can release an armed auto-merge.
			if event == "APPROVE" {
				s.maybeAutoMerge(review.PRID)
			}
			return map[string]interface{}{
				"pullRequestReview": prReviewToGQL(s.store.GetPullRequestReview(review.ID), s.store),
				"clientMutationId":  clientMutationID(input),
			}, nil
		},
	})

	// dismissPullRequestReview
	dismissPRReviewInputType := graphql.NewInputObject(graphql.InputObjectConfig{
		Name: "DismissPullRequestReviewInput",
		Fields: graphql.InputObjectConfigFieldMap{
			"pullRequestReviewId": &graphql.InputObjectFieldConfig{Type: graphql.NewNonNull(graphql.ID)},
			"message":             &graphql.InputObjectFieldConfig{Type: graphql.NewNonNull(graphql.String)},
			"clientMutationId":    &graphql.InputObjectFieldConfig{Type: graphql.String},
		},
	})
	dismissPRReviewPayloadType := graphql.NewObject(graphql.ObjectConfig{
		Name: "DismissPullRequestReviewPayload",
		Fields: graphql.Fields{
			"pullRequestReview": &graphql.Field{Type: prReviewType},
			"clientMutationId":  &graphql.Field{Type: graphql.String},
		},
	})
	s.registerMutation(mutationType, "dismissPullRequestReview", &graphql.Field{
		Type: dismissPRReviewPayloadType,
		Args: graphql.FieldConfigArgument{
			"input": &graphql.ArgumentConfig{Type: graphql.NewNonNull(dismissPRReviewInputType)},
		},
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			input, _ := p.Args["input"].(map[string]interface{})
			reviewNodeID, _ := input["pullRequestReviewId"].(string)
			message, _ := input["message"].(string)
			review := store.FindReviewByNodeID(s.store, reviewNodeID)
			if review == nil {
				return nil, gqlMissingNode("PullRequestReview", reviewNodeID)
			}
			if !s.store.DismissPullRequestReview(review.ID, message) {
				return nil, fmt.Errorf("review dismissal failed")
			}
			// Dismissing a blocking CHANGES_REQUESTED review can release an
			// armed auto-merge.
			s.maybeAutoMerge(review.PRID)
			return map[string]interface{}{
				"pullRequestReview": prReviewToGQL(s.store.GetPullRequestReview(review.ID), s.store),
				"clientMutationId":  clientMutationID(input),
			}, nil
		},
	})

	updatePRInputType := graphql.NewInputObject(graphql.InputObjectConfig{
		Name: "UpdatePullRequestInput",
		Fields: graphql.InputObjectConfigFieldMap{
			"pullRequestId":       &graphql.InputObjectFieldConfig{Type: graphql.NewNonNull(graphql.ID)},
			"title":               &graphql.InputObjectFieldConfig{Type: graphql.String},
			"body":                &graphql.InputObjectFieldConfig{Type: graphql.String},
			"baseRefName":         &graphql.InputObjectFieldConfig{Type: graphql.String},
			"labelIds":            &graphql.InputObjectFieldConfig{Type: graphql.NewList(graphql.NewNonNull(graphql.ID))},
			"assigneeIds":         &graphql.InputObjectFieldConfig{Type: graphql.NewList(graphql.NewNonNull(graphql.ID))},
			"milestoneId":         &graphql.InputObjectFieldConfig{Type: graphql.ID},
			"maintainerCanModify": &graphql.InputObjectFieldConfig{Type: graphql.Boolean},
			"projectIds":          &graphql.InputObjectFieldConfig{Type: graphql.NewList(graphql.NewNonNull(graphql.ID))},
			"state":               &graphql.InputObjectFieldConfig{Type: s.sharedEnum("PullRequestUpdateState", "CLOSED", "OPEN")},
		},
	})

	updatePRPayloadType := graphql.NewObject(graphql.ObjectConfig{
		Name: "UpdatePullRequestPayload",
		Fields: graphql.Fields{
			"pullRequest": &graphql.Field{Type: pullRequestType},
			"actor":       &graphql.Field{Type: s.graphqlTypes.actor},
		},
	})

	s.registerMutation(mutationType, "updatePullRequest", &graphql.Field{
		Type: updatePRPayloadType,
		Args: graphql.FieldConfigArgument{
			"input": &graphql.ArgumentConfig{Type: graphql.NewNonNull(updatePRInputType)},
		},
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			input, _ := p.Args["input"].(map[string]interface{})
			prNodeID, _ := input["pullRequestId"].(string)

			pr := store.FindPullRequestByNodeID(s.store, prNodeID)
			if pr == nil {
				return nil, gqlMissingNodeType("PullRequest")
			}

			repo := s.store.GetRepoByID(pr.RepoID)
			if repo == nil {
				return nil, gqlMissingNodeType("Repository")
			}
			labelIDs, err := resolveGQLLabelIDs(s.store, repo.ID, input["labelIds"])
			if err != nil {
				return nil, err
			}
			assigneeIDs, err := resolveGQLAssigneeIDs(s.store, input["assigneeIds"])
			if err != nil {
				return nil, err
			}
			milestoneID, err := resolveGQLMilestoneID(s.store, repo.ID, input, "milestoneId")
			if err != nil {
				return nil, err
			}

			// FindPullRequestByNodeID returns the live row; snapshot the
			// before-values the webhook fan-out diffs against.
			before := s.store.GetPullRequest(pr.ID)
			if before == nil {
				return nil, gqlMissingNodeType("PullRequest")
			}
			s.store.UpdatePullRequest(pr.ID, func(p *store.PullRequest) {
				if v, ok := input["title"].(string); ok {
					p.Title = v
				}
				if v, ok := input["body"].(string); ok {
					p.Body = v
				}
				if v, ok := input["baseRefName"].(string); ok {
					p.BaseRefName = v
				}
				if labelIDs != nil {
					p.LabelIDs = *labelIDs
				}
				if assigneeIDs != nil {
					p.AssigneeIDs = *assigneeIDs
				}
				if milestoneID != nil {
					p.MilestoneID = *milestoneID
				}
			})

			updated := s.store.GetPullRequest(pr.ID)
			// The only route to a PR's assignees and milestone, so it owns the
			// assigned/unassigned and milestoned/demilestoned actions.
			change := store.SubjectChange{
				LabelsFrom:    before.LabelIDs,
				LabelsTo:      labelIDs,
				AssigneesFrom: before.AssigneeIDs,
				AssigneesTo:   assigneeIDs,
				MilestoneFrom: before.MilestoneID,
				MilestoneTo:   milestoneID,
			}
			if v, ok := input["title"].(string); ok && v != before.Title {
				change.TitleFrom = &before.Title
			}
			if v, ok := input["body"].(string); ok && v != before.Body {
				change.BodyFrom = &before.Body
			}
			if v, ok := input["baseRefName"].(string); ok && v != before.BaseRefName {
				change.BaseRefFrom = &before.BaseRefName
			}
			user := s.ghUserFromContext(p.Context)
			// Record retitle/retarget timeline events here as well as on the
			// REST PATCH, so history is client-independent.
			if user != nil {
				if change.TitleFrom != nil {
					s.store.RecordIssueOrPREvent(repo.ID, updated.Number, user.ID, "renamed", map[string]interface{}{
						"rename_from": *change.TitleFrom,
						"rename_to":   updated.Title,
					})
				}
				if change.BaseRefFrom != nil {
					s.store.RecordIssueOrPREvent(repo.ID, updated.Number, user.ID, "base_ref_changed", map[string]interface{}{
						"rename_from": *change.BaseRefFrom,
						"rename_to":   updated.BaseRefName,
					})
				}
			}
			s.emitPullRequestChanges(repo, updated, user, change)
			return map[string]interface{}{
				"pullRequest": pullRequestToGQL(updated, s.store),
				"actor":       optionalRendered(user, userToGraphQL),
			}, nil
		},
	})

	// Issue.closedByPullRequestsReferences — the reverse of
	// PullRequest.closingIssuesReferences. Registered here, where both types
	// exist (issues are built before pulls).
	issueType.AddFieldConfig("closedByPullRequestsReferences", &graphql.Field{
		Type: prConnectionType,
		Args: graphql.FieldConfigArgument{
			"first":            &graphql.ArgumentConfig{Type: graphql.Int},
			"after":            &graphql.ArgumentConfig{Type: graphql.String},
			"includeClosedPrs": &graphql.ArgumentConfig{Type: graphql.Boolean},
		},
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			src, ok := p.Source.(map[string]interface{})
			if !ok {
				return nil, fmt.Errorf("resolve source: unexpected type %T", p.Source)
			}
			issueID, _ := src["databaseId"].(int)
			issue := s.store.GetIssue(issueID)
			if issue == nil {
				return nil, fmt.Errorf("issue %d not found", issueID)
			}
			repo := s.store.GetRepoByID(issue.RepoID)
			if repo == nil {
				return nil, fmt.Errorf("repository %d not found", issue.RepoID)
			}
			includeClosed := true
			if v, ok := p.Args["includeClosedPrs"].(bool); ok {
				includeClosed = v
			}
			prs := closedByPullRequestsForIssue(s.store, repo, issue, includeClosed)
			first, _ := intArg(p.Args, "first")
			after, _ := p.Args["after"].(string)
			return paginateGQL(prs, first, after, func(pr *store.PullRequest) map[string]interface{} {
				return pullRequestToGQL(pr, s.store)
			}, func(pr *store.PullRequest) string { return pr.NodeID }), nil
		},
	})

	return pullRequestType
}

// emitPullRequestAction delivers one `pull_request` webhook action for a
// GraphQL-driven change. Draft toggling and auto-merge arming have no REST
// endpoint, so these resolvers are their only emit site. A no-op mutation
// (changed=false) emits nothing.
func (s *Resolver) emitPullRequestAction(pr *store.PullRequest, user *store.User, action string, changed bool) {
	if !changed || pr == nil || user == nil {
		return
	}
	repo := s.store.GetRepoByID(pr.RepoID)
	if repo == nil {
		return
	}
	s.emitWebhookEvent(repo.FullName, "pull_request", action, s.buildPullRequestPayload(repo, pr, user, action))
}

// closedByPullRequestsForIssue returns the PRs whose body's closing keywords
// reference this issue; includeClosed=false narrows to open PRs.
func closedByPullRequestsForIssue(st *store.Store, repo *store.Repo, issue *store.Issue, includeClosed bool) []*store.PullRequest {
	var out []*store.PullRequest
	for _, pr := range st.ListPullRequests(repo.ID, "all") {
		if !includeClosed && pr.State != "OPEN" {
			continue
		}
		for _, ref := range closingIssueRefs(repo.FullName, pr.Body) {
			if strings.EqualFold(ref.repoFullName, repo.FullName) && ref.number == issue.Number {
				out = append(out, pr)
				break
			}
		}
	}
	return out
}

// GraphQL converter helpers

func pullRequestAndRepoFromGQLSource(src interface{}, st *store.Store) (*store.PullRequest, *store.Repo, error) {
	prMap, ok := src.(map[string]interface{})
	if !ok {
		return nil, nil, fmt.Errorf("resolve source: unexpected type %T", src)
	}
	prID, ok := prMap["databaseId"].(int)
	if !ok || prID <= 0 {
		return nil, nil, fmt.Errorf("pull request source missing databaseId")
	}
	pr := st.GetPullRequest(prID)
	if pr == nil {
		return nil, nil, fmt.Errorf("pull request not found")
	}
	repo := st.GetRepoByID(pr.RepoID)
	if repo == nil {
		return nil, nil, fmt.Errorf("repository not found")
	}
	return pr, repo, nil
}

// autoMergeRequestToGQL renders a PR's armed auto-merge request, or nil when off.
func autoMergeRequestToGQL(pr *store.PullRequest, st *store.Store) map[string]interface{} {
	if pr == nil || pr.AutoMerge == nil {
		return nil
	}
	var enabledBy map[string]interface{}
	st.Mu.RLock()
	if u := st.Users[pr.AutoMerge.EnabledByID]; u != nil {
		enabledBy = userToGraphQL(u)
	}
	st.Mu.RUnlock()
	var authorEmail interface{}
	if pr.AutoMerge.AuthorEmail != "" {
		authorEmail = pr.AutoMerge.AuthorEmail
	}
	return map[string]interface{}{
		"authorEmail":    authorEmail,
		"commitBody":     pr.AutoMerge.CommitBody,
		"commitHeadline": pr.AutoMerge.CommitHeadline,
		"mergeMethod":    pr.AutoMerge.MergeMethod,
		"enabledAt":      pr.AutoMerge.EnabledAt.UTC().Format(time.RFC3339),
		"enabledBy":      optionalObject(enabledBy),
		"prID":           pr.ID,
	}
}

func pullRequestChangedFileToGQL(file map[string]interface{}) map[string]interface{} {
	status, _ := file["status"].(string)
	changeType := "CHANGED"
	switch status {
	case "added":
		changeType = "ADDED"
	case "removed":
		changeType = "DELETED"
	case "renamed":
		changeType = "RENAMED"
	case "modified", "changed":
		changeType = "MODIFIED"
	}
	path, _ := file["filename"].(string)
	additions, _ := file["additions"].(int)
	deletions, _ := file["deletions"].(int)
	return map[string]interface{}{
		"path":       path,
		"additions":  additions,
		"deletions":  deletions,
		"changeType": changeType,
	}
}

var (
	closingKeywordRE = regexp.MustCompile(`(?i)\b(?:close[sd]?|fix(?:e[sd])?|resolve[sd]?)\b([^\n]*)`)
	closingIssueRE   = regexp.MustCompile(`(?i)(?:\b([A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+))?#([0-9]+)`)
)

func closingIssuesForPullRequest(st *store.Store, repo *store.Repo, pr *store.PullRequest) []*store.Issue {
	refs := closingIssueRefs(repo.FullName, pr.Body)
	if len(refs) == 0 {
		return nil
	}
	st.Mu.RLock()
	defer st.Mu.RUnlock()

	issues := make([]*store.Issue, 0, len(refs))
	seen := map[int]bool{}
	for _, ref := range refs {
		targetRepo := st.ReposByName[ref.repoFullName]
		if targetRepo == nil {
			continue
		}
		issue := st.IssuesByRepo[targetRepo.ID][ref.number]
		if issue == nil || seen[issue.ID] {
			continue
		}
		seen[issue.ID] = true
		issues = append(issues, issue)
	}
	return issues
}

type closingIssueRef struct {
	repoFullName string
	number       int
}

func closingIssueRefs(defaultRepoFullName, body string) []closingIssueRef {
	if strings.TrimSpace(body) == "" {
		return nil
	}
	refs := []closingIssueRef{}
	for _, match := range closingKeywordRE.FindAllStringSubmatch(body, -1) {
		tail := match[1]
		if idx := strings.IndexAny(tail, ".;"); idx >= 0 {
			tail = tail[:idx]
		}
		for _, refMatch := range closingIssueRE.FindAllStringSubmatch(tail, -1) {
			repoFullName := defaultRepoFullName
			if refMatch[1] != "" {
				repoFullName = refMatch[1]
			}
			number, err := strconv.Atoi(refMatch[2])
			if err != nil || number <= 0 {
				continue
			}
			refs = append(refs, closingIssueRef{repoFullName: repoFullName, number: number})
		}
	}
	return refs
}

func pullRequestToGQL(pr *store.PullRequest, st *store.Store) map[string]interface{} {
	baseRepo := st.GetRepoByID(pr.RepoID)
	stor, repoFullNameForCommits := store.PullRequestGitStorage(st, baseRepo, pr)
	realCommits := []*object.Commit(nil)
	var realMergeCommit *object.Commit
	if stor != nil {
		if commits, err := store.PullRequestCommitObjectsFromStorage(stor, pr); err == nil {
			realCommits = commits
		}
		if pr.MergeCommitSHA != "" {
			if repository, err := git.Open(stor, nil); err == nil {
				realMergeCommit, _ = repository.CommitObject(plumbing.NewHash(pr.MergeCommitSHA))
			}
		}
	}

	st.Mu.RLock()
	defer st.Mu.RUnlock()

	var author map[string]interface{}
	if u := store.ActorUserLocked(st, pr.AuthorID); u != nil {
		author = userToGraphQL(u)
	}

	var mergedBy map[string]interface{}
	if pr.MergedByID > 0 {
		if u, ok := st.Users[pr.MergedByID]; ok {
			mergedBy = userToGraphQL(u)
		}
	}

	labelNodes := make([]map[string]interface{}, 0)
	for _, lid := range pr.LabelIDs {
		if l, ok := st.Labels[lid]; ok {
			labelNodes = append(labelNodes, labelToGQL(l))
		}
	}

	assigneeNodes := make([]map[string]interface{}, 0)
	for _, aid := range pr.AssigneeIDs {
		if u, ok := st.Users[aid]; ok {
			assigneeNodes = append(assigneeNodes, userToGraphQL(u))
		}
	}

	// Inlined (not prReviewToGQL) to avoid re-locking st.Mu.
	prReviews := st.PRReviewsByPR[pr.ID]
	reviewNodes := make([]map[string]interface{}, 0, len(prReviews))
	for _, r := range prReviews {
		reviewNodes = append(reviewNodes, prReviewSourceLocked(r, st))
	}
	sortGQLNodesByCreatedAt(reviewNodes)

	// latestReviews — the newest review per author.
	latestByAuthor := map[int]*store.PullRequestReview{}
	for _, r := range prReviews {
		if cur, ok := latestByAuthor[r.AuthorID]; !ok || r.CreatedAt.After(cur.CreatedAt) {
			latestByAuthor[r.AuthorID] = r
		}
	}
	latestReviewNodes := make([]map[string]interface{}, 0, len(latestByAuthor))
	for _, r := range latestByAuthor {
		latestReviewNodes = append(latestReviewNodes, prReviewSourceLocked(r, st))
	}
	sort.Slice(latestReviewNodes, func(a, b int) bool {
		ca, _ := latestReviewNodes[a]["createdAt"].(string)
		cb, _ := latestReviewNodes[b]["createdAt"].(string)
		return ca < cb
	})

	var reviewDecision interface{}
	if rd := deriveReviewDecisionLocked(st, pr.ID); rd != "" {
		reviewDecision = rd
	}

	// PRs and Issues share the comment table (Comment.ParentType).
	prCommentNodes := make([]map[string]interface{}, 0)
	for _, c := range st.Comments {
		if c.ParentType == "pull_request" && c.IssueID == pr.ID {
			prCommentNodes = append(prCommentNodes, commentToGQLLocked(c, st))
		}
	}
	// st.Comments iteration order is nondeterministic; sort oldest-first for
	// stable cursors.
	sortGQLNodesByCreatedAt(prCommentNodes)

	reviewThreadNodes := reviewThreadsForGraphQL(st.PRReviewComments.ListThreads(pr.ID), st)

	repo := st.Repos[pr.RepoID]
	url, resourcePath := "", ""
	if repo != nil {
		resourcePath = "/" + repo.FullName + "/pull/" + strconv.Itoa(pr.Number)
		url = externalURL(resourcePath)
	}

	sha := store.PullRequestHeadSHALocked(pr, st)
	baseSha := pr.BaseSHA

	var closedAt interface{}
	if pr.ClosedAt != nil {
		closedAt = pr.ClosedAt.Format(time.RFC3339)
	}
	var mergedAt interface{}
	if pr.MergedAt != nil {
		mergedAt = pr.MergedAt.Format(time.RFC3339)
	}

	// mergeStateStatus from the stored mergeability, the only modeled gate.
	mergeStateStatus := "UNKNOWN"
	switch pr.Mergeable {
	case "MERGEABLE":
		mergeStateStatus = "CLEAN"
	case "CONFLICTING":
		mergeStateStatus = "DIRTY"
	}

	// interface{}, not map: a nil map boxed into an interface is not a nil
	// interface, so GraphQL would complete a non-null Repository.id off an empty
	// shell instead of answering null for a vanished head repository.
	var headRepository interface{}
	headRepo := store.PullRequestHeadRepoLocked(st, pr)
	if headRepo != nil {
		headRepository = repoToGraphQLLocked(st, headRepo)
	}
	var headRepositoryOwner interface{}
	if owner := repoOwnerGraphQLLocked(headRepo, st); owner != nil {
		headRepositoryOwner = owner
	}

	commitNodes := make([]map[string]interface{}, 0)
	var commitAuthors []interface{}
	if u := store.ActorUserLocked(st, pr.AuthorID); u != nil {
		commitAuthors = append(commitAuthors, map[string]interface{}{
			"name":  u.Name,
			"email": u.Email,
			"user":  userToGraphQL(u),
		})
	}
	repoFullName := ""
	if repo != nil {
		repoFullName = repo.FullName
	}
	if repoFullName == "" {
		repoFullName = repoFullNameForCommits
	}
	for _, c := range realCommits {
		commitNodes = append(commitNodes, prCommitNode(gitCommitToGQLLocked(c, st, repoFullName), pr.ID, repoFullName, pr.Number, c.Hash.String()))
	}
	if len(commitNodes) == 0 && sha != "" {
		headCommit := map[string]interface{}{
			"__typename":        "Commit",
			"oid":               sha,
			"repoFullName":      repoFullName,
			"message":           pr.Title,
			"messageHeadline":   pr.Title,
			"messageBody":       "",
			"committedDate":     pr.CreatedAt.Format(time.RFC3339),
			"authoredDate":      pr.CreatedAt.Format(time.RFC3339),
			"authors":           map[string]interface{}{"nodes": commitAuthors, "totalCount": len(commitAuthors)},
			"statusCheckRollup": statusCheckRollupSourceLocked(st, repoFullName, sha),
		}
		commitNodes = append(commitNodes, prCommitNode(headCommit, pr.ID, repoFullName, pr.Number, sha))
	}
	var mergeCommit interface{}
	if realMergeCommit != nil {
		mergeCommit = gitCommitToGQLLocked(realMergeCommit, st, repoFullName)
	}

	participantNodes := prParticipantsLocked(pr, st)

	return map[string]interface{}{
		"__typename":       "PullRequest",
		"nodeID":           pr.NodeID,
		"databaseId":       pr.ID,
		"repoID":           pr.RepoID,
		"number":           pr.Number,
		"title":            pr.Title,
		"body":             pr.Body,
		"state":            pr.State,
		"isDraft":          pr.IsDraft,
		"url":              url,
		"resourcePath":     resourcePath,
		"headRefName":      pr.HeadRefName,
		"baseRefName":      pr.BaseRefName,
		"headRefOid":       sha,
		"baseRefOid":       baseSha,
		"mergeable":        pr.Mergeable,
		"mergeStateStatus": mergeStateStatus,
		"merged":           pr.State == "MERGED",
		"mergedAt":         mergedAt,
		"mergedBy":         optionalObject(mergedBy),
		"mergeCommit":      mergeCommit,
		"additions":        pr.Additions,
		"deletions":        pr.Deletions,
		"changedFiles":     pr.ChangedFiles,
		"reviewDecision":   reviewDecision,
		"author":           optionalObject(author),
		"createdAt":        pr.CreatedAt.Format(time.RFC3339),
		"updatedAt":        pr.UpdatedAt.Format(time.RFC3339),
		"closedAt":         closedAt,
		"locked":           pr.Locked,
		"activeLockReason": graphQLLockReason(string(pr.ActiveLockReason)),
		"milestone":        prMilestoneToGQLLocked(pr, st),
		"labels": map[string]interface{}{
			"nodes":      labelNodes,
			"totalCount": len(labelNodes),
			"pageInfo": map[string]interface{}{
				"hasNextPage":     false,
				"hasPreviousPage": false,
				"startCursor":     nil,
				"endCursor":       nil,
			},
		},
		"assignees": map[string]interface{}{
			"nodes":      assigneeNodes,
			"totalCount": len(assigneeNodes),
			"pageInfo": map[string]interface{}{
				"hasNextPage":     false,
				"hasPreviousPage": false,
				"startCursor":     nil,
				"endCursor":       nil,
			},
		},
		"reviews": map[string]interface{}{
			"nodes":      reviewNodes,
			"totalCount": len(reviewNodes),
			"pageInfo": map[string]interface{}{
				"hasNextPage":     false,
				"hasPreviousPage": false,
				"startCursor":     nil,
				"endCursor":       nil,
			},
		},
		"latestReviews": map[string]interface{}{
			"nodes":      latestReviewNodes,
			"totalCount": len(latestReviewNodes),
		},
		"headRepository":      headRepository,
		"headRepositoryOwner": headRepositoryOwner,
		"repository":          repoToGraphQLLocked(st, st.Repos[pr.RepoID]),
		"isCrossRepository":   store.PullRequestHeadRepoID(pr) != pr.RepoID,
		"maintainerCanModify": pr.MaintainerCanModify,
		"reviewRequests": map[string]interface{}{
			"nodes":      pullRequestReviewRequestNodesLocked(pr, st),
			"totalCount": len(pr.RequestedReviewerIDs),
		},
		"comments": map[string]interface{}{
			"nodes":      prCommentNodes,
			"totalCount": len(prCommentNodes),
			"pageInfo": map[string]interface{}{
				"hasNextPage":     false,
				"hasPreviousPage": false,
				"startCursor":     nil,
				"endCursor":       nil,
			},
		},
		"commits": map[string]interface{}{
			"totalCount": len(commitNodes),
			"nodes":      commitNodes,
		},
		"reactionGroups": reactionGroupsForGraphQL(st.Reactions, "pull_request", pr.ID, 0),
		"reviewThreads": map[string]interface{}{
			"nodes":      reviewThreadNodes,
			"totalCount": len(reviewThreadNodes),
		},
		// Surface-completion keys (gh_pulls_fields_graphql.go); authorID drives
		// the viewer* permission fields.
		"authorID":           pr.AuthorID,
		"authorAssociation":  authorAssociationForRepoLocked(st, pr.RepoID, pr.AuthorID),
		"statusCheckRollup":  statusCheckRollupSourceLocked(st, repoFullName, sha),
		"totalCommentsCount": len(prCommentNodes) + prReviewThreadCommentCount(reviewThreadNodes),
		"participants": map[string]interface{}{
			"nodes":      participantNodes,
			"totalCount": len(participantNodes),
		},
		"assignedActors": map[string]interface{}{
			"nodes":      assigneeNodes,
			"totalCount": len(assigneeNodes),
		},
	}
}

// prReviewCommentToGQL renders a store PR review comment as its
// PullRequestReviewComment source map. Self-locking.
func prReviewCommentToGQL(c *store.PRReviewComment, st *store.Store) map[string]interface{} {
	var author map[string]interface{}
	if u := st.GetUserByID(c.AuthorID); u != nil {
		author = userToGraphQL(u)
	}
	var line, position interface{}
	if c.Line != nil {
		line = *c.Line
	}
	if c.Position != nil {
		position = *c.Position
	}
	return map[string]interface{}{
		"nodeID":     c.NodeID,
		"databaseId": c.ID,
		"body":       c.Body,
		"path":       c.Path,
		"diffHunk":   c.DiffHunk,
		"line":       line,
		"position":   position,
		"createdAt":  c.CreatedAt.Format(time.RFC3339),
		"updatedAt":  c.UpdatedAt.Format(time.RFC3339),
		"author":     optionalObject(author),
		"state":      "SUBMITTED",
	}
}

func reviewThreadsForGraphQL(threads []*store.ReviewThread, st *store.Store) []map[string]interface{} {
	out := make([]map[string]interface{}, 0, len(threads))
	for _, t := range threads {
		commentNodes := make([]map[string]interface{}, 0, len(t.Comments))
		for _, c := range t.Comments {
			var author map[string]interface{}
			if u, ok := st.Users[c.AuthorID]; ok {
				author = userToGraphQL(u)
			}
			var line interface{}
			if c.Line != nil {
				line = *c.Line
			}
			var position interface{}
			if c.Position != nil {
				position = *c.Position
			}
			commentNodes = append(commentNodes, map[string]interface{}{
				"nodeID":     c.NodeID,
				"databaseId": c.ID,
				"body":       c.Body,
				"path":       c.Path,
				"diffHunk":   c.DiffHunk,
				"line":       line,
				"position":   position,
				"createdAt":  c.CreatedAt.Format(time.RFC3339),
				"updatedAt":  c.UpdatedAt.Format(time.RFC3339),
				"author":     optionalObject(author),
				"state":      "SUBMITTED",
			})
		}
		// The thread's path/line/diff metadata track the root comment.
		var threadPath string
		var threadLine, originalLine, originalStartLine, startLine, startDiffSide interface{}
		diffSide := "RIGHT"
		subjectType := "FILE"
		prID, repoID, rootAuthorID := 0, 0, 0
		if len(t.Comments) > 0 {
			root := t.Comments[0]
			threadPath = root.Path
			prID = root.PullRequestID
			rootAuthorID = root.AuthorID
			if pr := st.PullRequests[root.PullRequestID]; pr != nil {
				repoID = pr.RepoID
			}
			if root.Side != "" {
				diffSide = root.Side
			}
			if root.StartSide != "" {
				startDiffSide = root.StartSide
			}
			if root.Line != nil {
				threadLine = *root.Line
				originalLine = *root.Line
				subjectType = "LINE"
			}
			if root.OriginalLine != nil {
				originalLine = *root.OriginalLine
			}
			if root.StartLine != nil {
				startLine = *root.StartLine
			}
			if root.OriginalStartLine != nil {
				originalStartLine = *root.OriginalStartLine
			}
		}
		var resolvedBy interface{}
		if t.ResolvedByID != 0 {
			if u, ok := st.Users[t.ResolvedByID]; ok {
				resolvedBy = userToGraphQL(u)
			}
		}
		out = append(out, map[string]interface{}{
			"id":                store.PRReviewThreadNodeID(t.ID),
			"isResolved":        t.IsResolved,
			"isOutdated":        false,
			"isCollapsed":       t.IsResolved,
			"resolvedBy":        resolvedBy,
			"path":              threadPath,
			"line":              threadLine,
			"originalLine":      originalLine,
			"startLine":         startLine,
			"originalStartLine": originalStartLine,
			"diffSide":          diffSide,
			"startDiffSide":     startDiffSide,
			"subjectType":       subjectType,
			"prID":              prID,
			"repoID":            repoID,
			"authorID":          rootAuthorID,
			"comments": map[string]interface{}{
				"nodes":      commentNodes,
				"totalCount": len(commentNodes),
				"pageInfo": map[string]interface{}{
					"hasNextPage": false, "hasPreviousPage": false,
					"startCursor": nil, "endCursor": nil,
				},
			},
		})
	}
	return out
}

func (s *Resolver) resolveReviewThreadGraphQL(p graphql.ResolveParams, resolved bool) (interface{}, error) {
	input, _ := p.Args["input"].(map[string]interface{})
	threadNodeID, _ := input["threadId"].(string)
	threadID, ok := store.ParsePRReviewThreadNodeID(threadNodeID)
	if !ok {
		return nil, &ghNotFoundError{
			message: fmt.Sprintf("Could not resolve to a PullRequestReviewThread with the global id of '%s'", threadNodeID),
		}
	}
	resolverID := 0
	if actor := s.ghUserFromContext(p.Context); actor != nil {
		resolverID = actor.ID
	}
	if !s.store.PRReviewComments.ResolveThread(threadID, resolved, resolverID) {
		return nil, &ghNotFoundError{
			message: fmt.Sprintf("Could not resolve to a PullRequestReviewThread with the global id of '%s'", threadNodeID),
		}
	}
	thread := s.store.PRReviewComments.GetThread(threadID)
	if thread == nil {
		return nil, &ghNotFoundError{
			message: fmt.Sprintf("Could not resolve to a PullRequestReviewThread with the global id of '%s'", threadNodeID),
		}
	}
	s.store.Mu.RLock()
	nodes := reviewThreadsForGraphQL([]*store.ReviewThread{thread}, s.store)
	s.store.Mu.RUnlock()
	var gqlThread interface{}
	if len(nodes) == 1 {
		gqlThread = nodes[0]
	}
	var clientMutationID interface{}
	if v, ok := input["clientMutationId"].(string); ok && v != "" {
		clientMutationID = v
	}
	return map[string]interface{}{
		"thread":           gqlThread,
		"clientMutationId": clientMutationID,
	}, nil
}

// prMilestoneToGQLLocked returns the PR's milestone source map, or nil when it
// has none or the milestone was deleted.
func prMilestoneToGQLLocked(pr *store.PullRequest, st *store.Store) interface{} {
	if pr.MilestoneID == 0 {
		return nil
	}
	ms, ok := st.Milestones[pr.MilestoneID]
	if !ok {
		return nil
	}
	var dueOn interface{}
	if ms.DueOn != nil {
		dueOn = ms.DueOn.Format(time.RFC3339)
	}
	return map[string]interface{}{
		"number":      ms.Number,
		"title":       ms.Title,
		"description": ms.Description,
		"dueOn":       dueOn,
	}
}

// deriveReviewDecisionLocked derives the review decision. Caller holds st.mu.RLock.
func deriveReviewDecisionLocked(st *store.Store, prID int) string {
	latest := map[int]*store.PullRequestReview{}
	for _, review := range st.PRReviewsByPR[prID] {
		switch review.State {
		case "APPROVED", "CHANGES_REQUESTED", "DISMISSED":
		default:
			continue
		}
		current := latest[review.AuthorID]
		if current == nil || review.UpdatedAt.After(current.UpdatedAt) ||
			(review.UpdatedAt.Equal(current.UpdatedAt) && review.ID > current.ID) {
			latest[review.AuthorID] = review
		}
	}
	hasApproved := false
	hasChangesRequested := false
	for _, review := range latest {
		switch review.State {
		case "APPROVED":
			hasApproved = true
		case "CHANGES_REQUESTED":
			hasChangesRequested = true
		}
	}
	if hasChangesRequested {
		return "CHANGES_REQUESTED"
	}
	if hasApproved {
		return "APPROVED"
	}
	return ""
}

func prHasAllLabels(st *store.Store, pr *store.PullRequest, labelNames []string) bool {
	for _, name := range labelNames {
		found := false
		for _, lid := range pr.LabelIDs {
			l := st.GetLabel(lid)
			if l != nil && l.Name == strings.TrimSpace(name) {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

// prReviewSourceLocked builds a single PR review's source map. Caller holds
// st.mu.RLock. submittedAt mirrors createdAt (reviews are submitted on
// creation); commit is the PR head the review was recorded against.
func prReviewSourceLocked(r *store.PullRequestReview, st *store.Store) map[string]interface{} {
	var reviewAuthor map[string]interface{}
	if u, ok := st.Users[r.AuthorID]; ok {
		reviewAuthor = userToGraphQL(u)
	}
	commitSHA := ""
	repoID := 0
	prNumber := 0
	if pr := st.PullRequests[r.PRID]; pr != nil {
		commitSHA = store.PullRequestHeadSHALocked(pr, st)
		repoID = pr.RepoID
		prNumber = pr.Number
	}
	resourcePath := ""
	if repo := st.Repos[repoID]; repo != nil {
		resourcePath = fmt.Sprintf("/%s/pull/%d#pullrequestreview-%d", repo.FullName, prNumber, r.ID)
	}
	return map[string]interface{}{
		"_dbID":             r.ID,
		"_prID":             r.PRID,
		"repoID":            repoID,
		"authorID":          r.AuthorID,
		"nodeID":            r.NodeID,
		"body":              r.Body,
		"state":             r.State,
		"author":            optionalObject(reviewAuthor),
		"authorAssociation": authorAssociationForRepoLocked(st, repoID, r.AuthorID),
		"createdAt":         r.CreatedAt.Format(time.RFC3339),
		"updatedAt":         r.UpdatedAt.Format(time.RFC3339),
		"submittedAt":       r.CreatedAt.Format(time.RFC3339),
		"resourcePath":      resourcePath,
		"commit":            map[string]interface{}{"oid": commitSHA},
		"reactionGroups":    reactionGroupsForGraphQL(st.Reactions, "pull_request_review", r.ID, 0),
	}
}

// requestedReviewerTypeName maps a requestedReviewer source to its union member
// via the __typename discriminator, defaulting to User.
func requestedReviewerTypeName(source interface{}) string {
	src, ok := source.(map[string]interface{})
	if !ok {
		return "User"
	}
	switch src["__typename"] {
	case "Bot":
		return "Bot"
	case "Team":
		return "Team"
	default:
		return "User"
	}
}

func pullRequestReviewRequestNodesLocked(pr *store.PullRequest, st *store.Store) []interface{} {
	nodes := make([]interface{}, 0, len(pr.RequestedReviewerIDs))
	for _, id := range pr.RequestedReviewerIDs {
		if u := st.Users[id]; u != nil {
			reviewer := userToGraphQL(u)
			reviewer["__typename"] = "User"
			nodes = append(nodes, map[string]interface{}{
				"requestedReviewer": reviewer,
				// A review request is a reviewer id, not a first-class row, so
				// databaseId is null and the node id is synthesised.
				"nodeID": fmt.Sprintf("RR_%s_%d", pr.NodeID, id),
				"prID":   pr.ID,
				// Review requests are not derived from CODEOWNERS here.
				"asCodeOwner": false,
			})
		}
	}
	return nodes
}

func gitCommitToGQLLocked(c *object.Commit, st *store.Store, repoFullName string) map[string]interface{} {
	authors := []interface{}{gitActorSourceLocked(st, c.Author)}
	return map[string]interface{}{
		"__typename":        "Commit",
		"oid":               c.Hash.String(),
		"repoFullName":      repoFullName,
		"message":           c.Message,
		"messageHeadline":   strings.SplitN(c.Message, "\n", 2)[0],
		"messageBody":       commitMessageBody(c.Message),
		"committedDate":     c.Committer.When.UTC().Format(time.RFC3339),
		"authoredDate":      c.Author.When.UTC().Format(time.RFC3339),
		"author":            gitActorSourceLocked(st, c.Author),
		"committer":         gitActorSourceLocked(st, c.Committer),
		"authors":           map[string]interface{}{"nodes": authors, "totalCount": len(authors)},
		"statusCheckRollup": statusCheckRollupSourceLocked(st, repoFullName, c.Hash.String()),
	}
}

// botLoginFromSource reads a Bot source's login, or "".
func botLoginFromSource(n map[string]interface{}) string {
	login, _ := n["login"].(string)
	return login
}

// botTimestampFromSource returns a Bot source's timestamp for key, or the epoch
// (a serialisable value for the non-null DateTime field).
func botTimestampFromSource(n map[string]interface{}, key string) string {
	if v, ok := n[key].(string); ok && v != "" {
		return v
	}
	return time.Unix(0, 0).UTC().Format(time.RFC3339)
}

// prCommitSourceByOID renders the Commit at oid from a PR's git storage, or nil
// when the sha is empty or the object cannot be loaded.
func (s *Resolver) prCommitSourceByOID(repo *store.Repo, pr *store.PullRequest, oid string) map[string]interface{} {
	if oid == "" {
		return nil
	}
	stor, repoFullName := store.PullRequestGitStorage(s.store, repo, pr)
	if stor == nil {
		return nil
	}
	repository, err := git.Open(stor, nil)
	if err != nil {
		return nil
	}
	commit, err := repository.CommitObject(plumbing.NewHash(oid))
	if err != nil {
		return nil
	}
	s.store.Mu.RLock()
	defer s.store.Mu.RUnlock()
	return gitCommitToGQLLocked(commit, s.store, repoFullName)
}

// prCommitOID returns a PullRequestCommit source's embedded commit sha, or "".
func prCommitOID(n map[string]interface{}) string {
	commit, _ := n["commit"].(map[string]interface{})
	oid, _ := commit["oid"].(string)
	return oid
}

// prCommitFallbackPath derives a PullRequestCommit resourcePath from the
// embedded commit when the renderer supplied none.
func prCommitFallbackPath(n map[string]interface{}) string {
	commit, _ := n["commit"].(map[string]interface{})
	repoFullName, _ := commit["repoFullName"].(string)
	oid, _ := commit["oid"].(string)
	if repoFullName == "" || oid == "" {
		return ""
	}
	return "/" + repoFullName + "/commit/" + oid
}

// prCommitNode wraps a rendered commit source as a PullRequestCommit source,
// carrying nodeID, prID and the pull/N/commits/SHA path.
func prCommitNode(commit map[string]interface{}, prID int, repoFullName string, prNumber int, sha string) map[string]interface{} {
	resourcePath := ""
	if repoFullName != "" && sha != "" {
		resourcePath = "/" + repoFullName + "/pull/" + strconv.Itoa(prNumber) + "/commits/" + sha
	}
	return map[string]interface{}{
		"commit":       commit,
		"nodeID":       "PRC_" + sha,
		"prID":         prID,
		"resourcePath": resourcePath,
		"url":          externalURL(resourcePath),
	}
}

// gitActorSourceLocked renders a git signature as GitActor. Caller holds st.Mu.
// The "user" member is null unless the signature's email belongs to an account;
// optionalObject keeps the miss an untyped nil so a User shell's non-null id
// cannot abort the query.
func gitActorSourceLocked(st *store.Store, signature object.Signature) map[string]interface{} {
	return map[string]interface{}{
		"name":  signature.Name,
		"email": signature.Email,
		"date":  signature.When.UTC().Format(time.RFC3339),
		"user":  optionalObject(userGraphQLByEmailLocked(st, signature.Email)),
	}
}

func userGraphQLByEmailLocked(st *store.Store, email string) map[string]interface{} {
	if email == "" {
		return nil
	}
	for _, u := range st.Users {
		if strings.EqualFold(u.Email, email) {
			return userToGraphQL(u)
		}
	}
	return nil
}

func commitMessageBody(message string) string {
	parts := strings.SplitN(message, "\n", 2)
	if len(parts) < 2 {
		return ""
	}
	body := strings.TrimSpace(parts[1])
	return body
}

// prReviewToGQL is the unlocked wrapper around prReviewSourceLocked.
func prReviewToGQL(r *store.PullRequestReview, st *store.Store) map[string]interface{} {
	st.Mu.RLock()
	defer st.Mu.RUnlock()
	return prReviewSourceLocked(r, st)
}

// statusCheckRollupSourceLocked builds Commit.statusCheckRollup for (repoKey,
// sha): one StatusContext per REST status context, one CheckRun per stored run.
// Returns nil when neither store has data. Caller holds st.mu.RLock.
func statusCheckRollupSourceLocked(st *store.Store, repoKey, sha string) interface{} {
	if repoKey == "" {
		return nil
	}
	_, _, statuses := st.CommitStatuses.Combined(repoKey, sha)
	var runs []*store.CheckRun
	for _, cr := range st.CheckRuns {
		if cr.RepoKey == repoKey && cr.HeadSHA == sha {
			runs = append(runs, cr)
		}
	}
	if len(statuses) == 0 && len(runs) == 0 {
		return nil
	}
	sort.Slice(statuses, func(a, b int) bool {
		return statuses[a].CreatedAt.Before(statuses[b].CreatedAt)
	})
	sort.Slice(runs, func(a, b int) bool { return runs[a].ID < runs[b].ID })

	allCompleted := true
	anyFailed := false
	nodes := make([]interface{}, 0, len(statuses)+len(runs))
	statusContextCounts := map[string]int{}
	for _, status := range statuses {
		if status.State == "pending" {
			allCompleted = false
		}
		statusState := strings.ToUpper(string(status.State))
		statusContextCounts[statusState]++
		switch status.State {
		case "failure", "error":
			anyFailed = true
		}
		nodes = append(nodes, map[string]interface{}{
			"__typename": "StatusContext",
			// repoKey travels with the node for isRequired.
			"repoKey":     repoKey,
			"context":     status.Context,
			"state":       statusState,
			"targetUrl":   nilStr(status.TargetURL),
			"createdAt":   status.CreatedAt.Format(time.RFC3339),
			"description": nilStr(status.Description),
			// Identity/authorship/sha for StatusContext.{id,creator,commit,…}.
			"nodeID":    status.NodeID,
			"creatorID": status.CreatorID,
			"updatedAt": status.UpdatedAt.Format(time.RFC3339),
			"sha":       sha,
		})
	}
	checkRunCounts := map[string]int{}
	for _, cr := range runs {
		var conclusion interface{}
		if cr.Conclusion != "" {
			conclusion = strings.ToUpper(cr.Conclusion)
		}
		var completedAt interface{}
		if cr.CompletedAt != nil {
			completedAt = cr.CompletedAt.Format(time.RFC3339)
		}
		if cr.Status != "completed" {
			allCompleted = false
		}
		checkRunCounts[checkRunCountState(cr)]++
		switch cr.Conclusion {
		case "failure", "timed_out", "cancelled", "startup_failure":
			anyFailed = true
		}
		// checkSuite is non-null, so always embed a source map (a run with no
		// recorded suite carries a null workflowRun).
		suiteSource := checkSuiteGraphQLSourceLocked(st, st.CheckSuites[cr.SuiteID])
		nodes = append(nodes, checkRunGraphQLSource(cr, repoKey, conclusion, completedAt, suiteSource))
	}

	state := "SUCCESS"
	switch {
	case anyFailed:
		state = "FAILURE"
	case !allCompleted:
		state = "PENDING"
	}

	return map[string]interface{}{
		"state": state,
		// repoKey/sha for StatusCheckRollup.{id,commit}.
		"repoKey": repoKey,
		"sha":     sha,
		"contexts": map[string]interface{}{
			"nodes":                      nodes,
			"totalCount":                 len(nodes),
			"checkRunCount":              len(runs),
			"checkRunCountsByState":      stateCountNodes(checkRunStatesForCounts(), checkRunCounts),
			"statusContextCount":         len(statuses),
			"statusContextCountsByState": stateCountNodes(statusContextStatesForCounts(), statusContextCounts),
			"pageInfo": map[string]interface{}{
				"hasNextPage":     false,
				"hasPreviousPage": false,
				"startCursor":     nil,
				"endCursor":       nil,
			},
		},
	}
}

func checkRunCountState(cr *store.CheckRun) string {
	if cr == nil {
		return "PENDING"
	}
	if cr.Status == "completed" && cr.Conclusion != "" {
		return strings.ToUpper(cr.Conclusion)
	}
	if cr.Status != "" {
		return strings.ToUpper(cr.Status)
	}
	return "PENDING"
}

func checkRunStatesForCounts() []string {
	return []string{
		"ACTION_REQUIRED",
		"CANCELLED",
		"COMPLETED",
		"FAILURE",
		"IN_PROGRESS",
		"NEUTRAL",
		"PENDING",
		"QUEUED",
		"SKIPPED",
		"STALE",
		"STARTUP_FAILURE",
		"SUCCESS",
		"TIMED_OUT",
		"WAITING",
	}
}

func statusContextStatesForCounts() []string {
	return []string{"EXPECTED", "ERROR", "FAILURE", "PENDING", "SUCCESS"}
}

func stateCountNodes(states []string, counts map[string]int) []interface{} {
	out := make([]interface{}, 0, len(states))
	for _, state := range states {
		out = append(out, map[string]interface{}{
			"state": state,
			"count": counts[state],
		})
	}
	return out
}

// checkRunGraphQLSource renders one check run as the CheckRun source shared by
// the rollup connection and the checks mutation payloads.
func checkRunGraphQLSource(cr *store.CheckRun, repoKey string, conclusion, completedAt interface{}, suiteSource map[string]interface{}) map[string]interface{} {
	source := map[string]interface{}{
		"__typename":  "CheckRun",
		"repoKey":     repoKey,
		"id":          cr.NodeID,
		"databaseId":  int(cr.ID),
		"name":        cr.Name,
		"status":      strings.ToUpper(cr.Status),
		"conclusion":  conclusion,
		"startedAt":   cr.StartedAt.Format(time.RFC3339),
		"completedAt": completedAt,
		"detailsUrl":  nilStr(cr.DetailsURL),
		"externalId":  nilStr(cr.ExternalID),
		"title":       nil,
		"summary":     nil,
		"text":        nil,
		"checkSuite":  suiteSource,
		// Keys the residual CheckRun fields (in gh_actions_fields_graphql.go)
		// resolve from.
		"checkRunID": cr.ID,
		"headSHA":    cr.HeadSHA,
	}
	if cr.Output != nil {
		source["annotations"] = cr.Output.Annotations
	}
	if cr.Output != nil {
		source["title"] = nilStr(cr.Output.Title)
		source["summary"] = nilStr(cr.Output.Summary)
		source["text"] = nilStr(cr.Output.Text)
	}
	return source
}

func checkSuiteGraphQLSourceLocked(st *store.Store, suite *store.CheckSuite) map[string]interface{} {
	// workflowRun is untyped nil: a typed-nil map passes graphql-go's isNullish
	// check and then fails WorkflowRun.workflow's non-null contract.
	source := map[string]interface{}{
		"workflowRun": nil,
		// A shell so a run whose recorded suite is gone still satisfies
		// CheckRun.checkSuite's non-null members.
		"id":         "",
		"databaseId": nil,
		"status":     "QUEUED",
		"conclusion": nil,
		"createdAt":  time.Time{}.Format(time.RFC3339),
		"updatedAt":  time.Time{}.Format(time.RFC3339),
		// Keys the residual CheckSuite fields (in gh_actions_fields_graphql.go)
		// resolve their records from.
		"suiteID":       int64(0),
		"repoKey":       "",
		"headSHA":       "",
		"headBranch":    "",
		"appID":         0,
		"workflowRunID": 0,
	}
	if suite != nil {
		source["id"] = suite.NodeID
		source["databaseId"] = int(suite.ID)
		source["status"] = strings.ToUpper(suite.Status)
		if suite.Conclusion != "" {
			source["conclusion"] = strings.ToUpper(suite.Conclusion)
		}
		source["createdAt"] = suite.CreatedAt.UTC().Format(time.RFC3339)
		source["updatedAt"] = suite.UpdatedAt.UTC().Format(time.RFC3339)
		source["suiteID"] = suite.ID
		source["repoKey"] = suite.RepoKey
		source["headSHA"] = suite.HeadSHA
		source["headBranch"] = suite.HeadBranch
		source["appID"] = suite.AppID
		source["workflowRunID"] = suite.WorkflowRunID
	}
	if run := checkSuiteWorkflowRunSourceLocked(st, suite); run != nil {
		source["workflowRun"] = run
	}
	return source
}

func checkSuiteWorkflowRunSourceLocked(st *store.Store, suite *store.CheckSuite) map[string]interface{} {
	if suite == nil || suite.WorkflowRunID == 0 {
		return nil
	}
	for _, wf := range st.Workflows {
		if wf.RunID == suite.WorkflowRunID && wf.RepoFullName == suite.RepoKey {
			return workflowRunGQLSourceLocked(st, wf)
		}
	}
	// The suite records a run id but the run is gone (pruned/restarted); answer
	// the minimal run shell CheckSuite.workflowRun's non-null members need.
	if suite.WorkflowName == "" {
		return nil
	}
	ts := suite.CreatedAt.UTC().Format(time.RFC3339)
	return map[string]interface{}{
		"id":             "WFR_" + strconv.Itoa(suite.WorkflowRunID),
		"databaseId":     suite.WorkflowRunID,
		"runNumber":      0,
		"runAttempt":     1,
		"event":          "",
		"displayTitle":   suite.WorkflowName,
		"createdAt":      ts,
		"updatedAt":      ts,
		"repoFullName":   suite.RepoKey,
		"runID":          suite.WorkflowRunID,
		"checkSuiteID":   suite.ID,
		"workflowFileID": suite.WorkflowFileID,
		"workflow": map[string]interface{}{
			"name":         suite.WorkflowName,
			"id":           "WF_" + suite.WorkflowName,
			"databaseId":   nil,
			"repoFullName": suite.RepoKey,
			"path":         suite.WorkflowFilePath,
			"state":        "ACTIVE",
			"createdAt":    ts,
			"updatedAt":    ts,
		},
	}
}

// searchIssuesAndPRs evaluates Query.search (type: ISSUE) against the issue/PR
// stores. Supported qualifiers (repo:, state:/is:/type:, author:, assignee:,
// label:, mentions:, involves:, review-requested:) are evaluated for real;
// bare keywords match title/body substrings. An unsupported qualifier yields
// empty results, never an over-matching ignore.
func (s *Resolver) searchIssuesAndPRs(query string, viewer *store.User) []gqlConnItem {
	type searchSpec struct {
		repos      []string // repo full names; empty = all
		states     []string // OPEN / CLOSED / MERGED; empty = all
		entity     string   // "issue", "pr", or "" for both
		author     string
		assignee   string
		mentions   string
		involves   string
		reviewer   string
		labels     []string
		keywords   []string
		draft      *bool
		sortField  string
		sortAsc    bool
		impossible bool
	}
	spec := searchSpec{}
	boolPtr := func(b bool) *bool { return &b }

	for _, tok := range strings.Fields(query) {
		// gh's advanced-search syntax may group qualifiers with parens and OR;
		// strip the punctuation (gh emits single-valued groups, which evaluate
		// identically to individual tokens).
		tok = strings.Trim(tok, "()")
		if tok == "" || strings.EqualFold(tok, "OR") || strings.EqualFold(tok, "AND") {
			continue
		}
		key, val, isQualifier := strings.Cut(tok, ":")
		if isQualifier {
			val = strings.Trim(val, `"`)
		}
		if !isQualifier {
			spec.keywords = append(spec.keywords, strings.Trim(tok, `"`))
			continue
		}
		switch strings.ToLower(key) {
		case "repo":
			spec.repos = append(spec.repos, val)
		case "state":
			spec.states = append(spec.states, strings.ToUpper(val))
		case "is":
			switch strings.ToLower(val) {
			case "open", "closed", "merged":
				spec.states = append(spec.states, strings.ToUpper(val))
			case "pr", "issue":
				spec.entity = strings.ToLower(val)
			case "draft":
				spec.entity = "pr"
				spec.draft = boolPtr(true)
			default:
				spec.impossible = true
			}
		case "type":
			spec.entity = strings.ToLower(val)
		case "draft":
			switch strings.ToLower(val) {
			case "true":
				spec.draft = boolPtr(true)
			case "false":
				spec.draft = boolPtr(false)
			default:
				spec.impossible = true
			}
		case "author":
			spec.author = val
		case "assignee":
			spec.assignee = val
		case "mentions":
			spec.mentions = val
		case "involves":
			spec.involves = val
		case "label":
			spec.labels = append(spec.labels, val)
		case "review-requested", "user-review-requested":
			spec.entity = "pr"
			spec.reviewer = val
		case "sort":
			sortParts := strings.Split(strings.ToLower(val), "-")
			switch sortParts[0] {
			case "created", "updated":
				spec.sortField = sortParts[0]
			default:
				spec.impossible = true
			}
			if len(sortParts) > 1 {
				switch sortParts[1] {
				case "asc":
					spec.sortAsc = true
				case "desc":
				default:
					spec.impossible = true
				}
			}
		default:
			spec.impossible = true
		}
	}
	if spec.impossible {
		return nil
	}

	s.store.Mu.RLock()
	// Search returns results only from repositories the viewer can access.
	// canReadRepoAsUser's logic is inlined because the RLock is already held
	// (the helpers take the lock themselves).
	repoReadable := func(repo *store.Repo) bool {
		if repo == nil {
			return false
		}
		if !repo.Private {
			return true
		}
		if viewer == nil {
			return false
		}
		if repo.Owner != nil && repo.Owner.ID == viewer.ID {
			return true
		}
		parts := strings.SplitN(repo.FullName, "/", 2)
		if len(parts) != 2 {
			return false
		}
		if m := s.store.Memberships[store.MembershipKey(parts[0], viewer.ID)]; m != nil && m.State == store.MembershipStateActive {
			return true
		}
		// Team-level pull access.
		if org := s.store.OrgsByLogin[parts[0]]; org != nil {
			for _, team := range s.store.TeamsBySlug {
				if team.OrgID != org.ID || !store.PermissionAtLeast(team.Permission, store.TeamPermissionPull) {
					continue
				}
				inRepo := false
				for _, rn := range team.RepoNames {
					if rn == repo.FullName {
						inRepo = true
						break
					}
				}
				if !inRepo {
					continue
				}
				for _, mid := range team.MemberIDs {
					if mid == viewer.ID {
						return true
					}
				}
			}
		}
		return false
	}
	// Candidate repos.
	var repoIDs []int
	if len(spec.repos) > 0 {
		for _, full := range spec.repos {
			if r, ok := s.store.ReposByName[full]; ok {
				repoIDs = append(repoIDs, r.ID)
			}
		}
	} else {
		for id := range s.store.Repos {
			repoIDs = append(repoIDs, id)
		}
	}
	repoSet := make(map[int]bool, len(repoIDs))
	for _, id := range repoIDs {
		if repoReadable(s.store.Repos[id]) {
			repoSet[id] = true
		}
	}

	loginOf := func(userID int) string {
		if u, ok := s.store.Users[userID]; ok {
			return u.Login
		}
		return ""
	}
	stateMatches := func(state string) bool {
		if len(spec.states) == 0 {
			return true
		}
		for _, want := range spec.states {
			if state == want {
				return true
			}
		}
		return false
	}
	keywordsMatch := func(title, body string) bool {
		haystack := strings.ToLower(title + "\n" + body)
		for _, kw := range spec.keywords {
			if !strings.Contains(haystack, strings.ToLower(kw)) {
				return false
			}
		}
		return true
	}
	commenterMatch := func(parentType string, parentID int, login string) bool {
		for _, c := range s.store.Comments {
			if c.ParentType == parentType && c.IssueID == parentID && loginOf(c.AuthorID) == login {
				return true
			}
		}
		return false
	}
	assigneeMatch := func(assigneeIDs []int, login string) bool {
		for _, id := range assigneeIDs {
			if loginOf(id) == login {
				return true
			}
		}
		return false
	}
	labelsMatch := func(labelIDs []int) bool {
		for _, want := range spec.labels {
			found := false
			for _, lid := range labelIDs {
				if l, ok := s.store.Labels[lid]; ok && l.Name == want {
					found = true
					break
				}
			}
			if !found {
				return false
			}
		}
		return true
	}

	var matchedIssues []*store.Issue
	var matchedPRs []*store.PullRequest

	if (spec.entity == "" || spec.entity == "issue") && spec.draft == nil {
		for _, issue := range s.store.Issues {
			if !repoSet[issue.RepoID] || !stateMatches(issue.State) {
				continue
			}
			if spec.author != "" && loginOf(issue.AuthorID) != spec.author {
				continue
			}
			if spec.assignee != "" && !assigneeMatch(issue.AssigneeIDs, spec.assignee) {
				continue
			}
			if spec.mentions != "" && !strings.Contains(issue.Body, "@"+spec.mentions) {
				continue
			}
			if spec.involves != "" &&
				loginOf(issue.AuthorID) != spec.involves &&
				!assigneeMatch(issue.AssigneeIDs, spec.involves) &&
				!commenterMatch("issue", issue.ID, spec.involves) {
				continue
			}
			if !labelsMatch(issue.LabelIDs) || !keywordsMatch(issue.Title, issue.Body) {
				continue
			}
			matchedIssues = append(matchedIssues, issue)
		}
	}
	if spec.entity == "" || spec.entity == "pr" {
		for _, pr := range s.store.PullRequests {
			if !repoSet[pr.RepoID] || !stateMatches(pr.State) {
				continue
			}
			if spec.draft != nil && pr.IsDraft != *spec.draft {
				continue
			}
			if spec.author != "" && loginOf(pr.AuthorID) != spec.author {
				continue
			}
			if spec.reviewer != "" {
				reviewer := spec.reviewer
				if reviewer == "@me" && viewer != nil {
					reviewer = viewer.Login
				}
				found := false
				for _, reviewerID := range pr.RequestedReviewerIDs {
					if strings.EqualFold(loginOf(reviewerID), reviewer) {
						found = true
						break
					}
				}
				if !found {
					continue
				}
			}
			if spec.assignee != "" && !assigneeMatch(pr.AssigneeIDs, spec.assignee) {
				continue
			}
			if spec.mentions != "" && !strings.Contains(pr.Body, "@"+spec.mentions) {
				continue
			}
			if spec.involves != "" &&
				loginOf(pr.AuthorID) != spec.involves &&
				!assigneeMatch(pr.AssigneeIDs, spec.involves) &&
				!commenterMatch("pull_request", pr.ID, spec.involves) {
				continue
			}
			if !labelsMatch(pr.LabelIDs) || !keywordsMatch(pr.Title, pr.Body) {
				continue
			}
			matchedPRs = append(matchedPRs, pr)
		}
	}
	s.store.Mu.RUnlock()

	// Render outside the lock (the toGQL converters take it themselves).
	// Collect lazily-rendered items and sort by date, so the expensive
	// issueToGQL/pullRequestToGQL runs only for the items pagination keeps
	// (GQL-026).
	type dated struct {
		created time.Time
		updated time.Time
		item    gqlConnItem
	}
	out := make([]dated, 0, len(matchedIssues)+len(matchedPRs))
	for _, issue := range matchedIssues {
		issue := issue
		out = append(out, dated{created: issue.CreatedAt, updated: issue.UpdatedAt, item: gqlConnItem{
			identity: issue.NodeID,
			render: func() map[string]interface{} {
				node := issueToGQL(issue, s.store)
				node["__typename"] = "Issue"
				return node
			},
		}})
	}
	for _, pr := range matchedPRs {
		pr := pr
		out = append(out, dated{created: pr.CreatedAt, updated: pr.UpdatedAt, item: gqlConnItem{
			identity: pr.NodeID,
			render:   func() map[string]interface{} { return pullRequestToGQL(pr, s.store) },
		}})
	}
	sort.SliceStable(out, func(a, b int) bool {
		left, right := out[a].created, out[b].created
		if spec.sortField == "updated" {
			left, right = out[a].updated, out[b].updated
		}
		if left.Equal(right) {
			return false
		}
		if spec.sortAsc {
			return left.Before(right)
		}
		return left.After(right)
	})

	items := make([]gqlConnItem, 0, len(out))
	for _, d := range out {
		items = append(items, d.item)
	}
	return items
}

// clientMutationID echoes back the input's clientMutationId, nil when absent.
func clientMutationID(input map[string]interface{}) interface{} {
	if v, ok := input["clientMutationId"].(string); ok && v != "" {
		return v
	}
	return nil
}
