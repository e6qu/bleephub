package bleephub

import (
	"net/http"
	"strings"

	"github.com/e6qu/bleephub/internal/store"
	git "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
)

// GitHub exposes blame only over its web UI / GraphQL, never plain REST, so it
// lives under /ui-data (auto-authenticated as the viewer).
func (s *Server) registerGHBlameRoutes() {
	s.route("GET /ui-data/repos/{owner}/{repo}/blame/{path...}", s.handleUIDataBlame)
}

type blameHunk struct {
	SHA       string   `json:"sha"`
	ShortSHA  string   `json:"short_sha"`
	Summary   string   `json:"summary"`
	Author    string   `json:"author"`
	Date      string   `json:"date"`
	StartLine int      `json:"start_line"`
	Lines     []string `json:"lines"`
}

func (s *Server) handleUIDataBlame(w http.ResponseWriter, r *http.Request) {
	owner := r.PathValue("owner")
	repoName := r.PathValue("repo")
	path := strings.Trim(r.PathValue("path"), "/")

	repo := s.lookupReadableRepoFromPath(w, r)
	if repo == nil {
		return
	}
	if path == "" {
		store.WriteGHValidationError(w, "Blame", "path", "missing_field")
		return
	}

	stor := s.store.GetGitStorage(owner, repoName)
	if stor == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	ref := r.URL.Query().Get("ref")
	if ref == "" {
		ref = repo.DefaultBranch
	}
	hash, ok, err := store.ResolveGitObjectReference(stor, ref)
	if err != nil || !ok {
		writeGHError(w, http.StatusNotFound, "No commit found for ref: "+ref)
		return
	}
	commit, err := object.GetCommit(stor, hash)
	if err != nil {
		writeGHError(w, http.StatusNotFound, "No commit found for ref: "+ref)
		return
	}
	result, err := git.Blame(commit, path)
	if err != nil {
		writeGHError(w, http.StatusNotFound, "No blame available for path: "+path)
		return
	}

	// Cache each blamed commit's summary/author (a Line carries only the author
	// email), then coalesce runs of the same commit into hunks.
	commitCache := map[plumbing.Hash]*object.Commit{}
	lookup := func(h plumbing.Hash) *object.Commit {
		if c, seen := commitCache[h]; seen {
			return c
		}
		c, cerr := object.GetCommit(stor, h)
		if cerr != nil {
			c = nil
		}
		commitCache[h] = c
		return c
	}

	hunks := []blameHunk{}
	for i, line := range result.Lines {
		if n := len(hunks); n > 0 && hunks[n-1].SHA == line.Hash.String() {
			hunks[n-1].Lines = append(hunks[n-1].Lines, line.Text)
			continue
		}
		summary := ""
		author := line.Author
		date := line.Date.UTC().Format("2006-01-02T15:04:05Z")
		if c := lookup(line.Hash); c != nil {
			summary = strings.SplitN(strings.TrimSpace(c.Message), "\n", 2)[0]
			author = c.Author.Name
			date = c.Author.When.UTC().Format("2006-01-02T15:04:05Z")
		}
		sha := line.Hash.String()
		short := sha
		if len(short) > 7 {
			short = short[:7]
		}
		hunks = append(hunks, blameHunk{
			SHA:       sha,
			ShortSHA:  short,
			Summary:   summary,
			Author:    author,
			Date:      date,
			StartLine: i + 1,
			Lines:     []string{line.Text},
		})
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"path":  path,
		"ref":   ref,
		"sha":   commit.Hash.String(),
		"hunks": hunks,
	})
}
