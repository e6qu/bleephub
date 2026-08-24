package graphqlapi

// Repository metadata mutations: the label CRUD `gh label create/edit/delete`
// speaks, the topic surface, the archive/unarchive pair, the settings
// mutation `gh repo edit` sends, and the web-commit-signoff toggle.
//
// Every one of them writes through the same store primitive the REST route
// for the same change writes through — CreateLabel/UpdateLabel/DeleteLabel and
// UpdateRepo — and emits the same webhook, so a label created over GraphQL is
// indistinguishable from one created over REST, and neither can drift into
// being the only way to make a change.

import (
	"encoding/base64"
	"fmt"
	"strings"

	"github.com/graphql-go/graphql"

	"github.com/e6qu/bleephub/internal/store"
)

// topicNodeID is the global id GitHub gives a Topic node. The topic's name is
// its identity (there is no numeric key), so the id is a base64 of the type
// name and the topic name, matching the RepositoryTopic scheme in the
// conversation-fields family.
func topicNodeID(name string) string {
	return "TO_" + base64.RawURLEncoding.EncodeToString([]byte("Topic"+name))
}

// addTopicResidueFields completes the Topic object's Starrable and repository
// members. bleephub models neither topic stars nor a related-topics graph, so
// stargazerCount/viewerHasStarred/relatedTopics answer truthful zero/false/empty;
// repositories is backed by the repositories that actually carry the topic.
func (s *Resolver) addTopicResidueFields() {
	topicType := s.gqlTopicType()

	topicType.AddFieldConfig("id", &graphql.Field{
		Type: graphql.NewNonNull(graphql.ID),
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			return topicNodeID(topicSourceName(p.Source)), nil
		},
	})
	topicType.AddFieldConfig("stargazerCount", &graphql.Field{
		Type:    graphql.NewNonNull(graphql.Int),
		Resolve: func(graphql.ResolveParams) (interface{}, error) { return 0, nil },
	})
	topicType.AddFieldConfig("viewerHasStarred", &graphql.Field{
		Type:    graphql.NewNonNull(graphql.Boolean),
		Resolve: func(graphql.ResolveParams) (interface{}, error) { return false, nil },
	})
	topicType.AddFieldConfig("stargazers", &graphql.Field{
		Type: graphql.NewNonNull(s.gqlStargazerConnectionType()),
		Args: s.stargazerConnectionArgs(),
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			return repaginateConnection(gqlConnectionSource(nil), p.Args), nil
		},
	})
	topicType.AddFieldConfig("relatedTopics", &graphql.Field{
		Type: graphql.NewNonNull(graphql.NewList(graphql.NewNonNull(topicType))),
		Args: graphql.FieldConfigArgument{
			"first": &graphql.ArgumentConfig{Type: graphql.Int, DefaultValue: 3},
		},
		Resolve: func(graphql.ResolveParams) (interface{}, error) {
			// No related-topics graph is modeled; the list is empty rather than
			// invented, and non-nil so the non-null list resolves.
			return []interface{}{}, nil
		},
	})

	affiliation := s.sharedEnum("RepositoryAffiliation", "OWNER", "COLLABORATOR", "ORGANIZATION_MEMBER")
	topicType.AddFieldConfig("repositories", &graphql.Field{
		Type: graphql.NewNonNull(s.graphqlTypes.repositoryConnection),
		Args: graphql.FieldConfigArgument{
			"affiliations":      &graphql.ArgumentConfig{Type: graphql.NewList(affiliation)},
			"after":             &graphql.ArgumentConfig{Type: graphql.String},
			"before":            &graphql.ArgumentConfig{Type: graphql.String},
			"first":             &graphql.ArgumentConfig{Type: graphql.Int},
			"hasIssuesEnabled":  &graphql.ArgumentConfig{Type: graphql.Boolean},
			"isLocked":          &graphql.ArgumentConfig{Type: graphql.Boolean},
			"last":              &graphql.ArgumentConfig{Type: graphql.Int},
			"orderBy":           &graphql.ArgumentConfig{Type: s.gqlRepositoryOrderInput()},
			"ownerAffiliations": &graphql.ArgumentConfig{Type: graphql.NewList(affiliation), DefaultValue: []interface{}{"OWNER", "COLLABORATOR"}},
			"privacy":           &graphql.ArgumentConfig{Type: s.sharedEnum("RepositoryPrivacy", "PUBLIC", "PRIVATE")},
			"sponsorableOnly":   &graphql.ArgumentConfig{Type: graphql.Boolean, DefaultValue: false},
			"visibility":        &graphql.ArgumentConfig{Type: s.sharedEnum("RepositoryVisibility", "INTERNAL", "PRIVATE", "PUBLIC")},
		},
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			name := topicSourceName(p.Source)
			items := make([]gqlConnItem, 0)
			for _, repo := range s.readableRepos(p) {
				if !containsFold(repo.Topics, name) {
					continue
				}
				repo := repo
				items = append(items, gqlConnItem{
					identity: repo.NodeID,
					render:   func() map[string]interface{} { return repoToGraphQL(s.store, repo) },
				})
			}
			return paginateGQLItems(items, p.Args), nil
		},
	})
}

