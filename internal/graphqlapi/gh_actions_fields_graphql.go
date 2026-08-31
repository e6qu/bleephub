package graphqlapi

// The Actions run graph and the residual members of the Checks family.
// WorkflowRun and Workflow are minted as shells by the CheckSuite rollup
// (gh_pulls_graphql.go); this file finishes them and the remaining
// CheckSuite/CheckRun members, backed by the Actions/Checks stores or a
// truthful null/empty where bleephub models nothing.

import (
	"encoding/base64"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/e6qu/bleephub/internal/store"
	"github.com/graphql-go/graphql"
)

// addActionsFamilyFields runs after every family it depends on (Repository,
// Commit, Ref, App, Deployment, Environment, User, Team and the Check shells).
func (s *Resolver) addActionsFamilyFields() {
	s.buildActionsSupportTypes()
	s.addWorkflowRunFields()
	s.addWorkflowFileFields()
	s.addCheckSuiteResidueFields()
	s.addCheckRunResidueFields()
}

// actionsFamilyTypes holds the leaf types and connections built once by
// buildActionsSupportTypes.
type actionsFamilyTypes struct {
	checkAnnotationConnection    *graphql.Object
	checkStepConnection          *graphql.Object
	checkRunConnection           *graphql.Object
	workflowRunConnection        *graphql.Object
	workflowRunFile              *graphql.Object
	push                         *graphql.Object
	deploymentRequest            *graphql.Object
	deploymentRequestConnection  *graphql.Object
	deploymentReview             *graphql.Object
	deploymentReviewConnection   *graphql.Object
	deploymentReviewerConnection *graphql.Object
	environmentConnection        *graphql.Object
}

