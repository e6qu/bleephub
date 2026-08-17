package bleephub

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"

	"github.com/graphql-go/graphql"
	"github.com/graphql-go/graphql/gqlerrors"

	"github.com/e6qu/bleephub/internal/graphqlapi"
)

func (s *Server) registerGHGraphQLRoutes() {
	s.route("POST /api/graphql", s.handleGraphQL)
}

// handleGraphQL executes a GraphQL query.
//
// The endpoint requires authentication. GitHub's /graphql rejects every
// anonymous request with 401 and has no public read surface at all, which
// matters here for more than parity: resolvers reach the store directly, so an
// anonymous caller admitted at this door is a caller inside every connection
// the schema exposes.
func (s *Server) handleGraphQL(w http.ResponseWriter, r *http.Request) {
	if ghUserFromContext(r.Context()) == nil &&
		ghInstallationTokenFromContext(r.Context()) == nil &&
		ghUserToServerTokenFromContext(r.Context()) == nil {
		writeGHError(w, http.StatusUnauthorized, "This endpoint requires you to be authenticated.")
		return
	}

	var req struct {
		Query         string                 `json:"query"`
		Variables     map[string]interface{} `json:"variables"`
		OperationName string                 `json:"operationName"`
	}
	if !decodeJSONBody(w, r, &req) {
		return
	}

	document, validationErrors, err := graphqlapi.PrepareDocument(s.graphql.Schema(), req.Query)
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]interface{}{
			"data":   nil,
			"errors": []map[string]interface{}{{"message": err.Error()}},
		})
		return
	}
	var result *graphql.Result
	if len(validationErrors) != 0 {
		result = &graphql.Result{Errors: validationErrors}
	} else if err := graphqlapi.CheckDocumentLimits(document, req.Variables, 20, 5000); err != nil {
		result = &graphql.Result{Errors: []gqlerrors.FormattedError{gqlerrors.NewFormattedError(err.Error())}}
	} else {
		result = graphql.Execute(graphql.ExecuteParams{
			Schema:        s.graphql.Schema(),
			AST:           document,
			OperationName: req.OperationName,
			Args:          req.Variables,
			Context:       r.Context(),
		})
	}

	if len(result.Errors) > 0 {
		digest := sha256.Sum256([]byte(req.Query))
		s.logger.Debug().
			Str("operation", req.OperationName).
			Str("query_sha256", hex.EncodeToString(digest[:])).
			Int("query_bytes", len(req.Query)).
			Interface("errors", result.Errors).
			Msg("graphql errors")
	}

	// Re-shape errors[] into GitHub's wire form: real GitHub adds a
	// non-spec top-level "type" member (NOT_FOUND, FORBIDDEN, ...) that
	// graphql-go's FormattedError cannot carry, so the envelope is built
	// by hand instead of serializing graphql.Result directly.
	out := map[string]interface{}{"data": result.Data}
	if len(result.Errors) > 0 {
		errItems := make([]map[string]interface{}, 0, len(result.Errors))
		for _, fe := range result.Errors {
			item := map[string]interface{}{"message": fe.Message}
			if len(fe.Locations) > 0 {
				item["locations"] = fe.Locations
			}
			if len(fe.Path) > 0 {
				item["path"] = fe.Path
			}
			if len(fe.Extensions) > 0 {
				item["extensions"] = fe.Extensions
			}
			if graphqlapi.ErrorIsNotFound(fe) {
				item["type"] = "NOT_FOUND"
			} else if graphqlapi.ErrorIsForbidden(fe) {
				item["type"] = "FORBIDDEN"
			}
			errItems = append(errItems, item)
		}
		out["errors"] = errItems
	}
	writeJSON(w, http.StatusOK, out)
}
