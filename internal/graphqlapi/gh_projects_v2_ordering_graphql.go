package graphqlapi

import (
	"sort"
	"strings"

	"github.com/e6qu/bleephub/internal/store"
	"github.com/graphql-go/graphql"
)

// Projects v2 — the ordering and filtering arguments its connections take.
//
// Every ProjectV2 connection on GitHub accepts an `orderBy`, and the items
// connection additionally accepts `archivedStates` and a `query` search. A
// connection that declares none of them is not merely less capable: a client
// that passes one gets a validation error for an unknown argument and the
// whole request fails, so the arguments have to exist before they can be
// honoured.

// projectV2OrderInputFor builds one of GitHub's ProjectV2 ordering inputs: a
// direction plus an enum of the orderable fields. Each is memoized under its
// own name, because a schema may declare a name once.
func (s *Resolver) projectV2OrderInputFor(name, fieldEnumName string, fields ...string) *graphql.InputObject {
	if s.graphqlTypes.projectV2OrderInputs == nil {
		s.graphqlTypes.projectV2OrderInputs = map[string]*graphql.InputObject{}
	}
	if memo := s.graphqlTypes.projectV2OrderInputs[name]; memo != nil {
		return memo
	}
	input := graphql.NewInputObject(graphql.InputObjectConfig{
		Name: name,
		Fields: graphql.InputObjectConfigFieldMap{
			"direction": &graphql.InputObjectFieldConfig{
				Type: graphql.NewNonNull(s.graphQLEnum("OrderDirection", "ASC", "DESC")),
			},
			"field": &graphql.InputObjectFieldConfig{
				Type: graphql.NewNonNull(s.graphQLEnum(fieldEnumName, fields...)),
			},
		},
	})
	s.graphqlTypes.projectV2OrderInputs[name] = input
	return input
}

func (s *Resolver) projectV2ItemOrderInput() *graphql.InputObject {
	return s.projectV2OrderInputFor("ProjectV2ItemOrder", "ProjectV2ItemOrderField", "POSITION")
}

func (s *Resolver) projectV2FieldOrderInput() *graphql.InputObject {
	return s.projectV2OrderInputFor("ProjectV2FieldOrder", "ProjectV2FieldOrderField", "CREATED_AT", "NAME", "POSITION")
}

func (s *Resolver) projectV2ViewOrderInput() *graphql.InputObject {
	return s.projectV2OrderInputFor("ProjectV2ViewOrder", "ProjectV2ViewOrderField", "CREATED_AT", "NAME", "POSITION")
}

func (s *Resolver) projectV2WorkflowOrderInput() *graphql.InputObject {
	return s.projectV2OrderInputFor("ProjectV2WorkflowOrder", "ProjectV2WorkflowsOrderField", "CREATED_AT", "NAME", "NUMBER", "UPDATED_AT")
}

func (s *Resolver) projectV2StatusOrderInput() *graphql.InputObject {
	return s.projectV2OrderInputFor("ProjectV2StatusOrder", "ProjectV2StatusUpdateOrderField", "CREATED_AT")
}

func (s *Resolver) projectV2ItemFieldValueOrderInput() *graphql.InputObject {
	return s.projectV2OrderInputFor("ProjectV2ItemFieldValueOrder", "ProjectV2ItemFieldValueOrderField", "POSITION")
}

// projectV2ItemConnectionArgs is the argument set ProjectV2.items takes.
func (s *Resolver) projectV2ItemConnectionArgs() graphql.FieldConfigArgument {
	args := relayConnectionArgs()
	args["orderBy"] = &graphql.ArgumentConfig{Type: s.projectV2ItemOrderInput()}
	args["query"] = &graphql.ArgumentConfig{Type: graphql.String}
	args["archivedStates"] = &graphql.ArgumentConfig{
		Type: graphql.NewList(graphql.NewNonNull(
			s.graphQLEnum("ProjectV2ItemArchivedState", "ARCHIVED", "NOT_ARCHIVED"),
		)),
	}
	return args
}

// orderedConnectionArgs is the Relay window plus one orderBy.
func orderedConnectionArgs(order *graphql.InputObject) graphql.FieldConfigArgument {
	args := relayConnectionArgs()
	args["orderBy"] = &graphql.ArgumentConfig{Type: order}
	return args
}

