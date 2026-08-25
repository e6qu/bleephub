package graphqlapi

import (
	"fmt"

	"github.com/e6qu/bleephub/internal/store"
	"github.com/graphql-go/graphql"
)

// addProjectV2FieldFamilyResidualFields fills in the members GitHub's
// ProjectV2FieldCommon interface and its four implementers (ProjectV2Field,
// ProjectV2IterationField, ProjectV2MultiSelectField,
// ProjectV2SingleSelectField), the single-select option and iteration objects,
// ProjectV2View and DraftIssue declare that the core Projects v2 read surface
// did not install. It runs at the end of enrichProjectV2Types, once every type
// it hangs a field off is assembled, and mirrors the addAccountActionsFields
// pattern of hanging residual members off already-built types.
//
// databaseId is decoded from the field's own node id; project follows the
// stored project id to the owning ProjectV2. isIssueField / issueField model
// GitHub's organization issue-type custom fields: bleephub records no
// association between a project field and an org issue field (a project field
// is a standalone copy), so isIssueField is a truthful constant false and
// issueField a truthful null rather than a stub. The *HTML members render the
// plain-text name / title / description through the same markdown renderer the
// rest of the GraphQL surface uses.
func (s *Resolver) addProjectV2FieldFamilyResidualFields() {
	// Guarantee the field-configuration and view types exist before hanging
	// members off them (both builders are memoized, so this is idempotent).
	s.projectV2FieldConnectionType()
	s.projectV2ViewConnectionType()

	s.addProjectV2FieldCommonFields()
	s.addProjectV2SingleSelectOptionHTML()
	s.addProjectV2IterationTitleHTML()
	s.addProjectV2ViewFullDatabaseID()
	s.addDraftIssueResidualFields()
}

// addProjectV2FieldCommonFields installs databaseId / isIssueField / project on
// the ProjectV2FieldCommon interface and every implementer, plus issueField on
// the three implementers GitHub declares it on (all but ProjectV2IterationField).
func (s *Resolver) addProjectV2FieldCommonFields() {
	projectType := s.graphqlTypes.projectV2Type
	issueFieldsUnion := s.graphqlTypes.issueFieldsUnion

	databaseID := func() *graphql.Field {
		return &graphql.Field{
			Type:    graphql.Int,
			Resolve: projectV2FieldDatabaseID,
		}
	}
	isIssueField := func() *graphql.Field {
		return &graphql.Field{
			Type:    graphql.NewNonNull(graphql.Boolean),
			Resolve: func(graphql.ResolveParams) (interface{}, error) { return false, nil },
		}
	}
	project := func() *graphql.Field {
		return &graphql.Field{
			Type:    graphql.NewNonNull(projectType),
			Resolve: s.projectV2FieldOwningProject,
		}
	}
	issueField := func() *graphql.Field {
		return &graphql.Field{
			Type:    issueFieldsUnion,
			Resolve: func(graphql.ResolveParams) (interface{}, error) { return nil, nil },
		}
	}

	// The interface only carries the field type (its resolvers are supplied by
	// the concrete objects graphql-go dispatches to).
	if iface := projectV2FieldCommonInterface(s.graphqlTypes.projectV2FieldTypeMemo); iface != nil {
		iface.AddFieldConfig("databaseId", &graphql.Field{Type: graphql.Int})
		iface.AddFieldConfig("isIssueField", &graphql.Field{Type: graphql.NewNonNull(graphql.Boolean)})
		iface.AddFieldConfig("project", &graphql.Field{Type: graphql.NewNonNull(projectType)})
	}

	for _, obj := range []*graphql.Object{
		s.graphqlTypes.projectV2FieldTypeMemo,
		s.graphqlTypes.projectV2MultiSelectFieldMemo,
		s.graphqlTypes.projectV2SingleSelectFieldMemo,
		s.graphqlTypes.projectV2IterationFieldMemo,
	} {
		if obj == nil {
			continue
		}
		obj.AddFieldConfig("databaseId", databaseID())
		obj.AddFieldConfig("isIssueField", isIssueField())
		obj.AddFieldConfig("project", project())
	}

	// issueField is declared on every implementer except ProjectV2IterationField.
	if issueFieldsUnion != nil {
		for _, obj := range []*graphql.Object{
			s.graphqlTypes.projectV2FieldTypeMemo,
			s.graphqlTypes.projectV2MultiSelectFieldMemo,
			s.graphqlTypes.projectV2SingleSelectFieldMemo,
		} {
			if obj != nil {
				obj.AddFieldConfig("issueField", issueField())
			}
		}
	}
}

