package bleephub

import (
	"net/http"

	"github.com/e6qu/bleephub/internal/store"
)

// GitHub's codes-of-conduct REST surface. The catalog itself (the two
// entries and their embedded body texts) lives in internal/store so the
// GraphQL resolver layer renders from the same data (ARCH-003).

func (s *Server) registerGHCodesOfConductRoutes() {
	s.route("GET /api/v3/codes_of_conduct", s.handleListCodesOfConduct)
	s.route("GET /api/v3/codes_of_conduct/{key}", s.handleGetCodeOfConduct)
}

// codeOfConductToJSON renders the spec `code-of-conduct` shape. The list
// endpoint omits body (matching GitHub); the get-by-key endpoint includes it.
func codeOfConductToJSON(c store.CodeOfConduct, baseURL string, withBody bool) map[string]interface{} {
	out := map[string]interface{}{
		"key":      c.Key,
		"name":     c.Name,
		"url":      baseURL + "/api/v3/codes_of_conduct/" + c.Key,
		"html_url": nil,
	}
	if withBody {
		out["body"] = c.Body
	}
	return out
}

func (s *Server) handleListCodesOfConduct(w http.ResponseWriter, r *http.Request) {
	base := s.baseURL(r)
	out := make([]map[string]interface{}, 0, len(store.CodesOfConductCatalog))
	for _, c := range store.CodesOfConductCatalog {
		out = append(out, codeOfConductToJSON(c, base, false))
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleGetCodeOfConduct(w http.ResponseWriter, r *http.Request) {
	key := r.PathValue("key")
	for _, c := range store.CodesOfConductCatalog {
		if c.Key == key {
			writeJSON(w, http.StatusOK, codeOfConductToJSON(c, s.baseURL(r), true))
			return
		}
	}
	writeGHError(w, http.StatusNotFound, "Not Found")
}
