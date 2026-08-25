package graphqlapi

// The Checks mutation family: createCheckRun, createCheckSuite,
// rerequestCheckSuite, updateCheckRun and updateCheckSuitePreferences.
//
// Every mutation writes through the same store primitives the REST checks
// routes write — CreateCheckRun/UpdateCheckRun, CreateCheckSuite/
// UpdateCheckSuite and SetCheckSuitePreferences — releases the same armed
// auto-merges when a run lands completed, and emits the same check_suite
// "rerequested" webhook, so a check reported over GraphQL is
// indistinguishable from one reported over REST.

import (
	"fmt"
	"strings"
	"time"

	"github.com/graphql-go/graphql"

	"github.com/e6qu/bleephub/internal/store"
)

func init() {
	for name, rule := range map[string]mutationRule{
		// GitHub gates the checks writes on the Checks permission at write;
		// reporting a check is a write on the repository, so the standing is
		// push, exactly as the REST routes' requirePerm(checks, write).
		"createCheckRun":      repoRule{scope: store.ScopeChecks, level: mutationPushRepo, target: mutationTargetRepo("repositoryId")},
		"createCheckSuite":    repoRule{scope: store.ScopeChecks, level: mutationPushRepo, target: mutationTargetRepo("repositoryId")},
		"rerequestCheckSuite": repoRule{scope: store.ScopeChecks, level: mutationPushRepo, target: mutationTargetRepo("repositoryId")},
		"updateCheckRun":      repoRule{scope: store.ScopeChecks, level: mutationPushRepo, target: mutationTargetRepo("repositoryId")},
		// The auto-trigger preferences are a repository setting: the REST
		// route demands Administration at write, so the GraphQL one does too.
		"updateCheckSuitePreferences": repoRule{scope: store.ScopeAdministration, level: mutationAdminRepo, target: mutationTargetRepo("repositoryId")},
	} {
		if _, exists := graphqlMutationAuthz[name]; exists {
			panic(fmt.Sprintf("graphql mutation %q already has a policy row", name))
		}
		graphqlMutationAuthz[name] = rule
	}
}