func (s *Resolver) buildActionsSupportTypes() {
	uri := s.graphQLStringScalar("URI")
	dateTime := s.graphQLStringScalar("DateTime")
	bigInt := s.graphQLStringScalar("BigInt")
	gitObjectID := s.graphQLStringScalar("GitObjectID")
	checkStatus := s.graphQLEnum("CheckStatusState", "COMPLETED", "IN_PROGRESS", "PENDING", "QUEUED", "REQUESTED", "WAITING")
	checkConclusion := s.graphQLEnum("CheckConclusionState",
		"ACTION_REQUIRED", "CANCELLED", "FAILURE", "NEUTRAL", "SKIPPED", "STALE", "STARTUP_FAILURE", "SUCCESS", "TIMED_OUT")
	annotationLevel := s.graphQLEnum("CheckAnnotationLevel", "FAILURE", "NOTICE", "WARNING")
	deploymentReviewState := s.graphQLEnum("DeploymentReviewState", "APPROVED", "REJECTED")

	t := &actionsFamilyTypes{}

	// Check annotations
	positionType := graphql.NewObject(graphql.ObjectConfig{
		Name: "CheckAnnotationPosition",
		Fields: graphql.Fields{
			"column": &graphql.Field{Type: graphql.Int},
			"line":   &graphql.Field{Type: graphql.NewNonNull(graphql.Int)},
		},
	})
	spanType := graphql.NewObject(graphql.ObjectConfig{
		Name: "CheckAnnotationSpan",
		Fields: graphql.Fields{
			"start": &graphql.Field{Type: graphql.NewNonNull(positionType)},
			"end":   &graphql.Field{Type: graphql.NewNonNull(positionType)},
		},
	})
	annotationType := graphql.NewObject(graphql.ObjectConfig{
		Name: "CheckAnnotation",
		Fields: graphql.Fields{
			"annotationLevel": &graphql.Field{Type: annotationLevel},
			"blobUrl":         &graphql.Field{Type: graphql.NewNonNull(uri)},
			"databaseId":      &graphql.Field{Type: graphql.Int},
			"fullDatabaseId":  &graphql.Field{Type: bigInt},
			"location":        &graphql.Field{Type: graphql.NewNonNull(spanType)},
			"message":         &graphql.Field{Type: graphql.NewNonNull(graphql.String)},
			"path":            &graphql.Field{Type: graphql.NewNonNull(graphql.String)},
			"rawDetails":      &graphql.Field{Type: graphql.String},
			"title":           &graphql.Field{Type: graphql.String},
		},
	})
	t.checkAnnotationConnection = s.gqlConnectionType("CheckAnnotation", annotationType)

	// Check steps (bleephub models steps on jobs, not check runs)
	stepType := graphql.NewObject(graphql.ObjectConfig{
		Name: "CheckStep",
		Fields: graphql.Fields{
			"completedAt":         &graphql.Field{Type: dateTime},
			"conclusion":          &graphql.Field{Type: checkConclusion},
			"externalId":          &graphql.Field{Type: graphql.String},
			"name":                &graphql.Field{Type: graphql.NewNonNull(graphql.String)},
			"number":              &graphql.Field{Type: graphql.NewNonNull(graphql.Int)},
			"secondsToCompletion": &graphql.Field{Type: graphql.Int},
			"startedAt":           &graphql.Field{Type: dateTime},
			"status":              &graphql.Field{Type: graphql.NewNonNull(checkStatus)},
		},
	})
	t.checkStepConnection = s.gqlConnectionType("CheckStep", stepType)

	// Check-run / workflow-run connections
	t.checkRunConnection = s.gqlConnectionType("CheckRun", s.graphqlTypes.checkRun)
	t.workflowRunConnection = s.gqlConnectionType("WorkflowRun", s.graphqlTypes.workflowRun)

	// Push (a run's originating git push; not modeled — always null)
	t.push = graphql.NewObject(graphql.ObjectConfig{
		Name: "Push",
		Fields: graphql.Fields{
			"id":          &graphql.Field{Type: graphql.NewNonNull(graphql.ID)},
			"nextSha":     &graphql.Field{Type: gitObjectID},
			"permalink":   &graphql.Field{Type: graphql.NewNonNull(uri)},
			"previousSha": &graphql.Field{Type: gitObjectID},
			"pusher":      &graphql.Field{Type: graphql.NewNonNull(s.graphqlTypes.actor)},
			"repository":  &graphql.Field{Type: graphql.NewNonNull(s.graphqlTypes.repository)},
		},
	})

	// WorkflowRunFile (the workflow file, from the run's perspective)
	t.workflowRunFile = graphql.NewObject(graphql.ObjectConfig{
		Name: "WorkflowRunFile",
		Fields: graphql.Fields{
			"id":                &graphql.Field{Type: graphql.NewNonNull(graphql.ID)},
			"path":              &graphql.Field{Type: graphql.NewNonNull(graphql.String)},
			"repositoryFileUrl": &graphql.Field{Type: graphql.NewNonNull(uri)},
			"repositoryName":    &graphql.Field{Type: graphql.NewNonNull(uri)},
			"resourcePath":      &graphql.Field{Type: graphql.NewNonNull(uri)},
			"run": &graphql.Field{
				Type:    graphql.NewNonNull(s.graphqlTypes.workflowRun),
				Resolve: sourceKeyResolver("run"),
			},
			"url": &graphql.Field{Type: graphql.NewNonNull(uri)},
			"viewerCanPushRepository": &graphql.Field{
				Type: graphql.NewNonNull(graphql.Boolean),
				Resolve: func(p graphql.ResolveParams) (interface{}, error) {
					repo := s.store.GetRepoByFullName(sourceString(p.Source, "repoFullName"))
					return repo != nil && s.viewerCanPushRepo(p.Context, repo), nil
				},
			},
			"viewerCanReadRepository": &graphql.Field{
				Type: graphql.NewNonNull(graphql.Boolean),
				Resolve: func(p graphql.ResolveParams) (interface{}, error) {
					repo := s.store.GetRepoByFullName(sourceString(p.Source, "repoFullName"))
					return repo != nil && s.viewerCanReadRepo(p.Context, repo), nil
				},
			},
		},
	})

	// Reuse the account deployment surface's connection objects; the schema
	// keeps one type of each name.
	t.deploymentReviewerConnection = s.graphqlTypes.deploymentReviewerConnection
	t.environmentConnection = s.graphqlTypes.environmentConnection

	// DeploymentRequest
	t.deploymentRequest = graphql.NewObject(graphql.ObjectConfig{
		Name: "DeploymentRequest",
		Fields: graphql.Fields{
			"currentUserCanApprove": &graphql.Field{
				Type:    graphql.NewNonNull(graphql.Boolean),
				Resolve: sourceKeyResolverDefault("currentUserCanApprove", false),
			},
			"environment": &graphql.Field{
				Type:    graphql.NewNonNull(s.graphqlTypes.environment),
				Resolve: sourceKeyResolver("environment"),
			},
			"reviewers": &graphql.Field{
				Type:    graphql.NewNonNull(t.deploymentReviewerConnection),
				Args:    relayConnectionArgs(),
				Resolve: connectionFromSourceKey("reviewers"),
			},
			"waitTimer": &graphql.Field{
				Type:    graphql.NewNonNull(graphql.Int),
				Resolve: sourceKeyResolverDefault("waitTimer", 0),
			},
			"waitTimerStartedAt": &graphql.Field{Type: dateTime},
		},
	})
	t.deploymentRequestConnection = s.gqlConnectionType("DeploymentRequest", t.deploymentRequest)

	// DeploymentReview
	t.deploymentReview = graphql.NewObject(graphql.ObjectConfig{
		Name: "DeploymentReview",
		Fields: graphql.Fields{
			"comment":    &graphql.Field{Type: graphql.NewNonNull(graphql.String)},
			"databaseId": &graphql.Field{Type: graphql.Int},
			"environments": &graphql.Field{
				Type:    graphql.NewNonNull(t.environmentConnection),
				Args:    relayConnectionArgs(),
				Resolve: connectionFromSourceKey("environments"),
			},
			"id":    &graphql.Field{Type: graphql.NewNonNull(graphql.ID)},
			"state": &graphql.Field{Type: graphql.NewNonNull(deploymentReviewState)},
			"user": &graphql.Field{
				Type:    graphql.NewNonNull(s.graphqlTypes.user),
				Resolve: sourceKeyResolver("user"),
			},
		},
	})
	t.deploymentReviewConnection = s.gqlConnectionType("DeploymentReview", t.deploymentReview)

	s.actionsTypes = t
}

