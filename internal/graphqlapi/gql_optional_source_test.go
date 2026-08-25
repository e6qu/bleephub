package graphqlapi

import (
	"strings"
	"testing"
	"time"

	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/graphql-go/graphql"

	"github.com/e6qu/bleephub/internal/store"
)

// fixedSignatureTime pins the signature timestamps these tests render with:
// the resolver layer's tests must not read the wall clock.
var fixedSignatureTime = time.Date(2042, 7, 15, 12, 0, 0, 0, time.UTC)

// TestTypedNilChildAbortsTheWholeSubtree is the executable statement of the
// defect gql_optional_source.go exists to prevent, built on a schema of its
// own so it keeps holding whatever the bleephub schema does.
//
// A nil map[string]interface{} in an interface-typed member is not a nil
// interface: graphql-go's isNullish sees reflect.Map and reports the child
// present, descends into an empty shell and fails the child's non-null id.
// The failure is not confined to the child — it unwinds to the nearest
// nullable ancestor and puts an error in the response, so a client that
// treats `errors` as fatal sees the whole query fail.
func TestTypedNilChildAbortsTheWholeSubtree(t *testing.T) {
	t.Parallel()
	child := graphql.NewObject(graphql.ObjectConfig{
		Name: "Child",
		Fields: graphql.Fields{
			"id": &graphql.Field{Type: graphql.NewNonNull(graphql.ID)},
		},
	})
	parent := graphql.NewObject(graphql.ObjectConfig{
		Name: "Parent",
		Fields: graphql.Fields{
			"name":  &graphql.Field{Type: graphql.NewNonNull(graphql.String)},
			"child": &graphql.Field{Type: child},
		},
	})

	run := func(t *testing.T, absent interface{}) *graphql.Result {
		t.Helper()
		schema, err := graphql.NewSchema(graphql.SchemaConfig{
			Query: graphql.NewObject(graphql.ObjectConfig{
				Name: "Query",
				Fields: graphql.Fields{
					"parent": &graphql.Field{
						Type: parent,
						Resolve: func(graphql.ResolveParams) (interface{}, error) {
							return map[string]interface{}{"name": "p", "child": absent}, nil
						},
					},
				},
			}),
		})
		if err != nil {
			t.Fatalf("build schema: %v", err)
		}
		return graphql.Do(graphql.Params{Schema: schema, RequestString: "{parent{name child{id}}}"})
	}

	var typedNil map[string]interface{}
	broken := run(t, typedNil)
	if len(broken.Errors) == 0 {
		t.Fatal("a typed-nil child no longer aborts the query — the helpers in gql_optional_source.go may no longer be needed, but confirm against the graphql-go version before relaxing anything")
	}
	if !strings.Contains(broken.Errors[0].Message, "Cannot return null for non-nullable field") {
		t.Fatalf("unexpected failure mode: %v", broken.Errors[0].Message)
	}

	fixed := run(t, optionalObject(typedNil))
	if len(fixed.Errors) > 0 {
		t.Fatalf("optionalObject must render an absent child as null: %v", fixed.Errors)
	}
	data, _ := fixed.Data.(map[string]interface{})
	parentData, _ := data["parent"].(map[string]interface{})
	if parentData["child"] != nil {
		t.Fatalf("child = %#v, want null", parentData["child"])
	}
	if parentData["name"] != "p" {
		t.Fatalf("the surrounding subtree was discarded: %#v", parentData)
	}
}

// TestOptionalRenderedSkipsTheRendererForAnAbsentRecord: the point of the
// helper is that a renderer which dereferences its argument can be called
// unconditionally, so the caller never writes the declare-then-assign shape
// the defect hides in.
func TestOptionalRenderedSkipsTheRendererForAnAbsentRecord(t *testing.T) {
	t.Parallel()
	rendered := optionalRendered(nil, func(*store.User) map[string]interface{} {
		t.Error("renderer ran for an absent record")
		return map[string]interface{}{}
	})
	if rendered != nil {
		t.Fatalf("optionalRendered(nil) = %#v, want an untyped nil interface", rendered)
	}
	present := optionalRendered(&store.User{Login: "octocat"}, func(u *store.User) map[string]interface{} {
		return map[string]interface{}{"login": u.Login}
	})
	source, ok := present.(map[string]interface{})
	if !ok || source["login"] != "octocat" {
		t.Fatalf("optionalRendered dropped a present record: %#v", present)
	}
}

// TestGitActorSourceLeavesUserNullForAnUnknownSigner pins the first of the two
// instances the class was found in: `gh pr view --json commits` failed
// outright because a commit signed by an address no account owns rendered a
// User shell with no node id.
func TestGitActorSourceLeavesUserNullForAnUnknownSigner(t *testing.T) {
	t.Parallel()
	st := newSeededTestStore()
	unknown := object.Signature{Name: "Nobody", Email: "nobody@example.invalid", When: fixedSignatureTime}

	for name, actor := range map[string]map[string]interface{}{
		"gitActorSource":       gitActorSource(st, unknown),
		"gitActorSourceLocked": gitActorSourceLocked(st, unknown),
	} {
		if user := actor["user"]; user != nil {
			t.Errorf("%s: user = %#v, want a null GitActor.user for an address no account owns", name, user)
		}
	}

	admin := st.UsersByLogin["admin"]
	known := object.Signature{Name: admin.Name, Email: admin.Email, When: fixedSignatureTime}
	if actor := gitActorSourceLocked(st, known); actor["user"] == nil {
		t.Error("gitActorSourceLocked dropped the account that owns the signature's email")
	}
}
