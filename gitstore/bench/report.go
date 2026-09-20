package main

import (
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
	"text/tabwriter"
	"time"
)

// Report is everything one invocation measured, in a form that can be diffed
// against another invocation's.
type Report struct {
	Workload WorkloadSpec `json:"workload"`
	Objects  int          `json:"objects"`
	// InitialObjects and InitialPackBytes size the first push, the one that
	// carries the starting history.
	InitialObjects   int               `json:"initial_objects"`
	InitialPackBytes int               `json:"initial_pack_bytes"`
	Latency          time.Duration     `json:"latency_ns"`
	Endpoint         string            `json:"endpoint"`
	Runs             int               `json:"runs"`
	Drivers          map[string]string `json:"drivers"`
	Results          []Result          `json:"results"`
}

func (r *Report) writeJSON(w io.Writer) error {
	encoder := json.NewEncoder(w)
	encoder.SetIndent("", "  ")
	return encoder.Encode(r)
}

// writeBenchfmt emits one Go benchmark line per measurement, so benchstat can
// compare two invocations (a branch against main, one endpoint against another)
// with its usual statistics.
func (r *Report) writeBenchfmt(w io.Writer) error {
	for _, result := range r.Results {
		if result.Error != "" || result.Skipped != "" {
			continue
		}
		_, err := fmt.Fprintf(w, "BenchmarkStorer/scenario=%s/driver=%s 1 %d ns/op %d s3-requests/op %d s3-bytes/op\n",
			result.Scenario, result.Driver, result.Elapsed.Nanoseconds(), result.S3.Total(), result.S3.BytesDown+result.S3.BytesUp)
		if err != nil {
			return err
		}
	}
	return nil
}

// median is the statistic reported: with a handful of runs, one run that caught
// a garbage collection or a noisy neighbour should not move the figure.
func median(values []float64) float64 {
	sort.Float64s(values)
	n := len(values)
	if n == 0 {
		return 0
	}
	if n%2 == 1 {
		return values[n/2]
	}
	return (values[n/2-1] + values[n/2]) / 2
}

func (r *Report) writeTable(w io.Writer, driverOrder []string) {
	fmt.Fprintf(w, "workload: %d files, %d commits then %d pushes, %d files changed per commit → %d objects (initial push: %d objects, %s pack)\n",
		r.Workload.Files, r.Workload.Commits, r.Workload.Pushes, r.Workload.Changes, r.Objects,
		r.InitialObjects, humanBytes(float64(r.InitialPackBytes)))
	fmt.Fprintf(w, "endpoint: %s, injected latency %s per request, median of %d run(s)\n\n", r.Endpoint, r.Latency, r.Runs)
	for _, name := range driverOrder {
		fmt.Fprintf(w, "  %-14s %s\n", name, r.Drivers[name])
	}

	byKey := map[string][]Result{}
	for _, result := range r.Results {
		key := result.Scenario + "\x00" + result.Driver
		byKey[key] = append(byKey[key], result)
	}

	for _, scenario := range scenarioOrder {
		var rows []string
		var baseline float64
		for _, driver := range driverOrder {
			results := byKey[scenario+"\x00"+driver]
			if len(results) == 0 {
				continue
			}
			if failed := firstFailure(results); failed != "" {
				rows = append(rows, fmt.Sprintf("%s\t%s\t\t\t\t\t\t\t\t\t", driver, failed))
				continue
			}
			var elapsed, requests, get, ranged, head, put, list, del, down, up []float64
			for _, result := range results {
				elapsed = append(elapsed, float64(result.Elapsed))
				requests = append(requests, float64(result.S3.Total()))
				get = append(get, float64(result.S3.Get))
				ranged = append(ranged, float64(result.S3.GetRanged))
				head = append(head, float64(result.S3.Head))
				put = append(put, float64(result.S3.Put+result.S3.Multipart+result.S3.Copy))
				list = append(list, float64(result.S3.List))
				del = append(del, float64(result.S3.Delete))
				down = append(down, float64(result.S3.BytesDown))
				up = append(up, float64(result.S3.BytesUp))
			}
			took := median(elapsed)
			if baseline == 0 {
				baseline = took
			}
			rows = append(rows, fmt.Sprintf("%s\t%s\t%.2fx\t%.0f\t%.0f\t%.0f\t%.0f\t%.0f\t%.0f\t%.0f\t%s\t%s",
				driver, time.Duration(took).Round(10*time.Microsecond), took/baseline, median(requests),
				median(get), median(ranged), median(head), median(put), median(list), median(del),
				humanBytes(median(down)), humanBytes(median(up))))
		}
		if len(rows) == 0 {
			continue
		}
		fmt.Fprintf(w, "\n%s — %s\n", scenario, scenarioNotes[scenario])
		table := tabwriter.NewWriter(w, 0, 0, 2, ' ', tabwriter.AlignRight)
		fmt.Fprintln(table, "driver\ttime\tvs first\tS3 reqs\tGET\tranged\tHEAD\twrite\tLIST\tDELETE\tdown\tup\t")
		for _, row := range rows {
			fmt.Fprintln(table, row+"\t")
		}
		_ = table.Flush()
	}
}

func firstFailure(results []Result) string {
	for _, result := range results {
		if result.Error != "" {
			return "FAILED: " + strings.ReplaceAll(result.Error, "\n", " ")
		}
		if result.Skipped != "" {
			return "skipped: " + result.Skipped
		}
	}
	return ""
}

func humanBytes(n float64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.2fGiB", n/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.2fMiB", n/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1fKiB", n/(1<<10))
	default:
		return fmt.Sprintf("%.0fB", n)
	}
}
