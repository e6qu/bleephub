// Command bench measures git-on-object-storage implementations against one
// another: the same generated repository, the same go-git plumbing, the same
// metered path to the same object store, with only the storage design varying.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/e6qu/bleephub/gitstore/s3fake"
	minio "github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

type config struct {
	drivers   []string
	scenarios map[string]bool
	spec      WorkloadSpec
	latency   time.Duration
	runs      int
	parallel  int
	probes    int
	endpoint  string
	bucket    string
	region    string
	jsonPath  string
	benchPath string
	keep      bool
}

func parseFlags() (config, error) {
	var cfg config
	var drivers, scenarios string
	flag.StringVar(&drivers, "drivers", "gitstore,ogit,gogit-disk,gogit-memory", "comma-separated drivers; the first is the baseline of the \"vs first\" column")
	flag.StringVar(&scenarios, "scenarios", "all", "comma-separated scenarios to report, or all")
	flag.IntVar(&cfg.spec.Files, "files", 1000, "files in the generated tree")
	flag.IntVar(&cfg.spec.Commits, "commits", 30, "commits in the initial push")
	flag.IntVar(&cfg.spec.Pushes, "pushes", 10, "single-commit pushes after it")
	flag.IntVar(&cfg.spec.Changes, "changes", 8, "files each commit rewrites")
	flag.Uint64Var(&cfg.spec.Seed, "seed", 1, "workload seed")
	flag.DurationVar(&cfg.latency, "latency", 0, "delay injected into every object-store request, standing in for a remote region (try 5ms)")
	flag.IntVar(&cfg.runs, "runs", 3, "times to run each driver; the table reports medians")
	flag.IntVar(&cfg.parallel, "parallel", 8, "concurrent clones in the clone-parallel scenario")
	flag.IntVar(&cfg.probes, "probes", 1000, "absent objects asked about in the probe-absent scenario")
	flag.StringVar(&cfg.endpoint, "endpoint", "", "S3-compatible endpoint URL, credentials from AWS_ACCESS_KEY_ID/AWS_SECRET_ACCESS_KEY; empty runs an in-process fake")
	flag.StringVar(&cfg.bucket, "bucket", "gitstore-bench", "bucket to use; created if missing")
	flag.StringVar(&cfg.region, "region", "us-east-1", "region to sign for")
	flag.StringVar(&cfg.jsonPath, "json", "", "write the full report as JSON to this file")
	flag.StringVar(&cfg.benchPath, "benchfmt", "", "write Go benchmark lines to this file, for benchstat")
	flag.BoolVar(&cfg.keep, "keep", false, "leave this run's objects in the bucket")
	flag.Usage = func() {
		fmt.Fprintf(flag.CommandLine.Output(), "usage: bench [flags]\n\ndrivers:\n")
		for _, name := range storerDriverNames() {
			driver, _ := newStorerDriver(name)
			fmt.Fprintf(flag.CommandLine.Output(), "  %-14s %s\n", name, driver.Describe())
		}
		fmt.Fprintf(flag.CommandLine.Output(), "\nscenarios, in execution order:\n")
		for _, name := range scenarioOrder {
			fmt.Fprintf(flag.CommandLine.Output(), "  %-22s %s\n", name, scenarioNotes[name])
		}
		fmt.Fprintf(flag.CommandLine.Output(), "\nflags:\n")
		flag.PrintDefaults()
	}
	flag.Parse()

	cfg.drivers = strings.Split(drivers, ",")
	for _, name := range cfg.drivers {
		if _, err := newStorerDriver(name); err != nil {
			return cfg, err
		}
	}
	cfg.scenarios = map[string]bool{}
	for _, name := range scenarioOrder {
		cfg.scenarios[name] = scenarios == "all"
	}
	if scenarios != "all" {
		for _, name := range strings.Split(scenarios, ",") {
			if _, known := cfg.scenarios[name]; !known {
				return cfg, fmt.Errorf("unknown scenario %q (have %v)", name, scenarioOrder)
			}
			cfg.scenarios[name] = true
		}
	}
	if cfg.runs <= 0 || cfg.parallel <= 0 || cfg.probes <= 0 {
		return cfg, fmt.Errorf("runs, parallel and probes must be positive")
	}
	return cfg, nil
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "bench:", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := parseFlags()
	if err != nil {
		return err
	}
	ctx := context.Background()

	fmt.Fprintf(os.Stderr, "generating workload…\n")
	workload, err := GenerateWorkload(cfg.spec)
	if err != nil {
		return err
	}

	accessKey, secretKey := os.Getenv("AWS_ACCESS_KEY_ID"), os.Getenv("AWS_SECRET_ACCESS_KEY")
	endpointLabel := cfg.endpoint
	target := cfg.endpoint
	if target == "" {
		fake := s3fake.New()
		defer fake.Close()
		target, endpointLabel = fake.URL(), "in-process s3fake"
		accessKey, secretKey = "fake", "fake"
	}
	targetURL, err := url.Parse(target)
	if err != nil {
		return fmt.Errorf("endpoint %q: %w", target, err)
	}
	meter := NewMeter(targetURL)
	defer meter.Close()

	tempDir, err := os.MkdirTemp("", "gitstore-bench-*")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(tempDir) }()

	env := Env{
		Endpoint: meter.URL(), Bucket: cfg.bucket, Region: cfg.region,
		AccessKey: accessKey, SecretKey: secretKey,
		Prefix:  fmt.Sprintf("bench-%d", time.Now().UnixNano()),
		TempDir: tempDir,
	}
	if cfg.endpoint != "" {
		cleanup, err := prepareBucket(ctx, targetURL, env, cfg.keep)
		if err != nil {
			return err
		}
		defer cleanup()
	}

	report := &Report{
		Workload: cfg.spec, Objects: workload.Objects, Latency: cfg.latency,
		InitialObjects: workload.Initial.Objects, InitialPackBytes: len(workload.Initial.Pack),
		Endpoint: endpointLabel, Runs: cfg.runs, Drivers: map[string]string{},
	}
	meter.SetLatency(cfg.latency)
	for _, name := range cfg.drivers {
		for runIndex := range cfg.runs {
			driver, err := newStorerDriver(name)
			if err != nil {
				return err
			}
			report.Drivers[name] = driver.Describe()
			fmt.Fprintf(os.Stderr, "%s: run %d/%d…\n", name, runIndex+1, cfg.runs)
			if err := driver.Setup(ctx, env); err != nil {
				return fmt.Errorf("%s: setup: %w", name, err)
			}
			r := &runner{
				driver: driver, meter: meter, workload: workload,
				// A repository per run: a run must not inherit the packs and
				// caches the previous one left.
				repo: fmt.Sprintf("bench/repo-%d", runIndex),
				run:  runIndex, parallel: cfg.parallel, probes: cfg.probes, selected: cfg.scenarios,
			}
			report.Results = append(report.Results, r.execute(ctx)...)
			if err := driver.Close(); err != nil {
				return fmt.Errorf("%s: close: %w", name, err)
			}
		}
	}

	report.writeTable(os.Stdout, cfg.drivers)
	if cfg.jsonPath != "" {
		if err := writeFile(cfg.jsonPath, report.writeJSON); err != nil {
			return err
		}
	}
	if cfg.benchPath != "" {
		if err := writeFile(cfg.benchPath, report.writeBenchfmt); err != nil {
			return err
		}
	}
	for _, result := range report.Results {
		if result.Error != "" {
			return fmt.Errorf("%s/%s failed: %s", result.Driver, result.Scenario, result.Error)
		}
	}
	return nil
}