// topicSourceName reads the topic name off a Topic source map.
func topicSourceName(source interface{}) string {
	m, _ := source.(map[string]interface{})
	name, _ := m["name"].(string)
	return name
}

// maxRepositoryTopics is github.com's cap on the topics one repository may
// carry; the REST topics route enforces the same number.
const maxRepositoryTopics = 20

// addRepositoryMetadataMutations registers the label, topic, archive and
// settings mutations.
func (s *Resolver) addRepositoryMetadataMutations(mutationType *graphql.Object) {
	repositoryType := s.graphqlTypes.repository
	labelType := s.gqlLabelType()
	topicType := s.gqlTopicType()
	uri := s.graphQLStringScalar("URI")

	// --- labels ------------------------------------------------------------

	s.registerMutation(mutationType, "createLabel", &graphql.Field{
		Type: s.mutationPayload("CreateLabelPayload", graphql.Fields{
			"label": gqlField(labelType),
		}),
		Args: graphql.FieldConfigArgument{"input": &graphql.ArgumentConfig{
			Type: graphql.NewNonNull(s.mutationInput("CreateLabelInput", graphql.InputObjectConfigFieldMap{
				"repositoryId": gqlNonNullID(),
				"name":         gqlNonNullString(),
				"color":        gqlNonNullString(),
				"description":  gqlString(),
			})),
		}},
		Resolve: s.resolveCreateLabel,
	})

	s.registerMutation(mutationType, "updateLabel", &graphql.Field{
		Type: s.mutationPayload("UpdateLabelPayload", graphql.Fields{
			"label": gqlField(labelType),
		}),
		Args: graphql.FieldConfigArgument{"input": &graphql.ArgumentConfig{
			Type: graphql.NewNonNull(s.mutationInput("UpdateLabelInput", graphql.InputObjectConfigFieldMap{
				"id":          gqlNonNullID(),
				"name":        gqlString(),
				"color":       gqlString(),
				"description": gqlString(),
			})),
		}},
		Resolve: s.resolveUpdateLabel,
	})

	s.registerMutation(mutationType, "deleteLabel", &graphql.Field{
		Type: s.mutationPayload("DeleteLabelPayload", graphql.Fields{}),
		Args: graphql.FieldConfigArgument{"input": &graphql.ArgumentConfig{
			Type: graphql.NewNonNull(s.mutationInput("DeleteLabelInput", graphql.InputObjectConfigFieldMap{
				"id": gqlNonNullID(),
			})),
		}},
		Resolve: s.resolveDeleteLabel,
	})

	// --- topics ------------------------------------------------------------

	s.registerMutation(mutationType, "updateTopics", &graphql.Field{
		Type: s.mutationPayload("UpdateTopicsPayload", graphql.Fields{
			"invalidTopicNames": gqlFieldListOf(graphql.String),
			"repository":        gqlField(repositoryType),
		}),
		Args: graphql.FieldConfigArgument{"input": &graphql.ArgumentConfig{
			Type: graphql.NewNonNull(s.mutationInput("UpdateTopicsInput", graphql.InputObjectConfigFieldMap{
				"repositoryId": gqlNonNullID(),
				"topicNames":   gqlNonNullListOf(graphql.String),
			})),
		}},
		Resolve: s.resolveUpdateTopics,
	})

	s.registerMutation(mutationType, "acceptTopicSuggestion", &graphql.Field{
		Type: s.mutationPayload("AcceptTopicSuggestionPayload", graphql.Fields{
			"topic": gqlField(topicType),
		}),
		Args: graphql.FieldConfigArgument{"input": &graphql.ArgumentConfig{
			Type: graphql.NewNonNull(s.mutationInput("AcceptTopicSuggestionInput", graphql.InputObjectConfigFieldMap{
				"repositoryId": gqlID(),
				"name":         gqlString(),
			})),
		}},
		Resolve: s.resolveAcceptTopicSuggestion,
	})

	s.registerMutation(mutationType, "declineTopicSuggestion", &graphql.Field{
		Type: s.mutationPayload("DeclineTopicSuggestionPayload", graphql.Fields{
			"topic": gqlField(topicType),
		}),
		Args: graphql.FieldConfigArgument{"input": &graphql.ArgumentConfig{
			Type: graphql.NewNonNull(s.mutationInput("DeclineTopicSuggestionInput", graphql.InputObjectConfigFieldMap{
				"repositoryId": gqlID(),
				"name":         gqlString(),
				"reason": gqlInputOf(s.sharedEnum("TopicSuggestionDeclineReason",
					"NOT_RELEVANT", "PERSONAL_PREFERENCE", "TOO_GENERAL", "TOO_SPECIFIC")),
			})),
		}},
		Resolve: s.resolveDeclineTopicSuggestion,
	})

	// --- lifecycle ---------------------------------------------------------

	s.registerMutation(mutationType, "archiveRepository", &graphql.Field{
		Type: s.mutationPayload("ArchiveRepositoryPayload", graphql.Fields{
			"repository": gqlField(repositoryType),
		}),
		Args: graphql.FieldConfigArgument{"input": &graphql.ArgumentConfig{
			Type: graphql.NewNonNull(s.mutationInput("ArchiveRepositoryInput", graphql.InputObjectConfigFieldMap{
				"repositoryId": gqlNonNullID(),
			})),
		}},
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			return s.resolveRepositoryArchival(p, true)
		},
	})

	s.registerMutation(mutationType, "unarchiveRepository", &graphql.Field{
		Type: s.mutationPayload("UnarchiveRepositoryPayload", graphql.Fields{
			"repository": gqlField(repositoryType),
		}),
		Args: graphql.FieldConfigArgument{"input": &graphql.ArgumentConfig{
			Type: graphql.NewNonNull(s.mutationInput("UnarchiveRepositoryInput", graphql.InputObjectConfigFieldMap{
				"repositoryId": gqlNonNullID(),
			})),
		}},
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			return s.resolveRepositoryArchival(p, false)
		},
	})

	// --- settings ----------------------------------------------------------

	s.registerMutation(mutationType, "updateRepository", &graphql.Field{
		Type: s.mutationPayload("UpdateRepositoryPayload", graphql.Fields{
			"repository": gqlField(repositoryType),
		}),
		Args: graphql.FieldConfigArgument{"input": &graphql.ArgumentConfig{
			Type: graphql.NewNonNull(s.mutationInput("UpdateRepositoryInput", graphql.InputObjectConfigFieldMap{
				"repositoryId":              gqlNonNullID(),
				"name":                      gqlString(),
				"description":               gqlString(),
				"homepageUrl":               gqlInputOf(uri),
				"hasIssuesEnabled":          gqlBool(),
				"hasWikiEnabled":            gqlBool(),
				"hasProjectsEnabled":        gqlBool(),
				"hasDiscussionsEnabled":     gqlBool(),
				"hasPullRequestsEnabled":    gqlBool(),
				"hasSponsorshipsEnabled":    gqlBool(),
				"template":                  gqlBool(),
				"issueCreationPolicy":       gqlInputOf(s.sharedEnum("IssueCreationPolicy", "ALL", "COLLABORATORS_ONLY")),
				"pullRequestCreationPolicy": gqlInputOf(s.sharedEnum("PullRequestCreationPolicy", "ALL", "COLLABORATORS_ONLY")),
			})),
		}},
		Resolve: s.resolveUpdateRepository,
	})

	s.registerMutation(mutationType, "updateRepositoryWebCommitSignoffSetting", &graphql.Field{
		Type: s.mutationPayload("UpdateRepositoryWebCommitSignoffSettingPayload", graphql.Fields{
			"message":    gqlField(graphql.String),
			"repository": gqlField(repositoryType),
		}),
		Args: graphql.FieldConfigArgument{"input": &graphql.ArgumentConfig{
			Type: graphql.NewNonNull(s.mutationInput("UpdateRepositoryWebCommitSignoffSettingInput", graphql.InputObjectConfigFieldMap{
				"repositoryId":             gqlNonNullID(),
				"webCommitSignoffRequired": gqlNonNullBool(),
			})),
		}},
		Resolve: s.resolveUpdateRepositoryWebCommitSignoff,
	})
}