// WorkflowRun

func (s *Resolver) addWorkflowRunFields() {
	runType := s.graphqlTypes.workflowRun
	uri := s.graphQLStringScalar("URI")
	dateTime := s.graphQLStringScalar("DateTime")
	t := s.actionsTypes

	runType.AddFieldConfig("id", &graphql.Field{Type: graphql.NewNonNull(graphql.ID), Resolve: sourceKeyResolver("id")})
	runType.AddFieldConfig("databaseId", &graphql.Field{Type: graphql.Int, Resolve: sourceKeyResolver("databaseId")})
	runType.AddFieldConfig("runNumber", &graphql.Field{Type: graphql.NewNonNull(graphql.Int), Resolve: sourceKeyResolverDefault("runNumber", 0)})
	runType.AddFieldConfig("runAttempt", &graphql.Field{Type: graphql.NewNonNull(graphql.Int), Resolve: sourceKeyResolverDefault("runAttempt", 1)})
	runType.AddFieldConfig("event", &graphql.Field{Type: graphql.NewNonNull(graphql.String), Resolve: sourceKeyResolverDefault("event", "")})
	runType.AddFieldConfig("displayTitle", &graphql.Field{Type: graphql.String, Resolve: sourceKeyResolver("displayTitle")})
	runType.AddFieldConfig("createdAt", &graphql.Field{Type: graphql.NewNonNull(dateTime), Resolve: sourceKeyResolver("createdAt")})
	runType.AddFieldConfig("updatedAt", &graphql.Field{Type: graphql.NewNonNull(dateTime), Resolve: sourceKeyResolver("updatedAt")})
	runType.AddFieldConfig("resourcePath", &graphql.Field{
		Type:    graphql.NewNonNull(uri),
		Resolve: func(p graphql.ResolveParams) (interface{}, error) { return workflowRunResourcePath(p.Source), nil },
	})
	runType.AddFieldConfig("url", &graphql.Field{
		Type: graphql.NewNonNull(uri),
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			return externalURL(workflowRunResourcePath(p.Source)), nil
		},
	})
	runType.AddFieldConfig("checkSuite", &graphql.Field{
		Type: graphql.NewNonNull(s.graphqlTypes.checkSuite),
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			suiteID := sourceInt64(p.Source, "checkSuiteID")
			s.store.Mu.RLock()
			suite := s.store.CheckSuites[suiteID]
			source := checkSuiteGraphQLSourceLocked(s.store, suite)
			s.store.Mu.RUnlock()
			return source, nil
		},
	})
	runType.AddFieldConfig("file", &graphql.Field{
		Type: t.workflowRunFile,
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			repoFullName := sourceString(p.Source, "repoFullName")
			fileID := sourceInt64(p.Source, "workflowFileID")
			if repoFullName == "" || fileID == 0 {
				return nil, nil
			}
			file := s.store.GetWorkflowFile(repoFullName, fileID)
			if file == nil {
				return nil, nil
			}
			runSource, _ := p.Source.(map[string]interface{})
			return optionalObject(workflowRunFileGQLSource(file, runSource)), nil
		},
	})
	runType.AddFieldConfig("deploymentReviews", &graphql.Field{
		Type: graphql.NewNonNull(t.deploymentReviewConnection),
		Args: relayConnectionArgs(),
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			return paginateGQLMaps(s.workflowRunDeploymentReviews(p.Source), p.Args), nil
		},
	})
	runType.AddFieldConfig("pendingDeploymentRequests", &graphql.Field{
		Type: graphql.NewNonNull(t.deploymentRequestConnection),
		Args: relayConnectionArgs(),
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			return paginateGQLMaps(s.workflowRunPendingDeployments(p.Source), p.Args), nil
		},
	})
}

