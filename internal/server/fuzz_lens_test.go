package bleephub

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// graphQLFuzzServer builds a server with the GraphQL schema initialized and a
// minimal amount of seeded state so resolvers exercise real lookup paths
// rather than bailing at "not found" immediately. It takes *testing.T so a
// *testing.F cannot be passed: GraphQL mutations write to the store, and a
// server shared across fuzz executions makes replay order-dependent.
func graphQLFuzzServer(t *testing.T) *Server {
	t.Helper()
	s := newTestServer()
	s.graphql = s.newGraphQLResolver()
	return s
}

// FuzzGraphQLWithVariables drives arbitrary GraphQL queries together with an
// arbitrary JSON variables document through the real handler. graphql-go
// coerces declared-typed args, but resolver code that asserts p.Args[...] /
// input[...] / p.Source.(T) must never panic on any combination — a wrong
// type, a null, a string where an Int is declared, a deeply nested object.
func FuzzGraphQLWithVariables(f *testing.F) {
	seeds := []struct {
		q    string
		vars string
	}{
		{"{viewer{login}}", `{}`},
		{"query($n:Int){repository(owner:\"a\",name:\"b\"){issue(number:$n){title}}}", `{"n":1}`},
		{"query($n:Int){repository(owner:\"a\",name:\"b\"){issue(number:$n){title}}}", `{"n":"not-an-int"}`},
		{"query($n:Int){repository(owner:\"a\",name:\"b\"){issue(number:$n){title}}}", `{"n":99999999999999999999}`},
		{"query($n:Int){repository(owner:\"a\",name:\"b\"){issue(number:$n){title}}}", `{"n":true}`},
		{"query($n:Int){repository(owner:\"a\",name:\"b\"){issue(number:$n){title}}}", `{"n":[1,2,3]}`},
		{"query($a:String){search(query:$a,type:REPOSITORY,first:5){repositoryCount}}", `{"a":{"nested":1}}`},
		// minimizedComment is the Minimizable interface (no `id` field there,
		// matching real GitHub) — select an interface field.
		{"mutation($i:MinimizeCommentInput!){minimizeComment(input:$i){minimizedComment{isMinimized}}}", `{"i":{"subjectId":123,"classifier":["x"]}}`},
		{"mutation($i:CreateIssueInput!){createIssue(input:$i){issue{number}}}", `{"i":{"repositoryId":42,"title":null,"body":true}}`},
		{"query($f:Int,$a:String){viewer{repositories(first:$f,after:$a){nodes{name}}}}", `{"f":-1,"a":99}`},
		{"query($f:Int,$a:String){viewer{repositories(first:$f,after:$a){nodes{name}}}}", `{"f":2147483647,"a":""}`},
		// license/codeOfConduct read a String! key arg; keep them in corpus so
		// the resolver arg-coercion path is exercised (see gh_meta_graphql.go).
		{"{license(key:\"mit\"){key name}}", `{}`},
		{"query($k:String!){license(key:$k){key}}", `{"k":"apache-2.0"}`},
		{"{codeOfConduct(key:\"citizen_code_of_conduct\"){key name}}", `{}`},
		{"query($k:String!){codeOfConduct(key:$k){key}}", `{"k":"contributor_covenant"}`},
	}
	for _, sd := range seeds {
		f.Add(sd.q, sd.vars)
	}

	f.Fuzz(func(t *testing.T, query, varsJSON string) {
		s := graphQLFuzzServer(t)
		var vars map[string]interface{}
		// Tolerate non-object / invalid variable docs; an invalid doc yields a
		// nil map, which is exactly what a real malformed request produces.
		_ = json.Unmarshal([]byte(varsJSON), &vars)

		body, _ := json.Marshal(map[string]interface{}{
			"query":     query,
			"variables": vars,
		})
		req := httptest.NewRequest(http.MethodPost, "/api/graphql", bytes.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+defaultToken)
		w := httptest.NewRecorder()
		// Drive through the real handler; a panic in any resolver fails the fuzz.
		s.handleGraphQL(w, req)
		if w.Code == 0 {
			t.Fatal("handler wrote no status")
		}
	})
}

// FuzzGraphQLRawVariables additionally fuzzes the case where the variables
// member is itself an arbitrary JSON value (not just an object), driving the
// whole decode path in handleGraphQL.
func FuzzGraphQLRawVariables(f *testing.F) {
	f.Add(`{"query":"{viewer{login}}","variables":{"x":1}}`)
	f.Add(`{"query":"{viewer{login}}","variables":[1,2,3]}`)
	f.Add(`{"query":"{viewer{login}}","variables":"str"}`)
	f.Add(`{"query":"{viewer{login}}","variables":42}`)
	f.Add(`{"query":42}`)
	f.Add(`{}`)
	f.Add(`not json`)
	f.Fuzz(func(t *testing.T, rawBody string) {
		s := graphQLFuzzServer(t)
		req := httptest.NewRequest(http.MethodPost, "/api/graphql", bytes.NewReader([]byte(rawBody)))
		req.Header.Set("Authorization", "Bearer "+defaultToken)
		w := httptest.NewRecorder()
		s.handleGraphQL(w, req)
	})
}

// FuzzRESTMapBody drives the map[string]interface{} REST body parsers that read
// fields with type assertions. These must never panic on a wrong-typed field.
func FuzzRESTMapBody(f *testing.F) {
	// Endpoints whose handlers decode into a bare map and read typed fields.
	paths := []string{
		"/api/v3/orgs/acme/teams",
	}
	f.Add(`{"name":"t","privacy":123,"permission":true,"notification_setting":["x"],"parent_team_id":"oops"}`)
	f.Add(`{"name":123}`)
	f.Add(`{}`)
	f.Add(`null`)
	f.Add(`[]`)
	f.Add(`"string"`)
	f.Add(`{"parent_team_id":1.5}`)
	f.Fuzz(func(t *testing.T, body string) {
		// A team create that succeeds would make the next execution's duplicate
		// name a 422 instead, so the server is rebuilt per execution.
		s := newTestServer()
		s.registerRoutes()
		for _, p := range paths {
			req := httptest.NewRequest(http.MethodPost, p, bytes.NewReader([]byte(body)))
			req.Header.Set("Authorization", "Bearer "+defaultToken)
			req.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()
			s.requestHandler().ServeHTTP(w, req)
		}
	})
}