// gqlTopicType returns the shared Topic object (memoized). Repository.topics
// names it through RepositoryTopic and the two topic-suggestion payloads
// return it directly, so it has to be one object rather than one per consumer.
func (s *Resolver) gqlTopicType() *graphql.Object {
	return s.mutationObject("Topic", graphql.Fields{
		"name": gqlNonNull(graphql.String),
	})
}

func topicToGQL(name string) map[string]interface{} {
	return map[string]interface{}{"name": name}
}

// --- label resolvers --------------------------------------------------------

func (s *Resolver) resolveCreateLabel(p graphql.ResolveParams) (interface{}, error) {
	input, _ := p.Args["input"].(map[string]interface{})
	repo, err := s.mutationRepoFromInput(input, "repositoryId")
	if err != nil {
		return nil, err
	}
	name, _ := gqlInputString(input, "name")
	if strings.TrimSpace(name) == "" {
		return nil, fmt.Errorf("Name can't be blank")
	}
	color, _ := gqlInputString(input, "color")
	description, _ := gqlInputString(input, "description")

	label := s.store.CreateLabel(repo.ID, name, description, strings.TrimPrefix(color, "#"))
	if label == nil {
		return nil, fmt.Errorf("Name has already been taken")
	}
	s.emitLabelEvent(repo, label, p, "created")
	return map[string]interface{}{"label": optionalRendered(label, labelToGQL)}, nil
}

