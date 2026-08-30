package bleephub

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/e6qu/bleephub/internal/store"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/rs/zerolog"
)

// corpusConfig controls the size of the seeded benchmark fixture. The defaults
// are large enough (hundreds of repos, thousands of issues/PRs) that any
// per-request full-store scan or O(n²) handler shows up as superlinear growth
// in ns/op; shrink via the fields when a target only needs a small corpus.
type corpusConfig struct {
	repos          int
	issuesPerRepo  int
	prsPerRepo     int
	commentsPerPR  int
	reviewsPerPR   int
	gitCommitsRepo int // commit-chain length on the single git-backed repo
}

func defaultCorpus() corpusConfig {
	return scaleCorpus(corpusConfig{
		repos:          200,
		issuesPerRepo:  20,
		prsPerRepo:     10,
		commentsPerPR:  3,
		reviewsPerPR:   2,
		gitCommitsRepo: 60,
	})
}

// scaleCorpus lets a scaling run grow the fixture without editing code, via
// BLEEPHUB_BENCH_REPOS / _ISSUES / _PRS (each overriding the matching dimension).
// A scaling sweep sets these to chart how a handler's ns/op grows with data.
func scaleCorpus(cfg corpusConfig) corpusConfig {
	if v := envInt("BLEEPHUB_BENCH_REPOS"); v > 0 {
		cfg.repos = v
	}
	if v := envInt("BLEEPHUB_BENCH_ISSUES"); v >= 0 && os.Getenv("BLEEPHUB_BENCH_ISSUES") != "" {
		cfg.issuesPerRepo = v
	}
	if v := envInt("BLEEPHUB_BENCH_PRS"); v >= 0 && os.Getenv("BLEEPHUB_BENCH_PRS") != "" {
		cfg.prsPerRepo = v
	}
	return cfg
}

func envInt(name string) int {
	if v := os.Getenv(name); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return -1
}

// seedUnboundedRateLimits marks every rate-limit window for the default token
// (and the anonymous loopback identity) unbounded, so a benchmark or load run
// that exceeds the 5000/hr core limit is not silently answered with 403s —
// which benchDo does not catch (it fails only on 5xx/404). Mirrors the shared
// test server's setup in TestMain.
func seedUnboundedRateLimits(s *Server) {
	if s.rateLimits == nil {
		s.rateLimits = map[string]*apiRateWindow{}
	}
	authed := httptest.NewRequest(http.MethodGet, "/api/v3/user", nil)
	authed.Header.Set("Authorization", "Bearer "+defaultToken)
	anon := httptest.NewRequest(http.MethodGet, "/api/v3/user", nil)
	anon.RemoteAddr = "127.0.0.1:1"
	for _, identity := range []string{apiRateIdentity(authed), apiRateIdentity(anon)} {
		for resource := range apiRateResourceLimits {
			s.rateLimits[identity+"\x1f"+resource] = &apiRateWindow{
				Limit:     apiRateResourceLimits[resource],
				Reset:     testRateLimitReset,
				unbounded: true,
			}
		}
	}
}

// benchServer builds a fully-routed in-process server (all routes registered,
// in-memory store and git storage) and seeds a large corpus. It returns the
// server, the fully-wrapped handler chain (matching ListenAndServe minus otel /
// logging), the org login, and the name of the one repo that carries real git
// history.
func benchServer(tb testing.TB, cfg corpusConfig) (*Server, http.Handler, string, string) {
	tb.Helper()
	logger := zerolog.Nop()
	s := NewServer("127.0.0.1:0", logger)

	seedUnboundedRateLimits(s)
	admin := s.store.LookupUserByLogin("admin")
	if admin == nil {
		tb.Fatal("admin user not seeded")
	}
	org := s.store.CreateOrg(admin, "bench-org", "Bench Org", "")
	if org == nil {
		tb.Fatal("failed to create org")
	}

	gitRepoName := ""
	for i := 0; i < cfg.repos; i++ {
		name := fmt.Sprintf("repo-%04d", i)
		repo := s.store.CreateOrgRepo(org, admin, name, "seeded", false)
		if repo == nil {
			tb.Fatalf("failed to create repo %s", name)
		}
		branches := make([]string, 0, cfg.prsPerRepo)
		for j := 0; j < cfg.prsPerRepo; j++ {
			branches = append(branches, fmt.Sprintf("feature-%d", j))
		}
		seedPullRequestBranches(tb, s, repo, branches...)
		for j := 0; j < cfg.issuesPerRepo; j++ {
			iss := s.store.CreateIssue(repo.ID, admin.ID,
				fmt.Sprintf("issue %d in %s about widgets", j, name),
				fmt.Sprintf("body text for issue %d discussing throughput", j),
				nil, nil, 0)
			if iss == nil {
				tb.Fatal("failed to create issue")
			}
		}
		for j := 0; j < cfg.prsPerRepo; j++ {
			pr := s.store.CreatePullRequest(repo.ID, admin.ID,
				fmt.Sprintf("pr %d in %s about latency", j, name),
				"pr body", fmt.Sprintf("feature-%d", j), "main", false, nil, nil, 0)
			if pr == nil {
				tb.Fatal("failed to create PR")
			}
			for k := 0; k < cfg.commentsPerPR; k++ {
				s.store.CreateCommentFor("pull_request", pr.ID, admin.ID, fmt.Sprintf("comment %d", k))
			}
			for k := 0; k < cfg.reviewsPerPR; k++ {
				s.store.CreatePullRequestReview(repo.FullName, pr.Number, admin.ID, "lgtm", "APPROVED")
			}
		}
		if i == 0 {
			gitRepoName = name
			seedGitHistory(tb, s, repo.FullName, cfg.gitCommitsRepo)
		}
	}
	handler := s.requestHandler()
	return s, handler, org.Login, gitRepoName
}