func workflowRunResourcePath(source interface{}) string {
	repoFullName := sourceString(source, "repoFullName")
	runID := sourceInt(source, "runID")
	return "/" + repoFullName + "/actions/runs/" + strconv.Itoa(runID)
}

// workflowRunGQLSourceLocked renders a run as the WorkflowRun source map.
// Caller holds st.Mu.
func workflowRunGQLSourceLocked(st *store.Store, wf *store.Workflow) map[string]interface{} {
	if wf == nil {
		return nil
	}
	ts := wf.CreatedAt.UTC().Format(time.RFC3339)
	source := map[string]interface{}{
		"id":             "WFR_" + wf.ID,
		"databaseId":     wf.RunID,
		"runNumber":      wf.RunNumber,
		"runAttempt":     wf.AttemptNumber(),
		"event":          wf.EventName,
		"displayTitle":   wf.RunDisplayTitle(),
		"createdAt":      ts,
		"updatedAt":      ts,
		"repoFullName":   wf.RepoFullName,
		"runID":          wf.RunID,
		"checkSuiteID":   wf.CheckSuiteID,
		"workflowFileID": wf.WorkflowFileID,
		// Back deploymentReviews / pendingDeploymentRequests.
		"_envApprovals":       wf.EnvApprovals,
		"_pendingDeployments": wf.PendingDeployments,
	}
	// WorkflowRun.workflow: resolve the backing file, else synthesize from the
	// run's own name/path.
	if wf.WorkflowFileID != 0 {
		// Caller holds st.Mu (this is a ...Locked renderer); use the lock-free
		// lookup so we don't recursively read-lock and deadlock under a writer.
		if file := st.GetWorkflowFileLocked(wf.RepoFullName, wf.WorkflowFileID); file != nil {
			source["workflow"] = workflowFileGQLSource(file)
		}
	}
	if source["workflow"] == nil {
		source["workflow"] = map[string]interface{}{
			"name":         wf.Name,
			"id":           "WF_" + wf.WorkflowFilePath,
			"databaseId":   nil,
			"repoFullName": wf.RepoFullName,
			"path":         wf.WorkflowFilePath,
			"state":        "ACTIVE",
			"createdAt":    ts,
			"updatedAt":    ts,
		}
	}
	return source
}

// Workflow (the file)