func (s *Resolver) addChecksMutationsToSchema(mutationType *graphql.Object) {
	dateTime := s.graphQLStringScalar("DateTime")
	uri := s.graphQLStringScalar("URI")
	gitObjectID := s.graphQLStringScalar("GitObjectID")
	requestableStatus := s.sharedEnum("RequestableCheckStatusState",
		"COMPLETED", "IN_PROGRESS", "PENDING", "QUEUED", "WAITING")
	// CheckConclusionState already exists in the enum memo (the rollup's
	// CheckRun.conclusion names it); asking with the same values returns it.
	conclusionEnum := s.graphQLEnum(
		"CheckConclusionState",
		"ACTION_REQUIRED", "CANCELLED", "FAILURE", "NEUTRAL", "SKIPPED", "STALE",
		"STARTUP_FAILURE", "SUCCESS", "TIMED_OUT",
	)
	annotationLevel := s.sharedEnum("CheckAnnotationLevel", "FAILURE", "NOTICE", "WARNING")

	checkRunActionInput := s.mutationInput("CheckRunAction", graphql.InputObjectConfigFieldMap{
		"description": gqlNonNullString(),
		"identifier":  gqlNonNullString(),
		"label":       gqlNonNullString(),
	})
	annotationRangeInput := s.mutationInput("CheckAnnotationRange", graphql.InputObjectConfigFieldMap{
		"endColumn":   gqlInt(),
		"endLine":     gqlNonNullInt(),
		"startColumn": gqlInt(),
		"startLine":   gqlNonNullInt(),
	})
	annotationDataInput := s.mutationInput("CheckAnnotationData", graphql.InputObjectConfigFieldMap{
		"annotationLevel": gqlNonNullInputOf(annotationLevel),
		"location":        gqlNonNullInputOf(annotationRangeInput),
		"message":         gqlNonNullString(),
		"path":            gqlNonNullString(),
		"rawDetails":      gqlString(),
		"title":           gqlString(),
	})
	outputImageInput := s.mutationInput("CheckRunOutputImage", graphql.InputObjectConfigFieldMap{
		"alt":      gqlNonNullString(),
		"caption":  gqlString(),
		"imageUrl": gqlNonNullInputOf(uri),
	})
	checkRunOutputInput := s.mutationInput("CheckRunOutput", graphql.InputObjectConfigFieldMap{
		"annotations": gqlListOf(annotationDataInput),
		"images":      gqlListOf(outputImageInput),
		"summary":     gqlNonNullString(),
		"text":        gqlString(),
		"title":       gqlNonNullString(),
	})
	autoTriggerInput := s.mutationInput("CheckSuiteAutoTriggerPreference", graphql.InputObjectConfigFieldMap{
		"appId":   gqlNonNullID(),
		"setting": gqlNonNullBool(),
	})

	s.registerMutation(mutationType, "createCheckRun", &graphql.Field{
		Type: s.mutationPayload("CreateCheckRunPayload", graphql.Fields{
			"checkRun": gqlField(s.graphqlTypes.checkRun),
		}),
		Args: graphql.FieldConfigArgument{"input": &graphql.ArgumentConfig{
			Type: graphql.NewNonNull(s.mutationInput("CreateCheckRunInput", graphql.InputObjectConfigFieldMap{
				"actions":      gqlListOf(checkRunActionInput),
				"completedAt":  gqlInputOf(dateTime),
				"conclusion":   gqlInputOf(conclusionEnum),
				"detailsUrl":   gqlInputOf(uri),
				"externalId":   gqlString(),
				"headSha":      gqlNonNullInputOf(gitObjectID),
				"name":         gqlNonNullString(),
				"output":       gqlInputOf(checkRunOutputInput),
				"repositoryId": gqlNonNullID(),
				"startedAt":    gqlInputOf(dateTime),
				"status":       gqlInputOf(requestableStatus),
			})),
		}},
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			input, _ := p.Args["input"].(map[string]interface{})
			repo := store.FindRepoByNodeID(s.store, str(input["repositoryId"]))
			if repo == nil {
				return nil, gqlMissingNode("Repository", str(input["repositoryId"]))
			}
			name := str(input["name"])
			headSha := str(input["headSha"])
			if name == "" || headSha == "" {
				return nil, fmt.Errorf("name and headSha are required")
			}
			write, err := checkRunWriteFromInput(input)
			if err != nil {
				return nil, err
			}
			created := s.store.CreateCheckRun(repo.FullName, headSha, name, 0, 0)
			s.store.UpdateCheckRun(created.ID, write.apply)
			run := s.store.GetCheckRun(created.ID)
			// A check run created already-completed can be the condition an
			// armed auto-merge was waiting for, exactly as on the REST path.
			if run != nil && run.Status == "completed" {
				s.pulls.MaybeAutoMergeHeadSHA(repo, run.HeadSHA)
			}
			return map[string]interface{}{
				"checkRun": optionalRendered(run, s.checkRunMutationSource),
			}, nil
		},
	})

	s.registerMutation(mutationType, "createCheckSuite", &graphql.Field{
		Type: s.mutationPayload("CreateCheckSuitePayload", graphql.Fields{
			"checkSuite": gqlField(s.graphqlTypes.checkSuite),
		}),
		Args: graphql.FieldConfigArgument{"input": &graphql.ArgumentConfig{
			Type: graphql.NewNonNull(s.mutationInput("CreateCheckSuiteInput", graphql.InputObjectConfigFieldMap{
				"headSha":      gqlNonNullInputOf(gitObjectID),
				"repositoryId": gqlNonNullID(),
			})),
		}},
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			input, _ := p.Args["input"].(map[string]interface{})
			repo := store.FindRepoByNodeID(s.store, str(input["repositoryId"]))
			if repo == nil {
				return nil, gqlMissingNode("Repository", str(input["repositoryId"]))
			}
			headSha := str(input["headSha"])
			if headSha == "" {
				return nil, fmt.Errorf("headSha is required")
			}
			suite := s.store.CreateCheckSuite(repo.FullName, "", headSha, 0)
			return map[string]interface{}{
				"checkSuite": optionalRendered(suite, s.checkSuiteMutationSource),
			}, nil
		},
	})

	s.registerMutation(mutationType, "rerequestCheckSuite", &graphql.Field{
		Type: s.mutationPayload("RerequestCheckSuitePayload", graphql.Fields{
			"checkSuite": gqlField(s.graphqlTypes.checkSuite),
		}),
		Args: graphql.FieldConfigArgument{"input": &graphql.ArgumentConfig{
			Type: graphql.NewNonNull(s.mutationInput("RerequestCheckSuiteInput", graphql.InputObjectConfigFieldMap{
				"checkSuiteId": gqlNonNullID(),
				"repositoryId": gqlNonNullID(),
			})),
		}},
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			input, _ := p.Args["input"].(map[string]interface{})
			repo := store.FindRepoByNodeID(s.store, str(input["repositoryId"]))
			if repo == nil {
				return nil, gqlMissingNode("Repository", str(input["repositoryId"]))
			}
			nodeID := str(input["checkSuiteId"])
			suite := s.store.FindCheckSuiteByNodeID(repo.FullName, nodeID)
			if suite == nil {
				return nil, gqlMissingNode("CheckSuite", nodeID)
			}
			s.store.UpdateCheckSuite(suite.ID, func(cs *store.CheckSuite) {
				cs.Status = "queued"
				cs.Conclusion = ""
			})
			s.events.EmitCheckSuiteEvent(repo.FullName, suite.ID, "rerequested")
			return map[string]interface{}{
				"checkSuite": optionalRendered(s.store.GetCheckSuite(suite.ID), s.checkSuiteMutationSource),
			}, nil
		},
	})

	s.registerMutation(mutationType, "updateCheckRun", &graphql.Field{
		Type: s.mutationPayload("UpdateCheckRunPayload", graphql.Fields{
			"checkRun": gqlField(s.graphqlTypes.checkRun),
		}),
		Args: graphql.FieldConfigArgument{"input": &graphql.ArgumentConfig{
			Type: graphql.NewNonNull(s.mutationInput("UpdateCheckRunInput", graphql.InputObjectConfigFieldMap{
				"actions":      gqlListOf(checkRunActionInput),
				"checkRunId":   gqlNonNullID(),
				"completedAt":  gqlInputOf(dateTime),
				"conclusion":   gqlInputOf(conclusionEnum),
				"detailsUrl":   gqlInputOf(uri),
				"externalId":   gqlString(),
				"name":         gqlString(),
				"output":       gqlInputOf(checkRunOutputInput),
				"repositoryId": gqlNonNullID(),
				"startedAt":    gqlInputOf(dateTime),
				"status":       gqlInputOf(requestableStatus),
			})),
		}},
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			input, _ := p.Args["input"].(map[string]interface{})
			repo := store.FindRepoByNodeID(s.store, str(input["repositoryId"]))
			if repo == nil {
				return nil, gqlMissingNode("Repository", str(input["repositoryId"]))
			}
			nodeID := str(input["checkRunId"])
			existing := s.store.FindCheckRunByNodeID(repo.FullName, nodeID)
			if existing == nil {
				return nil, gqlMissingNode("CheckRun", nodeID)
			}
			write, err := checkRunWriteFromInput(input)
			if err != nil {
				return nil, err
			}
			s.store.UpdateCheckRun(existing.ID, write.apply)
			run := s.store.GetCheckRun(existing.ID)
			// A run transitioning to completed can clear the condition an
			// armed auto-merge was waiting for.
			if run != nil && run.Status == "completed" {
				s.pulls.MaybeAutoMergeHeadSHA(repo, run.HeadSHA)
			}
			return map[string]interface{}{
				"checkRun": optionalRendered(run, s.checkRunMutationSource),
			}, nil
		},
	})

	s.registerMutation(mutationType, "updateCheckSuitePreferences", &graphql.Field{
		Type: s.mutationPayload("UpdateCheckSuitePreferencesPayload", graphql.Fields{
			"repository": gqlField(s.graphqlTypes.repository),
		}),
		Args: graphql.FieldConfigArgument{"input": &graphql.ArgumentConfig{
			Type: graphql.NewNonNull(s.mutationInput("UpdateCheckSuitePreferencesInput", graphql.InputObjectConfigFieldMap{
				"autoTriggerPreferences": gqlNonNullListOf(autoTriggerInput),
				"repositoryId":           gqlNonNullID(),
			})),
		}},
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			input, _ := p.Args["input"].(map[string]interface{})
			repo := store.FindRepoByNodeID(s.store, str(input["repositoryId"]))
			if repo == nil {
				return nil, gqlMissingNode("Repository", str(input["repositoryId"]))
			}
			raw, _ := input["autoTriggerPreferences"].([]interface{})
			prefs := make([]*store.CheckSuitePref, 0, len(raw))
			for _, entry := range raw {
				pref, _ := entry.(map[string]interface{})
				appNodeID := str(pref["appId"])
				app := s.appByNodeID(appNodeID)
				if app == nil {
					return nil, gqlMissingNode("App", appNodeID)
				}
				setting, _ := pref["setting"].(bool)
				prefs = append(prefs, &store.CheckSuitePref{AppID: app.ID, Setting: setting})
			}
			s.store.SetCheckSuitePreferences(repo.FullName, prefs)
			return map[string]interface{}{
				"repository": repoToGraphQL(s.store, s.store.SnapRepo(repo)),
			}, nil
		},
	})
}

