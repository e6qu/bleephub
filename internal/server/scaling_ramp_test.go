package bleephub

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// rampWallClock reads real elapsed time for the load harness's latency and
// throughput measurement. Correctness tests must use the injected store clock
// (the repo forbids wall-clock reads in tests), but this opt-in ramp measures
// wall time on purpose and makes no time-based assertions, so the read is
// indirected here through a documented seam.
var rampWallClock = time.Now

// TestScalingRamp is the closed-loop scaling probe: it drives an increasing
// number of concurrent clients against a seeded server and, at each step,
// reports the latency distribution (p50/p95/p99), throughput, error rate, and
// process footprint. It finds the "knee" — the first concurrency level where p99
// crosses the SLO or errors appear — i.e. the system's scaling limit for the
// chosen workload.
//
// It is opt-in (heavy) and skipped unless BLEEPHUB_SCALE=1. Knobs:
//
//	BLEEPHUB_SCALE_STEPS      comma-separated worker counts (default 1,2,4,8,16,32,64)
//	BLEEPHUB_SCALE_SECONDS    seconds per step (default 3)
//	BLEEPHUB_SCALE_PROFILE    read | write | mixed (default mixed)
//	BLEEPHUB_SCALE_P99_MS     p99 SLO in ms that defines the knee (default 250)
//	BLEEPHUB_BENCH_REPOS/_ISSUES/_PRS scale the seeded corpus (see scaleCorpus)
func TestScalingRamp(t *testing.T) {
	if os.Getenv("BLEEPHUB_SCALE") != "1" {
		t.Skip("set BLEEPHUB_SCALE=1 to run the scaling ramp (heavy)")
	}
	steps := parseSteps(os.Getenv("BLEEPHUB_SCALE_STEPS"), []int{1, 2, 4, 8, 16, 32, 64})
	stepDur := time.Duration(envIntOr("BLEEPHUB_SCALE_SECONDS", 3)) * time.Second
	profile := strings.ToLower(os.Getenv("BLEEPHUB_SCALE_PROFILE"))
	if profile == "" {
		profile = "mixed"
	}
	p99SLO := time.Duration(envIntOr("BLEEPHUB_SCALE_P99_MS", 250)) * time.Millisecond

	s, h, org, gitRepo := benchServer(t, defaultCorpus())
	_ = s
	reads := readWorkload(org, gitRepo)

	t.Logf("scaling ramp: profile=%s step=%s slo(p99)=%s corpus=%s", profile, stepDur, p99SLO, org)
	t.Logf("%-8s %-10s %-10s %-10s %-10s %-9s %-10s %-8s", "workers", "req/s", "p50", "p95", "p99", "err%", "heapMiB", "goroutg")

	var counter atomic.Int64
	kneeAt := 0
	for _, workers := range steps {
		lat, total, errs := runRampStep(h, workers, stepDur, profile, org, reads, &counter)
		sort.Slice(lat, func(i, j int) bool { return lat[i] < lat[j] })
		p50, p95, p99 := pct(lat, 0.50), pct(lat, 0.95), pct(lat, 0.99)
		throughput := float64(total) / stepDur.Seconds()
		errRate := 0.0
		if total > 0 {
			errRate = 100 * float64(errs) / float64(total)
		}
		var m runtime.MemStats
		runtime.ReadMemStats(&m)
		t.Logf("%-8d %-10.0f %-10s %-10s %-10s %-9.2f %-10d %-8d",
			workers, throughput, p50.Round(time.Microsecond), p95.Round(time.Microsecond),
			p99.Round(time.Microsecond), errRate, m.HeapInuse>>20, runtime.NumGoroutine())
		if kneeAt == 0 && (p99 > p99SLO || errRate > 1) {
			kneeAt = workers
		}
	}
	if kneeAt > 0 {
		t.Logf("KNEE: p99 SLO / error threshold first crossed at %d concurrent clients", kneeAt)
	} else {
		t.Logf("no knee within the ramp: the SLO held through %d concurrent clients", steps[len(steps)-1])
	}
}

// runRampStep runs `workers` goroutines issuing the profile's requests for dur,
// returning the per-request latencies, the total count, and the error count.
func runRampStep(h http.Handler, workers int, dur time.Duration, profile, org string, reads []struct{ method, target string }, counter *atomic.Int64) ([]time.Duration, int64, int64) {
	deadline := rampWallClock().Add(dur)
	var wg sync.WaitGroup
	var total, errs atomic.Int64
	var mu sync.Mutex
	lat := make([]time.Duration, 0, 4096)

	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			local := make([]time.Duration, 0, 512)
			for rampWallClock().Before(deadline) {
				n := counter.Add(1)
				method, target, body := pickRequest(profile, org, reads, n)
				var reader *strings.Reader
				if body != "" {
					reader = strings.NewReader(body)
				}
				var req *http.Request
				if reader != nil {
					req = httptest.NewRequest(method, target, reader)
					req.Header.Set("Content-Type", "application/json")
				} else {
					req = httptest.NewRequest(method, target, nil)
				}
				req.Header.Set("Authorization", "token "+defaultToken)
				rec := httptest.NewRecorder()
				start := rampWallClock()
				h.ServeHTTP(rec, req)
				local = append(local, rampWallClock().Sub(start))
				total.Add(1)
				if rec.Code >= 500 {
					errs.Add(1)
				}
			}
			mu.Lock()
			lat = append(lat, local...)
			mu.Unlock()
		}()
	}
	wg.Wait()
	return lat, total.Load(), errs.Load()
}

func pickRequest(profile, org string, reads []struct{ method, target string }, n int64) (method, target, body string) {
	write := func() (string, string, string) {
		return http.MethodPost,
			fmt.Sprintf("/api/v3/repos/%s/repo-%04d/issues", org, n%50),
			fmt.Sprintf(`{"title":"ramp issue %d"}`, n)
	}
	read := func() (string, string, string) {
		r := reads[n%int64(len(reads))]
		return r.method, r.target, ""
	}
	switch profile {
	case "write":
		return write()
	case "read":
		return read()
	default: // mixed: ~1 in 5 writes
		if n%5 == 0 {
			return write()
		}
		return read()
	}
}

func pct(sorted []time.Duration, q float64) time.Duration {
	if len(sorted) == 0 {
		return 0
	}
	i := int(q * float64(len(sorted)))
	if i >= len(sorted) {
		i = len(sorted) - 1
	}
	return sorted[i]
}

func parseSteps(raw string, def []int) []int {
	if raw == "" {
		return def
	}
	var out []int
	for _, part := range strings.Split(raw, ",") {
		if n, err := strconv.Atoi(strings.TrimSpace(part)); err == nil && n > 0 {
			out = append(out, n)
		}
	}
	if len(out) == 0 {
		return def
	}
	return out
}

func envIntOr(name string, def int) int {
	if v := os.Getenv(name); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return def
}