// seedGitHistory writes a linear commit chain onto the repo's in-memory git
// storage so the commit / contributor / stats / archive read handlers have
// real objects to walk.
func seedGitHistory(tb testing.TB, s *Server, fullName string, commits int) {
	tb.Helper()
	if commits <= 0 {
		return
	}
	storer := s.store.GitStorages[fullName]
	var parent plumbing.Hash
	for i := 0; i < commits; i++ {
		blob, err := storeBlob(storer, []byte(fmt.Sprintf("line %d\ncontent revision %d\n", i, i)))
		if err != nil {
			tb.Fatalf("store blob: %v", err)
		}
		tree, err := storeTree(storer, []object.TreeEntry{
			{Name: "README.md", Mode: 0o100644, Hash: blob},
		})
		if err != nil {
			tb.Fatalf("store tree: %v", err)
		}
		commit, err := storeCommit(storer, tree, parent, fmt.Sprintf("commit %d", i))
		if err != nil {
			tb.Fatalf("store commit: %v", err)
		}
		parent = commit
	}
	if err := storer.SetReference(plumbing.NewHashReference(plumbing.NewBranchReferenceName("main"), parent)); err != nil {
		tb.Fatalf("set ref: %v", err)
	}
}

// benchDo issues one authenticated request through the wrapped handler and
// fails the benchmark if the status is unexpected (a 5xx or 404 during a
// benchmark means the corpus/route is wrong and the numbers are meaningless).
func benchDo(b *testing.B, h http.Handler, method, target string, body string) {
	var r *http.Request
	if body != "" {
		r = httptest.NewRequest(method, target, strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
	} else {
		r = httptest.NewRequest(method, target, nil)
	}
	r.Header.Set("Authorization", "token "+defaultToken)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code >= 500 || w.Code == http.StatusNotFound {
		b.Fatalf("%s %s -> %d: %s", method, target, w.Code, w.Body.String())
	}
}

// List / read hot paths

func BenchmarkListOrgRepos(b *testing.B) {
	_, h, org, _ := benchServer(b, defaultCorpus())
	target := "/api/v3/orgs/" + org + "/repos?per_page=30"
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		benchDo(b, h, "GET", target, "")
	}
}

func BenchmarkListRepoIssues(b *testing.B) {
	_, h, org, _ := benchServer(b, defaultCorpus())
	target := "/api/v3/repos/" + org + "/repo-0100/issues?state=all&per_page=30"
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		benchDo(b, h, "GET", target, "")
	}
}

func BenchmarkListRepoPulls(b *testing.B) {
	_, h, org, _ := benchServer(b, defaultCorpus())
	target := "/api/v3/repos/" + org + "/repo-0100/pulls?state=all&per_page=30"
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		benchDo(b, h, "GET", target, "")
	}
}

func BenchmarkGetIssueByNumber(b *testing.B) {
	_, h, org, _ := benchServer(b, defaultCorpus())
	target := "/api/v3/repos/" + org + "/repo-0150/issues/10"
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		benchDo(b, h, "GET", target, "")
	}
}

func BenchmarkSearchIssues(b *testing.B) {
	_, h, _, _ := benchServer(b, defaultCorpus())
	target := "/api/v3/search/issues?q=widgets+throughput"
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		benchDo(b, h, "GET", target, "")
	}
}

func BenchmarkListNotifications(b *testing.B) {
	_, h, _, _ := benchServer(b, defaultCorpus())
	target := "/api/v3/notifications?all=true&per_page=50"
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		benchDo(b, h, "GET", target, "")
	}
}

func BenchmarkGraphQLPullRequests(b *testing.B) {
	_, h, org, _ := benchServer(b, defaultCorpus())
	query := fmt.Sprintf(`{"query":"{ repository(owner:\"%s\", name:\"repo-0050\") { pullRequests(first: 20) { nodes { number title reviews(first: 10) { nodes { state } } } } } }"}`, org)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		benchDo(b, h, "POST", "/api/graphql", query)
	}
}

// Serializer micro-benchmarks (no HTTP framing)

func BenchmarkRepoToJSON(b *testing.B) {
	s, _, org, _ := benchServer(b, defaultCorpus())
	repo := s.store.GetRepo(org, "repo-0100")
	if repo == nil {
		b.Fatal("repo not found")
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = store.RepoToJSON(repo, s.store, "http://bench.local")
	}
}

// Git-backed reads

func BenchmarkListCommits(b *testing.B) {
	_, h, org, gitRepo := benchServer(b, defaultCorpus())
	target := "/api/v3/repos/" + org + "/" + gitRepo + "/commits?per_page=30"
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		benchDo(b, h, "GET", target, "")
	}
}

func BenchmarkListContributors(b *testing.B) {
	_, h, org, gitRepo := benchServer(b, defaultCorpus())
	target := "/api/v3/repos/" + org + "/" + gitRepo + "/contributors"
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		benchDo(b, h, "GET", target, "")
	}
}

func BenchmarkStatsContributors(b *testing.B) {
	_, h, org, gitRepo := benchServer(b, defaultCorpus())
	target := "/api/v3/repos/" + org + "/" + gitRepo + "/stats/contributors"
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		benchDo(b, h, "GET", target, "")
	}
}

func BenchmarkGetTarball(b *testing.B) {
	_, h, org, gitRepo := benchServer(b, defaultCorpus())
	target := "/api/v3/repos/" + org + "/" + gitRepo + "/tarball/main"
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		benchDo(b, h, "GET", target, "")
	}
}