// appByNodeID resolves a GitHub App's global id, or nil.
func (s *Resolver) appByNodeID(nodeID string) *store.App {
	if nodeID == "" {
		return nil
	}
	s.store.Mu.RLock()
	defer s.store.Mu.RUnlock()
	for _, app := range s.store.Apps {
		if app.NodeID == nodeID {
			cp := *app
			return &cp
		}
	}
	return nil
}

// checkRunWrite is a decoded createCheckRun/updateCheckRun input: only the
// members the client supplied are applied, which is exactly the REST PATCH
// semantics (an absent member leaves the field alone).
type checkRunWrite struct {
	name        string
	hasName     bool
	status      string
	conclusion  string
	detailsURL  *string
	externalID  *string
	startedAt   *time.Time
	completedAt *time.Time
	output      *store.CheckRunOutput
	actions     []*store.CheckRunAction
	hasActions  bool
}

func checkRunWriteFromInput(input map[string]interface{}) (*checkRunWrite, error) {
	write := &checkRunWrite{}
	if name, ok := gqlInputString(input, "name"); ok {
		write.name, write.hasName = name, true
	}
	if status, ok := gqlInputString(input, "status"); ok {
		write.status = strings.ToLower(status)
	}
	if conclusion, ok := gqlInputString(input, "conclusion"); ok {
		write.conclusion = strings.ToLower(conclusion)
	}
	if detailsURL, ok := gqlInputString(input, "detailsUrl"); ok {
		write.detailsURL = &detailsURL
	}
	if externalID, ok := gqlInputString(input, "externalId"); ok {
		write.externalID = &externalID
	}
	for key, member := range map[string]**time.Time{"startedAt": &write.startedAt, "completedAt": &write.completedAt} {
		raw, ok := gqlInputString(input, key)
		if !ok {
			continue
		}
		parsed, err := time.Parse(time.RFC3339, raw)
		if err != nil {
			return nil, fmt.Errorf("%s is not a valid DateTime: %q", key, raw)
		}
		utc := parsed.UTC()
		*member = &utc
	}
	if rawOutput, _ := input["output"].(map[string]interface{}); rawOutput != nil {
		write.output = checkRunOutputFromInput(rawOutput)
	}
	if rawActions, ok := input["actions"].([]interface{}); ok {
		write.hasActions = true
		for _, entry := range rawActions {
			action, _ := entry.(map[string]interface{})
			write.actions = append(write.actions, &store.CheckRunAction{
				Label:       str(action["label"]),
				Description: str(action["description"]),
				Identifier:  str(action["identifier"]),
			})
		}
	}
	return write, nil
}

