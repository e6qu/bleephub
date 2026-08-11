package bleephub

import (
	"net/http"
	"sort"
)

func (s *Server) handleGetRepoLanguages(w http.ResponseWriter, r *http.Request) {
	owner := r.PathValue("owner")
	name := r.PathValue("repo")

	repo := s.store.GetRepo(owner, name)
	if repo == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}

	if repo.Private && !s.viewerCanReadRepo(r.Context(), repo) {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}

	counts := s.store.ComputeRepoLanguages(repo)
	if counts == nil {
		writeJSON(w, http.StatusOK, map[string]interface{}{})
		return
	}

	// GitHub sorts languages by byte count descending.
	type pair struct {
		lang  string
		bytes int64
	}
	pairs := make([]pair, 0, len(counts))
	for lang, n := range counts {
		pairs = append(pairs, pair{lang, n})
	}
	sort.Slice(pairs, func(i, j int) bool {
		if pairs[i].bytes != pairs[j].bytes {
			return pairs[i].bytes > pairs[j].bytes
		}
		return pairs[i].lang < pairs[j].lang
	})

	out := make(map[string]interface{}, len(pairs))
	for _, p := range pairs {
		out[p.lang] = p.bytes
	}
	writeJSON(w, http.StatusOK, out)
}
