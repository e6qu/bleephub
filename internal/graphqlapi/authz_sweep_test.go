package graphqlapi

import (
	"fmt"
	"strings"
	"testing"

	"github.com/graphql-go/graphql"
)

// TestGraphQLMutationSweepRejectsAnUnguardedMutation drives the schema-build
// sweep directly. The registrar cannot catch a mutation that never calls it, so
// the sweep is what makes coverage structural rather than a convention every
// new file has to remember.
func TestGraphQLMutationSweepRejectsAnUnguardedMutation(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		field string
		want  string
	}{
		{name: "no policy row", field: "smuggledMutation", want: "no row in graphqlMutationAuthz"},
		{name: "row but no registrar", field: "createRepository", want: "not registered through registerMutation"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mutationType := graphql.NewObject(graphql.ObjectConfig{
				Name:   "SweepProbe" + tc.field,
				Fields: graphql.Fields{},
			})
			mutationType.AddFieldConfig(tc.field, &graphql.Field{
				Type:    graphql.String,
				Resolve: func(graphql.ResolveParams) (interface{}, error) { return nil, nil },
			})
			defer func() {
				r := recover()
				if r == nil {
					t.Fatalf("the sweep admitted %s registered outside registerMutation", tc.field)
				}
				if msg := fmt.Sprint(r); !strings.Contains(msg, tc.field) || !strings.Contains(msg, tc.want) {
					t.Fatalf("sweep panic = %q, want it to name %s and %q", msg, tc.field, tc.want)
				}
			}()
			assertMutationsAuthorized(mutationType)
		})
	}
}
