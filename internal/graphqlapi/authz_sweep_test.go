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

// TestMutationAuthzTableMatchesSchema is the resolver-layer half of the
// policy-coverage gate (the server-side half asserts every repo/project-
// scoped mutation is exercised by a refusal case): every mutation the
// schema exposes has a row in graphqlMutationAuthz and vice versa, so a
// mutation added without deciding who may call it fails here rather than
// shipping open.
func TestMutationAuthzTableMatchesSchema(t *testing.T) {
	t.Parallel()
	r := NewResolver(Config{Store: newSeededTestStore()})
	schema := r.Schema()
	mutation := schema.MutationType()
	if mutation == nil {
		t.Fatalf("the schema exposes no mutation type")
	}
	fields := mutation.Fields()
	if len(fields) != len(graphqlMutationAuthz) {
		t.Fatalf("the schema exposes %d mutations but the policy table has %d rows", len(fields), len(graphqlMutationAuthz))
	}
	for name := range fields {
		if _, covered := graphqlMutationAuthz[name]; !covered {
			t.Errorf("mutation %s reaches the store with no row in graphqlMutationAuthz", name)
		}
	}
	for name := range graphqlMutationAuthz {
		if _, ok := fields[name]; !ok {
			t.Errorf("graphqlMutationAuthz has a row for %s, which the schema does not expose", name)
		}
	}
}

// TestMutationAuthzAccountScopedRowsArePinned pins the exact set of
// account-scoped policy rows (rules with no repository/project subject).
// The server-side coverage gate exempts exactly these names from its
// refusal-case requirement, so adding another account-scoped rule must
// update both this pin and that exemption together.
func TestMutationAuthzAccountScopedRowsArePinned(t *testing.T) {
	t.Parallel()
	got := map[string]bool{}
	for name, rule := range graphqlMutationAuthz {
		if _, accountScoped := rule.(repoCreationRule); accountScoped {
			got[name] = true
		}
	}
	if len(got) != 1 || !got["createRepository"] {
		t.Fatalf("account-scoped policy rows = %v, want exactly {createRepository}; update the server-side coverage gate's exemption in the same change", got)
	}
}