func (s *Resolver) addWorkflowFileFields() {
	fileType := s.graphqlTypes.workflow
	uri := s.graphQLStringScalar("URI")
	dateTime := s.graphQLStringScalar("DateTime")
	workflowState := s.graphQLEnum("WorkflowState", "ACTIVE", "DELETED", "DISABLED_FORK", "DISABLED_INACTIVITY", "DISABLED_MANUALLY")
	t := s.actionsTypes

	fileType.AddFieldConfig("id", &graphql.Field{Type: graphql.NewNonNull(graphql.ID), Resolve: sourceKeyResolver("id")})
	fileType.AddFieldConfig("databaseId", &graphql.Field{Type: graphql.Int, Resolve: sourceKeyResolver("databaseId")})
	fileType.AddFieldConfig("createdAt", &graphql.Field{Type: graphql.NewNonNull(dateTime), Resolve: sourceKeyResolver("createdAt")})
	fileType.AddFieldConfig("updatedAt", &graphql.Field{Type: graphql.NewNonNull(dateTime), Resolve: sourceKeyResolver("updatedAt")})
	fileType.AddFieldConfig("state", &graphql.Field{Type: graphql.NewNonNull(workflowState), Resolve: sourceKeyResolverDefault("state", "ACTIVE")})
	fileType.AddFieldConfig("resourcePath", &graphql.Field{
		Type:    graphql.NewNonNull(uri),
		Resolve: func(p graphql.ResolveParams) (interface{}, error) { return workflowFileResourcePath(p.Source), nil },
	})
	fileType.AddFieldConfig("url", &graphql.Field{
		Type: graphql.NewNonNull(uri),
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			return externalURL(workflowFileResourcePath(p.Source)), nil
		},
	})
	fileType.AddFieldConfig("runs", &graphql.Field{
		Type: graphql.NewNonNull(t.workflowRunConnection),
		Args: relayConnectionArgs(),
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			repoFullName := sourceString(p.Source, "repoFullName")
			fileID := sourceInt64(p.Source, "databaseId")
			s.store.Mu.RLock()
			runs := make([]*store.Workflow, 0)
			for _, wf := range s.store.Workflows {
				if wf.RepoFullName == repoFullName && wf.WorkflowFileID == fileID {
					runs = append(runs, wf)
				}
			}
			nodes := make([]map[string]interface{}, 0, len(runs))
			for _, wf := range runs {
				nodes = append(nodes, workflowRunGQLSourceLocked(s.store, wf))
			}
			s.store.Mu.RUnlock()
			sort.Slice(nodes, func(a, b int) bool { return sourceInt(nodes[a], "runID") > sourceInt(nodes[b], "runID") })
			return paginateGQLMaps(nodes, p.Args), nil
		},
	})
}

func workflowFileResourcePath(source interface{}) string {
	repoFullName := sourceString(source, "repoFullName")
	path := sourceString(source, "path")
	base := path
	if idx := strings.LastIndex(path, "/"); idx >= 0 {
		base = path[idx+1:]
	}
	return "/" + repoFullName + "/actions/workflows/" + base
}

func workflowFileGQLSource(file *store.WorkflowFile) map[string]interface{} {
	if file == nil {
		return nil
	}
	return map[string]interface{}{
		"name":         file.Name,
		"id":           file.NodeID,
		"databaseId":   int(file.ID),
		"repoFullName": file.RepoFullName,
		"path":         file.Path,
		"state":        workflowStateFor(file.State),
		"createdAt":    file.CreatedAt.UTC().Format(time.RFC3339),
		"updatedAt":    file.UpdatedAt.UTC().Format(time.RFC3339),
	}
}

func workflowStateFor(state string) string {
	switch strings.ToLower(state) {
	case "", "active":
		return "ACTIVE"
	case "deleted":
		return "DELETED"
	case "disabled_fork":
		return "DISABLED_FORK"
	case "disabled_inactivity":
		return "DISABLED_INACTIVITY"
	case "disabled_manually":
		return "DISABLED_MANUALLY"
	default:
		return strings.ToUpper(state)
	}
}

func workflowRunFileGQLSource(file *store.WorkflowFile, runSource map[string]interface{}) map[string]interface{} {
	repoFullName := file.RepoFullName
	resourcePath := "/" + repoFullName + "/blob/HEAD/" + file.Path
	return map[string]interface{}{
		"id":                file.NodeID,
		"path":              file.Path,
		"repositoryFileUrl": externalURL(resourcePath),
		"repositoryName":    externalURL("/" + repoFullName),
		"resourcePath":      resourcePath,
		"url":               externalURL(resourcePath),
		"repoFullName":      repoFullName,
		"run":               runSource,
	}
}

// CheckSuite residue