func (s *Resolver) resolveUpdateLabel(p graphql.ResolveParams) (interface{}, error) {
	input, _ := p.Args["input"].(map[string]interface{})
	nodeID, _ := gqlInputString(input, "id")
	// The node finder hands back the live row, so the id is all that is read
	// off it; the label the payload renders is re-read after the write.
	found := store.FindLabelByNodeID(s.store, nodeID)
	if found == nil {
		return nil, gqlMissingNode("Label", nodeID)
	}
	labelID := found.ID
	repo := s.store.GetRepoByID(found.RepoID)
	if repo == nil {
		return nil, gqlMissingNodeType("Repository")
	}
	name, renaming := gqlInputString(input, "name")
	if renaming && strings.TrimSpace(name) == "" {
		return nil, fmt.Errorf("Name can't be blank")
	}
	if renaming {
		if clash := s.store.GetLabelByName(repo.ID, name); clash != nil && clash.ID != labelID {
			return nil, fmt.Errorf("Name has already been taken")
		}
	}
	color, recoloring := gqlInputString(input, "color")
	description, redescribing := gqlInputString(input, "description")

	s.store.UpdateLabel(labelID, func(l *store.IssueLabel) {
		if renaming {
			l.Name = name
		}
		if recoloring {
			l.Color = strings.TrimPrefix(color, "#")
		}
		if redescribing {
			l.Description = description
		}
	})
	updated := s.store.GetLabel(labelID)
	if updated == nil {
		return nil, gqlMissingNodeType("Label")
	}
	s.emitLabelEvent(repo, updated, p, "edited")
	return map[string]interface{}{"label": optionalRendered(updated, labelToGQL)}, nil
}