// projectV2FieldDatabaseID reads the field's primary key straight out of its
// node id, so it stays correct even for the snapshot a delete payload renders.
func projectV2FieldDatabaseID(p graphql.ResolveParams) (interface{}, error) {
	nodeID, err := projectV2FieldSourceNodeID(p.Source)
	if err != nil {
		return nil, err
	}
	if id, ok := store.DecodeNodeDBID(nodeID, "PVTF_kgDO"); ok {
		return id, nil
	}
	return nil, nil
}

// projectV2FieldOwningProject follows a field to the ProjectV2 that contains it.
func (s *Resolver) projectV2FieldOwningProject(p graphql.ResolveParams) (interface{}, error) {
	nodeID, err := projectV2FieldSourceNodeID(p.Source)
	if err != nil {
		return nil, err
	}
	field := s.store.ProjectsV2.LookupFieldByNodeID(nodeID)
	if field == nil {
		return nil, &ghNotFoundError{message: "Could not resolve to a project."}
	}
	proj := s.store.ProjectsV2.GetProject(field.ProjectID)
	if proj == nil {
		return nil, &ghNotFoundError{message: "Could not resolve to a project."}
	}
	return projectV2ToGQLFull(s.store, proj), nil
}

// addProjectV2SingleSelectOptionHTML installs descriptionHTML / nameHTML on the
// ProjectV2SingleSelectFieldOption object, reached through the single-select
// field's options member (its map source carries "name"/"description").
func (s *Resolver) addProjectV2SingleSelectOptionHTML() {
	optionType := namedObjectFromField(s.graphqlTypes.projectV2SingleSelectFieldMemo, "options")
	if optionType == nil {
		return
	}
	optionType.AddFieldConfig("nameHTML", &graphql.Field{
		Type:    graphql.NewNonNull(graphql.String),
		Resolve: projectV2RenderedSourceHTML("name"),
	})
	optionType.AddFieldConfig("descriptionHTML", &graphql.Field{
		Type:    graphql.NewNonNull(graphql.String),
		Resolve: projectV2RenderedSourceHTML("description"),
	})
}

// addProjectV2IterationTitleHTML installs titleHTML on the
// ProjectV2IterationFieldIteration object, reached through the iteration
// field's configuration.iterations member (its map source carries "title").
func (s *Resolver) addProjectV2IterationTitleHTML() {
	configType := namedObjectFromField(s.graphqlTypes.projectV2IterationFieldMemo, "configuration")
	iterationType := namedObjectFromField(configType, "iterations")
	if iterationType == nil {
		return
	}
	iterationType.AddFieldConfig("titleHTML", &graphql.Field{
		Type:    graphql.NewNonNull(graphql.String),
		Resolve: projectV2RenderedSourceHTML("title"),
	})
}

// addProjectV2ViewFullDatabaseID installs fullDatabaseId on ProjectV2View, the
// 64-bit-safe form of the numeric id the view source already carries.
func (s *Resolver) addProjectV2ViewFullDatabaseID() {
	viewType := s.graphqlTypes.projectV2ViewTypeMemo
	if viewType == nil {
		return
	}
	viewType.AddFieldConfig("fullDatabaseId", &graphql.Field{
		Type:    s.graphQLStringScalar("BigInt"),
		Resolve: sourceKeyResolver("databaseId"),
	})
}