func (s *Resolver) addCheckSuiteResidueFields() {
	suiteType := s.graphqlTypes.checkSuite
	uri := s.graphQLStringScalar("URI")
	t := s.actionsTypes

	suiteType.AddFieldConfig("app", &graphql.Field{
		Type: s.gqlAppType(),
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			appID := sourceInt(p.Source, "appID")
			if appID == 0 {
				return nil, nil
			}
			return optionalRendered(s.store.GetApp(appID), appGQLSource), nil
		},
	})
	suiteType.AddFieldConfig("branch", &graphql.Field{
		Type: s.graphqlTypes.ref,
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			repoKey := sourceString(p.Source, "repoKey")
			branch := sourceString(p.Source, "headBranch")
			sha := sourceString(p.Source, "headSHA")
			if repoKey == "" || branch == "" {
				return nil, nil
			}
			return gitRefSource(repoKey, "refs/heads/"+branch, sha), nil
		},
	})
	suiteType.AddFieldConfig("checkRuns", &graphql.Field{
		Type: t.checkRunConnection,
		Args: relayConnectionArgs(),
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			suiteID := sourceInt64(p.Source, "suiteID")
			repoKey := sourceString(p.Source, "repoKey")
			if suiteID == 0 {
				return paginateGQLMaps(nil, p.Args), nil
			}
			runs := s.store.ListCheckRunsForSuite(suiteID)
			s.store.Mu.RLock()
			nodes := make([]map[string]interface{}, 0, len(runs))
			for _, cr := range runs {
				var conclusion interface{}
				if cr.Conclusion != "" {
					conclusion = strings.ToUpper(cr.Conclusion)
				}
				var completedAt interface{}
				if cr.CompletedAt != nil {
					completedAt = cr.CompletedAt.Format(time.RFC3339)
				}
				suiteSource := checkSuiteGraphQLSourceLocked(s.store, s.store.CheckSuites[cr.SuiteID])
				nodes = append(nodes, checkRunGraphQLSource(cr, repoKey, conclusion, completedAt, suiteSource))
			}
			s.store.Mu.RUnlock()
			return paginateGQLMaps(nodes, p.Args), nil
		},
	})
	suiteType.AddFieldConfig("commit", &graphql.Field{
		Type: graphql.NewNonNull(s.graphqlTypes.commit),
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			repoKey := sourceString(p.Source, "repoKey")
			sha := sourceString(p.Source, "headSHA")
			if commit := s.commitSourceForRepoSHA(p.Context, repoKey, sha); commit != nil {
				return commit, nil
			}
			// CheckSuite.commit is non-null; keep id/oid truthful when the
			// object is unreadable or gone.
			return gitObjectSourceFields("Commit", repoKey, sha), nil
		},
	})
	suiteType.AddFieldConfig("creator", &graphql.Field{
		Type: s.graphqlTypes.user,
		// bleephub does not attribute a check suite to a creating account.
		Resolve: func(graphql.ResolveParams) (interface{}, error) { return nil, nil },
	})
	suiteType.AddFieldConfig("matchingPullRequests", &graphql.Field{
		Type: s.graphqlTypes.pullRequestConnection,
		Args: relayConnectionArgs(),
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			repoKey := sourceString(p.Source, "repoKey")
			sha := sourceString(p.Source, "headSHA")
			repo := s.store.GetRepoByFullName(repoKey)
			if repo == nil || sha == "" {
				return paginateGQLMaps(nil, p.Args), nil
			}
			nodes := make([]map[string]interface{}, 0)
			for _, pr := range s.store.ListPullRequests(repo.ID, "") {
				if s.prHeadSha(repo, pr) == sha {
					nodes = append(nodes, pullRequestToGQL(pr, s.store))
				}
			}
			return paginateGQLMaps(nodes, p.Args), nil
		},
	})
	suiteType.AddFieldConfig("push", &graphql.Field{
		Type: t.push,
		// The originating git push is not modeled.
		Resolve: func(graphql.ResolveParams) (interface{}, error) { return nil, nil },
	})
	suiteType.AddFieldConfig("repository", &graphql.Field{
		Type: graphql.NewNonNull(s.graphqlTypes.repository),
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			repo := s.store.GetRepoByFullName(sourceString(p.Source, "repoKey"))
			if repo == nil {
				return nil, fmt.Errorf("check suite repository %q not found", sourceString(p.Source, "repoKey"))
			}
			return repoToGraphQL(s.store, repo), nil
		},
	})
	suiteType.AddFieldConfig("resourcePath", &graphql.Field{
		Type:    graphql.NewNonNull(uri),
		Resolve: func(p graphql.ResolveParams) (interface{}, error) { return checkSuiteResourcePath(p.Source), nil },
	})
	suiteType.AddFieldConfig("url", &graphql.Field{
		Type: graphql.NewNonNull(uri),
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			return externalURL(checkSuiteResourcePath(p.Source)), nil
		},
	})
}

func checkSuiteResourcePath(source interface{}) string {
	repoKey := sourceString(source, "repoKey")
	sha := sourceString(source, "headSHA")
	return "/" + repoKey + "/commit/" + sha + "/checks"
}

// CheckRun residue

