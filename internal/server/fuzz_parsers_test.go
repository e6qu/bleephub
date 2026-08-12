package bleephub

import (
	"testing"

	"github.com/e6qu/bleephub/internal/store"
	"github.com/graphql-go/graphql"
)

// FuzzGraphQLQuery drives arbitrary query strings through the real schema's
// executor. A malformed or pathological query must surface as a GraphQL error,
// never a panic.
func FuzzGraphQLQuery(f *testing.F) {
	f.Add("{viewer{login}}")
	f.Add("")
	f.Add("{")
	f.Add("query{repository(owner:\"a\",name:\"b\"){name}}")
	f.Add("{__schema{types{name}}}")
	f.Add("mutation{")
	f.Fuzz(func(t *testing.T, q string) {
		s := newTestServer()
		s.graphql = s.newGraphQLResolver()
		_ = graphql.Do(graphql.Params{Schema: s.graphql.Schema(), RequestString: q})
	})
}

// FuzzParseContentRange checks the cache chunk Content-Range parser.
func FuzzParseContentRange(f *testing.F) {
	f.Add("bytes 0-1023/*")
	f.Add("bytes -")
	f.Add("bytes 9999999999999999999999-0/*")
	f.Add("")
	f.Add("bytes 0-/*")
	f.Fuzz(func(t *testing.T, h string) {
		_, _, _ = parseContentRange(h)
	})
}

// FuzzParseAndVerifyAppJWT feeds arbitrary token strings to the app-JWT parser.
func FuzzParseAndVerifyAppJWT(f *testing.F) {
	f.Add("a.b.c")
	f.Add("")
	f.Add("....")
	f.Add("eyJ.eyJ.")
	f.Fuzz(func(t *testing.T, tok string) {
		st := store.NewStore()
		_, _ = st.ParseAndVerifyAppJWT(tok)
	})
}

// FuzzAgentRSAPublicKey feeds arbitrary modulus/exponent strings.
func FuzzAgentRSAPublicKey(f *testing.F) {
	f.Add("AQAB", "AQAB")
	f.Add("", "")
	f.Add("////", "!!!!")
	f.Fuzz(func(t *testing.T, mod, exp string) {
		_, _ = agentRSAPublicKey(&store.AgentPublicKey{Modulus: mod, Exponent: exp})
	})
}