func (s *Resolver) resolveDeleteLabel(p graphql.ResolveParams) (interface{}, error) {
	input, _ := p.Args["input"].(map[string]interface{})
	nodeID, _ := gqlInputString(input, "id")
	found := store.FindLabelByNodeID(s.store, nodeID)
	if found == nil {
		return nil, gqlMissingNode("Label", nodeID)
	}
	repo := s.store.GetRepoByID(found.RepoID)
	if repo == nil {
		return nil, gqlMissingNodeType("Repository")
	}
	// The webhook body carries the label as it was, so it is snapshotted
	// before the row is destroyed.
	deleted := s.store.GetLabel(found.ID)
	if deleted == nil {
		return nil, gqlMissingNodeType("Label")
	}
	if !s.store.DeleteLabel(deleted.ID) {
		return nil, gqlMissingNodeType("Label")
	}
	s.emitLabelEvent(repo, deleted, p, "deleted")
	return map[string]interface{}{}, nil
}

// emitLabelEvent delivers the `label` webhook the REST label routes deliver,
// through the same repo-keyed fan-out.
func (s *Resolver) emitLabelEvent(repo *store.Repo, label *store.IssueLabel, p graphql.ResolveParams, action string) {
	sender := s.ghUserFromContext(p.Context)
	s.emitWebhookEvent(repo.FullName, "label", action, map[string]interface{}{
		"action": action,
		"label": map[string]interface{}{
			"id":          label.ID,
			"node_id":     label.NodeID,
			"url":         externalURL("/api/v3/repos/" + repo.FullName + "/labels/" + label.Name),
			"name":        label.Name,
			"description": label.Description,
			"color":       label.Color,
			"default":     label.Default,
		},
		"repository": s.repoPayload(repo),
		"sender":     s.senderPayload(sender),
	})
}

// --- topic resolvers --------------------------------------------------------

func (s *Resolver) resolveUpdateTopics(p graphql.ResolveParams) (interface{}, error) {
	input, _ := p.Args["input"].(map[string]interface{})
	repo, err := s.mutationRepoFromInput(input, "repositoryId")
	if err != nil {
		return nil, err
	}
	requested, _ := gqlInputStrings(input, "topicNames")

	valid := make([]string, 0, len(requested))
	invalid := []string{}
	seen := map[string]bool{}
	for _, name := range requested {
		normalized := strings.ToLower(strings.TrimSpace(name))
		if !validRepositoryTopic(normalized) {
			invalid = append(invalid, name)
			continue
		}
		if seen[normalized] {
			continue
		}
		seen[normalized] = true
		valid = append(valid, normalized)
	}
	if len(valid) > maxRepositoryTopics {
		return nil, fmt.Errorf("a repository may carry at most %d topics", maxRepositoryTopics)
	}

	updated, err := s.updateRepoRow(repo, func(r *store.Repo) {
		r.Topics = valid
	})
	if err != nil {
		return nil, err
	}
	return map[string]interface{}{
		"invalidTopicNames": invalid,
		"repository":        optionalObject(repoToGraphQL(s.store, updated)),
	}, nil
}

// validRepositoryTopic is the same rule PUT /repos/{owner}/{repo}/topics
// enforces: a non-empty name of at most fifty characters carrying none of the
// separators a topic may not contain.
func validRepositoryTopic(name string) bool {
	return name != "" && len(name) <= 50 && !strings.ContainsAny(name, " /\\:")
}