func (s *Resolver) addCheckRunResidueFields() {
	runType := s.graphqlTypes.checkRun
	uri := s.graphQLStringScalar("URI")
	t := s.actionsTypes

	runType.AddFieldConfig("annotations", &graphql.Field{
		Type: t.checkAnnotationConnection,
		Args: relayConnectionArgs(),
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			repoKey := sourceString(p.Source, "repoKey")
			sha := sourceString(p.Source, "headSHA")
			raw, _ := sourceMap(p.Source)["annotations"].([]*store.CheckAnnotation)
			nodes := make([]map[string]interface{}, 0, len(raw))
			for _, a := range raw {
				nodes = append(nodes, checkAnnotationGQLSource(a, repoKey, sha))
			}
			return paginateGQLMaps(nodes, p.Args), nil
		},
	})
	runType.AddFieldConfig("deployment", &graphql.Field{
		Type: s.graphqlTypes.deployment,
		// bleephub check runs are not linked to a deployment record.
		Resolve: func(graphql.ResolveParams) (interface{}, error) { return nil, nil },
	})
	runType.AddFieldConfig("pendingDeploymentRequest", &graphql.Field{
		Type: t.deploymentRequest,
		// Pending requests live on the workflow run's environments, not the
		// check run.
		Resolve: func(graphql.ResolveParams) (interface{}, error) { return nil, nil },
	})
	runType.AddFieldConfig("permalink", &graphql.Field{
		Type: graphql.NewNonNull(uri),
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			return externalURL(checkRunResourcePath(p.Source)), nil
		},
	})
	runType.AddFieldConfig("repository", &graphql.Field{
		Type: graphql.NewNonNull(s.graphqlTypes.repository),
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			repo := s.store.GetRepoByFullName(sourceString(p.Source, "repoKey"))
			if repo == nil {
				return nil, fmt.Errorf("check run repository %q not found", sourceString(p.Source, "repoKey"))
			}
			return repoToGraphQL(s.store, repo), nil
		},
	})
	runType.AddFieldConfig("resourcePath", &graphql.Field{
		Type:    graphql.NewNonNull(uri),
		Resolve: func(p graphql.ResolveParams) (interface{}, error) { return checkRunResourcePath(p.Source), nil },
	})
	runType.AddFieldConfig("steps", &graphql.Field{
		Type: t.checkStepConnection,
		Args: relayConnectionArgs(),
		// Steps are recorded on the workflow job, not the check run.
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			return paginateGQLMaps(nil, p.Args), nil
		},
	})
	runType.AddFieldConfig("url", &graphql.Field{
		Type: graphql.NewNonNull(uri),
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			return externalURL(checkRunResourcePath(p.Source)), nil
		},
	})
}

func checkRunResourcePath(source interface{}) string {
	repoKey := sourceString(source, "repoKey")
	id := sourceInt64(source, "checkRunID")
	return "/" + repoKey + "/runs/" + strconv.FormatInt(id, 10)
}

func checkAnnotationGQLSource(a *store.CheckAnnotation, repoKey, sha string) map[string]interface{} {
	if a == nil {
		return nil
	}
	position := func(line int, column *int) map[string]interface{} {
		out := map[string]interface{}{"line": line, "column": nil}
		if column != nil {
			out["column"] = *column
		}
		return out
	}
	var level interface{}
	if a.AnnotationLevel != "" {
		level = strings.ToUpper(a.AnnotationLevel)
	}
	return map[string]interface{}{
		"annotationLevel": level,
		"blobUrl":         externalURL("/" + repoKey + "/blob/" + sha + "/" + a.Path),
		"databaseId":      nil,
		"fullDatabaseId":  nil,
		"location": map[string]interface{}{
			"start": position(a.StartLine, a.StartColumn),
			"end":   position(a.EndLine, a.EndColumn),
		},
		"message":    a.Message,
		"path":       a.Path,
		"rawDetails": nilStr(a.RawDetails),
		"title":      nilStr(a.Title),
	}
}

// deployment review / request wiring

func (s *Resolver) workflowRunDeploymentReviews(source interface{}) []map[string]interface{} {
	approvals, _ := sourceMap(source)["_envApprovals"].([]*store.EnvApproval)
	repoFullName := sourceString(source, "repoFullName")
	runID := sourceInt(source, "runID")
	nodes := make([]map[string]interface{}, 0, len(approvals))
	for i, ea := range approvals {
		if ea == nil {
			continue
		}
		state := "APPROVED"
		if strings.EqualFold(ea.State, "rejected") {
			state = "REJECTED"
		}
		nodes = append(nodes, map[string]interface{}{
			"id":           deploymentReviewNodeID(repoFullName, runID, i),
			"comment":      ea.Comment,
			"databaseId":   nil,
			"state":        state,
			"user":         optionalRendered(s.store.GetUserByID(ea.UserID), userToGraphQL),
			"environments": s.environmentSourcesForIDs(repoFullName, ea.EnvIDs),
		})
	}
	return nodes
}

