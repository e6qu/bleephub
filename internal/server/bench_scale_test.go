package bleephub

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/e6qu/bleephub/internal/store"
)

// seedWorkflowRuns inserts n completed workflow runs spread across the corpus
// repos directly into the store, so a run-listing benchmark can measure how the
// per-repo listing scales with the *total* number of runs in the instance. This
// is the fastest-growing collection in a CI workload and (before the per-repo
// index) forces a full scan of every run on each list.
func seedWorkflowRuns(s *Server, org string, repos, n int) {
	s.store.Mu.Lock()
	defer s.store.Mu.Unlock()
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for i := 0; i < n; i++ {
		repo := fmt.Sprintf("%s/repo-%04d", org, i%repos)
		wf := &store.Workflow{
			ID:           fmt.Sprintf("bench-wf-%d", i),
			RunID:        1_000_000 + i,
			RunNumber:    i + 1,
			RepoFullName: repo,
			Name:         "CI",
			Status:       store.WorkflowStatusCompleted,
			CreatedAt:    base.Add(time.Duration(i) * time.Second),
			Sha:          "0000000000000000000000000000000000000000",
			Ref:          "refs/heads/main",
			EventName:    "push",
			Jobs:         map[string]*store.WorkflowJob{},
		}
		s.store.Workflows[wf.ID] = wf
		if s.store.WorkflowsByRunID != nil {
			s.store.WorkflowsByRunID[wf.RunID] = wf
		}
	}
}

// BenchmarkWorkflowRunListingScale measures GET .../actions/runs for one repo as
// the instance-wide run count grows (BLEEPHUB_BENCH_RUNS, default 5000). Today
// the handler scans every run in the store; with a per-repo index it becomes
// independent of the instance total. Run before/after the WS5 index to compare.
func BenchmarkWorkflowRunListingScale(b *testing.B) {
	cfg := corpusConfig{repos: 50, issuesPerRepo: 0, prsPerRepo: 0, gitCommitsRepo: 0}
	s, h, org, _ := benchServer(b, cfg)
	total := 5000
	if v := envInt("BLEEPHUB_BENCH_RUNS"); v > 0 {
		total = v
	}
	seedWorkflowRuns(s, org, cfg.repos, total)
	target := "/api/v3/repos/" + org + "/repo-0000/actions/runs?per_page=30"
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		benchDo(b, h, http.MethodGet, target, "")
	}
}

// BenchmarkConcurrentIssueCreate is the write-contention benchmark. Many
// goroutines create issues across many repos through the full handler stack, so
// the run measures how the single global Store.Mu write lock (held across the
// persistence commit) serializes unrelated writers. Run with -cpu 1,2,4,8 to see
// the scaling, and with -mutexprofile to attribute the contention.
func BenchmarkConcurrentIssueCreate(b *testing.B) {
	cfg := corpusConfig{repos: 32, issuesPerRepo: 0, prsPerRepo: 0, gitCommitsRepo: 0}
	_, h, org, _ := benchServer(b, cfg)
	var counter atomic.Int64
	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			n := counter.Add(1)
			target := fmt.Sprintf("/api/v3/repos/%s/repo-%04d/issues", org, n%int64(cfg.repos))
			body := fmt.Sprintf(`{"title":"bench issue %d"}`, n)
			r := httptest.NewRequest(http.MethodPost, target, strings.NewReader(body))
			r.Header.Set("Authorization", "token "+defaultToken)
			r.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if w.Code >= 500 {
				b.Fatalf("POST %s -> %d: %s", target, w.Code, w.Body.String())
			}
		}
	})
}

// BenchmarkGraphQLNestedConnections measures a nested-connection query that is
// small in static fields (a handful, well under the 5000 cap and depth 20) but
// resolves up to first^depth nodes. It quantifies the gap between the static
// field cap and real resolved work — and, once the resolved-node cost limit
// lands (WS5), that the over-budget query is now rejected cheaply.
func BenchmarkGraphQLNestedConnections(b *testing.B) {
	s, h, org, _ := benchServer(b, corpusConfig{repos: 20, issuesPerRepo: 30, prsPerRepo: 10, commentsPerPR: 3, gitCommitsRepo: 0})
	_ = s
	query := fmt.Sprintf(`{"query":"{ organization(login:\"%s\") { repositories(first:50){ nodes { issues(first:50){ nodes { comments(first:50){ nodes { id } } } } } } } }"}`, org)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		r := httptest.NewRequest(http.MethodPost, "/api/graphql", strings.NewReader(query))
		r.Header.Set("Authorization", "token "+defaultToken)
		r.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code >= 500 {
			b.Fatalf("graphql -> %d: %s", w.Code, w.Body.String())
		}
	}
}

// BenchmarkGlobalIssuesMentionScan measures GET /issues?filter=mentioned, a
// cross-repo endpoint that scans every issue, every PR, and (for the mention
// test) every comment — O((issues+PRs)×comments). It charts the Tier-A curve as
// the corpus grows (BLEEPHUB_BENCH_* dimensions).
func BenchmarkGlobalIssuesMentionScan(b *testing.B) {
	_, h, _, _ := benchServer(b, defaultCorpus())
	target := "/api/v3/issues?filter=mentioned&state=all&per_page=30"
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		benchDo(b, h, http.MethodGet, target, "")
	}
}