// apply writes the supplied members onto the store record, inside
// UpdateCheckRun's lock.
func (w *checkRunWrite) apply(cr *store.CheckRun) {
	if w.hasName {
		cr.Name = w.name
	}
	if w.status != "" {
		cr.Status = w.status
	}
	if w.conclusion != "" {
		cr.Conclusion = w.conclusion
	}
	if w.detailsURL != nil {
		cr.DetailsURL = *w.detailsURL
	}
	if w.externalID != nil {
		cr.ExternalID = *w.externalID
	}
	if w.startedAt != nil {
		cr.StartedAt = *w.startedAt
	}
	if w.completedAt != nil {
		cr.CompletedAt = w.completedAt
	}
	if w.output != nil {
		cr.Output = w.output
	}
	if w.hasActions {
		cr.Actions = w.actions
	}
}

// checkRunOutputFromInput converts the CheckRunOutput input into the store's
// output bundle, the same shape the REST create/update bodies decode into.
func checkRunOutputFromInput(input map[string]interface{}) *store.CheckRunOutput {
	output := &store.CheckRunOutput{
		Title:       str(input["title"]),
		Summary:     str(input["summary"]),
		Text:        str(input["text"]),
		Annotations: []*store.CheckAnnotation{},
	}
	if raw, _ := input["annotations"].([]interface{}); raw != nil {
		for _, entry := range raw {
			data, _ := entry.(map[string]interface{})
			location, _ := data["location"].(map[string]interface{})
			annotation := &store.CheckAnnotation{
				Path:            str(data["path"]),
				Message:         str(data["message"]),
				Title:           str(data["title"]),
				RawDetails:      str(data["rawDetails"]),
				AnnotationLevel: strings.ToLower(str(data["annotationLevel"])),
			}
			if line, ok := gqlInputInt(location, "startLine"); ok {
				annotation.StartLine = line
			}
			if line, ok := gqlInputInt(location, "endLine"); ok {
				annotation.EndLine = line
			}
			if column, ok := gqlInputInt(location, "startColumn"); ok {
				annotation.StartColumn = &column
			}
			if column, ok := gqlInputInt(location, "endColumn"); ok {
				annotation.EndColumn = &column
			}
			output.Annotations = append(output.Annotations, annotation)
		}
	}
	if raw, _ := input["images"].([]interface{}); raw != nil {
		for _, entry := range raw {
			image, _ := entry.(map[string]interface{})
			output.Images = append(output.Images, &store.CheckImage{
				Alt:      str(image["alt"]),
				ImageURL: str(image["imageUrl"]),
				Caption:  str(image["caption"]),
			})
		}
	}
	output.AnnotationsCount = len(output.Annotations)
	return output
}

// checkRunMutationSource renders a check run for a mutation payload, through
// the same source builder the status-check rollup uses.
func (s *Resolver) checkRunMutationSource(cr *store.CheckRun) map[string]interface{} {
	var conclusion interface{}
	if cr.Conclusion != "" {
		conclusion = strings.ToUpper(cr.Conclusion)
	}
	var completedAt interface{}
	if cr.CompletedAt != nil {
		completedAt = cr.CompletedAt.UTC().Format(time.RFC3339)
	}
	s.store.Mu.RLock()
	suiteSource := checkSuiteGraphQLSourceLocked(s.store, s.store.CheckSuites[cr.SuiteID])
	s.store.Mu.RUnlock()
	return checkRunGraphQLSource(cr, cr.RepoKey, conclusion, completedAt, suiteSource)
}

// checkSuiteMutationSource is checkRunMutationSource for a suite payload.
func (s *Resolver) checkSuiteMutationSource(suite *store.CheckSuite) map[string]interface{} {
	s.store.Mu.RLock()
	defer s.store.Mu.RUnlock()
	return checkSuiteGraphQLSourceLocked(s.store, suite)
}