func (s *Resolver) workflowRunPendingDeployments(source interface{}) []map[string]interface{} {
	pending, _ := sourceMap(source)["_pendingDeployments"].([]*store.PendingDeployment)
	repoFullName := sourceString(source, "repoFullName")
	repo := s.store.GetRepoByFullName(repoFullName)
	if repo == nil {
		return nil
	}
	repoSource := repoToGraphQL(s.store, repo)
	nodes := make([]map[string]interface{}, 0, len(pending))
	for _, pd := range pending {
		if pd == nil {
			continue
		}
		env := s.store.Deployments.GetEnvironmentByID(pd.EnvID)
		if env == nil {
			continue
		}
		envSource := s.environmentSource(repo, env, repoSource)
		waitTimer := env.WaitTimer
		node := map[string]interface{}{
			"currentUserCanApprove": false,
			"environment":           envSource,
			"waitTimer":             waitTimer,
			"reviewers":             s.environmentReviewerSources(env),
		}
		if !pd.WaitTimerStartedAt.IsZero() {
			node["waitTimerStartedAt"] = pd.WaitTimerStartedAt.UTC().Format(time.RFC3339)
		} else {
			node["waitTimerStartedAt"] = nil
		}
		nodes = append(nodes, node)
	}
	return nodes
}

// environmentSourcesForIDs renders the environments a review names, skipping
// any that no longer exist.
func (s *Resolver) environmentSourcesForIDs(repoFullName string, envIDs []int) []map[string]interface{} {
	repo := s.store.GetRepoByFullName(repoFullName)
	if repo == nil {
		return nil
	}
	repoSource := repoToGraphQL(s.store, repo)
	out := make([]map[string]interface{}, 0, len(envIDs))
	for _, id := range envIDs {
		if env := s.store.Deployments.GetEnvironmentByID(id); env != nil {
			out = append(out, s.environmentSource(repo, env, repoSource))
		}
	}
	return out
}

func deploymentReviewNodeID(repoFullName string, runID, index int) string {
	return "DREV_" + base64.RawURLEncoding.EncodeToString([]byte(repoFullName+":"+strconv.Itoa(runID)+":"+strconv.Itoa(index)))
}

// App source

func appGQLSource(app *store.App) map[string]interface{} {
	return map[string]interface{}{
		"id":          app.NodeID,
		"databaseId":  app.ID,
		"clientId":    nilStr(app.ClientID),
		"description": nilStr(app.Description),
		"name":        app.Name,
		"slug":        app.Slug,
		"createdAt":   app.CreatedAt.UTC().Format(time.RFC3339),
		"updatedAt":   app.UpdatedAt.UTC().Format(time.RFC3339),
	}
}

// small source helpers

func sourceMap(source interface{}) map[string]interface{} {
	m, _ := source.(map[string]interface{})
	return m
}

func sourceString(source interface{}, key string) string {
	v, _ := sourceMap(source)[key].(string)
	return v
}

func sourceInt(source interface{}, key string) int {
	switch v := sourceMap(source)[key].(type) {
	case int:
		return v
	case int64:
		return int(v)
	}
	return 0
}

func sourceInt64(source interface{}, key string) int64 {
	switch v := sourceMap(source)[key].(type) {
	case int64:
		return v
	case int:
		return int64(v)
	}
	return 0
}

func sourceKeyResolverDefault(key string, fallback interface{}) graphql.FieldResolveFn {
	return func(p graphql.ResolveParams) (interface{}, error) {
		if v, ok := sourceMap(p.Source)[key]; ok && v != nil {
			return v, nil
		}
		return fallback, nil
	}
}

// connectionFromSourceKey paginates a slice of already-rendered node maps
// carried on the source under key.
func connectionFromSourceKey(key string) graphql.FieldResolveFn {
	return func(p graphql.ResolveParams) (interface{}, error) {
		nodes, _ := sourceMap(p.Source)[key].([]map[string]interface{})
		return paginateGQLMaps(nodes, p.Args), nil
	}
}