func (s *Resolver) resolveAcceptTopicSuggestion(p graphql.ResolveParams) (interface{}, error) {
	input, _ := p.Args["input"].(map[string]interface{})
	repo, err := s.mutationRepoFromInput(input, "repositoryId")
	if err != nil {
		return nil, err
	}
	name, _ := gqlInputString(input, "name")
	name = strings.ToLower(strings.TrimSpace(name))
	if !validRepositoryTopic(name) {
		return nil, fmt.Errorf("topic name %q is invalid", name)
	}

	updated, err := s.updateRepoRow(repo, func(r *store.Repo) {
		r.DeclinedTopics = withoutString(r.DeclinedTopics, name)
		if containsFold(r.Topics, name) || len(r.Topics) >= maxRepositoryTopics {
			return
		}
		r.Topics = append(append([]string(nil), r.Topics...), name)
	})
	if err != nil {
		return nil, err
	}
	if !containsFold(updated.Topics, name) {
		return nil, fmt.Errorf("a repository may carry at most %d topics", maxRepositoryTopics)
	}
	return map[string]interface{}{"topic": topicToGQL(name)}, nil
}

func (s *Resolver) resolveDeclineTopicSuggestion(p graphql.ResolveParams) (interface{}, error) {
	input, _ := p.Args["input"].(map[string]interface{})
	repo, err := s.mutationRepoFromInput(input, "repositoryId")
	if err != nil {
		return nil, err
	}
	name, _ := gqlInputString(input, "name")
	name = strings.ToLower(strings.TrimSpace(name))
	if !validRepositoryTopic(name) {
		return nil, fmt.Errorf("topic name %q is invalid", name)
	}
	// Declining a topic both records the decision — so the topic is never
	// suggested for this repository again — and takes the topic off the
	// repository if it was already carrying it.
	if _, err := s.updateRepoRow(repo, func(r *store.Repo) {
		r.Topics = withoutString(r.Topics, name)
		if !containsFold(r.DeclinedTopics, name) {
			r.DeclinedTopics = append(append([]string(nil), r.DeclinedTopics...), name)
		}
	}); err != nil {
		return nil, err
	}
	return map[string]interface{}{"topic": topicToGQL(name)}, nil
}

func containsFold(values []string, want string) bool {
	for _, value := range values {
		if strings.EqualFold(value, want) {
			return true
		}
	}
	return false
}

func withoutString(values []string, drop string) []string {
	out := make([]string, 0, len(values))
	for _, value := range values {
		if strings.EqualFold(value, drop) {
			continue
		}
		out = append(out, value)
	}
	return out
}

// --- lifecycle and settings resolvers ---------------------------------------

func (s *Resolver) resolveRepositoryArchival(p graphql.ResolveParams, archived bool) (interface{}, error) {
	input, _ := p.Args["input"].(map[string]interface{})
	repo, err := s.mutationRepoFromInput(input, "repositoryId")
	if err != nil {
		return nil, err
	}
	now := s.store.CurrentTime()
	updated, err := s.updateRepoRow(repo, func(r *store.Repo) {
		switch {
		case archived && (!r.Archived || r.ArchivedAt == nil):
			r.ArchivedAt = &now
		case !archived:
			r.ArchivedAt = nil
		}
		r.Archived = archived
	})
	if err != nil {
		return nil, err
	}
	return map[string]interface{}{"repository": optionalObject(repoToGraphQL(s.store, updated))}, nil
}