// projectV2ApplyItemFilters narrows a project's items the way GitHub's items
// connection does before paginating: by archived state, then by the free-text
// `query`.
//
// The archived default is the schema's — [NOT_ARCHIVED] — so an archived item
// stays out of a listing that did not ask for it. Returning archived items by
// default would make `gh project item-list` show work somebody deliberately
// filed away.
func projectV2ApplyItemFilters(st *store.Store, items []*store.ProjectV2Item, args map[string]interface{}) []*store.ProjectV2Item {
	states := map[string]bool{}
	if raw, ok := args["archivedStates"].([]interface{}); ok && len(raw) > 0 {
		for _, entry := range raw {
			if state, ok := entry.(string); ok {
				states[state] = true
			}
		}
	} else {
		states["NOT_ARCHIVED"] = true
	}

	needle := strings.ToLower(strings.TrimSpace(argString(args, "query")))
	kept := make([]*store.ProjectV2Item, 0, len(items))
	for _, item := range items {
		state := "NOT_ARCHIVED"
		if item.ArchivedAt != nil {
			state = "ARCHIVED"
		}
		if !states[state] {
			continue
		}
		if needle != "" && !strings.Contains(strings.ToLower(projectV2ItemSearchText(st, item)), needle) {
			continue
		}
		kept = append(kept, item)
	}
	return kept
}

// projectV2ItemSearchText is what the items connection's `query` matches on:
// the title the item shows, which is the draft's title or the content's.
func projectV2ItemSearchText(st *store.Store, item *store.ProjectV2Item) string {
	switch item.ContentType {
	case "Issue":
		if issue := st.GetIssue(item.ContentID); issue != nil {
			return issue.Title
		}
	case "PullRequest":
		if pr := st.GetPullRequest(item.ContentID); pr != nil {
			return pr.Title
		}
	default:
		return item.DraftTitle
	}
	return ""
}

// argString reads a string argument, tolerating its absence.
func argString(args map[string]interface{}, key string) string {
	value, _ := args[key].(string)
	return value
}

// projectV2OrderDirection reports whether an orderBy argument asked for
// descending order, and which field it named.
func projectV2OrderDirection(args map[string]interface{}) (field string, descending bool) {
	orderBy, _ := args["orderBy"].(map[string]interface{})
	if orderBy == nil {
		return "", false
	}
	field, _ = orderBy["field"].(string)
	direction, _ := orderBy["direction"].(string)
	return field, direction == "DESC"
}

// projectV2SortNodes applies an orderBy over already-rendered connection
// nodes, using the source-map key each orderable field reads. Nodes carry the
// values the order names, so ordering does not need a second store pass.
func projectV2SortNodes(nodes []map[string]interface{}, args map[string]interface{}, keys map[string]string) {
	field, descending := projectV2OrderDirection(args)
	key, ok := keys[field]
	if !ok {
		// POSITION is the stored order, which the caller already produced.
		if descending {
			for i, j := 0, len(nodes)-1; i < j; i, j = i+1, j-1 {
				nodes[i], nodes[j] = nodes[j], nodes[i]
			}
		}
		return
	}
	less := func(a, b map[string]interface{}) bool {
		return projectV2NodeSortKey(a, key) < projectV2NodeSortKey(b, key)
	}
	sort.SliceStable(nodes, func(i, j int) bool {
		if descending {
			return less(nodes[j], nodes[i])
		}
		return less(nodes[i], nodes[j])
	})
}

// projectV2NodeSortKey renders one node's ordering key as a comparable string.
// Numbers are zero-padded so a numeric order does not become lexicographic.
func projectV2NodeSortKey(node map[string]interface{}, key string) string {
	switch value := node[key].(type) {
	case string:
		return strings.ToLower(value)
	case int:
		return padSortableInt(value)
	default:
		return ""
	}
}

func padSortableInt(value int) string {
	digits := []byte("0000000000")
	negative := value < 0
	if negative {
		value = -value
	}
	for i := len(digits) - 1; i >= 0 && value > 0; i-- {
		digits[i] = byte('0' + value%10)
		value /= 10
	}
	if negative {
		return "-" + string(digits)
	}
	return "+" + string(digits)
}