// writeFile creates the output file through a root scoped to its directory, so
// the name the operator passed cannot resolve — through a symlink, say — to
// anywhere outside the directory they named.
func writeFile(path string, write func(w io.Writer) error) error {
	root, err := os.OpenRoot(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()
	file, err := root.Create(filepath.Base(path))
	if err != nil {
		return err
	}
	if err := write(file); err != nil {
		_ = file.Close()
		return err
	}
	return file.Close()
}

// prepareBucket makes sure the bucket exists on a real endpoint, talking to it
// directly so the setup is not billed to any driver, and returns the function
// that removes this run's objects.
func prepareBucket(ctx context.Context, target *url.URL, env Env, keep bool) (func(), error) {
	client, err := minio.New(target.Host, &minio.Options{
		Creds:        credentials.NewStaticV4(env.AccessKey, env.SecretKey, ""),
		Secure:       target.Scheme == "https",
		Region:       env.Region,
		BucketLookup: minio.BucketLookupPath,
	})
	if err != nil {
		return nil, err
	}
	exists, err := client.BucketExists(ctx, env.Bucket)
	if err != nil {
		return nil, fmt.Errorf("check bucket %s: %w", env.Bucket, err)
	}
	if !exists {
		if err := client.MakeBucket(ctx, env.Bucket, minio.MakeBucketOptions{Region: env.Region}); err != nil {
			return nil, fmt.Errorf("make bucket %s: %w", env.Bucket, err)
		}
	}
	return func() {
		if keep {
			return
		}
		objects := client.ListObjects(ctx, env.Bucket, minio.ListObjectsOptions{Prefix: env.Prefix + "/", Recursive: true})
		for failure := range client.RemoveObjects(ctx, env.Bucket, objects, minio.RemoveObjectsOptions{}) {
			fmt.Fprintf(os.Stderr, "bench: cleanup %s: %v\n", failure.ObjectName, failure.Err)
		}
	}, nil
}