func (s *Resolver) resolveUpdateRepository(p graphql.ResolveParams) (interface{}, error) {
	input, _ := p.Args["input"].(map[string]interface{})
	repo, err := s.mutationRepoFromInput(input, "repositoryId")
	if err != nil {
		return nil, err
	}
	// A rename moves every record that embeds the repository's full name, so
	// it goes through the server's seam and happens before the field writes:
	// UpdateRepo is keyed on owner/name, and writing under the old name after
	// a successful rename would write nothing.
	if newName, renaming := gqlInputString(input, "name"); renaming && newName != "" && newName != repo.Name {
		if err := s.repos.RenameRepository(repo, newName); err != nil {
			return nil, err
		}
		renamed := s.store.GetRepoByID(repo.ID)
		if renamed == nil {
			return nil, gqlMissingNodeType("Repository")
		}
		repo = renamed
	}

	updated, err := s.updateRepoRow(repo, func(r *store.Repo) {
		if value, ok := gqlInputString(input, "description"); ok {
			r.Description = value
		}
		if value, ok := gqlInputString(input, "homepageUrl"); ok {
			r.Homepage = value
		}
		if value, ok := gqlInputBool(input, "hasIssuesEnabled"); ok {
			r.HasIssues = value
		}
		if value, ok := gqlInputBool(input, "hasWikiEnabled"); ok {
			r.HasWiki = value
		}
		if value, ok := gqlInputBool(input, "hasProjectsEnabled"); ok {
			r.HasProjects = value
		}
		if value, ok := gqlInputBool(input, "hasDiscussionsEnabled"); ok {
			r.HasDiscussions = store.BoolPointer(value)
		}
		if value, ok := gqlInputBool(input, "hasPullRequestsEnabled"); ok {
			r.HasPullRequests = value
		}
		if value, ok := gqlInputBool(input, "hasSponsorshipsEnabled"); ok {
			r.HasSponsorships = store.BoolPointer(value)
		}
		if value, ok := gqlInputBool(input, "template"); ok {
			r.IsTemplate = value
		}
		if value, ok := gqlInputString(input, "issueCreationPolicy"); ok {
			r.IssueCreationPolicy = strings.ToLower(value)
		}
		if value, ok := gqlInputString(input, "pullRequestCreationPolicy"); ok {
			r.PullRequestCreationPolicy = strings.ToLower(value)
		}
	})
	if err != nil {
		return nil, err
	}
	return map[string]interface{}{"repository": optionalObject(repoToGraphQL(s.store, updated))}, nil
}

func (s *Resolver) resolveUpdateRepositoryWebCommitSignoff(p graphql.ResolveParams) (interface{}, error) {
	input, _ := p.Args["input"].(map[string]interface{})
	repo, err := s.mutationRepoFromInput(input, "repositoryId")
	if err != nil {
		return nil, err
	}
	required, _ := gqlInputBool(input, "webCommitSignoffRequired")
	updated, err := s.updateRepoRow(repo, func(r *store.Repo) {
		r.WebCommitSignoffRequired = required
	})
	if err != nil {
		return nil, err
	}
	message := "web commit signoff is not required"
	if required {
		message = "web commit signoff is required"
	}
	return map[string]interface{}{
		"message":    message,
		"repository": optionalObject(repoToGraphQL(s.store, updated)),
	}, nil
}

// --- shared helpers ---------------------------------------------------------

// mutationRepoFromInput resolves the repository a mutation input names. The
// policy row already refused a caller who may not reach it, so a miss here is
// a repository that stopped existing between the two lookups.
func (s *Resolver) mutationRepoFromInput(input map[string]interface{}, key string) (*store.Repo, error) {
	nodeID, _ := gqlInputString(input, key)
	repo := store.FindRepoByNodeID(s.store, nodeID)
	if repo == nil {
		return nil, gqlMissingNode("Repository", nodeID)
	}
	return repo, nil
}

// updateRepoRow applies a field write to a repository through the same
// copy-on-write primitive the REST repository routes use, and answers the
// detached row that resulted. The timestamp bump is part of the write so a
// GraphQL edit ages the repository exactly as a REST edit does.
func (s *Resolver) updateRepoRow(repo *store.Repo, apply func(*store.Repo)) (*store.Repo, error) {
	owner, name, ok := store.SplitRepoFullName(repo.FullName)
	if !ok {
		return nil, gqlMissingNodeType("Repository")
	}
	now := s.store.CurrentTime()
	if !s.store.UpdateRepo(owner, name, func(r *store.Repo) {
		apply(r)
		r.UpdatedAt = now
	}) {
		return nil, gqlMissingNodeType("Repository")
	}
	updated := s.store.GetRepoByID(repo.ID)
	if updated == nil {
		return nil, gqlMissingNodeType("Repository")
	}
	return updated, nil
}