// addDraftIssueResidualFields installs assignees, projectV2Items and projectsV2
// on DraftIssue. A draft has no row of its own — it is the project item — so
// projectV2Items yields that one item and projectsV2 the one project that
// contains it. bleephub's draft-issue model records no assignees, so assignees
// is a truthful empty UserConnection.
func (s *Resolver) addDraftIssueResidualFields() {
	draftType := s.graphqlTypes.projectV2DraftIssueType
	if draftType == nil {
		return
	}

	if userConn := s.graphqlTypes.userConnection; userConn != nil {
		draftType.AddFieldConfig("assignees", &graphql.Field{
			Type: graphql.NewNonNull(userConn),
			Args: relayConnectionArgs(),
			Resolve: func(p graphql.ResolveParams) (interface{}, error) {
				return paginateGQLMaps(nil, p.Args), nil
			},
		})
	}

	draftType.AddFieldConfig("projectV2Items", &graphql.Field{
		Type: graphql.NewNonNull(s.projectV2ItemConnectionType()),
		Args: relayConnectionArgs(),
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			nodes := make([]map[string]interface{}, 0, 1)
			if item := s.draftIssueItem(p.Source); item != nil {
				nodes = append(nodes, projectV2ItemToGQL(item, s.store))
			}
			return paginateGQLMaps(nodes, p.Args), nil
		},
	})

	draftType.AddFieldConfig("projectsV2", &graphql.Field{
		Type: graphql.NewNonNull(s.gqlConnectionType("ProjectV2", s.graphqlTypes.projectV2Type)),
		Args: relayConnectionArgs(),
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			nodes := make([]map[string]interface{}, 0, 1)
			if item := s.draftIssueItem(p.Source); item != nil {
				if proj := s.store.ProjectsV2.GetProject(item.ProjectID); proj != nil {
					nodes = append(nodes, projectV2ToGQLFull(s.store, proj))
				}
			}
			return paginateGQLMaps(nodes, p.Args), nil
		},
	})
}

// draftIssueItem resolves the project item a DraftIssue source describes. The
// draft's node id is the item's node id (a draft has no row of its own).
func (s *Resolver) draftIssueItem(source interface{}) *store.ProjectV2Item {
	src, ok := source.(map[string]interface{})
	if !ok {
		return nil
	}
	nodeID, _ := src["nodeID"].(string)
	if nodeID == "" {
		return nil
	}
	return s.store.ProjectsV2.LookupItemByNodeID(nodeID)
}

// projectV2FieldSourceNodeID reads the node id every ProjectV2 field source map
// carries under "nodeID".
func projectV2FieldSourceNodeID(source interface{}) (string, error) {
	src, ok := source.(map[string]interface{})
	if !ok {
		return "", fmt.Errorf("resolve source: unexpected type %T", source)
	}
	nodeID, _ := src["nodeID"].(string)
	return nodeID, nil
}

// projectV2RenderedSourceHTML renders a plain-text source-map string to HTML.
func projectV2RenderedSourceHTML(key string) graphql.FieldResolveFn {
	return func(p graphql.ResolveParams) (interface{}, error) {
		src, ok := p.Source.(map[string]interface{})
		if !ok {
			return nil, fmt.Errorf("resolve source: unexpected type %T", p.Source)
		}
		text, _ := src[key].(string)
		return discussionBodyToHTML(text), nil
	}
}

// projectV2FieldCommonInterface returns the ProjectV2FieldCommon interface an
// object implements, so its residual fields can be installed on the interface
// declaration itself (which lives on a local var in the field-type builder).
func projectV2FieldCommonInterface(obj *graphql.Object) *graphql.Interface {
	if obj == nil {
		return nil
	}
	for _, iface := range obj.Interfaces() {
		if iface != nil && iface.Name() == "ProjectV2FieldCommon" {
			return iface
		}
	}
	return nil
}

// namedObjectFromField unwraps a field's (possibly non-null / list) output type
// to the named object underneath it.
func namedObjectFromField(obj *graphql.Object, field string) *graphql.Object {
	if obj == nil {
		return nil
	}
	def, ok := obj.Fields()[field]
	if !ok || def == nil {
		return nil
	}
	named, _ := graphql.GetNamed(def.Type).(*graphql.Object)
	return named
}
